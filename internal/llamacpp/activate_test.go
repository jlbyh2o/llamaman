package llamacpp

import (
	"context"
	"os"
	"testing"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// TestActivationCommitsFlagsThenSymlinks is §6.6 steps 2 to 4 in order: the
// flags and every instance's `config_hash` move in one transaction, and only
// then are the symlinks repaired FROM the rows.
func TestActivationCommitsFlagsThenSymlinks(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil)
	f.registerActivate(&fakeRoller{failAt: -1})
	f.seedVersion("b10600-cpu-src", model.VersionReady)
	f.seedVersion("b10621-cpu-src", model.VersionReady)

	f.activate(t, "b10600-cpu-src", RestartNone)
	f.activate(t, "b10621-cpu-src", RestartNone)

	newer := f.version("b10621-cpu-src")
	older := f.version("b10600-cpu-src")
	switch {
	case !newer.IsActive:
		t.Error("the activated version is not is_active")
	case newer.ActivatedAt == nil:
		t.Error("activated_at was not stamped")
	case older.IsActive:
		t.Error("the outgoing version is still is_active")
	case !older.PreviousActive:
		t.Error("the outgoing version did not take the rollback slot")
	}

	if got := f.link(ActiveLink); got != "b10621-cpu-src" {
		t.Errorf("versions/active -> %q, want b10621-cpu-src", got)
	}
	if got := f.link(PreviousLink); got != "b10600-cpu-src" {
		t.Errorf("versions/previous -> %q, want b10600-cpu-src", got)
	}
	// One per activation: D69 recomputes for every non-deleted instance inside
	// the transaction that sets `is_active`.
	if got := f.recomputes.count(); got != 2 {
		t.Errorf("RecomputeConfigHash calls = %d, want 2", got)
	}
	// §2.3a's activate column: an activation never leaves `ready`.
	if newer.State != model.VersionReady || older.State != model.VersionReady {
		t.Errorf("states = %q/%q, want both ready", newer.State, older.State)
	}
}

// TestCanaryFailureRevertsRowsThenSymlinks is D24, and it is the test the whole
// revert exists for.
//
// The assertion that matters most is the ROW one. Reverting only the symlink
// would look right on disk and be wrong where it counts: §6.6's boot
// reconciliation makes the row win, so a build left `is_active=1` after its
// canary failed would be re-pointed at on the next daemon start and the whole
// fleet restarted onto it.
func TestCanaryFailureRevertsRowsThenSymlinks(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil)
	roller := &fakeRoller{
		targets: []RollTarget{{ID: "i1", Name: "chat"}, {ID: "i2", Name: "code"}},
		failAt:  0, // the canary
	}
	f.registerActivate(roller)
	f.seedVersion("b10600-cpu-src", model.VersionReady)
	f.seedVersion("b10621-cpu-src", model.VersionReady)

	f.activate(t, "b10600-cpu-src", RestartNone)
	before := f.recomputes.count()
	job := f.activate(t, "b10621-cpu-src", RestartRolling)

	failed := f.version("b10621-cpu-src")
	restored := f.version("b10600-cpu-src")
	if failed.IsActive {
		t.Error("the build whose canary failed is still is_active — the next boot would " +
			"re-point versions/active at it and restart the fleet onto it")
	}
	if !restored.IsActive {
		t.Error("the previous build was not restored to is_active")
	}
	if restored.PreviousActive {
		t.Error("the restored build kept the rollback slot the activation gave it")
	}
	if failed.State != model.VersionReady {
		t.Errorf("version state = %q, want ready — a failed canary is not a failed build",
			failed.State)
	}

	if got := f.link(ActiveLink); got != "b10600-cpu-src" {
		t.Errorf("versions/active -> %q, want the restored build", got)
	}
	// The revert re-runs D69, which is what clears the `restart_required` badge
	// step 3 raised across the fleet.
	if got := f.recomputes.count() - before; got != 2 {
		t.Errorf("recomputes across the activation and its revert = %d, want 2", got)
	}

	if got := f.job(job.ID); got.State != model.JobFailed {
		t.Errorf("activation job state = %q, want failed", got.State)
	} else if got.ErrorCode == nil || *got.ErrorCode != string(CodeCanaryFailed) {
		t.Errorf("activation job error_code = %v, want %q", got.ErrorCode, CodeCanaryFailed)
	}

	// The canary is restarted twice: once onto the new build, where it failed,
	// and once back onto the old one. No other instance is touched.
	want := []string{"chat", "chat"}
	if len(roller.restarts) != len(want) {
		t.Fatalf("restarts = %v, want %v — the roll must abort without touching the rest",
			roller.restarts, want)
	}
	for i := range want {
		if roller.restarts[i] != want[i] {
			t.Errorf("restart %d = %q, want %q", i, roller.restarts[i], want[i])
		}
	}
}

// TestLaterRollFailureDoesNotRevert is the other half of D24: by the time a
// later instance fails, instances are already serving on the new build, and
// reverting under them is a second unplanned restart of everything that worked.
func TestLaterRollFailureDoesNotRevert(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil)
	roller := &fakeRoller{
		targets: []RollTarget{{ID: "i1", Name: "chat"}, {ID: "i2", Name: "code"}},
		failAt:  1, // not the canary
	}
	f.registerActivate(roller)
	f.seedVersion("b10600-cpu-src", model.VersionReady)
	f.seedVersion("b10621-cpu-src", model.VersionReady)

	f.activate(t, "b10600-cpu-src", RestartNone)
	job := f.activate(t, "b10621-cpu-src", RestartRolling)

	if !f.version("b10621-cpu-src").IsActive {
		t.Error("a failure after the canary reverted the activation; it must not")
	}
	if got := f.job(job.ID); got.State != model.JobSucceeded {
		t.Errorf("activation job state = %q, want succeeded", got.State)
	}
	if got := f.link(ActiveLink); got != "b10621-cpu-src" {
		t.Errorf("versions/active -> %q, want the new build", got)
	}
}

// TestActivationEnqueuesTheDeleteOnlyOnSuccess is §6.6 step 2's ordering fix:
// the retired build's `llamacpp_delete` is enqueued when the ACTIVATION job
// succeeds, never at step 2, because step 5 may revert an activation and cannot
// revert a directory a delete worker has already removed.
func TestActivationEnqueuesTheDeleteOnlyOnSuccess(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil)
	f.registerActivate(&fakeRoller{failAt: -1})
	f.registerDelete()
	f.seedVersion("b10500-cpu-src", model.VersionReady)
	f.seedVersion("b10600-cpu-src", model.VersionReady)
	f.seedVersion("b10621-cpu-src", model.VersionReady)

	f.activate(t, "b10500-cpu-src", RestartNone)
	f.activate(t, "b10600-cpu-src", RestartNone)
	// Rollback depth is one, so activating a third build leaves b10500 with no
	// slot at all — it is the deletion candidate.
	f.activate(t, "b10621-cpu-src", RestartNone)

	jobs, err := f.queue.List(context.Background(), store.JobFilter{
		Kinds: []model.JobKind{model.JobLlamacppDelete},
	})
	if err != nil {
		t.Fatalf("list delete jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("delete jobs = %d, want exactly one for the retired build", len(jobs))
	}
	if jobs[0].SubjectID != "b10500-cpu-src" {
		t.Errorf("delete subject = %q, want b10500-cpu-src", jobs[0].SubjectID)
	}
}

// TestActivateGuards covers §6.6 step 1 and §3.5's documented refusals.
func TestActivateGuards(t *testing.T) {
	t.Parallel()

	t.Run("a version that is not ready", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, nil)
		f.seedVersion("b10621-cpu-src", model.VersionBuilding)

		_, err := f.svc.Activate(context.Background(), "b10621-cpu-src", ActivateRequest{})
		assertCode(t, err, CodeVersionNotReady)
	})

	t.Run("another activation is live", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, nil)
		f.seedVersion("b10600-cpu-src", model.VersionReady)
		f.seedVersion("b10621-cpu-src", model.VersionReady)

		if _, err := f.svc.Activate(context.Background(), "b10600-cpu-src",
			ActivateRequest{}); err != nil {
			t.Fatalf("first Activate: %v", err)
		}
		_, err := f.svc.Activate(context.Background(), "b10621-cpu-src", ActivateRequest{})
		assertCode(t, err, CodeActivationInFlight)
	})

	t.Run("a bench is live", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, func(c *Config) { c.Bench = benchAlways(true) })
		f.seedVersion("b10621-cpu-src", model.VersionReady)

		_, err := f.svc.Activate(context.Background(), "b10621-cpu-src", ActivateRequest{})
		assertCode(t, err, CodeBenchInFlight)
	})

	t.Run("restart_instances must be none or rolling", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, nil)
		f.seedVersion("b10621-cpu-src", model.VersionReady)

		_, err := f.svc.Activate(context.Background(), "b10621-cpu-src",
			ActivateRequest{RestartInstances: "all-at-once"})
		assertCode(t, err, model.CodeBadFlags)
	})
}

// TestRollbackWithNoTarget: `llamacpp.keep_previous` off, or nothing replaced
// yet, and the answer is the documented 409 rather than a 404 on an empty read.
func TestRollbackWithNoTarget(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil)
	_, err := f.svc.Rollback(context.Background(), ActivateRequest{})
	assertCode(t, err, CodeNoRollbackTarget)
}

// TestReconcileRepairsSymlinksFromRows is §6.6's boot reconciliation: the row
// wins, always, in both directions — a missing link is created and a link
// pointing at the wrong build is re-pointed.
func TestReconcileRepairsSymlinksFromRows(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil)
	f.registerActivate(&fakeRoller{failAt: -1})
	f.seedVersion("b10600-cpu-src", model.VersionReady)
	f.seedVersion("b10621-cpu-src", model.VersionReady)
	f.activate(t, "b10600-cpu-src", RestartNone)
	f.activate(t, "b10621-cpu-src", RestartNone)

	// Whatever a crash, a hand edit or an older binary left behind.
	if err := os.Remove(f.svc.layout.LinkPath(ActiveLink)); err != nil {
		t.Fatalf("remove versions/active: %v", err)
	}
	if err := f.svc.layout.SetLink(PreviousLink, "b10621-cpu-src"); err != nil {
		t.Fatalf("mis-point versions/previous: %v", err)
	}

	if err := f.svc.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := f.link(ActiveLink); got != "b10621-cpu-src" {
		t.Errorf("versions/active -> %q after reconcile, want the is_active row", got)
	}
	if got := f.link(PreviousLink); got != "b10600-cpu-src" {
		t.Errorf("versions/previous -> %q after reconcile, want the previous_active row", got)
	}
}

// TestReconcileClosesInterruptedActivations is the `llamacpp_activate`
// finalizer of §2.3: the ROW decides, and it decides two different ways.
func TestReconcileClosesInterruptedActivations(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		// activateFirst runs the activation to completion before the job is
		// re-opened as `interrupted`, which is what "the step-3 transaction
		// committed" looks like from the next boot.
		activateFirst bool
		wantState     model.JobState
	}{
		{
			name:          "the target is active, so the transaction committed",
			activateFirst: true,
			wantState:     model.JobSucceeded,
		},
		{
			name:          "the target is not active, so nothing happened",
			activateFirst: false,
			wantState:     model.JobFailed,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, nil)
			f.registerActivate(&fakeRoller{failAt: -1})
			f.seedVersion("b10621-cpu-src", model.VersionReady)

			var job model.Job
			if tc.activateFirst {
				job = f.activate(t, "b10621-cpu-src", RestartNone)
			} else {
				res, err := f.svc.Activate(context.Background(), "b10621-cpu-src",
					ActivateRequest{})
				if err != nil {
					t.Fatalf("Activate: %v", err)
				}
				job = res
			}

			// Put the job where a daemon restart would have left it (§2.3's
			// second bucket).
			ctx := context.Background()
			if err := f.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
				return f.store.SetJobState(ctx, tx, job.ID, model.JobInterrupted)
			}); err != nil {
				t.Fatalf("interrupt the job: %v", err)
			}

			if err := f.svc.Reconcile(ctx); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if got := f.job(job.ID).State; got != tc.wantState {
				t.Errorf("job state after the finalizer = %q, want %q", got, tc.wantState)
			}
			// Either way the version row stays `ready`, which is what §2.3a's
			// activate column asserts.
			if got := f.version("b10621-cpu-src").State; got != model.VersionReady {
				t.Errorf("version state = %q, want ready", got)
			}
		})
	}
}

// TestReconcileReleasesAForeignBuildLease is D70's boot half: a lease whose
// owner is not this boot belongs to a daemon that is provably gone.
func TestReconcileReleasesAForeignBuildLease(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil)
	first := f.seedVersion("b10621-cpu-src", model.VersionPending)
	ctx := context.Background()

	res, err := f.queue.Enqueue(ctx, jobsEnqueueInstall(first.ID, first.Tag))
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := f.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		_, err := f.store.AcquireBuildLease(ctx, tx, res.Job.ID, first.ID, "a-boot-that-is-gone",
			f.clock.Now().UnixMilli(), f.clock.Now().Add(DefaultLeaseTTL).UnixMilli())
		return err
	}); err != nil {
		t.Fatalf("hold the lease: %v", err)
	}

	if err := f.svc.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var lease store.BuildLease
	if err := f.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		lease, err = f.store.BuildLease(ctx, tx)
		return err
	}); err != nil {
		t.Fatalf("read the lease: %v", err)
	}
	if lease.Held() {
		t.Errorf("a lease owned by %v survived this boot's reconcile", lease.Owner)
	}
}

// activate runs one activation to completion and returns its job row.
func (f *fixture) activate(t *testing.T, id, restart string) model.Job {
	t.Helper()
	job, err := f.svc.Activate(context.Background(), id,
		ActivateRequest{RestartInstances: restart})
	if err != nil {
		t.Fatalf("Activate %s: %v", id, err)
	}
	f.runOne()
	return job
}

func (f *fixture) registerActivate(roller Roller) {
	f.t.Helper()
	w := f.svc.NewActivateWorker(ActivateWorkerConfig{Roller: roller})
	if err := f.queue.Register(w); err != nil {
		f.t.Fatalf("register the activate worker: %v", err)
	}
}

func (f *fixture) registerDelete() {
	f.t.Helper()
	if err := f.queue.Register(f.svc.NewDeleteWorker()); err != nil {
		f.t.Fatalf("register the delete worker: %v", err)
	}
}

// benchAlways is §6.6 step 1's bench term with a fixed answer.
type benchAlways bool

func (b benchAlways) BenchLive(context.Context, store.Tx) (bool, error) { return bool(b), nil }
