package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// DomainFunc is the domain-row write that must commit in the SAME transaction as
// the new job row (§2.3a). `j` is the row as it was just inserted, so the domain
// write can record the job id it is paired with.
//
// It is not optional decoration: every kind but `maintenance` has a domain row,
// and §2.3a's guarantee is exactly that one transaction writes both. A caller
// that already holds a transaction — the downloads service writing `downloads`,
// `download_tasks` and `models` beside the job (§2.7) — uses EnqueueTx and does
// its own writes there instead.
type DomainFunc func(ctx context.Context, tx store.Tx, j model.Job) error

// Idempotency carries the D65 replay window's three inputs. The middleware of
// §3 fills it from the `Idempotency-Key` header, the matched route pattern and
// the sha256 of the canonicalized request body.
type Idempotency struct {
	// Key is the client's `Idempotency-Key` header, and the primary key of
	// `idempotency_keys`.
	Key string
	// Route is method + pattern. The same key on a different route is a 422, not
	// a replay, because the client cannot have meant the two as one request.
	Route string
	// RequestFingerprint is the sha256 of the canonicalized request body. A hit
	// whose fingerprint differs is `422 idempotency_key_reused`: the client sent
	// two different requests under one key, which is a bug rather than a replay.
	RequestFingerprint string
}

// EnqueueParams describes one job to create.
type EnqueueParams struct {
	// Kind is the `jobs.kind`, and with DomainID it fixes the (subject_type,
	// subject_id) pair through model.SubjectFor — the closed mapping of §2.3a.
	Kind model.JobKind
	// DomainID is the domain row's id. It is ignored for `toolchain_probe` and
	// `maintenance`, whose subject ids are the fixed synthetic constants that make
	// the one-live-job-per-subject index bind them (§2.3a).
	DomainID string

	// Priority is `jobs.priority`; lower runs first. Zero means DefaultPriority,
	// which is the column's own default.
	Priority int
	// MaxAttempts is the retry budget of §2.3. Zero means 1, the column's default:
	// one attempt and no retry.
	MaxAttempts int
	// RunAfter is the earliest instant the job may be leased. The zero time means
	// now, which is what an ordinary enqueue wants.
	RunAfter time.Time

	// Params is marshaled into `params_json`, the column a worker reads its inputs
	// back out of after a restart. Nil writes SQL NULL.
	Params any

	// Idempotency, when set, applies the D65 ten-minute replay window.
	Idempotency *Idempotency

	// Domain is the domain-row write that commits with the job row (§2.3a).
	Domain DomainFunc
}

// EnqueueResult is a created — or replayed — job.
type EnqueueResult struct {
	// Job is the row, newly inserted or, on a replay, the original.
	Job model.Job
	// Replayed reports the D65 hit: an `Idempotency-Key` seen inside its window
	// with the same route and fingerprint. §3's handlers answer `200` with the
	// same body rather than the `202` a fresh job gets, which is what makes a
	// double-clicked Build a replay instead of a 409.
	Replayed bool
}

// Enqueue creates a job and its domain row in one transaction, and wakes the
// runners.
//
// Two refusals are part of its contract. `409 job_in_flight` (model.Error with
// model.CodeJobInFlight) names the live job already holding this subject —
// `idx_jobs_one_live_per_subject` is what makes that impossible, and the read
// only supplies the id for the message. `422 idempotency_key_reused` is the D65
// fingerprint mismatch.
func (q *Queue) Enqueue(ctx context.Context, p EnqueueParams) (EnqueueResult, error) {
	var res EnqueueResult
	err := q.s.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		res, err = q.EnqueueTx(ctx, tx, p)
		return err
	})
	if err != nil {
		return EnqueueResult{}, err
	}
	if !res.Replayed {
		q.Wake()
	}
	return res, nil
}

// EnqueueTx is Enqueue inside a transaction the caller already holds, for the
// handlers that write several domain rows beside the job — §2.7's download,
// which commits `downloads`, `download_tasks`, `models` and the job together, so
// that "the job IS the receipt".
//
// The caller is responsible for calling Wake after its transaction commits.
func (q *Queue) EnqueueTx(ctx context.Context, tx store.Tx, p EnqueueParams) (EnqueueResult, error) {
	subjectType, subjectID := model.SubjectFor(p.Kind, p.DomainID)
	if subjectType == "" {
		return EnqueueResult{}, fmt.Errorf("jobs: %w: %q", errUnknownKind, p.Kind)
	}
	if subjectID == "" {
		return EnqueueResult{}, fmt.Errorf("jobs: a %s job needs a %s id", p.Kind, subjectType)
	}

	now := q.now()

	// The D65 lookup runs first and inside the caller's transaction, so a
	// concurrent double-submit either replays or collides on the primary key —
	// never creates a second job.
	if p.Idempotency != nil {
		if p.Idempotency.Key == "" {
			return EnqueueResult{}, errors.New("jobs: Idempotency.Key is empty")
		}
		hit, err := q.s.LiveIdempotencyKey(ctx, tx, p.Idempotency.Key, ms(now))
		switch {
		case err == nil:
			if hit.Route != p.Idempotency.Route ||
				hit.RequestFingerprint != p.Idempotency.RequestFingerprint {
				return EnqueueResult{}, errIdempotencyKeyReused(hit)
			}
			j, err := q.s.Job(ctx, tx, hit.JobID)
			if err != nil {
				return EnqueueResult{}, fmt.Errorf("jobs: replay of key %q: %w", hit.Key, err)
			}
			return EnqueueResult{Job: j, Replayed: true}, nil
		case errors.Is(err, store.ErrNotFound):
			// A miss. InsertIdempotencyKey sweeps the expired row, if any.
		default:
			return EnqueueResult{}, err
		}
	}

	paramsJSON, err := marshalJSON(p.Params)
	if err != nil {
		return EnqueueResult{}, fmt.Errorf("jobs: marshal params for a %s job: %w", p.Kind, err)
	}

	runAfter := p.RunAfter
	if runAfter.IsZero() {
		runAfter = now
	}
	priority := p.Priority
	if priority == 0 {
		priority = DefaultPriority
	}
	maxAttempts := p.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 1
	}

	j := model.Job{
		ID:          store.NewID(now),
		Kind:        p.Kind,
		SubjectType: subjectType,
		SubjectID:   subjectID,
		State:       model.JobQueued,
		Priority:    priority,
		RunAfter:    ms(runAfter),
		MaxAttempts: maxAttempts,
		ParamsJSON:  paramsJSON,
		CreatedAt:   ms(now),
	}
	if p.Idempotency != nil {
		j.IdempotencyKey = &p.Idempotency.Key
	}

	if err := q.s.InsertJob(ctx, tx, j); err != nil {
		// The unique index is the guarantee; this read only lets the refusal NAME
		// the row it collided with. A constraint failure rolls back the statement
		// and not the transaction, so the SELECT below is legal here.
		if existing, lookupErr := q.s.LiveJobForSubject(ctx, tx, subjectType, subjectID); lookupErr == nil {
			return EnqueueResult{}, errJobInFlight(existing)
		}
		return EnqueueResult{}, err
	}

	if p.Idempotency != nil {
		// job_id REFERENCES jobs(id), so this follows the insert above.
		k := model.IdempotencyKey{
			Key:                p.Idempotency.Key,
			Route:              p.Idempotency.Route,
			RequestFingerprint: p.Idempotency.RequestFingerprint,
			JobID:              j.ID,
			CreatedAt:          ms(now),
			ExpiresAt:          ms(now.Add(model.IdempotencyWindow)),
		}
		if err := q.s.InsertIdempotencyKey(ctx, tx, k, ms(now)); err != nil {
			return EnqueueResult{}, err
		}
	}

	if p.Domain != nil {
		if err := p.Domain(ctx, tx, j); err != nil {
			return EnqueueResult{}, err
		}
	}
	return EnqueueResult{Job: j}, nil
}

// LiveCountByKind counts live jobs of one kind — a build or any `self_update`
// job, `interrupted` included — in its own read-only snapshot transaction.
//
// It is NOT the guard of §3.14, and must not be used as one. That guard —
// `409 job_in_flight` on `POST /update/apply` — is one of exactly four clauses
// D97 requires to be evaluated INSIDE the single `BEGIN IMMEDIATE` transaction
// that inserts the `self_updates` row and its job, because `jobs.subject_id` for
// a self-update is a fresh id and `idx_jobs_one_live_per_subject` therefore
// cannot express "one at a time". Read-then-write against this method is exactly
// the failure D97 exists to prevent: two concurrent applies both see 0, both
// insert, and step 1 of the second empties `update/` while the first is still
// downloading into it. The transactional form is
// store.CountLiveJobsByKind, which takes a Tx and composes with EnqueueTx inside
// the caller's write transaction.
//
// What this method is for is the reads that are not guards: a status endpoint or
// a UI hint reporting whether a build is currently live.
func (q *Queue) LiveCountByKind(ctx context.Context, kind model.JobKind) (int, error) {
	var n int
	err := q.s.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		n, err = q.s.CountLiveJobsByKind(ctx, tx, kind)
		return err
	})
	return n, err
}

// marshalJSON renders a value for a nullable json_valid column: nil stays NULL
// rather than becoming the four bytes "null", which would read back as a value.
func marshalJSON(v any) (*string, error) {
	if v == nil {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	s := string(b)
	return &s, nil
}
