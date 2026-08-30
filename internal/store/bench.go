package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// Benchmark queries: `bench_runs`, `bench_points`, `bench_results` and the
// `bench_lease` singleton (DESIGN sections 2.10 and 10).
//
// Three of the statements in this file are the ones section 10 argues for by
// name, and each is written the way it is because a simpler phrasing is a trap:
//
//   - BenchRunsOwingRestore selects on `restore_done = 0` in ANY state, never on
//     `state = 'running'`. Whatever moves a run out of `running` — a crash
//     mid-sweep, a cancel that did not finish, an operator's delete — would
//     silently disqualify the very rows that still owe production instances
//     their restart. `state` says what the BENCHMARK did; `restore_done` says
//     what the HOST is owed, and they are separate columns for exactly this.
//   - ReleaseBenchLease will not release a lease whose run still owes a restore
//     (D75). A run that has stopped two serving instances is still occupying the
//     host even though nothing is executing, and releasing early would let a
//     second sweep start into a fleet the first one has not put back.
//   - BenchLive is therefore two terms, not one: the lease is held, OR some run
//     has `restore_done = 0 AND stopped_instances_json IS NOT NULL`. That
//     disjunction is what makes section 6.6 step 1's "refuse an activation while
//     a bench is live" total.

// BenchRun is a `bench_runs` row (§2.10).
//
// The model and llama.cpp identity are DENORMALIZED onto it on purpose: a
// benchmark is history, and history outlives the model file it measured and the
// build it measured with. `model_id` and `llamacpp_version_id` are
// ON DELETE SET NULL for the same reason, so the label and the tag stay readable
// after the row they pointed at is gone.
type BenchRun struct {
	ID    string
	Name  string
	State model.BenchRunState

	ModelID    *string
	ModelLabel string
	ModelPath  string
	QuantLabel *string

	LlamacppVersionID *string
	LlamacppTag       string
	LlamacppCommit    *string
	LlamacppBackend   string

	// GPUJSON and HostJSON are §10's environment capture: GPU names, UUIDs,
	// VRAM, driver and CUDA version; CPU model and cores, RAM, kernel.
	// Cross-version comparisons are meaningless without them.
	GPUJSON  string
	HostJSON string
	// SweepJSON is the canonicalized Sweep document the points were expanded
	// from — what the UI re-opens as "run this again".
	SweepJSON   string
	Repetitions int

	PointsTotal  int
	PointsDone   int
	PointsFailed int

	// StoppedInstancesJSON names the production instances this run stopped, and
	// RestoreDone says whether they have been put back. Both are read by the
	// boot sweep, which is why neither is derived from `State`.
	StoppedInstancesJSON *string
	RestoreDone          bool

	ErrorMessage *string
	Notes        *string

	CreatedAt  int64
	StartedAt  *int64
	FinishedAt *int64
}

// Live reports whether this run still owes the host a restore: it stopped
// instances and has not put them back. It is the second term of BenchLive, as a
// method so a caller that already holds the row does not re-query for it.
func (r BenchRun) OwesRestore() bool {
	return !r.RestoreDone && r.StoppedInstancesJSON != nil
}

// BenchPoint is one `bench_points` row — one cell of the cross-product, created
// BEFORE execution so progress and resume are exact rather than estimated.
type BenchPoint struct {
	ID      string
	RunID   string
	Ordinal int
	State   model.BenchPointState
	// ArgsJSON is the rendered `llama-bench` command line as a JSON array of
	// strings. It is stored rather than re-derived so that a sweep interrupted
	// at point 40 resumes with byte-identical command lines — "exact resume"
	// means the same argv, not an argv rebuilt from inputs that may have moved.
	ArgsJSON string

	NGpuLayers  *int64
	NBatch      *int64
	NUbatch     *int64
	NThreads    *int64
	FlashAttn   *bool
	TypeK       *string
	TypeV       *string
	SplitMode   *string
	TensorSplit *string
	NDepth      *int64

	StartedAt    *int64
	FinishedAt   *int64
	ErrorMessage *string
}

// BenchResult is one `bench_results` row: one object of llama-bench's JSON
// array, with `raw_json` preserved verbatim so a future llama-bench schema
// change never loses data.
type BenchResult struct {
	ID       string
	PointID  string
	RunID    string
	TestKind model.BenchTestKind

	NPrompt int64
	NGen    int64
	NDepth  int64

	AvgTS    float64
	StddevTS float64
	AvgNS    int64
	StddevNS int64

	SamplesJSON *string
	RawJSON     string
	CreatedAt   int64
}

const benchRunColumns = `id, name, state, model_id, model_label, model_path, quant_label,
	llamacpp_version_id, llamacpp_tag, llamacpp_commit, llamacpp_backend,
	gpu_json, host_json, sweep_json, repetitions,
	points_total, points_done, points_failed,
	stopped_instances_json, restore_done, error_message, notes,
	created_at, started_at, finished_at`

// InsertBenchRun writes a new run row.
func (s *Store) InsertBenchRun(ctx context.Context, tx Tx, r BenchRun) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO bench_runs (`+benchRunColumns+`)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.Name, string(r.State), r.ModelID, r.ModelLabel, r.ModelPath, r.QuantLabel,
		r.LlamacppVersionID, r.LlamacppTag, r.LlamacppCommit, r.LlamacppBackend,
		r.GPUJSON, r.HostJSON, r.SweepJSON, r.Repetitions,
		r.PointsTotal, r.PointsDone, r.PointsFailed,
		r.StoppedInstancesJSON, boolInt(r.RestoreDone), r.ErrorMessage, r.Notes,
		r.CreatedAt, r.StartedAt, r.FinishedAt)
	if err != nil {
		return fmt.Errorf("insert bench run %s: %w", r.ID, err)
	}
	return nil
}

// BenchRun reads one run.
func (s *Store) BenchRun(ctx context.Context, tx Tx, id string) (BenchRun, error) {
	row := tx.QueryRowContext(ctx, `SELECT `+benchRunColumns+` FROM bench_runs WHERE id = ?`, id)
	r, err := scanBenchRun(row)
	if err != nil {
		return BenchRun{}, notFound(err)
	}
	return r, nil
}

// BenchRunFilter narrows a run listing.
type BenchRunFilter struct {
	// States, when non-empty, keeps only runs in one of these states.
	States []model.BenchRunState
	// ModelID keeps only runs against one model.
	ModelID string
	// Limit caps the result; zero means DefaultBenchRunLimit.
	Limit int
}

// DefaultBenchRunLimit bounds a run listing. Benchmarks are never auto-deleted
// (§2.11: they are the product), so this is a page size rather than a guard.
const DefaultBenchRunLimit = 200

// BenchRuns lists runs, NEWEST first — the order the history table reads in.
// Ids are ULIDs, so descending id is descending creation with a unique tiebreak.
func (s *Store) BenchRuns(ctx context.Context, tx Tx, f BenchRunFilter) ([]BenchRun, error) {
	q := `SELECT ` + benchRunColumns + ` FROM bench_runs`
	var (
		clauses []string
		args    []any
	)
	if len(f.States) > 0 {
		marks := make([]string, len(f.States))
		for i, st := range f.States {
			marks[i] = "?"
			args = append(args, string(st))
		}
		clauses = append(clauses, "state IN ("+strings.Join(marks, ",")+")")
	}
	if f.ModelID != "" {
		clauses = append(clauses, "model_id = ?")
		args = append(args, f.ModelID)
	}
	if len(clauses) > 0 {
		q += " WHERE " + strings.Join(clauses, " AND ")
	}
	limit := f.Limit
	if limit <= 0 {
		limit = DefaultBenchRunLimit
	}
	q += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("select bench runs: %w", err)
	}
	defer rows.Close()

	var out []BenchRun
	for rows.Next() {
		r, err := scanBenchRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// BenchRunsOwingRestore is §10's boot sweep, and its predicate is deliberately
// `restore_done = 0 AND stopped_instances_json IS NOT NULL` over EVERY state
// including the terminal ones.
//
// A state predicate alone is a trap: whatever moved the run out of `running` —
// a crash mid-run, a cancel that did not finish, an operator's delete — would
// disqualify exactly the rows that still owe a restore, and a benchmark that
// stopped two serving instances would leave them down forever.
func (s *Store) BenchRunsOwingRestore(ctx context.Context, tx Tx) ([]BenchRun, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT `+benchRunColumns+` FROM bench_runs
		  WHERE restore_done = 0 AND stopped_instances_json IS NOT NULL
		  ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("select bench runs owing a restore: %w", err)
	}
	defer rows.Close()

	var out []BenchRun
	for rows.Next() {
		r, err := scanBenchRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetBenchRunState moves the run's state and stamps whichever timestamp the new
// state implies: `started_at` on the first move out of `draft`/`queued`,
// `finished_at` on a terminal state.
func (s *Store) SetBenchRunState(ctx context.Context, tx Tx, id string,
	state model.BenchRunState, at int64) (bool, error) {

	var started, finished any
	switch state {
	case model.BenchPreflight, model.BenchRunning:
		started = at
	case model.BenchSucceeded, model.BenchPartial, model.BenchFailed, model.BenchCanceled:
		finished = at
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE bench_runs
		    SET state = ?,
		        started_at = COALESCE(started_at, ?),
		        finished_at = COALESCE(?, finished_at)
		  WHERE id = ?`, string(state), started, finished, id)
	if err != nil {
		return false, fmt.Errorf("set bench run %s state: %w", id, err)
	}
	return rowsChanged(res)
}

// FinishBenchRun closes a run: its terminal state, its counters and its error
// message in one statement, so a reader never sees `succeeded` with a stale
// `points_done`.
func (s *Store) FinishBenchRun(ctx context.Context, tx Tx, id string,
	state model.BenchRunState, done, failed int, errorMessage *string, at int64) (bool, error) {

	res, err := tx.ExecContext(ctx,
		`UPDATE bench_runs
		    SET state = ?, points_done = ?, points_failed = ?,
		        error_message = ?, finished_at = ?
		  WHERE id = ?`, string(state), done, failed, errorMessage, at, id)
	if err != nil {
		return false, fmt.Errorf("finish bench run %s: %w", id, err)
	}
	return rowsChanged(res)
}

// SetBenchRunCounters writes the progress counters alone, which is what the
// worker does after each point.
func (s *Store) SetBenchRunCounters(ctx context.Context, tx Tx, id string, done, failed int) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE bench_runs SET points_done = ?, points_failed = ? WHERE id = ?`, done, failed, id)
	if err != nil {
		return false, fmt.Errorf("set bench run %s counters: %w", id, err)
	}
	return rowsChanged(res)
}

// SetBenchRunNotes rewrites the annotation `PATCH /bench/runs/{id}` edits, and
// is also how §10.1's substitution notes ("flash_attn auto ran as -fa 1") reach
// the run row.
func (s *Store) SetBenchRunNotes(ctx context.Context, tx Tx, id string, name string, notes *string) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE bench_runs
		    SET name = COALESCE(NULLIF(?, ''), name), notes = COALESCE(?, notes)
		  WHERE id = ?`, name, notes, id)
	if err != nil {
		return false, fmt.Errorf("annotate bench run %s: %w", id, err)
	}
	return rowsChanged(res)
}

// SetBenchRunStopped records the instances this run stopped, which arms the boot
// sweep: from this write until MarkBenchRestoreDone, the host is owed a restart
// whatever happens to the run.
func (s *Store) SetBenchRunStopped(ctx context.Context, tx Tx, id string, stoppedJSON *string) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE bench_runs SET stopped_instances_json = ?, restore_done = 0 WHERE id = ?`,
		stoppedJSON, id)
	if err != nil {
		return false, fmt.Errorf("record the instances bench run %s stopped: %w", id, err)
	}
	return rowsChanged(res)
}

// MarkBenchRestoreDone is written only after every named instance has been
// restarted, found already running, or found deleted.
func (s *Store) MarkBenchRestoreDone(ctx context.Context, tx Tx, id string) (bool, error) {
	res, err := tx.ExecContext(ctx, `UPDATE bench_runs SET restore_done = 1 WHERE id = ?`, id)
	if err != nil {
		return false, fmt.Errorf("mark bench run %s restored: %w", id, err)
	}
	return rowsChanged(res)
}

// DeleteBenchRun removes a run. `bench_points` and `bench_results` cascade.
func (s *Store) DeleteBenchRun(ctx context.Context, tx Tx, id string) (bool, error) {
	res, err := tx.ExecContext(ctx, `DELETE FROM bench_runs WHERE id = ?`, id)
	if err != nil {
		return false, fmt.Errorf("delete bench run %s: %w", id, err)
	}
	return rowsChanged(res)
}

func scanBenchRun(sc interface{ Scan(...any) error }) (BenchRun, error) {
	var (
		r           BenchRun
		state       string
		restoreDone int64
	)
	err := sc.Scan(&r.ID, &r.Name, &state, &r.ModelID, &r.ModelLabel, &r.ModelPath, &r.QuantLabel,
		&r.LlamacppVersionID, &r.LlamacppTag, &r.LlamacppCommit, &r.LlamacppBackend,
		&r.GPUJSON, &r.HostJSON, &r.SweepJSON, &r.Repetitions,
		&r.PointsTotal, &r.PointsDone, &r.PointsFailed,
		&r.StoppedInstancesJSON, &restoreDone, &r.ErrorMessage, &r.Notes,
		&r.CreatedAt, &r.StartedAt, &r.FinishedAt)
	if err != nil {
		return BenchRun{}, err
	}
	r.State = model.BenchRunState(state)
	r.RestoreDone = restoreDone != 0
	return r, nil
}

// -----------------------------------------------------------------------------
// Points
// -----------------------------------------------------------------------------

const benchPointColumns = `id, run_id, ordinal, state, args_json,
	n_gpu_layers, n_batch, n_ubatch, n_threads, flash_attn, type_k, type_v,
	split_mode, tensor_split, n_depth, started_at, finished_at, error_message`

// InsertBenchPoint writes one expanded cell.
func (s *Store) InsertBenchPoint(ctx context.Context, tx Tx, p BenchPoint) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO bench_points (`+benchPointColumns+`)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.ID, p.RunID, p.Ordinal, string(p.State), p.ArgsJSON,
		p.NGpuLayers, p.NBatch, p.NUbatch, p.NThreads, boolArg(p.FlashAttn), p.TypeK, p.TypeV,
		p.SplitMode, p.TensorSplit, p.NDepth, p.StartedAt, p.FinishedAt, p.ErrorMessage)
	if err != nil {
		return fmt.Errorf("insert bench point %d of run %s: %w", p.Ordinal, p.RunID, err)
	}
	return nil
}

// BenchPoints lists a run's points in ordinal order — the execution order.
func (s *Store) BenchPoints(ctx context.Context, tx Tx, runID string) ([]BenchPoint, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT `+benchPointColumns+` FROM bench_points WHERE run_id = ? ORDER BY ordinal`, runID)
	if err != nil {
		return nil, fmt.Errorf("select the points of bench run %s: %w", runID, err)
	}
	defer rows.Close()

	var out []BenchPoint
	for rows.Next() {
		p, err := scanBenchPoint(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetBenchPointState moves one point. `started_at` is stamped on the move to
// `running` and `finished_at` on every terminal move, so a point's duration is
// on the row rather than inferred from the next point's start.
func (s *Store) SetBenchPointState(ctx context.Context, tx Tx, id string,
	state model.BenchPointState, errorMessage *string, at int64) (bool, error) {

	var started, finished any
	switch state {
	case model.PointRunning:
		started = at
	case model.PointSucceeded, model.PointFailed, model.PointSkipped:
		finished = at
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE bench_points
		    SET state = ?, error_message = COALESCE(?, error_message),
		        started_at = COALESCE(started_at, ?),
		        finished_at = COALESCE(?, finished_at)
		  WHERE id = ?`, string(state), errorMessage, started, finished, id)
	if err != nil {
		return false, fmt.Errorf("set bench point %s state: %w", id, err)
	}
	return rowsChanged(res)
}

// SkipPendingBenchPoints marks everything not yet run `skipped`, which is what a
// cancel does to the tail of a sweep — and what makes `points_done +
// points_failed + skipped == points_total` hold for a canceled run.
func (s *Store) SkipPendingBenchPoints(ctx context.Context, tx Tx, runID string, at int64) (int64, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE bench_points
		    SET state = 'skipped', finished_at = COALESCE(finished_at, ?)
		  WHERE run_id = ? AND state IN ('pending','running')`, at, runID)
	if err != nil {
		return 0, fmt.Errorf("skip the pending points of bench run %s: %w", runID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected: %w", err)
	}
	return n, nil
}

func scanBenchPoint(sc interface{ Scan(...any) error }) (BenchPoint, error) {
	var (
		p     BenchPoint
		state string
		fa    *int64
	)
	err := sc.Scan(&p.ID, &p.RunID, &p.Ordinal, &state, &p.ArgsJSON,
		&p.NGpuLayers, &p.NBatch, &p.NUbatch, &p.NThreads, &fa, &p.TypeK, &p.TypeV,
		&p.SplitMode, &p.TensorSplit, &p.NDepth, &p.StartedAt, &p.FinishedAt, &p.ErrorMessage)
	if err != nil {
		return BenchPoint{}, err
	}
	p.State = model.BenchPointState(state)
	p.FlashAttn = boolPtr(fa)
	return p, nil
}

// -----------------------------------------------------------------------------
// Results
// -----------------------------------------------------------------------------

const benchResultColumns = `id, point_id, run_id, test_kind, n_prompt, n_gen, n_depth,
	avg_ts, stddev_ts, avg_ns, stddev_ns, samples_json, raw_json, created_at`

// InsertBenchResult writes one parsed llama-bench object.
func (s *Store) InsertBenchResult(ctx context.Context, tx Tx, r BenchResult) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO bench_results (`+benchResultColumns+`)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.PointID, r.RunID, string(r.TestKind), r.NPrompt, r.NGen, r.NDepth,
		r.AvgTS, r.StddevTS, r.AvgNS, r.StddevNS, r.SamplesJSON, r.RawJSON, r.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert bench result for point %s: %w", r.PointID, err)
	}
	return nil
}

// BenchResults lists a run's results in point order.
func (s *Store) BenchResults(ctx context.Context, tx Tx, runID string) ([]BenchResult, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT r.id, r.point_id, r.run_id, r.test_kind, r.n_prompt, r.n_gen, r.n_depth,
		        r.avg_ts, r.stddev_ts, r.avg_ns, r.stddev_ns, r.samples_json, r.raw_json,
		        r.created_at
		   FROM bench_results r JOIN bench_points p ON p.id = r.point_id
		  WHERE r.run_id = ?
		  ORDER BY p.ordinal, r.id`, runID)
	if err != nil {
		return nil, fmt.Errorf("select the results of bench run %s: %w", runID, err)
	}
	defer rows.Close()

	var out []BenchResult
	for rows.Next() {
		v, err := scanBenchResult(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func scanBenchResult(sc interface{ Scan(...any) error }) (BenchResult, error) {
	var (
		r    BenchResult
		kind string
	)
	err := sc.Scan(&r.ID, &r.PointID, &r.RunID, &kind, &r.NPrompt, &r.NGen, &r.NDepth,
		&r.AvgTS, &r.StddevTS, &r.AvgNS, &r.StddevNS, &r.SamplesJSON, &r.RawJSON, &r.CreatedAt)
	if err != nil {
		return BenchResult{}, err
	}
	r.TestKind = model.BenchTestKind(kind)
	return r, nil
}

// -----------------------------------------------------------------------------
// The lease (D75)
// -----------------------------------------------------------------------------

// BenchLease is the D75 singleton: one bench at a time, held by a row rather
// than by a process mutex, because the second sweep can be the NEXT boot.
//
// It is the exact counterpart of BuildLease and for the exact same reason:
// `jobs.subject_id` for a bench is `bench_runs.id`, so
// `idx_jobs_one_live_per_subject` is per RUN and two `bench_run` jobs on two
// different runs are perfectly legal under it.
type BenchLease struct {
	JobID      *string
	RunID      *string
	Owner      *string
	AcquiredAt *int64
	ExpiresAt  *int64
}

// Held reports whether the lease is held at all.
func (l BenchLease) Held() bool { return l.JobID != nil }

// BenchLease reads the singleton.
func (s *Store) BenchLease(ctx context.Context, tx Tx) (BenchLease, error) {
	var l BenchLease
	err := tx.QueryRowContext(ctx,
		`SELECT job_id, run_id, owner, acquired_at, expires_at FROM bench_lease WHERE id = 1`).
		Scan(&l.JobID, &l.RunID, &l.Owner, &l.AcquiredAt, &l.ExpiresAt)
	if err != nil {
		return BenchLease{}, fmt.Errorf("read the bench lease: %w", err)
	}
	return l, nil
}

// AcquireBenchLease is §10's conditional UPDATE, the same one the build worker
// uses. Zero rows changed means another sweep holds it, and that is an ANSWER:
// the caller leaves its job `queued` with `run_after = now + 15 s` and the UI
// says "waiting for the running benchmark", which is a queue rather than an
// error.
func (s *Store) AcquireBenchLease(ctx context.Context, tx Tx, jobID, runID, owner string,
	at, expiresAt int64) (bool, error) {

	res, err := tx.ExecContext(ctx,
		`UPDATE bench_lease
		    SET job_id = ?, run_id = ?, owner = ?, acquired_at = ?, expires_at = ?
		  WHERE id = 1
		    AND (job_id IS NULL OR owner = ? OR expires_at IS NULL OR expires_at < ?)`,
		jobID, runID, owner, at, expiresAt, owner, at)
	if err != nil {
		return false, fmt.Errorf("acquire the bench lease: %w", err)
	}
	return rowsChanged(res)
}

// TouchBenchLease extends the horizon, which the worker does on every progress
// write — the same rhythm §6.5 gives the build lease.
func (s *Store) TouchBenchLease(ctx context.Context, tx Tx, jobID, owner string,
	expiresAt int64) (bool, error) {

	res, err := tx.ExecContext(ctx,
		`UPDATE bench_lease SET expires_at = ?
		  WHERE id = 1 AND job_id = ? AND owner = ?`, expiresAt, jobID, owner)
	if err != nil {
		return false, fmt.Errorf("extend the bench lease: %w", err)
	}
	return rowsChanged(res)
}

// benchOwesRestore is the NOT EXISTS clause both release statements carry. A
// lease whose run still owes production instances their restart is NOT released,
// however terminal the job is: nothing is executing, but the host is still
// occupied, and a second sweep starting into a half-restored fleet is the one
// outcome §10 calls the worst possible one.
const benchOwesRestore = `NOT EXISTS (
	SELECT 1 FROM bench_runs r
	 WHERE r.id = bench_lease.run_id
	   AND r.restore_done = 0 AND r.stopped_instances_json IS NOT NULL)`

// ReleaseBenchLease frees the singleton for one job — but only once its run owes
// no restore. It reports whether the lease was actually released, so a caller
// can tell "freed" from "still owed" without a second read.
func (s *Store) ReleaseBenchLease(ctx context.Context, tx Tx, jobID string) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE bench_lease
		    SET job_id = NULL, run_id = NULL, owner = NULL,
		        acquired_at = NULL, expires_at = NULL
		  WHERE id = 1 AND job_id = ? AND `+benchOwesRestore, jobID)
	if err != nil {
		return false, fmt.Errorf("release the bench lease: %w", err)
	}
	return rowsChanged(res)
}

// ReleaseForeignBenchLease frees a lease whose owner is not this boot — the
// holding daemon is provably gone. It runs at boot, and it carries the same
// restore guard: the finalizer restores first and releases second, never the
// other way round.
func (s *Store) ReleaseForeignBenchLease(ctx context.Context, tx Tx, bootID string) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE bench_lease
		    SET job_id = NULL, run_id = NULL, owner = NULL,
		        acquired_at = NULL, expires_at = NULL
		  WHERE id = 1 AND owner IS NOT NULL AND owner != ? AND `+benchOwesRestore, bootID)
	if err != nil {
		return false, fmt.Errorf("release a foreign bench lease: %w", err)
	}
	return rowsChanged(res)
}

// BenchLive answers §6.6 step 1's second refusal term, and it is TWO terms
// because §10 says the guard is only total that way: the lease is held, OR some
// run has stopped instances it has not put back. The first covers a sweep that
// is executing; the second covers one that is over — canceled, failed, crashed —
// and still owes the host a restart.
func (s *Store) BenchLive(ctx context.Context, tx Tx) (bool, error) {
	var live int64
	err := tx.QueryRowContext(ctx,
		`SELECT
		   (SELECT COUNT(*) FROM bench_lease WHERE id = 1 AND job_id IS NOT NULL)
		 + (SELECT COUNT(*) FROM bench_runs
		     WHERE restore_done = 0 AND stopped_instances_json IS NOT NULL)`).Scan(&live)
	if err != nil {
		return false, fmt.Errorf("read whether a bench is live: %w", err)
	}
	return live > 0, nil
}
