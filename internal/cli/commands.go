package cli

// The command set below is the authoritative subcommand list of DESIGN section
// 1. Every unit file's ExecStart names one of these, and a CI test asserts that
// correspondence, so a name added or removed here is a change to the units too.

// Serve runs the daemon: it will hand off to internal/app, which opens the
// database, migrates, constructs the services, starts the workers and serves
// HTTP until shutdown (DESIGN section 11.1).
func Serve(env Env, args []string) error { return stub(env, "serve") }

// Status prints the health of this installation — units, instances, database,
// disk — for a human or, with --json, for a script (DESIGN section 11.2).
func Status(env Env, args []string) error { return stub(env, "status") }

// Doctor checks the host for the conditions this daemon needs and prints
// remediation for each one it fails (DESIGN sections 11.2 and 17).
func Doctor(env Env, args []string) error { return stub(env, "doctor") }

// Diagnostics writes a redacted support bundle to --out (D50).
func Diagnostics(env Env, args []string) error { return stub(env, "diagnostics") }

// ResetPassword resets the admin password from the host, for the operator who
// locked themselves out of the web UI (DESIGN section 11.2).
func ResetPassword(env Env, args []string) error { return stub(env, "reset-password") }

// RestoreDB is the explicit, offline database restore of DESIGN section 12.4:
// step 3 of a five-command procedure, never run by itself, refusing while the
// lock is held and refusing a snapshot from outside db-backups/ (D90).
func RestoreDB(env Env, args []string) error { return stub(env, "restore-db") }

// InstallUnits renders and installs the systemd unit and polkit files (DESIGN
// section 5).
func InstallUnits(env Env, args []string) error { return stub(env, "install-units") }

// VerifyRelease verifies a downloaded release against the embedded ed25519
// public key and its sha256 checksums file (DESIGN section 12).
func VerifyRelease(env Env, args []string) error { return stub(env, "verify-release") }

// InstanceExec is the launcher named by every instance unit's ExecStart: it
// opens the database, renders the argv for its instance and execs llama-server.
// Unit-only (DESIGN section 5.6).
func InstanceExec(env Env, args []string) error {
	if err := unitOnly(env, "instance-exec", args); err != nil {
		return err
	}
	return stub(env, "instance-exec")
}

// SelfupdateApply is the root oneshot of DESIGN section 12.1: it performs the
// staged binary swap and restarts the daemon. Unit-only.
func SelfupdateApply(env Env, args []string) error {
	if err := unitOnly(env, "selfupdate-apply", args); err != nil {
		return err
	}
	return stub(env, "selfupdate-apply")
}

// UpdateVerify is the judge of DESIGN section 12.2, started by the OnFailure=
// on llamaman.service: it reverts an unconfirmed update. Unit-only.
func UpdateVerify(env Env, args []string) error {
	if err := unitOnly(env, "update-verify", args); err != nil {
		return err
	}
	return stub(env, "update-verify")
}
