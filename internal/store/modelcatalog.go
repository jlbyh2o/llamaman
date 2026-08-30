package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// `models` and `model_files` (DESIGN sections 2.6, 3.7, 7.2).
//
// The identity of a row is `UNIQUE(root_id, repo_id, revision, primary_file)`,
// and every part of it is load-bearing:
//
//   - root_id scopes identity BY ROOT, so the same repo found under two cache
//     roots is two rows. §7.2a says `models.root_id` is never rewritten by a
//     relocation — a model's row belongs to the root whose filesystem actually
//     holds it, forever.
//   - revision is the SNAPSHOT DIRECTORY NAME, never a ref. A repo directory
//     legitimately holds several snapshots at once, and taking the revision from
//     `refs/main` would collapse them all onto one identity.
//   - primary_file is what distinguishes two quants of one repo+revision: they
//     are separate logical models, separately deletable and separately
//     referenceable by an instance.

const localModelColumns = `id, root_id, repo_id, revision, ref_name, quant_label, kind, state,
	origin, snapshot_dir, primary_file, shard_count, total_bytes, bytes_on_disk,
	mmproj_model_id, mmproj_auto, gguf_parsed_at, arch, n_layer, n_ctx_train, n_embd,
	n_ff, n_head, n_head_kv_json, head_dim_k, head_dim_v, n_vocab, n_expert,
	n_expert_used, swa_window, swa_pattern, tokenizer_model, file_type, has_vision,
	metadata_json, tensor_summary_json, hf_gguf_json, card_fetched_at, last_verified_at,
	created_at, updated_at`

// ModelFilter is the `?state=&kind=&q=` of `GET /api/v1/models`.
type ModelFilter struct {
	// RootID narrows to one cache root; empty means every root.
	RootID string
	// States and Kinds are OR-ed within themselves and AND-ed with each other.
	// Empty means no restriction — NOT "no rows", which is the mistake a naive
	// IN-list builder makes.
	States []model.ModelState
	Kinds  []model.ModelKind
	// Query is a case-insensitive substring of the repo id or the primary file.
	Query string
	// IncludeDeleted admits rows in state `deleted`. They are excluded by
	// default: a deleted model's row survives (§7.2 — deleting a model never
	// issues a SQL DELETE) precisely so a retained instance keeps a readable
	// record, and that is a history view rather than a catalog listing.
	IncludeDeleted bool
	// Sort is `repo`, `size` or `recent`; empty means `repo`.
	Sort string
}

// LocalModels lists the catalog.
func (s *Store) LocalModels(ctx context.Context, tx Tx, f ModelFilter) ([]model.LocalModel, error) {
	var (
		where []string
		args  []any
	)
	if f.RootID != "" {
		where = append(where, "root_id = ?")
		args = append(args, f.RootID)
	}
	if len(f.States) > 0 {
		ph := make([]any, 0, len(f.States))
		for _, st := range f.States {
			ph = append(ph, string(st))
		}
		where = append(where, "state IN ("+placeholders(len(ph))+")")
		args = append(args, ph...)
	} else if !f.IncludeDeleted {
		where = append(where, "state <> ?")
		args = append(args, string(model.ModelDeleted))
	}
	if len(f.Kinds) > 0 {
		ph := make([]any, 0, len(f.Kinds))
		for _, k := range f.Kinds {
			ph = append(ph, string(k))
		}
		where = append(where, "kind IN ("+placeholders(len(ph))+")")
		args = append(args, ph...)
	}
	if f.Query != "" {
		// LIKE with an explicit ESCAPE so a repo id containing `_` — which is
		// LIKE's single-character wildcard — is searched for literally.
		where = append(where, "(repo_id LIKE ?1 ESCAPE '\\' OR primary_file LIKE ?1 ESCAPE '\\')")
		args = append(args, "%"+escapeLike(f.Query)+"%")
	}

	q := `SELECT ` + localModelColumns + ` FROM models`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	switch f.Sort {
	case "size":
		q += " ORDER BY total_bytes DESC, repo_id, primary_file"
	case "recent":
		q += " ORDER BY created_at DESC, repo_id, primary_file"
	default:
		q += " ORDER BY repo_id, revision, primary_file"
	}

	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("select models: %w", err)
	}
	defer rows.Close()

	var out []model.LocalModel
	for rows.Next() {
		m, err := scanLocalModel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// LocalModel returns one row by id.
func (s *Store) LocalModel(ctx context.Context, tx Tx, id string) (model.LocalModel, error) {
	row := tx.QueryRowContext(ctx, `SELECT `+localModelColumns+` FROM models WHERE id = ?`, id)
	m, err := scanLocalModel(row)
	return m, notFound(err)
}

// LocalModelByIdentity looks a row up by the four columns UNIQUE covers. It is
// what a scan calls before deciding between an insert and an update, and it is
// why a rescan of an unchanged cache writes nothing.
func (s *Store) LocalModelByIdentity(ctx context.Context, tx Tx,
	rootID, repoID, revision, primaryFile string) (model.LocalModel, error) {

	row := tx.QueryRowContext(ctx,
		`SELECT `+localModelColumns+` FROM models
		  WHERE root_id = ? AND repo_id = ? AND revision = ? AND primary_file = ?`,
		rootID, repoID, revision, primaryFile)
	m, err := scanLocalModel(row)
	return m, notFound(err)
}

// InsertLocalModel writes a new catalog row.
func (s *Store) InsertLocalModel(ctx context.Context, tx Tx, m model.LocalModel) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO models (`+localModelColumns+`)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		         ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		localModelArgs(m)...)
	if err != nil {
		return fmt.Errorf("insert model: %w", err)
	}
	return nil
}

// UpdateLocalModel rewrites every mutable column of an existing row.
//
// It deliberately does NOT touch `mmproj_model_id` or `mmproj_auto`: a manual
// pairing (§3.7's `pair-mmproj`, which sets `mmproj_auto=0`) must survive every
// subsequent rescan, and a rescan that rewrote the whole row would silently
// undo it. SetLocalModelMmproj is the only writer of those two columns.
func (s *Store) UpdateLocalModel(ctx context.Context, tx Tx, m model.LocalModel) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE models SET
		   ref_name = ?, quant_label = ?, kind = ?, state = ?, snapshot_dir = ?,
		   primary_file = ?, shard_count = ?, total_bytes = ?, bytes_on_disk = ?,
		   gguf_parsed_at = ?, arch = ?, n_layer = ?, n_ctx_train = ?, n_embd = ?,
		   n_ff = ?, n_head = ?, n_head_kv_json = ?, head_dim_k = ?, head_dim_v = ?,
		   n_vocab = ?, n_expert = ?, n_expert_used = ?, swa_window = ?, swa_pattern = ?,
		   tokenizer_model = ?, file_type = ?, has_vision = ?, metadata_json = ?,
		   tensor_summary_json = ?, last_verified_at = ?, updated_at = ?
		 WHERE id = ?`,
		m.RefName, m.QuantLabel, string(m.Kind), string(m.State), m.SnapshotDir,
		m.PrimaryFile, m.ShardCount, m.TotalBytes, m.BytesOnDisk,
		m.GGUFParsedAt, m.Arch, m.NLayer, m.NCtxTrain, m.NEmbd,
		m.NFF, m.NHead, m.NHeadKVJSON, m.HeadDimK, m.HeadDimV,
		m.NVocab, m.NExpert, m.NExpertUsed, m.SWAWindow, m.SWAPattern,
		m.TokenizerModel, m.FileType, boolInt(m.HasVision), m.MetadataJSON,
		m.TensorSummaryJSON, m.LastVerifiedAt, m.UpdatedAt, m.ID)
	if err != nil {
		return false, fmt.Errorf("update model: %w", err)
	}
	return rowsChanged(res)
}

// SetLocalModelState moves a row's state and bumps `updated_at`.
//
// The transition table is §2.6's and it is the SERVICE's to enforce (D42: no
// generic engine, each guard is its own code). What this statement guarantees is
// only that the value is a member of the CHECK constraint — the database
// refusing an illegal state is the backstop, not the rule.
func (s *Store) SetLocalModelState(ctx context.Context, tx Tx, id string,
	state model.ModelState, at int64) (bool, error) {

	res, err := tx.ExecContext(ctx,
		`UPDATE models SET state = ?, updated_at = ? WHERE id = ?`, string(state), at, id)
	if err != nil {
		return false, fmt.Errorf("set model state: %w", err)
	}
	return rowsChanged(res)
}

// SetLocalModelMmproj is the only writer of the pairing columns. `auto=false`
// records a human's choice, which a later scan must not overrule (§7.2).
func (s *Store) SetLocalModelMmproj(ctx context.Context, tx Tx, id string,
	mmprojID *string, auto bool, at int64) (bool, error) {

	res, err := tx.ExecContext(ctx,
		`UPDATE models SET mmproj_model_id = ?, mmproj_auto = ?, updated_at = ? WHERE id = ?`,
		mmprojID, boolInt(auto), at, id)
	if err != nil {
		return false, fmt.Errorf("set model mmproj: %w", err)
	}
	return rowsChanged(res)
}

// StampLocalModelCard records when the model card was last fetched. Cards are
// fetched LAZILY when a model is opened (§7.2), so a 300-model scan makes no
// network calls and works offline; this column is what makes "lazily" mean
// "once".
func (s *Store) StampLocalModelCard(ctx context.Context, tx Tx, id string, at int64) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE models SET card_fetched_at = ?, updated_at = ? WHERE id = ?`, at, at, id)
	if err != nil {
		return false, fmt.Errorf("stamp model card: %w", err)
	}
	return rowsChanged(res)
}

// ModelInstanceRefs lists the instances that reference a model through any of
// the three columns.
//
// includeDeleted is the caller's decision and the two callers differ (§7.2a):
// a MODEL delete passes false, because it never issues a SQL DELETE and a
// soft-deleted instance keeping a readable record of what it pointed at is the
// intended behavior; a ROOT detach passes true, because it does issue one and
// `ON DELETE RESTRICT` does not care that a row is soft-deleted.
func (s *Store) ModelInstanceRefs(ctx context.Context, tx Tx, modelID string,
	includeDeleted bool) ([]model.InstanceRef, error) {

	q := `SELECT i.id, i.name, i.deleted_at,
	             CASE WHEN i.model_id = ?1 THEN 'model'
	                  WHEN i.mmproj_model_id = ?1 THEN 'mmproj'
	                  ELSE 'draft' END AS role
	        FROM instances i
	       WHERE (i.model_id = ?1 OR i.mmproj_model_id = ?1 OR i.draft_model_id = ?1)`
	if !includeDeleted {
		q += ` AND i.deleted_at IS NULL`
	}
	q += ` ORDER BY i.name`
	return s.instanceRefs(ctx, tx, q, modelID)
}

// ModelUsers maps model id → the non-deleted instances referencing it. It backs
// the `in_use_by:[instance ids]` column of `GET /api/v1/models` in one query
// rather than one per row.
func (s *Store) ModelUsers(ctx context.Context, tx Tx) (map[string][]model.InstanceRef, error) {
	// One SELECT per referencing column, unioned. An instance may reference the
	// same model in two roles — its own weights and its draft — and the union
	// reports both, which a single OR-ed join could not: a row cannot say
	// "model AND draft" in one `role` column, and picking one would hide the
	// other from the guard's message.
	rows, err := tx.QueryContext(ctx,
		`SELECT model_id AS m, id, name, deleted_at, 'model'  AS role FROM instances
		   WHERE deleted_at IS NULL AND model_id IS NOT NULL
		 UNION ALL
		 SELECT mmproj_model_id, id, name, deleted_at, 'mmproj' FROM instances
		   WHERE deleted_at IS NULL AND mmproj_model_id IS NOT NULL
		 UNION ALL
		 SELECT draft_model_id, id, name, deleted_at, 'draft'  FROM instances
		   WHERE deleted_at IS NULL AND draft_model_id IS NOT NULL
		 ORDER BY m, name, role`)
	if err != nil {
		return nil, fmt.Errorf("select model users: %w", err)
	}
	defer rows.Close()

	out := map[string][]model.InstanceRef{}
	for rows.Next() {
		var (
			modelID string
			ref     model.InstanceRef
		)
		if err := rows.Scan(&modelID, &ref.ID, &ref.Name, &ref.DeletedAt, &ref.Role); err != nil {
			return nil, fmt.Errorf("scan model user: %w", err)
		}
		out[modelID] = append(out[modelID], ref)
	}
	return out, rows.Err()
}

// InstancesForModels lists the non-deleted instance ids that reference any of
// the given models, deduplicated. It is D69's input: the models service calls
// `instances.RecomputeConfigHash` for exactly these, in the same transaction
// that moved `snapshot_dir`, `primary_file` or `state`.
func (s *Store) InstancesForModels(ctx context.Context, tx Tx, modelIDs []string) ([]string, error) {
	args := distinctArgs(modelIDs)
	if len(args) == 0 {
		return nil, nil
	}
	ph := placeholders(len(args))
	rows, err := tx.QueryContext(ctx,
		`SELECT DISTINCT id FROM instances
		  WHERE deleted_at IS NULL
		    AND (model_id IN (`+ph+`) OR mmproj_model_id IN (`+ph+`) OR draft_model_id IN (`+ph+`))
		  ORDER BY id`,
		append(append(append([]any{}, args...), args...), args...)...)
	if err != nil {
		return nil, fmt.Errorf("select instances for models: %w", err)
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

// RootUsage is one root's share of the catalog, for `GET /api/v1/system/disk`.
type RootUsage struct {
	RootID string
	Path   string
	// Models counts rows that are not `deleted`.
	Models int64
	// TotalBytes is the logical size; BytesOnDisk is what those files occupy.
	// They differ, and §3.7's disk view shows the second — "will free" is an
	// allocation question.
	TotalBytes  int64
	BytesOnDisk int64
	// StrayBytes is what `stray_files` under this root accounts for, which is
	// the number that explains a disk fuller than the model total says.
	StrayBytes int64
}

// ModelDiskUsage aggregates the catalog per root. Every registered root appears,
// including one holding nothing, because "this disk has no models on it" is an
// answer the storage view has to be able to give.
func (s *Store) ModelDiskUsage(ctx context.Context, tx Tx) ([]RootUsage, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT r.id, r.path,
		        (SELECT COUNT(*)                  FROM models m WHERE m.root_id = r.id AND m.state <> 'deleted'),
		        (SELECT COALESCE(SUM(total_bytes),0)   FROM models m WHERE m.root_id = r.id AND m.state <> 'deleted'),
		        (SELECT COALESCE(SUM(bytes_on_disk),0) FROM models m WHERE m.root_id = r.id AND m.state <> 'deleted'),
		        (SELECT COALESCE(SUM(size_bytes),0)    FROM stray_files s WHERE s.root_id = r.id)
		   FROM hf_cache_roots r
		  ORDER BY r.is_primary DESC, r.path`)
	if err != nil {
		return nil, fmt.Errorf("select model disk usage: %w", err)
	}
	defer rows.Close()

	var out []RootUsage
	for rows.Next() {
		var u RootUsage
		if err := rows.Scan(&u.RootID, &u.Path, &u.Models, &u.TotalBytes,
			&u.BytesOnDisk, &u.StrayBytes); err != nil {
			return nil, fmt.Errorf("scan root usage: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// MmprojCandidates lists the `kind='mmproj'` rows of one repo+revision under one
// root — the candidate set §7.2's auto-pairing rule chooses from, and the picker
// the UI shows when the rule declines to guess.
func (s *Store) MmprojCandidates(ctx context.Context, tx Tx,
	rootID, repoID, revision string) ([]model.LocalModel, error) {

	rows, err := tx.QueryContext(ctx,
		`SELECT `+localModelColumns+` FROM models
		  WHERE root_id = ? AND repo_id = ? AND revision = ? AND kind = 'mmproj'
		    AND state <> 'deleted'
		  ORDER BY primary_file`, rootID, repoID, revision)
	if err != nil {
		return nil, fmt.Errorf("select mmproj candidates: %w", err)
	}
	defer rows.Close()

	var out []model.LocalModel
	for rows.Next() {
		m, err := scanLocalModel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// -----------------------------------------------------------------------------
// model_files
// -----------------------------------------------------------------------------

const modelFileColumns = `id, model_id, filename, shard_index, shard_total, size_bytes,
	etag, blob_path, link_path, bytes_on_disk, state, checksum_verified, created_at, updated_at`

// ModelFiles lists a model's files in shard order.
func (s *Store) ModelFiles(ctx context.Context, tx Tx, modelID string) ([]model.ModelFile, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT `+modelFileColumns+` FROM model_files WHERE model_id = ?
		  ORDER BY shard_index, filename`, modelID)
	if err != nil {
		return nil, fmt.Errorf("select model files: %w", err)
	}
	defer rows.Close()

	var out []model.ModelFile
	for rows.Next() {
		var (
			f        model.ModelFile
			state    string
			verified int64
		)
		if err := rows.Scan(&f.ID, &f.ModelID, &f.Filename, &f.ShardIndex, &f.ShardTotal,
			&f.SizeBytes, &f.Etag, &f.BlobPath, &f.LinkPath, &f.BytesOnDisk,
			&state, &verified, &f.CreatedAt, &f.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan model file: %w", err)
		}
		f.State = model.ModelFileState(state)
		f.ChecksumVerified = verified != 0
		out = append(out, f)
	}
	return out, rows.Err()
}

// UpsertModelFile writes a file row, keyed by `UNIQUE(model_id, filename)`.
//
// The conflict clause preserves `created_at` and `id`: a rescan of an unchanged
// file must not mint a new row id, because `download_tasks.model_file_id`
// references it and a resumed download would lose its task.
func (s *Store) UpsertModelFile(ctx context.Context, tx Tx, f model.ModelFile) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO model_files (`+modelFileColumns+`)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(model_id, filename) DO UPDATE SET
		   shard_index = excluded.shard_index,
		   shard_total = excluded.shard_total,
		   size_bytes = excluded.size_bytes,
		   etag = excluded.etag,
		   blob_path = excluded.blob_path,
		   link_path = excluded.link_path,
		   bytes_on_disk = excluded.bytes_on_disk,
		   state = excluded.state,
		   checksum_verified = excluded.checksum_verified,
		   updated_at = excluded.updated_at`,
		f.ID, f.ModelID, f.Filename, f.ShardIndex, f.ShardTotal, f.SizeBytes,
		f.Etag, f.BlobPath, f.LinkPath, f.BytesOnDisk, string(f.State),
		boolInt(f.ChecksumVerified), f.CreatedAt, f.UpdatedAt)
	if err != nil {
		return fmt.Errorf("upsert model file: %w", err)
	}
	return nil
}

// SetModelFileState moves one file row.
func (s *Store) SetModelFileState(ctx context.Context, tx Tx, id string,
	state model.ModelFileState, at int64) (bool, error) {

	res, err := tx.ExecContext(ctx,
		`UPDATE model_files SET state = ?, updated_at = ? WHERE id = ?`, string(state), at, id)
	if err != nil {
		return false, fmt.Errorf("set model file state: %w", err)
	}
	return rowsChanged(res)
}

// DeleteModelFilesNotIn removes the file rows of a model whose names a rescan no
// longer found. It is scoped by model id and by name, so a shard set that lost a
// member loses that row and keeps the rest — which is what makes the model
// `incomplete` rather than silently smaller.
//
// An empty `keep` deletes every file of the model, which is what a scan that
// found the snapshot directory empty means.
func (s *Store) DeleteModelFilesNotIn(ctx context.Context, tx Tx, modelID string, keep []string) (int64, error) {
	q := `DELETE FROM model_files WHERE model_id = ?`
	args := []any{modelID}
	if len(keep) > 0 {
		names := distinctArgs(keep)
		q += ` AND filename NOT IN (` + placeholders(len(names)) + `)`
		args = append(args, names...)
	}
	res, err := tx.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, fmt.Errorf("delete stale model files: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected: %w", err)
	}
	return n, nil
}

func scanLocalModel(sc scanner) (model.LocalModel, error) {
	var (
		m          model.LocalModel
		kind       string
		state      string
		origin     string
		mmprojAuto int64
		hasVision  int64
	)
	if err := sc.Scan(
		&m.ID, &m.RootID, &m.RepoID, &m.Revision, &m.RefName, &m.QuantLabel, &kind, &state,
		&origin, &m.SnapshotDir, &m.PrimaryFile, &m.ShardCount, &m.TotalBytes, &m.BytesOnDisk,
		&m.MmprojModelID, &mmprojAuto, &m.GGUFParsedAt, &m.Arch, &m.NLayer, &m.NCtxTrain, &m.NEmbd,
		&m.NFF, &m.NHead, &m.NHeadKVJSON, &m.HeadDimK, &m.HeadDimV, &m.NVocab, &m.NExpert,
		&m.NExpertUsed, &m.SWAWindow, &m.SWAPattern, &m.TokenizerModel, &m.FileType, &hasVision,
		&m.MetadataJSON, &m.TensorSummaryJSON, &m.HFGGUFJSON, &m.CardFetchedAt, &m.LastVerifiedAt,
		&m.CreatedAt, &m.UpdatedAt); err != nil {
		return model.LocalModel{}, err
	}
	m.Kind = model.ModelKind(kind)
	m.State = model.ModelState(state)
	m.Origin = model.ModelOrigin(origin)
	m.MmprojAuto = mmprojAuto != 0
	m.HasVision = hasVision != 0
	return m, nil
}

func localModelArgs(m model.LocalModel) []any {
	return []any{
		m.ID, m.RootID, m.RepoID, m.Revision, m.RefName, m.QuantLabel, string(m.Kind),
		string(m.State), string(m.Origin), m.SnapshotDir, m.PrimaryFile, m.ShardCount,
		m.TotalBytes, m.BytesOnDisk, m.MmprojModelID, boolInt(m.MmprojAuto), m.GGUFParsedAt,
		m.Arch, m.NLayer, m.NCtxTrain, m.NEmbd, m.NFF, m.NHead, m.NHeadKVJSON, m.HeadDimK,
		m.HeadDimV, m.NVocab, m.NExpert, m.NExpertUsed, m.SWAWindow, m.SWAPattern,
		m.TokenizerModel, m.FileType, boolInt(m.HasVision), m.MetadataJSON,
		m.TensorSummaryJSON, m.HFGGUFJSON, m.CardFetchedAt, m.LastVerifiedAt,
		m.CreatedAt, m.UpdatedAt,
	}
}

// escapeLike neutralizes LIKE's two wildcards so a search for `Qwen_3` finds
// that string rather than `Qwen13`. The escape character is `\`, declared by the
// ESCAPE clause at every call site.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}
