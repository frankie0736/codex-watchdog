// Package home discovers Codex home directories and their versioned SQLite files.
package home

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/frankie0736/codex-watchdog/internal/config"
)

// Home is one CODEX_HOME with resolved database paths. QueueDB may be empty.
type Home struct {
	Path      string
	StateDB   string
	HistoryDB string
	QueueDB   string
}

// Label is a short display name, e.g. "~/.codex".
func (h Home) Label() string {
	if u, err := os.UserHomeDir(); err == nil && strings.HasPrefix(h.Path, u+string(filepath.Separator)) {
		return "~" + strings.TrimPrefix(h.Path, u)
	}
	return h.Path
}

// Resolve returns the configured homes, plus auto-discovered ~/.codex* homes when enabled.
// Duplicates are removed by absolute path.
func Resolve(cfg config.Config) ([]Home, []error) {
	var paths []string
	for _, hc := range cfg.CodexHomes {
		paths = append(paths, config.ExpandHome(hc.Path))
	}
	if cfg.AutoDiscoverHomes {
		matches, _ := filepath.Glob(config.ExpandHome("~/.codex*"))
		paths = append(paths, matches...)
	}

	seen := map[string]bool{}
	var homes []Home
	var errs []error
	for _, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil || seen[abs] {
			continue
		}
		seen[abs] = true
		if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
			if !cfg.AutoDiscoverHomes || isConfigured(cfg, abs) {
				errs = append(errs, fmt.Errorf("codex home %s: not a directory", abs))
			}
			continue
		}
		h, err := Inspect(abs)
		if err != nil {
			// Auto-discovered dirs without Codex databases are silently skipped.
			if isConfigured(cfg, abs) {
				errs = append(errs, err)
			}
			continue
		}
		homes = append(homes, h)
	}
	sort.Slice(homes, func(i, j int) bool { return homes[i].Path < homes[j].Path })
	return homes, errs
}

func isConfigured(cfg config.Config, abs string) bool {
	for _, hc := range cfg.CodexHomes {
		if p, _ := filepath.Abs(config.ExpandHome(hc.Path)); p == abs {
			return true
		}
	}
	return false
}

// Inspect resolves the highest-versioned state_N / thread_history_N / queue_N databases.
func Inspect(dir string) (Home, error) {
	h := Home{Path: dir}
	h.StateDB = latest(dir, "state")
	h.HistoryDB = latest(dir, "thread_history")
	h.QueueDB = latest(dir, "queue")
	if h.StateDB == "" || h.HistoryDB == "" {
		return h, fmt.Errorf("codex home %s: missing state_*.sqlite or thread_history_*.sqlite", dir)
	}
	return h, nil
}

func latest(dir, prefix string) string {
	re := regexp.MustCompile("^" + regexp.QuoteMeta(prefix) + `_(\d+)\.sqlite$`)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	best, bestN := "", -1
	for _, e := range entries {
		m := re.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		if n, _ := strconv.Atoi(m[1]); n > bestN {
			best, bestN = filepath.Join(dir, e.Name()), n
		}
	}
	return best
}
