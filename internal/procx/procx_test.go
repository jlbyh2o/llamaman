package procx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The tests drive a child process, and the child is this test binary re-invoked
// with PROCX_CHILD set — the os/exec convention. A shell script would have been
// shorter and would have made every case depend on which /bin/sh the host
// ships; a Go child can ignore SIGTERM, fork a grandchild into the same group
// and print a megabyte-long line, which is precisely the surface under test.
func TestMain(m *testing.M) {
	if mode := os.Getenv("PROCX_CHILD"); mode != "" {
		os.Exit(childMain(mode))
	}
	os.Exit(m.Run())
}

func childMain(mode string) int {
	switch mode {
	case "quiet":
		return intEnv("PROCX_EXIT", 0)

	case "talk":
		// Alternates the two streams so a merged reader has something to
		// interleave, and flushes by writing to unbuffered os.Stdout/os.Stderr.
		for i := range 3 {
			fmt.Fprintf(os.Stdout, "out %d\n", i)
			fmt.Fprintf(os.Stderr, "err %d\n", i)
		}
		return intEnv("PROCX_EXIT", 0)

	case "partial":
		fmt.Fprint(os.Stdout, "no trailing newline")
		return 0

	case "crlf":
		fmt.Fprint(os.Stdout, "carriage\r\nreturn\r\n")
		return 0

	case "longline":
		fmt.Fprintln(os.Stdout, strings.Repeat("x", intEnv("PROCX_LINE", 100)))
		fmt.Fprintln(os.Stdout, "after")
		return 0

	case "env":
		fmt.Fprintln(os.Stdout, os.Getenv("PROCX_MARKER"))
		wd, _ := os.Getwd()
		fmt.Fprintln(os.Stdout, wd)
		return 0

	case "sleep":
		// Dies on the default SIGTERM disposition.
		time.Sleep(time.Minute)
		return 0

	case "ignore-term":
		signal.Ignore(syscall.SIGTERM)
		fmt.Fprintln(os.Stdout, "ready")
		time.Sleep(time.Minute)
		return 0

	case "spawn":
		// A grandchild in the SAME process group that ignores SIGTERM and
		// inherits our stdout. The parent exits immediately, so the only thing
		// that can end the run is a signal delivered to the group.
		child := exec.Command(os.Args[0])
		child.Env = append(os.Environ(), "PROCX_CHILD=ignore-term")
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			return 1
		}
		fmt.Fprintln(os.Stdout, "spawned")
		return 0

	case "kill-self":
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		time.Sleep(time.Minute)
		return 0
	}
	fmt.Fprintf(os.Stderr, "unknown child mode %q\n", mode)
	return 2
}

func intEnv(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

// child builds a Cmd that re-runs this test binary in the named mode.
func child(mode string, env ...string) Cmd {
	return Cmd{
		Path:     os.Args[0],
		ExtraEnv: append([]string{"PROCX_CHILD=" + mode}, env...),
	}
}

type collector struct {
	mu    sync.Mutex
	lines []Line
}

func (c *collector) add(l Line) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, l)
}

func (c *collector) texts() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.lines))
	for i, l := range c.lines {
		out[i] = l.Text
	}
	return out
}

func (c *collector) all() []Line {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Line(nil), c.lines...)
}

func TestRunExitStatus(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		code int
	}{
		{"clean", 0},
		{"failure", 1},
		{"launcher code", 78},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			res, err := Run(t.Context(), child("quiet", "PROCX_EXIT="+strconv.Itoa(tc.code)))
			if res.ExitCode != tc.code {
				t.Fatalf("exit code = %d, want %d (err %v)", res.ExitCode, tc.code, err)
			}
			if res.PID == 0 {
				t.Error("Result.PID = 0, want the child's pid")
			}
			if tc.code == 0 {
				if err != nil {
					t.Fatalf("Run: %v", err)
				}
				if !res.OK() {
					t.Error("OK() = false for a clean exit")
				}
				return
			}
			var exitErr *ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("err = %v, want *ExitError", err)
			}
			if exitErr.Result.ExitCode != tc.code {
				t.Errorf("ExitError carries exit %d, want %d", exitErr.Result.ExitCode, tc.code)
			}
			if !strings.Contains(exitErr.Error(), strconv.Itoa(tc.code)) {
				t.Errorf("message %q does not name the status", exitErr.Error())
			}
		})
	}
}

func TestRunMergesBothStreams(t *testing.T) {
	t.Parallel()

	var c collector
	cmd := child("talk")
	cmd.OnLine = c.add
	if _, err := Run(t.Context(), cmd); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var out, errs int
	for _, l := range c.all() {
		switch l.Stream {
		case StreamStdout:
			out++
			if !strings.HasPrefix(l.Text, "out ") {
				t.Errorf("stdout line %q", l.Text)
			}
		case StreamStderr:
			errs++
			if !strings.HasPrefix(l.Text, "err ") {
				t.Errorf("stderr line %q", l.Text)
			}
		}
		if l.At.IsZero() {
			t.Error("line carries no timestamp")
		}
	}
	if out != 3 || errs != 3 {
		t.Fatalf("got %d stdout and %d stderr lines, want 3 and 3: %q", out, errs, c.texts())
	}
}

func TestRunLineShapes(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		mode  string
		env   []string
		max   int
		want  []string
		trunc []bool
	}{
		{
			name: "unterminated final line is still delivered",
			mode: "partial",
			want: []string{"no trailing newline"},
		},
		{
			name: "carriage returns are stripped",
			mode: "crlf",
			want: []string{"carriage", "return"},
		},
		{
			name:  "an over-long line is cut and marked, and the next line survives",
			mode:  "longline",
			env:   []string{"PROCX_LINE=5000"},
			max:   16,
			want:  []string{strings.Repeat("x", 16), "after"},
			trunc: []bool{true, false},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var c collector
			cmd := child(tc.mode, tc.env...)
			cmd.MaxLineBytes = tc.max
			cmd.OnLine = c.add
			if _, err := Run(t.Context(), cmd); err != nil {
				t.Fatalf("Run: %v", err)
			}
			got := c.texts()
			if len(got) != len(tc.want) {
				t.Fatalf("lines = %q, want %q", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("line %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
			for i, want := range tc.trunc {
				if got := c.all()[i].Truncated; got != want {
					t.Errorf("line %d Truncated = %v, want %v", i, got, want)
				}
			}
		})
	}
}

func TestRunEnvironmentAndDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	var c collector
	cmd := child("env", "PROCX_MARKER=hello")
	cmd.Dir = dir
	cmd.OnLine = c.add
	if _, err := Run(t.Context(), cmd); err != nil {
		t.Fatalf("Run: %v", err)
	}
	lines := c.texts()
	if len(lines) != 2 {
		t.Fatalf("lines = %q", lines)
	}
	if lines[0] != "hello" {
		t.Errorf("ExtraEnv did not reach the child: %q", lines[0])
	}
	// macOS symlinks /tmp; the daemon is Linux-only but the comparison should
	// still not be fooled by one.
	if want, err := os.Readlink(dir); err == nil && lines[1] != want && lines[1] != dir {
		t.Errorf("working directory = %q, want %q", lines[1], dir)
	}
}

func TestRunCancelSendsSIGTERM(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	res, err := Run(ctx, child("sleep"))
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("took %s: the child was not signaled", elapsed)
	}
	if !res.Terminated {
		t.Error("Result.Terminated = false, want true")
	}
	if res.Killed {
		t.Error("Result.Killed = true: SIGTERM should have been enough")
	}
	if res.Signal != syscall.SIGTERM {
		t.Errorf("Signal = %v, want SIGTERM", res.Signal)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to unwrap to context.Canceled", err)
	}
	if res.OOMKilled() {
		t.Error("OOMKilled() = true for a cancellation we performed")
	}
}

func TestRunEscalatesToSIGKILL(t *testing.T) {
	t.Parallel()

	ready := make(chan struct{})
	var once sync.Once
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	cmd := child("ignore-term")
	cmd.Grace = 200 * time.Millisecond
	cmd.OnLine = func(l Line) {
		if l.Text == "ready" {
			once.Do(func() { close(ready) })
		}
	}

	go func() {
		<-ready
		cancel()
	}()

	res, err := Run(ctx, cmd)
	if !res.Killed {
		t.Fatalf("Result.Killed = false: a child ignoring SIGTERM must be SIGKILLed (%+v, err %v)", res, err)
	}
	if res.Signal != syscall.SIGKILL {
		t.Errorf("Signal = %v, want SIGKILL", res.Signal)
	}
	if res.OOMKilled() {
		t.Error("OOMKilled() = true for a SIGKILL we sent ourselves — D20's retry would fire on a cancellation")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to unwrap to context.Canceled", err)
	}
}

// A grandchild that outlives its parent and holds the inherited pipe is the
// case that makes signaling the GROUP rather than the process load-bearing: the
// leader is already gone by the time the context ends, so a kill(pid) would
// reach nothing and Run would block on a pipe nobody will close.
func TestRunSignalsTheWholeGroup(t *testing.T) {
	t.Parallel()

	spawned := make(chan struct{})
	var once sync.Once
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	cmd := child("spawn")
	cmd.Grace = 200 * time.Millisecond
	cmd.OnLine = func(l Line) {
		if l.Text == "ready" {
			once.Do(func() { close(spawned) })
		}
	}

	go func() {
		select {
		case <-spawned:
		case <-ctx.Done():
		}
		cancel()
	}()

	done := make(chan Result, 1)
	go func() {
		res, _ := Run(ctx, cmd)
		done <- res
	}()

	select {
	case res := <-done:
		if !res.Terminated {
			t.Error("Result.Terminated = false")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Run never returned: the grandchild kept the pipe open, so the group was not signaled")
	}
}

func TestRunReportsAnExternalSignal(t *testing.T) {
	t.Parallel()

	res, err := Run(t.Context(), child("kill-self"))
	if res.Signal != syscall.SIGKILL {
		t.Fatalf("Signal = %v, want SIGKILL (err %v)", res.Signal, err)
	}
	if !res.OOMKilled() {
		t.Error("OOMKilled() = false: a SIGKILL this package did not send is the D20 shape")
	}
	if !res.SignaledExternally() {
		t.Error("SignaledExternally() = false")
	}
	if err == nil {
		t.Error("err = nil for a signaled child")
	}
}

func TestCapture(t *testing.T) {
	t.Parallel()

	var c collector
	cmd := child("talk")
	cmd.OnLine = c.add
	out, res, err := Capture(t.Context(), cmd)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if !res.OK() {
		t.Fatalf("result = %+v", res)
	}
	for _, want := range []string{"out 0\n", "err 2\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("captured output %q missing %q", out, want)
		}
	}
	if len(c.texts()) != 6 {
		t.Errorf("Capture dropped the caller's own OnLine: %q", c.texts())
	}
}

func TestRunRejectsBadInput(t *testing.T) {
	t.Parallel()

	if _, err := Run(t.Context(), Cmd{}); err == nil {
		t.Error("empty Path: err = nil")
	}
	if _, err := Run(t.Context(), Cmd{Path: "llamaman-no-such-binary-xyz"}); err == nil {
		t.Error("unresolvable Path: err = nil")
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Run(ctx, child("quiet")); !errors.Is(err, context.Canceled) {
		t.Errorf("already-canceled context: err = %v", err)
	}
}
