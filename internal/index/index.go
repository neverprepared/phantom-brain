// Package index owns vectors.db — the per-brain SQLite database that
// holds the embedding vectors (vec0 virtual table) and the full-text
// search index (FTS5 virtual table) for the brain's Wiki/ pages.
//
// One DB, two indexes. Co-located so a single connection can run both
// halves of a hybrid query and RRF-fuse the rankings without two
// network roundtrips. This matches the v4.x TypeScript layout, so a
// vectors.db born under the old runtime opens cleanly under the new one.
//
// # Schema
//
//	vec_map(rowid PK, sha TEXT UNIQUE, source_path TEXT, updated_at TEXT,
//	        title TEXT, kind TEXT, tags TEXT)
//	    -- bookkeeping: maps content SHA to its source file and emit time
//	    -- so sync can skip already-embedded content in O(1). title/kind/
//	    -- tags are denormalised here so renderRecallHits can describe a
//	    -- hit without a second hop into fts_memories.
//
//	vec_embeddings USING vec0(embedding float[<dims>])
//	    -- the actual vectors. vec_embeddings.rowid == vec_map.rowid.
//
//	fts_memories USING fts5(sha UNINDEXED, title, tags, body,
//	                        tokenize='porter unicode61')
//	    -- BM25-ranked full-text search over the Wiki page.
//
// # Embedder dependency injection
//
// This package does NOT import internal/ollama. It accepts an
// Embedder interface so tests can use deterministic fake embeddings
// and so the production code can swap embedding backends later
// without touching the index layer.
package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	pbsqlite "github.com/neverprepared/mcp-phantom-brain/internal/sqlite"
)

// Embedder is the minimal interface index needs from any embedding
// backend. internal/ollama.Client satisfies it.
type Embedder interface {
	Embed(ctx context.Context, inputs []string) ([][]float32, error)
	Dims() int
}

// Index wraps the open vectors.db.
type Index struct {
	sql  *sql.DB
	path string
	dims int
}

// Open creates (or reuses) vectors.db at indexDir/vectors.db with the
// schema below applied. dims pins the vector dimensionality at first
// CREATE; reopening with a different dims returns an error rather
// than silently truncating or corrupting.
func Open(indexDir string, dims int) (*Index, error) {
	if indexDir == "" {
		return nil, fmt.Errorf("index: Open: indexDir is required")
	}
	if dims <= 0 {
		return nil, fmt.Errorf("index: Open: dims must be positive, got %d", dims)
	}
	path := filepath.Join(indexDir, "vectors.db")

	conn, err := pbsqlite.Open(pbsqlite.Options{Path: path})
	if err != nil {
		return nil, fmt.Errorf("index: open db: %w", err)
	}

	if err := applySchema(conn, dims); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("index: schema: %w", err)
	}

	return &Index{sql: conn, path: path, dims: dims}, nil
}

// Path returns the on-disk database path.
func (i *Index) Path() string { return i.path }

// Dims returns the vector dimensionality the index was opened with.
func (i *Index) Dims() int { return i.dims }

// Close releases the underlying connection.
func (i *Index) Close() error { return i.sql.Close() }

func applySchema(db *sql.DB, dims int) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS vec_map (
		    rowid       INTEGER PRIMARY KEY AUTOINCREMENT,
		    sha         TEXT NOT NULL UNIQUE,
		    source_path TEXT NOT NULL,
		    updated_at  TEXT NOT NULL,
		    title       TEXT NOT NULL DEFAULT '',
		    kind        TEXT NOT NULL DEFAULT '',
		    tags        TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_vec_map_source ON vec_map(source_path)`,
		// Backfill columns when reopening a snapshot built by an older
		// daemon that wrote only the original four columns. SQLite has
		// no IF NOT EXISTS for ADD COLUMN; the duplicate-column error is
		// swallowed in applySchema below.
		`ALTER TABLE vec_map ADD COLUMN title TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE vec_map ADD COLUMN kind  TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE vec_map ADD COLUMN tags  TEXT NOT NULL DEFAULT ''`,
		fmt.Sprintf(`CREATE VIRTUAL TABLE IF NOT EXISTS vec_embeddings USING vec0(
		    embedding float[%d]
		)`, dims),
		`CREATE VIRTUAL TABLE IF NOT EXISTS fts_memories USING fts5(
		    sha UNINDEXED,
		    title,
		    tags,
		    body,
		    tokenize='porter unicode61'
		)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			if isDuplicateColumnErr(err) {
				continue
			}
			return fmt.Errorf("apply schema: %w", err)
		}
	}
	return nil
}

// isDuplicateColumnErr matches SQLite's "duplicate column name" error
// raised when an ALTER TABLE ADD COLUMN runs against a table that
// already has the column. Treated as a no-op so schema application is
// idempotent across snapshot generations.
func isDuplicateColumnErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "duplicate column name")
}

// ErrDimMismatch is returned when Upsert receives an embedding whose
// length disagrees with the index's configured dims.
var ErrDimMismatch = errors.New("index: embedding dimension mismatch")
