package jobs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// Run drives the queue until ctx is done, then returns once every in-flight job
// has been given the chance to finish. It is started by the composition root at
// §11.1 step 12 — after RecoverOrphans has triaged the previous boot's rows,
// because a runner that started first could lease a subject whose orphan row has
// not been resolved yet.
//
// Shutdown is deliberately not a state transition. A worker whose context is
// canceled by shutdown leaves its job `running`, and what that row becomes is
// the NEXT boot's decision, taken against §2.3's three-outcome table by a daemon
// that knows which kinds owe durable work.
func (q *Queue) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	for range q.concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			q.runner(ctx)
		}()
	}
	wg.Wait()
	return nil
}

// runner is one concurrency slot: claim, run, repeat; and when there is nothing
// ready, wait for a Wake or for the poll interval, whichever comes first.
func (q *Queue) runner(ctx context.Context) {
	timer := time.NewTimer(q.pollEvery)
	defer timer.Stop()

	for {
		if ctx.Err() != nil {
			return
		}
		ran, err := q.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			q.log.Error("job runner failed to claim work", "error", err)
		}
		if ran {
			// A sibling should look too: one Wake carries one signal, and the
			// enqueue that produced this job may have created several.
			q.Wake()
			continue
		}

		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(q.pollEvery)
		select {
		case <-ctx.Done():
			return
		case <-q.wake:
		case <-timer.C:
		}
	}
}

// RunOnce claims at most one ready job and runs it to completion inline,
// reporting whether it found one. It is the whole of a runner's body, and it is
// the seam a test drives the engine through without a background loop.
func (q *Queue) RunOnce(ctx context.Context) (bool, error) {
	t, err := q.claim(ctx)
	if err != nil || t == nil {
		return false, err
	}
	q.execute(ctx, t)
	return true, nil
}

// claim leases the highest-priority ready job of a registered kind. A nil Task
// with a nil error means the queue is idle, which is the ordinary answer.
func (q *Queue) claim(ctx context.Context) (*Task, error) {
	kinds := q.reg.Kinds()
	if len(kinds) == 0 {
		// Leasing with no kind filter would claim work this daemon cannot run and
		// burn its attempt budget. An empty registry claims nothing.
		return nil, nil
	}

	now := q.now()
	var j model.Job
	err := q.s.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		j, err = q.s.LeaseNextJob(ctx, tx, store.LeaseParams{
			Owner:          q.bootID,
			Kinds:          kinds,
			Now:            ms(now),
			LeaseExpiresAt: ms(now.Add(q.leaseTTL)),
		})
		return err
	})
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, nil
	case err != nil:
		return nil, err
	}
	return &Task{q: q, job: j}, nil
}

// errDeferRollback rolls the start transaction back for a Starter that returned
// Defer. It never leaves this file.
var errDeferRollback = errors.New("jobs: start deferred")

// execute takes a leased job through `running`, the worker, and the terminal
// transition that closes it beside its domain row.
func (q *Queue) execute(ctx context.Context, t *Task) {
	w, ok := q.reg.Worker(t.job.Kind)
	if !ok {
		// The lease filter makes this unreachable; if it happens anyway the job is
		// returned to `queued` rather than failed, because nothing was attempted.
		q.log.Error("leased a job with no worker", "job", t.job.ID, "kind", t.job.Kind,
			"error", ErrNoWorker)
		q.releaseUnrun(ctx, t)
		return
	}

	started, err := q.start(ctx, t, w)
	if err != nil {
		q.log.Error("failed to start job", "job", t.job.ID, "kind", t.job.Kind, "error", err)
		// The start transaction rolled back, so the row is still `leased` under
		// THIS boot's id — and nothing in this process would ever look at it
		// again: LeaseNextJob claims only `queued`, Retry accepts only a job
		// that has stopped, and boot triage keys off a lease_owner that is NOT
		// this boot's. Left alone it would hold its subject against
		// `idx_jobs_one_live_per_subject` until the daemon restarted, which for
		// D70's build-lease acquisition — a store error or a busy writer inside
		// Start — would wedge that `llamacpp_versions` id for good. Nothing ran,
		// so this is an ordinary failed attempt: §2.3's retry while the budget
		// lasts, terminal `failed` when it is spent, with the domain row moved
		// in the same transaction (§2.3a).
		q.finish(ctx, t, Outcome{}, err)
		return
	}
	if !started {
		return
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	stop := make(chan struct{})
	var hb sync.WaitGroup
	hb.Add(1)
	go func() {
		defer hb.Done()
		q.heartbeat(ctx, t, cancel, stop)
	}()

	out, runErr := runWorker(runCtx, w, t)

	close(stop)
	hb.Wait()

	q.finish(ctx, t, out, runErr)
}

// runWorker calls the worker and converts a panic into a failure, so one
// worker's bug cannot take the daemon down with it.
func runWorker(ctx context.Context, w Worker, t *Task) (out Outcome, err error) {
	defer func() {
		if p := recover(); p != nil {
			out = Outcome{}
			err = fmt.Errorf("jobs: worker for %s panicked: %v", t.job.Kind, p)
		}
	}()
	return w.Run(ctx, t)
}

// start moves the job from `leased` to `running` and gives a Starter the same
// transaction to move its domain row — or its singleton lease — in (§2.3a).
//
// It reports false, with a nil error, for the three ways a claim can evaporate
// before any work happens: the lease was taken over, a cancel arrived between
// the claim and the start, and a Starter deferred.
func (q *Queue) start(ctx context.Context, t *Task, w Worker) (bool, error) {
	var (
		deferFor time.Duration
		canceled bool
		started  bool
	)
	now := q.now()

	err := q.s.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		cur, err := q.s.Job(ctx, tx, t.job.ID)
		if err != nil {
			return err
		}
		if cur.State != model.JobLeased || cur.LeaseOwner == nil || *cur.LeaseOwner != q.bootID {
			return nil // somebody else owns it now
		}

		// A cancel that landed between the claim and the start is honored here
		// rather than by a worker that has not begun: there is nothing to wind
		// down, and the guard of §3.14 already ran when the cancel was accepted.
		if cur.CancelRequested {
			if err := q.s.FinishJob(ctx, tx, cur.ID, model.JobCanceled, nil, nil, ms(now)); err != nil {
				return err
			}
			if err := q.commitDomain(ctx, tx, cur, model.JobCanceled); err != nil {
				return err
			}
			canceled = true
			return nil
		}

		ok, err := q.s.StartJob(ctx, tx, cur.ID, q.bootID, ms(now))
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		cur.State = model.JobRunning
		if cur.StartedAt == nil {
			at := ms(now)
			cur.StartedAt = &at
		}

		if s, isStarter := w.(Starter); isStarter {
			if err := s.Start(ctx, tx, cur); err != nil {
				if d, isDeferral := asDeferral(err); isDeferral {
					deferFor = d
					return errDeferRollback
				}
				return err
			}
		}
		t.job = cur
		started = true
		return nil
	})

	if errors.Is(err, errDeferRollback) {
		// §2.3's build-lease queue: the job waits and the UI says "waiting for the
		// running build", which is a queue and not an error, so it wears none and
		// spends no part of its attempt budget.
		return false, q.deferJob(ctx, t.job.ID, deferFor)
	}
	if err != nil {
		return false, err
	}
	if canceled {
		q.log.Info("job canceled before it started", "job", t.job.ID, "kind", t.job.Kind)
	}
	return started, nil
}

// deferJob re-queues a job that never ran, in its own transaction because the
// one that would have started it was rolled back.
func (q *Queue) deferJob(ctx context.Context, id string, after time.Duration) error {
	if after <= 0 {
		after = q.pollEvery
	}
	runAfter := ms(q.now().Add(after))
	return q.s.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		return q.s.DeferJob(ctx, tx, id, runAfter)
	})
}

// releaseUnrun returns a claimed-but-never-started job to `queued` immediately,
// handing back the attempt the claim counted.
func (q *Queue) releaseUnrun(ctx context.Context, t *Task) {
	if err := q.deferJob(ctx, t.job.ID, q.pollEvery); err != nil {
		q.log.Error("failed to release an unrun job", "job", t.job.ID, "error", err)
	}
}

// heartbeat extends the lease on a tick and reads `cancel_requested` in the same
// statement, which is the one query that answers both questions a running worker
// has: do I still own this job, and has somebody asked me to stop (§6.5).
//
// It keeps ticking after a cancel is observed: the worker is winding down and
// still needs its lease, and losing it mid-cleanup would hand the subject to
// somebody else while the first worker is still touching it.
func (q *Queue) heartbeat(ctx context.Context, t *Task, cancel context.CancelFunc, stop <-chan struct{}) {
	tick := time.NewTicker(q.heartbeatEvery)
	defer tick.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-tick.C:
		}

		var cancelRequested bool
		err := q.s.Write(ctx, func(ctx context.Context, tx store.Tx) error {
			var err error
			cancelRequested, err = q.s.TouchJobLease(ctx, tx, t.job.ID, q.bootID,
				ms(q.now().Add(q.leaseTTL)))
			return err
		})
		switch {
		case errors.Is(err, store.ErrNotFound):
			// The row is gone, or the lease was taken over or released — a pause
			// releases it (§2.3a). Either way this worker no longer owns the job.
			t.leaseLost.Store(true)
			cancel()
			return
		case err != nil:
			if ctx.Err() != nil {
				return
			}
			q.log.Warn("job heartbeat failed", "job", t.job.ID, "error", err)
		case cancelRequested && !t.cancelRequested.Swap(true):
			cancel()
		}
	}
}

// finish writes the transition the worker's Outcome asks for, together with the
// domain write that must commit with it (§2.3a).
func (q *Queue) finish(ctx context.Context, t *Task, out Outcome, runErr error) {
	if t.leaseLost.Load() {
		// Writing to a job somebody else now owns is the one thing this must never
		// do; the daemon that holds the lease will close it.
		q.log.Warn("dropping the outcome of a job whose lease was lost",
			"job", t.job.ID, "kind", t.job.Kind)
		return
	}

	// The rewrites below rebuild the Outcome around the worker's own Commit; the
	// hook that publishes what that Commit appends belongs to the same write and
	// travels with it. A deferral is the one case that drops it, because a
	// deferral runs no Commit at all.
	afterCommit := out.AfterCommit

	if runErr != nil {
		if d, ok := asDeferral(runErr); ok {
			out, runErr = Deferred(d), nil
			afterCommit = nil
		}
	}
	if runErr != nil {
		switch {
		case t.cancelRequested.Load() && errors.Is(runErr, context.Canceled):
			// The worker stopped because the cancel it was asked for cut its
			// context. That is a completed cancellation, not a failure.
			out = Canceled(out.Commit)
		case errors.Is(runErr, context.Canceled) && ctx.Err() != nil:
			// The daemon is shutting down. The row stays `running` and the NEXT boot
			// decides what it becomes (§2.3) — a daemon on its way out does not know
			// which kinds owe durable work.
			q.log.Info("leaving a job running across shutdown", "job", t.job.ID, "kind", t.job.Kind)
			return
		default:
			out = RetryableFailure(CodeInternalError, runErr.Error(), out.Commit)
		}
	}
	if out.State == "" {
		out.State = model.JobSucceeded
	}

	// The write must land even when the daemon is shutting down: the work is
	// already done and losing its result would re-run it on the next boot.
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()

	// Whether the domain write ran at all, which is what decides if there is
	// anything for afterCommit to publish. It is set inside the transaction and
	// only ever read after Write returned nil, so no commit that rolled back can
	// reach the wire.
	var domainWritten bool

	err := q.s.Write(fctx, func(ctx context.Context, tx store.Tx) error {
		now := ms(q.now())
		domainWritten = false
		switch {
		case out.State == model.JobQueued:
			// A deferral moves no domain row: nothing about the activity changed,
			// it simply has not started yet.
			after := out.After
			if after <= 0 {
				after = q.pollEvery
			}
			return q.s.DeferJob(ctx, tx, t.job.ID, ms(q.now().Add(after)))

		case out.State == model.JobFailed && out.Retryable && t.job.Attempts < t.job.MaxAttempts:
			// §2.3's retry: `failed → queued` while the budget lasts, with the
			// error kept visible, because a job sitting in `queued` for 32 s with no
			// stated reason is indistinguishable from one that never ran.
			runAfter := q.now().Add(model.JobBackoff(t.job.Attempts))
			if err := q.s.RequeueJob(ctx, tx, t.job.ID, ms(runAfter),
				strPtr(out.ErrorCode), strPtr(out.ErrorMessage)); err != nil {
				return err
			}
			domainWritten = true
			return commit(ctx, tx, q, t.job, out, model.JobQueued)

		default:
			if !out.State.IsTerminal() {
				return fmt.Errorf("jobs: worker for %s returned the non-terminal outcome %q",
					t.job.Kind, out.State)
			}
			if err := q.s.FinishJob(ctx, tx, t.job.ID, out.State,
				strPtr(out.ErrorCode), strPtr(out.ErrorMessage), now); err != nil {
				return err
			}
			domainWritten = true
			return commit(ctx, tx, q, t.job, out, out.State)
		}
	})
	if err == nil {
		// The row has committed, so the frame describes something that happened.
		if domainWritten && afterCommit != nil {
			afterCommit()
		}
		q.notify(ctx, t.job.ID)
		return
	}
	q.log.Error("failed to close job", "job", t.job.ID, "kind", t.job.Kind, "error", err)
	if rerr := q.recoverUnclosed(ctx, t, err); rerr != nil {
		q.log.Error("failed to recover a job that could not be closed",
			"job", t.job.ID, "kind", t.job.Kind, "error", rerr)
	}
}

// recoverUnclosed resolves a job whose closing write failed. The row is still
// `running` under THIS boot's lease, and nothing else in this process would
// ever look at it again — LeaseNextJob claims only `queued`, Retry accepts only
// a job that has stopped, and boot triage keys off a lease_owner that is not
// this boot's — so the subject would be held against
// `idx_jobs_one_live_per_subject` until the daemon restarted. The shutdown
// branch above leaves a row `running` deliberately, for the next boot to triage;
// a failed write leaves the same row behind with none of that intent, so it is
// triaged here instead.
//
// The outcome is §2.3's own three-outcome table, applied by the daemon that
// still owns the lease rather than by the next boot, which is what keeps a kind
// that owes durable work — a bench that stopped production instances, a staged
// self-update — out of `failed`: those become `interrupted`, live for the unique
// index, resolvable by their finalizer or by the user's Retry, and their domain
// row keeps the state that is the finalizer's input.
func (q *Queue) recoverUnclosed(ctx context.Context, t *Task, cause error) error {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()

	return q.s.Write(rctx, func(ctx context.Context, tx store.Tx) error {
		state := model.JobBootTriage(t.job.Kind)
		switch state {
		case model.JobQueued:
			// Idempotent and resumable: re-run from the top, refunding the
			// attempt, exactly as DeferJob does for the build-lease queue.
			if err := q.s.DeferJob(ctx, tx, t.job.ID, ms(q.now())); err != nil {
				return err
			}
		case model.JobInterrupted:
			if err := q.s.SetJobState(ctx, tx, t.job.ID, model.JobInterrupted); err != nil {
				return err
			}
		default:
			// Nothing durable is owed that the row does not already describe.
			// The error is this failure rather than `daemon_restarted`: no
			// daemon restarted, and the two must stay distinguishable.
			state = model.JobFailed
			if err := q.s.FinishJob(ctx, tx, t.job.ID, state,
				strPtr(CodeInternalError), strPtr(cause.Error()), ms(q.now())); err != nil {
				return err
			}
		}
		return q.commitDomain(ctx, tx, t.job, state)
	})
}

// finishTimeout bounds the closing write, which runs on a context deliberately
// detached from the daemon's shutdown.
const finishTimeout = 15 * time.Second

// commit runs the domain write that pairs with a job transition: the Outcome's
// own CommitFunc when it has one, the Worker's DomainWriter otherwise, and
// nothing at all for a kind with no domain row (§2.3a's `maintenance`).
func commit(ctx context.Context, tx store.Tx, q *Queue, j model.Job, out Outcome, state model.JobState) error {
	if out.Commit != nil {
		return out.Commit(ctx, tx, state)
	}
	return q.commitDomain(ctx, tx, j, state)
}

// asDeferral unwraps the sentinel Defer and Deferred travel as.
func asDeferral(err error) (time.Duration, bool) {
	var d *deferral
	if errors.As(err, &d) {
		return d.after, true
	}
	return 0, false
}

// strPtr renders an optional error column: the empty string is NULL, not "".
func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
