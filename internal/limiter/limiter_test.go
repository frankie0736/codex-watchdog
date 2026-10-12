package limiter

import (
	"testing"
	"time"

	"github.com/frankie0736/codex-watchdog/internal/state"
)

var (
	t0 = time.Unix(1_800_000_000, 0)
	p  = Params{
		Grace:           30 * time.Second,
		Cooldown:        5 * time.Minute,
		QueueRetry:      2 * time.Minute,
		DeliveryTimeout: 5 * time.Minute,
		MaxAttempts:     3,
		GlobalPerMinute: 5,
		StartupRecover:  true,
	}
)

func attempt(no int, r state.Result, at time.Time) state.Attempt {
	return state.Attempt{No: no, Result: r, DecidedAt: at, FinishedAt: at}
}

func TestDecide(t *testing.T) {
	errAt := t0
	cases := []struct {
		name   string
		in     Input
		params func(Params) Params
		want   Status
	}{
		{"within grace", Input{Now: errAt.Add(10 * time.Second), ErrorAt: errAt}, nil, WaitGrace},
		{"after grace sends", Input{Now: errAt.Add(31 * time.Second), ErrorAt: errAt}, nil, Send},
		{"cooldown from another turn", Input{Now: errAt.Add(time.Minute), ErrorAt: errAt, LastThreadAt: errAt.Add(-time.Minute)}, nil, Cooldown},
		{"backlog blocks stacking", Input{Now: errAt.Add(time.Minute), ErrorAt: errAt, PendingQueue: 1}, nil, Backlog},
		{"global limit", Input{Now: errAt.Add(time.Minute), ErrorAt: errAt, GlobalLastMinute: 5}, nil, RateLimited},
		{"sent waits for delivery", Input{
			Now: errAt.Add(2 * time.Minute), ErrorAt: errAt,
			TurnAttempts: []state.Attempt{attempt(1, state.Sent, errAt.Add(time.Minute))},
		}, nil, Queued},
		{"interrupted treated as sent", Input{
			Now: errAt.Add(2 * time.Minute), ErrorAt: errAt,
			TurnAttempts: []state.Attempt{attempt(1, state.Interrupted, errAt.Add(time.Minute))},
		}, nil, Queued},
		{"pending is in flight", Input{
			Now: errAt.Add(2 * time.Minute), ErrorAt: errAt,
			TurnAttempts: []state.Attempt{{No: 1, Result: state.Pending, DecidedAt: errAt.Add(time.Minute)}},
		}, nil, InFlight},
		{"delivery timeout allows retry", Input{
			Now: errAt.Add(7 * time.Minute), ErrorAt: errAt,
			TurnAttempts: []state.Attempt{attempt(1, state.Sent, errAt.Add(time.Minute))},
			LastThreadAt: errAt.Add(time.Minute),
		}, nil, Send},
		{"failed waits retry interval", Input{
			Now: errAt.Add(2 * time.Minute), ErrorAt: errAt,
			TurnAttempts: []state.Attempt{attempt(1, state.Failed, errAt.Add(time.Minute))},
		}, nil, QueueFailed},
		{"max attempts", Input{
			Now: errAt.Add(time.Hour), ErrorAt: errAt,
			TurnAttempts: []state.Attempt{
				attempt(1, state.Sent, errAt.Add(time.Minute)),
				attempt(2, state.Sent, errAt.Add(10*time.Minute)),
				attempt(3, state.Sent, errAt.Add(20*time.Minute)),
			},
		}, nil, NeedsAttention},
		{"startup skip when disabled", Input{Now: errAt.Add(time.Minute), ErrorAt: errAt, WatcherStart: errAt.Add(time.Second)},
			func(p Params) Params { p.StartupRecover = false; return p }, Skipped},
		{"startup allowed when enabled", Input{Now: errAt.Add(time.Minute), ErrorAt: errAt, WatcherStart: errAt.Add(time.Second)}, nil, Send},
	}
	for _, c := range cases {
		pp := p
		if c.params != nil {
			pp = c.params(pp)
		}
		got := Decide(c.in, pp)
		if got.Status != c.want {
			t.Errorf("%s: got %s (%s) want %s", c.name, got.Status, got.Reason, c.want)
		}
	}
}

func TestDecideAttemptNumber(t *testing.T) {
	in := Input{
		Now: t0.Add(time.Hour), ErrorAt: t0,
		TurnAttempts: []state.Attempt{attempt(1, state.Failed, t0.Add(time.Minute))},
	}
	got := Decide(in, p)
	if got.Status != Send || got.AttemptNo != 2 {
		t.Fatalf("got %+v, want SEND attempt 2", got)
	}
}
