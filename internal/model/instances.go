package model

// Instances (DESIGN section 2.8).
//
// Instance state has TWO AXES kept deliberately separate: DESIRED
// (`instances.desired_state`, set by the API and — once per host boot — by the
// D53 autostart coupling) and ACTUAL (`instance_status.state`, set only by the
// supervisor). Neither is derivable from the other, and the four derived flags
// below are computed on read from both plus the start ledger.

// AuthMode is `instances.auth_mode` and `instance_usage_daily.auth_mode` (§2.8,
// §2.9). Gateway traffic is counted for EVERY proxied request including
// auth_mode='none' (D56), which is why the accounting table carries the mode as
// part of its primary key rather than assuming a credential existed.
type AuthMode string

const (
	AuthToken AuthMode = "token"
	AuthNone  AuthMode = "none"
)

// AuthModeValues lists the members of the `instances.auth_mode` CHECK
// constraint, in order.
func AuthModeValues() []AuthMode { return []AuthMode{AuthToken, AuthNone} }

// Valid reports whether m is a member of the CHECK constraint.
func (m AuthMode) Valid() bool { return valid(m, AuthModeValues()) }

// RestartPolicy is `instances.restart_policy` (§2.8). `on-failure` carries the
// promise "a clean exit is not a failure, so we do not restart it, and we tell
// you why" — which is what the `clean_exit` inhibit reason makes visible.
type RestartPolicy string

const (
	RestartAlways    RestartPolicy = "always"
	RestartOnFailure RestartPolicy = "on-failure"
	RestartNever     RestartPolicy = "never"
)

// RestartPolicyValues lists the members of the `instances.restart_policy` CHECK
// constraint, in order.
func RestartPolicyValues() []RestartPolicy {
	return []RestartPolicy{RestartAlways, RestartOnFailure, RestartNever}
}

// Valid reports whether p is a member of the CHECK constraint.
func (p RestartPolicy) Valid() bool { return valid(p, RestartPolicyValues()) }

// DesiredState is `instances.desired_state` (§2.8): a statement about NOW.
// `instances.autostart` is the other axis — a statement about HOST BOOTS — and
// the two are joined at exactly one point, the first supervisor pass after a
// host boot (D53, §5.8 boot reconciliation step 1).
type DesiredState string

const (
	DesiredRunning DesiredState = "running"
	DesiredStopped DesiredState = "stopped"
)

// DesiredStateValues lists the members of the `instances.desired_state` CHECK
// constraint, in order.
func DesiredStateValues() []DesiredState { return []DesiredState{DesiredRunning, DesiredStopped} }

// Valid reports whether s is a member of the CHECK constraint.
func (s DesiredState) Valid() bool { return valid(s, DesiredStateValues()) }

// DraftValidation is `instances.draft_validation` (§2.8, D34, §3.10a):
// `deferred` means the draft model's GGUF metadata did not exist yet when the
// instance was saved, so the check is owed and the models service re-runs it
// when parsing finishes.
type DraftValidation string

const (
	DraftOK       DraftValidation = "ok"
	DraftDeferred DraftValidation = "deferred"
	DraftMismatch DraftValidation = "mismatch"
)

// DraftValidationValues lists the members of the `instances.draft_validation`
// CHECK constraint, in order.
func DraftValidationValues() []DraftValidation {
	return []DraftValidation{DraftOK, DraftDeferred, DraftMismatch}
}

// Valid reports whether v is a member of the CHECK constraint.
func (v DraftValidation) Valid() bool { return valid(v, DraftValidationValues()) }

// PendingTrigger is `instances.pending_trigger` (§2.8): a HAND-OFF CHANNEL, not
// configuration. The daemon stamps it before StartUnit and `instance-exec`
// consumes and clears it in its own transaction (§5.6 step 3), which is how the
// launcher learns why it was started.
//
// It is `instance_starts.trigger` minus 'external', deliberately: an external
// start is one the daemon did not stamp — a `systemctl start` by hand, or a boot
// start of an enabled unit — so it can never be a pending value, only an
// observed one.
type PendingTrigger string

const (
	TriggerUser              PendingTrigger = "user"
	TriggerAutostart         PendingTrigger = "autostart"
	TriggerSupervisorRestart PendingTrigger = "supervisor_restart"
	TriggerRolling           PendingTrigger = "rolling"
	TriggerBenchRestore      PendingTrigger = "bench_restore"
	TriggerSafeStart         PendingTrigger = "safe_start"
)

// PendingTriggerValues lists the members of the `instances.pending_trigger`
// CHECK constraint, in order.
func PendingTriggerValues() []PendingTrigger {
	return []PendingTrigger{
		TriggerUser, TriggerAutostart, TriggerSupervisorRestart, TriggerRolling,
		TriggerBenchRestore, TriggerSafeStart,
	}
}

// Valid reports whether t is a member of the CHECK constraint.
func (t PendingTrigger) Valid() bool { return valid(t, PendingTriggerValues()) }

// InstanceState is `instance_status.state` (§2.8) — the ACTUAL state, written
// only by the supervisor with three named exceptions (§2.8's writer table).
//
// Transitions:
//
//	from                    trigger                                          to
//	----------------------- ------------------------------------------------ --------------------
//	stopped|failed|unknown  supervisor starts the unit, or an EXTERNAL start  starting
//	                        is observed
//	starting                unit active, /health 503 "loading model"          loading
//	starting|loading        /health 200                                       ready
//	starting|loading        start_timeout_sec elapsed, or unit failed         failed
//	ready                   3 consecutive health failures, unit still active  degraded
//	degraded                /health 200                                       ready
//	ready|degraded|loading  stop requested                                    stopping → stopped
//	ready|degraded          the unit goes inactive or failed with NO stop     failed
//	                        requested — llama-server exited on its own,
//	                        cleanly or not
//	stopping                unit inactive                                     stopped
//	failed                  > restart_max FAILED starts within                crash-looping
//	                        restart_window_sec, counted per D64
//	failed|crash-looping    stop requested                                    stopped
//	crash-looping           POST …/reset-failed or …/safe-start               stopped — the ONLY
//	                                                                         actual-state
//	                                                                         transition an API
//	                                                                         handler may write
//	unknown                 unit properties readable again                    re-derived from the
//	                                                                         properties and the
//	                                                                         health probe
//	any                     unit gone / systemd unreachable                   unknown
//
// The ready → failed row is load-bearing, not tidiness. An UNREQUESTED exit is
// `failed` regardless of exit code; the exit CODE decides the ledger outcome,
// and the ledger outcome decides whether the supervisor restarts. Those are two
// different questions and the table answers them separately on purpose — without
// this row, `inhibit_reason='clean_exit'` (defined as on-failure AND
// LAST_CLOSED.outcome='stopped' AND state IN ('failed','crash-looping')) would
// be unreachable.
//
// `stale-version` and `inhibited` are deliberately NOT members: an instance that
// is serving traffic cannot also be in a state that excludes it from every
// ready-gated behavior. They are derived flags — see the four functions below.
type InstanceState string

const (
	InstanceUnknown      InstanceState = "unknown"
	InstanceStopped      InstanceState = "stopped"
	InstanceStarting     InstanceState = "starting"
	InstanceLoading      InstanceState = "loading"
	InstanceReady        InstanceState = "ready"
	InstanceDegraded     InstanceState = "degraded"
	InstanceStopping     InstanceState = "stopping"
	InstanceFailed       InstanceState = "failed"
	InstanceCrashLooping InstanceState = "crash-looping"
)

// InstanceStateValues lists the members of the `instance_status.state` CHECK
// constraint, in order.
func InstanceStateValues() []InstanceState {
	return []InstanceState{
		InstanceUnknown, InstanceStopped, InstanceStarting, InstanceLoading,
		InstanceReady, InstanceDegraded, InstanceStopping, InstanceFailed,
		InstanceCrashLooping,
	}
}

// Valid reports whether s is a member of the CHECK constraint.
func (s InstanceState) Valid() bool { return valid(s, InstanceStateValues()) }

// GPUAttribution is `instance_status.gpu_attribution` (§2.8, §8.6): how
// `gpu_uuids_json` was obtained. The bench exclusivity guard treats anything but
// `measured` conservatively, which is the whole reason the column exists.
type GPUAttribution string

const (
	AttributionMeasured GPUAttribution = "measured"
	AttributionDeclared GPUAttribution = "declared"
	AttributionUnknown  GPUAttribution = "unknown"
)

// GPUAttributionValues lists the members of the `instance_status.gpu_attribution`
// CHECK constraint, in order.
func GPUAttributionValues() []GPUAttribution {
	return []GPUAttribution{AttributionMeasured, AttributionDeclared, AttributionUnknown}
}

// Valid reports whether a is a member of the CHECK constraint.
func (a GPUAttribution) Valid() bool { return valid(a, GPUAttributionValues()) }

// StartTrigger is `instance_starts.trigger` (§2.8): what caused this launch
// attempt. It is PendingTrigger plus 'external' — a `systemctl start` by hand,
// or a boot start of an enabled unit that the daemon did not stamp.
type StartTrigger string

const (
	StartByUser              StartTrigger = "user"
	StartByAutostart         StartTrigger = "autostart"
	StartBySupervisorRestart StartTrigger = "supervisor_restart"
	StartByRolling           StartTrigger = "rolling"
	StartByBenchRestore      StartTrigger = "bench_restore"
	StartBySafeStart         StartTrigger = "safe_start"
	StartByExternal          StartTrigger = "external"
)

// StartTriggerValues lists the members of the `instance_starts.trigger` CHECK
// constraint, in order.
func StartTriggerValues() []StartTrigger {
	return []StartTrigger{
		StartByUser, StartByAutostart, StartBySupervisorRestart, StartByRolling,
		StartByBenchRestore, StartBySafeStart, StartByExternal,
	}
}

// Valid reports whether t is a member of the CHECK constraint.
func (t StartTrigger) Valid() bool { return valid(t, StartTriggerValues()) }

// StartOutcome is `instance_starts.outcome` (§2.8, D63). NULL means the run is
// in flight; the value is written EXACTLY ONCE, at the end of the run.
//
// 'ready' is deliberately not a member: reaching ready is an EVENT WITHIN a run,
// not the end of one, and `instance_starts.ready_at` records it without closing
// the row. 'inhibited' rows describe a REFUSAL to start rather than a run — no
// execve happened, no exit_code exists, nothing ended — so they are excluded
// both from LAST_CLOSED and from the crash-loop count (D64). A refusal that made
// itself more certain by being recorded would be a state no user could leave.
type StartOutcome string

const (
	OutcomeFailed    StartOutcome = "failed"
	OutcomeInhibited StartOutcome = "inhibited"
	OutcomeStopped   StartOutcome = "stopped"
)

// StartOutcomeValues lists the members of the `instance_starts.outcome` CHECK
// constraint, in order.
func StartOutcomeValues() []StartOutcome {
	return []StartOutcome{OutcomeFailed, OutcomeInhibited, OutcomeStopped}
}

// Valid reports whether o is a member of the CHECK constraint.
func (o StartOutcome) Valid() bool { return valid(o, StartOutcomeValues()) }

// InhibitReason is the machine-readable reason carried by the `inhibited`
// derived flag, and written into `instance_starts.error_code` on the refusal row
// (§2.8). The column is not CHECK-constrained — it also carries launcher error
// codes like 'model_missing' — so this set is closed by the application alone
// and is absent from ClosedEnums.
type InhibitReason string

const (
	InhibitPolicyNever InhibitReason = "policy_never"
	InhibitCrashLoop   InhibitReason = "crash_loop"
	InhibitCleanExit   InhibitReason = "clean_exit"
)

// InhibitReasonValues lists the three reasons the design names, in order.
func InhibitReasonValues() []InhibitReason {
	return []InhibitReason{InhibitPolicyNever, InhibitCrashLoop, InhibitCleanExit}
}

// Valid reports whether r is one of the three reasons.
func (r InhibitReason) Valid() bool { return valid(r, InhibitReasonValues()) }

// Instance is one row of `instances` (§2.8): the CONFIGURATION half, written by
// the API with the seven named exceptions of §2.8's writer table. Pointer fields
// are the nullable columns, where NULL is a fact rather than an absent zero — no
// draft model, no pending hand-off, not deleted.
type Instance struct {
	ID          string
	Name        string
	DisplayName *string
	Description *string

	ModelID       *string
	MmprojModelID *string
	DraftModelID  *string

	PublicPort   int
	InternalPort int

	AuthMode         AuthMode
	Autostart        bool
	RestartPolicy    RestartPolicy
	RestartMax       int
	RestartWindowSec int

	FlagsJSON  string
	ExtraFlags string
	// ConfigHash is a STORED column with three inputs (D52) and three writers
	// (D69): POST/PATCH, llama.cpp activation, and the models service. It is
	// never computed on read — `instance_starts.config_hash` has to record the
	// value at the moment of an attempt, which only a stored column supplies.
	ConfigHash      string
	DesiredState    DesiredState
	DraftValidation DraftValidation

	// PendingTrigger and PendingOverrideJSON are the hand-off channel, not
	// configuration: the daemon stamps them before StartUnit and `instance-exec`
	// consumes and clears BOTH in one transaction (§5.6 step 3, D61).
	PendingTrigger      *PendingTrigger
	PendingOverrideJSON *string

	UnitName string
	// Generation is optimistic concurrency for PATCH. None of the seven
	// exceptional writers bumps it, so a mismatch always means "someone edited
	// the configuration under you" rather than "housekeeping happened".
	Generation int64

	CreatedAt int64
	UpdatedAt int64
	// DeletedAt is D68's soft delete. All three unique indexes are partial over
	// it, so the name and both ports are reusable the instant it is stamped.
	DeletedAt *int64
}

// Deleted reports whether this row is soft-deleted.
func (i Instance) Deleted() bool { return i.DeletedAt != nil }

// InstanceStatus is one row of `instance_status` (§2.8): OBSERVED reality,
// separated from the config row so high-frequency writes never touch it. The
// supervisor owns it, with the three narrow exceptions §2.8's second writer
// table names (reset-failed and safe-start clearing the crash-loop latch).
//
// The row is inserted by the instances service in the SAME transaction as the
// `instances` row, which is what lets every reader use an inner join and lets
// the derived flags below reason about a brand-new instance as `state='unknown'`
// rather than as an absent row.
type InstanceStatus struct {
	InstanceID string
	State      InstanceState

	SystemdActive *string
	SystemdSub    *string
	SystemdResult *string
	MainPID       *int64
	ExeVersionID  *string

	// AppliedConfigHash is written in exactly one place — the supervisor, at
	// the first /health 200 — so a launcher that reached execve and then died
	// during model load never records a configuration that ran.
	AppliedConfigHash *string

	ReadyAt      *int64
	LastChangeAt int64
	LastHealthAt *int64
	HealthCode   *int64

	SlotsTotal     *int64
	SlotsBusy      *int64
	CtxSize        *int64
	RequestsServed *int64
	RSSBytes       *int64
	VRAMBytes      *int64

	GPUUUIDsJSON   *string
	GPUAttribution GPUAttribution
	FitReportJSON  *string

	LastExitCode *int64
	LastError    *string

	ReconcileBackoffUntil *int64
	// RestartWindowResetAt is D64's cutoff: crash-loop counting ignores
	// `instance_starts` rows at or before this instant.
	RestartWindowResetAt int64
	DeviceMapJSON        *string
}

// InstanceStart is one row of `instance_starts` (§2.8): every launch attempt,
// opened before preflight (D54) and closed exactly once (D63).
type InstanceStart struct {
	ID         string
	InstanceID string
	At         int64
	Trigger    StartTrigger

	ConfigHash string
	// EffectiveConfigHash is the hash of what was ACTUALLY rendered: equal to
	// ConfigHash normally, and the override hash for a safe start (D61).
	EffectiveConfigHash *string
	OverrideJSON        *string
	ArgvJSON            *string
	LlamacppVersionID   *string

	// ReadyAt is stamped by the supervisor at the first /health 200. It does
	// NOT close the row — the run is still in flight (D63).
	ReadyAt *int64
	// Outcome is NULL while the run is in flight and is written exactly once.
	Outcome *StartOutcome

	ExitCode     *int64
	ErrorCode    *string
	ErrorMessage *string
	DetailJSON   *string
	EndedAt      *int64
}

// Open reports whether this row is THE_OPEN_ROW — the run that is happening now.
func (s InstanceStart) Open() bool { return s.Outcome == nil }

// InstancePorts is one NON-DELETED instance's claim on both ports: the
// exclusion set §2.8's port rules, `GET /ports/suggest` and the management-port
// walk of §11.1 step 7 all read. A soft-deleted instance holds neither port
// (D68), which is why nothing here has a `deleted_at`.
type InstancePorts struct {
	InstanceID   string
	Name         string
	PublicPort   int
	InternalPort int
}

// InstanceView is the config ⋈ status join `GET /instances` returns, plus the
// two `instance_starts` facts the derived flags read. It is one struct because
// those four flags are computed on read from all three tables at once and there
// is no honest way to answer a list request without them.
type InstanceView struct {
	Instance
	Status InstanceStatus

	// LastClosedOutcome is LAST_CLOSED.outcome: the outcome of the row with the
	// greatest `at` among rows whose outcome is neither NULL nor `inhibited`.
	// Nil means this instance has never completed a run. The `inhibited`
	// exclusion is load-bearing: a refusal row counted as LAST_CLOSED would
	// falsify the very `clean_exit` clause that produced it (§2.8).
	LastClosedOutcome *StartOutcome
	// OpenOverride describes THE_OPEN_ROW: nil when nothing is running, false
	// for an ordinary run, true while a safe start is the running
	// configuration. `restart_required` reads THIS and never LAST_CLOSED —
	// reading the closed row latched the flag forever after one safe start.
	OpenOverride *bool
}

// DerivedFlags are §2.8's four "computed on read, never stored, never states"
// booleans. Keeping them out of the `state` column is what lets an instance be
// simultaneously `ready` — serving traffic, answering /health 200 — and flagged.
type DerivedFlags struct {
	RestartRequired bool
	StaleVersion    bool
	Inhibited       bool
	// InhibitReason is set exactly when Inhibited is true, so the remediation
	// card can say which of the three refusals this is.
	InhibitReason   *InhibitReason
	DraftUnverified bool
}

// Derived computes the four flags for this view against the id of the
// `is_active=1` llama.cpp version (which may be empty when no build is active).
//
// Two NULL hazards are closed by construction, exactly as §2.8 argues. A
// never-started instance — `state='unknown'`, no closed row, no open row, NULL
// `applied_config_hash` — is none of the four: `restart_required` follows SQL's
// three-valued comparison, where `hash != NULL` is unknown rather than true, and
// the state clause excludes it anyway. And `inhibited` keys off `outcome`, which
// is never NULL on a closed row and never `'ready'` (D63), rather than off the
// `exit_code` an earlier rule read and which was NULL for exactly the rows it
// needed it on.
func (v InstanceView) Derived(activeVersionID string) DerivedFlags {
	var out DerivedFlags

	serving := v.Status.State == InstanceReady || v.Status.State == InstanceDegraded

	hashDiffers := v.Status.AppliedConfigHash != nil && v.ConfigHash != *v.Status.AppliedConfigHash
	safeStartRunning := v.OpenOverride != nil && *v.OpenOverride
	out.RestartRequired = (hashDiffers || safeStartRunning) && serving

	out.StaleVersion = v.Status.ExeVersionID != nil &&
		*v.Status.ExeVersionID != activeVersionID &&
		(serving || v.Status.State == InstanceLoading)

	if (v.Status.State == InstanceFailed || v.Status.State == InstanceCrashLooping) &&
		v.DesiredState == DesiredRunning {
		switch {
		case v.RestartPolicy == RestartNever:
			out.Inhibited, out.InhibitReason = true, ptrTo(InhibitPolicyNever)
		case v.Status.State == InstanceCrashLooping:
			out.Inhibited, out.InhibitReason = true, ptrTo(InhibitCrashLoop)
		case v.RestartPolicy == RestartOnFailure &&
			v.LastClosedOutcome != nil && *v.LastClosedOutcome == OutcomeStopped:
			out.Inhibited, out.InhibitReason = true, ptrTo(InhibitCleanExit)
		}
	}

	out.DraftUnverified = v.DraftValidation == DraftDeferred
	return out
}

// ptrTo is the one-line address-of a constant that Go otherwise forbids.
func ptrTo[T any](v T) *T { return &v }
