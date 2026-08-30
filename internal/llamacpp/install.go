package llamacpp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jlbyh2o/llamaman/internal/buildinfo"
	"github.com/jlbyh2o/llamaman/internal/events"
	"github.com/jlbyh2o/llamaman/internal/hw"
	"github.com/jlbyh2o/llamaman/internal/jobs"
	"github.com/jlbyh2o/llamaman/internal/llamacpp/github"
	"github.com/jlbyh2o/llamaman/internal/llamacpp/prebuilt"
	"github.com/jlbyh2o/llamaman/internal/llamacpp/source"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// The `llamacpp_install` worker: DESIGN sections 6.4 and 6.5, driven from the
// job queue.
//
// It owns three things the pipelines deliberately do not:
//
//   - the D70 build lease, acquired in the SAME transaction that moves the job
//     to `running`, because "one build at a time" cannot be expressed by
//     `idx_jobs_one_live_per_subject` (two installs of two different ids are two
//     subjects) and an in-process mutex is not shared with the next boot;
//   - the §2.5 state machine, which the pipelines report into but never write;
//   - D18's fallback, which is not a retry but a NEW ROW — a source build of the
//     same tag and backend whose prebuilt just failed to execute — linked to the
//     failed one through `superseded_by`, and possible only because D60 made the
//     identity three-part.

// PrebuiltInstaller runs §6.4's pipeline. prebuilt.Install is the production
// implementation, wrapped by PrebuiltFunc.
type PrebuiltInstaller interface {
	Install(ctx context.Context, req prebuilt.InstallRequest) (prebuilt.InstallResult, error)
}

// PrebuiltFunc adapts a function to PrebuiltInstaller, so the package function
// prebuilt.Install can be wired without a shim type at the call site.
type PrebuiltFunc func(ctx context.Context, req prebuilt.InstallRequest) (prebuilt.InstallResult, error)

// Install implements PrebuiltInstaller.
func (f PrebuiltFunc) Install(ctx context.Context, req prebuilt.InstallRequest) (
	prebuilt.InstallResult, error) {
	return f(ctx, req)
}

// SourceBuilder runs §6.5's pipeline. *source.Builder satisfies it.
type SourceBuilder interface {
	Build(ctx context.Context, req source.Request) (*source.Result, error)
	CanResume(versionID string) bool
	Discard(ctx context.Context, versionID string) error
}

// GPUProber supplies the compute capabilities a CUDA build compiles for (D21).
// A nil GPUProber leaves them to `llamacpp.cuda_arch_list`; when that is empty
// too, the source pipeline's preflight fails rather than guessing, which is
// exactly what D21 asks for — `all` multiplies compile time and `native`
// silently produces a binary that will not run if the GPU set changes.
type GPUProber interface {
	Probe(ctx context.Context) ([]hw.GPU, error)
}

// InstallWorkerConfig wires the worker's two pipelines.
type InstallWorkerConfig struct {
	// Prebuilt runs §6.4. Nil uses prebuilt.Install.
	Prebuilt PrebuiltInstaller
	// Source runs §6.5. It is required: every CUDA build, every custom build and
	// every D18 fallback goes through it, so a daemon without one could not
	// install anything on the commonest host there is.
	Source SourceBuilder
	// GPUs supplies D21's architectures. Nil is documented above.
	GPUs GPUProber
	// LeaseTTL is how far ahead `build_lease.expires_at` is set, and it is
	// refreshed on every progress write (§6.5). Zero means DefaultLeaseTTL.
	LeaseTTL time.Duration
	// Retry is how long a build that could not take the lease waits before
	// asking again. Zero means DefaultBuildRetry, which is §2.3's 15 seconds.
	Retry time.Duration
}

// Defaults for the two knobs §2.3 pins and §6.5 repeats.
const (
	// DefaultLeaseTTL is the build lease horizon. It only has to outlive one
	// progress write by a comfortable margin; a lapsed lease is reclaimable by
	// the next builder, and boot releases any lease this boot does not own.
	DefaultLeaseTTL = 2 * time.Minute
	// DefaultBuildRetry is §2.3's "stays `queued` with `run_after = now + 15 s`"
	// — a queue, which is what a user expects, rather than a 409.
	DefaultBuildRetry = 15 * time.Second
)

// InstallWorker runs `llamacpp_install`.
type InstallWorker struct {
	svc      *Service
	prebuilt PrebuiltInstaller
	source   SourceBuilder
	gpus     GPUProber
	leaseTTL time.Duration
	retry    time.Duration
}

// NewInstallWorker builds the worker. Register it with the job registry; the
// queue leases only registered kinds, so an unregistered install simply waits
// rather than being burned through its attempt budget.
func (s *Service) NewInstallWorker(cfg InstallWorkerConfig) (*InstallWorker, error) {
	if cfg.Source == nil {
		return nil, errors.New("llamacpp: an install worker needs a source builder")
	}
	if s.bootID == "" {
		return nil, errors.New("llamacpp: an install worker needs a BootID to own the build lease")
	}
	w := &InstallWorker{
		svc:      s,
		prebuilt: cfg.Prebuilt,
		source:   cfg.Source,
		gpus:     cfg.GPUs,
		leaseTTL: cfg.LeaseTTL,
		retry:    cfg.Retry,
	}
	if w.prebuilt == nil {
		w.prebuilt = PrebuiltFunc(prebuilt.Install)
	}
	if w.leaseTTL <= 0 {
		w.leaseTTL = DefaultLeaseTTL
	}
	if w.retry <= 0 {
		w.retry = DefaultBuildRetry
	}
	return w, nil
}

// Kind implements jobs.Worker.
func (w *InstallWorker) Kind() model.JobKind { return model.JobLlamacppInstall }

// Start implements jobs.Starter: the build lease and the row's move into
// `resolving` commit in the same transaction that moves the job to `running`.
//
// Zero rows changed on the lease is the D70 queue: the job goes back to `queued`
// for another try in fifteen seconds and the UI says "waiting for the running
// build". It spends no part of the attempt budget and wears no error, because it
// is not a failure.
func (w *InstallWorker) Start(ctx context.Context, tx store.Tx, j model.Job) error {
	var p installParams
	if err := decodeParams(j.ParamsJSON, &p); err != nil {
		return err
	}
	now := w.svc.now()
	ok, err := w.svc.store.AcquireBuildLease(ctx, tx, j.ID, p.VersionID, w.svc.bootID,
		now.UnixMilli(), now.Add(w.leaseTTL).UnixMilli())
	if err != nil {
		return err
	}
	if !ok {
		return jobs.Defer(w.retry)
	}
	_, err = w.svc.store.SetLlamacppVersionState(ctx, tx, p.VersionID,
		model.VersionResolving, now.UnixMilli())
	return err
}

// SetDomainState implements jobs.DomainWriter for the three transitions the
// QUEUE performs with no worker running (§2.3).
//
// `interrupted` is a deliberate NO-OP and is the whole point of §2.3's second
// bucket: D4 keeps the build directory warm, and the row's build state is what
// Retry reuses. Every terminal state releases the build lease, because a lease
// held by a job that is over would make the next build wait for its expiry for
// no reason.
func (w *InstallWorker) SetDomainState(ctx context.Context, tx store.Tx, j model.Job,
	state model.JobState) error {

	var p installParams
	if err := decodeParams(j.ParamsJSON, &p); err != nil {
		return err
	}
	now := w.svc.now().UnixMilli()

	switch state {
	case model.JobInterrupted:
		return nil
	case model.JobQueued:
		// A retry: the row starts again from `pending`, which is the state the
		// §2.5 table pairs with a queued install.
		//
		// For a row in one of the terminal-failure states that is the table's
		// REUSE-AND-RESET (D71), so the error columns, `failing_step`,
		// `exit_code` and `superseded_by` are cleared with it — a row that came
		// back to `pending` still wearing last week's `failed_verification`
		// diagnosis would render as a build that is both queued and broken.
		// ResetLlamacppVersion is scoped to exactly those states plus `ready`,
		// so a row that was merely `building` when the daemon stopped (D4's
		// interrupted case) matches nothing there and takes the plain state
		// move below — which is right: it has nothing to clear, and its warm
		// build directory is the whole point of the retry.
		ok, err := w.svc.store.ResetLlamacppVersion(ctx, tx, p.VersionID, now)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		_, err = w.svc.store.SetLlamacppVersionState(ctx, tx, p.VersionID,
			model.VersionPending, now)
		return err
	}

	if _, err := w.svc.store.ReleaseBuildLease(ctx, tx, j.ID); err != nil {
		return err
	}
	switch state {
	case model.JobCanceled:
		_, err := w.svc.store.SetLlamacppVersionState(ctx, tx, p.VersionID,
			model.VersionCanceled, now)
		return err
	case model.JobFailed:
		_, err := w.svc.store.FailLlamacppVersion(ctx, tx, p.VersionID, store.LlamacppFailure{
			State:        model.VersionFailed,
			ErrorCode:    strPtr(string(model.CodeDaemonRestarted)),
			ErrorMessage: strPtr("the daemon stopped while this build was queued"),
		}, now)
		return err
	}
	return nil
}

// Run implements jobs.Worker.
func (w *InstallWorker) Run(ctx context.Context, t *jobs.Task) (jobs.Outcome, error) {
	j := t.Job()
	var p installParams
	if err := decodeParams(j.ParamsJSON, &p); err != nil {
		return jobs.Outcome{}, err
	}

	if err := w.resolveLate(ctx, &p); err != nil {
		return w.failed(j.ID, p, model.VersionFailed, model.FailingStep(""), CodeResolveFailed,
			err.Error(), nil), nil
	}

	if p.Acquisition == model.AcquisitionPrebuilt {
		return w.runPrebuilt(ctx, t, p)
	}
	return w.runSource(ctx, t, p)
}

// resolveLate fills in what a row enqueued without a resolution is missing —
// the D18 fallback's own job, above all, which this worker enqueued from inside
// a failure transaction with nothing but a tag and a backend.
func (w *InstallWorker) resolveLate(ctx context.Context, p *installParams) error {
	if p.Acquisition != model.AcquisitionPrebuilt || p.AssetURL != "" {
		return nil
	}
	if w.svc.rel == nil {
		return errors.New("no release client is wired, so the prebuilt asset cannot be resolved")
	}
	res, _, err := w.svc.rel.Resolve(ctx, github.ResolveRequest{
		Channel: p.Channel, Tag: p.Tag, GOARCH: w.svc.goarch,
	})
	if err != nil {
		return err
	}
	if !res.AssetFound {
		return fmt.Errorf("release %s carries no prebuilt asset for this architecture", p.Tag)
	}
	p.AssetName = res.Asset.Name
	p.AssetURL = res.Asset.DownloadURL
	p.AssetReleaseTag = res.AssetRelease
	if sum, ok := res.Asset.SHA256(); ok {
		p.AssetSHA256 = sum
	}
	return nil
}

// runPrebuilt is §6.4.
func (w *InstallWorker) runPrebuilt(ctx context.Context, t *jobs.Task, p installParams) (
	jobs.Outcome, error) {

	if err := w.setState(ctx, t, p.VersionID, model.VersionFetching); err != nil {
		return jobs.Outcome{}, err
	}
	// §2.5's `fetching → verifying` edge, taken when the pipeline reaches its
	// D18 execute-on-this-host check. Without it the row would go straight from
	// `fetching` to `ready` or to `failed_verification` — two edges the
	// transition table does not define — and the UI would never be able to say
	// "extracted; checking that it runs here", which on a host where the answer
	// is no is the minute that explains the fallback.
	verifying := w.enterVerifying(ctx, t, p.VersionID)

	res, err := w.prebuilt.Install(ctx, prebuilt.InstallRequest{
		VersionID:       p.VersionID,
		Tag:             p.Tag,
		BuildTag:        p.BuildTag,
		Channel:         p.Channel,
		Backend:         p.Backend,
		AssetName:       p.AssetName,
		AssetURL:        p.AssetURL,
		PublishedSHA256: p.AssetSHA256,
		AssetReleaseTag: p.AssetReleaseTag,
		VersionsRoot:    w.svc.layout.VersionsRoot(),
		TmpDir:          w.svc.layout.TmpDir(),
		Guard:           w.svc.guard,
		BuiltBy:         buildinfo.Version,
		Now:             w.svc.now,
		Progress: func(step model.FailingStep, done, total int64, note string) {
			if step == model.StepVerify {
				verifying()
			}
			w.progress(ctx, t, p.VersionID, string(step), done, total, note)
		},
	})

	switch {
	case err == nil:
		return w.succeeded(t.Job().ID, p, store.LlamacppInstallResult{
			SizeBytes:     &res.SizeBytes,
			BinariesJSON:  jsonPtr(res.Manifest.Binaries),
			DevicesOutput: strPtrOrNil(res.Verify.DevicesOutput),
			HelpFlagsJSON: jsonPtr(res.Verify.HelpFlags),
			SupportsFit:   res.Verify.SupportsFit,
		}), nil

	case ctx.Err() != nil && t.CancelRequested():
		_ = prebuilt.CleanStaging(w.svc.layout.StagingDir(p.VersionID))
		return w.canceled(t.Job().ID, p), nil

	case res.SourceFallback:
		// D18: the tarball will not execute on this host. The row is KEPT as the
		// record of why, and a SOURCE build of the same tag and backend is
		// enqueued beside it — an insert that a two-part identity could not have
		// made, because the failed row already held the key (D60).
		return w.fallbackToSource(t.Job().ID, p, res), nil

	default:
		return w.failed(t.Job().ID, p, model.VersionFailed, res.FailingStep, CodeBuildFailed,
			err.Error(), nil), nil
	}
}

// runSource is §6.5.
func (w *InstallWorker) runSource(ctx context.Context, t *jobs.Task, p installParams) (
	jobs.Outcome, error) {

	if err := w.setState(ctx, t, p.VersionID, model.VersionBuilding); err != nil {
		return jobs.Outcome{}, err
	}
	// §2.5's `building → verifying` edge — the same rule the prebuilt path
	// applies, taken when the pipeline announces its `verify` phase (D18's
	// execute check plus, for CUDA, D19's device list).
	verifying := w.enterVerifying(ctx, t, p.VersionID)

	req := source.Request{
		VersionID:       p.VersionID,
		Tag:             p.Tag,
		BuildTag:        p.BuildTag,
		Channel:         p.Channel,
		GitURL:          p.GitURL,
		GitRef:          fetchRef(p),
		Backend:         p.Backend,
		ExtraCMakeFlags: p.ExtraCMake,
		Resume:          w.source.CanResume(p.VersionID),
		Observer: source.ObserverFunc(func(ctx context.Context, pr source.Progress) error {
			if pr.Phase == source.PhaseVerify {
				verifying()
			}
			w.progressJSON(ctx, t, p.VersionID, pr)
			return nil
		}),
	}
	if err := w.fillBuildInputs(ctx, p, &req); err != nil {
		return w.failed(t.Job().ID, p, model.VersionFailed, model.StepPreflight, CodeBuildFailed,
			err.Error(), nil), nil
	}

	res, err := w.source.Build(ctx, req)
	switch {
	case err == nil:
		return w.succeeded(t.Job().ID, p, store.LlamacppInstallResult{
			ResolvedCommit: strPtrOrNil(res.ResolvedCommit),
			SizeBytes:      &res.SizeBytes,
			BinariesJSON:   jsonPtr(res.Binaries),
			DevicesOutput:  strPtrOrNil(res.Verification.DevicesOutput),
			HelpFlagsJSON:  jsonPtr(res.Verification.HelpFlags),
			SupportsFit:    res.Verification.SupportsFit,
			BuildOptions:   jsonPtr(res.BuildOptions),
			CUDAArchList:   strPtrOrNil(res.CUDAArchList),
			HostCPUFlags:   strPtrOrNil(res.HostCPUFlags),
			GPUUUIDsJSON:   jsonPtr(res.GPUUUIDs),
			LogPath:        strPtrOrNil(res.LogPath),
		}), nil

	case ctx.Err() != nil && t.CancelRequested():
		// §6.5's cancellation: the worktree and the partial staging tree go, so
		// a canceled build leaves no half-installed directory behind.
		_ = w.source.Discard(context.WithoutCancel(ctx), p.VersionID)
		return w.canceled(t.Job().ID, p), nil

	default:
		if f, ok := source.AsFailure(err); ok {
			var exit *int64
			if f.ExitCode != 0 {
				e := int64(f.ExitCode)
				exit = &e
			}
			return w.failed(t.Job().ID, p, f.VersionState(), f.FailingStep(),
				model.ErrorCode(f.Code), f.Message, exit), nil
		}
		return w.failed(t.Job().ID, p, model.VersionFailed, model.FailingStep(""), CodeBuildFailed,
			err.Error(), nil), nil
	}
}

// fillBuildInputs supplies D20's parallelism and D21's architectures.
func (w *InstallWorker) fillBuildInputs(ctx context.Context, p installParams,
	req *source.Request) error {

	if w.svc.settings != nil {
		n, err := w.svc.settings.GetInt(ctx, "llamacpp.build_jobs")
		if err == nil {
			req.Jobs = int(n)
		}
	}
	if p.Backend != model.BackendCUDA {
		return nil
	}

	if p.CUDAArchList != "" {
		archs, err := source.ParseCUDAArchList(p.CUDAArchList)
		if err != nil {
			return fmt.Errorf("llamacpp.cuda_arch_list: %w", err)
		}
		req.CUDAArchs = archs
	}
	if w.gpus == nil {
		return nil
	}
	gpus, err := w.gpus.Probe(ctx)
	if err != nil {
		// A probe that could not run is not a reason to fail a build that was
		// given its architectures explicitly.
		if len(req.CUDAArchs) > 0 {
			return nil
		}
		return fmt.Errorf("could not detect the GPUs to compile for: %w", err)
	}
	req.GPUs = gpus
	if len(req.CUDAArchs) == 0 {
		req.CUDAArchs = source.CUDAArchsFromGPUs(gpus)
	}
	return nil
}

// fetchRef is what the source pipeline checks out. On the stable channel that is
// the PINNED build, not the semver tag: `v0.3.0` is a release whose
// nightly-tag.txt names `b10621`, and `b10621` is what is actually built (§6.2).
func fetchRef(p installParams) string {
	switch {
	case p.Commit != "":
		return p.Commit
	case p.GitRef != "":
		return p.GitRef
	case p.BuildTag != "":
		return p.BuildTag
	default:
		return p.Tag
	}
}

// setState moves the version row through the finer states §2.3a folds to
// `running`, inside the queue's own write transaction.
func (w *InstallWorker) setState(ctx context.Context, t *jobs.Task, id string,
	state model.VersionState) error {

	now := w.svc.now()
	var sink events.Sink
	if err := t.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		if _, err := w.svc.store.SetLlamacppVersionState(ctx, tx, id, state,
			now.UnixMilli()); err != nil {
			return err
		}
		return w.svc.event(ctx, tx, now, id, "llamacpp_version_state", model.LevelInfo,
			fmt.Sprintf("llama.cpp %s is %s", id, state), nil, ptr(string(state)), &sink)
	}); err != nil {
		return err
	}
	// AFTER the commit, never inside it. These are the transitions the wizard's
	// llama.cpp step and the versions list narrate — `fetching`, `building`,
	// `verifying` — and a row that moved without a frame is a screen that says
	// "Working" until someone reloads it.
	w.svc.publish(&sink)
	return nil
}

// enterVerifying returns a function that moves the row into `verifying` the
// first time it is called, and does nothing on every call after that.
//
// §2.5's table gives `verifying` two entry edges — "hardened extract ok" from
// `fetching`, "compile + install exit 0" from `building` — and two exits,
// `ready` and `failed_verification`. Both pipelines announce the boundary
// already: the prebuilt one reports `model.StepVerify`, the source one reports
// `source.PhaseVerify`, each at the moment D18's execute-on-this-host check
// begins. This turns that announcement into the row transition the table
// defines, and it is why `failed_verification` is now reached FROM `verifying`
// rather than from `fetching`.
//
// It is once-only because a phase can report progress more than once, and it is
// sync.Once rather than a bare bool because a pipeline is free to report from a
// goroutine of its own. A failed transition is logged and not returned: a build
// that reached its verification step must not be abandoned because a
// bookkeeping write lost a race.
func (w *InstallWorker) enterVerifying(ctx context.Context, t *jobs.Task, id string) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			if err := w.setState(ctx, t, id, model.VersionVerifying); err != nil {
				w.svc.log.Warn("could not record the verifying state",
					"version_id", id, "error", err)
			}
		})
	}
}

// progress writes one frame and refreshes the build lease, which §6.5 pairs
// deliberately: a build that is reporting progress is a build that is alive.
func (w *InstallWorker) progress(ctx context.Context, t *jobs.Task, id, phase string,
	done, total int64, note string) {

	w.progressJSON(ctx, t, id, map[string]any{
		"phase": phase, "done": done, "total": total, "message": note,
	})
}

func (w *InstallWorker) progressJSON(ctx context.Context, t *jobs.Task, id string, v any) {
	if err := t.SetProgress(ctx, v); err != nil {
		w.svc.log.Debug("could not write build progress", "version_id", id, "error", err)
	}
	now := w.svc.now()
	err := t.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		_, err := w.svc.store.TouchBuildLease(ctx, tx, t.Job().ID, w.svc.bootID,
			now.Add(w.leaseTTL).UnixMilli())
		return err
	})
	if err != nil {
		w.svc.log.Debug("could not extend the build lease", "version_id", id, "error", err)
	}
}

// succeeded is §2.5's `verifying → ready` edge.
func (w *InstallWorker) succeeded(jobID string, p installParams,
	r store.LlamacppInstallResult) jobs.Outcome {

	now := w.svc.now()
	sink := &events.Sink{}
	out := jobs.Succeeded(func(ctx context.Context, tx store.Tx, _ model.JobState) error {
		if _, err := w.svc.store.CompleteLlamacppInstall(ctx, tx, p.VersionID, r,
			now.UnixMilli()); err != nil {
			return err
		}
		if _, err := w.svc.store.ReleaseBuildLease(ctx, tx, jobID); err != nil {
			return err
		}
		return w.svc.event(ctx, tx, now, p.VersionID, "llamacpp_version_ready",
			model.LevelInfo, fmt.Sprintf("llama.cpp %s is ready", p.VersionID),
			nil, ptr(string(model.VersionReady)), sink)
	})
	out.AfterCommit = func() { w.svc.publish(sink) }
	return out
}

// canceled is §2.5's cancel edge.
func (w *InstallWorker) canceled(jobID string, p installParams) jobs.Outcome {
	now := w.svc.now()
	sink := &events.Sink{}
	out := jobs.Canceled(func(ctx context.Context, tx store.Tx, _ model.JobState) error {
		if _, err := w.svc.store.SetLlamacppVersionState(ctx, tx, p.VersionID,
			model.VersionCanceled, now.UnixMilli()); err != nil {
			return err
		}
		if _, err := w.svc.store.ReleaseBuildLease(ctx, tx, jobID); err != nil {
			return err
		}
		return w.svc.event(ctx, tx, now, p.VersionID, "llamacpp_version_canceled",
			model.LevelInfo, fmt.Sprintf("llama.cpp %s was canceled", p.VersionID),
			nil, ptr(string(model.VersionCanceled)), sink)
	})
	out.AfterCommit = func() { w.svc.publish(sink) }
	return out
}

// failed keeps the log and the failing step, which is what makes a Retry a warm
// rerun rather than a fresh start (D4).
func (w *InstallWorker) failed(jobID string, p installParams, state model.VersionState,
	step model.FailingStep, code model.ErrorCode, message string, exit *int64) jobs.Outcome {

	now := w.svc.now()
	var stepPtr *model.FailingStep
	if step != "" {
		stepPtr = &step
	}
	sink := &events.Sink{}
	out := jobs.Failed(string(code), message, func(ctx context.Context, tx store.Tx,
		_ model.JobState) error {

		if _, err := w.svc.store.FailLlamacppVersion(ctx, tx, p.VersionID, store.LlamacppFailure{
			State:        state,
			FailingStep:  stepPtr,
			ErrorCode:    strPtr(string(code)),
			ErrorMessage: strPtr(message),
			ExitCode:     exit,
			LogPath:      strPtrOrNil(w.svc.layout.LogPath(p.VersionID)),
		}, now.UnixMilli()); err != nil {
			return err
		}
		if _, err := w.svc.store.ReleaseBuildLease(ctx, tx, jobID); err != nil {
			return err
		}
		return w.svc.event(ctx, tx, now, p.VersionID, "llamacpp_version_failed",
			model.LevelError, message, nil, ptr(string(state)), sink)
	})
	out.AfterCommit = func() { w.svc.publish(sink) }
	return out
}

// fallbackToSource is D18's second half: the prebuilt row is kept as
// `failed_verification`, a `<tag>-<backend>-src` row is inserted beside it, the
// two are linked, and a fresh install job is enqueued — all in the transaction
// that closes this job, so a crash cannot leave a superseded row pointing at a
// build nobody is making.
func (w *InstallWorker) fallbackToSource(jobID string, p installParams,
	res prebuilt.InstallResult) jobs.Outcome {

	now := w.svc.now()
	// The diagnosis is what makes the replacement row legible — "requires
	// GLIBC_2.38, host has 2.36" rather than a raw loader error — but D18's
	// fallback is not conditional on having found one: a tarball that will not
	// execute is a tarball that will not execute.
	diagnosis := "the prebuilt tarball would not execute on this host"
	if res.Verify.Diagnosis != nil && res.Verify.Diagnosis.Summary != "" {
		diagnosis = res.Verify.Diagnosis.Summary
	}
	srcID := VersionID(p.Tag, p.Backend, model.AcquisitionSource)

	// Both rows the fallback writes — the prebuilt that was rejected and the
	// source build enqueued in its place — go out together once this closes.
	sink := &events.Sink{}
	out := jobs.Failed(string(CodeVerificationFailed), diagnosis,
		func(ctx context.Context, tx store.Tx, _ model.JobState) error {
			if _, err := w.svc.store.FailLlamacppVersion(ctx, tx, p.VersionID,
				store.LlamacppFailure{
					State:        model.VersionFailedVerification,
					FailingStep:  ptr(model.StepVerify),
					ErrorCode:    strPtr(string(CodeVerificationFailed)),
					ErrorMessage: strPtr(diagnosis),
				}, now.UnixMilli()); err != nil {
				return err
			}
			if _, err := w.svc.store.ReleaseBuildLease(ctx, tx, jobID); err != nil {
				return err
			}

			next := installParams{
				VersionID:    srcID,
				Channel:      p.Channel,
				Tag:          p.Tag,
				BuildTag:     p.BuildTag,
				Backend:      p.Backend,
				Acquisition:  model.AcquisitionSource,
				GitURL:       p.GitURL,
				GitRef:       p.GitRef,
				Commit:       p.Commit,
				ExtraCMake:   p.ExtraCMake,
				CUDAArchList: p.CUDAArchList,
				Diagnosis:    diagnosis,
				SupersedesID: p.VersionID,
			}
			if _, err := w.svc.queue.EnqueueTx(ctx, tx, jobs.EnqueueParams{
				Kind:     model.JobLlamacppInstall,
				DomainID: srcID,
				Params:   next,
				Domain: func(ctx context.Context, tx store.Tx, _ model.Job) error {
					return w.svc.upsertFallbackRow(ctx, tx, now, next, sink)
				},
			}); err != nil {
				return err
			}
			if _, err := w.svc.store.SetLlamacppSupersededBy(ctx, tx, p.VersionID,
				srcID); err != nil {
				return err
			}
			return w.svc.event(ctx, tx, now, p.VersionID, "llamacpp_version_superseded",
				model.LevelWarn,
				fmt.Sprintf("the %s prebuilt was rejected (%s) — building %s from source instead",
					p.Tag, diagnosis, p.Tag),
				nil, ptr(string(model.VersionFailedVerification)), sink)
		})
	out.AfterCommit = func() { w.svc.publish(sink) }
	return out
}

// upsertFallbackRow inserts the replacement row, or resets one left over from an
// earlier attempt at the same fallback.
func (s *Service) upsertFallbackRow(ctx context.Context, tx store.Tx, now time.Time,
	p installParams, sink *events.Sink) error {

	existing, err := s.store.LlamacppVersion(ctx, tx, p.VersionID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		row := store.LlamacppVersion{
			ID:          p.VersionID,
			Channel:     p.Channel,
			Tag:         p.Tag,
			GitURL:      p.GitURL,
			Acquisition: model.AcquisitionSource,
			Backend:     p.Backend,
			DirName:     p.VersionID,
			State:       model.VersionPending,
			SupportsFit: true,
			CreatedAt:   now.UnixMilli(),
		}
		if p.BuildTag != "" {
			row.BuildTag = &p.BuildTag
		}
		if p.CUDAArchList != "" {
			row.CUDAArchList = &p.CUDAArchList
		}
		if err := s.store.InsertLlamacppVersion(ctx, tx, row); err != nil {
			return err
		}
	case err != nil:
		return err
	default:
		if _, err := s.store.ResetLlamacppVersion(ctx, tx, existing.ID,
			now.UnixMilli()); err != nil {
			return err
		}
	}
	return s.event(ctx, tx, now, p.VersionID, "llamacpp_version_created", model.LevelInfo,
		fmt.Sprintf("llama.cpp %s was enqueued to replace a prebuilt that would not run here",
			p.VersionID),
		nil, ptr(string(model.VersionPending)), sink)
}

func strPtr(s string) *string { return &s }

func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// jsonPtr marshals a value into a *string for a json_valid column. A value that
// will not marshal writes SQL NULL rather than failing the transition: the
// column is a projection, and losing it must never lose the build.
func jsonPtr(v any) *string {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	s := string(b)
	return &s
}
