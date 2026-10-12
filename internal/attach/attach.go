// Package attach detects which Codex threads are currently loaded by a running codex process.
//
// Evidence (2026-10-12): an app-server keeps a thread's rollout-*.jsonl open while the thread is
// loaded; threads whose client was exited are not held. `codex queue` is only consumed for loaded
// threads, so sending to an unloaded thread just leaves the message in queue_*.sqlite.
package attach

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// LoadedRollouts returns the set of rollout file paths held open by processes named "codex".
func LoadedRollouts(ctx context.Context) (map[string]bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "lsof", "-n", "-P", "-F", "n", "-c", "codex")
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	err := cmd.Run()
	// lsof exits 1 when nothing matched (or some files were unreadable); output is still valid.
	var ee *exec.ExitError
	if err != nil && !(errors.As(err, &ee) && ee.ExitCode() == 1) {
		return nil, fmt.Errorf("lsof: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return Parse(out.Bytes()), nil
}

// Parse extracts rollout jsonl paths from `lsof -F n` output.
func Parse(out []byte) map[string]bool {
	set := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "n") {
			continue
		}
		p := line[1:]
		if base := filepath.Base(p); strings.HasPrefix(base, "rollout-") && strings.HasSuffix(base, ".jsonl") {
			set[filepath.Clean(p)] = true
		}
	}
	return set
}
