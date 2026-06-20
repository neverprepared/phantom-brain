package sqlite

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenAppliesPRAGMAs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := Open(Options{Path: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	checks := []struct {
		pragma string
		want   string
	}{
		{"journal_mode", "wal"},
		{"synchronous", "1"}, // 1 == NORMAL
		{"foreign_keys", "1"},
	}
	for _, c := range checks {
		var got string
		if err := db.QueryRow("PRAGMA " + c.pragma).Scan(&got); err != nil {
			t.Fatalf("PRAGMA %s: %v", c.pragma, err)
		}
		if got != c.want {
			t.Errorf("PRAGMA %s = %q, want %q", c.pragma, got, c.want)
		}
	}

	// Verify the WAL sidecar files exist after a write.
	MustExec(t, db, `CREATE TABLE t(x INTEGER); INSERT INTO t VALUES (1)`)
	for _, suf := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + suf); err != nil {
			t.Errorf("expected %s sidecar to exist after write: %v", suf, err)
		}
	}
}

func TestOpenRejectsEmptyPath(t *testing.T) {
	if _, err := Open(Options{}); err == nil {
		t.Fatal("Open with empty path should fail")
	}
}

func TestReadOnlySkipsWAL(t *testing.T) {
	// Seed a database, close it, reopen read-only.
	path := filepath.Join(t.TempDir(), "ro.db")
	{
		db, err := Open(Options{Path: path})
		if err != nil {
			t.Fatal(err)
		}
		MustExec(t, db, `CREATE TABLE t(x INTEGER); INSERT INTO t VALUES (42)`)
		db.Close()
	}

	db, err := Open(Options{Path: path, ReadOnly: true})
	if err != nil {
		t.Fatalf("Open ReadOnly: %v", err)
	}
	defer db.Close()

	var x int
	if err := db.QueryRow("SELECT x FROM t").Scan(&x); err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	if x != 42 {
		t.Errorf("x = %d, want 42", x)
	}

	// Writes must fail on a read-only handle.
	if _, err := db.Exec("INSERT INTO t VALUES (99)"); err == nil {
		t.Error("INSERT on read-only DB should fail")
	}
}

// TestSqliteVecLoaded is the canary for the sqlite-vec extension
// being available on every connection. Day-3 ships only the plain
// SQLite wrapper; sqlite-vec loading is its own dep-management
// problem that the internal/index package will solve when it lands
// (see package docstring for the three rejected approaches).
//
// To re-enable: pick a sqlite-vec binding strategy in internal/index,
// confirm the vec0 module is registered on connections returned by
// Open(), then delete this Skip.
func TestSqliteVecLoaded(t *testing.T) {
	t.Skip("sqlite-vec extension loading is internal/index work; see sqlite.go package doc")
}

func TestBackupProducesConsistentSingleFileCopy(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.db")
	dstPath := filepath.Join(dir, "dst.db")

	src, err := Open(Options{Path: srcPath})
	if err != nil {
		t.Fatal(err)
	}
	MustExec(t, src, `CREATE TABLE notes(id INTEGER PRIMARY KEY, body TEXT)`)
	MustExec(t, src, `INSERT INTO notes(body) VALUES ('alpha'), ('bravo'), ('charlie')`)
	if err := src.Close(); err != nil {
		t.Fatal(err)
	}

	if err := Backup(context.Background(), srcPath, dstPath); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	// The destination must be a clean single-file copy: NO -wal or -shm
	// sidecars left over from the backup. (db.backup checkpoints the
	// WAL into the main file as part of completion.)
	for _, suf := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(dstPath + suf); err == nil {
			t.Errorf("dst should have no %s sidecar after backup, but it exists", suf)
		}
	}

	// Read the destination back and confirm content.
	dst, err := Open(Options{Path: dstPath, ReadOnly: true})
	if err != nil {
		t.Fatalf("Open dst ro: %v", err)
	}
	defer dst.Close()

	var count int
	if err := dst.QueryRow("SELECT COUNT(*) FROM notes").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Errorf("dst row count = %d, want 3", count)
	}

	var second string
	if err := dst.QueryRow("SELECT body FROM notes WHERE id = 2").Scan(&second); err != nil {
		t.Fatal(err)
	}
	if second != "bravo" {
		t.Errorf("notes[2].body = %q, want %q", second, "bravo")
	}
}

func TestBackupPreservesVecData(t *testing.T) {
	t.Skip("sqlite-vec extension loading is internal/index work; see sqlite.go package doc")
}
