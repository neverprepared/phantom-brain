package sqlite

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// serializeF32 mirrors what asg017's SerializeFloat32 does — turn a
// []float32 into the little-endian byte blob sqlite-vec expects in a
// vec0 BLOB column. Inlined here so the test has no dep on the binding
// package whose history is documented in internal/sqlite/sqlite.go.
func serializeF32(v []float32) []byte {
	buf := new(bytes.Buffer)
	_ = binary.Write(buf, binary.LittleEndian, v)
	return buf.Bytes()
}

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
// being available on every connection. Day-6a wires this via
// internal/vec's per-connection LoadExtension hook.
func TestSqliteVecLoaded(t *testing.T) {
	db, err := Open(Options{Path: filepath.Join(t.TempDir(), "vec.db")})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	MustExec(t, db, `CREATE VIRTUAL TABLE vec_test USING vec0(embedding float[3])`)

	v100 := serializeF32([]float32{1, 0, 0})
	v010 := serializeF32([]float32{0, 1, 0})
	v001 := serializeF32([]float32{0, 0, 1})

	MustExec(t, db, `INSERT INTO vec_test(rowid, embedding) VALUES (1, ?), (2, ?), (3, ?)`, v100, v010, v001)

	var rowid int
	var distance float64
	err = db.QueryRow(
		`SELECT rowid, distance FROM vec_test WHERE embedding MATCH ? ORDER BY distance LIMIT 1`,
		v100,
	).Scan(&rowid, &distance)
	if err != nil {
		t.Fatalf("nearest-neighbor query: %v", err)
	}
	if rowid != 1 {
		t.Errorf("nearest neighbor rowid = %d, want 1", rowid)
	}
	if distance > 1e-6 {
		t.Errorf("self-match distance = %v, want ~0", distance)
	}
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

// TestBackupPreservesVecData is the real-world test: snapshotting a
// vectors.db with sqlite-vec data must preserve the vec0 virtual
// table and its blob storage intact. This is what the Phase 2
// snapshot builder relies on.
func TestBackupPreservesVecData(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "vec-src.db")
	dstPath := filepath.Join(dir, "vec-dst.db")

	src, err := Open(Options{Path: srcPath})
	if err != nil {
		t.Fatal(err)
	}
	MustExec(t, src, `CREATE VIRTUAL TABLE v USING vec0(embedding float[3])`)
	v := serializeF32([]float32{0.5, 0.5, 0.5})
	MustExec(t, src, `INSERT INTO v(rowid, embedding) VALUES (7, ?)`, v)
	src.Close()

	if err := Backup(context.Background(), srcPath, dstPath); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	dst, err := Open(Options{Path: dstPath, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()

	var rowid int
	var distance float64
	err = dst.QueryRow(
		`SELECT rowid, distance FROM v WHERE embedding MATCH ? ORDER BY distance LIMIT 1`,
		v,
	).Scan(&rowid, &distance)
	if err != nil {
		t.Fatalf("post-backup vec query: %v", err)
	}
	if rowid != 7 {
		t.Errorf("rowid = %d, want 7", rowid)
	}
}
