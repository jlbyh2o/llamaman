package llamacpp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jlbyh2o/llamaman/internal/llamacpp/source"
	"github.com/jlbyh2o/llamaman/internal/model"
)

// DESIGN section 6.1's `versions/` layout, and the two symlinks that are the
// ONLY activation mechanism.
//
// The invariant every function here serves: `versions/active` always points at
// the `is_active=1` row's directory, `versions/previous` at the
// `previous_active=1` row's, and the flip is `symlink(new, .active.tmp)` +
// `rename(.active.tmp, active)` — atomic, so there is no instant at which
// `active` is missing and no way to observe a half-written pointer.
//
// The direction of repair is fixed and is the whole reason D24's revert is a
// database write first: ON BOOT THE ROW WINS and the symlink is rebuilt from it
// (§2.5, §6.6). Flipping a symlink back while leaving `is_active=1` on the
// build whose canary just failed does not undo an activation — it creates a
// disagreement the next daemon start resolves in favor of the failed build.

const (
	// ActiveLink is `versions/active`, the path every instance's argv resolves
	// through at start time. A running process has already mapped its
	// executable and its lib/, so flipping this never affects one (§6.1).
	ActiveLink = "active"
	// PreviousLink is `versions/previous`, the one-click rollback target. It is
	// absent whenever `llamacpp.keep_previous` is off.
	PreviousLink = "previous"

	// tmpPrefix names the transient symlink the flip renames into place. It is
	// a dotfile so a directory listing of `versions/` never shows it as a
	// version, and it is per-link so two repairs cannot collide.
	tmpPrefix = "."
	tmpSuffix = ".tmp"

	// AcqPrebuilt and AcqSource are the third axis of D60's identity as it
	// appears in the id and the directory name: `b10621-cpu-bin`,
	// `b10621-cpu-src`. They are deliberately shorter than the
	// `llamacpp_versions.acquisition` values they encode — the column is read by
	// SQL, the suffix by humans reading directory names.
	AcqPrebuilt = "bin"
	AcqSource   = "src"
)

// Layout resolves section 6.1's paths under the RESOLVED state directory (D72).
// It embeds the source builder's layout rather than restating it, so the paths a
// build writes and the paths this package publishes and deletes can never drift.
type Layout struct {
	source.Layout
}

// NewLayout returns the layout under a state directory.
func NewLayout(stateDir string) Layout {
	return Layout{Layout: source.Layout{StateDir: stateDir}}
}

// VersionsRoot is `<state_dir>/versions`.
func (l Layout) VersionsRoot() string {
	return filepath.Join(l.StateDir, source.VersionsDirName)
}

// TmpDir is `<state_dir>/tmp`, the download staging area of section 6.1.
func (l Layout) TmpDir() string { return filepath.Join(l.StateDir, "tmp") }

// LinkPath is `versions/active` or `versions/previous`.
func (l Layout) LinkPath(name string) string { return filepath.Join(l.VersionsRoot(), name) }

// VersionID is D60's three-part identity: `<tag>-<backend>-<acq>`. It is also
// the directory name, which is why every caller that needs either uses this one
// function.
func VersionID(tag string, backend model.Backend, acq model.Acquisition) string {
	return tag + "-" + string(backend) + "-" + AcqSuffix(acq)
}

// AcqSuffix is the id's third component for an acquisition.
func AcqSuffix(a model.Acquisition) string {
	if a == model.AcquisitionSource {
		return AcqSource
	}
	return AcqPrebuilt
}

// ValidateVersionID refuses an id that would not be a safe directory name. It
// is the source builder's rule, re-exported here because every id this package
// mints or accepts becomes a path under `versions/`.
func ValidateVersionID(id string) error { return source.ValidateVersionID(id) }

// defaultGuard is D25's production implementation: `readlink /proc/<pid>/exe`
// over every process this identity can see. It is a lower bound by construction
// — a process it cannot read is skipped rather than assumed absent — and the
// states it protects are all reachable only through processes it CAN see, since
// every llama-server this daemon starts runs as this identity.
func defaultGuard() DirGuard { return source.ProcExeGuard{} }

// SetLink points `versions/<name>` at a version directory, atomically.
//
// The two steps are the whole of it: a symlink is created under a temporary
// name and renamed over the target. `rename(2)` on a symlink replaces the
// existing one in one operation, so a crash lands either on the old pointer or
// on the new one and never on neither.
//
// The target is stored RELATIVE (`b10621-cuda-src`, not an absolute path), which
// is what lets a whole state directory be moved — D72's promise that
// /var/lib/llamaman is a default rather than a constant.
func (l Layout) SetLink(name, dirName string) error {
	if dirName == "" {
		return fmt.Errorf("llamacpp: refusing to point versions/%s at an empty directory name", name)
	}
	if filepath.Base(dirName) != dirName {
		return fmt.Errorf("llamacpp: versions/%s target %q contains a path separator", name, dirName)
	}
	root := l.VersionsRoot()
	if err := os.MkdirAll(root, 0o750); err != nil {
		return fmt.Errorf("llamacpp: create %s: %w", root, err)
	}

	tmp := filepath.Join(root, tmpPrefix+name+tmpSuffix)
	// A leftover from a killed process would make the symlink call fail with
	// EEXIST; it names nothing anybody reads, so removing it is safe.
	if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("llamacpp: clear %s: %w", tmp, err)
	}
	if err := os.Symlink(dirName, tmp); err != nil {
		return fmt.Errorf("llamacpp: stage versions/%s: %w", name, err)
	}
	if err := os.Rename(tmp, l.LinkPath(name)); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("llamacpp: publish versions/%s: %w", name, err)
	}
	return nil
}

// RemoveLink drops `versions/<name>`. It is what `keep_previous = false` does to
// `versions/previous` (§6.6 step 2), and removing something that is not there is
// success rather than an error.
func (l Layout) RemoveLink(name string) error {
	if err := os.Remove(l.LinkPath(name)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("llamacpp: remove versions/%s: %w", name, err)
	}
	return nil
}

// ReadLink returns the directory name `versions/<name>` points at, or "" when
// the link is absent. A dangling link — one whose target no longer exists — is
// reported by its name rather than as an error: that is exactly the state boot
// reconciliation repairs, and it must be able to see it.
func (l Layout) ReadLink(name string) (string, error) {
	target, err := os.Readlink(l.LinkPath(name))
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("llamacpp: read versions/%s: %w", name, err)
	}
	// A link written by an older build, or by hand, may be absolute.
	return filepath.Base(strings.TrimSuffix(target, string(os.PathSeparator))), nil
}

// ExeVersionID maps a resolved `/proc/<pid>/exe` path back to the version id it
// runs from — D25's question answered in the direction the guard asks it.
//
// It returns "" for a path that is not under a `versions/<id>/` directory at
// all, which is every process this daemon did not start.
func (l Layout) ExeVersionID(exe string) string {
	root := l.VersionsRoot() + string(os.PathSeparator)
	if !strings.HasPrefix(exe, root) {
		return ""
	}
	rest := strings.TrimPrefix(exe, root)
	id, _, found := strings.Cut(rest, string(os.PathSeparator))
	if !found {
		return ""
	}
	return id
}
