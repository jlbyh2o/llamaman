package bench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/jlbyh2o/llamaman/internal/hw"
	"github.com/jlbyh2o/llamaman/internal/instances"
	"github.com/jlbyh2o/llamaman/internal/jobs"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/procx"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// The bench runner's service half (DESIGN sections 3.13 and 10): what the API
// calls, what enqueues the `bench_run` job, and what the worker in worker.go
// reads its inputs back out of.
//
// Every collaborator is an interface this package declares rather than a
// concrete type it imports — D49 invariant 1 in practice, since only
// internal/store contains SQL — and three of them are legitimately nil on a
// running host: a daemon with no GPU prober cannot attribute VRAM (the guard
// then fails closed on every instance, which is the documented behavior), a
// daemon with no supervisor cannot drive a restore faster than the reconcile
// loop, and a host with no event sink still benchmarks.

// Store is the persistence this package needs. *store.Store satisfies it.
type Store interface {
	Read(ctx context.Context, fn func(context.Context, store.Tx) error) error
	Write(ctx context.Context, fn func(context.Context, store.Tx) error) error

	InsertBenchRun(ctx context.Context, tx store.Tx, r store.BenchRun) error
	BenchRun(ctx context.Context, tx store.Tx, id string) (store.BenchRun, error)
	BenchRuns(ctx context.Context, tx store.Tx, f store.BenchRunFilter) ([]store.BenchRun, error)
	BenchRunsOwingRestore(ctx context.Context, tx store.Tx) ([]store.BenchRun, error)
	SetBenchRunState(ctx context.Context, tx store.Tx, id string,
		state model.BenchRunState, at int64) (bool, error)
	FinishBenchRun(ctx context.Context, tx store.Tx, id string, state model.BenchRunState,
		done, failed int, errorMessage *string, at int64) (bool, error)
	SetBenchRunCounters(ctx context.Context, tx store.Tx, id string, done, failed int) (bool, error)
	SetBenchRunNotes(ctx context.Context, tx store.Tx, id, name string, notes *string) (bool, error)
	SetBenchRunStopped(ctx context.Context, tx store.Tx, id string, stoppedJSON *string) (bool, error)
	MarkBenchRestoreDone(ctx context.Context, tx store.Tx, id string) (bool, error)
	DeleteBenchRun(ctx context.Context, tx store.Tx, id string) (bool, error)

	InsertBenchPoint(ctx context.Context, tx store.Tx, p store.BenchPoint) error
	BenchPoints(ctx context.Context, tx store.Tx, runID string) ([]store.BenchPoint, error)
	SetBenchPointState(ctx context.Context, tx store.Tx, id string,
		state model.BenchPointState, errorMessage *string, at int64) (bool, error)
	SkipPendingBenchPoints(ctx context.Context, tx store.Tx, runID string, at int64) (int64, error)

	InsertBenchResult(ctx context.Context, tx store.Tx, r store.BenchResult) error
	BenchResults(ctx context.Context, tx store.Tx, runID string) ([]store.BenchResult, error)

	BenchCompare(ctx context.Context, tx store.Tx, q store.BenchCompareQuery) (
		[]store.BenchComparePoint, error)
	BenchSeries(ctx context.Context, tx store.Tx, q store.BenchSeriesQuery) (
		[]store.BenchSeriesPoint, error)
	BenchSecondsPerPoint(ctx context.Context, tx store.Tx, modelID string) (float64, int, error)

	BenchLease(ctx context.Context, tx store.Tx) (store.BenchLease, error)
	AcquireBenchLease(ctx context.Context, tx store.Tx, jobID, runID, owner string,
		at, expiresAt int64) (bool, error)
	TouchBenchLease(ctx context.Context, tx store.Tx, jobID, owner string, expiresAt int64) (bool, error)
	ReleaseBenchLease(ctx context.Context, tx store.Tx, jobID string) (bool, error)
	ReleaseForeignBenchLease(ctx context.Context, tx store.Tx, bootID string) (bool, error)
	BenchLive(ctx context.Context, tx store.Tx) (bool, error)

	InstanceViews(ctx context.Context, tx store.Tx, f store.InstanceFilter) (
		[]model.InstanceView, error)
	ModelRefsByID(ctx context.Context, tx store.Tx, ids []string) (map[string]store.ModelRef, error)
	ActiveVersion(ctx context.Context, tx store.Tx) (store.ActiveVersion, error)
	LlamacppVersion(ctx context.Context, tx store.Tx, id string) (store.LlamacppVersion, error)

	Jobs(ctx context.Context, tx store.Tx, f store.JobFilter) ([]model.Job, error)
	FinishJob(ctx context.Context, tx store.Tx, id string, state model.JobState,
		errorCode, errorMessage *string, at int64) error
}

// Queue is the job engine this service enqueues into. *jobs.Queue satisfies it.
type Queue interface {
	EnqueueTx(ctx context.Context, tx store.Tx, p jobs.EnqueueParams) (jobs.EnqueueResult, error)
	Cancel(ctx context.Context, id string) (model.Job, error)
	Wake()
}

// Events is the events/SSE seam. Append belongs inside the caller's write
// transaction; Publish runs only after it commits.
type Events interface {
	Append(ctx context.Context, tx store.Tx, ev model.Event) error
	Publish(ev model.Event)
}

// Settings reads `bench.exclusive_gpu` and `bench.default_repetitions` (§2.1).
type Settings interface {
	GetBool(ctx context.Context, key string) (bool, error)
	GetInt(ctx context.Context, key string) (int64, error)
}

// Fleet writes the DESIRED axis and lets the supervisor act, which is the same
// split every other restart in this system uses (§5.8) — and it is what makes
// the stop-and-restore's restart show up in `instance_starts` as
// `trigger='bench_restore'` rather than as an anonymous start.
// *instances.Service satisfies it.
type Fleet interface {
	SetDesiredState(ctx context.Context, id string, desired model.DesiredState,
		trigger model.PendingTrigger) (instances.View, error)
}

// Reconciler is the supervisor's own exported pass. The stop-and-restore asks
// for it so a bench proceeds at the pace of the stops rather than of the poll
// interval; a nil Reconciler simply waits for the loop, which is slower and
// equally correct.
type Reconciler interface {
	Reconcile(ctx context.Context) error
}

// GPUProber is the attribution source of §8.6/D17. A nil prober leaves the
// inventory empty, and the guard then has nothing to intersect: it reports that
// GPU identity is unavailable rather than silently allowing a collision.
type GPUProber interface {
	Probe(ctx context.Context) ([]hw.GPU, error)
}

// Runner executes one llama-bench invocation. procx.Run is the production
// implementation, wrapped by RunnerFunc; a test supplies a fake so the whole
// worker can be exercised without a GPU.
type Runner interface {
	Run(ctx context.Context, c procx.Cmd) (procx.Result, error)
}

// RunnerFunc adapts a function to Runner.
type RunnerFunc func(ctx context.Context, c procx.Cmd) (procx.Result, error)

// Run implements Runner.
func (f RunnerFunc) Run(ctx context.Context, c procx.Cmd) (procx.Result, error) { return f(ctx, c) }

// Config wires a Service.
type Config struct {
	Store    Store
	Queue    Queue
	Events   Events
	Settings Settings
	// Fleet is required for `on_conflict:"stop_and_restore"`. A nil Fleet makes
	// that policy unavailable — the preflight says so and the run is refused
	// rather than started with no way to put the instances back.
	Fleet Fleet
	// Supervisor drives the stop and the restore now rather than at the next
	// tick. Nil is supported.
	Supervisor Reconciler
	// GPUs is the attribution probe. Nil is supported and fails closed.
	GPUs GPUProber
	// Runner executes llama-bench. Nil uses procx.Run.
	Runner Runner

	// StateDir is `runtime_info.state_dir` (D72) — never a literal
	// /var/lib/llamaman. `<StateDir>/versions/<dir_name>/bin/llama-bench` is
	// what gets executed.
	StateDir string
	// BootID is `runtime_info.boot_id`, and it is what the D75 bench lease is
	// owned by: "this lease belongs to a boot that is gone" has to be a string
	// comparison at the next boot, so it must be THIS boot's id.
	BootID string

	// LeaseTTL is how far ahead `bench_lease.expires_at` is set; it is
	// refreshed on every progress write. Zero means DefaultLeaseTTL.
	LeaseTTL time.Duration
	// LeaseBeat is how often the worker re-extends that horizon while a point is
	// executing. Zero means LeaseTTL/LeaseBeatDivisor — comfortably inside the
	// horizon, so a couple of missed ticks still cannot let it lapse.
	LeaseBeat time.Duration
	// Retry is how long a sweep that could not take the lease waits before
	// asking again. Zero means DefaultBenchRetry, which is §10's 15 seconds.
	Retry time.Duration
	// RestorePoll is how often the restore re-reads the fleet while waiting for
	// an instance to go down or come back. Zero means DefaultRestorePoll.
	RestorePoll time.Duration
	// StopGrace bounds the wait for an instance to actually stop before the
	// sweep starts. Zero means DefaultStopGrace.
	StopGrace time.Duration

	// Now supplies every instant this service stamps. Nil uses time.Now.
	Now func() time.Time
	// NewID mints row and event ids. Nil uses store.NewID.
	NewID func(time.Time) string
	// Logger is the daemon's slog. Nil uses slog.Default.
	Logger *slog.Logger
}

// Defaults for the knobs section 10 does not pin by number.
const (
	// DefaultLeaseTTL is the bench lease horizon. It is longer than the build
	// lease's because one point of a sweep is one whole model load plus `-r`
	// repetitions, and a lease that lapses mid-point would let a second sweep in.
	DefaultLeaseTTL = 5 * time.Minute
	// LeaseBeatDivisor sets the default heartbeat to a third of the horizon, so
	// two consecutive missed ticks still leave the lease valid. The horizon is
	// NOT sized to cover a whole point — no number could, since a point is a
	// model load plus `-r` repetitions at depth and can run for an hour — which
	// is exactly why it is heartbeated rather than merely made generous.
	LeaseBeatDivisor = 3
	// DefaultBenchRetry is §10's "stays `queued` with `run_after = now + 15 s`"
	// — a queue, which is what a user expects, rather than a 409.
	DefaultBenchRetry = 15 * time.Second
	// DefaultRestorePoll is how often the stop-and-restore looks again.
	DefaultRestorePoll = time.Second
	// DefaultStopGrace bounds the wait for an instance to go down. A stop that
	// has not been observed within it does not fail the sweep: the instance is
	// recorded as stopped either way, so the restore still owes it a restart.
	DefaultStopGrace = 60 * time.Second
	// DefaultSecondsPerPoint is the duration estimate for a model class with no
	// history. It is a guess and the preflight labels it one.
	DefaultSecondsPerPoint = 45.0
)

// Service is the bench runner: the sweep builder's preflight, the run CRUD, the
// comparison and history queries, the export, and the boot finalizer.
type Service struct {
	store Store
	queue Queue
	// events, fleet, supervisor, gpus may be nil; each nil is documented where
	// it is read.
	events   Events
	settings Settings
	fleet    Fleet
	sup      Reconciler
	gpus     GPUProber
	runner   Runner

	stateDir string
	bootID   string

	leaseTTL    time.Duration
	leaseBeat   time.Duration
	retry       time.Duration
	restorePoll time.Duration
	stopGrace   time.Duration

	now   func() time.Time
	newID func(time.Time) string
	log   *slog.Logger
}

// New constructs a Service.
func New(cfg Config) (*Service, error) {
	switch {
	case cfg.Store == nil:
		return nil, errors.New("bench: a Store is required")
	case cfg.Queue == nil:
		return nil, errors.New("bench: a Queue is required")
	case cfg.StateDir == "":
		return nil, errors.New("bench: a StateDir is required")
	case cfg.BootID == "":
		return nil, errors.New("bench: a BootID is required — the D75 lease is owned by it")
	}

	s := &Service{
		store:       cfg.Store,
		queue:       cfg.Queue,
		events:      cfg.Events,
		settings:    cfg.Settings,
		fleet:       cfg.Fleet,
		sup:         cfg.Supervisor,
		gpus:        cfg.GPUs,
		runner:      cfg.Runner,
		stateDir:    cfg.StateDir,
		bootID:      cfg.BootID,
		leaseTTL:    cfg.LeaseTTL,
		leaseBeat:   cfg.LeaseBeat,
		retry:       cfg.Retry,
		restorePoll: cfg.RestorePoll,
		stopGrace:   cfg.StopGrace,
		now:         cfg.Now,
		newID:       cfg.NewID,
		log:         cfg.Logger,
	}
	if s.runner == nil {
		s.runner = RunnerFunc(procx.Run)
	}
	if s.leaseTTL <= 0 {
		s.leaseTTL = DefaultLeaseTTL
	}
	if s.leaseBeat <= 0 {
		s.leaseBeat = s.leaseTTL / LeaseBeatDivisor
	}
	if s.retry <= 0 {
		s.retry = DefaultBenchRetry
	}
	if s.restorePoll <= 0 {
		s.restorePoll = DefaultRestorePoll
	}
	if s.stopGrace <= 0 {
		s.stopGrace = DefaultStopGrace
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.newID == nil {
		s.newID = store.NewID
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	return s, nil
}

// BenchLive implements llamacpp.BenchGuard — §6.6 step 1's second refusal term,
// answered from the one row D75 made singular plus the restore predicate that
// makes the guard total.
func (s *Service) BenchLive(ctx context.Context, tx store.Tx) (bool, error) {
	return s.store.BenchLive(ctx, tx)
}

// -----------------------------------------------------------------------------
// Views
// -----------------------------------------------------------------------------

// View is one run as the API renders it.
type View struct {
	Run    store.BenchRun
	Points []store.BenchPoint
	// Job is the live or last job for this run, when there is one. It carries
	// the progress the SSE stream pushes.
	Job *model.Job
}

// ResultRow is one flattened row of `GET /bench/runs/{id}/results` and of the
// CSV export: the point's axes beside the result's numbers, which is the shape a
// table and a spreadsheet both want.
type ResultRow struct {
	RunID   string
	RunName string
	PointID string
	Ordinal int
	Label   string

	NGpuLayers  *int64
	NBatch      *int64
	NUbatch     *int64
	NThreads    *int64
	FlashAttn   *bool
	TypeK       *string
	TypeV       *string
	SplitMode   *string
	TensorSplit *string

	TestKind model.BenchTestKind
	NPrompt  int64
	NGen     int64
	NDepth   int64

	AvgTS    float64
	StddevTS float64
	AvgNS    int64
	StddevNS int64
	Samples  int
}

// List answers `GET /api/v1/bench/runs`.
func (s *Service) List(ctx context.Context, f store.BenchRunFilter) ([]store.BenchRun, error) {
	var out []store.BenchRun
	err := s.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		out, err = s.store.BenchRuns(ctx, tx, f)
		return err
	})
	return out, err
}

// Get answers `GET /api/v1/bench/runs/{id}` — the run, its points and the job
// carrying its progress.
func (s *Service) Get(ctx context.Context, id string) (View, error) {
	var v View
	err := s.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		run, err := s.store.BenchRun(ctx, tx, id)
		if err != nil {
			return err
		}
		points, err := s.store.BenchPoints(ctx, tx, id)
		if err != nil {
			return err
		}
		v = View{Run: run, Points: points}
		j, err := s.jobFor(ctx, tx, id)
		if err != nil {
			return err
		}
		v.Job = j
		return nil
	})
	return v, err
}

// Results answers `GET /api/v1/bench/runs/{id}/results`.
func (s *Service) Results(ctx context.Context, id string) ([]ResultRow, error) {
	var out []ResultRow
	err := s.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		run, err := s.store.BenchRun(ctx, tx, id)
		if err != nil {
			return err
		}
		points, err := s.store.BenchPoints(ctx, tx, id)
		if err != nil {
			return err
		}
		results, err := s.store.BenchResults(ctx, tx, id)
		if err != nil {
			return err
		}
		out = flattenResults(run, points, results)
		return nil
	})
	return out, err
}

// flattenResults joins the two lists in memory. The join is by point id over at
// most a few hundred rows, so it is a map lookup rather than a second query —
// and doing it here rather than in SQL keeps the point's LABEL, which is derived
// from the axes rather than stored, in one place.
func flattenResults(run store.BenchRun, points []store.BenchPoint,
	results []store.BenchResult) []ResultRow {

	byID := make(map[string]store.BenchPoint, len(points))
	for _, p := range points {
		byID[p.ID] = p
	}

	out := make([]ResultRow, 0, len(results))
	for _, r := range results {
		p := byID[r.PointID]
		row := ResultRow{
			RunID:       run.ID,
			RunName:     run.Name,
			PointID:     r.PointID,
			Ordinal:     p.Ordinal,
			Label:       PointLabel(p),
			NGpuLayers:  p.NGpuLayers,
			NBatch:      p.NBatch,
			NUbatch:     p.NUbatch,
			NThreads:    p.NThreads,
			FlashAttn:   p.FlashAttn,
			TypeK:       p.TypeK,
			TypeV:       p.TypeV,
			SplitMode:   p.SplitMode,
			TensorSplit: p.TensorSplit,
			TestKind:    r.TestKind,
			NPrompt:     r.NPrompt,
			NGen:        r.NGen,
			NDepth:      r.NDepth,
			AvgTS:       r.AvgTS,
			StddevTS:    r.StddevTS,
			AvgNS:       r.AvgNS,
			StddevNS:    r.StddevNS,
		}
		if r.SamplesJSON != nil {
			var samples []float64
			if err := json.Unmarshal([]byte(*r.SamplesJSON), &samples); err == nil {
				row.Samples = len(samples)
			}
		}
		out = append(out, row)
	}
	return out
}

// PointLabel reconstructs a point's human-readable cell from its stored columns.
// It is the same vocabulary Expand produces, derived rather than stored so that
// a column added later shows up in the label without a migration.
func PointLabel(p store.BenchPoint) string {
	var parts []string
	if p.NGpuLayers != nil {
		parts = append(parts, "ngl="+strconv.FormatInt(*p.NGpuLayers, 10))
	}
	if p.NBatch != nil {
		parts = append(parts, "b="+strconv.FormatInt(*p.NBatch, 10))
	}
	if p.NUbatch != nil {
		parts = append(parts, "ub="+strconv.FormatInt(*p.NUbatch, 10))
	}
	if p.NThreads != nil {
		parts = append(parts, "t="+strconv.FormatInt(*p.NThreads, 10))
	}
	if p.FlashAttn != nil {
		v := "0"
		if *p.FlashAttn {
			v = "1"
		}
		parts = append(parts, "fa="+v)
	}
	if p.TypeK != nil {
		parts = append(parts, "ctk="+*p.TypeK)
	}
	if p.TypeV != nil {
		parts = append(parts, "ctv="+*p.TypeV)
	}
	if p.SplitMode != nil {
		parts = append(parts, "sm="+*p.SplitMode)
	}
	if p.TensorSplit != nil {
		parts = append(parts, "ts="+*p.TensorSplit)
	}
	if p.NDepth != nil && *p.NDepth > 0 {
		parts = append(parts, "d="+strconv.FormatInt(*p.NDepth, 10))
	}
	if len(parts) == 0 {
		return "point " + strconv.Itoa(p.Ordinal)
	}
	return strings.Join(parts, " ")
}

// Compare answers `POST /api/v1/bench/compare`.
func (s *Service) Compare(ctx context.Context, q store.BenchCompareQuery) (
	[]store.BenchComparePoint, error) {

	var out []store.BenchComparePoint
	err := s.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		out, err = s.store.BenchCompare(ctx, tx, q)
		return err
	})
	return out, err
}

// Series answers `GET /api/v1/bench/series`.
func (s *Service) Series(ctx context.Context, q store.BenchSeriesQuery) (
	[]store.BenchSeriesPoint, error) {

	var out []store.BenchSeriesPoint
	err := s.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		out, err = s.store.BenchSeries(ctx, tx, q)
		return err
	})
	return out, err
}

// -----------------------------------------------------------------------------
// Preflight
// -----------------------------------------------------------------------------

// Preflight is `GET /api/v1/bench/preflight`: what a sweep would do before it is
// committed.
type Preflight struct {
	ModelID    string
	ModelLabel string
	ModelPath  string

	LlamacppVersionID string
	LlamacppTag       string
	RuntimeReady      bool

	// PointsTotal is the expanded cross-product size, and Estimate is
	// PointsTotal × the median seconds-per-point of prior runs against this
	// model. EstimateFromHistory says whether that median came from history or
	// from DefaultSecondsPerPoint, because an estimate and a guess deserve
	// different words on screen.
	PointsTotal         int
	Repetitions         int
	Estimate            time.Duration
	EstimateFromHistory bool

	// ExclusiveGPU is `bench.exclusive_gpu`. Conflicts is empty when it is off.
	ExclusiveGPU bool
	TargetGPUs   []string
	// GPUIdentityKnown reports whether this host could enumerate its GPUs at
	// all. When it is false the guard has nothing to intersect and every entry
	// in Conflicts is Assumed.
	GPUIdentityKnown bool
	Conflicts        []Occupancy
	FreeVRAMBytes    map[string]uint64

	// IgnoredFlags is §10.1's loud drop list: every FlagSet field llama-bench
	// has no equivalent for, with the reason. The sweep builder renders these
	// as a dismissible note above the estimate, so "why is my benchmark not
	// measuring my 32k context" is answered BEFORE the run rather than after it.
	IgnoredFlags []instances.IgnoredFlag
	// Notes is §10.1's substitution list — the `-fa auto → 1` and
	// `-ngl auto → 999` translations, which are made visibly rather than
	// silently.
	Notes []string
}

// PreflightRequest is what the endpoint is asked.
type PreflightRequest struct {
	ModelID     string
	Sweep       Sweep
	Repetitions int
}

// Preflight computes the answer without writing anything.
func (s *Service) Preflight(ctx context.Context, req PreflightRequest) (Preflight, error) {
	if req.ModelID == "" {
		return Preflight{}, errorf(model.CodeModelMissing,
			"a benchmark needs a model: pass ?model_id=")
	}
	if err := req.Sweep.Validate(); err != nil {
		return Preflight{}, err
	}

	points, err := Expand(req.Sweep)
	if err != nil {
		return Preflight{}, err
	}

	out := Preflight{
		ModelID:      req.ModelID,
		PointsTotal:  len(points),
		Repetitions:  req.Repetitions,
		IgnoredFlags: benchIgnoredFlags(req.Sweep),
		Notes:        benchNotes(req.Sweep),
	}

	base := model.FlagSet{}
	if req.Sweep.Base != nil {
		base = *req.Sweep.Base
	}

	gpus, inv := s.probeGPUs(ctx)
	out.GPUIdentityKnown = inv.Known()
	out.TargetGPUs = inv.Resolve(base)
	out.FreeVRAMBytes = freeVRAM(gpus, out.TargetGPUs)

	err = s.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		refs, err := s.store.ModelRefsByID(ctx, tx, []string{req.ModelID})
		if err != nil {
			return err
		}
		ref, ok := refs[req.ModelID]
		if !ok {
			return errorf(model.CodeModelMissing, "no model has id %s", req.ModelID)
		}
		out.ModelLabel = ref.ID
		if ref.State == model.ModelReady {
			out.ModelPath = filepath.Join(ref.SnapshotDir, ref.PrimaryFile)
		}

		active, err := s.store.ActiveVersion(ctx, tx)
		switch {
		case errors.Is(err, store.ErrNotFound):
			// No active build is an ordinary state on a fresh install. The
			// preflight reports it as `runtime_ready:false` rather than
			// failing, because the sweep builder is exactly where a user should
			// learn they need to install llama.cpp first.
		case err != nil:
			return err
		default:
			out.LlamacppVersionID = active.ID
			out.RuntimeReady = active.Ready()
			if v, err := s.store.LlamacppVersion(ctx, tx, active.ID); err == nil {
				out.LlamacppTag = v.Tag
			}
		}

		if s.settingBool(ctx, "bench.exclusive_gpu", true) {
			out.ExclusiveGPU = true
			views, err := s.store.InstanceViews(ctx, tx, store.InstanceFilter{})
			if err != nil {
				return err
			}
			out.Conflicts = Conflicts(out.TargetGPUs, views, inv)
		}

		secs, samples, err := s.store.BenchSecondsPerPoint(ctx, tx, req.ModelID)
		if err != nil {
			return err
		}
		if samples > 0 && secs > 0 {
			out.EstimateFromHistory = true
		} else {
			secs = DefaultSecondsPerPoint
		}
		out.Estimate = time.Duration(secs*float64(out.PointsTotal)) * time.Second
		return nil
	})
	return out, err
}

// freeVRAM projects the probe onto the target GPUs. A card whose memory the
// driver did not report is ABSENT from the map rather than present with zero:
// D16 forbids confusing "unknown" with "none", and a sweep builder that showed
// 0 MiB free would send the user to look for a leak that is not there.
func freeVRAM(gpus []hw.GPU, target []string) map[string]uint64 {
	want := make(map[string]struct{}, len(target))
	for _, uuid := range target {
		want[uuid] = struct{}{}
	}
	out := map[string]uint64{}
	for _, g := range gpus {
		if _, hit := want[g.UUID]; !hit || g.VRAMFreeBytes == nil {
			continue
		}
		out[g.UUID] = *g.VRAMFreeBytes
	}
	return out
}

// benchIgnoredFlags and benchNotes lift §10.1's two visibility lists off the
// sweep's base FlagSet, which is the only place a dropped flag can have been
// written.
func benchIgnoredFlags(s Sweep) []instances.IgnoredFlag {
	if s.Base == nil {
		return nil
	}
	// The second argument is `instances.extra_flags`, which a SWEEP never
	// carries: the sweep's own escape hatch is `bench.extra_flags`, and it is
	// passed through to llama-bench rather than dropped. An empty string is
	// therefore the honest input, not a placeholder.
	return instances.BenchIgnoredFlags(*s.Base, "")
}

func benchNotes(s Sweep) []string {
	var notes []string
	if s.Base != nil {
		notes = append(notes, instances.BenchNotes(*s.Base)...)
	}
	for _, v := range s.NGpuLayers {
		if v.Mode == model.NGLAuto {
			notes = append(notes, fmt.Sprintf("ngl=%d (auto): llama-bench has no --fit, so "+
				"\"let llama.cpp decide\" has no meaning here", instances.NGLAllValue))
			break
		}
	}
	return notes
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

// probeGPUs returns the probe's cards AND the inventory built from them, because
// the two answers a caller needs — "what did the driver say" and "did the driver
// answer at all" — cannot both be carried by a slice. A nil prober and a failed
// probe both yield UnknownGPUInventory, which makes the guard fail closed
// (section 10, F14); an EMPTY successful probe yields a known, empty inventory,
// which is a CPU-only host with nothing to be exclusive about.
func (s *Service) probeGPUs(ctx context.Context) ([]hw.GPU, GPUInventory) {
	if s.gpus == nil {
		s.log.Warn("bench: no GPU prober is wired; the exclusivity guard fails closed and " +
			"treats every loaded instance as a conflict")
		return nil, UnknownGPUInventory()
	}
	gpus, err := s.gpus.Probe(ctx)
	if err != nil {
		s.log.Warn("bench: could not probe the GPUs; the exclusivity guard fails closed and "+
			"treats every loaded instance as a conflict", "error", err)
		return nil, UnknownGPUInventory()
	}
	return gpus, NewGPUInventory(gpus)
}

func (s *Service) settingBool(ctx context.Context, key string, def bool) bool {
	if s.settings == nil {
		return def
	}
	v, err := s.settings.GetBool(ctx, key)
	if err != nil {
		return def
	}
	return v
}

func (s *Service) settingInt(ctx context.Context, key string, def int64) int64 {
	if s.settings == nil {
		return def
	}
	v, err := s.settings.GetInt(ctx, key)
	if err != nil {
		return def
	}
	return v
}

// jobFor returns the run's live job, or its most recent one. §2.3a pairs the two
// rows, and the UI reads progress off the job while reading state off the run.
func (s *Service) jobFor(ctx context.Context, tx store.Tx, runID string) (*model.Job, error) {
	rows, err := s.store.Jobs(ctx, tx, store.JobFilter{
		Kinds:       []model.JobKind{model.JobBenchRun},
		SubjectType: model.SubjectBenchRun,
		SubjectID:   runID,
		Limit:       1,
	})
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	j := rows[0]
	return &j, nil
}

// versionDir is `<state_dir>/versions/<dir_name>` (§6.1). The directory is
// resolved from the ROW rather than from the `versions/active` symlink, so a
// point's argv never depends on a filesystem read and a sweep that outlives an
// activation still names the build it measured.
func (s *Service) versionDir(dirName string) string {
	return filepath.Join(s.stateDir, "versions", dirName)
}

// hostFacts is §10's host half of the environment capture: CPU model and cores,
// RAM, kernel. Every lookup is best effort and an unreadable one is OMITTED
// rather than defaulted, because a `host_json` claiming 0 GB of RAM would make
// every comparison that reads it wrong in a way nobody would question.
func hostFacts() map[string]any {
	out := map[string]any{
		"arch":  runtime.GOARCH,
		"cores": runtime.NumCPU(),
	}
	if model := firstFieldAfter("/proc/cpuinfo", "model name"); model != "" {
		out["cpu"] = model
	}
	if kb := firstFieldAfter("/proc/meminfo", "MemTotal"); kb != "" {
		if n, err := strconv.ParseInt(strings.Fields(kb)[0], 10, 64); err == nil {
			out["ram_bytes"] = n * 1024
		}
	}
	if b, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		out["kernel"] = strings.TrimSpace(string(b))
	}
	return out
}

// firstFieldAfter returns the value of the first `key: value` line of a
// /proc file, or "" when the file or the key is absent.
func firstFieldAfter(path, key string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		name, value, ok := strings.Cut(line, ":")
		if ok && strings.TrimSpace(name) == key {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// gpuFacts is the GPU half of the same capture: names, UUIDs, VRAM, driver and
// compute capability. Cross-version comparisons are meaningless without it, so
// it is written onto the RUN rather than looked up when a chart is drawn — the
// card may not be in the host any more by then.
func gpuFacts(gpus []hw.GPU) []map[string]any {
	out := make([]map[string]any, 0, len(gpus))
	for _, g := range gpus {
		item := map[string]any{
			"index":       g.Index,
			"uuid":        g.UUID,
			"name":        g.Name,
			"driver":      g.DriverVersion,
			"compute_cap": g.ComputeCap,
		}
		if g.VRAMTotalBytes != nil {
			item["vram_total_bytes"] = *g.VRAMTotalBytes
		}
		if g.VRAMFreeBytes != nil {
			item["vram_free_bytes"] = *g.VRAMFreeBytes
		}
		out = append(out, item)
	}
	return out
}

// marshal renders a value for a NOT NULL json_valid column, falling back to an
// empty document rather than failing a run over a capture that would not encode.
func marshal(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}
