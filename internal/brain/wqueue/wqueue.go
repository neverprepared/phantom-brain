// Package wqueue is the agent-side write-ahead queue for daemon-bound
// operations (perceive/learn/attach/trace). Writes that fail to reach
// the daemon are persisted here and retried by the drainer goroutine
// in internal/brain.
//
// Queued writes are deliberately invisible to brain_recall: they live
// only in this SQLite file and the staging directory until the daemon
// round-trips them and the next snapshot ships. This is a locked
// design decision — do not project queued items into vectors.db.
//
// One queue per (profile, vault) binding; the on-disk layout is:
//
//	<dir>/wqueue.sqlite          — the rows
//	<dir>/wqueue-attach/<sha><ext> — staged attachment blobs
//
// Open takes an absolute path (typically agent.VaultBaseDir()), so the
// package has no dependency on internal/config.
package wqueue

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/neverprepared/phantom-brain/internal/sqlite"
)

// Kind enumerates the queueable write operations. The daemon dedups by
// SHA, so duplicate POSTs from the immediate-attempt + drainer paths
// are safe.
type Kind string

const (
	KindPerceive    Kind = "perceive"
	KindLearn       Kind = "learn"
	KindAttach      Kind = "attach"
	KindTrace       Kind = "trace"
	KindTaskPromote Kind = "task_promote" // semantically Learn; tagged separately for ops visibility
)

func (k Kind) valid() bool {
	switch k {
	case KindPerceive, KindLearn, KindAttach, KindTrace, KindTaskPromote:
		return true
	}
	return false
}

// MaxStagedBytes caps a single attachment's staged blob. Mirrors the
// daemon's attach ceiling so the queue can't be used to bypass it.
const MaxStagedBytes int64 = 100 * 1024 * 1024

// Item is one row in the queue.
type Item struct {
	ID            int64
	Kind          Kind
	SHA           string
	PayloadJSON   []byte
	StagedPath    string // non-empty only for KindAttach
	EnqueuedAt    time.Time
	Attempts      int
	LastAttemptAt time.Time // zero == never attempted
	LastError     string
	// Dead marks a dead-lettered row: a permanent (non-retryable)
	// dispatch failure, or one that exhausted MaxAttempts. Dead rows are
	// excluded from NextEligible (no more retries) but retained for
	// operator inspection until cleared.
	Dead       bool
	DeadReason string
}

// MaxAttempts caps how many times a TRANSIENT dispatch failure is
// retried before the row is dead-lettered. Permanent failures (HTTP
// 4xx, JSON unmarshal, unknown/invalid kind) are dead-lettered
// immediately regardless of this cap.
const MaxAttempts = 20

// EnqueueOpts is the caller-side request. For KindAttach, Bytes is the
// raw blob (copied into the staging directory before the row inserts);
// PayloadJSON must NOT carry the base64-encoded bytes — the drainer
// re-reads from disk and re-encodes on retry.
type EnqueueOpts struct {
	Kind        Kind
	SHA         string
	PayloadJSON []byte
	Bytes       []byte // KindAttach only
	Ext         string // KindAttach only; include leading dot, may be ""
}

// Queue is the durable write-ahead store. Safe for concurrent goroutine
// use; safe across multiple processes via SQLite WAL + busy_timeout.
type Queue struct {
	dir       string
	attachDir string
	db        *sql.DB
}

// Sentinel errors.
var (
	ErrOversize    = errors.New("wqueue: staged bytes exceed MaxStagedBytes")
	ErrInvalidKind = errors.New("wqueue: invalid Kind")
	ErrEmptySHA    = errors.New("wqueue: SHA required")
	// ErrNotExist is returned by OpenReadOnly when no wqueue.sqlite is
	// present yet (i.e. the agent has never had offline writes for this
	// binding). Lets ops tooling print a friendly "no queue" message
	// without creating side-effect files just to look.
	ErrNotExist = errors.New("wqueue: queue does not exist")
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
  claimed_at INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  dead INTEGER NOT NULL DEFAULT 0,
  dead_reason TEXT NOT NULL DEFAULT '',
  UNIQUE(kind, sha)
);
CREATE INDEX IF NOT EXISTS wqueue_eligible ON wqueue(last_attempt_at, id);
`

// migrateSchema applies schemaSQL (create-if-not-exists for fresh DBs)
// then idempotently ALTERs in the v3.x dead-letter columns for any
// pre-existing wqueue.sqlite that predates them. A "duplicate column"
// error means the column already exists (already-migrated DB) and is
// benign.
func migrateSchema(db *sql.DB) error {
	if _, err := db.Exec(schemaSQL); err != nil {
		return fmt.Errorf("wqueue: schema: %w", err)
	}
	for _, stmt := range []string{
		`ALTER TABLE wqueue ADD COLUMN dead INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE wqueue ADD COLUMN dead_reason TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := db.Exec(stmt); err != nil && !isDuplicateColumn(err) {
			return fmt.Errorf("wqueue: migrate: %w", err)
		}
	}
	return nil
}

func isDuplicateColumn(err error) bool {
	return err != nil && strings.Contains(err.Error(), "duplicate column name")
}

// Open opens (or creates) the queue under dir. dir must be an absolute
// path. Creates dir, dir/wqueue-attach, and dir/wqueue.sqlite if absent.
func Open(dir string) (*Queue, error) {
	if dir == "" {
		return nil, errors.New("wqueue: Open requires a directory")
	}
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("wqueue: Open requires an absolute path, got %q", dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("wqueue: mkdir %s: %w", dir, err)
	}
	attachDir := filepath.Join(dir, "wqueue-attach")
	if err := os.MkdirAll(attachDir, 0o755); err != nil {
		return nil, fmt.Errorf("wqueue: mkdir %s: %w", attachDir, err)
	}
	db, err := sqlite.Open(sqlite.Options{Path: filepath.Join(dir, "wqueue.sqlite")})
	if err != nil {
		return nil, fmt.Errorf("wqueue: open db: %w", err)
	}
	if err := migrateSchema(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Queue{dir: dir, attachDir: attachDir, db: db}, nil
}

// OpenReadOnly opens the queue without creating any side-effect files
// when none exist. Returns ErrNotExist when dir/wqueue.sqlite is
// missing. Used by `pbrainctl client queue list` so an inspection
// command never materialises a queue just by looking.
func OpenReadOnly(dir string) (*Queue, error) {
	if dir == "" {
		return nil, errors.New("wqueue: OpenReadOnly requires a directory")
	}
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("wqueue: OpenReadOnly requires an absolute path, got %q", dir)
	}
	dbPath := filepath.Join(dir, "wqueue.sqlite")
	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotExist
		}
		return nil, err
	}
	db, err := sqlite.Open(sqlite.Options{Path: dbPath})
	if err != nil {
		return nil, fmt.Errorf("wqueue: open db: %w", err)
	}
	if err := migrateSchema(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Queue{dir: dir, attachDir: filepath.Join(dir, "wqueue-attach"), db: db}, nil
}

// Close releases the database handle. The staging directory is left in
// place — call Clear first if the caller wants a clean slate.
func (q *Queue) Close() error {
	if q == nil || q.db == nil {
		return nil
	}
	return q.db.Close()
}

// Dir returns the queue's root directory. Useful for ops tooling.
func (q *Queue) Dir() string { return q.dir }

// StageDir returns the staging directory holding attachment bytes.
func (q *Queue) StageDir() string { return q.attachDir }

// Enqueue persists one item. For KindAttach the bytes are copied to
// the staging directory BEFORE the row inserts, so a crash mid-Enqueue
// leaves an orphan file (recoverable via Cleanup) rather than a row
// pointing at a missing file (unrecoverable).
func (q *Queue) Enqueue(ctx context.Context, opts EnqueueOpts) (*Item, error) {
	if !opts.Kind.valid() {
		return nil, ErrInvalidKind
	}
	if opts.SHA == "" {
		return nil, ErrEmptySHA
	}
	var staged string
	if opts.Kind == KindAttach {
		if int64(len(opts.Bytes)) > MaxStagedBytes {
			return nil, ErrOversize
		}
		staged = filepath.Join(q.attachDir, opts.SHA+opts.Ext)
		if err := os.WriteFile(staged, opts.Bytes, 0o600); err != nil {
			return nil, fmt.Errorf("wqueue: stage attachment: %w", err)
		}
	}
	now := time.Now()
	payload := opts.PayloadJSON
	if payload == nil {
		payload = []byte{}
	}
	res, err := q.db.ExecContext(ctx,
		`INSERT INTO wqueue(kind, sha, payload_json, staged_path, enqueued_at) VALUES(?, ?, ?, ?, ?)`,
		string(opts.Kind), opts.SHA, payload, staged, now.UnixNano())
	if err != nil {
		// UNIQUE(kind, sha) collision: an earlier Enqueue (this process
		// or a sibling MCP child) already staged the same write. Return
		// the existing row — daemon dedups by SHA anyway, so the second
		// caller's "Queued" UX is honest.
		if isUniqueViolation(err) {
			if staged != "" {
				_ = os.Remove(staged) // duplicate blob; first staging wins
			}
			existing, lerr := q.lookupByKindSHA(ctx, opts.Kind, opts.SHA)
			if lerr == nil && existing != nil {
				return existing, nil
			}
		}
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

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "UNIQUE constraint failed") || strings.Contains(s, "constraint failed: UNIQUE")
}

func (q *Queue) lookupByKindSHA(ctx context.Context, kind Kind, sha string) (*Item, error) {
	row := q.db.QueryRowContext(ctx,
		`SELECT id, kind, sha, payload_json, staged_path, enqueued_at, attempts, last_attempt_at, last_error, dead, dead_reason
		 FROM wqueue WHERE kind = ? AND sha = ?`, string(kind), sha)
	return scanItem(row)
}

// Depth returns the current row count.
func (q *Queue) Depth(ctx context.Context) (int, error) {
	var n int
	if err := q.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM wqueue`).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// ClaimTTL bounds how long a claim is honoured. A drainer that crashes
// mid-dispatch leaves a row with claimed_at != 0; after ClaimTTL the
// next drainer treats the row as unclaimed and can pick it up.
const ClaimTTL = 5 * time.Minute

// NextEligible returns up to limit rows whose backoff window has
// elapsed AND that are not currently claimed by another drainer. Each
// returned row is atomically claimed (claimed_at = now) inside a single
// BEGIN IMMEDIATE transaction so concurrent drainers can't pick the
// same row. Rows returned oldest-first (id ASC).
func (q *Queue) NextEligible(ctx context.Context, now time.Time, limit int) ([]*Item, error) {
	if limit <= 0 {
		limit = 16
	}
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck
	rows, err := tx.QueryContext(ctx,
		`SELECT id, kind, sha, payload_json, staged_path, enqueued_at, attempts, last_attempt_at, last_error, dead, dead_reason, claimed_at
		 FROM wqueue WHERE dead = 0 ORDER BY id ASC LIMIT ?`, limit*4)
	if err != nil {
		return nil, err
	}
	out := make([]*Item, 0, limit)
	ids := make([]int64, 0, limit)
	staleClaim := now.Add(-ClaimTTL).UnixNano()
	for rows.Next() {
		it, claimedAt, err := scanItemWithClaim(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		if !eligible(it, now) {
			continue
		}
		if claimedAt != 0 && claimedAt >= staleClaim {
			continue
		}
		out = append(out, it)
		ids = append(ids, it.ID)
		if len(out) >= limit {
			break
		}
	}
	rows.Close()
	if rerr := rows.Err(); rerr != nil {
		return nil, rerr
	}
	for _, id := range ids {
		if _, uerr := tx.ExecContext(ctx,
			`UPDATE wqueue SET claimed_at = ? WHERE id = ?`,
			now.UnixNano(), id); uerr != nil {
			return nil, uerr
		}
	}
	if cerr := tx.Commit(); cerr != nil {
		return nil, cerr
	}
	return out, nil
}

// ReleaseClaim drops a stale claim without touching attempt state.
// Used by drainers that decide not to dispatch a claimed row (e.g.
// context cancelled). MarkAttempt and Delete also implicitly release
// the claim.
func (q *Queue) ReleaseClaim(ctx context.Context, id int64) error {
	_, err := q.db.ExecContext(ctx, `UPDATE wqueue SET claimed_at = 0 WHERE id = ?`, id)
	return err
}

func eligible(it *Item, now time.Time) bool {
	if it.LastAttemptAt.IsZero() {
		return true
	}
	// Eligibility uses the unjittered base so tests can advance the
	// clock deterministically. Jitter shows up only in BackoffFor for
	// callers that want a true wait duration.
	wait := backoffBase(it.Attempts)
	return !now.Before(it.LastAttemptAt.Add(wait))
}

// MarkAttempt records a failed attempt: increments attempts, stamps
// last_attempt_at, stores last_error.
func (q *Queue) MarkAttempt(ctx context.Context, id int64, now time.Time, attemptErr error) error {
	msg := ""
	if attemptErr != nil {
		msg = attemptErr.Error()
	}
	_, err := q.db.ExecContext(ctx,
		`UPDATE wqueue SET attempts = attempts + 1, last_attempt_at = ?, last_error = ?, claimed_at = 0 WHERE id = ?`,
		now.UnixNano(), msg, id)
	return err
}

// MarkDead dead-letters a row: it records a final failed attempt
// (increments attempts, stamps last_attempt_at + last_error) AND sets
// dead = 1 with the supplied reason. Dead rows are excluded from
// NextEligible so they are never retried again, but are retained for
// `pbrainctl client queue list --dead` inspection until cleared.
func (q *Queue) MarkDead(ctx context.Context, id int64, now time.Time, reason string) error {
	_, err := q.db.ExecContext(ctx,
		`UPDATE wqueue SET attempts = attempts + 1, last_attempt_at = ?, last_error = ?, dead = 1, dead_reason = ?, claimed_at = 0 WHERE id = ?`,
		now.UnixNano(), reason, reason, id)
	return err
}

// Delete removes the row and, when applicable, the staged file. Both
// removals are idempotent.
func (q *Queue) Delete(ctx context.Context, id int64) error {
	var staged string
	row := q.db.QueryRowContext(ctx, `SELECT staged_path FROM wqueue WHERE id = ?`, id)
	if err := row.Scan(&staged); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := q.db.ExecContext(ctx, `DELETE FROM wqueue WHERE id = ?`, id); err != nil {
		return err
	}
	if staged != "" {
		if err := os.Remove(staged); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("wqueue: remove staged %s: %w", staged, err)
		}
	}
	return nil
}

// List returns rows newest-first. limit <= 0 means no cap. Includes both
// live and dead-lettered rows (dead ones carry Dead=true + DeadReason).
func (q *Queue) List(ctx context.Context, limit int) ([]*Item, error) {
	return q.listWhere(ctx, "", limit)
}

// ListDead returns only dead-lettered rows, newest-first. limit <= 0
// means no cap. Used by `pbrainctl client queue list --dead`.
func (q *Queue) ListDead(ctx context.Context, limit int) ([]*Item, error) {
	return q.listWhere(ctx, "WHERE dead = 1", limit)
}

func (q *Queue) listWhere(ctx context.Context, where string, limit int) ([]*Item, error) {
	q1 := `SELECT id, kind, sha, payload_json, staged_path, enqueued_at, attempts, last_attempt_at, last_error, dead, dead_reason
	       FROM wqueue `
	if where != "" {
		q1 += where + " "
	}
	q1 += `ORDER BY id DESC`
	var rows *sql.Rows
	var err error
	if limit > 0 {
		rows, err = q.db.QueryContext(ctx, q1+` LIMIT ?`, limit)
	} else {
		rows, err = q.db.QueryContext(ctx, q1)
	}
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

// Clear deletes every row and every staged file. Returns count removed.
func (q *Queue) Clear(ctx context.Context) (int, error) {
	n, err := q.Depth(ctx)
	if err != nil {
		return 0, err
	}
	if _, err := q.db.ExecContext(ctx, `DELETE FROM wqueue`); err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(q.attachDir)
	if err != nil {
		return n, nil
	}
	for _, e := range entries {
		_ = os.Remove(filepath.Join(q.attachDir, e.Name()))
	}
	return n, nil
}

// Cleanup sweeps the staging directory for files whose row no longer
// exists (orphans from a crashed Enqueue or external tampering).
// Returns count + bytes freed.
func (q *Queue) Cleanup(ctx context.Context) (int, int64, error) {
	entries, err := os.ReadDir(q.attachDir)
	if err != nil {
		return 0, 0, err
	}
	if len(entries) == 0 {
		return 0, 0, nil
	}
	rows, err := q.db.QueryContext(ctx, `SELECT staged_path FROM wqueue WHERE staged_path <> ''`)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	known := make(map[string]struct{})
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return 0, 0, err
		}
		known[p] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	var orphans int
	var freed int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		full := filepath.Join(q.attachDir, e.Name())
		if _, ok := known[full]; ok {
			continue
		}
		info, err := e.Info()
		if err == nil {
			freed += info.Size()
		}
		if err := os.Remove(full); err == nil {
			orphans++
		}
	}
	return orphans, freed, nil
}

// BackoffFor returns the wait duration for the given attempt count.
// Exponential base 30s, doubling each attempt, capped at 5min, with
// ±20% jitter applied via rng. Exposed for tests and operator tools.
func BackoffFor(attempts int, rng *rand.Rand) time.Duration {
	base := backoffBase(attempts)
	if rng == nil {
		return base
	}
	delta := float64(base) * 0.2
	offset := (rng.Float64()*2 - 1) * delta
	return time.Duration(float64(base) + offset)
}

func backoffBase(attempts int) time.Duration {
	const (
		base = 30 * time.Second
		cap_ = 5 * time.Minute
	)
	if attempts <= 0 {
		return base
	}
	mult := math.Pow(2, float64(attempts))
	dur := time.Duration(float64(base) * mult)
	if dur > cap_ {
		return cap_
	}
	return dur
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanItemWithClaim(s rowScanner) (*Item, int64, error) {
	var (
		it          Item
		kind        string
		enqueued    int64
		lastAttempt int64
		dead        int64
		claimedAt   int64
	)
	if err := s.Scan(&it.ID, &kind, &it.SHA, &it.PayloadJSON, &it.StagedPath,
		&enqueued, &it.Attempts, &lastAttempt, &it.LastError, &dead, &it.DeadReason, &claimedAt); err != nil {
		return nil, 0, err
	}
	it.Kind = Kind(kind)
	it.EnqueuedAt = time.Unix(0, enqueued)
	if lastAttempt != 0 {
		it.LastAttemptAt = time.Unix(0, lastAttempt)
	}
	it.Dead = dead != 0
	return &it, claimedAt, nil
}

func scanItem(s rowScanner) (*Item, error) {
	var (
		it          Item
		kind        string
		enqueued    int64
		lastAttempt int64
		dead        int64
	)
	if err := s.Scan(&it.ID, &kind, &it.SHA, &it.PayloadJSON, &it.StagedPath,
		&enqueued, &it.Attempts, &lastAttempt, &it.LastError, &dead, &it.DeadReason); err != nil {
		return nil, err
	}
	it.Kind = Kind(kind)
	it.EnqueuedAt = time.Unix(0, enqueued)
	if lastAttempt != 0 {
		it.LastAttemptAt = time.Unix(0, lastAttempt)
	}
	it.Dead = dead != 0
	return &it, nil
}
