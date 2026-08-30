package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// ErrNotImplemented is returned by every command in this skeleton. cmd/llamaman
// maps it to a non-zero exit status.
var ErrNotImplemented = errors.New("not implemented")

// ErrInteractive is returned when a unit-only entry point is invoked from an
// interactive terminal without --force (DESIGN section 1).
var ErrInteractive = errors.New("refusing to run from an interactive terminal without --force")

// ExitError carries the exact process exit status a command must produce, for
// the cases where the status is itself a contract rather than a bare "it
// failed". cmd/llamaman unwraps it and exits with Code.
//
// The reason it exists is DESIGN section 5.6: instance-exec's exit codes (64
// instance missing, 65 bad flags, 69 runtime missing or rebuilding, 70 launcher
// DB unavailable, 72 model file missing, 75 schema ahead, 78 internal port
// taken) are read back into instance_starts.exit_code by the supervisor, drive
// the restart policy and back failure rows F4, F5 and F7. Collapsing them to 1
// would erase a contract section 15 requires a test to assert in full.
type ExitError struct {
	Code int
	Err  error
}

// NewExitError returns an ExitError with code and a message.
func NewExitError(code int, format string, args ...any) *ExitError {
	return &ExitError{Code: code, Err: fmt.Errorf(format, args...)}
}

func (e *ExitError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("exit status %d", e.Code)
	}
	return e.Err.Error()
}

// Unwrap exposes the wrapped cause to errors.Is and errors.As.
func (e *ExitError) Unwrap() error { return e.Err }

// ExitCode reports the process exit status an error asks for, and whether the
// error asked for one at all. cmd/llamaman is its only caller.
func ExitCode(err error) (int, bool) {
	var ee *ExitError
	if errors.As(err, &ee) {
		return ee.Code, true
	}
	return 0, false
}

// Env is what a command needs from the process it runs in. Tests supply their
// own; cmd/llamaman supplies the real one.
type Env struct {
	Stdout io.Writer
	Stderr io.Writer
	// Stdin is where `reset-password --stdin` reads from. Nil uses os.Stdin.
	Stdin io.Reader
	// Interactive reports whether stdin is a terminal. It is a field rather
	// than a call so a test can exercise both arms of the unit-only guard.
	Interactive bool
	// Getenv reads the environment. Nil uses os.Getenv. It is injectable
	// because D72's chain — which is how `status`, `doctor` and
	// `reset-password` find the database — is entirely environment-driven, and
	// a test that had to mutate the real environment could not run in parallel.
	Getenv func(string) string
	// Now supplies the instants a command stamps or measures against. Nil uses
	// time.Now.
	Now func() time.Time
}

// DefaultEnv is the Env for a real process.
func DefaultEnv() Env {
	return Env{
		Stdout:      os.Stdout,
		Stderr:      os.Stderr,
		Stdin:       os.Stdin,
		Interactive: stdinIsTerminal(),
		Getenv:      os.Getenv,
		Now:         time.Now,
	}
}

func (e Env) getenv() func(string) string {
	if e.Getenv == nil {
		return os.Getenv
	}
	return e.Getenv
}

func (e Env) now() time.Time {
	if e.Now == nil {
		return time.Now()
	}
	return e.Now()
}

func (e Env) stdin() io.Reader {
	if e.Stdin == nil {
		return os.Stdin
	}
	return e.Stdin
}

// stdinIsTerminal reports whether stdin is a real terminal.
//
// It asks the kernel for the terminal attributes rather than checking
// os.ModeCharDevice, because /dev/null is a character device too — and a
// systemd unit's stdin is /dev/null by default, so the cheap check would refuse
// to run exactly the three commands that only systemd ever starts.
func stdinIsTerminal() bool {
	_, err := unix.IoctlGetTermios(int(os.Stdin.Fd()), unix.TCGETS)
	return err == nil
}

// stub reports that a command exists but has no implementation yet.
func stub(env Env, name string) error {
	fmt.Fprintf(env.Stderr, "llamaman %s: not implemented\n", name)
	return ErrNotImplemented
}

// unitOnly strips the one flag every unit-only entry point shares and enforces
// the TTY refusal. The three commands that use it are started by systemd, never
// by a human, and --force exists only so a developer can say they meant it.
//
// It returns everything else VERBATIM — flags included — because each of the
// three owns its own argument surface and this function must not stand between
// a rendered `ExecStart=` and the FlagSet that understands it. That is not a
// stylistic preference: the units render
//
//	ExecStart=<prefix>/llamaman selfupdate-apply --scope system
//	ExecStart=<prefix>/llamaman.prev update-verify --scope <scope>
//
// (§12.2), and an earlier version of this function re-parsed the whole argument
// list through a FlagSet that defined only `force` the moment the first
// remaining argument began with `-`. Both actors therefore died with "flag
// provided but not defined: -scope" before their own parser ever ran, which
// killed the swap and D88's automatic revert on every host at once.
//
// `--force` is recognized WHEREVER it appears, because Go's flag package stops
// parsing at the first non-flag argument: `instance-exec qwen --force` would
// otherwise hand the launcher two "instance names", and `<subcommand> <name>
// --force` is exactly what a human types when a unit-only entry point refuses
// them.
func unitOnly(env Env, name string, args []string) ([]string, error) {
	var (
		force bool
		rest  []string
	)
	for _, a := range args {
		if a == "--force" || a == "-force" {
			force = true
			continue
		}
		rest = append(rest, a)
	}

	if env.Interactive && !force {
		fmt.Fprintf(env.Stderr, "llamaman %s: %v\n", name, ErrInteractive)
		return nil, ErrInteractive
	}
	return rest, nil
}

// unitOnlyUsage is the help text all three entry points share, printed by
// whichever FlagSet reports a bad argument. It names `--force` even though
// unitOnly consumed it before any FlagSet could see it, because a human who
// reaches this text is exactly the reader that flag exists for.
func unitOnlyUsage(env Env, name, argSummary string) func() {
	return func() {
		fmt.Fprintf(env.Stderr, "Usage: llamaman %s %s\n\n", name, argSummary)
		fmt.Fprintf(env.Stderr, "This is a unit-only entry point: it is started by systemd and refuses\n")
		fmt.Fprintf(env.Stderr, "to run from an interactive terminal without --force.\n")
	}
}

// unitOnlyPositional parses the arguments of a unit-only entry point that takes
// NO flags of its own, so an unknown one is still reported with usage rather
// than mistaken for a positional. `instance-exec %i` is the only such command.
func unitOnlyPositional(env Env, name, argSummary string, args []string) ([]string, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.Usage = unitOnlyUsage(env, name, argSummary)
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return fs.Args(), nil
}
