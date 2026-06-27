package vec

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// deterministicDylibPath recomputes the per-process temp path that
// extractDylib derives from the embedded bytes, so a test can probe or
// delete the on-disk file without reaching into unexported helpers.
func deterministicDylibPath(t *testing.T) string {
	t.Helper()
	if len(dylibBytes) == 0 {
		t.Skip("no vendored dylib on this platform; extractDylib write paths are unreachable")
	}
	sum := sha256.Sum256(dylibBytes)
	name := fmt.Sprintf("pbrainctl-vec-%s%s", hex.EncodeToString(sum[:8]), dylibExt)
	return filepath.Join(os.TempDir(), name)
}

// TestExtractDylibWritesFreshWhenMissing forces the CreateTemp → Write →
// Chmod → Rename branch by deleting the deterministic target first. The
// happy fast-path (stat hit) skips all of that, which is why the cold
// write path was previously uncovered.
func TestExtractDylibWritesFreshWhenMissing(t *testing.T) {
	path := deterministicDylibPath(t)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		t.Fatalf("pre-clean target: %v", err)
	}

	got, err := extractDylib()
	if err != nil {
		t.Fatalf("extractDylib: %v", err)
	}
	if got != path {
		t.Fatalf("extractDylib returned %q, want deterministic path %q", got, path)
	}

	// The freshly written file must byte-for-byte equal the embedded
	// dylib — a truncated or scrambled write would make LoadExtension
	// fail at first Open.
	onDisk, err := os.ReadFile(got)
	if err != nil {
		t.Fatalf("read written dylib: %v", err)
	}
	if !bytes.Equal(onDisk, dylibBytes) {
		t.Fatalf("written dylib (%d bytes) != embedded bytes (%d bytes)", len(onDisk), len(dylibBytes))
	}

	st, err := os.Stat(got)
	if err != nil {
		t.Fatalf("stat written dylib: %v", err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Fatalf("written dylib perm = %o, want 0600", perm)
	}
}

// TestExtractDylibRewritesOnSizeMismatch verifies the stat-hit-but-wrong-size
// guard: if a stale/truncated file sits at the target path, extractDylib
// must NOT trust it and must rewrite full content. A size check that
// trusted any existing file would leave a corrupt extension on disk.
func TestExtractDylibRewritesOnSizeMismatch(t *testing.T) {
	path := deterministicDylibPath(t)

	// Plant a wrong-size file (1 byte) where the real dylib belongs.
	if err := os.WriteFile(path, []byte{0x00}, 0o600); err != nil {
		t.Fatalf("plant truncated file: %v", err)
	}

	got, err := extractDylib()
	if err != nil {
		t.Fatalf("extractDylib: %v", err)
	}
	if got != path {
		t.Fatalf("extractDylib returned %q, want %q", got, path)
	}

	onDisk, err := os.ReadFile(got)
	if err != nil {
		t.Fatalf("read rewritten dylib: %v", err)
	}
	if len(onDisk) != len(dylibBytes) {
		t.Fatalf("rewritten dylib size %d != embedded %d (size-mismatch guard did not rewrite)", len(onDisk), len(dylibBytes))
	}
	if !bytes.Equal(onDisk, dylibBytes) {
		t.Fatalf("rewritten dylib content differs from embedded bytes")
	}
}

// TestExtractDylibRenameError exercises the write-then-rename failure
// cleanup path. Planting a non-empty directory at the deterministic
// target makes os.Stat succeed (so the fast path is skipped on a size
// mismatch), the fresh write succeed, and the final os.Rename(tmp, dir)
// fail — driving the error return + temp-file cleanup branch. We then
// restore a good file so later tests' fast path still works.
func TestExtractDylibRenameError(t *testing.T) {
	path := deterministicDylibPath(t)

	// Remove any existing regular file, then plant a non-empty dir.
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		t.Fatalf("pre-clean: %v", err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("plant dir at target: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "occupant"), []byte("x"), 0o600); err != nil {
		t.Fatalf("populate dir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(path)
		// Re-materialize a valid dylib so sibling tests / packages that
		// open the vec driver after this one still find a good file.
		_, _ = extractDylib()
	})

	tmpBefore, _ := filepath.Glob(filepath.Join(os.TempDir(), "pbrainctl-vec-*.tmp"))

	_, err := extractDylib()
	if err == nil {
		t.Fatal("extractDylib succeeded renaming onto a directory; want error")
	}

	// The failed write must not leak its O_EXCL temp file: cleanup runs
	// os.Remove on the temp before returning the error.
	tmpAfter, _ := filepath.Glob(filepath.Join(os.TempDir(), "pbrainctl-vec-*.tmp"))
	if len(tmpAfter) > len(tmpBefore) {
		t.Errorf("rename-error path leaked a temp file: before=%v after=%v", tmpBefore, tmpAfter)
	}
}

// TestExtractDylibFastPathReusesGoodFile verifies the cheap branch: when a
// correctly-sized file already exists, extractDylib returns it without a
// rewrite. We prove "no rewrite" by stamping the file with an old mtime
// and asserting it is preserved across the call.
func TestExtractDylibFastPathReusesGoodFile(t *testing.T) {
	path := deterministicDylibPath(t)

	// Ensure a correct file is present first.
	if _, err := extractDylib(); err != nil {
		t.Fatalf("seed extractDylib: %v", err)
	}

	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat seeded file: %v", err)
	}

	got, err := extractDylib()
	if err != nil {
		t.Fatalf("extractDylib (fast path): %v", err)
	}
	if got != path {
		t.Fatalf("extractDylib returned %q, want %q", got, path)
	}

	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after fast path: %v", err)
	}
	// ModTime equality is the observable signal that the file was NOT
	// rewritten (a rewrite goes through CreateTemp+Rename → new mtime).
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("fast path rewrote file: mtime changed %v -> %v", before.ModTime(), after.ModTime())
	}
}
