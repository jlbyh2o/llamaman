package systemd

import (
	"context"
	"errors"
	"os"

	sddbus "github.com/coreos/go-systemd/v22/dbus"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// Connect chooses the control channel at boot: D-Bus first, systemctl second,
// nothing third (DESIGN section 5.3 and section 11.1 step 6/6a).
//
// The third answer is not an error. `systemd_control='unavailable'` is a
// degraded mode the daemon starts into, not a refusal to start, and it never
// means the daemon spawns llama-server itself (section 11.1a). The caller
// records the returned SystemdControl in runtime_info and reports it through
// GET /system/info so the UI can say whether status updates are pushed or
// polled.
func Connect(ctx context.Context, opts Options) (Controller, model.SystemdControl, error) {
	log := opts.logger()

	c, err := NewDBusController(ctx, opts)
	if err == nil {
		return c, model.ControlDBus, nil
	}
	log.Warn("systemd D-Bus unavailable; falling back to systemctl",
		"scope", string(opts.Scope), "error", err)

	e, execErr := NewExecController(ExecOptions{Scope: opts.Scope, Logger: log})
	if execErr == nil {
		return e, model.ControlExec, nil
	}
	log.Warn("systemctl unavailable; the control plane is degraded",
		"scope", string(opts.Scope), "error", execErr)

	return nil, model.ControlUnavailable, errors.Join(err, execErr)
}

// Available reports whether this host runs systemd at all, by the same test
// install.sh uses: /run/systemd/system exists only under a systemd PID 1.
//
// It is a cheap pre-check, not the answer — a host with systemd whose bus the
// service identity cannot reach still degrades through Connect.
func Available() bool {
	fi, err := os.Stat("/run/systemd/system")
	return err == nil && fi.IsDir()
}

// ScopeProbe answers DESIGN section 11.1 step 1's fallback question: with no
// `serve --scope` flag from the unit, is this daemon running under a user
// manager?
//
// The rule is deliberately narrow, and both halves matter: the scope is `user`
// when a connection to the USER bus succeeds **and** that manager reports
// llamaman.service as a known unit. A bare connection is not enough — a
// developer running `llamaman serve` from a desktop session has a perfectly
// good user bus and is not in the D2 topology at all — and the obvious
// alternatives ($XDG_RUNTIME_DIR, $INVOCATION_ID, the euid) disagree with one
// another in exactly the cases that matter.
//
// It is wired into internal/app as Options.ScopeProbe: D49's second invariant
// puts every D-Bus question in this package, and app takes the answer as a
// callback.
func ScopeProbe() (model.SystemdScope, bool) {
	return scopeProbe(context.Background(), func(ctx context.Context) (unitLister, error) {
		return sddbus.NewUserConnectionContext(ctx)
	})
}

// unitLister is the one method the scope probe needs, extracted so a test can
// answer it without a bus.
type unitLister interface {
	ListUnitsByNamesContext(ctx context.Context, units []string) ([]sddbus.UnitStatus, error)
	Close()
}

func scopeProbe(ctx context.Context, dial func(context.Context) (unitLister, error)) (model.SystemdScope, bool) {
	conn, err := dial(ctx)
	if err != nil {
		return "", false
	}
	defer conn.Close()

	units, err := conn.ListUnitsByNamesContext(ctx, []string{UnitDaemon})
	if err != nil {
		return "", false
	}
	for _, u := range units {
		if u.Name != UnitDaemon {
			continue
		}
		// A manager that has never heard of the unit answers with
		// LoadState=not-found rather than omitting the row, so the row's
		// presence alone proves nothing.
		if u.LoadState == "not-found" {
			return "", false
		}
		return model.ScopeUser, true
	}
	return "", false
}
