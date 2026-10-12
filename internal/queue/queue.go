// Package queue executes `codex queue` for a specific CODEX_HOME.
package queue

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

type Sender interface {
	Send(ctx context.Context, codexHome, threadID, message string) (output string, err error)
}

type CLI struct {
	Bin     string
	Timeout time.Duration
}

const maxOutput = 500

// Send runs `codex queue --thread <id> --message <msg>` with CODEX_HOME set.
// Arguments are passed as an argv array; no shell is involved.
func (c CLI) Send(ctx context.Context, codexHome, threadID, message string) (string, error) {
	if threadID == "" || strings.HasPrefix(threadID, "-") {
		return "", fmt.Errorf("invalid thread id %q", threadID)
	}
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Bin, "queue", "--thread", threadID, "--message", message)
	cmd.Env = append(withoutEnv(os.Environ(), "CODEX_HOME"), "CODEX_HOME="+codexHome)
	cmd.Stdin = nil
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	summary := truncate(strings.TrimSpace(out.String()), maxOutput)
	if ctx.Err() == context.DeadlineExceeded {
		return summary, fmt.Errorf("codex queue timed out after %s", c.Timeout)
	}
	if err != nil {
		return summary, fmt.Errorf("codex queue: %w", err)
	}
	return summary, nil
}

func withoutEnv(env []string, key string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, key+"=") {
			out = append(out, kv)
		}
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
