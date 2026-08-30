package source

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/jlbyh2o/llamaman/internal/hw"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/procx"
	"github.com/jlbyh2o/llamaman/internal/toolchain"
)

func TestBuildJobs(t *testing.T) {
	t.Parallel()

	// D20: N = min(NumCPU, max(2, MemAvailableGiB/2)).
	for _, tc := range []struct {
		name   string
		cpus   int
		memGiB float64
		want   int
	}{
		{"memory is the binding constraint", 32, 8, 4},
		{"cpus are the binding constraint", 4, 64, 4},
		{"the floor of two survives a starved host", 32, 1, 2},
		{"the floor never exceeds the cpu count", 1, 64, 1},
		{"a workstation with 64 GiB and 16 cores", 16, 64, 16},
		{"an unreadable MemAvailable still gives two", 8, 0, 2},
		{"a nonsense cpu count is treated as one", 0, 8, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := BuildJobs(tc.cpus, uint64(tc.memGiB*GiB))
			if got != tc.want {
				t.Errorf("BuildJobs(%d, %.0f GiB) = %d, want %d", tc.cpus, tc.memGiB, got, tc.want)
			}
		})
	}
}

func TestMemAvailableBytes(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "meminfo")
	content := "MemTotal:       65707412 kB\nMemFree:          402340 kB\nMemAvailable:   12345678 kB\nBuffers:          123 kB\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := MemAvailableBytes(path)
	if err != nil {
		t.Fatalf("MemAvailableBytes: %v", err)
	}
	if want := uint64(12345678 * 1024); got != want {
		t.Errorf("MemAvailableBytes = %d, want %d", got, want)
	}

	missing := filepath.Join(dir, "no-memavailable")
	if err := os.WriteFile(missing, []byte("MemTotal: 1 kB\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := MemAvailableBytes(missing); err == nil {
		t.Error("a file with no MemAvailable line returned no error")
	}
}

func TestParseBuildProgress(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name             string
		line             string
		done, total, pct int
		ok               bool
	}{
		{"ninja counters", "[812/1930] Building CXX object ggml.dir/ggml.c.o", 812, 1930, 42, true},
		{"ninja at the start", "[1/1930] Building", 1, 1930, 0, true},
		{"ninja complete", "[1930/1930] Linking", 1930, 1930, 100, true},
		{"make percentage", "[ 42%] Building CXX object", 0, 0, 42, true},
		{"make at 100", "[100%] Built target llama", 0, 0, 100, true},
		{"a leading space is tolerated", "  [7/9] Building", 7, 9, 77, true},
		{"a compiler warning is not progress", "warning: [x/y] looks like this", 0, 0, 0, false},
		{"a zero total is not progress", "[0/0] nothing", 0, 0, 0, false},
		{"plain output", "-- Configuring done", 0, 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			done, total, pct, ok := parseBuildProgress(tc.line)
			if ok != tc.ok || done != tc.done || total != tc.total || pct != tc.pct {
				t.Errorf("parseBuildProgress(%q) = (%d, %d, %d, %v), want (%d, %d, %d, %v)",
					tc.line, done, total, pct, ok, tc.done, tc.total, tc.pct, tc.ok)
			}
		})
	}
}

func TestCountCUDADevices(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		out  string
		want int
	}{
		{
			name: "one device",
			out:  "Available devices:\n  CUDA0: NVIDIA GeForce RTX 4090 (24210 MiB, 23900 MiB free)\n",
			want: 1,
		},
		{
			name: "two devices",
			out: "Available devices:\n" +
				"  CUDA0: NVIDIA GeForce RTX 4090 (24210 MiB, 23900 MiB free)\n" +
				"  CUDA1: NVIDIA GeForce RTX 3090 (24576 MiB, 24000 MiB free)\n",
			want: 2,
		},
		{
			name: "the backend's own init line",
			out:  "ggml_cuda_init: found 3 CUDA devices:\n  Device 0: NVIDIA A100\n",
			want: 3,
		},
		{
			name: "a CPU build lists nothing",
			out:  "Available devices:\n",
			want: 0,
		},
		{
			name: "a CUDA build that fell back to CPU — the D19 case",
			out:  "Available devices:\n  CPU: 32 cores\n",
			want: 0,
		},
		{
			name: "the word CUDA in prose is not a device",
			out:  "warning: CUDA support was requested but is unavailable\n",
			want: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := CountCUDADevices(tc.out); got != tc.want {
				t.Errorf("CountCUDADevices = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestParseHelpFlags(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		out  string
		want []string
	}{
		{
			name: "upstream's own shape",
			out: "----- common params -----\n" +
				"\n" +
				"-h,    --help, --usage          print usage and exit\n" +
				"-c,    --ctx-size N             size of the prompt context (default: 4096)\n" +
				"       --fit                    let llama.cpp choose the offload\n" +
				"-ngl,  --gpu-layers N           number of layers to offload\n",
			want: []string{"--ctx-size", "--fit", "--gpu-layers", "--help", "--usage", "-c", "-h", "-ngl"},
		},
		{
			name: "an alias after a placeholder",
			out:  "--ctx-size N, -c N              size of the prompt context\n",
			want: []string{"--ctx-size", "-c"},
		},
		{
			name: "an = placeholder",
			out:  "--fit=BOOL                      whether to fit\n",
			want: []string{"--fit"},
		},
		{
			name: "a flag named in prose is NOT a flag the build supports",
			out:  "--jinja                         use the chat template; see --fit for the offload\n",
			want: []string{"--jinja"},
		},
		{
			name: "a deeply indented continuation line is not a cluster",
			out: "--foo                           does a thing\n" +
				"                                --not-a-flag is mentioned here\n",
			want: []string{"--foo"},
		},
		{
			name: "an empty capture is an empty set, which section 5.7 reads as unavailable",
			out:  "",
			want: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ParseHelpFlags(tc.out)
			if diff := cmp.Diff(model.HelpFlags(tc.want), got); diff != "" {
				t.Errorf("ParseHelpFlags mismatch (-want +got):\n%s", diff)
			}
			if tc.want == nil && got.Available() {
				t.Error("an empty capture reports Available() = true")
			}
		})
	}
}

func TestConfigureArgs(t *testing.T) {
	t.Parallel()

	base := ConfigureOptions{
		SourceDir:     "/var/lib/llamaman/build/b10621-cuda-src",
		BuildDir:      "/var/lib/llamaman/build/b10621-cuda-src/build",
		InstallPrefix: "/var/lib/llamaman/versions/b10621-cuda-src.staging",
		Generator:     "Ninja",
	}

	t.Run("the CPU flag set is section 6.5's, in order", func(t *testing.T) {
		t.Parallel()
		o := base
		o.Backend = model.BackendCPU
		want := []string{
			"-S", "/var/lib/llamaman/build/b10621-cuda-src",
			"-B", "/var/lib/llamaman/build/b10621-cuda-src/build",
			"-G", "Ninja",
			"-DCMAKE_BUILD_TYPE=Release",
			"-DCMAKE_INSTALL_PREFIX=/var/lib/llamaman/versions/b10621-cuda-src.staging",
			"-DLLAMA_BUILD_SERVER=ON",
			"-DLLAMA_BUILD_TOOLS=ON",
			"-DLLAMA_BUILD_TESTS=OFF",
			"-DLLAMA_BUILD_EXAMPLES=OFF",
			"-DLLAMA_CURL=OFF",
			"-DBUILD_SHARED_LIBS=ON",
			"-DGGML_NATIVE=ON",
			"-DCMAKE_INSTALL_RPATH=$ORIGIN/../lib",
			"-DCMAKE_BUILD_WITH_INSTALL_RPATH=ON",
		}
		if diff := cmp.Diff(want, ConfigureArgs(o)); diff != "" {
			t.Errorf("ConfigureArgs mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("D22: the RPATH is passed literally, with no shell to expand it", func(t *testing.T) {
		t.Parallel()
		args := ConfigureArgs(base)
		if !slices.Contains(args, "-DCMAKE_INSTALL_RPATH=$ORIGIN/../lib") {
			t.Errorf("the install RPATH is not literal: %v", args)
		}
		if !slices.Contains(args, "-DCMAKE_BUILD_WITH_INSTALL_RPATH=ON") {
			t.Error("BUILD_WITH_INSTALL_RPATH is missing, so cmake would strip the RPATH at install time")
		}
	})

	t.Run("the CUDA block follows the common flags", func(t *testing.T) {
		t.Parallel()
		o := base
		o.Backend = model.BackendCUDA
		o.CUDAArchs = []string{"86", "89"}
		args := ConfigureArgs(o)
		if !slices.Contains(args, "-DGGML_CUDA=ON") {
			t.Error("-DGGML_CUDA=ON is missing")
		}
		if !slices.Contains(args, "-DCMAKE_CUDA_ARCHITECTURES=86;89") {
			t.Errorf("the architecture list is wrong: %v", args)
		}
	})

	t.Run("ccache sets both launchers, and extra flags come last", func(t *testing.T) {
		t.Parallel()
		o := base
		o.CCache = "/usr/bin/ccache"
		o.ExtraFlags = []string{"-DGGML_LTO=ON", "-DCMAKE_CUDA_HOST_COMPILER=/usr/bin/g++-13"}
		args := ConfigureArgs(o)
		if !slices.Contains(args, "-DCMAKE_C_COMPILER_LAUNCHER=ccache") ||
			!slices.Contains(args, "-DCMAKE_CXX_COMPILER_LAUNCHER=ccache") {
			t.Errorf("ccache launchers are missing: %v", args)
		}
		if got := args[len(args)-2:]; !slices.Equal(got, o.ExtraFlags) {
			t.Errorf("extra flags are not last: %v", got)
		}
	})

	t.Run("no generator means Ninja, and the make generator is spelled the way cmake spells it", func(t *testing.T) {
		t.Parallel()
		o := base
		o.Generator = ""
		if got := ConfigureArgs(o)[5]; got != "Ninja" {
			t.Errorf("default generator = %q", got)
		}
		if got := (Tools{}).Generator(); got != "Unix Makefiles" {
			t.Errorf("Tools.Generator with no ninja = %q, want Unix Makefiles", got)
		}
		if got := (Tools{Ninja: "/usr/bin/ninja"}).Generator(); got != "Ninja" {
			t.Errorf("Tools.Generator with ninja = %q", got)
		}
	})
}

func TestBuildAndInstallArgs(t *testing.T) {
	t.Parallel()

	if got := BuildArgs("/b", 8); !slices.Equal(got, []string{"--build", "/b", "-j", "8"}) {
		t.Errorf("BuildArgs = %v", got)
	}
	if got := BuildArgs("/b", 0); !slices.Equal(got, []string{"--build", "/b", "-j", "1"}) {
		t.Errorf("BuildArgs with no jobs = %v, want -j 1", got)
	}
	// `cmake --install --prefix` needs cmake 3.15 and the floor is 3.14, so the
	// prefix must come from the cache the configure step wrote.
	if got := InstallArgs("/b"); !slices.Equal(got, []string{"--install", "/b"}) {
		t.Errorf("InstallArgs = %v", got)
	}
}

func TestLayout(t *testing.T) {
	t.Parallel()

	l := Layout{StateDir: "/var/lib/llamaman"}
	const id = "b10621-cuda-src"
	for _, tc := range []struct{ name, got, want string }{
		{"repo", l.RepoDir(), "/var/lib/llamaman/src/llama.cpp"},
		{"worktree", l.WorktreeDir(id), "/var/lib/llamaman/build/b10621-cuda-src"},
		{"cmake dir", l.CMakeDir(id), "/var/lib/llamaman/build/b10621-cuda-src/build"},
		{"staging", l.StagingDir(id), "/var/lib/llamaman/versions/b10621-cuda-src.staging"},
		{"version", l.VersionDir(id), "/var/lib/llamaman/versions/b10621-cuda-src"},
		{"superseded", l.SupersededDir(id), "/var/lib/llamaman/versions/b10621-cuda-src.old"},
		{"log", l.LogPath(id), "/var/lib/llamaman/logs/build/b10621-cuda-src.log"},
		{"server", ServerPath(l.VersionDir(id)), "/var/lib/llamaman/versions/b10621-cuda-src/bin/llama-server"},
		{"bench", BenchPath(l.VersionDir(id)), "/var/lib/llamaman/versions/b10621-cuda-src/bin/llama-bench"},
		{"lib", LibDir(l.VersionDir(id)), "/var/lib/llamaman/versions/b10621-cuda-src/lib"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

func TestValidateVersionID(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		id string
		ok bool
	}{
		{"b10621-cpu-bin", true},
		{"b10621-cuda-src", true},
		{"v0.3.0-cpu-bin", true},
		{"fork-a1b2c3-1234567-cuda-src", true},
		{"", false},
		{"../escape", false},
		{"a/b", false},
		{".hidden", false},
		{"has space", false},
	} {
		err := ValidateVersionID(tc.id)
		if (err == nil) != tc.ok {
			t.Errorf("ValidateVersionID(%q) = %v, want ok=%v", tc.id, err, tc.ok)
		}
	}
}

func TestCUDAArchitectures(t *testing.T) {
	t.Parallel()

	t.Run("detected capabilities become a sorted, deduplicated cmake list", func(t *testing.T) {
		t.Parallel()
		gpus := []hw.GPU{
			{UUID: "GPU-1", ComputeCap: "8.9"},
			{UUID: "GPU-2", ComputeCap: "8.6"},
			{UUID: "GPU-3", ComputeCap: "8.9"},
			{UUID: "GPU-4"}, // the driver did not report one
		}
		if got := CUDAArchsFromGPUs(gpus); !slices.Equal(got, []string{"86", "89"}) {
			t.Errorf("CUDAArchsFromGPUs = %v, want [86 89]", got)
		}
	})

	t.Run("the settings list is parsed the way a person writes it", func(t *testing.T) {
		t.Parallel()
		for _, in := range []string{"89;86", "86,89", "8.6 8.9", "89; 86 ;89"} {
			got, err := ParseCUDAArchList(in)
			if err != nil {
				t.Fatalf("ParseCUDAArchList(%q): %v", in, err)
			}
			if !slices.Equal(got, []string{"86", "89"}) {
				t.Errorf("ParseCUDAArchList(%q) = %v", in, got)
			}
		}
		got, err := ParseCUDAArchList("")
		if err != nil || len(got) != 0 {
			t.Errorf(`ParseCUDAArchList("") = %v, %v — empty means auto-detect, not an error`, got, err)
		}
	})

	t.Run("D21: native and all are refused", func(t *testing.T) {
		t.Parallel()
		for _, in := range []string{"native", "all", "all-major", "NATIVE"} {
			if _, err := ParseCUDAArchList(in); err == nil {
				t.Errorf("ParseCUDAArchList(%q) was accepted", in)
			}
			req := Request{VersionID: "x-cuda-src", Backend: model.BackendCUDA, CUDAArchs: []string{in}}
			if err := req.Validate(); err == nil {
				t.Errorf("Request with CUDAArchs=[%q] validated", in)
			}
		}
	})

	t.Run("the -real and -virtual suffixes are legal", func(t *testing.T) {
		t.Parallel()
		req := Request{VersionID: "x-cuda-src", Backend: model.BackendCUDA, CUDAArchs: []string{"89-real", "86-virtual"}}
		if err := req.Validate(); err != nil {
			t.Errorf("Validate: %v", err)
		}
		if got := req.CUDAArchList(); got != "89-real;86-virtual" {
			t.Errorf("CUDAArchList = %q", got)
		}
	})
}

func TestToolsFromReport(t *testing.T) {
	t.Parallel()

	// The projection is deliberately narrow: this pipeline execs four binaries
	// and records the rest, so a tool the probe did not find must leave an EMPTY
	// path rather than a guess that fails at execve time.
	r := toolchain.Report{Tools: []toolchain.Tool{
		{Name: toolchain.ToolGit, Found: true, Path: "/usr/bin/git", OK: true},
		{Name: toolchain.ToolCMake, Found: true, Path: "/usr/bin/cmake", Version: "3.28.3", OK: true},
		{Name: toolchain.ToolNinja, Found: true, Path: "/usr/bin/ninja", OK: true},
		{Name: toolchain.ToolMake, Found: true, Path: "/usr/bin/make", OK: true},
		{Name: toolchain.ToolGCC, Found: true, Path: "/usr/bin/gcc", OK: true},
		{Name: toolchain.ToolGXX, Found: true, Path: "/usr/bin/g++", OK: true},
		{Name: toolchain.ToolCcache, Found: false},
		{Name: toolchain.ToolNvcc, Found: true, Path: "/usr/local/cuda/bin/nvcc", OK: true},
	}}
	got := ToolsFromReport(r)
	want := Tools{
		Git: "/usr/bin/git", CMake: "/usr/bin/cmake", CMakeVersion: "3.28.3",
		Ninja: "/usr/bin/ninja", Make: "/usr/bin/make",
		CC: "/usr/bin/gcc", CXX: "/usr/bin/g++", NVCC: "/usr/local/cuda/bin/nvcc",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ToolsFromReport mismatch (-want +got):\n%s", diff)
	}
	if got.Generator() != "Ninja" {
		t.Errorf("Generator = %q", got.Generator())
	}
}

func TestProbeToolchain(t *testing.T) {
	t.Parallel()

	// The version floor is internal/toolchain's (cmake >= 3.14, section 6.5),
	// and this asserts the pipeline HONORS its verdict rather than re-deciding
	// it: a preflight with its own copy of the rule is a preflight that can
	// disagree with the plan endpoint the user just read.
	opts := func(version string, absent ...string) toolchain.Options {
		missing := make(map[string]bool, len(absent))
		for _, n := range absent {
			missing[n] = true
		}
		return toolchain.Options{
			Family: toolchain.FamilyDebian,
			LookPath: func(name string) (string, error) {
				if missing[name] {
					return "", os.ErrNotExist
				}
				return "/usr/bin/" + name, nil
			},
			Run: func(_ context.Context, name string, _ ...string) (string, int, error) {
				switch filepath.Base(name) {
				case "cmake":
					return "cmake version " + version + "\n", 0, nil
				case "nvidia-smi":
					// driver_version, compute_cap — the CSV the probe asks for.
					return "580.65.06, 8.9\n", 0, nil
				default:
					return filepath.Base(name) + " 14.2.0\n", 0, nil
				}
			},
		}
	}

	t.Run("a complete host is accepted", func(t *testing.T) {
		t.Parallel()
		tools, _, err := probeToolchain(t.Context(), opts("3.28.3"), model.BackendCUDA)
		if err != nil {
			t.Fatalf("probeToolchain: %v", err)
		}
		if tools.CMakeVersion != "3.28.3" || tools.Git == "" || tools.NVCC == "" {
			t.Errorf("tools = %+v", tools)
		}
	})

	t.Run("a too-old cmake is refused with its version named", func(t *testing.T) {
		t.Parallel()
		_, _, err := probeToolchain(t.Context(), opts("3.10.2"), model.BackendCPU)
		f := mustFailure(t, err)
		if f.Code != CodeToolchainMissing || f.Phase != PhasePreflight {
			t.Fatalf("failure = %+v", f)
		}
		if !strings.Contains(f.Message, "3.10.2") || !strings.Contains(f.Message, "3.14") {
			t.Errorf("the refusal does not name the versions: %q", f.Message)
		}
	})

	t.Run("ninja is optional as long as make is present", func(t *testing.T) {
		t.Parallel()
		tools, _, err := probeToolchain(t.Context(), opts("3.28.3", "ninja"), model.BackendCPU)
		if err != nil {
			t.Fatalf("probeToolchain: %v", err)
		}
		if tools.Generator() != "Unix Makefiles" {
			t.Errorf("Generator = %q", tools.Generator())
		}
	})

	t.Run("neither generator is a refusal", func(t *testing.T) {
		t.Parallel()
		_, _, err := probeToolchain(t.Context(), opts("3.28.3", "ninja", "make"), model.BackendCPU)
		f := mustFailure(t, err)
		if !strings.Contains(f.Message, "make") {
			t.Errorf("the refusal does not name the generator: %q", f.Message)
		}
	})

	t.Run("nvcc blocks only a CUDA build", func(t *testing.T) {
		t.Parallel()
		if _, _, err := probeToolchain(t.Context(), opts("3.28.3", "nvcc"), model.BackendCPU); err != nil {
			t.Errorf("a CPU build was refused for want of nvcc: %v", err)
		}
		if _, _, err := probeToolchain(t.Context(), opts("3.28.3", "nvcc"), model.BackendCUDA); err == nil {
			t.Error("a CUDA build was accepted with no nvcc")
		}
	})
}

func TestFailureAttribution(t *testing.T) {
	t.Parallel()

	lines := []string{
		"[1/9] Building CXX object",
		"-- Configuring done",
		"/src/llama.cpp:42:7: error: use of undeclared identifier",
		"CMake Error at CMakeLists.txt:1",
	}
	var s logScanner
	for _, l := range lines {
		s.observe(l)
	}
	// The FIRST match wins and its line number is what the viewer scrolls to.
	if s.errLine != 3 || !strings.Contains(s.errText, "undeclared identifier") {
		t.Errorf("attribution = (%d, %q), want the third line", s.errLine, s.errText)
	}

	var clean logScanner
	for _, l := range lines[:2] {
		clean.observe(l)
	}
	if clean.errLine != 0 {
		t.Errorf("attribution over clean output = %d, want 0", clean.errLine)
	}

	for _, tc := range []struct {
		name, line, wantIn string
	}{
		{"unsupported architecture", "nvcc fatal   : Unsupported gpu architecture 'compute_120'", "Settings → Builds"},
		{"missing nvcc", "  No CMAKE_CUDA_COMPILER could be found.", "CUDA toolkit"},
		{"a full disk", "c++: error: ggml.o: No space left on device", "Free space"},
		{"nothing known", "[3/9] Building CXX object", ""},
	} {
		got := hintForLine(tc.line)
		if tc.wantIn == "" {
			if got != "" {
				t.Errorf("%s: hint = %q, want none", tc.name, got)
			}
			continue
		}
		if !strings.Contains(got, tc.wantIn) {
			t.Errorf("%s: hint = %q, want it to mention %q", tc.name, got, tc.wantIn)
		}
	}
}

func TestLogScanner(t *testing.T) {
	t.Parallel()

	var s logScanner
	for _, l := range []string{
		"[1/9] Building",
		"c++: fatal error: Killed signal terminated program cc1plus",
		"/src/x.cpp:1:1: error: first error",
		"/src/y.cpp:2:2: error: second error",
	} {
		s.observe(l)
	}
	if s.lines != 4 {
		t.Errorf("lines = %d", s.lines)
	}
	// The "fatal error:" line matches the error patterns first, which is
	// correct: it IS the first line that explains the failure.
	if s.errLine != 2 {
		t.Errorf("errLine = %d, want 2", s.errLine)
	}
	if s.oomLine == "" {
		t.Error("the OOM evidence line was not captured")
	}
	s.resetOOM()
	if s.oomLine != "" {
		t.Error("resetOOM did not clear the evidence before the retry")
	}
}

func TestSuspectOOM(t *testing.T) {
	t.Parallel()

	killed := procx.Result{ExitCode: -1, Signal: 9}
	ours := procx.Result{ExitCode: -1, Signal: 9, Terminated: true, Killed: true}
	plainFail := procx.Result{ExitCode: 1}

	for _, tc := range []struct {
		name      string
		res       procx.Result
		line      string
		kernel    kernelVerdict
		want      bool
		reasonHas string
	}{
		{
			name: "the child itself was SIGKILLed", res: killed,
			kernel: kernelVerdict{confirmed: true}, want: true, reasonHas: "kernel log",
		},
		{
			name: "the compiler driver reported the kill", res: plainFail,
			line: "c++: fatal error: Killed signal terminated program cc1plus",
			want: true, reasonHas: "no matching oom-kill line",
		},
		{
			name: "an unreadable kernel log still retries", res: plainFail,
			line:   "virtual memory exhausted: Cannot allocate memory",
			kernel: kernelVerdict{err: os.ErrPermission},
			want:   true, reasonHas: "could not be read",
		},
		{
			name: "a plain compile error is not an OOM", res: plainFail,
			line: "", want: false,
		},
		{
			name: "OUR OWN SIGKILL after a cancellation is not an OOM", res: ours,
			line: "", want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := suspectOOM(tc.res, tc.line, tc.kernel)
			if got.Suspected != tc.want {
				t.Fatalf("Suspected = %v, want %v (reason %q)", got.Suspected, tc.want, got.Reason)
			}
			if tc.reasonHas != "" && !strings.Contains(got.Reason, tc.reasonHas) {
				t.Errorf("Reason = %q, want it to mention %q", got.Reason, tc.reasonHas)
			}
		})
	}
}

func TestScanKmsg(t *testing.T) {
	t.Parallel()

	boot := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	const records = "6,1,1000000,-;Linux version 6.11.0\n" +
		"3,900,60000000,-;oom-kill:constraint=CONSTRAINT_NONE,nodemask=(null),cpuset=/,mems_allowed=0,global_oom,task=cc1plus,pid=4242\n" +
		"3,901,60000100,-;Out of memory: Killed process 4242 (cc1plus) total-vm:8000000kB\n"

	for _, tc := range []struct {
		name  string
		since time.Time
		want  bool
	}{
		{"the kill is after the compile started", boot.Add(30 * time.Second), true},
		{"the kill predates this compile", boot.Add(90 * time.Second), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := scanKmsg(strings.NewReader(records), boot, tc.since)
			if err != nil {
				t.Fatalf("scanKmsg: %v", err)
			}
			if got != tc.want {
				t.Errorf("scanKmsg = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("unrelated records are ignored", func(t *testing.T) {
		t.Parallel()
		got, err := scanKmsg(strings.NewReader("6,1,1000,-;usb 1-1: new high-speed USB device\nnot a record\n"), boot, boot)
		if err != nil || got {
			t.Errorf("scanKmsg = %v, %v", got, err)
		}
	})
}

func TestHostCPUFlags(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "cpuinfo")
	content := "processor\t: 0\nvendor_id\t: AuthenticAMD\nflags\t\t: fpu vme avx2 avx512f sse2\n" +
		"processor\t: 1\nflags\t\t: fpu vme avx2 avx512f sse2\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := HostCPUFlags(path)
	if err != nil {
		t.Fatalf("HostCPUFlags: %v", err)
	}
	if want := "avx2 avx512f fpu sse2 vme"; got != want {
		t.Errorf("HostCPUFlags = %q, want the sorted %q", got, want)
	}
}

func TestLogSink(t *testing.T) {
	t.Parallel()

	t.Run("the ring keeps the last N lines in order", func(t *testing.T) {
		t.Parallel()
		s := NewMemoryLog(3)
		s.SetPhase(PhaseCompile)
		for _, text := range []string{"one", "two", "three", "four"} {
			s.Line(procx.Line{Text: text})
		}
		got := s.Tail(0)
		if len(got) != 3 || got[0].Text != "two" || got[2].Text != "four" {
			t.Fatalf("Tail = %+v", got)
		}
		if got[0].Phase != PhaseCompile {
			t.Errorf("entries are not phase-stamped: %+v", got[0])
		}
		if got := s.Tail(2); len(got) != 2 || got[0].Text != "three" {
			t.Errorf("Tail(2) = %+v", got)
		}
	})

	t.Run("a subscriber sees new lines and a full one drops rather than blocks", func(t *testing.T) {
		t.Parallel()
		s := NewMemoryLog(10)
		ch, stop := s.Subscribe(1)
		defer stop()

		s.Line(procx.Line{Text: "first"})
		s.Line(procx.Line{Text: "second"}) // the buffer is full; this one drops
		s.Line(procx.Line{Text: "third"})

		select {
		case e := <-ch:
			if e.Text != "first" {
				t.Errorf("first entry = %q", e.Text)
			}
		default:
			t.Fatal("the subscriber received nothing")
		}
		// The file and the ring are the complete records; the channel is not.
		if len(s.Tail(0)) != 3 {
			t.Errorf("the ring dropped lines too: %+v", s.Tail(0))
		}
		stop()
		stop() // idempotent
	})

	t.Run("the file is written, prefixed, and copyable into the version tree", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "logs", "build", "b1-cpu-src.log")
		s, err := OpenLog(path, 10)
		if err != nil {
			t.Fatalf("OpenLog: %v", err)
		}
		s.SetPhase(PhaseFetch)
		s.Printf("cloning %s", "https://example.invalid/llama.cpp")
		s.SetPhase(PhaseCompile)
		s.Line(procx.Line{Text: "[1/2] Building", Stream: procx.StreamStdout})
		s.Line(procx.Line{Text: "very long", Truncated: true})

		copyPath := filepath.Join(dir, "build.log")
		if err := s.CopyTo(copyPath); err != nil {
			t.Fatalf("CopyTo: %v", err)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(b)
		for _, want := range []string{"[fetch] cloning https://", "[compile] [1/2] Building", "line truncated"} {
			if !strings.Contains(text, want) {
				t.Errorf("the log is missing %q:\n%s", want, text)
			}
		}
		copied, err := os.ReadFile(copyPath)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(copied), "[compile] [1/2] Building") {
			t.Errorf("the copied log is missing content:\n%s", copied)
		}

		// A second build appends rather than truncating, which is what makes a
		// D4 retry's log readable beside the attempt it resumed.
		s2, err := OpenLog(path, 10)
		if err != nil {
			t.Fatal(err)
		}
		s2.Printf("second attempt")
		if err := s2.Close(); err != nil {
			t.Fatal(err)
		}
		b, _ = os.ReadFile(path)
		if !strings.Contains(string(b), "[compile] [1/2] Building") || !strings.Contains(string(b), "second attempt") {
			t.Errorf("the log was truncated by the second attempt:\n%s", b)
		}
	})

	t.Run("a nil sink is usable, so a probe with no log does not need a branch", func(t *testing.T) {
		t.Parallel()
		var s *LogSink
		s.SetPhase(PhaseVerify)
		s.Line(procx.Line{Text: "x"})
		s.Printf("y")
		if got := s.Tail(0); got != nil {
			t.Errorf("Tail on a nil sink = %v", got)
		}
		if err := s.Close(); err != nil {
			t.Errorf("Close on a nil sink: %v", err)
		}
	})
}

func TestProcExeGuard(t *testing.T) {
	t.Parallel()

	// A fake /proc: one process executing out of the version directory, one
	// somewhere else, and one whose exe link cannot be read at all.
	root := t.TempDir()
	versionDir := filepath.Join(root, "versions", "b1-cpu-src")
	if err := os.MkdirAll(filepath.Join(versionDir, "bin"), 0o750); err != nil {
		t.Fatal(err)
	}
	server := filepath.Join(versionDir, "bin", "llama-server")
	if err := os.WriteFile(server, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	proc := filepath.Join(root, "proc")
	link := func(pid, target string) {
		t.Helper()
		dir := filepath.Join(proc, pid)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(dir, "exe")); err != nil {
			t.Fatal(err)
		}
	}
	link("101", "/usr/bin/bash")
	link("202", server)
	if err := os.MkdirAll(filepath.Join(proc, "self"), 0o750); err != nil { // not numeric
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(proc, "303"), 0o750); err != nil { // no exe link
		t.Fatal(err)
	}

	g := ProcExeGuard{Root: proc}
	pid, inUse, err := g.InUse(t.Context(), versionDir)
	if err != nil {
		t.Fatalf("InUse: %v", err)
	}
	if !inUse || pid != 202 {
		t.Errorf("InUse = (%d, %v), want (202, true)", pid, inUse)
	}

	other := filepath.Join(root, "versions", "b2-cpu-src")
	if err := os.MkdirAll(other, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, inUse, err := g.InUse(t.Context(), other); err != nil || inUse {
		t.Errorf("InUse on an unused directory = %v, %v", inUse, err)
	}
	if _, inUse, err := g.InUse(t.Context(), filepath.Join(root, "versions", "gone")); err != nil || inUse {
		t.Errorf("InUse on a missing directory = %v, %v", inUse, err)
	}
}

func TestUnderDir(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		path, dir string
		want      bool
	}{
		{"/v/b1/bin/llama-server", "/v/b1", true},
		{"/v/b1", "/v/b1", true},
		{"/v/b1/bin/llama-server (deleted)", "/v/b1", true},
		{"/v/b10/bin/llama-server", "/v/b1", false},
		{"/usr/bin/bash", "/v/b1", false},
		{"/v/b1/bin/llama-server", "", false},
	} {
		if got := underDir(tc.path, tc.dir); got != tc.want {
			t.Errorf("underDir(%q, %q) = %v, want %v", tc.path, tc.dir, got, tc.want)
		}
	}
}

func TestFailureVersionState(t *testing.T) {
	t.Parallel()

	// Section 2.5 gives `failed_verification` its own edge for exactly two
	// outcomes, and this mapping is the one place that decides it.
	for _, tc := range []struct {
		code string
		want model.VersionState
	}{
		{CodeFailedVerification, model.VersionFailedVerification},
		{CodeNoCUDADevice, model.VersionFailedVerification},
		{CodeCanceled, model.VersionCanceled},
		{CodeCompileFailed, model.VersionFailed},
		{CodeOOMKilled, model.VersionFailed},
		{CodeToolchainMissing, model.VersionFailed},
		{CodeVersionInUse, model.VersionFailed},
	} {
		f := &Failure{Code: tc.code}
		if got := f.VersionState(); got != tc.want {
			t.Errorf("Failure{%s}.VersionState() = %q, want %q", tc.code, got, tc.want)
		}
	}
}

func TestPhaseFailingStep(t *testing.T) {
	t.Parallel()

	for _, p := range Phases() {
		step := p.FailingStep()
		if !step.Valid() {
			t.Errorf("phase %q maps to %q, which is not one of model's steps", p, step)
		}
	}
	// `publish` is not in section 2.5's vocabulary and folds to `install`.
	if got := PhasePublish.FailingStep(); got != model.StepInstall {
		t.Errorf("PhasePublish.FailingStep() = %q, want install", got)
	}
	if got := PhaseCompile.FailingStep(); got != model.StepCompile {
		t.Errorf("PhaseCompile.FailingStep() = %q", got)
	}
}

func TestRequestValidate(t *testing.T) {
	t.Parallel()

	ok := Request{VersionID: "b1-cpu-src", Backend: model.BackendCPU}
	if err := ok.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got := ok.GitURLOrDefault(); got != DefaultGitURL {
		t.Errorf("GitURLOrDefault = %q", got)
	}

	for _, tc := range []struct {
		name string
		req  Request
	}{
		{"no version id", Request{Backend: model.BackendCPU}},
		{"a path in the version id", Request{VersionID: "../x", Backend: model.BackendCPU}},
		{"an unknown backend", Request{VersionID: "b1-x-src", Backend: model.Backend("rocm")}},
		{"negative jobs", Request{VersionID: "b1-cpu-src", Backend: model.BackendCPU, Jobs: -1}},
		{"an empty git url", Request{VersionID: "b1-cpu-src", Backend: model.BackendCPU, GitURL: "   "}},
	} {
		if err := tc.req.Validate(); err == nil {
			t.Errorf("%s: Validate returned nil", tc.name)
		}
	}
}
