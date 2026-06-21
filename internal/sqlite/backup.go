package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/mattn/go-sqlite3"
)

// Backup performs a SQLite online backup from srcPath to dstPath.
//
// Online backup (https://www.sqlite.org/backup.html) is the only safe
// way to clone an actively-written WAL-mode database. It copies pages
// without blocking writers; the result is a consistent point-in-time
// snapshot with NO sidecar -wal or -shm file — a single-file database
// that can be opened with ReadOnly + immutable=1, copied across hosts,
// or reflinked into a brain at birth.
//
// dstPath is created if it does not exist. If it exists, its contents
// are replaced.
//
// We step the backup with pagesPerStep=-1 (copy all pages in one
// step). Every database we back up is small enough that chunked
// copying buys nothing. Switch to a paged loop if vectors.db starts
// pushing past a few GB.
func Backup(ctx context.Context, srcPath, dstPath string) error {
	src, err := Open(Options{Path: srcPath, ReadOnly: true})
	if err != nil {
		return fmt.Errorf("backup: open src: %w", err)
	}
	defer src.Close()

	dst, err := Open(Options{Path: dstPath})
	if err != nil {
		return fmt.Errorf("backup: open dst: %w", err)
	}
	defer dst.Close()

	srcConn, err := src.Conn(ctx)
	if err != nil {
		return fmt.Errorf("backup: get src conn: %w", err)
	}
	defer srcConn.Close()

	dstConn, err := dst.Conn(ctx)
	if err != nil {
		return fmt.Errorf("backup: get dst conn: %w", err)
	}
	defer dstConn.Close()

	return srcConn.Raw(func(srcRaw any) error {
		return dstConn.Raw(func(dstRaw any) error {
			srcSQLite, ok := srcRaw.(*sqlite3.SQLiteConn)
			if !ok {
				return errors.New("backup: src is not a *sqlite3.SQLiteConn")
			}
			dstSQLite, ok := dstRaw.(*sqlite3.SQLiteConn)
			if !ok {
				return errors.New("backup: dst is not a *sqlite3.SQLiteConn")
			}

			b, err := dstSQLite.Backup("main", srcSQLite, "main")
			if err != nil {
				return fmt.Errorf("backup: init: %w", err)
			}

			done, err := b.Step(-1)
			if err != nil {
				_ = b.Finish()
				return fmt.Errorf("backup: step: %w", err)
			}
			if err := b.Finish(); err != nil {
				return fmt.Errorf("backup: finish: %w", err)
			}
			if !done {
				return errors.New("backup: step returned not-done with pagesPerStep=-1")
			}
			return nil
		})
	})
}

// MustExec runs a single statement and panics on error. Useful in test
// setup; do NOT use in production code paths.
func MustExec(t interface{ Fatalf(string, ...any) }, db *sql.DB, query string, args ...any) {
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("MustExec %q: %v", query, err)
	}
}
