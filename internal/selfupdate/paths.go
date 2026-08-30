package selfupdate

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// The on-disk vocabulary of DESIGN section 12, in one place.
//
// Two directories and five names carry the whole protocol, and every one of them
// is derived rather than hardcoded:
//
//   - `<state_dir>/update/` is the staging directory, owned by the SERVICE
//     IDENTITY. Everything the unprivileged daemon downloads lands here, and the
//     one file in it that any other process reads is `pending`.
//   - `<prefix>/` is the installation directory, owned by ROOT. Both the
//     installed binary and the retained previous one live there, which is what
//     makes install and revert alike one rename() between two names in one
//     directory (D89) and what puts the judge's own executable somewhere the
//     service identity cannot write.
//
// `<prefix>` is never a constant: `install-units --prefix` writes it into the
// units and each actor re-derives it from its own
// filepath.EvalSymlinks(os.Executable()), so a `--prefix /opt/bin` install
// self-updates /opt/bin/llamaman rather than dropping a root-owned binary into
// /usr/local/bin that nothing runs (D15).

const (
	// UpdateDirName is `<state_dir>/update`, the staging directory.
	UpdateDirName = "update"

	// PendingFileName is the ONE marker of this protocol (D87). Its path is also
	// `llamaman-selfupdate.service`'s only trigger
	// (`ConditionPathExists=%S/llamaman/update/pending`) and one of the judge
	// unit's two, so a file the daemon did not create is a unit that reports
	// "condition failed" and an update that can never begin.
	PendingFileName = "pending"

	// StagedBinaryName is `update/llamaman.new`, the copy the daemon extracts
	// only to run its `version` probe and then unlinks. Nothing ever installs it:
	// the privileged actor extracts its own from the tarball it re-verified, so a
	// file the service identity could rewrite after verification is never the file
	// that lands on `<prefix>` (D89 (c)).
	StagedBinaryName = "llamaman.new"

	// ChecksumsName and SignatureName are the two verification artifacts of
	// section 16.2 step 3.
	ChecksumsName = "checksums.txt"
	SignatureName = "checksums.txt.sig"

	// DBBackupsDirName holds the D14 pre-update snapshots. It is the same
	// constant internal/app's nightly maintenance sweeps, restated here because
	// this package is the only writer of a file in it.
	DBBackupsDirName = "db-backups"

	// BinaryName is what `<prefix>` holds, and the member this package extracts
	// out of a release tarball.
	BinaryName = "llamaman"

	// RetainedName is `<prefix>/llamaman.prev`: the binary the swap replaced,
	// kept beside the installed one with the same owner and mode (D89). It is the
	// judge's own ExecStart, the emergency manual restore the F24 card names, and
	// it is replaced wholesale by the next update rather than accumulating —
	// rollback depth is exactly one.
	RetainedName = "llamaman.prev"

	// retainTmpName and installTmpName are the two staging names inside
	// `<prefix>`. Each exists so that the step that changes what is installed is a
	// rename between two names in ONE directory, which is what makes the
	// power-loss rows of section 12.3's table one line each.
	retainTmpName  = "llamaman.prev.tmp"
	installTmpName = "llamaman.new.tmp"
)

// Layout resolves every path this protocol touches from the two facts that vary
// per host: the state directory (D72) and the installation prefix (D15).
type Layout struct {
	// StateDir is `<state_dir>`, resolved by section 11.1 step 1 and never a
	// literal /var/lib/llamaman.
	StateDir string
	// Prefix is the installation directory, `<prefix>`. It is empty for a caller
	// that only reads the staging directory — the confirmation gate, which never
	// touches the installed binary.
	Prefix string
}

// UpdateDir is `<state_dir>/update`.
func (l Layout) UpdateDir() string { return filepath.Join(l.StateDir, UpdateDirName) }

// PendingPath is `<state_dir>/update/pending`.
func (l Layout) PendingPath() string { return filepath.Join(l.UpdateDir(), PendingFileName) }

// StagedBinaryPath is `<state_dir>/update/llamaman.new`.
func (l Layout) StagedBinaryPath() string { return filepath.Join(l.UpdateDir(), StagedBinaryName) }

// ChecksumsPath and SignaturePath are the two verification artifacts.
func (l Layout) ChecksumsPath() string { return filepath.Join(l.UpdateDir(), ChecksumsName) }

// SignaturePath is `<state_dir>/update/checksums.txt.sig`.
func (l Layout) SignaturePath() string { return filepath.Join(l.UpdateDir(), SignatureName) }

// TarballPath is where the release tarball for a version lands.
func (l Layout) TarballPath(version string) string {
	return filepath.Join(l.UpdateDir(), TarballName(version, runtime.GOARCH))
}

// BackupsDir is `<state_dir>/db-backups`.
func (l Layout) BackupsDir() string { return filepath.Join(l.StateDir, DBBackupsDirName) }

// InstalledPath is `<prefix>/llamaman`.
func (l Layout) InstalledPath() string { return filepath.Join(l.Prefix, BinaryName) }

// RetainedPath is `<prefix>/llamaman.prev`.
func (l Layout) RetainedPath() string { return filepath.Join(l.Prefix, RetainedName) }

func (l Layout) retainTmpPath() string  { return filepath.Join(l.Prefix, retainTmpName) }
func (l Layout) installTmpPath() string { return filepath.Join(l.Prefix, installTmpName) }

// TarballName is the release asset name of section 16.2 step 2:
// `llamaman_<ver>_linux_<arch>.tar.gz`, where `<arch>` is the Go arch string —
// amd64 or arm64, the two `make build-all` produces.
func TarballName(version, goarch string) string {
	return fmt.Sprintf("llamaman_%s_linux_%s.tar.gz", version, goarch)
}

// SnapshotName is the D14 snapshot file name:
// `llamaman-<version-being-replaced>-<unix seconds>.db`.
//
// The version in the name is the one being REPLACED, not the one being
// installed, and that is what makes D14's retention rule arithmetic rather than
// a search: a snapshot is written only immediately before an update, so the
// newest snapshot is always the database as the version now at
// `<prefix>/llamaman.prev` left it — exactly the schema section 12.4's downgrade
// procedure needs.
func SnapshotName(fromVersion string, unixSeconds int64) string {
	return fmt.Sprintf("llamaman-%s-%d.db", sanitizeVersion(fromVersion), unixSeconds)
}

// sanitizeVersion keeps a version string usable as one path segment. A tag is
// `v1.2.0` in every case this design produces; a hand-built binary reporting
// something with a slash in it must not be able to write outside db-backups/.
func sanitizeVersion(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return "unknown"
	}
	var b strings.Builder
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_', r == '+':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

// ResolvePrefix is D15's "no actor hardcodes the path": the installation
// directory is the directory of this process's own resolved executable.
//
// EvalSymlinks matters. `<prefix>/llamaman` may be reached through a symlink on
// a hand-built host, and the retain-and-swap sequence has to operate on the real
// file in the real directory — a rename against a symlink's own path would
// replace the link rather than the binary, and would not be in the same
// directory as `<prefix>/llamaman.prev`.
func ResolvePrefix() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("selfupdate: resolve this process's own binary: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", fmt.Errorf("selfupdate: resolve symlinks on %s: %w", exe, err)
	}
	return filepath.Dir(resolved), nil
}

// EnsureUpdateDir creates `<state_dir>/update` if it is absent. The state
// directory itself is guaranteed by the unit's StateDirectory=; this is the one
// subdirectory the protocol needs and the daemon is the only process that
// creates it (section 12.1: `update/pending` is written by the daemon, and that
// is not negotiable).
func (l Layout) EnsureUpdateDir() error {
	if err := os.MkdirAll(l.UpdateDir(), 0o750); err != nil {
		return fmt.Errorf("selfupdate: create %s: %w", l.UpdateDir(), err)
	}
	return nil
}

// ClearScratch empties `<state_dir>/update` of everything except a live
// `pending`, which is what section 12.1 step 1 does after its transaction
// commits and what the gate's two writing branches do after they unlink the
// marker.
//
// Keeping `pending` is not an optimization: the caller decides the marker's fate
// and the order matters. Step 1 has just PROVED no marker is live; the gate
// unlinks it first and then calls this. A sweep that took the marker with it
// would race the one file the whole protocol is keyed on.
func (l Layout) ClearScratch() error {
	entries, err := os.ReadDir(l.UpdateDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("selfupdate: read %s: %w", l.UpdateDir(), err)
	}
	var first error
	for _, e := range entries {
		if e.Name() == PendingFileName {
			continue
		}
		path := filepath.Join(l.UpdateDir(), e.Name())
		if err := os.RemoveAll(path); err != nil && first == nil {
			first = fmt.Errorf("selfupdate: remove %s: %w", path, err)
		}
	}
	return first
}
