package supervisor_test

import (
	"context"
	"testing"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store/storetest"
	"github.com/jlbyh2o/llamaman/internal/supervisor"
)

// Boot reconciliation (DESIGN section 5.8), and in particular the two things
// that are easy to get subtly wrong and impossible to notice afterwards: the
// D53 autostart coupling, which fires exactly once per HOST boot, and D74's one
// relabel, which is keyed to the host boot instant rather than to the daemon's.

type bootFixture struct {
	*storetest.StateDir
	sup   *supervisor.Supervisor
	ctl   *fakeController
	ev    *recordingEvents
	clock *testClock
}

// newBootFixture seeds the instances and wires a supervisor whose host boot
// identity the test controls.
//
// Control is deliberately left nil here. Boot reconciliation's own decisions —
// the coupling, the relabel, the abandoned rows — are database work, and a live
// controller would let the reconcile pass at the end of BootReconcile issue
// starts that have nothing to do with what is being asserted.
func newBootFixture(t *testing.T, boot supervisor.HostBoot, insts ...model.Instance) *bootFixture {
	t.Helper()
	sd := storetest.NewStateDir(t, testVersionID, "")
	sd.SeedModel(t, "m-1", true)
	for _, inst := range insts {
		sd.SeedInstance(t, inst)
	}

	f := &bootFixture{
		StateDir: sd,
		ctl:      newFakeController(),
		ev:       &recordingEvents{},
		clock:    newTestClock(),
	}
	sup, err := supervisor.New(supervisor.Config{
		Store:    sd.DB,
		Settings: fakeSettings{},
		Events:   f.ev,
		StateDir: sd.Dir,
		Now:      f.clock.now,
		NewID:    (&monotonicIDs{}).next,
		Host:     func() (supervisor.HostBoot, error) { return boot, nil },
		Exe:      func(int) (string, error) { return "", context.Canceled },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	f.sup = sup
	return f
}

func (f *bootFixture) bootReconcile(t *testing.T) {
	t.Helper()
	if err := f.sup.BootReconcile(context.Background()); err != nil {
		t.Fatalf("BootReconcile: %v", err)
	}
}

func autostartInstance(id, name string, public, internal int,
	autostart bool, desired model.DesiredState) model.Instance {

	inst := storetest.NewInstance(id, name, "m-1", public, internal)
	inst.Autostart = autostart
	inst.DesiredState = desired
	return inst
}

// TestBootReconcileAppliesTheAutostartCoupling is D53, in both directions. The
// coupling exists because `autostart` is a statement about HOST BOOTS and
// `desired_state` is a statement about NOW, and they are joined at exactly one
// point: the first supervisor pass after a host boot.
func TestBootReconcileAppliesTheAutostartCoupling(t *testing.T) {
	// `autostart=1, desired_state='stopped'`: systemd has already started the
	// unit at boot, and the reconciler must agree rather than kill it a second
	// later.
	on := autostartInstance("i-on", "qwen", 8081, 21001, true, model.DesiredStopped)
	// `autostart=0, desired_state='running'`: systemd did not start the unit,
	// and "autostart off" has to actually mean off.
	off := autostartInstance("i-off", "gemma", 8082, 21002, false, model.DesiredRunning)
	// Already in agreement: no write, and no event.
	same := autostartInstance("i-same", "llama", 8083, 21003, true, model.DesiredRunning)

	hostBoot := supervisor.HostBoot{ID: "boot-new", At: time.Date(2026, 3, 1, 11, 0, 0, 0, time.UTC)}
	f := newBootFixture(t, hostBoot, on, off, same)
	f.SeedRuntimeInfo(t, "boot-old", 0, f.clock.now().Add(-30*time.Minute).UnixMilli())

	f.bootReconcile(t)

	if got := f.Instance(t, "i-on").DesiredState; got != model.DesiredRunning {
		t.Errorf("autostart=1 instance: desired_state = %q, want running", got)
	}
	if got := f.Instance(t, "i-off").DesiredState; got != model.DesiredStopped {
		t.Errorf("autostart=0 instance: desired_state = %q, want stopped", got)
	}
	if got := f.Instance(t, "i-same").DesiredState; got != model.DesiredRunning {
		t.Errorf("already-agreeing instance: desired_state = %q, want running", got)
	}

	// One events row per CHANGE, not per instance: the coupling is a claim a
	// user can check against the log rather than take on faith.
	coupled := 0
	for _, action := range f.ev.actions() {
		if action == "desired_state_coupled" {
			coupled++
		}
	}
	if coupled != 2 {
		t.Errorf("recorded %d coupling events, want 2 (one per change)", coupled)
	}

	// And ONLY THEN is the new identity written. Writing it first would make
	// the very next daemon start see equality and skip a coupling that had not
	// happened — the exact failure D53 exists to prevent.
	var gotID string
	var gotAt int64
	row := f.QueryRow(t, `SELECT host_boot_id, host_boot_at FROM runtime_info WHERE id = 1`)
	if err := row.Scan(&gotID, &gotAt); err != nil {
		t.Fatalf("read runtime_info: %v", err)
	}
	if gotID != hostBoot.ID || gotAt != hostBoot.At.UnixMilli() {
		t.Errorf("runtime_info host boot = (%q, %d), want (%q, %d)",
			gotID, gotAt, hostBoot.ID, hostBoot.At.UnixMilli())
	}
}

// TestBootReconcileLeavesDesiredStateAloneWithinOneHostBoot is the other half of
// D53, and it is the half that keeps D7 true: an instance that crashed while
// the daemon was down must be restarted when the daemon returns, which cannot
// happen if a daemon restart rewrites `desired_state` from `autostart`.
func TestBootReconcileLeavesDesiredStateAloneWithinOneHostBoot(t *testing.T) {
	// Autostart is off, but the user started it by hand five minutes ago. A
	// daemon restart must not undo that.
	inst := autostartInstance("i-1", "qwen", 8081, 21001, false, model.DesiredRunning)

	hostBoot := supervisor.HostBoot{ID: "boot-same", At: time.Date(2026, 3, 1, 11, 0, 0, 0, time.UTC)}
	f := newBootFixture(t, hostBoot, inst)
	f.SeedRuntimeInfo(t, "boot-same", hostBoot.At.UnixMilli(),
		f.clock.now().Add(-30*time.Minute).UnixMilli())

	f.bootReconcile(t)

	if got := f.Instance(t, "i-1").DesiredState; got != model.DesiredRunning {
		t.Fatalf("desired_state = %q, want running: a daemon restart is not a host boot", got)
	}
	for _, action := range f.ev.actions() {
		if action == "desired_state_coupled" {
			t.Fatal("the coupling fired within one host boot")
		}
	}
}

// TestBootReconcileRelabelsOnlyTheBootWindow is D74. The relabel is keyed to
// `host_boot_at` from /proc/stat's btime, NOT to the daemon's own `boot_at`:
// using the daemon start time made every ordinary daemon restart rewrite a
// deliberate `systemctl start` typed at a shell days ago as `autostart`, which
// defeats the honesty the whole trigger contract argues for.
func TestBootReconcileRelabelsOnlyTheBootWindow(t *testing.T) {
	auto := autostartInstance("i-auto", "qwen", 8081, 21001, true, model.DesiredRunning)
	manual := autostartInstance("i-manual", "gemma", 8082, 21002, false, model.DesiredStopped)

	hostBootAt := time.Date(2026, 3, 1, 11, 0, 0, 0, time.UTC)
	daemonBootAt := hostBootAt.Add(30 * time.Second)

	f := newBootFixture(t, supervisor.HostBoot{ID: "boot-new", At: hostBootAt}, auto, manual)
	f.SeedRuntimeInfo(t, "boot-old", 0, daemonBootAt.UnixMilli())

	rows := []struct {
		id       string
		instance string
		at       time.Time
		want     model.StartTrigger
		why      string
	}{
		{
			id: "in-window", instance: "i-auto", at: hostBootAt.Add(10 * time.Second),
			want: model.StartByAutostart,
			why:  "an autostart instance started in the boot window before the daemon came up",
		},
		{
			id: "after-daemon", instance: "i-auto", at: daemonBootAt.Add(time.Minute),
			want: model.StartByExternal,
			why:  "a hand-run start AFTER the daemon was up is a hand-run start forever",
		},
		{
			id: "before-host-boot", instance: "i-auto", at: hostBootAt.Add(-72 * time.Hour),
			want: model.StartByExternal,
			why:  "a start from three days ago is not this boot's autostart",
		},
		{
			id: "not-autostart", instance: "i-manual", at: hostBootAt.Add(10 * time.Second),
			want: model.StartByExternal,
			why:  "the instance does not have autostart enabled, so nobody asked for a boot start",
		},
	}
	for _, r := range rows {
		f.Exec(t,
			`INSERT INTO instance_starts (id, instance_id, at, trigger, config_hash, outcome, ended_at)
			 VALUES (?, ?, ?, 'external', 'hash', 'stopped', ?)`,
			r.id, r.instance, r.at.UnixMilli(), r.at.UnixMilli())
	}

	f.bootReconcile(t)

	for _, r := range rows {
		var got string
		row := f.QueryRow(t, `SELECT trigger FROM instance_starts WHERE id = ?`, r.id)
		if err := row.Scan(&got); err != nil {
			t.Fatalf("read %s: %v", r.id, err)
		}
		if model.StartTrigger(got) != r.want {
			t.Errorf("%s: trigger = %q, want %q — %s", r.id, got, r.want, r.why)
		}
	}
}

// TestBootReconcileNeverRelabelsWithinOneHostBoot is condition 1 of the three,
// and it is what bounds the ambiguity: within one host boot no relabel ever
// happens, so a hand-run start stays a hand-run start.
func TestBootReconcileNeverRelabelsWithinOneHostBoot(t *testing.T) {
	auto := autostartInstance("i-auto", "qwen", 8081, 21001, true, model.DesiredRunning)

	hostBootAt := time.Date(2026, 3, 1, 11, 0, 0, 0, time.UTC)
	f := newBootFixture(t, supervisor.HostBoot{ID: "boot-same", At: hostBootAt}, auto)
	f.SeedRuntimeInfo(t, "boot-same", hostBootAt.UnixMilli(),
		hostBootAt.Add(30*time.Second).UnixMilli())

	at := hostBootAt.Add(10 * time.Second)
	f.Exec(t,
		`INSERT INTO instance_starts (id, instance_id, at, trigger, config_hash, outcome, ended_at)
		 VALUES ('in-window', 'i-auto', ?, 'external', 'hash', 'stopped', ?)`,
		at.UnixMilli(), at.UnixMilli())

	f.bootReconcile(t)

	var got string
	if err := f.QueryRow(t, `SELECT trigger FROM instance_starts WHERE id = 'in-window'`).
		Scan(&got); err != nil {
		t.Fatal(err)
	}
	if model.StartTrigger(got) != model.StartByExternal {
		t.Errorf("trigger = %q, want external: the host boot did not change", got)
	}
}

// TestBootReconcileClosesAbandonedRows is step 3. A row left open by the
// previous daemon describes a run nobody can report the end of — unless the
// unit is still there, in which case the process it describes is still running
// and the row must stay open.
func TestBootReconcileClosesAbandonedRows(t *testing.T) {
	gone := autostartInstance("i-gone", "qwen", 8081, 21001, false, model.DesiredRunning)
	alive := autostartInstance("i-alive", "gemma", 8082, 21002, false, model.DesiredRunning)
	// A SOFT-DELETED instance with an open row is the reason the reconcile set
	// carries the open-row term at all (§3.10c): the supervisor is the only
	// writer allowed to close that row, and it gets exactly one chance.
	deleted := autostartInstance("i-deleted", "phi", 8083, 21003, false, model.DesiredStopped)

	hostBootAt := time.Date(2026, 3, 1, 11, 0, 0, 0, time.UTC)
	f := newBootFixture(t, supervisor.HostBoot{ID: "boot-same", At: hostBootAt}, gone, alive, deleted)
	f.SeedRuntimeInfo(t, "boot-same", hostBootAt.UnixMilli(), hostBootAt.UnixMilli())
	f.Exec(t, `UPDATE instances SET deleted_at = 1 WHERE id = 'i-deleted'`)

	for _, id := range []string{"i-gone", "i-alive", "i-deleted"} {
		f.Exec(t,
			`INSERT INTO instance_starts (id, instance_id, at, trigger, config_hash)
			 VALUES (?, ?, 1000, 'autostart', 'hash')`, "open-"+id, id)
	}

	// The controller is wired only for this test, because what is being
	// asserted is a decision made FROM unit properties.
	ctl := newFakeController()
	ctl.setExited("llamaman-instance@qwen.service", 139, "exit-code", hostBootAt)
	ctl.setActive("llamaman-instance@gemma.service", 999)
	ctl.setExited("llamaman-instance@phi.service", 0, "success", hostBootAt)

	sup, err := supervisor.New(supervisor.Config{
		Store:    f.DB,
		Settings: fakeSettings{},
		Events:   f.ev,
		Control:  ctl,
		Prober:   &fakeProber{},
		StateDir: f.Dir,
		Now:      f.clock.now,
		NewID:    (&monotonicIDs{}).next,
		Host: func() (supervisor.HostBoot, error) {
			return supervisor.HostBoot{ID: "boot-same", At: hostBootAt}, nil
		},
		Exe: func(int) (string, error) { return "", context.Canceled },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := sup.BootReconcile(context.Background()); err != nil {
		t.Fatalf("BootReconcile: %v", err)
	}

	cases := []struct {
		row      string
		wantOpen bool
		wantCode string
		why      string
	}{
		{"open-i-gone", false, supervisor.ErrDaemonRestarted,
			"the unit is gone, so the run ended and nothing observed how"},
		{"open-i-alive", true, "",
			"the unit is still active, so the process the row describes is still running"},
		{"open-i-deleted", false, supervisor.ErrDaemonRestarted,
			"a soft delete's own stop must still be ledgered by the supervisor"},
	}
	for _, tc := range cases {
		var outcome, code *string
		err := f.QueryRow(t, `SELECT outcome, error_code FROM instance_starts WHERE id = ?`, tc.row).
			Scan(&outcome, &code)
		if err != nil {
			t.Fatalf("read %s: %v", tc.row, err)
		}
		if tc.wantOpen {
			if outcome != nil {
				t.Errorf("%s: outcome = %q, want NULL — %s", tc.row, *outcome, tc.why)
			}
			continue
		}
		if outcome == nil || *outcome != string(model.OutcomeFailed) {
			t.Errorf("%s: outcome = %v, want failed — %s", tc.row, outcome, tc.why)
		}
		if code == nil || *code != tc.wantCode {
			t.Errorf("%s: error_code = %v, want %q", tc.row, code, tc.wantCode)
		}
	}
}
