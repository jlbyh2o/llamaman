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
