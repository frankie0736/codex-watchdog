// Package store reads Codex SQLite state strictly read-only.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"

	"github.com/frankie0736/codex-watchdog/internal/home"

	_ "modernc.org/sqlite"
)

// Turn is the latest turn of a thread.
type Turn struct {
	ThreadID    string
	TurnID      string
	Status      string
	ErrorJSON   string
	StartedAt   int64 // unix seconds
	CompletedAt int64 // unix seconds, 0 if unset
	Ordinal     int64
}

// LastActivity returns the most recent known timestamp of the turn.
func (t Turn) LastActivity() int64 { return max(t.StartedAt, t.CompletedAt) }

type Thread struct {
	ID          string
	Title       string
	Source      string
	Originator  string
	Model       string
	Cwd         string
	UpdatedAt   int64
	Archived    bool
	RolloutPath string
}

type Store struct {
	Home    home.Home
	history *sql.DB
	state   *sql.DB
	queue   *sql.DB // nil if the home has no queue db
}

// OpenRO opens a SQLite file read-only. WAL contents are visible (no immutable=1).
func OpenRO(path string) (*sql.DB, error) {
	q := url.Values{}
	q.Set("mode", "ro")
	q.Add("_pragma", "busy_timeout(3000)")
	q.Add("_pragma", "query_only(1)")
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

var requiredColumns = map[string][]string{
	"history.thread_turns": {"thread_id", "turn_id", "rollout_ordinal", "status", "error_json", "started_at", "completed_at"},
	"state.threads":        {"id", "title", "source", "originator", "model", "cwd", "updated_at", "archived", "rollout_path"},
	"queue.queued_items":   {"thread_id"},
}

// Open opens all databases of a home and verifies the schema columns we depend on.
func Open(ctx context.Context, h home.Home) (*Store, error) {
	s := &Store{Home: h}
	var err error
	if s.history, err = OpenRO(h.HistoryDB); err != nil {
		return nil, err
	}
	if s.state, err = OpenRO(h.StateDB); err != nil {
		s.Close()
		return nil, err
	}
	if h.QueueDB != "" {
		if s.queue, err = OpenRO(h.QueueDB); err != nil {
			s.Close()
			return nil, err
		}
	}
	type check struct {
		db    *sql.DB
		key   string
		table string
	}
	checks := []check{
		{s.history, "history.thread_turns", "thread_turns"},
		{s.state, "state.threads", "threads"},
	}
	if s.queue != nil {
		checks = append(checks, check{s.queue, "queue.queued_items", "queued_items"})
	}
	for _, c := range checks {
		if err := checkColumns(ctx, c.db, c.table, requiredColumns[c.key]); err != nil {
			s.Close()
			return nil, fmt.Errorf("%s: schema incompatible: %w", h.Path, err)
		}
	}
	return s, nil
}

func (s *Store) Close() {
	for _, db := range []*sql.DB{s.history, s.state, s.queue} {
		if db != nil {
			db.Close()
		}
	}
}

func checkColumns(ctx context.Context, db *sql.DB, table string, want []string) error {
	rows, err := db.QueryContext(ctx, "SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return fmt.Errorf("table_info %s: %w", table, err)
	}
	defer rows.Close()
	have := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return err
		}
		have[n] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(have) == 0 {
		return fmt.Errorf("table %s not found", table)
	}
	var missing []string
	for _, c := range want {
		if !have[c] {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("table %s missing columns %v", table, missing)
	}
	return nil
}

// LatestTurns returns the latest turn (by rollout_ordinal) of every thread that had
// turn activity at or after sinceUnix, plus threads listed in extra (e.g. ones we queued).
func (s *Store) LatestTurns(ctx context.Context, sinceUnix int64, extra []string) ([]Turn, error) {
	args := []any{sinceUnix, sinceUnix}
	extraClause := ""
	if len(extra) > 0 {
		extraClause = " OR thread_id IN (" + placeholders(len(extra)) + ")"
		for _, id := range extra {
			args = append(args, id)
		}
	}
	q := `
SELECT thread_id, turn_id, status, COALESCE(error_json, ''), COALESCE(started_at, 0),
       COALESCE(completed_at, 0), rollout_ordinal
FROM (
  SELECT *, ROW_NUMBER() OVER (PARTITION BY thread_id ORDER BY rollout_ordinal DESC) AS rn
  FROM thread_turns
  WHERE thread_id IN (
    SELECT DISTINCT thread_id FROM thread_turns
    WHERE started_at >= ? OR completed_at >= ?` + extraClause + `
  )
) WHERE rn = 1`
	rows, err := s.history.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query latest turns: %w", err)
	}
	defer rows.Close()
	var out []Turn
	for rows.Next() {
		var t Turn
		if err := rows.Scan(&t.ThreadID, &t.TurnID, &t.Status, &t.ErrorJSON, &t.StartedAt, &t.CompletedAt, &t.Ordinal); err != nil {
			return nil, fmt.Errorf("scan turn: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Threads returns thread metadata keyed by id.
func (s *Store) Threads(ctx context.Context, ids []string) (map[string]Thread, error) {
	out := make(map[string]Thread, len(ids))
	const batch = 500
	for i := 0; i < len(ids); i += batch {
		chunk := ids[i:min(i+batch, len(ids))]
		args := make([]any, len(chunk))
		for j, id := range chunk {
			args[j] = id
		}
		q := `SELECT id, COALESCE(title, ''), COALESCE(source, ''), COALESCE(originator, ''),
       COALESCE(model, ''), COALESCE(cwd, ''), COALESCE(updated_at, 0), COALESCE(archived, 0), COALESCE(rollout_path, '')
FROM threads WHERE id IN (` + placeholders(len(chunk)) + `)`
		rows, err := s.state.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, fmt.Errorf("query threads: %w", err)
		}
		for rows.Next() {
			var t Thread
			var archived int64
			if err := rows.Scan(&t.ID, &t.Title, &t.Source, &t.Originator, &t.Model, &t.Cwd, &t.UpdatedAt, &archived, &t.RolloutPath); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan thread: %w", err)
			}
			t.Archived = archived != 0
			out[t.ID] = t
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// PendingQueue returns the number of unconsumed queued items per thread.
// Returns an empty map when the home has no queue database.
func (s *Store) PendingQueue(ctx context.Context) (map[string]int, error) {
	out := map[string]int{}
	if s.queue == nil {
		return out, nil
	}
	rows, err := s.queue.QueryContext(ctx, `SELECT thread_id, COUNT(*) FROM queued_items GROUP BY thread_id`)
	if err != nil {
		return nil, fmt.Errorf("query queued_items: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
