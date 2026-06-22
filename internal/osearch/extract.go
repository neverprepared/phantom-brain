package osearch

import (
	"archive/tar"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// ExtractTarZst unpacks a .tar.zst built by WriteTarZst into dest,
// creating subdirectories as needed. Refuses entries whose paths
// would escape dest (path traversal guard). Pure decompress + file
// writes; no symlinks, no executable bits, no preservation of
// uid/gid.
func ExtractTarZst(tarPath, dest string) error {
	f, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()

	zr, err := zstd.NewReader(f)
	if err != nil {
		return err
	}
	defer zr.Close()

	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		// Path traversal guard: reject anything that doesn't resolve
		// cleanly under dest.
		target := filepath.Join(dest, hdr.Name)
		clean := filepath.Clean(target)
		destAbs, _ := filepath.Abs(dest)
		cleanAbs, _ := filepath.Abs(clean)
		if !strings.HasPrefix(cleanAbs, destAbs+string(os.PathSeparator)) && cleanAbs != destAbs {
			return errors.New("osearch.ExtractTarZst: path traversal blocked: " + hdr.Name)
		}
		if hdr.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		out, err := os.Create(target)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, tr); err != nil {
			out.Close()
			return err
		}
		out.Close()
	}
}
