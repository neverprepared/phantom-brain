//go:build linux && arm64

package vec

import _ "embed"

//go:embed dylibs/sqlite-vec-linux-arm64.so
var linuxArm64Bytes []byte

func init() {
	dylibBytes = linuxArm64Bytes
	dylibExt = ".so"
}
