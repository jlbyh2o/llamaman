package bench

import (
	"fmt"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// The error codes DESIGN section 3.13's table names, plus the ones a bench job
// row carries.
//
// They are declared here rather than in internal/model for the reason
// internal/llamacpp declares its own where it does: a code belongs to the
// package that decides to return it, and model's catalog is the set of codes
// some COLUMN or schema guard writes. Nothing in section 2 writes any of these;
// every one is a refusal or a failure this subsystem produces.
const (
	// CodeBenchGPUConflict is section 3.13's named 409: `bench.exclusive_gpu` is
	// on, the target GPUs already carry loaded instances, and `on_conflict` was
	// `abort`. `details.instances` names them, including the ones included on a
	// FAIL-CLOSED basis — an instance whose `gpu_attribution` is `declared` or
	// `unknown` is treated as occupying every GPU it could occupy, because
	// without per-GPU identity the guard cannot tell "loaded on the GPU you are
	// about to benchmark" from "loaded on the other one" (section 10, D17).
	CodeBenchGPUConflict model.ErrorCode = "bench_gpu_conflict"

	// CodeBenchNotStartable is `POST /bench/runs/{id}/start` for a run that is
	// not a `draft`. A run that is already queued or running is watched, not
	// started again; a finished one is re-posted as a new run, because its
	// points and results are history.
	CodeBenchNotStartable model.ErrorCode = "bench_not_startable"

	// CodeBenchNotCancelable is `POST /bench/runs/{id}/cancel` for a run with no
	// live job. It is a 409 rather than a 404 for the same reason
	// `build_not_cancelable` is: the run exists, and one that has already
	// finished is a state rather than a missing thing.
	CodeBenchNotCancelable model.ErrorCode = "bench_not_cancelable"

	// CodeBenchRunning refuses a DELETE of a run whose job is live. Deleting the
	// row under the worker would cascade its points away mid-sweep and leave the
	// stop-and-restore finalizer with no `stopped_instances_json` to read —
	// which is precisely how a benchmark leaves production instances down.
	CodeBenchRunning model.ErrorCode = "bench_running"

	// CodeSweepTooLarge is the 422 for a cross-product past MaxPoints, or an
	// axis past MaxAxisValues. The message carries the actual count, because "too
	// large" without a number is not actionable when the number is a product of
	// six lists.
	CodeSweepTooLarge model.ErrorCode = "sweep_too_large"

	// CodeBenchNoRuntime is the 409 for a host with no active llama.cpp build:
	// there is no `bin/llama-bench` to run. It is the same condition
	// `runtime_missing` names for an instance start, under this subsystem's own
	// name because the remediation is the same page but a different button.
	CodeBenchNoRuntime model.ErrorCode = "bench_no_runtime"

	// CodeBenchFailed is the run-level `error_code` for a sweep in which no
	// point produced a result. A sweep where SOME points succeeded ends
	// `partial` and carries no error code at all: partial results are results.
	CodeBenchFailed model.ErrorCode = "bench_failed"

	// CodePointFailed is written to `bench_points.error_message`'s companion job
	// code when llama-bench exits non-zero for one point. Per-point failure
	// isolation is the whole reason section 10 invokes llama-bench once per
	// point, so this never fails the run on its own.
	CodePointFailed model.ErrorCode = "bench_point_failed"
)

// Statuses pairs each code with the HTTP status section 3.13 gives it, so the
// API layer maps them without re-deciding. A code with no entry never reaches
// HTTP — `bench_failed` and `bench_point_failed` are job and row codes — which
// is what makes absence mean "this is not an API answer" rather than "nobody has
// decided yet".
func Statuses() map[model.ErrorCode]int {
	return map[model.ErrorCode]int{
		CodeBenchGPUConflict:   409,
		CodeBenchNotStartable:  409,
		CodeBenchNotCancelable: 409,
		CodeBenchRunning:       409,
		CodeBenchNoRuntime:     409,
		CodeSweepTooLarge:      422,
	}
}

// errorf builds a model.Error, which internal/api renders into section 3's
// envelope with no translation.
func errorf(code model.ErrorCode, format string, args ...any) model.Error {
	return model.Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// withDetails attaches the `details` object section 3's envelope carries.
func withDetails(e model.Error, d map[string]any) model.Error {
	e.Details = d
	return e
}
