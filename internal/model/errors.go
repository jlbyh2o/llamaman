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

	// CodeBadCredentials is the 401 POST /auth/login answers a wrong password
	// with (§3.1). It is deliberately the same code for "no account exists" and
	// "wrong password": telling the two apart would make the endpoint an oracle
	// for whether a host has been claimed, which `GET /api/v1/meta` answers
	// honestly to anyone who is entitled to ask.
	CodeBadCredentials ErrorCode = "bad_credentials"
	// CodeLockedOut is the 429 of §3.1, carrying `retry_after_sec` in details:
	// this address has exhausted its login attempts and is blocked until then
	// (SPEC §4). It is the ONE 429 in this API that is not §3.3's restart rate
	// limit, and §3.1's own table names it.
	CodeLockedOut ErrorCode = "locked_out"
	// CodePasswordInvalid is the 400 for a password that fails the strength
	// rule the wizard's meter shows (§11.2) — too short, or absurdly long.
	CodePasswordInvalid ErrorCode = "password_invalid"

	// CodeWizardStepUnknown is the 400 for a `step` that is not one of §11.2's
	// seven.
	CodeWizardStepUnknown ErrorCode = "wizard_step_unknown"
	// CodeWizardStepLocked is the 409 for a wizard move the server refuses:
	// skipping a step §11.2 marks non-skippable, or entering one whose
	// prerequisites are unfinished. The gate is server-side because the wizard
	// is resumable and a client's idea of where it is cannot be trusted.
	CodeWizardStepLocked ErrorCode = "wizard_step_locked"

	// CodePortUnavailable is §2.8's 422 for a port that breaks one of the six
	// port rules; the reason travels in details as a PortReason.
	CodePortUnavailable ErrorCode = "port_unavailable"
	// CodeSettingInvalid is the 400 PATCH /settings answers when a value fails
	// its validator — including a `ui.port_desired` that collides with an
	// existing public_port (§2.8).
	CodeSettingInvalid ErrorCode = "setting_invalid"

	// The launcher error codes recorded in `instance_starts.error_code` for a
	// run that never reached execve (§2.8, §5.6).
	//
	// CodeModelMissing and CodeBadFlags are also the save-time answers of §3.10:
	// a `model_id` naming no row, and a `flags_json` whose value fails
	// FlagSet.Validate, are the same two conditions the launcher exits 72 and
	// 65 for, caught one step earlier. One name per condition beats two.
	CodeModelMissing ErrorCode = "model_missing"
	CodePortConflict ErrorCode = "port_conflict"
	CodeBadFlags     ErrorCode = "bad_flags"

	// CodeConflictGeneration is §3's optimistic-concurrency 409: a PATCH on an
	// instance or a preset whose `generation` is not the current one. None of
	// the seven exceptional writers of §2.8 bumps that column, so this code
	// always means a human edited the configuration under this form.
	CodeConflictGeneration ErrorCode = "conflict_generation"
	// CodeDraftVocabMismatch is D34's 422: both models are parsed and their
	// `tokenizer_model`/`n_vocab` differ, so speculative decoding would emit
	// garbage. Both values travel in details (§3.10a).
	CodeDraftVocabMismatch ErrorCode = "draft_vocab_mismatch"
	// CodeNGLAutoConflict is §5.7's 422 for `n_gpu_layers.mode = "auto"` saved
	// together with an explicit `tensor_split`: upstream disables `--fit` when
	// either -ngl or --tensor-split is pinned, so `auto` would mean nothing.
	CodeNGLAutoConflict ErrorCode = "ngl_auto_conflict"
	// CodeExtraFlagForbidden is §5.7's 422 for an `extra_flags` string that
	// overrides something the renderer owns — `--host`, `--port`, `-m`,
	// `--model` or `--api-key`. The escape hatch may add flags; it may not
	// contradict the ones that make the instance reachable.
	CodeExtraFlagForbidden ErrorCode = "extra_flag_forbidden"
	// CodeInstanceNameInvalid is the 422 for a name that fails D11's grammar
	// `^[a-z0-9][a-z0-9-]{0,31}$`. The same string becomes a systemd unit
	// instance id and is matched by the polkit regex, which is why the rule is
	// enforced in three places and why a violation is an input error rather
	// than something to normalize.
	CodeInstanceNameInvalid ErrorCode = "instance_name_invalid"
	// CodeInstanceNameTaken is the 409 for a name another NON-DELETED instance
	// holds. Soft deletion scopes the unique index to live rows (D68), so a
	// deleted instance's name is free and this code never fires for one.
	CodeInstanceNameTaken ErrorCode = "instance_name_taken"

	// CodeModelInUse is the 409 of §3.7 and §7.2a, and the two paths that
	// return it count DIFFERENT rows on purpose.
	//
	// Deleting a MODEL counts non-deleted instances only: that path never
	// issues a SQL DELETE — the row moves `deleting → deleted` and stays — so
	// `instances.model_id`'s ON DELETE RESTRICT is never exercised and a
	// soft-deleted instance keeps a readable record of what it pointed at (D68).
	// Detaching a cache ROOT counts every referencing row including the
	// soft-deleted ones, because that path DOES issue one, `models` cascades
	// away, and RESTRICT does not care that a row is soft-deleted. Details
	// carry the instances and which of them are deleted.
	CodeModelInUse ErrorCode = "model_in_use"
	// CodeRootIsPrimary is the 409 for detaching the primary cache root
	// (§3.7). Exactly one primary always exists — it is the only root Llama Man
	// writes to — so detaching it is refused rather than left to leave the host
	// with nowhere to download.
	CodeRootIsPrimary ErrorCode = "root_is_primary"
	// CodeRootNotWritable is the 422 for promoting a `writable=0` root
	// (§7.2a). Such a root is read, scanned and served forever; it simply can
	// never be the one downloads land in.
	CodeRootNotWritable ErrorCode = "root_not_writable"
	// CodeRootPathProtected is the 422 for a cache root under a prefix the unit
	// mounts read-only through `ProtectSystem=full` (§3.7, D57). The daemon
	// could not write there whatever the file mode says, and registration is
	// the honest moment to say so rather than the first download.
	CodeRootPathProtected ErrorCode = "root_path_protected"

	// The degraded-mode refusals of §11.1a. Every one of them names a thing
	// this host cannot do rather than a thing the request got wrong, and each
	// carries the exact manual command in `details.hints` — which is the whole
	// point: F9 and F10 are supported modes this daemon serves in, so the API's
	// job is to say so precisely enough that the user can finish the job by
	// hand.

	// CodeSystemdUnavailable is the 409 every instance control answers in the
	// F10 mode: no service manager is reachable, so there is nothing to ask.
	CodeSystemdUnavailable ErrorCode = "systemd_unavailable"
	// CodeSystemdDenied is the 409 for a call the name-scoped `manage-units`
	// polkit grant was refused for (F9, §5.2 branch (b)).
	CodeSystemdDenied ErrorCode = "systemd_denied"
	// CodeAutostartUnavailable is §3.10's 409 on `PUT /instances/{id}/autostart`
	// when the `manage-unit-files` grant was withheld — a narrower refusal than
	// CodeSystemdDenied, because starting and stopping still work.
	CodeAutostartUnavailable ErrorCode = "autostart_unavailable"
	// CodeRestartUnavailable is §3.3's 409 on `POST /system/restart` when
	// `systemd_control='unavailable'`; the response carries
	// `sudo systemctl restart llamaman.service`.
	CodeRestartUnavailable ErrorCode = "restart_unavailable"
	// CodeRestartRateLimited is D93's 429 — the ONE meaning §3 gives 429 outside
	// the login lockout — while this boot has not yet cleared its unit's
	// start-limit counter.
	CodeRestartRateLimited ErrorCode = "restart_rate_limited"
	// CodeJournalUnavailable is D77's 409 on `GET /system/journal` when
	// `runtime_info.journal_read != 'ok'`: an empty stream and a denied one must
	// not look alike.
	CodeJournalUnavailable ErrorCode = "journal_unavailable"
)

// WarningCode is the machine-readable code of an entry in a response's
// `warnings` array. Warnings are not errors: the request succeeded and the row
// was written, and the client is being told something it should show rather
// than something it must fix. §3.10a's deferred draft validation is the
// original: a save that succeeds with `201` while carrying the note that a
// check is owed.
type WarningCode string

const (
	// WarnDraftVocabUnverified is §3.10a's third row: one side of the draft
	// pairing has no GGUF metadata yet, so the check is deferred rather than
	// performed or refused.
	WarnDraftVocabUnverified WarningCode = "draft_vocab_unverified"
	// WarnUnknownFlags is §5.7's flag-churn guard: the rendered argv contains a
	// flag the active build's `--help` does not advertise. It is a WARNING by
	// design — llama.cpp ships ~10 nightlies a day and a hard failure would
	// make the tool brittle by design.
	WarnUnknownFlags WarningCode = "unknown_flags"
	// WarnFlagCheckUnavailable is what the guard says on a build with no help
	// capture (`help_flags_json IS NULL`): the check could not run, which is
	// not the same as "every flag is unknown".
	WarnFlagCheckUnavailable WarningCode = "flag_check_unavailable"
	// WarnNGLAutoWithoutFit is §5.7's second `-ngl auto` rule: this build
	// predates `--fit`, so `auto` renders as `-ngl 999` and behaves as `all`.
	WarnNGLAutoWithoutFit WarningCode = "ngl_auto_without_fit"
)

// Warning is one entry of a response's `warnings` array.
type Warning struct {
	Code    WarningCode    `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

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
