package app

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
	"github.com/jlbyh2o/llamaman/internal/systemd"
)

// The step-6 probe's results are persisted, and sections 3.3, 5.8 and 11.1a all
// read the columns rather than re-deriving the facts. A probe whose answers went
// nowhere would leave every one of those readers guessing.

func TestManageUnitFiles(t *testing.T) {
	t.Parallel()

	granted := &systemd.PolkitResult{ManageUnits: true, ManageUnitFiles: true}
	withheld := &systemd.PolkitResult{ManageUnits: true}
	denied := &systemd.PolkitResult{}

	// A non-nil Controller is what separates "the manager is reachable" from
	// F10; its behavior is never called here.
	reachable := systemd.Controller(&systemd.ExecController{})

	tests := []struct {
		name  string
		env   SystemdEnv
		scope model.SystemdScope
		want  bool
	}{
		{
			name:  "granted in system scope",
			env:   SystemdEnv{Control: reachable, Polkit: granted},
			scope: model.ScopeSystem,
			want:  true,
		},
		{
			name:  "withheld by --no-autostart-grant",
			env:   SystemdEnv{Control: reachable, Polkit: withheld},
			scope: model.ScopeSystem,
			want:  false,
		},
		{
			name:  "denied at boot (F9)",
			env:   SystemdEnv{Control: reachable, Polkit: denied},
			scope: model.ScopeSystem,
			want:  false,
		},
		{
			// D2: a user manager authorizes its owner unconditionally, so
			// neither CheckAuthorization call is made and both columns are NULL
			// — "not applicable", never "denied" (section 11.1a).
			name:  "user scope asks nobody and may still manage unit files",
			env:   SystemdEnv{Control: reachable},
			scope: model.ScopeUser,
			want:  true,
		},
		{
			name:  "no bus was reached, so the question was never asked",
			env:   SystemdEnv{Control: reachable},
			scope: model.ScopeSystem,
			want:  false,
		},
		{
			// F10: there is no manager to enable a unit file with.
			name:  "systemd is unreachable",
			env:   SystemdEnv{},
			scope: model.ScopeSystem,
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.env.ManageUnitFiles(tt.scope); got != tt.want {
				t.Errorf("ManageUnitFiles(%s) = %v, want %v", tt.scope, got, tt.want)
			}
		})
	}
}

// TestBootPersistsTheSystemdProbe is section 11.1 steps 6 and 10 together: what
// the probe learned reaches `runtime_info`, and a fact it did NOT learn stays
// NULL rather than becoming a zero that reads as an answer (F14).
func TestBootPersistsTheSystemdProbe(t *testing.T) {
	dir := t.TempDir()
	seedLoopback(t, dir)

	probed := make(chan SystemdOptions, 1)
	probe := func(_ context.Context, opts SystemdOptions) SystemdEnv {
		probed <- opts
		return SystemdEnv{
			ControlKind: model.ControlExec,
			// manage-units granted, manage-unit-files withheld: the
			// `--no-autostart-grant` host of section 11.1a's second row.
			Polkit:       &systemd.PolkitResult{ManageUnits: true},
			PolkitDetail: "manage-units granted; manage-unit-files withheld",
			Journal:      model.JournalDenied,
		}
	}

	ready := make(chan string, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			Logger:           quiet(),
			Notifier:         &recordingNotifier{},
			StateDirOverride: dir,
			Getenv:           func(string) string { return "" },
			Systemd:          probe,
			ReadyHook:        func(addr string) { ready <- addr },
		})
	}()

	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("Run returned before it was listening: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the daemon never became ready")
	}

	// ReadyHook fires at the port walk, which is several steps before the
	// workers start. Waiting for the supervisor's own boot write is what makes
	// the shutdown below a shutdown of a fully started daemon rather than a
	// cancellation of one that is still coming up.
	waitForHostBoot(t, dir)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want a clean shutdown", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the daemon did not shut down")
	}

	select {
	case opts := <-probed:
		if opts.Scope != model.ScopeSystem {
			t.Errorf("the probe was given scope %q, want the one step 1 resolved", opts.Scope)
		}
		if opts.OnReconnect == nil {
			t.Error("the probe was given no reconnect callback; section 5.3 requires one")
		}
	default:
		t.Fatal("the boot sequence never probed the service manager")
	}

	ri := readRuntimeInfo(t, dir)
	if ri.SystemdControl == nil || *ri.SystemdControl != model.ControlExec {
		t.Errorf("runtime_info.systemd_control = %v, want %q", ri.SystemdControl, model.ControlExec)
	}
	if ri.JournalRead == nil || *ri.JournalRead != model.JournalDenied {
		t.Errorf("runtime_info.journal_read = %v, want %q", ri.JournalRead, model.JournalDenied)
	}
	if ri.PolkitOK == nil || !*ri.PolkitOK {
		t.Errorf("runtime_info.polkit_ok = %v, want true", ri.PolkitOK)
	}
	if ri.PolkitUnitFiles == nil || *ri.PolkitUnitFiles {
		t.Errorf("runtime_info.polkit_unit_files = %v, want false", ri.PolkitUnitFiles)
	}
	if ri.PolkitDetail == nil || *ri.PolkitDetail == "" {
		t.Error("runtime_info.polkit_detail is NULL; doctor and the UI explain the mode from it")
	}
}

// TestBootWithoutAProbeLeavesTheColumnsNull is the other half of F14: a daemon
// that never asked must not answer.
func TestBootWithoutAProbeLeavesTheColumnsNull(t *testing.T) {
	dir := t.TempDir()
	seedLoopback(t, dir)
	addr := startDaemon(t, dir)
	if addr == "" {
		t.Fatal("no listener")
	}

	ri := readRuntimeInfo(t, dir)
	if ri.SystemdControl != nil {
		t.Errorf("runtime_info.systemd_control = %q with no probe wired", *ri.SystemdControl)
	}
	if ri.PolkitOK != nil || ri.PolkitUnitFiles != nil {
		t.Errorf("the polkit columns are %v/%v with no probe wired; NULL means not asked",
			ri.PolkitOK, ri.PolkitUnitFiles)
	}
}

func readRuntimeInfo(t *testing.T, dir string) model.RuntimeInfo {
	t.Helper()
	ctx := context.Background()

	st, err := store.OpenReadOnly(ctx, filepath.Join(dir, DatabaseFileName))
	if err != nil {
		t.Fatalf("reopen the store: %v", err)
	}
	defer st.Close()

	var ri model.RuntimeInfo
	if err := st.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		ri, err = st.RuntimeInfo(ctx, tx)
		return err
	}); err != nil {
		t.Fatalf("read runtime_info: %v", err)
	}
	return ri
}
