package systemd

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// fakeRunner records every invocation and answers from a scripted table.
type fakeRunner struct {
	mu   sync.Mutex
	args [][]string
	// respond answers one invocation. Nil means "exit 0, no output".
	respond func(args []string) (stdout, stderr string, code int, err error)
}

func (f *fakeRunner) run(_ context.Context, name string, args ...string) (string, string, int, error) {
	f.mu.Lock()
	f.args = append(f.args, append([]string{name}, args...))
	respond := f.respond
	f.mu.Unlock()
	if respond == nil {
		return "", "", 0, nil
	}
	return respond(args)
}

func (f *fakeRunner) invocations() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.args))
	copy(out, f.args)
	return out
}

func newTestExec(t *testing.T, scope model.SystemdScope, r *fakeRunner) *ExecController {
	t.Helper()
	c, err := NewExecController(ExecOptions{
		Scope:        scope,
		Path:         "/usr/bin/systemctl",
		Logger:       quietLogger(),
		PollInterval: 5 * time.Millisecond,
		run:          r.run,
	})
	if err != nil {
		t.Fatalf("NewExecController: %v", err)
	}
	return c
}

// TestExecArgs is the fallback's half of the blocking/no-wait split, plus the
// --user prefix that is not optional in the D2 topology: root's own systemctl
// addresses root's manager, and a missing --user in a user-scope daemon
// addresses the system one.
func TestExecArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		scope  model.SystemdScope
		invoke func(context.Context, *ExecController) error
		want   []string
	}{
		{
			name: "start blocks", scope: model.ScopeSystem,
			invoke: func(ctx context.Context, c *ExecController) error { _, err := c.Start(ctx, "u.service"); return err },
			want:   []string{"/usr/bin/systemctl", "start", "u.service"},
		},
		{
			name: "stop blocks", scope: model.ScopeSystem,
			invoke: func(ctx context.Context, c *ExecController) error { _, err := c.Stop(ctx, "u.service"); return err },
			want:   []string{"/usr/bin/systemctl", "stop", "u.service"},
		},
		{
			name: "start no-wait passes --no-block", scope: model.ScopeSystem,
			invoke: func(ctx context.Context, c *ExecController) error {
				_, err := c.StartNoWait(ctx, "u.service")
				return err
			},
			want: []string{"/usr/bin/systemctl", "start", "--no-block", "u.service"},
		},
		{
			name: "restart no-wait passes --no-block", scope: model.ScopeSystem,
			invoke: func(ctx context.Context, c *ExecController) error {
				_, err := c.RestartNoWait(ctx, "u.service")
				return err
			},
			want: []string{"/usr/bin/systemctl", "restart", "--no-block", "u.service"},
		},
		{
			name: "reset-failed", scope: model.ScopeSystem,
			invoke: func(ctx context.Context, c *ExecController) error { return c.ResetFailed(ctx, "llamaman.service") },
			want:   []string{"/usr/bin/systemctl", "reset-failed", "llamaman.service"},
		},
		{
			name: "user scope carries --user", scope: model.ScopeUser,
			invoke: func(ctx context.Context, c *ExecController) error { _, err := c.Start(ctx, "u.service"); return err },
			want:   []string{"/usr/bin/systemctl", "--user", "start", "u.service"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r := &fakeRunner{}
			c := newTestExec(t, tc.scope, r)
			if err := tc.invoke(context.Background(), c); err != nil {
				t.Fatalf("invoke: %v", err)
			}
			got := r.invocations()
			if len(got) != 1 {
				t.Fatalf("invocations = %v, want one", got)
			}
			if diff := cmp.Diff(tc.want, got[0]); diff != "" {
				t.Errorf("argv (-want +got):\n%s", diff)
			}
		})
	}
}

// TestExecEnableReloads: the same reload-after-enable contract the D-Bus
// controller keeps, so a caller cannot tell which one it has.
func TestExecEnableReloads(t *testing.T) {
	t.Parallel()

	r := &fakeRunner{}
	c := newTestExec(t, model.ScopeSystem, r)
	if err := c.Enable(context.Background(), []string{"a.service", "b.service"}); err != nil {
		t.Fatalf("Enable: %v", err)
	}

	want := [][]string{
		{"/usr/bin/systemctl", "enable", "a.service", "b.service"},
		{"/usr/bin/systemctl", "daemon-reload"},
	}
	if diff := cmp.Diff(want, r.invocations()); diff != "" {
		t.Errorf("argv (-want +got):\n%s", diff)
	}
}

// TestExecErrorTranslation is where the fallback earns its keep or fails
// silently: systemctl reports a missing unit as exit 5 plus prose and a polkit
// denial as exit 1 plus different prose, and collapsing the two would show the
// user "no such unit" for a host that is merely unauthorized.
func TestExecErrorTranslation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		code   int
		stderr string
		want   error
	}{
		{
			name: "missing unit, exit 5",
			code: 5, stderr: "Failed to start x.service: Unit x.service not found.",
			want: ErrNoSuchUnit,
		},
		{
			name: "missing unit, exit 1 with prose",
			code: 1, stderr: "Failed to restart x.service: Unit x.service not found.",
			want: ErrNoSuchUnit,
		},
		{
			name: "polkit denial",
			code: 1, stderr: "Failed to start x.service: Access denied\nSee system logs.",
			want: ErrDenied,
		},
		{
			name: "polkit would have prompted",
			code: 1, stderr: "Failed to start x.service: Interactive authentication required.",
			want: ErrDenied,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r := &fakeRunner{respond: func([]string) (string, string, int, error) {
				return "", tc.stderr, tc.code, nil
			}}
			c := newTestExec(t, model.ScopeSystem, r)

			res, err := c.Start(context.Background(), "x.service")
			if !errors.Is(err, tc.want) {
				t.Fatalf("Start = %v, want %v", err, tc.want)
			}
			if res != JobFailed {
				t.Errorf("JobResult = %q, want %q", res, JobFailed)
			}
		})
	}
}

// TestExecUnknownFailureKeepsItsProse: an exit status this design has no name
// for must reach the operator with systemctl's own first line attached, not as
// a bare "failed".
func TestExecUnknownFailureKeepsItsProse(t *testing.T) {
	t.Parallel()

	r := &fakeRunner{respond: func([]string) (string, string, int, error) {
		return "", "Job for x.service failed because a timeout was exceeded.\nSee 'systemctl status'.", 1, nil
	}}
	c := newTestExec(t, model.ScopeSystem, r)

	_, err := c.Start(context.Background(), "x.service")
	if err == nil {
		t.Fatal("Start succeeded on a failing job")
	}
	if errors.Is(err, ErrNoSuchUnit) || errors.Is(err, ErrDenied) {
		t.Errorf("an unclassified failure was classified: %v", err)
	}
	if !strings.Contains(err.Error(), "a timeout was exceeded") {
		t.Errorf("error lost systemctl's prose: %v", err)
	}
	if strings.Contains(err.Error(), "See 'systemctl status'") {
		t.Errorf("error carried more than the first line: %v", err)
	}
}

// TestExecProps is the parsing this controller lives or dies by.
func TestExecProps(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		stdout  string
		want    UnitProps
		wantErr error
	}{
		{
			name: "a running service",
			stdout: strings.Join([]string{
				"Id=llamaman.service",
				"ActiveState=active",
				"SubState=running",
				"MainPID=2446",
				"Result=success",
				"NRestarts=0",
				"MemoryCurrent=6066176",
				"ExecMainExitTimestamp=",
				"ExecMainStatus=0",
				"LoadState=loaded",
			}, "\n") + "\n",
			want: UnitProps{
				ActiveState: "active", SubState: "running", MainPID: 2446,
				Result: "success", MemoryCurrent: 6066176,
			},
		},
		{
			name: "a failed instance carries its exit code and exit instant",
			stdout: strings.Join([]string{
				"Id=llamaman-instance@qwen.service",
				"ActiveState=failed",
				"SubState=failed",
				"MainPID=0",
				"Result=exit-code",
				"NRestarts=3",
				"MemoryCurrent=[not set]",
				"ExecMainExitTimestamp=@1788042587",
				"ExecMainStatus=78",
				"LoadState=loaded",
			}, "\n") + "\n",
			want: UnitProps{
				ActiveState: "failed", SubState: "failed", Result: "exit-code",
				NRestarts: 3, ExecMainStatus: 78,
				ExecMainExitTimestamp: time.Unix(1788042587, 0).UTC(),
			},
		},
		{
			// A systemd too old for --timestamp=unix prints a localized date.
			// The field stays zero — unknown — rather than becoming a wrong
			// instant a restart policy would then reason from.
			name: "a localized timestamp is not guessed at",
			stdout: strings.Join([]string{
				"Id=x.service", "ActiveState=inactive", "SubState=dead",
				"ExecMainExitTimestamp=Sat 2026-08-29 16:29:47 MDT",
				"LoadState=loaded",
			}, "\n") + "\n",
			want: UnitProps{ActiveState: "inactive", SubState: "dead"},
		},
		{
			// `systemctl show` answers for a unit it has never heard of with
			// exit 0, so only LoadState can tell "stopped" from "absent".
			name: "an unknown unit is not a stopped unit",
			stdout: strings.Join([]string{
				"Id=nope.service", "ActiveState=inactive", "SubState=dead",
				"LoadState=not-found",
			}, "\n") + "\n",
			wantErr: ErrNoSuchUnit,
		},
		{
			name:    "no output at all",
			stdout:  "",
			wantErr: ErrNoSuchUnit,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r := &fakeRunner{respond: func([]string) (string, string, int, error) {
				return tc.stdout, "", 0, nil
			}}
			c := newTestExec(t, model.ScopeSystem, r)

			got, err := c.Props(context.Background(), "u.service")
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Props = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Props: %v", err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("Props (-want +got):\n%s", diff)
			}
		})
	}
}

// TestExecPropsAsksForUnixTimestamps: the option is what makes the exit instant
// parseable at all, so its absence from the argv is a regression worth catching
// directly.
func TestExecPropsAsksForUnixTimestamps(t *testing.T) {
	t.Parallel()

	r := &fakeRunner{respond: func([]string) (string, string, int, error) {
		return "Id=u.service\nLoadState=loaded\n", "", 0, nil
	}}
	c := newTestExec(t, model.ScopeSystem, r)
	if _, err := c.Props(context.Background(), "u.service"); err != nil {
		t.Fatalf("Props: %v", err)
	}

	argv := r.invocations()[0]
	if !contains(argv, "--timestamp=unix") {
		t.Errorf("argv = %v, want --timestamp=unix", argv)
	}
	for _, p := range showProperties {
		if !contains(argv, "--property="+p) {
			t.Errorf("argv does not request %s", p)
		}
	}
}

// TestParseShow: `systemctl show` against a pattern returns one block per unit,
// separated by a blank line.
func TestParseShow(t *testing.T) {
	t.Parallel()

	out := "Id=a.service\nSubState=running\n\nId=b.service\nSubState=dead\n\n"
	got := parseShow(out)
	want := []map[string]string{
		{"Id": "a.service", "SubState": "running"},
		{"Id": "b.service", "SubState": "dead"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("parseShow (-want +got):\n%s", diff)
	}

	// A value containing '=' survives: unit descriptions routinely do.
	one := parseShow("Description=x = y\n")
	if len(one) != 1 || one[0]["Description"] != "x = y" {
		t.Errorf("parseShow lost a value containing '=': %v", one)
	}
}

// TestExecSubscribePolls asserts the degraded contract: the first pass is a
// baseline rather than a flood of "transitions" for every already-running unit,
// and only real changes are reported afterwards.
func TestExecSubscribePolls(t *testing.T) {
	t.Parallel()

	var pass int
	var mu sync.Mutex
	r := &fakeRunner{respond: func([]string) (string, string, int, error) {
		mu.Lock()
		pass++
		n := pass
		mu.Unlock()
		if n == 1 {
			return "Id=llamaman-instance@a.service\nSubState=running\nLoadState=loaded\n", "", 0, nil
		}
		return "Id=llamaman-instance@a.service\nSubState=failed\nLoadState=loaded\n" +
			"\nId=llamaman-instance@b.service\nSubState=running\nLoadState=loaded\n", "", 0, nil
	}}
	c := newTestExec(t, model.ScopeSystem, r)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, err := c.SubscribeSubState(ctx, "llamaman-instance@*.service")
	if err != nil {
		t.Fatalf("SubscribeSubState: %v", err)
	}

	seen := map[string]string{}
	for len(seen) < 2 {
		ev := receive(t, events)
		seen[ev.Unit] = ev.SubState
	}
	want := map[string]string{
		"llamaman-instance@a.service": "failed",
		"llamaman-instance@b.service": "running",
	}
	if diff := cmp.Diff(want, seen); diff != "" {
		t.Errorf("events (-want +got):\n%s", diff)
	}
}

// TestExecSubscribeFiltersByPattern: `systemctl show` expands a pattern with its
// own matcher, so the answer is filtered again against the caller's.
func TestExecSubscribeFiltersByPattern(t *testing.T) {
	t.Parallel()

	r := &fakeRunner{respond: func([]string) (string, string, int, error) {
		return "Id=sshd.service\nSubState=running\nLoadState=loaded\n" +
			"\nId=gone.service\nSubState=dead\nLoadState=not-found\n", "", 0, nil
	}}
	c := newTestExec(t, model.ScopeSystem, r)

	got, err := c.pollSubStates(context.Background(), "llamaman-instance@*.service")
	if err != nil {
		t.Fatalf("pollSubStates: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("pollSubStates returned %v for a pattern nothing matches", got)
	}
}

// TestExecNoSystemctl: with neither candidate path present the fallback refuses
// to be constructed, which is what makes `systemd_control='unavailable'` a real
// state rather than a controller that fails on every call.
func TestExecNoSystemctl(t *testing.T) {
	restore := setSystemctlPath("")
	systemctlOverride.Lock()
	systemctlOverride.set = false
	systemctlOverride.Unlock()
	prev := systemctlCandidates
	systemctlCandidates = []string{t.TempDir() + "/absent"}
	defer func() {
		systemctlCandidates = prev
		restore()
	}()

	if _, err := NewExecController(ExecOptions{Scope: model.ScopeSystem}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("NewExecController = %v, want ErrUnavailable", err)
	}
}
