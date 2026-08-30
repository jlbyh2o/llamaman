package source

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// ManifestVersion is the manifest format's own version, so a later reader can
// tell a v1 manifest from whatever replaces it without guessing from the keys.
const ManifestVersion = 1

// ManifestGPU is one device the build was compiled for (D21): the compute
// capability that went into CMAKE_CUDA_ARCHITECTURES, recorded with the UUID it
// came from so a changed GPU set can be DETECTED rather than assumed.
type ManifestGPU struct {
	UUID       string `json:"uuid"`
	Name       string `json:"name,omitempty"`
	ComputeCap string `json:"compute_capability,omitempty"`
}

// Manifest is `versions/<id>/manifest.json` for a source build (section 6.5's
// `publish` row). Its defining contents are the ones no database column can
// hold: the verbatim `llama-server --help` capture, and the exact toolchain and
// flags this tree was produced by.
//
// The database is still the source of truth for everything the API serves —
// section 2.5 has columns for the state, the binaries, the sizes and the parsed
// help flags. The manifest is what makes a version DIRECTORY self-describing:
// after a restore from backup, or on a state directory carried to another
// machine, it is the only record of how these binaries came to exist.
//
// The keys shared with a prebuilt install's manifest (section 6.4 step 4) are
// deliberately spelled the same — `manifest_version`, `version_id`, `tag`,
// `build_tag`, `channel`, `acquisition`, `backend`, `built_at`, `built_by`,
// `binaries`, `size_bytes`, `server_help`, `help_flags`, `supports_fit`,
// `devices_output`, `version_output` — so one reader decodes both and
// `GET /api/v1/llamacpp/active` needs no branch. The fields below that a
// prebuilt has no equivalent for describe the BUILD: the commit, the toolchain,
// the flags, the parallelism and the GPUs it was compiled for.
type Manifest struct {
	ManifestVersion int    `json:"manifest_version"`
	VersionID       string `json:"version_id"`
	Tag             string `json:"tag,omitempty"`
	BuildTag        string `json:"build_tag,omitempty"`
	Channel         string `json:"channel,omitempty"`
	Acquisition     string `json:"acquisition"`
	Backend         string `json:"backend"`

	GitURL         string `json:"git_url"`
	GitRef         string `json:"git_ref,omitempty"`
	ResolvedCommit string `json:"resolved_commit,omitempty"`

	BuiltAt      time.Time `json:"built_at"`
	BuiltBy      string    `json:"built_by,omitempty"` // the llamaman version that built it
	Generator    string    `json:"generator"`
	CMakeVersion string    `json:"cmake_version,omitempty"`
	CCache       bool      `json:"ccache"`
	Jobs         int       `json:"jobs"`
	OOMRetried   bool      `json:"oom_retried,omitempty"`

	CMakeFlags   []string      `json:"cmake_flags"`
	CUDAArchList string        `json:"cuda_arch_list,omitempty"`
	GPUs         []ManifestGPU `json:"gpus,omitempty"`
	HostCPUFlags string        `json:"host_cpu_flags,omitempty"`

	Binaries  []string `json:"binaries"`
	SizeBytes int64    `json:"size_bytes"`

	// ServerHelp is the capture section 2.5 calls for verbatim; HelpFlags is
	// the projection that becomes the column.
	ServerHelp    string   `json:"server_help,omitempty"`
	HelpFlags     []string `json:"help_flags,omitempty"`
	SupportsFit   bool     `json:"supports_fit"`
	DevicesOutput string   `json:"devices_output,omitempty"`
	VersionOutput string   `json:"version_output,omitempty"`
}

// WriteManifest writes the manifest into a version or staging directory.
func WriteManifest(dir string, m Manifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("source: encode manifest: %w", err)
	}
	b = append(b, '\n')
	path := ManifestPath(dir)
	if err := os.WriteFile(path, b, 0o640); err != nil {
		return fmt.Errorf("source: write %s: %w", path, err)
	}
	return nil
}

// ReadManifest reads a version directory's manifest.
func ReadManifest(dir string) (Manifest, error) {
	var m Manifest
	b, err := os.ReadFile(ManifestPath(dir))
	if err != nil {
		return m, fmt.Errorf("source: read manifest: %w", err)
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("source: parse manifest: %w", err)
	}
	return m, nil
}

// DirGuard answers D25's question: is a live process executing out of this
// directory? Publish asks it again immediately before a forced rebuild's swap,
// because between the request and the rename an instance may have started.
type DirGuard interface {
	InUse(ctx context.Context, dir string) (pid int, inUse bool, err error)
}

// ProcExeGuard resolves /proc/<pid>/exe for every process it can see, which is
// D25's rule verbatim: "Database bookkeeping alone is not trusted for this."
//
// An unprivileged daemon can only read the exe link of processes running as its
// own identity — which is exactly the set that matters, since every
// llama-server this daemon starts runs as that identity. A process it cannot
// see is skipped rather than treated as absent-with-certainty; the guard is
// therefore honest about being a lower bound, and the states it protects
// (`versions/<id>` being renamed out from under a running server) are all
// reachable only through processes it CAN see.
type ProcExeGuard struct {
	// Root is the proc filesystem; empty means /proc.
	Root string
}

// InUse reports the first live process whose executable resolves inside dir.
func (g ProcExeGuard) InUse(ctx context.Context, dir string) (int, bool, error) {
	root := g.Root
	if root == "" {
		root = "/proc"
	}
	target, err := filepath.EvalSymlinks(dir)
	if err != nil {
		// The directory does not exist, so nothing can be executing from it.
		if os.IsNotExist(err) {
			return 0, false, nil
		}
		target = dir
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		return 0, false, fmt.Errorf("source: read %s: %w", root, err)
	}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return 0, false, err
		}
		pid, ok := numericName(e.Name())
		if !ok {
			continue
		}
		exe, err := os.Readlink(filepath.Join(root, e.Name(), "exe"))
		if err != nil {
			// A process that exited, or one belonging to another user. Both are
			// skips, not errors: the loop is a scan, and one unreadable entry
			// must not turn a publish into a failure.
			continue
		}
		if underDir(exe, target) || underDir(exe, dir) {
			return pid, true, nil
		}
	}
	return 0, false, nil
}

func numericName(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
	}
	return n, true
}

func underDir(path, dir string) bool {
	if dir == "" {
		return false
	}
	// A deleted executable's link reads as "/path/to/bin (deleted)"; the
	// process is still running it, so the prefix test must still match.
	path = strings.TrimSuffix(path, " (deleted)")
	return path == dir || strings.HasPrefix(path, dir+string(os.PathSeparator))
}

// assertInstalled is section 6.5's `install` assertion: the three binaries and
// the shared-library directory must all be present in the staging tree before
// anything is verified, let alone renamed into place.
func assertInstalled(dir string) error {
	for _, p := range []string{ServerPath(dir), BenchPath(dir), CLIPath(dir)} {
		st, err := os.Stat(p)
		if err != nil {
			return &Failure{
				Phase:   PhaseInstall,
				Code:    CodeInstallIncomplete,
				Message: fmt.Sprintf("the install did not produce %s", p),
				cause:   err,
			}
		}
		if st.Mode()&0o111 == 0 {
			return &Failure{
				Phase:   PhaseInstall,
				Code:    CodeInstallIncomplete,
				Message: fmt.Sprintf("%s is not executable", p),
			}
		}
	}
	lib := LibDir(dir)
	st, err := os.Stat(lib)
	if err != nil || !st.IsDir() {
		return &Failure{
			Phase: PhaseInstall,
			Code:  CodeInstallIncomplete,
			Message: fmt.Sprintf("the install did not produce %s, so this tree is not self-contained "+
				"and the $ORIGIN/../lib RPATH resolves to nothing (D22)", lib),
			cause: err,
		}
	}
	return nil
}

// publishDir performs D78's protocol: the staging tree becomes `versions/<id>`
// by rename.
//
// For a fresh id that is one atomic rename into a non-existent target. For an id
// whose directory already exists — a forced rebuild — it is the guarded
// two-rename swap of section 6.2: re-check the live-process guard, rename the
// old directory aside, rename the new one in, then remove the old one.
// `versions/active` names `<id>` and is never touched here, so it is correct
// before and after; the only window in which it dangles is between the two
// renames, and that window is closed from the other side by data — the row is
// not `ready` for the whole rebuild, and both the supervisor and instance-exec
// refuse to start an instance while the is_active=1 row is not `ready`.
func (b *Builder) publishDir(ctx context.Context, id string) error {
	staging := b.Layout.StagingDir(id)
	target := b.Layout.VersionDir(id)

	if _, err := os.Lstat(target); err != nil {
		if !os.IsNotExist(err) {
			return &Failure{
				Phase:   PhasePublish,
				Code:    CodePublishFailed,
				Message: fmt.Sprintf("cannot inspect %s", target),
				cause:   err,
			}
		}
		if err := os.Rename(staging, target); err != nil {
			return &Failure{
				Phase:   PhasePublish,
				Code:    CodePublishFailed,
				Message: fmt.Sprintf("cannot move the staging tree into %s", target),
				cause:   err,
			}
		}
		return nil
	}

	// The target exists: this is a rebuild of an id that is already installed,
	// and possibly the ACTIVE one.
	if pid, inUse, err := b.guard().InUse(ctx, target); err != nil {
		b.log().Warn("live-process guard failed; refusing to swap the version directory",
			"version_id", id, "error", err)
		return &Failure{
			Phase:   PhasePublish,
			Code:    CodeVersionInUse,
			Message: fmt.Sprintf("could not determine whether a process is executing out of %s", target),
			cause:   err,
		}
	} else if inUse {
		return &Failure{
			Phase: PhasePublish,
			Code:  CodeVersionInUse,
			Message: fmt.Sprintf("process %d is executing out of %s, so this directory cannot be replaced",
				pid, target),
			Hint: "Stop the instances running on this version and retry; the finished build is kept in " +
				filepath.Base(staging) + " until then.",
		}
	}

	old := b.Layout.SupersededDir(id)
	if err := os.RemoveAll(old); err != nil {
		return &Failure{
			Phase:   PhasePublish,
			Code:    CodePublishFailed,
			Message: fmt.Sprintf("cannot clear %s", old),
			cause:   err,
		}
	}
	if err := os.Rename(target, old); err != nil {
		return &Failure{
			Phase:   PhasePublish,
			Code:    CodePublishFailed,
			Message: fmt.Sprintf("cannot move %s aside", target),
			cause:   err,
		}
	}
	if err := os.Rename(staging, target); err != nil {
		// Put the old tree back: a failed publish must not leave the id with no
		// directory at all, because `versions/active` may name it.
		if restoreErr := os.Rename(old, target); restoreErr != nil {
			return &Failure{
				Phase: PhasePublish,
				Code:  CodePublishFailed,
				Message: fmt.Sprintf("cannot move the staging tree into %s, and restoring the previous "+
					"directory also failed (%v): %s still holds it", target, restoreErr, old),
				cause: err,
			}
		}
		return &Failure{
			Phase:   PhasePublish,
			Code:    CodePublishFailed,
			Message: fmt.Sprintf("cannot move the staging tree into %s; the previous build was restored", target),
			cause:   err,
		}
	}
	if err := os.RemoveAll(old); err != nil {
		// The swap succeeded; a leftover .old directory is disk to reclaim, not
		// a failed build.
		b.log().Warn("could not remove the superseded version directory",
			"version_id", id, "dir", old, "error", err)
	}
	return nil
}

// dirStats walks a version tree for the two columns section 2.5 fills at
// `ready`: `binaries_json` (the executables, relative to the directory) and
// `size_bytes`.
func dirStats(dir string) (binaries []string, size int64, err error) {
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			size += info.Size()
			if info.Mode()&0o111 != 0 && strings.HasPrefix(path, filepath.Join(dir, "bin")+string(os.PathSeparator)) {
				rel, relErr := filepath.Rel(dir, path)
				if relErr == nil {
					binaries = append(binaries, rel)
				}
			}
		}
		return nil
	})
	sort.Strings(binaries)
	if err != nil {
		return nil, 0, fmt.Errorf("source: measure %s: %w", dir, err)
	}
	return binaries, size, nil
}

// HostCPUFlags reads the CPU feature set from a /proc/cpuinfo-shaped file, for
// `llamacpp_versions.host_cpu_flags`.
//
// The build sets GGML_NATIVE=ON because the build host is the run host (section
// 6.5). That assumption breaks the day a state directory is moved to another
// machine, and this string is what lets the UI say "rebuild recommended"
// instead of letting an illegal-instruction crash explain it. The flags are
// sorted so two reads of the same host compare equal regardless of which core
// answered.
func HostCPUFlags(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("source: read %s: %w", path, err)
	}
	for line := range strings.Lines(string(b)) {
		key, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "flags", "Features":
			fields := strings.Fields(rest)
			sort.Strings(fields)
			return strings.Join(fields, " "), nil
		}
	}
	return "", nil
}

// manifestGPUs projects the request's device set into the manifest's form.
func manifestGPUs(req Request) []ManifestGPU {
	out := make([]ManifestGPU, 0, len(req.GPUs))
	for _, g := range req.GPUs {
		out = append(out, ManifestGPU{UUID: g.UUID, Name: g.Name, ComputeCap: g.ComputeCap})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// acquisition is always `source` here, and is written into the manifest so a
// directory says which of D60's three axes it sits on without the database.
const acquisition = string(model.AcquisitionSource)
