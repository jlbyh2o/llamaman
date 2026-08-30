package app

import (
	"context"
	"sync"
	"time"

	"github.com/jlbyh2o/llamaman/internal/store"
	"github.com/jlbyh2o/llamaman/internal/systemd"
)

// The three things step 12 starts beside the workers: the watchdog, D93's
// start-limit reset, and the reconnect hand-off between the control channel and
// the supervisor.

// WatchdogFallback is the ping period used when the unit declared
// `WatchdogSec=` but the environment did not survive to this process — it never
// should, and pinging on the design's own 10 s cadence is the safe answer if it
// does.
const WatchdogFallback = 10 * time.Second

// WatchdogProbeTimeout bounds the liveness query. It is deliberately far below
// the ping period: a `SELECT 1` that has not answered in two seconds is exactly
// the wedged database the watchdog exists to escalate, and waiting longer would
// only delay the restart.
const WatchdogProbeTimeout = 2 * time.Second

// StartLimitSettle is D93's "sixty seconds of continuous readiness" — the same
// threshold D64 uses for "this start served".
//
// The delay is what stops a binary that reaches READY=1 and then panics from
// resetting its own counter on every attempt and never letting the unit reach
// `failed` at all, which is the state `OnFailure=` needs in order to summon the
// judge.
const StartLimitSettle = 60 * time.Second

// watchdog sends WATCHDOG=1 on the unit's cadence, gated on a live `SELECT 1`
// (section 5.4a).
//
// The gate is the whole point. `WatchdogSec=30` on a daemon that pings
// unconditionally only proves the process still schedules goroutines; gating on
// a database round trip makes it prove the thing every request needs. A daemon
// wedged on its database stops pinging, systemd kills and restarts it, and the
// restart is a start that never became healthy — which is exactly what
// `StartLimitBurst=` counts.
//
// A ping is skipped rather than faked when the probe fails: skipping is what
// escalates, and one skipped ping is not yet a kill (systemd allows the full
// WatchdogSec= between pings, and this pings at half of it).
func (d *daemon) watchdog(ctx context.Context) {
	interval, ok := systemd.WatchdogInterval(d.opts.Getenv)
	if !ok {
		// No WATCHDOG_USEC, or it names another process: the unit did not ask
		// for a watchdog, or this is a hand-run daemon. Either way there is
		// nothing listening, and a ping into a closed socket is noise.
		d.log.Debug("no watchdog was requested by the service manager")
		return
	}
	// systemd's own recommendation, and what the unit's 30 s implies: ping at
	// half the declared period, so one lost ping is not a kill.
	period := interval / 2
	if period <= 0 {
		period = WatchdogFallback
	}

	ticker := time.NewTicker(period)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		if !d.databaseAlive(ctx) {
			// Deliberately no ping. The next WatchdogSec= elapses, systemd
			// restarts the unit, and the journal already carries the error.
			d.log.Error("the database did not answer a liveness query; withholding the watchdog ping")
			continue
		}
		if err := d.opts.Notifier.Watchdog(); err != nil {
			d.log.Debug("could not send the watchdog ping", "error", err)
		}
	}
}

// databaseAlive is the `SELECT 1` the ping is gated on. It uses the READ pool
// deliberately: the write pool is a single connection (section 2), and a
// liveness probe that queued behind a slow writer would report a healthy daemon
// as wedged.
func (d *daemon) databaseAlive(ctx context.Context) bool {
	if d.store == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, WatchdogProbeTimeout)
	defer cancel()
	return d.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var one int
		return tx.QueryRowContext(ctx, `SELECT 1`).Scan(&one)
	}) == nil
}

// clearStartLimit is D93: sixty seconds after a boot that stayed ready, clear
// the unit's start-limit counter.
//
// Without it that counter is a budget every ordinary restart spends — the four
// `restart_required` settings behind `POST /system/restart`, each installer
// re-run, each self-update. With it, `StartLimitBurst=5` counts only
// CONSECUTIVE STARTS THAT NEVER BECAME HEALTHY, which is what section 5.4
// always claimed the revert deadline measured.
//
// Where the call is refused — the name-scoped `manage-units` grant withheld
// (F9) — the fact is recorded rather than retried: that same grant is what
// `RestartUnit` needs, so `POST /system/restart` answers `409 systemd_denied`
// and spends nothing, `doctor` raises the warning carrying
// `sudo systemctl reset-failed llamaman.service`, and F26 states the residual.
func (d *daemon) clearStartLimit(ctx context.Context) {
	if d.systemd.Control == nil {
		return
	}
	timer := time.NewTimer(StartLimitSettle)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}

	if err := d.systemd.Control.ResetFailed(ctx, systemd.UnitDaemon); err != nil {
		d.log.Warn("could not clear this unit's start-limit counter (F26)",
			"unit", systemd.UnitDaemon, "error", err,
			"remediation", "sudo systemctl reset-failed "+systemd.UnitDaemon)
		return
	}
	d.log.Info("cleared this unit's start-limit counter after 60 s of readiness (D93)",
		"unit", systemd.UnitDaemon)
}

// resync is the control channel's reconnect callback (section 5.3): after the
// bus comes back, every managed unit's properties are resynchronized and
// reconciled against the database before event processing resumes.
//
// It is a slot rather than a direct call because the two ends are built in the
// order the boot sequence requires: the controller is constructed at step 6 and
// the supervisor after it, so the callback the controller was given has to be
// able to find a supervisor that did not exist yet. A reconnect before then is
// a no-op, which is correct — there is nothing to resynchronize against a
// database no subject set has been read from.
type resyncSlot struct {
	mu sync.Mutex
	fn func()
}

func (d *daemon) setResync(fn func()) {
	d.resync.mu.Lock()
	d.resync.fn = fn
	d.resync.mu.Unlock()
}

func (d *daemon) resynchronize() {
	d.resync.mu.Lock()
	fn := d.resync.fn
	d.resync.mu.Unlock()
	if fn != nil {
		fn()
	}
}
