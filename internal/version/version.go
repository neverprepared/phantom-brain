// Package version exposes build metadata for pbrainctl.
//
// Values are stamped at link time via -ldflags:
//
//	go build -ldflags "-X github.com/neverprepared/phantom-brain/internal/version.Version=v5.0.0-dev \
//	                   -X github.com/neverprepared/phantom-brain/internal/version.Commit=$(git rev-parse --short HEAD) \
//	                   -X github.com/neverprepared/phantom-brain/internal/version.BuildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
//	    ./cmd/pbrainctl
package version

var (
	// Version is the semantic version of pbrainctl (e.g. v5.0.0-dev).
	Version = "v5.0.0-dev"

	// Commit is the short git SHA the binary was built from.
	Commit = "unknown"

	// BuildDate is RFC3339 UTC.
	BuildDate = "unknown"
)
