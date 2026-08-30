package toolchain

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// fixture reads one captured tool output. Every probe parser in this package is
// tested against real output rather than against a string a test author
// imagined the tool prints — see testdata/README.md for what each file is.
func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "probe", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return string(b)
}

// host is a fake machine: which binaries are on PATH and what each prints.
type host struct {
	bins map[string]string // binary name -> stdout fixture content
	// codes overrides the exit status of a binary.
	codes map[string]int
	// broken names binaries that cannot be executed at all.
	broken map[string]bool
	// calls records every command run, so a test can assert the exact query.
	calls []string
}

func (h *host) options() Options {
	return Options{
		LookPath: func(file string) (string, error) {
			if _, ok := h.bins[file]; !ok {
				return "", errors.New("exec: \"" + file + "\": executable file not found in $PATH")
			}
			return "/usr/bin/" + file, nil
		},
		Run: func(_ context.Context, name string, args ...string) (string, int, error) {
			h.calls = append(h.calls, strings.Join(append([]string{name}, args...), " "))
			bin := filepath.Base(name)
			out, ok := h.bins[bin]
			if !ok {
				return "", -1, errors.New("no such binary")
			}
			if h.broken[bin] {
				return "", -1, errors.New("permission denied")
			}
			return out, h.codes[bin], nil
		},
		Family: FamilyFedora,
		Now:    func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}
}

// fullHost is a machine with everything, built from the captured fixtures.
func fullHost(t *testing.T) *host {
	t.Helper()
	return &host{bins: map[string]string{
		"gcc":        fixture(t, "gcc-fedora.txt"),
		"g++":        fixture(t, "gxx-fedora.txt"),
		"cmake":      fixture(t, "cmake-4.3.txt"),
		"ninja":      fixture(t, "ninja-1.13.txt"),
		"make":       fixture(t, "make-4.4.txt"),
		"git":        fixture(t, "git-2.55.txt"),
		"ccache":     fixture(t, "ccache-4.10.txt"),
		"nvcc":       fixture(t, "nvcc-12.6.txt"),
		"getconf":    fixture(t, "getconf-glibc-2.43.txt"),
		"ldd":        fixture(t, "ldd-glibc-2.43.txt"),
		"nvidia-smi": fixture(t, "nvidia-smi-dual.txt"),
	}}
}

func TestProbeFullyEquippedCUDAHost(t *testing.T) {
	h := fullHost(t)
	r := Probe(t.Context(), h.options())

	if !r.OKCPU || !r.OKCUDA {
		t.Fatalf("ok_cpu=%v ok_cuda=%v on a host with every tool; summary %q", r.OKCPU, r.OKCUDA, r.Summary)
	}
	want := map[string]string{
		ToolGCC: "16.2.1", ToolGXX: "16.2.1", ToolCMake: "4.3.0", ToolNinja: "1.13.2",
		ToolMake: "4.4.1", ToolGit: "2.55.0", ToolCcache: "4.10.2", ToolNvcc: "12.6.85",
		ToolGlibc: "2.43", ToolDriver: "560.35.03",
	}
	for name, version := range want {
		tool, ok := r.Tool(name)
		if !ok {
			t.Errorf("%s missing from the report", name)
			continue
		}
		if !tool.Found || !tool.OK {
			t.Errorf("%s: found=%v ok=%v note=%q", name, tool.Found, tool.OK, tool.Note)
		}
		if tool.Version != version {
			t.Errorf("%s version = %q, want %q", name, tool.Version, version)
		}
	}
	if got := r.CUDAArch; !slices.Equal(got, []string{"86", "89"}) {
		t.Errorf("cuda_arch = %v, want [86 89]", got)
	}
	if len(r.Missing(model.BackendCUDA)) != 0 {
		t.Errorf("missing tools on a complete host: %v", r.Missing(model.BackendCUDA))
	}
	if r.Libc.Kind != LibcGlibc || r.Libc.Source != "getconf" {
		t.Errorf("libc = %+v, want glibc via getconf", r.Libc)
	}
}

func TestProbeCPUOnlyHostIsNotAFailure(t *testing.T) {
	h := fullHost(t)
	delete(h.bins, "nvcc")
	delete(h.bins, "nvidia-smi")
	delete(h.bins, "ccache")

	r := Probe(t.Context(), h.options())
	if !r.OKCPU {
		t.Fatalf("ok_cpu is false on a host that can build CPU: %q", r.Summary)
	}
	if r.OKCUDA {
		t.Error("ok_cuda is true with neither nvcc nor a driver")
	}
	// The plan endpoint asks for exactly this list before a user commits.
	if got := r.Missing(model.BackendCPU); len(got) != 0 {
		t.Errorf("Missing(cpu) = %v, want none — nvcc and the driver are CUDA-only", got)
	}
	if got := r.Missing(model.BackendCUDA); !slices.Equal(got, []string{ToolNvcc, ToolDriver}) {
		t.Errorf("Missing(cuda) = %v, want [nvcc driver]", got)
	}
	// An optional tool that is absent is OK but not found: a renderer must be
	// able to tell "ccache missing, fine" from "cmake missing, nothing builds".
	cc, _ := r.Tool(ToolCcache)
	if cc.Found || !cc.OK || !cc.Optional {
		t.Errorf("ccache = %+v, want absent-but-ok-and-optional", cc)
	}
	if !strings.Contains(r.Summary, "CUDA needs") {
		t.Errorf("summary %q does not say what CUDA is missing", r.Summary)
	}
}

func TestProbeMissingCompilerBlocksEverything(t *testing.T) {
	h := fullHost(t)
	delete(h.bins, "g++")

	r := Probe(t.Context(), h.options())
	if r.OKCPU || r.OKCUDA {
		t.Fatal("a host with no C++ compiler is reported as buildable")
	}
	gxx, _ := r.Tool(ToolGXX)
	if gxx.Found || gxx.OK {
		t.Fatalf("g++ = %+v, want absent", gxx)
	}
	// Per-distro guidance, and never a package-manager call (section 6.5).
	if !strings.Contains(gxx.Note, "gcc-c++") {
		t.Errorf("note %q does not name the Fedora package", gxx.Note)
	}
	for _, banned := range []string{"sudo", "dnf install", "apt install", "pacman -S"} {
		if strings.Contains(gxx.Note, banned) {
			t.Errorf("note %q contains a package-manager invocation", gxx.Note)
		}
	}
	if gxx.DocsURL == "" {
		t.Error("no docs_url for a missing tool; the wizard has nothing to link")
	}
	if !strings.Contains(r.Summary, "g++") {
		t.Errorf("summary %q does not name the missing tool", r.Summary)
	}
}

func TestProbeNinjaOrMakeIsAChoice(t *testing.T) {
	tests := []struct {
		name   string
		remove []string
		wantOK bool
	}{
		{name: "both present", wantOK: true},
		{name: "ninja only", remove: []string{"make"}, wantOK: true},
		{name: "make only", remove: []string{"ninja"}, wantOK: true},
		{name: "neither", remove: []string{"ninja", "make"}, wantOK: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := fullHost(t)
			for _, b := range tc.remove {
				delete(h.bins, b)
			}
			r := Probe(t.Context(), h.options())
			if r.OKCPU != tc.wantOK {
				t.Errorf("ok_cpu = %v, want %v (summary %q)", r.OKCPU, tc.wantOK, r.Summary)
			}
			if !tc.wantOK && !strings.Contains(r.Summary, "ninja or make") {
				t.Errorf("summary %q does not state that one generator is enough", r.Summary)
			}
		})
	}
}

func TestProbeCMakeTooOld(t *testing.T) {
	h := fullHost(t)
	h.bins["cmake"] = fixture(t, "cmake-3.10.txt")

	r := Probe(t.Context(), h.options())
	cm, _ := r.Tool(ToolCMake)
	if !cm.Found {
		t.Fatal("cmake 3.10 is on PATH but the report says it is not found")
	}
	if cm.OK {
		t.Error("cmake 3.10 passes a minimum of 3.14")
	}
	if cm.Version != "3.10.2" || cm.MinVersion != MinCMake {
		t.Errorf("cmake = %+v, want version 3.10.2 against min %s", cm, MinCMake)
	}
	if !strings.Contains(cm.Note, "3.14") {
		t.Errorf("note %q does not state the required version", cm.Note)
	}
	if r.OKCPU {
		t.Error("ok_cpu is true with a cmake too old to configure the project")
	}
}

func TestProbeBinaryPresentButUnrunnable(t *testing.T) {
	h := fullHost(t)
	h.broken = map[string]bool{"cmake": true}

	r := Probe(t.Context(), h.options())
	cm, _ := r.Tool(ToolCMake)
	if !cm.Found {
		t.Error("a binary that exists but cannot be executed should still be found")
	}
	if cm.OK {
		t.Error("an unrunnable cmake is reported as ok")
	}
	if !strings.Contains(cm.Note, "could not be run") {
		t.Errorf("note %q does not say the binary would not run", cm.Note)
	}
}

func TestProbeMuslHostSaysPrebuiltsWillNotRun(t *testing.T) {
	h := fullHost(t)
	delete(h.bins, "getconf")
	h.bins["ldd"] = fixture(t, "ldd-musl-1.2.5.txt")
	h.codes = map[string]int{"ldd": 1} // musl's ldd exits 1 while printing its banner

	r := Probe(t.Context(), h.options())
	if r.Libc.Kind != LibcMusl {
		t.Fatalf("libc = %+v, want musl", r.Libc)
	}
	if r.Libc.VersionString != "1.2.5" {
		t.Errorf("musl version = %q, want 1.2.5", r.Libc.VersionString)
	}
	glibc, _ := r.Tool(ToolGlibc)
	if !strings.Contains(glibc.Note, "builds from source") {
		t.Errorf("note %q does not say every install builds from source here", glibc.Note)
	}
	// A musl host still builds: the libc is a fact, not a gate.
	if !r.OKCPU {
		t.Errorf("ok_cpu is false on a musl host with a full toolchain: %q", r.Summary)
	}
}

func TestProbeUnknownLibcIsReportedNotInvented(t *testing.T) {
	h := fullHost(t)
	delete(h.bins, "getconf")
	delete(h.bins, "ldd")

	r := Probe(t.Context(), h.options())
	if r.Libc.Kind != LibcUnknown || r.Libc.Known() {
		t.Fatalf("libc = %+v, want unknown", r.Libc)
	}
	glibc, _ := r.Tool(ToolGlibc)
	if glibc.Version != "" {
		t.Errorf("an unknown libc reported version %q", glibc.Version)
	}
}

func TestReportJSONIsTheStoredShape(t *testing.T) {
	r := Probe(t.Context(), fullHost(t).options())
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// toolchain_probes.result_json (section 2.4) and section 3.3's per-tool
	// object: the field names are a contract with the UI, not an accident.
	for _, key := range []string{"at", "family", "tools", "libc", "ok_cpu", "ok_cuda", "summary"} {
		if _, ok := got[key]; !ok {
			t.Errorf("result_json is missing %q", key)
		}
	}
	tools, _ := got["tools"].([]any)
	if len(tools) == 0 {
		t.Fatal("no tools in result_json")
	}
	first, _ := tools[0].(map[string]any)
	for _, key := range []string{"name", "found", "ok"} {
		if _, ok := first[key]; !ok {
			t.Errorf("tool object is missing %q", key)
		}
	}
}

func TestRequiredFreeBytes(t *testing.T) {
	const giB = 1 << 30
	tests := []struct {
		backend model.Backend
		want    int64
	}{
		{backend: model.BackendCUDA, want: 12 * giB},
		{backend: model.BackendCPU, want: 3 * giB},
	}
	for _, tc := range tests {
		t.Run(string(tc.backend), func(t *testing.T) {
			if got := RequiredFreeBytes(tc.backend); got != tc.want {
				t.Errorf("RequiredFreeBytes(%s) = %d, want %d", tc.backend, got, tc.want)
			}
		})
	}
}

func TestDetectFamily(t *testing.T) {
	tests := []struct {
		file string
		want Family
	}{
		{file: "ubuntu.txt", want: FamilyDebian},  // via ID_LIKE
		{file: "cachyos.txt", want: FamilyArch},   // via ID
		{file: "alpine.txt", want: FamilyAlpine},  // via ID, no ID_LIKE
		{file: "nixos.txt", want: FamilyUnknown},  // no package name we can name
		{file: "absent.txt", want: FamilyUnknown}, // unreadable is an answer
	}
	for _, tc := range tests {
		t.Run(tc.file, func(t *testing.T) {
			got := DetectFamily(filepath.Join("testdata", "osrelease", tc.file))
			if got != tc.want {
				t.Errorf("DetectFamily(%s) = %q, want %q", tc.file, got, tc.want)
			}
		})
	}
}

func TestGuidanceFallsBackToEveryDistro(t *testing.T) {
	h := fullHost(t)
	delete(h.bins, "ninja")
	opts := h.options()
	opts.Family = FamilyUnknown

	r := Probe(t.Context(), opts)
	ninja, _ := r.Tool(ToolNinja)
	// On a host whose distro we cannot name, the note lists every family's
	// package rather than guessing one.
	for _, want := range []string{"debian: ninja-build", "arch: ninja", "alpine: samurai"} {
		if !strings.Contains(ninja.Note, want) {
			t.Errorf("note %q is missing %q", ninja.Note, want)
		}
	}
}
