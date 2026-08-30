package store

import (
	"context"
	"fmt"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// instance_status queries (DESIGN section 2.8).
//
// The general rule is "the API writes config, the supervisor writes status", and
// this file is where it is kept honest. There are exactly two non-supervisor
// writers, both named by §2.8's second writer table:
//
//   - InsertInstanceStatus, called by the instances service in the SAME
//     transaction as the `instances` row. The table has three NOT NULL columns
//     and no default for two of them, so the row cannot spring into existence
//     lazily; creating it with the instance is what lets every reader use an
//     inner join.
//   - ClearCrashLoopLatch, called by `POST …/reset-failed` and
//     `POST …/safe-start`. It writes three columns and no others, because a
//     "Reset failed" that took effect on the NEXT supervisor pass would be a
//     button that changes a label and nothing else.
//
// `applied_config_hash` is emphatically not among them: it is stamped by the
// supervisor at the first /health 200 and nowhere else, so a launcher that
// reached execve and then died during model load never clears
// `restart_required` for a configuration that never ran.

const statusColumns = `s.instance_id, s.state, s.systemd_active, s.systemd_sub, s.systemd_result,
	s.main_pid, s.exe_version_id, s.applied_config_hash, s.ready_at, s.last_change_at,
	s.last_health_at, s.health_code, s.slots_total, s.slots_busy, s.ctx_size,
	s.requests_served, s.rss_bytes, s.vram_bytes, s.gpu_uuids_json, s.gpu_attribution,
	s.fit_report_json, s.last_exit_code, s.last_error, s.reconcile_backoff_until,
	s.restart_window_reset_at, s.device_map_json`

// statusInsertColumns is the same list without the table alias, for statements
// that name only this table.
const statusInsertColumns = `instance_id, state, systemd_active, systemd_sub, systemd_result,
	main_pid, exe_version_id, applied_config_hash, ready_at, last_change_at,
	last_health_at, health_code, slots_total, slots_busy, ctx_size,
	requests_served, rss_bytes, vram_bytes, gpu_uuids_json, gpu_attribution,
	fit_report_json, last_exit_code, last_error, reconcile_backoff_until,
	restart_window_reset_at, device_map_json`

// InsertInstanceStatus writes the observed-reality row for a new instance:
// `state='unknown'`, `last_change_at=now`, `restart_window_reset_at=0`,
// everything else NULL. It must be called in the transaction that inserts the
// `instances` row.
func (s *Store) InsertInstanceStatus(ctx context.Context, tx Tx, st model.InstanceStatus) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO instance_status (`+statusInsertColumns+`)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		statusArgs(st)...)
	if err != nil {
		return fmt.Errorf("insert instance_status %s: %w", st.InstanceID, err)
	}
	return nil
}

// InstanceStatus returns one status row, or ErrNotFound.
func (s *Store) InstanceStatus(ctx context.Context, tx Tx, instanceID string) (model.InstanceStatus, error) {
	var (
		st          model.InstanceStatus
		state       string
		attribution string
	)
	dest := statusDest(&st, &state, &attribution)
	err := tx.QueryRowContext(ctx,
		`SELECT `+statusColumns+` FROM instance_status s WHERE s.instance_id = ?`, instanceID).
		Scan(dest...)
	if err != nil {
		return model.InstanceStatus{}, notFound(err)
	}
	st.State = model.InstanceState(state)
	st.GPUAttribution = model.GPUAttribution(attribution)
	return st, nil
}

// UpdateInstanceStatus replaces every observed column at once. It is the
// SUPERVISOR's statement: the whole row is what one reconcile pass learns, and
// writing it in one UPDATE is what keeps a pass atomic against a reader.
func (s *Store) UpdateInstanceStatus(ctx context.Context, tx Tx, st model.InstanceStatus) (bool, error) {
	args := statusArgs(st)
	// Move instance_id from the front of the column list to the end, where the
	// WHERE clause needs it.
	args = append(args[1:], st.InstanceID)

	res, err := tx.ExecContext(ctx,
		`UPDATE instance_status SET
		   state = ?, systemd_active = ?, systemd_sub = ?, systemd_result = ?,
		   main_pid = ?, exe_version_id = ?, applied_config_hash = ?, ready_at = ?,
		   last_change_at = ?, last_health_at = ?, health_code = ?, slots_total = ?,
		   slots_busy = ?, ctx_size = ?, requests_served = ?, rss_bytes = ?, vram_bytes = ?,
		   gpu_uuids_json = ?, gpu_attribution = ?, fit_report_json = ?, last_exit_code = ?,
		   last_error = ?, reconcile_backoff_until = ?, restart_window_reset_at = ?,
		   device_map_json = ?
		 WHERE instance_id = ?`, args...)
	if err != nil {
		return false, fmt.Errorf("update instance_status %s: %w", st.InstanceID, err)
	}
	return rowsChanged(res)
}

// ClearCrashLoopLatch is the API's ONLY reach into `instance_status`, and it is
// exactly three columns (§2.8): `crash-looping → stopped`, the backoff cleared,
// and the crash-loop window restarted at now (D64).
//
// The state move is guarded on `crash-looping` because it is the only
// actual-state transition an API handler may write. An instance that is merely
// `failed` keeps its state and still gets the other two columns, which is what
// makes "Reset failed" and "Safe start" mean "try again NOW" rather than "wait
// out the five-minute backoff".
func (s *Store) ClearCrashLoopLatch(ctx context.Context, tx Tx, instanceID string, now int64) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE instance_status SET
		   state = CASE WHEN state = 'crash-looping' THEN 'stopped' ELSE state END,
		   last_change_at = CASE WHEN state = 'crash-looping' THEN ? ELSE last_change_at END,
		   reconcile_backoff_until = NULL,
		   restart_window_reset_at = ?
		 WHERE instance_id = ?`, now, now, instanceID)
	if err != nil {
		return false, fmt.Errorf("clear crash-loop latch on %s: %w", instanceID, err)
	}
	return rowsChanged(res)
}

// statusArgs renders a status row as driver arguments, in statusColumns order.
func statusArgs(st model.InstanceStatus) []any {
	return []any{
		st.InstanceID, string(st.State), st.SystemdActive, st.SystemdSub, st.SystemdResult,
		st.MainPID, st.ExeVersionID, st.AppliedConfigHash, st.ReadyAt, st.LastChangeAt,
		st.LastHealthAt, st.HealthCode, st.SlotsTotal, st.SlotsBusy, st.CtxSize,
		st.RequestsServed, st.RSSBytes, st.VRAMBytes, st.GPUUUIDsJSON, string(st.GPUAttribution),
		st.FitReportJSON, st.LastExitCode, st.LastError, st.ReconcileBackoffUntil,
		st.RestartWindowResetAt, st.DeviceMapJSON,
	}
}

// statusDest is the scan destination list, in statusColumns order. The two
// enum-valued columns are scanned into plain strings the caller converts, which
// is the same shape every other scan in this package uses.
func statusDest(st *model.InstanceStatus, state, attribution *string) []any {
	return []any{
		&st.InstanceID, state, &st.SystemdActive, &st.SystemdSub, &st.SystemdResult,
		&st.MainPID, &st.ExeVersionID, &st.AppliedConfigHash, &st.ReadyAt, &st.LastChangeAt,
		&st.LastHealthAt, &st.HealthCode, &st.SlotsTotal, &st.SlotsBusy, &st.CtxSize,
		&st.RequestsServed, &st.RSSBytes, &st.VRAMBytes, &st.GPUUUIDsJSON, attribution,
		&st.FitReportJSON, &st.LastExitCode, &st.LastError, &st.ReconcileBackoffUntil,
		&st.RestartWindowResetAt, &st.DeviceMapJSON,
	}
}
