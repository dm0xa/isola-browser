// Package version provides build-time version information via ldflags.
// Values are injected at build time using:
//
//	go build -ldflags "-X github.com/dm0xa/isola/backend/pkg/version.Version=x.y.z \
//	                  -X github.com/dm0xa/isola/backend/pkg/version.Commit=$(git rev-parse --short HEAD) \
//	                  -X github.com/dm0xa/isola/backend/pkg/version.BuildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
package version

import "fmt"

// Build-time variables (set via ldflags). Do not edit directly.
var (
	// Version is the semantic version of the current build (e.g. "1.0.0").
	Version = "1.1.0"
	// Commit is the git commit hash at build time.
	Commit = "unknown"
	// BuildTime is the UTC timestamp when the binary was built.
	BuildTime = "unknown"
)

// String returns a human-readable version string.
func String() string {
	return fmt.Sprintf("%s commit %s, built %s", Version, Commit, BuildTime)
}
