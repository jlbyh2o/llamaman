package bench

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// The bench lease (D75) and the stop-and-restore protocol (DESIGN section 10),
// driven through the real job queue against a real database.
//
// DESIGN section 15 names both suites: "Bench lease (D75), the same suite over
// `bench_lease`: two `bench_run` jobs on DIFFERENT runs started simultaneously
// produce exactly one `running` sweep and one `queued` 'waiting for the running
// benchmark'; a lease whose owner is a dead `boot_id` is reclaimed at boot BUT
// ONLY AFTER the restore finalizer has set `restore_done=1`" — and "a `bench_run`
// job whose lease owner died becomes `interrupted` with `bench_runs.state`
// untouched, and the boot finalizer restores the stopped production instances,
// sets `restore_done=1` and closes both rows".

func simpleSweep() Sweep {
	return Sweep{
		NBatch: IntAxis{512, 2048},
		Tests:  []Test{{PP: ptr(512)}},
	}
}

// TestBenchLeaseExclusivity is D75's central claim: `jobs.subject_id` for a
// bench is `bench_runs.id`, so two jobs on two DIFFERENT runs are perfectly
// legal under `idx_jobs_one_live_per_subject` — and the lease is what stops the
// second one anyway.
func TestBenchLeaseExclusivity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := newFixture(t, nil)
	// The runner blocks on the first point of whichever sweep gets the lease, so
	// the second job is claimed while the first is genuinely mid-sweep.
	held := make(chan struct{})
	release := make(chan struct{})
	f.run.onPoint = func(_ context.Context, n int) ([]byte, error) {
		if n == 0 {
			close(held)
			<-release
		}
		return mustFixture(t, "llama-bench-pp-tg.json"), nil
	}

	first := f.createRun(simpleSweep(), false)
	second := f.createRun(simpleSweep(), false)
	if first.Run.ID == second.Run.ID {
		t.Fatal("two Creates produced one run")
	}
	// Which run wins the lease is first-come-first-served through the queue:
	// LeaseNextJob orders by `priority, run_after, id`, both jobs carry the
	// registry priority and this fixture's fake clock, so the id decides — and
	// store.NewID is strictly increasing, so the id order IS the create order.
	// Asserted rather than assumed: if the mint ever stopped being monotonic,
	// this says so instead of failing further down as a mysterious lease owner.
	if first.Job.ID >= second.Job.ID {
		t.Fatalf("job ids %s then %s are not in create order; the queue's tie-break is the id",
			first.Job.ID, second.Job.ID)
	}

	// Both jobs are live under the per-subject index, which is exactly the hole
	// D75 exists to close: the index binds per RUN and says nothing about "one
	// bench at a time".
	if !first.Job.State.IsLive() || !second.Job.State.IsLive() {
		t.Fatalf("both jobs should be live: %s, %s", first.Job.State, second.Job.State)
	}

	done := make(chan error, 1)
	go func() {
		_, err := f.queue.RunOnce(ctx)
		done <- err
	}()
	<-held

	// The second runner finds the lease held. Its job stays `queued` with a
	// `run_after` in the future — a queue, not an error — and it spends no part
	// of its attempt budget.
	ran, err := f.queue.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if !ran {
		t.Fatal("the second job was not claimed at all")
	}

	lease := f.mustLease()
	if lease.RunID == nil || *lease.RunID != first.Run.ID {
		t.Fatalf("the lease names %s, want the first run %s",
			orAbsent(lease.RunID, "nobody"), first.Run.ID)
	}

	deferred := f.mustJob(second.Job.ID)
	if deferred.State != model.JobQueued {
		t.Errorf("the second job is %s, want %s — a queue, not a failure",
			deferred.State, model.JobQueued)
	}
	if deferred.ErrorCode != nil {
		t.Errorf("the second job wears error %q; waiting for the lease is not a failure",
			*deferred.ErrorCode)
	}
	if deferred.RunAfter <= f.clock.Now().UnixMilli() {
		t.Errorf("the second job is ready immediately; it should wait for the lease")
	}
	if got := f.mustRun(second.Run.ID).State; got != model.BenchQueued {
		t.Errorf("the second run is %s, want %s", got, model.BenchQueued)
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("the first RunOnce: %v", err)
	}

	// The first sweep is over and stopped nothing, so the lease is released and
	// the second sweep can take it.
	if lease := f.mustLease(); lease.Held() {
		t.Errorf("the lease is still held by job %s after the sweep finished",
			orAbsent(lease.JobID, "nobody"))
	}
	if got := f.mustRun(first.Run.ID).State; got != model.BenchSucceeded {
		t.Errorf("the first run is %s, want %s", got, model.BenchSucceeded)
	}
}

// TestLeaseSurvivesALongPoint is the other half of D75, and the half a static
// clock hides: `expires_at` is a HEARTBEAT, and one point is a whole model load
// plus `-r` repetitions at depth, which routinely outlives DefaultLeaseTTL.
//
// Touching the lease only between points lets it lapse while llama-bench is
// still on the GPU. AcquireBenchLease's `expires_at < ?` clause then matches, a
// second `bench_run` of the same daemon takes the lease, and two sweeps measure
// garbage while writing `stopped_instances_json` over each other — the outcome
// section 10 calls the worst possible one, arrived at by two well-behaved
// workers. So the horizon is extended from a ticker beside the process, and the
// Start guard refuses a lease held by a live sibling of this boot however stale
// its horizon looks.
func TestLeaseSurvivesALongPoint(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := newFixture(t, func(c *Config) {
		// A heartbeat the test can outwait. The horizon it writes is still
		// LeaseTTL ahead of the (fake) clock, so this changes the RHYTHM only.
		c.LeaseBeat = 2 * time.Millisecond
	})

	held := make(chan struct{})
	release := make(chan struct{})
	f.run.onPoint = func(_ context.Context, n int) ([]byte, error) {
		if n == 0 {
			close(held)
			<-release
		}
		return mustFixture(t, "llama-bench-pp-tg.json"), nil
	}

	first := f.createRun(simpleSweep(), false)
	second := f.createRun(simpleSweep(), false)
	// Same tie-break as TestBenchLeaseExclusivity: the queue orders by id, and
	// store.NewID mints in create order.
	if first.Job.ID >= second.Job.ID {
		t.Fatalf("job ids %s then %s are not in create order; the queue's tie-break is the id",
			first.Job.ID, second.Job.ID)
	}

	done := make(chan error, 1)
	go func() {
		_, err := f.queue.RunOnce(ctx)
		done <- err
	}()
	<-held

	// The point outlives the whole lease horizon, several times over.
	f.clock.Advance(DefaultLeaseTTL + time.Minute)

	// The ticker catches the horizon back up without the sweep making any
	// progress, which is precisely what a between-points touch could not do.
	deadline := time.Now().Add(5 * time.Second)
	for {
		lease := f.mustLease()
		if lease.ExpiresAt != nil && *lease.ExpiresAt > f.clock.Now().UnixMilli() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the lease horizon %s never caught up with the clock %d; a point "+
				"longer than the TTL lapses the lease and lets a second sweep in",
				orAbsent(lease.ExpiresAt, "unset"), f.clock.Now().UnixMilli())
		}
		time.Sleep(time.Millisecond)
	}

	if _, err := f.queue.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got := f.mustJob(second.Job.ID).State; got != model.JobQueued {
		t.Errorf("the second job is %s, want %s — it must not take a lease from a sweep "+
			"that is still executing", got, model.JobQueued)
	}
	if n := len(f.run.Argv()); n != 1 {
		t.Errorf("llama-bench ran %d times while one point was still executing; two "+
			"concurrent sweeps is the outcome D75 exists to prevent", n)
	}
	if lease := f.mustLease(); lease.RunID == nil || *lease.RunID != first.Run.ID {
		t.Errorf("the lease names %s, want the first run %s",
			orAbsent(lease.RunID, "nobody"), first.Run.ID)
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("the first RunOnce: %v", err)
	}
}

// TestSweepPersistsResults walks the ordinary path: every point runs, every
// llama-bench object becomes a `bench_results` row, and the counters agree.
func TestSweepPersistsResults(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := newFixture(t, nil)
	created := f.createRun(simpleSweep(), false)

	if _, err := f.queue.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	run := f.mustRun(created.Run.ID)
	if run.State != model.BenchSucceeded {
		t.Fatalf("run is %s (%v), want succeeded", run.State, run.ErrorMessage)
	}
	if run.PointsTotal != 2 || run.PointsDone != 2 || run.PointsFailed != 0 {
		t.Errorf("counters are %d/%d done, %d failed; want 2/2, 0",
			run.PointsDone, run.PointsTotal, run.PointsFailed)
	}

	for _, p := range f.mustPoints(run.ID) {
		if p.State != model.PointSucceeded {
			t.Errorf("point %d is %s", p.Ordinal, p.State)
		}
		if p.StartedAt == nil || p.FinishedAt == nil {
			t.Errorf("point %d has no duration, so the estimate has nothing to learn from",
				p.Ordinal)
		}
	}

	// Two points × two objects per fixture.
	results := f.mustResults(run.ID)
	if len(results) != 4 {
		t.Fatalf("got %d results, want 4", len(results))
	}
	var pp, tg int
	for _, r := range results {
		switch r.TestKind {
		case model.TestPP:
			pp++
			if r.AvgTS != 6058.31 || r.StddevTS != 86.42 {
				t.Errorf("pp result = %v ± %v, want 6058.31 ± 86.42", r.AvgTS, r.StddevTS)
			}
			if r.SamplesJSON == nil {
				t.Error("pp result has no samples_json, so its stddev cannot be checked")
			}
		case model.TestTG:
			tg++
		}
		if r.RawJSON == "" {
			t.Error("a result has no raw_json")
		}
	}
	if pp != 2 || tg != 2 {
		t.Errorf("got %d pp and %d tg results, want 2 and 2", pp, tg)
	}

	// The argv the runner was handed is the argv the point row stored, which is
	// what "exact resume" means.
	argv := f.run.Argv()
	if len(argv) != 2 {
		t.Fatalf("llama-bench was invoked %d times, want once per point", len(argv))
	}
	var stored []string
	if err := json.Unmarshal([]byte(f.mustPoints(run.ID)[0].ArgsJSON), &stored); err != nil {
		t.Fatalf("args_json: %v", err)
	}
	if diff := cmp.Diff(stored, argv[0]); diff != "" {
		t.Errorf("the executed argv is not the stored argv (-stored +executed):\n%s", diff)
	}
}

// TestPointFailureIsIsolated: per-point failure isolation is one of the four
// reasons section 10 invokes llama-bench once per point. One failing point
// leaves the run `partial` and the other point's results intact.
func TestPointFailureIsIsolated(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := newFixture(t, nil)
	f.run.onPoint = func(_ context.Context, n int) ([]byte, error) {
		if n == 0 {
			return []byte("error: CUDA out of memory\n"), errors.New("exit status 1")
		}
		return mustFixture(t, "llama-bench-pp-tg.json"), nil
	}

	created := f.createRun(simpleSweep(), false)
	if _, err := f.queue.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	run := f.mustRun(created.Run.ID)
	if run.State != model.BenchPartial {
		t.Errorf("run is %s, want partial: some points succeeded and partial results are results",
			run.State)
	}
	if run.PointsDone != 1 || run.PointsFailed != 1 {
		t.Errorf("counters are %d done, %d failed; want 1 and 1", run.PointsDone, run.PointsFailed)
	}

	points := f.mustPoints(run.ID)
	if points[0].State != model.PointFailed {
		t.Errorf("point 0 is %s, want failed", points[0].State)
	}
	if points[0].ErrorMessage == nil {
		t.Error("the failed point records no reason")
	}
	if points[1].State != model.PointSucceeded {
		t.Errorf("point 1 is %s, want succeeded", points[1].State)
	}
	if len(f.mustResults(run.ID)) != 2 {
		t.Errorf("the surviving point's results were lost")
	}
}

// TestEveryPointFailingIsAFailedRun: `failed` only when NOTHING measured.
func TestEveryPointFailingIsAFailedRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := newFixture(t, nil)
	f.run.err = errors.New("exit status 1")
	f.run.stdout = []byte("error: no CUDA device\n")

	created := f.createRun(simpleSweep(), false)
	if _, err := f.queue.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	run := f.mustRun(created.Run.ID)
	if run.State != model.BenchFailed {
		t.Errorf("run is %s, want failed", run.State)
	}
	job := f.mustJob(created.Job.ID)
	if job.State != model.JobFailed {
		t.Errorf("job is %s, want failed", job.State)
	}
	if job.ErrorCode == nil || *job.ErrorCode != string(CodeBenchFailed) {
		t.Errorf("job error_code = %v, want %s", job.ErrorCode, CodeBenchFailed)
	}
}

// TestStopAndRestore is the protocol end to end: an instance loaded on the
// target GPU is stopped, recorded, benchmarked around, and restarted with the
// `bench_restore` trigger — and `restore_done` moves to 1 only after that.
func TestStopAndRestore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := newFixture(t, nil)
	gpus := `["GPU-aaaa"]`
	victim := f.seedInstance("busy", model.InstanceReady, model.AttributionMeasured, &gpus,
		`{"device_filter":"CUDA0"}`)

	sweep := simpleSweep()
	sweep.Base = &model.FlagSet{DeviceFilter: ptr("CUDA0")}
	sweep.OnConflict = ConflictStopAndRestore
	created := f.createRun(sweep, false)

	// Before the worker runs, the run owes nothing: the stop is the WORKER's,
	// because the fleet is live processes and a check made at create time would
	// have missed an instance started since.
	if f.mustRun(created.Run.ID).OwesRestore() {
		t.Fatal("the run recorded stopped instances before it ran")
	}

	if _, err := f.queue.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	run := f.mustRun(created.Run.ID)
	if run.State != model.BenchSucceeded {
		t.Fatalf("run is %s (%v)", run.State, run.ErrorMessage)
	}
	if run.StoppedInstancesJSON == nil {
		t.Fatal("the run did not record the instance it stopped")
	}
	var stopped []string
	if err := json.Unmarshal([]byte(*run.StoppedInstancesJSON), &stopped); err != nil {
		t.Fatalf("stopped_instances_json: %v", err)
	}
	if diff := cmp.Diff([]string{victim}, stopped); diff != "" {
		t.Errorf("stopped_instances_json (-want +got):\n%s", diff)
	}
	if !run.RestoreDone {
		t.Error("restore_done is 0 after a clean finish")
	}

	want := []fleetCall{
		{InstanceID: victim, Desired: model.DesiredStopped, Trigger: ""},
		{InstanceID: victim, Desired: model.DesiredRunning, Trigger: model.TriggerBenchRestore},
	}
	if diff := cmp.Diff(want, f.fleet.Calls()); diff != "" {
		t.Errorf("fleet calls (-want +got):\n%s", diff)
	}

	inst := f.instance(victim)
	if inst.DesiredState != model.DesiredRunning {
		t.Errorf("the instance was left %s", inst.DesiredState)
	}
	if inst.PendingTrigger == nil || *inst.PendingTrigger != model.TriggerBenchRestore {
		t.Errorf("pending_trigger = %v, want %s — `instance_starts.trigger` must say WHY "+
			"the instance came back", inst.PendingTrigger, model.TriggerBenchRestore)
	}

	// The lease is released only once the restore is done, and it is.
	if lease := f.mustLease(); lease.Held() {
		t.Errorf("the lease is still held after a completed restore")
	}
	if f.benchLive() {
		t.Error("BenchLive is true after a completed restore")
	}
}

// failRunning wraps the store so the ONE write between the preflight and the
// point loop fails. Everything else goes through the real store, because the
// property under test is which finalizer runs, not which SQL does.
type failRunning struct {
	Store
	err error
}

func (s failRunning) SetBenchRunState(ctx context.Context, tx store.Tx, id string,
	state model.BenchRunState, at int64) (bool, error) {

	if state == model.BenchRunning {
		return false, s.err
	}
	return s.Store.SetBenchRunState(ctx, tx, id, state, at)
}

// TestFailureAfterTheStopStillRestores: the stop half of stop-and-restore has
// already put production instances down by the time the run is moved to
// `running`, so a failure of THAT write must still route through the finalizer.
//
// Returning it bare would skip the restore entirely — jobs are enqueued with
// MaxAttempts 1, so the job goes straight to terminal `failed`, the store
// refuses to release a lease whose run still owes a restore, `BenchLive` stays
// true and blocks every future bench AND every llama.cpp activation (section 6.6
// step 1), and the stopped instances stay down until the next boot. One
// transient SQLite error is enough.
func TestFailureAfterTheStopStillRestores(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := newFixture(t, func(c *Config) {
		c.Store = failRunning{Store: c.Store, err: errors.New("database is locked")}
	})
	gpus := `["GPU-aaaa"]`
	victim := f.seedInstance("busy", model.InstanceReady, model.AttributionMeasured, &gpus,
		`{"device_filter":"CUDA0"}`)

	sweep := simpleSweep()
	sweep.Base = &model.FlagSet{DeviceFilter: ptr("CUDA0")}
	sweep.OnConflict = ConflictStopAndRestore
	created := f.createRun(sweep, false)

	if _, err := f.queue.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	run := f.mustRun(created.Run.ID)
	if run.State != model.BenchFailed {
		t.Errorf("run is %s, want %s", run.State, model.BenchFailed)
	}
	if !run.RestoreDone {
		t.Fatal("restore_done is 0: the instances this sweep stopped were left down")
	}

	want := []fleetCall{
		{InstanceID: victim, Desired: model.DesiredStopped, Trigger: ""},
		{InstanceID: victim, Desired: model.DesiredRunning, Trigger: model.TriggerBenchRestore},
	}
	if diff := cmp.Diff(want, f.fleet.Calls()); diff != "" {
		t.Errorf("fleet calls (-want +got):\n%s", diff)
	}
	if lease := f.mustLease(); lease.Held() {
		t.Errorf("the lease is still held by job %s; the subsystem is wedged",
			orAbsent(lease.JobID, "nobody"))
	}
	if f.benchLive() {
		t.Error("BenchLive is true, so no bench and no llama.cpp activation can ever run again")
	}
}

// TestAbortRefusesRatherThanStopping: `on_conflict: "abort"` is the default, and
// it refuses with the instances named rather than stopping somebody's production
// server by omission.
func TestAbortRefusesRatherThanStopping(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := newFixture(t, nil)
	gpus := `["GPU-aaaa"]`
	f.seedInstance("busy", model.InstanceReady, model.AttributionMeasured, &gpus,
		`{"device_filter":"CUDA0"}`)

	sweep := simpleSweep()
	sweep.Base = &model.FlagSet{DeviceFilter: ptr("CUDA0")}

	_, err := f.svc.Create(ctx, CreateRequest{
		Name: "aborted", ModelID: f.ModelID, Repetitions: 3, Sweep: sweep,
	})
	if err == nil {
		t.Fatal("Create accepted a sweep that collides with a loaded instance")
	}
	var me model.Error
	if !errors.As(err, &me) || me.Code != CodeBenchGPUConflict {
		t.Fatalf("got %v, want %s", err, CodeBenchGPUConflict)
	}
	if me.Details["instances"] == nil {
		t.Error("the 409 does not name the conflicting instances")
	}
	if len(f.fleet.Calls()) != 0 {
		t.Errorf("an aborted create touched the fleet: %v", f.fleet.Calls())
	}
}

// TestCancelRestores: the finalizer runs on cancellation too, and the remaining
// points are marked `skipped` so `done + failed + skipped == total` holds.
func TestCancelRestores(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := newFixture(t, nil)
	gpus := `["GPU-aaaa"]`
	victim := f.seedInstance("busy", model.InstanceReady, model.AttributionMeasured, &gpus,
		`{"device_filter":"CUDA0"}`)

	sweep := Sweep{
		NBatch:     IntAxis{512, 1024, 2048},
		Tests:      []Test{{PP: ptr(512)}},
		Base:       &model.FlagSet{DeviceFilter: ptr("CUDA0")},
		OnConflict: ConflictStopAndRestore,
	}
	created := f.createRun(sweep, false)

	// Cancel from inside the first point, which is the only moment a cancel has
	// something to interrupt.
	// The cancel is issued from INSIDE the first point, which is the only moment
	// it has something to interrupt, and the hook then waits for the heartbeat to
	// carry `cancel_requested` into the worker's context — the same path §6.5
	// describes, rather than a sleep that would make this test flaky.
	f.run.onPoint = func(ctx context.Context, n int) ([]byte, error) {
		if n != 0 {
			return mustFixture(t, "llama-bench-pp-tg.json"), nil
		}
		if _, err := f.svc.Cancel(context.Background(), created.Run.ID); err != nil {
			t.Errorf("Cancel: %v", err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
			t.Error("the cancel was never carried into the worker")
			return mustFixture(t, "llama-bench-pp-tg.json"), nil
		}
	}

	if _, err := f.queue.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	run := f.mustRun(created.Run.ID)
	if run.State != model.BenchCanceled {
		t.Errorf("run is %s, want canceled", run.State)
	}
	if !run.RestoreDone {
		t.Error("a canceled run did not restore the instances it stopped — the finalizer " +
			"must run on cancellation too")
	}

	var skipped int
	for _, p := range f.mustPoints(run.ID) {
		if p.State == model.PointSkipped {
			skipped++
		}
	}
	if skipped == 0 {
		t.Error("no point was marked skipped, so the counters do not add up to points_total")
	}

	calls := f.fleet.Calls()
	last := calls[len(calls)-1]
	if last.InstanceID != victim || last.Desired != model.DesiredRunning ||
		last.Trigger != model.TriggerBenchRestore {
		t.Errorf("the last fleet call was %+v, want a bench_restore start of %s", last, victim)
	}
}

// TestCrashedBenchRestoresAtBoot is section 15's named integration case, and the
// reason `bench_run` survives a restart as `interrupted` rather than `failed`:
// generic triage marking it `failed` would — under section 2.3a — also mark the
// run `failed`, and a restore rule phrased over `running` would then match
// nothing at all.
//
// The three sub-cases force the run row to `running`, `canceled` and `failed`
// before the boot sweep, proving it keys off `restore_done` and NOT off `state`.
func TestCrashedBenchRestoresAtBoot(t *testing.T) {
	t.Parallel()

	for _, forced := range []model.BenchRunState{
		model.BenchRunning, model.BenchCanceled, model.BenchFailed,
	} {
		t.Run(string(forced), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			f := newFixture(t, nil)
			gpus := `["GPU-aaaa"]`
			victim := f.seedInstance("busy", model.InstanceReady, model.AttributionMeasured,
				&gpus, `{"device_filter":"CUDA0"}`)

			created := f.crashedRun(victim, forced)

			// Boot: the queue triages the previous boot's leases first, exactly
			// as serve() does, and a `bench_run` must survive as `interrupted`.
			triage, err := f.queue.RecoverOrphans(ctx)
			if err != nil {
				t.Fatalf("RecoverOrphans: %v", err)
			}
			if len(triage) != 1 {
				t.Fatalf("triaged %d jobs, want 1", len(triage))
			}
			job := f.mustJob(created.Job.ID)
			if job.State != model.JobInterrupted {
				t.Fatalf("the bench job was triaged to %s; it must survive as %s or the "+
					"restore rule has nothing to act on", job.State, model.JobInterrupted)
			}
			if got := f.mustRun(created.Run.ID).State; got != forced {
				t.Fatalf("triage rewrote the run to %s; the DomainWriter's `interrupted` "+
					"branch must be a no-op", got)
			}

			// The lease is still held by the dead boot, and BenchLive is true —
			// which is what makes section 6.6 step 1 refuse an activation into a
			// half-restored fleet.
			if !f.benchLive() {
				t.Error("BenchLive is false while a run still owes a restore")
			}

			if err := f.svc.Reconcile(ctx); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}

			run := f.mustRun(created.Run.ID)
			if !run.RestoreDone {
				t.Fatal("the boot sweep did not restore the instances the crashed run stopped")
			}
			// `partial` because one point succeeded before the crash.
			if run.State != model.BenchPartial {
				t.Errorf("run is %s, want partial: one point measured before the crash",
					run.State)
			}

			inst := f.instance(victim)
			if inst.DesiredState != model.DesiredRunning {
				t.Errorf("the instance was left %s after the boot restore", inst.DesiredState)
			}
			if inst.PendingTrigger == nil || *inst.PendingTrigger != model.TriggerBenchRestore {
				t.Errorf("pending_trigger = %v, want %s", inst.PendingTrigger,
					model.TriggerBenchRestore)
			}

			job = f.mustJob(created.Job.ID)
			if job.State != model.JobFailed {
				t.Errorf("job is %s, want failed", job.State)
			}
			if job.ErrorCode == nil || *job.ErrorCode != string(model.CodeDaemonRestarted) {
				t.Errorf("job error_code = %v, want %s", job.ErrorCode, model.CodeDaemonRestarted)
			}

			if lease := f.mustLease(); lease.Held() {
				t.Errorf("the dead boot's lease was not reclaimed after the restore")
			}
			if f.benchLive() {
				t.Error("BenchLive is still true after the restore finished")
			}
		})
	}
}

// TestForeignLeaseIsNotReleasedBeforeTheRestore is the second half of section
// 15's lease suite: "a lease whose owner is a dead `boot_id` is reclaimed at boot
// BUT ONLY AFTER the restore finalizer has set `restore_done=1`".
//
// A run that still owes a restart is still occupying the host even though
// nothing is executing, and releasing the lease early would let a second sweep
// start into a fleet the first one has not put back.
func TestForeignLeaseIsNotReleasedBeforeTheRestore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := newFixture(t, nil)
	gpus := `["GPU-aaaa"]`
	victim := f.seedInstance("busy", model.InstanceReady, model.AttributionMeasured, &gpus,
		`{"device_filter":"CUDA0"}`)
	f.crashedRun(victim, model.BenchRunning)

	// The restore cannot complete: the instance will not start.
	f.fleet.failFor = victim
	f.fleet.err = errors.New("systemd is unavailable")

	if err := f.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		released, err := f.store.ReleaseForeignBenchLease(ctx, tx, testBootID)
		if err != nil {
			return err
		}
		if released {
			t.Error("a foreign lease was released while its run still owed a restore")
		}
		return nil
	}); err != nil {
		t.Fatalf("ReleaseForeignBenchLease: %v", err)
	}

	if err := f.svc.Reconcile(ctx); err == nil {
		t.Fatal("Reconcile reported success while the restore failed")
	}
	if f.mustRun(f.onlyRunID()).RestoreDone {
		t.Error("restore_done was set although the instance was not restarted")
	}
	if lease := f.mustLease(); !lease.Held() {
		t.Error("the lease was released although the host is still owed a restart")
	}
	if !f.benchLive() {
		t.Error("BenchLive is false while instances are still down")
	}

	// Once the fleet works again, the next boot's sweep completes and the lease
	// is finally reclaimed. Restoration is idempotent by design.
	f.fleet.failFor = ""
	if err := f.svc.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !f.mustRun(f.onlyRunID()).RestoreDone {
		t.Error("the second sweep did not finish the restore")
	}
	if lease := f.mustLease(); lease.Held() {
		t.Error("the lease was not reclaimed after the restore finished")
	}
}

// crashedRun manufactures the state a SIGKILL mid-sweep leaves behind: a queued
// job leased by a boot that is gone, a lease that boot still holds, one point
// succeeded, one still pending, and `stopped_instances_json` written with
// `restore_done = 0`.
func (f *fixture) crashedRun(victimID string, forced model.BenchRunState) CreateResult {
	f.t.Helper()
	ctx := context.Background()

	sweep := simpleSweep()
	sweep.Base = &model.FlagSet{DeviceFilter: ptr("CUDA0")}
	sweep.OnConflict = ConflictStopAndRestore
	created := f.createRun(sweep, false)

	points := f.mustPoints(created.Run.ID)
	stopped, err := json.Marshal([]string{victimID})
	if err != nil {
		f.t.Fatalf("marshal the stopped list: %v", err)
	}
	s := string(stopped)

	err = f.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		// The previous boot took the lease and stopped the instance.
		if _, err := f.store.AcquireBenchLease(ctx, tx, created.Job.ID, created.Run.ID,
			otherBootID, 1000, 1_000_000_000_000); err != nil {
			return err
		}
		if _, err := f.store.SetBenchRunStopped(ctx, tx, created.Run.ID, &s); err != nil {
			return err
		}
		if _, err := f.store.SetInstanceDesiredState(ctx, tx, victimID,
			model.DesiredStopped, f.clock.Now().UnixMilli()); err != nil {
			return err
		}
		// One point measured before the crash, which is what makes the resolved
		// run `partial` rather than `failed`.
		if _, err := f.store.SetBenchPointState(ctx, tx, points[0].ID, model.PointSucceeded,
			nil, f.clock.Now().UnixMilli()); err != nil {
			return err
		}
		if _, err := f.store.SetBenchRunState(ctx, tx, created.Run.ID, forced,
			f.clock.Now().UnixMilli()); err != nil {
			return err
		}
		// The job as the dead boot left it: leased, with an owner that is gone.
		if _, err := f.store.LeaseNextJob(ctx, tx, store.LeaseParams{
			Owner: otherBootID, Kinds: []model.JobKind{model.JobBenchRun},
			Now: f.clock.Now().UnixMilli(), LeaseExpiresAt: f.clock.Now().UnixMilli() + 60_000,
		}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		f.t.Fatalf("manufacture the crashed run: %v", err)
	}
	return created
}

// onlyRunID returns the single run this fixture created, for the tests that make
// exactly one.
func (f *fixture) onlyRunID() string {
	f.t.Helper()
	var runs []store.BenchRun
	err := f.store.Read(context.Background(), func(ctx context.Context, tx store.Tx) error {
		var err error
		runs, err = f.store.BenchRuns(ctx, tx, store.BenchRunFilter{})
		return err
	})
	if err != nil {
		f.t.Fatalf("list runs: %v", err)
	}
	if len(runs) != 1 {
		f.t.Fatalf("expected exactly one run, got %d", len(runs))
	}
	return runs[0].ID
}
