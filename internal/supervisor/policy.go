package supervisor

import (
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// The restart policy and the crash-loop cutoff (D7/D8/D63/D64, §5.8).
//
// Everything in this file is a pure function of rows already read. That is
// deliberate: the policy is the one part of the supervisor whose behavior a
// user reasons about directly ("why did it not come back?"), and a decision
// that can only be exercised by starting a real unit is a decision nobody
// tests exhaustively. Reading it out of the `instance_starts` ledger rather
// than out of systemd's own restart counters is the same argument one level up
// — behavior is observable data instead of guesswork.

// Decision is what one reconcile pass decided to do about an instance that is
// not running and is wanted running.
type Decision int

const (
	// DecideStart issues the start.
	DecideStart Decision = iota
	// DecideInhibit declines it, with a Reason.
	DecideInhibit
	// DecideWait takes no action at all and says nothing: a backoff that has
	// not expired, or an active build that is being rebuilt in place. Neither
	// is a refusal — the instance is still on its way up — so neither writes an
	// `inhibited` row, which would otherwise turn "try again in five seconds"
	// into a permanent-looking badge.
	DecideWait
)

// String renders a decision for a log line.
func (d Decision) String() string {
	switch d {
	case DecideStart:
		return "start"
	case DecideInhibit:
		return "inhibit"
	default:
		return "wait"
	}
}

// StartInput is everything the policy reads. It is a value rather than a set of
// store calls so the whole table below can be exercised without a database.
type StartInput struct {
	// State is `instance_status.state`, the ACTUAL axis.
	State model.InstanceState
	// Policy, RestartMax and RestartWindowSec are the instance's configuration.
	Policy           model.RestartPolicy
	RestartMax       int
	RestartWindowSec int

	// LastClosed is LAST_CLOSED: the most recent row with `outcome IS NOT NULL`
	// AND `outcome != 'inhibited'` — the last COMPLETED run. Nil when the
	// instance has never finished one.
	//
	// The `inhibited` exclusion belongs to the query that produced this field
	// and is load-bearing: a refusal row that counted as the previous start
	// would make `inhibit_reason='clean_exit'` false one pass after it became
	// true, and the badge would flicker off while the instance stayed inhibited.
	LastClosed *model.InstanceStart

	// FailedInWindow is D64's count: `outcome='failed'` rows after
	// MAX(restart_window_reset_at, now - restart_window_sec), excluding the
	// three error codes §5.8 names.
	FailedInWindow int

	// BackoffUntil is `instance_status.reconcile_backoff_until`.
	BackoffUntil *int64
	// RuntimeReady is false while the `is_active=1` build is out of `ready` —
	// a forced rebuild in place (D78). No start is attempted at all, so the
	// launcher's exit 69 `runtime_rebuilding` is the backstop rather than the
	// normal path.
	RuntimeReady bool
	// Now is the instant the pass is reasoning about.
	Now time.Time
}

// StartVerdict is the decision plus everything the caller needs to act on it.
type StartVerdict struct {
	Decision Decision
	// Reason is set only for DecideInhibit, and is written verbatim into the
	// refusal row's `error_code` — which is what makes §2.8's
	// one-row-per-EPISODE rule a query rather than a memory.
	Reason model.InhibitReason
	// CrashLooping reports that this pass found the cutoff exceeded and the
	// latch must be set. It is separate from Reason because the latch is a
	// state write and the refusal is a ledger write, and a pass that finds an
	// instance ALREADY latched writes only the second.
	CrashLooping bool
}

// EvaluateStart is §5.8's restart policy and D64's cutoff, in the order they
// actually apply.
//
// The ordering is the argument. A backoff that has not expired is checked
// before anything that writes, so a five-second tick cannot produce five
// refusal rows a second. The crash-loop cutoff is checked before the policy,
// because `crash-looping` overrides every policy including `always` — that is
// what "no further automatic starts" means. And the policy itself is consulted
// only from `failed`/`crash-looping`, never from `stopped`: a first start, and a
// start after a clean stop the user asked for, are not restarts, so
// `restart_policy='never'` must not prevent the Start button from working.
func EvaluateStart(in StartInput) StartVerdict {
	// While the active version is being reinstalled in place, the directory
	// `versions/active` names is the one being written. Waiting is not a
	// refusal: the supervisor starts the instance on its own once the row is
	// `ready` again, and the UI says "waiting for the rebuild".
	if !in.RuntimeReady {
		return StartVerdict{Decision: DecideWait}
	}
	if in.BackoffUntil != nil && in.Now.UnixMilli() < *in.BackoffUntil {
		return StartVerdict{Decision: DecideWait}
	}

	// A state that is not a failure is not a restart. `stopped` and `unknown`
	// with `desired_state='running'` mean somebody asked for this instance to
	// run and it is not running — the policy has nothing to say about that.
	if in.State != model.InstanceFailed && in.State != model.InstanceCrashLooping {
		return StartVerdict{Decision: DecideStart}
	}

	// The latch, already set by an earlier pass. It clears only through
	// `POST /instances/{id}/reset-failed` or `/safe-start`, which is the whole
	// point of it being a state rather than a recomputed condition.
	if in.State == model.InstanceCrashLooping {
		return StartVerdict{Decision: DecideInhibit, Reason: model.InhibitCrashLoop}
	}

	// D64, counted from failures only. Preflight failures ARE counted, which is
	// the whole reason D54 opens the ledger row before preflight: a model file
	// that has been deleted fails at exit 72 forever, and without counted rows
	// the supervisor would retry it on backoff until the heat death of the
	// universe instead of stopping after restart_max and showing F4's card.
	if in.FailedInWindow > in.RestartMax {
		return StartVerdict{
			Decision:     DecideInhibit,
			Reason:       model.InhibitCrashLoop,
			CrashLooping: true,
		}
	}

	switch in.Policy {
	case model.RestartAlways:
		// Restart on any exit, clean or not.
		return StartVerdict{Decision: DecideStart}

	case model.RestartNever:
		return StartVerdict{Decision: DecideInhibit, Reason: model.InhibitPolicyNever}

	case model.RestartOnFailure:
		// The decision reads `outcome`, never `exit_code`. That is what makes
		// it well-defined on a preflight row (whose exit code is a launcher
		// status, not llama-server's) and on a row the supervisor closed from
		// unit properties it could not fully observe (whose exit code may be
		// NULL). A previous `stopped` is not restarted WHATEVER its exit code —
		// a clean exit is not a failure, and the instance says so through the
		// `clean_exit` inhibit reason.
		if in.LastClosed != nil && in.LastClosed.Outcome != nil &&
			*in.LastClosed.Outcome == model.OutcomeStopped {
			return StartVerdict{Decision: DecideInhibit, Reason: model.InhibitCleanExit}
		}
		return StartVerdict{Decision: DecideStart}

	default:
		// An unknown policy is treated as the schema's default rather than as a
		// reason to leave an instance down: the CHECK constraint makes this
		// unreachable from the database, and a corrupted value should not be
		// the thing that decides an instance never comes back.
		return StartVerdict{Decision: DecideStart}
	}
}

// Backoff bounds: exponential 5 s → 5 m (§5.8).
const (
	// BackoffMin is the first interval after a failed start.
	BackoffMin = 5 * time.Second
	// BackoffMax is the ceiling. Five minutes is short enough that a host
	// repaired by hand recovers without anybody restarting the daemon, and long
	// enough that a permanently broken configuration is not a busy loop.
	BackoffMax = 5 * time.Minute
)

// BackoffFor returns the interval to wait after a start attempt for an instance
// with this many failures in the window.
//
// It doubles from BackoffMin and saturates at BackoffMax, and it is computed
// from the LEDGER rather than from a counter held in memory — so a daemon
// restart does not reset an instance that has been failing for an hour back to
// a five-second retry.
func BackoffFor(failures int) time.Duration {
	if failures <= 1 {
		return BackoffMin
	}
	d := BackoffMin
	for i := 1; i < failures; i++ {
		d *= 2
		if d >= BackoffMax {
			return BackoffMax
		}
	}
	return d
}

// CrashWindowStart is the instant D64's count begins at: the later of the
// window's own start and the last reset.
//
// `restart_window_reset_at` is what makes "Reset failed" and "Safe start"
// actually clear the state rather than merely change a label, and what makes a
// start that SERVED for a minute forget the failures that came before it — an
// instance that ran for an hour and then crashed twice is at 2, not at 2 plus
// whatever it accumulated last week.
func CrashWindowStart(now time.Time, windowSec int, resetAt int64) int64 {
	from := now.Add(-time.Duration(windowSec) * time.Second).UnixMilli()
	if resetAt > from {
		return resetAt
	}
	return from
}
