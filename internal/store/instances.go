package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// instances queries (DESIGN section 2.8).
//
// Two schema facts shape every statement here, and neither is expressed in Go:
//
//   - Deletion is SOFT (D68). All three unique indexes are partial
//     (`WHERE deleted_at IS NULL`), so `name`, `public_port` and
//     `internal_port` are free the instant `deleted_at` is stamped. Every
//     listing therefore says which side of that line it wants, rather than
//     assuming.
//   - The status row is inserted by the instances service in the SAME
//     transaction as the config row, which is what lets every read use an INNER
//     join (§2.8's "the row's lifecycle"). InsertInstanceStatus is the only
//     non-supervisor INSERT into that table and it is not an exception to the
//     update rules — see internal/store/instancestatus.go.

const instanceColumns = `id, name, display_name, description,
	model_id, mmproj_model_id, draft_model_id,
	public_port, internal_port, auth_mode, autostart,
	restart_policy, restart_max, restart_window_sec,
	flags_json, extra_flags, config_hash, desired_state, draft_validation,
	pending_trigger, pending_override_json, unit_name, generation,
	created_at, updated_at, deleted_at`

// InsertInstance writes a new config row. The caller supplies the id (a ULID
// from NewID) and MUST insert the `instance_status` row in the same transaction
// — §2.8 makes that pairing the reason every reader may assume the status row
// exists.
func (s *Store) InsertInstance(ctx context.Context, tx Tx, i model.Instance) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO instances (`+instanceColumns+`)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		i.ID, i.Name, i.DisplayName, i.Description,
		i.ModelID, i.MmprojModelID, i.DraftModelID,
		i.PublicPort, i.InternalPort, string(i.AuthMode), boolInt(i.Autostart),
		string(i.RestartPolicy), i.RestartMax, i.RestartWindowSec,
		i.FlagsJSON, i.ExtraFlags, i.ConfigHash, string(i.DesiredState), string(i.DraftValidation),
		enumArg(i.PendingTrigger), i.PendingOverrideJSON, i.UnitName, i.Generation,
		i.CreatedAt, i.UpdatedAt, i.DeletedAt)
	if err != nil {
		return fmt.Errorf("insert instance %s: %w", i.ID, err)
	}
	return nil
}

// Instance returns one row by id, deleted or not. A soft-deleted instance is
// still readable — its start history and usage are a product feature (§3.10c) —
// so the caller decides what to do about `deleted_at`, and `instance-exec` in
// particular exits 64 on it (§5.6 step 2).
func (s *Store) Instance(ctx context.Context, tx Tx, id string) (model.Instance, error) {
	row := tx.QueryRowContext(ctx, `SELECT `+instanceColumns+` FROM instances WHERE id = ?`, id)
	i, err := scanInstance(row)
	if err != nil {
		return model.Instance{}, notFound(err)
	}
	return i, nil
}

// InstanceByName returns the one LIVE instance with this name — what
// `instance-exec %i` resolves and what the name-uniqueness check reads. A
// soft-deleted instance does not hold its name, so it is not found here.
func (s *Store) InstanceByName(ctx context.Context, tx Tx, name string) (model.Instance, error) {
	row := tx.QueryRowContext(ctx,
		`SELECT `+instanceColumns+` FROM instances WHERE name = ? AND deleted_at IS NULL`, name)
	i, err := scanInstance(row)
	if err != nil {
		return model.Instance{}, notFound(err)
	}
	return i, nil
}

// InstanceFilter selects rows for `GET /api/v1/instances` (§3.10).
type InstanceFilter struct {
	// IncludeDeleted is `?include_deleted=true`. Soft-deleted instances are
	// excluded by default, which is what makes D68's "it is not in
	// GET /instances" literal.
	IncludeDeleted bool
	// IDs, when non-empty, restricts to these instances — how
	// RecomputeConfigHash reads back the rows it is about to re-render.
	IDs []string
	// Limit caps the result; zero means DefaultInstanceLimit.
	Limit int
}

// DefaultInstanceLimit bounds an instance listing. A host with more than 255
// instances cannot exist — `FileDescriptorStoreMax=256` covers the management
// listener plus 255 gateway listeners (D58) — so this is a guard rather than a
// pagination scheme.
const DefaultInstanceLimit = 500

// Instances lists config rows, oldest first, which is creation order because
// ids are ULIDs.
func (s *Store) Instances(ctx context.Context, tx Tx, f InstanceFilter) ([]model.Instance, error) {
	q := `SELECT ` + instanceColumns + ` FROM instances`
	where, args := instanceWhere(f)
	if where != "" {
		q += " WHERE " + where
	}
	q += " ORDER BY id LIMIT ?"
	args = append(args, instanceLimit(f))

	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("select instances: %w", err)
	}
	defer rows.Close()

	var out []model.Instance
	for rows.Next() {
		i, err := scanInstance(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// The joined projection `GET /instances` returns: config ⋈ status, plus the two
// `instance_starts` facts the four derived flags read (§2.8).
//
// Both start facts are correlated subqueries rather than joins, and each reads
// an index the schema declares for exactly this: `idx_instance_starts` for
// LAST_CLOSED and `idx_instance_starts_open` for THE_OPEN_ROW. Computing them
// per row in SQL rather than per instance in Go is what keeps a list request one
// query instead of 1+2N.
const instanceViewColumns = `i.id, i.name, i.display_name, i.description,
	i.model_id, i.mmproj_model_id, i.draft_model_id,
	i.public_port, i.internal_port, i.auth_mode, i.autostart,
	i.restart_policy, i.restart_max, i.restart_window_sec,
	i.flags_json, i.extra_flags, i.config_hash, i.desired_state, i.draft_validation,
	i.pending_trigger, i.pending_override_json, i.unit_name, i.generation,
	i.created_at, i.updated_at, i.deleted_at,
	` + statusColumns + `,
	(SELECT st.outcome FROM instance_starts st
	  WHERE st.instance_id = i.id AND st.outcome IS NOT NULL AND st.outcome != 'inhibited'
	  ORDER BY st.at DESC, st.id DESC LIMIT 1) AS last_closed_outcome,
	(SELECT st.override_json IS NOT NULL FROM instance_starts st
	  WHERE st.instance_id = i.id AND st.outcome IS NULL) AS open_override`

// InstanceViews lists the joined projection. The join is INNER on purpose: a
// missing status row is not a state this schema can reach, and a LEFT join
// would paper over the bug that produced one.
func (s *Store) InstanceViews(ctx context.Context, tx Tx, f InstanceFilter) ([]model.InstanceView, error) {
	q := `SELECT ` + instanceViewColumns + `
	        FROM instances i JOIN instance_status s ON s.instance_id = i.id`
	where, args := instanceWhere(f)
	if where != "" {
		q += " WHERE " + where
	}
	q += " ORDER BY i.id LIMIT ?"
	args = append(args, instanceLimit(f))

	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("select instance views: %w", err)
	}
	defer rows.Close()

	var out []model.InstanceView
	for rows.Next() {
		v, err := scanInstanceView(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// InstanceView returns the joined projection for one instance, deleted or not.
func (s *Store) InstanceView(ctx context.Context, tx Tx, id string) (model.InstanceView, error) {
	row := tx.QueryRowContext(ctx,
		`SELECT `+instanceViewColumns+`
		   FROM instances i JOIN instance_status s ON s.instance_id = i.id
		  WHERE i.id = ?`, id)
	v, err := scanInstanceView(row)
	if err != nil {
		return model.InstanceView{}, notFound(err)
	}
	return v, nil
}

// UpdateInstanceConfig is the API's own write: every user-editable column at
// once, guarded on `generation`.
//
// It reports whether the update matched. False means the generation moved or the
// row was deleted, and the handler answers `409 conflict_generation` — which
// keeps meaning "someone edited the configuration under you" precisely because
// none of §2.8's seven exceptional writers bumps the column.
//
// `autostart` and `desired_state` are deliberately NOT here. Autostart is
// `PUT /instances/{id}/autostart`, which only enables or disables the unit and
// never starts or stops anything (D53); desired_state is the start/stop
// endpoints'. Folding either into a config PATCH would make an edit to an
// unrelated field silently change what happens at the next boot.
func (s *Store) UpdateInstanceConfig(ctx context.Context, tx Tx, i model.Instance) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE instances SET
		   name = ?, display_name = ?, description = ?,
		   model_id = ?, mmproj_model_id = ?, draft_model_id = ?,
		   public_port = ?, internal_port = ?, auth_mode = ?,
		   restart_policy = ?, restart_max = ?, restart_window_sec = ?,
		   flags_json = ?, extra_flags = ?, config_hash = ?, draft_validation = ?,
		   unit_name = ?, generation = generation + 1, updated_at = ?
		 WHERE id = ? AND generation = ? AND deleted_at IS NULL`,
		i.Name, i.DisplayName, i.Description,
		i.ModelID, i.MmprojModelID, i.DraftModelID,
		i.PublicPort, i.InternalPort, string(i.AuthMode),
		string(i.RestartPolicy), i.RestartMax, i.RestartWindowSec,
		i.FlagsJSON, i.ExtraFlags, i.ConfigHash, string(i.DraftValidation),
		i.UnitName, i.UpdatedAt,
		i.ID, i.Generation)
	if err != nil {
		return false, fmt.Errorf("update instance %s: %w", i.ID, err)
	}
	return rowsChanged(res)
}

// SetInstanceDesiredState writes the DESIRED axis (§2.8). It is the API's on
// start/stop/restart, and the supervisor's once per host boot for the D53
// autostart coupling — the only automatic write to the column.
func (s *Store) SetInstanceDesiredState(ctx context.Context, tx Tx,
	id string, desired model.DesiredState, at int64) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE instances SET desired_state = ?, updated_at = ? WHERE id = ?`,
		string(desired), at, id)
	if err != nil {
		return false, fmt.Errorf("set desired_state of %s: %w", id, err)
	}
	return rowsChanged(res)
}

// StampPendingStart writes the hand-off channel: the trigger the daemon is about
// to start for, and — for a safe start only — the transient FlagSet patch
// (D61). Both are consumed and cleared by `instance-exec` in one transaction.
//
// Neither bumps `generation`: they are not configuration, and an admin's
// in-flight PATCH must not be rejected because someone pressed Start.
func (s *Store) StampPendingStart(ctx context.Context, tx Tx,
	id string, trigger model.PendingTrigger, overrideJSON *string, at int64) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE instances SET pending_trigger = ?, pending_override_json = ?, updated_at = ?
		  WHERE id = ?`,
		string(trigger), overrideJSON, at, id)
	if err != nil {
		return false, fmt.Errorf("stamp pending start on %s: %w", id, err)
	}
	return rowsChanged(res)
}

// TakePendingStart reads and clears both hand-off columns, returning what was
// there. It is `instance-exec`'s step-3 half of the trigger contract (§5.6): a
// start nobody stamped comes back as (nil, nil) and is honestly recorded as
// `external` rather than guessed.
//
// The caller must already hold a write transaction — clearing the override in
// the same transaction that consumes it is what makes safe start one-shot
// against a crash, a reboot or a supervisor restart.
func (s *Store) TakePendingStart(ctx context.Context, tx Tx, id string) (*model.PendingTrigger, *string, error) {
	var (
		trigger  *string
		override *string
	)
	err := tx.QueryRowContext(ctx,
		`SELECT pending_trigger, pending_override_json FROM instances WHERE id = ?`, id).
		Scan(&trigger, &override)
	if err != nil {
		return nil, nil, notFound(err)
	}
	if trigger == nil && override == nil {
		return nil, nil, nil
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE instances SET pending_trigger = NULL, pending_override_json = NULL WHERE id = ?`,
		id); err != nil {
		return nil, nil, fmt.Errorf("clear pending start on %s: %w", id, err)
	}
	return enumPtr[model.PendingTrigger](trigger), override, nil
}

// SetInstanceAutostart records that the unit is enabled or disabled. It is a
// statement about HOST BOOTS and nothing else: the endpoint behind it never
// starts or stops anything (§2.8's coupling rules).
func (s *Store) SetInstanceAutostart(ctx context.Context, tx Tx, id string, on bool, at int64) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE instances SET autostart = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL`,
		boolInt(on), at, id)
	if err != nil {
		return false, fmt.Errorf("set autostart of %s: %w", id, err)
	}
	return rowsChanged(res)
}

// SetInstanceDraftValidation is the models service's exceptional write (§3.10a):
// a deferred check resolved when the GGUF metadata finally landed. It does not
// bump `generation` — nobody edited a configuration.
func (s *Store) SetInstanceDraftValidation(ctx context.Context, tx Tx,
	id string, v model.DraftValidation, at int64) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE instances SET draft_validation = ?, updated_at = ? WHERE id = ?`,
		string(v), at, id)
	if err != nil {
		return false, fmt.Errorf("set draft_validation of %s: %w", id, err)
	}
	return rowsChanged(res)
}

// SetInstanceConfigHash is the ONLY writer of `config_hash` outside POST/PATCH
// (D69). Its two callers are llama.cpp activation — for every non-deleted
// instance, inside the activation transaction — and the models service, when a
// referenced model's resolved path moves.
//
// It touches neither `generation` (a user's open edit form must not be
// invalidated by a version flip they did not make) nor `applied_config_hash`
// (which is the entire point: leaving it is what makes `restart_required` light
// up for every running instance the moment a new build is activated).
func (s *Store) SetInstanceConfigHash(ctx context.Context, tx Tx, id, hash string, at int64) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE instances SET config_hash = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL`,
		hash, at, id)
	if err != nil {
		return false, fmt.Errorf("set config_hash of %s: %w", id, err)
	}
	return rowsChanged(res)
}

// ReassignInternalPort is F5's repair, and the first of §2.8's seven exceptions:
// after a start closed with exit 78 the supervisor allocates the next free port
// and writes it here. The port is invisible in `config_hash` (D52), so this
// bumps neither `generation` nor the hash and raises no `restart_required` badge
// for a change the user did not make.
func (s *Store) ReassignInternalPort(ctx context.Context, tx Tx, id string, port int, at int64) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE instances SET internal_port = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL`,
		port, at, id)
	if err != nil {
		return false, fmt.Errorf("reassign internal port of %s: %w", id, err)
	}
	return rowsChanged(res)
}

// SoftDeleteInstance stamps `deleted_at` (D68). Every row survives — the start
// ledger, both usage tables and the denial counters are accounting this design
// calls a product feature — and `name`, `public_port` and `internal_port` are
// free immediately, because all three unique indexes are partial.
//
// It deliberately does NOT close the open `instance_starts` row: that row is
// closed by the supervisor, from the stop the delete just requested, which is
// the one actor allowed to write the column (§3.10c).
func (s *Store) SoftDeleteInstance(ctx context.Context, tx Tx, id string, at int64) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE instances SET deleted_at = ?, desired_state = 'stopped', updated_at = ?
		  WHERE id = ? AND deleted_at IS NULL`,
		at, at, id)
	if err != nil {
		return false, fmt.Errorf("soft delete instance %s: %w", id, err)
	}
	return rowsChanged(res)
}

// PurgeInstance is `?purge=true`, the explicit hard delete. `instance_status`,
// `instance_starts`, `instance_usage_daily`, `token_usage_daily`,
// `gateway_denials_daily` and `token_instances` cascade with it — which is why
// the UI puts it behind a second confirmation naming the row counts about to be
// discarded. That history is the one thing in this system that cannot be
// recomputed.
func (s *Store) PurgeInstance(ctx context.Context, tx Tx, id string) (bool, error) {
	res, err := tx.ExecContext(ctx, `DELETE FROM instances WHERE id = ?`, id)
	if err != nil {
		return false, fmt.Errorf("purge instance %s: %w", id, err)
	}
	return rowsChanged(res)
}

// DeleteTokenInstances removes an instance's token scope rows. The default
// delete does this — a scope entry for an instance nobody can reach is noise —
// and `?keep_tokens=true` skips it (§3.10c step 3).
func (s *Store) DeleteTokenInstances(ctx context.Context, tx Tx, instanceID string) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM token_instances WHERE instance_id = ?`, instanceID); err != nil {
		return fmt.Errorf("delete token scopes of %s: %w", instanceID, err)
	}
	return nil
}

// InstancePortHolders returns every NON-DELETED instance's claim on both ports.
// It is the exclusion set for §2.8's port rules, for `GET /ports/suggest`, and
// for the management-port walk of §11.1 step 7 — which must consult this table
// rather than live socket state, because it runs before the gateway listeners
// open.
func (s *Store) InstancePortHolders(ctx context.Context, tx Tx) ([]model.InstancePorts, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, name, public_port, internal_port
		   FROM instances WHERE deleted_at IS NULL ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("select instance ports: %w", err)
	}
	defer rows.Close()

	var out []model.InstancePorts
	for rows.Next() {
		var p model.InstancePorts
		if err := rows.Scan(&p.InstanceID, &p.Name, &p.PublicPort, &p.InternalPort); err != nil {
			return nil, fmt.Errorf("scan instance ports: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func instanceWhere(f InstanceFilter) (string, []any) {
	var (
		where []string
		args  []any
	)
	// Both column names below are unambiguous in the joined query too:
	// `instance_status` carries neither `id` nor `deleted_at`.
	if !f.IncludeDeleted {
		where = append(where, "deleted_at IS NULL")
	}
	if len(f.IDs) > 0 {
		ph := make([]string, len(f.IDs))
		for i, id := range f.IDs {
			ph[i] = "?"
			args = append(args, id)
		}
		where = append(where, "id IN ("+strings.Join(ph, ",")+")")
	}
	return strings.Join(where, " AND "), args
}

func instanceLimit(f InstanceFilter) int {
	if f.Limit <= 0 {
		return DefaultInstanceLimit
	}
	return f.Limit
}

func scanInstance(sc rowScanner) (model.Instance, error) {
	var (
		i        model.Instance
		authMode string
		auto     int64
		policy   string
		desired  string
		draftVal string
		trigger  *string
	)
	if err := sc.Scan(&i.ID, &i.Name, &i.DisplayName, &i.Description,
		&i.ModelID, &i.MmprojModelID, &i.DraftModelID,
		&i.PublicPort, &i.InternalPort, &authMode, &auto,
		&policy, &i.RestartMax, &i.RestartWindowSec,
		&i.FlagsJSON, &i.ExtraFlags, &i.ConfigHash, &desired, &draftVal,
		&trigger, &i.PendingOverrideJSON, &i.UnitName, &i.Generation,
		&i.CreatedAt, &i.UpdatedAt, &i.DeletedAt); err != nil {
		return model.Instance{}, err
	}
	i.AuthMode = model.AuthMode(authMode)
	i.Autostart = auto != 0
	i.RestartPolicy = model.RestartPolicy(policy)
	i.DesiredState = model.DesiredState(desired)
	i.DraftValidation = model.DraftValidation(draftVal)
	i.PendingTrigger = enumPtr[model.PendingTrigger](trigger)
	return i, nil
}

func scanInstanceView(sc rowScanner) (model.InstanceView, error) {
	var (
		v        model.InstanceView
		authMode string
		auto     int64
		policy   string
		desired  string
		draftVal string
		trigger  *string

		state       string
		attribution string

		lastClosed   *string
		openOverride *int64
	)
	dest := []any{
		&v.ID, &v.Name, &v.DisplayName, &v.Description,
		&v.ModelID, &v.MmprojModelID, &v.DraftModelID,
		&v.PublicPort, &v.InternalPort, &authMode, &auto,
		&policy, &v.RestartMax, &v.RestartWindowSec,
		&v.FlagsJSON, &v.ExtraFlags, &v.ConfigHash, &desired, &draftVal,
		&trigger, &v.PendingOverrideJSON, &v.UnitName, &v.Generation,
		&v.CreatedAt, &v.UpdatedAt, &v.DeletedAt,
	}
	dest = append(dest, statusDest(&v.Status, &state, &attribution)...)
	dest = append(dest, &lastClosed, &openOverride)

	if err := sc.Scan(dest...); err != nil {
		return model.InstanceView{}, err
	}
	v.AuthMode = model.AuthMode(authMode)
	v.Autostart = auto != 0
	v.RestartPolicy = model.RestartPolicy(policy)
	v.DesiredState = model.DesiredState(desired)
	v.DraftValidation = model.DraftValidation(draftVal)
	v.PendingTrigger = enumPtr[model.PendingTrigger](trigger)
	v.Status.State = model.InstanceState(state)
	v.Status.GPUAttribution = model.GPUAttribution(attribution)
	v.LastClosedOutcome = enumPtr[model.StartOutcome](lastClosed)
	v.OpenOverride = boolPtr(openOverride)
	return v, nil
}
