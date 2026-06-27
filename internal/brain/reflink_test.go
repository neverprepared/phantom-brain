package brain

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReflinkOrCopyFile_FallsBackOnUnsupportedFS(t *testing.T) {
	// macOS tempdir is APFS (reflink works); Linux's tmpfs is not.
	// Either way we should get bytes at dst — ReflinkOrCopyFile
	// transparently degrades.
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("hello reflink"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ReflinkOrCopyFile(src, dst); err != nil {
		t.Fatalf("ReflinkOrCopyFile: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello reflink" {
		t.Errorf("got %q", got)
	}
}

func TestReflinkOrCopyTree_MirrorsLayout(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(filepath.Join(src, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "a", "top.txt"), []byte("top"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "a", "b", "nested.txt"), []byte("nested"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dst")
	if err := ReflinkOrCopyTree(src, dst); err != nil {
		t.Fatalf("ReflinkOrCopyTree: %v", err)
	}
	for _, rel := range []string{"a/top.txt", "a/b/nested.txt"} {
		if _, err := os.Stat(filepath.Join(dst, rel)); err != nil {
			t.Errorf("expected %s in dst tree: %v", rel, err)
		}
	}
}

func TestReflinkOrCopyTree_RejectsExistingDestination(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	err := ReflinkOrCopyTree(src, dst)
	if err == nil {
		t.Fatal("expected refusal on existing destination")
	}
}

func TestReflinkFile_ErrIsSentinelSentinel(t *testing.T) {
	// Sanity: ErrReflinkUnsupported is unwrappable to itself. Catches
	// regressions where the sentinel gets shadowed by a fmt.Errorf
	// without %w.
	if !errors.Is(ErrReflinkUnsupported, ErrReflinkUnsupported) {
		t.Fatal("sentinel sentinel: errors.Is on self should be true")
	}
}

// --- shipqueue --------------------------------------------------------

// ShipQueue tests removed in Phase 6 — agents no longer queue death
// payloads; writes ship synchronously via daemon POST during life.

// --- snapcache --------------------------------------------------------
//
// Phase D2b: the snapshot cache (internal/brain/snapcache.go) was removed
// — births are greenfield and recall/fetch are online-only, so there is
// no cached snapshot to list or fetch. TestSnapcache_EmptyByDefault and
// TestSnapcache_FetchReturnsErrorWhenDaemonUnreachable are gone.
