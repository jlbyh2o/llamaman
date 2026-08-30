package store

import (
	"context"
	"fmt"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// jobs queries the queue engine needs beyond the ones in jobs.go (DESIGN
// sections 2.3 and 2.3a).
//
// Same rule as that file: every statement here is one atomic move a caller can
// compose with a domain-row write in ONE transaction, and none of them decides
// anything. Which failures are worth retrying, how long a lease lives, when a
// deferral is a queue rather than an error — all of that is internal/jobs.

// TouchJobLease extends a lease and reads `cancel_requested` in one statement,
// which is what lets a single heartbeat tick answer both questions the running
// worker has: do I still own this job, and has somebody asked me to stop
// (§6.5: `jobs.cancel_requested=1` → context cancel → SIGTERM the process group).
//
// ErrNotFound means the row is gone or the lease has been taken over or
// released — a pause releases it (§2.3a) — and that is a worker's signal to
// abandon the work rather than keep writing to a job somebody else now owns.
func (s *Store) TouchJobLease(ctx context.Context, tx Tx, id, owner string, expiresAt int64) (bool, error) {
	var cancelRequested int64
	err := tx.QueryRowContext(ctx,
		`UPDATE jobs SET lease_expires_at = ?
		  WHERE id = ? AND lease_owner = ? AND state IN ('leased','running')
		  RETURNING cancel_requested`,
		expiresAt, id, owner).Scan(&cancelRequested)
	if err != nil {
		return false, notFound(err)
	}
	return cancelRequested != 0, nil
}

// RequeueJob returns a job to `queued` at a future instant, drops its lease and
// writes the error columns. It is RescheduleJob plus those two columns, and both
// of its callers need them:
//
//   - The retry of §2.3 — `failed → queued` while `attempts < max_attempts`, with
//     `run_after` from model.JobBackoff — keeps the error that caused the retry
//     visible, because a job silently sitting in `queued` for 32 s with no stated
//     reason is indistinguishable in the UI from one that never ran.
//   - A deferral passes NULL for both, which clears a previous attempt's error:
//     §2.3's build-lease queue leaves the job `queued` with
//     `run_after = now + 15 s` and the UI says "waiting for the running build",
//     which is a queue and not an error, so it must not wear one.
//
// `attempts` is deliberately untouched: it counts claims, and LeaseNextJob is
// what increments it.
func (s *Store) RequeueJob(ctx context.Context, tx Tx, id string, runAfter int64, errorCode, errorMessage *string) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE jobs
		    SET state = 'queued', run_after = ?, error_code = ?, error_message = ?,
		        lease_owner = NULL, lease_expires_at = NULL
		  WHERE id = ?`,
		runAfter, errorCode, errorMessage, id)
	if err != nil {
		return fmt.Errorf("requeue job %s: %w", id, err)
	}
	return nil
}

// DeferJob is RequeueJob's sibling for the case that is a QUEUE rather than a
// failure: §2.3's build-lease wait, where the install worker could not take the
// `build_lease` singleton and the job "stays `queued` with `run_after = now +
// 15 s` and the UI says 'waiting for the running build'".
//
// It differs from RequeueJob in the one column that matters: `attempts` is
// decremented back, because LeaseNextJob counted a claim that turned out not to
// be an attempt at anything. A deferral that spent budget would let a job whose
// only problem is a busy neighbor exhaust `max_attempts` and fail without ever
// having run. The error columns are cleared for the same reason — a job waiting
// its turn must not wear a previous attempt's error.
func (s *Store) DeferJob(ctx context.Context, tx Tx, id string, runAfter int64) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE jobs
		    SET state = 'queued', run_after = ?, attempts = MAX(attempts - 1, 0),
		        error_code = NULL, error_message = NULL,
		        lease_owner = NULL, lease_expires_at = NULL
		  WHERE id = ?`,
		runAfter, id)
	if err != nil {
		return fmt.Errorf("defer job %s: %w", id, err)
	}
	return nil
}

// RetryJob is the user's Retry button: a job that stopped — `failed`,
// `canceled`, or the `interrupted` of §2.3's second bucket, which is exactly the
// state D4's warm build directory is waiting in — returns to `queued` and runs
// again now.
//
// Two columns move for reasons a plain requeue would get wrong. `max_attempts`
// is raised to at least `attempts + 1`, because a job that exhausted its budget
// is the most likely thing a human presses Retry on, and leaving the budget
// where it is would hand the job straight back to a queue that immediately
// refuses to run it. `cancel_requested` is cleared, because a retry of a job the
// user canceled must not be canceled again the moment a worker claims it.
//
// It reports false when the job was not in one of those three states — a live
// job has nothing to retry — and the caller resolves the domain row in the same
// transaction (§2.3a).
func (s *Store) RetryJob(ctx context.Context, tx Tx, id string, runAfter int64) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE jobs
		    SET state = 'queued', run_after = ?, cancel_requested = 0,
		        error_code = NULL, error_message = NULL, finished_at = NULL,
		        lease_owner = NULL, lease_expires_at = NULL,
		        max_attempts = MAX(max_attempts, attempts + 1)
		  WHERE id = ? AND state IN ('failed','canceled','interrupted')`,
		runAfter, id)
	if err != nil {
		return false, fmt.Errorf("retry job %s: %w", id, err)
	}
	return rowsChanged(res)
}

// ExpireJobLeases brings every lease this daemon holds to its end, which is
// §9.4 step 5's "close the job queue's leases" during shutdown. It does NOT
// change any state: a job that was `running` when the daemon went down is still
// `running`, and resolving it is boot triage's decision (§2.3), made by the next
// daemon against the three-outcome table rather than guessed at by the one on
// its way out.
func (s *Store) ExpireJobLeases(ctx context.Context, tx Tx, owner string, at int64) (int64, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE jobs SET lease_expires_at = ?
		  WHERE lease_owner = ? AND state IN ('leased','running')`, at, owner)
	if err != nil {
		return 0, fmt.Errorf("expire job leases for %s: %w", owner, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("expire job leases for %s: %w", owner, err)
	}
	return n, nil
}

// CountLiveJobsByKind counts the live rows of one kind, which is the read behind
// the guards §3.14 states over a kind rather than over a subject: `409
// job_in_flight` while a build or any `self_update` job is live, `interrupted`
// included. `idx_jobs_one_live_per_subject` cannot answer that question — it is
// per subject, and two `llamacpp_install` jobs on two different version ids are
// legal under it (D70).
func (s *Store) CountLiveJobsByKind(ctx context.Context, tx Tx, kind model.JobKind) (int, error) {
	var n int
	err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM jobs WHERE kind = ? AND state IN `+liveStatesSQL,
		string(kind)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count live %s jobs: %w", kind, err)
	}
	return n, nil
}
