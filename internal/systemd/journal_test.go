package systemd

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// fakeJournal replaces the journalctl subprocess with a canned stream.
type fakeJournal struct {
	mu       sync.Mutex
	argv     []string
	out      string
	startErr error
	waitErr  error
	// block, when set, keeps the reader open until it is closed, which is what
	// a --follow subprocess does.
	block chan struct{}
}

func (f *fakeJournal) run(_ context.Context, name string, args ...string) (io.ReadCloser, func() error, error) {
	f.mu.Lock()
	f.argv = append([]string{name}, args...)
	f.mu.Unlock()
	if f.startErr != nil {
		return nil, nil, f.startErr
	}
	var r io.Reader = strings.NewReader(f.out)
	if f.block != nil {
		r = io.MultiReader(r, blockingReader{f.block})
	}
	return io.NopCloser(r), func() error { return f.waitErr }, nil
}

func (f *fakeJournal) recordedArgv() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.argv...)
}

// blockingReader never returns data; it returns EOF when its channel closes.
type blockingReader struct{ done chan struct{} }

func (b blockingReader) Read([]byte) (int, error) {
	<-b.done
	return 0, io.EOF
}

// TestJournalArgs pins the argument list, including the two choices that make
// the reader locale-free: `-o json`, and `@<seconds>` for --since rather than a
// formatted date the host would parse in its own locale and time zone.
func TestJournalArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		opts   JournalOptions
		follow bool
		want   []string
	}{
		{
			name: "a bounded tail of one unit",
			opts: JournalOptions{Units: []string{"llamaman.service"}, Lines: 200},
			want: []string{"-o", "json", "--no-pager", "--unit=llamaman.service", "-n", "200"},
		},
		{
			name: "user scope is not optional",
			opts: JournalOptions{Scope: model.ScopeUser, Units: []string{"llamaman.service"}},
			want: []string{"--user", "-o", "json", "--no-pager", "--unit=llamaman.service"},
		},
		{
			name:   "following resumes from a cursor",
			opts:   JournalOptions{Units: []string{"a.service", "b.service"}, Cursor: "s=abc;i=1"},
			follow: true,
			want: []string{"-o", "json", "--no-pager", "--unit=a.service", "--unit=b.service",
				"--after-cursor=s=abc;i=1", "--follow"},
		},
		{
			name: "since is a unix timestamp",
			opts: JournalOptions{Since: time.Unix(1788042587, 0)},
			want: []string{"-o", "json", "--no-pager", "--since=@1788042587"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(tc.want, tc.opts.args(tc.follow)); diff != "" {
				t.Errorf("args (-want +got):\n%s", diff)
			}
		})
	}
}

// TestJournalDecode covers journald's JSON dialect, whose corners are where a
// naive reader silently loses lines.
func TestJournalDecode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		out  string
		want []Entry
	}{
		{
			name: "an ordinary system-unit entry",
			out: `{"__CURSOR":"s=1;i=2","__REALTIME_TIMESTAMP":"1788042587000000",` +
				`"_SYSTEMD_UNIT":"llamaman.service","SYSLOG_IDENTIFIER":"llamaman",` +
				`"PRIORITY":"6","_PID":"8421","MESSAGE":"llamaman listening on http://10.0.0.1:5526"}` + "\n",
			want: []Entry{{
				Cursor: "s=1;i=2", Realtime: time.UnixMicro(1788042587000000).UTC(),
				Unit: "llamaman.service", Identifier: "llamaman", Priority: 6, PID: 8421,
				Message: "llamaman listening on http://10.0.0.1:5526",
			}},
		},
		{
			// Under a user manager _SYSTEMD_UNIT names the manager, not the
			// unit the caller asked about, so reading it would attribute every
			// instance's output to user@<uid>.service.
			name: "a user-unit entry is attributed to the user unit",
			out: `{"_SYSTEMD_UNIT":"user@1000.service",` +
				`"_SYSTEMD_USER_UNIT":"llamaman-instance@qwen.service","MESSAGE":"loading model"}` + "\n",
			want: []Entry{{Unit: "llamaman-instance@qwen.service", Message: "loading model"}},
		},
		{
			// A message that is not valid UTF-8 is exported as an array of
			// byte values. llama.cpp's progress output lands here.
			name: "a byte-array message decodes to its bytes",
			out:  `{"MESSAGE":[108,111,97,100,58,32,49,48,37]}` + "\n",
			want: []Entry{{Message: "load: 10%"}},
		},
		{
			// A field that appeared twice in one entry is exported as an array
			// of strings.
			name: "a repeated field joins its values",
			out:  `{"MESSAGE":["first","second"]}` + "\n",
			want: []Entry{{Message: "first\nsecond"}},
		},
		{
			name: "a non-entry notice does not end the stream",
			out: "-- Journal begins at Sat 2026-08-29 --\n" +
				`{"MESSAGE":"after the notice"}` + "\n",
			want: []Entry{{Message: "after the notice"}},
		},
		{
			name: "an empty stream is not an error",
			out:  "",
			want: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := &fakeJournal{out: tc.out}
			got, err := Tail(context.Background(), JournalOptions{Path: "/usr/bin/journalctl", run: f.run})
			if err != nil {
				t.Fatalf("Tail: %v", err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("entries (-want +got):\n%s", diff)
			}
		})
	}
}

// TestJournalFollowStopsWithItsContext: one subprocess per active log viewer,
// killed with its context (section 5.3). The channel must close rather than
// leave a goroutine parked on a reader that never ends.
func TestJournalFollowStopsWithItsContext(t *testing.T) {
	t.Parallel()

	block := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(block) }) }
	defer unblock()

	f := &fakeJournal{out: `{"MESSAGE":"one"}` + "\n", block: block}

	ctx, cancel := context.WithCancel(context.Background())
	entries, errs, err := Follow(ctx, JournalOptions{Path: "/usr/bin/journalctl", run: f.run})
	if err != nil {
		t.Fatalf("Follow: %v", err)
	}

	select {
	case e := <-entries:
		if e.Message != "one" {
			t.Errorf("first entry = %+v", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no entry arrived")
	}

	cancel()
	// The reader ending is what a killed subprocess looks like; the goroutine
	// must then close both channels rather than parking forever.
	unblock()

	waitClosed(t, entries)
	select {
	case err, open := <-errs:
		if open && err != nil {
			t.Errorf("a canceled follow reported %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the error channel was not closed")
	}
}

func waitClosed(t *testing.T, ch <-chan Entry) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, open := <-ch:
			if !open {
				return
			}
		case <-deadline:
			t.Fatal("the entry channel was not closed")
		}
	}
}

// TestJournalUnavailable: a journalctl that cannot be started at all is a
// distinct answer from one that ran and returned nothing.
func TestJournalUnavailable(t *testing.T) {
	t.Parallel()

	f := &fakeJournal{startErr: errors.New("exec: not found")}
	_, err := Tail(context.Background(), JournalOptions{Path: "/usr/bin/journalctl", run: f.run})
	if err == nil {
		t.Fatal("Tail succeeded with no journalctl")
	}
}

// TestProbeJournalRead is D77's three answers, which the UI renders differently:
// `unavailable` means journalctl itself could not run, `denied` means it ran and
// returned nothing for a unit that has demonstrably logged, and only `ok` lets
// the fit observation and the log panes work.
func TestProbeJournalRead(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		out      string
		startErr error
		want     model.JournalRead
	}{
		{
			name: "an entry comes back",
			out:  `{"MESSAGE":"llamaman listening"}` + "\n",
			want: model.JournalOK,
		},
		{
			// journalctl exits 0 and prints nothing for a unit that is running
			// this very daemon. That is journald's access control, not an empty
			// journal — the --dedicated-user topology's silent failure.
			name: "exit 0 with no entries is a denial",
			out:  "",
			want: model.JournalDenied,
		},
		{
			name:     "journalctl absent",
			startErr: errors.New("exec: not found"),
			want:     model.JournalUnavailable,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := &fakeJournal{out: tc.out, startErr: tc.startErr}
			got := ProbeJournalRead(context.Background(), model.ScopeSystem, UnitDaemon,
				JournalOptions{Path: "/usr/bin/journalctl", run: f.run})
			if got != tc.want {
				t.Errorf("ProbeJournalRead = %q, want %q", got, tc.want)
			}
			if tc.startErr == nil {
				argv := f.recordedArgv()
				if !contains(argv, "--unit="+UnitDaemon) || !contains(argv, "-n") {
					t.Errorf("probe argv = %v, want a one-line read of %s", argv, UnitDaemon)
				}
			}
		})
	}
}

// TestProbeJournalReadScopesToTheUserManager: the system journal has none of a
// user manager's units, so a missing --user would report `denied` on a perfectly
// healthy D2 host.
func TestProbeJournalReadScopesToTheUserManager(t *testing.T) {
	t.Parallel()

	f := &fakeJournal{out: `{"MESSAGE":"x"}` + "\n"}
	ProbeJournalRead(context.Background(), model.ScopeUser, UnitDaemon,
		JournalOptions{Path: "/usr/bin/journalctl", run: f.run})
	if !contains(f.recordedArgv(), "--user") {
		t.Errorf("argv = %v, want --user", f.recordedArgv())
	}
}

// TestJournalStringShapes exercises the decoder directly, including the shapes
// that must decode to an empty string rather than to nonsense.
func TestJournalStringShapes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"a string", `"hello"`, "hello"},
		{"bytes", `[104,105]`, "hi"},
		{"strings", `["a","b"]`, "a\nb"},
		{"a number out of byte range", `[300]`, ""},
		{"an object", `{"x":1}`, ""},
		{"null", `null`, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := journalString([]byte(tc.raw)); got != tc.want {
				t.Errorf("journalString(%s) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}
