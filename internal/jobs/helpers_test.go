package jobs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// testBootID stands in for `runtime_info.boot_id`. Boot triage is a comparison
// against it, so the tests below spell the "dead" boot out too rather than
// relying on a zero value.
const (
	testBootID = "01BOOT000000000000000000TH"
	deadBootID = "01BOOT000000000000000DEAD1"
)

// testClock is the queue's clock under test. Backoff, lease horizons and the
// D65 window are all arithmetic on an instant, so driving that instant by hand
// is what makes them assertable without sleeping.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock() *testClock {
	return &testClock{t: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newTestStore opens a fresh database and applies every embedded migration,
// which is the state every boot after the first one starts from.
func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()

	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "llamaman.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	if _, err := s.Migrate(ctx, store.MigrateOptions{}); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}
	return s
}

// newTestQueue returns a queue over a fresh database, with a hand-driven clock
// and a discarding logger.
func newTestQueue(t *testing.T, opts ...func(*Options)) (*Queue, *store.Store, *testClock) {
	t.Helper()
	s := newTestStore(t)
	clock := newTestClock()

	o := Options{
		BootID: testBootID,
		Now:    clock.now,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	for _, fn := range opts {
		fn(&o)
	}

	q, err := New(s, o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return q, s, clock
}

// fakeWorker is a Worker whose every hook is a field. It implements Starter,
// DomainWriter and CancelGuard unconditionally — a nil hook behaves exactly as
// the absent interface would — and records the domain states the queue asked it
// to write, which is how the tests below assert §2.3a's "one transaction moves
// both rows" without a real domain table.
type fakeWorker struct {
	kind model.JobKind

	run   func(ctx context.Context, t *Task) (Outcome, error)
	start func(ctx context.Context, tx store.Tx, j model.Job) error
	guard func(ctx context.Context, tx store.Tx, j model.Job) error

	mu     sync.Mutex
	runs   int
	starts int
	states []model.JobState
}

func (w *fakeWorker) Kind() model.JobKind { return w.kind }

func (w *fakeWorker) Run(ctx context.Context, t *Task) (Outcome, error) {
	w.mu.Lock()
	w.runs++
	w.mu.Unlock()
	if w.run == nil {
		return Succeeded(nil), nil
	}
	return w.run(ctx, t)
}

func (w *fakeWorker) Start(ctx context.Context, tx store.Tx, j model.Job) error {
	w.mu.Lock()
	w.starts++
	w.mu.Unlock()
	if w.start == nil {
		return nil
	}
	return w.start(ctx, tx, j)
}

func (w *fakeWorker) CheckCancel(ctx context.Context, tx store.Tx, j model.Job) error {
	if w.guard == nil {
		return nil
	}
	return w.guard(ctx, tx, j)
}

func (w *fakeWorker) SetDomainState(ctx context.Context, tx store.Tx, j model.Job, state model.JobState) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.states = append(w.states, state)
	return nil
}

// domainStates is the sequence of states the queue asked this worker to write to
// its domain row.
func (w *fakeWorker) domainStates() []model.JobState {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]model.JobState(nil), w.states...)
}

func (w *fakeWorker) runCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.runs
}

// bareWorker implements Worker and nothing else, which is the correct shape for
// `maintenance` — the one kind whose job row IS the record, with no domain row
// to move (§2.3a).
type bareWorker struct {
	kind model.JobKind
	run  func(ctx context.Context, t *Task) (Outcome, error)
}

func (w *bareWorker) Kind() model.JobKind { return w.kind }

func (w *bareWorker) Run(ctx context.Context, t *Task) (Outcome, error) {
	if w.run == nil {
		return Succeeded(nil), nil
	}
	return w.run(ctx, t)
}

// register wires a fakeWorker for a kind and hands it back.
func register(t *testing.T, q *Queue, kind model.JobKind) *fakeWorker {
	t.Helper()
	w := &fakeWorker{kind: kind}
	if err := q.Register(w); err != nil {
		t.Fatalf("Register(%s): %v", kind, err)
	}
	return w
}

// mustEnqueue enqueues and fails the test on any refusal.
func mustEnqueue(t *testing.T, q *Queue, p EnqueueParams) model.Job {
	t.Helper()
	res, err := q.Enqueue(context.Background(), p)
	if err != nil {
		t.Fatalf("Enqueue(%s): %v", p.Kind, err)
	}
	return res.Job
}

// jobRow re-reads a job straight from the database, which is what every
// assertion below is made against — the in-memory copy a method returned could
// agree with itself while the row says something else.
func jobRow(t *testing.T, s *store.Store, id string) model.Job {
	t.Helper()
	var j model.Job
	err := s.Read(context.Background(), func(ctx context.Context, tx store.Tx) error {
		var err error
		j, err = s.Job(ctx, tx, id)
		return err
	})
	if err != nil {
		t.Fatalf("read job %s: %v", id, err)
	}
	return j
}

// insertJob writes a row verbatim. Every state the engine has to READ rather
// than produce — a dead boot's `running` row, a `paused` download, a job whose
// budget is already spent — is set up through this rather than through raw SQL,
// because only internal/store writes SQL (D49).
func insertJob(t *testing.T, s *store.Store, j model.Job) model.Job {
	t.Helper()
	err := s.Write(context.Background(), func(ctx context.Context, tx store.Tx) error {
		return s.InsertJob(ctx, tx, j)
	})
	if err != nil {
		t.Fatalf("insert job %s: %v", j.ID, err)
	}
	return j
}

// jobIn builds a row in a given state, stamped with a lease owner when the state
// is one that holds a lease.
func jobIn(clock *testClock, id string, kind model.JobKind, domainID string,
	state model.JobState, owner string) model.Job {
	subjectType, subjectID := model.SubjectFor(kind, domainID)
	at := clock.now().UnixMilli()
	j := model.Job{
		ID: id, Kind: kind, SubjectType: subjectType, SubjectID: subjectID,
		State: state, Priority: DefaultPriority, RunAfter: at,
		Attempts: 1, MaxAttempts: 3, CreatedAt: at, StartedAt: &at,
	}
	if state == model.JobLeased || state == model.JobRunning {
		expires := at + int64(DefaultLeaseTTL/time.Millisecond)
		j.LeaseOwner, j.LeaseExpiresAt = &owner, &expires
	}
	return j
}

// insertOrphan writes a row exactly as a daemon that died mid-run would have
// left it: `running` under a lease owner that is not this boot's id.
func insertOrphan(t *testing.T, s *store.Store, clock *testClock, id string,
	kind model.JobKind, domainID string, state model.JobState, owner string) model.Job {
	t.Helper()
	return insertJob(t, s, jobIn(clock, id, kind, domainID, state, owner))
}

// asModelError unwraps the API-shaped error this package refuses with.
func asModelError(t *testing.T, err error) model.Error {
	t.Helper()
	var me model.Error
	if !errors.As(err, &me) {
		t.Fatalf("error %v is not a model.Error", err)
	}
	return me
}
