package jobs

import (
	"context"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// Cancel is `POST /api/v1/jobs/{id}/cancel` (§3.14). It has two shapes, and
// which one applies is decided by whether a worker of THIS daemon holds the
// lease:
//
//   - Held here — `leased` or `running` under this boot's id. `cancel_requested`
//     is raised and the running worker's context is cut at the next heartbeat;
//     the worker decides when it can stop and closes the job itself, beside its
//     domain row. The answer is `202` and the job is still live.
//   - Held by nobody — `queued`, `paused`, or the `interrupted` of §2.3's second
//     bucket. There is no worker to ask, so the queue closes the job and its
//     domain row here, in one transaction (§2.3a).
//
// Two kinds carry a cut-off rather than a blanket accept, and both refuse
// through CancelGuard: `llamacpp_activate` past its step-3 commit, and
// `self_update` at or after the `staged` commit, where D96's answer is
// `409 selfupdate_not_cancelable` because the marker is on disk and the swap
// belongs to systemd. A guard's error is passed through unchanged.
//
// The whole of it runs in ONE `BEGIN IMMEDIATE` transaction, so a `queued` job
// cannot be leased between the guard and the close.
func (q *Queue) Cancel(ctx context.Context, id string) (model.Job, error) {
	var out model.Job
	err := q.s.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		j, err := q.s.Job(ctx, tx, id)
		if err != nil {
			return err
		}
		if j.State.IsTerminal() {
			return ErrNotCancelable
		}
		if g, ok := q.cancelGuard(j.Kind); ok {
			if err := g.CheckCancel(ctx, tx, j); err != nil {
				return err
			}
		}

		if _, err := q.s.RequestJobCancel(ctx, tx, id); err != nil {
			return err
		}
		j.CancelRequested = true

		heldHere := (j.State == model.JobLeased || j.State == model.JobRunning) &&
			j.LeaseOwner != nil && *j.LeaseOwner == q.bootID
		if heldHere {
			out = j
			return nil
		}

		at := ms(q.now())
		if err := q.s.FinishJob(ctx, tx, id, model.JobCanceled, nil, nil, at); err != nil {
			return err
		}
		if err := q.commitDomain(ctx, tx, j, model.JobCanceled); err != nil {
			return err
		}
		j.State = model.JobCanceled
		j.FinishedAt = &at
		j.LeaseOwner, j.LeaseExpiresAt = nil, nil
		out = j
		return nil
	})
	return out, err
}

// Retry returns a stopped job to `queued` and runs it again now. The three
// states it accepts are the three a job can stop in without being finished with:
// `failed`, `canceled`, and the `interrupted` of §2.3's second bucket — which is
// exactly the state D4's warm build directory waits in, and the reason Retry on
// an interrupted install reuses the object files rather than starting over.
//
// The attempt budget is raised to at least `attempts + 1` by the store, because
// a job that exhausted its budget is the most likely thing a human presses Retry
// on, and the domain row moves back to `queued` in the same transaction (§2.3a).
func (q *Queue) Retry(ctx context.Context, id string) (model.Job, error) {
	var out model.Job
	err := q.s.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		j, err := q.s.Job(ctx, tx, id)
		if err != nil {
			return err
		}
		ok, err := q.s.RetryJob(ctx, tx, id, ms(q.now()))
		if err != nil {
			return err
		}
		if !ok {
			return ErrNotRetryable
		}
		if err := q.commitDomain(ctx, tx, j, model.JobQueued); err != nil {
			return err
		}
		out, err = q.s.Job(ctx, tx, id)
		return err
	})
	if err != nil {
		return model.Job{}, err
	}
	q.Wake()
	return out, nil
}

// Pause moves a `running` or `queued` download and its domain row to `paused`,
// releasing the lease — the job half of §2.3a's "pause/resume moves both rows".
// The domain write is the caller's, because `POST /downloads/{id}/pause` knows
// what a paused download row looks like and this package does not.
//
// `paused` counts as live for `idx_jobs_one_live_per_subject`, which is the
// whole reason it is a `jobs` state rather than a downloads-only concept:
// without it a paused download would either hold a lease forever or free its
// subject for a duplicate job.
func (q *Queue) Pause(ctx context.Context, id string, domain CommitFunc) error {
	return q.setLiveState(ctx, id, model.JobPaused, domain)
}

// Resume returns a paused job and its domain row to `queued`.
func (q *Queue) Resume(ctx context.Context, id string, domain CommitFunc) error {
	if err := q.setLiveState(ctx, id, model.JobQueued, domain); err != nil {
		return err
	}
	q.Wake()
	return nil
}

func (q *Queue) setLiveState(ctx context.Context, id string, state model.JobState, domain CommitFunc) error {
	return q.s.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		j, err := q.s.Job(ctx, tx, id)
		if err != nil {
			return err
		}
		if j.State.IsTerminal() {
			return ErrNotCancelable
		}
		if err := q.s.SetJobState(ctx, tx, id, state); err != nil {
			return err
		}
		if domain != nil {
			return domain(ctx, tx, state)
		}
		return q.commitDomain(ctx, tx, j, state)
	})
}

// Triage is one row boot recovery resolved, and the state it was moved to.
type Triage struct {
	// Job is the row as it stood before triage — with the state and lease owner
	// of the boot that died, which is what makes a log line about it readable.
	Job model.Job
	// State is the §2.3 outcome written: `queued`, `interrupted` or `failed`.
	State model.JobState
}

// RecoverOrphans is boot triage (§2.3), and it runs once, before Run, in §11.1's
// boot sequence. Every row left `leased` or `running` by a daemon that is gone —
// the test is the OWNER, not the lease expiry, because a boot that is over is
// over whether or not its horizon has passed — is moved to one of three states,
// with its domain row written in the same transaction:
//
//	queued        model_download, cache_scan, toolchain_probe — idempotent and
//	              resumable, so the activity re-runs from the top and the domain
//	              row returns to `queued` with it
//	interrupted   llamacpp_install, llamacpp_activate, bench_run, self_update —
//	              durable state exists outside the job row that only that
//	              subsystem can settle, so a DOMAIN FINALIZER resolves it and the
//	              domain row KEEPS ITS STATE, because that state is the
//	              finalizer's input. The correct DomainWriter here is a no-op
//	failed        llamacpp_delete, model_verify, model_delete, maintenance, with
//	              error_code `daemon_restarted` — nothing durable is owed that the
//	              row does not already describe, and the domain row is resolved
//	              beside it
//
// `paused` rows are never touched: a pause is a user decision that must survive
// a restart, and it holds its subject against the unique index while it stands.
//
// The whole pass is one transaction: a crash partway through must not leave half
// the fleet triaged and half of it claimable.
func (q *Queue) RecoverOrphans(ctx context.Context) ([]Triage, error) {
	var out []Triage
	err := q.s.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		orphans, err := q.s.OrphanedJobs(ctx, tx, q.bootID)
		if err != nil {
			return err
		}
		at := ms(q.now())
		out = make([]Triage, 0, len(orphans))
		for _, j := range orphans {
			state, err := q.s.TriageOrphanedJob(ctx, tx, j, at)
			if err != nil {
				return err
			}
			if err := q.commitDomain(ctx, tx, j, state); err != nil {
				return err
			}
			out = append(out, Triage{Job: j, State: state})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, t := range out {
		q.log.Info("recovered an orphaned job", "job", t.Job.ID, "kind", t.Job.Kind,
			"was", t.Job.State, "now", t.State)
	}
	if len(out) > 0 {
		q.Wake()
	}
	return out, nil
}
