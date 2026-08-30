// Package cli implements every subcommand except the dispatch itself: status,
// doctor, diagnostics, reset-password, restore-db, install-units,
// instance-exec, selfupdate-apply, update-verify and verify-release, plus the
// serve entry point that hands off to internal/app. cmd/llamaman does argument
// dispatch and nothing else, so each command's behavior is testable without a
// process (DESIGN sections 1, 11.3 and 12).
//
// Five commands are implemented today — `status`, `doctor`, `reset-password`,
// `install-units` and `instance-exec`, in status.go, doctor.go,
// resetpassword.go, installunits.go and instanceexec.go. The rest print "not
// implemented" and return ErrNotImplemented, which cmd/llamaman turns into a
// non-zero exit.
//
// `instance-exec` is the odd one, and deliberately so: it is not a command a
// human runs but the ExecStart of every instance unit (DESIGN section 5.6). It
// runs with no D-Bus, no HTTP, no GPU probe and no network — its entire world is
// this binary, the database file and `%i` — which is what makes instances start
// correctly at boot even when llamaman.service itself is failing. Its exit
// statuses are a CONTRACT rather than a diagnostic: the supervisor reads them
// back into `instance_starts.exit_code`, the restart policy counts them, and the
// UI maps them onto remediation cards. They are declared once, in
// internal/supervisor, and imported here.
//
// `install-units` is the only privileged one: it renders the systemd units and
// polkit rules from the templates embedded in the binary, adds the service
// identity to the systemd-journal group and reloads the manager. Every byte it
// writes is produced by internal/systemd, which is also what the drift check of
// DESIGN section 5.4a re-renders — one producer, so a host can never be told it
// has drifted from a template nothing would have produced.
//
// One rule binds every subcommand a root user can invoke (DESIGN section 11.3):
// they create NOTHING under the state directory — not the database, not its
// `-wal`/`-shm` sidecars, not `secret.key`, not a directory. `status` and
// `doctor` therefore open the database read-only, and only if it already exists.
// `reset-password` is deliberately outside that rule, because its whole purpose
// is to write the database with the daemon down; what it obeys instead is the
// exception stated beside it — the euid is checked against `stat`, and a root
// caller chowns every file it may have created back to the database's identity
// before it exits.
package cli
