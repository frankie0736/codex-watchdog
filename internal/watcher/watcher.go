// Package watcher polls Codex homes, classifies threads, and triggers rate-limited continues.
package watcher

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/frankie0736/codex-watchdog/internal/config"
	"github.com/frankie0736/codex-watchdog/internal/detector"
	"github.com/frankie0736/codex-watchdog/internal/home"
	"github.com/frankie0736/codex-watchdog/internal/limiter"
	"github.com/frankie0736/codex-watchdog/internal/queue"
	"github.com/frankie0736/codex-watchdog/internal/state"
	"github.com/frankie0736/codex-watchdog/internal/store"
)

// Display-only statuses beyond detector classes and limiter statuses.
const (
	StatusRecovered = "RECOVERED"
	StatusPaused    = "PAUSED"
	StatusWouldSend = "WOULD_SEND"
	StatusDryRun    = "DRY_RUN"
	StatusDetached  = "DETACHED"
)

type Row struct {
	HomeLabel    string
	HomePath     string
	ThreadID     string
	Title        string
	Source       string
	Model        string
	Cwd          string
	TurnID       string
	TurnStatus   string
	Class        detector.Class
	Status       string
	Reason       string
	Until        time.Time
	LastActivity time.Time
	Error        string
	LastAttempt  *state.Attempt
	Attempts     int // attempts on this thread in the current mode within the history window
	PendingQueue int
	RolloutPath  string
	Attached     bool // thread's rollout file is held open by a running codex process
}

type HomeStatus struct {
	Label   string
	Path    string
	Threads int
	Err     string
}

type Snapshot struct {
	At          time.Time
	RunID       string
	Homes       []HomeStatus
	Rows        []Row
	Paused      bool
	DryRun      bool
	SentThisRun int
	Recent      []state.Attempt // newest first
	Err         string
	Config      config.Config
}

func (s Snapshot) Count(pred func(Row) bool) int {
	n := 0
	for _, r := range s.Rows {
		if pred(r) {
			n++
		}
	}
	return n
}

type key struct{ home, thread, turn string }

type Watcher struct {
	cfg        config.Config
	log        *slog.Logger
	st         *state.Store
	sender     queue.Sender
	homes      []home.Home
	homeErrs   []error
	RunID      string
	start      time.Time
	now        func() time.Time
	readOnly   bool   // never records or sends; used by -once
	onlyThread string // when set, only this thread may be acted on
	loaded     func(context.Context) (map[string]bool, error)

	mu          sync.Mutex
	paused      bool
	dryRun      bool
	sentThisRun int
	inflight    map[key]bool
	stores      map[string]*store.Store
	prevStatus  map[string]string

	wg      sync.WaitGroup
	refresh chan struct{}
	updates chan Snapshot
}

type Options struct {
	Config   config.Config
	Logger   *slog.Logger
	State    *state.Store
	Sender   queue.Sender
	Homes    []home.Home
	HomeErrs []error
	ReadOnly bool
	// OnlyThread restricts sending to one thread id; others are displayed but never acted on.
	OnlyThread string
	// Loaded returns rollout paths currently held open by codex processes.
	// Nil means every thread counts as attached (tests).
	Loaded func(context.Context) (map[string]bool, error)
	Now    func() time.Time
}

func New(o Options) *Watcher {
	now := o.Now
	if now == nil {
		now = time.Now
	}
	runID := newRunID()
	return &Watcher{
		cfg:        o.Config,
		log:        o.Logger.With("run_id", runID),
		st:         o.State,
		sender:     o.Sender,
		homes:      o.Homes,
		homeErrs:   o.HomeErrs,
		RunID:      runID,
		start:      now(),
		now:        now,
		readOnly:   o.ReadOnly,
		onlyThread: o.OnlyThread,
		loaded:     o.Loaded,
		dryRun:     o.Config.DryRun,
		inflight:   map[key]bool{},
		stores:     map[string]*store.Store{},
		prevStatus: map[string]string{},
		refresh:    make(chan struct{}, 1),
		updates:    make(chan Snapshot, 1),
	}
}

func newRunID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (w *Watcher) Updates() <-chan Snapshot { return w.updates }

func (w *Watcher) Refresh() {
	select {
	case w.refresh <- struct{}{}:
	default:
	}
}

func (w *Watcher) TogglePause() {
	w.mu.Lock()
	w.paused = !w.paused
	p := w.paused
	w.mu.Unlock()
	w.log.Info("mode_change", "paused", p)
	w.Refresh()
}

func (w *Watcher) SetDryRun(v bool) {
	w.mu.Lock()
	from := w.dryRun
	w.dryRun = v
	w.mu.Unlock()
	w.log.Info("mode_change", "dry_run_from", from, "dry_run_to", v)
	w.Refresh()
}

func (w *Watcher) DryRun() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dryRun
}

// Run polls until ctx is cancelled, publishing a snapshot after each poll.
func (w *Watcher) Run(ctx context.Context) {
	t := time.NewTicker(w.cfg.PollInterval())
	defer t.Stop()
	for {
		snap := w.Poll(ctx)
		select {
		case <-w.updates: // drop stale snapshot
		default:
		}
		w.updates <- snap
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-w.refresh:
		}
	}
}

// Close waits for in-flight sends and closes database handles.
func (w *Watcher) Close() {
	w.wg.Wait()
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, s := range w.stores {
		s.Close()
	}
	w.stores = map[string]*store.Store{}
}

func (w *Watcher) storeFor(ctx context.Context, h home.Home) (*store.Store, error) {
	w.mu.Lock()
	s := w.stores[h.Path]
	w.mu.Unlock()
	if s != nil {
		return s, nil
	}
	s, err := store.Open(ctx, h)
	if err != nil {
		return nil, err
	}
	w.mu.Lock()
	w.stores[h.Path] = s
	w.mu.Unlock()
	return s, nil
}

func (w *Watcher) dropStore(path string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if s := w.stores[path]; s != nil {
		s.Close()
		delete(w.stores, path)
	}
}

// Poll performs one full observe/decide/act cycle.
func (w *Watcher) Poll(ctx context.Context) Snapshot {
	now := w.now()
	w.mu.Lock()
	paused, dry, sent := w.paused, w.dryRun, w.sentThisRun
	w.mu.Unlock()

	snap := Snapshot{At: now, RunID: w.RunID, Paused: paused, DryRun: dry, Config: w.cfg}
	for _, e := range w.homeErrs {
		snap.Homes = append(snap.Homes, HomeStatus{Label: "config", Err: e.Error()})
	}

	historyWindow := max(w.cfg.ErrorMaxAge(), 24*time.Hour)
	attempts, err := w.st.Since(ctx, now.Add(-historyWindow))
	if err != nil {
		snap.Err = err.Error()
		w.log.Error("state_read_failed", "err", err)
		return snap
	}
	for i := len(attempts) - 1; i >= 0 && len(snap.Recent) < 100; i-- {
		snap.Recent = append(snap.Recent, attempts[i])
	}

	// Index attempts of the current mode only, so dry-run never consumes live budget.
	byTurn := map[key][]state.Attempt{}
	lastByThread := map[string]state.Attempt{} // home|thread
	countByThread := map[string]int{}
	extra := map[string][]string{} // home -> thread ids with attempts (any mode)
	globalLastMinute := 0
	for _, a := range attempts {
		extra[a.Home] = append(extra[a.Home], a.ThreadID)
		if a.DryRun != dry {
			continue
		}
		k := key{a.Home, a.ThreadID, a.TurnID}
		byTurn[k] = append(byTurn[k], a)
		tk := a.Home + "|" + a.ThreadID
		lastByThread[tk] = a
		countByThread[tk]++
		if !a.DecidedAt.Before(now.Add(-time.Minute)) {
			globalLastMinute++
		}
	}

	lp := limiter.Params{
		Grace:           w.cfg.Grace(),
		Cooldown:        w.cfg.Cooldown(),
		QueueRetry:      w.cfg.QueueRetry(),
		DeliveryTimeout: w.cfg.DeliveryTimeout(),
		MaxAttempts:     w.cfg.MaxAttemptsPerTurn,
		GlobalPerMinute: w.cfg.GlobalMaxSendsPerMinute,
		StartupRecover:  w.cfg.StartupRecoverRecentErrors,
	}
	// Look back a day so recently orphaned turns stay visible (with "a") instead of vanishing.
	since := now.Add(-max(w.cfg.ActiveMaxAge(), w.cfg.ErrorMaxAge(), 24*time.Hour)).Unix()

	// Fail closed: if attachment cannot be determined, nothing counts as attached.
	var loaded map[string]bool
	if w.loaded != nil && w.cfg.RequireAttached {
		if loaded, err = w.loaded(ctx); err != nil {
			snap.Err = "attach check failed, not sending: " + err.Error()
			w.log.Error("attach_check_failed", "err", err)
			loaded = map[string]bool{}
		}
	}

	for _, h := range w.homes {
		hs := HomeStatus{Label: h.Label(), Path: h.Path}
		rows, err := w.pollHome(ctx, h, since, uniq(extra[h.Path]))
		if err != nil {
			hs.Err = err.Error()
			w.log.Error("home_poll_failed", "home", h.Path, "err", err)
			w.dropStore(h.Path)
			snap.Homes = append(snap.Homes, hs)
			continue
		}
		hs.Threads = len(rows)
		snap.Homes = append(snap.Homes, hs)

		for i := range rows {
			r := &rows[i]
			tk := h.Path + "|" + r.ThreadID
			r.Attempts = countByThread[tk]
			r.Attached = loaded == nil || (r.RolloutPath != "" && loaded[filepath.Clean(r.RolloutPath)])
			if a, ok := lastByThread[tk]; ok {
				a := a
				r.LastAttempt = &a
			}
			if r.Class != detector.Capacity {
				if r.LastAttempt != nil && r.LastAttempt.TurnID != r.TurnID &&
					r.LastAttempt.At().After(now.Add(-w.cfg.ErrorMaxAge())) &&
					(r.Class == detector.Active || r.Class == detector.Idle) {
					r.Status = StatusRecovered
					r.Reason = fmt.Sprintf("new turn after attempt %d", r.LastAttempt.No)
				}
				continue
			}
			if w.onlyThread != "" && r.ThreadID != w.onlyThread {
				r.Status, r.Reason = string(limiter.Skipped), "not the --thread target"
				continue
			}
			if !r.Attached {
				r.Status, r.Reason = StatusDetached, "session not open in any codex client; not sending"
				continue
			}
			k := key{h.Path, r.ThreadID, r.TurnID}
			w.mu.Lock()
			busy := w.inflight[k]
			w.mu.Unlock()
			if busy {
				r.Status, r.Reason = string(limiter.InFlight), "codex queue running"
				continue
			}
			var lastThreadAt time.Time
			if r.LastAttempt != nil {
				lastThreadAt = r.LastAttempt.At()
			}
			d := limiter.Decide(limiter.Input{
				Now:              now,
				ErrorAt:          r.LastActivity,
				WatcherStart:     w.start,
				TurnAttempts:     byTurn[k],
				LastThreadAt:     lastThreadAt,
				GlobalLastMinute: globalLastMinute,
				PendingQueue:     r.PendingQueue,
			}, lp)
			r.Status, r.Reason, r.Until = string(d.Status), d.Reason, d.Until
			if d.Status != limiter.Send {
				continue
			}
			switch {
			case w.readOnly:
				r.Status = StatusWouldSend
			case paused:
				r.Status, r.Reason = StatusPaused, "would send "+d.Reason
			default:
				if a, ok := w.launch(ctx, h, *r, d.AttemptNo, dry, now); ok {
					globalLastMinute++
					r.LastAttempt = &a
					r.Attempts++
					if dry {
						r.Status, r.Reason = StatusDryRun, fmt.Sprintf("would run codex queue (attempt %d)", d.AttemptNo)
					} else {
						r.Status, r.Reason = string(limiter.InFlight), fmt.Sprintf("attempt %d", d.AttemptNo)
					}
				} else {
					r.Status, r.Reason = string(limiter.QueueFailed), "could not record attempt; see log"
				}
			}
		}
		snap.Rows = append(snap.Rows, rows...)
	}

	sortRows(snap.Rows)
	w.logTransitions(snap.Rows)
	w.mu.Lock()
	snap.SentThisRun = max(sent, w.sentThisRun)
	w.mu.Unlock()
	return snap
}

func (w *Watcher) pollHome(ctx context.Context, h home.Home, since int64, extra []string) ([]Row, error) {
	s, err := w.storeFor(ctx, h)
	if err != nil {
		return nil, err
	}
	turns, err := s.LatestTurns(ctx, since, extra)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(turns))
	for i, t := range turns {
		ids[i] = t.ThreadID
	}
	threads, err := s.Threads(ctx, ids)
	if err != nil {
		return nil, err
	}
	pending, err := s.PendingQueue(ctx)
	if err != nil {
		return nil, err
	}
	now := w.now()
	params := detector.Params{
		ActiveMaxAge:  w.cfg.ActiveMaxAge(),
		ErrorMaxAge:   w.cfg.ErrorMaxAge(),
		SourceAllowed: w.cfg.SourceAllowed,
	}
	rows := make([]Row, 0, len(turns))
	for _, t := range turns {
		var th *store.Thread
		if v, ok := threads[t.ThreadID]; ok {
			th = &v
		}
		cls := detector.Classify(t, th, now, params)
		r := Row{
			HomeLabel:    h.Label(),
			HomePath:     h.Path,
			ThreadID:     t.ThreadID,
			Source:       detector.SourceLabel(th),
			TurnID:       t.TurnID,
			TurnStatus:   t.Status,
			Class:        cls,
			Status:       string(cls),
			LastActivity: time.Unix(t.LastActivity(), 0),
			Error:        detector.ErrorSummary(t.ErrorJSON),
			PendingQueue: pending[t.ThreadID],
		}
		if th != nil {
			r.Title, r.Model, r.Cwd, r.RolloutPath = th.Title, th.Model, th.Cwd, th.RolloutPath
		}
		if cls == detector.Excluded {
			r.Reason = "capacity error, but thread is archived / missing / source not allowed"
		}
		rows = append(rows, r)
	}
	return rows, nil
}

// launch persists the attempt first, then executes the send (async for live mode).
func (w *Watcher) launch(ctx context.Context, h home.Home, r Row, no int, dry bool, now time.Time) (state.Attempt, bool) {
	a := state.Attempt{
		Home: h.Path, ThreadID: r.ThreadID, TurnID: r.TurnID, No: no, DryRun: dry,
		Result: state.Pending, DecidedAt: now,
	}
	id, err := w.st.Begin(ctx, a)
	if err != nil {
		w.log.Error("attempt_record_failed", "home", h.Path, "thread_id", r.ThreadID, "turn_id", r.TurnID, "attempt", no, "err", err)
		return a, false
	}
	a.ID = id
	logArgs := []any{"home", h.Path, "thread_id", r.ThreadID, "turn_id", r.TurnID, "attempt", no, "dry_run", dry}

	if dry {
		a.Result, a.Detail, a.FinishedAt = state.Sent, "dry-run: codex queue not executed", now
		if err := w.st.Finish(ctx, id, a.Result, a.Detail, now); err != nil {
			w.log.Error("attempt_finish_failed", append(logArgs, "err", err)...)
		}
		w.log.Info("continue_simulated", logArgs...)
		return a, true
	}

	k := key{h.Path, r.ThreadID, r.TurnID}
	w.mu.Lock()
	w.inflight[k] = true
	w.mu.Unlock()
	w.log.Info("continue_start", logArgs...)

	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		// Let an in-flight send finish even during shutdown; it is bounded by queue_timeout_seconds.
		bg := context.WithoutCancel(ctx)
		began := time.Now()
		out, err := w.sender.Send(bg, h.Path, r.ThreadID, w.cfg.Message)
		result, detail := state.Sent, out
		if err != nil {
			result = state.Failed
			detail = err.Error()
			if out != "" {
				detail += ": " + out
			}
		}
		if ferr := w.st.Finish(bg, id, result, detail, w.now()); ferr != nil {
			w.log.Error("attempt_finish_failed", append(logArgs, "err", ferr)...)
		}
		w.mu.Lock()
		delete(w.inflight, k)
		if result == state.Sent {
			w.sentThisRun++
		}
		w.mu.Unlock()
		w.log.Info("continue_done", append(logArgs, "result", result, "latency_ms", time.Since(began).Milliseconds(), "detail", detail)...)
		w.Refresh()
	}()
	return a, true
}

func (w *Watcher) logTransitions(rows []Row) {
	seen := make(map[string]bool, len(rows))
	for _, r := range rows {
		k := r.HomePath + "|" + r.ThreadID
		seen[k] = true
		prev, ok := w.prevStatus[k]
		if ok && prev == r.Status {
			continue
		}
		w.prevStatus[k] = r.Status
		if !ok && (r.Class == detector.Idle || r.Class == detector.Orphaned || r.Class == detector.Stale) {
			continue // don't log the initial load of uninteresting threads
		}
		w.log.Info("status_change", "home", r.HomePath, "thread_id", r.ThreadID, "turn_id", r.TurnID,
			"from", prev, "to", r.Status, "reason", r.Reason)
	}
	for k := range w.prevStatus {
		if !seen[k] {
			delete(w.prevStatus, k)
		}
	}
}

var statusRank = map[string]int{
	string(limiter.NeedsAttention): 0,
	string(limiter.QueueFailed):    1,
	string(limiter.InFlight):       2,
	string(limiter.Send):           2,
	StatusWouldSend:                2,
	StatusDryRun:                   2,
	StatusPaused:                   3,
	string(limiter.Queued):         3,
	string(limiter.Backlog):        3,
	string(limiter.WaitGrace):      4,
	string(limiter.Cooldown):       4,
	string(limiter.RateLimited):    4,
	string(limiter.Skipped):        5,
	StatusDetached:                 5,
	StatusRecovered:                6,
	string(detector.Active):        7,
	string(detector.Excluded):      8,
	string(detector.OtherError):    9,
	string(detector.Stale):         10,
	string(detector.Orphaned):      11,
	string(detector.Idle):          12,
}

func sortRows(rows []Row) {
	sort.SliceStable(rows, func(i, j int) bool {
		ri, rj := statusRank[rows[i].Status], statusRank[rows[j].Status]
		if ri != rj {
			return ri < rj
		}
		return rows[i].LastActivity.After(rows[j].LastActivity)
	})
}

func uniq(ss []string) []string {
	seen := map[string]bool{}
	out := ss[:0:0]
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
