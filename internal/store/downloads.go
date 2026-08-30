package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// `downloads` and `download_tasks` (DESIGN sections 2.7, 3.8, 7.3, 7.4).
//
// Three layers sit over one download and section 2.3a fixes their relationship
// so they cannot drift: exactly one `jobs` row (`subject_type='download'`)
// carries scheduling, `downloads.state` carries the domain state, and
// `download_tasks` carry per-file state and are folded upward. Every method here
// takes a Tx for that reason — the job row and the domain row are written by ONE
// transaction, and a query method that opened its own would make that promise
// untrue.
//
// The two ETag columns are the subtlety this file exists to keep straight, and
// section 7.4 is emphatic about it:
//
//   - `etag` is the BLOB NAME — `x-linked-etag`, de-quoted and `W/`-stripped,
//     equal to the sha256 hex for an LFS object. It names `blobs/<etag>` and is
//     NEVER sent in a header.
//   - `validator` is the HTTP VALIDATOR — the byte-exact `ETag` header of the
//     final response, quotes and any `W/` included, together with the host that
//     issued it. It is sent as `If-Range` and as nothing else.
//
// Conflating them is the bug section 7.4 spends a table preventing: sending the
// blob name as `If-Range` matches no validator any origin will ever compare it
// against, the server answers `200`, the partial is discarded, and resume
// silently never works while every stubbed test passes.

// Download is one row of `downloads` (section 2.7): one logical model's
// transfer, whose `state` is a stored fold of its tasks.
//
// The row struct lives here rather than in internal/model for the same reason
// LlamacppVersion does: it is a projection this package assembles, and nothing
// outside the download service and the API's DTO layer reads it.
type Download struct {
	ID      string
	ModelID string
	State   model.DownloadState
	// Priority orders the queue; lower runs first, matching `jobs.priority`.
	Priority int
	// IncludeMmproj records what the request asked for, so a retry expands to
	// the same file set the original did.
	IncludeMmproj bool

	BytesTotal int64
	BytesDone  int64
	// BytesAtStart is the offset the transfer resumed from. It exists so the ETA
	// is honest: a download that found 38 of 40 GB already on disk has not
	// achieved a 40 GB/s transfer rate, and an ETA computed from `bytes_done`
	// alone would say so.
	BytesAtStart int64
	SpeedBPS     int64
	ETASec       *int64

	Attempts     int
	ErrorCode    *string
	ErrorMessage *string

	CreatedAt  int64
	StartedAt  *int64
	FinishedAt *int64
}

// DownloadTask is one row of `download_tasks`: one FILE's transfer, which for a
// sharded model is one shard.
type DownloadTask struct {
	ID          string
	DownloadID  string
	ModelFileID string
	// URL is the huggingface.co resolve URL and never a signed CDN URL. A CDN
	// URL expires; storing one would make a download that resumes tomorrow fail
	// with a signature error instead of re-resolving.
	URL   string
	State model.DownloadTaskState

	BytesTotal int64
	BytesDone  int64

	// Etag is the BLOB NAME. See the file comment.
	Etag *string
	// Validator, ValidatorHost and LastModified are the `If-Range` inputs. See
	// the file comment and section 7.4's three-row table.
	Validator     *string
	ValidatorHost *string
	LastModified  *string

	IncompletePath *string

	Attempts  int
	LastError *string

	StartedAt  *int64
	FinishedAt *int64
}

// DownloadTaskView is a task joined to the `model_files` row it transfers, which
// is what `GET /api/v1/downloads/{id}`'s per-file progress needs: a user reads
// "shard 2 of 5" and a filename, not a `model_file_id`.
type DownloadTaskView struct {
	DownloadTask
	Filename   string
	ShardIndex int
	ShardTotal int
	// ModelID is the file's own model, which is NOT always the download's:
	// section 7.3 makes an mmproj a separate `models` row of `kind='mmproj'`,
	// downloaded under the same logical job.
	ModelID string
}

// DownloadFilter is the `?state=active|all` of `GET /api/v1/downloads`.
type DownloadFilter struct {
	// ActiveOnly restricts to the states a user would call "in flight". A
	// finished download stays in the table — it is the receipt for what landed
	// on this disk — so the default listing has to say which it wants.
	ActiveOnly bool
	// ModelID narrows to one model's downloads.
	ModelID string
	// Limit caps the result; zero means every row.
	Limit int
}

const downloadColumns = `id, model_id, state, priority, include_mmproj, bytes_total, bytes_done,
	bytes_at_start, speed_bps, eta_sec, attempts, error_code, error_message,
	created_at, started_at, finished_at`

const downloadTaskColumns = `id, download_id, model_file_id, url, state, bytes_total, bytes_done,
	etag, validator, validator_host, last_modified, incomplete_path, attempts, last_error,
	started_at, finished_at`

// InsertDownload writes a new download row. It is written in the SAME
// transaction as its `models` rows, its `download_tasks` and its `model_download`
// job (section 2.7), which is what makes `idx_jobs_one_live_per_subject` mean
// "one live job per download".
func (s *Store) InsertDownload(ctx context.Context, tx Tx, d Download) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO downloads (`+downloadColumns+`)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.ModelID, string(d.State), d.Priority, boolInt(d.IncludeMmproj),
		d.BytesTotal, d.BytesDone, d.BytesAtStart, d.SpeedBPS, d.ETASec,
		d.Attempts, d.ErrorCode, d.ErrorMessage, d.CreatedAt, d.StartedAt, d.FinishedAt)
	if err != nil {
		return fmt.Errorf("insert download: %w", err)
	}
	return nil
}

// Download returns one download by id.
func (s *Store) Download(ctx context.Context, tx Tx, id string) (Download, error) {
	row := tx.QueryRowContext(ctx, `SELECT `+downloadColumns+` FROM downloads WHERE id = ?`, id)
	d, err := scanDownload(row)
	return d, notFound(err)
}

// Downloads lists downloads newest first, honoring the filter.
func (s *Store) Downloads(ctx context.Context, tx Tx, f DownloadFilter) ([]Download, error) {
	q := `SELECT ` + downloadColumns + ` FROM downloads`
	var (
		where []string
		args  []any
	)
	if f.ActiveOnly {
		states := LiveDownloadStates()
		ph := make([]string, len(states))
		for i, st := range states {
			ph[i] = "?"
			args = append(args, string(st))
		}
		where = append(where, `state IN (`+strings.Join(ph, ", ")+`)`)
	}
	if f.ModelID != "" {
		where = append(where, `model_id = ?`)
		args = append(args, f.ModelID)
	}
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, " AND ")
	}
	// Priority first, then id — the same `(priority, created_at)` order section
	// 7.4 gives the worker pool, so the list a user reorders is the list the
	// pool will actually work through.
	q += ` ORDER BY priority ASC, id DESC`
	if f.Limit > 0 {
		q += ` LIMIT ?`
		args = append(args, f.Limit)
	}

	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("select downloads: %w", err)
	}
	defer rows.Close()

	var out []Download
	for rows.Next() {
		d, err := scanDownload(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// LiveDownloadStates are the states `?state=active` selects: everything that has
// not finished. `paused` is one of them — a paused download is in flight from
// the user's point of view, and it still holds its subject against
// `idx_jobs_one_live_per_subject`.
func LiveDownloadStates() []model.DownloadState {
	return []model.DownloadState{
		model.DownloadQueued, model.DownloadResolving, model.DownloadRunning,
		model.DownloadPaused, model.DownloadVerifying,
	}
}

// LiveDownloadForModel returns the unfinished download of a model, if one
// exists. It is the `409 download_exists` guard of section 3.8, and it is a
// query rather than a unique index because a model may legitimately be
// downloaded again after a previous attempt failed or was canceled.
func (s *Store) LiveDownloadForModel(ctx context.Context, tx Tx, modelID string) (Download, error) {
	states := LiveDownloadStates()
	ph := make([]string, len(states))
	args := make([]any, 0, len(states)+1)
	args = append(args, modelID)
	for i, st := range states {
		ph[i] = "?"
		args = append(args, string(st))
	}
	row := tx.QueryRowContext(ctx,
		`SELECT `+downloadColumns+` FROM downloads
		  WHERE model_id = ? AND state IN (`+strings.Join(ph, ", ")+`)
		  ORDER BY id DESC LIMIT 1`, args...)
	d, err := scanDownload(row)
	return d, notFound(err)
}

// SetDownloadState moves the domain row, in the job's own transaction (section
// 2.3a). startedAt and finishedAt are written only when non-nil, so a state move
// never clears a timestamp a previous move stamped; errorCode and errorMessage
// are written unconditionally, because a transition out of `failed` has to be
// able to CLEAR them.
func (s *Store) SetDownloadState(ctx context.Context, tx Tx, id string,
	state model.DownloadState, startedAt, finishedAt *int64, errorCode, errorMessage *string) (bool, error) {

	res, err := tx.ExecContext(ctx,
		`UPDATE downloads
		    SET state         = ?,
		        started_at    = COALESCE(?, started_at),
		        finished_at   = COALESCE(?, finished_at),
		        error_code    = ?,
		        error_message = ?
		  WHERE id = ?`,
		string(state), startedAt, finishedAt, errorCode, errorMessage, id)
	if err != nil {
		return false, fmt.Errorf("set download state: %w", err)
	}
	return rowsChanged(res)
}

// UpdateDownloadProgress writes the counters section 7.4 refreshes every two
// seconds. It touches nothing else, so a progress write can never move the state
// out from under the worker that owns it.
func (s *Store) UpdateDownloadProgress(ctx context.Context, tx Tx, d Download) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE downloads
		    SET bytes_total = ?, bytes_done = ?, bytes_at_start = ?, speed_bps = ?, eta_sec = ?
		  WHERE id = ?`,
		d.BytesTotal, d.BytesDone, d.BytesAtStart, d.SpeedBPS, d.ETASec, d.ID)
	if err != nil {
		return false, fmt.Errorf("update download progress: %w", err)
	}
	return rowsChanged(res)
}

// SetDownloadPriority is `PATCH /api/v1/downloads/{id} {"priority":10}` — the
// queue reorder. It moves the `downloads` row only; the caller moves the `jobs`
// row in the same transaction, because the pool leases on `jobs.priority` and a
// reorder that touched one of the two would be a reorder the worker never saw.
func (s *Store) SetDownloadPriority(ctx context.Context, tx Tx, id string, priority int) (bool, error) {
	res, err := tx.ExecContext(ctx, `UPDATE downloads SET priority = ? WHERE id = ?`, priority, id)
	if err != nil {
		return false, fmt.Errorf("set download priority: %w", err)
	}
	return rowsChanged(res)
}

// SetDownloadJobPriority moves the `jobs` row's priority alongside the
// `downloads` row's.
//
// It lives in this file rather than in jobs.go because the download reorder is
// its only caller and the pairing is what makes the reorder mean anything: the
// worker pool leases on `jobs.priority`, so a reorder that moved only
// `downloads.priority` would change the list the user reads without changing the
// order the pool works through — the most confusing possible outcome for a
// control whose entire purpose is order.
//
// It refuses to touch a job that has finished: reordering history is meaningless
// and would rewrite a row the scheduler is entitled to consider settled.
func (s *Store) SetDownloadJobPriority(ctx context.Context, tx Tx, jobID string, priority int) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE jobs SET priority = ? WHERE id = ? AND state IN `+liveStatesSQL, priority, jobID)
	if err != nil {
		return false, fmt.Errorf("set download job priority: %w", err)
	}
	return rowsChanged(res)
}

// BumpDownloadAttempts increments `attempts`. It is a statement of its own
// because the counter belongs to the download and not to the job: a retry of a
// failed download re-runs the same job row, and the two counters answer
// different questions.
func (s *Store) BumpDownloadAttempts(ctx context.Context, tx Tx, id string) (bool, error) {
	res, err := tx.ExecContext(ctx, `UPDATE downloads SET attempts = attempts + 1 WHERE id = ?`, id)
	if err != nil {
		return false, fmt.Errorf("bump download attempts: %w", err)
	}
	return rowsChanged(res)
}

// -----------------------------------------------------------------------------
// download_tasks
// -----------------------------------------------------------------------------

// InsertDownloadTask writes one file's task row.
func (s *Store) InsertDownloadTask(ctx context.Context, tx Tx, t DownloadTask) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO download_tasks (`+downloadTaskColumns+`)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.DownloadID, t.ModelFileID, t.URL, string(t.State), t.BytesTotal, t.BytesDone,
		t.Etag, t.Validator, t.ValidatorHost, t.LastModified, t.IncompletePath,
		t.Attempts, t.LastError, t.StartedAt, t.FinishedAt)
	if err != nil {
		return fmt.Errorf("insert download task: %w", err)
	}
	return nil
}

// DownloadTask returns one task by id.
func (s *Store) DownloadTask(ctx context.Context, tx Tx, id string) (DownloadTask, error) {
	row := tx.QueryRowContext(ctx,
		`SELECT `+downloadTaskColumns+` FROM download_tasks WHERE id = ?`, id)
	t, err := scanDownloadTask(row)
	return t, notFound(err)
}

// DownloadTasks lists one download's tasks in shard order, which is the order a
// user reads them in and the order section 7.3's progress panel shows.
func (s *Store) DownloadTasks(ctx context.Context, tx Tx, downloadID string) ([]DownloadTask, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT t.id, t.download_id, t.model_file_id, t.url, t.state, t.bytes_total, t.bytes_done,
		        t.etag, t.validator, t.validator_host, t.last_modified, t.incomplete_path,
		        t.attempts, t.last_error, t.started_at, t.finished_at
		   FROM download_tasks t
		   JOIN model_files f ON f.id = t.model_file_id
		  WHERE t.download_id = ?
		  ORDER BY f.shard_index ASC, f.filename ASC`, downloadID)
	if err != nil {
		return nil, fmt.Errorf("select download tasks: %w", err)
	}
	defer rows.Close()

	var out []DownloadTask
	for rows.Next() {
		t, err := scanDownloadTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DownloadTaskViews is DownloadTasks joined to `model_files`, for the per-file
// progress of `GET /api/v1/downloads/{id}`.
func (s *Store) DownloadTaskViews(ctx context.Context, tx Tx, downloadID string) ([]DownloadTaskView, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT t.id, t.download_id, t.model_file_id, t.url, t.state, t.bytes_total, t.bytes_done,
		        t.etag, t.validator, t.validator_host, t.last_modified, t.incomplete_path,
		        t.attempts, t.last_error, t.started_at, t.finished_at,
		        f.filename, f.shard_index, f.shard_total, f.model_id
		   FROM download_tasks t
		   JOIN model_files f ON f.id = t.model_file_id
		  WHERE t.download_id = ?
		  ORDER BY f.shard_index ASC, f.filename ASC`, downloadID)
	if err != nil {
		return nil, fmt.Errorf("select download task views: %w", err)
	}
	defer rows.Close()

	var out []DownloadTaskView
	for rows.Next() {
		var v DownloadTaskView
		var state string
		if err := rows.Scan(&v.ID, &v.DownloadID, &v.ModelFileID, &v.URL, &state,
			&v.BytesTotal, &v.BytesDone, &v.Etag, &v.Validator, &v.ValidatorHost,
			&v.LastModified, &v.IncompletePath, &v.Attempts, &v.LastError,
			&v.StartedAt, &v.FinishedAt,
			&v.Filename, &v.ShardIndex, &v.ShardTotal, &v.ModelID); err != nil {
			return nil, fmt.Errorf("scan download task view: %w", err)
		}
		v.State = model.DownloadTaskState(state)
		out = append(out, v)
	}
	return out, rows.Err()
}

// SetDownloadTaskState moves one task. Like SetDownloadState it COALESCEs the
// timestamps and overwrites `last_error`, so `waiting_for_lock` — which section
// 7.2a writes while the task is still `running` and healthy — can be cleared the
// moment the lock is taken.
func (s *Store) SetDownloadTaskState(ctx context.Context, tx Tx, id string,
	state model.DownloadTaskState, startedAt, finishedAt *int64, lastError *string) (bool, error) {

	res, err := tx.ExecContext(ctx,
		`UPDATE download_tasks
		    SET state       = ?,
		        started_at  = COALESCE(?, started_at),
		        finished_at = COALESCE(?, finished_at),
		        last_error  = ?
		  WHERE id = ?`,
		string(state), startedAt, finishedAt, lastError, id)
	if err != nil {
		return false, fmt.Errorf("set download task state: %w", err)
	}
	return rowsChanged(res)
}

// SetDownloadTasksState moves every non-terminal task of one download at once —
// the pause, the resume and the cancel, each of which is a statement about the
// whole file set rather than about one shard.
//
// Terminal tasks are deliberately excluded: a shard that already landed must not
// be moved back to `paused` by a pause of the four that are still running, or
// the resume would re-download bytes that are already verified on disk.
func (s *Store) SetDownloadTasksState(ctx context.Context, tx Tx, downloadID string,
	state model.DownloadTaskState, at *int64) (int64, error) {

	q := `UPDATE download_tasks
	         SET state = ?, finished_at = COALESCE(?, finished_at)
	       WHERE download_id = ? AND state NOT IN ('succeeded', 'canceled')`
	var finished any
	if state == model.TaskCanceled && at != nil {
		finished = *at
	}
	res, err := tx.ExecContext(ctx, q, string(state), finished, downloadID)
	if err != nil {
		return 0, fmt.Errorf("set download tasks state: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected: %w", err)
	}
	return n, nil
}

// UpdateDownloadTaskTransfer records everything one attempt learned about a
// file: the blob name, the HTTP validator and the host that issued it, the
// partial file's path, the true size and the bytes on disk.
//
// The two ETag columns are written by this ONE statement on purpose. They are
// learned from the same response and mean different things, and a design where
// two setters could write one without the other is a design where `validator`
// eventually describes a host `validator_host` does not name — at which point
// section 7.4's rule silently sends an `If-Range` the origin will refuse.
func (s *Store) UpdateDownloadTaskTransfer(ctx context.Context, tx Tx, t DownloadTask) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE download_tasks
		    SET bytes_total     = ?,
		        bytes_done      = ?,
		        etag            = ?,
		        validator       = ?,
		        validator_host  = ?,
		        last_modified   = ?,
		        incomplete_path = ?
		  WHERE id = ?`,
		t.BytesTotal, t.BytesDone, t.Etag, t.Validator, t.ValidatorHost,
		t.LastModified, t.IncompletePath, t.ID)
	if err != nil {
		return false, fmt.Errorf("update download task transfer: %w", err)
	}
	return rowsChanged(res)
}

// UpdateDownloadTaskProgress writes only the byte counter — the 2 s tick of
// section 7.4, which must not be able to disturb the validator columns.
func (s *Store) UpdateDownloadTaskProgress(ctx context.Context, tx Tx, id string, bytesDone int64) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE download_tasks SET bytes_done = ? WHERE id = ?`, bytesDone, id)
	if err != nil {
		return false, fmt.Errorf("update download task progress: %w", err)
	}
	return rowsChanged(res)
}

// ClearDownloadTaskValidator drops a validator the origin just proved useless —
// section 7.4's "a `200` means the server ignored the range or the file changed
// upstream, so the partial is discarded and the transfer restarts (and the stale
// `validator` is cleared)".
func (s *Store) ClearDownloadTaskValidator(ctx context.Context, tx Tx, id string) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE download_tasks
		    SET validator = NULL, validator_host = NULL, last_modified = NULL
		  WHERE id = ?`, id)
	if err != nil {
		return false, fmt.Errorf("clear download task validator: %w", err)
	}
	return rowsChanged(res)
}

// BumpDownloadTaskAttempts increments one task's retry counter.
func (s *Store) BumpDownloadTaskAttempts(ctx context.Context, tx Tx, id string) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE download_tasks SET attempts = attempts + 1 WHERE id = ?`, id)
	if err != nil {
		return false, fmt.Errorf("bump download task attempts: %w", err)
	}
	return rowsChanged(res)
}

// KnownIncompletePaths returns every `.incomplete` path any `download_tasks` row
// names, in any state.
//
// It is the startup sweep of section 7.4, and its correctness is entirely in the
// word "every": the sweep removes `.incomplete` files under OUR repo directories
// that no task row names, and leaves every other one alone, because it may
// belong to a concurrent `hf download`. A filter on state here — "only the live
// ones" — would make the sweep delete the partial of a paused download, which is
// precisely the file a pause exists to keep.
func (s *Store) KnownIncompletePaths(ctx context.Context, tx Tx) (map[string]struct{}, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT incomplete_path FROM download_tasks WHERE incomplete_path IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("select incomplete paths: %w", err)
	}
	defer rows.Close()

	out := map[string]struct{}{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("scan incomplete path: %w", err)
		}
		out[p] = struct{}{}
	}
	return out, rows.Err()
}

func scanDownload(sc rowScanner) (Download, error) {
	var (
		d             Download
		state         string
		includeMmproj int64
	)
	if err := sc.Scan(&d.ID, &d.ModelID, &state, &d.Priority, &includeMmproj,
		&d.BytesTotal, &d.BytesDone, &d.BytesAtStart, &d.SpeedBPS, &d.ETASec,
		&d.Attempts, &d.ErrorCode, &d.ErrorMessage,
		&d.CreatedAt, &d.StartedAt, &d.FinishedAt); err != nil {
		return Download{}, err
	}
	d.State = model.DownloadState(state)
	d.IncludeMmproj = includeMmproj != 0
	return d, nil
}

func scanDownloadTask(sc rowScanner) (DownloadTask, error) {
	var (
		t     DownloadTask
		state string
	)
	if err := sc.Scan(&t.ID, &t.DownloadID, &t.ModelFileID, &t.URL, &state,
		&t.BytesTotal, &t.BytesDone, &t.Etag, &t.Validator, &t.ValidatorHost,
		&t.LastModified, &t.IncompletePath, &t.Attempts, &t.LastError,
		&t.StartedAt, &t.FinishedAt); err != nil {
		return DownloadTask{}, err
	}
	t.State = model.DownloadTaskState(state)
	return t, nil
}
