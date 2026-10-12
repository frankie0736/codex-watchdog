package detector

import (
	"testing"
	"time"

	"github.com/frankie0736/codex-watchdog/internal/store"
)

const capJSON = `{"message":"Selected model is at capacity. Please try a different model.","codexErrorInfo":"serverOverloaded","additionalDetails":null,"misalignment":null}`

func TestIsCapacityError(t *testing.T) {
	cases := map[string]bool{
		capJSON: true,
		`{"message":"x","codexErrorInfo":"serverOverloaded"}`:                                       true,
		`{"message":"SELECTED MODEL IS AT CAPACITY now","codexErrorInfo":"other"}`:                  true,
		`{"message":"x","codexErrorInfo":{"responseTooManyFailedAttempts":{"httpStatusCode":429}}}`: false,
		`not json but Selected model is at capacity`:                                                true,
		`{"message":"cyber","codexErrorInfo":"cyberPolicy"}`:                                        false,
		"": false,
	}
	for in, want := range cases {
		if got := IsCapacityError(in); got != want {
			t.Errorf("IsCapacityError(%q)=%v want %v", in, got, want)
		}
	}
}

func TestNormalizeSource(t *testing.T) {
	cases := map[string]string{
		"cli":    "cli",
		"vscode": "vscode",
		"":       "unknown",
		`{"subagent":{"thread_spawn":{"parent_thread_id":"x"}}}`: "subagent",
	}
	for in, want := range cases {
		if got := NormalizeSource(in); got != want {
			t.Errorf("NormalizeSource(%q)=%q want %q", in, got, want)
		}
	}
}

func TestClassify(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	p := Params{
		ActiveMaxAge:  6 * time.Hour,
		ErrorMaxAge:   time.Hour,
		SourceAllowed: func(k string) bool { return k == "cli" || k == "vscode" },
	}
	cli := &store.Thread{Source: "cli"}
	ago := func(d time.Duration) int64 { return now.Add(-d).Unix() }

	cases := []struct {
		name string
		turn store.Turn
		th   *store.Thread
		want Class
	}{
		{"recent inProgress", store.Turn{Status: "inProgress", StartedAt: ago(time.Minute)}, cli, Active},
		{"zombie inProgress", store.Turn{Status: "inProgress", StartedAt: ago(48 * time.Hour)}, cli, Orphaned},
		{"fresh capacity", store.Turn{Status: "failed", ErrorJSON: capJSON, StartedAt: ago(5 * time.Minute), CompletedAt: ago(time.Minute)}, cli, Capacity},
		{"old capacity", store.Turn{Status: "failed", ErrorJSON: capJSON, StartedAt: ago(3 * time.Hour), CompletedAt: ago(2 * time.Hour)}, cli, Stale},
		{"capacity exec", store.Turn{Status: "failed", ErrorJSON: capJSON, CompletedAt: ago(time.Minute)}, &store.Thread{Source: "exec"}, Excluded},
		{"capacity subagent", store.Turn{Status: "failed", ErrorJSON: capJSON, CompletedAt: ago(time.Minute)}, &store.Thread{Source: `{"subagent":{}}`}, Excluded},
		{"capacity archived", store.Turn{Status: "failed", ErrorJSON: capJSON, CompletedAt: ago(time.Minute)}, &store.Thread{Source: "cli", Archived: true}, Excluded},
		{"capacity no metadata", store.Turn{Status: "failed", ErrorJSON: capJSON, CompletedAt: ago(time.Minute)}, nil, Excluded},
		{"other error", store.Turn{Status: "failed", ErrorJSON: `{"message":"boom","codexErrorInfo":"other"}`, CompletedAt: ago(time.Minute)}, cli, OtherError},
		{"interrupted", store.Turn{Status: "interrupted", CompletedAt: ago(time.Minute)}, cli, Idle},
		{"completed", store.Turn{Status: "completed", CompletedAt: ago(time.Minute)}, cli, Idle},
	}
	for _, c := range cases {
		if got := Classify(c.turn, c.th, now, p); got != c.want {
			t.Errorf("%s: got %s want %s", c.name, got, c.want)
		}
	}
}
