package systemd

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// testInstall wires an install against a temp root with every host-touching
// seam replaced, so the whole command can be asserted without root and without
// a systemd.
func testInstall(t *testing.T, spec Spec, mutate func(*InstallOptions)) (string, InstallResult, error) {
	t.Helper()

	root := t.TempDir()
	reloads := 0
	opts := InstallOptions{
		Spec:         spec,
		Root:         root,
		PolkitFormat: PolkitFormatRules,
		grantJournal: func(string, string) (string, error) {
			return "journal group: llamaman is already a member of systemd-journal", nil
		},
		reload: func(context.Context) error { reloads++; return nil },
	}
	if mutate != nil {
		mutate(&opts)
	}
	res, err := Install(context.Background(), opts)
	return root, res, err
}

func systemSpec() Spec {
	return Spec{
		Scope:          model.ScopeSystem,
		Identity:       "llamaman",
		IdentityGroup:  "llamaman",
		Prefix:         "/usr/local/bin",
		UnitFilesGrant: true,
		Systemctl:      "/usr/bin/systemctl",
	}
}

func userSpec() Spec {
	s := systemSpec()
	s.Scope = model.ScopeUser
	s.Identity, s.IdentityGroup = "alice", "alice"
	s.Prefix = "/home/alice/.local/bin"
	return s
}

// TestInstallWritesTheExpectedSet: which files land where, per scope. The
// user-scope leg is the one worth asserting explicitly — there is no polkit rule
// in that topology at all, and writing one would advertise an authorization the
// host does not use.
func TestInstallWritesTheExpectedSet(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		spec Spec
		want []string
	}{
		{
			name: "system scope",
			spec: systemSpec(),
			want: []string{
				"/etc/polkit-1/rules.d/49-llamaman.rules",
				"/etc/systemd/system/llamaman-instance@.service",
				"/etc/systemd/system/llamaman-instances.target",
				"/etc/systemd/system/llamaman-selfupdate.service",
				"/etc/systemd/system/llamaman-update-verify.service",
				"/etc/systemd/system/llamaman.service",
			},
		},
		{
			name: "user scope: no polkit, no swap actor",
			spec: userSpec(),
			want: []string{
				"/etc/systemd/user/llamaman-instance@.service",
				"/etc/systemd/user/llamaman-instances.target",
				"/etc/systemd/user/llamaman-update-verify.service",
				"/etc/systemd/user/llamaman.service",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root, res, err := testInstall(t, tc.spec, nil)
			if err != nil {
				t.Fatalf("Install: %v", err)
			}

			got := relativize(t, root, res.Written)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("written (-want +got):\n%s", diff)
			}
			if len(res.Unchanged) != 0 {
				t.Errorf("a fresh install reported %v as unchanged", res.Unchanged)
			}
			if res.TemplateVersion != TemplateVersion {
				t.Errorf("TemplateVersion = %d, want %d", res.TemplateVersion, TemplateVersion)
			}

			// Every file on disk is byte-identical to what the renderer would
			// produce, which is the property section 5.4a's drift check depends
			// on: the installed file and the rendered one come from one
			// producer.
			for _, p := range res.Written {
				name := filepath.Base(p)
				want, err := tc.spec.RenderUnit(name)
				if err != nil {
					t.Fatalf("RenderUnit(%s): %v", name, err)
				}
				b, err := os.ReadFile(p)
				if err != nil {
					t.Fatalf("read %s: %v", p, err)
				}
				if Classify(string(b), true, want) != DriftNone {
					t.Errorf("%s does not match its own template", p)
				}
			}
		})
	}
}

// TestInstallIsIdempotent is section 13 step 11's promise: re-running the
// installer rewrites only what changed, so an upgrade does not churn /etc.
func TestInstallIsIdempotent(t *testing.T) {
	t.Parallel()

	spec := systemSpec()
	root := t.TempDir()
	opts := InstallOptions{
		Spec:         spec,
		Root:         root,
		PolkitFormat: PolkitFormatRules,
		grantJournal: func(string, string) (string, error) { return "", nil },
		reload:       func(context.Context) error { return nil },
	}

	first, err := Install(context.Background(), opts)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	second, err := Install(context.Background(), opts)
	if err != nil {
		t.Fatalf("Install (second): %v", err)
	}

	if len(second.Written) != 0 {
		t.Errorf("the second run rewrote %v", second.Written)
	}
	if diff := cmp.Diff(first.Written, second.Unchanged); diff != "" {
		t.Errorf("unchanged set (-first written +second unchanged):\n%s", diff)
	}

	// A hand-edited unit IS rewritten: that is the F16 repair path.
	unit := filepath.Join(root, DirSystemUnits, UnitDaemon)
	if err := os.WriteFile(unit, []byte("# hand edited\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	third, err := Install(context.Background(), opts)
	if err != nil {
		t.Fatalf("Install (third): %v", err)
	}
	if diff := cmp.Diff([]string{unit}, third.Written); diff != "" {
		t.Errorf("repair run (-want +got):\n%s", diff)
	}
}

// TestInstallModes: units and polkit files are world-readable, because systemd
// and polkitd read them as themselves.
func TestInstallModes(t *testing.T) {
	t.Parallel()

	_, res, err := testInstall(t, systemSpec(), nil)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	for _, p := range res.Written {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if got := fi.Mode().Perm(); got != 0o644 {
			t.Errorf("%s mode = %o, want 644", p, got)
		}
	}
}

// TestInstallCreatesNothingUnderTheStateDirectory is section 11.3's blanket rule
// for every root-invocable subcommand, asserted by directory diff. A
// root-created llamaman.db — or a -wal or -shm beside one — is a database the
// service identity can never write again.
func TestInstallCreatesNothingUnderTheStateDirectory(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	stateDir := filepath.Join(root, "var", "lib", "llamaman")
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	before := treeOf(t, stateDir)
	if _, err := Install(context.Background(), InstallOptions{
		Spec:         systemSpec(),
		Root:         root,
		PolkitFormat: PolkitFormatRules,
		grantJournal: func(string, string) (string, error) { return "", nil },
		reload:       func(context.Context) error { return nil },
	}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	after := treeOf(t, stateDir)

	if diff := cmp.Diff(before, after); diff != "" {
		t.Errorf("install-units touched the state directory (-before +after):\n%s", diff)
	}
}

// TestInstallUserScopeDoesNotReload: root's own `systemctl --user` addresses
// root's manager and silently does nothing useful, so this command prints the
// section 5.2a item (3) sequence instead of pretending.
func TestInstallUserScopeDoesNotReload(t *testing.T) {
	t.Parallel()

	reloads := 0
	_, res, err := testInstall(t, userSpec(), func(o *InstallOptions) {
		o.reload = func(context.Context) error { reloads++; return nil }
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if reloads != 0 {
		t.Errorf("user-scope install called daemon-reload %d times", reloads)
	}

	notes := strings.Join(res.Notes, "\n")
	for _, want := range []string{"loginctl enable-linger alice", "runuser -u alice", "systemctl --user daemon-reload"} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes do not carry %q:\n%s", want, notes)
		}
	}
}

// TestInstallSystemScopeReloads: the units are on disk and the manager has not
// read them until this runs (D48).
func TestInstallSystemScopeReloads(t *testing.T) {
	t.Parallel()

	reloads := 0
	_, res, err := testInstall(t, systemSpec(), func(o *InstallOptions) {
		o.reload = func(context.Context) error { reloads++; return nil }
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if reloads != 1 {
		t.Errorf("daemon-reload ran %d times, want 1", reloads)
	}
	if !strings.Contains(strings.Join(res.Notes, "\n"), "daemon-reload: ok") {
		t.Errorf("notes do not report the reload:\n%v", res.Notes)
	}
}

// TestInstallReloadFailureIsReported: a reload that fails leaves the files in
// place and says so, rather than reporting a successful install of units the
// manager has not read.
func TestInstallReloadFailureIsReported(t *testing.T) {
	t.Parallel()

	boom := errors.New("bus is down")
	_, res, err := testInstall(t, systemSpec(), func(o *InstallOptions) {
		o.reload = func(context.Context) error { return boom }
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Install = %v, want the reload error", err)
	}
	if len(res.Written) == 0 {
		t.Error("the files were reported as unwritten even though the failure was the reload")
	}
}

// TestInstallRepairPolkit: --repair-polkit writes BOTH formats and rewrites them
// even when identical, because "the file already matches" is exactly the answer
// that leaves a user with a denied control plane stuck.
func TestInstallRepairPolkit(t *testing.T) {
	t.Parallel()

	spec := systemSpec()
	root := t.TempDir()
	base := InstallOptions{
		Spec:         spec,
		Root:         root,
		PolkitFormat: PolkitFormatRules,
		grantJournal: func(string, string) (string, error) { return "", nil },
		reload:       func(context.Context) error { return nil },
	}
	if _, err := Install(context.Background(), base); err != nil {
		t.Fatalf("Install: %v", err)
	}

	repair := base
	repair.RepairPolkit = true
	res, err := Install(context.Background(), repair)
	if err != nil {
		t.Fatalf("Install (repair): %v", err)
	}

	written := relativize(t, root, res.Written)
	want := []string{
		"/etc/polkit-1/localauthority/50-local.d/49-llamaman.pkla",
		"/etc/polkit-1/rules.d/49-llamaman.rules",
	}
	if diff := cmp.Diff(want, written); diff != "" {
		t.Errorf("repair rewrote (-want +got):\n%s", diff)
	}

	// The units are untouched by a polkit repair: they already matched.
	for _, p := range res.Unchanged {
		if strings.Contains(p, "polkit") {
			t.Errorf("%s was reported unchanged during a --repair-polkit run", p)
		}
	}
}

// TestInstallNoAutostartGrant: the opt-out reaches the file, in both formats.
func TestInstallNoAutostartGrant(t *testing.T) {
	t.Parallel()

	spec := systemSpec()
	spec.UnitFilesGrant = false

	root, _, err := testInstall(t, spec, func(o *InstallOptions) { o.PolkitFormat = PolkitFormatBoth })
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	// Only branch (c) changes. reload-daemon and the name-scoped manage-units
	// branch still grant YES, which is what keeps start/stop/restart working on
	// a --no-autostart-grant host.
	rules := readFile(t, filepath.Join(root, DirPolkitRules, PolkitRules))
	const withheld = "\"org.freedesktop.systemd1.manage-unit-files\") {\n        return polkit.Result.NOT_HANDLED;"
	if !strings.Contains(rules, withheld) {
		t.Errorf("--no-autostart-grant did not withhold manage-unit-files:\n%s", rules)
	}
	if !strings.Contains(rules, "if (action.id === \"org.freedesktop.systemd1.reload-daemon\") {\n        return polkit.Result.YES;") {
		t.Error("--no-autostart-grant also withheld reload-daemon, which every other verb needs")
	}

	pkla := readFile(t, filepath.Join(root, DirPolkitPKLA, PolkitPKLA))
	if !strings.Contains(pkla, "ResultAny=no") {
		t.Errorf(".pkla did not carry the withheld grant:\n%s", pkla)
	}
}

// TestPolkitFormatFromVersion is the 0.106 boundary, plus the ambiguity that
// writes both rather than guessing.
func TestPolkitFormatFromVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		out  string
		want PolkitFormat
	}{
		{"modern", "pkaction version 0.120\n", PolkitFormatRules},
		{"exactly 0.106", "pkaction version 0.106\n", PolkitFormatRules},
		{"just below", "pkaction version 0.105\n", PolkitFormatPKLA},
		{"a 1.x line", "pkaction version 1.2\n", PolkitFormatRules},
		{"unparsable", "pkaction: unrecognized option\n", PolkitFormatBoth},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, why := polkitFormatFromVersion(tc.out)
			if got != tc.want {
				t.Errorf("polkitFormatFromVersion(%q) = %q, want %q", tc.out, got, tc.want)
			}
			if why == "" {
				t.Error("no explanation was produced for the operator")
			}
		})
	}
}

// TestGroupMembers covers the D77 grant's idempotency check.
func TestGroupMembers(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "group")
	content := "root:x:0:\n" +
		"systemd-journal:x:190:llamaman,alice\n" +
		"wheel:x:10:alice\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	members, found, err := groupMembers(path, JournalGroup)
	if err != nil || !found {
		t.Fatalf("groupMembers = (%v, %v, %v)", members, found, err)
	}
	if diff := cmp.Diff([]string{"llamaman", "alice"}, members); diff != "" {
		t.Errorf("members (-want +got):\n%s", diff)
	}

	// A group with no members is found with an empty list, which is not the
	// same as absent: absent means journald did not ship the group at all.
	if err := os.WriteFile(path, []byte("systemd-journal:x:190:\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	members, found, err = groupMembers(path, JournalGroup)
	if err != nil || !found || len(members) != 0 {
		t.Errorf("empty group = (%v, %v, %v), want ([], true, nil)", members, found, err)
	}

	_, found, err = groupMembers(path, "nosuchgroup")
	if err != nil || found {
		t.Errorf("missing group = (%v, %v), want (false, nil)", found, err)
	}

	_, found, err = groupMembers(filepath.Join(dir, "absent"), JournalGroup)
	if err != nil || found {
		t.Errorf("missing file = (%v, %v), want (false, nil)", found, err)
	}
}

// TestInstallReportsTheJournalGrant: the grant is part of what the command
// prints, because a user whose journal panes are empty needs to know whether it
// was applied.
func TestInstallReportsTheJournalGrant(t *testing.T) {
	t.Parallel()

	_, res, err := testInstall(t, systemSpec(), func(o *InstallOptions) {
		o.grantJournal = func(root, identity string) (string, error) {
			if identity != "llamaman" {
				t.Errorf("grantJournal got identity %q", identity)
			}
			return "journal group: added llamaman to systemd-journal", nil
		}
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !strings.Contains(strings.Join(res.Notes, "\n"), "added llamaman to systemd-journal") {
		t.Errorf("notes do not report the grant:\n%v", res.Notes)
	}
}

// TestInstallSurvivesAFailedJournalGrant: a host where usermod is missing still
// gets its units. The grant is important and it is not a reason to leave /etc
// half-written.
func TestInstallSurvivesAFailedJournalGrant(t *testing.T) {
	t.Parallel()

	_, res, err := testInstall(t, systemSpec(), func(o *InstallOptions) {
		o.grantJournal = func(string, string) (string, error) {
			return "", errors.New("usermod: command not found")
		}
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if len(res.Written) == 0 {
		t.Error("no units were written")
	}
	if !strings.Contains(strings.Join(res.Notes, "\n"), "usermod: command not found") {
		t.Errorf("the failure was not reported:\n%v", res.Notes)
	}
}

// TestInstallRejectsABadSpec: nothing is written when the spec cannot render.
func TestInstallRejectsABadSpec(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	_, err := Install(context.Background(), InstallOptions{
		Spec:         Spec{Scope: model.ScopeSystem, Prefix: "/usr/local/bin", Systemctl: "/usr/bin/systemctl"},
		Root:         root,
		PolkitFormat: PolkitFormatRules,
		grantJournal: func(string, string) (string, error) { return "", nil },
		reload:       func(context.Context) error { return nil },
	})
	if err == nil {
		t.Fatal("Install accepted a spec with no identity")
	}
	if got := treeOf(t, root); len(got) != 0 {
		t.Errorf("a refused install still wrote %v", got)
	}
}

func relativize(t *testing.T, root string, paths []string) []string {
	t.Helper()
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		rel, err := filepath.Rel(root, p)
		if err != nil {
			t.Fatalf("rel: %v", err)
		}
		out = append(out, "/"+rel)
	}
	sort.Strings(out)
	return out
}

// treeOf lists every path under dir, relative, for a directory diff.
func treeOf(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if rel != "." {
			out = append(out, rel)
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("walk %s: %v", dir, err)
	}
	sort.Strings(out)
	return out
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// TestInstallAlternateRootDoesNotReload: an install into a root other than "/"
// wrote files the host's manager does not have, so telling it to re-read its
// unit directories would be a privileged call about nothing.
func TestInstallAlternateRootDoesNotReload(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	res, err := Install(context.Background(), InstallOptions{
		Spec:         systemSpec(),
		Root:         root,
		PolkitFormat: PolkitFormatRules,
		grantJournal: func(string, string) (string, error) { return "", nil },
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !strings.Contains(strings.Join(res.Notes, "\n"), "was not reloaded") {
		t.Errorf("notes do not say the reload was skipped:\n%v", res.Notes)
	}
}
