package llamacpp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jlbyh2o/llamaman/internal/events"
	"github.com/jlbyh2o/llamaman/internal/jobs"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// The `llamacpp_activate` worker: DESIGN section 6.6, steps 2 through 5, plus
// the boot reconciliation that is also this kind's finalizer (§2.3).
//
// The ordering in this file is the design's, and it is the whole of D24:
//
//  1. one transaction moves `is_active`, `previous_active` and every instance's
//     `config_hash` (§6.6 steps 2 and 3);
//  2. then, and only then, the two symlinks are repaired FROM those rows;
//  3. then the canary roll.
//
// A canary failure runs the mirror image, in the same order: rows first,
// symlinks second, canary third. Reverting only the symlink would be worse than
// not reverting at all — §6.6's boot reconciliation makes the ROW win, so the
// next daemon start would re-point `active` at the failed build and restart
// every instance onto it, and the fleet would meanwhile carry a permanent false
// `restart_required` from a recompute nobody undid.

// ActivateWorkerConfig wires the worker's one collaborator.
type ActivateWorkerConfig struct {
	// Roller performs the canary roll. Nil is a documented mode rather than a
	// bug: a daemon with no systemd control channel (F10, §11.1a) starts
	// nothing, so an activation commits and every instance simply wears
	// `restart_required` until a human restarts it — which is exactly what
	// `restart_instances: "none"` asks for anyway.
	Roller Roller
}

// ActivateWorker runs `llamacpp_activate`.
type ActivateWorker struct {
	svc  *Service
	roll Roller
}

// NewActivateWorker builds the worker.
func (s *Service) NewActivateWorker(cfg ActivateWorkerConfig) *ActivateWorker {
	return &ActivateWorker{svc: s, roll: cfg.Roller}
}

// Kind implements jobs.Worker.
func (w *ActivateWorker) Kind() model.JobKind { return model.JobLlamacppActivate }

// CheckCancel implements jobs.CancelGuard: §2.3a's activate column says a cancel
// is accepted only before the step-3 transaction commits, and "has it
// committed" is not a flag this worker keeps — it is the target row's own
// `is_active`, which is the one place that cannot be wrong.
func (w *ActivateWorker) CheckCancel(ctx context.Context, tx store.Tx, j model.Job) error {
	var p activateParams
	if err := decodeParams(j.ParamsJSON, &p); err != nil {
		return err
	}
	target, err := w.svc.store.LlamacppVersion(ctx, tx, p.VersionID)
	if err != nil {
		return err
	}
	if target.IsActive {
		return withDetails(errorf(CodeActivationNotCancelable,
			"llama.cpp %s is already active; roll back instead of canceling", p.VersionID),
			map[string]any{"version_id": p.VersionID})
	}
	return nil
}

// SetDomainState implements jobs.DomainWriter. Every cell of §2.3a's activate
// column is the same version state — `ready` — because an activation never
// moves the row through the pipeline states at all: what it moves is two flags,
// a hash and two symlinks. So this is a no-op in every direction, and saying so
// explicitly is what stops a future edit from "helpfully" marking a version
// `failed` when its canary was the thing that failed.
func (w *ActivateWorker) SetDomainState(context.Context, store.Tx, model.Job,
	model.JobState) error {
	return nil
}

// Run implements jobs.Worker.
func (w *ActivateWorker) Run(ctx context.Context, t *jobs.Task) (jobs.Outcome, error) {
	var p activateParams
	if err := decodeParams(t.Job().ParamsJSON, &p); err != nil {
		return jobs.Outcome{}, err
	}
	now := w.svc.now()

	var (
		target store.LlamacppVersion
		act    store.Activation
	)
	// Steps 2 and 3: ONE transaction. `config_hash` folds in the active version
	// id (D52), so the flip changes that input for every instance at once —
	// and leaving the recompute out of this transaction is what would make the
	// stored hash silently disagree with its own definition.
	var sink events.Sink
	err := t.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		target, err = w.svc.store.LlamacppVersion(ctx, tx, p.VersionID)
		if err != nil {
			return err
		}
		if target.State != model.VersionReady {
			return errorf(CodeVersionNotReady,
				"llama.cpp %s is %s, not ready", target.ID, target.State)
		}
		act, err = w.svc.store.ActivateLlamacppVersion(ctx, tx, p.VersionID,
			p.KeepPrevious, now.UnixMilli())
		if err != nil {
			return err
		}
		if err := w.svc.insts.RecomputeConfigHash(ctx, tx); err != nil {
			return err
		}
		// §6.6 step 2's record, written here because the candidate is only known
		// once the flags have moved — and written BEFORE the roll, so a daemon
		// restart mid-roll still finds the delete this activation owes.
		p.DeletionCandidateID = act.DeletionCandidateID
		if err := w.svc.store.SetJobParams(ctx, tx, t.Job().ID, jsonPtr(p)); err != nil {
			return err
		}
		return w.svc.event(ctx, tx, now, target.ID, "llamacpp_activated", model.LevelInfo,
			fmt.Sprintf("llama.cpp %s is the active build", target.ID), nil, nil, &sink)
	})
	if err != nil {
		return w.activationFailed(p, err), nil
	}

	// Step 4: the symlinks, repaired from the rows that just committed.
	if err := w.repairLinks(ctx); err != nil {
		return jobs.Outcome{}, err
	}
	w.svc.publish(&sink)

	// §11.2's `llamacpp` step, closed here and nowhere else.
	//
	// It is the wizard's one non-skippable step after the password, and its
	// meaning is precisely this moment: not "a build was downloaded" but "a
	// build is ACTIVE", which is what every instance executes out of and what
	// `POST /setup/complete` refuses without. Nothing marked it, so a host that
	// had installed and activated a build still had the step `active`, every
	// step behind it blocked, and the wizard permanently unfinishable.
	//
	// It runs after the commit and its failure is logged rather than returned:
	// the activation succeeded, and failing the job over the wizard's
	// bookkeeping would roll back nothing and lose the build.
	if w.svc.wizard != nil {
		if err := w.svc.wizard.MarkStep(ctx, model.StepLlamacpp); err != nil {
			w.svc.log.Warn("could not mark the wizard's llama.cpp step complete",
				"version_id", target.ID, "error", err)
		}
	}

	// Step 5: the canary roll, when one was asked for.
	if p.RestartInstances == RestartRolling {
		if err := w.rollFleet(ctx, t, p, act); err != nil {
			return w.revert(ctx, p, act, err), nil
		}
	}

	return w.activationSucceeded(p, act), nil
}

// rollFleet is §6.6 step 5. The canary decides: its failure reverts the whole
// activation, and a LATER failure does not — by then instances are serving on
// the new build, and reverting under them is a second unplanned restart of
// everything that already worked.
func (w *ActivateWorker) rollFleet(ctx context.Context, t *jobs.Task, p activateParams,
	act store.Activation) error {

	if w.roll == nil {
		w.svc.log.Warn("a rolling restart was requested but this daemon cannot restart instances",
			"version_id", p.VersionID)
		return nil
	}
	targets, err := w.roll.Targets(ctx, p.CanaryInstanceID)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		return nil
	}

	for i, target := range targets {
		if err := t.SetProgress(ctx, map[string]any{
			"phase": "rolling", "done": i, "total": len(targets), "current": target.Name,
		}); err != nil {
			w.svc.log.Debug("could not write roll progress", "error", err)
		}

		if err := w.roll.Restart(ctx, target); err != nil {
			if i == 0 {
				// The canary. Its failure is the one that is cheap to undo, and
				// undoing it is the whole reason it goes first.
				return &canaryError{target: target, err: err}
			}
			// A later failure stops the roll and leaves the remainder untouched:
			// "3 of 7 migrated" is the honest report, and continuing silently
			// would not be.
			w.svc.log.Warn("the rolling restart stopped part way",
				"version_id", p.VersionID, "migrated", i, "total", len(targets),
				"instance", target.Name, "error", err)
			_ = w.notify(ctx, model.SeverityWarn, "rolling_restart_incomplete",
				"The rolling restart did not finish",
				fmt.Sprintf("%d of %d instances are on %s; %s did not come back: %v",
					i, len(targets), p.VersionID, target.Name, err),
				p.VersionID)
			return nil
		}
	}
	return nil
}

// canaryError marks the one roll failure that reverts.
type canaryError struct {
	target RollTarget
	err    error
}

func (e *canaryError) Error() string {
	return fmt.Sprintf("the canary %s did not come back: %v", e.target.Name, e.err)
}

func (e *canaryError) Unwrap() error { return e.err }

// revert is §6.6 step 5's revert, in the order the design fixes: rows, then
// symlinks, then the canary.
func (w *ActivateWorker) revert(ctx context.Context, p activateParams, act store.Activation,
	cause error) jobs.Outcome {

	now := w.svc.now()
	// context.WithoutCancel: a revert that stopped half way because the roll's
	// context was canceled would leave exactly the disagreement this whole
	// routine exists to prevent.
	rctx := context.WithoutCancel(ctx)

	var sink events.Sink
	err := w.svc.store.Write(rctx, func(ctx context.Context, tx store.Tx) error {
		if err := w.svc.store.RestoreLlamacppFlags(ctx, tx, act.Before); err != nil {
			return err
		}
		// Without this second recompute every instance would wear a permanent
		// "restart to apply" prompt against a version that was rolled back:
		// `applied_config_hash` was never touched, and the stored hash would
		// still fold in the abandoned version id.
		if err := w.svc.insts.RecomputeConfigHash(ctx, tx); err != nil {
			return err
		}
		return w.svc.event(ctx, tx, now, p.VersionID, "llamacpp_activation_reverted",
			model.LevelError, cause.Error(), nil, nil, &sink)
	})
	if err != nil {
		// The rows are the thing that must be right. If they cannot be put
		// back, say so loudly and leave the symlinks alone rather than making
		// the two disagree.
		w.svc.log.Error("could not revert a failed activation",
			"version_id", p.VersionID, "error", err)
		return jobs.Failed(string(CodeCanaryFailed),
			fmt.Sprintf("%v; the activation could not be reverted: %v", cause, err), nil)
	}

	if err := w.repairLinks(rctx); err != nil {
		w.svc.log.Error("could not repair the version symlinks after a revert",
			"version_id", p.VersionID, "error", err)
	}

	// Restore the canary onto the build it was running before, and abort the
	// rollout without touching any other instance.
	var ce *canaryError
	if errors.As(cause, &ce) && w.roll != nil {
		if err := w.roll.Restart(rctx, ce.target); err != nil {
			w.svc.log.Error("the canary did not come back on the previous build",
				"instance", ce.target.Name, "error", err)
		}
	}

	_ = w.notify(rctx, model.SeverityError, string(CodeCanaryFailed),
		"The canary failed and the activation was rolled back",
		fmt.Sprintf("%v. Every instance is back on the build it was running.", cause),
		p.VersionID)
	w.svc.publish(&sink)

	return jobs.Failed(string(CodeCanaryFailed), cause.Error(), nil)
}

// activationSucceeded closes the job and, only now, enqueues the delete of the
// build that lost its rollback slot (§6.6 step 2). Enqueuing it earlier was a
// bug of ordering: step 5 may revert the whole activation, and it cannot revert
// a version directory a delete worker has already removed.
func (w *ActivateWorker) activationSucceeded(p activateParams, act store.Activation) jobs.Outcome {
	now := w.svc.now()
	sink := &events.Sink{}
	out := jobs.Succeeded(func(ctx context.Context, tx store.Tx, _ model.JobState) error {
		if act.DeletionCandidateID == "" {
			return nil
		}
		_, err := w.svc.queue.EnqueueTx(ctx, tx, jobs.EnqueueParams{
			Kind:     model.JobLlamacppDelete,
			DomainID: act.DeletionCandidateID,
			Params:   deleteParams{VersionID: act.DeletionCandidateID},
			Domain: func(ctx context.Context, tx store.Tx, _ model.Job) error {
				return w.svc.event(ctx, tx, now, act.DeletionCandidateID,
					"llamacpp_version_delete_requested", model.LevelInfo,
					fmt.Sprintf("llama.cpp %s is no longer retained and was queued for deletion",
						act.DeletionCandidateID), nil, nil, sink)
			},
		})
		var me model.Error
		if errors.As(err, &me) && me.Code == model.CodeJobInFlight {
			// Something already holds that subject. The activation is not the
			// place to fight over it, and the version stays on disk until the
			// next GC has a clear run at it.
			w.svc.log.Info("the retired build already has a live job; leaving it on disk",
				"version_id", act.DeletionCandidateID)
			return nil
		}
		return err
	})
	out.AfterCommit = func() { w.svc.publish(sink) }
	return out
}

// activationFailed closes a job whose step-3 transaction never committed. No
// version state changed, so nothing has to be undone.
func (w *ActivateWorker) activationFailed(p activateParams, cause error) jobs.Outcome {
	code := model.ErrorCode(CodeVersionNotReady)
	var me model.Error
	if errors.As(cause, &me) {
		code = me.Code
	}
	w.svc.log.Warn("an activation did not commit", "version_id", p.VersionID, "error", cause)
	return jobs.Failed(string(code), cause.Error(), nil)
}

// repairLinks rebuilds `versions/active` and `versions/previous` FROM the rows —
// §6.6's boot reconciliation, and the same two lines step 4 and the revert both
// use, because "the row wins" must be one implementation or it is not an
// invariant.
func (w *ActivateWorker) repairLinks(ctx context.Context) error {
	return w.svc.RepairLinks(ctx)
}

// RepairLinks points `versions/active` and `versions/previous` at whatever the
// rows say, and removes a link no row claims.
//
// It is idempotent and cheap, which is why it runs at boot, after every
// activation and after every revert rather than only where a disagreement is
// suspected: the DB is the source of truth and the symlink is the mechanism, so
// re-deriving one from the other is never wrong.
func (s *Service) RepairLinks(ctx context.Context) error {
	var active, previous string
	err := s.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		row, err := s.store.LlamacppVersionByFlag(ctx, tx, false)
		switch {
		case err == nil:
			active = row.DirName
		case !errors.Is(err, store.ErrNotFound):
			return err
		}
		row, err = s.store.LlamacppVersionByFlag(ctx, tx, true)
		switch {
		case err == nil:
			previous = row.DirName
		case !errors.Is(err, store.ErrNotFound):
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}

	for _, link := range []struct {
		name    string
		dirName string
	}{{ActiveLink, active}, {PreviousLink, previous}} {
		if link.dirName == "" {
			if err := s.layout.RemoveLink(link.name); err != nil {
				return err
			}
			continue
		}
		if err := s.layout.SetLink(link.name, link.dirName); err != nil {
			return err
		}
	}
	return nil
}

// finishJob closes an `interrupted` job the queue is not running. It is the one
// place this package writes a `jobs` row directly, and it exists because a
// finalizer by definition has no worker holding the lease: there is nothing for
// the queue's own close path to act on.
func (s *Service) finishJob(ctx context.Context, tx store.Tx, id string,
	state model.JobState, code, message string, now time.Time) error {

	return s.store.FinishJob(ctx, tx, id, state, strPtrOrNil(code), strPtrOrNil(message),
		now.UnixMilli())
}

// notify raises one notification, in its own transaction, and never fails the
// operation that asked for it.
func (w *ActivateWorker) notify(ctx context.Context, severity model.NotificationSeverity,
	code, title, body, versionID string) error {

	if w.svc.notify == nil {
		return nil
	}
	return w.svc.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		return w.svc.notify.Notify(ctx, tx, severity, code, title, body, versionID)
	})
}

// Reconcile is §6.6's boot reconciliation, and it is also the `llamacpp_activate`
// finalizer §2.3 requires (the kind is triaged to `interrupted`, which means a
// domain finalizer — this one — must resolve it).
//
// Three things happen, in this order:
//
//  1. the build lease is released if a boot that is gone still holds it (D70);
//  2. `versions/active` and `versions/previous` are repaired from the rows;
//  3. every `interrupted` activation job is closed, and THE ROW DECIDES how —
//     `succeeded` when the `is_active=1` version is the job's target (the step-3
//     transaction committed, and only a roll nobody is waiting on was lost),
//     `failed` with `daemon_restarted` when it is not (the transaction never
//     committed and nothing happened).
//
// No reading of the boot state can re-activate a build whose canary failed,
// because step 5 reverted the row before the daemon ever went down.
func (s *Service) Reconcile(ctx context.Context) error {
	now := s.now()

	if s.bootID != "" {
		if err := s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
			_, err := s.store.ReleaseForeignBuildLease(ctx, tx, s.bootID)
			return err
		}); err != nil {
			return err
		}
	}

	if err := s.RepairLinks(ctx); err != nil {
		return err
	}

	var interrupted []model.Job
	if err := s.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		interrupted, err = s.store.Jobs(ctx, tx, store.JobFilter{
			Kinds:  []model.JobKind{model.JobLlamacppActivate},
			States: []model.JobState{model.JobInterrupted},
		})
		return err
	}); err != nil {
		return err
	}

	for _, j := range interrupted {
		if err := s.closeInterruptedActivation(ctx, j, now); err != nil {
			return err
		}
	}
	return nil
}

// closeInterruptedActivation resolves one row of the finalizer above.
func (s *Service) closeInterruptedActivation(ctx context.Context, j model.Job,
	now time.Time) error {

	var p activateParams
	if err := decodeParams(j.ParamsJSON, &p); err != nil {
		// A job with no readable params cannot be resolved by its own rule, and
		// leaving it `interrupted` would hold its subject forever.
		return s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
			return s.finishJob(ctx, tx, j.ID, model.JobFailed,
				string(model.CodeDaemonRestarted), err.Error(), now)
		})
	}

	var committed bool
	if err := s.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		active, err := s.store.LlamacppVersionByFlag(ctx, tx, false)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		committed = active.ID == p.VersionID
		return nil
	}); err != nil {
		return err
	}

	var sink events.Sink
	if !committed {
		if err := s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
			if err := s.event(ctx, tx, now, p.VersionID, "llamacpp_activation_abandoned",
				model.LevelWarn,
				fmt.Sprintf("the daemon restarted before %s was activated; nothing changed",
					p.VersionID), nil, nil, &sink); err != nil {
				return err
			}
			return s.finishJob(ctx, tx, j.ID, model.JobFailed,
				string(model.CodeDaemonRestarted),
				"the daemon restarted before the activation committed", now)
		}); err != nil {
			return err
		}
		s.publish(&sink)
		return nil
	}

	// The activation is complete but for a roll nobody is waiting on. Close it
	// `succeeded`, enqueue the delete the params named if one is due, and offer
	// the restart that did not finish — `restart_required` is already true on
	// every running instance, because step 3 recomputed the hashes.
	if err := s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		if p.DeletionCandidateID != "" {
			if _, err := s.queue.EnqueueTx(ctx, tx, jobs.EnqueueParams{
				Kind:     model.JobLlamacppDelete,
				DomainID: p.DeletionCandidateID,
				Params:   deleteParams{VersionID: p.DeletionCandidateID},
			}); err != nil {
				var me model.Error
				if !errors.As(err, &me) || me.Code != model.CodeJobInFlight {
					return err
				}
			}
		}
		if s.notify != nil && p.RestartInstances == RestartRolling {
			if err := s.notify.Notify(ctx, tx, model.SeverityWarn,
				"rolling_restart_interrupted",
				"A rolling restart did not finish",
				fmt.Sprintf("llama.cpp %s is active. The daemon restarted part way through the "+
					"rolling restart, so some instances are still on the previous build.",
					p.VersionID), p.VersionID); err != nil {
				return err
			}
		}
		if err := s.event(ctx, tx, now, p.VersionID, "llamacpp_activation_completed",
			model.LevelInfo,
			fmt.Sprintf("llama.cpp %s was already active when the daemon restarted", p.VersionID),
			nil, nil, &sink); err != nil {
			return err
		}
		return s.finishJob(ctx, tx, j.ID, model.JobSucceeded, "", "", now)
	}); err != nil {
		return err
	}
	s.publish(&sink)
	return nil
}
