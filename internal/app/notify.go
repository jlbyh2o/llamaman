package app

import (
	"log/slog"
	"time"
)

// Notifier is the sd_notify(3) surface the boot sequence needs (D9: "Type=notify,
// WatchdogSec=30, STATUS= carries the bound URL; sd_notify implemented in ~25
// lines over $NOTIFY_SOCKET").
//
// It is an interface here and a no-op by default because the implementation
// belongs to internal/systemd — DESIGN section 1 lists sdnotify among that
// package's contents, alongside the D-Bus controller and the journal reader,
// and D49's second invariant keeps the systemd vocabulary in one package.
// internal/app is the composition root: it decides WHEN each notification is
// sent, not HOW.
//
// The three moments the boot sequence names:
//
//   - ExtendTimeout while `PRAGMA integrity_check` or a migration is running
//     (step 4) and while the self-update gate reads a journal tail (step 11),
//     every 10 s, so a legitimately slow start extends TimeoutStartSec= instead
//     of being killed and judged (D88).
//   - Status once the port walk has landed, carrying the URL, so
//     `systemctl status llamaman` shows the truth (D9/D24, step 7).
//   - Ready at step 12, and never before step 11 — "a daemon that ever signals
//     readiness has already resolved the marker".
//   - Watchdog every WatchdogSec/2 from step 12 onwards, gated on a live
//     `SELECT 1` (section 5.4a), so a daemon wedged on its database is killed
//     and restarted instead of accepting requests it cannot serve.
type Notifier interface {
	// Ready sends READY=1.
	Ready() error
	// Status sends STATUS=<s>.
	Status(s string) error
	// ExtendTimeout sends EXTEND_TIMEOUT_USEC=<d>.
	ExtendTimeout(d time.Duration) error
	// Watchdog sends WATCHDOG=1.
	Watchdog() error
	// Stopping sends STOPPING=1.
	Stopping() error
}

// nopNotifier is the Notifier a daemon started outside systemd gets, and the
// one wired until internal/systemd lands its implementation. It logs at debug
// rather than doing nothing silently, so a developer running `llamaman serve`
// from a shell can see the readiness protocol the unit would have seen.
type nopNotifier struct{ log *slog.Logger }

func (n nopNotifier) Ready() error {
	n.log.Debug("sd_notify", "message", "READY=1")
	return nil
}

func (n nopNotifier) Status(s string) error {
	n.log.Debug("sd_notify", "message", "STATUS="+s)
	return nil
}

func (n nopNotifier) ExtendTimeout(d time.Duration) error {
	n.log.Debug("sd_notify", "message", "EXTEND_TIMEOUT_USEC", "duration", d)
	return nil
}

func (n nopNotifier) Watchdog() error {
	n.log.Debug("sd_notify", "message", "WATCHDOG=1")
	return nil
}

func (n nopNotifier) Stopping() error {
	n.log.Debug("sd_notify", "message", "STOPPING=1")
	return nil
}
