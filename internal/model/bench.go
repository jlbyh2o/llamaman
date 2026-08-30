package model

// Benchmarks (DESIGN section 2.10).
//
// Benchmarks are never auto-deleted — they are the product (§2.11 retention).

// BenchRunState is `bench_runs.state` (§2.10).
//
// The §2.3a pairing with the job row, and the one cell that carries a recovery:
//
//	jobs.state          bench_runs.state
//	------------------- ----------------------------------------------------
//	queued              queued
//	leased|running      preflight|running
//	interrupted         `running` with restore_done=0 — THE FINALIZER'S INPUT.
//	                    §10 restores bench-stopped production instances from
//	                    exactly that pairing, which is why a bench_run job is
//	                    never triaged to `failed` at boot: a `failed` row can
//	                    never satisfy the condition, so a bench that stopped two
//	                    serving instances would leave them down
//	succeeded           succeeded|partial
//	failed              failed|partial
//	canceled            canceled
//
// `draft` has no job at all: it is a run being composed in the UI.
type BenchRunState string

const (
	BenchDraft     BenchRunState = "draft"
	BenchQueued    BenchRunState = "queued"
	BenchPreflight BenchRunState = "preflight"
	BenchRunning   BenchRunState = "running"
	BenchSucceeded BenchRunState = "succeeded"
	BenchPartial   BenchRunState = "partial"
	BenchFailed    BenchRunState = "failed"
	BenchCanceled  BenchRunState = "canceled"
)

// BenchRunStateValues lists the members of the `bench_runs.state` CHECK
// constraint, in order.
func BenchRunStateValues() []BenchRunState {
	return []BenchRunState{
		BenchDraft, BenchQueued, BenchPreflight, BenchRunning, BenchSucceeded,
		BenchPartial, BenchFailed, BenchCanceled,
	}
}

// Valid reports whether s is a member of the CHECK constraint.
func (s BenchRunState) Valid() bool { return valid(s, BenchRunStateValues()) }

// BenchPointState is `bench_points.state` (§2.10). Point rows are created BEFORE
// execution, one per cross-product cell, which is what makes progress exact and
// resume exact.
type BenchPointState string

const (
	PointPending   BenchPointState = "pending"
	PointRunning   BenchPointState = "running"
	PointSucceeded BenchPointState = "succeeded"
	PointFailed    BenchPointState = "failed"
	PointSkipped   BenchPointState = "skipped"
)

// BenchPointStateValues lists the members of the `bench_points.state` CHECK
// constraint, in order.
func BenchPointStateValues() []BenchPointState {
	return []BenchPointState{
		PointPending, PointRunning, PointSucceeded, PointFailed, PointSkipped,
	}
}

// Valid reports whether s is a member of the CHECK constraint.
func (s BenchPointState) Valid() bool { return valid(s, BenchPointStateValues()) }

// BenchTestKind is `bench_results.test_kind` (§2.10): llama-bench's own three
// test shapes, kept verbatim so a result row can be traced back to the tool's
// output.
type BenchTestKind string

const (
	TestPP   BenchTestKind = "pp"
	TestTG   BenchTestKind = "tg"
	TestPPTG BenchTestKind = "pp+tg"
)

// BenchTestKindValues lists the members of the `bench_results.test_kind` CHECK
// constraint, in order.
func BenchTestKindValues() []BenchTestKind { return []BenchTestKind{TestPP, TestTG, TestPPTG} }

// Valid reports whether k is a member of the CHECK constraint.
func (k BenchTestKind) Valid() bool { return valid(k, BenchTestKindValues()) }
