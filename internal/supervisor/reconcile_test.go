package supervisor_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
	"github.com/jlbyh2o/llamaman/internal/store/storetest"
	"github.com/jlbyh2o/llamaman/internal/supervisor"
	"github.com/jlbyh2o/llamaman/internal/systemd"
)

// The reconcile loop of DESIGN section 5.8, against a REAL database and a faked
// systemd.
//
// The database is real because half of what is being asserted is enforced by
// the schema rather than by the code: `idx_instance_starts_open` is what makes
// "at most one open row" true, and a fake store would let a bug that writes two
// pass silently. systemd is faked because the alternative is a suite that only
// runs on a host with a user manager — see integration_test.go for the half
// that does run against real processes.

const testVersionID = "b10621-cpu-bin"

type fixture struct {
	*storetest.StateDir
	sup   *supervisor.Supervisor
	ctl   *fakeController
	probe *fakeProber
	ev    *recordingEvents
	clock *testClock
	ids   *monotonicIDs
	inst  model.Instance
	unit  string
}

// newFixture builds one instance, wanted running, with a fake unit and a
// controllable clock.
func newFixture(t *testing.T, mutate func(*model.Instance)) *fixture {
	t.Helper()
	sd := storetest.NewStateDir(t, testVersionID, "")
	sd.SeedModel(t, "m-1", true)

	inst := storetest.NewInstance("i-1", "qwen", "m-1", 8081, 21001)
	inst.DesiredState = model.DesiredRunning
	if mutate != nil {
		mutate(&inst)
	}
	sd.SeedInstance(t, inst)

	f := &fixture{
		StateDir: sd,
		ctl:      newFakeController(),
		probe:    &fakeProber{},
		ev:       &recordingEvents{},
		clock:    newTestClock(),
		ids:      &monotonicIDs{},
		inst:     inst,
		unit:     inst.UnitName,
	}
	// A never-started unit is loaded and dead, which is what systemd reports
	// for a template instance whose unit file is installed.
	f.ctl.setUnit(f.unit, unitDead())

	sup, err := supervisor.New(supervisor.Config{
		Store:    sd.DB,
		Settings: fakeSettings{"instances.health_poll_sec": 5, "instances.start_timeout_sec": 900},
		Events:   f.ev,
		Control:  f.ctl,
		Prober:   f.probe,
		StateDir: sd.Dir,
		Now:      f.clock.now,
		NewID:    f.ids.next,
		Host: func() (supervisor.HostBoot, error) {
			return supervisor.HostBoot{ID: "boot-1", At: f.clock.now().Add(-time.Hour)}, nil
		},
		// Nothing in these tests reads /proc.
		Exe: func(int) (string, error) { return "", context.Canceled },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	f.sup = sup
	return f
}

// unitDead is what systemd reports for a template instance whose unit file is
// installed and which has never been started: loaded, inactive, dead.
func unitDead() systemd.UnitProps {
	return systemd.UnitProps{ActiveState: "inactive", SubState: "dead"}
}

func (f *fixture) reconcile(t *testing.T) {
	t.Helper()
	if err := f.sup.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

// launchLikeInstanceExec is what `instance-exec` step 3 does, and nothing more:
// consume the hand-off columns and open the ledger row. It stands in for the
// launcher in the tests that are about the SUPERVISOR's half of the protocol;
// the launcher's own half is asserted in internal/cli, and the two halves meet
// for real in integration_test.go.
func (f *fixture) launchLikeInstanceExec(t *testing.T, pid uint32) string {
	t.Helper()
	id := f.ids.next(f.clock.now())
	err := f.DB.Write(context.Background(), func(ctx context.Context, tx store.Tx) error {
		trigger, override, err := f.DB.TakePendingStart(ctx, tx, f.inst.ID)
		if err != nil {
			return err
		}
		row := model.InstanceStart{
			ID:                  id,
			InstanceID:          f.inst.ID,
			At:                  f.clock.now().UnixMilli(),
			Trigger:             model.StartByExternal,
			ConfigHash:          f.inst.ConfigHash,
			EffectiveConfigHash: &f.inst.ConfigHash,
			ArgvJSON:            ptr(`["llama-server"]`),
			LlamacppVersionID:   ptr(testVersionID),
			OverrideJSON:        override,
		}
		if trigger != nil {
			row.Trigger = model.StartTrigger(*trigger)
		}
		return f.DB.InsertInstanceStart(ctx, tx, row)
	})
	if err != nil {
		t.Fatalf("open the ledger row: %v", err)
	}
	f.ctl.setActive(f.unit, pid)
	return id
}

func (f *fixture) openRow(t *testing.T) *model.InstanceStart {
	t.Helper()
	for _, r := range f.Starts(t, f.inst.ID) {
		if r.Outcome == nil {
			row := r
			return &row
		}
	}
	return nil
}

func ptr[T any](v T) *T { return &v }

// TestStartLedgerLifecycle walks one successful run end to end: the supervisor
// stamps a trigger and starts, the launcher's row is stamped ready at the first
// 200 WITHOUT being closed (D63), and the stop the user asked for closes it
// `stopped` even though nothing about the exit says so.
func TestStartLedgerLifecycle(t *testing.T) {
	f := newFixture(t, nil)

	// Pass 1: desired running, unit dead. One corrective action — a start —
	// preceded by the trigger stamp that makes provenance honest.
	f.reconcile(t)
	if got := f.ctl.callsToStart(); len(got) != 1 || got[0] != f.unit {
		t.Fatalf("start calls = %v, want exactly one for %s", got, f.unit)
	}
	if pending := f.Instance(t, f.inst.ID).PendingTrigger; pending == nil ||
		*pending != model.TriggerSupervisorRestart {
		t.Fatalf("pending_trigger = %v, want %q — a start nobody stamped is recorded as external",
			pending, model.TriggerSupervisorRestart)
	}

	// The launcher runs, consumes the stamp and opens the row.
	f.clock.advance(time.Second)
	rowID := f.launchLikeInstanceExec(t, 4242)
	if trig := f.Starts(t, f.inst.ID)[0].Trigger; trig != model.StartBySupervisorRestart {
		t.Errorf("trigger = %q, want %q", trig, model.StartBySupervisorRestart)
	}

	// Pass 2: active, still loading the model.
	f.probe.set(http.StatusServiceUnavailable)
	f.clock.advance(time.Second)
	f.reconcile(t)
	if got := f.Status(t, f.inst.ID).State; got != model.InstanceLoading {
		t.Fatalf("state = %q, want %q", got, model.InstanceLoading)
	}
	if row := f.openRow(t); row == nil || row.ReadyAt != nil {
		t.Error("ready_at must not be stamped before the first 200")
	}

	// Pass 3: the first 200. `ready_at` is stamped, `applied_config_hash` is
	// copied from the row — the ONLY write of that column anywhere — and
	// `outcome` stays NULL, because reaching ready is an event within a run,
	// not the end of one.
	f.probe.set(http.StatusOK)
	f.clock.advance(time.Second)
	f.reconcile(t)

	st := f.Status(t, f.inst.ID)
	if st.State != model.InstanceReady {
		t.Fatalf("state = %q, want ready", st.State)
	}
	if st.AppliedConfigHash == nil || *st.AppliedConfigHash != f.inst.ConfigHash {
		t.Errorf("applied_config_hash = %v, want %q", st.AppliedConfigHash, f.inst.ConfigHash)
	}
	row := f.openRow(t)
	if row == nil {
		t.Fatal("the run's row was closed by reaching ready; `ready` is not an outcome (D63)")
	}
	if row.ID != rowID || row.ReadyAt == nil {
		t.Errorf("ready_at = %v on row %s, want a stamp on %s", row.ReadyAt, row.ID, rowID)
	}

	// A run that has served for a minute restarts the crash-loop window, at its
	// own ready_at rather than at now.
	f.clock.advance(90 * time.Second)
	f.reconcile(t)
	if got, want := f.Status(t, f.inst.ID).RestartWindowResetAt, *row.ReadyAt; got != want {
		t.Errorf("restart_window_reset_at = %d, want the run's ready_at %d", got, want)
	}

	// The user stops it. The supervisor issues the stop; the row is still open,
	// because the process it describes is still running.
	f.Exec(t, `UPDATE instances SET desired_state = 'stopped' WHERE id = ?`, f.inst.ID)
	f.clock.advance(time.Second)
	f.reconcile(t)
	if got := f.ctl.callsToStop(); len(got) != 1 {
		t.Fatalf("stop calls = %v, want exactly one", got)
	}
	if f.openRow(t) == nil {
		t.Error("the row was closed while the unit was still active")
	}

	// llama-server exits non-zero on SIGINT. A stop the user asked for is not a
	// failure, whatever the status says.
	f.clock.advance(time.Second)
	f.ctl.setExited(f.unit, 130, "success", f.clock.now())
	f.reconcile(t)

	if f.openRow(t) != nil {
		t.Fatal("the row is still open after the unit went away")
	}
	closedRow := f.Starts(t, f.inst.ID)[0]
	if closedRow.Outcome == nil || *closedRow.Outcome != model.OutcomeStopped {
		t.Errorf("outcome = %v, want stopped — a stop the user asked for is not a failure",
			closedRow.Outcome)
	}
	if closedRow.EndedAt == nil {
		t.Error("ended_at is NULL on a closed row")
	}
	if got := f.Status(t, f.inst.ID).State; got != model.InstanceStopped {
		t.Errorf("state = %q, want stopped", got)
	}
	// One row for one run, start to finish.
	if n := len(f.Starts(t, f.inst.ID)); n != 1 {
		t.Errorf("the run produced %d ledger rows, want 1", n)
	}
}

// TestUnrequestedCleanExitIsFailedButStopped is the pair of answers §2.8 insists
// are two different questions: an unrequested exit puts the STATE in `failed`
// whatever the code, and the exit code puts the LEDGER in `stopped` when it is
// clean. The second is what makes `on-failure` decline; the first is what makes
// the `clean_exit` reason visible at all.
func TestUnrequestedCleanExitIsFailedButStopped(t *testing.T) {
	f := newFixture(t, nil)
	f.reconcile(t)
	f.launchLikeInstanceExec(t, 4242)
	f.probe.set(http.StatusOK)
	f.clock.advance(time.Second)
	f.reconcile(t)

	// llama-server exits 0 on an internal error, with nobody having asked.
	f.clock.advance(time.Second)
	f.ctl.setExited(f.unit, 0, "success", f.clock.now())
	f.reconcile(t)

	row := f.Starts(t, f.inst.ID)[0]
	if row.Outcome == nil || *row.Outcome != model.OutcomeStopped {
		t.Errorf("outcome = %v, want stopped: the exit was clean", row.Outcome)
	}
	if got := f.Status(t, f.inst.ID).State; got != model.InstanceFailed {
		t.Fatalf("state = %q, want failed: nobody asked for this exit", got)
	}

	// And `on-failure` declines it, naming the reason.
	f.clock.advance(time.Minute)
	f.reconcile(t)
	if got := f.ctl.callsToStart(); len(got) != 1 {
		t.Errorf("start calls = %v, want no restart after a clean exit under on-failure", got)
	}
	inhibited := findInhibited(f.Starts(t, f.inst.ID))
	if inhibited == nil || inhibited.ErrorCode == nil ||
		*inhibited.ErrorCode != string(model.InhibitCleanExit) {
		t.Errorf("inhibited row = %+v, want error_code %q", inhibited, model.InhibitCleanExit)
	}
}

// TestRestartPolicies proves each of the three, from the same starting state:
// an instance that is `failed` after a run that closed the way the row says.
func TestRestartPolicies(t *testing.T) {
	cases := []struct {
		name       string
		policy     model.RestartPolicy
		lastRun    model.StartOutcome
		exitCode   int64
		wantStart  bool
		wantReason model.InhibitReason
	}{
		{"always restarts a clean exit", model.RestartAlways, model.OutcomeStopped, 0, true, ""},
		{"always restarts a crash", model.RestartAlways, model.OutcomeFailed, 139, true, ""},
		{"on-failure restarts a crash", model.RestartOnFailure, model.OutcomeFailed, 139, true, ""},
		{"on-failure declines a clean exit", model.RestartOnFailure, model.OutcomeStopped, 0, false,
			model.InhibitCleanExit},
		{"never declines a crash", model.RestartNever, model.OutcomeFailed, 139, false,
			model.InhibitPolicyNever},
		{"never declines a clean exit", model.RestartNever, model.OutcomeStopped, 0, false,
			model.InhibitPolicyNever},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, func(i *model.Instance) { i.RestartPolicy = tc.policy })

			// A completed run in the past, and a unit that is gone.
			at := f.clock.now().Add(-time.Minute)
			f.Exec(t,
				`INSERT INTO instance_starts
				   (id, instance_id, at, trigger, config_hash, outcome, exit_code, ended_at)
				 VALUES ('prev', ?, ?, 'user', ?, ?, ?, ?)`,
				f.inst.ID, at.UnixMilli(), f.inst.ConfigHash, string(tc.lastRun),
				tc.exitCode, at.UnixMilli())
			f.Exec(t, `UPDATE instance_status SET state = 'failed' WHERE instance_id = ?`, f.inst.ID)
			f.ctl.setExited(f.unit, int32(tc.exitCode), "exit-code", at)

			f.reconcile(t)

			starts := f.ctl.callsToStart()
			if tc.wantStart {
				if len(starts) != 1 {
					t.Fatalf("start calls = %v, want one", starts)
				}
				if r := findInhibited(f.Starts(t, f.inst.ID)); r != nil {
					t.Errorf("a start was issued AND a refusal recorded: %+v", r)
				}
				return
			}
			if len(starts) != 0 {
				t.Fatalf("start calls = %v, want none", starts)
			}
			row := findInhibited(f.Starts(t, f.inst.ID))
			if row == nil {
				t.Fatal("no refusal was recorded; the history must show why nothing happened")
			}
			if row.ErrorCode == nil || *row.ErrorCode != string(tc.wantReason) {
				t.Errorf("inhibit reason = %v, want %q", row.ErrorCode, tc.wantReason)
			}
			if row.ExitCode != nil {
				t.Errorf("exit_code = %d on a refusal; no execve happened", *row.ExitCode)
			}
		})
	}
}

// TestCrashLoopCutoffTripsAndInhibits is D8/D64: more than `restart_max` FAILED
// starts inside the window latches `crash-looping`, and the refusal is recorded
// once per episode rather than once per pass.
func TestCrashLoopCutoffTripsAndInhibits(t *testing.T) {
	f := newFixture(t, func(i *model.Instance) {
		i.RestartPolicy = model.RestartAlways
		i.RestartMax = 2
		i.RestartWindowSec = 600
	})

	base := f.clock.now().Add(-5 * time.Minute)
	for i, spec := range []struct {
		outcome   model.StartOutcome
		exit      int64
		errorCode string
	}{
		{model.OutcomeFailed, 139, "unit_failed"},
		{model.OutcomeFailed, 139, "unit_failed"},
		{model.OutcomeFailed, 72, "model_missing"},
		// A user restart in the middle is NOT a crash: `stopped` rows are never
		// counted, which is what stops six restarts while tuning flags from
		// looking like a crash loop.
		{model.OutcomeStopped, 0, ""},
		// A schema-gate failure is a property of the daemon's upgrade state, not
		// of this instance, and is excluded by error_code (§5.6a).
		{model.OutcomeFailed, 75, "schema_mismatch"},
		// A run whose outcome nothing observed is a guess, not a failure.
		{model.OutcomeFailed, 0, "launcher_superseded"},
	} {
		at := base.Add(time.Duration(i) * time.Second)
		var code any
		if spec.errorCode != "" {
			code = spec.errorCode
		}
		f.Exec(t,
			`INSERT INTO instance_starts
			   (id, instance_id, at, trigger, config_hash, outcome, exit_code, error_code, ended_at)
			 VALUES (?, ?, ?, 'supervisor_restart', ?, ?, ?, ?, ?)`,
			"prev-"+string(rune('a'+i)), f.inst.ID, at.UnixMilli(), f.inst.ConfigHash,
			string(spec.outcome), spec.exit, code, at.UnixMilli())
	}
	f.Exec(t, `UPDATE instance_status SET state = 'failed' WHERE instance_id = ?`, f.inst.ID)
	f.ctl.setExited(f.unit, 139, "exit-code", base)

	// Three countable failures against restart_max=2: over the line.
	f.reconcile(t)

	if got := f.Status(t, f.inst.ID).State; got != model.InstanceCrashLooping {
		t.Fatalf("state = %q, want crash-looping", got)
	}
	if starts := f.ctl.callsToStart(); len(starts) != 0 {
		t.Fatalf("start calls = %v, want none: `crash-looping` overrides `always`", starts)
	}
	refusals := countInhibited(f.Starts(t, f.inst.ID))
	if refusals != 1 {
		t.Fatalf("recorded %d refusals, want 1", refusals)
	}

	// The reconciler runs every health_poll_sec. Fifty more passes must record
	// no more rows: an unconditional write would add ~17 000 a day against a
	// 500-row cap and bury the history the user came to read.
	for i := 0; i < 50; i++ {
		f.clock.advance(5 * time.Second)
		f.reconcile(t)
	}
	if got := countInhibited(f.Starts(t, f.inst.ID)); got != 1 {
		t.Errorf("recorded %d refusals after 50 more passes, want 1", got)
	}
	if got := f.Status(t, f.inst.ID).State; got != model.InstanceCrashLooping {
		t.Errorf("state = %q after 50 passes, want the latch to hold", got)
	}

	// The latch clears only through the recovery endpoints, and then the
	// instance starts again.
	err := f.DB.Write(context.Background(), func(ctx context.Context, tx store.Tx) error {
		_, err := f.DB.ClearCrashLoopLatch(ctx, tx, f.inst.ID, f.clock.now().UnixMilli())
		return err
	})
	if err != nil {
		t.Fatalf("ClearCrashLoopLatch: %v", err)
	}
	f.clock.advance(time.Second)
	f.reconcile(t)
	if got := f.ctl.callsToStart(); len(got) != 1 {
		t.Errorf("start calls = %v, want one after Reset failed", got)
	}
}

// TestSynthesizedRowsForLauncherExitsBeforeTheRow is the supervisor's half of
// §5.6's "no row" table: exits 70 and 75 are synthesized from the unit's
// ExecMainStatus, exit 64 is not synthesized at all, and a unit that stays
// failed produces ONE row rather than one per pass.
func TestSynthesizedRowsForLauncherExitsBeforeTheRow(t *testing.T) {
	cases := []struct {
		name     string
		exit     int32
		wantRows int
		wantCode string
	}{
		{"70 launcher db unavailable", supervisor.ExitDBUnavailable, 1,
			supervisor.ErrLauncherDBUnavailable},
		{"75 schema gate", supervisor.ExitSchemaMismatch, 1, supervisor.ErrSchemaMismatch},
		{"64 instance missing writes nothing", supervisor.ExitInstanceMissing, 0, ""},
		{"72 closed its own row", supervisor.ExitModelMissing, 0, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, func(i *model.Instance) { i.RestartPolicy = model.RestartNever })
			exitAt := f.clock.now().Add(-time.Minute)
			f.ctl.setExited(f.unit, tc.exit, "exit-code", exitAt)

			f.reconcile(t)
			f.clock.advance(5 * time.Second)
			f.reconcile(t)

			var rows []model.InstanceStart
			for _, r := range f.Starts(t, f.inst.ID) {
				if r.Outcome != nil && *r.Outcome == model.OutcomeFailed {
					rows = append(rows, r)
				}
			}
			if len(rows) != tc.wantRows {
				t.Fatalf("got %d synthesized rows after two passes, want %d: %+v",
					len(rows), tc.wantRows, rows)
			}
			if tc.wantRows == 0 {
				return
			}
			row := rows[0]
			if row.ErrorCode == nil || *row.ErrorCode != tc.wantCode {
				t.Errorf("error_code = %v, want %q", row.ErrorCode, tc.wantCode)
			}
			if row.ExitCode == nil || int32(*row.ExitCode) != tc.exit {
				t.Errorf("exit_code = %v, want %d", row.ExitCode, tc.exit)
			}
			// Nothing was rendered, so these three are NULL — the one respect in
			// which a synthesized row differs from a launcher-written one.
			if row.ArgvJSON != nil || row.EffectiveConfigHash != nil || row.LlamacppVersionID != nil {
				t.Error("a synthesized row must carry no argv, hash or version: nothing was rendered")
			}
		})
	}
}

// TestSchemaGateFailuresDoNotTripTheCutoff pins §5.6a's exclusion. Five
// instances failing the gate during a slow migration must not all reach
// `crash-looping` and require a manual Reset failed for a condition that fixed
// itself.
func TestSchemaGateFailuresDoNotTripTheCutoff(t *testing.T) {
	f := newFixture(t, func(i *model.Instance) {
		i.RestartPolicy = model.RestartAlways
		i.RestartMax = 2
	})

	base := f.clock.now().Add(-time.Minute)
	for i := 0; i < 6; i++ {
		at := base.Add(time.Duration(i) * time.Second)
		f.Exec(t,
			`INSERT INTO instance_starts
			   (id, instance_id, at, trigger, config_hash, outcome, exit_code, error_code, ended_at)
			 VALUES (?, ?, ?, 'autostart', ?, 'failed', 75, 'schema_mismatch', ?)`,
			"gate-"+string(rune('a'+i)), f.inst.ID, at.UnixMilli(), f.inst.ConfigHash, at.UnixMilli())
	}
	f.Exec(t, `UPDATE instance_status SET state = 'failed' WHERE instance_id = ?`, f.inst.ID)
	f.ctl.setUnit(f.unit, unitDead())

	f.reconcile(t)

	if got := f.Status(t, f.inst.ID).State; got == model.InstanceCrashLooping {
		t.Fatal("six schema-gate failures latched crash-looping; the daemon's own arrival is the fix")
	}
	if got := f.ctl.callsToStart(); len(got) != 1 {
		t.Errorf("start calls = %v, want one: the supervisor starts it as soon as it is up", got)
	}
}

// TestPortConflictIsReassignedRatherThanRetried is F5. The supervisor — not the
// launcher — allocates the next free port, and does it exactly once per
// conflict.
func TestPortConflictIsReassignedRatherThanRetried(t *testing.T) {
	f := newFixture(t, nil)

	at := f.clock.now().Add(-time.Minute)
	f.Exec(t,
		`INSERT INTO instance_starts
		   (id, instance_id, at, trigger, config_hash, outcome, exit_code, error_code,
		    detail_json, ended_at)
		 VALUES ('conflict', ?, ?, 'user', ?, 'failed', 78, 'port_conflict', ?, ?)`,
		f.inst.ID, at.UnixMilli(), f.inst.ConfigHash,
		`{"internal_port":21001,"public_port":8081}`, at.UnixMilli())
	f.Exec(t, `UPDATE instance_status SET state = 'failed' WHERE instance_id = ?`, f.inst.ID)
	f.ctl.setExited(f.unit, 78, "exit-code", at)

	sup, err := supervisor.New(supervisor.Config{
		Store:    f.DB,
		Settings: fakeSettings{"instances.internal_port_min": 21000, "instances.internal_port_max": 21999},
		Events:   f.ev,
		Control:  f.ctl,
		Prober:   f.probe,
		StateDir: f.Dir,
		Now:      f.clock.now,
		NewID:    f.ids.next,
		// Every candidate but the one it already has is free.
		Probe: func(_ string, port int) bool { return port != 21001 },
		Host:  func() (supervisor.HostBoot, error) { return supervisor.HostBoot{ID: "boot-1"}, nil },
		Exe:   func(int) (string, error) { return "", context.Canceled },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := sup.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	after := f.Instance(t, f.inst.ID)
	if after.InternalPort == f.inst.InternalPort {
		t.Fatalf("internal_port is still %d; the supervisor must reassign after exit 78",
			after.InternalPort)
	}
	if after.InternalPort < 21000 || after.InternalPort > 21999 {
		t.Errorf("internal_port = %d, want a port from the pool", after.InternalPort)
	}
	// D52: an allocation detail is not a configuration edit.
	if after.Generation != f.inst.Generation {
		t.Errorf("generation = %d, want %d — a port reassignment is not an edit",
			after.Generation, f.inst.Generation)
	}
	if after.ConfigHash != f.inst.ConfigHash {
		t.Error("config_hash changed; the listener identity is elided from it (D52)")
	}
	// The reassignment was this pass's ONE action.
	if got := f.ctl.callsToStart(); len(got) != 0 {
		t.Errorf("start calls = %v, want none in the pass that moved the port", got)
	}
}

// TestDegradedAfterThreeHealthFailures pins the `ready → degraded` edge, and the
// rule that a run which recovers has ONE ledger row rather than three.
func TestDegradedAfterThreeHealthFailures(t *testing.T) {
	f := newFixture(t, nil)
	f.reconcile(t)
	f.launchLikeInstanceExec(t, 4242)
	f.probe.set(http.StatusOK)
	f.clock.advance(time.Second)
	f.reconcile(t)

	f.probe.unreachable()
	for i := 1; i <= 2; i++ {
		f.clock.advance(time.Second)
		f.reconcile(t)
		if got := f.Status(t, f.inst.ID).State; got != model.InstanceReady {
			t.Fatalf("after %d failures state = %q, want ready: three CONSECUTIVE failures degrade",
				i, got)
		}
	}
	f.clock.advance(time.Second)
	f.reconcile(t)
	if got := f.Status(t, f.inst.ID).State; got != model.InstanceDegraded {
		t.Fatalf("state = %q, want degraded", got)
	}

	f.probe.set(http.StatusOK)
	f.clock.advance(time.Second)
	f.reconcile(t)
	if got := f.Status(t, f.inst.ID).State; got != model.InstanceReady {
		t.Errorf("state = %q, want ready again", got)
	}
	if n := len(f.Starts(t, f.inst.ID)); n != 1 {
		t.Errorf("a run that recovered produced %d rows, want 1", n)
	}
}

// TestRebuildingRuntimeWaitsRatherThanStarting is D78: while a forced rebuild
// has moved the active row out of `ready`, no start is attempted at all, so the
// launcher's exit 69 is the backstop rather than the normal path.
func TestRebuildingRuntimeWaitsRatherThanStarting(t *testing.T) {
	f := newFixture(t, nil)
	f.SetVersionState(t, testVersionID, model.VersionBuilding)

	f.reconcile(t)
	if got := f.ctl.callsToStart(); len(got) != 0 {
		t.Fatalf("start calls = %v, want none while the active version is rebuilding", got)
	}
	if r := findInhibited(f.Starts(t, f.inst.ID)); r != nil {
		t.Error("waiting for a rebuild was recorded as a refusal; it is not one")
	}

	// The rebuild finishes and the supervisor starts it on its own.
	f.SetVersionState(t, testVersionID, model.VersionReady)
	f.clock.advance(5 * time.Second)
	f.reconcile(t)
	if got := f.ctl.callsToStart(); len(got) != 1 {
		t.Errorf("start calls = %v, want one once the rebuild finished", got)
	}
}

func findInhibited(rows []model.InstanceStart) *model.InstanceStart {
	for i := range rows {
		if rows[i].Outcome != nil && *rows[i].Outcome == model.OutcomeInhibited {
			return &rows[i]
		}
	}
	return nil
}

func countInhibited(rows []model.InstanceStart) int {
	n := 0
	for _, r := range rows {
		if r.Outcome != nil && *r.Outcome == model.OutcomeInhibited {
			n++
		}
	}
	return n
}
