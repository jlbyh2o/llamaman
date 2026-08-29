// Command llamaman is the single binary: daemon, launcher, installer and
// support tooling in one. main() does subcommand dispatch and nothing else —
// every command's behavior lives in internal/cli (DESIGN section 1).
package main

import (
	"errors"
	"fmt"
	"os"
	"sort"

	"github.com/jlbyh2o/llamaman/internal/buildinfo"
	"github.com/jlbyh2o/llamaman/internal/cli"
)

// commands is the authoritative subcommand list of DESIGN section 1. Every unit
// file's ExecStart names one of these and a CI test asserts that
// correspondence.
var commands = map[string]struct {
	summary string
	run     func(cli.Env, []string) error
}{
	"serve":            {"run the daemon", cli.Serve},
	"status":           {"print installation health", cli.Status},
	"doctor":           {"check the host and print remediation", cli.Doctor},
	"diagnostics":      {"write a redacted support bundle", cli.Diagnostics},
	"reset-password":   {"reset the admin password from the host", cli.ResetPassword},
	"restore-db":       {"restore the database from a db-backups/ snapshot", cli.RestoreDB},
	"install-units":    {"render and install the systemd units and polkit rules", cli.InstallUnits},
	"instance-exec":    {"launch one instance (unit-only)", cli.InstanceExec},
	"selfupdate-apply": {"apply a staged binary update (unit-only)", cli.SelfupdateApply},
	"update-verify":    {"judge an unconfirmed update and revert it (unit-only)", cli.UpdateVerify},
	"verify-release":   {"verify a release against the embedded signing key", cli.VerifyRelease},
	// "version" is unreachable through the map lookup in run(): the switch
	// below matches "version" (and -v/--version) and returns before this map
	// is ever consulted, so versionCmd itself is dead as a dispatch target.
	// The entry stays anyway, deliberately, so usage() can build its listing
	// from this one table instead of a second, hand-maintained list that
	// could drift from it — do not "fix" this by deleting the entry.
	"version": {"print version, commit, build date and channel", versionCmd},
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	env := cli.DefaultEnv()

	if len(args) == 0 {
		usage()
		return 2
	}

	name := args[0]
	rest := args[1:]

	switch name {
	case "-h", "--help", "help":
		usage()
		return 0
	case "version", "-v", "--version":
		printVersion()
		return 0
	}

	cmd, ok := commands[name]
	if !ok {
		fmt.Fprintf(os.Stderr, "llamaman: unknown subcommand %q\n\n", name)
		usage()
		return 2
	}

	if err := cmd.run(env, rest); err != nil {
		// The command has already explained itself on stderr; all that is left
		// is choosing the status. A command whose exit code is itself a
		// contract — instance-exec's 64/65/69/70/72/75/78 of DESIGN section
		// 5.6, which the supervisor reads back into instance_starts.exit_code —
		// says so with a cli.ExitError, and that code wins over the generic
		// arms below.
		if code, ok := cli.ExitCode(err); ok {
			return code
		}
		if errors.Is(err, cli.ErrInteractive) {
			return 3
		}
		return 1
	}
	return 0
}

// versionCmd is the map entry for `version`; the dispatch switch also answers
// -v and --version with the same output.
func versionCmd(cli.Env, []string) error {
	printVersion()
	return nil
}

func printVersion() {
	fmt.Printf("llamaman %s\n", buildinfo.Version)
	fmt.Printf("commit:  %s\n", buildinfo.Commit)
	fmt.Printf("built:   %s\n", buildinfo.Date)
	fmt.Printf("channel: %s\n", buildinfo.Channel)
}

func usage() {
	fmt.Fprintf(os.Stderr, "llamaman %s — manage llama.cpp servers on this host\n\n", buildinfo.Version)
	fmt.Fprintf(os.Stderr, "Usage: llamaman <subcommand> [flags]\n\nSubcommands:\n")

	names := make([]string, 0, len(commands))
	for n := range commands {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(os.Stderr, "  %-17s %s\n", n, commands[n].summary)
	}
	fmt.Fprintf(os.Stderr, "\nUnit-only entry points (instance-exec, selfupdate-apply, update-verify)\n")
	fmt.Fprintf(os.Stderr, "are started by systemd and refuse an interactive terminal without --force.\n")
}
