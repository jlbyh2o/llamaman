package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// Worker runs one kind of job. Exactly one is registered per kind, and it owns
// everything about that kind the queue deliberately does not know: what the work
// is, which failures are worth retrying, and — the part §2.3a makes structural —
// how the DOMAIN row moves alongside the job row.
type Worker interface {
	// Kind is the one `jobs.kind` this worker runs.
	Kind() model.JobKind

	// Run performs the work. The job is already `running` and its lease is being
	// heartbeaten; ctx is canceled when the daemon shuts down, when
	// `cancel_requested` is raised (§6.5), and when the lease is lost.
	//
	// The returned Outcome carries the domain write that must commit in the SAME
	// transaction as the job's terminal state. Returning a bare error is allowed
	// and is treated as a retryable failure with error_code CodeInternalError,
	// but a kind with a vocabulary for its failures should return an Outcome that
	// names one.
	Run(ctx context.Context, t *Task) (Outcome, error)
}

// Starter is implemented by a Worker whose domain row — or whose singleton lease
// — must move in the SAME transaction that moves the job from `leased` to
// `running` (§2.3a).
//
// It is also the only place a worker may DEFER: returning Defer(d) rolls the
// start transaction back and leaves the job `queued` with `run_after = now + d`.
// That is §2.3's build-lease queue exactly — the install worker acquires
// `build_lease` by conditional UPDATE inside this transaction, and zero rows
// changed means another build holds it, so the job waits 15 s and the UI says
// "waiting for the running build", which is a queue and not an error (D70).
type Starter interface {
	Start(ctx context.Context, tx store.Tx, j model.Job) error
}

// DomainWriter is implemented by a Worker whose domain row must move in the same
// transaction as a job transition the QUEUE performs on its behalf, with no
// worker running to do it: boot triage (§2.3), a cancel of a job no worker
// holds, and a retry. `state` is what the queue is about to write to
// `jobs.state`, and §2.3a's invariant table is the specification of what the
// domain row must then be.
//
// For an `interrupted` triage the correct implementation is a NO-OP, and that is
// the whole point of §2.3's second bucket: the domain row keeps its state —
// `bench_runs.state='running'` with `restore_done=0`, `self_updates.state` at
// whichever non-terminal value it held, `llamacpp_versions` at its build state
// with the directory kept — because that state is precisely the finalizer's
// input, and overwriting it destroys the recovery that follows.
type DomainWriter interface {
	SetDomainState(ctx context.Context, tx store.Tx, j model.Job, state model.JobState) error
}

// CancelGuard is implemented by a Worker whose kind carries a cut-off rather
// than a blanket accept (§3.14). Both of the two that do are the same shape:
// `llamacpp_activate` is cancelable only before its step-3 transaction commits,
// and `self_update` only before the `staged` commit — at or after it, D96 says
// the answer is `409 selfupdate_not_cancelable`, because from that instant the
// marker is on disk and the swap is a unit systemd owns.
//
// Returning a non-nil error refuses the cancel and the queue passes it through
// unchanged, so a guard should return the model.Error its section names.
type CancelGuard interface {
	CheckCancel(ctx context.Context, tx store.Tx, j model.Job) error
}

// CommitFunc is the domain-row write that commits with a job transition (§2.3a).
// `state` is the state actually written to `jobs.state`, which is not always the
// State the Outcome asked for: a retryable failure still inside its attempt
// budget becomes `queued`, not `failed`.
type CommitFunc func(ctx context.Context, tx store.Tx, state model.JobState) error

// Outcome is how a Worker ends a run.
type Outcome struct {
	// State is the disposition: `succeeded`, `failed` or `canceled` to close the
	// job, or `queued` to defer it by After.
	State model.JobState

	// ErrorCode and ErrorMessage are written to the job row's columns. §2.3a puts
	// the code on the job and the message on the domain row for several kinds;
	// nothing stops a worker from writing both.
	ErrorCode    string
	ErrorMessage string

	// Retryable marks a failure the queue may return to `queued` with
	// model.JobBackoff's delay while `attempts < max_attempts` (§2.3). A failure
	// that will fail again the same way — bad flags, a vocabulary mismatch, a
	// refusal — should leave it false.
	Retryable bool

	// After is how long a `queued` outcome waits before it is ready again.
	After time.Duration

	// Commit is the domain write that commits with the transition. When it is
	// nil and the Worker implements DomainWriter, SetDomainState is called
	// instead; when neither is present the job row moves alone, which is correct
	// only for a kind with no domain row — `maintenance`, whose job row IS the
	// record (§2.3a).
	Commit CommitFunc

	// AfterCommit runs once Commit's transaction has COMMITTED, and never if it
	// did not run or did not commit. It is the second half of DESIGN §1's
	// "emits an events row, and publishes an SSE frame" for the transitions a
	// worker writes on its way out — `ready`, `failed`, `canceled`, `deleted`.
	//
	// Those are precisely the transitions a service cannot publish for itself:
	// the write lives in a closure the queue runs inside its own closing
	// transaction, minutes after the method that built the Outcome returned, so
	// there is no "after the write" for the worker to hook. Without this the
	// terminal states are written to `events` and never reach the wire, and the
	// audit views — the Events screen, the dashboard's recent events — miss
	// every ending while narrating every beginning.
	//
	// It runs on the queue's goroutine, so it must not block: publishing to the
	// Hub is what it is for.
	AfterCommit func()
}

// Succeeded closes the job `succeeded`.
func Succeeded(commit CommitFunc) Outcome {
	return Outcome{State: model.JobSucceeded, Commit: commit}
}

// Failed closes the job `failed` with no retry: the queue writes the terminal
// state even if the attempt budget is not spent.
func Failed(code, message string, commit CommitFunc) Outcome {
	return Outcome{State: model.JobFailed, ErrorCode: code, ErrorMessage: message, Commit: commit}
}

// RetryableFailure fails in a way §2.3's backoff may re-run: the job returns to
// `queued` at `now + min(60s, 2^attempts × 2s)` while `attempts < max_attempts`,
// and becomes terminal `failed` on the attempt that exhausts the budget.
func RetryableFailure(code, message string, commit CommitFunc) Outcome {
	return Outcome{
		State: model.JobFailed, ErrorCode: code, ErrorMessage: message,
		Retryable: true, Commit: commit,
	}
}

// Canceled closes the job `canceled`, which is what a worker returns once it has
// stopped for a `cancel_requested` it observed.
func Canceled(commit CommitFunc) Outcome {
	return Outcome{State: model.JobCanceled, Commit: commit}
}

// Deferred puts the job back in `queued` for another try in d, spending no part
// of the attempt budget and wearing no error. It is a queue, not a failure.
func Deferred(d time.Duration) Outcome {
	return Outcome{State: model.JobQueued, After: d}
}

// Defer is the Starter form of Deferred: returned from Start, it rolls that
// transaction back and re-queues the job in d.
func Defer(d time.Duration) error { return &deferral{after: d} }

type deferral struct{ after time.Duration }

func (d *deferral) Error() string {
	return fmt.Sprintf("jobs: deferred for %s", d.after)
}

// Task is one leased job, handed to the Worker that runs it.
type Task struct {
	q   *Queue
	job model.Job

	cancelRequested atomic.Bool
	leaseLost       atomic.Bool
}

// Job is the row as it stood at the moment it was claimed — with `attempts`
// already incremented, because LeaseNextJob counts the claim.
func (t *Task) Job() model.Job { return t.job }

// Now is the queue's clock, so a worker computing a deadline uses the same one
// the queue writes timestamps from (and the one a test replaces).
func (t *Task) Now() time.Time { return t.q.now() }

// Log is the queue's logger, already carrying nothing about this job; a worker
// that wants job-scoped fields should derive its own With.
func (t *Task) Log() *slog.Logger { return t.q.log }

// CancelRequested reports whether the heartbeat has seen `cancel_requested=1`.
// A worker that cannot simply respect ctx cancellation — one in the middle of a
// step that must not be abandoned halfway — polls this instead.
func (t *Task) CancelRequested() bool { return t.cancelRequested.Load() }

// LeaseLost reports whether the heartbeat found the lease gone: the row was
// taken over, or paused, which releases it (§2.3a). A worker that sees this must
// abandon the work rather than keep writing to a job somebody else now owns; the
// queue will not close a job whose lease it lost.
func (t *Task) LeaseLost() bool { return t.leaseLost.Load() }

// SetProgress marshals v into `progress_json` — the column §6.5 streams build
// phases into and §10 streams `{points_done, points_total, current}` into.
//
// It deliberately does not touch the lease: progress is not a heartbeat, and a
// worker that reports progress while its lease has lapsed has still lost the
// job.
func (t *Task) SetProgress(ctx context.Context, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("jobs: marshal progress for %s: %w", t.job.ID, err)
	}
	return t.SetProgressJSON(ctx, string(b))
}

// SetProgressJSON writes `progress_json` verbatim. The column carries a
// json_valid CHECK, so a malformed string is refused by the database.
func (t *Task) SetProgressJSON(ctx context.Context, progressJSON string) error {
	if err := t.q.s.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		return t.q.s.SetJobProgress(ctx, tx, t.job.ID, &progressJSON)
	}); err != nil {
		return err
	}
	// Section 3.14's "progress arrives over SSE". This is the only moment a
	// long action has anything new to say between its start and its end, so a
	// queue that wrote progress and did not publish it left every progress bar
	// in the app frozen at whatever it read when it mounted.
	t.q.notify(ctx, t.job.ID)
	return nil
}

// Write runs fn in one write transaction, which is how a worker moves its domain
// row through the finer states §2.3a folds to `running` —
// `downloads.resolving`/`verifying`, `llamacpp_versions.fetching`/`building` —
// without leaving the queue's hands.
func (t *Task) Write(ctx context.Context, fn func(context.Context, store.Tx) error) error {
	return t.q.s.Write(ctx, fn)
}
