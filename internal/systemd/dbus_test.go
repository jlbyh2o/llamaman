package systemd

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	sddbus "github.com/coreos/go-systemd/v22/dbus"
	godbus "github.com/godbus/dbus/v5"
	"github.com/google/go-cmp/cmp"
)

// fakeBus is a busConn a test drives.
//
// It exists because *dbus.Conn is a concrete struct wrapping a live socket:
// without a seam, every assertion about job completion, error identity and
// reconnection would need a running systemd and would therefore not be written.
type fakeBus struct {
	mu    sync.Mutex
	calls []call

	// jobResult is what JobRemoved reports for the next blocking verb.
	jobResult string
	// jobDelay defers that report, so a test can expire a context first.
	jobDelay time.Duration
	// err, when set, fails the next verb.
	err error

	props    map[string]any
	propsErr error

	// pathProps answers GetUnitPathProperties for the units push() has
	// announced, and sig is the signal source armSubscription attaches to.
	pathProps map[godbus.ObjectPath]map[string]any
	sig       *fakeSignals

	connected bool
	closes    int
}

type call struct {
	Method string
	Unit   string
	Units  []string
	// Waited records whether the caller passed a JobRemoved channel, which is
	// the whole difference between the blocking and no-wait variants.
	Waited bool
}

func newFakeBus() *fakeBus {
	return &fakeBus{
		jobResult: string(JobDone),
		connected: true,
		props:     map[string]any{},
		pathProps: map[godbus.ObjectPath]map[string]any{},
		sig:       newFakeSignals(),
	}
}

// fakeSignals stands in for the manager's PropertiesChanged stream.
type fakeSignals struct {
	paths chan godbus.ObjectPath
	once  sync.Once
}

func newFakeSignals() *fakeSignals {
	return &fakeSignals{paths: make(chan godbus.ObjectPath, 64)}
}

func (f *fakeSignals) Paths() <-chan godbus.ObjectPath { return f.paths }

func (f *fakeSignals) Close() error {
	f.once.Do(func() { close(f.paths) })
	return nil
}

// unitPathFor is systemd's own escaping, so the test exercises the real
// path-to-name decoding rather than a convenient shortcut.
func unitPathFor(unit string) godbus.ObjectPath {
	return godbus.ObjectPath(unitObjectPrefix + sddbus.PathBusEscape(unit))
}

func (f *fakeBus) record(c call) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
}

func (f *fakeBus) recorded() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call(nil), f.calls...)
}

func (f *fakeBus) job(method, unit string, ch chan<- string) (int, error) {
	f.record(call{Method: method, Unit: unit, Waited: ch != nil})
	f.mu.Lock()
	err, result, delay := f.err, f.jobResult, f.jobDelay
	f.mu.Unlock()
	if err != nil {
		return 0, err
	}
	if ch != nil {
		go func() {
			if delay > 0 {
				time.Sleep(delay)
			}
			ch <- result
		}()
	}
	return 42, nil
}

func (f *fakeBus) StartUnitContext(_ context.Context, name, _ string, ch chan<- string) (int, error) {
	return f.job("StartUnit", name, ch)
}

func (f *fakeBus) StopUnitContext(_ context.Context, name, _ string, ch chan<- string) (int, error) {
	return f.job("StopUnit", name, ch)
}

func (f *fakeBus) RestartUnitContext(_ context.Context, name, _ string, ch chan<- string) (int, error) {
	return f.job("RestartUnit", name, ch)
}

func (f *fakeBus) ResetFailedUnitContext(_ context.Context, name string) error {
	f.record(call{Method: "ResetFailedUnit", Unit: name})
	return f.err
}

func (f *fakeBus) EnableUnitFilesContext(_ context.Context, files []string, _, _ bool) (bool, []sddbus.EnableUnitFileChange, error) {
	f.record(call{Method: "EnableUnitFiles", Units: files})
	return false, nil, f.err
}

func (f *fakeBus) DisableUnitFilesContext(_ context.Context, files []string, _ bool) ([]sddbus.DisableUnitFileChange, error) {
	f.record(call{Method: "DisableUnitFiles", Units: files})
	return nil, f.err
}

func (f *fakeBus) ReloadContext(context.Context) error {
	f.record(call{Method: "Reload"})
	return f.err
}

func (f *fakeBus) GetAllPropertiesContext(_ context.Context, unit string) (map[string]any, error) {
	f.record(call{Method: "GetAllProperties", Unit: unit})
	if f.propsErr != nil {
		return nil, f.propsErr
	}
	return f.props, nil
}

func (f *fakeBus) GetUnitPathPropertiesContext(_ context.Context, path godbus.ObjectPath) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	props, ok := f.pathProps[path]
	if !ok {
		return nil, godbus.Error{Name: dbusNoSuchUnit}
	}
	f.calls = append(f.calls, call{Method: "GetUnitPathProperties", Unit: string(path)})
	return props, nil
}

// push announces a unit transition the way systemd does: a signal naming the
// unit's object path, with the new sub-state readable from the object.
func (f *fakeBus) push(unit, sub string) {
	path := unitPathFor(unit)
	f.mu.Lock()
	f.pathProps[path] = map[string]any{"Id": unit, "SubState": sub}
	f.mu.Unlock()
	f.sig.paths <- path
}

func (f *fakeBus) Connected() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connected
}

func (f *fakeBus) disconnect() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connected = false
}

func (f *fakeBus) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes++
	f.connected = false
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestController wires a DBusController onto one fake bus.
func newTestController(t *testing.T, bus *fakeBus) *DBusController {
	t.Helper()

	c, err := NewDBusController(context.Background(), Options{
		Scope:          "system",
		Logger:         quietLogger(),
		dial:           func(context.Context) (busConn, error) { return bus, nil },
		dialSignals:    func(context.Context) (signalSource, error) { return bus.sig, nil },
		healthInterval: time.Hour, // the supervisor is exercised by its own test
	})
	if err != nil {
		t.Fatalf("NewDBusController: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestDBusVerbs is the contract for the three blocking verbs and the two
// no-wait ones. The assertion that matters is the last column: a blocking verb
// passes a JobRemoved channel, a no-wait one does not, and that is the whole
// mechanism keeping the two calls of section 5.3 from deadlocking a request
// goroutine against its own death.
func TestDBusVerbs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		invoke     func(context.Context, Controller) error
		wantMethod string
		wantWaited bool
	}{
		{
			name:       "Start blocks on JobRemoved",
			invoke:     func(ctx context.Context, c Controller) error { _, err := c.Start(ctx, "u.service"); return err },
			wantMethod: "StartUnit", wantWaited: true,
		},
		{
			name:       "Stop blocks on JobRemoved",
			invoke:     func(ctx context.Context, c Controller) error { _, err := c.Stop(ctx, "u.service"); return err },
			wantMethod: "StopUnit", wantWaited: true,
		},
		{
			name:       "Restart blocks on JobRemoved",
			invoke:     func(ctx context.Context, c Controller) error { _, err := c.Restart(ctx, "u.service"); return err },
			wantMethod: "RestartUnit", wantWaited: true,
		},
		{
			name:       "StartNoWait does not",
			invoke:     func(ctx context.Context, c Controller) error { _, err := c.StartNoWait(ctx, "u.service"); return err },
			wantMethod: "StartUnit", wantWaited: false,
		},
		{
			name:       "RestartNoWait does not",
			invoke:     func(ctx context.Context, c Controller) error { _, err := c.RestartNoWait(ctx, "u.service"); return err },
			wantMethod: "RestartUnit", wantWaited: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			bus := newFakeBus()
			c := newTestController(t, bus)
			if err := tc.invoke(context.Background(), c); err != nil {
				t.Fatalf("invoke: %v", err)
			}

			got := bus.recorded()
			if len(got) != 1 {
				t.Fatalf("calls = %v, want exactly one", got)
			}
			if got[0].Method != tc.wantMethod || got[0].Unit != "u.service" {
				t.Errorf("call = %+v, want %s on u.service", got[0], tc.wantMethod)
			}
			if got[0].Waited != tc.wantWaited {
				t.Errorf("passed a JobRemoved channel = %v, want %v", got[0].Waited, tc.wantWaited)
			}
		})
	}
}

// TestDBusJobResult: whatever systemd reports for the job is what the caller
// sees, including the failures that are not errors on the wire.
func TestDBusJobResult(t *testing.T) {
	t.Parallel()

	for _, want := range []JobResult{JobDone, JobFailed, JobCanceled, JobTimeout, JobDependency, JobSkipped} {
		t.Run(string(want), func(t *testing.T) {
			t.Parallel()

			bus := newFakeBus()
			bus.jobResult = string(want)
			c := newTestController(t, bus)

			got, err := c.Start(context.Background(), "u.service")
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			if got != want {
				t.Errorf("Start = %q, want %q", got, want)
			}
			if got.OK() != (want == JobDone) {
				t.Errorf("%q.OK() = %v", got, got.OK())
			}
		})
	}
}

// TestDBusJobRespectsContext: a caller whose context expires stops waiting; the
// job itself is left alone, because a StopUnit issued to undo a start is a
// different job, not a cancellation.
func TestDBusJobRespectsContext(t *testing.T) {
	t.Parallel()

	bus := newFakeBus()
	bus.jobDelay = time.Minute
	c := newTestController(t, bus)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	if _, err := c.Start(ctx, "u.service"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Start = %v, want a deadline error", err)
	}
}

// TestDBusErrorIdentity is the reason section 5.3 prefers the bus: a missing
// unit and a polkit denial arrive as NAMES, not as an exit code plus English
// prose, and the two must not collapse into one error the UI cannot act on.
func TestDBusErrorIdentity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		dbusErr error
		want    error
	}{
		{"no such unit", godbus.Error{Name: dbusNoSuchUnit}, ErrNoSuchUnit},
		{"load failed", godbus.Error{Name: dbusLoadFailed}, ErrNoSuchUnit},
		{"access denied", godbus.Error{Name: dbusAccessDenied}, ErrDenied},
		{"interactive auth required", godbus.Error{Name: dbusInteractive}, ErrDenied},
		{"not supported", godbus.Error{Name: dbusNotSupported}, ErrUnavailable},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			bus := newFakeBus()
			bus.err = tc.dbusErr
			c := newTestController(t, bus)

			if _, err := c.Start(context.Background(), "u.service"); !errors.Is(err, tc.want) {
				t.Errorf("Start = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestDBusEnableReloads: EnableUnitFiles writes symlinks the manager has not
// read, so the reload is part of the operation rather than a caller's
// responsibility — which is also why the polkit rule grants reload-daemon in its
// own branch.
func TestDBusEnableReloads(t *testing.T) {
	t.Parallel()

	bus := newFakeBus()
	c := newTestController(t, bus)
	units := []string{"llamaman-instance@a.service", "llamaman-instance@b.service"}

	if err := c.Enable(context.Background(), units); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if err := c.Disable(context.Background(), units); err != nil {
		t.Fatalf("Disable: %v", err)
	}

	var methods []string
	for _, c := range bus.recorded() {
		methods = append(methods, c.Method)
	}
	want := []string{"EnableUnitFiles", "Reload", "DisableUnitFiles", "Reload"}
	if diff := cmp.Diff(want, methods); diff != "" {
		t.Errorf("calls (-want +got):\n%s", diff)
	}

	// An empty set is a no-op, not a bus round trip: the supervisor computes a
	// diff every pass and most passes have nothing to do.
	before := len(bus.recorded())
	if err := c.Enable(context.Background(), nil); err != nil {
		t.Fatalf("Enable(nil): %v", err)
	}
	if got := len(bus.recorded()); got != before {
		t.Errorf("Enable(nil) made %d calls, want none", got-before)
	}
}

// TestDBusProps covers the variant-map decoding, including the two values that
// would be nonsense if passed through: an unset MemoryCurrent, which systemd
// reports as UINT64_MAX, and a zero exit timestamp, which is "never" rather than
// 1970.
func TestDBusProps(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		props map[string]any
		want  UnitProps
	}{
		{
			name: "a running service",
			props: map[string]any{
				"ActiveState":           "active",
				"SubState":              "running",
				"MainPID":               uint32(8421),
				"ExecMainStatus":        int32(0),
				"Result":                "success",
				"NRestarts":             uint32(2),
				"MemoryCurrent":         uint64(6066176),
				"ExecMainExitTimestamp": uint64(0),
			},
			want: UnitProps{
				ActiveState: "active", SubState: "running", MainPID: 8421,
				Result: "success", NRestarts: 2, MemoryCurrent: 6066176,
			},
		},
		{
			name: "an unset MemoryCurrent is zero, not 16 exabytes",
			props: map[string]any{
				"ActiveState":   "inactive",
				"SubState":      "dead",
				"MemoryCurrent": uint64(math.MaxUint64),
			},
			want: UnitProps{ActiveState: "inactive", SubState: "dead"},
		},
		{
			name: "an exited service carries its status and exit instant",
			props: map[string]any{
				"ActiveState":           "failed",
				"SubState":              "failed",
				"ExecMainStatus":        int32(78),
				"Result":                "exit-code",
				"ExecMainExitTimestamp": uint64(1788012345678000),
			},
			want: UnitProps{
				ActiveState: "failed", SubState: "failed", ExecMainStatus: 78,
				Result:                "exit-code",
				ExecMainExitTimestamp: time.UnixMicro(1788012345678000).UTC(),
			},
		},
		{
			name:  "an empty property set decodes to the zero value",
			props: map[string]any{},
			want:  UnitProps{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			bus := newFakeBus()
			bus.props = tc.props
			c := newTestController(t, bus)

			got, err := c.Props(context.Background(), "u.service")
			if err != nil {
				t.Fatalf("Props: %v", err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("Props (-want +got):\n%s", diff)
			}
		})
	}
}

// TestDBusSubscribeSubState: each subscriber sees only the units its pattern
// matches, which is what lets the supervisor watch every instance without also
// receiving every unit on the host.
func TestDBusSubscribeSubState(t *testing.T) {
	t.Parallel()

	bus := newFakeBus()
	c := newTestController(t, bus)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	instances, err := c.SubscribeSubState(ctx, "llamaman-instance@*.service")
	if err != nil {
		t.Fatalf("SubscribeSubState: %v", err)
	}
	daemon, err := c.SubscribeSubState(ctx, UnitDaemon)
	if err != nil {
		t.Fatalf("SubscribeSubState: %v", err)
	}

	bus.push("llamaman-instance@qwen.service", "running")
	bus.push("sshd.service", "running")
	bus.push(UnitDaemon, "running")

	got := receive(t, instances)
	if got.Unit != "llamaman-instance@qwen.service" || got.SubState != "running" {
		t.Errorf("instance subscriber got %+v", got)
	}
	if extra, ok := tryReceive(instances); ok {
		t.Errorf("instance subscriber also got %+v; sshd and the daemon are not instances", extra)
	}

	got = receive(t, daemon)
	if got.Unit != UnitDaemon {
		t.Errorf("daemon subscriber got %+v", got)
	}
}

// TestDBusSubscribeClosesOnContext: a log viewer that navigated away must not
// leak a subscriber, and its channel must close rather than block forever.
func TestDBusSubscribeClosesOnContext(t *testing.T) {
	t.Parallel()

	bus := newFakeBus()
	c := newTestController(t, bus)

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := c.SubscribeSubState(ctx, "*")
	if err != nil {
		t.Fatalf("SubscribeSubState: %v", err)
	}
	cancel()

	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, open := <-ch:
			if !open {
				return
			}
		case <-deadline:
			t.Fatal("the subscription channel was not closed after its context ended")
		}
	}
}

// TestDBusBadPattern: a malformed glob is refused at subscribe time rather than
// silently matching nothing forever.
func TestDBusBadPattern(t *testing.T) {
	t.Parallel()

	c := newTestController(t, newFakeBus())
	if _, err := c.SubscribeSubState(context.Background(), "llamaman-instance@[a.service"); err == nil {
		t.Fatal("SubscribeSubState accepted a malformed pattern")
	}
}

// TestDBusReconnect drives the supervision of section 5.3: a dropped connection
// is re-dialed, the consumer is told so it can resynchronize, and a subscriber
// registered against the OLD connection keeps receiving from the new one.
func TestDBusReconnect(t *testing.T) {
	t.Parallel()

	first, second := newFakeBus(), newFakeBus()
	dials := make(chan *fakeBus, 2)
	dials <- first
	dials <- second

	var mu sync.Mutex
	current := first

	resync := make(chan struct{}, 1)
	c, err := NewDBusController(context.Background(), Options{
		Scope:  "system",
		Logger: quietLogger(),
		dial: func(context.Context) (busConn, error) {
			select {
			case b := <-dials:
				mu.Lock()
				current = b
				mu.Unlock()
				return b, nil
			default:
				return nil, errors.New("no more buses")
			}
		},
		dialSignals: func(context.Context) (signalSource, error) {
			mu.Lock()
			defer mu.Unlock()
			return current.sig, nil
		},
		OnReconnect:    func() { resync <- struct{}{} },
		healthInterval: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewDBusController: %v", err)
	}
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, err := c.SubscribeSubState(ctx, "*")
	if err != nil {
		t.Fatalf("SubscribeSubState: %v", err)
	}

	first.disconnect()

	select {
	case <-resync:
	case <-time.After(5 * time.Second):
		t.Fatal("OnReconnect never fired after the connection dropped")
	}

	// The subscription survived: the underlying connection was replaced, the
	// subscriber's channel was not.
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return current == second
	}, "the second connection was never dialed")

	second.push("llamaman.service", "running")
	if got := receive(t, events); got.Unit != "llamaman.service" {
		t.Errorf("after reconnect got %+v", got)
	}
}

// TestDBusDisconnectedCallsFail: while the reconnect is in flight, calls report
// a distinct, retryable error rather than the "no systemd at all" one that would
// make the daemon degrade permanently.
func TestDBusDisconnectedCallsFail(t *testing.T) {
	t.Parallel()

	bus := newFakeBus()
	c := newTestController(t, bus)

	c.mu.Lock()
	c.conn = nil
	c.mu.Unlock()

	if _, err := c.Start(context.Background(), "u.service"); !errors.Is(err, ErrDisconnected) {
		t.Errorf("Start while disconnected = %v, want ErrDisconnected", err)
	}
	if _, err := c.Props(context.Background(), "u.service"); !errors.Is(err, ErrDisconnected) {
		t.Errorf("Props while disconnected = %v, want ErrDisconnected", err)
	}
}

// TestDBusDialFailureIsUnavailable: a controller that cannot connect at all
// reports the degraded mode of section 11.1a rather than a bare dial error.
func TestDBusDialFailureIsUnavailable(t *testing.T) {
	t.Parallel()

	_, err := NewDBusController(context.Background(), Options{
		Scope:  "system",
		Logger: quietLogger(),
		dial:   func(context.Context) (busConn, error) { return nil, errors.New("no bus") },
	})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("NewDBusController = %v, want ErrUnavailable", err)
	}
}

// TestConnectFallsBackToExec: the boot probe prefers the bus and settles for
// systemctl, and it says which one won so the UI can tell the user whether state
// is pushed or polled.
func TestConnectFallsBackToExec(t *testing.T) {
	restore := setSystemctlPath("/usr/bin/systemctl")
	defer restore()

	c, control, err := Connect(context.Background(), Options{
		Scope:  "system",
		Logger: quietLogger(),
		dial:   func(context.Context) (busConn, error) { return nil, errors.New("no bus") },
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() {
		if cl, ok := c.(interface{ Close() error }); ok {
			_ = cl.Close()
		}
	}()

	if control != "exec" {
		t.Errorf("control = %q, want exec", control)
	}
	if _, ok := c.(*ExecController); !ok {
		t.Errorf("Connect returned %T, want *ExecController", c)
	}
}

func receive(t *testing.T, ch <-chan SubStateEvent) SubStateEvent {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("the subscription channel closed unexpectedly")
		}
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a sub-state event")
		return SubStateEvent{}
	}
}

func tryReceive(ch <-chan SubStateEvent) (SubStateEvent, bool) {
	select {
	case ev := <-ch:
		return ev, true
	case <-time.After(100 * time.Millisecond):
		return SubStateEvent{}, false
	}
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal(msg)
}

// TestDBusPropsUnknownUnit: systemd materializes a unit object on demand and
// answers a properties read for a name it has never heard of with
// LoadState=not-found and no error. Reading that as a healthy stopped unit is
// how a deleted instance's unit ends up being started forever.
func TestDBusPropsUnknownUnit(t *testing.T) {
	t.Parallel()

	bus := newFakeBus()
	bus.props = map[string]any{
		"LoadState":   "not-found",
		"ActiveState": "inactive",
		"SubState":    "dead",
	}
	c := newTestController(t, bus)

	if _, err := c.Props(context.Background(), "nope.service"); !errors.Is(err, ErrNoSuchUnit) {
		t.Fatalf("Props = %v, want ErrNoSuchUnit", err)
	}
}

// TestDBusSubStateIsDeduplicated: systemd emits several PropertiesChanged per
// state change, and forwarding each one would make the supervisor take the same
// corrective action several times for one transition.
func TestDBusSubStateIsDeduplicated(t *testing.T) {
	t.Parallel()

	bus := newFakeBus()
	c := newTestController(t, bus)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, err := c.SubscribeSubState(ctx, "llamaman-instance@*.service")
	if err != nil {
		t.Fatalf("SubscribeSubState: %v", err)
	}

	// The signal is only a trigger — the value is read back from the unit
	// object — so each transition is settled before the next is announced,
	// exactly as systemd delivers them.
	bus.push("llamaman-instance@qwen.service", "running")
	if got := receive(t, events); got.SubState != "running" {
		t.Fatalf("first event = %+v", got)
	}

	// A second signal carrying no change: systemd emits several
	// PropertiesChanged per state change, and forwarding each would make the
	// supervisor take the same corrective action several times.
	bus.push("llamaman-instance@qwen.service", "running")
	bus.push("llamaman-instance@qwen.service", "failed")

	if got := receive(t, events); got.SubState != "failed" {
		t.Errorf("second event = %+v, want the failed transition (the repeat was forwarded)", got)
	}
	if extra, ok := tryReceive(events); ok {
		t.Errorf("a third event arrived: %+v", extra)
	}
}

// TestDBusSkipsUnwatchedUnits: the unit name is decoded from the object path so
// that a unit nobody subscribed to costs no D-Bus round trip at all. On a system
// manager that is nearly every signal.
func TestDBusSkipsUnwatchedUnits(t *testing.T) {
	t.Parallel()

	bus := newFakeBus()
	c := newTestController(t, bus)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, err := c.SubscribeSubState(ctx, "llamaman-instance@*.service")
	if err != nil {
		t.Fatalf("SubscribeSubState: %v", err)
	}

	bus.push("sshd.service", "running")
	bus.push("llamaman-instance@qwen.service", "running")

	if got := receive(t, events); got.Unit != "llamaman-instance@qwen.service" {
		t.Fatalf("event = %+v", got)
	}
	for _, c := range bus.recorded() {
		if c.Method == "GetUnitPathProperties" && strings.Contains(c.Unit, "sshd") {
			t.Error("properties were read for a unit nobody subscribed to")
		}
	}
}
