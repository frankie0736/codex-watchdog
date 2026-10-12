// Package limiter decides whether to queue a continue message. Pure function, time injected.
package limiter

import (
	"fmt"
	"time"

	"github.com/frankie0736/codex-watchdog/internal/state"
)

type Status string

const (
	Send           Status = "SEND"
	InFlight       Status = "QUEUEING"
	WaitGrace      Status = "WAIT_GRACE"
	Cooldown       Status = "COOLDOWN"
	RateLimited    Status = "RATE_LIMITED"
	Backlog        Status = "BACKLOG"
	Queued         Status = "QUEUED"
	QueueFailed    Status = "QUEUE_FAILED"
	NeedsAttention Status = "NEEDS_ATTENTION"
	Skipped        Status = "SKIPPED"
)

type Params struct {
	Grace           time.Duration
	Cooldown        time.Duration
	QueueRetry      time.Duration
	DeliveryTimeout time.Duration
	MaxAttempts     int
	GlobalPerMinute int
	StartupRecover  bool
}

// Input describes one capacity-failed turn. All attempt data must be from the same mode (dry-run or live).
type Input struct {
	Now              time.Time
	ErrorAt          time.Time // when the failed turn finished
	WatcherStart     time.Time
	TurnAttempts     []state.Attempt // attempts for this exact (home, thread, turn), oldest first
	LastThreadAt     time.Time       // last attempt on this thread (any turn), zero if none
	GlobalLastMinute int             // attempts across all threads in the last minute
	PendingQueue     int             // unconsumed items in codex queue db for this thread
}

type Decision struct {
	Status    Status
	Reason    string
	AttemptNo int       // set when Status == Send
	Until     time.Time // earliest time the blocking condition may clear; zero if unknown
}

// Decide applies, in order: startup policy, prior attempts / retry budget, grace,
// per-thread cooldown, codex queue backlog, global rate limit.
func Decide(in Input, p Params) Decision {
	n := len(in.TurnAttempts)
	if n == 0 && !p.StartupRecover && in.ErrorAt.Before(in.WatcherStart) {
		return Decision{Status: Skipped, Reason: "error predates watchdog start (startup_recover_recent_errors=false)"}
	}

	if n > 0 {
		last := in.TurnAttempts[n-1]
		switch last.Result {
		case state.Pending:
			return Decision{Status: InFlight, Reason: fmt.Sprintf("attempt %d running", last.No)}
		case state.Sent, state.Interrupted:
			if until := last.At().Add(p.DeliveryTimeout); in.Now.Before(until) {
				return Decision{Status: Queued, Reason: fmt.Sprintf("attempt %d queued, waiting for new turn", last.No), Until: until}
			}
		case state.Failed:
			if until := last.At().Add(p.QueueRetry); in.Now.Before(until) {
				return Decision{Status: QueueFailed, Reason: fmt.Sprintf("attempt %d failed: %s", last.No, last.Detail), Until: until}
			}
		}
		if n >= p.MaxAttempts {
			return Decision{Status: NeedsAttention, Reason: fmt.Sprintf("%d attempts without recovery; check manually", n)}
		}
	}

	if until := in.ErrorAt.Add(p.Grace); in.Now.Before(until) {
		return Decision{Status: WaitGrace, Reason: "grace period", Until: until}
	}
	if !in.LastThreadAt.IsZero() {
		if until := in.LastThreadAt.Add(p.Cooldown); in.Now.Before(until) {
			return Decision{Status: Cooldown, Reason: "per-thread cooldown", Until: until}
		}
	}
	if in.PendingQueue > 0 {
		return Decision{Status: Backlog, Reason: fmt.Sprintf("%d message(s) still unconsumed in codex queue; not stacking", in.PendingQueue)}
	}
	if in.GlobalLastMinute >= p.GlobalPerMinute {
		return Decision{Status: RateLimited, Reason: fmt.Sprintf("global limit %d/min reached", p.GlobalPerMinute)}
	}
	return Decision{Status: Send, Reason: fmt.Sprintf("attempt %d", n+1), AttemptNo: n + 1}
}
