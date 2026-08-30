package supervisor

import (
	"testing"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// The restart policy and the crash-loop cutoff, as a table (D7/D8/D63/D64).
//
// Every row here is a decision a user reasons about directly — "why did it not
// come back?" — and the four consequences §5.8 calls deliberate are each a row
// of their own, because each is a bug the naive "count every row and restart on
// any exit" rule would have shipped.

var testNow = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func closed(outcome model.StartOutcome, exit int64) *model.InstanceStart {
	return &model.InstanceStart{
		At:       testNow.Add(-time.Minute).UnixMilli(),
		Outcome:  &outcome,
		ExitCode: &exit,
		EndedAt:  ptr(testNow.Add(-time.Minute).UnixMilli()),
	}
}

func TestEvaluateStart(t *testing.T) {
	base := StartInput{
		Policy:           model.RestartOnFailure,
		RestartMax:       5,
		RestartWindowSec: 600,
		RuntimeReady:     true,
		Now:              testNow,
	}

	cases := []struct {
		name string
		in   func(StartInput) StartInput
		want StartVerdict
	}{
		{
			name: "a never-started instance starts whatever the policy says",
			in: func(in StartInput) StartInput {
				in.State = model.InstanceStopped
				in.Policy = model.RestartNever
				return in
			},
			want: StartVerdict{Decision: DecideStart},
		},
		{
			name: "a clean stop the user asked for starts again on request",
			in: func(in StartInput) StartInput {
				in.State = model.InstanceStopped
				in.LastClosed = closed(model.OutcomeStopped, 0)
				return in
			},
			want: StartVerdict{Decision: DecideStart},
		},
		{
			name: "always restarts a clean exit",
			in: func(in StartInput) StartInput {
				in.State = model.InstanceFailed
				in.Policy = model.RestartAlways
				in.LastClosed = closed(model.OutcomeStopped, 0)
				return in
			},
			want: StartVerdict{Decision: DecideStart},
		},
		{
			name: "always restarts a dirty exit",
			in: func(in StartInput) StartInput {
				in.State = model.InstanceFailed
				in.Policy = model.RestartAlways
				in.LastClosed = closed(model.OutcomeFailed, 139)
				return in
			},
			want: StartVerdict{Decision: DecideStart},
		},
		{
			name: "on-failure restarts a failure",
			in: func(in StartInput) StartInput {
				in.State = model.InstanceFailed
				in.LastClosed = closed(model.OutcomeFailed, 1)
				return in
			},
			want: StartVerdict{Decision: DecideStart},
		},
		{
			name: "on-failure declines a clean exit, and says which reason",
			in: func(in StartInput) StartInput {
				in.State = model.InstanceFailed
				in.LastClosed = closed(model.OutcomeStopped, 0)
				return in
			},
			want: StartVerdict{Decision: DecideInhibit, Reason: model.InhibitCleanExit},
		},
		{
			name: "on-failure reads outcome, not exit_code: a preflight row has a launcher status",
			in: func(in StartInput) StartInput {
				in.State = model.InstanceFailed
				// Exit 72 is the launcher's "model file missing", not
				// llama-server's. The decision must still be "restart", because
				// the OUTCOME is `failed`.
				in.LastClosed = closed(model.OutcomeFailed, ExitModelMissing)
				return in
			},
			want: StartVerdict{Decision: DecideStart},
		},
		{
			name: "on-failure restarts a clean exit whose row the supervisor could not attribute",
			in: func(in StartInput) StartInput {
				in.State = model.InstanceFailed
				// `stopped` with a NULL exit code is what a row closed from
				// partial unit properties looks like. It is still `stopped`, so
				// it is still not restarted — the point is that the decision is
				// WELL-DEFINED rather than NULL-dependent.
				row := closed(model.OutcomeStopped, 0)
				row.ExitCode = nil
				in.LastClosed = row
				return in
			},
			want: StartVerdict{Decision: DecideInhibit, Reason: model.InhibitCleanExit},
		},
		{
			name: "never declines from failed",
			in: func(in StartInput) StartInput {
				in.State = model.InstanceFailed
				in.Policy = model.RestartNever
				in.LastClosed = closed(model.OutcomeFailed, 1)
				return in
			},
			want: StartVerdict{Decision: DecideInhibit, Reason: model.InhibitPolicyNever},
		},
		{
			name: "the cutoff overrides `always` — that is what no further automatic starts means",
			in: func(in StartInput) StartInput {
				in.State = model.InstanceFailed
				in.Policy = model.RestartAlways
				in.FailedInWindow = 6
				in.LastClosed = closed(model.OutcomeFailed, 1)
				return in
			},
			want: StartVerdict{
				Decision: DecideInhibit, Reason: model.InhibitCrashLoop, CrashLooping: true,
			},
		},
		{
			name: "exactly restart_max failures is not yet a crash loop",
			in: func(in StartInput) StartInput {
				in.State = model.InstanceFailed
				in.FailedInWindow = 5
				in.LastClosed = closed(model.OutcomeFailed, 1)
				return in
			},
			want: StartVerdict{Decision: DecideStart},
		},
		{
			name: "the latch keeps declining without recounting",
			in: func(in StartInput) StartInput {
				in.State = model.InstanceCrashLooping
				in.Policy = model.RestartAlways
				in.LastClosed = closed(model.OutcomeFailed, 1)
				return in
			},
			// CrashLooping is false: the latch is already set, so this pass
			// writes the refusal and not the state.
			want: StartVerdict{Decision: DecideInhibit, Reason: model.InhibitCrashLoop},
		},
		{
			name: "a live backoff waits, and waiting is not a refusal",
			in: func(in StartInput) StartInput {
				in.State = model.InstanceFailed
				in.LastClosed = closed(model.OutcomeFailed, 1)
				in.BackoffUntil = ptr(testNow.Add(time.Minute).UnixMilli())
				return in
			},
			want: StartVerdict{Decision: DecideWait},
		},
		{
			name: "an expired backoff no longer waits",
			in: func(in StartInput) StartInput {
				in.State = model.InstanceFailed
				in.LastClosed = closed(model.OutcomeFailed, 1)
				in.BackoffUntil = ptr(testNow.Add(-time.Minute).UnixMilli())
				return in
			},
			want: StartVerdict{Decision: DecideStart},
		},
		{
			name: "a rebuild of the active version waits rather than refusing (D78)",
			in: func(in StartInput) StartInput {
				in.State = model.InstanceFailed
				in.Policy = model.RestartNever
				in.RuntimeReady = false
				return in
			},
			// `never` would refuse; the rebuild check comes first, because the
			// instance is not being declined — it is waiting for a runtime.
			want: StartVerdict{Decision: DecideWait},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := EvaluateStart(tc.in(base))
			if got != tc.want {
				t.Errorf("EvaluateStart = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestBackoffFor pins the 5 s → 5 m curve and, more importantly, its ceiling: a
// backoff that kept doubling would put a broken instance hours away from its
// next attempt, so a host repaired by hand would look permanently dead.
func TestBackoffFor(t *testing.T) {
	cases := []struct {
		failures int
		want     time.Duration
	}{
		{0, 5 * time.Second},
		{1, 5 * time.Second},
		{2, 10 * time.Second},
		{3, 20 * time.Second},
		{6, 160 * time.Second},
		{7, 5 * time.Minute},
		{50, 5 * time.Minute},
	}
	for _, tc := range cases {
		if got := BackoffFor(tc.failures); got != tc.want {
			t.Errorf("BackoffFor(%d) = %s, want %s", tc.failures, got, tc.want)
		}
	}
}

// TestCrashWindowStart pins D64's "a start that worked resets the window": the
// count begins at the LATER of the window and the last reset, which is what
// makes "Reset failed" clear the state rather than relabel it.
func TestCrashWindowStart(t *testing.T) {
	windowStart := testNow.Add(-10 * time.Minute).UnixMilli()

	if got := CrashWindowStart(testNow, 600, 0); got != windowStart {
		t.Errorf("with no reset, start = %d, want %d", got, windowStart)
	}

	reset := testNow.Add(-time.Minute).UnixMilli()
	if got := CrashWindowStart(testNow, 600, reset); got != reset {
		t.Errorf("a recent reset must win: start = %d, want %d", got, reset)
	}

	old := testNow.Add(-time.Hour).UnixMilli()
	if got := CrashWindowStart(testNow, 600, old); got != windowStart {
		t.Errorf("a stale reset must not widen the window: start = %d, want %d", got, windowStart)
	}
}

// TestSynthesizedErrorCode pins the half of §5.6's table only the supervisor can
// satisfy: three exits happen before the ledger row exists, and exactly two of
// them are owed a synthesized row.
func TestSynthesizedErrorCode(t *testing.T) {
	cases := []struct {
		exit     int
		wantCode string
		wantOwed bool
	}{
		{ExitDBUnavailable, ErrLauncherDBUnavailable, true},
		{ExitSchemaMismatch, ErrSchemaMismatch, true},
		// Nothing is written for 64: the instance row is gone, so the foreign
		// key has no parent and an instance the user deleted needs no history.
		{ExitInstanceMissing, "", false},
		// Every other status closed its own row on the way out, so synthesizing
		// one would be a SECOND row for a single run.
		{ExitBadFlags, "", false},
		{ExitRuntimeMissing, "", false},
		{ExitModelMissing, "", false},
		{ExitPortConflict, "", false},
		{0, "", false},
	}
	for _, tc := range cases {
		code, owed := SynthesizedErrorCode(tc.exit)
		if code != tc.wantCode || owed != tc.wantOwed {
			t.Errorf("SynthesizedErrorCode(%d) = (%q, %t), want (%q, %t)",
				tc.exit, code, owed, tc.wantCode, tc.wantOwed)
		}
		if got := WritesOwnLedgerRow(tc.exit); got == (tc.exit == ExitDBUnavailable ||
			tc.exit == ExitSchemaMismatch || tc.exit == ExitInstanceMissing) {
			t.Errorf("WritesOwnLedgerRow(%d) = %t, which contradicts §5.6's table", tc.exit, got)
		}
	}
}
