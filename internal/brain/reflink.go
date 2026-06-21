package brain

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ErrReflinkUnsupported is returned by ReflinkFile when the underlying
// filesystem (or GOOS) doesn't support copy-on-write cloning. Callers
// should treat this as a soft failure and fall back to a real copy
// — that's what ReflinkOrCopyFile and ReflinkOrCopyTree do.
var ErrReflinkUnsupported = errors.New("brain: reflink not supported on this filesystem")

// ReflinkFile clones src to dst using a copy-on-write filesystem
// primitive (clonefile on macOS, FICLONE ioctl on Linux). On
// filesystems without CoW support it returns ErrReflinkUnsupported so
// callers can decide whether to fall back to a real copy.
//
// The destination must NOT already exist — both clonefile and FICLONE
// require an empty destination. Callers ensure the parent dir exists
// first; this function does not MkdirAll.
//
// Phase 1 ships the primitive but does not yet wire it into the
// checkpoint flow (the Phase 1 marker is sufficient). The daemon
// snapshot publisher in Phase 2 is the heavy user.
func ReflinkFile(src, dst string) error {
	return reflinkFileOS(src, dst)
}

// ReflinkOrCopyFile attempts a CoW clone, then transparently falls
// back to a stream copy on ErrReflinkUnsupported. Use this when
// correctness matters more than disk efficiency (which is most
// callers — the wins from CoW are nice-to-have, not required).
func ReflinkOrCopyFile(src, dst string) error {
	err := ReflinkFile(src, dst)
	if err == nil {
		return nil
	}
	if !errors.Is(err, ErrReflinkUnsupported) {
		return err
	}
	return copyFile(src, dst)
}

// ReflinkOrCopyTree walks src and reflinks (or copies) every regular
// file into dst, mirroring the directory structure. Symlinks are
// recreated; non-regular non-directory entries (sockets, devices) are
// skipped with no error. dst must not already exist — guard against
// accidental overwrites.
func ReflinkOrCopyTree(src, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("brain: ReflinkOrCopyTree refuses to overwrite existing %s", dst)
	}
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, path)
		if rerr != nil {
			return rerr
		}
		out := filepath.Join(dst, rel)
		switch {
		case info.IsDir():
			return os.MkdirAll(out, info.Mode().Perm())
		case info.Mode()&os.ModeSymlink != 0:
			target, lerr := os.Readlink(path)
			if lerr != nil {
				return lerr
			}
			return os.Symlink(target, out)
		case info.Mode().IsRegular():
			if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
				return err
			}
			return ReflinkOrCopyFile(path, out)
		default:
			return nil
		}
	})
}

// copyFile is the stream-copy fallback. fsync at the end so the copy
// is durable enough to satisfy the same crash-safety story callers
// expect from CoW clones.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, st.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
