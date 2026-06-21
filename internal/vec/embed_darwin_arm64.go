//go:build darwin && arm64

package vec

import _ "embed"

//go:embed dylibs/sqlite-vec-darwin-arm64.dylib
var darwinArm64Bytes []byte

func init() {
	dylibBytes = darwinArm64Bytes
	dylibExt = ".dylib"
}
