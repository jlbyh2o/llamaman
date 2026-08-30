package source

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/toolchain"
)

// fakeSHA is what the fake git resolves every ref to.
const fakeSHA = "1111111111111111111111111111111111111111"

// fixture is one build's worth of world: a state directory, a fake toolchain,
// and a Builder wired to both.
//
// Nothing here compiles anything. DESIGN section 15 is explicit that this is
// the point — the phase machine, the log streaming, the cancellation, the
// staging protocol and the retry are all exercised against a cmake that is a
// shell script, "which is why this area can be tested at all".
type fixture struct {
	t     *testing.T
	State string // the resolved state directory (D72)
	Rec   string // FAKE_STATE: where the fake tools record what they were asked
	Bin   string // the fake toolchain
	B     *Builder
	Obs   *recordObserver
}

func newFixture(t *testing.T, env ...string) *fixture {
	t.Helper()

	root := t.TempDir()
	f := &fixture{
		t:     t,
		State: filepath.Join(root, "state"),
		Rec:   filepath.Join(root, "rec"),
		Bin:   filepath.Join(root, "bin"),
		Obs:   &recordObserver{},
	}
	for _, d := range []string{f.State, f.Rec, f.Bin} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatalf("create %s: %v", d, err)
		}
	}
	copyFake(t, f.Bin, "cmake", "git")

	f.B = &Builder{
		Layout: Layout{StateDir: f.State},
		Logs:   NewLogRegistry(),
		Tools: Tools{
			Git:          filepath.Join(f.Bin, "git"),
			CMake:        filepath.Join(f.Bin, "cmake"),
			CMakeVersion: "3.28.3",
			// Only its presence is read — Tools.Generator turns it into the
			// "Ninja" generator name, and the fake cmake never execs it.
			Ninja: filepath.Join(f.Bin, "ninja"),
			CC:    "/usr/bin/cc",
			CXX:   "/usr/bin/c++",
		},
		Guard:        stubGuard{},
		OOM:          stubOOM{},
		NumCPU:       func() int { return 8 },
		MemAvailable: func() (uint64, error) { return 16 * GiB, nil },
		FreeSpace:    func(string) (uint64, error) { return 100 * GiB, nil },
		CPUFlags:     func() (string, error) { return "avx avx2 avx512f", nil },
		Env:          append([]string{"FAKE_STATE=" + f.Rec, "FAKE_SHA=" + fakeSHA}, env...),
		Grace:        500 * time.Millisecond,
	}
	return f
}

// request is the ordinary CPU build every test starts from.
func (f *fixture) request() Request {
	return Request{
		VersionID: "b10621-cpu-src",
		Tag:       "b10621",
		GitRef:    "b10621",
		Backend:   model.BackendCPU,
		Observer:  f.Obs,
	}
}

// setEnv adds or replaces one variable in the fake toolchain's environment.
func (f *fixture) setEnv(kv string) {
	key, _, _ := strings.Cut(kv, "=")
	for i, e := range f.B.Env {
		if strings.HasPrefix(e, key+"=") {
			f.B.Env[i] = kv
			return
		}
	}
	f.B.Env = append(f.B.Env, kv)
}

// write puts a file into the fake tools' state directory — devices.txt and
// help.txt are how a test changes what the installed `llama-server` answers.
func (f *fixture) write(name, content string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.Rec, name), []byte(content), 0o600); err != nil {
		f.t.Fatalf("write %s: %v", name, err)
	}
}

// recorded returns the lines one fake tool logged, e.g. "cmake.args".
func (f *fixture) recorded(name string) []string {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.Rec, name))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		f.t.Fatalf("read %s: %v", name, err)
	}
	var out []string
	for _, l := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// countRecorded counts recorded invocations containing substr.
func (f *fixture) countRecorded(name, substr string) int {
	n := 0
	for _, l := range f.recorded(name) {
		if strings.Contains(l, substr) {
			n++
		}
	}
	return n
}

// buildLog is the durable log the build wrote (F15).
func (f *fixture) buildLog(id string) string {
	f.t.Helper()
	b, err := os.ReadFile(f.B.Layout.LogPath(id))
	if err != nil {
		f.t.Fatalf("read the build log: %v", err)
	}
	return string(b)
}

func copyFake(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join("testdata", "fakebin", n))
		if err != nil {
			t.Fatalf("read the fake %s: %v", n, err)
		}
		// 0o755 rather than preserving the mode: a checkout that lost the
		// executable bit must not turn every test in this file into a confusing
		// "permission denied".
		if err := os.WriteFile(filepath.Join(dir, n), b, 0o755); err != nil {
			t.Fatalf("install the fake %s: %v", n, err)
		}
	}
}

// fakeProbe builds toolchain.Options describing a host that has every tool
// except the named ones, so a test can state "this host has no nvcc" in one
// line instead of stubbing a report.
//
// It goes through the real internal/toolchain probe rather than around it,
// which is the point: section 6.5's preflight IS that probe, and a test that
// substituted its own would stop noticing when the two disagree.
func fakeProbe(f *fixture, absent ...string) toolchain.Options {
	missing := make(map[string]bool, len(absent))
	for _, n := range absent {
		missing[n] = true
	}
	return toolchain.Options{
		// FamilyUnknown makes the guidance list every distribution's package
		// name, which is the widest form of the per-distro hint.
		Family: toolchain.FamilyUnknown,
		LookPath: func(name string) (string, error) {
			if missing[name] {
				return "", os.ErrNotExist
			}
			// The fake tools directory is only where the paths POINT; the
			// runner below is what answers for them.
			return filepath.Join(f.Bin, name), nil
		},
		Run: func(_ context.Context, name string, _ ...string) (string, int, error) {
			switch filepath.Base(name) {
			case "cmake":
				return "cmake version 3.28.3\n", 0, nil
			case "ninja":
				return "1.11.1\n", 0, nil
			case "nvcc":
				return "Cuda compilation tools, release 12.6, V12.6.85\n", 0, nil
			case "nvidia-smi":
				return "580.65.06, 8.9\n", 0, nil
			case "ldd":
				return "ldd (GNU libc) 2.40\n", 0, nil
			default:
				return filepath.Base(name) + " (fake) 14.2.0\n", 0, nil
			}
		},
	}
}

// recordObserver keeps every progress frame, which is how the tests assert the
// phase order and the compile counters that reach `jobs.progress_json`.
type recordObserver struct {
	mu     sync.Mutex
	frames []Progress
	err    error
}

func (o *recordObserver) Progress(_ context.Context, p Progress) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.frames = append(o.frames, p)
	return o.err
}

func (o *recordObserver) all() []Progress {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]Progress(nil), o.frames...)
}

// phases returns the phase of each frame, collapsing repeats so a compile that
// reported forty percentages still reads as one phase.
func (o *recordObserver) phases() []Phase {
	var out []Phase
	for _, p := range o.all() {
		if len(out) == 0 || out[len(out)-1] != p.Phase {
			out = append(out, p.Phase)
		}
	}
	return out
}

func (o *recordObserver) has(match func(Progress) bool) bool {
	for _, p := range o.all() {
		if match(p) {
			return true
		}
	}
	return false
}

// stubGuard is the default D25 answer in tests: nothing is executing out of the
// directory. The cases that care supply their own.
type stubGuard struct {
	pid   int
	inUse bool
	err   error
}

func (g stubGuard) InUse(context.Context, string) (int, bool, error) {
	return g.pid, g.inUse, g.err
}

// stubOOM is the default kernel-log answer: no oom-kill line. D20's suspicion
// then rests on the build output alone, which is exactly the degraded case a
// host with an unreadable /dev/kmsg is in.
type stubOOM struct {
	confirmed bool
	err       error
}

func (o stubOOM) OOMKillSince(context.Context, time.Time) (bool, error) {
	return o.confirmed, o.err
}

func mustFailure(t *testing.T, err error) *Failure {
	t.Helper()
	f, ok := AsFailure(err)
	if !ok {
		t.Fatalf("err = %v, want a *Failure", err)
	}
	return f
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("stat %s: %v", path, err)
	}
	return err == nil
}
