package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os/signal"

	"github.com/jlbyh2o/llamaman/internal/app"
	"github.com/jlbyh2o/llamaman/internal/systemd"
	"golang.org/x/sys/unix"
)

// The command set below is the authoritative subcommand list of DESIGN section
// 1. Every unit file's ExecStart names one of these, and a CI test asserts that
// correspondence, so a name added or removed here is a change to the units too.

// Serve runs the daemon. Everything it does lives in internal/app, the
// composition root: it opens the database, migrates, constructs the services,
// starts the workers and serves HTTP until shutdown (DESIGN section 11.1).
//
// This function owns exactly three things, and each is a process-level concern
// internal/app deliberately does not have:
//
//   - The signal context. SIGTERM is how systemd stops this unit and how
//     section 9.4's drain begins; SIGINT is the same thing typed by a
//     developer. NotifyContext restores the default disposition after the
//     first signal, so a second Ctrl-C kills a daemon that is wedged in its
//     drain instead of being swallowed.
//   - The F11 exit status. A second daemon on one state directory exits **70**
//     with a message naming the holding PID, so a hand-run `llamaman serve` can
//     never race the unit (section 11.1 step 2).
//   - Reporting the error on stderr. main() only chooses the status.
//
// It is also where the three seams internal/app declares are filled with
// internal/systemd's implementations, because D49's second invariant keeps the
// systemd vocabulary in one package and the composition root takes the answers
// as callbacks:
//
//   - The NOTIFIER. The unit is `Type=notify` with `NotifyAccess=main`,
//     `WatchdogSec=30` and `TimeoutStartSec=45` (section 5.4): a daemon that
//     never sends READY=1 is killed at 45 s, retried until the start limit is
//     exhausted, and reaches `failed` — which arms the revert judge through
//     `OnFailure=`. Without this the shipped binary cannot start under the
//     shipped unit.
//   - The SCOPE PROBE, section 11.1 step 1's single fallback for a unit
//     installed before `--scope` existed.
//   - The SYSTEMD PROBE, step 6's control channel, polkit and journal answers.
func Serve(env Env, args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), unix.SIGINT, unix.SIGTERM)
	defer stop()

	// $NOTIFY_SOCKET is set by systemd, not by a user, so reading it is not a
	// configuration file and does not breach SPEC section 3.9. Its absence is
	// the ordinary hand-run case and is not an error: internal/app falls back to
	// the debug-logging no-op, which is what a developer running
	// `llamaman serve` from a shell should get.
	var notifier app.Notifier
	sd, ok, err := systemd.NewNotifier(env.getenv())
	switch {
	case err != nil:
		// $NOTIFY_SOCKET was set and could not be opened. Say so and continue:
		// refusing to start would turn a manager-side problem into no daemon at
		// all, and the unit's own TimeoutStartSec= already reports the
		// consequence.
		fmt.Fprintf(env.Stderr, "llamaman serve: the readiness socket could not be opened: %v\n", err)
	case ok:
		notifier = sd
		defer sd.Close()
	}

	err = app.Run(ctx, app.Options{
		Args:       args,
		Stdout:     env.Stdout,
		Stderr:     env.Stderr,
		Notifier:   notifier,
		ScopeProbe: systemd.ScopeProbe,
		Systemd:    app.ProbeSystemd,
	})
	if err == nil {
		return nil
	}

	var locked *app.LockedError
	if errors.As(err, &locked) {
		fmt.Fprintf(env.Stderr, "llamaman serve: %v\n", locked)
		return &ExitError{Code: 70, Err: locked}
	}
	if errors.Is(err, flag.ErrHelp) {
		// The flag set has already printed usage.
		return err
	}
	fmt.Fprintf(env.Stderr, "llamaman serve: %v\n", err)
	return err
}

// Status, Doctor and ResetPassword live in their own files: status.go,
// doctor.go and resetpassword.go. They are the three subcommands of DESIGN
// section 11.3 that a human runs against a host, and each is long enough that a
// shared file would hide the one rule they have in common — status and doctor
// create NOTHING under the state directory, while reset-password is deliberately
// outside that rule because its whole purpose is to write.

// Diagnostics writes a redacted support bundle to --out (D50).
func Diagnostics(env Env, args []string) error { return stub(env, "diagnostics") }

// RestoreDB is the explicit, offline database restore of DESIGN section 12.4:
// step 3 of a five-command procedure, never run by itself, refusing while the
// lock is held and refusing a snapshot from outside db-backups/ (D90).
func RestoreDB(env Env, args []string) error { return stub(env, "restore-db") }

// InstallUnits lives in installunits.go: it renders and installs the systemd
// unit and polkit files, adds the identity to the systemd-journal group and
// reloads the manager (DESIGN sections 5, 11.3 and 13 step 7).

// VerifyRelease verifies a downloaded release against the embedded ed25519
// public key and its sha256 checksums file (DESIGN section 12).
func VerifyRelease(env Env, args []string) error { return stub(env, "verify-release") }

// InstanceExec lives in instanceexec.go: it is the launcher named by every
// instance unit's ExecStart, and it opens the database, renders the argv for
// its instance and execs llama-server (DESIGN section 5.6). Unit-only.

// SelfupdateApply is the root oneshot of DESIGN section 12.1: it performs the
// staged binary swap and restarts the daemon. Unit-only.
func SelfupdateApply(env Env, args []string) error {
	if _, err := unitOnly(env, "selfupdate-apply", args); err != nil {
		return err
	}
	return stub(env, "selfupdate-apply")
}

// UpdateVerify is the judge of DESIGN section 12.2, started by the OnFailure=
// on llamaman.service: it reverts an unconfirmed update. Unit-only.
func UpdateVerify(env Env, args []string) error {
	if _, err := unitOnly(env, "update-verify", args); err != nil {
		return err
	}
	return stub(env, "update-verify")
}
