// Package wqueue is the agent-side write-ahead queue for daemon writes.
//
// When the daemon is unreachable, brain_perceive / brain_learn / brain_attach
// persist the wire request to this queue and return success to the caller.
// A background drainer attempts each item on a backoff schedule; on the
// first success the connectivity state flips back to online and the item
// is deleted.
//
// Queued writes are deliberately INVISIBLE to brain_recall until the
// daemon synthesises them and the next snapshot ships. The agent never
// projects pending writes into the local read cache.
//
// This is the Stream C dependency surface. The drainer goroutine and
// operator CLI live in Stream B / Stream D respectively; this package
// provides Open / Enqueue / Depth / Delete / List / NextEligible /
// MarkAttempt / Clear / Cleanup / Close.
package wqueue

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"time"

	pbsqlite "github.com/neverprepared/phantom-brain/internal/sqlite"
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

// Sentinel errors.
var (
	ErrOversize    = errors.New("wqueue: staged bytes exceed MaxStagedBytes")
	ErrInvalidKind = errors.New("wqueue: invalid Kind")
	ErrEmptySHA    = errors.New("wqueue: SHA required")
)

// Item is a row in the queue.
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

// EnqueueOpts is the request shape callers fill.
type EnqueueOpts struct {
	Kind        Kind
	SHA         string
	PayloadJSON []byte
	Bytes       []byte // Attach only: copied to staging dir
	Ext         string // Attach only: extension including leading dot
}

// Queue is the agent's write-ahead store.
type Queue struct {
	dir      string
	stageDir string
	db       *sql.DB
}

const schema = `
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

// Open opens (or creates) the queue at dir. dir is an absolute path —
// typically agent.VaultBaseDir().
func Open(dir string) (*Queue, error) {
	if dir == "" {
		return nil, errors.New("wqueue: Open requires non-empty dir")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("wqueue: mkdir %q: %w", dir, err)
	}
	stage := filepath.Join(dir, "wqueue-attach")
	if err := os.MkdirAll(stage, 0o755); err != nil {
		return nil, fmt.Errorf("wqueue: mkdir staging %q: %w", stage, err)
	}
	dbPath := filepath.Join(dir, "wqueue.sqlite")
	db, err := pbsqlite.Open(pbsqlite.Options{Path: dbPath})
	if err != nil {
		return nil, fmt.Errorf("wqueue: open db: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("wqueue: apply schema: %w", err)
	}
	return &Queue{dir: dir, stageDir: stage, db: db}, nil
}

// Close releases the underlying DB handle.
func (q *Queue) Close() error {
	if q == nil || q.db == nil {
		return nil
	}
	return q.db.Close()
}

// Dir returns the queue's root dir.
func (q *Queue) Dir() string { return q.dir }

// StageDir returns the staging dir for attachment bytes.
func (q *Queue) StageDir() string { return q.stageDir }

// Enqueue persists the item. For KindAttach, copies opts.Bytes into
// <dir>/wqueue-attach/<sha><ext> BEFORE inserting the row so a crash
// mid-Enqueue can't leave a row pointing at a missing file.
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
		staged = filepath.Join(q.stageDir, opts.SHA+opts.Ext)
		if err := os.WriteFile(staged, opts.Bytes, 0o600); err != nil {
			return nil, fmt.Errorf("wqueue: write staged bytes: %w", err)
		}
	}
	now := time.Now()
	res, err := q.db.ExecContext(ctx,
		`INSERT INTO wqueue (kind, sha, payload_json, staged_path, enqueued_at) VALUES (?, ?, ?, ?, ?)`,
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
		ID: id, Kind: opts.Kind, SHA: opts.SHA,
		PayloadJSON: opts.PayloadJSON, StagedPath: staged,
		EnqueuedAt: now,
	}, nil
}

// Depth returns the current row count.
func (q *Queue) Depth(ctx context.Context) (int, error) {
	var n int
	if err := q.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM wqueue`).Scan(&n); err != nil {
		return 0, fmt.Errorf("wqueue: depth: %w", err)
	}
	return n, nil
}

// Delete removes the row and, for attach rows, the staging file.
// Idempotent.
func (q *Queue) Delete(ctx context.Context, id int64) error {
	var staged string
	err := q.db.QueryRowContext(ctx, `SELECT staged_path FROM wqueue WHERE id = ?`, id).Scan(&staged)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("wqueue: read staged_path: %w", err)
	}
	if _, err := q.db.ExecContext(ctx, `DELETE FROM wqueue WHERE id = ?`, id); err != nil {
		return fmt.Errorf("wqueue: delete row: %w", err)
	}
	if staged != "" {
		_ = os.Remove(staged)
	}
	return nil
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
	if err != nil {
		return fmt.Errorf("wqueue: mark attempt: %w", err)
	}
	return nil
}

// List returns rows newest-first up to limit (0 == no cap).
func (q *Queue) List(ctx context.Context, limit int) ([]*Item, error) {
	query := `SELECT id, kind, sha, payload_json, staged_path, enqueued_at, attempts, last_attempt_at, last_error FROM wqueue ORDER BY id DESC`
	args := []any{}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := q.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("wqueue: list: %w", err)
	}
	defer rows.Close()
	return scanItems(rows)
}

// NextEligible returns up to limit rows whose backoff window has
// expired, oldest first.
func (q *Queue) NextEligible(ctx context.Context, now time.Time, limit int) ([]*Item, error) {
	if limit <= 0 {
		limit = 16
	}
	rows, err := q.db.QueryContext(ctx,
		`SELECT id, kind, sha, payload_json, staged_path, enqueued_at, attempts, last_attempt_at, last_error FROM wqueue ORDER BY id ASC LIMIT ?`,
		limit*4,
	)
	if err != nil {
		return nil, fmt.Errorf("wqueue: next eligible: %w", err)
	}
	defer rows.Close()
	all, err := scanItems(rows)
	if err != nil {
		return nil, err
	}
	rng := rand.New(rand.NewSource(now.UnixNano()))
	out := make([]*Item, 0, limit)
	for _, it := range all {
		if it.LastAttemptAt.IsZero() {
			out = append(out, it)
		} else {
			wait := BackoffFor(it.Attempts, rng)
			if now.Sub(it.LastAttemptAt) >= wait {
				out = append(out, it)
			}
		}
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// Clear deletes every row and every staging file. Returns rows deleted.
func (q *Queue) Clear(ctx context.Context) (int, error) {
	n, err := q.Depth(ctx)
	if err != nil {
		return 0, err
	}
	if _, err := q.db.ExecContext(ctx, `DELETE FROM wqueue`); err != nil {
		return 0, fmt.Errorf("wqueue: clear: %w", err)
	}
	entries, err := os.ReadDir(q.stageDir)
	if err == nil {
		for _, e := range entries {
			_ = os.Remove(filepath.Join(q.stageDir, e.Name()))
		}
	}
	return n, nil
}

// Cleanup sweeps the staging dir for orphaned files (no matching row).
func (q *Queue) Cleanup(ctx context.Context) (orphans int, bytesFreed int64, err error) {
	entries, err := os.ReadDir(q.stageDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, fmt.Errorf("wqueue: read staging dir: %w", err)
	}
	rows, err := q.db.QueryContext(ctx, `SELECT staged_path FROM wqueue WHERE staged_path != ''`)
	if err != nil {
		return 0, 0, fmt.Errorf("wqueue: read staged paths: %w", err)
	}
	referenced := map[string]struct{}{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err == nil {
			referenced[filepath.Base(p)] = struct{}{}
		}
	}
	rows.Close()
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if _, ok := referenced[e.Name()]; ok {
			continue
		}
		full := filepath.Join(q.stageDir, e.Name())
		info, statErr := os.Stat(full)
		if statErr == nil {
			bytesFreed += info.Size()
		}
		if rmErr := os.Remove(full); rmErr == nil {
			orphans++
		}
	}
	return orphans, bytesFreed, nil
}

// BackoffFor returns the wait duration for attempt N (0-indexed).
// Exponential base 30s, cap 5min, +/-20% jitter.
func BackoffFor(attempts int, rng *rand.Rand) time.Duration {
	if attempts < 0 {
		attempts = 0
	}
	base := 30 * time.Second
	maxWait := 5 * time.Minute
	d := base << attempts
	if d <= 0 || d > maxWait {
		d = maxWait
	}
	if rng != nil {
		jitter := (rng.Float64()*0.4 - 0.2) * float64(d)
		d = time.Duration(float64(d) + jitter)
	}
	if d < base/2 {
		d = base / 2
	}
	return d
}

// MarshalRequest is a convenience wrapper.
func MarshalRequest(v any) ([]byte, error) {
	return json.Marshal(v)
}

func scanItems(rows *sql.Rows) ([]*Item, error) {
	var out []*Item
	for rows.Next() {
		var (
			it   Item
			eAt  int64
			laAt int64
			kStr string
		)
		if err := rows.Scan(&it.ID, &kStr, &it.SHA, &it.PayloadJSON, &it.StagedPath, &eAt, &it.Attempts, &laAt, &it.LastError); err != nil {
			return nil, fmt.Errorf("wqueue: scan: %w", err)
		}
		it.Kind = Kind(kStr)
		it.EnqueuedAt = time.Unix(0, eAt)
		if laAt > 0 {
			it.LastAttemptAt = time.Unix(0, laAt)
		}
		out = append(out, &it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func validKind(k Kind) bool {
	switch k {
	case KindPerceive, KindLearn, KindAttach, KindTrace, KindTaskPromote:
		return true
	}
	return false
}
