package download

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jlbyh2o/llamaman/internal/hf/cache"
	"github.com/jlbyh2o/llamaman/internal/jobs"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// The `model_download` worker (DESIGN sections 2.3a, 2.7, 7.3, 7.4).
//
// One job, one download, N files. The pool is at the FILE level (D26: up to
// `hf.download_concurrency` files at a time, one connection each), shared across
// every download this daemon is running, so two queued downloads cannot between
// them open twice the connections the setting allows.
//
// # Progress, and why the ETA is honest
//
// Each transfer counts its own bytes into an atomic. A 1 Hz ticker sums them,
// computes an EWMA speed and an ETA, writes the rows every 2 s and publishes an
// SSE patch every 1 s. `bytes_at_start` is recorded, so a download that found
// 38 of 40 GB already on disk does not report a 40 GB/s transfer rate — the ETA
// divides the bytes REMAINING by the speed of the bytes THIS RUN moved.

// ProgressTick is section 7.4's SSE cadence.
const ProgressTick = 1 * time.Second

// RowWriteEvery is how often the counters reach the database. Twice the SSE
// cadence, because a row write is a transaction and an ETA that is one second
// stale is an ETA nobody can tell from a fresh one.
const RowWriteEvery = 2 * time.Second

// SpeedSmoothing is the EWMA factor: each tick is 30% of the new sample and 70%
// of the history. A raw per-second rate on a link that pauses for a TCP
// retransmit reads as "0 B/s, ETA infinite", which is a worse lie than a number
// that lags by a few seconds.
const SpeedSmoothing = 0.3

// Params is what a `model_download` job carries in `params_json` — everything
// the worker needs to resume after a restart, since the leased row is all it
// gets.
type Params struct {
	DownloadID string `json:"download_id"`
	RepoID     string `json:"repo_id"`
	// Revision is the RESOLVED commit, never a branch: the job must fetch the
	// same bytes it was created for even if `main` moved in between.
	Revision string `json:"revision"`
	RootID   string `json:"root_id"`
	HubDir   string `json:"hub_dir"`
	// RefName is the branch the user asked for, for the `refs/` entry. Empty
	// when the request named a commit.
	RefName string `json:"ref_name,omitempty"`
}

// Worker runs `model_download` jobs. The composition root registers it with
// `jobs.Queue.Register`.
type Worker struct{ svc *Service }

// NewWorker builds the worker for `jobs.Queue.Register`.
func NewWorker(s *Service) *Worker { return &Worker{svc: s} }

// Kind is `model_download`.
func (w *Worker) Kind() model.JobKind { return model.JobModelDownload }

// Register registers this package's worker with a queue. It exists so the
// composition root has one call to make and cannot forget half of a subsystem
// that will grow a second kind.
func Register(q interface{ Register(jobs.Worker) error }, s *Service) error {
	return q.Register(NewWorker(s))
}

// SetDomainState is the DomainWriter half of section 2.3a: the queue moves the
// job row on this worker's behalf — boot triage, a cancel of a job no worker
// holds, a retry — and the domain row must move in the SAME transaction.
//
// Boot triage sends `model_download` to `queued` (section 2.3's first bucket:
// idempotent and resumable), and the download row follows it there. That is the
// whole of why a download survives a restart: the partial files are on disk, the
// task rows remember their offsets, and the job runs again from the top.
func (w *Worker) SetDomainState(ctx context.Context, tx store.Tx, j model.Job, state model.JobState) error {
	s := w.svc
	id := j.SubjectID
	now := s.now().UnixMilli()

	// Only the TASK state is chosen here. The download's state is whatever those
	// tasks then fold to (section 2.7), which is why this switch names one
	// column and not two: a mapping that named both would be a second definition
	// of the fold, and the two would drift.
	var tstate model.DownloadTaskState
	switch state {
	case model.JobQueued:
		tstate = model.TaskQueued
	case model.JobPaused:
		tstate = model.TaskPaused
	case model.JobCanceled:
		tstate = model.TaskCanceled
	case model.JobFailed:
		tstate = model.TaskFailed
	case model.JobSucceeded:
		tstate = model.TaskSucceeded
	case model.JobInterrupted:
		// Never reached for this kind — section 2.3's table sends
		// `model_download` to `queued` — and a no-op is the correct behavior if
		// it ever were: the domain row's state would be the finalizer's input.
		return nil
	default:
		return nil
	}

	var finished *int64
	if state == model.JobCanceled || state == model.JobFailed || state == model.JobSucceeded {
		finished = &now
	}
	if _, err := s.store.SetDownloadTasksState(ctx, tx, id, tstate, finished); err != nil {
		return err
	}
	var code, msg *string
	if state == model.JobFailed {
		code, msg = j.ErrorCode, j.ErrorMessage
	}
	// The queue declaring the job succeeded is the queue saying the verification
	// step is behind us — the only fact about a download its task rows do not
	// carry.
	_, err := s.writeState(ctx, tx, id, stateWrite{
		ErrorCode: code, ErrorMessage: msg, Verified: state == model.JobSucceeded,
	})
	return err
}

// Run performs one download.
func (w *Worker) Run(ctx context.Context, t *jobs.Task) (jobs.Outcome, error) {
	s := w.svc
	id := t.Job().SubjectID

	var p Params
	if raw := t.Job().ParamsJSON; raw != nil {
		if err := json.Unmarshal([]byte(*raw), &p); err != nil {
			return jobs.Failed("bad_params", "the download job's parameters could not be decoded",
				s.commitState(id, model.DownloadFailed, model.TaskFailed,
					strPtr("bad_params"), strPtr("unreadable job parameters"))), nil
		}
	}
	if p.DownloadID == "" {
		p.DownloadID = id
	}

	tasks, dl, err := s.loadRun(ctx, id, &p)
	if err != nil {
		return jobs.Outcome{}, err
	}
	if len(tasks) == 0 {
		// Every file is already present — the blob short-circuit landed them, or
		// a previous run finished and the job was retried. Completion is still
		// the honest outcome, and it re-links and re-marks the models so a
		// half-finished previous run cannot leave a `planned` model behind.
		return w.complete(ctx, id, p, dl)
	}

	// `started_at` is stamped here; the STATE is the tasks' to imply. It stays
	// `queued` for the moment it takes the first file to take a pool slot, and
	// startTask folds it to `running` when one does — which is section 2.7's
	// rule ("any running → `running`") applied rather than anticipated.
	started := s.now().UnixMilli()
	if err := t.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		if _, err := s.writeState(ctx, tx, id, stateWrite{StartedAt: &started}); err != nil {
			return err
		}
		return s.markModelsDownloading(ctx, tx, tasks)
	}); err != nil {
		return jobs.Outcome{}, err
	}

	runErr := s.runTasks(ctx, t, id, tasks, dl)

	switch {
	case ctx.Err() != nil && t.CancelRequested():
		// A cancel. The queue writes `canceled`; the Commit moves the rows with
		// it, and the partial files are kept unless the cancel asked otherwise
		// (the transfer already removed those on its way out).
		s.forgetDrop(id)
		return jobs.Canceled(s.commitState(id, model.DownloadCanceled, model.TaskCanceled, nil, nil)), nil
	case ctx.Err() != nil:
		// A pause or a shutdown. Neither is this worker's to record: a pause
		// already wrote `paused` and released the lease, so the queue will drop
		// whatever this returns, and a shutdown leaves the row `running` for the
		// next boot to triage.
		return jobs.Outcome{}, ctx.Err()
	case runErr != nil:
		code, msg := failureFor(runErr)
		return jobs.RetryableFailure(code, msg,
			s.commitState(id, model.DownloadFailed, model.TaskFailed, &code, &msg)), nil
	}

	return w.complete(ctx, id, p, dl)
}

// complete is the `verifying → succeeded` half: every file is on disk, so the
// models move to `ready` with their resolved paths, D69 recomputes every
// referencing instance's `config_hash`, and a `post_download` scan is requested
// to fill the GGUF geometry.
func (w *Worker) complete(ctx context.Context, id string, p Params, dl store.Download) (jobs.Outcome, error) {
	s := w.svc

	// Every task has succeeded, so the fold answers `verifying` on its own —
	// which is exactly section 2.7's "all succeeded → `verifying` → `succeeded`"
	// with the first arrow taken.
	if err := s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		_, err := s.writeState(ctx, tx, id, stateWrite{})
		return err
	}); err != nil {
		return jobs.Outcome{}, err
	}

	modelIDs, total, onDisk, err := s.verifyModels(ctx, id, p)
	if err != nil {
		code, msg := failureFor(err)
		return jobs.RetryableFailure(code, msg,
			s.commitState(id, model.DownloadFailed, model.TaskFailed, &code, &msg)), nil
	}

	// The scan is requested AFTER the transaction that made the models ready, so
	// it walks a snapshot that is already complete. Its failure is logged and
	// never fails the download: the bytes are on disk and the catalog names
	// them, and geometry is something the next ordinary scan will fill.
	if s.scans != nil && p.RootID != "" {
		if _, err := s.scans.RequestScan(ctx, p.RootID, model.ScanTriggerPostDownload); err != nil {
			s.log.Warn("hf/download: could not queue the post-download scan", "error", err)
		}
	}

	done := total
	return jobs.Succeeded(func(ctx context.Context, tx store.Tx, _ model.JobState) error {
		now := s.now().UnixMilli()
		row := dl
		row.ID = id
		row.BytesDone = done
		row.BytesTotal = maxInt64(total, dl.BytesTotal)
		row.SpeedBPS = 0
		row.ETASec = nil
		if _, err := s.store.UpdateDownloadProgress(ctx, tx, row); err != nil {
			return err
		}
		if _, err := s.store.SetDownloadTasksState(ctx, tx, id, model.TaskSucceeded, &now); err != nil {
			return err
		}
		// The second arrow of section 2.7's "all succeeded → `verifying` →
		// `succeeded`": the links are written, the models are `ready` and D69
		// has run, so this is the one commit that may promote the fold.
		if _, err := s.writeState(ctx, tx, id, stateWrite{Verified: true}); err != nil {
			return err
		}
		if err := s.appendEvent(ctx, tx, model.Event{
			ID: s.newID(s.now()), At: now, Level: model.LevelInfo,
			Category: model.CategoryDownload, Action: "succeeded",
			SubjectType: strPtr(string(model.SubjectDownload)), SubjectID: strPtr(id),
			ToState: strPtr(string(model.DownloadSucceeded)), Actor: model.ActorSystem,
			Message: fmt.Sprintf("downloaded %s to %s", humanBytes(onDisk), p.RepoID),
		}); err != nil {
			return err
		}
		// D69, at the moment it matters most: the models just became `ready`
		// with a real `snapshot_dir` and `primary_file`, so every non-deleted
		// instance referencing them gets a recomputed `config_hash` in this same
		// transaction. Without it, section 3.10a's "queue the download,
		// configure the instance while it runs" ends with an instance whose
		// stored hash describes a path that never existed.
		return s.recompute(ctx, tx, modelIDs...)
	}), nil
}

// loadRun reads the rows this run works from and returns the tasks that still
// have work to do.
func (s *Service) loadRun(ctx context.Context, id string, p *Params) ([]*task, store.Download, error) {
	var (
		out []*task
		dl  store.Download
	)
	err := s.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		dl, err = s.store.Download(ctx, tx, id)
		if err != nil {
			return err
		}
		if p.HubDir == "" || p.RepoID == "" || p.Revision == "" {
			// A job whose params were lost — an older row, or one written by
			// hand. The rows themselves are the authority and can rebuild all of
			// it.
			m, err := s.store.LocalModel(ctx, tx, dl.ModelID)
			if err != nil {
				return err
			}
			p.RepoID, p.Revision = m.RepoID, m.Revision
			if m.RefName != nil {
				p.RefName = *m.RefName
			}
			p.RootID = m.RootID
			root, err := s.store.PrimaryCacheRoot(ctx, tx)
			if err != nil {
				return err
			}
			p.HubDir = root.Path
		}

		views, err := s.store.DownloadTaskViews(ctx, tx, id)
		if err != nil {
			return err
		}
		layout := cache.NewLayout(p.HubDir)
		for _, v := range views {
			if v.State == model.TaskSucceeded {
				continue
			}
			files, err := s.store.ModelFiles(ctx, tx, v.ModelID)
			if err != nil {
				return err
			}
			var f model.ModelFile
			for _, cand := range files {
				if cand.ID == v.ModelFileID {
					f = cand
					break
				}
			}
			if f.ID == "" {
				return fmt.Errorf("hf/download: task %s names a model file that is gone", v.ID)
			}
			out = append(out, &task{
				row: v.DownloadTask, file: f, modelID: v.ModelID,
				repo: p.RepoID, revision: p.Revision, refName: p.RefName, layout: layout,
			})
		}
		return nil
	})
	return out, dl, err
}

// runTasks runs every outstanding file through the D26 pool and reports progress
// while they do.
func (s *Service) runTasks(ctx context.Context, t *jobs.Task, id string,
	tasks []*task, dl store.Download) error {

	sem := s.filePool(ctx)

	stop := make(chan struct{})
	var reporter sync.WaitGroup
	reporter.Add(1)
	go func() {
		defer reporter.Done()
		s.report(ctx, t, id, tasks, dl, stop)
	}()
	defer func() {
		close(stop)
		reporter.Wait()
	}()

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for _, tk := range tasks {
		tk := tk
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			if err := s.startTask(ctx, tk); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
				return
			}
			err := s.runTask(ctx, tk, func() bool { return s.shouldDrop(id) })
			tk.done.Store(true)
			if err == nil {
				return
			}
			if ctx.Err() == nil {
				if ferr := s.failTask(ctx, tk, err); ferr != nil {
					s.log.Warn("hf/download: could not record a task failure", "error", ferr)
				}
			}
			mu.Lock()
			errs = append(errs, fmt.Errorf("%s: %w", tk.file.Filename, err))
			mu.Unlock()
		}()
	}
	wg.Wait()

	if ctx.Err() != nil {
		return ctx.Err()
	}
	return errors.Join(errs...)
}

// filePool is D26's file-level semaphore, created on the first run because the
// width comes from a setting this service cannot read at construction.
func (s *Service) filePool(ctx context.Context) chan struct{} {
	s.filesOnce.Do(func() {
		width := 3
		if s.config != nil {
			if v, err := s.config.GetInt(ctx, KeyConcurrency); err == nil && v > 0 {
				width = int(v)
			}
		}
		s.files = make(chan struct{}, width)

		if s.config != nil {
			if v, err := s.config.GetInt(ctx, KeyRateLimit); err == nil && v > 0 {
				s.limit = newRateLimiter(v, s.now)
			}
		}
	})
	return s.files
}

func (s *Service) startTask(ctx context.Context, tk *task) error {
	now := s.now().UnixMilli()
	return s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		if _, err := s.store.SetDownloadTaskState(ctx, tx, tk.row.ID, model.TaskRunning,
			&now, nil, nil); err != nil {
			return err
		}
		if _, err := s.store.SetModelFileState(ctx, tx, tk.file.ID, model.FileDownloading, now); err != nil {
			return err
		}
		// A task moved, so the download's stored state is re-folded in the same
		// transaction (section 2.7). This is the write that makes the row
		// `running`.
		_, err := s.writeState(ctx, tx, tk.row.DownloadID, stateWrite{})
		return err
	})
}

func (s *Service) failTask(ctx context.Context, tk *task, cause error) error {
	now := s.now().UnixMilli()
	msg := cause.Error()
	return s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		if _, err := s.store.SetDownloadTaskState(ctx, tx, tk.row.ID, model.TaskFailed,
			nil, &now, &msg); err != nil {
			return err
		}
		// The fold decides whether one dead file makes the DOWNLOAD failed: with
		// another shard still transferring it does not, because `running` beats
		// `failed` in section 2.7's order.
		_, err := s.writeState(ctx, tx, tk.row.DownloadID, stateWrite{})
		return err
	})
}

// report is the 1 Hz progress loop.
func (s *Service) report(ctx context.Context, t *jobs.Task, id string,
	tasks []*task, dl store.Download, stop <-chan struct{}) {

	tick := time.NewTicker(ProgressTick)
	defer tick.Stop()

	var (
		speed     float64
		lastBytes int64
		lastAt    = s.now()
		lastWrite time.Time
		// resumedFrom is the sum of every task's starting offset. It is what
		// makes the ETA honest: the bytes already on disk are not bytes this run
		// moved, and dividing them into the elapsed time would report a rate no
		// link has ever achieved.
		resumedFrom int64
		haveStart   bool
	)

	emit := func() {
		var done int64
		for _, tk := range tasks {
			done += tk.progress.Load()
		}
		if !haveStart {
			for _, tk := range tasks {
				resumedFrom += tk.startedAt.Load()
			}
			haveStart = true
		}

		now := s.now()
		if elapsed := now.Sub(lastAt).Seconds(); elapsed > 0 {
			sample := float64(done-lastBytes) / elapsed
			if sample < 0 {
				sample = 0
			}
			if speed == 0 {
				speed = sample
			} else {
				speed = SpeedSmoothing*sample + (1-SpeedSmoothing)*speed
			}
			lastBytes, lastAt = done, now
		}

		total := dl.BytesTotal
		var eta *int64
		if speed > 1 && total > done {
			v := int64(float64(total-done) / speed)
			eta = &v
		}

		// The SSE patch, every tick.
		_ = t.SetProgress(ctx, ProgressPatch{
			DownloadID: id, BytesDone: done, BytesTotal: total,
			SpeedBPS: int64(speed), ETASec: eta, Files: fileProgress(tasks),
		})

		// The rows, every other tick.
		if now.Sub(lastWrite) < RowWriteEvery {
			return
		}
		lastWrite = now
		row := dl
		row.ID = id
		row.BytesDone = done
		row.BytesAtStart = resumedFrom
		row.SpeedBPS = int64(speed)
		row.ETASec = eta
		// Progress writes are best effort and their errors are deliberately
		// dropped: failing a 40 GB download because one counter UPDATE lost a
		// race would trade the whole transfer for a number that is rewritten a
		// second later.
		_ = s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
			if _, err := s.store.UpdateDownloadProgress(ctx, tx, row); err != nil {
				return err
			}
			for _, tk := range tasks {
				if tk.done.Load() {
					continue
				}
				if _, err := s.store.UpdateDownloadTaskProgress(ctx, tx, tk.row.ID,
					tk.progress.Load()); err != nil {
					return err
				}
			}
			return nil
		})
	}

	for {
		select {
		case <-stop:
			emit()
			return
		case <-ctx.Done():
			return
		case <-tick.C:
			emit()
		}
	}
}

// ProgressPatch is what a running download pushes over SSE, and what
// `jobs.progress_json` holds between pushes.
type ProgressPatch struct {
	DownloadID string `json:"download_id"`
	BytesDone  int64  `json:"bytes_done"`
	BytesTotal int64  `json:"bytes_total"`
	SpeedBPS   int64  `json:"speed_bps"`
	ETASec     *int64 `json:"eta_sec"`
	// Files is the per-file half of section 7.3: a sharded download shows five
	// bars, not one, because "47%" of a five-shard model tells a user nothing
	// about which shard is stuck.
	Files []FileProgress `json:"files"`
}

// FileProgress is one file's line in the progress panel.
type FileProgress struct {
	Filename   string `json:"filename"`
	ShardIndex int    `json:"shard_index"`
	ShardTotal int    `json:"shard_total"`
	BytesDone  int64  `json:"bytes_done"`
	BytesTotal int64  `json:"bytes_total"`
}

func fileProgress(tasks []*task) []FileProgress {
	out := make([]FileProgress, 0, len(tasks))
	for _, tk := range tasks {
		out = append(out, FileProgress{
			Filename:   tk.file.Filename,
			ShardIndex: tk.file.ShardIndex,
			ShardTotal: tk.file.ShardTotal,
			BytesDone:  tk.progress.Load(),
			BytesTotal: tk.row.BytesTotal,
		})
	}
	return out
}

// markModelsDownloading moves every model this run touches out of `planned`.
// A model already `ready` is left alone: its files are on disk and moving it
// backwards would make an instance that is happily running report a model that
// is not there.
func (s *Service) markModelsDownloading(ctx context.Context, tx store.Tx, tasks []*task) error {
	seen := map[string]bool{}
	now := s.now().UnixMilli()
	for _, tk := range tasks {
		if seen[tk.modelID] {
			continue
		}
		seen[tk.modelID] = true
		m, err := s.store.LocalModel(ctx, tx, tk.modelID)
		if err != nil {
			return err
		}
		if m.State == model.ModelReady {
			continue
		}
		if _, err := s.store.SetLocalModelState(ctx, tx, tk.modelID,
			model.ModelDownloading, now); err != nil {
			return err
		}
		// D69: `models.state` moved, so every referencing instance's
		// `config_hash` is recomputed in this same transaction.
		if err := s.recompute(ctx, tx, tk.modelID); err != nil {
			return err
		}
	}
	return nil
}

// verifyModels is the `verifying` step: every file of every model this download
// touched is confirmed present, the model's byte counters are refreshed from
// what actually landed, and the row moves to `ready`.
func (s *Service) verifyModels(ctx context.Context, id string, p Params) (
	modelIDs []string, total, onDisk int64, err error) {

	err = s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		views, err := s.store.DownloadTaskViews(ctx, tx, id)
		if err != nil {
			return err
		}
		seen := map[string]bool{}
		modelIDs = modelIDs[:0]
		total, onDisk = 0, 0
		for _, v := range views {
			if seen[v.ModelID] {
				continue
			}
			seen[v.ModelID] = true
			modelIDs = append(modelIDs, v.ModelID)
		}

		now := s.now().UnixMilli()
		for _, mid := range modelIDs {
			m, err := s.store.LocalModel(ctx, tx, mid)
			if err != nil {
				return err
			}
			files, err := s.store.ModelFiles(ctx, tx, mid)
			if err != nil {
				return err
			}
			var (
				sum   int64
				disk  int64
				ready = len(files) > 0
			)
			for _, f := range files {
				if f.State != model.FilePresent {
					ready = false
				}
				sum += f.SizeBytes
				disk += f.BytesOnDisk
			}
			if !ready {
				// A model with a file that is not present is `incomplete`, not
				// `ready`. It is a state the schema has for exactly this: the
				// bytes that landed are kept and the row says so.
				if _, err := s.store.SetLocalModelState(ctx, tx, mid,
					model.ModelIncomplete, now); err != nil {
					return err
				}
				if err := s.recompute(ctx, tx, mid); err != nil {
					return err
				}
				return fmt.Errorf("hf/download: %s is missing files after the transfer", m.RepoID)
			}
			m.TotalBytes = sum
			m.BytesOnDisk = disk
			m.ShardCount = len(files)
			m.State = model.ModelReady
			m.UpdatedAt = now
			if _, err := s.store.UpdateLocalModel(ctx, tx, m); err != nil {
				return err
			}
			// D69 again: `state` moved and `snapshot_dir`/`primary_file` are
			// now real paths.
			if err := s.recompute(ctx, tx, mid); err != nil {
				return err
			}
			total += sum
			onDisk += disk
		}
		return nil
	})
	return modelIDs, total, onDisk, err
}

// shouldDrop reports whether a cancel asked for this download's partials to be
// removed, and forgetDrop clears the note once the run is over.
func (s *Service) shouldDrop(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped[id]
}

func (s *Service) forgetDrop(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.dropped, id)
}

// failureFor names a failure in the vocabulary of errors.go, so
// `downloads.error_code` carries a word the UI can render rather than a Go error
// string.
func failureFor(err error) (code, message string) {
	msg := err.Error()
	var me model.Error
	if errors.As(err, &me) {
		return string(me.Code), me.Message
	}
	switch {
	case mentions(msg, ErrChecksumMismatch):
		return ErrChecksumMismatch, msg
	case mentions(msg, ErrSizeMismatch), isSizeMismatch(err):
		return ErrSizeMismatch, msg
	case mentions(msg, ErrLockTimeout):
		return ErrLockTimeout, msg
	case mentions(msg, ErrLinkFailed):
		return ErrLinkFailed, msg
	default:
		return ErrTransferFailed, msg
	}
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
