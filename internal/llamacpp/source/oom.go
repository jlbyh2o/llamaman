package source

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/jlbyh2o/llamaman/internal/procx"
	"golang.org/x/sys/unix"
)

// oomOutputPatterns are the shapes an OOM kill takes in a build's OUTPUT,
// lowercased.
//
// D20's test is "signal 9 on a child plus a matching oom-kill line in the
// kernel log", and the first half of it needs care: the process this daemon
// waits on is `cmake`, which is not the process the kernel kills. The kernel
// kills `cc1plus` or `nvcc`, several levels down; ninja and the compiler driver
// then REPORT that death and exit 1 themselves. So the honest reading of "signal
// 9 on a child" includes a child of the child, and these are the strings that
// say so.
var oomOutputPatterns = []string{
	"killed signal terminated program", // gcc driver: "fatal error: Killed signal terminated program cc1plus"
	"signal terminated program",
	"subcommand killed by signal 9",
	"terminated with signal 9",
	"internal compiler error: killed",
	"out of memory allocating", // cc1plus: "out of memory allocating N bytes"
	"virtual memory exhausted",
	"cannot allocate memory",
	"g++: fatal error: killed",
	"c++: fatal error: killed",
}

// OOMSuspicion is what the compile phase concluded about a failure.
type OOMSuspicion struct {
	// Suspected is true when the failure looks like an OOM kill.
	Suspected bool
	// Reason names the evidence, for the log and for the UI's "why is this
	// build suddenly at -j1" line.
	Reason string
	// KernelConfirmed reports that the kernel log corroborated it. When the
	// kernel log could not be read at all, this is false and Reason says so —
	// the retry still happens, because a needlessly slow build is a far better
	// outcome than a needless failure, and the design requires the reason to be
	// shown either way.
	KernelConfirmed bool
}

// isOOMLine reports whether one line of build output looks like a report of a
// process the kernel killed.
func isOOMLine(text string) bool {
	low := strings.ToLower(text)
	for _, p := range oomOutputPatterns {
		if strings.Contains(low, p) {
			return true
		}
	}
	return false
}

// suspectOOM applies D20's test to one failed compile: the signal the child
// died on, the line the build printed, and the kernel's own log.
func suspectOOM(res procx.Result, oomLine string, kernel kernelVerdict) OOMSuspicion {
	var evidence []string
	if res.OOMKilled() {
		evidence = append(evidence, "the compiler process was killed by SIGKILL")
	}
	if oomLine != "" {
		evidence = append(evidence, fmt.Sprintf("the build reported %q", oomLine))
	}
	if len(evidence) == 0 {
		return OOMSuspicion{}
	}

	s := OOMSuspicion{Suspected: true, KernelConfirmed: kernel.confirmed}
	switch {
	case kernel.confirmed:
		evidence = append(evidence, "the kernel log carries a matching oom-kill line")
	case kernel.err != nil:
		evidence = append(evidence, fmt.Sprintf("the kernel log could not be read (%v), so this is inferred from the build output alone", kernel.err))
	default:
		evidence = append(evidence, "no matching oom-kill line was found in the kernel log, so this is inferred from the build output alone")
	}
	s.Reason = strings.Join(evidence, "; ")
	return s
}

type kernelVerdict struct {
	confirmed bool
	err       error
}

// OOMWatcher answers the second half of D20's test: did the kernel record an
// OOM kill since the compile started?
//
// It is an interface because the answer is not always available. /dev/kmsg is
// world-readable on most distributions and root-only on some, and the daemon
// runs unprivileged by design — so "I cannot tell" is a supported answer here,
// not a failure.
type OOMWatcher interface {
	OOMKillSince(ctx context.Context, since time.Time) (bool, error)
}

// KmsgWatcher reads the kernel ring buffer directly.
//
// It does NOT shell out to journalctl, and that is not a preference: D49's
// second invariant reserves every journalctl invocation to internal/systemd,
// and the kernel ring buffer is readable without it.
type KmsgWatcher struct {
	// Path is the device; empty means /dev/kmsg.
	Path string
	// Open overrides how the device is read (tests supply a fixture).
	Open func() (io.ReadCloser, error)
	// BootTime resolves the instant kmsg's monotonic timestamps count from;
	// empty means "now minus the kernel's uptime".
	BootTime func() (time.Time, error)
}

// OOMKillSince reports whether the kernel logged an OOM kill at or after since.
func (w KmsgWatcher) OOMKillSince(ctx context.Context, since time.Time) (bool, error) {
	open := w.Open
	if open == nil {
		path := w.Path
		if path == "" {
			path = "/dev/kmsg"
		}
		open = func() (io.ReadCloser, error) { return openKmsg(path) }
	}
	bootAt := w.BootTime
	if bootAt == nil {
		bootAt = bootTime
	}

	boot, err := bootAt()
	if err != nil {
		return false, err
	}
	r, err := open()
	if err != nil {
		return false, err
	}
	defer r.Close()

	return scanKmsg(r, boot, since)
}

// kmsgOOMPatterns are the kernel's own words, lowercased.
var kmsgOOMPatterns = []string{
	"oom-kill:",
	"out of memory: killed process",
	"memory cgroup out of memory",
}

// scanKmsg parses the ring buffer's records. The format is
// "<prio>,<seq>,<usec-since-boot>,<flags>;<message>", one record per line, with
// continuation lines indented — which is why anything not matching the prefix is
// skipped rather than treated as a record.
func scanKmsg(r io.Reader, boot, since time.Time) (bool, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 16*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		prefix, msg, ok := strings.Cut(line, ";")
		if !ok {
			continue
		}
		fields := strings.Split(prefix, ",")
		if len(fields) < 3 {
			continue
		}
		usec, err := strconv.ParseInt(strings.TrimSpace(fields[2]), 10, 64)
		if err != nil {
			continue
		}
		at := boot.Add(time.Duration(usec) * time.Microsecond)
		if at.Before(since) {
			continue
		}
		low := strings.ToLower(msg)
		for _, p := range kmsgOOMPatterns {
			if strings.Contains(low, p) {
				return true, nil
			}
		}
	}
	if err := sc.Err(); err != nil {
		return false, fmt.Errorf("source: read kernel log: %w", err)
	}
	return false, nil
}

// openKmsg opens the ring buffer with raw syscalls rather than os.OpenFile.
//
// The reason is specific: /dev/kmsg opened non-blocking returns EAGAIN once the
// buffer is drained, and an *os.File hands that to the runtime poller, which
// then WAITS for the next kernel message — turning "read what is there" into
// "block until something happens". A raw fd has no poller behind it, so EAGAIN
// simply means "that is all there is".
func openKmsg(path string) (io.ReadCloser, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("source: open %s: %w", path, err)
	}
	return &kmsgReader{fd: fd}, nil
}

type kmsgReader struct{ fd int }

// Read returns one record per call, or io.EOF once the buffer is drained.
func (k *kmsgReader) Read(p []byte) (int, error) {
	for {
		n, err := unix.Read(k.fd, p)
		switch {
		case err == nil:
			// Records carry no trailing newline; the scanner needs one.
			if n < len(p) && (n == 0 || p[n-1] != '\n') {
				p[n] = '\n'
				n++
			}
			return n, nil
		case err == unix.EAGAIN:
			return 0, io.EOF
		case err == unix.EPIPE:
			// Records were overwritten while we read; the next read resumes.
			continue
		case err == unix.EINTR:
			continue
		default:
			return 0, err
		}
	}
}

func (k *kmsgReader) Close() error { return unix.Close(k.fd) }

// bootTime is now minus the kernel's uptime — the instant kmsg's timestamps
// count from.
func bootTime() (time.Time, error) {
	var si unix.Sysinfo_t
	if err := unix.Sysinfo(&si); err != nil {
		return time.Time{}, fmt.Errorf("source: sysinfo: %w", err)
	}
	return time.Now().Add(-time.Duration(si.Uptime) * time.Second), nil
}
