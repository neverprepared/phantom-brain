package vault

import (
	"fmt"
	"os"
	"path/filepath"
)

// WriteAtomicFile writes content to path with the temp+fsync+rename
// pattern that guarantees readers never see a partial file.
//
// Steps:
//  1. Ensure the parent directory exists (MkdirAll with 0755).
//  2. Create a temp file in the SAME directory as the target — same-
//     directory rename is the only one POSIX guarantees atomic.
//  3. Write the bytes.
//  4. fsync the file so the kernel commits the data before the rename
//     publishes it.
//  5. Close the file.
//  6. rename(2) the temp file over the target.
//  7. fsync the parent directory so the rename itself is durable.
//
// On any failure, the temp file is removed so we don't leak.
//
// perm sets the final file mode. 0644 is the typical value; pass 0600
// for secrets.
func WriteAtomicFile(path string, content []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("vault: mkdir %q: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("vault: create temp in %q: %w", dir, err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }

	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("vault: write %q: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("vault: fsync %q: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("vault: close %q: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		cleanup()
		return fmt.Errorf("vault: chmod %q: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("vault: rename %q -> %q: %w", tmpName, path, err)
	}

	// fsync the directory so the rename is durable. Best-effort:
	// failure here means the rename may not survive a crash, but the
	// target file IS already in place — surfacing the error to the
	// caller would mislead.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
