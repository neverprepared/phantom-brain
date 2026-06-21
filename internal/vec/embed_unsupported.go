//go:build !(darwin && arm64) && !(linux && amd64) && !(linux && arm64)

// This file populates the package's embed slot with nil bytes on
// platforms we haven't vendored a sqlite-vec binary for yet. Init()
// returns ErrUnsupportedPlatform with a pointer to internal/vec/dylibs/.
//
// To add a platform:
//  1. Download the matching loadable from
//     https://github.com/asg017/sqlite-vec/releases.
//  2. Drop the file in internal/vec/dylibs/ as
//     sqlite-vec-<GOOS>-<GOARCH>.<ext>.
//  3. Add a build-tag-gated embed_<GOOS>_<GOARCH>.go that mirrors
//     embed_darwin_arm64.go.
//  4. Update the build tag in this file's // !(...) clause to exclude
//     the new platform combination.

package vec
