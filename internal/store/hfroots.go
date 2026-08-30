package store

import (
	"context"
	"fmt"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// `hf_cache_roots` (DESIGN sections 2.6, 7.2a).
//
// The table is the AUTHORITY for the cache location; `settings['hf.hub_dir']`,
// the derived `settings['hf.home']` and `runtime_info.hf_hub_dir`/`hf_home` are
// projections of its `is_primary=1` row. That is a service-level rule (§7.2a's
// `cache.SetPrimaryRoot` is the only writer of all four), so it is not enforced
// here — this file supplies the statements and nothing else, per §1's "methods
// take ctx + *Tx; no business logic".
//
// One thing IS enforced here, because only SQL can: `idx_cache_one_primary` is a
// partial unique index on `is_primary=1`, so promoting a root has to clear the
// old flag BEFORE setting the new one. SetPrimaryCacheRoot does the two
// statements in that order for that reason, and a caller that wrote them itself
// in the other order would get a constraint violation instead of a promotion.

const cacheRootColumns = `id, path, is_primary, writable, symlinks_ok, detected_from,
	fs_type, total_bytes, free_bytes, last_scan_at, created_at`

// CacheRoots returns every registered hub directory, primary first and then by
// path, which is the order §3.7's list endpoint renders.
func (s *Store) CacheRoots(ctx context.Context, tx Tx) ([]model.CacheRoot, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT `+cacheRootColumns+` FROM hf_cache_roots ORDER BY is_primary DESC, path`)
	if err != nil {
		return nil, fmt.Errorf("select cache roots: %w", err)
	}
	defer rows.Close()

	var out []model.CacheRoot
	for rows.Next() {
		r, err := scanCacheRoot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CacheRoot returns one root by id.
func (s *Store) CacheRoot(ctx context.Context, tx Tx, id string) (model.CacheRoot, error) {
	row := tx.QueryRowContext(ctx, `SELECT `+cacheRootColumns+` FROM hf_cache_roots WHERE id = ?`, id)
	r, err := scanCacheRoot(row)
	return r, notFound(err)
}

// CacheRootByPath returns the root registered for a hub directory. It is how
// §7.2a's SetPrimaryRoot decides between an upsert and a promotion: the path is
// UNIQUE, so a directory that is already known must be promoted rather than
// inserted a second time.
func (s *Store) CacheRootByPath(ctx context.Context, tx Tx, path string) (model.CacheRoot, error) {
	row := tx.QueryRowContext(ctx, `SELECT `+cacheRootColumns+` FROM hf_cache_roots WHERE path = ?`, path)
	r, err := scanCacheRoot(row)
	return r, notFound(err)
}

// PrimaryCacheRoot returns the `is_primary=1` row — the only root Llama Man ever
// writes to. ErrNotFound means no root has been registered yet, which is the
// state of a database whose first boot has not run the detection chain.
func (s *Store) PrimaryCacheRoot(ctx context.Context, tx Tx) (model.CacheRoot, error) {
	row := tx.QueryRowContext(ctx,
		`SELECT `+cacheRootColumns+` FROM hf_cache_roots WHERE is_primary = 1`)
	r, err := scanCacheRoot(row)
	return r, notFound(err)
}

// InsertCacheRoot writes a new root. A new root is NEVER primary (§3.7): it is
// scan-and-serve until something promotes it, so `is_primary` is taken from the
// value the caller set and SetPrimaryCacheRoot is the only path that raises it.
func (s *Store) InsertCacheRoot(ctx context.Context, tx Tx, r model.CacheRoot) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO hf_cache_roots
		   (id, path, is_primary, writable, symlinks_ok, detected_from,
		    fs_type, total_bytes, free_bytes, last_scan_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.Path, boolInt(r.IsPrimary), boolInt(r.Writable), boolInt(r.SymlinksOK),
		enumArg(r.DetectedFrom), r.FSType, r.TotalBytes, r.FreeBytes, r.LastScanAt, r.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert cache root: %w", err)
	}
	return nil
}

// UpdateCacheRootFacts refreshes what the filesystem answered: writability, the
// F17 symlink probe, the filesystem type and the space on it. These are measured
// at registration and re-measured on every scan, because all four change without
// anyone touching a row — a disk fills, a mount goes read-only, a directory is
// remounted from another filesystem entirely.
func (s *Store) UpdateCacheRootFacts(ctx context.Context, tx Tx, r model.CacheRoot) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE hf_cache_roots
		    SET writable = ?, symlinks_ok = ?, fs_type = ?, total_bytes = ?, free_bytes = ?
		  WHERE id = ?`,
		boolInt(r.Writable), boolInt(r.SymlinksOK), r.FSType, r.TotalBytes, r.FreeBytes, r.ID)
	if err != nil {
		return false, fmt.Errorf("update cache root facts: %w", err)
	}
	return rowsChanged(res)
}

// SetPrimaryCacheRoot moves the primary flag onto id.
//
// The two statements are in this order because `idx_cache_one_primary` is a
// partial UNIQUE index on `is_primary = 1`: setting the new flag first would
// collide with the old row inside the same transaction. Clearing first cannot
// leave the table without a primary, because both statements are the caller's
// one transaction.
func (s *Store) SetPrimaryCacheRoot(ctx context.Context, tx Tx, id string) (bool, error) {
	if _, err := tx.ExecContext(ctx,
		`UPDATE hf_cache_roots SET is_primary = 0 WHERE is_primary = 1 AND id <> ?`, id); err != nil {
		return false, fmt.Errorf("clear the previous primary cache root: %w", err)
	}
	res, err := tx.ExecContext(ctx, `UPDATE hf_cache_roots SET is_primary = 1 WHERE id = ?`, id)
	if err != nil {
		return false, fmt.Errorf("set the primary cache root: %w", err)
	}
	return rowsChanged(res)
}

// StampCacheRootScan records when a root was last walked.
func (s *Store) StampCacheRootScan(ctx context.Context, tx Tx, id string, at int64) (bool, error) {
	res, err := tx.ExecContext(ctx, `UPDATE hf_cache_roots SET last_scan_at = ? WHERE id = ?`, at, id)
	if err != nil {
		return false, fmt.Errorf("stamp the cache root scan: %w", err)
	}
	return rowsChanged(res)
}

// DeleteCacheRoot removes a root row. Its `models`, `model_files` and
// `stray_files` cascade away; NO FILE IS TOUCHED (§7.2a).
//
// This is THE ONE PATH in the design that issues a SQL DELETE against `models`,
// and `instances.model_id` is ON DELETE RESTRICT — so the caller must have run
// CacheRootInstanceRefs first and refused on any referencing instance, INCLUDING
// a soft-deleted one. Without that guard this statement fails inside the
// transaction with a raw foreign-key violation instead of the documented
// `409 model_in_use`.
func (s *Store) DeleteCacheRoot(ctx context.Context, tx Tx, id string) (bool, error) {
	res, err := tx.ExecContext(ctx, `DELETE FROM hf_cache_roots WHERE id = ?`, id)
	if err != nil {
		return false, fmt.Errorf("delete cache root: %w", err)
	}
	return rowsChanged(res)
}

// CacheRootInstanceRefs lists every instance that references any model of this
// root, through any of the three columns, INCLUDING soft-deleted instances.
//
// The soft-deleted rows are not a courtesy here; they are the whole of the guard
// (§7.2a). Under D68 a soft-deleted instance keeps its row and its `model_id`
// deliberately, so the history stays readable — and `ON DELETE RESTRICT` does
// not care that a row is soft-deleted.
func (s *Store) CacheRootInstanceRefs(ctx context.Context, tx Tx, rootID string) ([]model.InstanceRef, error) {
	return s.instanceRefs(ctx, tx,
		`SELECT i.id, i.name, i.deleted_at,
		        CASE WHEN i.model_id       IN (SELECT id FROM models WHERE root_id = ?1) THEN 'model'
		             WHEN i.mmproj_model_id IN (SELECT id FROM models WHERE root_id = ?1) THEN 'mmproj'
		             ELSE 'draft' END AS role
		   FROM instances i
		  WHERE i.model_id        IN (SELECT id FROM models WHERE root_id = ?1)
		     OR i.mmproj_model_id IN (SELECT id FROM models WHERE root_id = ?1)
		     OR i.draft_model_id  IN (SELECT id FROM models WHERE root_id = ?1)
		  ORDER BY i.name`, rootID)
}

func (s *Store) instanceRefs(ctx context.Context, tx Tx, query string, args ...any) ([]model.InstanceRef, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("select referencing instances: %w", err)
	}
	defer rows.Close()

	var out []model.InstanceRef
	for rows.Next() {
		var r model.InstanceRef
		if err := rows.Scan(&r.ID, &r.Name, &r.DeletedAt, &r.Role); err != nil {
			return nil, fmt.Errorf("scan referencing instance: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func scanCacheRoot(sc scanner) (model.CacheRoot, error) {
	var (
		r         model.CacheRoot
		primary   int64
		writable  int64
		symlinks  int64
		detected  *string
		fsType    *string
		totalB    *int64
		freeB     *int64
		lastScanA *int64
	)
	if err := sc.Scan(&r.ID, &r.Path, &primary, &writable, &symlinks, &detected,
		&fsType, &totalB, &freeB, &lastScanA, &r.CreatedAt); err != nil {
		return model.CacheRoot{}, err
	}
	r.IsPrimary = primary != 0
	r.Writable = writable != 0
	r.SymlinksOK = symlinks != 0
	r.DetectedFrom = enumPtr[model.CacheRootDetectedFrom](detected)
	r.FSType = fsType
	r.TotalBytes = totalB
	r.FreeBytes = freeB
	r.LastScanAt = lastScanA
	return r, nil
}

// scanner is what *sql.Row and *sql.Rows have in common, so one scan function
// serves both the single-row and the list query for an aggregate.
type scanner interface {
	Scan(dest ...any) error
}
