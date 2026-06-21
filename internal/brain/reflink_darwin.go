//go:build darwin

package brain

import (
	"errors"

	"golang.org/x/sys/unix"
)

// reflinkFileOS uses Apple's clonefile(2) to produce a CoW clone of
// src at dst. Works on APFS (every modern macOS install). Returns
// ErrReflinkUnsupported when the underlying FS doesn't support cloning
// (HFS+, FAT on a thumb drive, etc.) — those map to ENOTSUP.
//
// The dst path must not already exist; clonefile rejects EEXIST.
func reflinkFileOS(src, dst string) error {
	err := unix.Clonefile(src, dst, 0)
	if err == nil {
		return nil
	}
	if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EXDEV) {
		return ErrReflinkUnsupported
	}
	return err
}
