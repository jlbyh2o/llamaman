package app

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jlbyh2o/llamaman/internal/model"
)

func TestResolveScope(t *testing.T) {
	t.Parallel()

	userProbe := func() (model.SystemdScope, bool) { return model.ScopeUser, true }
	silentProbe := func() (model.SystemdScope, bool) { return "", false }

	cases := []struct {
		name    string
		flag    string
		probe   func() (model.SystemdScope, bool)
		want    model.SystemdScope
		wantErr bool
	}{
		{"the flag wins over the probe", "system", userProbe, model.ScopeSystem, false},
		{"the flag alone", "user", nil, model.ScopeUser, false},
		{"the probe answers user", "", userProbe, model.ScopeUser, false},
		{"a silent probe falls back to system", "", silentProbe, model.ScopeSystem, false},
		{"no probe at all falls back to system", "", nil, model.ScopeSystem, false},
		{"an invalid flag is an error", "root", nil, "", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := resolveScope(tc.flag, tc.probe)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolveScope(%q) = %q, want an error", tc.flag, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveScope(%q): %v", tc.flag, err)
			}
			if got != tc.want {
				t.Errorf("resolveScope(%q) = %q, want %q", tc.flag, got, tc.want)
			}
		})
	}
}

// TestResolveStateDir walks D72's chain. The cases are the ones section 11.1
// step 1 names, including the two that used to break the --user-units install:
// a user-scope manager whose $STATE_DIRECTORY is under $HOME, and a hand-run
// daemon with no manager at all.
func TestResolveStateDir(t *testing.T) {
	t.Parallel()

	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}

	cases := []struct {
		name     string
		scope    model.SystemdScope
		override string
		env      map[string]string
		want     string
	}{
		{
			name:  "the manager's own answer wins",
			scope: model.ScopeSystem,
			env: map[string]string{
				"STATE_DIRECTORY": "/var/lib/llamaman",
				"INVOCATION_ID":   "abc",
				"HOME":            "/root",
			},
			want: "/var/lib/llamaman",
		},
		{
			name:  "a user unit's state directory is under HOME",
			scope: model.ScopeUser,
			env: map[string]string{
				"STATE_DIRECTORY": "/home/u/.local/state/llamaman",
				"INVOCATION_ID":   "abc",
			},
			want: "/home/u/.local/state/llamaman",
		},
		{
			name:  "several directories: the first wins",
			scope: model.ScopeSystem,
			env: map[string]string{
				"STATE_DIRECTORY": "/var/lib/llamaman:/var/lib/other",
				"INVOCATION_ID":   "abc",
			},
			want: "/var/lib/llamaman",
		},
		{
			name:  "user scope with no manager variable falls to XDG_STATE_HOME",
			scope: model.ScopeUser,
			env:   map[string]string{"XDG_STATE_HOME": "/home/u/.state", "HOME": "/home/u"},
			want:  "/home/u/.state/llamaman",
		},
		{
			name:  "user scope with no XDG falls to HOME",
			scope: model.ScopeUser,
			env:   map[string]string{"HOME": "/home/u"},
			want:  "/home/u/.local/state/llamaman",
		},
		{
			name:  "a hand-run system-scope daemon is still not under a manager",
			scope: model.ScopeSystem,
			env:   map[string]string{"HOME": "/home/u"},
			want:  "/home/u/.local/state/llamaman",
		},
		{
			name:  "a system unit with no STATE_DIRECTORY gets the default",
			scope: model.ScopeSystem,
			env:   map[string]string{"INVOCATION_ID": "abc", "HOME": "/root"},
			want:  DefaultStateDir,
		},
		{
			name:     "the override short-circuits everything",
			scope:    model.ScopeUser,
			override: "/tmp/x",
			env:      map[string]string{"STATE_DIRECTORY": "/var/lib/llamaman"},
			want:     "/tmp/x",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := resolveStateDir(tc.scope, tc.override, env(tc.env))
			if got != tc.want {
				t.Errorf("resolveStateDir = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestLockStateDirRefusesASecondDaemon is F11: a second daemon on one state
// directory exits 70 rather than racing the first one.
func TestLockStateDirRefusesASecondDaemon(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	release, err := lockStateDir(dir)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	t.Cleanup(func() { _ = release() })

	// The same process re-locking its own flock would succeed (flock is
	// per-open-file-description, and this is a second open), so the assertion
	// that matters is the error TYPE the composition root maps to exit 70.
	if _, err := lockStateDir(dir); err == nil {
		t.Fatal("a second lock on the same state directory was granted")
	} else {
		var locked *LockedError
		if !errors.As(err, &locked) {
			t.Fatalf("second lock error = %v (%T), want *LockedError", err, err)
		}
		if locked.Path != filepath.Join(dir, LockFileName) {
			t.Errorf("LockedError.Path = %q, want the lock file in the state directory", locked.Path)
		}
		// F11 promises "a message naming the holding PID", and the kernel does
		// not reliably answer for a flock(2) lock, so the holder writes it.
		if locked.PID != os.Getpid() {
			t.Errorf("LockedError.PID = %d, want the holding process %d", locked.PID, os.Getpid())
		}
	}

	if err := release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	release2, err := lockStateDir(dir)
	if err != nil {
		t.Fatalf("locking after release: %v", err)
	}
	_ = release2()
}

func TestParseFlags(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		args    []string
		want    flags
		wantErr bool
	}{
		{"no flags", nil, flags{}, false},
		{"scope and port", []string{"--scope", "user", "--port", "8080"},
			flags{scope: "user", port: 8080}, false},
		{"a positional argument is refused", []string{"extra"}, flags{}, true},
		{"a privileged port is refused", []string{"--port", "80"}, flags{}, true},
		{"an out-of-range port is refused", []string{"--port", "70000"}, flags{}, true},
		{"an unknown flag is refused", []string{"--config", "/etc/llamaman.conf"}, flags{}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseFlags(tc.args, discard{})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseFlags(%v) = %+v, want an error", tc.args, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseFlags(%v): %v", tc.args, err)
			}
			if got != tc.want {
				t.Errorf("parseFlags(%v) = %+v, want %+v", tc.args, got, tc.want)
			}
		})
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
