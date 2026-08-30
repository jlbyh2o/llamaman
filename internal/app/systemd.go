package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/systemd"
)

// The systemd half of DESIGN section 11.1 step 6, and step 6a's degraded exit.
//
// Everything the daemon learns about its service manager is learned HERE, once,
// and persisted into `runtime_info`: the control channel (section 5.3), the two
// boot `CheckAuthorization` calls (section 5.2) and the journal-readability
// probe (D77). Nothing re-derives any of it later — sections 3.3, 5.8 and 11.1a
// all read the columns this probe fills.
//
// It is an injected function rather than a direct call for the same reason
// Options.ScopeProbe is: D49's second invariant keeps the systemd vocabulary in
// internal/systemd, and a unit test of the boot sequence must not dial the
// host's bus, run `journalctl`, or depend on whether the machine it is compiled
// on has a service manager at all. internal/cli wires ProbeSystemd; a nil probe
// records NULL in every column, which is F14's "a fact nobody learned" rather
// than a denial.

// ProbeTimeout bounds the whole step-6 probe. Each half of it talks to
// something that can be slow or absent — a bus, a subprocess — and a boot
// sequence that hung here would burn `TimeoutStartSec=` on a fact the daemon is
// designed to live without (section 11.1a).
const ProbeTimeout = 10 * time.Second

// SystemdOptions is what the probe needs from the boot sequence.
type SystemdOptions struct {
	// Scope was settled in step 1 and is never re-derived (section 11.1 step 6:
	// "the scope was settled in step 1 and is not re-derived here").
	Scope model.SystemdScope
	// Logger receives the two degraded-mode warnings.
	Logger *slog.Logger
	// OnReconnect is section 5.3's resynchronization callback. It fires after
	// the bus connection has been re-established and before any event from the
	// new connection is forwarded; the supervisor supplies the body.
	OnReconnect func()
}

// SystemdEnv is what the probe learned. Every field maps onto a `runtime_info`
// column, and a nil or zero one means "not learned" rather than "denied".
type SystemdEnv struct {
	// Control is the channel section 5.3 chose, or nil in the F10 degraded mode.
	// It never means the daemon spawns llama-server itself.
	Control systemd.Controller
	// ControlKind is `runtime_info.systemd_control`.
	ControlKind model.SystemdControl
	// Polkit is the result of the two boot CheckAuthorization calls, or nil when
	// they were not made at all: user scope, where a user manager authorizes its
	// owner unconditionally (section 5.2a), and a host whose bus could not be
	// reached. Both leave `polkit_ok` and `polkit_unit_files` NULL — "not
	// applicable", never 0 (section 11.1a).
	Polkit *systemd.PolkitResult
	// PolkitDetail is `runtime_info.polkit_detail`: how the answer was reached.
	PolkitDetail string
	// Journal is `runtime_info.journal_read` (D77). It is orthogonal to all four
	// control facts: a fully granted host can still have an identity that cannot
	// read the journal.
	Journal model.JournalRead
}

// ManageUnitFiles reports whether the daemon may enable and disable unit files
// — the capability section 11.1a gates `PUT /instances/{id}/autostart`, the
// supervisor's autostart action and `DELETE /instances/{id}`'s disable on.
//
// It is false in the F10 mode (no manager to ask), false when the grant was
// withheld or denied, and true in user scope, where a user manager authorizes
// its owner unconditionally and the two checks are deliberately not made.
func (e SystemdEnv) ManageUnitFiles(scope model.SystemdScope) bool {
	if e.Control == nil {
		return false
	}
	if e.Polkit == nil {
		return scope == model.ScopeUser
	}
	return e.Polkit.ManageUnitFiles
}

// SystemdProbe is Options.Systemd's shape.
type SystemdProbe func(ctx context.Context, opts SystemdOptions) SystemdEnv

// ProbeSystemd is the production probe: the control channel, the two polkit
// calls and the journal probe, in that order, none of them fatal.
//
// It never returns an error. Section 11.1 step 6a is explicit that an
// unreachable systemd records `systemd_control='unavailable'` and continues
// into the degraded mode of section 11.1a — the daemon does not refuse to
// start, because the model, download, cache, fit and benchmark half of the
// product works perfectly well on a host with no service manager.
func ProbeSystemd(ctx context.Context, opts SystemdOptions) SystemdEnv {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}

	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()

	env := SystemdEnv{ControlKind: model.ControlUnavailable, Journal: model.JournalUnavailable}

	control, kind, err := systemd.Connect(ctx, systemd.Options{
		Scope:       opts.Scope,
		Logger:      log,
		OnReconnect: opts.OnReconnect,
	})
	env.ControlKind = kind
	if err != nil {
		// F10, and it is a degraded mode rather than a refusal. Instance
		// control, autostart and self-update are unavailable; everything else
		// keeps working, and `GET /instances/{id}/command` prints the argv to
		// run by hand.
		log.Warn("no service manager is reachable; instance control is unavailable (F10)",
			"scope", string(opts.Scope), "error", err)
		env.PolkitDetail = "systemd is unreachable, so neither CheckAuthorization call was made"
		return env
	}
	env.Control = control

	// The two boot CheckAuthorization calls (section 5.2), made before anything
	// a user clicks depends on the answer. In user scope they are not made at
	// all and both columns stay NULL.
	res, applicable, perr := systemd.CheckPolkit(ctx, opts.Scope)
	switch {
	case !applicable:
		env.PolkitDetail = "user scope: a user manager authorizes its owner unconditionally, " +
			"so neither CheckAuthorization call is made (section 5.2a)"
	case perr != nil:
		// The answer was not learned, which is not the same as `denied`: leaving
		// Polkit nil keeps both columns NULL and the detail says why.
		env.PolkitDetail = "the polkit authorization check could not be made: " + perr.Error()
		log.Warn("the polkit authorization check could not be made", "error", perr)
	default:
		env.Polkit = &res
		env.PolkitDetail = res.Detail
		if !res.ManageUnits {
			log.Warn("polkit denied manage-units; the control plane is read-only (F9)",
				"remediation", "sudo llamaman install-units --repair-polkit")
		}
	}

	// D77: the grant is arranged by `install-units` and PROBED here rather than
	// trusted, because a denial that showed as an empty log pane would be a
	// required SPEC section 3.3 feature failing quietly.
	env.Journal = systemd.ProbeJournalRead(ctx, opts.Scope, systemd.UnitDaemon, systemd.JournalOptions{})
	if env.Journal != model.JournalOK {
		log.Warn("this identity cannot read the journal; log panes will say so (F23)",
			"journal_read", string(env.Journal),
			"remediation", "sudo usermod -aG systemd-journal <identity> && sudo systemctl restart llamaman.service")
	}
	return env
}

// closeController closes a control channel that owns one. The Controller
// interface deliberately does not declare Close — a consumer of the vocabulary
// has no business closing the daemon's only bus connection — so the composition
// root, which built it, asks for the method it knows the implementations have.
func closeController(c systemd.Controller) error {
	if closer, ok := c.(interface{ Close() error }); ok {
		return closer.Close()
	}
	return nil
}

// journalTail satisfies supervisor.Journal (section 5.8's fit observation) over
// internal/systemd's reader, which is the one package allowed to run
// `journalctl` (D49 invariant 2).
//
// It reduces each entry to its MESSAGE because that is all ParseFitReport reads:
// llama.cpp's own startup lines — `load_tensors: CUDA0 model buffer size = …`
// and its two siblings — are the ground truth D33 shows beside the estimate and
// the numerator of the ratio D32 learns.
//
// The scope is carried because `--user` is not optional in the D2 topology: the
// system journal has none of a user manager's units, so a user-scope daemon
// reading without it would find no lines and would silently record no
// observation at all.
type journalTail struct{ scope model.SystemdScope }

func (j journalTail) Tail(ctx context.Context, unit string, n int) ([]string, error) {
	entries, err := systemd.Tail(ctx, systemd.JournalOptions{
		Scope: j.scope,
		Units: []string{unit},
		Lines: n,
	})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Message)
	}
	return out, nil
}
