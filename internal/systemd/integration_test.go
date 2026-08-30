package systemd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// One real round trip against a live `systemd --user`, behind a
// skip-if-unavailable guard.
//
// Everything else in this package is driven through a seam, which is what makes
// the suite run on any machine — but a fake bus cannot catch the two mistakes
// that would matter most here: a D-Bus verb whose arguments systemd rejects, and
// a `systemctl show` dialect that changed. This test is the one that would.
//
// It is deliberately confined to a unit of its own, written into
// $XDG_RUNTIME_DIR/systemd/user (the runtime unit directory, which the manager
// reads and which does not survive a logout) and removed again, so it never
// touches a unit that belongs to whoever is running the tests.

func requireUserManager(t *testing.T) (*DBusController, string) {
	t.Helper()

	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeDir == "" {
		t.Skip("no $XDG_RUNTIME_DIR: there is no user manager to talk to")
	}
	echo, err := exec.LookPath("echo")
	if err != nil {
		t.Skipf("no echo binary for the test unit: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c, err := NewDBusController(ctx, Options{Scope: model.ScopeUser, Logger: quietLogger()})
	if err != nil {
		t.Skipf("no reachable user bus: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	unitDir := filepath.Join(runtimeDir, "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		t.Skipf("cannot write the runtime unit directory: %v", err)
	}

	unit := fmt.Sprintf("llamaman-selftest-%d.service", os.Getpid())
	path := filepath.Join(unitDir, unit)
	content := "[Unit]\n" +
		"Description=Llama Man controller self-test\n" +
		"[Service]\n" +
		"Type=oneshot\n" +
		"RemainAfterExit=yes\n" +
		"SyslogIdentifier=llamaman-selftest\n" +
		"ExecStart=" + echo + " llamaman-selftest-marker\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Skipf("cannot write %s: %v", path, err)
	}

	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = c.Stop(cleanup, unit)
		_ = c.ResetFailed(cleanup, unit)
		_ = os.Remove(path)
		_ = c.Reload(cleanup)
	})

	if err := c.Reload(ctx); err != nil {
		t.Skipf("daemon-reload failed on the user manager: %v", err)
	}
	return c, unit
}

// TestUserManagerRoundTrip drives the real verbs: reload, props, a blocking
// start that waits on JobRemoved, a pushed sub-state transition, a stop, and
// reset-failed.
func TestUserManagerRoundTrip(t *testing.T) {
	c, unit := requireUserManager(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	before, err := c.Props(ctx, unit)
	if err != nil {
		t.Fatalf("Props before start: %v", err)
	}
	if before.ActiveState != "inactive" {
		t.Fatalf("the self-test unit was already %q; refusing to touch it", before.ActiveState)
	}

	// Subscribe before starting, so the transition cannot be missed.
	subCtx, subCancel := context.WithCancel(ctx)
	defer subCancel()
	events, err := c.SubscribeSubState(subCtx, "llamaman-selftest-*.service")
	if err != nil {
		t.Fatalf("SubscribeSubState: %v", err)
	}

	res, err := c.Start(ctx, unit)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res != JobDone {
		t.Fatalf("Start = %q, want %q", res, JobDone)
	}

	after, err := c.Props(ctx, unit)
	if err != nil {
		t.Fatalf("Props after start: %v", err)
	}
	// Type=oneshot with RemainAfterExit=yes: active, and its main process has
	// already exited cleanly.
	if after.ActiveState != "active" {
		t.Errorf("ActiveState = %q, want active", after.ActiveState)
	}
	if after.SubState != "exited" {
		t.Errorf("SubState = %q, want exited", after.SubState)
	}
	if after.ExecMainStatus != 0 {
		t.Errorf("ExecMainStatus = %d, want 0", after.ExecMainStatus)
	}
	if after.Result != "success" {
		t.Errorf("Result = %q, want success", after.Result)
	}
	if after.ExecMainExitTimestamp.IsZero() {
		t.Error("ExecMainExitTimestamp is zero for a process that has exited")
	}

	if got := receive(t, events); got.Unit != unit {
		t.Errorf("sub-state event for %q, want %q", got.Unit, unit)
	}

	// ResetFailed runs while the unit is still loaded, which is the only state
	// it is ever called in: D93's caller names llamaman.service, and a unit
	// systemd has already garbage-collected has no failed state to clear.
	if err := c.ResetFailed(ctx, unit); err != nil {
		t.Errorf("ResetFailed: %v", err)
	}
	if res, err := c.Stop(ctx, unit); err != nil || res != JobDone {
		t.Errorf("Stop = (%q, %v)", res, err)
	}
}

// TestUserManagerNoSuchUnit: the error identity that makes D-Bus worth
// preferring, asserted against a real manager rather than a fake.
func TestUserManagerNoSuchUnit(t *testing.T) {
	c, _ := requireUserManager(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := c.Props(ctx, fmt.Sprintf("llamaman-absent-%d.service", os.Getpid()))
	if err == nil {
		t.Fatal("Props succeeded for a unit that does not exist")
	}
	// systemd answers a properties read for an unknown unit with NoSuchUnit or
	// LoadFailed depending on version; both translate to the same thing here.
	if !strings.Contains(err.Error(), "no such unit") {
		t.Errorf("Props for an absent unit = %v, want a no-such-unit error", err)
	}
}

// TestUserManagerExecControllerAgrees runs the fallback against the same live
// manager. The point is that the two controllers are interchangeable: a host
// that fell back to systemctl must see the same unit in the same state, not a
// differently-shaped answer.
func TestUserManagerExecControllerAgrees(t *testing.T) {
	c, unit := requireUserManager(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	e, err := NewExecController(ExecOptions{Scope: model.ScopeUser, Logger: quietLogger()})
	if err != nil {
		t.Skipf("no systemctl: %v", err)
	}

	if res, err := e.Start(ctx, unit); err != nil || res != JobDone {
		t.Fatalf("exec Start = (%q, %v)", res, err)
	}

	viaExec, err := e.Props(ctx, unit)
	if err != nil {
		t.Fatalf("exec Props: %v", err)
	}
	viaBus, err := c.Props(ctx, unit)
	if err != nil {
		t.Fatalf("bus Props: %v", err)
	}

	if viaExec.ActiveState != viaBus.ActiveState || viaExec.SubState != viaBus.SubState {
		t.Errorf("the two controllers disagree: exec %q/%q, bus %q/%q",
			viaExec.ActiveState, viaExec.SubState, viaBus.ActiveState, viaBus.SubState)
	}
	if viaExec.ExecMainStatus != viaBus.ExecMainStatus {
		t.Errorf("ExecMainStatus: exec %d, bus %d", viaExec.ExecMainStatus, viaBus.ExecMainStatus)
	}
	// The exit instant is second-resolution over systemctl and microsecond over
	// the bus, so they are compared at the coarser one.
	if !viaExec.ExecMainExitTimestamp.IsZero() &&
		viaExec.ExecMainExitTimestamp.Unix() != viaBus.ExecMainExitTimestamp.Unix() {
		t.Errorf("exit instant: exec %v, bus %v",
			viaExec.ExecMainExitTimestamp, viaBus.ExecMainExitTimestamp)
	}

	if _, err := e.Stop(ctx, unit); err != nil {
		t.Errorf("exec Stop: %v", err)
	}

	if _, err := e.Props(ctx, fmt.Sprintf("llamaman-absent-%d.service", os.Getpid())); err == nil {
		t.Error("exec Props succeeded for a unit that does not exist")
	}
}

// TestUserManagerJournalRoundTrip reads back what the self-test unit logged,
// which exercises the `journalctl -o json` decoding against a real journal
// rather than a fixture.
func TestUserManagerJournalRoundTrip(t *testing.T) {
	c, unit := requireUserManager(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if res, err := c.Start(ctx, unit); err != nil || res != JobDone {
		t.Fatalf("Start = (%q, %v)", res, err)
	}

	opts := JournalOptions{Scope: model.ScopeUser, Units: []string{unit}, Lines: 50}
	if got := ProbeJournalRead(ctx, model.ScopeUser, unit, opts); got != model.JournalOK {
		t.Skipf("journal read is %q on this host; nothing to assert", got)
	}

	// journald writes asynchronously, so the marker is waited for rather than
	// demanded on the first read.
	deadline := time.Now().Add(10 * time.Second)
	for {
		entries, err := Tail(ctx, opts)
		if err != nil {
			t.Fatalf("Tail: %v", err)
		}
		for _, e := range entries {
			if strings.Contains(e.Message, "llamaman-selftest-marker") {
				if e.Unit != unit {
					t.Errorf("entry attributed to %q, want %q — a user unit's identity is _SYSTEMD_USER_UNIT", e.Unit, unit)
				}
				if e.Realtime.IsZero() {
					t.Error("entry carries no timestamp")
				}
				if e.Cursor == "" {
					t.Error("entry carries no cursor; a log viewer could not resume")
				}
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the unit's own output never appeared in the journal (%d entries read)", len(entries))
		}
		time.Sleep(200 * time.Millisecond)
	}
}
