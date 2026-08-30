package systemd

import (
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/jlbyh2o/llamaman/internal/model"
)

var update = flag.Bool("update", false, "rewrite the golden unit files in testdata/units")

// goldenSpecs are the two fixtures every rendering test uses: one per topology.
//
// The systemctl path is pinned rather than probed so the goldens are the same
// on a host that keeps it in /bin and one that keeps it in /usr/bin — the drift
// check's agreement with install-units is asserted separately, by
// TestSystemctlPathIsTheOnlyProducer.
func goldenSpecs() map[string]Spec {
	return map[string]Spec{
		"system": {
			Scope:          model.ScopeSystem,
			Identity:       "llamaman",
			IdentityGroup:  "llamaman",
			Prefix:         "/usr/local/bin",
			UnitFilesGrant: true,
			Systemctl:      "/usr/bin/systemctl",
		},
		"user": {
			Scope:          model.ScopeUser,
			Identity:       "alice",
			IdentityGroup:  "alice",
			Prefix:         "/home/alice/.local/bin",
			UnitFilesGrant: true,
			Systemctl:      "/usr/bin/systemctl",
		},
	}
}

// TestRenderGolden pins every rendered file byte for byte, in both scopes.
//
// Byte-exact matters more here than in most golden tests: section 5.4a hashes
// the installed file as a whole, so a stray space this renderer emits becomes a
// permanent hash mismatch on every host, and D95's stamp is what decides whether
// that mismatch reads as a hand-edit or as a stale install.
func TestRenderGolden(t *testing.T) {
	t.Parallel()

	for scope, spec := range goldenSpecs() {
		t.Run(scope, func(t *testing.T) {
			t.Parallel()

			format := PolkitFormatBoth
			if spec.Scope == model.ScopeUser {
				format = PolkitFormatNone
			}
			files, err := spec.Files(format)
			if err != nil {
				t.Fatalf("Files: %v", err)
			}
			if len(files) == 0 {
				t.Fatal("Files returned nothing")
			}

			for _, f := range files {
				golden := filepath.Join("testdata", "units", scope, f.Name)
				if *update {
					if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
						t.Fatalf("mkdir: %v", err)
					}
					if err := os.WriteFile(golden, []byte(f.Content), 0o644); err != nil {
						t.Fatalf("write golden: %v", err)
					}
					continue
				}
				want, err := os.ReadFile(golden)
				if err != nil {
					t.Fatalf("read golden %s: %v (run `go test ./internal/systemd -update`)", golden, err)
				}
				if diff := cmp.Diff(string(want), f.Content); diff != "" {
					t.Errorf("%s (-golden +rendered):\n%s", f.Name, diff)
				}
			}
		})
	}
}

// TestStampIsFirstLine asserts D95's position rule for every file this binary
// writes.
//
// The position is not decoration. Section 5.4a hashes the file as a whole, so a
// unit transcribed without the line hashes differently AND reports an absent
// stamp — a permanent `drift: stale` that makes F16 unreachable for the
// hand-edit it exists to catch.
func TestStampIsFirstLine(t *testing.T) {
	t.Parallel()

	for scope, spec := range goldenSpecs() {
		format := PolkitFormatBoth
		if spec.Scope == model.ScopeUser {
			format = PolkitFormatNone
		}
		files, err := spec.Files(format)
		if err != nil {
			t.Fatalf("%s: Files: %v", scope, err)
		}
		for _, f := range files {
			first, _, _ := strings.Cut(f.Content, "\n")
			wantUnit := "# llamaman-units: " + strconv.Itoa(TemplateVersion)
			wantRules := "// llamaman-units: " + strconv.Itoa(TemplateVersion)
			if first != wantUnit && first != wantRules {
				t.Errorf("%s/%s first line = %q, want the version stamp", scope, f.Name, first)
			}
			if n, ok := Stamp(f.Content); !ok || n != TemplateVersion {
				t.Errorf("%s/%s Stamp() = (%d, %v), want (%d, true)", scope, f.Name, n, ok, TemplateVersion)
			}
		}
	}
}

// TestUserScopeRewrite is the CI assertion section 5.2a item (1) names: a
// rendered user unit may not mention network-online.target (a system target a
// user manager does not have, whose Wants= would be a hard start failure) or
// user@ (a unit a user unit runs INSIDE and therefore cannot order against).
func TestUserScopeRewrite(t *testing.T) {
	t.Parallel()

	spec := goldenSpecs()["user"]
	files, err := spec.Files(PolkitFormatNone)
	if err != nil {
		t.Fatalf("Files: %v", err)
	}

	for _, f := range files {
		if strings.Contains(f.Content, "network-online.target") {
			t.Errorf("%s names network-online.target in user scope", f.Name)
		}
		if strings.Contains(f.Content, "user@") {
			t.Errorf("%s names user@ in user scope", f.Name)
		}
		if strings.Contains(f.Content, "multi-user.target") {
			t.Errorf("%s names multi-user.target in user scope", f.Name)
		}
	}
}

// TestOrderingDirectives is section 5.2a item (1)'s table, asserted per unit and
// per scope rather than by eye.
func TestOrderingDirectives(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		scope      model.SystemdScope
		unit       string
		wantAfter  []string
		wantWants  []string
		wantWanted []string
	}{
		{
			name: "daemon, system", scope: model.ScopeSystem, unit: UnitDaemon,
			wantAfter:  []string{"network-online.target dbus.service"},
			wantWants:  []string{"network-online.target"},
			wantWanted: []string{"multi-user.target"},
		},
		{
			name: "daemon, user", scope: model.ScopeUser, unit: UnitDaemon,
			wantAfter:  []string{"basic.target dbus.socket"},
			wantWants:  []string{"dbus.socket"},
			wantWanted: []string{"default.target"},
		},
		{
			name: "instance, system", scope: model.ScopeSystem, unit: UnitInstance,
			wantAfter:  []string{"network-online.target", "llamaman.service"},
			wantWants:  []string{"network-online.target"},
			wantWanted: []string{"llamaman-instances.target"},
		},
		{
			// No Wants= at all: a Wants= on a unit the user manager cannot find
			// fails the start, and nothing is lost — llama-server binds
			// 127.0.0.1 only and never needed a routable address.
			name: "instance, user", scope: model.ScopeUser, unit: UnitInstance,
			wantAfter:  []string{"basic.target", "llamaman.service"},
			wantWants:  nil,
			wantWanted: []string{"llamaman-instances.target"},
		},
		{
			name: "target, system", scope: model.ScopeSystem, unit: UnitInstancesTgt,
			wantAfter:  []string{"network-online.target"},
			wantWants:  nil,
			wantWanted: []string{"multi-user.target"},
		},
		{
			name: "target, user", scope: model.ScopeUser, unit: UnitInstancesTgt,
			wantAfter:  []string{"basic.target"},
			wantWants:  nil,
			wantWanted: []string{"default.target"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			spec := goldenSpecs()[string(tc.scope)]
			content, err := spec.RenderUnit(tc.unit)
			if err != nil {
				t.Fatalf("RenderUnit: %v", err)
			}
			d := Directives(content)
			if diff := cmp.Diff(tc.wantAfter, d["After"]); diff != "" {
				t.Errorf("After= (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.wantWants, d["Wants"]); diff != "" {
				t.Errorf("Wants= (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.wantWanted, d["WantedBy"]); diff != "" {
				t.Errorf("WantedBy= (-want +got):\n%s", diff)
			}
		})
	}
}

// TestInstanceUnitIsNotPartOfTheDaemon is the property SPEC section 3.8 rests
// on: instances survive daemon restarts and self-update. A PartOf= or Requires=
// on llamaman.service would stop every instance the moment the daemon was
// restarted for an update.
func TestInstanceUnitIsNotPartOfTheDaemon(t *testing.T) {
	t.Parallel()

	for scope, spec := range goldenSpecs() {
		content, err := spec.RenderUnit(UnitInstance)
		if err != nil {
			t.Fatalf("%s: RenderUnit: %v", scope, err)
		}
		d := Directives(content)
		for _, v := range append(append([]string{}, d["PartOf"]...), d["Requires"]...) {
			if v == UnitDaemon {
				t.Errorf("%s: instance unit declares a requirement on %s", scope, UnitDaemon)
			}
		}
		if got := d["Restart"]; len(got) != 1 || got[0] != "no" {
			t.Errorf("%s: Restart= = %v, want [no] — the supervisor is the only restarter (D7)", scope, got)
		}
	}
}

// TestRevertDeadlineArithmetic asserts the values AND the arithmetic from the
// PARSED unit rather than from constants in the test, which is the point:
// section 5.4's revert deadline is a property of the installed file, and a
// future edit to either half has to keep the inequality true.
//
//	StartLimitBurst x (TimeoutStartSec + RestartSec) < StartLimitIntervalSec
//
// The left side is how long it takes to reach `failed`; the right side is the
// window the burst counter resets in. If the left side were larger the counter
// would reset between attempts and a hanging daemon would loop in `activating`
// forever without ever reaching `failed` — so the judge would never run.
func TestRevertDeadlineArithmetic(t *testing.T) {
	t.Parallel()

	for scope, spec := range goldenSpecs() {
		t.Run(scope, func(t *testing.T) {
			t.Parallel()

			content, err := spec.RenderUnit(UnitDaemon)
			if err != nil {
				t.Fatalf("RenderUnit: %v", err)
			}
			d := Directives(content)

			if !HasDirective(content, "OnFailure", UnitUpdateVerify) {
				t.Fatalf("llamaman.service does not carry OnFailure=%s — no update could ever be reverted (D88)", UnitUpdateVerify)
			}

			burst := mustInt(t, d, "StartLimitBurst")
			interval := mustInt(t, d, "StartLimitIntervalSec")
			startTimeout := mustInt(t, d, "TimeoutStartSec")
			restartSec := mustInt(t, d, "RestartSec")

			if burst != 5 || interval != 600 || startTimeout != 45 || restartSec != 2 {
				t.Errorf("got burst=%d interval=%d TimeoutStartSec=%d RestartSec=%d, want 5/600/45/2",
					burst, interval, startTimeout, restartSec)
			}
			if reach := burst * (startTimeout + restartSec); reach >= interval {
				t.Errorf("time to reach `failed` = %d s >= StartLimitIntervalSec = %d s: "+
					"the burst counter resets between attempts and the unit never reaches `failed`",
					reach, interval)
			}
			if got := d["Type"]; len(got) != 1 || got[0] != "notify" {
				t.Errorf("Type= = %v, want [notify] (D9)", got)
			}
			if got := d["WatchdogSec"]; len(got) != 1 || got[0] != "30" {
				t.Errorf("WatchdogSec= = %v, want [30] (D9)", got)
			}
			if got := d["FileDescriptorStoreMax"]; len(got) != 1 || got[0] != "256" {
				t.Errorf("FileDescriptorStoreMax= = %v, want [256] (D58)", got)
			}
		})
	}
}

func mustInt(t *testing.T, d map[string][]string, key string) int {
	t.Helper()
	vs := d[key]
	if len(vs) != 1 {
		t.Fatalf("%s= appears %d times, want once", key, len(vs))
	}
	n, err := strconv.Atoi(vs[0])
	if err != nil {
		t.Fatalf("%s=%q is not an integer", key, vs[0])
	}
	return n
}

// TestExecStartNamesAKnownSubcommand is the correspondence section 1 requires:
// every unit's ExecStart names one of the twelve subcommands, so a renamed
// command cannot silently leave a unit pointing at nothing.
func TestExecStartNamesAKnownSubcommand(t *testing.T) {
	t.Parallel()

	// The authoritative list of DESIGN section 1. cmd/llamaman holds the other
	// half of this correspondence in its dispatch map.
	known := map[string]bool{
		"serve": true, "status": true, "doctor": true, "diagnostics": true,
		"reset-password": true, "restore-db": true, "install-units": true,
		"instance-exec": true, "selfupdate-apply": true, "update-verify": true,
		"verify-release": true, "version": true,
	}

	for scope, spec := range goldenSpecs() {
		for _, name := range UnitNames(spec.Scope) {
			content, err := spec.RenderUnit(name)
			if err != nil {
				t.Fatalf("%s/%s: %v", scope, name, err)
			}
			for _, line := range Directives(content)["ExecStart"] {
				fields := strings.Fields(line)
				if len(fields) < 2 {
					t.Errorf("%s/%s: ExecStart=%q names no subcommand", scope, name, line)
					continue
				}
				if !strings.HasPrefix(fields[0], "/") {
					t.Errorf("%s/%s: ExecStart=%q does not begin with an absolute path", scope, name, line)
				}
				if !known[fields[1]] {
					t.Errorf("%s/%s: ExecStart names unknown subcommand %q", scope, name, fields[1])
				}
			}
		}
	}
}

// TestSubstitutionTable covers the tokens whose two states are the whole point:
// the port flag, the scope flag, and the autostart grant.
func TestSubstitutionTable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*Spec)
		unit    string
		want    []string
		notWant []string
	}{
		{
			name:    "no port flag by default",
			unit:    UnitDaemon,
			want:    []string{"ExecStart=/usr/local/bin/llamaman serve\n"},
			notWant: []string{"--port"},
		},
		{
			name:   "a port renders one flag",
			mutate: func(s *Spec) { s.Port = 9000 },
			unit:   UnitDaemon,
			want:   []string{"ExecStart=/usr/local/bin/llamaman serve --port 9000\n"},
		},
		{
			name:    "system scope renders no scope flag on serve",
			unit:    UnitDaemon,
			notWant: []string{"--scope"},
		},
		{
			name: "the swap actor's scope is a literal, not a substitution",
			unit: UnitSelfUpdate,
			want: []string{"ExecStart=/usr/local/bin/llamaman selfupdate-apply --scope system\n"},
		},
		{
			name: "the judge execs the previous binary with the rendered scope",
			unit: UnitUpdateVerify,
			want: []string{
				"ExecStart=/usr/local/bin/llamaman.prev update-verify --scope system\n",
				"ConditionPathExists=/usr/local/bin/llamaman.prev\n",
				"ConditionPathExists=%S/llamaman/update/pending\n",
			},
		},
		{
			name: "the actors' ExecStopPost= carries the probed systemctl path",
			unit: UnitSelfUpdate,
			want: []string{
				"ExecStopPost=-/usr/bin/systemctl reset-failed llamaman.service\n",
				"ExecStopPost=/usr/bin/systemctl start --no-block llamaman.service\n",
			},
		},
		{
			name: "the autostart grant is on by default",
			unit: PolkitRules,
			want: []string{"return polkit.Result.YES;\n"},
		},
		{
			name:   "--no-autostart-grant renders NOT_HANDLED",
			mutate: func(s *Spec) { s.UnitFilesGrant = false },
			unit:   PolkitRules,
			want:   []string{"        return polkit.Result.NOT_HANDLED;\n    }\n\n    return polkit.Result.NOT_HANDLED;\n"},
		},
		{
			name:   "--no-autostart-grant renders `no` in the .pkla",
			mutate: func(s *Spec) { s.UnitFilesGrant = false },
			unit:   PolkitPKLA,
			want:   []string{"ResultAny=no\n"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			spec := goldenSpecs()["system"]
			if tc.mutate != nil {
				tc.mutate(&spec)
			}
			content, err := spec.RenderUnit(tc.unit)
			if err != nil {
				t.Fatalf("RenderUnit(%s): %v", tc.unit, err)
			}
			for _, want := range tc.want {
				if !strings.Contains(content, want) {
					t.Errorf("%s does not contain %q\n---\n%s", tc.unit, want, content)
				}
			}
			for _, notWant := range tc.notWant {
				if strings.Contains(content, notWant) {
					t.Errorf("%s unexpectedly contains %q", tc.unit, notWant)
				}
			}
		})
	}
}

// TestUserScopeRendersTheScopeFlag asserts the one thing that tells the daemon
// which topology it is in (section 5.4): install-units decides, the daemon is
// told, nothing infers.
func TestUserScopeRendersTheScopeFlag(t *testing.T) {
	t.Parallel()

	spec := goldenSpecs()["user"]
	content, err := spec.RenderUnit(UnitDaemon)
	if err != nil {
		t.Fatalf("RenderUnit: %v", err)
	}
	if !strings.Contains(content, "ExecStart=/home/alice/.local/bin/llamaman serve --scope user\n") {
		t.Errorf("user-scope daemon unit does not render `serve --scope user`:\n%s", content)
	}

	judge, err := spec.RenderUnit(UnitUpdateVerify)
	if err != nil {
		t.Fatalf("RenderUnit: %v", err)
	}
	if !strings.Contains(judge, "update-verify --scope user\n") {
		t.Error("user-scope judge does not carry --scope user")
	}
	if !strings.Contains(judge, "/usr/bin/systemctl --user reset-failed") {
		t.Error("user-scope judge does not address the user manager")
	}
}

// TestUnitNamesPerScope: the privileged swap actor exists only where there is a
// privilege boundary to cross.
func TestUnitNamesPerScope(t *testing.T) {
	t.Parallel()

	sys := UnitNames(model.ScopeSystem)
	usr := UnitNames(model.ScopeUser)

	if !contains(sys, UnitSelfUpdate) {
		t.Errorf("system scope omits %s", UnitSelfUpdate)
	}
	if contains(usr, UnitSelfUpdate) {
		t.Errorf("user scope installs %s; the daemon performs the swap in process there (section 5.2a)", UnitSelfUpdate)
	}
	for _, n := range []string{UnitDaemon, UnitInstance, UnitInstancesTgt, UnitUpdateVerify} {
		if !contains(sys, n) || !contains(usr, n) {
			t.Errorf("%s is missing from one of the scopes", n)
		}
	}
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// TestRenderRejectsBadSpec: every refusal is at render time, before anything is
// written to /etc.
func TestRenderRejectsBadSpec(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		spec Spec
	}{
		{"no scope", Spec{Identity: "u", Prefix: "/usr/local/bin", Systemctl: "/usr/bin/systemctl"}},
		{"bad scope", Spec{Scope: "root", Identity: "u", Prefix: "/usr/local/bin", Systemctl: "/usr/bin/systemctl"}},
		{"no identity", Spec{Scope: model.ScopeSystem, Prefix: "/usr/local/bin", Systemctl: "/usr/bin/systemctl"}},
		{"no prefix", Spec{Scope: model.ScopeSystem, Identity: "u", Systemctl: "/usr/bin/systemctl"}},
		{"relative prefix", Spec{Scope: model.ScopeSystem, Identity: "u", Prefix: "bin", Systemctl: "/usr/bin/systemctl"}},
		{"port out of range", Spec{Scope: model.ScopeSystem, Identity: "u", Prefix: "/usr/local/bin", Port: 70000, Systemctl: "/usr/bin/systemctl"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := tc.spec.RenderUnit(UnitDaemon); err == nil {
				t.Fatal("RenderUnit accepted an invalid spec")
			}
		})
	}
}

// TestRenderUnknownFile: a name with no template is an error, not an empty file.
func TestRenderUnknownFile(t *testing.T) {
	t.Parallel()

	spec := goldenSpecs()["system"]
	if _, err := spec.RenderUnit("llamaman-nope.service"); err == nil {
		t.Fatal("RenderUnit accepted an unknown name")
	}
}

// TestSubstituteDropsEmptyLines is the rule that makes one template correct in
// both scopes: a directive whose whole value came from a token that rendered
// empty must disappear, not survive as a bare `Wants=`.
func TestSubstituteDropsEmptyLines(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		subs map[string]string
		want string
	}{
		{
			name: "an emptied whole-line token drops the line",
			raw:  "After=basic.target\n@WANTS_LINE@\nPartOf=x",
			subs: map[string]string{"@WANTS_LINE@": ""},
			want: "After=basic.target\nPartOf=x",
		},
		{
			name: "an emptied inline token collapses the gap it left",
			raw:  "ExecStart=/bin/x serve @A@ @B@",
			subs: map[string]string{"@A@": "", "@B@": ""},
			want: "ExecStart=/bin/x serve",
		},
		{
			name: "one empty token beside a full one leaves no double space",
			raw:  "ExecStart=/bin/x serve @A@ @B@",
			subs: map[string]string{"@A@": "", "@B@": "--port 9000"},
			want: "ExecStart=/bin/x serve --port 9000",
		},
		{
			name: "indentation survives a substitution",
			raw:  "    return @G@;",
			subs: map[string]string{"@G@": "polkit.Result.YES"},
			want: "    return polkit.Result.YES;",
		},
		{
			name: "a blank line is preserved",
			raw:  "a\n\nb",
			subs: map[string]string{},
			want: "a\n\nb",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := substitute(tc.raw, tc.subs)
			if err != nil {
				t.Fatalf("substitute: %v", err)
			}
			if got != tc.want {
				t.Errorf("substitute() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSubstituteRejectsUnresolvedTokens: a token the table forgot is a loud
// failure, not a unit file with an @PLACEHOLDER@ in its ExecStart.
func TestSubstituteRejectsUnresolvedTokens(t *testing.T) {
	t.Parallel()

	if _, err := substitute("ExecStart=@PREFIX@/llamaman", map[string]string{}); err == nil {
		t.Fatal("substitute accepted an unresolved token")
	}
	// The polkit regex and the instance SyslogIdentifier both contain an `@`
	// that is not a token, and neither may trip the check.
	for _, ok := range []string{
		`    /^llamaman-instance@[a-z0-9][a-z0-9-]{0,31}\.service$/.test(unit)`,
		"SyslogIdentifier=llamaman-instance@%i",
	} {
		if _, err := substitute(ok, map[string]string{}); err != nil {
			t.Errorf("substitute(%q) = %v, want no error", ok, err)
		}
	}
}

// TestStamp covers the parser on its own, including the shapes that must NOT
// read as a stamp.
func TestStamp(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		want    int
		wantOK  bool
	}{
		{"a unit stamp", "# llamaman-units: 7\n[Unit]\n", 7, true},
		{"a rules stamp", "// llamaman-units: 12\npolkit.addRule(", 12, true},
		{"no stamp", "[Unit]\nDescription=x\n", 0, false},
		{"not on the first line", "[Unit]\n# llamaman-units: 7\n", 0, false},
		{"not a number", "# llamaman-units: seven\n", 0, false},
		{"trailing junk", "# llamaman-units: 7 (hand edited)\n", 0, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := Stamp(tc.content)
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("Stamp() = (%d, %v), want (%d, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// TestClassify is D95's whole argument as a table: an older stamp is `stale` and
// blocks nothing, the SAME stamp with different content is a hand-edit and is
// F16, and an absent file is F16.
func TestClassify(t *testing.T) {
	t.Parallel()

	rendered := "# llamaman-units: " + strconv.Itoa(TemplateVersion) + "\n[Unit]\nDescription=x\n"

	tests := []struct {
		name      string
		installed string
		found     bool
		want      Drift
	}{
		{"identical", rendered, true, DriftNone},
		{"absent", "", false, DriftMissing},
		{
			name:      "an older stamp is stale, not a hand-edit",
			installed: "# llamaman-units: 0\n[Unit]\nDescription=something older\n",
			found:     true, want: DriftStale,
		},
		{
			name:      "no stamp at all is stale",
			installed: "[Unit]\nDescription=predates the stamp\n",
			found:     true, want: DriftStale,
		},
		{
			name:      "the same stamp with different content is a hand-edit",
			installed: "# llamaman-units: " + strconv.Itoa(TemplateVersion) + "\n[Unit]\nDescription=edited\n",
			found:     true, want: DriftEdited,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Classify(tc.installed, tc.found, rendered); got != tc.want {
				t.Errorf("Classify() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDirectivesAreReadNotHashed is the property D95 insists on for the two
// update gates: a file that is byte-different from the template but still
// carries OnFailure= keeps the capability, because the predicate is a grep, not
// a hash.
func TestDirectivesAreReadNotHashed(t *testing.T) {
	t.Parallel()

	spec := goldenSpecs()["system"]
	rendered, err := spec.RenderUnit(UnitDaemon)
	if err != nil {
		t.Fatalf("RenderUnit: %v", err)
	}

	edited := strings.Replace(rendered,
		"Description=Llama Man — llama.cpp management daemon",
		"Description=Llama Man (locally retitled)", 1)
	if edited == rendered {
		t.Fatal("the fixture edit did not apply")
	}

	if Classify(edited, true, rendered) != DriftEdited {
		t.Error("an edit at the current stamp should classify as a hand-edit")
	}
	if !HasDirective(edited, "OnFailure", UnitUpdateVerify) {
		t.Error("the revert capability was lost to an edit that did not touch OnFailure=")
	}

	stripped := strings.Replace(rendered, "OnFailure="+UnitUpdateVerify+"\n", "", 1)
	if HasDirective(stripped, "OnFailure", UnitUpdateVerify) {
		t.Error("OnFailure= was reported present after being stripped")
	}
}

// TestDirectivesIgnoresComments: the templates carry explanatory comments, and a
// directive named inside one must not read as a directive — otherwise stripping
// OnFailure= from a unit would still "find" it in the paragraph above.
func TestDirectivesIgnoresComments(t *testing.T) {
	t.Parallel()

	d := Directives("[Unit]\n# OnFailure=llamaman-update-verify.service is what D88 arms\n; Wants=nothing\nAfter=x\n")
	if len(d["OnFailure"]) != 0 {
		t.Errorf("a commented directive was parsed: %v", d["OnFailure"])
	}
	if len(d["Wants"]) != 0 {
		t.Errorf("a semicolon-commented directive was parsed: %v", d["Wants"])
	}
	if diff := cmp.Diff([]string{"x"}, d["After"]); diff != "" {
		t.Errorf("After= (-want +got):\n%s", diff)
	}
}

// TestSystemctlPathIsTheOnlyProducer asserts the two-candidate probe, including
// the refusal that replaces a PATH search.
func TestSystemctlPathIsTheOnlyProducer(t *testing.T) {
	restore := setSystemctlPath("/usr/bin/systemctl")
	got, err := SystemctlPath()
	restore()
	if err != nil || got != "/usr/bin/systemctl" {
		t.Fatalf("SystemctlPath() = (%q, %v)", got, err)
	}

	// An unresolvable systemctl must fail the render rather than produce a unit
	// with a relative first token, which systemd refuses to load.
	restore = setSystemctlPath("")
	systemctlOverride.Lock()
	systemctlOverride.set = false
	systemctlOverride.Unlock()
	prev := systemctlCandidates
	systemctlCandidates = []string{filepath.Join(t.TempDir(), "nope")}
	defer func() {
		systemctlCandidates = prev
		restore()
	}()

	if _, err := SystemctlPath(); err == nil {
		t.Fatal("SystemctlPath found a systemctl that does not exist")
	}
	spec := Spec{Scope: model.ScopeSystem, Identity: "u", Prefix: "/usr/local/bin"}
	if _, err := spec.RenderUnit(UnitSelfUpdate); err == nil {
		t.Fatal("RenderUnit succeeded with no systemctl to name")
	}
}
