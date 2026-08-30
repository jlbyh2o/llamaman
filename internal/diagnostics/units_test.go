package diagnostics

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/systemd"
)

// TestUnitSectionClassifiesDrift exercises DESIGN section 5.4a's drift
// classification against a fabricated /etc: an installed daemon unit that
// exactly matches what this binary would render, and an instance template
// that was hand-edited after being rendered by this exact template version.
func TestUnitSectionClassifiesDrift(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	opt := Options{
		Scope: model.ScopeSystem, Identity: "llamaman", IdentityGroup: "llamaman",
		Prefix: "/usr/local/bin", Systemctl: "/usr/bin/systemctl", UnitRoot: root,
	}
	spec := systemd.Spec{
		Scope: opt.Scope, Identity: opt.Identity, IdentityGroup: opt.IdentityGroup,
		Prefix: opt.Prefix, Systemctl: opt.Systemctl,
	}

	rendered, err := spec.RenderUnit(systemd.UnitDaemon)
	if err != nil {
		t.Fatalf("RenderUnit(daemon): %v", err)
	}
	writeUnit(t, root, systemd.UnitDir(opt.Scope), systemd.UnitDaemon, rendered)

	edited, err := spec.RenderUnit(systemd.UnitInstance)
	if err != nil {
		t.Fatalf("RenderUnit(instance): %v", err)
	}
	edited += "\n# a human added this line\n"
	writeUnit(t, root, systemd.UnitDir(opt.Scope), systemd.UnitInstance, edited)

	// systemd.UnitInstancesTgt and the two update units are left uninstalled,
	// exercising the "missing" arm.

	files, drift := unitSection(opt)

	byName := map[string]map[string]any{}
	for _, row := range drift {
		byName[row["name"].(string)] = row
	}

	if got := byName[systemd.UnitDaemon]["drift"]; got != string(systemd.DriftNone) {
		t.Errorf("daemon unit drift = %v, want %q", got, systemd.DriftNone)
	}
	if got := byName[systemd.UnitInstance]["drift"]; got != string(systemd.DriftEdited) {
		t.Errorf("instance unit drift = %v, want %q (hand-edited after this template version rendered it)", got, systemd.DriftEdited)
	}
	if got := byName[systemd.UnitInstancesTgt]["drift"]; got != string(systemd.DriftMissing) {
		t.Errorf("uninstalled target drift = %v, want %q", got, systemd.DriftMissing)
	}
	if byName[systemd.UnitInstancesTgt]["installed"].(bool) {
		t.Error("an uninstalled unit reported installed=true")
	}

	// The raw content of every INSTALLED file rides along in the bundle
	// (D50's "unit and polkit files"); nothing is included for a file that
	// was never there.
	names := map[string]bool{}
	for _, f := range files {
		names[f.Name] = true
	}
	if !names["units/"+systemd.UnitDaemon] {
		t.Error("the installed daemon unit's content is not in the bundle")
	}
	if names["units/"+systemd.UnitInstancesTgt] {
		t.Error("a unit that was never installed has content in the bundle")
	}
}

// TestUnitSectionWithNoResolvedIdentity: a host that has never booted has no
// Identity to render a Spec with, and the section must still report the
// installed content — never crash trying to render an invalid Spec.
func TestUnitSectionWithNoResolvedIdentity(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeUnit(t, root, systemd.UnitDir(model.ScopeSystem), systemd.UnitDaemon, "[Unit]\nDescription=x\n")

	_, drift := unitSection(Options{Scope: model.ScopeSystem, UnitRoot: root})

	for _, row := range drift {
		if row["name"] == systemd.UnitDaemon {
			if row["drift"] != "unknown (identity not resolved)" {
				t.Errorf("drift = %v, want the unresolved-identity sentinel", row["drift"])
			}
			return
		}
	}
	t.Fatal("the daemon unit row is missing")
}

func writeUnit(t *testing.T, root, dir, name, content string) {
	t.Helper()
	full := filepath.Join(root, dir)
	if err := os.MkdirAll(full, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", full, err)
	}
	if err := os.WriteFile(filepath.Join(full, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}
