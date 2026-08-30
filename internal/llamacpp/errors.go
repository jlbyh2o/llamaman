package llamacpp

import (
	"fmt"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// The error codes DESIGN section 3.5's table names, and the two the job rows
// carry.
//
// They are declared here rather than in internal/model for the reason
// internal/api and internal/api/middleware declare theirs where they do: a code
// belongs to the package that decides to return it, and model's own catalog is
// the set of codes some COLUMN or schema guard writes (§2). Nothing in §2 writes
// any of these; every one of them is a refusal this service makes.
const (
	// CodeBuildInFlight is `POST /llamacpp/versions` for an id whose row is
	// live — `pending` through `verifying` (D71). It names the existing job so
	// the UI can jump to it. An Idempotency-Key replay inside its window
	// returns that job with 200 instead, which is the job queue's doing rather
	// than this package's.
	CodeBuildInFlight model.ErrorCode = "build_in_flight"

	// CodeVersionOptionsDiffer is D71's fourth branch: the id is installed and
	// `ready`, but the request asks for build options the installed tree was
	// not built with. `details.differences` names each one with both values,
	// and `force_rebuild: true` is the documented override.
	CodeVersionOptionsDiffer model.ErrorCode = "version_options_differ"

	// CodeVersionActive refuses `DELETE /llamacpp/versions/{id}` for the
	// `is_active=1` row: deleting the build every instance starts from is not a
	// state this system has a recovery for, and there is nothing to gain by
	// allowing it — activate something else first.
	CodeVersionActive model.ErrorCode = "version_active"

	// CodeVersionIsRollbackTarget refuses the same delete for the
	// `previous_active=1` row. Turning `llamacpp.keep_previous` off releases it,
	// which is the documented way to get rid of it.
	CodeVersionIsRollbackTarget model.ErrorCode = "version_is_rollback_target"

	// CodeVersionInUse is D25's refusal, and the only one of these that is
	// answered from the FILESYSTEM rather than from a row: a live process is
	// executing out of this version's directory. It refuses a delete and a
	// forced rebuild alike, because renaming a tree out from under a running
	// `llama-server` is the one thing neither operation may do.
	CodeVersionInUse model.ErrorCode = "version_in_use"

	// CodeNoRollbackTarget is `POST /llamacpp/rollback` with no
	// `previous_active=1` row — either nothing has been replaced yet, or
	// `llamacpp.keep_previous` is off and the version list says "rollback
	// disabled" (§6.6 step 2).
	CodeNoRollbackTarget model.ErrorCode = "no_rollback_target"

	// CodeVersionNotReady refuses an activation of a row that is not `ready`:
	// a build still compiling, or one that failed. It is a 409 rather than a
	// 404 because the version exists and will very likely become activatable.
	CodeVersionNotReady model.ErrorCode = "version_not_ready"

	// CodeActivationInFlight is §6.6 step 1's first guard: one activation at a
	// time. `idx_jobs_one_live_per_subject` cannot express it — two activations
	// of two different versions are two different subjects — so it is a
	// counted read inside the same transaction that enqueues, exactly as D97
	// requires of every guard whose index cannot hold it.
	CodeActivationInFlight model.ErrorCode = "activation_in_flight"

	// CodeBenchInFlight is that guard's second term: a bench is executing, or
	// one has stopped production instances and not yet put them back (D75). An
	// activation restarts the fleet, which would either corrupt the bench's
	// numbers or race its restore.
	CodeBenchInFlight model.ErrorCode = "bench_in_flight"

	// CodeActivationNotCancelable is §2.3a's activate column read as a refusal:
	// a cancel is accepted only BEFORE the step-3 transaction commits. Once
	// `is_active` has moved, there is no cancel — there is a rollback, which is
	// a different operation with its own canary.
	CodeActivationNotCancelable model.ErrorCode = "activation_not_cancelable"

	// CodeCanaryFailed is written to the activation job's `error_code` when
	// §6.6 step 5's revert runs. It never reaches an HTTP response — the
	// activation was a 202 long before — and it is the code the notification
	// and the job row are read by.
	CodeCanaryFailed model.ErrorCode = "canary_failed"

	// CodeBuildFailed is the install worker's fallback `error_code` for a
	// failure no build phase named. A phase that has a name writes its own into
	// `llamacpp_versions.failing_step` beside this.
	CodeBuildFailed model.ErrorCode = "build_failed"

	// CodeResolveFailed is the `resolving` state's failure: the channel, tag,
	// asset or git ref could not be resolved at all, so there is nothing to
	// fetch or build.
	CodeResolveFailed model.ErrorCode = "resolve_failed"

	// CodeVerificationFailed is D18/D19's rejection: the binaries installed but
	// would not execute on this host, or a CUDA build listed no CUDA device.
	CodeVerificationFailed model.ErrorCode = "verification_failed"

	// CodeBuildNotCancelable is `POST …/{id}/cancel` for a version with no live
	// job. It is a 409 rather than a 404 for the same reason
	// `version_not_ready` is: the version exists, and a build that has already
	// finished is a state rather than a missing thing.
	CodeBuildNotCancelable model.ErrorCode = "build_not_cancelable"

	// CodeBuildNotRetryable is `POST …/{id}/retry` for a version whose last
	// install is not in one of the three states a job stops in without being
	// finished with (`failed`, `canceled`, `interrupted`). A build that is
	// running is not retried, it is watched; one that succeeded is re-posted
	// with `force_rebuild`.
	CodeBuildNotRetryable model.ErrorCode = "build_not_retryable"
)

// Statuses pairs each code with the HTTP status DESIGN section 3.5 gives it, so
// the API layer maps them without re-deciding. The map is here rather than in
// internal/api for the same reason the codes are: the package that chooses the
// code knows what it meant by it.
//
// Every entry is 409 except the two that are not refusals of a conflicting
// state: `version_options_differ` is also a conflict (the row exists and
// disagrees), and the three job-only codes have no status at all and are
// absent, which is what makes "a code with no entry" mean "this never reaches
// HTTP" rather than "nobody has decided yet".
func Statuses() map[model.ErrorCode]int {
	return map[model.ErrorCode]int{
		CodeBuildInFlight:           409,
		CodeVersionOptionsDiffer:    409,
		CodeVersionActive:           409,
		CodeVersionIsRollbackTarget: 409,
		CodeVersionInUse:            409,
		CodeNoRollbackTarget:        409,
		CodeVersionNotReady:         409,
		CodeActivationInFlight:      409,
		CodeBenchInFlight:           409,
		CodeActivationNotCancelable: 409,
		CodeBuildNotCancelable:      409,
		CodeBuildNotRetryable:       409,
	}
}

// errorf builds a model.Error, which internal/api renders into the envelope
// with no translation.
func errorf(code model.ErrorCode, format string, args ...any) model.Error {
	return model.Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// withDetails attaches the `details` object §3's envelope carries.
func withDetails(e model.Error, d map[string]any) model.Error {
	e.Details = d
	return e
}
