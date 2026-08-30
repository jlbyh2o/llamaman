package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/systemd"
)

// The three rows of DESIGN section 11.3's doctor that ask about the service
// manager: systemd and D-Bus reachability, whether the two polkit actions were
// granted, and whether this identity can read the journal.
//
// They belong together because they are one probe with three answers, and
// because on a host in the F9 or F10 degraded mode (section 11.1a) `doctor` is
// the documented way to see it — the UI that would otherwise say so is served
// by the daemon these checks exist to explain the absence of.
//
// Nothing here writes anything, opens the database, or touches the state
// directory: the rule at the top of section 11.3 binds every root-invocable
// subcommand, and these checks are the ones most likely to be run as root.

// doctorProbeTimeout bounds each half of the probe. `doctor` is something a
// human is waiting on, and a bus that does not answer is itself the finding.
const doctorProbeTimeout = 5 * time.Second

func checkSystemd(ctx context.Context, env Env, p paths) []Check {
	scope := doctorScope(env)

	unit := checkUnitFiles(scope)
	if !systemd.Available() {
		// No systemd PID 1 at all: F10. It is a degraded mode rather than a
		// refusal — models, downloads, cache, fit, bench, tokens, the gateway
		// and settings all work — and saying so plainly is the whole job here.
		return []Check{
			{
				Name:   "systemd",
				Status: CheckWarn,
				Detail: "no service manager on this host (/run/systemd/system is absent): " +
					"instance control, autostart and self-update are unavailable (F10); " +
					"everything else works, and GET /instances/{id}/command prints the argv to run by hand",
			},
			unit,
			{
				Name:   "polkit",
				Status: CheckSkipped,
				Detail: "there is no service manager to authorize against",
			},
			{
				Name:   "journal",
				Status: CheckSkipped,
				Detail: "there is no journal on a host without systemd",
			},
		}
	}

	control, controlCheck := checkControlChannel(ctx, scope)
	if control != nil {
		defer func() { _ = closeControl(control) }()
	}

	// Both remaining checks describe an INSTALLED service, and on a host where
	// `install-units` has never run neither has an answer worth printing: there
	// is no polkit rule to have been granted, and `journalctl --unit
	// llamaman.service` returns nothing because the unit has never logged —
	// which the D77 probe would otherwise have to read as a denial. Section
	// 11.3 calls that case skipped rather than failed, exactly as it does for
	// the database that does not exist yet.
	if unit.Status == CheckWarn && unit.Remediation != "" {
		return []Check{controlCheck, unit,
			{
				Name:        "polkit",
				Status:      CheckSkipped,
				Detail:      "no units are installed, so no polkit rule has been written yet",
				Remediation: unit.Remediation,
			},
			{
				Name:        "journal",
				Status:      CheckSkipped,
				Detail:      systemd.UnitDaemon + " is not installed, so it has never logged",
				Remediation: unit.Remediation,
			},
		}
	}

	return []Check{controlCheck, unit, checkPolkit(ctx, scope), checkJournal(ctx, env, p, scope)}
}

// doctorScope answers section 11.1 step 1's question the same way the daemon
// does: the user bus, and only when that manager reports llamaman.service as a
// unit it knows. Every other case, including no bus at all, is `system`.
func doctorScope(env Env) model.SystemdScope {
	if scope, ok := systemd.ScopeProbe(); ok {
		return scope
	}
	_ = env
	return model.ScopeSystem
}

// checkControlChannel is the D-Bus half of section 5.3's probe, answered the
// same way the boot sequence answers it: D-Bus first, `systemctl` second,
// nothing third.
func checkControlChannel(ctx context.Context, scope model.SystemdScope) (systemd.Controller, Check) {
	ctx, cancel := context.WithTimeout(ctx, doctorProbeTimeout)
	defer cancel()

	c := Check{Name: "systemd"}
	control, kind, err := systemd.Connect(ctx, systemd.Options{Scope: scope})
	switch {
	case err != nil:
		c.Status = CheckFail
		c.Detail = fmt.Sprintf("scope %s: neither the D-Bus manager nor systemctl could be reached: %v",
			scope, err)
		c.Remediation = "check that the service identity may talk to the bus, or re-run `sudo llamaman install-units --identity <user>`"
		return nil, c
	case kind == model.ControlExec:
		c.Status = CheckWarn
		c.Detail = fmt.Sprintf("scope %s: D-Bus is unusable, so control falls back to systemctl; "+
			"unit state is polled rather than pushed", scope)
		return control, c
	default:
		c.Status = CheckOK
		c.Detail = fmt.Sprintf("scope %s, control: dbus", scope)
		return control, c
	}
}

// checkUnitFiles is section 11.3's "unit presence" row. It reports which of the
// units `install-units` writes are missing, and never repairs: the daemon
// cannot write /etc, and neither can this.
func checkUnitFiles(scope model.SystemdScope) Check {
	c := Check{Name: "units"}
	dir := systemd.UnitDir(scope)

	var missing []string
	for _, name := range systemd.UnitNames(scope) {
		if _, err := os.Stat(dir + "/" + name); err != nil {
			missing = append(missing, name)
		}
	}
	switch {
	case len(missing) == len(systemd.UnitNames(scope)):
		c.Status = CheckWarn
		c.Detail = "no llamaman units are installed in " + dir
		c.Remediation = "sudo llamaman install-units --identity <user>"
	case len(missing) > 0:
		// A missing unit is F16, and the repair is the same command in every
		// case (section 5.4a).
		c.Status = CheckFail
		c.Detail = "missing from " + dir + ": " + strings.Join(missing, ", ")
		c.Remediation = "sudo llamaman install-units --identity <user>"
	default:
		c.Status = CheckOK
		c.Detail = fmt.Sprintf("all %d units present in %s", len(systemd.UnitNames(scope)), dir)
	}
	return c
}

// checkPolkit is section 5.2's boot self-test, run on demand: may this identity
// start, stop, restart and reset-failed its own units, and may it enable and
// disable them?
func checkPolkit(ctx context.Context, scope model.SystemdScope) Check {
	ctx, cancel := context.WithTimeout(ctx, doctorProbeTimeout)
	defer cancel()

	c := Check{Name: "polkit"}
	res, applicable, err := systemd.CheckPolkit(ctx, scope)
	switch {
	case !applicable:
		// User scope: a user manager authorizes its owner unconditionally, so
		// neither call is made and there is no rule to install. "Not
		// applicable" is not "denied" (section 11.1a).
		c.Status = CheckOK
		c.Detail = "user scope: a user manager authorizes its owner, so no polkit rule is needed (D2)"
	case err != nil:
		c.Status = CheckWarn
		c.Detail = "the authorization check could not be made: " + err.Error()
		c.Remediation = "sudo llamaman install-units --repair-polkit"
	case !res.ManageUnits:
		// F9: the control plane is read-only, and every instance action
		// answers 409 systemd_denied rather than failing on the user's first
		// click.
		c.Status = CheckFail
		c.Detail = "manage-units is DENIED: instance start, stop and restart are unavailable (F9)"
		c.Remediation = "sudo llamaman install-units --repair-polkit"
	case !res.ManageUnitFiles:
		c.Status = CheckWarn
		c.Detail = "manage-units granted; manage-unit-files withheld, so autostart is read-only " +
			"(installed with --no-autostart-grant)"
		c.Remediation = "sudo systemctl enable llamaman-instance@<name>.service, " +
			"or re-run install-units without --no-autostart-grant"
	default:
		c.Status = CheckOK
		c.Detail = res.Detail
	}
	return c
}

// checkJournal is D77: the grant `install-units` arranges is PROBED, never
// trusted, because a denial that showed as an empty log pane would be a
// required SPEC section 3.3 feature failing quietly.
func checkJournal(ctx context.Context, env Env, p paths, scope model.SystemdScope) Check {
	ctx, cancel := context.WithTimeout(ctx, doctorProbeTimeout)
	defer cancel()

	c := Check{Name: "journal"}
	switch systemd.ProbeJournalRead(ctx, scope, systemd.UnitDaemon, systemd.JournalOptions{}) {
	case model.JournalOK:
		c.Status = CheckOK
		c.Detail = "this identity can read " + systemd.UnitDaemon + "'s journal"
	case model.JournalDenied:
		// The --dedicated-user topology's failure: a uid < 1000 whose messages
		// live in the SYSTEM journal, which journald will not show a caller
		// outside the systemd-journal group.
		c.Status = CheckFail
		c.Detail = "journalctl returned nothing for " + systemd.UnitDaemon +
			": log panes, the fit observation and diagnostics bundles will be empty (F23)"
		c.Remediation = fmt.Sprintf(
			"sudo usermod -aG %s %s && sudo systemctl restart %s",
			systemd.JournalGroup, doctorIdentity(env, p), systemd.UnitDaemon)
	default:
		c.Status = CheckWarn
		c.Detail = "journalctl could not be run at all; journal-derived features are unavailable"
	}
	return c
}

// doctorIdentity names the service identity for the remediation line: the owner
// of the database when there is one, because that is the account the units run
// as, and a placeholder otherwise.
func doctorIdentity(env Env, p paths) string {
	_ = env
	access, owner, err := classify(p)
	if err != nil || access == dbAbsent {
		return "<identity>"
	}
	return identityName(owner)
}

// closeControl closes a control channel doctor opened. The Controller interface
// does not declare Close — nothing that merely USES the vocabulary should be
// able to drop the connection — so the opener asks for the method it knows the
// implementations have.
func closeControl(c systemd.Controller) error {
	if cl, ok := c.(interface{ Close() error }); ok {
		return cl.Close()
	}
	return nil
}
