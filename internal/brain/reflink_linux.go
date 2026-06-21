//go:build linux

package brain

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// reflinkFileOS uses the FICLONE ioctl to produce a CoW clone of src
// at dst on btrfs, XFS (with reflink=1), and modern bcachefs. Returns
// ErrReflinkUnsupported when the underlying FS doesn't support it
// (ext4, tmpfs, NFS, …) — those map to EOPNOTSUPP / EINVAL / EXDEV.
//
// The dst path must not already exist; we open it O_CREATE|O_EXCL
// before the ioctl so concurrent reflink calls can't clobber each
// other's destinations.
func reflinkFileOS(src, dst string) error {
	srcF, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcF.Close()
	st, err := srcF.Stat()
	if err != nil {
		return err
	}
	dstF, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, st.Mode().Perm())
	if err != nil {
		return err
	}
	// IoctlFileClone takes (dst_fd, src_fd) — note the order.
	if err := unix.IoctlFileClone(int(dstF.Fd()), int(srcF.Fd())); err != nil {
		_ = dstF.Close()
		_ = os.Remove(dst)
		if errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EXDEV) || errors.Is(err, unix.EINVAL) {
			return ErrReflinkUnsupported
		}
		return err
	}
	return dstF.Close()
}
