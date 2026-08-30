package bench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jlbyh2o/llamaman/internal/jobs"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// The stop-and-restore protocol and its boot reconciliation (DESIGN section 10).
//
// "A benchmark that leaves production instances down is the worst possible
// outcome, so restoration is idempotent and re-checked at boot." Three
// properties follow from that sentence and each is load-bearing:
//
//  1. `stopped_instances_json` is written BEFORE the first StopUnit, not after
//     the last one. The window between "we stopped it" and "we wrote down that
//     we stopped it" is the one window in which a crash loses an instance
//     forever, and writing first closes it. The cost is that a crash in the
//     other direction leaves a run claiming to have stopped an instance it did
//     not — and the restore's answer to "this instance is already running" is to
//     do nothing, so that costs nothing.
//  2. The finalizer runs on success, failure AND cancellation, from the worker,
//     and again at boot from Reconcile. Four call sites, one idempotent
//     function.
//  3. `restore_done = 1` is written only after EVERY named instance has been
//     restarted, found already running, or found deleted. A partial restore
//     leaves the column at 0 so the next boot tries again — and leaves the D75
//     lease held, so no second sweep starts into a half-restored fleet.

// preflight is the stop half: the exclusivity guard, and the stop-and-restore
// when the policy asks for one.
//
// It runs at EXECUTION time rather than at create time because the fleet is live
// processes: an instance started in the minute between the POST and the lease is
// exactly the collision the guard exists to prevent, and a check made against
// rows read at create time would have missed it.
func (w *Worker) preflight(ctx context.Context, t *jobs.Task, run *store.BenchRun) error {
	s := w.svc
	if !s.settingBool(ctx, "bench.exclusive_gpu", true) {
		return nil
	}

	sweep, err := ParseSweep([]byte(run.SweepJSON))
	if err != nil {
		return err
	}
	base := model.FlagSet{}
	if sweep.Base != nil {
		base = *sweep.Base
	}
	policy := sweep.OnConflict
	if policy == "" {
		policy = ConflictAbort
	}

	_, inv := s.probeGPUs(ctx)
	target := inv.Resolve(base)

	var conflicts []Occupancy
	if err := s.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		views, err := s.store.InstanceViews(ctx, tx, store.InstanceFilter{})
		if err != nil {
			return err
		}
		conflicts = Conflicts(target, views, inv)
		return nil
	}); err != nil {
		return err
	}
	if len(conflicts) == 0 {
		return nil
	}

	if policy == ConflictAbort {
		return withDetails(errorf(CodeBenchGPUConflict,
			"%d instance(s) are loaded on the GPUs this benchmark needs", len(conflicts)),
			ConflictDetails(conflicts))
	}
	if s.fleet == nil {
		return errorf(CodeBenchGPUConflict,
			"this daemon cannot stop instances, so the %d conflicting one(s) must be stopped by hand",
			len(conflicts))
	}

	ids := make([]string, 0, len(conflicts))
	for _, c := range conflicts {
		ids = append(ids, c.InstanceID)
	}

	// Property 1: write the list FIRST. From this commit until
	// MarkBenchRestoreDone, every path — including a SIGKILL one statement
	// later — leaves a row the boot sweep will find.
	b, err := json.Marshal(ids)
	if err != nil {
		return fmt.Errorf("bench: record the instances to stop: %w", err)
	}
	stopped := string(b)
	now := s.now()
	if err := s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		if _, err := s.store.SetBenchRunStopped(ctx, tx, run.ID, &stopped); err != nil {
			return err
		}
		return s.appendEvent(ctx, tx, *run, now, "bench_stopped_instances", model.LevelWarn,
			fmt.Sprintf("%s is stopping %d instance(s) for exclusive GPU access", run.Name, len(ids)))
	}); err != nil {
		return err
	}
	run.StoppedInstancesJSON = &stopped
	run.RestoreDone = false

	for _, id := range ids {
		if _, err := s.fleet.SetDesiredState(ctx, id, model.DesiredStopped, ""); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			return fmt.Errorf("bench: stop instance %s: %w", id, err)
		}
	}
	s.reconcileNow(ctx)

	// Wait for them to actually go down. A stop that has not been observed
	// within the grace period does NOT fail the sweep: the list is already
	// written, so the restore still owes every one of them a restart, and
	// benchmarking beside an instance that is on its way down is a worse answer
	// than benchmarking a second later.
	if err := s.settle(ctx, ids, s.stopGrace, func(v model.InstanceView) bool {
		return !Loaded(v.Status.State)
	}); err != nil {
		s.log.Warn("bench: some instances had not stopped within the grace period",
			"run", run.ID, "error", err)
	}
	_ = t
	return nil
}

// Restore is the finalizer. It is idempotent, and it is called from four places:
// the worker's success path, its failure path, its cancellation path, and the
// boot reconciliation.
//
// A run that stopped nothing returns immediately without writing anything: there
// is no restore owed and setting `restore_done` on a run that never armed the
// boot sweep would be a claim about work that did not happen.
func (s *Service) Restore(ctx context.Context, runID string) error {
	var run store.BenchRun
	if err := s.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		run, err = s.store.BenchRun(ctx, tx, runID)
		return err
	}); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	if !run.OwesRestore() {
		return nil
	}

	var ids []string
	if err := json.Unmarshal([]byte(*run.StoppedInstancesJSON), &ids); err != nil {
		// A list this daemon cannot read is a list it cannot act on. Marking the
		// restore done would be a lie; leaving it undone would wedge the lease
		// forever. The honest middle is to say so loudly and mark it done, so the
		// host is not held hostage by one unreadable column — the instances are
		// still startable by hand and the event says which run stopped them.
		s.log.Error("bench: the stopped-instance list could not be read; "+
			"restart the affected instances by hand",
			"run", run.ID, "stopped_instances_json", *run.StoppedInstancesJSON)
		return s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
			_, err := s.store.MarkBenchRestoreDone(ctx, tx, run.ID)
			return err
		})
	}

	if s.fleet == nil {
		return fmt.Errorf("bench: run %s owes %d instance(s) a restart and this daemon has no "+
			"way to start them", run.ID, len(ids))
	}

	var restored []string
	for _, id := range ids {
		// `bench_restore` is the trigger §2.8's vocabulary reserves for exactly
		// this, so `instance_starts.trigger` says WHY the instance came back and
		// D64's crash-loop counter can tell a bench restore from a crash.
		if _, err := s.fleet.SetDesiredState(ctx, id, model.DesiredRunning,
			model.TriggerBenchRestore); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				// Found deleted. §10 names this as one of the three outcomes
				// that count as restored: there is nothing to put back.
				continue
			}
			return fmt.Errorf("bench: restore instance %s: %w", id, err)
		}
		restored = append(restored, id)
	}
	s.reconcileNow(ctx)

	now := s.now()
	if err := s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		if _, err := s.store.MarkBenchRestoreDone(ctx, tx, run.ID); err != nil {
			return err
		}
		return s.appendEvent(ctx, tx, run, now, "bench_restored_instances", model.LevelInfo,
			fmt.Sprintf("%s restarted %d of %d instance(s) it stopped",
				run.Name, len(restored), len(ids)))
	}); err != nil {
		return err
	}
	s.publish(run, now, "bench_restored_instances")
	return nil
}

// Reconcile is the boot half of §10, and it runs after the job queue's orphan
// triage has produced this boot's `interrupted` rows.
//
// Its three steps are ordered by a dependency, not by taste:
//
//  1. restore every run with `restore_done = 0 AND stopped_instances_json IS NOT
//     NULL`, in ANY state — the sweep keys off what the HOST is owed, never off
//     what the benchmark did;
//  2. resolve every `interrupted` `bench_run` job into a terminal pair (§2.3a):
//     the run to `partial` if any point succeeded and `failed` otherwise, the job
//     to `failed` with `error_code='daemon_restarted'`, both in one transaction;
//  3. release any lease this boot does not own — LAST, and the store refuses it
//     anyway while a restore is outstanding, which is what makes the ordering a
//     property of the schema rather than of this function.
func (s *Service) Reconcile(ctx context.Context) error {
	var owing []store.BenchRun
	if err := s.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		owing, err = s.store.BenchRunsOwingRestore(ctx, tx)
		return err
	}); err != nil {
		return err
	}
	var errs []error
	for _, run := range owing {
		s.log.Info("bench: a previous boot left instances stopped for a benchmark",
			"run", run.ID, "state", string(run.State))
		if err := s.Restore(ctx, run.ID); err != nil {
			errs = append(errs, err)
		}
	}

	if err := s.resolveInterrupted(ctx); err != nil {
		errs = append(errs, err)
	}

	if err := s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		released, err := s.store.ReleaseForeignBenchLease(ctx, tx, s.bootID)
		if err == nil && released {
			s.log.Info("bench: released a bench lease held by a boot that is gone")
		}
		return err
	}); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// resolveInterrupted is step 2: §2.3a's pairing for the one cell that carries a
// recovery.
//
// The job survived the restart as `interrupted` rather than `failed` precisely so
// that this function has something to resolve — and so that a restore rule
// phrased over `bench_runs.state='running'` was never needed, because the run
// row was left untouched.
func (s *Service) resolveInterrupted(ctx context.Context) error {
	var interrupted []model.Job
	if err := s.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		interrupted, err = s.store.Jobs(ctx, tx, store.JobFilter{
			Kinds:  []model.JobKind{model.JobBenchRun},
			States: []model.JobState{model.JobInterrupted},
		})
		return err
	}); err != nil {
		return err
	}

	now := s.now()
	var errs []error
	for _, j := range interrupted {
		p, err := decodeRunParams(j.ParamsJSON)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
			points, err := s.store.BenchPoints(ctx, tx, p.RunID)
			if err != nil {
				return err
			}
			done, failed := countFinished(points)

			// Partial results are results: a sweep that measured forty points
			// before the daemon stopped is `partial`, not `failed`.
			state := model.BenchFailed
			if done > 0 {
				state = model.BenchPartial
			}
			message := "the daemon restarted while this benchmark was running"

			if _, err := s.store.SkipPendingBenchPoints(ctx, tx, p.RunID, now.UnixMilli()); err != nil {
				return err
			}
			if _, err := s.store.FinishBenchRun(ctx, tx, p.RunID, state, done, failed,
				&message, now.UnixMilli()); err != nil {
				return err
			}
			code := string(model.CodeDaemonRestarted)
			if err := s.store.FinishJob(ctx, tx, j.ID, model.JobFailed, &code, &message,
				now.UnixMilli()); err != nil {
				return err
			}
			// The lease, last and inside the same transaction. The store's own
			// guard refuses this while the run still owes a restore, which is
			// why step 1 ran first.
			_, err = s.store.ReleaseBenchLease(ctx, tx, j.ID)
			return err
		}); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// reconcileNow asks the supervisor to take its one corrective action immediately
// rather than at its next tick, so a stop or a restore proceeds at the pace of
// the units rather than of the poll interval. A nil Reconciler simply waits for
// the loop, which is slower and equally correct.
func (s *Service) reconcileNow(ctx context.Context) {
	if s.sup == nil {
		return
	}
	if err := s.sup.Reconcile(ctx); err != nil {
		s.log.Warn("bench: the supervisor pass failed", "error", err)
	}
}

// settle waits until a condition holds over every named instance, driving the
// supervisor between looks.
func (s *Service) settle(ctx context.Context, ids []string, within time.Duration,
	done func(model.InstanceView) bool) error {

	deadline := s.now().Add(within)
	for {
		var pending []string
		if err := s.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
			views, err := s.store.InstanceViews(ctx, tx, store.InstanceFilter{IDs: ids})
			if err != nil {
				return err
			}
			for _, v := range views {
				if !done(v) {
					pending = append(pending, v.Name)
				}
			}
			return nil
		}); err != nil {
			return err
		}
		if len(pending) == 0 {
			return nil
		}
		if s.now().After(deadline) {
			return fmt.Errorf("bench: %v were still not settled after %s", pending, within)
		}

		t := time.NewTimer(s.restorePoll)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
		s.reconcileNow(ctx)
	}
}
