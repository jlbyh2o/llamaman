package diagnostics

import (
	"os"
	"path/filepath"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/systemd"
)

// unitSection gathers D50's "unit and polkit files": the raw installed
// content of every file this host's topology could carry, plus one summary of
// what this binary would render and how the installed copy compares (DESIGN
// section 5.4a's drift classification).
//
// It never repairs anything and it never fails the bundle: a file that is not
// there is reported `drift: missing` (or `installed: false` for a polkit file,
// which is optional even on a healthy host under --no-autostart-grant's
// cousin, user scope), and a Spec that cannot be resolved because Identity is
// unknown still reports the installed content with `drift: "unknown (identity
// not resolved)"`.
func unitSection(opt Options) ([]File, []map[string]any) {
	scope := opt.Scope
	if scope == "" {
		scope = model.ScopeSystem
	}

	names := append([]string{}, systemd.UnitNames(scope)...)
	if scope != model.ScopeUser {
		names = append(names, systemd.PolkitRules, systemd.PolkitPKLA)
	}

	var files []File
	var drift []map[string]any

	spec := systemd.Spec{
		Scope: scope, Identity: opt.Identity, IdentityGroup: opt.IdentityGroup,
		Prefix: opt.Prefix, Port: opt.Port, UnitFilesGrant: opt.UnitFilesGrant,
		Systemctl: opt.Systemctl,
	}
	canRender := opt.Identity != ""

	for _, name := range names {
		dir := unitDirFor(name, scope)
		path := filepath.Join(opt.UnitRoot, dir, name)
		content, err := os.ReadFile(path)
		found := err == nil

		row := map[string]any{"name": name, "path": filepath.Join(dir, name), "installed": found}
		if found {
			files = append(files, File{Name: "units/" + name, Content: content})
			if n, ok := systemd.Stamp(string(content)); ok {
				row["stamp"] = n
			}
		}
		row["template_version"] = systemd.TemplateVersion

		switch {
		case !canRender:
			row["drift"] = "unknown (identity not resolved)"
		default:
			rendered, rerr := spec.RenderUnit(name)
			if rerr != nil {
				row["drift"] = "unknown (" + rerr.Error() + ")"
			} else {
				row["drift"] = string(systemd.Classify(string(content), found, rendered))
			}
		}
		drift = append(drift, row)
	}
	return files, drift
}

// unitDirFor is the directory a name installs into: the two polkit files are
// scope-independent (and only ever written outside user scope), everything
// else follows systemd.UnitDir.
func unitDirFor(name string, scope model.SystemdScope) string {
	switch name {
	case systemd.PolkitRules:
		return systemd.DirPolkitRules
	case systemd.PolkitPKLA:
		return systemd.DirPolkitPKLA
	default:
		return systemd.UnitDir(scope)
	}
}
