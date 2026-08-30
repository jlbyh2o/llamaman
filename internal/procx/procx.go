package procx

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Stream names which of a child's two output streams a Line arrived on. Both
// are merged into one callback in arrival order (DESIGN section 6.5's "merges
// stdout and stderr line by line"), and this field is what lets a consumer that
// cares tell them apart afterwards.
type Stream uint8

const (
	// StreamStdout is the child's standard output.
	StreamStdout Stream = iota
	// StreamStderr is the child's standard error. cmake, ninja and the
	// compilers put most of what matters here, which is why a build log that
	// captured only stdout would be nearly empty exactly when it is needed.
	StreamStderr
)

// String renders the stream as "stdout" or "stderr".
func (s Stream) String() string {
	if s == StreamStderr {
		return "stderr"
	}
	return "stdout"
}

// Line is one line of a child's merged output, without its terminator.
type Line struct {
	// At is when the line was completed, from Cmd.Now.
	At time.Time
	// Stream is the pipe it arrived on.
	Stream Stream
	// Text is the line with its trailing "\n" and any "\r" before it removed.
	Text string
	// Truncated reports that the line was longer than Cmd.MaxLineBytes and the
	// remainder was dropped. A single pathological line — a compiler dumping a
	// template expansion, a linker echoing a 400 KB command — must not be able
	// to grow the daemon's memory without bound, and a consumer that wants to
	// say "line truncated" in the log viewer needs to know it happened.
	Truncated bool
}

const (
	// DefaultGrace is how long SIGTERM has to work before SIGKILL follows. It is
	// the ten seconds DESIGN section 6.5's cancellation rule names.
	DefaultGrace = 10 * time.Second

	// DefaultMaxLineBytes caps one line at 1 MiB.
	DefaultMaxLineBytes = 1 << 20
)

// Cmd describes one child process. It is deliberately not an *exec.Cmd: the
// only shapes this project ever needs are "run it, stream its output, and make
// sure it is gone when the context ends", and a struct that cannot express
// anything else cannot be misused into leaving a compiler running after the job
// that started it was canceled.
type Cmd struct {
	// Path is the program. A name with no separator is resolved through PATH.
	Path string

	// Args are the arguments after argv[0].
	Args []string

	// Dir is the working directory; empty means the parent's.
	Dir string

	// Env replaces the environment entirely when non-nil; nil inherits the
	// parent's.
	Env []string

	// ExtraEnv is appended to whichever of the two Env resolves to, which is the
	// ordinary way to add one variable without rebuilding the environment.
	ExtraEnv []string

	// Stdin is the child's standard input; nil gives it /dev/null.
	Stdin io.Reader

	// Grace is how long the child has to exit after SIGTERM before it is
	// SIGKILLed. Zero means DefaultGrace.
	Grace time.Duration

	// MaxLineBytes caps one line. Zero means DefaultMaxLineBytes.
	MaxLineBytes int

	// OnLine receives every line of merged output, in arrival order, from one
	// goroutine at a time — the callback is serialized, so an implementation
	// needs no lock of its own. It runs on the reader goroutines, so a slow
	// callback slows the child down rather than dropping its output; a consumer
	// that must not block should hand the line to a buffered channel.
	OnLine func(Line)

	// Now is the clock stamped onto each Line. Zero means time.Now.
	Now func() time.Time
}

// Result is how a child ended.
type Result struct {
	// PID is the child's pid, which is also its process-group id: every child
	// is started in its own group so that signaling -pid reaches the whole tree
	// (a cmake that started a ninja that started thirty compilers).
	PID int

	// ExitCode is the child's status, or -1 when it died on a signal.
	ExitCode int

	// Signal is the signal that killed it, or 0 when it exited normally.
	Signal syscall.Signal

	// Terminated reports that WE sent SIGTERM because the context ended.
	Terminated bool

	// Killed reports that SIGTERM did not work within Grace and WE sent
	// SIGKILL. Together with Terminated it is what separates "the OOM killer
	// took this compiler" from "we canceled this build" — a distinction D20's
	// automatic -j1 retry depends on entirely.
	Killed bool

	// Started and Finished bound the run.
	Started  time.Time
	Finished time.Time
}

// Duration is how long the child ran.
func (r Result) Duration() time.Duration { return r.Finished.Sub(r.Started) }

// OK reports a clean exit: status 0 and no signal.
func (r Result) OK() bool { return r.ExitCode == 0 && r.Signal == 0 }

// SignaledExternally reports a child killed by a signal this package did not
// send — something else on the host reached in.
func (r Result) SignaledExternally() bool {
	return r.Signal != 0 && !r.Terminated && !r.Killed
}

// OOMKilled reports the specific shape D20 acts on: SIGKILL from outside this
// process. It is only half of the test the design states — the other half is a
// matching `oom-kill` line in the kernel log — because SIGKILL alone is also
// what a human's `kill -9` looks like.
func (r Result) OOMKilled() bool {
	return r.Signal == syscall.SIGKILL && !r.Terminated && !r.Killed
}

// ExitError is the error Run returns for a child that did not exit 0, for one
// killed by a signal, and for one whose context ended. The Result is carried so
// a caller can branch on the exit code without a second call.
type ExitError struct {
	// Cmd is the command line, for the message only.
	Cmd string
	// Result is how the child ended.
	Result Result

	cause error
}

// Error renders the exit status, the signal, or the cancellation.
func (e *ExitError) Error() string {
	switch {
	case e.Result.Killed:
		return fmt.Sprintf("procx: %s: killed after SIGTERM was ignored", e.Cmd)
	case e.Result.Terminated:
		return fmt.Sprintf("procx: %s: terminated", e.Cmd)
	case e.Result.Signal != 0:
		return fmt.Sprintf("procx: %s: killed by %s", e.Cmd, e.Result.Signal)
	default:
		return fmt.Sprintf("procx: %s: exit status %d", e.Cmd, e.Result.ExitCode)
	}
}

// Unwrap returns the context's error when the run ended because the context
// did, so errors.Is(err, context.Canceled) answers "was this canceled" without
// the caller inspecting the Result.
func (e *ExitError) Unwrap() error { return e.cause }

// Run starts the command, streams its merged output to Cmd.OnLine, and waits
// for it to finish.
//
// Cancellation is the escalation DESIGN section 1 names: when ctx ends, the
// child's whole process GROUP is sent SIGTERM, and if it has not exited within
// Grace the group is sent SIGKILL. The group rather than the process is the
// point — a build is cmake → ninja → N compilers, and signaling only the leader
// leaves the compilers running and the pipes they inherited open.
//
// A non-zero exit is an *ExitError, not a nil error with a Result the caller
// might forget to read; the Result is returned alongside it either way.
func Run(ctx context.Context, c Cmd) (Result, error) {
	if c.Path == "" {
		return Result{}, errors.New("procx: Cmd.Path is empty")
	}
	if err := ctx.Err(); err != nil {
		return Result{}, fmt.Errorf("procx: %s: %w", c.Path, err)
	}

	now := c.Now
	if now == nil {
		now = time.Now
	}
	grace := c.Grace
	if grace <= 0 {
		grace = DefaultGrace
	}
	maxLine := c.MaxLineBytes
	if maxLine <= 0 {
		maxLine = DefaultMaxLineBytes
	}

	path := c.Path
	if !strings.ContainsRune(path, os.PathSeparator) {
		p, err := exec.LookPath(path)
		if err != nil {
			return Result{}, fmt.Errorf("procx: %s: %w", c.Path, err)
		}
		path = p
	}

	cmd := exec.Command(path, c.Args...)
	cmd.Dir = c.Dir
	cmd.Stdin = c.Stdin
	env := c.Env
	if env == nil {
		env = os.Environ()
	}
	if len(c.ExtraEnv) > 0 {
		env = append(append(make([]string, 0, len(env)+len(c.ExtraEnv)), env...), c.ExtraEnv...)
	}
	cmd.Env = env
	// Setpgid is what makes the child a group leader, so kill(-pid) below
	// reaches every descendant it did not reparent away.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, fmt.Errorf("procx: %s: stdout pipe: %w", c.Path, err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return Result{}, fmt.Errorf("procx: %s: stderr pipe: %w", c.Path, err)
	}

	started := now()
	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("procx: %s: %w", c.Path, err)
	}
	pid := cmd.Process.Pid

	var mu sync.Mutex
	emit := func(l Line) {
		if c.OnLine == nil {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		c.OnLine(l)
	}

	var readers sync.WaitGroup
	readers.Add(2)
	go func() {
		defer readers.Done()
		scanLines(stdout, StreamStdout, maxLine, now, emit)
	}()
	go func() {
		defer readers.Done()
		scanLines(stderr, StreamStderr, maxLine, now, emit)
	}()

	var terminated, killed atomic.Bool
	done := make(chan struct{})
	var killer sync.WaitGroup
	killer.Add(1)
	go func() {
		defer killer.Done()
		select {
		case <-done:
			return
		case <-ctx.Done():
		}
		terminated.Store(true)
		signalGroup(pid, syscall.SIGTERM)
		timer := time.NewTimer(grace)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
			killed.Store(true)
			signalGroup(pid, syscall.SIGKILL)
		}
	}()

	// Both pipes must be drained before Wait, which closes them (os/exec's
	// documented contract for StdoutPipe). A grandchild that outlives its parent
	// and holds the pipe open keeps this waiting — which is exactly the case the
	// group signal above exists to end.
	readers.Wait()
	waitErr := cmd.Wait()
	close(done)
	killer.Wait()

	res := Result{
		PID:        pid,
		Started:    started,
		Finished:   now(),
		Terminated: terminated.Load(),
		Killed:     killed.Load(),
	}
	if ps := cmd.ProcessState; ps != nil {
		res.ExitCode = ps.ExitCode()
		if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			res.Signal = ws.Signal()
		}
	} else if waitErr != nil {
		res.ExitCode = -1
	}

	line := path
	if len(c.Args) > 0 {
		line += " " + strings.Join(c.Args, " ")
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return res, &ExitError{Cmd: line, Result: res, cause: ctxErr}
	}
	if !res.OK() {
		return res, &ExitError{Cmd: line, Result: res}
	}
	return res, nil
}

// Capture runs the command and returns its merged output as one string, with a
// trailing newline after every line. It is the shape every probe in the build
// pipeline wants — `cmake --version`, `git rev-parse`, `llama-server
// --list-devices` — and it still calls Cmd.OnLine when one is set, so a probe's
// output can go to the build log at the same time.
func Capture(ctx context.Context, c Cmd) (string, Result, error) {
	var buf strings.Builder
	inner := c.OnLine
	c.OnLine = func(l Line) {
		buf.WriteString(l.Text)
		buf.WriteByte('\n')
		if inner != nil {
			inner(l)
		}
	}
	res, err := Run(ctx, c)
	return buf.String(), res, err
}

// signalGroup signals the whole process group, falling back to the leader alone
// if the group is gone (which happens when the child has already been reaped by
// the time the escalation fires).
func signalGroup(pid int, sig syscall.Signal) {
	if err := syscall.Kill(-pid, sig); err != nil {
		_ = syscall.Kill(pid, sig)
	}
}

// scanLines splits r into lines and hands them to emit. It reads with an
// explicit cap rather than a bufio.Scanner because a Scanner's answer to a line
// longer than its buffer is to stop scanning entirely — a compiler that emits
// one enormous line would silently truncate the REST of the build log, which is
// the one failure mode a build log must not have.
func scanLines(r io.Reader, stream Stream, max int, now func() time.Time, emit func(Line)) {
	br := bufio.NewReaderSize(r, 64*1024)
	var (
		buf       []byte
		truncated bool
	)
	flush := func() {
		text := strings.TrimSuffix(string(buf), "\r")
		emit(Line{At: now(), Stream: stream, Text: text, Truncated: truncated})
		buf = buf[:0]
		truncated = false
	}
	for {
		chunk, err := br.ReadSlice('\n')
		complete := err == nil
		if complete {
			chunk = chunk[:len(chunk)-1]
		}
		if len(chunk) > 0 {
			switch room := max - len(buf); {
			case room <= 0:
				truncated = true
			case len(chunk) > room:
				buf = append(buf, chunk[:room]...)
				truncated = true
			default:
				buf = append(buf, chunk...)
			}
		}
		switch {
		case complete:
			flush()
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		default:
			// io.EOF, or a read error on a pipe whose writer was killed. A
			// final unterminated line is still a line worth keeping: it is
			// usually the message that explains the death.
			if len(buf) > 0 || truncated {
				flush()
			}
			return
		}
	}
}
