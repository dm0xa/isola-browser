// Package version provides build-time version information via ldflags.
// The version comes from git tags: the release workflow uses the pushed `v*`
// tag (without the leading "v"), and scripted/Docker builds use
// `git describe --tags`. The defaults below are only a fallback for a bare
// `go build`; do not bump them per release.
//
//	go build -ldflags "-X github.com/dm0xa/isola-browser/backend/pkg/version.Version=x.y.z \
//	                  -X github.com/dm0xa/isola-browser/backend/pkg/version.Commit=$(git rev-parse --short HEAD) \
//	                  -X github.com/dm0xa/isola-browser/backend/pkg/version.BuildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
package version

import "fmt"

// Build-time variables (set via ldflags). Do not edit directly.
var (
	// Version is the version of the current build, without a leading "v".
	Version = "dev"
	// Commit is the git commit hash at build time.
	Commit = "unknown"
	// BuildTime is the UTC timestamp when the binary was built.
	BuildTime = "unknown"
)

// String returns a human-readable version string.
func String() string {
	return fmt.Sprintf("%s commit %s, built %s", Version, Commit, BuildTime)
}
