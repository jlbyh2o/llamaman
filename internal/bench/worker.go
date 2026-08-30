package bench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jlbyh2o/llamaman/internal/events"
	"github.com/jlbyh2o/llamaman/internal/jobs"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/procx"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// The `bench_run` worker (DESIGN section 10), driven from the job queue.
//
// It owns four things the service deliberately does not:
//
//   - the D75 bench lease, acquired in the SAME transaction that moves the job
//     to `running`, because "one bench at a time" cannot be expressed by
//     `idx_jobs_one_live_per_subject` (two sweeps on two different runs are two
//     subjects) and an in-process mutex is not shared with the next boot;
//   - the stop-and-restore protocol, whose finalizer runs on success, failure
//     AND cancellation — and again at boot, from restore.go;
//   - the per-point execution loop, which is one `llama-bench` invocation per
//     point precisely so that a three-hour sweep interrupted at point 40 keeps
//     40 results;
//   - the §2.3a pairing, including the one cell that carries a recovery:
//     `interrupted` is a NO-OP here, because `bench_runs.state='running'` with
//     `restore_done=0` is the finalizer's INPUT and overwriting it destroys the
//     recovery that follows.

// Worker runs `bench_run`. Register it with the job registry; the queue leases
// only registered kinds, so an unregistered sweep waits rather than being burned
// through its attempt budget.
type Worker struct{ svc *Service }

// NewWorker builds the worker.
func (s *Service) NewWorker() *Worker { return &Worker{svc: s} }

// Kind implements jobs.Worker.
func (w *Worker) Kind() model.JobKind { return model.JobBenchRun }

// Start implements jobs.Starter: the bench lease and the run's move into
// `preflight` commit in the same transaction that moves the job to `running`.
//
// Zero rows changed on the lease is the D75 queue: the job goes back to `queued`
// for another try in fifteen seconds and the UI says "waiting for the running
// benchmark". It spends no part of the attempt budget and wears no error,
// because it is not a failure — it is a queue.
func (w *Worker) Start(ctx context.Context, tx store.Tx, j model.Job) error {
	p, err := decodeRunParams(j.ParamsJSON)
	if err != nil {
		return err
	}
	now := w.svc.now()

	// The conditional UPDATE §10 quotes reclaims a lease whose `owner` matches,
	// which is what lets ONE job re-take a lease it already holds after a defer.
	// It does not, on its own, stop a SECOND job of the same daemon from taking
	// it: `owner` is the boot id, and the queue runs several jobs at once. So the
	// holder is read first, in the same BEGIN IMMEDIATE transaction that would
	// write — D97's pattern, where a guard evaluated inside the transaction that
	// acts is the whole mechanism, because SQLite serializes writers. Two sweeps
	// on one host is precisely the outcome D75 calls the worst possible one.
	//
	// Expiry settles ONE of the two cases, and only one. A lease whose `owner` is
	// a boot that is gone is stale by definition and `expires_at` is how a later
	// boot notices before restore.go's reconciliation gets to it. A lease whose
	// `owner` is THIS boot belongs to a live sibling of this very job, and its
	// `expires_at` is a HEARTBEAT: a heartbeat that fell behind — a point that
	// outran the horizon, a slow disk, a suspended VM — is not permission for a
	// second sweep, it is a missed tick. D75's `owner = ?` clause exists so ONE
	// job can retake its OWN lease after a defer, never so a second job can take
	// it from a sibling that is still executing.
	lease, err := w.svc.store.BenchLease(ctx, tx)
	if err != nil {
		return err
	}
	if lease.Held() && *lease.JobID != j.ID {
		sameBoot := lease.Owner != nil && *lease.Owner == w.svc.bootID
		fresh := lease.ExpiresAt == nil || *lease.ExpiresAt >= now.UnixMilli()
		if sameBoot || fresh {
			return jobs.Defer(w.svc.retry)
		}
	}

	ok, err := w.svc.store.AcquireBenchLease(ctx, tx, j.ID, p.RunID, w.svc.bootID,
		now.UnixMilli(), now.Add(w.svc.leaseTTL).UnixMilli())
	if err != nil {
		return err
	}
	if !ok {
		return jobs.Defer(w.svc.retry)
	}
	_, err = w.svc.store.SetBenchRunState(ctx, tx, p.RunID, model.BenchPreflight, now.UnixMilli())
	return err
}

// SetDomainState implements jobs.DomainWriter for the three transitions the
// QUEUE performs with no worker running (§2.3).
//
// `interrupted` is a deliberate NO-OP, and §10 spends a paragraph on why: generic
// orphan recovery marking a `bench_run` job `failed` would — under §2.3a — also
// mark `bench_runs.state='failed'`, and any restore rule phrased over `running`
// would then match nothing at all, so a bench that stopped two serving instances
// would leave them down forever. Keeping the row exactly as it stands hands it to
// the boot finalizer in restore.go, which keys off `restore_done` rather than off
// `state` and therefore does not care.
//
// Every terminal state tries to release the lease, and the STORE refuses while
// the run still owes a restore — which is the ordering D75 requires and the
// reason the release is a conditional UPDATE rather than an unconditional one.
func (w *Worker) SetDomainState(ctx context.Context, tx store.Tx, j model.Job,
	state model.JobState) error {

	p, err := decodeRunParams(j.ParamsJSON)
	if err != nil {
		return err
	}
	now := w.svc.now().UnixMilli()

	switch state {
	case model.JobInterrupted:
		return nil
	case model.JobQueued:
		// A retry: the run goes back to `queued` and its pending points are
		// still pending, which is what makes a retry a RESUME rather than a
		// re-run. Points that already succeeded keep their results.
		_, err := w.svc.store.SetBenchRunState(ctx, tx, p.RunID, model.BenchQueued, now)
		return err
	}

	if _, err := w.svc.store.ReleaseBenchLease(ctx, tx, j.ID); err != nil {
		return err
	}
	switch state {
	case model.JobCanceled:
		if _, err := w.svc.store.SkipPendingBenchPoints(ctx, tx, p.RunID, now); err != nil {
			return err
		}
		_, err := w.svc.store.SetBenchRunState(ctx, tx, p.RunID, model.BenchCanceled, now)
		return err
	case model.JobFailed:
		msg := "the daemon stopped while this benchmark was queued"
		_, err := w.svc.store.FinishBenchRun(ctx, tx, p.RunID, model.BenchFailed, 0, 0, &msg, now)
		return err
	}
	return nil
}

// progress is `jobs.progress_json` for a sweep — §10's exact three fields.
type progress struct {
	PointsDone  int    `json:"points_done"`
	PointsTotal int    `json:"points_total"`
	Current     string `json:"current"`
}

// Run implements jobs.Worker: preflight, then one llama-bench per point, then
// the finalizer.
func (w *Worker) Run(ctx context.Context, t *jobs.Task) (jobs.Outcome, error) {
	p, err := decodeRunParams(t.Job().ParamsJSON)
	if err != nil {
		return jobs.Outcome{}, err
	}

	run, points, err := w.load(ctx, p.RunID)
	if err != nil {
		return jobs.Outcome{}, err
	}

	// The D75 lease is a heartbeat, not a one-shot, and the gap between progress
	// writes is exactly where that matters: ONE point is a whole model load plus
	// `-r` repetitions at depth, which routinely outlives DefaultLeaseTTL. Touch
	// it only BETWEEN points and `expires_at` lapses while llama-bench is still
	// on the GPU; store.AcquireBenchLease's `expires_at < ?` clause then matches
	// and a second sweep starts beside the first — the outcome section 10 calls
	// "the worst possible one, arrived at by two well-behaved workers". So the
	// horizon is extended from a ticker that runs BESIDE the process, for as long
	// as this worker owns the lease, including through the finalizer: a run that
	// still owes production instances their restart is still occupying the host.
	stopBeat := w.beat(ctx, t)
	defer stopBeat()

	// Preflight: the exclusivity guard, and the stop half of stop-and-restore.
	// It runs HERE rather than at create time because the fleet is live
	// processes: an instance started in the minute between the POST and the
	// lease is exactly the collision the guard exists to prevent.
	if err := w.preflight(ctx, t, &run); err != nil {
		return w.fail(ctx, run, err), nil
	}

	// A failure HERE is past the preflight, so the stop half may already have run
	// and `stopped_instances_json` may already name production instances that are
	// down. Returning the error bare would skip the finalizer entirely: the job is
	// enqueued with MaxAttempts 1, so it goes straight to terminal `failed`,
	// SetDomainState's release is refused while the run owes a restore, the lease
	// is held forever, `BenchLive` stays true — blocking every future bench and
	// every llama.cpp activation, section 6.6 step 1 — and the instances stay down
	// until the next boot. One transient SQLite error must not cost that, so this
	// path goes through w.fail like every other exit, and w.fail restores first.
	startedAt := w.svc.now()
	var startSink events.Sink
	if err := w.svc.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		if _, err := w.svc.store.SetBenchRunState(ctx, tx, run.ID, model.BenchRunning,
			startedAt.UnixMilli()); err != nil {
			return err
		}
		started := run
		started.State = model.BenchRunning
		return w.svc.appendEvent(ctx, tx, started, startedAt, "bench_started",
			model.LevelInfo, fmt.Sprintf("%s started", run.Name), &startSink)
	}); err != nil {
		return w.fail(ctx, run, err), nil
	}
	w.svc.publish(&startSink)

	done, failed := countFinished(points)
	canceled := false

	for _, point := range points {
		if point.State != model.PointPending && point.State != model.PointRunning {
			continue
		}
		if ctx.Err() != nil || t.CancelRequested() {
			canceled = true
			break
		}

		_ = t.SetProgress(ctx, progress{
			PointsDone: done, PointsTotal: run.PointsTotal, Current: PointLabel(point),
		})
		w.touchLease(ctx, t)

		ok, err := w.runPoint(ctx, run, point)
		switch {
		case errors.Is(err, context.Canceled) || t.CancelRequested():
			canceled = true
		case ok:
			done++
		default:
			failed++
		}
		if canceled {
			break
		}

		if err := w.svc.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
			_, err := w.svc.store.SetBenchRunCounters(ctx, tx, run.ID, done, failed)
			return err
		}); err != nil {
			w.svc.log.Warn("bench: could not record progress", "run", run.ID, "error", err)
		}
		_ = t.SetProgress(ctx, progress{
			PointsDone: done, PointsTotal: run.PointsTotal, Current: PointLabel(point),
		})
	}

	// The finalizer, on EVERY path out of the loop: success, failure and
	// cancellation alike. A benchmark that leaves production instances down is
	// the worst possible outcome, so restoration is idempotent and is also
	// re-checked at boot.
	restoreErr := w.svc.Restore(context.WithoutCancel(ctx), run.ID)
	if restoreErr != nil {
		w.svc.log.Error("bench: could not restore the instances this run stopped",
			"run", run.ID, "error", restoreErr)
	}

	now := w.svc.now()
	if canceled {
		return jobs.Canceled(func(ctx context.Context, tx store.Tx, _ model.JobState) error {
			if _, err := w.svc.store.SkipPendingBenchPoints(ctx, tx, run.ID, now.UnixMilli()); err != nil {
				return err
			}
			if _, err := w.svc.store.FinishBenchRun(ctx, tx, run.ID, model.BenchCanceled,
				done, failed, nil, now.UnixMilli()); err != nil {
				return err
			}
			_, err := w.svc.store.ReleaseBenchLease(ctx, tx, t.Job().ID)
			return err
		}), nil
	}

	// `succeeded` when every point produced results, `partial` when some did,
	// `failed` only when none did. Partial results are results: a sweep whose
	// last three points OOM'd still measured the first forty.
	state, code, message := model.BenchSucceeded, "", ""
	switch {
	case done == 0:
		state = model.BenchFailed
		code = string(CodeBenchFailed)
		message = "no point of this sweep produced a result"
	case failed > 0:
		state = model.BenchPartial
		message = fmt.Sprintf("%d of %d points failed", failed, run.PointsTotal)
	}

	finishSink := &events.Sink{}
	commit := func(ctx context.Context, tx store.Tx, _ model.JobState) error {
		var msg *string
		if message != "" {
			msg = &message
		}
		if _, err := w.svc.store.FinishBenchRun(ctx, tx, run.ID, state,
			done, failed, msg, now.UnixMilli()); err != nil {
			return err
		}
		if _, err := w.svc.store.ReleaseBenchLease(ctx, tx, t.Job().ID); err != nil {
			return err
		}
		finished := run
		finished.State = state
		level := model.LevelInfo
		if state == model.BenchFailed {
			level = model.LevelError
		}
		summary := message
		if summary == "" {
			summary = fmt.Sprintf("%s finished %d of %d points", run.Name, done, run.PointsTotal)
		}
		return w.svc.appendEvent(ctx, tx, finished, now, "bench_finished", level, summary,
			finishSink)
	}

	// AFTER the closing transaction, not on the way out of this function: the
	// old `defer` published before commit had even been attempted, so a frame
	// could describe a finish that never landed.
	out := jobs.Succeeded(commit)
	if state == model.BenchFailed {
		out = jobs.Failed(code, message, commit)
	}
	out.AfterCommit = func() { w.svc.publish(finishSink) }
	return out, nil
}

// load reads the run and its points.
func (w *Worker) load(ctx context.Context, runID string) (store.BenchRun, []store.BenchPoint, error) {
	var (
		run    store.BenchRun
		points []store.BenchPoint
	)
	err := w.svc.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		if run, err = w.svc.store.BenchRun(ctx, tx, runID); err != nil {
			return err
		}
		points, err = w.svc.store.BenchPoints(ctx, tx, runID)
		return err
	})
	return run, points, err
}

// countFinished seeds the counters from what a previous attempt already did,
// which is what makes a resumed sweep continue its progress bar rather than
// restart it.
func countFinished(points []store.BenchPoint) (done, failed int) {
	for _, p := range points {
		switch p.State {
		case model.PointSucceeded:
			done++
		case model.PointFailed:
			failed++
		}
	}
	return done, failed
}

// runPoint executes one llama-bench invocation and persists what it produced.
//
// A point that fails is isolated: its row records the failure, the sweep moves
// on, and the run ends `partial`. Per-point failure isolation is one of the four
// reasons §10 gives for invoking llama-bench once per point rather than letting
// it expand the cross-product internally.
func (w *Worker) runPoint(ctx context.Context, run store.BenchRun, point store.BenchPoint) (bool, error) {
	now := w.svc.now()
	if err := w.svc.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		_, err := w.svc.store.SetBenchPointState(ctx, tx, point.ID, model.PointRunning, nil,
			now.UnixMilli())
		return err
	}); err != nil {
		return false, err
	}

	var argv []string
	if err := json.Unmarshal([]byte(point.ArgsJSON), &argv); err != nil || len(argv) == 0 {
		return false, w.failPoint(ctx, point, "this point's stored command line could not be read")
	}

	var stdout strings.Builder
	res, runErr := w.svc.runner.Run(ctx, procx.Cmd{
		Path: argv[0],
		Args: argv[1:],
		Now:  w.svc.now,
		OnLine: func(l procx.Line) {
			// Only stdout carries the JSON array. stderr is llama-bench's own
			// load and progress chatter, which is line-parsed for the `current`
			// field rather than accumulated — a model load prints a few hundred
			// lines and none of them belong in a result row.
			if l.Stream == procx.StreamStdout {
				stdout.WriteString(l.Text)
				stdout.WriteByte('\n')
			}
		},
	})
	if runErr != nil && errors.Is(runErr, context.Canceled) {
		return false, runErr
	}
	if runErr != nil {
		return false, w.failPoint(ctx, point,
			fmt.Sprintf("llama-bench exited %d: %s", res.ExitCode, lastLine(stdout.String())))
	}

	records, err := ParseOutput([]byte(stdout.String()))
	if err != nil {
		return false, w.failPoint(ctx, point, err.Error())
	}

	at := w.svc.now()
	err = w.svc.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		for _, rec := range records {
			samples, err := rec.SamplesJSON()
			if err != nil {
				return err
			}
			if err := w.svc.store.InsertBenchResult(ctx, tx, store.BenchResult{
				ID:          w.svc.newID(at),
				PointID:     point.ID,
				RunID:       run.ID,
				TestKind:    rec.Kind(),
				NPrompt:     int64(rec.NPrompt),
				NGen:        int64(rec.NGen),
				NDepth:      int64(rec.NDepth),
				AvgTS:       rec.AvgTS,
				StddevTS:    rec.StddevTS,
				AvgNS:       roundNS(rec.AvgNS),
				StddevNS:    roundNS(rec.StddevNS),
				SamplesJSON: samples,
				RawJSON:     string(rec.RawJSON),
				CreatedAt:   at.UnixMilli(),
			}); err != nil {
				return err
			}
		}
		_, err := w.svc.store.SetBenchPointState(ctx, tx, point.ID, model.PointSucceeded, nil,
			at.UnixMilli())
		return err
	})
	if err != nil {
		return false, err
	}
	return true, nil
}

// failPoint records one point's failure and returns nil: a failed point is not a
// failed sweep.
func (w *Worker) failPoint(ctx context.Context, point store.BenchPoint, message string) error {
	at := w.svc.now().UnixMilli()
	return w.svc.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		_, err := w.svc.store.SetBenchPointState(ctx, tx, point.ID, model.PointFailed,
			&message, at)
		return err
	})
}

// fail closes a sweep that could not start at all — a GPU conflict at execution
// time, or a stop that could not be performed. The finalizer still runs, because
// a preflight that stopped one instance before failing on the second still owes
// the first one a restart.
func (w *Worker) fail(ctx context.Context, run store.BenchRun, cause error) jobs.Outcome {
	if err := w.svc.Restore(context.WithoutCancel(ctx), run.ID); err != nil {
		w.svc.log.Error("bench: could not restore after a failed preflight",
			"run", run.ID, "error", err)
	}
	now := w.svc.now()
	message := cause.Error()
	code := string(CodeBenchFailed)
	var me model.Error
	if errors.As(cause, &me) {
		code, message = string(me.Code), me.Message
	}

	return jobs.Failed(code, message, func(ctx context.Context, tx store.Tx, _ model.JobState) error {
		if _, err := w.svc.store.SkipPendingBenchPoints(ctx, tx, run.ID, now.UnixMilli()); err != nil {
			return err
		}
		if _, err := w.svc.store.FinishBenchRun(ctx, tx, run.ID, model.BenchFailed, 0, 0,
			&message, now.UnixMilli()); err != nil {
			return err
		}
		live, err := w.svc.store.BenchLease(ctx, tx)
		if err != nil || live.JobID == nil {
			return err
		}
		_, err = w.svc.store.ReleaseBenchLease(ctx, tx, *live.JobID)
		return err
	})
}

// beat extends the D75 horizon on a ticker for as long as the sweep runs, and
// returns the stop function the caller defers.
//
// The ticker's context is derived with context.WithoutCancel on purpose: a
// CANCELED sweep still runs the stop-and-restore finalizer, and it must still
// hold the lease while it does — releasing it mid-restore is the half-restored
// fleet the store's own release guard exists to prevent. The returned function
// cancels the ticker and waits for the goroutine, so no touch is in flight once
// the caller returns and the queue's terminal transaction releases the lease.
func (w *Worker) beat(ctx context.Context, t *jobs.Task) func() {
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(w.svc.leaseBeat)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				w.touchLease(ctx, t)
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// touchLease extends the D75 horizon, from beat's ticker and from every progress
// write, which is the same rhythm §6.5 gives the build lease: a lease that lapsed
// mid-sweep would let a second sweep in beside the first.
func (w *Worker) touchLease(ctx context.Context, t *jobs.Task) {
	expires := w.svc.now().Add(w.svc.leaseTTL).UnixMilli()
	if err := w.svc.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		_, err := w.svc.store.TouchBenchLease(ctx, tx, t.Job().ID, w.svc.bootID, expires)
		return err
	}); err != nil {
		w.svc.log.Warn("bench: could not extend the bench lease", "job", t.Job().ID, "error", err)
	}
}

// decodeRunParams reads `jobs.params_json`.
func decodeRunParams(raw *string) (runParams, error) {
	var p runParams
	if raw == nil || *raw == "" {
		return p, errors.New("bench: a bench_run job carries no params")
	}
	if err := json.Unmarshal([]byte(*raw), &p); err != nil {
		return p, fmt.Errorf("bench: read a bench_run job's params: %w", err)
	}
	if p.RunID == "" {
		return p, errors.New("bench: a bench_run job names no run")
	}
	return p, nil
}

// lastLine is the tail of a captured stream, for an error message. A whole
// llama-bench transcript in `bench_points.error_message` would make the column
// unreadable in a table and is already in the journal.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			if len(line) > 400 {
				return line[:400] + "…"
			}
			return line
		}
	}
	return "it printed nothing"
}

// appendEvent writes a `bench` category event inside the caller's transaction.
func (s *Service) appendEvent(ctx context.Context, tx store.Tx, run store.BenchRun,
	now time.Time, action string, level model.EventLevel, message string,
	sink *events.Sink) error {

	if s.events == nil {
		return nil
	}
	ev := s.newEvent(run, now, action, level, message)
	if err := s.events.Append(ctx, tx, ev); err != nil {
		return err
	}
	sink.Add(ev)
	return nil
}

// publish fans the collected rows out AFTER the transaction that wrote them has
// committed, and publishes those rows rather than lookalikes minted beside
// them: a live frame must carry the ULID, level and to_state its row holds, or
// internal/sse's Last-Event-ID dedup cannot tell a replayed row from its live
// twin and a reconnecting client renders the run's history twice.
func (s *Service) publish(sink *events.Sink) {
	if s.events == nil {
		return
	}
	for _, ev := range sink.Drain() {
		s.events.Publish(ev)
	}
}

func (s *Service) newEvent(run store.BenchRun, now time.Time, action string,
	level model.EventLevel, message string) model.Event {

	subjectType, subjectID := string(model.SubjectBenchRun), run.ID
	state := string(run.State)
	return model.Event{
		ID:          s.newID(now),
		At:          now.UnixMilli(),
		Level:       level,
		Category:    model.CategoryBench,
		SubjectType: &subjectType,
		SubjectID:   &subjectID,
		Action:      action,
		ToState:     &state,
		Actor:       model.ActorSystem,
		Message:     message,
	}
}
