// Package wqueue is the agent-side write-ahead queue. Per-(profile,
// vault) SQLite store at <VaultBaseDir>/wqueue.sqlite plus a staging
// directory for attachment bytes. See issue #61.
//
// Queued writes are deliberately invisible to brain_recall until the
// daemon synthesises them and the next snapshot ships.
//
// This file is the minimal surface Stream D (operator CLI) depends on.
// Stream B owns the drainer + immediate-attempt wiring and may extend
// this file; the locked API is documented in the issue.
package wqueue

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// Kind enumerates the queueable write operations.
type Kind string

const (
	KindPerceive    Kind = "perceive"
	KindLearn       Kind = "learn"
	KindAttach      Kind = "attach"
	KindTrace       Kind = "trace"
	KindTaskPromote Kind = "task_promote"
)

// MaxStagedBytes is the per-attachment ceiling enforced at Enqueue.
const MaxStagedBytes int64 = 100 * 1024 * 1024

// Item is one row in the queue.
type Item struct {
	ID            int64
	Kind          Kind
	SHA           string
	PayloadJSON   []byte
	StagedPath    string
	EnqueuedAt    time.Time
	Attempts      int
	LastAttemptAt time.Time
	LastError     string
}

// EnqueueOpts is the input to Enqueue.
type EnqueueOpts struct {
	Kind        Kind
	SHA         string
	PayloadJSON []byte
	Bytes       []byte
	Ext         string
}

// Queue is the write-ahead store.
type Queue struct {
	dir        string
	stagingDir string
	db         *sql.DB
}

// Sentinel errors.
var (
	ErrOversize    = errors.New("wqueue: staged bytes exceed MaxStagedBytes")
	ErrInvalidKind = errors.New("wqueue: invalid Kind")
	ErrEmptySHA    = errors.New("wqueue: SHA required")
)

const schemaSQL = `
CREATE TABLE IF NOT EXISTS wqueue (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  kind TEXT NOT NULL,
  sha  TEXT NOT NULL,
  payload_json BLOB NOT NULL,
  staged_path TEXT NOT NULL DEFAULT '',
  enqueued_at INTEGER NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0,
  last_attempt_at INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS wqueue_eligible ON wqueue(last_attempt_at, id);
`

// Open opens (or creates) the queue at dir. dir is an absolute path;
// the caller typically passes Agent.VaultBaseDir().
func Open(dir string) (*Queue, error) {
	if dir == "" {
		return nil, errors.New("wqueue: dir required")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("wqueue: mkdir dir: %w", err)
	}
	staging := filepath.Join(dir, "wqueue-attach")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return nil, fmt.Errorf("wqueue: mkdir staging: %w", err)
	}
	dbPath := filepath.Join(dir, "wqueue.sqlite")
	dsn := fmt.Sprintf("file:%s?_journal_mode=WAL&_busy_timeout=5000&_synchronous=NORMAL", dbPath)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("wqueue: open sqlite: %w", err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("wqueue: schema: %w", err)
	}
	return &Queue{dir: dir, stagingDir: staging, db: db}, nil
}

// Close closes the underlying DB handle.
func (q *Queue) Close() error {
	if q == nil || q.db == nil {
		return nil
	}
	return q.db.Close()
}

// Dir returns the queue's root directory.
func (q *Queue) Dir() string { return q.dir }

// StagingDir returns the directory holding attachment staging files.
func (q *Queue) StagingDir() string { return q.stagingDir }

func validKind(k Kind) bool {
	switch k {
	case KindPerceive, KindLearn, KindAttach, KindTrace, KindTaskPromote:
		return true
	}
	return false
}

// Enqueue persists the item. For KindAttach, copies opts.Bytes into
// the staging dir before inserting the row so a crash mid-Enqueue
// leaves at worst an orphan file (recoverable via Cleanup).
func (q *Queue) Enqueue(ctx context.Context, opts EnqueueOpts) (*Item, error) {
	if !validKind(opts.Kind) {
		return nil, ErrInvalidKind
	}
	if opts.SHA == "" {
		return nil, ErrEmptySHA
	}
	staged := ""
	if opts.Kind == KindAttach {
		if int64(len(opts.Bytes)) > MaxStagedBytes {
			return nil, ErrOversize
		}
		staged = filepath.Join(q.stagingDir, opts.SHA+opts.Ext)
		if err := os.WriteFile(staged, opts.Bytes, 0o600); err != nil {
			return nil, fmt.Errorf("wqueue: stage attach: %w", err)
		}
	}
	now := time.Now()
	res, err := q.db.ExecContext(ctx,
		`INSERT INTO wqueue(kind, sha, payload_json, staged_path, enqueued_at) VALUES(?,?,?,?,?)`,
		string(opts.Kind), opts.SHA, opts.PayloadJSON, staged, now.UnixNano(),
	)
	if err != nil {
		if staged != "" {
			_ = os.Remove(staged)
		}
		return nil, fmt.Errorf("wqueue: insert: %w", err)
	}
	id, _ := res.LastInsertId()
	return &Item{
		ID:          id,
		Kind:        opts.Kind,
		SHA:         opts.SHA,
		PayloadJSON: opts.PayloadJSON,
		StagedPath:  staged,
		EnqueuedAt:  now,
	}, nil
}

// Depth returns the current row count.
func (q *Queue) Depth(ctx context.Context) (int, error) {
	var n int
	err := q.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM wqueue`).Scan(&n)
	return n, err
}

// NextEligible returns rows whose backoff window has expired, oldest
// first, capped at limit.
func (q *Queue) NextEligible(ctx context.Context, now time.Time, limit int) ([]*Item, error) {
	if limit <= 0 {
		limit = 16
	}
	rows, err := q.db.QueryContext(ctx,
		`SELECT id, kind, sha, payload_json, staged_path, enqueued_at, attempts, last_attempt_at, last_error
		   FROM wqueue
		  ORDER BY id ASC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*Item, 0, limit)
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		if it.Attempts > 0 {
			delay := BackoffFor(it.Attempts-1, nil)
			if it.LastAttemptAt.Add(delay).After(now) {
				continue
			}
		}
		out = append(out, it)
		if len(out) >= limit {
			break
		}
	}
	return out, rows.Err()
}

// MarkAttempt records a failed attempt.
func (q *Queue) MarkAttempt(ctx context.Context, id int64, now time.Time, attemptErr error) error {
	msg := ""
	if attemptErr != nil {
		msg = attemptErr.Error()
	}
	_, err := q.db.ExecContext(ctx,
		`UPDATE wqueue SET attempts = attempts + 1, last_attempt_at = ?, last_error = ? WHERE id = ?`,
		now.UnixNano(), msg, id,
	)
	return err
}

// Delete removes the row and (for KindAttach) the staging file.
func (q *Queue) Delete(ctx context.Context, id int64) error {
	var staged string
	err := q.db.QueryRowContext(ctx, `SELECT staged_path FROM wqueue WHERE id = ?`, id).Scan(&staged)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := q.db.ExecContext(ctx, `DELETE FROM wqueue WHERE id = ?`, id); err != nil {
		return err
	}
	if staged != "" {
		_ = os.Remove(staged)
	}
	return nil
}

// List returns every row, newest first. limit=0 means no cap.
func (q *Queue) List(ctx context.Context, limit int) ([]*Item, error) {
	q1 := `SELECT id, kind, sha, payload_json, staged_path, enqueued_at, attempts, last_attempt_at, last_error
	         FROM wqueue ORDER BY id DESC`
	if limit > 0 {
		q1 += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := q.db.QueryContext(ctx, q1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// Clear removes every row and every staging file.
func (q *Queue) Clear(ctx context.Context) (int, error) {
	res, err := q.db.ExecContext(ctx, `DELETE FROM wqueue`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	entries, err := os.ReadDir(q.stagingDir)
	if err == nil {
		for _, e := range entries {
			_ = os.Remove(filepath.Join(q.stagingDir, e.Name()))
		}
	}
	return int(n), nil
}

// Cleanup removes staging files with no matching row.
func (q *Queue) Cleanup(ctx context.Context) (int, int64, error) {
	entries, err := os.ReadDir(q.stagingDir)
	if err != nil {
		return 0, 0, err
	}
	known := make(map[string]struct{})
	rows, err := q.db.QueryContext(ctx, `SELECT staged_path FROM wqueue WHERE staged_path != ''`)
	if err != nil {
		return 0, 0, err
	}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return 0, 0, err
		}
		known[p] = struct{}{}
	}
	rows.Close()
	var orphans int
	var freed int64
	for _, e := range entries {
		full := filepath.Join(q.stagingDir, e.Name())
		if _, ok := known[full]; ok {
			continue
		}
		fi, err := os.Stat(full)
		if err != nil {
			continue
		}
		if err := os.Remove(full); err == nil {
			orphans++
			freed += fi.Size()
		}
	}
	return orphans, freed, nil
}

// BackoffFor returns the wait duration for attempt N (0-indexed).
// Exponential base 30s, cap 5min, +/-20% jitter when rng is non-nil.
func BackoffFor(attempts int, rng *rand.Rand) time.Duration {
	if attempts < 0 {
		attempts = 0
	}
	base := 30 * time.Second
	d := base << attempts
	if d <= 0 || d > 5*time.Minute {
		d = 5 * time.Minute
	}
	if rng == nil {
		return d
	}
	jitter := (rng.Float64()*0.4 - 0.2) // -0.2..+0.2
	return d + time.Duration(float64(d)*jitter)
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanItem(s rowScanner) (*Item, error) {
	var (
		it       Item
		kind     string
		enq, las int64
	)
	if err := s.Scan(&it.ID, &kind, &it.SHA, &it.PayloadJSON, &it.StagedPath, &enq, &it.Attempts, &las, &it.LastError); err != nil {
		return nil, err
	}
	it.Kind = Kind(kind)
	it.EnqueuedAt = time.Unix(0, enq)
	if las > 0 {
		it.LastAttemptAt = time.Unix(0, las)
	}
	return &it, nil
}
