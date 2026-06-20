// Package sqlite is the single owner of every SQLite handle in pbrainctl.
//
// Every database in the system — vectors.db (per-brain), wm-<PID>.sqlite
// (per-process working memory), merges.sqlite (per-vault ledger on the
// daemon) — opens through Open() so the connection options stay
// consistent: WAL mode, NORMAL synchronous, foreign keys, a non-trivial
// busy timeout.
//
// Driver: mattn/go-sqlite3 (CGO). The classic CGO bindings against the
// system libsqlite. Fast, mature, well-understood.
//
// # sqlite-vec extension: not loaded in this package
//
// vectors.db needs the sqlite-vec extension for the vec0 virtual table.
// Loading sqlite-vec across all platforms is its own non-trivial
// dependency problem:
//
//   - asg017/sqlite-vec-go-bindings/cgo uses sqlite3_auto_extension,
//     which Apple stubbed out in macOS 10.10. Silently broken on macOS.
//   - asg017/sqlite-vec-go-bindings/ncruces requires a specific (older)
//     ncruces/go-sqlite3 version whose WASM SQLite uses wasm features
//     the asg017-bundled SQLite WASM does not export. Version conflict.
//   - Installing sqlite-vec.dylib system-wide and using LoadExtension
//     defeats the single-binary distribution goal.
//
// Rather than wedge that decision into this Day-3 wrapper, we ship the
// plain Open()/Backup() helpers now and handle sqlite-vec loading in
// the package that actually needs it (internal/index, when it lands).
// Callers that need the vec0 module will get an explicit error at
// CREATE VIRTUAL TABLE time, not a silent miss.
package sqlite

import (
	"database/sql"
	"fmt"

	_ "github.com/mattn/go-sqlite3" // sql driver
)

// Options configures a database connection.
type Options struct {
	// Path is the filesystem path to the database. Use ":memory:" only
	// for unit tests that don't exercise WAL semantics.
	Path string

	// ReadOnly opens the database in read-only mode. Useful for opening
	// a published snapshot's vectors.db from a brain at birth time.
	ReadOnly bool

	// BusyTimeoutMs is the SQLite busy timeout in milliseconds. Default
	// is 5000 (5 seconds), which is generous for our usage patterns but
	// short enough to surface real deadlocks during testing.
	BusyTimeoutMs int
}

// Open returns a *sql.DB configured for phantom-brain's invariants.
//
// PRAGMAs applied to every connection (via DSN options):
//
//   - journal_mode = WAL  (concurrent reader + single writer)
//   - synchronous  = NORMAL  (durable across crashes, faster than FULL)
//   - foreign_keys = ON  (enforced; cheap)
//   - busy_timeout = configurable  (default 5000ms)
//
// Read-only connections use mode=ro + immutable=1 so SQLite skips
// -shm/-wal coordination entirely. Safe for snapshot reads where the
// source file is provably stable (we copied it with db.backup before
// opening).
func Open(opts Options) (*sql.DB, error) {
	if opts.Path == "" {
		return nil, fmt.Errorf("sqlite.Open: Path is required")
	}
	busy := opts.BusyTimeoutMs
	if busy == 0 {
		busy = 5000
	}

	var dsn string
	if opts.ReadOnly {
		dsn = fmt.Sprintf("file:%s?mode=ro&immutable=1&_busy_timeout=%d",
			opts.Path, busy)
	} else {
		dsn = fmt.Sprintf("file:%s?_journal_mode=WAL&_synchronous=NORMAL&_foreign_keys=ON&_busy_timeout=%d",
			opts.Path, busy)
	}

	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite.Open %q: %w", opts.Path, err)
	}

	// Single-writer single-process is our model. database/sql's
	// connection pool spawning multiple writer connections would only
	// trip the busy timeout. One connection per *sql.DB matches
	// SQLite's threading semantics cleanly.
	db.SetMaxOpenConns(1)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite.Open %q: ping: %w", opts.Path, err)
	}

	return db, nil
}
