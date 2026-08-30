package store

import (
	"context"
	"fmt"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// `cache_scans` and `stray_files` (DESIGN sections 2.6, 3.7, 7.2).
//
// A scan is a domain row paired with a `cache_scan` job (§2.3a), which is what
// makes it survive a daemon restart: the job is triaged back to `queued` at boot
// and the walk starts again, against counters this row already holds.

const cacheScanColumns = `id, root_id, state, trigger, dirs_seen, files_seen, models_found,
	models_added, models_missing, strays_found, bytes_total, error_message,
	started_at, finished_at, created_at`

// InsertCacheScan writes a new scan row. It is written in the SAME transaction
// as its `cache_scan` job (§2.3a), which is what makes
// `idx_jobs_one_live_per_subject` mean "one live scan per scan row".
func (s *Store) InsertCacheScan(ctx context.Context, tx Tx, c model.CacheScan) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO cache_scans (`+cacheScanColumns+`)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.RootID, string(c.State), string(c.Trigger), c.DirsSeen, c.FilesSeen,
		c.ModelsFound, c.ModelsAdded, c.ModelsMissing, c.StraysFound, c.BytesTotal,
		c.ErrorMessage, c.StartedAt, c.FinishedAt, c.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert cache scan: %w", err)
	}
	return nil
}

// CacheScan returns one scan by id — `GET /api/v1/cache/scans/{id}`.
func (s *Store) CacheScan(ctx context.Context, tx Tx, id string) (model.CacheScan, error) {
	row := tx.QueryRowContext(ctx, `SELECT `+cacheScanColumns+` FROM cache_scans WHERE id = ?`, id)
	c, err := scanCacheScan(row)
	return c, notFound(err)
}

// CacheScans lists recent scans, newest first. limit <= 0 means every row.
func (s *Store) CacheScans(ctx context.Context, tx Tx, rootID string, limit int) ([]model.CacheScan, error) {
	q := `SELECT ` + cacheScanColumns + ` FROM cache_scans`
	var args []any
	if rootID != "" {
		q += ` WHERE root_id = ?`
		args = append(args, rootID)
	}
	q += ` ORDER BY id DESC`
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("select cache scans: %w", err)
	}
	defer rows.Close()

	var out []model.CacheScan
	for rows.Next() {
		c, err := scanCacheScan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// UpdateCacheScanProgress writes the counters §7.2 refreshes every 250 ms. It
// touches nothing else, so a progress write can never move the state out from
// under the worker that owns it.
func (s *Store) UpdateCacheScanProgress(ctx context.Context, tx Tx, c model.CacheScan) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE cache_scans
		    SET dirs_seen = ?, files_seen = ?, models_found = ?, models_added = ?,
		        models_missing = ?, strays_found = ?, bytes_total = ?
		  WHERE id = ?`,
		c.DirsSeen, c.FilesSeen, c.ModelsFound, c.ModelsAdded, c.ModelsMissing,
		c.StraysFound, c.BytesTotal, c.ID)
	if err != nil {
		return false, fmt.Errorf("update cache scan progress: %w", err)
	}
	return rowsChanged(res)
}

// SetCacheScanState moves the domain row alongside its job row, in the job's own
// transaction (§2.3a). startedAt and finishedAt are written only when non-nil,
// so a state move never clears a timestamp a previous move stamped.
func (s *Store) SetCacheScanState(ctx context.Context, tx Tx, id string,
	state model.CacheScanState, startedAt, finishedAt *int64, errMessage *string) (bool, error) {

	res, err := tx.ExecContext(ctx,
		`UPDATE cache_scans
		    SET state = ?,
		        started_at    = COALESCE(?, started_at),
		        finished_at   = COALESCE(?, finished_at),
		        error_message = COALESCE(?, error_message)
		  WHERE id = ?`,
		string(state), startedAt, finishedAt, errMessage, id)
	if err != nil {
		return false, fmt.Errorf("set cache scan state: %w", err)
	}
	return rowsChanged(res)
}

// -----------------------------------------------------------------------------
// stray_files
// -----------------------------------------------------------------------------

const strayColumns = `id, root_id, path, size_bytes, reason, first_seen_at, last_seen_at, dismissed_at`

// StrayFiles lists the strays of a root (or of every root when rootID is
// empty). Dismissed rows are excluded unless includeDismissed: a dismissal is
// the user saying "I know, leave it alone", and re-listing it every scan would
// make the dismissal meaningless.
func (s *Store) StrayFiles(ctx context.Context, tx Tx, rootID string,
	includeDismissed bool) ([]model.StrayFile, error) {

	q := `SELECT ` + strayColumns + ` FROM stray_files`
	var (
		where []string
		args  []any
	)
	if rootID != "" {
		where = append(where, "root_id = ?")
		args = append(args, rootID)
	}
	if !includeDismissed {
		where = append(where, "dismissed_at IS NULL")
	}
	for i, w := range where {
		if i == 0 {
			q += " WHERE " + w
			continue
		}
		q += " AND " + w
	}
	q += " ORDER BY size_bytes DESC, path"

	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("select stray files: %w", err)
	}
	defer rows.Close()

	var out []model.StrayFile
	for rows.Next() {
		st, err := scanStray(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// StrayFile returns one stray by id.
func (s *Store) StrayFile(ctx context.Context, tx Tx, id string) (model.StrayFile, error) {
	row := tx.QueryRowContext(ctx, `SELECT `+strayColumns+` FROM stray_files WHERE id = ?`, id)
	st, err := scanStray(row)
	return st, notFound(err)
}

// UpsertStrayFile records a stray, keyed by its UNIQUE path.
//
// `first_seen_at` and `dismissed_at` survive the conflict deliberately: "this
// 40 GB blob has been orphaned since Tuesday" is the useful sentence, and a
// dismissal that a rescan undid would be a dismissal that never worked.
func (s *Store) UpsertStrayFile(ctx context.Context, tx Tx, st model.StrayFile) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO stray_files (`+strayColumns+`)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(path) DO UPDATE SET
		   root_id = excluded.root_id,
		   size_bytes = excluded.size_bytes,
		   reason = excluded.reason,
		   last_seen_at = excluded.last_seen_at`,
		st.ID, st.RootID, st.Path, st.SizeBytes, string(st.Reason),
		st.FirstSeenAt, st.LastSeenAt, st.DismissedAt)
	if err != nil {
		return fmt.Errorf("upsert stray file: %w", err)
	}
	return nil
}

// DeleteStrayFilesNotSeen removes the rows of a root whose files a scan no
// longer found — they were cleaned up by us, by another tool, or by the user.
// The row is the record of a problem, so it goes when the problem does.
func (s *Store) DeleteStrayFilesNotSeen(ctx context.Context, tx Tx, rootID string, before int64) (int64, error) {
	res, err := tx.ExecContext(ctx,
		`DELETE FROM stray_files WHERE root_id = ? AND last_seen_at < ?`, rootID, before)
	if err != nil {
		return 0, fmt.Errorf("delete unseen stray files: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected: %w", err)
	}
	return n, nil
}

// DeleteStrayFile removes one row. The FILE is the caller's to remove or not:
// `DELETE /cache/strays/{id}?delete_file=true` (§3.7) is two decisions, and this
// statement is only the row half.
func (s *Store) DeleteStrayFile(ctx context.Context, tx Tx, id string) (bool, error) {
	res, err := tx.ExecContext(ctx, `DELETE FROM stray_files WHERE id = ?`, id)
	if err != nil {
		return false, fmt.Errorf("delete stray file: %w", err)
	}
	return rowsChanged(res)
}

// DismissStrayFile stamps `dismissed_at`, which hides the row from the default
// listing without forgetting it.
func (s *Store) DismissStrayFile(ctx context.Context, tx Tx, id string, at int64) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE stray_files SET dismissed_at = ? WHERE id = ? AND dismissed_at IS NULL`, at, id)
	if err != nil {
		return false, fmt.Errorf("dismiss stray file: %w", err)
	}
	return rowsChanged(res)
}

func scanCacheScan(sc scanner) (model.CacheScan, error) {
	var (
		c       model.CacheScan
		state   string
		trigger string
	)
	if err := sc.Scan(&c.ID, &c.RootID, &state, &trigger, &c.DirsSeen, &c.FilesSeen,
		&c.ModelsFound, &c.ModelsAdded, &c.ModelsMissing, &c.StraysFound, &c.BytesTotal,
		&c.ErrorMessage, &c.StartedAt, &c.FinishedAt, &c.CreatedAt); err != nil {
		return model.CacheScan{}, err
	}
	c.State = model.CacheScanState(state)
	c.Trigger = model.CacheScanTrigger(trigger)
	return c, nil
}

func scanStray(sc scanner) (model.StrayFile, error) {
	var (
		st     model.StrayFile
		reason string
	)
	if err := sc.Scan(&st.ID, &st.RootID, &st.Path, &st.SizeBytes, &reason,
		&st.FirstSeenAt, &st.LastSeenAt, &st.DismissedAt); err != nil {
		return model.StrayFile{}, err
	}
	st.Reason = model.StrayReason(reason)
	return st, nil
}
