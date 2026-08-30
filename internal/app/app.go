package app

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// Options is what the composition root needs from the process it runs in.
// cmd/llamaman supplies the real one through internal/cli; a test supplies its
// own.
type Options struct {
	// Args are the `serve` subcommand's arguments, without the subcommand
	// name.
	Args []string

	// Stdout and Stderr are where flag parsing complains. Everything else goes
	// through Logger, because everything else goes to journald.
	Stdout io.Writer
	Stderr io.Writer

	// Logger is the daemon's logger. Nil uses slog.Default. internal/logx owns
	// the handler tuned for journald; this package only decides what to say.
	Logger *slog.Logger

	// Notifier is the sd_notify channel. Nil uses the debug-logging no-op,
	// which is what a hand-run `llamaman serve` gets.
	Notifier Notifier

	// Now supplies every instant the boot sequence stamps. Nil uses time.Now.
	Now func() time.Time

	// StateDirOverride short-circuits D72's resolution chain. It exists for
	// tests and for running out of a checkout; it is deliberately not a flag,
	// because SPEC section 3.9 says a user never hand-configures anything.
	StateDirOverride string

	// Getenv reads the environment. Nil uses os.Getenv. It is injectable
	// because D72's chain is entirely environment-driven and a test that had
	// to mutate the real environment could not run in parallel.
	Getenv func(string) string

	// ScopeProbe answers step 1's user-bus question when no --scope flag was
	// given. Nil falls back to `system`, which section 11.1 already names as
	// the answer in every case where the probe does not say `user`.
	// internal/systemd supplies the real one (D49 invariant 2).
	ScopeProbe func() (model.SystemdScope, bool)

	// Systemd is step 6's control-channel, polkit and journal probe. Nil makes
	// no probe at all and leaves every `runtime_info` column it fills NULL —
	// "not learned", never "denied" (F14). internal/cli wires ProbeSystemd; a
	// test of the boot sequence leaves it nil so it never dials the host's bus.
	Systemd SystemdProbe

	// Conformance is D43's response-conformance middleware, handed straight to
	// api.Config. It is nil in production — it buffers every response body,
	// which would defeat the streaming SSE is built around — and supplied by
	// the integration suite, where an undocumented endpoint, a missing
	// documented field or an extra field fails the run.
	Conformance func(http.Handler) http.Handler

	// ReadyHook, when set, is called once the listener is bound and the server
	// is accepting, with the address it landed on. It is how a test waits for
	// the daemon without polling, and how an integration suite learns the
	// ephemeral port the walk chose.
	ReadyHook func(addr string)
}

// flags are the ONLY two flags `serve` takes, and both are written by our own
// installer into the unit's ExecStart rather than typed by a user:
//
//   - --scope, rendered by `install-units` because that is the component that
//     DECIDES the topology (section 11.1 step 1).
//   - --port, a SEED for `ui.port_desired` on a fresh install and never an
//     override (step 6b) — the stored setting always wins, because it was set
//     in the UI, deliberately, by a human.
//
// There is no config file and no environment variable, and there never will be
// (SPEC section 3.9).
type flags struct {
	scope string
	port  int
}

func parseFlags(args []string, stderr io.Writer) (flags, error) {
	var f flags
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&f.scope, "scope", "", "systemd scope this daemon runs under: system|user (written by install-units)")
	fs.IntVar(&f.port, "port", 0, "seed for ui.port_desired on a fresh install (written by install-units --port)")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: llamaman serve [flags]\n\n")
		fmt.Fprintf(stderr, "Both flags are written into the unit by `llamaman install-units`.\n")
		fmt.Fprintf(stderr, "There is no configuration file: everything else is set in the web UI.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return flags{}, err
	}
	if fs.NArg() > 0 {
		return flags{}, fmt.Errorf("serve takes no positional arguments, got %q", fs.Arg(0))
	}
	if f.port != 0 && (f.port < 1024 || f.port > 65535) {
		return flags{}, fmt.Errorf("--port must be between 1024 and 65535, got %d", f.port)
	}
	return f, nil
}

// Run is `llamaman serve`: DESIGN section 11.1's boot sequence, then HTTP until
// ctx is canceled, then the graceful shutdown of section 9.4.
//
// It returns when the daemon has stopped. A nil return is a clean shutdown; an
// error is a start failure, which is exactly how the automatic revert of
// section 12.2 sees it — after StartLimitBurst attempts the unit reaches
// `failed` and OnFailure= starts the judge.
func Run(ctx context.Context, opts Options) error {
	opts = withDefaults(opts)

	f, err := parseFlags(opts.Args, opts.Stderr)
	if err != nil {
		return err
	}

	d, err := boot(ctx, opts, f)
	if err != nil {
		return err
	}
	defer d.close()

	return d.serve(ctx)
}

func withDefaults(o Options) Options {
	if o.Stdout == nil {
		o.Stdout = os.Stdout
	}
	if o.Stderr == nil {
		o.Stderr = os.Stderr
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	if o.Notifier == nil {
		o.Notifier = nopNotifier{log: o.Logger}
	}
	return o
}
