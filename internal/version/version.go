// Package version exposes build metadata injected at link time via
// -ldflags "-X github.com/nidjbs/go-ai-gateway/internal/version.Version=...".
package version

// Variables are overridable at build time; defaults keep local/dev builds
// self-describing.
var (
	// Version is the semantic version or tag of the build.
	Version = "dev"
	// Commit is the short git commit SHA of the build.
	Commit = "none"
	// BuildDate is the UTC build timestamp.
	BuildDate = "unknown"
)
