//go:build linux && amd64

package vec

import _ "embed"

//go:embed dylibs/sqlite-vec-linux-amd64.so
var linuxAmd64Bytes []byte

func init() {
	dylibBytes = linuxAmd64Bytes
	dylibExt = ".so"
}
