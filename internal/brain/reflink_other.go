//go:build !darwin && !linux

package brain

// reflinkFileOS on unsupported platforms always returns
// ErrReflinkUnsupported so ReflinkOrCopyFile transparently falls back
// to a stream copy. Windows / freebsd / etc.
func reflinkFileOS(src, dst string) error { return ErrReflinkUnsupported }
