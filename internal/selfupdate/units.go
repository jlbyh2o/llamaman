package selfupdate

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/systemd"
)

// Reading the installed units, for the two guard clauses that are facts about
// them (DESIGN section 12.1 step 1, D95).
//
// The last two clauses of `POST /update/apply` read THE INSTALLED UNITS' OWN
// DIRECTIVES, never a template hash. That is the whole of D95: units are written
// once at install time and are NOT rewritten by a self-update, while the drift
// check of section 5.4a renders from the running binary's templates — so a host
// that self-updated across a release which touched any template legitimately
// differs and nobody edited anything. Such a host is `drift: stale`, it blocks
// nothing, and it must still be able to update.
//
// So `self_update_revert` and the swap-unit clause are grep-shaped facts about a
// file's content, and they answer the same on a stale host as on a fresh one —
// which is exactly what "the drift check reports no drift" could never do.

// DiskUnits reads the units from the directory this scope installs them into.
type DiskUnits struct {
	// Dir is the unit directory. Empty resolves it from Scope, which is what the
	// daemon passes; a test points it at a temp directory.
	Dir string
	// Scope selects /etc/systemd/system or /etc/systemd/user when Dir is empty.
	Scope model.SystemdScope
}

// Unit implements UnitFacts.
//
// "Masked" is a unit symlinked to /dev/null, which is how `systemctl mask`
// writes it. systemd will not start such a unit, so for both self-update clauses
// it is exactly as disqualifying as absent — and both refusals say which of the
// two it was, because the repair lines differ (`install-units` versus `unmask`).
func (d DiskUnits) Unit(name string) (UnitFile, error) {
	dir := d.Dir
	if dir == "" {
		dir = systemd.UnitDir(d.Scope)
	}
	path := filepath.Join(dir, name)

	// Lstat first: a masked unit is a symlink, and following it would report
	// /dev/null's own (absent) content rather than the fact that matters.
	li, err := os.Lstat(path)
	switch {
	case os.IsNotExist(err):
		return UnitFile{}, nil
	case err != nil:
		return UnitFile{}, fmt.Errorf("selfupdate: stat %s: %w", path, err)
	}
	if li.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err == nil && target == os.DevNull {
			return UnitFile{Present: true, Masked: true}, nil
		}
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return UnitFile{}, fmt.Errorf("selfupdate: read %s: %w", path, err)
	}
	return UnitFile{Present: true, Content: string(content)}, nil
}
