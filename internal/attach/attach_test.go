package attach

import (
	"context"
	"testing"
)

func TestParse(t *testing.T) {
	out := []byte("p51674\nn/dev/null\nncodex-home/.codex-sub/sessions/2026/10/12/rollout-2026-10-12T14-00-00-abc.jsonl\n" +
		"ncwd\np35454\nncodex-home/.codex/sessions/rollout-x.jsonl\nncodex-home/.codex/state_5.sqlite\n")
	got := Parse(out)
	if len(got) != 2 ||
		!got["codex-home/.codex-sub/sessions/2026/10/12/rollout-2026-10-12T14-00-00-abc.jsonl"] ||
		!got["codex-home/.codex/sessions/rollout-x.jsonl"] {
		t.Fatalf("got %v", got)
	}
}

// Smoke test against the real system: must not error even when no codex process runs.
func TestLoadedRolloutsRuns(t *testing.T) {
	if _, err := LoadedRollouts(context.Background()); err != nil {
		t.Fatal(err)
	}
}
