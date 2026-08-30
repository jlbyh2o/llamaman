package llamacpp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jlbyh2o/llamaman/internal/events"
	"github.com/jlbyh2o/llamaman/internal/jobs"
	"github.com/jlbyh2o/llamaman/internal/llamacpp/github"
	"github.com/jlbyh2o/llamaman/internal/llamacpp/prebuilt"
	"github.com/jlbyh2o/llamaman/internal/llamacpp/source"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/settings"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// The fixture is a REAL store and a REAL job queue over a temp state directory.
//
// Faking either would have made these tests assertions about the fakes: §2.3a's
// invariant table is a statement about two rows written in one transaction, the
// D70 lease is a conditional UPDATE, and the activation flags are held by two
// UNIQUE partial indexes. None of that survives a stub. Nothing here writes SQL
// — every seed goes through the same store methods production uses — so D49's
// "only internal/store contains SQL" holds.

const testBootID = "01BOOTBOOTBOOTBOOTBOOTBOOT"

type fixture struct {
	t     *testing.T
	dir   string
	store *store.Store
	queue *jobs.Queue
	svc   *Service

	// recomputes counts D69's calls, which is how a test proves the activation
	// transaction and its revert each ran exactly one.
	recomputes *recomputeSpy
	guard      *fakeGuard
	clock      *testClock
}

// newFixture builds a daemon-shaped set of collaborators around a temp state
// directory.
func newFixture(t *testing.T, cfg func(*Config)) *fixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()

	st, err := store.Open(ctx, filepath.Join(dir, "llamaman.db"))
	if err != nil {
		t.Fatalf("open the fixture database: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.Migrate(ctx, store.MigrateOptions{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	clock := &testClock{now: time.Unix(1_700_000_000, 0).UTC()}
	q, err := jobs.New(st, jobs.Options{BootID: testBootID, Now: clock.Now})
	if err != nil {
		t.Fatalf("jobs.New: %v", err)
	}

	set := settings.New(settings.NewRegistry(), st)
	if err := set.Load(ctx); err != nil {
		t.Fatalf("load settings: %v", err)
	}

	spy := &recomputeSpy{}
	guard := &fakeGuard{}
	c := Config{
		Store:     st,
		Queue:     q,
		Events:    events.NewRecorder(st, events.NewHub(0)),
		Settings:  set,
		Instances: spy,
		StateDir:  dir,
		BootID:    testBootID,
		Guard:     guard,
		Now:       clock.Now,
	}
	if cfg != nil {
		cfg(&c)
	}
	svc, err := New(c)
	if err != nil {
		t.Fatalf("llamacpp.New: %v", err)
	}

	return &fixture{
		t: t, dir: dir, store: st, queue: q, svc: svc,
		recomputes: spy, guard: guard, clock: clock,
	}
}

// seedVersion inserts one row in a chosen state, through the production store
// method rather than through SQL.
func (f *fixture) seedVersion(id string, state model.VersionState) store.LlamacppVersion {
	f.t.Helper()
	row := store.LlamacppVersion{
		ID:          id,
		Channel:     model.ChannelNightly,
		Tag:         id,
		GitURL:      store.DefaultLlamacppGitURL,
		Acquisition: model.AcquisitionSource,
		Backend:     model.BackendCPU,
		DirName:     id,
		State:       state,
		SupportsFit: true,
		CreatedAt:   f.clock.Now().UnixMilli(),
	}
	ctx := context.Background()
	if err := f.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		return f.store.InsertLlamacppVersion(ctx, tx, row)
	}); err != nil {
		f.t.Fatalf("seed version %s: %v", id, err)
	}
	// The directory is what a delete removes and what a symlink points at.
	if err := os.MkdirAll(filepath.Join(f.svc.layout.VersionDir(id), "bin"), 0o750); err != nil {
		f.t.Fatalf("seed the version directory: %v", err)
	}
	return row
}

// version reads a row back.
func (f *fixture) version(id string) store.LlamacppVersion {
	f.t.Helper()
	var row store.LlamacppVersion
	err := f.store.Read(context.Background(), func(ctx context.Context, tx store.Tx) error {
		var err error
		row, err = f.store.LlamacppVersion(ctx, tx, id)
		return err
	})
	if err != nil {
		f.t.Fatalf("read version %s: %v", id, err)
	}
	return row
}

// job reads a job row back.
func (f *fixture) job(id string) model.Job {
	f.t.Helper()
	j, err := f.queue.Job(context.Background(), id)
	if err != nil {
		f.t.Fatalf("read job %s: %v", id, err)
	}
	return j
}

// runOne claims and runs one ready job inline, and fails the test when there was
// nothing to run.
func (f *fixture) runOne() {
	f.t.Helper()
	ran, err := f.queue.RunOnce(context.Background())
	if err != nil {
		f.t.Fatalf("RunOnce: %v", err)
	}
	if !ran {
		f.t.Fatal("RunOnce found no ready job")
	}
}

// link reads what a version symlink points at.
func (f *fixture) link(name string) string {
	f.t.Helper()
	target, err := f.svc.layout.ReadLink(name)
	if err != nil {
		f.t.Fatalf("read versions/%s: %v", name, err)
	}
	return target
}

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(time.Millisecond)
	return c.now
}

// recomputeSpy is D69's seam. It counts rather than computes, because what these
// tests assert is that the recompute happens inside the right transaction —
// exactly once on activation and exactly once more on a revert.
type recomputeSpy struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (s *recomputeSpy) RecomputeConfigHash(context.Context, store.Tx, ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.err
}

func (s *recomputeSpy) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// fakeGuard is D25's live-process check with the answer under the test's
// control, which is the only way to exercise a refusal that in production
// depends on a running llama-server.
type fakeGuard struct {
	pid   int
	inUse bool
	err   error
}

func (g *fakeGuard) InUse(context.Context, string) (int, bool, error) {
	return g.pid, g.inUse, g.err
}

// fakeRoller is §6.6 step 5's fleet. failAt names the index of the restart that
// fails, so a test can make the CANARY fail (0) or a later instance fail (1+)
// and assert the two different outcomes the design requires.
type fakeRoller struct {
	targets  []RollTarget
	failAt   int
	restarts []string
	err      error
}

func (r *fakeRoller) Targets(context.Context, string) ([]RollTarget, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.targets, nil
}

func (r *fakeRoller) Restart(_ context.Context, t RollTarget) error {
	i := len(r.restarts)
	r.restarts = append(r.restarts, t.Name)
	if r.failAt >= 0 && i == r.failAt {
		return errors.New("it never answered /health")
	}
	return nil
}

// fakeResolver answers the channel lookup with no network (DESIGN section 15:
// unit tests never make a live HTTP call).
type fakeResolver struct {
	res github.Resolution
	err error
}

func (r fakeResolver) Resolve(context.Context, github.ResolveRequest) (
	github.Resolution, github.Meta, error) {
	return r.res, github.Meta{}, r.err
}

// fakeSource is §6.5's pipeline with the outcome under the test's control.
type fakeSource struct {
	result *source.Result
	err    error
	built  []string
	// phases are reported to the Observer, in order, before Build returns. They
	// are how a test drives the §2.5 transitions the real pipeline announces —
	// `verify` above all, which is what moves the row into `verifying`.
	phases []source.Phase
}

func (s *fakeSource) Build(ctx context.Context, req source.Request) (*source.Result, error) {
	s.built = append(s.built, req.VersionID)
	for _, p := range s.phases {
		if req.Observer != nil {
			_ = req.Observer.Progress(ctx, source.Progress{Phase: p})
		}
	}
	if s.err != nil {
		return nil, s.err
	}
	res := s.result
	if res == nil {
		res = &source.Result{VersionID: req.VersionID, Backend: req.Backend}
	}
	return res, nil
}

func (s *fakeSource) CanResume(string) bool                 { return false }
func (s *fakeSource) Discard(context.Context, string) error { return nil }

// fakePrebuilt is §6.4's pipeline, likewise.
type fakePrebuilt struct {
	result prebuilt.InstallResult
	err    error
	// steps are reported through the request's ProgressReporter before Install
	// returns, for the same reason fakeSource has phases.
	steps []model.FailingStep
}

func (p fakePrebuilt) Install(_ context.Context, req prebuilt.InstallRequest) (
	prebuilt.InstallResult, error) {
	for _, s := range p.steps {
		if req.Progress != nil {
			req.Progress(s, 0, 0, "")
		}
	}
	return p.result, p.err
}

// jobsEnqueueInstall is one install job's enqueue params, for the tests that
// need a real `jobs` row to hang a build lease off — `build_lease.job_id`
// references `jobs(id)`, so a synthetic id will not do.
func jobsEnqueueInstall(id, tag string) jobs.EnqueueParams {
	return jobs.EnqueueParams{
		Kind:     model.JobLlamacppInstall,
		DomainID: id,
		Params: installParams{
			VersionID: id, Tag: tag,
			Backend: model.BackendCPU, Acquisition: model.AcquisitionSource,
		},
	}
}

// activateRowDirectly moves the activation flags without a job, for the tests
// that need the state a race would leave rather than the state an endpoint can
// produce — `idx_jobs_one_live_per_subject` will not let two live jobs exist on
// one version, which is exactly the situation being simulated.
func (f *fixture) activateRowDirectly(t *testing.T, id string) {
	t.Helper()
	if err := f.store.Write(context.Background(), func(ctx context.Context, tx store.Tx) error {
		_, err := f.store.ActivateLlamacppVersion(ctx, tx, id, true, f.clock.Now().UnixMilli())
		return err
	}); err != nil {
		t.Fatalf("activate %s directly: %v", id, err)
	}
}
