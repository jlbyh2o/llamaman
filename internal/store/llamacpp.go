package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// llamacpp_versions and build_lease queries (DESIGN sections 2.5, 2.3, 6.5, 6.6).
//
// Two things in this file are not mechanical, and both are here rather than in
// internal/llamacpp because they are single atomic moves over rows that carry
// UNIQUE partial indexes:
//
//   - ActivateLlamacppVersion performs section 6.6 step 2 and step 3's flag
//     moves in ONE statement sequence, ordered so `idx_llamacpp_one_active` and
//     `idx_llamacpp_one_prev` are never both satisfied by two rows at once. It
//     returns the pre-activation flags of every row it touched, which is the
//     input D24's revert needs — a revert that recomputed "what the flags used
//     to be" would be guessing.
//   - RestoreLlamacppFlags is that revert: clear, then reapply, for the same
//     index reason.
//
// Everything else is a single UPDATE whose WHERE clause is its precondition, so
// zero rows changed is an ANSWER (the row moved on, or never existed) and not
// an error.

const llamacppColumns = `id, channel, tag, build_tag, git_url, git_ref, resolved_commit,
	acquisition, backend, build_options_json, cuda_arch_list, host_cpu_flags, gpu_uuids_json,
	dir_name, superseded_by, state, is_active, activated_at, previous_active, size_bytes,
	binaries_json, devices_output, help_flags_json, supports_fit, log_path, exit_code,
	error_code, error_message, failing_step, created_at, started_at, finished_at`

// LlamacppVersion is one `llamacpp_versions` row (§2.5).
//
// The identity is three-part (D60): ID and DirName are both
// `<tag>-<backend>-<acq>`, and `UNIQUE(tag, backend, acquisition)` states the
// same thing a second way so that a two-part key cannot be reintroduced by
// accident.
type LlamacppVersion struct {
	ID      string
	Channel model.LlamacppChannel
	Tag     string
	// BuildTag is the `b#####` a stable release pinned through nightly-tag.txt.
	BuildTag       *string
	GitURL         string
	GitRef         *string
	ResolvedCommit *string
	Acquisition    model.Acquisition
	Backend        model.Backend

	BuildOptionsJSON *string
	CUDAArchList     *string
	HostCPUFlags     *string
	GPUUUIDsJSON     *string

	DirName string
	// SupersededBy links a `failed_verification` prebuilt to the source build
	// D18 enqueued in its place.
	SupersededBy *string

	State          model.VersionState
	IsActive       bool
	ActivatedAt    *int64
	PreviousActive bool

	SizeBytes     *int64
	BinariesJSON  *string
	DevicesOutput *string
	HelpFlagsJSON *string
	SupportsFit   bool

	LogPath      *string
	ExitCode     *int64
	ErrorCode    *string
	ErrorMessage *string
	FailingStep  *model.FailingStep

	CreatedAt  int64
	StartedAt  *int64
	FinishedAt *int64
}

// LlamacppVersionFilter selects rows for `GET /api/v1/llamacpp/versions` (§3.5).
type LlamacppVersionFilter struct {
	// States, when non-empty, restricts to these states.
	States []model.VersionState
	// Active and Previous, when set, restrict to the flagged row. Both are
	// pointers rather than bools because "the row that is not active" is a
	// question nothing asks and `false` must not be mistaken for "unset".
	Active   *bool
	Previous *bool
	// IncludeDeleted keeps `deleted` rows in the listing. The list endpoint
	// leaves it false: a deleted version is a row the UI has nothing to show
	// for, and D71's reuse-and-reset reads it by id rather than by listing.
	IncludeDeleted bool
}

// InsertLlamacppVersion writes a new row. The caller supplies the id, which is
// `<tag>-<backend>-<acq>` and equals DirName (D60).
func (s *Store) InsertLlamacppVersion(ctx context.Context, tx Tx, v LlamacppVersion) error {
	gitURL := v.GitURL
	if gitURL == "" {
		gitURL = DefaultLlamacppGitURL
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO llamacpp_versions (`+llamacppColumns+`)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		         ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		v.ID, string(v.Channel), v.Tag, v.BuildTag, gitURL, v.GitRef, v.ResolvedCommit,
		string(v.Acquisition), string(v.Backend), v.BuildOptionsJSON, v.CUDAArchList,
		v.HostCPUFlags, v.GPUUUIDsJSON, v.DirName, v.SupersededBy, string(v.State),
		boolInt(v.IsActive), v.ActivatedAt, boolInt(v.PreviousActive), v.SizeBytes,
		v.BinariesJSON, v.DevicesOutput, v.HelpFlagsJSON, boolInt(v.SupportsFit),
		v.LogPath, v.ExitCode, v.ErrorCode, v.ErrorMessage, enumArg(v.FailingStep),
		v.CreatedAt, v.StartedAt, v.FinishedAt)
	if err != nil {
		return fmt.Errorf("insert llamacpp version %s: %w", v.ID, err)
	}
	return nil
}

// DefaultLlamacppGitURL is the column default, repeated here so an insert that
// leaves GitURL empty writes the same string the schema would.
const DefaultLlamacppGitURL = "https://github.com/ggml-org/llama.cpp"

// LlamacppVersion returns one row by id, or ErrNotFound.
func (s *Store) LlamacppVersion(ctx context.Context, tx Tx, id string) (LlamacppVersion, error) {
	row := tx.QueryRowContext(ctx,
		`SELECT `+llamacppColumns+` FROM llamacpp_versions WHERE id = ?`, id)
	v, err := scanLlamacppVersion(row)
	if err != nil {
		return LlamacppVersion{}, notFound(err)
	}
	return v, nil
}

// LlamacppVersions lists rows newest first, which is the order the version list
// reads: the build a user just asked for is the one they are watching.
func (s *Store) LlamacppVersions(ctx context.Context, tx Tx, f LlamacppVersionFilter) (
	[]LlamacppVersion, error) {

	var (
		where []string
		args  []any
	)
	if len(f.States) > 0 {
		marks := make([]string, len(f.States))
		for i, st := range f.States {
			marks[i] = "?"
			args = append(args, string(st))
		}
		where = append(where, `state IN (`+strings.Join(marks, ",")+`)`)
	} else if !f.IncludeDeleted {
		where = append(where, `state != 'deleted'`)
	}
	if f.Active != nil {
		where = append(where, `is_active = ?`)
		args = append(args, boolInt(*f.Active))
	}
	if f.Previous != nil {
		where = append(where, `previous_active = ?`)
		args = append(args, boolInt(*f.Previous))
	}

	q := `SELECT ` + llamacppColumns + ` FROM llamacpp_versions`
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, " AND ")
	}
	q += ` ORDER BY created_at DESC, id DESC`

	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("select llamacpp versions: %w", err)
	}
	defer rows.Close()

	var out []LlamacppVersion
	for rows.Next() {
		v, err := scanLlamacppVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// LlamacppVersionByFlag returns the `is_active=1` or `previous_active=1` row, or
// ErrNotFound. No active build is an ordinary state on a fresh install, and no
// previous build is the ordinary state whenever `llamacpp.keep_previous` is off
// or nothing has ever been replaced.
func (s *Store) LlamacppVersionByFlag(ctx context.Context, tx Tx, previous bool) (
	LlamacppVersion, error) {

	column := "is_active"
	if previous {
		column = "previous_active"
	}
	row := tx.QueryRowContext(ctx,
		`SELECT `+llamacppColumns+` FROM llamacpp_versions WHERE `+column+` = 1`)
	v, err := scanLlamacppVersion(row)
	if err != nil {
		return LlamacppVersion{}, notFound(err)
	}
	return v, nil
}

// SetLlamacppVersionState moves a row through the pipeline states of §2.5's
// transition table. It stamps `started_at` on the first move out of `pending`
// and `finished_at` on a terminal state, so no caller has to remember which
// column belongs to which edge.
func (s *Store) SetLlamacppVersionState(ctx context.Context, tx Tx, id string,
	state model.VersionState, at int64) (bool, error) {

	res, err := tx.ExecContext(ctx,
		`UPDATE llamacpp_versions
		    SET state = ?,
		        started_at = CASE WHEN started_at IS NULL AND ? THEN ? ELSE started_at END,
		        finished_at = CASE WHEN ? THEN ? ELSE finished_at END
		  WHERE id = ?`,
		string(state),
		boolInt(state != model.VersionPending), at,
		boolInt(isTerminalVersionState(state)), at,
		id)
	if err != nil {
		return false, fmt.Errorf("set llamacpp version %s state: %w", id, err)
	}
	return rowsChanged(res)
}

// isTerminalVersionState reports whether a state stamps `finished_at`.
// `deleting` deliberately does not: it is a live state whose own edges out
// (§2.5) decide what the row finally becomes.
func isTerminalVersionState(s model.VersionState) bool {
	switch s {
	case model.VersionReady, model.VersionFailed, model.VersionFailedVerification,
		model.VersionCanceled, model.VersionDeleted:
		return true
	}
	return false
}

// LlamacppInstallResult is everything §2.5's `verifying → ready` edge writes.
type LlamacppInstallResult struct {
	ResolvedCommit *string
	SizeBytes      *int64
	BinariesJSON   *string
	DevicesOutput  *string
	HelpFlagsJSON  *string
	SupportsFit    bool
	BuildOptions   *string
	CUDAArchList   *string
	HostCPUFlags   *string
	GPUUUIDsJSON   *string
	LogPath        *string
}

// CompleteLlamacppInstall is that edge: `ready`, plus the columns that make the
// build usable — `help_flags_json` and `supports_fit` above all, because
// RenderArgv reads THOSE rather than manifest.json, which is what keeps the
// renderer pure (D51, §5.7).
func (s *Store) CompleteLlamacppInstall(ctx context.Context, tx Tx, id string,
	r LlamacppInstallResult, at int64) (bool, error) {

	res, err := tx.ExecContext(ctx,
		`UPDATE llamacpp_versions
		    SET state = 'ready',
		        resolved_commit = COALESCE(?, resolved_commit),
		        size_bytes = ?, binaries_json = ?, devices_output = ?,
		        help_flags_json = ?, supports_fit = ?,
		        build_options_json = COALESCE(?, build_options_json),
		        cuda_arch_list = COALESCE(?, cuda_arch_list),
		        host_cpu_flags = COALESCE(?, host_cpu_flags),
		        gpu_uuids_json = COALESCE(?, gpu_uuids_json),
		        log_path = COALESCE(?, log_path),
		        exit_code = NULL, error_code = NULL, error_message = NULL,
		        failing_step = NULL,
		        started_at = COALESCE(started_at, ?), finished_at = ?
		  WHERE id = ?`,
		r.ResolvedCommit, r.SizeBytes, r.BinariesJSON, r.DevicesOutput,
		r.HelpFlagsJSON, boolInt(r.SupportsFit), r.BuildOptions, r.CUDAArchList,
		r.HostCPUFlags, r.GPUUUIDsJSON, r.LogPath, at, at, id)
	if err != nil {
		return false, fmt.Errorf("complete llamacpp install %s: %w", id, err)
	}
	return rowsChanged(res)
}

// LlamacppFailure is §2.5's failure record: the state the row stops in, the
// phase that stopped it, and what to show a human.
type LlamacppFailure struct {
	State        model.VersionState
	FailingStep  *model.FailingStep
	ErrorCode    *string
	ErrorMessage *string
	ExitCode     *int64
	LogPath      *string
}

// FailLlamacppVersion writes one. The log path is kept rather than cleared:
// D4's whole point is that the build directory and its log survive a failure so
// Retry can reuse them.
func (s *Store) FailLlamacppVersion(ctx context.Context, tx Tx, id string,
	f LlamacppFailure, at int64) (bool, error) {

	res, err := tx.ExecContext(ctx,
		`UPDATE llamacpp_versions
		    SET state = ?, failing_step = ?, error_code = ?, error_message = ?,
		        exit_code = ?, log_path = COALESCE(?, log_path),
		        started_at = COALESCE(started_at, ?),
		        finished_at = CASE WHEN ? THEN ? ELSE finished_at END
		  WHERE id = ?`,
		string(f.State), enumArg(f.FailingStep), f.ErrorCode, f.ErrorMessage,
		f.ExitCode, f.LogPath, at,
		boolInt(isTerminalVersionState(f.State)), at, id)
	if err != nil {
		return false, fmt.Errorf("fail llamacpp version %s: %w", id, err)
	}
	return rowsChanged(res)
}

// ResetLlamacppVersion is D71's reuse-and-reset: a row in any terminal-failure
// state returns to `pending` with every trace of the previous attempt's OUTCOME
// cleared — while the outcome itself survives in `events` and in the rotated
// build log, which is why nothing here deletes anything.
//
// `superseded_by` is cleared too: a prebuilt that failed verification last week
// and is being retried on a newer host is no longer superseded by anything until
// it fails again.
func (s *Store) ResetLlamacppVersion(ctx context.Context, tx Tx, id string, at int64) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE llamacpp_versions
		    SET state = 'pending',
		        error_code = NULL, error_message = NULL, failing_step = NULL,
		        exit_code = NULL, superseded_by = NULL,
		        started_at = NULL, finished_at = NULL, created_at = ?
		  WHERE id = ? AND state IN ('failed','failed_verification','canceled','deleted','ready')`,
		at, id)
	if err != nil {
		return false, fmt.Errorf("reset llamacpp version %s: %w", id, err)
	}
	return rowsChanged(res)
}

// SetLlamacppBuildRequest records what a reset row is about to be built as:
// D71 lets a re-post change the build options, and `force_rebuild` is exactly
// the request that does. Nothing else may change — the id, and therefore the
// tag, backend and acquisition it encodes, is the identity.
func (s *Store) SetLlamacppBuildRequest(ctx context.Context, tx Tx, id string,
	buildOptionsJSON, cudaArchList *string) (bool, error) {

	res, err := tx.ExecContext(ctx,
		`UPDATE llamacpp_versions SET build_options_json = ?, cuda_arch_list = ? WHERE id = ?`,
		buildOptionsJSON, cudaArchList, id)
	if err != nil {
		return false, fmt.Errorf("set llamacpp build request %s: %w", id, err)
	}
	return rowsChanged(res)
}

// SetLlamacppSupersededBy links a `failed_verification` prebuilt to the source
// build D18 enqueued in its place (§6.4 step 3). The failed row is KEPT, and
// this column is what lets the UI say "prebuilt rejected (requires GLIBC_2.38,
// host has 2.36) — built from source instead" in one line.
func (s *Store) SetLlamacppSupersededBy(ctx context.Context, tx Tx, id, by string) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE llamacpp_versions SET superseded_by = ? WHERE id = ?`, by, id)
	if err != nil {
		return false, fmt.Errorf("supersede llamacpp version %s: %w", id, err)
	}
	return rowsChanged(res)
}

// LlamacppFlags is one row's activation flags as they stood at a moment.
// A slice of these is the input D24's revert restores from.
type LlamacppFlags struct {
	ID             string
	IsActive       bool
	PreviousActive bool
	ActivatedAt    *int64
}

// Activation is what ActivateLlamacppVersion did, and everything §6.6 needs
// afterwards.
type Activation struct {
	// Before is the pre-activation flags of every row this touched — the target,
	// the outgoing active and the row that held `previous_active`. Passing it to
	// RestoreLlamacppFlags is the whole of step 5's row revert.
	Before []LlamacppFlags
	// OutgoingID is the row that was `is_active=1`, empty on a first activation.
	OutgoingID string
	// DeletionCandidateID is the version §6.6 step 2 records in the activation
	// job's `params_json` — the row that lost its rollback slot, or, when
	// `keep_previous` is off, the outgoing build itself. The `llamacpp_delete`
	// job is enqueued only when the activation job SUCCEEDS, because step 5 may
	// revert an activation and cannot revert a directory a delete worker has
	// already removed.
	DeletionCandidateID string
}

// ActivateLlamacppVersion performs §6.6 steps 2 and 3's flag moves.
//
// The statement order is load-bearing: `idx_llamacpp_one_active` and
// `idx_llamacpp_one_prev` are UNIQUE partial indexes, so the flag must be
// cleared from its current holder before it is set on the new one. Doing it in
// the other order fails the insert with a constraint error that says nothing
// about which activation was attempted.
func (s *Store) ActivateLlamacppVersion(ctx context.Context, tx Tx, targetID string,
	keepPrevious bool, at int64) (Activation, error) {

	var act Activation

	before, err := s.llamacppFlagSnapshot(ctx, tx, targetID)
	if err != nil {
		return Activation{}, err
	}
	act.Before = before

	var priorPreviousID string
	for _, f := range before {
		if f.IsActive && f.ID != targetID {
			act.OutgoingID = f.ID
		}
		if f.PreviousActive {
			priorPreviousID = f.ID
		}
	}

	// (a) The rollback slot is emptied first, whoever held it.
	if _, err := tx.ExecContext(ctx,
		`UPDATE llamacpp_versions SET previous_active = 0 WHERE previous_active = 1`); err != nil {
		return Activation{}, fmt.Errorf("clear the previous llamacpp version: %w", err)
	}

	// (b) The outgoing active steps down, into the rollback slot when
	// `llamacpp.keep_previous` is on and out of the picture when it is not.
	if act.OutgoingID != "" {
		if _, err := tx.ExecContext(ctx,
			`UPDATE llamacpp_versions SET is_active = 0, previous_active = ? WHERE id = ?`,
			boolInt(keepPrevious), act.OutgoingID); err != nil {
			return Activation{}, fmt.Errorf("retire llamacpp version %s: %w", act.OutgoingID, err)
		}
	}

	// (c) The target takes the active slot.
	res, err := tx.ExecContext(ctx,
		`UPDATE llamacpp_versions
		    SET is_active = 1, previous_active = 0, activated_at = ?
		  WHERE id = ? AND state = 'ready'`,
		at, targetID)
	if err != nil {
		return Activation{}, fmt.Errorf("activate llamacpp version %s: %w", targetID, err)
	}
	changed, err := rowsChanged(res)
	if err != nil {
		return Activation{}, err
	}
	if !changed {
		return Activation{}, ErrNotFound
	}

	switch {
	case !keepPrevious:
		// The outgoing build has no rollback slot to fall into, so it is what
		// gets queued for deletion.
		act.DeletionCandidateID = act.OutgoingID
	case priorPreviousID != "" && priorPreviousID != targetID &&
		priorPreviousID != act.OutgoingID:
		// Rollback depth is one: the build that just lost the slot is the one
		// nothing references any more.
		act.DeletionCandidateID = priorPreviousID
	}
	return act, nil
}

// llamacppFlagSnapshot reads the flags of every row an activation can move: the
// target, whoever is active, and whoever holds the rollback slot.
func (s *Store) llamacppFlagSnapshot(ctx context.Context, tx Tx, targetID string) (
	[]LlamacppFlags, error) {

	rows, err := tx.QueryContext(ctx,
		`SELECT id, is_active, previous_active, activated_at
		   FROM llamacpp_versions
		  WHERE id = ? OR is_active = 1 OR previous_active = 1
		  ORDER BY id`, targetID)
	if err != nil {
		return nil, fmt.Errorf("read llamacpp activation flags: %w", err)
	}
	defer rows.Close()

	var out []LlamacppFlags
	for rows.Next() {
		var (
			f      LlamacppFlags
			active int64
			prev   int64
		)
		if err := rows.Scan(&f.ID, &active, &prev, &f.ActivatedAt); err != nil {
			return nil, fmt.Errorf("scan llamacpp activation flags: %w", err)
		}
		f.IsActive = active != 0
		f.PreviousActive = prev != 0
		out = append(out, f)
	}
	return out, rows.Err()
}

// RestoreLlamacppFlags puts `is_active`, `previous_active` and `activated_at`
// back exactly as ActivateLlamacppVersion found them — D24's row revert, and the
// FIRST half of §6.6 step 5, before any symlink is touched.
//
// It clears before it sets, for the same UNIQUE-partial-index reason the
// activation orders its statements.
func (s *Store) RestoreLlamacppFlags(ctx context.Context, tx Tx, before []LlamacppFlags) error {
	if len(before) == 0 {
		return nil
	}
	ids := make([]any, 0, len(before))
	for _, f := range before {
		ids = append(ids, f.ID)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE llamacpp_versions SET is_active = 0, previous_active = 0
		  WHERE id IN (`+placeholders(len(ids))+`)`, ids...); err != nil {
		return fmt.Errorf("clear llamacpp activation flags: %w", err)
	}
	for _, f := range before {
		if _, err := tx.ExecContext(ctx,
			`UPDATE llamacpp_versions
			    SET is_active = ?, previous_active = ?, activated_at = ?
			  WHERE id = ?`,
			boolInt(f.IsActive), boolInt(f.PreviousActive), f.ActivatedAt, f.ID); err != nil {
			return fmt.Errorf("restore llamacpp activation flags for %s: %w", f.ID, err)
		}
	}
	return nil
}

// SetJobParams rewrites a job's `params_json`.
//
// It lives here rather than in the jobs file because it exists for exactly one
// caller and one line of the design: §6.6 step 2 records the deletion candidate
// "in the activation job's `params_json`", and it can only do so AFTER the
// step-3 transaction has computed it. The boot finalizer then reads that field
// to enqueue the `llamacpp_delete` an interrupted activation still owes — which
// is the only way that delete survives a restart, since it is deliberately not
// enqueued until the activation succeeds.
//
// It is not a general facility: a worker that rewrote its own inputs mid-run
// would make `params_json` a scratch pad rather than the record of what was
// asked for, and §2.3's "a worker reads its inputs back out of this column after
// a restart" would stop meaning anything.
func (s *Store) SetJobParams(ctx context.Context, tx Tx, id string, paramsJSON *string) error {
	_, err := tx.ExecContext(ctx, `UPDATE jobs SET params_json = ? WHERE id = ?`, paramsJSON, id)
	if err != nil {
		return fmt.Errorf("set params for job %s: %w", id, err)
	}
	return nil
}

// BuildLease is the D70 singleton: one build at a time, held by a row rather
// than by a process mutex, because the second builder can be the NEXT boot.
type BuildLease struct {
	JobID      *string
	VersionID  *string
	Owner      *string
	AcquiredAt *int64
	ExpiresAt  *int64
}

// Held reports whether the lease is held at all.
func (l BuildLease) Held() bool { return l.JobID != nil }

// BuildLease reads the singleton.
func (s *Store) BuildLease(ctx context.Context, tx Tx) (BuildLease, error) {
	var l BuildLease
	err := tx.QueryRowContext(ctx,
		`SELECT job_id, version_id, owner, acquired_at, expires_at FROM build_lease WHERE id = 1`).
		Scan(&l.JobID, &l.VersionID, &l.Owner, &l.AcquiredAt, &l.ExpiresAt)
	if err != nil {
		return BuildLease{}, fmt.Errorf("read the build lease: %w", err)
	}
	return l, nil
}

// AcquireBuildLease is §2.3's conditional UPDATE verbatim. Zero rows changed
// means another build holds it, and that is an ANSWER: the caller leaves its job
// `queued` with `run_after = now + 15 s` and the UI says "waiting for the running
// build", which is a queue rather than an error.
func (s *Store) AcquireBuildLease(ctx context.Context, tx Tx, jobID, versionID, owner string,
	at, expiresAt int64) (bool, error) {

	res, err := tx.ExecContext(ctx,
		`UPDATE build_lease
		    SET job_id = ?, version_id = ?, owner = ?, acquired_at = ?, expires_at = ?
		  WHERE id = 1
		    AND (job_id IS NULL OR owner = ? OR expires_at IS NULL OR expires_at < ?)`,
		jobID, versionID, owner, at, expiresAt, owner, at)
	if err != nil {
		return false, fmt.Errorf("acquire the build lease: %w", err)
	}
	return rowsChanged(res)
}

// TouchBuildLease extends the horizon, which §6.5 does on every progress write.
func (s *Store) TouchBuildLease(ctx context.Context, tx Tx, jobID, owner string,
	expiresAt int64) (bool, error) {

	res, err := tx.ExecContext(ctx,
		`UPDATE build_lease SET expires_at = ?
		  WHERE id = 1 AND job_id = ? AND owner = ?`, expiresAt, jobID, owner)
	if err != nil {
		return false, fmt.Errorf("extend the build lease: %w", err)
	}
	return rowsChanged(res)
}

// ReleaseBuildLease frees the singleton for one job. It is called when a build
// reaches a terminal state and when it is canceled.
func (s *Store) ReleaseBuildLease(ctx context.Context, tx Tx, jobID string) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE build_lease
		    SET job_id = NULL, version_id = NULL, owner = NULL,
		        acquired_at = NULL, expires_at = NULL
		  WHERE id = 1 AND job_id = ?`, jobID)
	if err != nil {
		return false, fmt.Errorf("release the build lease: %w", err)
	}
	return rowsChanged(res)
}

// ReleaseForeignBuildLease frees a lease whose owner is not this boot — the
// holding daemon is provably gone (§2.3). It runs once at boot, beside the job
// queue's own orphan triage.
func (s *Store) ReleaseForeignBuildLease(ctx context.Context, tx Tx, bootID string) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE build_lease
		    SET job_id = NULL, version_id = NULL, owner = NULL,
		        acquired_at = NULL, expires_at = NULL
		  WHERE id = 1 AND owner IS NOT NULL AND owner != ?`, bootID)
	if err != nil {
		return false, fmt.Errorf("release a foreign build lease: %w", err)
	}
	return rowsChanged(res)
}

// scanLlamacppVersion reads one row from either a *sql.Row or a *sql.Rows.
func scanLlamacppVersion(sc interface{ Scan(...any) error }) (LlamacppVersion, error) {
	var (
		v           LlamacppVersion
		channel     string
		acquisition string
		backend     string
		state       string
		active      int64
		previous    int64
		fit         int64
		step        *string
	)
	err := sc.Scan(&v.ID, &channel, &v.Tag, &v.BuildTag, &v.GitURL, &v.GitRef,
		&v.ResolvedCommit, &acquisition, &backend, &v.BuildOptionsJSON, &v.CUDAArchList,
		&v.HostCPUFlags, &v.GPUUUIDsJSON, &v.DirName, &v.SupersededBy, &state,
		&active, &v.ActivatedAt, &previous, &v.SizeBytes, &v.BinariesJSON,
		&v.DevicesOutput, &v.HelpFlagsJSON, &fit, &v.LogPath, &v.ExitCode,
		&v.ErrorCode, &v.ErrorMessage, &step, &v.CreatedAt, &v.StartedAt, &v.FinishedAt)
	if err != nil {
		return LlamacppVersion{}, err
	}
	v.Channel = model.LlamacppChannel(channel)
	v.Acquisition = model.Acquisition(acquisition)
	v.Backend = model.Backend(backend)
	v.State = model.VersionState(state)
	v.IsActive = active != 0
	v.PreviousActive = previous != 0
	v.SupportsFit = fit != 0
	v.FailingStep = enumPtr[model.FailingStep](step)
	return v, nil
}
