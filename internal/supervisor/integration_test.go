package supervisor_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/netutil"
	"github.com/jlbyh2o/llamaman/internal/store/storetest"
	"github.com/jlbyh2o/llamaman/internal/supervisor"
	"github.com/jlbyh2o/llamaman/internal/systemd"
)

// The two halves of the start protocol, meeting for real.
//
// Everything else in this package tests the supervisor against a faked systemd
// and a faked launcher; internal/cli tests the launcher against a faked exec.
// Neither proves the HAND-OFF: that the trigger the supervisor stamps is the
// trigger the launcher consumes, that the row the launcher opens is the row the
// supervisor stamps ready, and that the exit status a real process produces is
// the one the ledger records. That is what runs here.
//
// systemd is replaced by a PLAIN-PROCESS controller rather than by a user
// manager, deliberately. What the units contribute to this protocol is a
// process lifecycle and an exit status, both of which a child process has; what
// a user manager would add is a dependency on the test host's session, a bus,
// and installed unit files, none of which the protocol depends on. A suite that
// needs all three is a suite that does not run.

// procController is a systemd.Controller backed by child processes.
//
// `Start` runs the REAL `llamaman instance-exec <name>`, which opens its own
// database connection, runs its own preflight, and `execve`s the stub — so the
// pid the supervisor sees is the launcher's pid and then the server's, exactly
// as it is under a real unit.
type procController struct {
	mu       sync.Mutex
	binary   string
	stateDir string
	// env is the STUBLLAMA_* script the launched server obeys.
	env    []string
	units  map[string]*procUnit
	starts int
	t      *testing.T
}

type procUnit struct {
	cmd     *exec.Cmd
	stderr  *bytes.Buffer
	done    chan struct{}
	running bool
	// stopRequested records that this exit was asked for, which is what lets
	// the controller report `Result=success` the way systemd does for a unit it
	// stopped rather than one that died.
	stopRequested bool
	status        int32
	result        string
	exitAt        time.Time
}

func newProcController(t *testing.T, binary, stateDir string, env ...string) *procController {
	return &procController{
		binary: binary, stateDir: stateDir, env: env,
		units: map[string]*procUnit{}, t: t,
	}
}

// instanceOf recovers `%i` from a unit name, which is the only thing the
// template carries (§5.5).
func instanceOf(unit string) string {
	name := strings.TrimSuffix(unit, ".service")
	if i := strings.IndexByte(name, '@'); i >= 0 {
		return name[i+1:]
	}
	return name
}

func (c *procController) Start(ctx context.Context, unit string) (systemd.JobResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if u, ok := c.units[unit]; ok && u.running {
		return systemd.JobDone, nil
	}

	cmd := exec.Command(c.binary, "instance-exec", instanceOf(unit), "--force")
	cmd.Env = append(append(os.Environ(),
		"STATE_DIRECTORY="+c.stateDir), c.env...)
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	cmd.Stdout = stderr

	u := &procUnit{cmd: cmd, stderr: stderr, done: make(chan struct{})}
	if err := cmd.Start(); err != nil {
		return systemd.JobFailed, err
	}
	c.starts++
	u.running = true
	c.units[unit] = u

	go func() {
		err := cmd.Wait()
		c.mu.Lock()
		defer c.mu.Unlock()
		u.running = false
		u.exitAt = time.Now()
		defer close(u.done)
		u.status = int32(cmd.ProcessState.ExitCode())
		switch {
		case u.stopRequested:
			// systemd reports a unit it stopped as `success`, whatever the
			// process did on the way out — which is why the §5.6 writer table
			// has to key "an explicit stop was requested" off the DATABASE
			// rather than off the exit status.
			u.result = "success"
		case u.status == 0:
			u.result = "success"
		case u.status < 0:
			u.status = 0
			u.result = "signal"
		default:
			u.result = "exit-code"
		}
		_ = err
	}()
	return systemd.JobDone, nil
}

func (c *procController) Stop(ctx context.Context, unit string) (systemd.JobResult, error) {
	c.mu.Lock()
	u, ok := c.units[unit]
	if !ok || !u.running {
		c.mu.Unlock()
		return systemd.JobDone, nil
	}
	u.stopRequested = true
	proc := u.cmd.Process
	done := u.done
	c.mu.Unlock()

	_ = proc.Kill()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	// Wait for the reaper goroutine to record the status.
	c.waitExited(unit, 5*time.Second)
	return systemd.JobDone, nil
}

// waitExited blocks until the unit's process has been reaped, which is what
// makes the next Props call describe a finished run rather than a racing one.
func (c *procController) waitExited(unit string, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		u, ok := c.units[unit]
		running := ok && u.running
		c.mu.Unlock()
		if ok && !running {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

func (c *procController) Props(ctx context.Context, unit string) (systemd.UnitProps, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	u, ok := c.units[unit]
	if !ok {
		// An installed template instance that has never been started is loaded
		// and dead, not unknown.
		return systemd.UnitProps{ActiveState: "inactive", SubState: "dead"}, nil
	}
	if u.running {
		return systemd.UnitProps{
			ActiveState: "active", SubState: "running", Result: "success",
			MainPID: uint32(u.cmd.Process.Pid),
		}, nil
	}
	state, sub := "inactive", "dead"
	if u.result != "success" {
		state, sub = "failed", "failed"
	}
	return systemd.UnitProps{
		ActiveState: state, SubState: sub, Result: u.result,
		ExecMainStatus: u.status, ExecMainExitTimestamp: u.exitAt,
	}, nil
}

// startCount is how many times Start has been called, which is what "no further
// automatic starts" is asserted against.
func (c *procController) startCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.starts
}

// journal returns everything the launcher and the server wrote, for a failure
// message that names the real cause instead of a status code.
func (c *procController) journal(unit string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if u, ok := c.units[unit]; ok {
		return u.stderr.String()
	}
	return ""
}

func (c *procController) Restart(ctx context.Context, unit string) (systemd.JobResult, error) {
	if _, err := c.Stop(ctx, unit); err != nil {
		return systemd.JobFailed, err
	}
	return c.Start(ctx, unit)
}

func (c *procController) StartNoWait(ctx context.Context, unit string) (systemd.JobPath, error) {
	_, err := c.Start(ctx, unit)
	return "", err
}

func (c *procController) RestartNoWait(ctx context.Context, unit string) (systemd.JobPath, error) {
	_, err := c.Restart(ctx, unit)
	return "", err
}

func (c *procController) Enable(context.Context, []string) error  { return nil }
func (c *procController) Disable(context.Context, []string) error { return nil }
func (c *procController) ResetFailed(context.Context, string) error {
	return nil
}

func (c *procController) SubscribeSubState(ctx context.Context, pattern string) (<-chan systemd.SubStateEvent, error) {
	ch := make(chan systemd.SubStateEvent)
	go func() { <-ctx.Done(); close(ch) }()
	return ch, nil
}

// killAll reaps anything still running, so a failing test cannot leave a
// listener behind for the next one.
func (c *procController) killAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, u := range c.units {
		if u.running && u.cmd.Process != nil {
			_ = u.cmd.Process.Kill()
			_, _ = u.cmd.Process.Wait()
		}
	}
}

// --- building the two real binaries ------------------------------------------

var (
	buildOnce   sync.Once
	builtDir    string
	builtErr    error
	llamamanBin string
	stubBin     string
)

// buildBinaries compiles `llamaman` and the stub server once per package run.
//
// The stub is its own module under testdata/, which is why it needs an explicit
// build here rather than appearing in the root module's package graph: a
// deliberately misbehaving HTTP server has no business being part of the
// product's own build.
func buildBinaries(t *testing.T) (string, string) {
	t.Helper()
	buildOnce.Do(func() {
		builtDir, builtErr = os.MkdirTemp("", "llamaman-supervisor-bins")
		if builtErr != nil {
			return
		}
		llamamanBin = filepath.Join(builtDir, "llamaman")
		stubBin = filepath.Join(builtDir, "llama-server")

		build := exec.Command("go", "build", "-o", llamamanBin,
			"github.com/jlbyh2o/llamaman/cmd/llamaman")
		if out, err := build.CombinedOutput(); err != nil {
			builtErr = fmt.Errorf("build llamaman: %w\n%s", err, out)
			return
		}

		stubSrc, err := filepath.Abs(filepath.Join("..", "..", "testdata", "stubllama"))
		if err != nil {
			builtErr = err
			return
		}
		build = exec.Command("go", "build", "-o", stubBin, ".")
		build.Dir = stubSrc
		if out, err := build.CombinedOutput(); err != nil {
			builtErr = fmt.Errorf("build stubllama: %w\n%s", err, out)
			return
		}
	})
	if builtErr != nil {
		t.Fatalf("%v", builtErr)
	}
	return llamamanBin, stubBin
}

// freePort takes a port the kernel says is free right now. The answer is
// advisory the moment it returns — which is exactly why the launcher's step 8
// exists — but it is enough to keep two parallel tests off each other's ports.
func freePort(t *testing.T) int {
	t.Helper()
	ln, port, err := netutil.Ephemeral("127.0.0.1")
	if err != nil {
		t.Fatalf("find a free port: %v", err)
	}
	ln.Close()
	return port
}

type integrationFixture struct {
	*storetest.StateDir
	sup  *supervisor.Supervisor
	ctl  *procController
	inst model.Instance
	unit string
	// clock is real time plus an offset the test advances by hand.
	//
	// The processes are real, so the health probe and the model "load" have to
	// happen in real time — but the RESTART BACKOFF is arithmetic on an instant
	// (5 s doubling to 5 m), and sleeping through it would make this suite spend
	// minutes proving something that is already asserted as a pure function in
	// policy_test.go. Shifting the supervisor's clock forward one step per pass
	// exercises the same branch at the same cost as any other pass.
	clock *shiftClock
}

// clockStep is how far the supervisor's clock moves per pass.
const clockStep = 2 * supervisor.BackoffMin

// shiftClock is time.Now plus an offset.
type shiftClock struct {
	mu     sync.Mutex
	offset time.Duration
}

func (c *shiftClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.offset)
}

func (c *shiftClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset += d
}

func newIntegrationFixture(t *testing.T, mutate func(*model.Instance), env ...string) *integrationFixture {
	t.Helper()
	if testing.Short() {
		t.Skip("builds two binaries and runs real processes")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("the go tool is needed to build the stub server")
	}

	llamaman, stub := buildBinaries(t)
	sd := storetest.NewStateDir(t, testVersionID, stub)
	sd.SeedModel(t, "m-1", true)

	inst := storetest.NewInstance("i-1", "qwen", "m-1", freePort(t), freePort(t))
	inst.DesiredState = model.DesiredRunning
	if mutate != nil {
		mutate(&inst)
	}
	sd.SeedInstance(t, inst)

	ctl := newProcController(t, llamaman, sd.Dir, env...)
	t.Cleanup(ctl.killAll)

	clock := &shiftClock{}
	sup, err := supervisor.New(supervisor.Config{
		Now:      clock.now,
		Store:    sd.DB,
		Settings: fakeSettings{"instances.start_timeout_sec": 30},
		Events:   &recordingEvents{},
		Control:  ctl,
		StateDir: sd.Dir,
		// The health prober is the REAL one: this test is the only place the
		// `starting → loading → ready` sequence is driven by an actual HTTP
		// server answering on an actual port.
		Host: func() (supervisor.HostBoot, error) {
			return supervisor.HostBoot{ID: "boot-1", At: time.Now().Add(-time.Hour)}, nil
		},
		Exe: func(int) (string, error) { return "", context.Canceled },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &integrationFixture{
		StateDir: sd, sup: sup, ctl: ctl, inst: inst, unit: inst.UnitName, clock: clock,
	}
}

// reconcileUntil runs passes until cond holds or the deadline passes. The
// supervisor takes at most one corrective action per pass, so every scenario
// here is several passes by construction, and a test that ran a fixed number
// would be asserting the count rather than the outcome.
func (f *integrationFixture) reconcileUntil(t *testing.T, within time.Duration,
	what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if err := f.sup.Reconcile(context.Background()); err != nil {
			t.Fatalf("Reconcile: %v\njournal:\n%s", err, f.ctl.journal(f.unit))
		}
		if cond() {
			return
		}
		// One step per pass, deliberately SMALL. It has to be big enough to
		// expire the restart backoff (5 s, doubling) and small enough that the
		// failures stay inside `restart_window_sec` — a step of minutes would
		// age each failure out of the crash-loop window before the next one
		// arrived, and the cutoff could never trip at all.
		f.clock.advance(clockStep)
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s\nstate: %s\nledger: %+v\njournal:\n%s",
				what, f.Status(t, f.inst.ID).State, f.Starts(t, f.inst.ID),
				f.ctl.journal(f.unit))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (f *integrationFixture) state(t *testing.T) model.InstanceState {
	t.Helper()
	return f.Status(t, f.inst.ID).State
}

// TestIntegrationFullStartLedger is the whole protocol, with real processes: the
// supervisor stamps a trigger and starts, the real launcher consumes it and
// opens the row, the real stub answers `/health`, the supervisor stamps
// `ready_at` on that row without closing it, and a stop the user asks for closes
// it `stopped`.
func TestIntegrationFullStartLedger(t *testing.T) {
	f := newIntegrationFixture(t, nil,
		// Answer 503 for a moment, so `loading` is a state this test actually
		// passes through rather than one it skips.
		"STUBLLAMA_READY_AFTER=250ms", "STUBLLAMA_CTX=8192", "STUBLLAMA_SLOTS=4")

	f.reconcileUntil(t, 30*time.Second, "the instance to become ready", func() bool {
		return f.state(t) == model.InstanceReady
	})

	starts := f.Starts(t, f.inst.ID)
	if len(starts) != 1 {
		t.Fatalf("got %d ledger rows for one run, want 1: %+v", len(starts), starts)
	}
	row := starts[0]

	// Opened by the launcher, with the trigger the supervisor stamped.
	if row.Trigger != model.StartBySupervisorRestart {
		t.Errorf("trigger = %q, want %q — the hand-off did not survive the process boundary",
			row.Trigger, model.StartBySupervisorRestart)
	}
	if row.ArgvJSON == nil || row.EffectiveConfigHash == nil || row.LlamacppVersionID == nil {
		t.Error("the launcher's step-9 write is missing; the row must describe what ran")
	}
	// Stamped ready by the supervisor, and STILL OPEN: reaching ready is an
	// event within a run, not the end of one (D63).
	if row.ReadyAt == nil {
		t.Error("ready_at was not stamped at the first /health 200")
	}
	if row.Outcome != nil {
		t.Errorf("outcome = %q on a run that is still serving", *row.Outcome)
	}
	st := f.Status(t, f.inst.ID)
	if st.AppliedConfigHash == nil || *st.AppliedConfigHash != *row.EffectiveConfigHash {
		t.Errorf("applied_config_hash = %v, want the row's effective hash %q",
			st.AppliedConfigHash, *row.EffectiveConfigHash)
	}
	if st.MainPID == nil || *st.MainPID == 0 {
		t.Error("main_pid was not recorded for a running instance")
	}
	// `/props` on the first ready: what the SERVER is really serving with,
	// which llama.cpp is entitled to disagree with the instance's flags about.
	if st.CtxSize == nil || *st.CtxSize != 8192 {
		t.Errorf("ctx_size = %v, want the server's own 8192", st.CtxSize)
	}
	if st.SlotsTotal == nil || *st.SlotsTotal != 4 {
		t.Errorf("slots_total = %v, want the server's own 4", st.SlotsTotal)
	}

	// The user stops it. llama-server dies on a signal, and the ledger still
	// says `stopped`: a stop the user asked for is not a failure.
	f.Exec(t, `UPDATE instances SET desired_state = 'stopped' WHERE id = ?`, f.inst.ID)
	f.reconcileUntil(t, 30*time.Second, "the run to be closed", func() bool {
		rows := f.Starts(t, f.inst.ID)
		return len(rows) == 1 && rows[0].Outcome != nil
	})

	closed := f.Starts(t, f.inst.ID)[0]
	if *closed.Outcome != model.OutcomeStopped {
		t.Errorf("outcome = %q, want stopped", *closed.Outcome)
	}
	if closed.EndedAt == nil {
		t.Error("ended_at is NULL on a closed row")
	}
	if got := f.state(t); got != model.InstanceStopped {
		t.Errorf("state = %q, want stopped", got)
	}
}

// TestIntegrationCrashLoopTripsTheCutoff is D8/D64 against a process that really
// does die every time: the supervisor restarts it, the failures accumulate in
// the ledger, and past `restart_max` it latches `crash-looping` and records ONE
// refusal rather than one per pass.
func TestIntegrationCrashLoopTripsTheCutoff(t *testing.T) {
	f := newIntegrationFixture(t, func(i *model.Instance) {
		i.RestartPolicy = model.RestartAlways
		i.RestartMax = 2
		// Wide, because the clock is stepped: what is being asserted is that
		// enough FAILURES accumulate, not how long that takes.
		i.RestartWindowSec = 3600
	},
		// A server that exits non-zero before it ever binds: the crash-on-load
		// case, where the row is open only because the launcher wrote it BEFORE
		// the exec (D54).
		"STUBLLAMA_EXIT_BEFORE_LISTEN=0s", "STUBLLAMA_EXIT_CODE=9")

	f.reconcileUntil(t, 60*time.Second, "the crash-loop latch", func() bool {
		return f.state(t) == model.InstanceCrashLooping
	})

	rows := f.Starts(t, f.inst.ID)
	failed, inhibited := 0, 0
	for _, r := range rows {
		if r.Outcome == nil {
			t.Errorf("a row is still open after the process died: %+v", r)
			continue
		}
		switch *r.Outcome {
		case model.OutcomeFailed:
			failed++
			if r.ExitCode == nil || *r.ExitCode != 9 {
				t.Errorf("exit_code = %v, want the stub's 9 — the status must survive the unit",
					r.ExitCode)
			}
		case model.OutcomeInhibited:
			inhibited++
			if r.ErrorCode == nil || *r.ErrorCode != string(model.InhibitCrashLoop) {
				t.Errorf("inhibit reason = %v, want %q", r.ErrorCode, model.InhibitCrashLoop)
			}
			if r.ExitCode != nil {
				t.Errorf("exit_code = %d on a refusal; no execve happened", *r.ExitCode)
			}
		}
	}
	if failed <= f.inst.RestartMax {
		t.Errorf("recorded %d failures, want more than restart_max (%d)", failed, f.inst.RestartMax)
	}
	if inhibited != 1 {
		t.Errorf("recorded %d refusals, want exactly 1 per episode", inhibited)
	}

	// And it STAYS inhibited: further passes take no action and write no rows.
	before := len(f.Starts(t, f.inst.ID))
	startsBefore := f.ctl.startCount()
	for i := 0; i < 5; i++ {
		f.clock.advance(clockStep)
		if err := f.sup.Reconcile(context.Background()); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
	}
	if got := len(f.Starts(t, f.inst.ID)); got != before {
		t.Errorf("the ledger grew from %d to %d rows while inhibited", before, got)
	}
	if f.ctl.startCount() != startsBefore {
		t.Error("a start was issued while crash-looping")
	}
	if got := f.state(t); got != model.InstanceCrashLooping {
		t.Errorf("state = %q, want the latch to hold", got)
	}
}

// TestIntegrationPreflightFailureIsCounted is what D54 is FOR. A model file that
// has been deleted fails at exit 72 forever; because the launcher opens the row
// before preflight, those failures are countable, and the supervisor stops after
// `restart_max` instead of retrying on backoff until the heat death of the
// universe.
func TestIntegrationPreflightFailureIsCounted(t *testing.T) {
	f := newIntegrationFixture(t, func(i *model.Instance) {
		i.RestartPolicy = model.RestartAlways
		i.RestartMax = 1
		i.RestartWindowSec = 3600
	})

	// The catalog knows about the model; the disk does not.
	var dir, file string
	if err := f.QueryRow(t, `SELECT snapshot_dir, primary_file FROM models WHERE id = 'm-1'`).
		Scan(&dir, &file); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, file)); err != nil {
		t.Fatal(err)
	}

	f.reconcileUntil(t, 60*time.Second, "the crash-loop latch from preflight failures", func() bool {
		return f.state(t) == model.InstanceCrashLooping
	})

	seen72 := false
	for _, r := range f.Starts(t, f.inst.ID) {
		if r.ExitCode != nil && *r.ExitCode == supervisor.ExitModelMissing {
			seen72 = true
			if r.ErrorCode == nil || *r.ErrorCode != supervisor.ErrModelMissing {
				t.Errorf("error_code = %v, want %q", r.ErrorCode, supervisor.ErrModelMissing)
			}
			if r.ArgvJSON != nil {
				t.Error("argv_json is set on a run that never rendered")
			}
		}
	}
	if !seen72 {
		t.Errorf("no exit-72 row in the ledger: %+v", f.Starts(t, f.inst.ID))
	}
}
