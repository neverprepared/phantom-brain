package server

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// SafeTarLimits caps a single extract to defend against zip-bomb-
// style payloads. Both limits come from the per-vault MergedDefaults
// in production; tests pass smaller values to exercise the cap.
type SafeTarLimits struct {
	// MaxUncompressedBytes is the upper bound on the total bytes
	// written across all extracted files. Headers + names don't count
	// (they're tiny); the regular-file payloads do.
	MaxUncompressedBytes int64

	// MaxFiles caps the number of extracted entries. Optional — pass 0
	// to disable the count cap and rely on bytes alone. Used to defend
	// against a tarball full of zero-byte entries that wouldn't trip
	// the byte cap but would still exhaust inodes.
	MaxFiles int
}

// SafeTarError discriminates the failure modes operators care about:
// path traversal, symlink escape, size cap, file count cap, generic
// IO. Reaper quarantine logic switches on these to decide whether to
// log + skip vs treat as a corrupt tarball.
type SafeTarError struct {
	Kind  SafeTarErrorKind
	Entry string
	err   error
}

// SafeTarErrorKind enumerates the safetar failure categories.
type SafeTarErrorKind int

const (
	SafeTarErrTraversal SafeTarErrorKind = iota + 1
	SafeTarErrSymlinkEscape
	SafeTarErrSizeCap
	SafeTarErrFileCap
	SafeTarErrIO
	SafeTarErrUnsupportedEntry
)

func (e *SafeTarError) Error() string {
	return fmt.Sprintf("safetar: %s at %q: %v", kindName(e.Kind), e.Entry, e.err)
}
func (e *SafeTarError) Unwrap() error { return e.err }

func kindName(k SafeTarErrorKind) string {
	switch k {
	case SafeTarErrTraversal:
		return "path traversal"
	case SafeTarErrSymlinkEscape:
		return "symlink escape"
	case SafeTarErrSizeCap:
		return "uncompressed size cap exceeded"
	case SafeTarErrFileCap:
		return "file count cap exceeded"
	case SafeTarErrUnsupportedEntry:
		return "unsupported entry type"
	default:
		return "io"
	}
}

// IsSafeTarErrorKind reports whether err is a SafeTarError of the
// given kind. Reaper code uses this to decide between "log + retry"
// (IO) and "log + quarantine" (traversal, escape, caps).
func IsSafeTarErrorKind(err error, k SafeTarErrorKind) bool {
	var ste *SafeTarError
	if !errors.As(err, &ste) {
		return false
	}
	return ste.Kind == k
}

// SafeExtract reads a tar archive from src and writes its contents
// under destRoot. Defenses applied:
//
//   - Path traversal: every entry name is cleaned and rejected if it
//     escapes destRoot (filepath.Rel(destRoot, target) starting with "..").
//   - Absolute paths: rejected outright.
//   - Symlinks: the link target is similarly checked; a link that
//     would point outside destRoot is rejected.
//   - Hardlinks: rejected (we don't need them; safer to refuse).
//   - Devices / FIFOs / sockets: rejected (no use case in our payloads).
//   - Size cap: each Copy is bounded by io.LimitReader so a corrupt
//     payload claiming a huge size can't allocate.
//
// destRoot must already exist; SafeExtract does not MkdirAll it.
// Parent directories of entries are created as needed.
func SafeExtract(src io.Reader, destRoot string, limits SafeTarLimits) error {
	if destRoot == "" {
		return errors.New("safetar: empty destRoot")
	}
	absRoot, err := filepath.Abs(destRoot)
	if err != nil {
		return fmt.Errorf("safetar: abs destRoot: %w", err)
	}

	tr := tar.NewReader(src)
	var written int64
	var fileCount int
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return &SafeTarError{Kind: SafeTarErrIO, err: err}
		}

		target, terr := safeJoin(absRoot, hdr.Name)
		if terr != nil {
			return terr
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return &SafeTarError{Kind: SafeTarErrIO, Entry: hdr.Name, err: err}
			}
		case tar.TypeReg, tar.TypeRegA:
			fileCount++
			if limits.MaxFiles > 0 && fileCount > limits.MaxFiles {
				return &SafeTarError{Kind: SafeTarErrFileCap, Entry: hdr.Name,
					err: fmt.Errorf("max=%d", limits.MaxFiles)}
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return &SafeTarError{Kind: SafeTarErrIO, Entry: hdr.Name, err: err}
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return &SafeTarError{Kind: SafeTarErrIO, Entry: hdr.Name, err: err}
			}
			// Cap the per-file read at remaining-budget+1 so we can
			// detect overrun: if Copy reads exactly remaining+1 we
			// know the next byte would have busted the cap.
			remaining := limits.MaxUncompressedBytes - written
			if limits.MaxUncompressedBytes <= 0 {
				remaining = 1<<62 - 1 // effectively unlimited
			}
			n, copyErr := io.Copy(f, io.LimitReader(tr, remaining+1))
			_ = f.Close()
			if copyErr != nil {
				return &SafeTarError{Kind: SafeTarErrIO, Entry: hdr.Name, err: copyErr}
			}
			if limits.MaxUncompressedBytes > 0 && n > remaining {
				_ = os.Remove(target)
				return &SafeTarError{Kind: SafeTarErrSizeCap, Entry: hdr.Name,
					err: fmt.Errorf("would exceed %d bytes", limits.MaxUncompressedBytes)}
			}
			written += n
		case tar.TypeSymlink:
			// Resolve linkname relative to the target file's dir,
			// then ensure the result is still under absRoot.
			linkAbs, err := safeJoin(filepath.Dir(target), hdr.Linkname)
			if err != nil {
				return &SafeTarError{Kind: SafeTarErrSymlinkEscape, Entry: hdr.Name,
					err: fmt.Errorf("linkname=%q", hdr.Linkname)}
			}
			if !isUnder(absRoot, linkAbs) {
				return &SafeTarError{Kind: SafeTarErrSymlinkEscape, Entry: hdr.Name,
					err: fmt.Errorf("resolved=%q", linkAbs)}
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return &SafeTarError{Kind: SafeTarErrIO, Entry: hdr.Name, err: err}
			}
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return &SafeTarError{Kind: SafeTarErrIO, Entry: hdr.Name, err: err}
			}
		default:
			return &SafeTarError{Kind: SafeTarErrUnsupportedEntry, Entry: hdr.Name,
				err: fmt.Errorf("typeflag=%d", hdr.Typeflag)}
		}
	}
}

// safeJoin cleans name, joins it with root, and ensures the result is
// still under root. Returns a SafeTarError of kind Traversal on absolute
// paths or escapes; returns the joined abs path on success.
func safeJoin(root, name string) (string, error) {
	if strings.HasPrefix(name, "/") {
		return "", &SafeTarError{Kind: SafeTarErrTraversal, Entry: name,
			err: errors.New("absolute path rejected")}
	}
	cleaned := filepath.Clean(name)
	if strings.HasPrefix(cleaned, "..") {
		return "", &SafeTarError{Kind: SafeTarErrTraversal, Entry: name,
			err: errors.New("relative path escapes root")}
	}
	full := filepath.Join(root, cleaned)
	abs, err := filepath.Abs(full)
	if err != nil {
		return "", &SafeTarError{Kind: SafeTarErrTraversal, Entry: name, err: err}
	}
	if !isUnder(root, abs) {
		return "", &SafeTarError{Kind: SafeTarErrTraversal, Entry: name,
			err: fmt.Errorf("resolved=%q", abs)}
	}
	return abs, nil
}

// isUnder reports whether child is the same as or nested inside root.
// Both inputs are expected to be absolute + clean.
func isUnder(root, child string) bool {
	root = filepath.Clean(root)
	child = filepath.Clean(child)
	if root == child {
		return true
	}
	rel, err := filepath.Rel(root, child)
	if err != nil {
		return false
	}
	return !strings.HasPrefix(rel, "..")
}
