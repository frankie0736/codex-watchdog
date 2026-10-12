// Package state persists the watchdog's own send attempts. It never touches Codex databases.
package state

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type Result string

const (
	Pending     Result = "pending"     // recorded before exec; still in flight
	Sent        Result = "sent"        // codex queue exited 0 (or simulated in dry-run)
	Failed      Result = "failed"      // codex queue failed
	Interrupted Result = "interrupted" // watchdog died mid-send; treated as possibly sent
)

type Attempt struct {
	ID         int64
	Home       string
	ThreadID   string
	TurnID     string
	No         int
	DryRun     bool
	Result     Result
	Detail     string
	DecidedAt  time.Time
	FinishedAt time.Time // zero while pending
}

// At is the time the attempt last changed state.
func (a Attempt) At() time.Time {
	if !a.FinishedAt.IsZero() {
		return a.FinishedAt
	}
	return a.DecidedAt
}

type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS attempts (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  home        TEXT NOT NULL,
  thread_id   TEXT NOT NULL,
  turn_id     TEXT NOT NULL,
  attempt_no  INTEGER NOT NULL,
  dry_run     INTEGER NOT NULL,
  result      TEXT NOT NULL,
  detail      TEXT NOT NULL DEFAULT '',
  decided_at  INTEGER NOT NULL,
  finished_at INTEGER NOT NULL DEFAULT 0,
  UNIQUE (home, thread_id, turn_id, attempt_no, dry_run)
);
CREATE INDEX IF NOT EXISTS attempts_decided_at ON attempts(decided_at);`

// Open opens (creating if needed) state.db in dir and marks leftover pending attempts as interrupted.
func Open(ctx context.Context, dir string) (*Store, int64, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, 0, fmt.Errorf("state dir %s: %w", dir, err)
	}
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(3000)")
	q.Add("_pragma", "journal_mode(WAL)")
	dsn := (&url.URL{Scheme: "file", Path: filepath.Join(dir, "state.db"), RawQuery: q.Encode()}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, 0, fmt.Errorf("open state db: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, schema); err != nil {
		db.Close()
		return nil, 0, fmt.Errorf("migrate state db: %w", err)
	}
	res, err := db.ExecContext(ctx,
		`UPDATE attempts SET result = ?, detail = 'watchdog exited while queue was running', finished_at = ? WHERE result = ?`,
		Interrupted, time.Now().Unix(), Pending)
	if err != nil {
		db.Close()
		return nil, 0, fmt.Errorf("recover pending attempts: %w", err)
	}
	n, _ := res.RowsAffected()
	return &Store{db: db}, n, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Begin records an attempt as pending before the side effect runs.
// The UNIQUE constraint rejects a duplicate attempt for the same turn and number.
func (s *Store) Begin(ctx context.Context, a Attempt) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO attempts (home, thread_id, turn_id, attempt_no, dry_run, result, detail, decided_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		a.Home, a.ThreadID, a.TurnID, a.No, boolInt(a.DryRun), Pending, a.Detail, a.DecidedAt.Unix())
	if err != nil {
		return 0, fmt.Errorf("record attempt %s/%s#%d: %w", a.ThreadID, a.TurnID, a.No, err)
	}
	return res.LastInsertId()
}

func (s *Store) Finish(ctx context.Context, id int64, r Result, detail string, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE attempts SET result = ?, detail = ?, finished_at = ? WHERE id = ?`,
		r, detail, at.Unix(), id)
	if err != nil {
		return fmt.Errorf("finish attempt %d: %w", id, err)
	}
	return nil
}

// Since returns attempts decided at or after t, oldest first.
func (s *Store) Since(ctx context.Context, t time.Time) ([]Attempt, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, home, thread_id, turn_id, attempt_no, dry_run, result, detail, decided_at, finished_at
		 FROM attempts WHERE decided_at >= ? ORDER BY decided_at, id`, t.Unix())
	if err != nil {
		return nil, fmt.Errorf("query attempts: %w", err)
	}
	defer rows.Close()
	var out []Attempt
	for rows.Next() {
		var a Attempt
		var dry int
		var decided, finished int64
		if err := rows.Scan(&a.ID, &a.Home, &a.ThreadID, &a.TurnID, &a.No, &dry, &a.Result, &a.Detail, &decided, &finished); err != nil {
			return nil, fmt.Errorf("scan attempt: %w", err)
		}
		a.DryRun = dry != 0
		a.DecidedAt = time.Unix(decided, 0)
		if finished > 0 {
			a.FinishedAt = time.Unix(finished, 0)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
