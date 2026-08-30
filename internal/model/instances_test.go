package model

import "testing"

// The derived-flag truth table (DESIGN section 15).
//
// "the derived-flag functions (restart_required, stale_version, inhibited +
// inhibit_reason, draft_unverified) as a truth table over (state, config_hash,
// exe_version_id, restart_policy, last closed start's outcome, open row's
// override), asserting in particular that a stale_version or inhibited instance
// can still be ready, that an instance with NO closed start row is none of the
// four, that a safe-started instance reads restart_required WHILE its override
// row is open, and — the regression this table exists for — that once that row
// is closed and an ordinary start has taken over, restart_required is FALSE
// again."

const activeVersion = "b10621-cuda-src"

// view builds an InstanceView with sane defaults, so each case names only the
// fields it is about.
func view(mutate func(*InstanceView)) InstanceView {
	v := InstanceView{
		Instance: Instance{
			ID:              "i-1",
			Name:            "qwen",
			ConfigHash:      "hash-a",
			DesiredState:    DesiredStopped,
			RestartPolicy:   RestartOnFailure,
			DraftValidation: DraftOK,
		},
		Status: InstanceStatus{
			InstanceID: "i-1",
			State:      InstanceUnknown,
		},
	}
	if mutate != nil {
		mutate(&v)
	}
	return v
}

func TestDerivedFlags(t *testing.T) {
	tests := []struct {
		name string
		view InstanceView
		want DerivedFlags
		why  string
	}{
		{
			name: "a brand-new instance is none of the four",
			view: view(nil),
			want: DerivedFlags{},
			why: "state='unknown', no closed row, no open row, NULL applied_config_hash — " +
				"every clause is false by construction (§2.8)",
		},
		{
			name: "ready and current",
			view: view(func(v *InstanceView) {
				v.Status.State = InstanceReady
				v.Status.AppliedConfigHash = ptrTo("hash-a")
				v.Status.ExeVersionID = ptrTo(activeVersion)
				v.OpenOverride = ptrTo(false)
			}),
			want: DerivedFlags{},
		},
		{
			name: "an edited configuration that has not been restarted",
			view: view(func(v *InstanceView) {
				v.Status.State = InstanceReady
				v.Status.AppliedConfigHash = ptrTo("hash-old")
				v.OpenOverride = ptrTo(false)
			}),
			want: DerivedFlags{RestartRequired: true},
		},
		{
			name: "the same edit while the instance is stopped",
			view: view(func(v *InstanceView) {
				v.Status.State = InstanceStopped
				v.Status.AppliedConfigHash = ptrTo("hash-old")
			}),
			want: DerivedFlags{},
			why:  "restart_required is about a RUNNING configuration; there is nothing to restart",
		},
		{
			name: "degraded still counts as serving",
			view: view(func(v *InstanceView) {
				v.Status.State = InstanceDegraded
				v.Status.AppliedConfigHash = ptrTo("hash-old")
			}),
			want: DerivedFlags{RestartRequired: true},
		},
		{
			name: "a safe start is running",
			view: view(func(v *InstanceView) {
				v.Status.State = InstanceReady
				v.Status.AppliedConfigHash = ptrTo("hash-a")
				v.OpenOverride = ptrTo(true)
			}),
			want: DerivedFlags{RestartRequired: true},
			why: "the running configuration is not the saved one, even though the hashes " +
				"agree (D61, §3.10b step 3)",
		},
		{
			name: "the safe start ended and an ordinary start took over",
			view: view(func(v *InstanceView) {
				v.Status.State = InstanceReady
				v.Status.AppliedConfigHash = ptrTo("hash-a")
				v.OpenOverride = ptrTo(false)
				v.LastClosedOutcome = ptrTo(OutcomeStopped)
			}),
			want: DerivedFlags{},
			why: "THE REGRESSION THIS TABLE EXISTS FOR: reading LAST_CLOSED latched the flag " +
				"forever after one safe start. It reads THE_OPEN_ROW (§2.8)",
		},
		{
			name: "serving from a superseded build",
			view: view(func(v *InstanceView) {
				v.Status.State = InstanceReady
				v.Status.AppliedConfigHash = ptrTo("hash-a")
				v.Status.ExeVersionID = ptrTo("b10604-cuda-src")
			}),
			want: DerivedFlags{StaleVersion: true},
			why:  "D25/F8: a badge, not a state — the instance is still ready and still serving",
		},
		{
			name: "still loading from a superseded build",
			view: view(func(v *InstanceView) {
				v.Status.State = InstanceLoading
				v.Status.ExeVersionID = ptrTo("b10604-cuda-src")
			}),
			want: DerivedFlags{StaleVersion: true},
		},
		{
			name: "a stopped instance is never stale",
			view: view(func(v *InstanceView) {
				v.Status.State = InstanceStopped
				v.Status.ExeVersionID = ptrTo("b10604-cuda-src")
			}),
			want: DerivedFlags{},
			why:  "exe_version_id describes a process; a stopped instance has none",
		},
		{
			name: "restart_policy never",
			view: view(func(v *InstanceView) {
				v.Status.State = InstanceFailed
				v.DesiredState = DesiredRunning
				v.RestartPolicy = RestartNever
				v.LastClosedOutcome = ptrTo(OutcomeFailed)
			}),
			want: DerivedFlags{Inhibited: true, InhibitReason: ptrTo(InhibitPolicyNever)},
		},
		{
			name: "the crash-loop latch",
			view: view(func(v *InstanceView) {
				v.Status.State = InstanceCrashLooping
				v.DesiredState = DesiredRunning
				v.LastClosedOutcome = ptrTo(OutcomeFailed)
			}),
			want: DerivedFlags{Inhibited: true, InhibitReason: ptrTo(InhibitCrashLoop)},
		},
		{
			name: "a clean exit under on-failure",
			view: view(func(v *InstanceView) {
				v.Status.State = InstanceFailed
				v.DesiredState = DesiredRunning
				v.RestartPolicy = RestartOnFailure
				v.LastClosedOutcome = ptrTo(OutcomeStopped)
			}),
			want: DerivedFlags{Inhibited: true, InhibitReason: ptrTo(InhibitCleanExit)},
			why: "on-failure's central promise — a clean exit is not a failure, so we do not " +
				"restart it, and we tell you why. The `ready → failed` transition is what " +
				"makes this reason reachable at all",
		},
		{
			name: "a failed start under on-failure is NOT inhibited",
			view: view(func(v *InstanceView) {
				v.Status.State = InstanceFailed
				v.DesiredState = DesiredRunning
				v.RestartPolicy = RestartOnFailure
				v.LastClosedOutcome = ptrTo(OutcomeFailed)
			}),
			want: DerivedFlags{},
			why:  "the supervisor is about to restart it; nothing is being declined",
		},
		{
			name: "failed but nobody asked for it to run",
			view: view(func(v *InstanceView) {
				v.Status.State = InstanceFailed
				v.DesiredState = DesiredStopped
				v.RestartPolicy = RestartNever
				v.LastClosedOutcome = ptrTo(OutcomeStopped)
			}),
			want: DerivedFlags{},
			why:  "inhibited means a restart is DUE and declined",
		},
		{
			name: "policy_never wins over the crash-loop latch",
			view: view(func(v *InstanceView) {
				v.Status.State = InstanceCrashLooping
				v.DesiredState = DesiredRunning
				v.RestartPolicy = RestartNever
			}),
			want: DerivedFlags{Inhibited: true, InhibitReason: ptrTo(InhibitPolicyNever)},
			why:  "the remediation differs: one is a setting to change, the other a button to press",
		},
		{
			name: "a deferred draft check",
			view: view(func(v *InstanceView) {
				v.DraftValidation = DraftDeferred
			}),
			want: DerivedFlags{DraftUnverified: true},
		},
		{
			name: "flagged and still ready at the same time",
			view: view(func(v *InstanceView) {
				v.Status.State = InstanceReady
				v.Status.AppliedConfigHash = ptrTo("hash-old")
				v.Status.ExeVersionID = ptrTo("b10604-cuda-src")
				v.DraftValidation = DraftDeferred
			}),
			want: DerivedFlags{RestartRequired: true, StaleVersion: true, DraftUnverified: true},
			why: "keeping these out of the `state` column is precisely what lets an instance " +
				"be serving traffic and flagged at once (§2.8)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.view.Derived(activeVersion)

			if got.RestartRequired != tt.want.RestartRequired {
				t.Errorf("restart_required = %v, want %v (%s)",
					got.RestartRequired, tt.want.RestartRequired, tt.why)
			}
			if got.StaleVersion != tt.want.StaleVersion {
				t.Errorf("stale_version = %v, want %v (%s)",
					got.StaleVersion, tt.want.StaleVersion, tt.why)
			}
			if got.DraftUnverified != tt.want.DraftUnverified {
				t.Errorf("draft_unverified = %v, want %v (%s)",
					got.DraftUnverified, tt.want.DraftUnverified, tt.why)
			}
			if got.Inhibited != tt.want.Inhibited {
				t.Errorf("inhibited = %v, want %v (%s)", got.Inhibited, tt.want.Inhibited, tt.why)
			}
			switch {
			case tt.want.InhibitReason == nil && got.InhibitReason != nil:
				t.Errorf("inhibit_reason = %q, want none", *got.InhibitReason)
			case tt.want.InhibitReason != nil && got.InhibitReason == nil:
				t.Errorf("inhibit_reason = none, want %q", *tt.want.InhibitReason)
			case tt.want.InhibitReason != nil && *got.InhibitReason != *tt.want.InhibitReason:
				t.Errorf("inhibit_reason = %q, want %q (%s)",
					*got.InhibitReason, *tt.want.InhibitReason, tt.why)
			}
		})
	}
}

// TestInhibitedCanBeReadyIsImpossible is the other half of "inhibited is not a
// state": it is defined only over `failed`/`crash-looping`, so an instance that
// is serving can never carry it however the rest of the row reads.
func TestInhibitedIsNeverSetWhileServing(t *testing.T) {
	for _, state := range []InstanceState{InstanceReady, InstanceDegraded, InstanceLoading} {
		v := view(func(v *InstanceView) {
			v.Status.State = state
			v.DesiredState = DesiredRunning
			v.RestartPolicy = RestartNever
			v.LastClosedOutcome = ptrTo(OutcomeStopped)
		})
		if got := v.Derived(activeVersion); got.Inhibited {
			t.Errorf("state %q came back inhibited; a serving instance is not being declined", state)
		}
	}
}

// TestOpenAndDeleted are the two one-line predicates the service and the
// supervisor branch on.
func TestOpenAndDeleted(t *testing.T) {
	if !(InstanceStart{}).Open() {
		t.Error("a row with a NULL outcome is the open row (D63)")
	}
	if (InstanceStart{Outcome: ptrTo(OutcomeStopped)}).Open() {
		t.Error("a closed row is not the open row")
	}
	if (Instance{}).Deleted() {
		t.Error("a NULL deleted_at is not a deletion")
	}
	if !(Instance{DeletedAt: ptrTo(int64(1))}).Deleted() {
		t.Error("a stamped deleted_at is a soft delete (D68)")
	}
}
