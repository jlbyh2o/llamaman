package app

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// serve is DESIGN section 11.1 step 12 and then section 9.4's shutdown.
//
// The ordering around READY=1 is the part that carries a promise. Section 11.1
// step 10 says "READY=1 is not sent here — step 11 runs first", and step 11 is
// the self-update confirmation gate, so that "a daemon that ever signals
// readiness has already resolved the marker" and the judge cannot be armed
// against a version that demonstrably booted. internal/selfupdate owns that
// gate; the call site is marked below so that landing it is a one-line change
// rather than a re-reading of this function.
func (d *daemon) serve(ctx context.Context) error {
	// --- step 11: the self-update confirmation gate (ResolveUpdateMarkers).
	// internal/selfupdate owns it. It runs HERE, before READY=1, and the D92
	// disarm at boot step 4 has already run.

	// Triage the previous boot's job rows before anything can lease a new one.
	// This is the orphan recovery section 2.3 asks for at boot: a lease whose
	// owner boot id is gone belongs to a daemon that is gone.
	triage, err := d.queue.RecoverOrphans(ctx)
	if err != nil {
		return err
	}
	if len(triage) > 0 {
		d.log.Info("triaged jobs left behind by a previous boot", "count", len(triage))
	}

	// §6.6's boot reconciliation, which is also the `llamacpp_activate`
	// finalizer: release a build lease a boot that is gone still holds, repair
	// `versions/active` and `versions/previous` FROM the rows — the row wins —
	// and close every activation job the restart left `interrupted`. It runs
	// after the triage that produced those `interrupted` rows and before the
	// runner can lease anything new.
	if d.llamacpp != nil {
		if err := d.llamacpp.Reconcile(ctx); err != nil {
			d.log.Error("could not reconcile the llama.cpp versions at boot", "error", err)
		}
	}

	// §10's boot restore, which is also the `bench_run` finalizer. It runs after
	// the triage that produced this boot's `interrupted` rows, and its first
	// step is the one predicate that must not be phrased over a state: every run
	// with `restore_done = 0 AND stopped_instances_json IS NOT NULL`, in ANY
	// state, gets the instances it stopped restarted. A benchmark that leaves
	// production instances down is the worst possible outcome, so this is
	// re-checked at every boot rather than trusted to have happened.
	if d.bench != nil {
		if err := d.bench.Reconcile(ctx); err != nil {
			d.log.Error("could not restore the instances a benchmark stopped", "error", err)
		}
	}

	errc := make(chan error, 1)
	go func() {
		err := d.server.Serve(d.listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errc <- err
	}()

	// --- step 12: READY=1, then the background workers.
	if err := d.opts.Notifier.Ready(); err != nil {
		d.log.Warn("could not signal readiness to the service manager", "error", err)
	}

	// The watchdog, gated on a live `SELECT 1` (section 5.4a). A daemon wedged
	// on its database stops pinging and is killed and restarted, instead of
	// staying `active` while accepting requests it cannot serve.
	go d.watchdog(ctx)

	// The job runner claims nothing while no worker kind is registered — its
	// own claim() returns early on an empty registry, deliberately, so that
	// "leasing with no kind filter would claim work this daemon cannot run"
	// cannot happen. Starting it now rather than when the first worker lands
	// means every subsystem that registers one gets a running queue for free.
	runnerCtx, stopRunner := context.WithCancel(ctx)
	defer stopRunner()
	go func() {
		if err := d.queue.Run(runnerCtx); err != nil && !errors.Is(err, context.Canceled) {
			d.log.Error("the job queue stopped", "error", err)
		}
	}()

	// The supervisor: boot reconciliation, then section 5.8's reconcile loop.
	// It is started after READY=1 because boot reconciliation's first step is
	// the host-boot decision that applies the D53 autostart coupling, and a
	// daemon that had not yet signaled readiness would be racing the instance
	// units systemd is starting through `llamaman-instances.target` — which
	// `After=llamaman.service` on the instance template exists to order.
	supCtx, stopSupervisor := context.WithCancel(ctx)
	defer stopSupervisor()
	supervisorDone := make(chan struct{})
	go func() {
		defer close(supervisorDone)
		if err := d.supervisor.Run(supCtx); err != nil && !errors.Is(err, context.Canceled) {
			d.log.Error("the supervisor stopped", "error", err)
		}
	}()
	d.setResync(d.supervisor.OnReconnect(supCtx))

	// The inference gateway: the per-instance public listeners of §9.1 and the
	// accounting flusher of §9.3. It is started after READY=1 for the same
	// reason the supervisor is — the instance units systemd starts through
	// `llamaman-instances.target` are ordered `After=llamaman.service` — and its
	// first reconcile is what actually binds SPEC §1's public ports.
	gatewayCtx, stopGateway := context.WithCancel(ctx)
	defer stopGateway()
	gatewayDone := make(chan struct{})
	go func() {
		defer close(gatewayDone)
		d.runGateway(gatewayCtx)
	}()

	// The nightly maintenance pass (§2.11, §11.1 step 12's background workers).
	go d.scheduleMaintenance(ctx)

	// Sixty seconds after a boot that stayed ready, clear the unit's
	// start-limit counter (D93). internal/systemd owns ResetFailed; the timer
	// is the composition root's, because it is a property of THIS boot having
	// stayed healthy rather than of the systemd vocabulary.
	go d.clearStartLimit(ctx)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	err = d.shutdown(errc)

	// The gateway's own Run loop performs the final accounting flush on the way
	// out (§9.3), so it is stopped AFTER the drain and hand-off that shutdown
	// performed and BEFORE close() shuts the database it flushes into.
	stopGateway()
	select {
	case <-gatewayDone:
	case <-time.After(gatewayStopGrace):
		d.log.Warn("the gateway did not stop within its grace period",
			"grace_sec", int(gatewayStopGrace.Seconds()))
	}

	// The supervisor holds the one write transaction that must not be cut off
	// half way: it is the only writer allowed to close an `instance_starts`
	// row. Waiting for its pass to finish before close() shuts the database is
	// what keeps a stop from leaving a row open for the next boot to synthesize
	// a closure for.
	stopSupervisor()
	select {
	case <-supervisorDone:
	case <-time.After(supervisorStopGrace):
		d.log.Warn("the supervisor did not stop within its grace period",
			"grace_sec", int(supervisorStopGrace.Seconds()))
	}
	return err
}

// supervisorStopGrace bounds the wait above. It is longer than the health
// probe's own 2 s timeout and shorter than the unit's TimeoutStopSec=45, so a
// pass that is mid-probe finishes and a pass that is wedged never holds the
// stop open long enough for systemd to SIGKILL the process.
const supervisorStopGrace = 10 * time.Second

// gatewayStopGrace bounds the wait for the gateway's Run loop to finish its
// final counter flush (§9.3). It is short because the flush is two upserts over
// a map that is already in memory; the drain that could actually take time has
// already happened, in shutdown.
const gatewayStopGrace = 10 * time.Second

// shutdown is section 9.4's ordered stop, for the steps that exist.
//
// The full sequence is: (1) commit the domain transition that prompted the
// restart, (2) flush the 202 to the caller, (3) stop accepting new connections
// while keeping the socket open, (4) drain in-flight proxied requests for
// gateway.drain_sec, (5) checkpoint the WAL and release the job leases,
// (6) hand each listener to the systemd fd store, (7) exit or wait.
//
// Steps 3, 4 and 6 run here for BOTH listener sets, and the two are not the
// same: the management listener is an ordinary http.Server that is closed, while
// each PUBLIC listener is paused, drained and then handed to systemd's
// file-descriptor store with its socket still open (D58). That difference is the
// whole of SPEC section 3.8's promise — `llama-server` is untouched by a restart
// either way, but only a preserved socket keeps a client from getting
// connection-refused on the port it was told to use.
//
// Steps 1 and 2 belong to `POST /system/restart`, which internal/api will call
// this from, and step 7's branch is section 12.1's.
func (d *daemon) shutdown(errc <-chan error) error {
	if err := d.opts.Notifier.Stopping(); err != nil {
		d.log.Debug("could not signal STOPPING=1", "error", err)
	}

	drain := d.drainWindow()
	d.log.Info("draining", "drain_sec", int(drain.Seconds()))

	// The public ports first, and with their own window: they carry generations
	// that may run for minutes, and they are the ones whose sockets survive.
	handOffCtx, cancelHandOff := context.WithTimeout(context.Background(), drain)
	d.handOffListeners(handOffCtx, drain)
	cancelHandOff()

	// Shutdown stops accepting and waits for in-flight requests. A stream that
	// outlives the window is closed by Close below, and the fact is logged
	// rather than swallowed, because section 9.4 makes "zero dropped requests"
	// a measured claim rather than a hope.
	ctx, cancel := context.WithTimeout(context.Background(), drain)
	defer cancel()

	err := d.server.Shutdown(ctx)
	if errors.Is(err, context.DeadlineExceeded) {
		d.log.Warn("the drain window expired with requests still in flight; closing them",
			"drain_sec", int(drain.Seconds()))
		err = d.server.Close()
	}

	// Serve's error arrives once the listener is closed.
	if serveErr := <-errc; serveErr != nil && err == nil {
		err = serveErr
	}
	return err
}

// drainWindow reads `gateway.drain_sec` (default 20 s), the setting section 9.4
// names. A read failure falls back to the registry default rather than failing
// the shutdown: refusing to stop because a setting could not be read would be a
// worse outcome than draining for the documented default.
func (d *daemon) drainWindow() time.Duration {
	const fallback = 20 * time.Second
	if d.settings == nil {
		return fallback
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	secs, err := d.settings.GetInt(ctx, "gateway.drain_sec")
	if err != nil {
		d.log.Warn("could not read gateway.drain_sec; draining for the default",
			"error", err, "drain_sec", int(fallback.Seconds()))
		return fallback
	}
	return time.Duration(secs) * time.Second
}

// close releases everything boot acquired, in the reverse order, and is safe to
// call on a partially constructed daemon — which is exactly what an error part
// way through boot leaves behind.
func (d *daemon) close() {
	// The public sockets, last thing before the database they account into.
	// This runs AFTER shutdown's hand-off, so on a restart systemd already holds
	// its own dup of every descriptor and closing ours releases nothing a client
	// can notice; on a full stop the store drops them anyway
	// (`FileDescriptorStorePreserve=restart`), which is exactly the wanted scope.
	if d.gateway != nil {
		if err := d.gateway.Close(); err != nil {
			d.log.Warn("closing the public listeners", "error", err)
		}
		d.gateway = nil
	}
	if d.systemd.Control != nil {
		if err := closeController(d.systemd.Control); err != nil {
			d.log.Warn("closing the systemd control channel", "error", err)
		}
		d.systemd.Control = nil
	}
	if d.hub != nil {
		d.hub.Close()
		d.hub = nil
	}
	if d.listener != nil {
		_ = d.listener.Close()
		d.listener = nil
	}
	if d.store != nil {
		if err := d.store.Close(); err != nil {
			d.log.Warn("closing the database", "error", err)
		}
		d.store = nil
	}
	if d.releaseLock != nil {
		if err := d.releaseLock(); err != nil {
			d.log.Warn("releasing the state-directory lock", "error", err)
		}
		d.releaseLock = nil
	}
}
