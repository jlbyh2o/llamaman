package supervisor_test

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
	"github.com/jlbyh2o/llamaman/internal/supervisor"
	"github.com/jlbyh2o/llamaman/internal/systemd"
)

// Test doubles for the supervisor.
//
// Faking systemd.Controller in a TEST is not the bypass D49's second invariant
// forbids. That invariant says no package outside internal/systemd may TALK to
// systemd — dial the bus, exec systemctl, implement the control channel for
// production use — and a double talks to nothing at all. The real channel's own
// behavior is pinned in internal/systemd's tests; what is pinned here is the
// supervisor's decisions given an answer, which is a different question and one
// that cannot be asked at all if every test needs a live manager.
//
// The doubles are deliberately mechanical: the controller records calls and
// answers with whatever properties a test set, and the prober answers with
// whatever status a test set. Anything cleverer would be a second
// implementation of the thing under test.

// fakeController is a systemd.Controller whose unit properties a test writes
// directly.
type fakeController struct {
	mu sync.Mutex

	props map[string]systemd.UnitProps
	// starts, stops and enables record every call, in order, so a test can
	// assert the "at most one corrective action per pass" rule rather than
	// assume it.
	starts  []string
	stops   []string
	enables []string
	// onStart runs inside Start, after the call is recorded. It is how a test
	// says what a start DOES — most usefully, opening the ledger row the real
	// `instance-exec` would open.
	onStart  func(unit string)
	startErr error
}

func newFakeController() *fakeController {
	return &fakeController{props: map[string]systemd.UnitProps{}}
}

// setUnit writes what the manager will report for a unit.
func (c *fakeController) setUnit(unit string, p systemd.UnitProps) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.props[unit] = p
}

// setActive is the shorthand for a running unit.
func (c *fakeController) setActive(unit string, pid uint32) {
	c.setUnit(unit, systemd.UnitProps{
		ActiveState: "active", SubState: "running", Result: "success", MainPID: pid,
	})
}

// setExited is the shorthand for a unit whose main process is gone, with the
// status and result systemd would report.
func (c *fakeController) setExited(unit string, status int32, result string, at time.Time) {
	state, sub := "inactive", "dead"
	if status != 0 || result != "success" {
		state, sub = "failed", "failed"
	}
	c.setUnit(unit, systemd.UnitProps{
		ActiveState: state, SubState: sub, Result: result,
		ExecMainStatus: status, ExecMainExitTimestamp: at,
	})
}

func (c *fakeController) callsToStart() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.starts...)
}

func (c *fakeController) callsToStop() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.stops...)
}

func (c *fakeController) Start(ctx context.Context, unit string) (systemd.JobResult, error) {
	c.mu.Lock()
	c.starts = append(c.starts, unit)
	onStart, err := c.onStart, c.startErr
	c.mu.Unlock()

	if err != nil {
		return systemd.JobFailed, err
	}
	if onStart != nil {
		onStart(unit)
	}
	return systemd.JobDone, nil
}

func (c *fakeController) Stop(ctx context.Context, unit string) (systemd.JobResult, error) {
	c.mu.Lock()
	c.stops = append(c.stops, unit)
	c.mu.Unlock()
	return systemd.JobDone, nil
}

func (c *fakeController) Restart(ctx context.Context, unit string) (systemd.JobResult, error) {
	return c.Start(ctx, unit)
}

func (c *fakeController) StartNoWait(ctx context.Context, unit string) (systemd.JobPath, error) {
	_, err := c.Start(ctx, unit)
	return "", err
}

func (c *fakeController) RestartNoWait(ctx context.Context, unit string) (systemd.JobPath, error) {
	return c.StartNoWait(ctx, unit)
}

func (c *fakeController) Enable(ctx context.Context, units []string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.enables = append(c.enables, "enable "+strings.Join(units, ","))
	return nil
}

func (c *fakeController) Disable(ctx context.Context, units []string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.enables = append(c.enables, "disable "+strings.Join(units, ","))
	return nil
}

func (c *fakeController) ResetFailed(ctx context.Context, unit string) error { return nil }

func (c *fakeController) Props(ctx context.Context, unit string) (systemd.UnitProps, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.props[unit]
	if !ok {
		return systemd.UnitProps{}, systemd.ErrNoSuchUnit
	}
	return p, nil
}

func (c *fakeController) SubscribeSubState(ctx context.Context, pattern string) (<-chan systemd.SubStateEvent, error) {
	ch := make(chan systemd.SubStateEvent)
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}

// fakeProber answers `/health` and `/props` from values a test sets.
type fakeProber struct {
	mu    sync.Mutex
	code  int
	err   error
	props supervisor.Props
	// calls counts probes, so a test can assert that a stopped instance is
	// never probed — a 2 s timeout per stopped instance per tick would be the
	// difference between a reconciler and a stall.
	calls int
}

func (p *fakeProber) set(code int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.code, p.err = code, nil
}

func (p *fakeProber) unreachable() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.code, p.err = 0, context.DeadlineExceeded
}

func (p *fakeProber) Health(ctx context.Context, port int) supervisor.Health {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	return supervisor.Health{Code: p.code, Err: p.err}
}

func (p *fakeProber) Props(ctx context.Context, port int) (supervisor.Props, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.props, nil
}

// recordingEvents captures the `events` rows a pass appends, so the coupling
// and the refusal can be asserted as auditable facts rather than as side
// effects nobody can see.
type recordingEvents struct {
	mu        sync.Mutex
	appended  []model.Event
	published []model.Event
}

func (e *recordingEvents) Append(ctx context.Context, tx store.Tx, ev model.Event) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.appended = append(e.appended, ev)
	return nil
}

func (e *recordingEvents) Publish(ev model.Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.published = append(e.published, ev)
}

func (e *recordingEvents) actions() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, 0, len(e.appended))
	for _, ev := range e.appended {
		out = append(out, ev.Action)
	}
	return out
}

// fakeSettings answers the four keys the supervisor reads.
type fakeSettings map[string]int64

func (s fakeSettings) GetInt(ctx context.Context, key string) (int64, error) {
	if v, ok := s[key]; ok {
		return v, nil
	}
	return 0, nil
}

// testClock drives every instant the supervisor stamps. The crash-loop window,
// the backoff and the 60 s ready-settle are all arithmetic on an instant, so
// driving it by hand is what makes them assertable without sleeping.
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

// monotonicIDs mints ids that sort in creation order, which is what the ledger's
// `ORDER BY at DESC, id DESC` needs when a test writes several rows at one
// frozen instant.
type monotonicIDs struct {
	mu sync.Mutex
	n  int
}

func (m *monotonicIDs) next(time.Time) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.n++
	return "row-" + string(rune('a'+m.n/26)) + string(rune('a'+m.n%26))
}
