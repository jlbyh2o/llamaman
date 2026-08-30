package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// jobs queries (DESIGN sections 2.3 and 2.3a).
//
// These methods are mechanical on purpose. The POLICY — which kind runs where,
// what a worker does with a lease, when a retry is worth making — belongs to
// internal/jobs; what belongs here is that every statement below is a single
// atomic move that a caller can compose with a domain-row write in ONE
// transaction, because §2.3a's whole guarantee is that the job row and its
// domain row are written together by the same worker.
//
// Two invariants the schema enforces, so no method here has to:
//
//   - `idx_jobs_one_live_per_subject` makes a second live job on one subject
//     impossible. InsertJob does not check for one; it lets the insert fail, and
//     the caller answers 409 (§2.7's `download_exists`, §3.14's `job_in_flight`).
//     A check-then-insert would be a race the index exists to remove.
//   - `paused` and `interrupted` count as live, so nothing else can claim a
//     subject while a pause or an unresolved finalizer stands.

const jobColumns = `id, kind, subject_type, subject_id, state, priority, run_after,
	attempts, max_attempts, lease_owner, lease_expires_at, cancel_requested,
	idempotency_key, progress_json, params_json, error_code, error_message,
	created_at, started_at, finished_at`

// InsertJob writes a new job row. The caller supplies the id (a ULID from NewID)
// and the state, which for a fresh job is `queued`.
func (s *Store) InsertJob(ctx context.Context, tx Tx, j model.Job) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO jobs (`+jobColumns+`)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		j.ID, string(j.Kind), string(j.SubjectType), j.SubjectID, string(j.State),
		j.Priority, j.RunAfter, j.Attempts, j.MaxAttempts, j.LeaseOwner, j.LeaseExpiresAt,
		boolInt(j.CancelRequested), j.IdempotencyKey, j.ProgressJSON, j.ParamsJSON,
		j.ErrorCode, j.ErrorMessage, j.CreatedAt, j.StartedAt, j.FinishedAt)
	if err != nil {
		return fmt.Errorf("insert job %s: %w", j.ID, err)
	}
	return nil
}

// Job returns one row by id, or ErrNotFound.
func (s *Store) Job(ctx context.Context, tx Tx, id string) (model.Job, error) {
	row := tx.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id = ?`, id)
	j, err := scanJob(row)
	if err != nil {
		return model.Job{}, notFound(err)
	}
	return j, nil
}

// LiveJobForSubject returns the one live job holding a subject, or ErrNotFound.
// It reads exactly what `idx_jobs_one_live_per_subject` enforces, which is what
// lets a 409 name the job the caller collided with rather than only refusing.
func (s *Store) LiveJobForSubject(ctx context.Context, tx Tx,
	subjectType model.JobSubjectType, subjectID string) (model.Job, error) {
	row := tx.QueryRowContext(ctx,
		`SELECT `+jobColumns+`
		   FROM jobs
		  WHERE subject_type = ? AND subject_id = ? AND state IN `+liveStatesSQL,
		string(subjectType), subjectID)
	j, err := scanJob(row)
	if err != nil {
		return model.Job{}, notFound(err)
	}
	return j, nil
}

// JobFilter selects rows for GET /api/v1/jobs (§3.14).
type JobFilter struct {
	// States, when non-empty, restricts to these states. `?state=active` is
	// expressed by passing model.LiveJobStates().
	States []model.JobState
	// Kinds, when non-empty, restricts to these kinds.
	Kinds []model.JobKind
	// SubjectType and SubjectID, when both set, restrict to one subject's
	// history — the query `idx_jobs_subject` exists for.
	SubjectType model.JobSubjectType
	SubjectID   string
	// Limit caps the result; zero means DefaultJobLimit.
	Limit int
}

// DefaultJobLimit bounds an unfiltered job listing.
const DefaultJobLimit = 200

// Jobs lists rows newest first. Ordering is by id descending rather than by
// created_at: ids are ULIDs, so that is the same order with a unique tiebreak.
func (s *Store) Jobs(ctx context.Context, tx Tx, f JobFilter) ([]model.Job, error) {
	var (
		where []string
		args  []any
	)
	if len(f.States) > 0 {
		ph := make([]string, len(f.States))
		for i, st := range f.States {
			ph[i] = "?"
			args = append(args, string(st))
		}
		where = append(where, "state IN ("+strings.Join(ph, ",")+")")
	}
	if len(f.Kinds) > 0 {
		ph := make([]string, len(f.Kinds))
		for i, k := range f.Kinds {
			ph[i] = "?"
			args = append(args, string(k))
		}
		where = append(where, "kind IN ("+strings.Join(ph, ",")+")")
	}
	if f.SubjectType != "" && f.SubjectID != "" {
		where = append(where, "subject_type = ? AND subject_id = ?")
		args = append(args, string(f.SubjectType), f.SubjectID)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = DefaultJobLimit
	}

	q := `SELECT ` + jobColumns + ` FROM jobs`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("select jobs: %w", err)
	}
	defer rows.Close()

	var out []model.Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// LeaseParams is one attempt to claim work.
type LeaseParams struct {
	// Owner is `runtime_info.boot_id` — the ULID of this daemon start. It is what
	// makes "this lease belongs to a boot that is gone" a string comparison at the
	// next boot.
	Owner string
	// Kinds, when non-empty, restricts the claim to workers registered for these
	// kinds.
	Kinds []model.JobKind
	// Now is the instant `run_after` is compared against.
	Now int64
	// LeaseExpiresAt is the horizon a heartbeat must extend before.
	LeaseExpiresAt int64
}

// LeaseNextJob claims the highest-priority ready job and moves it to `leased` in
// one statement, returning it. ErrNotFound means there was nothing to do, which
// is the ordinary answer on an idle host and not an error condition.
//
// Ordering is `priority, run_after, id`: lower priority runs first (the column's
// own comment), then the job that has been ready longest, then the oldest id.
// `attempts` is incremented here, at the moment the work is actually claimed, so
// the backoff of model.JobBackoff counts attempts made rather than jobs created.
func (s *Store) LeaseNextJob(ctx context.Context, tx Tx, p LeaseParams) (model.Job, error) {
	args := []any{p.Owner, p.LeaseExpiresAt, p.Now}
	kindClause := ""
	if len(p.Kinds) > 0 {
		ph := make([]string, len(p.Kinds))
		for i, k := range p.Kinds {
			ph[i] = "?"
			args = append(args, string(k))
		}
		kindClause = " AND kind IN (" + strings.Join(ph, ",") + ")"
	}

	row := tx.QueryRowContext(ctx,
		`UPDATE jobs
		    SET state = 'leased', lease_owner = ?, lease_expires_at = ?, attempts = attempts + 1
		  WHERE id = (
		        SELECT id FROM jobs
		         WHERE state = 'queued' AND run_after <= ?`+kindClause+`
		         ORDER BY priority, run_after, id
		         LIMIT 1)
		  RETURNING `+jobColumns, args...)
	j, err := scanJob(row)
	if err != nil {
		return model.Job{}, notFound(err)
	}
	return j, nil
}

// HeartbeatJob extends a lease, and only for the daemon that holds it. It
// reports false when the row is gone or the lease has been taken over, which is
// a worker's signal to abandon the work rather than keep writing to a job
// somebody else now owns.
func (s *Store) HeartbeatJob(ctx context.Context, tx Tx, id, owner string, expiresAt int64) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE jobs SET lease_expires_at = ?
		  WHERE id = ? AND lease_owner = ? AND state IN ('leased','running')`,
		expiresAt, id, owner)
	if err != nil {
		return false, fmt.Errorf("heartbeat job %s: %w", id, err)
	}
	return rowsChanged(res)
}

// StartJob moves a leased job to `running` and stamps started_at the first time
// only — a retry of the same row keeps the instant its first attempt began, so
// the history reads as one activity rather than several.
func (s *Store) StartJob(ctx context.Context, tx Tx, id, owner string, at int64) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE jobs
		    SET state = 'running', started_at = COALESCE(started_at, ?)
		  WHERE id = ? AND lease_owner = ? AND state = 'leased'`,
		at, id, owner)
	if err != nil {
		return false, fmt.Errorf("start job %s: %w", id, err)
	}
	return rowsChanged(res)
}

// SetJobProgress writes `progress_json`, the column §6.5 and §10 stream phase
// and percentage into. It deliberately does not touch the lease: progress is not
// a heartbeat, and a worker that reports progress while its lease has lapsed has
// still lost the job.
func (s *Store) SetJobProgress(ctx context.Context, tx Tx, id string, progressJSON *string) error {
	_, err := tx.ExecContext(ctx, `UPDATE jobs SET progress_json = ? WHERE id = ?`, progressJSON, id)
	if err != nil {
		return fmt.Errorf("set job progress %s: %w", id, err)
	}
	return nil
}

// FinishJob closes a job in a terminal state, releasing the lease. The caller
// writes the domain row in the same transaction (§2.3a).
func (s *Store) FinishJob(ctx context.Context, tx Tx, id string,
	state model.JobState, errorCode, errorMessage *string, at int64) error {
	if !state.IsTerminal() {
		return fmt.Errorf("finish job %s: %q is not a terminal state", id, state)
	}
	_, err := tx.ExecContext(ctx,
		`UPDATE jobs
		    SET state = ?, error_code = ?, error_message = ?, finished_at = ?,
		        lease_owner = NULL, lease_expires_at = NULL
		  WHERE id = ?`,
		string(state), errorCode, errorMessage, at, id)
	if err != nil {
		return fmt.Errorf("finish job %s: %w", id, err)
	}
	return nil
}

// RescheduleJob returns a job to `queued` at a future instant and drops its
// lease. It is both the retry of §2.3 — with `run_after` from model.JobBackoff —
// and the build-lease queue of §2.3, where a worker that could not take the
// singleton lease leaves its job queued with `run_after = now + 15 s` and the UI
// says "waiting for the running build", which is a queue and not an error.
func (s *Store) RescheduleJob(ctx context.Context, tx Tx, id string, runAfter int64) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE jobs
		    SET state = 'queued', run_after = ?, lease_owner = NULL, lease_expires_at = NULL
		  WHERE id = ?`,
		runAfter, id)
	if err != nil {
		return fmt.Errorf("reschedule job %s: %w", id, err)
	}
	return nil
}

// SetJobState moves a job to a non-terminal state, releasing the lease when the
// new state no longer holds one. It is how pause and resume move the job half of
// the pair §2.3a keeps in step: `POST /downloads/{id}/pause` sets
// jobs.state='paused' and downloads.state='paused' in ONE transaction, and
// resume returns both to `queued`.
func (s *Store) SetJobState(ctx context.Context, tx Tx, id string, state model.JobState) error {
	if state.IsTerminal() {
		return fmt.Errorf("set job state %s: %q is terminal — use FinishJob", id, state)
	}
	holdsLease := state == model.JobLeased || state == model.JobRunning
	q := `UPDATE jobs SET state = ? WHERE id = ?`
	if !holdsLease {
		q = `UPDATE jobs SET state = ?, lease_owner = NULL, lease_expires_at = NULL WHERE id = ?`
	}
	if _, err := tx.ExecContext(ctx, q, string(state), id); err != nil {
		return fmt.Errorf("set job state %s: %w", id, err)
	}
	return nil
}

// RequestJobCancel raises `cancel_requested`, which a running worker polls. It
// is a REQUEST and not a transition: the worker decides when it can stop, and
// several kinds refuse outright past a cut-off (D96's `staged` self-update,
// §2.3a's activate). Reports false when the job is already in a terminal state,
// where a cancel has nothing to act on.
func (s *Store) RequestJobCancel(ctx context.Context, tx Tx, id string) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE jobs SET cancel_requested = 1 WHERE id = ? AND state IN `+liveStatesSQL, id)
	if err != nil {
		return false, fmt.Errorf("request cancel for job %s: %w", id, err)
	}
	return rowsChanged(res)
}

// OrphanedJobs returns the rows boot triage must resolve: `leased` or `running`
// under a lease_owner that is not this boot's id. §2.3's rule is about the
// OWNER, not about the expiry — a daemon that is gone is gone whether or not its
// lease horizon has passed.
//
// `paused` rows are deliberately not returned: a pause is a user decision that
// must survive a restart.
func (s *Store) OrphanedJobs(ctx context.Context, tx Tx, bootID string) ([]model.Job, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT `+jobColumns+`
		   FROM jobs
		  WHERE state IN ('leased','running')
		    AND (lease_owner IS NULL OR lease_owner != ?)
		  ORDER BY id`, bootID)
	if err != nil {
		return nil, fmt.Errorf("select orphaned jobs: %w", err)
	}
	defer rows.Close()

	var out []model.Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// TriageOrphanedJob applies one row's §2.3 outcome — model.JobBootTriage decides
// which — and returns the state it wrote.
//
// The caller resolves the DOMAIN row in the same transaction, and what that
// means differs per outcome, which is the whole point of the three buckets:
// a `queued` row's domain row returns to `queued` with it; a `failed` row's
// domain row is resolved beside it; and an `interrupted` row's domain row KEEPS
// ITS STATE, because that state is precisely the finalizer's input.
func (s *Store) TriageOrphanedJob(ctx context.Context, tx Tx, j model.Job, at int64) (model.JobState, error) {
	outcome := model.JobBootTriage(j.Kind)
	switch outcome {
	case model.JobQueued:
		// Re-run from the top: the lease is dropped and the attempt that died is
		// not counted against max_attempts, because nothing was attempted — the
		// daemon went away.
		_, err := tx.ExecContext(ctx,
			`UPDATE jobs
			    SET state = 'queued', run_after = ?, attempts = MAX(attempts - 1, 0),
			        lease_owner = NULL, lease_expires_at = NULL
			  WHERE id = ?`, at, j.ID)
		if err != nil {
			return "", fmt.Errorf("triage job %s to queued: %w", j.ID, err)
		}
	case model.JobInterrupted:
		// The lease is released but the row stays LIVE for the unique index, so
		// nothing else can claim this subject until the domain finalizer, the user
		// or a retry resolves it.
		_, err := tx.ExecContext(ctx,
			`UPDATE jobs
			    SET state = 'interrupted', lease_owner = NULL, lease_expires_at = NULL
			  WHERE id = ?`, j.ID)
		if err != nil {
			return "", fmt.Errorf("triage job %s to interrupted: %w", j.ID, err)
		}
	case model.JobFailed:
		code := string(model.CodeDaemonRestarted)
		msg := "the daemon restarted while this job was running"
		if err := s.FinishJob(ctx, tx, j.ID, model.JobFailed, &code, &msg, at); err != nil {
			return "", err
		}
	default:
		return "", fmt.Errorf("triage job %s: no boot outcome for kind %q", j.ID, j.Kind)
	}
	return outcome, nil
}

// liveStatesSQL is the WHERE-clause form of model.LiveJobStates, spelled once so
// this file and `idx_jobs_one_live_per_subject` cannot drift apart. A test
// asserts the two lists are equal.
const liveStatesSQL = `('queued','leased','running','paused','interrupted')`

// rowScanner is satisfied by both *sql.Row and *sql.Rows, so one scanJob serves
// the single-row reads and the listings.
type rowScanner interface{ Scan(dest ...any) error }

func scanJob(sc rowScanner) (model.Job, error) {
	var (
		j               model.Job
		kind            string
		subjectType     string
		state           string
		cancelRequested int64
	)
	err := sc.Scan(&j.ID, &kind, &subjectType, &j.SubjectID, &state, &j.Priority, &j.RunAfter,
		&j.Attempts, &j.MaxAttempts, &j.LeaseOwner, &j.LeaseExpiresAt, &cancelRequested,
		&j.IdempotencyKey, &j.ProgressJSON, &j.ParamsJSON, &j.ErrorCode, &j.ErrorMessage,
		&j.CreatedAt, &j.StartedAt, &j.FinishedAt)
	if err != nil {
		return model.Job{}, err
	}
	j.Kind = model.JobKind(kind)
	j.SubjectType = model.JobSubjectType(subjectType)
	j.State = model.JobState(state)
	j.CancelRequested = cancelRequested != 0
	return j, nil
}
