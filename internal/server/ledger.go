package server

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	pbsqlite "github.com/neverprepared/phantom-brain/internal/sqlite"
)

// Ledger wraps the per-vault merges.sqlite database that records
// every death payload the reaper ingested into the collective. The
// rows are append-only — once written, never updated — so the file
// doubles as an audit log operators can grep / SQL against.
//
// Schema (v1):
//
//	CREATE TABLE merges (
//	  brain_id       TEXT PRIMARY KEY,
//	  contributor_id TEXT NOT NULL,
//	  profile        TEXT NOT NULL,
//	  vault          TEXT NOT NULL,
//	  merged_at      TEXT NOT NULL,   -- RFC3339
//	  raw_count      INTEGER NOT NULL,
//	  attachment_count INTEGER NOT NULL,
//	  payload_bytes  INTEGER NOT NULL
//	);
//	CREATE INDEX merges_by_contributor ON merges(contributor_id);
//	CREATE INDEX merges_by_merged_at   ON merges(merged_at);
//
// Open is idempotent (CREATE IF NOT EXISTS), so a fresh vault starts
// with an empty file and successive opens see the existing data.
type Ledger struct {
	db   *sql.DB
	path string
}

// MergeRecord is one row in merges.sqlite. Returned by Get/List and
// passed to Insert.
type MergeRecord struct {
	BrainID         string
	ContributorID   string
	Profile         string
	Vault           string
	MergedAt        time.Time
	RawCount        int
	AttachmentCount int
	PayloadBytes    int64
}

// OpenLedger opens (or creates) {dataDir}/{profile}/{vault}/collective
// /ledger/merges.sqlite in WAL mode and ensures the schema. Tests
// pass a tempdir-rooted DataDir; production uses DefaultDataDir().
//
// Returns the open *Ledger; caller must Close() during shutdown to
// flush WAL.
func OpenLedger(dataDir DataDir, profile, vault string) (*Ledger, error) {
	if profile == "" || vault == "" {
		return nil, errors.New("server: OpenLedger requires profile and vault")
	}
	if err := os.MkdirAll(dataDir.LedgerDir(profile, vault), 0o755); err != nil {
		return nil, fmt.Errorf("server: mkdir ledger dir: %w", err)
	}
	path := filepath.Join(dataDir.LedgerDir(profile, vault), "merges.sqlite")
	db, err := pbsqlite.Open(pbsqlite.Options{Path: path})
	if err != nil {
		return nil, fmt.Errorf("server: open ledger %s: %w", path, err)
	}
	if err := initLedgerSchema(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Ledger{db: db, path: path}, nil
}

// initLedgerSchema applies the CREATE statements idempotently. Schema
// migrations beyond v1 land here; for now the table is fixed.
func initLedgerSchema(db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS merges (
			brain_id         TEXT PRIMARY KEY,
			contributor_id   TEXT NOT NULL,
			profile          TEXT NOT NULL,
			vault            TEXT NOT NULL,
			merged_at        TEXT NOT NULL,
			raw_count        INTEGER NOT NULL,
			attachment_count INTEGER NOT NULL,
			payload_bytes    INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS merges_by_contributor ON merges(contributor_id)`,
		`CREATE INDEX IF NOT EXISTS merges_by_merged_at   ON merges(merged_at)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("server: ledger schema: %w", err)
		}
	}
	return nil
}

// Insert records a merge. brain_id is the primary key; re-merging a
// brain (which shouldn't happen — death is one-shot) returns
// ErrDuplicate so the reaper can quarantine the offending tarball.
func (l *Ledger) Insert(r MergeRecord) error {
	if r.BrainID == "" || r.Profile == "" || r.Vault == "" {
		return errors.New("server: ledger Insert requires brain_id/profile/vault")
	}
	if r.MergedAt.IsZero() {
		r.MergedAt = time.Now().UTC()
	}
	_, err := l.db.Exec(`INSERT INTO merges
		(brain_id, contributor_id, profile, vault, merged_at, raw_count, attachment_count, payload_bytes)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		r.BrainID, r.ContributorID, r.Profile, r.Vault,
		r.MergedAt.UTC().Format(time.RFC3339),
		r.RawCount, r.AttachmentCount, r.PayloadBytes,
	)
	if err != nil {
		if isSQLitePrimaryKeyConflict(err) {
			return ErrDuplicateMerge
		}
		return fmt.Errorf("server: ledger insert: %w", err)
	}
	return nil
}

// ErrDuplicateMerge is returned by Insert when the brain_id is
// already in the table. Sentinel so callers can errors.Is it.
var ErrDuplicateMerge = errors.New("server: brain_id already in ledger")

// isSQLitePrimaryKeyConflict pattern-matches on the mattn driver's
// error string. We don't import the driver's typed error here to
// avoid coupling internal/server to mattn's API surface; the string
// match is stable across mattn versions.
func isSQLitePrimaryKeyConflict(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "UNIQUE constraint failed") || strings.Contains(s, "PRIMARY KEY")
}

// Get returns the MergeRecord for a brain_id. Returns sql.ErrNoRows
// (wrapped) when the brain hasn't been merged.
func (l *Ledger) Get(brainID string) (*MergeRecord, error) {
	row := l.db.QueryRow(`SELECT brain_id, contributor_id, profile, vault, merged_at,
		raw_count, attachment_count, payload_bytes
		FROM merges WHERE brain_id = ?`, brainID)
	var r MergeRecord
	var mergedAt string
	if err := row.Scan(&r.BrainID, &r.ContributorID, &r.Profile, &r.Vault, &mergedAt,
		&r.RawCount, &r.AttachmentCount, &r.PayloadBytes); err != nil {
		return nil, err
	}
	t, err := time.Parse(time.RFC3339, mergedAt)
	if err != nil {
		return nil, fmt.Errorf("server: ledger parse merged_at %q: %w", mergedAt, err)
	}
	r.MergedAt = t
	return &r, nil
}

// List returns merge records ordered by merged_at descending (newest
// first), capped at limit. Used by ops tooling and brain_status.
func (l *Ledger) List(limit int) ([]MergeRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := l.db.Query(`SELECT brain_id, contributor_id, profile, vault, merged_at,
		raw_count, attachment_count, payload_bytes
		FROM merges ORDER BY merged_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("server: ledger list: %w", err)
	}
	defer rows.Close()
	var out []MergeRecord
	for rows.Next() {
		var r MergeRecord
		var mergedAt string
		if err := rows.Scan(&r.BrainID, &r.ContributorID, &r.Profile, &r.Vault, &mergedAt,
			&r.RawCount, &r.AttachmentCount, &r.PayloadBytes); err != nil {
			return nil, err
		}
		if t, err := time.Parse(time.RFC3339, mergedAt); err == nil {
			r.MergedAt = t
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Close flushes WAL and closes the underlying DB. Idempotent on the
// sql.DB.Close side; safe to call from defer in shutdown.
func (l *Ledger) Close() error {
	if l.db == nil {
		return nil
	}
	return l.db.Close()
}
