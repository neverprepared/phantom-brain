package index

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

// Record is one Wiki page's worth of data to insert into the index.
type Record struct {
	// SHA is the canonical content hash. Stable identity for this page.
	SHA string

	// SourcePath is the on-disk path relative to the vault root, used
	// for "what file did this come from?" queries and resync after a
	// rename.
	SourcePath string

	// Title is the page title (typically from frontmatter).
	Title string

	// Tags is a single space-separated token blob. FTS5 splits on
	// whitespace via the unicode61 tokenizer; we don't need to encode
	// tag boundaries any more carefully.
	Tags string

	// Body is the markdown body without the frontmatter block.
	Body string

	// Kind is the SummaryDoc kind ("note", "web_scrape",
	// "attachment_stub", "task_summary", ...). Stored on vec_map so
	// recall renders can label hits without a daemon round-trip.
	// Empty string is tolerated for legacy records.
	Kind string

	// Embedding is the dense vector. Length must equal Index.Dims().
	Embedding []float32
}

// Has reports whether sha is already indexed. Sync uses this to skip
// expensive re-embed work for content that hasn't changed.
func (i *Index) Has(sha string) (bool, error) {
	if sha == "" {
		return false, nil
	}
	var one int
	err := i.sql.QueryRow(`SELECT 1 FROM vec_map WHERE sha = ?`, sha).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("index: Has: %w", err)
	}
	return true, nil
}

// Upsert writes (or replaces) a record across all three tables in a
// single transaction.
//
// Replacement strategy: if a row with the same SHA already exists, the
// existing rowid is reused so cross-table references stay consistent.
// vec_embeddings and fts_memories rows for that rowid are deleted and
// re-inserted with the new data; vec_map's updated_at is bumped.
//
// Empty SHA, empty SourcePath, or mismatched embedding length all fail
// the call without partial writes.
func (i *Index) Upsert(ctx context.Context, r Record) error {
	if r.SHA == "" {
		return fmt.Errorf("index: Upsert: SHA is required")
	}
	if r.SourcePath == "" {
		return fmt.Errorf("index: Upsert: SourcePath is required")
	}
	if len(r.Embedding) != i.dims {
		return fmt.Errorf("%w: got %d, want %d", ErrDimMismatch, len(r.Embedding), i.dims)
	}

	tx, err := i.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("index: Upsert: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is a documented no-op

	// Find existing rowid (if any) so the replacement reuses it.
	var (
		rowid    int64
		existing bool
	)
	row := tx.QueryRowContext(ctx, `SELECT rowid FROM vec_map WHERE sha = ?`, r.SHA)
	switch err := row.Scan(&rowid); {
	case errors.Is(err, sql.ErrNoRows):
		existing = false
	case err != nil:
		return fmt.Errorf("index: Upsert: lookup: %w", err)
	default:
		existing = true
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)

	if existing {
		if _, err := tx.ExecContext(ctx,
			`UPDATE vec_map SET source_path = ?, updated_at = ?, title = ?, kind = ?, tags = ? WHERE rowid = ?`,
			r.SourcePath, now, r.Title, r.Kind, r.Tags, rowid,
		); err != nil {
			return fmt.Errorf("index: Upsert: vec_map update: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM vec_embeddings WHERE rowid = ?`, rowid); err != nil {
			return fmt.Errorf("index: Upsert: vec_embeddings clear: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM fts_memories WHERE sha = ?`, r.SHA); err != nil {
			return fmt.Errorf("index: Upsert: fts clear: %w", err)
		}
	} else {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO vec_map(sha, source_path, updated_at, title, kind, tags) VALUES (?, ?, ?, ?, ?, ?)`,
			r.SHA, r.SourcePath, now, r.Title, r.Kind, r.Tags,
		)
		if err != nil {
			return fmt.Errorf("index: Upsert: vec_map insert: %w", err)
		}
		rowid, err = res.LastInsertId()
		if err != nil {
			return fmt.Errorf("index: Upsert: vec_map rowid: %w", err)
		}
	}

	blob, err := serializeF32(r.Embedding)
	if err != nil {
		return fmt.Errorf("index: Upsert: serialize embedding: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO vec_embeddings(rowid, embedding) VALUES (?, ?)`,
		rowid, blob,
	); err != nil {
		return fmt.Errorf("index: Upsert: vec_embeddings insert: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO fts_memories(sha, title, tags, body) VALUES (?, ?, ?, ?)`,
		r.SHA, r.Title, r.Tags, r.Body,
	); err != nil {
		return fmt.Errorf("index: Upsert: fts insert: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("index: Upsert: commit: %w", err)
	}
	return nil
}

// Delete removes every trace of sha from the index. Used by sync when
// it observes a Wiki page that disappeared from disk.
func (i *Index) Delete(ctx context.Context, sha string) error {
	if sha == "" {
		return nil
	}
	tx, err := i.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("index: Delete: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	var rowid int64
	row := tx.QueryRowContext(ctx, `SELECT rowid FROM vec_map WHERE sha = ?`, sha)
	switch err := row.Scan(&rowid); {
	case errors.Is(err, sql.ErrNoRows):
		return nil // already gone — idempotent
	case err != nil:
		return fmt.Errorf("index: Delete: lookup: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM vec_embeddings WHERE rowid = ?`, rowid); err != nil {
		return fmt.Errorf("index: Delete: vec_embeddings: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM fts_memories WHERE sha = ?`, sha); err != nil {
		return fmt.Errorf("index: Delete: fts: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM vec_map WHERE rowid = ?`, rowid); err != nil {
		return fmt.Errorf("index: Delete: vec_map: %w", err)
	}
	return tx.Commit()
}

// AllSHAs returns every SHA currently indexed. Used by sync to compute
// the set of pages that exist in the index but no longer on disk.
func (i *Index) AllSHAs() (map[string]string, error) {
	rows, err := i.sql.Query(`SELECT sha, source_path FROM vec_map`)
	if err != nil {
		return nil, fmt.Errorf("index: AllSHAs: %w", err)
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var sha, path string
		if err := rows.Scan(&sha, &path); err != nil {
			return nil, fmt.Errorf("index: AllSHAs: scan: %w", err)
		}
		out[sha] = path
	}
	return out, rows.Err()
}

// serializeF32 packs a []float32 into the little-endian blob that
// sqlite-vec expects. Inlined here (same logic as internal/sqlite test
// helper) so the index package has no test-only dep paths.
func serializeF32(v []float32) ([]byte, error) {
	buf := new(bytes.Buffer)
	if err := binary.Write(buf, binary.LittleEndian, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
