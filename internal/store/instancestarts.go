package store

import (
	"context"
	"fmt"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// instance_starts queries (DESIGN sections 2.8, 5.6 and 5.8).
//
// The ledger is the forensic record of every launch attempt and the input to
// the restart policy, and three schema rules govern every statement here:
//
//   - The row is OPENED before preflight (D54) and CLOSED on every exit path.
//     A row written only on the happy path would make a configuration that
//     fails preflight on every attempt look like an instance that never tried
//     to start — no crash-loop cutoff, no history, infinite backoff.
//   - `outcome` is written EXACTLY ONCE, at the end of the run (D63). Reaching
//     `/health` 200 stamps `ready_at` and does not close the row, so
//     StampStartReady and CloseInstanceStart can never race for one column.
//   - At most one open row per instance, ENFORCED by the unique partial index
//     `idx_instance_starts_open` rather than by convention. Every writer that
//     could produce a second one closes the first in its own transaction.

const startColumns = `id, instance_id, at, trigger, config_hash, effective_config_hash,
	override_json, argv_json, llamacpp_version_id, ready_at, outcome, exit_code,
	error_code, error_message, detail_json, ended_at`

// InsertInstanceStart opens a run. It fails against the unique partial index if
// a row for this instance is still open, which is deliberate: `instance-exec`
// closes any survivor inside its own step-3 transaction BEFORE inserting, so the
// insert can never be the thing that fails (§5.6 step 3).
func (s *Store) InsertInstanceStart(ctx context.Context, tx Tx, r model.InstanceStart) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO instance_starts (`+startColumns+`)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.InstanceID, r.At, string(r.Trigger), r.ConfigHash, r.EffectiveConfigHash,
		r.OverrideJSON, r.ArgvJSON, r.LlamacppVersionID, r.ReadyAt, enumArg(r.Outcome),
		r.ExitCode, r.ErrorCode, r.ErrorMessage, r.DetailJSON, r.EndedAt)
	if err != nil {
		return fmt.Errorf("insert instance_start %s: %w", r.ID, err)
	}
	return nil
}

// StartClosure is what closing a run records.
type StartClosure struct {
	Outcome      model.StartOutcome
	ExitCode     *int64
	ErrorCode    *string
	ErrorMessage *string
	DetailJSON   *string
	EndedAt      int64
}

// CloseInstanceStart writes the single-shot closure, guarded on the row still
// being open. It reports whether it matched, so a caller that lost the race —
// the supervisor closing a row `instance-exec` already closed — learns that
// rather than overwriting a recorded outcome.
func (s *Store) CloseInstanceStart(ctx context.Context, tx Tx, id string, c StartClosure) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE instance_starts SET
		   outcome = ?, exit_code = ?, error_code = ?, error_message = ?,
		   detail_json = ?, ended_at = ?
		 WHERE id = ? AND outcome IS NULL`,
		string(c.Outcome), c.ExitCode, c.ErrorCode, c.ErrorMessage,
		c.DetailJSON, c.EndedAt, id)
	if err != nil {
		return false, fmt.Errorf("close instance_start %s: %w", id, err)
	}
	return rowsChanged(res)
}

// CloseOpenInstanceStart closes whatever row is open for an instance, by
// instance rather than by row id. It is `instance-exec`'s step-3 sweep
// (`launcher_superseded`) and the supervisor's boot reconciliation
// (`daemon_restarted`), both of which know the instance and not the row.
func (s *Store) CloseOpenInstanceStart(ctx context.Context, tx Tx, instanceID string, c StartClosure) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE instance_starts SET
		   outcome = ?, exit_code = ?, error_code = ?, error_message = ?,
		   detail_json = ?, ended_at = ?
		 WHERE instance_id = ? AND outcome IS NULL`,
		string(c.Outcome), c.ExitCode, c.ErrorCode, c.ErrorMessage,
		c.DetailJSON, c.EndedAt, instanceID)
	if err != nil {
		return false, fmt.Errorf("close the open instance_start of %s: %w", instanceID, err)
	}
	return rowsChanged(res)
}

// StampStartReady records the first `/health` 200 for a run. It writes
// `ready_at` and NOTHING else — the run is still in flight, and `ready` is
// deliberately not a member of `outcome` (D63).
func (s *Store) StampStartReady(ctx context.Context, tx Tx, id string, at int64) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE instance_starts SET ready_at = ? WHERE id = ? AND outcome IS NULL AND ready_at IS NULL`,
		at, id)
	if err != nil {
		return false, fmt.Errorf("stamp ready_at on %s: %w", id, err)
	}
	return rowsChanged(res)
}

// UpdateStartRender records what the launcher actually rendered — step 9 of
// §5.6, written BEFORE the exec so it survives a crash on model load.
func (s *Store) UpdateStartRender(ctx context.Context, tx Tx,
	id string, argvJSON, effectiveHash, versionID *string) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE instance_starts SET argv_json = ?, effective_config_hash = ?, llamacpp_version_id = ?
		  WHERE id = ? AND outcome IS NULL`,
		argvJSON, effectiveHash, versionID, id)
	if err != nil {
		return false, fmt.Errorf("record the rendered argv on %s: %w", id, err)
	}
	return rowsChanged(res)
}

// OpenInstanceStart returns THE_OPEN_ROW for an instance — the run that is
// happening now — or ErrNotFound when nothing is running.
func (s *Store) OpenInstanceStart(ctx context.Context, tx Tx, instanceID string) (model.InstanceStart, error) {
	row := tx.QueryRowContext(ctx,
		`SELECT `+startColumns+` FROM instance_starts
		  WHERE instance_id = ? AND outcome IS NULL`, instanceID)
	r, err := scanInstanceStart(row)
	if err != nil {
		return model.InstanceStart{}, notFound(err)
	}
	return r, nil
}

// LastClosedInstanceStart returns LAST_CLOSED: the closed row with the greatest
// `at`, EXCLUDING `inhibited` rows.
//
// The exclusion is load-bearing rather than tidiness. An `inhibited` row records
// a refusal to start — no execve happened, no exit code exists, nothing ended —
// and counting it as the previous run would destroy the very condition that
// produced it: `inhibit_reason='clean_exit'` is defined as
// `LAST_CLOSED.outcome='stopped'`, so writing the refusal would make that clause
// false on the next pass and the badge would vanish while the instance was
// still, demonstrably, inhibited.
func (s *Store) LastClosedInstanceStart(ctx context.Context, tx Tx, instanceID string) (model.InstanceStart, error) {
	row := tx.QueryRowContext(ctx,
		`SELECT `+startColumns+` FROM instance_starts
		  WHERE instance_id = ? AND outcome IS NOT NULL AND outcome != 'inhibited'
		  ORDER BY at DESC, id DESC LIMIT 1`, instanceID)
	r, err := scanInstanceStart(row)
	if err != nil {
		return model.InstanceStart{}, notFound(err)
	}
	return r, nil
}

// DefaultStartLimit bounds a start listing. `GET /instances/{id}` shows the last
// five; the ledger view shows more.
const DefaultStartLimit = 100

// InstanceStarts lists a ledger newest first — trigger, outcome, exit code,
// detail and argv — INCLUDING preflight failures that never reached execve
// (D54) and including the `inhibited` refusals, which belong in the history even
// though they are never LAST_CLOSED.
func (s *Store) InstanceStarts(ctx context.Context, tx Tx, instanceID string, limit int) ([]model.InstanceStart, error) {
	if limit <= 0 {
		limit = DefaultStartLimit
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT `+startColumns+` FROM instance_starts
		  WHERE instance_id = ? ORDER BY at DESC, id DESC LIMIT ?`, instanceID, limit)
	if err != nil {
		return nil, fmt.Errorf("select instance_starts: %w", err)
	}
	defer rows.Close()

	var out []model.InstanceStart
	for rows.Next() {
		r, err := scanInstanceStart(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CountFailedStartsSince is D64's crash-loop query, and it reads exactly
// `idx_instance_starts_failed`.
//
// It counts `outcome='failed'` rows only. `stopped` and `inhibited` are never
// counted — counting every start would make a healthy instance crash-loop after
// six user restarts, and would make `inhibited` rows self-reinforcing, since
// declining writes a row that pushes the count further over the line. The caller
// passes `MAX(restart_window_reset_at, now - restart_window_sec)` as after, which
// is how "Reset failed" starts the window over.
//
// Three error codes are excluded, and each exclusion is a bug the bare
// "count every failed row" query would have shipped:
//
//   - `schema_mismatch` and `schema_ahead` (§5.6a) are properties of the
//     DAEMON's upgrade state, not of the instance, and the daemon whose arrival
//     resolves them is the same one doing the counting. Counting them would put
//     every autostart instance into `crash-looping` after one slow post-update
//     boot, each needing a manual Reset failed for a condition that had already
//     fixed itself.
//   - `launcher_superseded` (§5.6) marks a run whose outcome was never
//     observed — the previous launcher's closing UPDATE could not land, or an
//     external `systemctl start` raced the supervisor. Counting a guess as a
//     failure is still a guess.
var CrashLoopExcludedErrorCodes = []string{"schema_mismatch", "schema_ahead", "launcher_superseded"}

func (s *Store) CountFailedStartsSince(ctx context.Context, tx Tx, instanceID string, after int64) (int, error) {
	var n int
	err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM instance_starts
		  WHERE instance_id = ? AND outcome = 'failed' AND at > ?
		    AND (error_code IS NULL
		         OR error_code NOT IN ('schema_mismatch','schema_ahead','launcher_superseded'))`,
		instanceID, after).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count failed starts of %s: %w", instanceID, err)
	}
	return n, nil
}

// HasInhibitedStartSince answers §2.8's one-row-per-refusal-EPISODE rule: the
// supervisor writes an `inhibited` row only when no row with this reason already
// exists after LAST_CLOSED.
//
// Without the rule the reconciler — which runs every `health_poll_sec`, 5 s by
// default — would add ~17 000 rows a day against a 500-row retention cap and
// bury the actual start history of a `restart_policy='never'` instance within an
// hour.
func (s *Store) HasInhibitedStartSince(ctx context.Context, tx Tx,
	instanceID string, reason model.InhibitReason, after int64) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM instance_starts
		  WHERE instance_id = ? AND outcome = 'inhibited' AND error_code = ? AND at > ?`,
		instanceID, string(reason), after).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("look for an inhibited start of %s: %w", instanceID, err)
	}
	return n > 0, nil
}

// InstancesWithOpenStarts returns the ids of instances holding an open ledger
// row. It is the second term of the supervisor's reconcile set (§3.10c): "every
// instance with `deleted_at IS NULL`, PLUS every instance with an open
// `instance_starts` row", which exists so a soft delete's own StopUnit is
// ledgered by the one actor allowed to close that row, and never again after.
func (s *Store) InstancesWithOpenStarts(ctx context.Context, tx Tx) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT instance_id FROM instance_starts WHERE outcome IS NULL ORDER BY instance_id`)
	if err != nil {
		return nil, fmt.Errorf("select instances with open starts: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan instance id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func scanInstanceStart(sc rowScanner) (model.InstanceStart, error) {
	var (
		r       model.InstanceStart
		trigger string
		outcome *string
	)
	if err := sc.Scan(&r.ID, &r.InstanceID, &r.At, &trigger, &r.ConfigHash,
		&r.EffectiveConfigHash, &r.OverrideJSON, &r.ArgvJSON, &r.LlamacppVersionID,
		&r.ReadyAt, &outcome, &r.ExitCode, &r.ErrorCode, &r.ErrorMessage,
		&r.DetailJSON, &r.EndedAt); err != nil {
		return model.InstanceStart{}, err
	}
	r.Trigger = model.StartTrigger(trigger)
	r.Outcome = enumPtr[model.StartOutcome](outcome)
	return r, nil
}
