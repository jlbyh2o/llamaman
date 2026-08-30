// Package jobs is the durable job queue. It handles enqueue, lease, heartbeat,
// progress reporting, cancellation and retry, recovers orphaned jobs whose
// worker died on a previous boot, and keeps the registry that maps a job kind to
// the Worker that runs it (DESIGN section 1).
//
// The shape of the package follows the two rules DESIGN sections 2.3 and 2.3a
// state about jobs, and almost everything here exists to serve one of them.
//
// # One job row per domain row, written together (D55)
//
// `jobs` is the SCHEDULING record — lease, retry, backoff, cancellation,
// progress — and the domain row it names through (SubjectType, SubjectID) is the
// DOMAIN record, which is the state the UI reads and the API returns. There is
// exactly one live `jobs` row per domain row and the same transaction writes
// both, which is what keeps two state machines over one activity from drifting.
//
// So no method here moves a job row without giving its Worker the same
// transaction to move the domain row in: EnqueueParams.Domain at enqueue,
// Outcome.Commit at the close, and DomainWriter for the three transitions the
// QUEUE performs on a worker's behalf — boot triage, a cancel with no worker
// running, and a retry.
//
// # The subject is held by an index, not by a convention (D39)
//
// `idx_jobs_one_live_per_subject` makes a second live job on one subject
// impossible, and `paused` and `interrupted` both count as live. This package
// therefore never checks-then-inserts as its guarantee; it reads the colliding
// row only so the refusal can NAME it (`409 job_in_flight`), and lets the index
// be the guarantee. On top of that, an Idempotency-Key replay inside the D65
// ten-minute window returns the ORIGINAL job rather than a 409, which is what
// makes a double-clicked Build a replay instead of an error.
package jobs
