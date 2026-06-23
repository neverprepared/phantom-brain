// Package working owns the per-process working memory database.
//
// Each MCP server process opens its own wm-<PID>.sqlite file under
// the brain's _index/ directory. Working memory is intentionally
// ephemeral: it captures in-flight task state (goals, plans, findings,
// artifacts, open questions) for the duration of a single Claude Code
// session. Important findings get promoted to Raw/curated/ on
// task_complete; everything else dies with the process. See v5.0 §2
// invariant #5.
//
// Because wm-<PID>.sqlite is per-process, multiple Claude Code
// sessions on the same host (one brain per session, one process per
// session) each get an isolated DB. Crashed processes leave their
// shards behind; ReapOrphanedShards walks the _index/ dir and deletes
// shards whose PID is no longer alive.
package working

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	pbsqlite "github.com/neverprepared/phantom-brain/internal/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS tasks (
    task_id      TEXT PRIMARY KEY,
    goal         TEXT NOT NULL,
    constraints  TEXT,
    plan         TEXT,
    current_step TEXT,
    status       TEXT NOT NULL DEFAULT 'active',
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS steps (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id      TEXT NOT NULL REFERENCES tasks(task_id),
    description  TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'pending',
    completed_at TEXT
);

CREATE TABLE IF NOT EXISTS findings (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id     TEXT NOT NULL REFERENCES tasks(task_id),
    content     TEXT NOT NULL,
    importance  TEXT NOT NULL DEFAULT 'medium',
    memory_type TEXT,
    created_at  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS artifacts (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id    TEXT NOT NULL REFERENCES tasks(task_id),
    name       TEXT NOT NULL,
    reference  TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS questions (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id    TEXT NOT NULL REFERENCES tasks(task_id),
    question   TEXT NOT NULL,
    resolved   INTEGER NOT NULL DEFAULT 0,
    resolution TEXT
);

CREATE INDEX IF NOT EXISTS idx_steps_task     ON steps(task_id);
CREATE INDEX IF NOT EXISTS idx_findings_task  ON findings(task_id);
CREATE INDEX IF NOT EXISTS idx_artifacts_task ON artifacts(task_id);
CREATE INDEX IF NOT EXISTS idx_questions_task ON questions(task_id);
`

// DB wraps the per-process working memory connection. Safe for use
// within one process; concurrent use across goroutines is fine because
// internal/sqlite caps the pool at one connection (single-writer model).
type DB struct {
	sql  *sql.DB
	path string
	pid  int
}

// Open creates (or reuses) the wm-<PID>.sqlite file under indexDir and
// applies the schema. indexDir is typically <brainDir>/_index/.
//
// Returns an error if indexDir doesn't exist or isn't writable; callers
// should ensure the brain skeleton was created first.
func Open(indexDir string) (*DB, error) {
	if indexDir == "" {
		return nil, fmt.Errorf("working: Open: indexDir is required")
	}
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		return nil, fmt.Errorf("working: mkdir %q: %w", indexDir, err)
	}

	pid := os.Getpid()
	path := filepath.Join(indexDir, fmt.Sprintf("wm-%d.sqlite", pid))

	conn, err := pbsqlite.Open(pbsqlite.Options{Path: path})
	if err != nil {
		return nil, fmt.Errorf("working: open db: %w", err)
	}
	if _, err := conn.Exec(schema); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("working: apply schema: %w", err)
	}
	return &DB{sql: conn, path: path, pid: pid}, nil
}

// Path returns the on-disk file path. Useful for tests and for the
// orphan reaper to identify a shard.
func (d *DB) Path() string { return d.path }

// PID returns the process ID this shard is bound to.
func (d *DB) PID() int { return d.pid }

// Close releases the underlying connection. Does NOT delete the
// on-disk file — that's the orphan reaper's job (on next brain
// startup) or the operator's via task_complete cleanup.
func (d *DB) Close() error {
	return d.sql.Close()
}

// Delete closes the connection AND removes the on-disk file. Use this
// when the operator explicitly drops a session — for example, when
// task_complete runs successfully and the working state has been
// promoted to Raw/curated/.
func (d *DB) Delete() error {
	if err := d.sql.Close(); err != nil {
		return fmt.Errorf("working: close before delete: %w", err)
	}
	// Remove the sqlite file and its WAL sidecars. Best-effort on
	// sidecars: if WAL was checkpointed already, they won't exist.
	if err := os.Remove(d.path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("working: remove %q: %w", d.path, err)
	}
	for _, suf := range []string{"-wal", "-shm"} {
		_ = os.Remove(d.path + suf)
	}
	return nil
}

// nowRFC3339 is the timestamp format we store in TEXT columns. Matches
// the v4.x TS encoding so a brain re-opened across the cut-over reads
// the same timestamps as it would have on the old runtime.
func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

// ctxBackground is a tiny helper so call sites don't have to pass
// context for trivial single-conn queries; we always run against one
// connection inside one process. If we ever need cancellation we'll
// thread ctx through the API explicitly.
func ctxBackground() context.Context { return context.Background() }
