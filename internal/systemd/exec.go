package systemd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// ExecOptions configures the degraded controller.
type ExecOptions struct {
	// Scope decides whether every invocation carries --user.
	Scope model.SystemdScope

	// Path is the systemctl binary. Empty resolves it through SystemctlPath(),
	// which is the only producer of that path in this design.
	Path string

	// Logger receives poll failures. Nil uses slog.Default.
	Logger *slog.Logger

	// PollInterval is how often SubscribeSubState re-reads unit state. Zero
	// uses 2 s. This is the whole difference the UI is told about when
	// GET /system/info reports `control: exec`: state is polled, not pushed.
	PollInterval time.Duration

	// run executes one command. Overridden by tests; nil uses os/exec.
	run runner
}

// runner is one systemctl invocation: stdout, stderr, exit status, and an error
// only when the process could not be run at all.
type runner func(ctx context.Context, name string, args ...string) (stdout, stderr string, code int, err error)

// ExecController is the fallback Controller: `systemctl`, chosen at boot when
// the D-Bus probe fails (DESIGN section 5.3). It implements the same interface,
// including the blocking/no-wait split — `--no-block` is the exact counterpart
// of not passing a JobRemoved channel.
//
// Everything it gives up is stated rather than hidden: state is polled instead
// of pushed, errors are recovered from exit statuses and English prose instead
// of D-Bus error names, and timestamps arrive at second rather than microsecond
// resolution.
type ExecController struct {
	scope    model.SystemdScope
	bin      string
	log      *slog.Logger
	interval time.Duration
	run      runner
}

var _ Controller = (*ExecController)(nil)

// NewExecController resolves systemctl and returns the fallback controller.
func NewExecController(opts ExecOptions) (*ExecController, error) {
	bin := opts.Path
	if bin == "" {
		p, err := SystemctlPath()
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
		}
		bin = p
	}
	c := &ExecController{
		scope:    opts.Scope,
		bin:      bin,
		log:      opts.Logger,
		interval: opts.PollInterval,
		run:      opts.run,
	}
	if c.log == nil {
		c.log = slog.Default()
	}
	if c.interval == 0 {
		c.interval = 2 * time.Second
	}
	if c.run == nil {
		c.run = execRunner
	}
	return c, nil
}

// Scope reports which manager this controller addresses.
func (c *ExecController) Scope() model.SystemdScope { return c.scope }

// Close releases nothing: every invocation is its own process.
func (c *ExecController) Close() error { return nil }

// args prefixes the scope flag onto a verb's arguments.
func (c *ExecController) args(rest ...string) []string {
	return append(scopeArgs(c.scope), rest...)
}

// execRunner is the real runner. Output is captured whole rather than streamed:
// every command here prints a handful of lines at most, and `systemctl show`
// against a pattern is the largest of them.
func execRunner(ctx context.Context, name string, args ...string) (string, string, int, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		// A non-zero exit is DATA, not a failure to run: `systemctl is-active`
		// exits 3 for a non-active unit and `start` exits 5 for a unit that
		// does not exist, and both answers are what the caller asked for. Only
		// a process that could not be started at all is an error here.
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return stdout.String(), stderr.String(), ee.ExitCode(), nil
		}
		return stdout.String(), stderr.String(), 0, err
	}
	return stdout.String(), stderr.String(), 0, nil
}

// verb runs one blocking job verb. `systemctl start` does not return until the
// job leaves the queue, which is the same completion semantics the D-Bus
// controller gets from JobRemoved — the difference is that a failed job is an
// exit status here instead of a result string.
func (c *ExecController) verb(ctx context.Context, name, unit string) (JobResult, error) {
	_, stderr, code, err := c.run(ctx, c.bin, c.args(name, unit)...)
	if err := translateExit(unit, code, stderr, err); err != nil {
		return JobFailed, err
	}
	return JobDone, nil
}

// Start blocks until the start job completes.
func (c *ExecController) Start(ctx context.Context, unit string) (JobResult, error) {
	return c.verb(ctx, "start", unit)
}

// Stop blocks until the stop job completes.
func (c *ExecController) Stop(ctx context.Context, unit string) (JobResult, error) {
	return c.verb(ctx, "stop", unit)
}

// Restart blocks until the restart job completes.
func (c *ExecController) Restart(ctx context.Context, unit string) (JobResult, error) {
	return c.verb(ctx, "restart", unit)
}

// StartNoWait enqueues the job and returns immediately.
//
// The returned JobPath is always empty: `systemctl --no-block` prints nothing a
// caller could correlate. That is acceptable because the two callers who need
// this variant need it for the side effect — not deadlocking against their own
// death — and neither reads the path back.
func (c *ExecController) StartNoWait(ctx context.Context, unit string) (JobPath, error) {
	return c.noBlock(ctx, "start", unit)
}

// RestartNoWait enqueues the restart job and returns immediately.
func (c *ExecController) RestartNoWait(ctx context.Context, unit string) (JobPath, error) {
	return c.noBlock(ctx, "restart", unit)
}

func (c *ExecController) noBlock(ctx context.Context, name, unit string) (JobPath, error) {
	_, stderr, code, err := c.run(ctx, c.bin, c.args(name, "--no-block", unit)...)
	if err := translateExit(unit, code, stderr, err); err != nil {
		return "", err
	}
	return "", nil
}

// Enable links units into their [Install] target and reloads the manager.
func (c *ExecController) Enable(ctx context.Context, units []string) error {
	return c.unitFiles(ctx, "enable", units)
}

// Disable unlinks units from their [Install] target and reloads the manager.
func (c *ExecController) Disable(ctx context.Context, units []string) error {
	return c.unitFiles(ctx, "disable", units)
}

func (c *ExecController) unitFiles(ctx context.Context, name string, units []string) error {
	if len(units) == 0 {
		return nil
	}
	_, stderr, code, err := c.run(ctx, c.bin, c.args(append([]string{name}, units...)...)...)
	if err := translateExit(joinUnits(units), code, stderr, err); err != nil {
		return err
	}
	return c.Reload(ctx)
}

// Reload is `daemon-reload`.
func (c *ExecController) Reload(ctx context.Context) error {
	_, stderr, code, err := c.run(ctx, c.bin, c.args("daemon-reload")...)
	return translateExit("daemon-reload", code, stderr, err)
}

// ResetFailed clears a unit's failed state and its start-limit counter.
func (c *ExecController) ResetFailed(ctx context.Context, unit string) error {
	_, stderr, code, err := c.run(ctx, c.bin, c.args("reset-failed", unit)...)
	return translateExit(unit, code, stderr, err)
}

// showProperties are the properties Props reads, in one invocation.
var showProperties = []string{
	"Id",
	"ActiveState",
	"SubState",
	"MainPID",
	"ExecMainStatus",
	"Result",
	"NRestarts",
	"MemoryCurrent",
	"ExecMainExitTimestamp",
	"LoadState",
}

// Props reads the unit properties with `systemctl show`.
//
// `--timestamp=unix` is what makes the exit timestamp parseable at all: without
// it systemctl prints a localized, human-formatted date, which is precisely the
// output-parsing hazard section 5.3 lists as a reason to prefer D-Bus. On a
// systemd too old to know the option the value simply fails to parse and the
// timestamp stays zero, which is honest — the field is unknown, not 1970.
func (c *ExecController) Props(ctx context.Context, unit string) (UnitProps, error) {
	args := c.args(append([]string{"--timestamp=unix", "show", unit},
		propertyArgs(showProperties)...)...)
	stdout, stderr, code, err := c.run(ctx, c.bin, args...)
	if err := translateExit(unit, code, stderr, err); err != nil {
		return UnitProps{}, err
	}

	blocks := parseShow(stdout)
	if len(blocks) == 0 {
		return UnitProps{}, fmt.Errorf("%w: %s", ErrNoSuchUnit, unit)
	}
	kv := blocks[0]

	// `systemctl show` answers for a unit it has never heard of with
	// LoadState=not-found and exit 0, so the exit status alone cannot
	// distinguish "stopped" from "does not exist".
	if kv["LoadState"] == "not-found" {
		return UnitProps{}, fmt.Errorf("%w: %s", ErrNoSuchUnit, unit)
	}
	return propsFromShow(kv), nil
}

func propertyArgs(props []string) []string {
	out := make([]string, 0, len(props))
	for _, p := range props {
		out = append(out, "--property="+p)
	}
	return out
}

// parseShow turns `systemctl show` output into one map per unit. Blocks are
// separated by a blank line when a pattern matched several units.
func parseShow(out string) []map[string]string {
	var blocks []map[string]string
	cur := map[string]string{}
	flush := func() {
		if len(cur) > 0 {
			blocks = append(blocks, cur)
			cur = map[string]string{}
		}
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		cur[k] = v
	}
	flush()
	return blocks
}

func propsFromShow(kv map[string]string) UnitProps {
	p := UnitProps{
		ActiveState: kv["ActiveState"],
		SubState:    kv["SubState"],
		Result:      kv["Result"],
	}
	if n, err := strconv.ParseUint(kv["MainPID"], 10, 32); err == nil {
		p.MainPID = uint32(n)
	}
	if n, err := strconv.ParseInt(kv["ExecMainStatus"], 10, 32); err == nil {
		p.ExecMainStatus = int32(n)
	}
	if n, err := strconv.ParseUint(kv["NRestarts"], 10, 32); err == nil {
		p.NRestarts = uint32(n)
	}
	// "[not set]" and the empty string both mean "no accounting figure", which
	// is zero here rather than a parse failure worth reporting.
	if n, err := strconv.ParseUint(kv["MemoryCurrent"], 10, 64); err == nil {
		p.MemoryCurrent = n
	}
	p.ExecMainExitTimestamp = parseUnixTimestamp(kv["ExecMainExitTimestamp"])
	return p
}

// parseUnixTimestamp reads the `@<seconds>` form `--timestamp=unix` produces.
// An empty value means the event never happened.
func parseUnixTimestamp(v string) time.Time {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, "@") {
		return time.Time{}
	}
	secs, err := strconv.ParseInt(v[1:], 10, 64)
	if err != nil || secs <= 0 {
		return time.Time{}
	}
	return time.Unix(secs, 0).UTC()
}

// SubscribeSubState polls `systemctl show` and emits a change per transition.
//
// It is a poll because there is nothing to push: the exec controller exists
// precisely for hosts where the bus is unusable. `GET /system/info` reports the
// winner so the UI can say status updates are polled rather than pushed.
func (c *ExecController) SubscribeSubState(ctx context.Context, pattern string) (<-chan SubStateEvent, error) {
	if _, err := path.Match(pattern, "probe"); err != nil {
		return nil, fmt.Errorf("systemd: bad unit pattern %q: %w", pattern, err)
	}

	out := make(chan SubStateEvent, 64)
	go func() {
		defer close(out)
		ticker := time.NewTicker(c.interval)
		defer ticker.Stop()

		last := map[string]string{}
		first := true
		for {
			seen, err := c.pollSubStates(ctx, pattern)
			if err != nil && ctx.Err() == nil {
				c.log.Warn("systemd poll failed", "pattern", pattern, "error", err)
			}
			if err == nil {
				// The first pass establishes the baseline rather than
				// reporting every already-running unit as a transition.
				for unit, sub := range seen {
					if prev, ok := last[unit]; !first && (!ok || prev != sub) {
						if !emit(ctx, out, SubStateEvent{Unit: unit, SubState: sub}) {
							return
						}
					}
				}
				// A unit that vanished was garbage-collected after going
				// inactive; `dead` is what its sub-state would have read.
				for unit := range last {
					if _, ok := seen[unit]; !ok && !first {
						if !emit(ctx, out, SubStateEvent{Unit: unit, SubState: "dead"}) {
							return
						}
					}
				}
				last = seen
				first = false
			}

			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return out, nil
}

// emit delivers one event, blocking until the consumer takes it. It reports
// false when ctx ended first, which is the poller's signal to stop.
func emit(ctx context.Context, out chan<- SubStateEvent, ev SubStateEvent) bool {
	select {
	case out <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}

// pollSubStates returns unit → sub-state for every loaded unit matching pattern.
func (c *ExecController) pollSubStates(ctx context.Context, pattern string) (map[string]string, error) {
	args := c.args(append([]string{"show", pattern},
		propertyArgs([]string{"Id", "SubState", "LoadState"})...)...)
	stdout, stderr, code, err := c.run(ctx, c.bin, args...)
	if err := translateExit(pattern, code, stderr, err); err != nil {
		return nil, err
	}

	out := map[string]string{}
	for _, kv := range parseShow(stdout) {
		id := kv["Id"]
		if id == "" || kv["LoadState"] == "not-found" {
			continue
		}
		// `systemctl show` expands the pattern itself, but it is not the same
		// matcher, so the answer is filtered against the caller's pattern too.
		if ok, _ := path.Match(pattern, id); !ok {
			continue
		}
		out[id] = kv["SubState"]
	}
	return out, nil
}
