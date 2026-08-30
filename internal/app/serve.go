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

// shutdown is section 9.4's ordered stop, for the steps that exist.
//
// The full sequence is: (1) commit the domain transition that prompted the
// restart, (2) flush the 202 to the caller, (3) stop accepting new connections
// while keeping the socket open, (4) drain in-flight proxied requests for
// gateway.drain_sec, (5) checkpoint the WAL and release the job leases,
// (6) hand each listener to the systemd fd store, (7) exit or wait.
//
// Steps 1, 2 and 6 belong to callers and subsystems that are not built yet —
// POST /system/restart, the gateway listeners, internal/systemd's FDSTORE=1 —
// and step 7's branch is section 12.1's. What runs here is 3, 4 and the job-lease
// half of 5, which are the parts that already have something to drain and
// something to release.
func (d *daemon) shutdown(errc <-chan error) error {
	if err := d.opts.Notifier.Stopping(); err != nil {
		d.log.Debug("could not signal STOPPING=1", "error", err)
	}

	drain := d.drainWindow()
	d.log.Info("draining", "drain_sec", int(drain.Seconds()))

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
