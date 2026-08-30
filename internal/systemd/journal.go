package systemd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// Journal reading is a `journalctl -o json [--follow]` subprocess (D6).
//
// sdjournal is deliberately not used: it requires cgo, and cgo forfeits the
// single static binary this project is built around. The cost is one subprocess
// per active log viewer, killed with its context, and a JSON dialect with two
// awkward corners — every field is a string, and a field can also be an array
// (of strings when the entry carried the field twice, of numbers when the value
// was not valid UTF-8).

// ErrJournalUnavailable means journalctl is absent or could not be run at all.
var ErrJournalUnavailable = errors.New("systemd: journalctl unavailable")

// Entry is one journal record, reduced to the fields this design reads.
type Entry struct {
	// Cursor is journald's opaque position token, which is what a log viewer
	// resumes from rather than a timestamp.
	Cursor string
	// Realtime is __REALTIME_TIMESTAMP, microseconds since the epoch.
	Realtime time.Time
	// Unit is _SYSTEMD_UNIT for a system unit, _SYSTEMD_USER_UNIT under a user
	// manager.
	Unit string
	// Identifier is SYSLOG_IDENTIFIER, which the units set explicitly so an
	// instance's lines are attributable without parsing the message.
	Identifier string
	// Priority is the syslog level, 0 (emerg) to 7 (debug).
	Priority int
	// PID is _PID.
	PID int
	// Message is MESSAGE, decoded from either of its two JSON shapes.
	Message string
}

// JournalOptions selects what to read.
type JournalOptions struct {
	// Scope adds --user, which is not optional in the D2 topology: the system
	// journal has none of a user manager's units.
	Scope model.SystemdScope
	// Units are matched with --unit=; empty reads everything the caller may
	// see.
	Units []string
	// Lines is -n. Zero means journalctl's own default.
	Lines int
	// Since is --since, formatted as a unix timestamp so no locale is
	// involved.
	Since time.Time
	// Cursor is --after-cursor, for resuming a viewer.
	Cursor string
	// Path is the journalctl binary. Empty resolves it through PATH once.
	Path string
	// run overrides process execution for tests.
	run journalRunner
}

// journalRunner starts a journalctl and returns its stdout, a wait func and a
// kill func. It exists so the follow path — a long-lived subprocess whose only
// exit is a context cancellation — can be tested without a journal.
type journalRunner func(ctx context.Context, name string, args ...string) (io.ReadCloser, func() error, error)

func (o JournalOptions) binary() (string, error) {
	if o.Path != "" {
		return o.Path, nil
	}
	p, err := exec.LookPath("journalctl")
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrJournalUnavailable, err)
	}
	return p, nil
}

// args builds the argument list. Order is fixed so a test can assert it.
func (o JournalOptions) args(follow bool) []string {
	args := append(scopeArgs(o.Scope), "-o", "json", "--no-pager")
	for _, u := range o.Units {
		args = append(args, "--unit="+u)
	}
	if o.Lines > 0 {
		args = append(args, "-n", strconv.Itoa(o.Lines))
	}
	if !o.Since.IsZero() {
		// @<seconds> is journalctl's locale-free timestamp form; a formatted
		// date would be parsed in the host's locale and time zone.
		args = append(args, "--since=@"+strconv.FormatInt(o.Since.Unix(), 10))
	}
	if o.Cursor != "" {
		args = append(args, "--after-cursor="+o.Cursor)
	}
	if follow {
		args = append(args, "--follow")
	}
	return args
}

// Tail reads a bounded slice of the journal and returns it.
func Tail(ctx context.Context, opts JournalOptions) ([]Entry, error) {
	stdout, wait, err := opts.start(ctx, false)
	if err != nil {
		return nil, err
	}
	defer stdout.Close()

	entries, scanErr := decodeEntries(stdout, nil)
	waitErr := wait()
	if scanErr != nil {
		return entries, scanErr
	}
	if waitErr != nil && ctx.Err() == nil {
		// A non-zero exit with entries already decoded is worth reporting but
		// not worth discarding the entries over: journalctl exits non-zero for
		// several conditions that still produced output.
		if len(entries) == 0 {
			return nil, fmt.Errorf("%w: %w", ErrJournalUnavailable, waitErr)
		}
	}
	return entries, nil
}

// Follow streams entries until ctx is done, then kills the subprocess.
//
// One subprocess per active log viewer (section 5.3). The returned channel is
// closed when the stream ends; a single error, if any, arrives on the error
// channel before that.
func Follow(ctx context.Context, opts JournalOptions) (<-chan Entry, <-chan error, error) {
	stdout, wait, err := opts.start(ctx, true)
	if err != nil {
		return nil, nil, err
	}

	out := make(chan Entry, 64)
	errs := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errs)
		defer stdout.Close()

		_, scanErr := decodeEntries(stdout, func(e Entry) bool {
			select {
			case out <- e:
				return true
			case <-ctx.Done():
				return false
			}
		})
		_ = wait()
		if scanErr != nil && ctx.Err() == nil {
			errs <- scanErr
		}
	}()
	return out, errs, nil
}

func (o JournalOptions) start(ctx context.Context, follow bool) (io.ReadCloser, func() error, error) {
	bin, err := o.binary()
	if err != nil {
		return nil, nil, err
	}
	run := o.run
	if run == nil {
		run = journalExecRunner
	}
	return run(ctx, bin, o.args(follow)...)
}

func journalExecRunner(ctx context.Context, name string, args ...string) (io.ReadCloser, func() error, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	// Cancelling the context kills the subprocess, which is what makes "killed
	// with its context" literally true for a --follow viewer that navigated
	// away.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrJournalUnavailable, err)
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrJournalUnavailable, err)
	}
	return stdout, cmd.Wait, nil
}

// decodeEntries reads one JSON object per line. onEntry, when non-nil, is called
// for each; returning false stops the scan.
func decodeEntries(r io.Reader, onEntry func(Entry) bool) ([]Entry, error) {
	var out []Entry
	sc := bufio.NewScanner(r)
	// journald messages are capped at 2 KB by default but can be far larger
	// when a unit logs a stack trace or llama.cpp dumps a device table, and a
	// truncated line is a JSON parse error rather than a short message.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(line, &raw); err != nil {
			// One unparsable line must not end a stream: journalctl emits
			// non-entry notices (`-- Journal begins at …`) on some versions.
			continue
		}
		e := entryFromRaw(raw)
		if onEntry != nil {
			if !onEntry(e) {
				return out, nil
			}
			continue
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		return out, fmt.Errorf("systemd: read journal: %w", err)
	}
	return out, nil
}

func entryFromRaw(raw map[string]json.RawMessage) Entry {
	e := Entry{
		Cursor:     journalString(raw["__CURSOR"]),
		Identifier: journalString(raw["SYSLOG_IDENTIFIER"]),
		Message:    journalString(raw["MESSAGE"]),
	}

	// A user unit's identity is _SYSTEMD_USER_UNIT; _SYSTEMD_UNIT on those
	// entries names user@<uid>.service, which is the manager, not the unit the
	// caller asked about.
	if u := journalString(raw["_SYSTEMD_USER_UNIT"]); u != "" {
		e.Unit = u
	} else {
		e.Unit = journalString(raw["_SYSTEMD_UNIT"])
	}

	if us, err := strconv.ParseInt(journalString(raw["__REALTIME_TIMESTAMP"]), 10, 64); err == nil && us > 0 {
		e.Realtime = time.UnixMicro(us).UTC()
	}
	if p, err := strconv.Atoi(journalString(raw["PRIORITY"])); err == nil {
		e.Priority = p
	}
	if p, err := strconv.Atoi(journalString(raw["_PID"])); err == nil {
		e.PID = p
	}
	return e
}

// journalString decodes journald's three JSON shapes for one field.
//
//   - a plain string, which is the ordinary case;
//   - an array of numbers, which is how a value that is not valid UTF-8 is
//     exported — llama.cpp's progress bars and any binary a model name carries
//     land here;
//   - an array of strings, which is how a field that appeared several times in
//     one entry is exported.
func journalString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}

	var bytesForm []int
	if err := json.Unmarshal(raw, &bytesForm); err == nil {
		b := make([]byte, 0, len(bytesForm))
		for _, n := range bytesForm {
			if n < 0 || n > 255 {
				return ""
			}
			b = append(b, byte(n))
		}
		return string(b)
	}

	var strs []string
	if err := json.Unmarshal(raw, &strs); err == nil {
		return strings.Join(strs, "\n")
	}
	return ""
}

// ProbeJournalRead is the boot probe of D77 (section 11.1 step 6): can this
// identity actually read the journal?
//
// The grant is arranged at install time — install-units adds the service
// identity to the systemd-journal group — but it is PROBED rather than trusted,
// because journald shows a caller only what its access rules permit. On the
// default topology the installing user's uid is >= 1000 and journald's
// SplitMode=uid happens to make it work; on a --dedicated-user install the
// account is a system account whose messages live in the SYSTEM journal, and
// every journal consumer in this design would silently show nothing.
//
// The three answers are not interchangeable: `unavailable` means journalctl
// itself could not be run, `denied` means it ran and returned nothing for a unit
// that has demonstrably logged this boot, and the UI says something different
// for each.
func ProbeJournalRead(ctx context.Context, scope model.SystemdScope, unit string, opts JournalOptions) model.JournalRead {
	opts.Scope = scope
	opts.Units = []string{unit}
	opts.Lines = 1

	entries, err := Tail(ctx, opts)
	switch {
	case errors.Is(err, ErrJournalUnavailable):
		return model.JournalUnavailable
	case err != nil:
		return model.JournalUnavailable
	case len(entries) == 0:
		// journalctl exited 0 and produced nothing for a unit that is running
		// this daemon and therefore has logged. That is a denial, not an empty
		// journal.
		return model.JournalDenied
	default:
		return model.JournalOK
	}
}
