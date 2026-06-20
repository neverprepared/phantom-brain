// Package sqlite is the single owner of every SQLite handle in pbrainctl.
//
// Every database in the system — vectors.db (per-brain), wm-<PID>.sqlite
// (per-process working memory), merges.sqlite (per-vault ledger on the
// daemon) — opens through Open() so the connection options stay
// consistent: WAL mode, NORMAL synchronous, foreign keys, a non-trivial
// busy timeout.
//
// Driver: mattn/go-sqlite3 (CGO) + a sqlite-vec loadable extension
// registered per-connection. internal/vec embeds the sqlite-vec binary
// for the host platform, extracts it to a temp file at process start,
// and registers a custom database/sql driver name ("sqlite3_vec")
// whose ConnectHook calls LoadExtension on every new conn. We open
// against that driver name instead of the default "sqlite3", so every
// *sql.DB this package returns already has the vec0 virtual-table
// module available.
//
// History of failed approaches (kept here so the next session doesn't
// re-litigate): see the internal/vec package doc. Three prior attempts
// (asg017 cgo Auto, asg017 ncruces, system-installed dylib) all failed
// on macOS for different reasons. The per-conn LoadExtension path is
// the one that actually works.
package sqlite

import (
	"database/sql"
	"fmt"

	_ "github.com/mattn/go-sqlite3" // base driver — internal/vec registers a vec-enabled variant

	"github.com/neverprepared/mcp-phantom-brain/internal/vec"
)

func init() {
	// Fail-loud at package init if the vec driver can't be registered
	// — for example because we're running on a GOOS/GOARCH for which
	// no sqlite-vec binary is vendored yet. The alternative (lazy fail
	// at first Open) hides the configuration problem behind whatever
	// other code happens to run first.
	if err := vec.Init(); err != nil {
		panic(fmt.Sprintf("sqlite: vec driver init: %v", err))
	}
}

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

	db, err := sql.Open(vec.DriverName, dsn)
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
