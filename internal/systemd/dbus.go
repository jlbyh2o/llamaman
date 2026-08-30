package systemd

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"path"
	"sync"
	"time"

	sddbus "github.com/coreos/go-systemd/v22/dbus"
	godbus "github.com/godbus/dbus/v5"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// busConn is the slice of go-systemd's *dbus.Conn this package actually uses.
//
// It exists so the controller can be driven by a fake in a test: *dbus.Conn is a
// concrete struct wrapping a live socket, and a suite that could only run
// against a real bus would test nothing on a developer's laptop and everything
// twice in CI. Every method below is satisfied by *dbus.Conn as written.
type busConn interface {
	StartUnitContext(ctx context.Context, name, mode string, ch chan<- string) (int, error)
	StopUnitContext(ctx context.Context, name, mode string, ch chan<- string) (int, error)
	RestartUnitContext(ctx context.Context, name, mode string, ch chan<- string) (int, error)
	ResetFailedUnitContext(ctx context.Context, name string) error
	EnableUnitFilesContext(ctx context.Context, files []string, runtime, force bool) (bool, []sddbus.EnableUnitFileChange, error)
	DisableUnitFilesContext(ctx context.Context, files []string, runtime bool) ([]sddbus.DisableUnitFileChange, error)
	ReloadContext(ctx context.Context) error
	GetAllPropertiesContext(ctx context.Context, unit string) (map[string]any, error)
	GetUnitPathPropertiesContext(ctx context.Context, path godbus.ObjectPath) (map[string]any, error)
	Connected() bool
	Close()
}

// Options configures a controller.
type Options struct {
	// Scope selects which manager to talk to: the system bus in the default
	// topology, the caller's own user manager under D2 (DESIGN section 5.2a).
	Scope model.SystemdScope

	// Logger receives connection-supervision events. Nil uses slog.Default.
	Logger *slog.Logger

	// OnReconnect fires after the bus connection has been re-established.
	//
	// It is a callback rather than a Controller method because the work it
	// triggers is not this package's: section 5.3 requires the consumer to
	// RESYNCHRONIZE every managed unit's properties and reconcile against the
	// database before resuming event processing, and reconciling against the
	// database is the supervisor's job. This package guarantees only that the
	// callback runs after a successful reconnect and before any event from the
	// new connection is forwarded.
	OnReconnect func()

	// dial and dialSignals are the two connection factories, overridden by
	// tests.
	dial        func(context.Context) (busConn, error)
	dialSignals func(context.Context) (signalSource, error)

	// healthInterval is how often the supervisor checks liveness. Zero uses
	// 5 s.
	healthInterval time.Duration
}

func (o Options) logger() *slog.Logger {
	if o.Logger == nil {
		return slog.Default()
	}
	return o.Logger
}

// DBusController is the primary Controller: a native D-Bus client.
//
// D-Bus wins over parsing systemctl for four reasons (section 5.3): push-based
// state, so instance state — the most-watched thing in the UI — arrives as it
// happens; typed properties in one call, so there is no output parsing to break
// across systemd versions or locales; job semantics, where a start returns a job
// path and completion is a signal rather than a timeout guess; and error
// identity. It is also pure Go.
type DBusController struct {
	scope model.SystemdScope
	log   *slog.Logger
	dial  func(context.Context) (busConn, error)

	onReconnect    func()
	healthInterval time.Duration
	dialSignals    func(context.Context) (signalSource, error)

	mu      sync.RWMutex
	conn    busConn
	signals signalSource

	hub *subHub

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

var _ Controller = (*DBusController)(nil)

// NewDBusController connects to the manager for this scope and starts the
// connection supervisor.
//
// The system-scope connection is made with NewSystemConnectionContext — the
// polkit-mediated system bus — and NOT with NewSystemdConnectionContext, which
// dials systemd's private socket and works only as root. Getting that wrong
// produces a design that appears to work in development and cannot start as the
// service identity (section 5.3).
func NewDBusController(ctx context.Context, opts Options) (*DBusController, error) {
	if opts.dial == nil {
		opts.dial = dialFor(opts.Scope)
	}
	if opts.dialSignals == nil {
		scope := opts.Scope
		opts.dialSignals = func(ctx context.Context) (signalSource, error) {
			return dialSignals(ctx, scope)
		}
	}
	if opts.healthInterval == 0 {
		opts.healthInterval = 5 * time.Second
	}

	// The connection is dialed with the CONTROLLER's lifetime context, not the
	// caller's, and that is not a detail: godbus ties a connection to the
	// context it was dialed with, so dialing with the boot sequence's own
	// context would close the bus the instant boot finished — a daemon whose
	// control plane worked for exactly as long as its startup did. The caller's
	// context still governs whether this call is made at all.
	lifetime, cancel := context.WithCancel(context.WithoutCancel(ctx))
	if err := ctx.Err(); err != nil {
		cancel()
		return nil, err
	}

	conn, err := opts.dial(lifetime)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}

	c := &DBusController{
		scope:          opts.Scope,
		log:            opts.logger(),
		dial:           opts.dial,
		onReconnect:    opts.OnReconnect,
		healthInterval: opts.healthInterval,
		dialSignals:    opts.dialSignals,
		conn:           conn,
		hub:            newSubHub(),
		ctx:            lifetime,
		cancel:         cancel,
	}
	c.armSubscription(conn)

	c.wg.Add(1)
	go c.supervise()
	return c, nil
}

func dialFor(scope model.SystemdScope) func(context.Context) (busConn, error) {
	return func(ctx context.Context) (busConn, error) {
		if scope == model.ScopeUser {
			return sddbus.NewUserConnectionContext(ctx)
		}
		return sddbus.NewSystemConnectionContext(ctx)
	}
}

// Scope reports which manager this controller addresses.
func (c *DBusController) Scope() model.SystemdScope { return c.scope }

// Close stops the supervisor, drops the connection and closes every
// subscription channel.
func (c *DBusController) Close() error {
	c.cancel()
	c.wg.Wait()

	c.mu.Lock()
	conn, signals := c.conn, c.signals
	c.conn, c.signals = nil, nil
	c.mu.Unlock()
	if signals != nil {
		_ = signals.Close()
	}
	if conn != nil {
		conn.Close()
	}
	c.hub.closeAll()
	return nil
}

func (c *DBusController) current() (busConn, error) {
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil {
		return nil, ErrDisconnected
	}
	return conn, nil
}

// supervise reconnects on drop with exponential backoff capped at 30 s
// (section 5.3). It does not itself resynchronize: it fires OnReconnect and
// lets the consumer, which is the only component that knows what the database
// expects, do the reconciling.
func (c *DBusController) supervise() {
	defer c.wg.Done()

	ticker := time.NewTicker(c.healthInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
		}

		conn, _ := c.current()
		if conn != nil && conn.Connected() {
			continue
		}
		if conn != nil {
			c.mu.Lock()
			signals := c.signals
			c.conn, c.signals = nil, nil
			c.mu.Unlock()
			if signals != nil {
				_ = signals.Close()
			}
			conn.Close()
			c.log.Warn("systemd bus connection lost", "scope", string(c.scope))
		}
		c.reconnect()
	}
}

func (c *DBusController) reconnect() {
	backoff := time.Second
	const maxBackoff = 30 * time.Second

	for attempt := 1; ; attempt++ {
		select {
		case <-c.ctx.Done():
			return
		default:
		}

		conn, err := c.dial(c.ctx)
		if err == nil {
			c.mu.Lock()
			c.conn = conn
			c.mu.Unlock()
			c.log.Info("systemd bus connection re-established",
				"scope", string(c.scope), "attempts", attempt)

			// The callback runs BEFORE the subscription is re-armed, in that
			// order deliberately: section 5.3 requires the consumer to
			// resynchronize every managed unit's properties and reconcile
			// against the database *before* resuming event processing, and
			// arming first would deliver transitions against a state the
			// consumer has not re-read yet.
			if c.onReconnect != nil {
				c.onReconnect()
			}
			c.armSubscription(conn)
			return
		}

		c.log.Warn("systemd bus reconnect failed",
			"scope", string(c.scope), "attempt", attempt, "backoff", backoff, "error", err)
		select {
		case <-c.ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

// armSubscription opens the signal connection for one method connection and
// starts forwarding unit transitions. Every reconnect re-arms it, which is what
// keeps a subscriber's channel valid across a bus restart instead of silently
// going quiet.
func (c *DBusController) armSubscription(conn busConn) {
	signals, err := c.dialSignals(c.ctx)
	if err != nil {
		c.log.Warn("systemd signal subscription unavailable; unit state will not be pushed",
			"scope", string(c.scope), "error", err)
		return
	}

	c.mu.Lock()
	c.signals = signals
	c.mu.Unlock()

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.forward(conn, signals)
	}()
}

// forward turns object paths into sub-state events.
//
// Two things happen here that go-systemd's own subscriber does not do, and both
// matter at the scale a busy system manager operates at: the unit NAME is
// decoded from the object path before any round trip, so units nobody
// subscribed to cost nothing; and a transition is published only when the
// sub-state actually CHANGED, so systemd's habit of emitting several
// PropertiesChanged per state change does not turn into several identical
// events for the supervisor to act on.
func (c *DBusController) forward(conn busConn, signals signalSource) {
	last := make(map[string]string)

	for {
		select {
		case <-c.ctx.Done():
			return
		case path, ok := <-signals.Paths():
			if !ok {
				return
			}
			unit := unitNameFromPath(path)
			if unit == "" || !c.hub.interested(unit) {
				continue
			}

			props, err := conn.GetUnitPathPropertiesContext(c.ctx, path)
			if err != nil {
				// A unit that vanished between the signal and this read is the
				// ordinary case, not an error worth a log line per event.
				continue
			}
			sub, _ := props["SubState"].(string)
			if sub == "" || last[unit] == sub {
				continue
			}
			last[unit] = sub
			c.hub.publish(SubStateEvent{Unit: unit, SubState: sub})
		}
	}
}

// job runs one blocking job verb and waits for JobRemoved.
//
// The channel is buffered: go-systemd hands the result to it from the
// connection's own dispatch goroutine, so an unbuffered channel nobody is
// reading blocks the whole connection, not just this call.
func (c *DBusController) job(
	ctx context.Context,
	unit string,
	call func(busConn, chan<- string) (int, error),
) (JobResult, error) {
	conn, err := c.current()
	if err != nil {
		return "", err
	}

	done := make(chan string, 1)
	if _, err := call(conn, done); err != nil {
		return "", translate(unit, err)
	}
	select {
	case <-ctx.Done():
		// The job stays queued; systemd will finish or fail it on its own. The
		// caller's context expiring is not a reason to leave the manager in a
		// half-issued state, and there is no cancel verb that would be correct
		// here — a StopUnit issued to undo a start is a different job.
		return "", ctx.Err()
	case res := <-done:
		return JobResult(res), nil
	}
}

// Start enqueues a start job and blocks on JobRemoved.
func (c *DBusController) Start(ctx context.Context, unit string) (JobResult, error) {
	return c.job(ctx, unit, func(b busConn, ch chan<- string) (int, error) {
		return b.StartUnitContext(ctx, unit, "replace", ch)
	})
}

// Stop enqueues a stop job and blocks on JobRemoved.
func (c *DBusController) Stop(ctx context.Context, unit string) (JobResult, error) {
	return c.job(ctx, unit, func(b busConn, ch chan<- string) (int, error) {
		return b.StopUnitContext(ctx, unit, "replace", ch)
	})
}

// Restart enqueues a restart job and blocks on JobRemoved.
func (c *DBusController) Restart(ctx context.Context, unit string) (JobResult, error) {
	return c.job(ctx, unit, func(b busConn, ch chan<- string) (int, error) {
		return b.RestartUnitContext(ctx, unit, "replace", ch)
	})
}

// StartNoWait enqueues a start job and returns its object path without waiting.
//
// Mandatory for starting llamaman-selfupdate.service: a Type=oneshot start job
// does not complete until its ExecStart exits, and that ExecStart ends by
// restarting llamaman.service — i.e. by SIGTERMing the process that would be
// waiting on the job (section 5.3).
func (c *DBusController) StartNoWait(ctx context.Context, unit string) (JobPath, error) {
	conn, err := c.current()
	if err != nil {
		return "", err
	}
	id, err := conn.StartUnitContext(ctx, unit, "replace", nil)
	if err != nil {
		return "", translate(unit, err)
	}
	return jobPath(id), nil
}

// RestartNoWait enqueues a restart job and returns its object path without
// waiting. Mandatory for POST /system/restart, whose completion requires this
// process to exit.
func (c *DBusController) RestartNoWait(ctx context.Context, unit string) (JobPath, error) {
	conn, err := c.current()
	if err != nil {
		return "", err
	}
	id, err := conn.RestartUnitContext(ctx, unit, "replace", nil)
	if err != nil {
		return "", translate(unit, err)
	}
	return jobPath(id), nil
}

// jobPath reconstructs the object path from the job id go-systemd returns. The
// path is what the caller correlates against JobRemoved in the journal; the id
// is the only half go-systemd exposes.
func jobPath(id int) JobPath {
	if id == 0 {
		return ""
	}
	return JobPath(fmt.Sprintf("/org/freedesktop/systemd1/job/%d", id))
}

// Enable links units into their [Install] target and reloads the manager.
//
// The reload is not optional: EnableUnitFiles writes symlinks the manager has
// not read yet, and the polkit rule grants reload-daemon in its own branch for
// exactly this reason (section 5.2).
func (c *DBusController) Enable(ctx context.Context, units []string) error {
	if len(units) == 0 {
		return nil
	}
	conn, err := c.current()
	if err != nil {
		return err
	}
	if _, _, err := conn.EnableUnitFilesContext(ctx, units, false, true); err != nil {
		return translate(joinUnits(units), err)
	}
	return c.Reload(ctx)
}

// Disable unlinks units from their [Install] target and reloads the manager.
func (c *DBusController) Disable(ctx context.Context, units []string) error {
	if len(units) == 0 {
		return nil
	}
	conn, err := c.current()
	if err != nil {
		return err
	}
	if _, err := conn.DisableUnitFilesContext(ctx, units, false); err != nil {
		return translate(joinUnits(units), err)
	}
	return c.Reload(ctx)
}

// Reload is `daemon-reload`.
func (c *DBusController) Reload(ctx context.Context) error {
	conn, err := c.current()
	if err != nil {
		return err
	}
	return translate("daemon-reload", conn.ReloadContext(ctx))
}

// ResetFailed clears a unit's failed state AND its start-limit counter, which is
// the half D93 cares about: systemd's start rate limit counts every start
// attempt, not only the failed ones, so the daemon clears the counter after 60 s
// of continuous readiness and the limit ends up counting consecutive starts that
// never became healthy.
func (c *DBusController) ResetFailed(ctx context.Context, unit string) error {
	conn, err := c.current()
	if err != nil {
		return err
	}
	return translate(unit, conn.ResetFailedUnitContext(ctx, unit))
}

// Props reads the unit properties in one call.
//
// GetAllProperties is used rather than the Unit-interface accessor because half
// the fields this design reads — ExecMainStatus, Result, NRestarts,
// MemoryCurrent, ExecMainExitTimestamp — live on the Service interface, and two
// round trips would let the two halves describe different moments.
func (c *DBusController) Props(ctx context.Context, unit string) (UnitProps, error) {
	conn, err := c.current()
	if err != nil {
		return UnitProps{}, err
	}
	raw, err := conn.GetAllPropertiesContext(ctx, unit)
	if err != nil {
		return UnitProps{}, translate(unit, err)
	}

	// systemd answers a properties read for a unit it has never heard of by
	// materializing the object with LoadState=not-found and NO error, exactly
	// as `systemctl show` does. Without this check a deleted instance's unit
	// would read as a healthy stopped one, and the supervisor would keep
	// starting it forever.
	if asString(raw["LoadState"]) == "not-found" {
		return UnitProps{}, fmt.Errorf("%w: %s", ErrNoSuchUnit, unit)
	}
	return propsFromMap(raw), nil
}

// SubscribeSubState delivers sub-state changes for units matching pattern until
// ctx is done. pattern is a path.Match glob, e.g. `llamaman-instance@*.service`.
func (c *DBusController) SubscribeSubState(ctx context.Context, pattern string) (<-chan SubStateEvent, error) {
	if _, err := path.Match(pattern, "probe"); err != nil {
		return nil, fmt.Errorf("systemd: bad unit pattern %q: %w", pattern, err)
	}
	return c.hub.subscribe(ctx, pattern), nil
}

// propsFromMap converts systemd's variant map into the typed snapshot.
func propsFromMap(raw map[string]any) UnitProps {
	p := UnitProps{
		ActiveState: asString(raw["ActiveState"]),
		SubState:    asString(raw["SubState"]),
		Result:      asString(raw["Result"]),
	}
	p.MainPID = uint32(asUint(raw["MainPID"]))
	p.NRestarts = uint32(asUint(raw["NRestarts"]))
	p.ExecMainStatus = int32(asInt(raw["ExecMainStatus"]))

	// systemd reports an unset MemoryCurrent as UINT64_MAX ("[not set]"), which
	// as a byte count is 16 exabytes. Reporting that verbatim would put a
	// nonsense number in the UI for every stopped instance.
	if v := asUint(raw["MemoryCurrent"]); v != math.MaxUint64 {
		p.MemoryCurrent = v
	}

	// Timestamps are microseconds since the epoch; zero means "never", which
	// must stay the zero time rather than becoming 1970.
	if us := asUint(raw["ExecMainExitTimestamp"]); us > 0 {
		p.ExecMainExitTimestamp = time.UnixMicro(int64(us)).UTC()
	}
	return p
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func asUint(v any) uint64 {
	switch n := v.(type) {
	case uint64:
		return n
	case uint32:
		return uint64(n)
	case int64:
		if n < 0 {
			return 0
		}
		return uint64(n)
	case int32:
		if n < 0 {
			return 0
		}
		return uint64(n)
	case int:
		if n < 0 {
			return 0
		}
		return uint64(n)
	}
	return 0
}

func asInt(v any) int64 {
	switch n := v.(type) {
	case int32:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case uint32:
		return int64(n)
	case uint64:
		return int64(n)
	}
	return 0
}

// joinUnits joins unit names for an error message.
func joinUnits(units []string) string {
	if len(units) == 1 {
		return units[0]
	}
	out := ""
	for i, u := range units {
		if i > 0 {
			out += ", "
		}
		out += u
	}
	return out
}
