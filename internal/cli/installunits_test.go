package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/systemd"
)

// installHarness records the options InstallUnits derives from its flags, which
// is the whole of what this command decides: everything below that line is
// internal/systemd's, and is asserted there.
type installHarness struct {
	env    Env
	out    *bytes.Buffer
	errOut *bytes.Buffer
	got    systemd.InstallOptions
	called int
	deps   installUnitsDeps
}

func newInstallHarness(t *testing.T) *installHarness {
	t.Helper()

	h := &installHarness{out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
	h.env = Env{Stdout: h.out, Stderr: h.errOut}
	h.deps = installUnitsDeps{
		geteuid: func() int { return 0 },
		lookup: func(name string) (userEntry, error) {
			switch name {
			case "llamaman":
				return userEntry{Name: "llamaman", UID: 986, GID: 986, Group: "llamaman", Home: "/var/lib/llamaman"}, nil
			case "alice":
				return userEntry{Name: "alice", UID: 1000, GID: 1000, Group: "users", Home: "/home/alice"}, nil
			}
			return userEntry{}, os.ErrNotExist
		},
		install: func(_ context.Context, opts systemd.InstallOptions) (systemd.InstallResult, error) {
			h.got = opts
			h.called++
			return systemd.InstallResult{
				Written:         []string{"/etc/systemd/system/llamaman.service"},
				Unchanged:       []string{"/etc/systemd/system/llamaman-instances.target"},
				Notes:           []string{"journal group: added llamaman to systemd-journal"},
				TemplateVersion: systemd.TemplateVersion,
			}, nil
		},
	}
	return h
}

// TestInstallUnitsFlags is the flag table of DESIGN section 11.3, asserted as
// the Spec each combination produces.
func TestInstallUnitsFlags(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want systemd.Spec
	}{
		{
			name: "defaults",
			args: []string{"--identity", "llamaman"},
			want: systemd.Spec{
				Scope: model.ScopeSystem, Identity: "llamaman", IdentityGroup: "llamaman",
				Prefix: "/usr/local/bin", UnitFilesGrant: true,
			},
		},
		{
			name: "a port is rendered into the daemon unit",
			args: []string{"--identity", "llamaman", "--port", "9000"},
			want: systemd.Spec{
				Scope: model.ScopeSystem, Identity: "llamaman", IdentityGroup: "llamaman",
				Prefix: "/usr/local/bin", Port: 9000, UnitFilesGrant: true,
			},
		},
		{
			name: "an explicit prefix is threaded, not decorative",
			args: []string{"--identity", "llamaman", "--prefix", "/opt/bin"},
			want: systemd.Spec{
				Scope: model.ScopeSystem, Identity: "llamaman", IdentityGroup: "llamaman",
				Prefix: "/opt/bin", UnitFilesGrant: true,
			},
		},
		{
			// D2: the binary lives in the identity's own ~/.local/bin, because
			// there the unprivileged daemon performs its own self-update by
			// renaming over that path.
			name: "--user-units defaults the prefix to the identity's home",
			args: []string{"--identity", "alice", "--user-units"},
			want: systemd.Spec{
				Scope: model.ScopeUser, Identity: "alice", IdentityGroup: "users",
				Prefix: "/home/alice/.local/bin", UnitFilesGrant: true,
			},
		},
		{
			name: "--no-autostart-grant withholds the unscopeable action",
			args: []string{"--identity", "llamaman", "--no-autostart-grant"},
			want: systemd.Spec{
				Scope: model.ScopeSystem, Identity: "llamaman", IdentityGroup: "llamaman",
				Prefix: "/usr/local/bin", UnitFilesGrant: false,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newInstallHarness(t)
			if err := installUnits(h.env, tc.args, h.deps); err != nil {
				t.Fatalf("installUnits: %v (stderr: %s)", err, h.errOut)
			}
			if h.called != 1 {
				t.Fatalf("install was called %d times", h.called)
			}
			if diff := cmp.Diff(tc.want, h.got.Spec); diff != "" {
				t.Errorf("Spec (-want +got):\n%s", diff)
			}
		})
	}
}

// TestInstallUnitsRepairPolkit: the F9 remediation flag reaches the installer.
func TestInstallUnitsRepairPolkit(t *testing.T) {
	t.Parallel()

	h := newInstallHarness(t)
	if err := installUnits(h.env, []string{"--identity", "llamaman", "--repair-polkit"}, h.deps); err != nil {
		t.Fatalf("installUnits: %v", err)
	}
	if !h.got.RepairPolkit {
		t.Error("--repair-polkit did not reach the installer")
	}
}

// TestInstallUnitsRefusals: every refusal happens before anything is written,
// and each says what to do instead.
func TestInstallUnitsRefusals(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		args    []string
		euid    int
		wantErr string
	}{
		{
			name: "no identity", args: nil, euid: 0,
			wantErr: "--identity is required",
		},
		{
			name: "not root", args: []string{"--identity", "llamaman"}, euid: 1000,
			wantErr: "must run as root",
		},
		{
			name: "no such user", args: []string{"--identity", "nobody-here"}, euid: 0,
			wantErr: "file does not exist",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newInstallHarness(t)
			h.deps.geteuid = func() int { return tc.euid }

			err := installUnits(h.env, tc.args, h.deps)
			if err == nil {
				t.Fatal("installUnits succeeded where it should have refused")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to mention %q", err, tc.wantErr)
			}
			if h.called != 0 {
				t.Error("the installer ran despite the refusal")
			}
		})
	}
}

// TestInstallUnitsNotRootPrintsTheCommand: a refusal that does not say how to
// succeed is a refusal a user has to guess their way past.
func TestInstallUnitsNotRootPrintsTheCommand(t *testing.T) {
	t.Parallel()

	h := newInstallHarness(t)
	h.deps.geteuid = func() int { return 1000 }
	_ = installUnits(h.env, []string{"--identity", "llamaman"}, h.deps)

	if !strings.Contains(h.errOut.String(), "sudo llamaman install-units --identity llamaman") {
		t.Errorf("stderr does not carry the sudo line:\n%s", h.errOut)
	}
}

// TestInstallUnitsPrintsWhatItChanged is section 13 step 7's requirement, and
// what an operator reads after running it.
func TestInstallUnitsPrintsWhatItChanged(t *testing.T) {
	t.Parallel()

	h := newInstallHarness(t)
	if err := installUnits(h.env, []string{"--identity", "llamaman"}, h.deps); err != nil {
		t.Fatalf("installUnits: %v", err)
	}

	out := h.out.String()
	for _, want := range []string{
		"system scope, identity llamaman, prefix /usr/local/bin",
		"wrote      /etc/systemd/system/llamaman.service",
		"unchanged  /etc/systemd/system/llamaman-instances.target",
		"added llamaman to systemd-journal",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout does not carry %q:\n%s", want, out)
		}
	}
}

// TestInstallUnitsNoAutostartGrantExplainsItself: the trade-off is stated where
// the operator will look for it, with the command that reconciles autostart by
// hand.
func TestInstallUnitsNoAutostartGrantExplainsItself(t *testing.T) {
	t.Parallel()

	h := newInstallHarness(t)
	if err := installUnits(h.env, []string{"--identity", "llamaman", "--no-autostart-grant"}, h.deps); err != nil {
		t.Fatalf("installUnits: %v", err)
	}
	if !strings.Contains(h.out.String(), "sudo systemctl enable llamaman-instance@<name>.service") {
		t.Errorf("stdout does not carry the manual enable line:\n%s", h.out)
	}
}

// TestInstallUnitsDryRun renders every file and writes none.
func TestInstallUnitsDryRun(t *testing.T) {
	t.Parallel()

	h := newInstallHarness(t)
	if err := installUnits(h.env, []string{"--identity", "llamaman", "--dry-run"}, h.deps); err != nil {
		t.Fatalf("installUnits: %v", err)
	}
	if h.called != 0 {
		t.Error("--dry-run called the installer")
	}

	out := h.out.String()
	for _, want := range []string{
		"/etc/systemd/system/llamaman.service",
		"ExecStart=/usr/local/bin/llamaman serve",
		"/etc/polkit-1/rules.d/49-llamaman.rules",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run does not show %q", want)
		}
	}
}

// TestInstallUnitsWritesRealFiles drives the whole command against a temp root,
// which is the only assertion that proves the flags, the renderer and the writer
// agree end to end.
func TestInstallUnitsWritesRealFiles(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	var out, errOut bytes.Buffer
	env := Env{Stdout: &out, Stderr: &errOut}

	deps := newInstallHarness(t).deps
	deps.install = systemd.Install
	deps.geteuid = func() int { return 1000 } // --root exempts the root check

	err := installUnits(env, []string{
		"--identity", "llamaman", "--root", root, "--port", "9000",
	}, deps)
	if err != nil {
		t.Fatalf("installUnits: %v (stderr: %s)", err, errOut.String())
	}

	unit, err := os.ReadFile(filepath.Join(root, "etc", "systemd", "system", "llamaman.service"))
	if err != nil {
		t.Fatalf("read the installed unit: %v", err)
	}
	if !strings.Contains(string(unit), "ExecStart=/usr/local/bin/llamaman serve --port 9000\n") {
		t.Errorf("the port did not reach the unit:\n%s", unit)
	}
	if !strings.HasPrefix(string(unit), "# llamaman-units: ") {
		t.Error("the installed unit carries no version stamp")
	}
}

// TestLookupUser parses the two files directly, because os/user's cgo path is
// unavailable in this binary and its pure-Go fallback reads the same files.
func TestLookupUser(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	passwd := filepath.Join(dir, "passwd")
	group := filepath.Join(dir, "group")

	writeTestFile(t, passwd,
		"root:x:0:0:root:/root:/bin/bash\n"+
			"llamaman:x:986:986::/var/lib/llamaman:/usr/sbin/nologin\n"+
			"alice:x:1000:1000:Alice:/home/alice:/bin/bash\n"+
			"broken:x:notanumber:1000::/home/broken:/bin/sh\n")
	writeTestFile(t, group,
		"root:x:0:\n"+
			"llamaman:x:986:\n"+
			"users:x:1000:alice\n")

	tests := []struct {
		name    string
		user    string
		want    userEntry
		wantErr string
	}{
		{
			name: "a dedicated system account",
			user: "llamaman",
			want: userEntry{Name: "llamaman", UID: 986, GID: 986, Group: "llamaman", Home: "/var/lib/llamaman"},
		},
		{
			name: "the installing user",
			user: "alice",
			want: userEntry{Name: "alice", UID: 1000, GID: 1000, Group: "users", Home: "/home/alice"},
		},
		{
			name: "no such user", user: "nobody-here",
			wantErr: "no such user",
		},
		{
			name: "an unparsable entry", user: "broken",
			wantErr: "unparsable uid/gid",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := lookupUser(passwd, group, tc.user)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("lookupUser = (%+v, %v), want an error mentioning %q", got, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("lookupUser: %v", err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("userEntry (-want +got):\n%s", diff)
			}
		})
	}
}

// TestLookupUserUnnamedGroup: a gid with no name is not fatal — systemd accepts
// a numeric Group=, and refusing here would block an install over a cosmetic
// lookup.
func TestLookupUserUnnamedGroup(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	passwd := filepath.Join(dir, "passwd")
	group := filepath.Join(dir, "group")
	writeTestFile(t, passwd, "svc:x:900:900::/var/lib/svc:/usr/sbin/nologin\n")
	writeTestFile(t, group, "root:x:0:\n")

	got, err := lookupUser(passwd, group, "svc")
	if err != nil {
		t.Fatalf("lookupUser: %v", err)
	}
	if got.Group != "900" {
		t.Errorf("Group = %q, want the numeric gid", got.Group)
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
