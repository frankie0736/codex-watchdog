// Package detector classifies a thread's latest turn. Pure functions, no I/O.
package detector

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/frankie0736/codex-watchdog/internal/store"
)

type Class string

const (
	Active     Class = "ACTIVE"   // latest turn inProgress and recent
	Orphaned   Class = "ORPHANED" // inProgress but too old: almost certainly a dead process
	Capacity   Class = "CAPACITY" // latest turn failed with a fresh capacity error
	Stale      Class = "STALE"    // capacity error older than the freshness window
	OtherError Class = "ERROR"    // failed for another reason; never auto-continued
	Idle       Class = "IDLE"     // completed / interrupted / unknown status
	Excluded   Class = "EXCLUDED" // capacity error on a thread we must not touch
)

const capacityText = "selected model is at capacity"

// IsCapacityError matches codexErrorInfo=serverOverloaded or the capacity message text.
func IsCapacityError(errorJSON string) bool {
	if errorJSON == "" {
		return false
	}
	var e struct {
		Message        string          `json:"message"`
		CodexErrorInfo json.RawMessage `json:"codexErrorInfo"`
	}
	if json.Unmarshal([]byte(errorJSON), &e) == nil {
		var info string
		if json.Unmarshal(e.CodexErrorInfo, &info) == nil && info == "serverOverloaded" {
			return true
		}
		if strings.Contains(strings.ToLower(e.Message), capacityText) {
			return true
		}
	}
	return strings.Contains(strings.ToLower(errorJSON), capacityText)
}

// ErrorSummary returns a short human-readable error message.
func ErrorSummary(errorJSON string) string {
	if errorJSON == "" {
		return ""
	}
	var e struct {
		Message string `json:"message"`
	}
	if json.Unmarshal([]byte(errorJSON), &e) == nil && e.Message != "" {
		return e.Message
	}
	return errorJSON
}

// NormalizeSource maps thread.source to a short kind: cli, vscode, exec, subagent, ...
func NormalizeSource(raw string) string {
	s := strings.TrimSpace(raw)
	if strings.HasPrefix(s, "{") {
		if strings.Contains(s, "subagent") {
			return "subagent"
		}
		return "structured"
	}
	if s == "" {
		return "unknown"
	}
	return s
}

// SourceLabel is the display origin: originator when present (e.g. "Codex Desktop"), else source kind.
func SourceLabel(th *store.Thread) string {
	if th == nil {
		return "unknown"
	}
	if th.Originator != "" {
		return th.Originator
	}
	return NormalizeSource(th.Source)
}

type Params struct {
	ActiveMaxAge  time.Duration
	ErrorMaxAge   time.Duration
	SourceAllowed func(kind string) bool
}

// Classify decides the class of a thread from its latest turn and metadata.
// th may be nil when the thread is missing from state db; such threads are never auto-continued.
func Classify(t store.Turn, th *store.Thread, now time.Time, p Params) Class {
	switch t.Status {
	case "inProgress":
		if now.Sub(time.Unix(t.StartedAt, 0)) <= p.ActiveMaxAge {
			return Active
		}
		return Orphaned
	case "failed":
		if !IsCapacityError(t.ErrorJSON) {
			return OtherError
		}
		if now.Sub(time.Unix(t.LastActivity(), 0)) > p.ErrorMaxAge {
			return Stale
		}
		if th == nil || th.Archived || !p.SourceAllowed(NormalizeSource(th.Source)) {
			return Excluded
		}
		return Capacity
	default:
		return Idle
	}
}
