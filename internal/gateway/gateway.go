package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
	"github.com/jlbyh2o/llamaman/internal/tokens"
)

// Store is the persistence this package needs. *store.Store satisfies it
// structurally — DESIGN section 1, invariant 1: only internal/store contains
// SQL, so every other package declares the repository interface it needs.
type Store interface {
	Read(ctx context.Context, fn func(context.Context, store.Tx) error) error
	Write(ctx context.Context, fn func(context.Context, store.Tx) error) error

	InstanceViews(ctx context.Context, tx store.Tx, f store.InstanceFilter) ([]model.InstanceView, error)
	InstanceStarts(ctx context.Context, tx store.Tx, instanceID string, limit int) ([]model.InstanceStart, error)

	AddInstanceUsage(ctx context.Context, tx store.Tx, d store.InstanceUsageDelta) error
	AddTokenUsage(ctx context.Context, tx store.Tx, d store.TokenUsageDelta) error
	AddGatewayDenial(ctx context.Context, tx store.Tx, d store.DenialDelta) error
	GatewayDenials(ctx context.Context, tx store.Tx, instanceID string, rng store.UsageRange) ([]store.DenialRow, error)
}

// Settings is the typed settings this package reads: the `gateway.*` keys of
// section 2.1.
type Settings interface {
	GetInt(ctx context.Context, key string) (int64, error)
	GetString(ctx context.Context, key string) (string, error)
}

// Verifier is the token half of the request path. *tokens.Service satisfies it.
type Verifier interface {
	Verify(ctx context.Context, secret, instanceID string) (tokens.Verified, error)
	RecordUse(id string, at time.Time, ip string)
	Flush(ctx context.Context, force bool) error
}

// Events is the events/SSE seam. Append belongs inside a write transaction;
// Publish runs only after it commits.
type Events interface {
	Append(ctx context.Context, tx store.Tx, ev model.Event) error
	Publish(ev model.Event)
}

// Config wires a Gateway.
type Config struct {
	Store    Store
	Settings Settings
	Tokens   Verifier
	// Events is optional. Without it the gateway still serves and still counts;
	// it simply cannot raise the F6 bind-failure banner or section 9.3's
	// "unauthorized attempts on port 8081" warning.
	Events Events
	// FDStore is systemd's file-descriptor store (D58). Nil is the documented
	// "no NOTIFY_SOCKET" case: the restart still happens, it simply has a short
	// connection-refused window, and Continuity reports `none` so the UI can say
	// so rather than silently degrading.
	FDStore FDStore

	Logger *slog.Logger
	// Now supplies every instant this package stamps. Nil uses time.Now.
	Now func() time.Time
	// NewID mints event ids. Nil uses store.NewID.
	NewID func(time.Time) string
	// RefreshInterval is how often Run re-reads the instance set even with no
	// explicit Trigger. Nil-valued (zero) uses DefaultRefreshInterval.
	RefreshInterval time.Duration
}

// DefaultRefreshInterval is the safety-net reconcile: the instance service calls
// Trigger on every create, update and delete, and this catches anything that
// changed the rows without saying so — an `instance_status` transition written
// by the supervisor, most of all, which is what `/health` answers from.
const DefaultRefreshInterval = 2 * time.Second

// Gateway runs the per-instance public listeners of DESIGN section 9.
type Gateway struct {
	store    Store
	settings Settings
	tokens   Verifier
	events   Events
	fdstore  FDStore

	log   *slog.Logger
	now   func() time.Time
	newID func(time.Time) string

	refresh time.Duration

	acct    *accountant
	watch   *denialWatch
	taps    *tapPool
	tune    atomic.Pointer[tuning]
	proxyTr *http.Transport

	// listenFn binds one public port. It defaults to the package's listen
	// function; tests in this package may replace it with one that hands over
	// an already-bound socket instead of calling net.Listen fresh, which is
	// what makes a chosen port immune to another process grabbing it in the
	// window between picking the number and this package's own bind.
	listenFn func(bind string, port int) (net.Listener, error)

	// continuity is D58's honest answer, and it can only ever move from
	// `fdstore` to `none`: a store that refused once will refuse again, and a
	// UI that said "no interruption" and then dropped connections would be worse
	// than one that never promised.
	continuity atomic.Pointer[model.ListenerContinuity]

	mu        sync.Mutex
	listeners map[string]*publicListener
	// adopted holds sockets recovered from LISTEN_FDS that no reconcile has
	// claimed yet.
	adopted map[string]*pausable
	// bindErrs is F6: a bind failure is a per-instance banner and a
	// notification, never a daemon start failure.
	bindErrs map[string]string
	closed   bool

	trigger chan struct{}
}

// tuning is the `gateway.*` settings as one immutable snapshot, swapped on every
// reconcile. Reading five settings per request would put the settings cache on
// the hot path for no benefit; reading them per reconcile means an edit takes
// effect within one refresh interval, which is what a setting that is not
// `restart_required` promises.
type tuning struct {
	bind           string
	idleTimeout    time.Duration
	maxBodyBytes   int64
	usageTapBytes  int
	requestTimeout time.Duration
}

// The per-listener http.Server constants of section 9.1. WriteTimeout is
// deliberately absent — it is 0, because a token stream can run for many
// minutes and a write deadline would cut a generation off mid-sentence.
const (
	readHeaderTimeout = 15 * time.Second
	maxHeaderBytes    = 1 << 20
)

// New builds a Gateway. It binds nothing: Adopt and Reconcile do that, in that
// order, so a restart re-adopts before it rebinds.
func New(cfg Config) (*Gateway, error) {
	if cfg.Store == nil {
		return nil, errors.New("gateway: a store is required")
	}
	if cfg.Settings == nil {
		return nil, errors.New("gateway: a settings source is required")
	}
	if cfg.Tokens == nil {
		return nil, errors.New("gateway: a token verifier is required")
	}

	g := &Gateway{
		store:     cfg.Store,
		settings:  cfg.Settings,
		tokens:    cfg.Tokens,
		events:    cfg.Events,
		fdstore:   cfg.FDStore,
		log:       cfg.Logger,
		now:       cfg.Now,
		newID:     cfg.NewID,
		refresh:   cfg.RefreshInterval,
		acct:      newAccountant(),
		watch:     newDenialWatch(),
		taps:      newTapPool(),
		listeners: map[string]*publicListener{},
		adopted:   map[string]*pausable{},
		bindErrs:  map[string]string{},
		trigger:   make(chan struct{}, 1),
		listenFn:  listen,
	}
	if g.log == nil {
		g.log = slog.Default()
	}
	if g.now == nil {
		g.now = time.Now
	}
	if g.newID == nil {
		g.newID = store.NewID
	}
	if g.refresh <= 0 {
		g.refresh = DefaultRefreshInterval
	}

	// D36, and the reason this transport is constructed here rather than left as
	// http.DefaultTransport: without DisableCompression, Go's Transport adds
	// `Accept-Encoding: gzip` when the client sent none and then transparently
	// decompresses — so the bytes the client receives are NOT the bytes
	// llama-server sent, breaking SPEC §3.4's byte-for-byte pass-through in a
	// way no test would notice. With it, the client's own negotiation passes
	// through untouched.
	g.proxyTr = &http.Transport{
		DisableCompression: true,
		// No overall response timeout (§9.2). ResponseHeaderTimeout covers
		// prompt processing on a cold cache; after the first byte a generation
		// may run for as long as it runs.
		ResponseHeaderTimeout: 10 * time.Minute,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   32,
		ForceAttemptHTTP2:     false,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}

	continuity := model.ContinuityNone
	if cfg.FDStore != nil {
		continuity = model.ContinuityFDStore
	}
	g.continuity.Store(&continuity)
	return g, nil
}

// Continuity is what §11.1 step 10 records in
// `runtime_info.listener_continuity` and what `GET /system/capabilities`
// reports: `fdstore` when the sockets survive a restart, `none` when they do not
// and the restart therefore has a short connection-refused window.
func (g *Gateway) Continuity() model.ListenerContinuity { return *g.continuity.Load() }

// degrade records that the fd store is unavailable after all. Nothing silently
// degrades (§9.4): the flag is what makes both the self-update dialog and the
// restart confirmation say "clients will see ~2 s of connection refused" instead
// of "no interruption".
func (g *Gateway) degrade(reason error) {
	if g.Continuity() == model.ContinuityNone {
		return
	}
	none := model.ContinuityNone
	g.continuity.Store(&none)
	g.log.Warn("the systemd file-descriptor store is unavailable; a restart will "+
		"briefly refuse connections on every public port", "error", reason)
}

// Adopt takes the descriptors systemd handed this process through
// LISTEN_FDS/LISTEN_FDNAMES and turns the ones this package named into
// listeners (§9.4 startup, §11.1 step 10).
//
// Matching against the instance set is Reconcile's job, deliberately: a name
// with no surviving instance must be closed, an instance with no stored fd must
// be bound fresh, and a stored fd whose `public_port` changed while the daemon
// was down must be closed and rebound — three decisions that all need the rows,
// which this method does not read.
//
// It returns the descriptors it did NOT claim, so the composition root can adopt
// `ui` from the same list without this package knowing anything about the
// management listener.
func (g *Gateway) Adopt(fds []InheritedFD) (unclaimed []InheritedFD) {
	g.mu.Lock()
	defer g.mu.Unlock()

	for _, fd := range fds {
		id, ok := InstanceIDFromFDName(fd.Name)
		if !ok {
			unclaimed = append(unclaimed, fd)
			continue
		}
		ln, err := adopt(fd)
		if err != nil {
			g.log.Warn("could not adopt a stored gateway listener; it will be rebound",
				"instance_id", id, "error", err)
			continue
		}
		g.adopted[id] = &pausable{inner: ln}
		g.log.Info("adopted a gateway listener from the service manager",
			"instance_id", id, "port", portOf(ln))
	}

	if len(g.adopted) > 0 {
		// Adoption is proof the store works, whatever the notifier says.
		fdstore := model.ContinuityFDStore
		g.continuity.Store(&fdstore)
	}
	return unclaimed
}

// HandOff is §9.4 step 6: hand every listening socket to systemd's
// file-descriptor store under `FDNAME=gw-<instance_id>`, so the next start
// re-adopts them and no client ever sees connection-refused.
//
// It reports how many it stored. A store that refuses is not an error the caller
// must handle — the restart still happens — but it IS recorded, so the UI stops
// promising an uninterrupted restart.
func (g *Gateway) HandOff() (int, error) {
	if g.fdstore == nil {
		return 0, nil
	}

	g.mu.Lock()
	pending := make([]*publicListener, 0, len(g.listeners))
	for _, l := range g.listeners {
		pending = append(pending, l)
	}
	g.mu.Unlock()

	stored := 0
	var first error
	for _, l := range pending {
		var storeErr error
		err := l.ln.controlFD(func(fd int) {
			storeErr = g.fdstore.StoreFD(FDName(l.instanceID), fd)
		})
		if err == nil {
			err = storeErr
		}
		if err != nil {
			if first == nil {
				first = err
			}
			g.degrade(err)
			continue
		}
		stored++
	}
	return stored, first
}

// Trigger asks for a reconcile now. It never blocks: the channel holds one
// pending request, which is all "something changed" ever needs to mean.
func (g *Gateway) Trigger() {
	select {
	case g.trigger <- struct{}{}:
	default:
	}
}

// Run reconciles on every Trigger and on a ticker, and flushes the accounting
// counters every FlushInterval (§9.3). It returns when ctx is done, after one
// final flush.
func (g *Gateway) Run(ctx context.Context) error {
	if err := g.Reconcile(ctx); err != nil {
		// A reconcile that fails at boot is not a reason to refuse to run: the
		// next tick tries again, and the listeners that DID open are already
		// serving.
		g.log.Error("the first gateway reconcile failed", "error", err)
	}

	refresh := time.NewTicker(g.refresh)
	defer refresh.Stop()
	flush := time.NewTicker(FlushInterval)
	defer flush.Stop()

	for {
		select {
		case <-ctx.Done():
			// The counters are in memory and this process may be about to end,
			// so the final flush is forced and gets a context of its own — the
			// one that just expired cannot carry a write.
			done, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			if err := g.Flush(done); err != nil {
				g.log.Error("could not flush the gateway counters", "error", err)
			}
			if err := g.tokens.Flush(done, true); err != nil {
				g.log.Error("could not flush the token usage stamps", "error", err)
			}
			cancel()
			return ctx.Err()

		case <-g.trigger:
			if err := g.Reconcile(ctx); err != nil && !errors.Is(err, context.Canceled) {
				g.log.Error("a gateway reconcile failed", "error", err)
			}

		case <-refresh.C:
			if err := g.Reconcile(ctx); err != nil && !errors.Is(err, context.Canceled) {
				g.log.Error("a gateway reconcile failed", "error", err)
			}

		case <-flush.C:
			if err := g.Flush(ctx); err != nil && !errors.Is(err, context.Canceled) {
				g.log.Error("could not flush the gateway counters", "error", err)
			}
		}
	}
}

// Reconcile brings the listener set in line with the database (§9.1).
//
// A listener is OPEN whenever the instance row exists and is not deleted — not
// only while the model is loaded. A client hitting a stopped instance gets a
// JSON `503 instance_not_running` instead of connection-refused, which is far
// easier to debug, and the port cannot be stolen by another process while the
// instance is stopped.
func (g *Gateway) Reconcile(ctx context.Context) error {
	tune, err := g.readTuning(ctx)
	if err != nil {
		return err
	}
	g.tune.Store(tune)

	var rows []model.InstanceView
	if err := g.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		rows, err = g.store.InstanceViews(ctx, tx, store.InstanceFilter{})
		return err
	}); err != nil {
		return err
	}

	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return nil
	}

	wanted := make(map[string]struct{}, len(rows))
	var opened []*publicListener
	for _, row := range rows {
		wanted[row.ID] = struct{}{}
		snap := snapshotOf(row)

		if l, ok := g.listeners[row.ID]; ok {
			l.h.snap.Store(&snap)
			if l.port == row.PublicPort {
				continue
			}
			// The port changed: close the old socket and open the new one
			// (§9.1). This is a real close, not a pause — nothing is going to
			// re-adopt a socket on a port no instance claims.
			g.closeListenerLocked(l, true)
		}

		l, err := g.newListenerLocked(tune, snap)
		if err != nil {
			g.noteBindFailureLocked(ctx, snap, err)
			continue
		}
		delete(g.bindErrs, row.ID)
		g.listeners[row.ID] = l
		opened = append(opened, l)
	}

	for id, l := range g.listeners {
		if _, ok := wanted[id]; ok {
			continue
		}
		g.closeListenerLocked(l, true)
	}
	for id, ln := range g.adopted {
		if _, ok := wanted[id]; ok && g.listeners[id] != nil {
			continue
		}
		// A name with no surviving instance, or one whose fresh bind replaced
		// it: close it (§9.4 startup).
		_ = ln.closeSocket()
		delete(g.adopted, id)
	}
	g.mu.Unlock()

	for _, l := range opened {
		l.serve()
	}
	return nil
}

// Ports reports the port each instance's listener is bound to. It is what a
// test and `llamaman status` read; nothing in the request path calls it.
func (g *Gateway) Ports() map[string]int {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make(map[string]int, len(g.listeners))
	for id, l := range g.listeners {
		out[id] = l.port
	}
	return out
}

// Denials is `GET /api/v1/gateway/denials` (§3.12): the refusal counters per
// instance and reason.
//
// It flushes first, so a denial that just happened is in the answer. A screen
// that showed a five-second-old zero while an attack was in progress would be
// worse than one that took a millisecond longer.
func (g *Gateway) Denials(ctx context.Context, instanceID string, rng store.UsageRange) ([]store.DenialRow, error) {
	if err := g.Flush(ctx); err != nil {
		g.log.Warn("could not flush the gateway counters before reading denials", "error", err)
	}
	var out []store.DenialRow
	err := g.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		out, err = g.store.GatewayDenials(ctx, tx, instanceID, rng)
		return err
	})
	return out, err
}

// BindErrors reports the instances whose public port could not be bound. F6: a
// bind failure is a per-instance banner and a notification, never a daemon start
// failure — the instance keeps serving on its internal port and the UI offers a
// port picker.
func (g *Gateway) BindErrors() map[string]string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make(map[string]string, len(g.bindErrs))
	for id, msg := range g.bindErrs {
		out[id] = msg
	}
	return out
}

// DrainResult is what a drain measured, so that "zero dropped requests" is a
// measured claim rather than a hope (§9.4 step 4).
type DrainResult struct {
	// Listeners is how many sockets were paused.
	Listeners int
	// Dropped is how many requests were still in flight when the window
	// expired and were therefore closed. A generation longer than the drain
	// window IS interrupted, and the UI's restart confirmation says so.
	Dropped int
}

// Drain is §9.4 steps 3 and 4: stop accepting new connections on each listener
// but KEEP THE SOCKET OPEN, then give in-flight proxied requests up to window to
// finish.
//
// Keeping the socket open is the whole mechanism. The kernel accept queue holds
// every connection that arrives during the gap, so from a client's perspective a
// self-update is a pause of a second or two rather than a refusal.
func (g *Gateway) Drain(ctx context.Context, window time.Duration) DrainResult {
	g.mu.Lock()
	pending := make([]*publicListener, 0, len(g.listeners))
	for _, l := range g.listeners {
		pending = append(pending, l)
	}
	g.mu.Unlock()

	result := DrainResult{Listeners: len(pending)}
	if len(pending) == 0 {
		return result
	}

	deadline, cancel := context.WithTimeout(ctx, window)
	defer cancel()

	var (
		wg      sync.WaitGroup
		dropped atomic.Int64
	)
	for _, l := range pending {
		wg.Add(1)
		go func(l *publicListener) {
			defer wg.Done()
			// Shutdown closes the server's listener — which for a pausable is a
			// pause, not a close — and then waits for in-flight requests.
			if err := l.srv.Shutdown(deadline); err != nil {
				dropped.Add(1)
				// The window expired with requests still running. Close them,
				// and say so: this is the one case §9.4's table calls an
				// interruption.
				_ = l.srv.Close()
			}
			<-l.done
		}(l)
	}
	wg.Wait()

	result.Dropped = int(dropped.Load())
	if result.Dropped > 0 {
		g.log.Warn("the drain window expired with requests still in flight",
			"drain_sec", int(window.Seconds()), "listeners_closed_early", result.Dropped)
	}
	return result
}

// Close releases every socket for good. It is what a daemon that is STOPPING
// calls — the fd store's default `FileDescriptorStorePreserve=restart` drops the
// stored descriptors on a full stop anyway, so there is nothing to preserve.
func (g *Gateway) Close() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closed = true

	var first error
	for _, l := range g.listeners {
		if err := g.closeListenerLocked(l, true); err != nil && first == nil {
			first = err
		}
	}
	for id, ln := range g.adopted {
		if err := ln.closeSocket(); err != nil && first == nil {
			first = err
		}
		delete(g.adopted, id)
	}
	g.proxyTr.CloseIdleConnections()
	return first
}

// newListenerLocked binds (or claims an adopted) socket for one instance and
// builds its http.Server. The caller holds g.mu.
func (g *Gateway) newListenerLocked(tune *tuning, snap instanceSnapshot) (*publicListener, error) {
	var ln *pausable
	if adopted, ok := g.adopted[snap.ID]; ok {
		delete(g.adopted, snap.ID)
		if portOf(adopted.inner) == snap.PublicPort || portOf(adopted.inner) == 0 {
			ln = adopted
		} else {
			// The port changed while the daemon was down (§9.4 startup).
			_ = adopted.closeSocket()
		}
	}
	if ln == nil {
		raw, err := g.listenFn(tune.bind, snap.PublicPort)
		if err != nil {
			return nil, err
		}
		ln = &pausable{inner: raw}
	}

	h := &instanceHandler{g: g}
	h.snap.Store(&snap)
	h.buildProxy()

	l := &publicListener{
		instanceID: snap.ID,
		port:       snap.PublicPort,
		ln:         ln,
		h:          h,
		done:       make(chan struct{}),
	}
	l.srv = &http.Server{
		Handler:           h,
		ReadHeaderTimeout: readHeaderTimeout,
		WriteTimeout:      0,
		IdleTimeout:       tune.idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          nil,
	}
	return l, nil
}

// closeListenerLocked removes a listener from the set. hard closes the socket
// itself; a soft close only pauses, which is what a drain wants.
func (g *Gateway) closeListenerLocked(l *publicListener, hard bool) error {
	delete(g.listeners, l.instanceID)
	g.watch.forget(l.instanceID)
	_ = l.srv.Close()
	if !hard {
		return nil
	}
	return l.ln.closeSocket()
}

// noteBindFailureLocked records F6. It is a per-instance banner and one warn
// event, never a daemon start failure — and the event is raised only on the
// TRANSITION into failure, so a port an unrelated process is holding does not
// write one event per refresh interval forever.
func (g *Gateway) noteBindFailureLocked(ctx context.Context, snap instanceSnapshot, err error) {
	msg := err.Error()
	if g.bindErrs[snap.ID] == msg {
		return
	}
	g.bindErrs[snap.ID] = msg
	g.log.Error("could not bind an instance's public port; it keeps serving on its internal port",
		"instance", snap.Name, "instance_id", snap.ID, "port", snap.PublicPort, "error", err)

	go g.recordEvent(context.WithoutCancel(ctx), snap.ID, "gateway_bind_failed", model.LevelError,
		fmt.Sprintf("could not bind port %d for instance %s; it is reachable on its internal "+
			"port only until the port is changed or freed", snap.PublicPort, snap.Name))
}

// readTuning reads the `gateway.*` settings this package acts on.
func (g *Gateway) readTuning(ctx context.Context) (*tuning, error) {
	bind, err := g.settings.GetString(ctx, "gateway.bind")
	if err != nil {
		return nil, err
	}
	idle, err := g.settings.GetInt(ctx, "gateway.idle_timeout_sec")
	if err != nil {
		return nil, err
	}
	maxBody, err := g.settings.GetInt(ctx, "gateway.max_body_mb")
	if err != nil {
		return nil, err
	}
	tapKB, err := g.settings.GetInt(ctx, "gateway.usage_parse_kb")
	if err != nil {
		return nil, err
	}
	reqTimeout, err := g.settings.GetInt(ctx, "gateway.request_timeout_sec")
	if err != nil {
		return nil, err
	}
	return &tuning{
		bind:          bind,
		idleTimeout:   time.Duration(idle) * time.Second,
		maxBodyBytes:  maxBody << 20,
		usageTapBytes: int(tapKB) << 10,
		// 0 = never cap a generation (§2.1, §9.2). A completion has no size or
		// duration known in advance, and a default timeout would truncate the
		// long ones — which are exactly the ones worth waiting for.
		requestTimeout: time.Duration(reqTimeout) * time.Second,
	}, nil
}

// recordEvent appends one `events` row and publishes it. Failure is logged and
// swallowed: the gateway must not stop serving because the event log did.
func (g *Gateway) recordEvent(ctx context.Context, instanceID, action string,
	level model.EventLevel, message string) {
	if g.events == nil {
		return
	}
	now := g.now()
	subjectType, subjectID := "instance", instanceID
	ev := model.Event{
		ID:          g.newID(now),
		At:          now.UnixMilli(),
		Level:       level,
		Category:    model.CategoryGateway,
		SubjectType: &subjectType,
		SubjectID:   &subjectID,
		Action:      action,
		Actor:       model.ActorSystem,
		Message:     message,
	}
	if err := g.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		return g.events.Append(ctx, tx, ev)
	}); err != nil {
		g.log.Warn("could not record a gateway event", "action", action, "error", err)
		return
	}
	g.events.Publish(ev)
}

// publicListener is one instance's front door.
type publicListener struct {
	instanceID string
	port       int
	ln         *pausable
	srv        *http.Server
	h          *instanceHandler
	done       chan struct{}
}

// serve starts the accept loop. It is called outside g.mu so that a Serve that
// returns instantly cannot deadlock against the reconcile that started it.
func (l *publicListener) serve() {
	go func() {
		defer close(l.done)
		err := l.srv.Serve(l.ln)
		if err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, errPaused) {
			l.h.g.log.Error("a gateway listener stopped",
				"instance_id", l.instanceID, "port", l.port, "error", err)
		}
	}()
}

// instanceSnapshot is one instance as the request path needs it: small, copied
// on every reconcile, and read through an atomic pointer so a handler never
// takes a lock to learn where to proxy.
type instanceSnapshot struct {
	ID           string
	Name         string
	ModelID      *string
	PublicPort   int
	InternalPort int
	AuthMode     model.AuthMode
	State        model.InstanceState
}

// serving reports whether the upstream is expected to answer. `degraded` counts:
// it means the model is loaded and something else is wrong, and refusing to
// proxy would turn a partial outage into a total one.
func (s instanceSnapshot) serving() bool {
	return s.State == model.InstanceReady || s.State == model.InstanceDegraded
}

// loading reports whether the upstream is on its way up, which is the case that
// earns a `Retry-After`.
func (s instanceSnapshot) loading() bool {
	return s.State == model.InstanceStarting || s.State == model.InstanceLoading
}

func snapshotOf(row model.InstanceView) instanceSnapshot {
	return instanceSnapshot{
		ID:           row.ID,
		Name:         row.Name,
		ModelID:      row.ModelID,
		PublicPort:   row.PublicPort,
		InternalPort: row.InternalPort,
		AuthMode:     row.AuthMode,
		State:        row.Status.State,
	}
}
