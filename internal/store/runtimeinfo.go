package store

import (
	"context"
	"fmt"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// runtime_info queries (DESIGN section 2.1).
//
// One row, `id = 1`, holding the facts about this daemon and this host that the
// CLI must be able to read without an HTTP call — `llamaman status` and `doctor`
// open the 0600 database directly, and that is the whole reason the table
// exists. It is written once at boot with everything the environment probe
// learned (§11.1 step 6), and thereafter only by the three narrow setters below.

const runtimeInfoColumns = `id, daemon_version, daemon_commit, pid, boot_id, boot_at,
	host_boot_id, host_boot_at, ui_bind_addr, ui_port, ui_port_flag, ui_url_hint,
	service_user, service_uid, service_group, service_gid,
	systemd_scope, systemd_control, journal_read, polkit_ok, polkit_detail,
	polkit_unit_files, listener_continuity, binary_path, hf_hub_dir, hf_home,
	state_dir, schema_version, last_heartbeat_at`

// RuntimeInfo returns the singleton row, or ErrNotFound before the first boot
// has written it.
func (s *Store) RuntimeInfo(ctx context.Context, tx Tx) (model.RuntimeInfo, error) {
	var (
		out             model.RuntimeInfo
		id              int64
		scope           *string
		control         *string
		journal         *string
		continuity      *string
		polkitOK        *int64
		polkitUnitFiles *int64
	)
	err := tx.QueryRowContext(ctx,
		`SELECT `+runtimeInfoColumns+` FROM runtime_info WHERE id = 1`).
		Scan(&id, &out.DaemonVersion, &out.DaemonCommit, &out.PID, &out.BootID, &out.BootAt,
			&out.HostBootID, &out.HostBootAt, &out.UIBindAddr, &out.UIPort, &out.UIPortFlag,
			&out.UIURLHint, &out.ServiceUser, &out.ServiceUID, &out.ServiceGroup, &out.ServiceGID,
			&scope, &control, &journal, &polkitOK, &out.PolkitDetail,
			&polkitUnitFiles, &continuity, &out.BinaryPath, &out.HFHubDir, &out.HFHome,
			&out.StateDir, &out.SchemaVersion, &out.LastHeartbeatAt)
	if err != nil {
		return model.RuntimeInfo{}, notFound(err)
	}
	out.SystemdScope = enumPtr[model.SystemdScope](scope)
	out.SystemdControl = enumPtr[model.SystemdControl](control)
	out.JournalRead = enumPtr[model.JournalRead](journal)
	out.ListenerContinuity = enumPtr[model.ListenerContinuity](continuity)
	out.PolkitOK = boolPtr(polkitOK)
	out.PolkitUnitFiles = boolPtr(polkitUnitFiles)
	return out, nil
}

// PutRuntimeInfo upserts the whole singleton. It is a whole-row write on purpose:
// the boot probe learns these facts together and a partial write would leave the
// row describing two different boots at once.
//
// Two columns it writes are governed by rules no signature can express, and both
// are stated here because this is the method that could break them:
//
//   - HostBootID / HostBootAt have EXACTLY ONE caller, supervisor boot
//     reconciliation step 1 (§5.8). §11.1 step 9 reads HostBootID, holds the
//     comparison in memory and writes NOTHING — persisting it there would make
//     the supervisor always find equality, the D53 autostart coupling would never
//     fire, and autostart would be broken in both directions. A boot-time caller
//     of this method must therefore carry the EXISTING values forward.
//   - HFHubDir / HFHome are a derived cache of the primary `hf_cache_roots` row,
//     never an input (§7.2a).
func (s *Store) PutRuntimeInfo(ctx context.Context, tx Tx, v model.RuntimeInfo) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO runtime_info (`+runtimeInfoColumns+`)
		 VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   daemon_version = excluded.daemon_version,
		   daemon_commit  = excluded.daemon_commit,
		   pid            = excluded.pid,
		   boot_id        = excluded.boot_id,
		   boot_at        = excluded.boot_at,
		   host_boot_id   = excluded.host_boot_id,
		   host_boot_at   = excluded.host_boot_at,
		   ui_bind_addr   = excluded.ui_bind_addr,
		   ui_port        = excluded.ui_port,
		   ui_port_flag   = excluded.ui_port_flag,
		   ui_url_hint    = excluded.ui_url_hint,
		   service_user   = excluded.service_user,
		   service_uid    = excluded.service_uid,
		   service_group  = excluded.service_group,
		   service_gid    = excluded.service_gid,
		   systemd_scope  = excluded.systemd_scope,
		   systemd_control = excluded.systemd_control,
		   journal_read   = excluded.journal_read,
		   polkit_ok      = excluded.polkit_ok,
		   polkit_detail  = excluded.polkit_detail,
		   polkit_unit_files   = excluded.polkit_unit_files,
		   listener_continuity = excluded.listener_continuity,
		   binary_path    = excluded.binary_path,
		   hf_hub_dir     = excluded.hf_hub_dir,
		   hf_home        = excluded.hf_home,
		   state_dir      = excluded.state_dir,
		   schema_version = excluded.schema_version,
		   last_heartbeat_at = excluded.last_heartbeat_at`,
		v.DaemonVersion, v.DaemonCommit, v.PID, v.BootID, v.BootAt,
		v.HostBootID, v.HostBootAt, v.UIBindAddr, v.UIPort, v.UIPortFlag, v.UIURLHint,
		v.ServiceUser, v.ServiceUID, v.ServiceGroup, v.ServiceGID,
		enumArg(v.SystemdScope), enumArg(v.SystemdControl), enumArg(v.JournalRead),
		boolArg(v.PolkitOK), v.PolkitDetail, boolArg(v.PolkitUnitFiles),
		enumArg(v.ListenerContinuity), v.BinaryPath, v.HFHubDir, v.HFHome,
		v.StateDir, v.SchemaVersion, v.LastHeartbeatAt)
	if err != nil {
		return fmt.Errorf("put runtime_info: %w", err)
	}
	return nil
}

// SetHostBoot stamps the host boot identity. THIS IS THE ONE WRITER of those two
// columns (D53, §5.8 boot reconciliation step 1), and it is deliberately a
// separate method from PutRuntimeInfo so that the boot sequence — which writes
// every other column at step 6, before the supervisor has decided anything —
// cannot touch them by accident.
func (s *Store) SetHostBoot(ctx context.Context, tx Tx, hostBootID string, hostBootAt int64) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE runtime_info SET host_boot_id = ?, host_boot_at = ? WHERE id = 1`,
		hostBootID, hostBootAt)
	if err != nil {
		return fmt.Errorf("set host boot: %w", err)
	}
	return nil
}

// SetListenerState records what §11.1 step 10 learns while it adopts the
// listeners systemd held across a restart: the schema version this boot ended up
// at, the port the walk actually landed on, and whether the sockets survived.
func (s *Store) SetListenerState(ctx context.Context, tx Tx,
	schemaVersion int, uiPort int, continuity model.ListenerContinuity) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE runtime_info
		    SET schema_version = ?, ui_port = ?, listener_continuity = ?
		  WHERE id = 1`,
		schemaVersion, uiPort, string(continuity))
	if err != nil {
		return fmt.Errorf("set listener state: %w", err)
	}
	return nil
}

// SetRuntimeHeartbeat stamps `last_heartbeat_at`, which is how `llamaman status`
// tells a live daemon from a stale row without a bus call.
func (s *Store) SetRuntimeHeartbeat(ctx context.Context, tx Tx, at int64) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE runtime_info SET last_heartbeat_at = ? WHERE id = 1`, at)
	if err != nil {
		return fmt.Errorf("set runtime heartbeat: %w", err)
	}
	return nil
}
