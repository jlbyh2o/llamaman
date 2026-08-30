package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// self_updates, the `llamaman` half of release_cache, and notifications
// (DESIGN sections 2.11, 12.1 and 12.3).
//
// Three things in this file are load-bearing rather than mechanical:
//
//   - CountLiveSelfUpdates is one of the four clauses D97 requires to be
//     evaluated INSIDE the single BEGIN IMMEDIATE transaction that inserts the
//     `self_updates` row and its job. It takes a Tx for exactly that reason:
//     `jobs.subject_id` for a self-update is a fresh id, so
//     `idx_jobs_one_live_per_subject` is silent and SQLite's writer serialization
//     is the whole mechanism.
//   - CloseOrphanedSelfUpdates is section 12.3's CLOSING PASS, and its guard is
//     the specification rather than an optimization: non-terminal row, paired job
//     `interrupted`, and not the id a surviving marker names. `interrupted` means
//     the lease belongs to a boot that is gone (§2.3), which is what lets the same
//     pass run in all three of the gate's callers rather than at boot alone — it
//     can never close work the calling process is itself performing.
//   - AppendNotification is how F20/F24 reach a human. The privileged actors
//     cannot do it — they must never open this database (§11.3) — so the daemon's
//     gate owns the row, and this is the one writer.

const selfUpdateColumns = `id, from_version, to_version, channel, state, asset_url, asset_sha256,
	signature_ok, db_backup_path, binary_path, error_message, created_at, finished_at`

// SelfUpdate is one `self_updates` row (§2.11).
type SelfUpdate struct {
	ID          string
	FromVersion string
	ToVersion   string
	// Channel is where the release came from. Section 12 has one source, so this
	// is `stable` for everything this design produces; the column exists because
	// the table was written beside `release_cache`, which has two.
	Channel string
	State   model.SelfUpdateState

	AssetURL    *string
	AssetSHA256 *string
	// SignatureOK is NULL until section 12.1 step 3 has run, 1 when the tarball
	// verified against a compiled-in key and 0 when it did not — which is a state
	// this pipeline never commits, because a signature failure aborts hard.
	SignatureOK *bool
	// DBBackupPath is the D14 snapshot taken before this update. Nothing restores
	// it automatically: it is step 3 of section 12.4's five-command procedure and
	// a human's decision (D90, D94).
	DBBackupPath *string
	// BinaryPath is the resolved `<prefix>/llamaman` being replaced (D15).
	BinaryPath *string
	// ErrorMessage carries the domain message; the PAIRED JOB carries the
	// error_code (§2.3a).
	ErrorMessage *string

	CreatedAt  int64
	FinishedAt *int64
}

// InsertSelfUpdate writes a new row. It is called from inside the same
// BEGIN IMMEDIATE transaction that inserts the job (D97, §12.1 step 1).
func (s *Store) InsertSelfUpdate(ctx context.Context, tx Tx, u SelfUpdate) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO self_updates (`+selfUpdateColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		u.ID, u.FromVersion, u.ToVersion, u.Channel, string(u.State),
		u.AssetURL, u.AssetSHA256, boolPtrToInt(u.SignatureOK), u.DBBackupPath, u.BinaryPath,
		u.ErrorMessage, u.CreatedAt, u.FinishedAt,
	)
	if err != nil {
		return fmt.Errorf("insert self_update %s: %w", u.ID, err)
	}
	return nil
}

// SelfUpdate reads one row.
func (s *Store) SelfUpdate(ctx context.Context, tx Tx, id string) (SelfUpdate, error) {
	row := tx.QueryRowContext(ctx,
		`SELECT `+selfUpdateColumns+` FROM self_updates WHERE id = ?`, id)
	u, err := scanSelfUpdate(row)
	if errors.Is(err, sql.ErrNoRows) {
		return SelfUpdate{}, fmt.Errorf("self_update %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return SelfUpdate{}, fmt.Errorf("read self_update %s: %w", id, err)
	}
	return u, nil
}

// LiveSelfUpdate returns the one non-terminal row, if there is one. It is what
// `GET /api/v1/update/status` renders as "the in-flight row" (§3.14).
func (s *Store) LiveSelfUpdate(ctx context.Context, tx Tx) (SelfUpdate, error) {
	row := tx.QueryRowContext(ctx, `
		SELECT `+selfUpdateColumns+` FROM self_updates
		WHERE state NOT IN ('succeeded','failed','canceled')
		ORDER BY created_at DESC, id DESC LIMIT 1`)
	u, err := scanSelfUpdate(row)
	if errors.Is(err, sql.ErrNoRows) {
		return SelfUpdate{}, ErrNotFound
	}
	if err != nil {
		return SelfUpdate{}, fmt.Errorf("read the live self_update: %w", err)
	}
	return u, nil
}

// SelfUpdates lists the newest rows first, for the Updates page's history.
func (s *Store) SelfUpdates(ctx context.Context, tx Tx, limit int) ([]SelfUpdate, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT `+selfUpdateColumns+` FROM self_updates ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list self_updates: %w", err)
	}
	defer rows.Close()

	var out []SelfUpdate
	for rows.Next() {
		u, err := scanSelfUpdate(rows)
		if err != nil {
			return nil, fmt.Errorf("scan a self_update: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// SetSelfUpdateState moves a row through the five states the daemon writes.
//
// The WHERE clause is the precondition: a row already in a terminal state is not
// moved, and zero rows changed is an ANSWER rather than an error — the gate's
// branch 1 is explicitly idempotent for a row already `succeeded`, because a
// kill between its commit and its unlink leaves the next caller resolving a
// marker beside a terminal row.
func (s *Store) SetSelfUpdateState(ctx context.Context, tx Tx, id string,
	state model.SelfUpdateState) (bool, error) {

	res, err := tx.ExecContext(ctx, `
		UPDATE self_updates SET state = ?
		WHERE id = ? AND state NOT IN ('succeeded','failed','canceled')`,
		string(state), id)
	if err != nil {
		return false, fmt.Errorf("set self_update %s to %s: %w", id, state, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// SelfUpdateStaged records what section 12.1 steps 2 through 4 learned — the
// asset, its digest, the signature verdict and the D14 snapshot — in the same
// transaction that commits `verifying → staged`, which is the cancel cut-off
// (D96).
type SelfUpdateStaged struct {
	AssetURL     *string
	AssetSHA256  *string
	SignatureOK  *bool
	DBBackupPath *string
	BinaryPath   *string
}

// SetSelfUpdateArtifacts writes the columns above without touching `state`, so a
// caller can record what it downloaded before it decides what state that leaves
// the row in.
func (s *Store) SetSelfUpdateArtifacts(ctx context.Context, tx Tx, id string,
	a SelfUpdateStaged) (bool, error) {

	var (
		sets []string
		args []any
	)
	if a.AssetURL != nil {
		sets, args = append(sets, "asset_url = ?"), append(args, *a.AssetURL)
	}
	if a.AssetSHA256 != nil {
		sets, args = append(sets, "asset_sha256 = ?"), append(args, *a.AssetSHA256)
	}
	if a.SignatureOK != nil {
		sets, args = append(sets, "signature_ok = ?"), append(args, boolInt(*a.SignatureOK))
	}
	if a.DBBackupPath != nil {
		sets, args = append(sets, "db_backup_path = ?"), append(args, *a.DBBackupPath)
	}
	if a.BinaryPath != nil {
		sets, args = append(sets, "binary_path = ?"), append(args, *a.BinaryPath)
	}
	if len(sets) == 0 {
		return false, nil
	}
	args = append(args, id)

	res, err := tx.ExecContext(ctx,
		`UPDATE self_updates SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
	if err != nil {
		return false, fmt.Errorf("record the artifacts of self_update %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// FinishSelfUpdate closes a row terminally, with the message the domain row
// carries. The job's own error_code is written by the queue in the same
// transaction (§2.3a).
func (s *Store) FinishSelfUpdate(ctx context.Context, tx Tx, id string,
	state model.SelfUpdateState, errorMessage *string, at int64) (bool, error) {

	if !state.IsTerminal() {
		return false, fmt.Errorf("finish self_update %s: %s is not a terminal state", id, state)
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE self_updates SET state = ?, error_message = ?, finished_at = ?
		WHERE id = ? AND state NOT IN ('succeeded','failed','canceled')`,
		string(state), errorMessage, at, id)
	if err != nil {
		return false, fmt.Errorf("finish self_update %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// CountLiveSelfUpdates counts non-terminal `self_updates` rows whose paired job
// is live — `interrupted` INCLUDED (§2.3, §3.14).
//
// It takes a Tx because it is a GUARD: D97 requires it to be evaluated inside
// the same BEGIN IMMEDIATE transaction that inserts the row and its job. A
// read-then-write against a snapshot is exactly the failure that decision exists
// to prevent — two concurrent applies both see 0, both insert, and step 1 of the
// second empties `update/` while the first is still downloading into it.
func (s *Store) CountLiveSelfUpdates(ctx context.Context, tx Tx) (int, error) {
	var n int
	err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM self_updates u
		WHERE u.state NOT IN ('succeeded','failed','canceled')
		  AND EXISTS (
		        SELECT 1 FROM jobs j
		        WHERE j.kind = 'self_update' AND j.subject_id = u.id
		          AND j.state IN ('queued','leased','running','interrupted'))`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count live self-updates: %w", err)
	}
	return n, nil
}

// OrphanedSelfUpdate is one row the closing pass will close, paired with the job
// row that made it an orphan.
type OrphanedSelfUpdate struct {
	SelfUpdateID string
	JobID        string
}

// OrphanedSelfUpdates is section 12.3's closing pass, as a query: every
// non-terminal `self_updates` row whose paired `self_update` job is
// `interrupted` and which `excludeID` does not name.
//
// Two things produce such an orphan and neither leaves a marker: a plain daemon
// restart during `downloading` or `verifying`, and a database restore — F12's
// boot recovery or `llamaman restore-db` — that resurrects a row from a snapshot
// taken mid-update. Without the pass, such a row's live job would refuse every
// future update at `409 job_in_flight` with no marker for any caller to find.
func (s *Store) OrphanedSelfUpdates(ctx context.Context, tx Tx, excludeID string) (
	[]OrphanedSelfUpdate, error) {

	rows, err := tx.QueryContext(ctx, `
		SELECT u.id, j.id FROM self_updates u
		JOIN jobs j ON j.kind = 'self_update' AND j.subject_id = u.id
		WHERE u.state NOT IN ('succeeded','failed','canceled')
		  AND j.state = 'interrupted'
		  AND u.id <> ?
		ORDER BY u.created_at`, excludeID)
	if err != nil {
		return nil, fmt.Errorf("find orphaned self-updates: %w", err)
	}
	defer rows.Close()

	var out []OrphanedSelfUpdate
	for rows.Next() {
		var o OrphanedSelfUpdate
		if err := rows.Scan(&o.SelfUpdateID, &o.JobID); err != nil {
			return nil, fmt.Errorf("scan an orphaned self-update: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// SelfUpdateJob returns the `self_update` job paired with a row, which §2.3a
// fixes as `(subject_type,subject_id) = ('self_update', self_updates.id)`.
func (s *Store) SelfUpdateJob(ctx context.Context, tx Tx, selfUpdateID string) (model.Job, error) {
	rows, err := s.Jobs(ctx, tx, JobFilter{
		Kinds:       []model.JobKind{model.JobSelfUpdate},
		SubjectType: model.SubjectSelfUpdate,
		SubjectID:   selfUpdateID,
		Limit:       1,
	})
	if err != nil {
		return model.Job{}, err
	}
	if len(rows) == 0 {
		return model.Job{}, ErrNotFound
	}
	return rows[0], nil
}

// Notification is one `notifications` row (§2.11) — the much smaller table
// beside `events`: things that need a human.
//
// It is a store type rather than a model one for the same reason
// LlamacppVersion is: nothing outside this package and its callers passes a
// notification around, and `events` has a model type only because an `events`
// row travels onward as an SSE frame.
type Notification struct {
	ID string
	At int64
	// Severity is the `notifications.severity` CHECK.
	Severity model.NotificationSeverity
	// Code maps to a UI remediation card (§17): `update_not_applied` is F24's,
	// `update_reverted` is F20's.
	Code  string
	Title string
	Body  string

	SubjectType *string
	SubjectID   *string
	// ActionJSON carries the card's buttons and the commands it prints — the
	// five-command downgrade procedure, the `install-units` repair line — as a
	// json_valid blob.
	ActionJSON *string
	// DismissedAt is when a human cleared the card, or nil while it is still
	// outstanding. It is a stamp rather than a delete so §2.11's "dismissed +
	// 30 days" retention has something to sweep.
	DismissedAt *int64
}

// AppendNotification writes one `notifications` row.
//
// The gate is its first caller, for F20 and F24. It is deliberately the DAEMON's
// row: neither privileged actor may open this database, so an actor that has
// something to say says it in the journal and the daemon's gate turns the fact
// into the card, carrying the actor units' journal tail with it.
func (s *Store) AppendNotification(ctx context.Context, tx Tx, n Notification) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO notifications
			(id, at, severity, code, title, body, subject_type, subject_id, action_json, dismissed_at)
		VALUES (?,?,?,?,?,?,?,?,?,NULL)`,
		n.ID, n.At, string(n.Severity), n.Code, n.Title, n.Body,
		n.SubjectType, n.SubjectID, n.ActionJSON)
	if err != nil {
		return fmt.Errorf("append notification %s: %w", n.Code, err)
	}
	return nil
}

// ReleaseCacheEntry is one `release_cache` row (§2.5). The self-update pipeline
// writes and reads the `llamaman` half; the llama.cpp manager owns the other
// source and its own cache.
type ReleaseCacheEntry struct {
	ID     string
	Source string
	Tag    string
	Name   *string
	// Prerelease is GitHub's own flag. `POST /update/apply` accepts any tag
	// `/update/releases` lists, and that listing hides prereleases.
	Prerelease  bool
	PublishedAt *int64
	BodyMD      *string
	// BodyHTML is the changelog rendered ONCE, server-side, by internal/mdrender
	// (D35). A release body is markdown written upstream and rendered into the
	// origin that holds the admin session cookie, so it is never rendered in a
	// browser from raw markdown.
	BodyHTML   *string
	AssetsJSON *string
	NightlyTag *string
	FetchedAt  int64
	ETag       *string
}

// PutReleaseCache upserts one row on `UNIQUE(source, tag)`.
func (s *Store) PutReleaseCache(ctx context.Context, tx Tx, e ReleaseCacheEntry) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO release_cache
			(id, source, tag, name, prerelease, published_at, body_md, body_html,
			 assets_json, nightly_tag, fetched_at, etag)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(source, tag) DO UPDATE SET
			name = excluded.name,
			prerelease = excluded.prerelease,
			published_at = excluded.published_at,
			body_md = excluded.body_md,
			body_html = excluded.body_html,
			assets_json = excluded.assets_json,
			nightly_tag = excluded.nightly_tag,
			fetched_at = excluded.fetched_at,
			etag = excluded.etag`,
		e.ID, e.Source, e.Tag, e.Name, boolInt(e.Prerelease), e.PublishedAt,
		e.BodyMD, e.BodyHTML, e.AssetsJSON, e.NightlyTag, e.FetchedAt, e.ETag)
	if err != nil {
		return fmt.Errorf("cache release %s/%s: %w", e.Source, e.Tag, err)
	}
	return nil
}

// VacuumInto writes a consistent copy of this database to path — the D14
// pre-update snapshot of §12.1 step 4, and `restore-db`'s own
// `llamaman.db.superseded-<ts>` (§12.4).
//
// It lives here rather than at either call site because D49's first invariant is
// that only this package writes SQL, and because VACUUM INTO takes a LITERAL
// rather than a bound parameter — so the one place that quotes a path into a
// statement should be the package whose job that is. The path is quoted the way
// SQLite quotes a string, with embedded single quotes doubled.
//
// It runs on the write pool: VACUUM is a write operation on the source
// connection even though it only reads the source's contents, and a Store from
// OpenReadOnly deliberately has no write pool at all.
func (s *Store) VacuumInto(ctx context.Context, path string) error {
	if s.RW == nil {
		return fmt.Errorf("store: VACUUM INTO %s: this store is read-only", path)
	}
	quoted := "'" + strings.ReplaceAll(path, "'", "''") + "'"
	if _, err := s.RW.ExecContext(ctx, "VACUUM INTO "+quoted); err != nil {
		return fmt.Errorf("VACUUM INTO %s: %w", path, err)
	}
	return nil
}

// CheckpointTruncate runs `PRAGMA wal_checkpoint(TRUNCATE)`, which is step (1)
// of `restore-db`'s five (§12.4) and part of §9.4 step 5's shutdown.
//
// After it the main database file is complete and its `-wal`/`-shm` sidecars are
// redundant, which is the property that makes the rest of `restore-db` crash
// safe: every window from there on lands on a database that opens.
func (s *Store) CheckpointTruncate(ctx context.Context) error {
	if s.RW == nil {
		return fmt.Errorf("store: wal_checkpoint(TRUNCATE) on %s: this store is read-only", s.Path)
	}
	if _, err := s.RW.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return fmt.Errorf("wal_checkpoint(TRUNCATE): %w", err)
	}
	return nil
}

// LossTables are the six tables `restore-db` counts its loss over (§12.4): "the
// LOSS, counted by opening both read-only: instances, tokens, benchmark runs,
// downloads, events and notifications present in the current database and absent
// from the snapshot, one line each".
//
// It is an ordered allowlist rather than a caller-supplied name because RowIDs
// interpolates the value into a statement — a table name cannot be a bound
// parameter — and an allowlist is what makes that safe by construction.
func LossTables() []string {
	return []string{"instances", "api_tokens", "bench_runs", "downloads", "events", "notifications"}
}

// RowIDs returns every id in one of the LossTables, for the set difference
// `restore-db` prints.
//
// A set difference rather than a count difference is deliberate: rows are
// deleted as well as created, so "the current database has 40 more events" is not
// the same statement as "40 events would be lost", and the operator is being
// asked to approve the second.
func (s *Store) RowIDs(ctx context.Context, tx Tx, table string) ([]string, error) {
	allowed := false
	for _, t := range LossTables() {
		if t == table {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, fmt.Errorf("store: %q is not one of the tables restore-db compares", table)
	}

	rows, err := tx.QueryContext(ctx, `SELECT id FROM `+table)
	if err != nil {
		return nil, fmt.Errorf("read %s ids: %w", table, err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan a %s id: %w", table, err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ReleaseCache lists the cached releases for one source, newest first.
func (s *Store) ReleaseCache(ctx context.Context, tx Tx, source string, limit int) (
	[]ReleaseCacheEntry, error) {

	if limit <= 0 {
		limit = 50
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT id, source, tag, name, prerelease, published_at, body_md, body_html,
		       assets_json, nightly_tag, fetched_at, etag
		FROM release_cache WHERE source = ?
		ORDER BY published_at DESC NULLS LAST, tag DESC LIMIT ?`, source, limit)
	if err != nil {
		return nil, fmt.Errorf("list cached %s releases: %w", source, err)
	}
	defer rows.Close()

	var out []ReleaseCacheEntry
	for rows.Next() {
		var (
			e          ReleaseCacheEntry
			prerelease int64
		)
		if err := rows.Scan(&e.ID, &e.Source, &e.Tag, &e.Name, &prerelease, &e.PublishedAt,
			&e.BodyMD, &e.BodyHTML, &e.AssetsJSON, &e.NightlyTag, &e.FetchedAt, &e.ETag); err != nil {
			return nil, fmt.Errorf("scan a cached release: %w", err)
		}
		e.Prerelease = prerelease != 0
		out = append(out, e)
	}
	return out, rows.Err()
}

func scanSelfUpdate(sc interface{ Scan(...any) error }) (SelfUpdate, error) {
	var (
		u           SelfUpdate
		state       string
		signatureOK *int64
	)
	if err := sc.Scan(&u.ID, &u.FromVersion, &u.ToVersion, &u.Channel, &state,
		&u.AssetURL, &u.AssetSHA256, &signatureOK, &u.DBBackupPath, &u.BinaryPath,
		&u.ErrorMessage, &u.CreatedAt, &u.FinishedAt); err != nil {
		return SelfUpdate{}, err
	}
	u.State = model.SelfUpdateState(state)
	if signatureOK != nil {
		ok := *signatureOK != 0
		u.SignatureOK = &ok
	}
	return u, nil
}
