package app

import (
	"context"
	"fmt"
	"time"

	"github.com/jlbyh2o/llamaman/internal/gateway"
	"github.com/jlbyh2o/llamaman/internal/systemd"
	"github.com/jlbyh2o/llamaman/internal/tokens"
)

// The inference gateway (DESIGN section 9) and the token store behind it
// (section 9.3).
//
// This is the subsystem SPEC section 1's central claim rests on: Llama Man owns
// the public inference ports. Everything that follows from that sentence is
// downstream of this constructor — a client hitting a stopped instance gets a
// JSON `503 instance_not_running` instead of connection-refused, a revoked token
// stops working within one request, `instance_usage_daily` accrues for every
// proxied request in both auth modes (D56), and a self-update does not drop the
// ports (D58).
//
// The two are built together because neither is useful alone: a token store with
// no listener verifies nothing, and a listener with no verifier could only serve
// `auth_mode='none'`.

// buildGateway constructs the token service and the gateway, and adopts any
// listener systemd held across a restart.
//
// It is called AFTER the instance service, because the gateway's first reconcile
// reads the instance rows, and BEFORE the API, because
// `GET /api/v1/gateway/denials` is answered by the value built here.
func (d *daemon) buildGateway() error {
	tok, err := tokens.New(tokens.Config{
		Store:  d.store,
		Events: d.recorder,
		Now:    d.opts.Now,
	})
	if err != nil {
		return fmt.Errorf("build the token store: %w", err)
	}
	d.tokens = tok

	// D58's fd store. The notifier is the ONE object allowed to speak sd_notify
	// (D49 invariant 2), and it satisfies gateway.FDStore structurally — so the
	// gateway keeps no dependency on internal/systemd and the composition root
	// does the two-field conversion, exactly as gateway.InheritedFD's own
	// comment asks.
	//
	// A daemon started outside systemd gets the no-op notifier, which does NOT
	// satisfy the interface: the assertion fails, FDStore stays nil,
	// `listener_continuity` records `none`, and `GET /system/capabilities` says
	// clients will see about two seconds of connection refused across a restart.
	// Nothing silently degrades (section 9.4).
	fdstore, _ := d.opts.Notifier.(gateway.FDStore)

	gw, err := gateway.New(gateway.Config{
		Store:    d.store,
		Settings: d.settings,
		Tokens:   tok,
		Events:   d.recorder,
		FDStore:  fdstore,
		Logger:   d.log,
		Now:      d.opts.Now,
	})
	if err != nil {
		return fmt.Errorf("build the inference gateway: %w", err)
	}
	d.gateway = gw

	// Section 11.1 step 10 / section 9.4 startup: adopt the descriptors systemd
	// held in its store. Matching them against the instance set is the
	// gateway's first Reconcile, deliberately — a name with no surviving
	// instance is closed, an instance with no stored fd is bound fresh, and a
	// stored fd whose `public_port` changed while the daemon was down is closed
	// and rebound.
	//
	// The management listener is NOT part of this: it is bound by step 7's port
	// walk, and nothing ever hands it to the store, so no `ui` descriptor can
	// come back. Every unclaimed name is therefore a name from some other
	// release, and it is left alone rather than closed.
	fds := systemd.ListenFDs(d.opts.Getenv)
	if len(fds) > 0 {
		inherited := make([]gateway.InheritedFD, 0, len(fds))
		for _, fd := range fds {
			inherited = append(inherited, gateway.InheritedFD{FD: fd.FD, Name: fd.Name})
		}
		if unclaimed := gw.Adopt(inherited); len(unclaimed) > 0 {
			d.log.Info("the service manager passed descriptors this release does not name",
				"count", len(unclaimed))
		}
	}
	return nil
}

// runGateway is the listener reconciler and the accounting flusher (section
// 9.1, section 9.3), started with the other background workers at step 12.
//
// A gateway that could not reconcile is NOT a reason to refuse to serve: F6
// makes a bind failure a per-instance banner and a notification, never a daemon
// start failure, and Run's own first pass already logs and continues.
func (d *daemon) runGateway(ctx context.Context) {
	if d.gateway == nil {
		return
	}
	if err := d.gateway.Run(ctx); err != nil && ctx.Err() == nil {
		d.log.Error("the inference gateway stopped", "error", err)
	}
}

// handOffListeners is section 9.4 steps 3, 4 and 6 for the public ports: pause
// accepting, drain in flight, then hand every socket to systemd's
// file-descriptor store so the next start re-adopts it.
//
// The socket stays OPEN through all three. That is the whole mechanism: the
// kernel accept queue holds every connection that arrives during the gap, so
// from a client's perspective a self-update is a pause of a second or two rather
// than a refusal — which is the only way SPEC section 1's port ownership and
// SPEC section 3.8's "running instances unaffected" can both be true.
func (d *daemon) handOffListeners(ctx context.Context, drain time.Duration) {
	if d.gateway == nil {
		return
	}

	res := d.gateway.Drain(ctx, drain)
	d.log.Info("drained the public listeners",
		"listeners", res.Listeners, "drain_sec", int(drain.Seconds()),
		// Section 9.4 makes "zero dropped requests" a MEASURED claim rather than
		// a hope, so the number is logged even when it is zero.
		"closed_after_the_window", res.Dropped)

	stored, err := d.gateway.HandOff()
	switch {
	case err != nil:
		// Not fatal, and not silent either: the restart still happens, it simply
		// has a short connection-refused window. The gateway has already
		// recorded `listener_continuity='none'` for it.
		d.log.Warn("the service manager would not hold the public listeners; "+
			"clients will see a brief connection refused on every instance port",
			"stored", stored, "error", err)
	case stored > 0:
		d.log.Info("handed the public listeners to the service manager", "count", stored)
	}
}
