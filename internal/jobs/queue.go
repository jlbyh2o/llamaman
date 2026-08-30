package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// Defaults for the knobs DESIGN does not pin. They are policy, not contract:
// section 2.3 fixes the lease COLUMN and the backoff formula and says nothing
// about how often a heartbeat writes or how many jobs run at once, so these are
// chosen here and overridable through Options rather than smuggled in as
// constants some other package would have to guess at.
const (
	// DefaultLeaseTTL is how far ahead `lease_expires_at` is set. It only has to
	// outlive one heartbeat interval by a comfortable margin; boot triage keys off
	// the OWNER, not the expiry (§2.3), so an over-long lease never strands a job
	// across a restart.
	DefaultLeaseTTL = 60 * time.Second
	// DefaultHeartbeatEvery is the tick that extends the lease and reads
	// `cancel_requested` (§6.5).
	DefaultHeartbeatEvery = 15 * time.Second
	// DefaultPollEvery is how long an idle runner waits before asking for work
	// again. Enqueue wakes the runners directly, so this is the backstop for a job
	// that became ready by the clock — a backoff retry or a deferral — rather than
	// the ordinary path.
	DefaultPollEvery = time.Second
	// DefaultConcurrency is how many jobs run at once. Kind-level exclusivity is
	// not this number's job: one build at a time is `build_lease` (D70) and one
	// bench at a time is `bench_lease` (D75), both acquired by the worker.
	DefaultConcurrency = 4
	// DefaultPriority is the schema default of `jobs.priority`. Lower runs first.
	DefaultPriority = 100
)

// CodeInternalError is written to `jobs.error_code` when a Worker returns a bare
// error, or panics, instead of an Outcome naming its own code. Every kind that
// has a vocabulary for its failures uses it (§6.5's build phases, §2.5's
// `delete_incomplete`); this is the fallback for the failure a worker did not
// anticipate, and it is deliberately distinguishable from `daemon_restarted`.
const CodeInternalError = "internal_error"

var (
	// ErrNoWorker means a job of this kind was found with nothing registered to
	// run it — a bug in the composition root, not a job failure, so the row is
	// left queued rather than burned through its attempt budget.
	ErrNoWorker = errors.New("jobs: no worker is registered for this kind")

	// ErrNotCancelable means the job is already in a terminal state, where a
	// cancel has nothing to act on. A kind that refuses a cancel for a reason of
	// its own returns its own model.Error instead (D96's
	// `selfupdate_not_cancelable`), through CancelGuard.
	ErrNotCancelable = errors.New("jobs: the job is not in a cancelable state")

	// ErrNotRetryable means the job is live, so there is nothing to retry.
	ErrNotRetryable = errors.New("jobs: the job is not in a retryable state")
)

// Options configures a Queue. Only BootID is required.
type Options struct {
	// BootID is `runtime_info.boot_id`, the ULID of this daemon start, and it is
	// what every lease this queue takes is owned by. It is what makes "this lease
	// belongs to a boot that is gone" a string comparison at the next boot (§2.3),
	// so it must be THIS boot's id and never a constant.
	BootID string

	// Now supplies every instant this package writes. Nil means time.Now.
	Now func() time.Time

	// LeaseTTL, HeartbeatEvery, PollEvery and Concurrency default to the
	// Default* constants above when zero.
	LeaseTTL       time.Duration
	HeartbeatEvery time.Duration
	PollEvery      time.Duration
	Concurrency    int

	// Logger defaults to slog.Default.
	Logger *slog.Logger

	// Publisher is the `jobs` SSE topic (section 3.14, publish.go). Nil
	// publishes nothing, which is the right behavior for a queue in a test and
	// the wrong one in a daemon: without it every screen that narrates work is
	// stale until reloaded.
	Publisher Publisher
}

// Queue is the job engine: one per daemon, constructed by the composition root
// after the store is open and migrated, and started at §11.1 step 12 — after
// RecoverOrphans has triaged the previous boot's rows.
type Queue struct {
	s   *store.Store
	reg *Registry
	log *slog.Logger

	bootID         string
	now            func() time.Time
	leaseTTL       time.Duration
	heartbeatEvery time.Duration
	pollEvery      time.Duration
	concurrency    int

	// publisher is the `jobs` SSE topic. See publish.go.
	publisher Publisher

	// wake carries at most one pending "there is new work" signal, so an Enqueue
	// does not wait out a poll interval. Losing one is harmless: the runners poll.
	wake chan struct{}
}

// New constructs a Queue. It fails only on a missing boot id, because a queue
// that leased under the wrong owner would make the next boot's triage lie in
// both directions — abandoning this daemon's live work, and adopting a dead
// daemon's.
func New(s *store.Store, opts Options) (*Queue, error) {
	if s == nil {
		return nil, errors.New("jobs: a store is required")
	}
	if opts.BootID == "" {
		return nil, errors.New("jobs: Options.BootID is required (runtime_info.boot_id)")
	}
	q := &Queue{
		s:              s,
		reg:            NewRegistry(),
		log:            opts.Logger,
		bootID:         opts.BootID,
		now:            opts.Now,
		leaseTTL:       opts.LeaseTTL,
		heartbeatEvery: opts.HeartbeatEvery,
		pollEvery:      opts.PollEvery,
		concurrency:    opts.Concurrency,
		publisher:      opts.Publisher,
		wake:           make(chan struct{}, 1),
	}
	if q.log == nil {
		q.log = slog.Default()
	}
	if q.now == nil {
		q.now = time.Now
	}
	if q.leaseTTL <= 0 {
		q.leaseTTL = DefaultLeaseTTL
	}
	if q.heartbeatEvery <= 0 {
		q.heartbeatEvery = DefaultHeartbeatEvery
	}
	if q.pollEvery <= 0 {
		q.pollEvery = DefaultPollEvery
	}
	if q.concurrency <= 0 {
		q.concurrency = DefaultConcurrency
	}
	return q, nil
}

// BootID is the lease owner every claim this queue makes is stamped with.
func (q *Queue) BootID() string { return q.bootID }

// Registry is the Worker registry this queue leases for.
func (q *Queue) Registry() *Registry { return q.reg }

// Register adds a Worker, and is the ordinary way the composition root wires
// one. It is Registry.Register.
func (q *Queue) Register(w Worker) error { return q.reg.Register(w) }

// Wake tells the runners to look for work now rather than at the next poll. It
// is called for you by Enqueue and EnqueueTx; a caller that inserts a job row by
// some other route may call it directly. A spurious wake costs one query.
func (q *Queue) Wake() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// Job returns one row by id, or store.ErrNotFound. It is the read behind
// `GET /api/v1/jobs/{id}` (§3.14).
func (q *Queue) Job(ctx context.Context, id string) (model.Job, error) {
	var j model.Job
	err := q.s.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		j, err = q.s.Job(ctx, tx, id)
		return err
	})
	return j, err
}

// List returns rows newest first, and is the read behind `GET /api/v1/jobs`.
// `?state=active` is store.JobFilter{States: model.LiveJobStates()}.
func (q *Queue) List(ctx context.Context, f store.JobFilter) ([]model.Job, error) {
	var out []model.Job
	err := q.s.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		out, err = q.s.Jobs(ctx, tx, f)
		return err
	})
	return out, err
}

// LiveJobFor returns the live job holding a kind's subject, or store.ErrNotFound.
// It is what lets a caller answer `409 job_in_flight` with the id of the job it
// collided with, before it has built a whole request.
func (q *Queue) LiveJobFor(ctx context.Context, kind model.JobKind, domainID string) (model.Job, error) {
	subjectType, subjectID := model.SubjectFor(kind, domainID)
	if subjectType == "" {
		return model.Job{}, fmt.Errorf("jobs: %w: %q", errUnknownKind, kind)
	}
	var j model.Job
	err := q.s.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		j, err = q.s.LiveJobForSubject(ctx, tx, subjectType, subjectID)
		return err
	})
	return j, err
}

// ReleaseLeases brings every lease this daemon holds to its end without moving
// any state — §9.4 step 5's "close the job queue's leases" on the way down. What
// a `running` row becomes is the NEXT boot's decision, made against §2.3's
// three-outcome table by a daemon that knows which kinds owe durable work, not a
// guess made by the one shutting down.
func (q *Queue) ReleaseLeases(ctx context.Context) (int64, error) {
	var n int64
	err := q.s.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		n, err = q.s.ExpireJobLeases(ctx, tx, q.bootID, ms(q.now()))
		return err
	})
	return n, err
}

var errUnknownKind = errors.New("unknown job kind")

// errJobInFlight is §3.14's `409 job_in_flight`, naming the live job the caller
// collided with rather than only refusing.
func errJobInFlight(existing model.Job) error {
	return model.Error{
		Code: model.CodeJobInFlight,
		Message: fmt.Sprintf("a %s job is already live for this %s",
			existing.Kind, existing.SubjectType),
		Details: map[string]any{
			"job_id":       existing.ID,
			"kind":         string(existing.Kind),
			"state":        string(existing.State),
			"subject_type": string(existing.SubjectType),
			"subject_id":   existing.SubjectID,
		},
	}
}

// errIdempotencyKeyReused is D65's `422 idempotency_key_reused`: a hit inside the
// window whose route or request fingerprint differs. The client sent two
// different requests under one key, which is a bug rather than a replay.
func errIdempotencyKeyReused(k model.IdempotencyKey) error {
	return model.Error{
		Code:    model.CodeIdempotencyKeyReused,
		Message: "this Idempotency-Key is already in use for a different request",
		Details: map[string]any{"job_id": k.JobID, "route": k.Route},
	}
}

// ms renders an instant as the INTEGER Unix milliseconds every timestamp column
// in the schema carries (D40).
func ms(t time.Time) int64 { return t.UnixMilli() }
