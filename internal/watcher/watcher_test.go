package watcher

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/frankie0736/codex-watchdog/internal/config"
	"github.com/frankie0736/codex-watchdog/internal/home"
	"github.com/frankie0736/codex-watchdog/internal/state"

	_ "modernc.org/sqlite"
)

const capJSON = `{"message":"Selected model is at capacity. Please try a different model.","codexErrorInfo":"serverOverloaded"}`

// fixture is a fake Codex home whose databases are written through a separate
// read-write connection in WAL mode, like the real app-server does.
type fixture struct {
	t       *testing.T
	home    home.Home
	history *sql.DB
	state   *sql.DB
	queue   *sql.DB
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	open := func(name, ddl string) *sql.DB {
		db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, name)+"?_pragma=journal_mode(WAL)")
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		if _, err := db.Exec(ddl); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}
	f := &fixture{t: t}
	f.history = open("thread_history_1.sqlite", `CREATE TABLE thread_turns (
		thread_id TEXT NOT NULL, turn_id TEXT NOT NULL, rollout_ordinal INTEGER NOT NULL, status TEXT NOT NULL,
		error_json TEXT, started_at INTEGER, completed_at INTEGER, PRIMARY KEY(thread_id, turn_id))`)
	f.state = open("state_5.sqlite", `CREATE TABLE threads (
		id TEXT PRIMARY KEY, title TEXT, source TEXT, originator TEXT, model TEXT, cwd TEXT,
		updated_at INTEGER, archived INTEGER, rollout_path TEXT)`)
	f.queue = open("queue_1.sqlite", `CREATE TABLE queued_items (id TEXT PRIMARY KEY, thread_id TEXT NOT NULL)`)
	h, err := home.Inspect(dir)
	if err != nil {
		t.Fatal(err)
	}
	f.home = h
	return f
}

func (f *fixture) thread(id, source string) {
	f.exec(f.state, `INSERT INTO threads VALUES (?, 'title', ?, '', 'gpt', '/tmp', 0, 0, '')`, id, source)
}

func (f *fixture) turn(thread, turn string, ord int, status, errJSON string, at time.Time) {
	f.exec(f.history, `INSERT INTO thread_turns VALUES (?, ?, ?, ?, ?, ?, ?)`,
		thread, turn, ord, status, errJSON, at.Unix()-10, at.Unix())
}

func (f *fixture) exec(db *sql.DB, q string, args ...any) {
	f.t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		f.t.Fatal(err)
	}
}

type fakeSender struct {
	mu    sync.Mutex
	calls []string // codexHome|thread|message
	err   error
}

func (s *fakeSender) Send(_ context.Context, codexHome, thread, msg string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, codexHome+"|"+thread+"|"+msg)
	return "ok", s.err
}

func (s *fakeSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func testConfig(dry bool) config.Config {
	c := config.Default()
	c.DryRun = dry
	return c
}

func newWatcher(t *testing.T, f *fixture, st *state.Store, s *fakeSender, c *clock, cfg config.Config) *Watcher {
	w := New(Options{
		Config: cfg,
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
		State:  st,
		Sender: s,
		Homes:  []home.Home{f.home},
		Now:    c.now,
	})
	t.Cleanup(w.Close)
	return w
}

func openState(t *testing.T, dir string) *state.Store {
	st, _, err := state.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func poll(w *Watcher) Snapshot {
	s := w.Poll(context.Background())
	w.wg.Wait() // let async sends finish so results are deterministic
	return s
}

func rowFor(t *testing.T, s Snapshot, thread string) Row {
	t.Helper()
	for _, r := range s.Rows {
		if r.ThreadID == thread {
			return r
		}
	}
	t.Fatalf("thread %s not in snapshot (%d rows)", thread, len(s.Rows))
	return Row{}
}

func TestEndToEndRecoveryFlow(t *testing.T) {
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	f := newFixture(t)
	f.thread("T1", "cli")
	f.turn("T1", "turn-a", 1, "completed", "", c.t.Add(-10*time.Minute))
	f.turn("T1", "turn-b", 2, "failed", capJSON, c.t)

	stateDir := t.TempDir()
	st := openState(t, stateDir)
	s := &fakeSender{}
	w := newWatcher(t, f, st, s, c, testConfig(false))

	if got := rowFor(t, poll(w), "T1").Status; got != "WAIT_GRACE" {
		t.Fatalf("within grace: %s", got)
	}

	c.advance(31 * time.Second)
	for i := 0; i < 100; i++ {
		poll(w)
	}
	if s.count() != 1 {
		t.Fatalf("100 polls produced %d sends, want 1", s.count())
	}
	if want := f.home.Path + "|T1|继续"; s.calls[0] != want {
		t.Fatalf("call %q want %q", s.calls[0], want)
	}
	if got := rowFor(t, poll(w), "T1").Status; got != "QUEUED" {
		t.Fatalf("after send: %s", got)
	}

	// Restart: a fresh watcher on the same state db must not resend.
	w2 := newWatcher(t, f, openState(t, stateDir), s, c, testConfig(false))
	if got := rowFor(t, poll(w2), "T1").Status; got != "QUEUED" || s.count() != 1 {
		t.Fatalf("after restart: status=%s sends=%d", got, s.count())
	}

	// New turn appears (written through another connection while reader is open) -> RECOVERED.
	c.advance(time.Minute)
	f.turn("T1", "turn-c", 3, "inProgress", "", c.t)
	if got := rowFor(t, poll(w2), "T1").Status; got != StatusRecovered {
		t.Fatalf("after new turn: %s", got)
	}
}

func TestRetryBudgetThenNeedsAttention(t *testing.T) {
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	f := newFixture(t)
	f.thread("T1", "cli")
	f.turn("T1", "turn-a", 1, "failed", capJSON, c.t.Add(-time.Minute))
	s := &fakeSender{}
	cfg := testConfig(false)
	cfg.CapacityErrorMaxAgeSeconds = 24 * 3600
	w := newWatcher(t, f, openState(t, t.TempDir()), s, c, cfg)

	for i := 0; i < 20; i++ {
		poll(w)
		c.advance(time.Minute)
	}
	if s.count() != cfg.MaxAttemptsPerTurn {
		t.Fatalf("sends=%d want %d", s.count(), cfg.MaxAttemptsPerTurn)
	}
	if got := rowFor(t, poll(w), "T1").Status; got != "NEEDS_ATTENTION" {
		t.Fatalf("status %s", got)
	}
}

func TestBacklogBlocksSend(t *testing.T) {
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	f := newFixture(t)
	f.thread("T1", "cli")
	f.turn("T1", "turn-a", 1, "failed", capJSON, c.t.Add(-time.Minute))
	f.exec(f.queue, `INSERT INTO queued_items VALUES ('q1', 'T1')`)
	s := &fakeSender{}
	w := newWatcher(t, f, openState(t, t.TempDir()), s, c, testConfig(false))
	if got := rowFor(t, poll(w), "T1").Status; got != "BACKLOG" || s.count() != 0 {
		t.Fatalf("status=%s sends=%d", got, s.count())
	}
}

func TestFailedSendIsRetriedAfterInterval(t *testing.T) {
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	f := newFixture(t)
	f.thread("T1", "cli")
	f.turn("T1", "turn-a", 1, "failed", capJSON, c.t.Add(-time.Minute))
	s := &fakeSender{err: context.DeadlineExceeded}
	cfg := testConfig(false)
	cfg.PerThreadCooldownSeconds = 0
	w := newWatcher(t, f, openState(t, t.TempDir()), s, c, cfg)

	poll(w)
	if got := rowFor(t, poll(w), "T1").Status; got != "QUEUE_FAILED" {
		t.Fatalf("status %s", got)
	}
	c.advance(time.Duration(cfg.QueueRetrySeconds+1) * time.Second)
	s.err = nil
	poll(w)
	if s.count() != 2 {
		t.Fatalf("sends=%d want 2", s.count())
	}
}

func TestDryRunNeverSendsAndDoesNotConsumeLiveBudget(t *testing.T) {
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	f := newFixture(t)
	f.thread("T1", "cli")
	f.turn("T1", "turn-a", 1, "failed", capJSON, c.t.Add(-time.Minute))
	s := &fakeSender{}
	w := newWatcher(t, f, openState(t, t.TempDir()), s, c, testConfig(true))

	if got := rowFor(t, poll(w), "T1").Status; got != StatusDryRun {
		t.Fatalf("status %s", got)
	}
	poll(w)
	if s.count() != 0 {
		t.Fatalf("dry-run sent %d", s.count())
	}
	w.SetDryRun(false)
	poll(w)
	if s.count() != 1 {
		t.Fatalf("live after dry-run sent %d, want 1", s.count())
	}
}

func TestGlobalLimitAcrossThreads(t *testing.T) {
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	f := newFixture(t)
	for _, id := range []string{"A", "B", "C", "D"} {
		f.thread(id, "cli")
		f.turn(id, "turn-"+id, 1, "failed", capJSON, c.t.Add(-time.Minute))
	}
	s := &fakeSender{}
	cfg := testConfig(false)
	cfg.GlobalMaxSendsPerMinute = 2
	w := newWatcher(t, f, openState(t, t.TempDir()), s, c, cfg)
	snap := poll(w)
	if s.count() != 2 {
		t.Fatalf("sends=%d want 2", s.count())
	}
	if n := snap.Count(func(r Row) bool { return r.Status == "RATE_LIMITED" }); n != 2 {
		t.Fatalf("rate limited rows=%d want 2", n)
	}
	c.advance(61 * time.Second)
	poll(w)
	if s.count() != 4 {
		t.Fatalf("after window sends=%d want 4", s.count())
	}
}

func TestExcludedAndStaleAndPausedNeverSend(t *testing.T) {
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	f := newFixture(t)
	f.thread("EXEC", "exec")
	f.turn("EXEC", "t1", 1, "failed", capJSON, c.t.Add(-time.Minute))
	f.thread("SUB", `{"subagent":{"thread_spawn":{}}}`)
	f.turn("SUB", "t1", 1, "failed", capJSON, c.t.Add(-time.Minute))
	f.thread("OLD", "cli")
	f.turn("OLD", "t1", 1, "failed", capJSON, c.t.Add(-2*time.Hour))
	f.thread("ZOMBIE", "cli")
	f.turn("ZOMBIE", "t1", 1, "inProgress", "", c.t.Add(-12*time.Hour))
	f.thread("LIVE", "cli")
	f.turn("LIVE", "t1", 1, "failed", capJSON, c.t.Add(-time.Minute))

	s := &fakeSender{}
	w := newWatcher(t, f, openState(t, t.TempDir()), s, c, testConfig(false))
	w.TogglePause()
	snap := poll(w)
	want := map[string]string{"EXEC": "EXCLUDED", "SUB": "EXCLUDED", "OLD": "STALE", "ZOMBIE": "ORPHANED", "LIVE": StatusPaused}
	for id, st := range want {
		if got := rowFor(t, snap, id).Status; got != st {
			t.Errorf("%s: %s want %s", id, got, st)
		}
	}
	if s.count() != 0 {
		t.Fatalf("sent %d while paused", s.count())
	}
}

func TestOnlyThreadRestrictsSending(t *testing.T) {
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	f := newFixture(t)
	for _, id := range []string{"A", "B"} {
		f.thread(id, "cli")
		f.turn(id, "turn-"+id, 1, "failed", capJSON, c.t.Add(-time.Minute))
	}
	s := &fakeSender{}
	w := New(Options{
		Config: testConfig(false), Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
		State: openState(t, t.TempDir()), Sender: s, Homes: []home.Home{f.home}, Now: c.now, OnlyThread: "B",
	})
	t.Cleanup(w.Close)
	snap := poll(w)
	if s.count() != 1 || s.calls[0] != f.home.Path+"|B|继续" {
		t.Fatalf("calls=%v", s.calls)
	}
	if got := rowFor(t, snap, "A").Status; got != "SKIPPED" {
		t.Fatalf("A: %s", got)
	}
}

func TestDetachedThreadIsNotSent(t *testing.T) {
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	f := newFixture(t)
	f.thread("OPEN", "cli")
	f.turn("OPEN", "t1", 1, "failed", capJSON, c.t.Add(-time.Minute))
	f.thread("GONE", "cli")
	f.turn("GONE", "t1", 1, "failed", capJSON, c.t.Add(-time.Minute))
	f.exec(f.state, `UPDATE threads SET rollout_path = '/r/' || id || '.jsonl'`)
	s := &fakeSender{}
	loaded := func(context.Context) (map[string]bool, error) { return map[string]bool{"/r/OPEN.jsonl": true}, nil }
	w := New(Options{
		Config: testConfig(false), Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
		State: openState(t, t.TempDir()), Sender: s, Homes: []home.Home{f.home}, Now: c.now, Loaded: loaded,
	})
	t.Cleanup(w.Close)
	snap := poll(w)
	if s.count() != 1 || s.calls[0] != f.home.Path+"|OPEN|继续" {
		t.Fatalf("calls=%v", s.calls)
	}
	if got := rowFor(t, snap, "GONE").Status; got != StatusDetached {
		t.Fatalf("GONE: %s", got)
	}

	// Attach check failure must fail closed.
	f.turn("OPEN", "t2", 2, "failed", capJSON, c.t.Add(10*time.Minute))
	c.advance(20 * time.Minute)
	w.loaded = func(context.Context) (map[string]bool, error) { return nil, context.DeadlineExceeded }
	snap = poll(w)
	if s.count() != 1 || snap.Err == "" {
		t.Fatalf("sends=%d err=%q", s.count(), snap.Err)
	}
}
