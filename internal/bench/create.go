package bench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/jlbyh2o/llamaman/internal/instances"
	"github.com/jlbyh2o/llamaman/internal/jobs"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// The run lifecycle: create, start, cancel, annotate and delete (DESIGN section
// 3.13), with §10's "expansion first" as the shape of the create.
//
// EVERY point row is written before anything executes, in the same transaction
// as the run row and the job row. That is what buys exact progress ("point 40 of
// 144", not a percentage), exact resume after a crash (the pending points are a
// query, not a replay of the expansion), and a duration estimate the sweep
// builder can show before the user commits.

// runParams is `jobs.params_json` for a `bench_run`. It carries only the run id:
// everything else the worker needs is on the run row, which is the row that
// survives a restart with its state intact.
type runParams struct {
	RunID string `json:"run_id"`
}

// CreateRequest is `POST /api/v1/bench/runs`.
type CreateRequest struct {
	Name    string
	ModelID string
	// Repetitions is llama-bench's `-r`. Zero means
	// `settings.bench.default_repetitions`.
	Repetitions int
	Sweep       Sweep
	// Draft creates the run without queueing it — §3.13's `201 draft`, which is
	// the sweep builder saving work in progress. The default queues it and
	// answers `202`.
	Draft bool

	Idempotency *jobs.Idempotency
}

// CreateResult is what the endpoint answers with.
type CreateResult struct {
	Run    store.BenchRun
	Points []store.BenchPoint
	Job    model.Job
	// Replayed is the D65 hit: an `Idempotency-Key` seen inside its window with
	// the same route and fingerprint, answered `200` with the original run.
	Replayed bool
}

// Create expands the sweep and writes the run, its points and — unless a draft
// was asked for — its job, in ONE transaction.
//
// The GPU conflict check happens BEFORE that transaction and refuses with
// `409 bench_gpu_conflict` when `on_conflict` is `abort`. It is deliberately not
// a guard inside the write: the conflicting instances are live processes, not
// rows, so a serialized read of them proves nothing that a check one second
// earlier did not — and the worker re-runs the same check at execution time,
// which is the check that actually matters.
func (s *Service) Create(ctx context.Context, req CreateRequest) (CreateResult, error) {
	if err := req.Sweep.Validate(); err != nil {
		return CreateResult{}, err
	}

	now := s.now()
	reps := req.Repetitions
	if reps <= 0 {
		reps = int(s.settingInt(ctx, "bench.default_repetitions", 3))
	}
	if reps <= 0 {
		reps = 1
	}
	if reps > MaxRepetitions {
		return CreateResult{}, errorf(CodeSweepTooLarge,
			"repetitions %d is above the limit of %d; every repetition is a full run of every "+
				"point, so this multiplies the whole sweep", reps, MaxRepetitions)
	}

	points, err := Expand(req.Sweep)
	if err != nil {
		return CreateResult{}, err
	}

	policy := req.Sweep.OnConflict
	if policy == "" {
		policy = ConflictAbort
	}
	if policy == ConflictStopAndRestore && s.fleet == nil {
		return CreateResult{}, errorf(CodeBenchGPUConflict,
			"this daemon cannot stop and restore instances, so on_conflict=%q is unavailable; "+
				"stop the conflicting instances by hand and use %q",
			ConflictStopAndRestore, ConflictAbort)
	}

	gpus, inv := s.probeGPUs(ctx)
	base := model.FlagSet{}
	if req.Sweep.Base != nil {
		base = *req.Sweep.Base
	}
	target := inv.Resolve(base)

	var (
		run     store.BenchRun
		rows    []store.BenchPoint
		job     model.Job
		replay  bool
		conflix []Occupancy
	)

	err = s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		refs, err := s.store.ModelRefsByID(ctx, tx, []string{req.ModelID})
		if err != nil {
			return err
		}
		ref, ok := refs[req.ModelID]
		if !ok {
			return errorf(model.CodeModelMissing, "no model has id %s", req.ModelID)
		}
		if ref.State != model.ModelReady {
			return errorf(model.CodeModelMissing,
				"model %s is %s, so there is no file on disk to benchmark", req.ModelID, ref.State)
		}
		modelPath := filepath.Join(ref.SnapshotDir, ref.PrimaryFile)

		active, err := s.store.ActiveVersion(ctx, tx)
		if errors.Is(err, store.ErrNotFound) {
			return errorf(CodeBenchNoRuntime,
				"no llama.cpp build is active, so there is no llama-bench to run")
		}
		if err != nil {
			return err
		}
		if !active.Ready() {
			return errorf(CodeBenchNoRuntime,
				"the active llama.cpp build is %s, not ready", active.State)
		}
		version, err := s.store.LlamacppVersion(ctx, tx, active.ID)
		if err != nil {
			return err
		}

		if s.settingBool(ctx, "bench.exclusive_gpu", true) {
			views, err := s.store.InstanceViews(ctx, tx, store.InstanceFilter{})
			if err != nil {
				return err
			}
			conflix = Conflicts(target, views, inv)
			if len(conflix) > 0 && policy == ConflictAbort {
				return withDetails(errorf(CodeBenchGPUConflict,
					"%d instance(s) are loaded on the GPUs this benchmark would use; stop them, "+
						"or re-post with on_conflict=%q", len(conflix), ConflictStopAndRestore),
					ConflictDetails(conflix))
			}
		}

		runtime := instances.Runtime{
			ID:          version.ID,
			Dir:         s.versionDir(version.DirName),
			SupportsFit: version.SupportsFit,
		}
		primary := &instances.ModelFile{ID: ref.ID, Path: modelPath}

		sweepJSON, err := req.Sweep.Canonical()
		if err != nil {
			return err
		}

		state := model.BenchQueued
		if req.Draft {
			state = model.BenchDraft
		}
		run = store.BenchRun{
			ID:                s.newID(now),
			Name:              runName(req.Name, ref.ID, now),
			State:             state,
			ModelID:           &ref.ID,
			ModelLabel:        ref.ID,
			ModelPath:         modelPath,
			LlamacppVersionID: &version.ID,
			LlamacppTag:       version.Tag,
			LlamacppCommit:    version.ResolvedCommit,
			LlamacppBackend:   string(version.Backend),
			GPUJSON:           marshal(gpuFacts(gpus)),
			HostJSON:          marshal(hostFacts()),
			SweepJSON:         sweepJSON,
			Repetitions:       reps,
			PointsTotal:       len(points),
			CreatedAt:         now.UnixMilli(),
		}
		if notes := benchNotes(req.Sweep); len(notes) > 0 {
			joined := joinLines(notes)
			run.Notes = &joined
		}
		if err := s.store.InsertBenchRun(ctx, tx, run); err != nil {
			return err
		}

		rows = make([]store.BenchPoint, 0, len(points))
		for _, p := range points {
			argv, err := instances.RenderBenchArgv(p.Flags, primary, runtime,
				p.BenchPoint(reps, req.Sweep.ExtraFlags))
			if err != nil {
				return err
			}
			row, err := pointRow(s.newID(now), run.ID, p, argv)
			if err != nil {
				return err
			}
			if err := s.store.InsertBenchPoint(ctx, tx, row); err != nil {
				return err
			}
			rows = append(rows, row)
		}

		if err := s.appendEvent(ctx, tx, run, now, "bench_created", model.LevelInfo,
			fmt.Sprintf("%s: %d points against %s", run.Name, run.PointsTotal, run.ModelLabel)); err != nil {
			return err
		}

		if req.Draft {
			return nil
		}
		res, err := s.enqueue(ctx, tx, run.ID, req.Idempotency)
		if err != nil {
			return err
		}
		job, replay = res.Job, res.Replayed
		return nil
	})
	if err != nil {
		return CreateResult{}, err
	}

	if !req.Draft && !replay {
		s.queue.Wake()
	}
	s.publish(run, now, "bench_created")
	return CreateResult{Run: run, Points: rows, Job: job, Replayed: replay}, nil
}

// Start is `POST /api/v1/bench/runs/{id}/start`: draft → queued.
func (s *Service) Start(ctx context.Context, id string) (model.Job, error) {
	var job model.Job
	now := s.now()

	err := s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		run, err := s.store.BenchRun(ctx, tx, id)
		if err != nil {
			return err
		}
		if run.State != model.BenchDraft {
			return errorf(CodeBenchNotStartable,
				"this benchmark is %s; only a draft can be started", run.State)
		}
		if _, err := s.store.SetBenchRunState(ctx, tx, id, model.BenchQueued, now.UnixMilli()); err != nil {
			return err
		}
		res, err := s.enqueue(ctx, tx, id, nil)
		if err != nil {
			return err
		}
		job = res.Job
		return s.appendEvent(ctx, tx, run, now, "bench_queued", model.LevelInfo,
			run.Name+" was queued")
	})
	if err != nil {
		return model.Job{}, err
	}
	s.queue.Wake()
	return job, nil
}

// enqueue inserts the `bench_run` job inside the caller's transaction. The
// domain row is written by the caller rather than through jobs.DomainFunc,
// because both callers already hold the run row and §2.3a only requires the two
// writes to share a transaction — not to share a callback.
func (s *Service) enqueue(ctx context.Context, tx store.Tx, runID string,
	idem *jobs.Idempotency) (jobs.EnqueueResult, error) {

	return s.queue.EnqueueTx(ctx, tx, jobs.EnqueueParams{
		Kind:     model.JobBenchRun,
		DomainID: runID,
		Params:   runParams{RunID: runID},
		// One attempt. A sweep that failed did not fail transiently: it failed
		// because llama-bench would not run, or because the GPUs were occupied,
		// and re-running it automatically would stop the same production
		// instances a second time. Retry is a button.
		MaxAttempts: 1,
		Idempotency: idem,
	})
}

// Cancel is `POST /api/v1/bench/runs/{id}/cancel`.
//
// It raises `cancel_requested` on the job and returns; the WORKER stops the
// process group, marks the remaining points `skipped` and runs the restore
// finalizer. A cancel that closed the run here would be a cancel that skipped
// the restore, which is the one thing this subsystem may not do.
func (s *Service) Cancel(ctx context.Context, id string) (model.Job, error) {
	var live model.Job
	err := s.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		if _, err := s.store.BenchRun(ctx, tx, id); err != nil {
			return err
		}
		j, err := s.jobFor(ctx, tx, id)
		if err != nil {
			return err
		}
		if j == nil || !j.State.IsLive() {
			return errorf(CodeBenchNotCancelable,
				"no benchmark job for this run is live, so there is nothing to cancel")
		}
		live = *j
		return nil
	})
	if err != nil {
		return model.Job{}, err
	}
	return s.queue.Cancel(ctx, live.ID)
}

// Annotate is `PATCH /api/v1/bench/runs/{id}`: rename and annotate, and nothing
// else. A benchmark's inputs are immutable once expanded — the points are
// already written and the results already measured — so there is no edit here
// that could make the row disagree with what ran.
func (s *Service) Annotate(ctx context.Context, id, name string, notes *string) (store.BenchRun, error) {
	var out store.BenchRun
	err := s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		if _, err := s.store.BenchRun(ctx, tx, id); err != nil {
			return err
		}
		if _, err := s.store.SetBenchRunNotes(ctx, tx, id, name, notes); err != nil {
			return err
		}
		var err error
		out, err = s.store.BenchRun(ctx, tx, id)
		return err
	})
	return out, err
}

// Delete is `DELETE /api/v1/bench/runs/{id}`.
//
// It refuses while a job is live. Deleting the row under the worker would
// cascade its points away mid-sweep and leave the finalizer with no
// `stopped_instances_json` to read — which is exactly how a benchmark ends up
// leaving production instances down, and §10 calls that the worst possible
// outcome.
func (s *Service) Delete(ctx context.Context, id string) error {
	return s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		run, err := s.store.BenchRun(ctx, tx, id)
		if err != nil {
			return err
		}
		j, err := s.jobFor(ctx, tx, id)
		if err != nil {
			return err
		}
		if j != nil && j.State.IsLive() {
			return errorf(CodeBenchRunning,
				"this benchmark is still running; cancel it first")
		}
		if run.OwesRestore() {
			return errorf(CodeBenchRunning,
				"this benchmark stopped %s and has not restarted them yet; "+
					"the restore runs at the next boot and deleting the row would lose the list",
				*run.StoppedInstancesJSON)
		}
		if _, err := s.store.DeleteBenchRun(ctx, tx, id); err != nil {
			return err
		}
		return s.appendEvent(ctx, tx, run, s.now(), "bench_deleted", model.LevelInfo,
			run.Name+" was deleted")
	})
}

// pointRow projects an expanded Point onto its `bench_points` row, with the
// rendered argv in `args_json`.
func pointRow(id, runID string, p Point, argv []string) (store.BenchPoint, error) {
	b, err := json.Marshal(argv)
	if err != nil {
		return store.BenchPoint{}, fmt.Errorf("bench: render the argv of point %d: %w", p.Ordinal, err)
	}

	row := store.BenchPoint{
		ID:       id,
		RunID:    runID,
		Ordinal:  p.Ordinal,
		State:    model.PointPending,
		ArgsJSON: string(b),
	}
	if p.Flags.NGpuLayers != nil {
		row.NGpuLayers = i64(benchNGL(*p.Flags.NGpuLayers))
	}
	row.NBatch = i64p(p.Flags.BatchSize)
	row.NUbatch = i64p(p.Flags.UbatchSize)
	row.NThreads = i64p(p.Flags.Threads)
	if p.Flags.FlashAttn != nil {
		// The column is the two-valued llama-bench flag, not the tri-state:
		// section 10.1 maps on→1, off→0 and auto→1, and the run's `notes` is
		// where the substitution is recorded.
		on := *p.Flags.FlashAttn != model.FlashAttnOff
		row.FlashAttn = &on
	}
	row.TypeK = p.Flags.CacheTypeK
	row.TypeV = p.Flags.CacheTypeV
	if p.Flags.SplitMode != nil {
		sm := string(*p.Flags.SplitMode)
		row.SplitMode = &sm
	}
	if len(p.Flags.TensorSplit) > 0 {
		ts := joinRatios(p.Flags.TensorSplit)
		row.TensorSplit = &ts
	}
	if p.Test.Depth != nil {
		row.NDepth = i64(*p.Test.Depth)
	}
	return row, nil
}

// runName falls back to a name a user can find again: the model and the day.
func runName(name, modelLabel string, at time.Time) string {
	if name != "" {
		return name
	}
	return modelLabel + " " + at.UTC().Format("2006-01-02 15:04")
}

func i64(v int) *int64 { n := int64(v); return &n }

func i64p(v *int) *int64 {
	if v == nil {
		return nil
	}
	return i64(*v)
}

func joinRatios(vs []float64) string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = trimFloat(v)
	}
	return joinComma(out)
}

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ","
		}
		out += p
	}
	return out
}

func joinLines(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += "\n"
		}
		out += p
	}
	return out
}

// trimFloat renders a ratio in decimal, never in exponent form — the same rule
// the argv renderer follows, so `-ts 0.6,0.4` and the stored `tensor_split`
// column read identically.
func trimFloat(v float64) string {
	return trimZeros(fmt.Sprintf("%.6f", v))
}

func trimZeros(s string) string {
	if !containsDot(s) {
		return s
	}
	for len(s) > 0 && s[len(s)-1] == '0' {
		s = s[:len(s)-1]
	}
	if len(s) > 0 && s[len(s)-1] == '.' {
		s = s[:len(s)-1]
	}
	return s
}

func containsDot(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			return true
		}
	}
	return false
}
