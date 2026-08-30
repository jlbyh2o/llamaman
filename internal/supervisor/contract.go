package supervisor

// The §5.6 exit-code contract, in one place because it has exactly two readers
// and they are different processes.
//
// `instance-exec` produces these codes; the supervisor reads them back — from
// `instance_starts.exit_code` on a row the launcher closed, and from the unit's
// `ExecMainStatus` on the three exits that happen before a row exists. Two
// copies of this table would be two copies of a wire format, and the half that
// drifted would be the half nobody ran: exits 70 and 75 are precisely the ones
// no test on the launcher side can observe, because there is no row to assert
// against.
//
// It lives in this package rather than in internal/cli because the ledger is
// the supervisor's: these are values of a column it owns and closes. The
// launcher imports the table; nothing here imports the launcher.

// Launcher exit statuses. The numbers are sysexits.h values chosen so that a
// journal reader and a `systemctl status` line say something recognizable, and
// they are frozen: the supervisor maps them onto `error_code`, the UI maps
// `error_code` onto a remediation card (§17), and a renumbering would silently
// re-file every historical row.
const (
	// ExitInstanceMissing: the instance row is gone or `deleted_at` is set.
	// NO ledger row is written and none is synthesized — the FK has no parent,
	// and an instance the user deleted needs no history.
	ExitInstanceMissing = 64
	// ExitBadFlags: `flags_json`, `extra_flags` or the start override would not
	// parse, or `draft_validation='mismatch'` refuses the start (§3.10a).
	ExitBadFlags = 65
	// ExitRuntimeMissing: `versions/active` does not resolve to a directory
	// holding `bin/llama-server`, or the active build is being rebuilt in place
	// (D78). Two error codes share this status because the remediation differs
	// and the failure does not: `runtime_missing` offers a rebuild or a
	// rollback, `runtime_rebuilding` says to wait and the supervisor starts the
	// instance on its own once the row is `ready` again.
	ExitRuntimeMissing = 69
	// ExitDBUnavailable: there is no working read-write connection to the
	// database. NO ledger row is written — writing one is the operation that
	// just failed — so the supervisor synthesizes a closed row from the unit's
	// ExecMainStatus.
	ExitDBUnavailable = 70
	// ExitModelMissing: a referenced GGUF is not on disk. The resolved path
	// travels in the message so F4's re-download card can name it.
	ExitModelMissing = 72
	// ExitSchemaMismatch: the schema gate of §5.6a. The DB is behind this
	// binary and did not catch up within the bounded wait, or it is ahead.
	// NO ledger row is written — `instance_starts` is a table under a schema
	// this binary does not understand — and the synthesized row is EXCLUDED
	// from the crash-loop count.
	ExitSchemaMismatch = 75
	// ExitPortConflict: `127.0.0.1:<internal_port>` is occupied. The supervisor
	// reassigns the port from the pool rather than retrying (F5, §5.8).
	ExitPortConflict = 78
)

// The `instance_starts.error_code` values the launcher and the supervisor
// write. The column is not CHECK-constrained — §2.8 says so, because it also
// carries the three inhibit reasons — so this set is closed by the application
// alone, and these constants are what closes it.
const (
	// ErrInstanceMissing has no row to appear on. It is here because the
	// exit-code table names it and a reader of one should find the other.
	ErrInstanceMissing = "instance_missing"
	// ErrBadFlags is exit 65 from a `flags_json`/`extra_flags`/override parse.
	ErrBadFlags = "bad_flags"
	// ErrDraftVocabMismatch is exit 65 from a refused start on
	// `draft_validation='mismatch'` (§3.10a) — a different remediation from a
	// parse failure, which is why it is a different code on the same status.
	ErrDraftVocabMismatch = "draft_vocab_mismatch"
	// ErrRuntimeMissing is exit 69 with no usable `bin/llama-server`.
	ErrRuntimeMissing = "runtime_missing"
	// ErrRuntimeRebuilding is exit 69 against a version being reinstalled in
	// place (D78). No user action is needed, and the card says so.
	ErrRuntimeRebuilding = "runtime_rebuilding"
	// ErrLauncherDBUnavailable is the synthesized code for exit 70.
	ErrLauncherDBUnavailable = "launcher_db_unavailable"
	// ErrModelMissing is exit 72.
	ErrModelMissing = "model_missing"
	// ErrSchemaMismatch is exit 75 with the DB BEHIND this binary and the
	// bounded wait exhausted.
	ErrSchemaMismatch = "schema_mismatch"
	// ErrSchemaAhead is exit 75 with the DB AHEAD of this binary — the state
	// after a downgrade across a schema bump, until §12.4's procedure has run.
	// Waiting cannot help, so the launcher does not wait.
	ErrSchemaAhead = "schema_ahead"
	// ErrPortConflict is exit 78.
	ErrPortConflict = "port_conflict"

	// ErrLauncherSuperseded closes a row a PREVIOUS run left open, inside the
	// new run's own transaction (§5.6 step 3). `exit_code` stays NULL: nothing
	// observed how that run ended, which is exactly why the row was left open,
	// and D64 excludes it from the crash-loop count for the same reason.
	ErrLauncherSuperseded = "launcher_superseded"
	// ErrDaemonRestarted closes a row left open by a PREVIOUS DAEMON whose unit
	// is no longer there (boot reconciliation, §5.8 step 3). A unit that is
	// still active leaves its row open instead — the process it describes is
	// still running.
	ErrDaemonRestarted = "daemon_restarted"
	// ErrStartTimeout closes a run that never reached `/health` 200 within
	// `instances.start_timeout_sec` (§2.8's transition table).
	ErrStartTimeout = "start_timeout"
	// ErrUnitFailed closes a run from unit properties that say it failed with
	// no launcher code to attribute it to.
	ErrUnitFailed = "unit_failed"
)

// SynthesizedErrorCode maps a launcher exit status the supervisor observed as
// `ExecMainStatus` onto the `error_code` of the row it must synthesize, and
// reports whether a row is owed at all.
//
// Only the three statuses that fail BEFORE the ledger row exists are answered
// here, and the third is answered `false`: exit 64 means the instance row is
// gone, so the foreign key has no parent and the insert could not succeed even
// if history were wanted. Every other status closed its own row on the way out,
// so a synthesized row for one would be a second row for a single run.
//
// The two schema statuses collapse into `schema_mismatch` unless the journal
// says otherwise. The distinction between `schema_mismatch` and `schema_ahead`
// is visible only in the launcher's own stderr line, and both are excluded from
// the crash-loop count identically, so guessing the more common one and letting
// the journal carry the detail costs nothing a user can see.
func SynthesizedErrorCode(exitStatus int) (string, bool) {
	switch exitStatus {
	case ExitDBUnavailable:
		return ErrLauncherDBUnavailable, true
	case ExitSchemaMismatch:
		return ErrSchemaMismatch, true
	case ExitInstanceMissing:
		return "", false
	default:
		return "", false
	}
}

// WritesOwnLedgerRow reports whether a launcher exiting with this status had
// already opened its `instance_starts` row, and therefore closed it on the way
// out.
//
// It is the exact complement of §5.6's "steps 1, 1b and 2 are before the row
// exists" paragraph, and the supervisor consults it before deciding whether a
// unit that failed with no open row is a launcher that never got that far or a
// row somebody else already closed.
func WritesOwnLedgerRow(exitStatus int) bool {
	switch exitStatus {
	case ExitDBUnavailable, ExitSchemaMismatch, ExitInstanceMissing:
		return false
	default:
		return true
	}
}
