package source

import (
	"fmt"
	"path/filepath"
	"regexp"
)

// Directory and file names from DESIGN section 6.1's on-disk layout. They are
// constants rather than literals at their use sites because three of them are
// named by other packages too — `versions` by the launcher and the instance
// service, `build` and `logs` by the diagnostics bundle — and a second spelling
// of any of them is a second answer to a question D72 already settled.
const (
	// SrcDirName is <state_dir>/src, which holds the one shared partial clone.
	SrcDirName = "src"
	// RepoDirName is that clone: <state_dir>/src/llama.cpp. It is shared by
	// every build precisely so the second build of the day downloads only new
	// objects (section 6.5's `fetch` phase).
	RepoDirName = "llama.cpp"
	// BuildDirName is <state_dir>/build, the parent of one git worktree plus
	// cmake directory per version id.
	BuildDirName = "build"
	// VersionsDirName is <state_dir>/versions.
	VersionsDirName = "versions"
	// LogsDirName is <state_dir>/logs; build logs live in its build/ child.
	LogsDirName = "logs"

	// StagingSuffix is D78's protocol in one string: every install lands in
	// `versions/<id>.staging` and is renamed into place at publish, so no
	// directory `versions/active` can resolve into is ever written in place.
	StagingSuffix = ".staging"
	// SupersededSuffix names the outgoing directory during the two-rename swap
	// a forced rebuild of an existing id performs (D78, section 6.2).
	SupersededSuffix = ".old"

	// CMakeSubdir is the cmake binary directory INSIDE the worktree:
	// build/<id>/build. Section 6.5 configures `-S build/<id> -B
	// build/<id>/build`, which is what makes D4's warm rerun possible — the
	// object cache is a child of the worktree and survives with it.
	CMakeSubdir = "build"

	// BuildLogName is the copy of the build log that ships inside the published
	// version directory (section 6.5's destination (b)).
	BuildLogName = "build.log"
	// ManifestName is the version's manifest (sections 6.4 step 4 and 6.5
	// `publish`), which carries the verbatim `llama-server --help` capture.
	ManifestName = "manifest.json"
)

// versionIDRe is what may become a path component. `llamacpp_versions.id` is
// `<tag>-<backend>-<acq>` (D60) and a tag is upstream's or a `fork-<hash>-<sha>`
// of our own, so this is deliberately narrow: an id that could contain a
// separator or start with a dot would let a request name a directory outside
// `versions/`, and every path in this package is built by joining it.
var versionIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ValidateVersionID rejects an id that cannot safely become a directory name.
func ValidateVersionID(id string) error {
	if !versionIDRe.MatchString(id) {
		return fmt.Errorf("source: version id %q is not a usable directory name", id)
	}
	if filepath.Base(id) != id {
		return fmt.Errorf("source: version id %q contains a path separator", id)
	}
	return nil
}

// Layout resolves DESIGN section 6.1's paths under the RESOLVED state directory
// (D72). It holds no handles and touches no disk, so it is safe to copy and
// trivial to point at a temp directory in a test.
type Layout struct {
	// StateDir is `runtime_info.state_dir` — never a literal
	// /var/lib/llamaman, which is only the default (D72, section 11.1 step 1).
	StateDir string
}

// RepoDir is the shared partial clone: <state_dir>/src/llama.cpp.
func (l Layout) RepoDir() string { return filepath.Join(l.StateDir, SrcDirName, RepoDirName) }

// WorktreeDir is this version's git worktree: <state_dir>/build/<id>. D4 keeps
// it across a daemon restart, which is what makes Retry a warm `cmake --build`
// rather than a full CUDA rebuild.
func (l Layout) WorktreeDir(id string) string {
	return filepath.Join(l.StateDir, BuildDirName, id)
}

// CMakeDir is the cmake binary directory inside that worktree.
func (l Layout) CMakeDir(id string) string {
	return filepath.Join(l.WorktreeDir(id), CMakeSubdir)
}

// StagingDir is <state_dir>/versions/<id>.staging — the install destination for
// EVERY build, prebuilt or source (D78).
func (l Layout) StagingDir(id string) string {
	return filepath.Join(l.StateDir, VersionsDirName, id+StagingSuffix)
}

// VersionDir is the published <state_dir>/versions/<id>, which `versions/active`
// may point at and which nothing writes into in place.
func (l Layout) VersionDir(id string) string {
	return filepath.Join(l.StateDir, VersionsDirName, id)
}

// SupersededDir is the transient <state_dir>/versions/<id>.old that exists only
// between the two renames of a forced rebuild's swap.
func (l Layout) SupersededDir(id string) string {
	return filepath.Join(l.StateDir, VersionsDirName, id+SupersededSuffix)
}

// LogPath is the durable build log, <state_dir>/logs/build/<id>.log (F15). It
// outlives the version directory on purpose: a failed build has no version
// directory at all, and its log is the whole record of why.
func (l Layout) LogPath(id string) string {
	return filepath.Join(l.StateDir, LogsDirName, BuildDirName, id+".log")
}

// ManifestPath is <dir>/manifest.json for a version or staging directory.
func ManifestPath(dir string) string { return filepath.Join(dir, ManifestName) }

// ServerPath, BenchPath and CLIPath are the three binaries section 6.5's
// `install` phase asserts, resolved inside a version or staging directory.
func ServerPath(dir string) string { return filepath.Join(dir, "bin", "llama-server") }

// BenchPath is <dir>/bin/llama-bench, the tool D23's LLAMA_BUILD_TOOLS=ON exists
// to produce.
func BenchPath(dir string) string { return filepath.Join(dir, "bin", "llama-bench") }

// CLIPath is <dir>/bin/llama-cli.
func CLIPath(dir string) string { return filepath.Join(dir, "bin", "llama-cli") }

// LibDir is <dir>/lib, which D22's `$ORIGIN/../lib` RPATH resolves to and which
// is why a version directory is relocatable at all.
func LibDir(dir string) string { return filepath.Join(dir, "lib") }
