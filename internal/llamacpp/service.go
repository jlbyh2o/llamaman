package llamacpp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/jlbyh2o/llamaman/internal/instances"
	"github.com/jlbyh2o/llamaman/internal/jobs"
	"github.com/jlbyh2o/llamaman/internal/llamacpp/github"
	"github.com/jlbyh2o/llamaman/internal/llamacpp/source"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
	"github.com/jlbyh2o/llamaman/internal/toolchain"
)

// The version lifecycle service (DESIGN sections 2.5, 6.2, 6.6): what the API
// calls, and what enqueues every job the workers in this package run.
//
// Every collaborator below is an interface this package declares rather than a
// concrete type it imports — D49 invariant 1 in practice, since only
// internal/store contains SQL — and three of them are legitimately nil on a
// running host: a daemon with no bench subsystem has no bench to be live, a
// daemon with no notification sink still activates builds, and a host whose
// GitHub client was never constructed can still install a tag it was given.
// Each nil is documented where it is read.

// Store is the persistence this package needs. *store.Store satisfies it.
type Store interface {
	Read(ctx context.Context, fn func(context.Context, store.Tx) error) error
	Write(ctx context.Context, fn func(context.Context, store.Tx) error) error

	LlamacppVersion(ctx context.Context, tx store.Tx, id string) (store.LlamacppVersion, error)
	LlamacppVersions(ctx context.Context, tx store.Tx, f store.LlamacppVersionFilter) (
		[]store.LlamacppVersion, error)
	LlamacppVersionByFlag(ctx context.Context, tx store.Tx, previous bool) (
		store.LlamacppVersion, error)
	InsertLlamacppVersion(ctx context.Context, tx store.Tx, v store.LlamacppVersion) error
	SetLlamacppVersionState(ctx context.Context, tx store.Tx, id string,
		state model.VersionState, at int64) (bool, error)
	CompleteLlamacppInstall(ctx context.Context, tx store.Tx, id string,
		r store.LlamacppInstallResult, at int64) (bool, error)
	FailLlamacppVersion(ctx context.Context, tx store.Tx, id string,
		f store.LlamacppFailure, at int64) (bool, error)
	ResetLlamacppVersion(ctx context.Context, tx store.Tx, id string, at int64) (bool, error)
	SetLlamacppBuildRequest(ctx context.Context, tx store.Tx, id string,
		buildOptionsJSON, cudaArchList *string) (bool, error)
	SetLlamacppSupersededBy(ctx context.Context, tx store.Tx, id, by string) (bool, error)

	ActivateLlamacppVersion(ctx context.Context, tx store.Tx, targetID string,
		keepPrevious bool, at int64) (store.Activation, error)
	RestoreLlamacppFlags(ctx context.Context, tx store.Tx, before []store.LlamacppFlags) error

	AcquireBuildLease(ctx context.Context, tx store.Tx, jobID, versionID, owner string,
		at, expiresAt int64) (bool, error)
	TouchBuildLease(ctx context.Context, tx store.Tx, jobID, owner string, expiresAt int64) (bool, error)
	ReleaseBuildLease(ctx context.Context, tx store.Tx, jobID string) (bool, error)
	ReleaseForeignBuildLease(ctx context.Context, tx store.Tx, bootID string) (bool, error)

	CountLiveJobsByKind(ctx context.Context, tx store.Tx, kind model.JobKind) (int, error)
	SetJobParams(ctx context.Context, tx store.Tx, id string, paramsJSON *string) error
	Jobs(ctx context.Context, tx store.Tx, f store.JobFilter) ([]model.Job, error)
	FinishJob(ctx context.Context, tx store.Tx, id string, state model.JobState,
		errorCode, errorMessage *string, at int64) error

	InstanceViews(ctx context.Context, tx store.Tx, f store.InstanceFilter) (
		[]model.InstanceView, error)
}

// Queue is the job engine this service enqueues into. *jobs.Queue satisfies it.
//
// Cancel and Retry are here rather than being reached through
// `POST /api/v1/jobs/{id}/…` because §3.5 gives them their own routes on the
// VERSION: a user cancels a build, not a job id they never saw.
type Queue interface {
	EnqueueTx(ctx context.Context, tx store.Tx, p jobs.EnqueueParams) (jobs.EnqueueResult, error)
	Cancel(ctx context.Context, id string) (model.Job, error)
	Retry(ctx context.Context, id string) (model.Job, error)
	Wake()
}

// ToolchainProber probes this host for `GET /api/v1/llamacpp/plan` (§6.3). It is
// the SAME probe the wizard's toolchain step and the source pipeline's
// `preflight` phase read, which is what makes a plan and a build unable to
// disagree about whether this host can build.
type ToolchainProber interface {
	Probe(ctx context.Context) toolchain.Report
}

// Events is the events/SSE seam. Append belongs inside the caller's write
// transaction; Publish runs only after it commits, because a subscriber told
// about a row that then rolled back would have been told something that did not
// happen.
type Events interface {
	Append(ctx context.Context, tx store.Tx, ev model.Event) error
	Publish(ev model.Event)
}

// Settings is the typed settings this package reads (§6.2, §6.5, §6.6).
type Settings interface {
	GetBool(ctx context.Context, key string) (bool, error)
	GetInt(ctx context.Context, key string) (int64, error)
	GetString(ctx context.Context, key string) (string, error)
}

// Instances is D69's one method, taking the CALLER's transaction because
// activation must recompute every instance's `config_hash` in the SAME
// transaction that sets `is_active` (§6.6 step 3). *instances.Service satisfies
// it.
type Instances interface {
	RecomputeConfigHash(ctx context.Context, tx store.Tx, ids ...string) error
}

// DirGuard answers D25's question: is a live process executing out of this
// directory? source.ProcExeGuard is the production implementation — a readlink
// of `/proc/<pid>/exe` over every visible process — and "database bookkeeping
// alone is not trusted for this" is why this is asked of the filesystem and not
// of `instance_status`.
type DirGuard interface {
	InUse(ctx context.Context, dir string) (pid int, inUse bool, err error)
}

// BenchGuard is §6.6 step 1's second refusal term: a bench is executing, or one
// has stopped production instances and not yet put them back (D75). A nil
// BenchGuard means the bench subsystem is not wired into this daemon, so no
// bench can be live — which is a fact about the build, not an assumption.
type BenchGuard interface {
	BenchLive(ctx context.Context, tx store.Tx) (bool, error)
}

// Notifier raises the things §6.6 says need a human: a failed canary, and an
// activation whose roll a daemon restart interrupted. A nil Notifier drops
// them; the same facts are always also in `events`, which is why that is
// degradation rather than data loss.
type Notifier interface {
	Notify(ctx context.Context, tx store.Tx, severity model.NotificationSeverity,
		code, title, body, subjectID string) error
}

// Resolver turns a channel into the concrete identity a version row is built
// from — the tag, the pinned build tag and the prebuilt asset for this host.
// *github.Client satisfies it.
//
// A nil Resolver is a daemon with no GitHub client: an install request must then
// carry an explicit tag, and the acquisition decision has no asset to find, so
// it falls to a source build. That is exactly §6.3's "otherwise" branch, reached
// for a reason rather than by accident.
type Resolver interface {
	Resolve(ctx context.Context, req github.ResolveRequest) (github.Resolution, github.Meta, error)
}

// RefResolver validates a custom channel's git ref and resolves it to the
// concrete SHA that makes "rebuild the same thing" reproducible (§6.2). A nil
// RefResolver means a custom request must name a 40-hex commit itself.
type RefResolver interface {
	LsRemote(ctx context.Context, gitURL, ref string) (commit string, err error)
}

// Config wires a Service.
type Config struct {
	Store    Store
	Queue    Queue
	Events   Events
	Settings Settings
	// Instances is D69's recompute. It is required: an activation that could
	// not recompute `config_hash` would leave every instance's stored hash
	// disagreeing with its own definition, and `restart_required` would never
	// light (§6.6 step 3).
	Instances Instances

	// StateDir is `runtime_info.state_dir` (D72) — never a literal
	// /var/lib/llamaman.
	StateDir string
	// BootID is `runtime_info.boot_id`, and it is what the D70 build lease is
	// owned by: "this lease belongs to a boot that is gone" has to be a string
	// comparison at the next boot, so it must be THIS boot's id.
	BootID string

	// Guard is D25's live-process check. Nil uses source.ProcExeGuard over
	// /proc, which is the production implementation.
	Guard DirGuard

	// Logs is the registry of the log sink of every build that is running
	// (§6.5). It must be THE SAME registry the source.Builder was given, or
	// `GET /llamacpp/versions/{id}/log`'s live tail looks for a running build
	// in a map nothing writes to. Nil means no live tail: the endpoint still
	// serves the file, which is the whole log, and reports `live: false`.
	Logs *source.LogRegistry

	// Toolchain probes this host for `GET /llamacpp/plan` (§6.3). Nil probes
	// the real host through internal/toolchain.
	Toolchain ToolchainProber

	// FreeSpace reports the bytes available on the filesystem holding a path,
	// for the plan's `free_space_ok`. Nil uses statfs through the source
	// pipeline's own implementation, so a plan and a build agree about what
	// "enough room" means.
	FreeSpace func(path string) (uint64, error)
	// Bench, Notify, Releases and Refs are the four documented nils above.
	Bench    BenchGuard
	Notify   Notifier
	Releases Resolver
	Refs     RefResolver

	// ReleaseList is the listing half of the same GitHub client, for
	// `GET /api/v1/llamacpp/releases` (§3.5). It is a second field rather than
	// a wider Resolver because a daemon may legitimately resolve a tag it was
	// given without ever listing a channel; nil answers that endpoint with
	// `resolve_failed` rather than an empty list, because "there are no
	// releases" and "this build cannot ask" are different sentences.
	ReleaseList ReleaseLister

	// GOARCH is the host architecture for the asset lookup; empty means
	// runtime.GOARCH.
	GOARCH string

	// Now supplies every instant this service stamps. Nil uses time.Now.
	Now func() time.Time
	// NewID mints row and event ids. Nil uses store.NewID.
	NewID func(time.Time) string
	// Logger is the daemon's slog. Nil uses slog.Default.
	Logger *slog.Logger
}

// Service is the version lifecycle: the list, the five branches of D71's
// install, activation, rollback and delete.
type Service struct {
	store    Store
	queue    Queue
	events   Events
	settings Settings
	insts    Instances

	layout   Layout
	guard    DirGuard
	logs     *source.LogRegistry
	probe    ToolchainProber
	space    func(path string) (uint64, error)
	bench    BenchGuard
	notify   Notifier
	rel      Resolver
	releases ReleaseLister
	refs     RefResolver
	goarch   string
	bootID   string

	now   func() time.Time
	newID func(time.Time) string
	log   *slog.Logger
}

// New constructs a Service.
func New(cfg Config) (*Service, error) {
	switch {
	case cfg.Store == nil:
		return nil, errors.New("llamacpp: a Store is required")
	case cfg.Queue == nil:
		return nil, errors.New("llamacpp: a Queue is required")
	case cfg.Instances == nil:
		return nil, errors.New("llamacpp: an Instances is required — D69's recompute is not optional")
	case cfg.StateDir == "":
		return nil, errors.New("llamacpp: a StateDir is required")
	}

	s := &Service{
		store:    cfg.Store,
		queue:    cfg.Queue,
		events:   cfg.Events,
		settings: cfg.Settings,
		insts:    cfg.Instances,
		layout:   NewLayout(cfg.StateDir),
		guard:    cfg.Guard,
		logs:     cfg.Logs,
		probe:    cfg.Toolchain,
		space:    cfg.FreeSpace,
		bench:    cfg.Bench,
		notify:   cfg.Notify,
		rel:      cfg.Releases,
		releases: cfg.ReleaseList,
		refs:     cfg.Refs,
		goarch:   cfg.GOARCH,
		bootID:   cfg.BootID,
		now:      cfg.Now,
		newID:    cfg.NewID,
		log:      cfg.Logger,
	}
	if s.guard == nil {
		s.guard = defaultGuard()
	}
	if s.goarch == "" {
		s.goarch = runtime.GOARCH
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

// Layout is the `versions/` layout this service publishes into, for the workers
// and for a test that needs to look at the tree.
func (s *Service) Layout() Layout { return s.layout }

// View is one version as the API returns it: the row, plus the one fact that is
// not in it.
type View struct {
	Version store.LlamacppVersion
	// InUseBy names the instances whose live process is executing out of this
	// version's directory (D25). It is what makes `stale_version` legible on the
	// version side — "you cannot delete this, three instances are still on it" —
	// and it is read from `instance_status.exe_version_id`, which the supervisor
	// stamps from `/proc/<pid>/exe`.
	InUseBy []string
}

// List is `GET /api/v1/llamacpp/versions`.
func (s *Service) List(ctx context.Context) ([]View, error) {
	var out []View
	err := s.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		rows, err := s.store.LlamacppVersions(ctx, tx, store.LlamacppVersionFilter{})
		if err != nil {
			return err
		}
		users, err := s.inUseBy(ctx, tx)
		if err != nil {
			return err
		}
		out = make([]View, 0, len(rows))
		for _, r := range rows {
			out = append(out, View{Version: r, InUseBy: users[r.ID]})
		}
		return nil
	})
	return out, err
}

// Get is `GET /api/v1/llamacpp/versions/{id}`.
func (s *Service) Get(ctx context.Context, id string) (View, error) {
	var v View
	err := s.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		row, err := s.store.LlamacppVersion(ctx, tx, id)
		if err != nil {
			return err
		}
		users, err := s.inUseBy(ctx, tx)
		if err != nil {
			return err
		}
		v = View{Version: row, InUseBy: users[row.ID]}
		return nil
	})
	return v, err
}

// Active is `GET /api/v1/llamacpp/active`. store.ErrNotFound is the ordinary
// answer on a fresh install: no build has been activated yet.
func (s *Service) Active(ctx context.Context) (View, error) {
	var v View
	err := s.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		row, err := s.store.LlamacppVersionByFlag(ctx, tx, false)
		if err != nil {
			return err
		}
		users, err := s.inUseBy(ctx, tx)
		if err != nil {
			return err
		}
		v = View{Version: row, InUseBy: users[row.ID]}
		return nil
	})
	return v, err
}

// inUseBy groups the running instances by the version id their live process is
// executing out of.
func (s *Service) inUseBy(ctx context.Context, tx store.Tx) (map[string][]string, error) {
	rows, err := s.store.InstanceViews(ctx, tx, store.InstanceFilter{})
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, r := range rows {
		if r.Status.ExeVersionID == nil || *r.Status.ExeVersionID == "" {
			continue
		}
		out[*r.Status.ExeVersionID] = append(out[*r.Status.ExeVersionID], r.Name)
	}
	for id := range out {
		sort.Strings(out[id])
	}
	return out, nil
}

// InstallRequest is the body of `POST /api/v1/llamacpp/versions` (§3.5).
type InstallRequest struct {
	Channel model.LlamacppChannel
	// Tag pins a release. On the stable channel it is ignored — "stable" means
	// whatever `releases/latest` says, by definition (§6.2).
	Tag string
	// GitURL and GitRef are the custom channel's inputs. GitURL empty means the
	// upstream repository.
	GitURL  string
	GitRef  string
	Backend model.Backend
	// ForceSource takes §6.3's "otherwise" branch whatever the asset lookup
	// would have said.
	ForceSource bool
	// CMakeExtra is appended after `settings.llamacpp.extra_cmake_flags`, last,
	// so a request can override anything the setting put before it (§6.5).
	CMakeExtra []string
	// ForceRebuild is D71's override: a `ready` row is reused-and-reset and
	// rebuilt in place, refused only when it is active AND a live process is
	// executing out of its directory.
	ForceRebuild bool

	// Idempotency, when set, applies D65's ten-minute replay window, which is
	// what makes a double-clicked Build a replay rather than a 409.
	Idempotency *jobs.Idempotency
}

// InstallResult is what `POST /api/v1/llamacpp/versions` answers with.
type InstallResult struct {
	Version store.LlamacppVersion
	Job     model.Job
	// Reused is D71's third branch: the id is installed, `ready`, and its build
	// options match, so nothing was rebuilt. The response is 200 and the UI says
	// "already installed" and offers Activate.
	Reused bool
	// Replayed is the D65 hit; the response is 200 with the original job.
	Replayed bool
}

// Install is `POST /api/v1/llamacpp/versions` and every one of D71's five
// branches. The identity is resolved to an id BEFORE anything is inserted (§6.2),
// because "UNIQUE constraint failed" is not a user-facing answer.
func (s *Service) Install(ctx context.Context, req InstallRequest) (InstallResult, error) {
	ident, err := s.resolve(ctx, req)
	if err != nil {
		return InstallResult{}, err
	}

	// D25's refusal is asked of the FILESYSTEM, before the transaction, because
	// it is not a fact any row holds: rebuilding a directory a live process is
	// running from is the one case that must be a 409 rather than a rebuild.
	if req.ForceRebuild {
		if err := s.refuseIfInUse(ctx, ident.ID); err != nil {
			return InstallResult{}, err
		}
	}

	now := s.now()
	var out InstallResult
	err = s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		existing, err := s.store.LlamacppVersion(ctx, tx, ident.ID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			return s.installFresh(ctx, tx, ident, req, now, &out)
		case err != nil:
			return err
		}

		switch {
		case isLiveVersionState(existing.State):
			// The enqueue runs anyway, and that is deliberate: an
			// Idempotency-Key replay inside its window must return the ORIGINAL
			// job with 200, and only the queue knows whether this is one. What
			// comes back otherwise is `job_in_flight`, which this branch renames
			// to the code §3.5's table documents.
			return s.installLive(ctx, tx, ident, req, existing, now, &out)
		case existing.State == model.VersionReady && !req.ForceRebuild:
			diffs := optionDifferences(existing, ident)
			if len(diffs) > 0 {
				return withDetails(errorf(CodeVersionOptionsDiffer,
					"%s is installed with different build options; "+
						"pass force_rebuild to replace it", ident.ID),
					map[string]any{"version_id": ident.ID, "differences": diffs})
			}
			out = InstallResult{Version: existing, Reused: true}
			return nil
		default:
			// `ready` with force_rebuild, or any terminal-failure state:
			// reuse-and-reset (D71). The prior failure survives in `events` and
			// in the rotated build log; the row itself starts again.
			return s.installReset(ctx, tx, ident, req, existing, now, &out)
		}
	})
	if err != nil {
		return InstallResult{}, err
	}
	if out.Job.ID != "" && !out.Replayed {
		s.queue.Wake()
	}
	return out, nil
}

// installFresh is D71's first branch: no row with this id.
func (s *Service) installFresh(ctx context.Context, tx store.Tx, ident identity,
	req InstallRequest, now time.Time, out *InstallResult) error {

	row := ident.row(now)
	res, err := s.enqueueInstall(ctx, tx, ident, req, now,
		func(ctx context.Context, tx store.Tx, _ model.Job) error {
			if err := s.store.InsertLlamacppVersion(ctx, tx, row); err != nil {
				return err
			}
			return s.event(ctx, tx, now, row.ID, "llamacpp_version_created",
				model.LevelInfo, fmt.Sprintf("llama.cpp %s was requested", row.ID), nil,
				ptr(string(model.VersionPending)))
		})
	if err != nil {
		return err
	}
	*out = InstallResult{Version: row, Job: res.Job, Replayed: res.Replayed}
	return nil
}

// installLive is D71's second branch: the row is `pending` … `verifying`.
func (s *Service) installLive(ctx context.Context, tx store.Tx, ident identity,
	req InstallRequest, existing store.LlamacppVersion, now time.Time,
	out *InstallResult) error {

	res, err := s.enqueueInstall(ctx, tx, ident, req, now, nil)
	if err == nil {
		// A replay: the original job comes back and nothing was created.
		*out = InstallResult{Version: existing, Job: res.Job, Replayed: res.Replayed}
		return nil
	}

	var me model.Error
	if errors.As(err, &me) && me.Code == model.CodeJobInFlight {
		details := map[string]any{"version_id": existing.ID}
		for k, v := range me.Details {
			details[k] = v
		}
		return withDetails(errorf(CodeBuildInFlight,
			"llama.cpp %s is already being installed", existing.ID), details)
	}
	return err
}

// installReset is D71's fifth branch, and its `force_rebuild` override of the
// third: the row returns to `pending` and a fresh job is enqueued.
func (s *Service) installReset(ctx context.Context, tx store.Tx, ident identity,
	req InstallRequest, existing store.LlamacppVersion, now time.Time,
	out *InstallResult) error {

	from := string(existing.State)
	res, err := s.enqueueInstall(ctx, tx, ident, req, now,
		func(ctx context.Context, tx store.Tx, _ model.Job) error {
			ok, err := s.store.ResetLlamacppVersion(ctx, tx, existing.ID, now.UnixMilli())
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("llamacpp: %s could not be reset from %s", existing.ID, from)
			}
			if _, err := s.store.SetLlamacppBuildRequest(ctx, tx, existing.ID,
				ident.buildOptionsJSON(), ident.cudaArchList()); err != nil {
				return err
			}
			return s.event(ctx, tx, now, existing.ID, "llamacpp_version_reset",
				model.LevelInfo,
				fmt.Sprintf("llama.cpp %s was reset for another attempt", existing.ID),
				&from, ptr(string(model.VersionPending)))
		})
	if err != nil {
		return err
	}

	row, err := s.store.LlamacppVersion(ctx, tx, existing.ID)
	if err != nil {
		return err
	}
	*out = InstallResult{Version: row, Job: res.Job, Replayed: res.Replayed}
	return nil
}

// enqueueInstall writes the `llamacpp_install` job beside whatever the branch
// does to the domain row — §2.3a's one transaction, one pair.
func (s *Service) enqueueInstall(ctx context.Context, tx store.Tx, ident identity,
	req InstallRequest, now time.Time, domain jobs.DomainFunc) (jobs.EnqueueResult, error) {

	return s.queue.EnqueueTx(ctx, tx, jobs.EnqueueParams{
		Kind:        model.JobLlamacppInstall,
		DomainID:    ident.ID,
		Params:      ident.params(req),
		Idempotency: req.Idempotency,
		Domain:      domain,
	})
}

// refuseIfInUse is D25's guard, in the direction a forced rebuild asks it.
func (s *Service) refuseIfInUse(ctx context.Context, id string) error {
	pid, inUse, err := s.guard.InUse(ctx, s.layout.VersionDir(id))
	if err != nil {
		// The guard is a lower bound by construction — it can only see the
		// processes this identity may read — so a failure to look is reported
		// rather than treated as "nothing is running".
		return fmt.Errorf("llamacpp: could not check whether %s is in use: %w", id, err)
	}
	if inUse {
		return withDetails(errorf(CodeVersionInUse,
			"a running process is still executing out of %s", id),
			map[string]any{"version_id": id, "pid": pid})
	}
	return nil
}

// ActivateRequest is the body of `POST /llamacpp/versions/{id}/activate` and of
// `POST /llamacpp/rollback`.
type ActivateRequest struct {
	// RestartInstances is "none" or "rolling" (§3.5). "none" commits the
	// activation and leaves every instance wearing `restart_required` until a
	// human restarts it; "rolling" runs §6.6 step 5's canary-gated roll.
	RestartInstances string
	// CanaryInstanceID is the instance the roll gates on. Empty means the first
	// running instance in creation order.
	CanaryInstanceID string

	Idempotency *jobs.Idempotency
}

// RestartInstances values.
const (
	RestartNone    = "none"
	RestartRolling = "rolling"
)

// Activate is `POST /api/v1/llamacpp/versions/{id}/activate`. It performs §6.6
// step 1's guards and enqueues the activation; steps 2 through 5 are the
// worker's, because a canary roll takes as long as the fleet takes to restart
// and no HTTP request may wait for that.
func (s *Service) Activate(ctx context.Context, id string, req ActivateRequest) (model.Job, error) {
	return s.activate(ctx, id, req, false)
}

// Rollback is `POST /api/v1/llamacpp/rollback` — the identical routine with
// `previous_active` as the target, including the revert path, so a rollback
// whose own canary fails goes back to where it started (§6.6 step 6).
func (s *Service) Rollback(ctx context.Context, req ActivateRequest) (model.Job, error) {
	return s.activate(ctx, "", req, true)
}

func (s *Service) activate(ctx context.Context, id string, req ActivateRequest,
	rollback bool) (model.Job, error) {

	if req.RestartInstances == "" {
		req.RestartInstances = RestartNone
	}
	if req.RestartInstances != RestartNone && req.RestartInstances != RestartRolling {
		return model.Job{}, errorf(model.CodeBadFlags,
			"restart_instances must be %q or %q", RestartNone, RestartRolling)
	}

	now := s.now()
	var out model.Job
	err := s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		target, err := s.activationTarget(ctx, tx, id, rollback)
		if err != nil {
			return err
		}
		if target.State != model.VersionReady {
			return withDetails(errorf(CodeVersionNotReady,
				"llama.cpp %s is %s, not ready", target.ID, target.State),
				map[string]any{"version_id": target.ID, "state": string(target.State)})
		}
		if err := s.refuseWhileBusy(ctx, tx); err != nil {
			return err
		}

		keep, err := s.keepPrevious(ctx)
		if err != nil {
			return err
		}
		res, err := s.queue.EnqueueTx(ctx, tx, jobs.EnqueueParams{
			Kind:     model.JobLlamacppActivate,
			DomainID: target.ID,
			Params: activateParams{
				VersionID:        target.ID,
				RestartInstances: req.RestartInstances,
				CanaryInstanceID: req.CanaryInstanceID,
				KeepPrevious:     keep,
				Rollback:         rollback,
			},
			Idempotency: req.Idempotency,
			// The version row does NOT move: §2.3a's activate column says an
			// activation never leaves `ready`, because what it moves is
			// `is_active`, `previous_active`, `config_hash` and two symlinks —
			// all of which a failed canary reverts together.
			Domain: func(ctx context.Context, tx store.Tx, _ model.Job) error {
				return s.event(ctx, tx, now, target.ID, "llamacpp_activation_requested",
					model.LevelInfo,
					fmt.Sprintf("llama.cpp %s was asked to become active", target.ID),
					nil, nil)
			},
		})
		if err != nil {
			return err
		}
		out = res.Job
		return nil
	})
	if err != nil {
		return model.Job{}, err
	}
	s.queue.Wake()
	return out, nil
}

// activationTarget is the version an activate or a rollback names.
func (s *Service) activationTarget(ctx context.Context, tx store.Tx, id string,
	rollback bool) (store.LlamacppVersion, error) {

	if !rollback {
		return s.store.LlamacppVersion(ctx, tx, id)
	}
	target, err := s.store.LlamacppVersionByFlag(ctx, tx, true)
	if errors.Is(err, store.ErrNotFound) {
		return store.LlamacppVersion{}, errorf(CodeNoRollbackTarget,
			"there is no retained previous build to roll back to")
	}
	return target, err
}

// refuseWhileBusy is §6.6 step 1's two guards, both evaluated INSIDE the
// transaction that enqueues, because neither can be expressed by
// `idx_jobs_one_live_per_subject`: two activations of two different versions are
// two different subjects, and a bench is not a job subject at all.
func (s *Service) refuseWhileBusy(ctx context.Context, tx store.Tx) error {
	live, err := s.store.CountLiveJobsByKind(ctx, tx, model.JobLlamacppActivate)
	if err != nil {
		return err
	}
	if live > 0 {
		return errorf(CodeActivationInFlight, "another activation is already running")
	}
	if s.bench == nil {
		return nil
	}
	busy, err := s.bench.BenchLive(ctx, tx)
	if err != nil {
		return err
	}
	if busy {
		return errorf(CodeBenchInFlight,
			"a benchmark is running or has not yet restored the instances it stopped")
	}
	return nil
}

// Delete is `DELETE /api/v1/llamacpp/versions/{id}`. Its three refusals are
// §3.5's table, and the third is D25's: asked of `/proc`, not of a row.
func (s *Service) Delete(ctx context.Context, id string) (model.Job, error) {
	now := s.now()

	var target store.LlamacppVersion
	if err := s.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		target, err = s.store.LlamacppVersion(ctx, tx, id)
		return err
	}); err != nil {
		return model.Job{}, err
	}
	switch {
	case target.IsActive:
		return model.Job{}, withDetails(errorf(CodeVersionActive,
			"llama.cpp %s is the active build; activate another one first", id),
			map[string]any{"version_id": id})
	case target.PreviousActive:
		return model.Job{}, withDetails(errorf(CodeVersionIsRollbackTarget,
			"llama.cpp %s is the retained rollback target; "+
				"turn llamacpp.keep_previous off to release it", id),
			map[string]any{"version_id": id})
	}
	if err := s.refuseIfInUse(ctx, id); err != nil {
		return model.Job{}, err
	}

	var out model.Job
	err := s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		res, err := s.queue.EnqueueTx(ctx, tx, jobs.EnqueueParams{
			Kind:     model.JobLlamacppDelete,
			DomainID: id,
			Params:   deleteParams{VersionID: id},
			// The row stays `ready` while the job is `queued` (§2.3a): it moves
			// to `deleting` when the worker starts, which is the first moment
			// anything on disk is at risk.
			Domain: func(ctx context.Context, tx store.Tx, _ model.Job) error {
				return s.event(ctx, tx, now, id, "llamacpp_version_delete_requested",
					model.LevelInfo, fmt.Sprintf("llama.cpp %s was queued for deletion", id),
					nil, nil)
			},
		})
		if err != nil {
			return err
		}
		out = res.Job
		return nil
	})
	if err != nil {
		return model.Job{}, err
	}
	s.queue.Wake()
	return out, nil
}

// keepPrevious reads `llamacpp.keep_previous`, defaulting to the registry's own
// default when no Settings is wired.
func (s *Service) keepPrevious(ctx context.Context) (bool, error) {
	if s.settings == nil {
		return true, nil
	}
	return s.settings.GetBool(ctx, "llamacpp.keep_previous")
}

// event appends one `events` row inside the caller's transaction. Every
// transition in §2.5 writes one, which is why this is a method and not a
// decision each call site makes.
func (s *Service) event(ctx context.Context, tx store.Tx, now time.Time, versionID,
	action string, level model.EventLevel, message string, from, to *string) error {

	if s.events == nil {
		return nil
	}
	subjectType := "llamacpp_version"
	return s.events.Append(ctx, tx, model.Event{
		ID:          s.newID(now),
		At:          now.UnixMilli(),
		Level:       level,
		Category:    model.CategoryLlamacpp,
		SubjectType: &subjectType,
		SubjectID:   &versionID,
		Action:      action,
		FromState:   from,
		ToState:     to,
		Actor:       model.ActorAdmin,
		Message:     message,
	})
}

// publish pushes an event onto the SSE hub after the transaction that wrote it
// has committed.
func (s *Service) publish(now time.Time, versionID, action string,
	level model.EventLevel, message string) {

	if s.events == nil {
		return
	}
	subjectType := "llamacpp_version"
	s.events.Publish(model.Event{
		ID:          s.newID(now),
		At:          now.UnixMilli(),
		Level:       level,
		Category:    model.CategoryLlamacpp,
		SubjectType: &subjectType,
		SubjectID:   &versionID,
		Action:      action,
		Actor:       model.ActorSystem,
		Message:     message,
	})
}

// identity is a request resolved to the three-part id and everything the worker
// needs to act on it without resolving again.
type identity struct {
	ID          string
	Channel     model.LlamacppChannel
	Tag         string
	BuildTag    string
	GitURL      string
	GitRef      string
	Commit      string
	Backend     model.Backend
	Acquisition model.Acquisition

	AssetName       string
	AssetURL        string
	AssetSHA256     string
	AssetReleaseTag string

	// ExtraCMake is the EFFECTIVE flag list: `settings.llamacpp.extra_cmake_flags`
	// followed by the request's own `cmake_extra`, in that order, because §6.5
	// passes them through verbatim and last so a user can override anything
	// above them. It is computed here rather than in the worker so that D71's
	// "do the requested options match the stored ones" compares like with like.
	ExtraCMake   []string
	CUDAArchList string
}

// row is the `pending` row a fresh install inserts.
func (id identity) row(now time.Time) store.LlamacppVersion {
	v := store.LlamacppVersion{
		ID:          id.ID,
		Channel:     id.Channel,
		Tag:         id.Tag,
		GitURL:      id.gitURLOrDefault(),
		Acquisition: id.Acquisition,
		Backend:     id.Backend,
		DirName:     id.ID,
		State:       model.VersionPending,
		SupportsFit: true,
		CreatedAt:   now.UnixMilli(),
	}
	if id.BuildTag != "" {
		v.BuildTag = &id.BuildTag
	}
	if id.GitRef != "" {
		v.GitRef = &id.GitRef
	}
	if id.Commit != "" {
		v.ResolvedCommit = &id.Commit
	}
	v.BuildOptionsJSON = id.buildOptionsJSON()
	v.CUDAArchList = id.cudaArchList()
	return v
}

func (id identity) gitURLOrDefault() string {
	if id.GitURL == "" {
		return store.DefaultLlamacppGitURL
	}
	return id.GitURL
}

// buildOptionsJSON is the column as the REQUEST describes it: the effective
// extra cmake flags and the CUDA architectures. The worker overwrites it at
// publish with the full source.BuildOptions the build actually ran with, so the
// column always describes the build that is happening or has happened — which
// is exactly what makes D71's third and fourth branches a comparison rather
// than a guess.
func (id identity) buildOptionsJSON() *string {
	b, err := json.Marshal(source.BuildOptions{
		ExtraCMakeFlags: id.ExtraCMake,
		CUDAArchList:    id.CUDAArchList,
	})
	if err != nil {
		return nil
	}
	s := string(b)
	return &s
}

func (id identity) cudaArchList() *string {
	if id.CUDAArchList == "" {
		return nil
	}
	return &id.CUDAArchList
}

// optionDifferences is D71's fourth branch: which options the request asks for
// that the installed row was not built with, each with both values, so the 409
// can name them rather than only refuse.
func optionDifferences(existing store.LlamacppVersion, want identity) []map[string]any {
	var out []map[string]any

	var have source.BuildOptions
	if existing.BuildOptionsJSON != nil {
		_ = json.Unmarshal([]byte(*existing.BuildOptionsJSON), &have)
	}
	if strings.Join(have.ExtraCMakeFlags, "\x00") != strings.Join(want.ExtraCMake, "\x00") {
		out = append(out, map[string]any{
			"option":    "extra_cmake_flags",
			"installed": have.ExtraCMakeFlags,
			"requested": want.ExtraCMake,
		})
	}

	haveArch := ""
	if existing.CUDAArchList != nil {
		haveArch = *existing.CUDAArchList
	}
	if want.CUDAArchList != "" && haveArch != want.CUDAArchList {
		out = append(out, map[string]any{
			"option":    "cuda_arch_list",
			"installed": haveArch,
			"requested": want.CUDAArchList,
		})
	}
	return out
}

// resolve turns a request into D60's three-part identity, and does it BEFORE the
// transaction because it can reach the network (§6.2).
func (s *Service) resolve(ctx context.Context, req InstallRequest) (identity, error) {
	backend := req.Backend
	if backend == "" {
		backend = model.BackendCPU
	}
	if !backend.Valid() {
		return identity{}, errorf(model.CodeBadFlags, "backend %q is not cpu or cuda", backend)
	}
	channel := req.Channel
	if channel == "" {
		channel = model.ChannelStable
	}
	if !channel.Valid() {
		return identity{}, errorf(model.CodeBadFlags,
			"channel %q is not stable, nightly or custom", channel)
	}

	// §6.2 validates the git URL "before the row leaves `resolving`", and this
	// is the one place every channel passes through: `force_source: true` on the
	// stable channel carries a `git_url` into the row exactly as a custom build
	// does, so the check cannot live in resolveCustom. What it refuses is the
	// set source.ValidateGitURL names — anything outside the four transports, a
	// leading `-`, the `ext::` shell escape, and a URL carrying credentials that
	// the build log and this API would otherwise publish.
	if strings.TrimSpace(req.GitURL) != "" {
		if err := source.ValidateGitURL(req.GitURL); err != nil {
			return identity{}, errorf(model.CodeBadFlags, "%s", err.Error())
		}
	}

	id := identity{
		Channel: channel,
		Backend: backend,
		GitURL:  strings.TrimSpace(req.GitURL),
		GitRef:  req.GitRef,
	}
	if s.settings != nil && backend == model.BackendCUDA {
		archList, err := s.settings.GetString(ctx, "llamacpp.cuda_arch_list")
		if err != nil {
			return identity{}, err
		}
		id.CUDAArchList = archList
	}
	extra, err := s.extraCMakeFlags(ctx, req.CMakeExtra)
	if err != nil {
		return identity{}, err
	}
	id.ExtraCMake = extra

	if channel == model.ChannelCustom {
		if err := s.resolveCustom(ctx, &id); err != nil {
			return identity{}, err
		}
		id.Acquisition = model.AcquisitionSource
		id.ID = VersionID(id.Tag, backend, id.Acquisition)
		return id, ValidateVersionID(id.ID)
	}

	if err := s.resolveRelease(ctx, req, &id); err != nil {
		return identity{}, err
	}
	id.Acquisition = s.decideAcquisition(ctx, req, id)
	id.ID = VersionID(id.Tag, backend, id.Acquisition)
	return id, ValidateVersionID(id.ID)
}

// resolveRelease fills the tag, the pinned build tag and the asset from the
// GitHub release list — §6.2's channel-resolution table.
func (s *Service) resolveRelease(ctx context.Context, req InstallRequest, id *identity) error {
	if s.rel == nil {
		if req.Tag == "" {
			return errorf(model.CodeBadFlags,
				"this daemon has no release client wired, so an install must name a tag")
		}
		id.Tag = req.Tag
		return nil
	}

	goarch := s.goarch
	if id.Backend == model.BackendCUDA {
		// No Linux CUDA prebuilt exists, so there is nothing to look for and
		// the lookup is skipped entirely (§6.3).
		goarch = ""
	}
	res, _, err := s.rel.Resolve(ctx, github.ResolveRequest{
		Channel: id.Channel,
		Tag:     req.Tag,
		GOARCH:  goarch,
	})
	if err != nil {
		if req.Tag != "" {
			// A named tag that could not be looked up is still buildable from
			// source: the resolution only ever adds an asset.
			id.Tag = req.Tag
			return nil
		}
		return errorf(CodeResolveFailed,
			"could not resolve the %s channel: %v", id.Channel, err)
	}

	id.Tag = res.Tag
	id.BuildTag = res.BuildTag
	if res.AssetFound {
		id.AssetName = res.Asset.Name
		id.AssetURL = res.Asset.DownloadURL
		id.AssetReleaseTag = res.AssetRelease
		if sum, ok := res.Asset.SHA256(); ok {
			id.AssetSHA256 = sum
		}
	}
	return nil
}

// decideAcquisition is §6.3's table, in the order it is written there.
func (s *Service) decideAcquisition(ctx context.Context, req InstallRequest,
	id identity) model.Acquisition {

	if id.Backend == model.BackendCUDA || req.ForceSource || id.AssetURL == "" {
		return model.AcquisitionSource
	}
	prefer := true
	if s.settings != nil {
		v, err := s.settings.GetBool(ctx, "llamacpp.prefer_prebuilt_cpu")
		if err == nil {
			prefer = v
		}
	}
	if !prefer {
		return model.AcquisitionSource
	}
	return model.AcquisitionPrebuilt
}

// extraCMakeFlags composes §6.5's flag order: the setting first, the request
// last, so a per-build flag overrides a host-wide one.
//
// The setting is one string and is split with the same shell-word splitter
// `extra_flags` uses on an instance, because a cmake flag legitimately carries
// quoted spaces (`-DCMAKE_CXX_FLAGS="-O2 -g"`) and strings.Fields would tear it
// in half.
func (s *Service) extraCMakeFlags(ctx context.Context, requested []string) ([]string, error) {
	var out []string
	if s.settings != nil {
		raw, err := s.settings.GetString(ctx, "llamacpp.extra_cmake_flags")
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(raw) != "" {
			words, err := instances.SplitWords(raw)
			if err != nil {
				return nil, errorf(model.CodeBadFlags,
					"llamacpp.extra_cmake_flags is not parseable: %v", err)
			}
			out = append(out, words...)
		}
	}
	return append(out, requested...), nil
}

// forkTagRe accepts the 40-hex a custom ref may already be.
var forkTagRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// resolveCustom mints `fork-<urlhash6>-<short>` (§6.2).
//
// The URL discriminator is not decoration: without it two unrelated forks that
// share a seven-hex prefix — or the same commit fetched from a mirror — collide
// on `UNIQUE(tag, backend, acquisition)` and the second silently reuses the
// first one's build.
func (s *Service) resolveCustom(ctx context.Context, id *identity) error {
	if id.GitURL == "" {
		id.GitURL = store.DefaultLlamacppGitURL
	}
	ref := strings.TrimSpace(id.GitRef)
	if ref == "" {
		return errorf(model.CodeBadFlags, "a custom build needs a git_ref")
	}

	switch {
	case forkTagRe.MatchString(ref):
		id.Commit = ref
	case s.refs != nil:
		commit, err := s.refs.LsRemote(ctx, id.GitURL, ref)
		if err != nil {
			return errorf(CodeResolveFailed,
				"could not resolve %q at %s: %v", ref, id.GitURL, err)
		}
		if !forkTagRe.MatchString(commit) {
			return errorf(CodeResolveFailed,
				"%q at %s resolved to %q, which is not a commit", ref, id.GitURL, commit)
		}
		id.Commit = commit
	default:
		return errorf(model.CodeBadFlags,
			"this daemon cannot resolve a git ref, so a custom build must name a 40-hex commit")
	}

	id.Tag = fmt.Sprintf("fork-%s-%s", urlHash6(id.GitURL), id.Commit[:7])
	return nil
}

// RedactGitURL strips any credentials from a stored git URL on its way to the
// wire. It is source.RedactGitURL re-exported, so the API layer can apply the
// rule without importing the build pipeline: `llamacpp_versions.git_url` is
// returned by `GET /api/v1/llamacpp/versions/{id}`, and a row written before
// ValidateGitURL refused userinfo — or edited into the database by hand — must
// not publish a token through it (DESIGN sections 2.2 and 7.1).
func RedactGitURL(raw string) string { return source.RedactGitURL(raw) }

// urlHash6 is the first six hex characters of sha256 over the normalized git
// URL: scheme and any `.git` suffix stripped, host lowercased (§6.2).
func urlHash6(raw string) string {
	normalized := strings.TrimSuffix(strings.TrimSpace(raw), ".git")
	if u, err := url.Parse(normalized); err == nil && u.Host != "" {
		u.Scheme = ""
		u.Host = strings.ToLower(u.Host)
		normalized = strings.TrimPrefix(u.String(), "//")
	}
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:])[:6]
}

// isLiveVersionState reports whether a row is in one of the states D71 calls
// "live": the build is happening now, so a second POST is `409 build_in_flight`.
func isLiveVersionState(s model.VersionState) bool {
	switch s {
	case model.VersionPending, model.VersionResolving, model.VersionFetching,
		model.VersionBuilding, model.VersionVerifying, model.VersionDeleting:
		return true
	}
	return false
}

func ptr[T any](v T) *T { return &v }
