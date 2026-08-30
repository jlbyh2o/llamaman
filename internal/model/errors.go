package model

import "errors"

// ErrorCode is the closed enum of machine-readable API error codes (DESIGN
// section 3). Codes are mirrored into TypeScript by the generated OpenAPI
// schema, so a code that is not listed here cannot appear on the wire.
type ErrorCode string

// The codes DESIGN section 2 names. Each one is a value some column or some
// guard in the schema section writes or answers with, so they belong beside the
// enums those columns are closed by; the rest of section 3's catalog arrives
// with the endpoints that return it.
const (
	// CodeDaemonRestarted is written to `jobs.error_code` by boot triage for the
	// kinds §2.3 sends straight to `failed`, and by §11.1 step 11's closing pass
	// for a `self_updates` row no marker names (§2.3, §12.3).
	CodeDaemonRestarted ErrorCode = "daemon_restarted"
	// CodeUpdateNotApplied closes a self-update whose marker names a version this
	// binary is not, with no actor still running: the update did not take, whether
	// because the swap never happened or because the judge reverted it (§11.1 step 11).
	CodeUpdateNotApplied ErrorCode = "update_not_applied"
	// CodeDeleteIncomplete is §2.5's edge out of `deleting` when the removal failed
	// and the directory is incomplete; it pairs with failing_step='delete'.
	CodeDeleteIncomplete ErrorCode = "delete_incomplete"

	// CodeIdempotencyKeyReused is the 422 for an Idempotency-Key hit whose
	// request fingerprint differs — two different requests under one key, which
	// is a client bug rather than a replay (D65, §2.3).
	CodeIdempotencyKeyReused ErrorCode = "idempotency_key_reused"
	// CodeJobInFlight is the 409 a job-creating request gets while another job
	// holds the same subject, or while a build or self-update is live (§3.14).
	CodeJobInFlight ErrorCode = "job_in_flight"
	// CodeDownloadExists is the 409 a repeat POST /downloads gets, naming the
	// existing download rather than creating a second live job for it (§2.7).
	CodeDownloadExists ErrorCode = "download_exists"
	// CodeSelfUpdateNotCancelable is the 409 for a cancel at or after the
	// `staged` commit: the marker is on disk and the swap belongs to systemd
	// (D96, §2.3a).
	CodeSelfUpdateNotCancelable ErrorCode = "selfupdate_not_cancelable"

	// CodePortUnavailable is §2.8's 422 for a port that breaks one of the six
	// port rules; the reason travels in details as a PortReason.
	CodePortUnavailable ErrorCode = "port_unavailable"
	// CodeSettingInvalid is the 400 PATCH /settings answers when a value fails
	// its validator — including a `ui.port_desired` that collides with an
	// existing public_port (§2.8).
	CodeSettingInvalid ErrorCode = "setting_invalid"

	// The launcher error codes recorded in `instance_starts.error_code` for a
	// run that never reached execve (§2.8, §5.6).
	CodeModelMissing ErrorCode = "model_missing"
	CodePortConflict ErrorCode = "port_conflict"
	CodeBadFlags     ErrorCode = "bad_flags"
)

// PortReason is the `reason` carried in the details of a CodePortUnavailable
// response — the six port rules of §2.8, which are validated at save time rather
// than discovered at bind time.
type PortReason string

const (
	// PortReservedManagement: equal to ui.port_desired, or to the port the
	// management walk actually landed on (runtime_info.ui_port). Public ports.
	PortReservedManagement PortReason = "reserved_management"
	// PortReservedInternalPool: a public port inside
	// [instances.internal_port_min, instances.internal_port_max].
	PortReservedInternalPool PortReason = "reserved_internal_pool"
	// PortOutsideInternalPool: an internal port outside that same pool.
	PortOutsideInternalPool PortReason = "outside_internal_pool"
	// PortInUseByInstance: held by another instance in either column.
	PortInUseByInstance PortReason = "in_use_by_instance"
	// PortBindFailed: the advisory live bind probe failed. It is advisory because
	// another process can take the port between the probe and the listen, which
	// is exactly why F6 still exists as a runtime fallback.
	PortBindFailed PortReason = "bind_failed"
)

// PortReasonValues lists the reasons of §2.8's port-rule table, in order.
func PortReasonValues() []PortReason {
	return []PortReason{
		PortReservedManagement, PortReservedInternalPool, PortOutsideInternalPool,
		PortInUseByInstance, PortBindFailed,
	}
}

// Valid reports whether r is one of the reasons §2.8's table names.
func (r PortReason) Valid() bool { return valid(r, PortReasonValues()) }

// ErrIllegalTransition is returned by every transition function in this package
// when the move is not in that aggregate's table. There is no generic
// state-machine engine (D42) — each table is its own code — so this sentinel is
// what makes them uniformly testable.
var ErrIllegalTransition = errors.New("illegal state transition")

// Error is the body of every non-2xx API response:
//
//	{"error":{"code":"model_in_use","message":"…","details":{…}}}
//
// The HTTP status is chosen by the handler to match the code.
type Error struct {
	Code    ErrorCode      `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// ErrorEnvelope is the top-level wrapper the API marshals.
type ErrorEnvelope struct {
	Error Error `json:"error"`
}

// Error implements the error interface so a model.Error can be returned from
// service code and rendered by the API layer without translation.
func (e Error) Error() string { return string(e.Code) + ": " + e.Message }
