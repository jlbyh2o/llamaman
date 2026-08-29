package buildinfo

// Values injected at build time via
// -ldflags "-X github.com/jlbyh2o/llamaman/internal/buildinfo.Version=…".
// The defaults below are what an unstamped `go build` produces.
var (
	// Version is the release version, e.g. "v1.2.3".
	Version = "dev"
	// Commit is the git commit the binary was built from.
	Commit = "none"
	// Date is the build timestamp in RFC 3339 UTC.
	Date = "unknown"
	// Channel is the release channel this binary tracks, e.g. "stable".
	Channel = "dev"
)
