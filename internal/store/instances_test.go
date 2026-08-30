package store

import (
	"context"
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/jlbyh2o/llamaman/internal/model"
)

// instances, instance_status and instance_starts queries (DESIGN section 2.8).

// newInstance builds a config row with the defaults the schema declares, so each
// test names only the columns it is about.
func newInstance(id, name string, public, internal int) model.Instance {
	return model.Instance{
		ID:               id,
		Name:             name,
		PublicPort:       public,
		InternalPort:     internal,
		AuthMode:         model.AuthToken,
		RestartPolicy:    model.RestartOnFailure,
		RestartMax:       5,
		RestartWindowSec: 600,
		FlagsJSON:        `{"ctx_size":8192}`,
		ConfigHash:       "hash-" + id,
		DesiredState:     model.DesiredStopped,
		DraftValidation:  model.DraftOK,
		UnitName:         "llamaman-instance@" + name + ".service",
		Generation:       1,
		CreatedAt:        1000,
		UpdatedAt:        1000,
	}
}

// seedInstance writes a config row and its status row in one transaction, which
// is the pairing §2.8 requires of every creator.
func seedInstance(t *testing.T, s *Store, inst model.Instance) {
	t.Helper()
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		if err := s.InsertInstance(ctx, tx, inst); err != nil {
			return err
		}
		return s.InsertInstanceStatus(ctx, tx, model.InstanceStatus{
			InstanceID:     inst.ID,
			State:          model.InstanceUnknown,
			LastChangeAt:   inst.CreatedAt,
			GPUAttribution: model.AttributionUnknown,
		})
	})
}

func TestInstanceRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	want := newInstance("i-1", "qwen", 8081, 21001)
	want.DisplayName = ptr("Qwen 3 8B")
	want.Description = ptr("the everyday model")
	want.ExtraFlags = "--log-colors"
	want.Autostart = true
	seedInstance(t, s, want)

	got, err := s.Instance(ctx, s.RO, "i-1")
	if err != nil {
		t.Fatalf("Instance: %v", err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("instance mismatch (-want +got):\n%s", diff)
	}

	byName, err := s.InstanceByName(ctx, s.RO, "qwen")
	if err != nil {
		t.Fatalf("InstanceByName: %v", err)
	}
	if byName.ID != want.ID {
		t.Errorf("InstanceByName returned %s, want %s", byName.ID, want.ID)
	}

	if _, err := s.Instance(ctx, s.RO, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a missing instance = %v, want ErrNotFound", err)
	}
}

// TestSoftDeleteKeepsTheRowAndFreesTheName is D68 through the query layer: the
// row and its history survive, the name and both ports are immediately reusable,
// and the deleted instance disappears from the default listing.
func TestSoftDeleteKeepsTheRowAndFreesTheName(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	seedInstance(t, s, newInstance("i-1", "qwen", 8081, 21001))

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		ok, err := s.SoftDeleteInstance(ctx, tx, "i-1", 2000)
		if err != nil {
			return err
		}
		if !ok {
			t.Error("SoftDeleteInstance matched no row")
		}
		return nil
	})

	// The row is still there, and still readable.
	deleted, err := s.Instance(ctx, s.RO, "i-1")
	if err != nil {
		t.Fatalf("a soft-deleted instance must stay readable: %v", err)
	}
	if !deleted.Deleted() {
		t.Error("deleted_at was not stamped")
	}
	if deleted.DesiredState != model.DesiredStopped {
		t.Error("a delete must leave desired_state stopped")
	}

	// It no longer holds its name…
	if _, err := s.InstanceByName(ctx, s.RO, "qwen"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a deleted instance still answers to its name: %v", err)
	}
	// …nor either port.
	holders, err := s.InstancePortHolders(ctx, s.RO)
	if err != nil {
		t.Fatalf("InstancePortHolders: %v", err)
	}
	if len(holders) != 0 {
		t.Errorf("a deleted instance still holds ports: %+v", holders)
	}

	// And the name and both ports can be taken again immediately.
	seedInstance(t, s, newInstance("i-2", "qwen", 8081, 21001))

	live, err := s.Instances(ctx, s.RO, InstanceFilter{})
	if err != nil {
		t.Fatalf("Instances: %v", err)
	}
	if len(live) != 1 || live[0].ID != "i-2" {
		t.Errorf("the default listing returned %d rows, want only the live one", len(live))
	}

	all, err := s.Instances(ctx, s.RO, InstanceFilter{IncludeDeleted: true})
	if err != nil {
		t.Fatalf("Instances(include_deleted): %v", err)
	}
	if len(all) != 2 {
		t.Errorf("?include_deleted=true returned %d rows, want 2", len(all))
	}
}

// TestUpdateInstanceConfigIsGuardedByGeneration is §3's optimistic concurrency.
func TestUpdateInstanceConfigIsGuardedByGeneration(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedInstance(t, s, newInstance("i-1", "qwen", 8081, 21001))

	edit := newInstance("i-1", "qwen", 8081, 21001)
	edit.FlagsJSON = `{"ctx_size":4096}`
	edit.UpdatedAt = 2000

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		ok, err := s.UpdateInstanceConfig(ctx, tx, edit)
		if err != nil {
			return err
		}
		if !ok {
			t.Fatal("the first edit did not match")
		}
		return nil
	})

	after, err := s.Instance(ctx, s.RO, "i-1")
	if err != nil {
		t.Fatal(err)
	}
	if after.Generation != 2 {
		t.Errorf("generation = %d, want 2", after.Generation)
	}

	// The same edit again, still carrying generation 1, must not match.
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		ok, err := s.UpdateInstanceConfig(ctx, tx, edit)
		if err != nil {
			return err
		}
		if ok {
			t.Error("a stale generation was accepted — 409 conflict_generation would never fire")
		}
		return nil
	})
}

// TestExceptionalWritersDoNotBumpGeneration is §2.8's writer table as an
// assertion: a housekeeping write must never invalidate an admin's open edit
// form.
func TestExceptionalWritersDoNotBumpGeneration(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedInstance(t, s, newInstance("i-1", "qwen", 8081, 21001))

	// Each writer gets its own instant: they all bump `updated_at`, so reusing
	// one timestamp would make the second assertion pass vacuously.
	writers := []struct {
		name  string
		at    int64
		write func(context.Context, Tx, int64) error
	}{
		{"the supervisor reassigning the internal port after exit 78", 2000,
			func(ctx context.Context, tx Tx, at int64) error {
				_, err := s.ReassignInternalPort(ctx, tx, "i-1", 21099, at)
				return err
			}},
		{"the daemon stamping a pending start", 2100,
			func(ctx context.Context, tx Tx, at int64) error {
				_, err := s.StampPendingStart(ctx, tx, "i-1", model.TriggerUser, nil, at)
				return err
			}},
		{"the supervisor writing desired_state at a host boot", 2200,
			func(ctx context.Context, tx Tx, at int64) error {
				_, err := s.SetInstanceDesiredState(ctx, tx, "i-1", model.DesiredRunning, at)
				return err
			}},
		{"the models service resolving a deferred draft check", 2300,
			func(ctx context.Context, tx Tx, at int64) error {
				_, err := s.SetInstanceDraftValidation(ctx, tx, "i-1", model.DraftDeferred, at)
				return err
			}},
		{"llama.cpp activation recomputing config_hash", 2400,
			func(ctx context.Context, tx Tx, at int64) error {
				_, err := s.SetInstanceConfigHash(ctx, tx, "i-1", "hash-new", at)
				return err
			}},
		{"the autostart endpoint", 2500,
			func(ctx context.Context, tx Tx, at int64) error {
				_, err := s.SetInstanceAutostart(ctx, tx, "i-1", true, at)
				return err
			}},
	}

	for _, w := range writers {
		t.Run(w.name, func(t *testing.T) {
			before, err := s.Instance(ctx, s.RO, "i-1")
			if err != nil {
				t.Fatal(err)
			}
			mustWrite(t, s, func(ctx context.Context, tx Tx) error {
				return w.write(ctx, tx, w.at)
			})
			after, err := s.Instance(ctx, s.RO, "i-1")
			if err != nil {
				t.Fatal(err)
			}
			if after.Generation != before.Generation {
				t.Errorf("generation moved %d \u2192 %d", before.Generation, after.Generation)
			}
			if after.UpdatedAt != w.at {
				t.Errorf("updated_at = %d, want %d; \u00a72.8 requires every exceptional writer to bump it",
					after.UpdatedAt, w.at)
			}
		})
	}

	// And the one that must NOT move the hash: the port reassignment (D52).
	row, err := s.Instance(ctx, s.RO, "i-1")
	if err != nil {
		t.Fatal(err)
	}
	if row.InternalPort != 21099 {
		t.Errorf("internal_port = %d, want the reassigned 21099", row.InternalPort)
	}
}

// TestTakePendingStartIsOneShot is the launcher's half of the trigger contract
// (§5.6 step 3, D61): both columns are consumed and cleared together, so the
// next start — from any trigger — is the saved configuration again.
func TestTakePendingStartIsOneShot(t *testing.T) {
	s := newTestStore(t)
	seedInstance(t, s, newInstance("i-1", "qwen", 8081, 21001))

	const override = `{"n_gpu_layers":{"mode":"none"},"ctx_size":2048,"parallel":1}`
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		_, err := s.StampPendingStart(ctx, tx, "i-1", model.TriggerSafeStart, ptr(override), 2000)
		return err
	})

	var (
		trigger *model.PendingTrigger
		patch   *string
	)
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		var err error
		trigger, patch, err = s.TakePendingStart(ctx, tx, "i-1")
		return err
	})
	if trigger == nil || *trigger != model.TriggerSafeStart {
		t.Fatalf("trigger = %v, want safe_start", trigger)
	}
	if patch == nil || *patch != override {
		t.Fatalf("override = %v, want the stamped patch", patch)
	}

	// The second consumption finds nothing: a crash, a reboot or a supervisor
	// restart all launch the saved configuration.
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		var err error
		trigger, patch, err = s.TakePendingStart(ctx, tx, "i-1")
		return err
	})
	if trigger != nil || patch != nil {
		t.Errorf("the hand-off columns survived consumption: %v %v", trigger, patch)
	}
}

// TestPurgeCascades is `?purge=true`: the row and everything keyed to it go.
func TestPurgeCascades(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedInstance(t, s, newInstance("i-1", "qwen", 8081, 21001))

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		return s.InsertInstanceStart(ctx, tx, model.InstanceStart{
			ID: "s-1", InstanceID: "i-1", At: 1500, Trigger: model.StartByUser, ConfigHash: "hash-i-1",
		})
	})

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		ok, err := s.PurgeInstance(ctx, tx, "i-1")
		if err != nil {
			return err
		}
		if !ok {
			t.Error("PurgeInstance matched no row")
		}
		return nil
	})

	if _, err := s.Instance(ctx, s.RO, "i-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("the purged row survives: %v", err)
	}
	if _, err := s.InstanceStatus(ctx, s.RO, "i-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("instance_status did not cascade: %v", err)
	}
	starts, err := s.InstanceStarts(ctx, s.RO, "i-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(starts) != 0 {
		t.Errorf("instance_starts did not cascade: %d rows left", len(starts))
	}
}

// TestInstanceViewCarriesTheDerivedInputs is what makes a list request one query
// rather than 1+2N: both `instance_starts` facts the derived flags read come
// back with the joined row.
func TestInstanceViewCarriesTheDerivedInputs(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedInstance(t, s, newInstance("i-1", "qwen", 8081, 21001))

	// Nothing has run yet.
	v, err := s.InstanceView(ctx, s.RO, "i-1")
	if err != nil {
		t.Fatalf("InstanceView: %v", err)
	}
	if v.LastClosedOutcome != nil {
		t.Errorf("last_closed_outcome = %v, want none", *v.LastClosedOutcome)
	}
	if v.OpenOverride != nil {
		t.Errorf("open_override = %v, want none (nothing is running)", *v.OpenOverride)
	}
	if v.Status.State != model.InstanceUnknown {
		t.Errorf("state = %q, want unknown", v.Status.State)
	}

	// A completed ordinary run, then a live safe start.
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		if err := s.InsertInstanceStart(ctx, tx, model.InstanceStart{
			ID: "s-1", InstanceID: "i-1", At: 1500, Trigger: model.StartByUser,
			ConfigHash: "hash-i-1", Outcome: ptr(model.OutcomeStopped), EndedAt: ptr(int64(1600)),
		}); err != nil {
			return err
		}
		return s.InsertInstanceStart(ctx, tx, model.InstanceStart{
			ID: "s-2", InstanceID: "i-1", At: 1700, Trigger: model.StartBySafeStart,
			ConfigHash: "hash-i-1", OverrideJSON: ptr(`{"ctx_size":2048}`),
		})
	})

	v, err = s.InstanceView(ctx, s.RO, "i-1")
	if err != nil {
		t.Fatal(err)
	}
	if v.LastClosedOutcome == nil || *v.LastClosedOutcome != model.OutcomeStopped {
		t.Errorf("last_closed_outcome = %v, want stopped", v.LastClosedOutcome)
	}
	if v.OpenOverride == nil || !*v.OpenOverride {
		t.Errorf("open_override = %v, want true — a safe start is the running configuration", v.OpenOverride)
	}
}

// TestLastClosedExcludesInhibitedRows is §2.8's load-bearing exclusion: a
// refusal to start is not a completed run, and counting it would falsify the
// very `clean_exit` clause that produced it.
func TestLastClosedExcludesInhibitedRows(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedInstance(t, s, newInstance("i-1", "qwen", 8081, 21001))

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		if err := s.InsertInstanceStart(ctx, tx, model.InstanceStart{
			ID: "s-1", InstanceID: "i-1", At: 1000, Trigger: model.StartByUser,
			ConfigHash: "h", Outcome: ptr(model.OutcomeStopped), EndedAt: ptr(int64(1100)),
		}); err != nil {
			return err
		}
		// The supervisor declines, later, and records the refusal.
		return s.InsertInstanceStart(ctx, tx, model.InstanceStart{
			ID: "s-2", InstanceID: "i-1", At: 2000, Trigger: model.StartBySupervisorRestart,
			ConfigHash: "h", Outcome: ptr(model.OutcomeInhibited),
			ErrorCode: ptr(string(model.InhibitCleanExit)), EndedAt: ptr(int64(2000)),
		})
	})

	last, err := s.LastClosedInstanceStart(ctx, s.RO, "i-1")
	if err != nil {
		t.Fatalf("LastClosedInstanceStart: %v", err)
	}
	if last.ID != "s-1" {
		t.Errorf("LAST_CLOSED = %s, want the completed run s-1 rather than the refusal", last.ID)
	}

	v, err := s.InstanceView(ctx, s.RO, "i-1")
	if err != nil {
		t.Fatal(err)
	}
	if v.LastClosedOutcome == nil || *v.LastClosedOutcome != model.OutcomeStopped {
		t.Errorf("the joined projection disagrees with LastClosedInstanceStart: %v", v.LastClosedOutcome)
	}
}

// TestCloseInstanceStartIsSingleShot is D63: `outcome` is written exactly once,
// so no two rules can close the same row and there is no precedence question.
func TestCloseInstanceStartIsSingleShot(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedInstance(t, s, newInstance("i-1", "qwen", 8081, 21001))

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		return s.InsertInstanceStart(ctx, tx, model.InstanceStart{
			ID: "s-1", InstanceID: "i-1", At: 1000, Trigger: model.StartByUser, ConfigHash: "h",
		})
	})

	// The open row is stamped ready, which does NOT close it.
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		ok, err := s.StampStartReady(ctx, tx, "s-1", 1100)
		if err != nil {
			return err
		}
		if !ok {
			t.Error("StampStartReady matched no row")
		}
		return nil
	})
	if _, err := s.OpenInstanceStart(ctx, s.RO, "i-1"); err != nil {
		t.Errorf("stamping ready_at closed the row; the run is still in flight (D63): %v", err)
	}

	closure := StartClosure{Outcome: model.OutcomeFailed, ExitCode: ptr(int64(72)),
		ErrorCode: ptr("model_missing"), EndedAt: 1200}
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		ok, err := s.CloseInstanceStart(ctx, tx, "s-1", closure)
		if err != nil {
			return err
		}
		if !ok {
			t.Error("the first close did not match")
		}
		return nil
	})

	// A second closer — the supervisor, arriving after the launcher already
	// recorded the outcome — must not overwrite it.
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		ok, err := s.CloseInstanceStart(ctx, tx, "s-1", StartClosure{
			Outcome: model.OutcomeStopped, EndedAt: 1300})
		if err != nil {
			return err
		}
		if ok {
			t.Error("a closed row was closed again — `outcome` is single-shot (D63)")
		}
		return nil
	})

	rows, err := s.InstanceStarts(ctx, s.RO, "i-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Outcome == nil || *rows[0].Outcome != model.OutcomeFailed {
		t.Errorf("the recorded outcome was overwritten: %+v", rows)
	}
	if rows[0].ReadyAt == nil || *rows[0].ReadyAt != 1100 {
		t.Errorf("ready_at was lost: %+v", rows[0].ReadyAt)
	}
}

// TestCrashLoopCountingIsFailureOnly is D64: `stopped` and `inhibited` rows are
// never counted, and `restart_window_reset_at` starts the window over.
func TestCrashLoopCountingIsFailureOnly(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedInstance(t, s, newInstance("i-1", "qwen", 8081, 21001))

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		rows := []model.InstanceStart{
			{ID: "s-1", At: 1000, Outcome: ptr(model.OutcomeFailed)},
			{ID: "s-2", At: 2000, Outcome: ptr(model.OutcomeStopped)},
			{ID: "s-3", At: 3000, Outcome: ptr(model.OutcomeInhibited), ErrorCode: ptr("policy_never")},
			{ID: "s-4", At: 4000, Outcome: ptr(model.OutcomeFailed)},
			{ID: "s-5", At: 5000, Outcome: ptr(model.OutcomeFailed)},
			{ID: "s-6", At: 6000}, // still open
		}
		for _, r := range rows {
			r.InstanceID, r.Trigger, r.ConfigHash = "i-1", model.StartByUser, "h"
			if err := s.InsertInstanceStart(ctx, tx, r); err != nil {
				return err
			}
		}
		return nil
	})

	n, err := s.CountFailedStartsSince(ctx, s.RO, "i-1", 0)
	if err != nil {
		t.Fatalf("CountFailedStartsSince: %v", err)
	}
	if n != 3 {
		t.Errorf("count = %d, want 3 — only `failed` rows count (D64)", n)
	}

	// "Reset failed" stamps restart_window_reset_at, and the cutoff ignores
	// everything at or before it.
	n, err = s.CountFailedStartsSince(ctx, s.RO, "i-1", 4000)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("count after the reset = %d, want 1", n)
	}

	// The refusal episode rule reads the same table.
	has, err := s.HasInhibitedStartSince(ctx, s.RO, "i-1", model.InhibitPolicyNever, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Error("the recorded refusal was not found; the supervisor would write a second one")
	}
	has, err = s.HasInhibitedStartSince(ctx, s.RO, "i-1", model.InhibitCrashLoop, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Error("a refusal for a DIFFERENT reason was matched; a new episode must record a new row")
	}
}

// TestClearCrashLoopLatch is the API's only reach into `instance_status`, and it
// is exactly three columns (§2.8).
func TestClearCrashLoopLatch(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedInstance(t, s, newInstance("i-1", "qwen", 8081, 21001))

	crashed := model.InstanceStatus{
		InstanceID:            "i-1",
		State:                 model.InstanceCrashLooping,
		LastChangeAt:          1000,
		GPUAttribution:        model.AttributionUnknown,
		AppliedConfigHash:     ptr("hash-applied"),
		ExeVersionID:          ptr("b10604-cuda-src"),
		ReconcileBackoffUntil: ptr(int64(9_999_999)),
		MainPID:               ptr(int64(4242)),
	}
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		ok, err := s.UpdateInstanceStatus(ctx, tx, crashed)
		if err != nil {
			return err
		}
		if !ok {
			t.Fatal("UpdateInstanceStatus matched no row")
		}
		return nil
	})

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		ok, err := s.ClearCrashLoopLatch(ctx, tx, "i-1", 5000)
		if err != nil {
			return err
		}
		if !ok {
			t.Fatal("ClearCrashLoopLatch matched no row")
		}
		return nil
	})

	got, err := s.InstanceStatus(ctx, s.RO, "i-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != model.InstanceStopped {
		t.Errorf("state = %q, want stopped — clearing the latch IS the operation", got.State)
	}
	if got.ReconcileBackoffUntil != nil {
		t.Error("the backoff survived; F3's recovery is `try again NOW`")
	}
	if got.RestartWindowResetAt != 5000 {
		t.Errorf("restart_window_reset_at = %d, want 5000", got.RestartWindowResetAt)
	}
	// And nothing else moved: the columns that carry the correctness argument
	// are supervisor-only without exception.
	if got.AppliedConfigHash == nil || *got.AppliedConfigHash != "hash-applied" {
		t.Error("applied_config_hash was touched by an API-side write")
	}
	if got.ExeVersionID == nil || *got.ExeVersionID != "b10604-cuda-src" {
		t.Error("exe_version_id was touched by an API-side write")
	}
	if got.MainPID == nil || *got.MainPID != 4242 {
		t.Error("main_pid was touched by an API-side write")
	}
}

// TestClearCrashLoopLatchLeavesOtherStatesAlone: `crash-looping → stopped` is
// the ONLY actual-state transition an API handler may write, so a merely
// `failed` instance keeps its state while still getting its backoff cleared.
func TestClearCrashLoopLatchLeavesOtherStatesAlone(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedInstance(t, s, newInstance("i-1", "qwen", 8081, 21001))

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		_, err := s.UpdateInstanceStatus(ctx, tx, model.InstanceStatus{
			InstanceID: "i-1", State: model.InstanceFailed, LastChangeAt: 1000,
			GPUAttribution: model.AttributionUnknown, ReconcileBackoffUntil: ptr(int64(9999)),
		})
		return err
	})
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		_, err := s.ClearCrashLoopLatch(ctx, tx, "i-1", 5000)
		return err
	})

	got, err := s.InstanceStatus(ctx, s.RO, "i-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != model.InstanceFailed {
		t.Errorf("state = %q, want failed — an API handler may not write it", got.State)
	}
	if got.ReconcileBackoffUntil != nil {
		t.Error("the backoff was not cleared")
	}
}

// TestInstancesWithOpenStarts is the second term of the supervisor's reconcile
// set (§3.10c): a soft-deleted instance is reconciled exactly until its last run
// is ledgered, and never again.
func TestInstancesWithOpenStarts(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedInstance(t, s, newInstance("i-1", "qwen", 8081, 21001))
	seedInstance(t, s, newInstance("i-2", "gemma", 8082, 21002))

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		if err := s.InsertInstanceStart(ctx, tx, model.InstanceStart{
			ID: "s-1", InstanceID: "i-1", At: 1000, Trigger: model.StartByUser, ConfigHash: "h",
		}); err != nil {
			return err
		}
		_, err := s.SoftDeleteInstance(ctx, tx, "i-1", 2000)
		return err
	})

	ids, err := s.InstancesWithOpenStarts(ctx, s.RO)
	if err != nil {
		t.Fatalf("InstancesWithOpenStarts: %v", err)
	}
	if diff := cmp.Diff([]string{"i-1"}, ids); diff != "" {
		t.Errorf("open-start set mismatch (-want +got):\n%s", diff)
	}

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		ok, err := s.CloseOpenInstanceStart(ctx, tx, "i-1", StartClosure{
			Outcome: model.OutcomeStopped, EndedAt: 3000})
		if err != nil {
			return err
		}
		if !ok {
			t.Error("CloseOpenInstanceStart matched no row")
		}
		return nil
	})

	ids, err = s.InstancesWithOpenStarts(ctx, s.RO)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Errorf("the deleted instance is still reconciled after its last run was ledgered: %v", ids)
	}
}
