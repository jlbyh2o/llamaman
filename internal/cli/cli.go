package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

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
	// Interactive reports whether stdin is a terminal. It is a field rather
	// than a call so a test can exercise both arms of the unit-only guard.
	Interactive bool
}

// DefaultEnv is the Env for a real process.
func DefaultEnv() Env {
	return Env{Stdout: os.Stdout, Stderr: os.Stderr, Interactive: stdinIsTerminal()}
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

// unitOnly parses the shared flags of a unit-only entry point and enforces the
// TTY refusal. The three commands that use it are started by systemd, never by
// a human, and --force exists only so a developer can say they meant it.
func unitOnly(env Env, name string, args []string) error {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	force := fs.Bool("force", false, "run even from an interactive terminal (this is a unit-only entry point)")
	fs.Usage = func() {
		fmt.Fprintf(env.Stderr, "Usage: llamaman %s [flags]\n\n", name)
		fmt.Fprintf(env.Stderr, "This is a unit-only entry point: it is started by systemd and refuses\n")
		fmt.Fprintf(env.Stderr, "to run from an interactive terminal without --force.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if env.Interactive && !*force {
		fmt.Fprintf(env.Stderr, "llamaman %s: %v\n", name, ErrInteractive)
		return ErrInteractive
	}
	return nil
}
