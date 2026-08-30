package source

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jlbyh2o/llamaman/internal/hw"
	"github.com/jlbyh2o/llamaman/internal/model"
)

func TestBuildHappyPath(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	req := f.request()

	res, err := f.B.Build(t.Context(), req)
	if err != nil {
		t.Fatalf("Build: %v\n--- log ---\n%s", err, f.buildLog(req.VersionID))
	}

	t.Run("every phase reported, in order", func(t *testing.T) {
		want := []Phase{
			PhasePreflight, PhaseSpace, PhaseFetch, PhaseConfigure,
			PhaseCompile, PhaseInstall, PhaseVerify, PhasePublish,
		}
		if got := f.Obs.phases(); !slices.Equal(got, want) {
			t.Errorf("phases = %v, want %v", got, want)
		}
	})

	t.Run("ninja counters reach progress_json", func(t *testing.T) {
		ok := f.Obs.has(func(p Progress) bool {
			return p.Phase == PhaseCompile && p.Pct != nil && p.Done > 0 && p.Total == 3 && p.Jobs == 8
		})
		if !ok {
			t.Errorf("no compile frame carried ninja's [n/total] counters: %+v", f.Obs.all())
		}
	})

	t.Run("the staging tree was renamed into place", func(t *testing.T) {
		if res.VersionDir != f.B.Layout.VersionDir(req.VersionID) {
			t.Errorf("VersionDir = %q", res.VersionDir)
		}
		if !exists(t, ServerPath(res.VersionDir)) {
			t.Errorf("%s is missing", ServerPath(res.VersionDir))
		}
		if exists(t, f.B.Layout.StagingDir(req.VersionID)) {
			t.Error("the staging directory survived publish")
		}
		if exists(t, f.B.Layout.SupersededDir(req.VersionID)) {
			t.Error("a .old directory was left behind")
		}
	})

	t.Run("D78: nothing was ever installed into the version directory itself", func(t *testing.T) {
		// The install prefix is the assertion: a configure that pointed at
		// versions/<id> would let an instance start mid-`cmake --install`.
		want := "-DCMAKE_INSTALL_PREFIX=" + f.B.Layout.StagingDir(req.VersionID)
		if f.countRecorded("cmake.args", want) != 1 {
			t.Errorf("configure did not install into the staging directory:\n%s",
				strings.Join(f.recorded("cmake.args"), "\n"))
		}
	})

	t.Run("the row's columns are filled", func(t *testing.T) {
		if res.ResolvedCommit != fakeSHA {
			t.Errorf("ResolvedCommit = %q, want %q", res.ResolvedCommit, fakeSHA)
		}
		wantBins := []string{"bin/llama-bench", "bin/llama-cli", "bin/llama-server"}
		if !slices.Equal(res.Binaries, wantBins) {
			t.Errorf("Binaries = %v, want %v", res.Binaries, wantBins)
		}
		if res.SizeBytes <= 0 {
			t.Errorf("SizeBytes = %d", res.SizeBytes)
		}
		if res.HostCPUFlags != "avx avx2 avx512f" {
			t.Errorf("HostCPUFlags = %q", res.HostCPUFlags)
		}
		if res.Jobs != 8 {
			t.Errorf("Jobs = %d, want D20's min(NumCPU=8, max(2, 16 GiB/2)) = 8", res.Jobs)
		}
		if res.OOMRetried || res.Resumed {
			t.Errorf("OOMRetried = %v, Resumed = %v, want both false", res.OOMRetried, res.Resumed)
		}
	})

	t.Run("the help capture becomes help_flags_json and supports_fit", func(t *testing.T) {
		if !res.Verification.SupportsFit {
			t.Error("SupportsFit = false, but the help capture advertises --fit")
		}
		for _, want := range []string{"--fit", "--ctx-size", "-c", "--help"} {
			if !res.Verification.HelpFlags.Has(want) {
				t.Errorf("HelpFlags is missing %s: %v", want, res.Verification.HelpFlags)
			}
		}
	})

	t.Run("manifest.json describes the tree", func(t *testing.T) {
		m, err := ReadManifest(res.VersionDir)
		if err != nil {
			t.Fatalf("ReadManifest: %v", err)
		}
		if m.VersionID != req.VersionID || m.Tag != req.Tag {
			t.Errorf("manifest identity = %q/%q", m.VersionID, m.Tag)
		}
		if m.Acquisition != string(model.AcquisitionSource) {
			t.Errorf("manifest acquisition = %q", m.Acquisition)
		}
		if m.ResolvedCommit != fakeSHA {
			t.Errorf("manifest commit = %q", m.ResolvedCommit)
		}
		if !strings.Contains(m.ServerHelp, "--fit") {
			t.Error("the verbatim --help capture is not in the manifest")
		}
		for _, want := range []string{
			"-DLLAMA_BUILD_TOOLS=ON", "-DLLAMA_BUILD_TESTS=OFF", "-DLLAMA_BUILD_EXAMPLES=OFF",
			"-DLLAMA_CURL=OFF", "-DBUILD_SHARED_LIBS=ON",
			"-DCMAKE_INSTALL_RPATH=" + InstallRPATH, "-DCMAKE_BUILD_WITH_INSTALL_RPATH=ON",
		} {
			if !slices.Contains(m.CMakeFlags, want) {
				t.Errorf("manifest cmake flags are missing %s", want)
			}
		}
	})

	t.Run("the build log is durable and phase-prefixed", func(t *testing.T) {
		log := f.buildLog(req.VersionID)
		for _, want := range []string{
			"[fetch] cloning",
			"[configure] -- Configuring done",
			"[compile] [1/3] Building C object",
			"[verify] version: 6000",
			"[publish] === ready in",
		} {
			if !strings.Contains(log, want) {
				t.Errorf("the build log does not contain %q:\n%s", want, log)
			}
		}
		inside := filepath.Join(res.VersionDir, BuildLogName)
		if !exists(t, inside) {
			t.Errorf("%s is missing: the published tree carries no build log", inside)
		}
	})
}

func TestBuildCUDA(t *testing.T) {
	t.Parallel()

	cudaRequest := func(f *fixture) Request {
		req := f.request()
		req.VersionID = "b10621-cuda-src"
		req.Backend = model.BackendCUDA
		req.CUDAArchs = []string{"86", "89"}
		req.GPUs = []hw.GPU{
			{UUID: "GPU-aaaa", Name: "NVIDIA GeForce RTX 4090", ComputeCap: "8.9"},
			{UUID: "GPU-bbbb", Name: "NVIDIA GeForce RTX 3090", ComputeCap: "8.6"},
		}
		return req
	}

	t.Run("D21: the detected architectures reach cmake, and the GPUs reach the manifest", func(t *testing.T) {
		t.Parallel()

		f := newFixture(t)
		f.write("devices.txt", "Available devices:\n  CUDA0: NVIDIA GeForce RTX 4090 (24210 MiB, 23900 MiB free)\n")
		req := cudaRequest(f)

		res, err := f.B.Build(t.Context(), req)
		if err != nil {
			t.Fatalf("Build: %v\n--- log ---\n%s", err, f.buildLog(req.VersionID))
		}
		configure := f.recorded("cmake.args")[0]
		for _, want := range []string{"-DGGML_CUDA=ON", "-DCMAKE_CUDA_ARCHITECTURES=86;89"} {
			if !strings.Contains(configure, want) {
				t.Errorf("configure line is missing %q:\n%s", want, configure)
			}
		}
		for _, forbidden := range []string{"native", "all"} {
			if strings.Contains(configure, "CMAKE_CUDA_ARCHITECTURES="+forbidden) {
				t.Errorf("configure used the forbidden %q architecture list", forbidden)
			}
		}
		if res.Verification.CUDADevices != 1 {
			t.Errorf("CUDADevices = %d, want 1", res.Verification.CUDADevices)
		}
		if !strings.Contains(res.Verification.DevicesOutput, "CUDA0") {
			t.Errorf("devices_output = %q", res.Verification.DevicesOutput)
		}
		m, err := ReadManifest(res.VersionDir)
		if err != nil {
			t.Fatalf("ReadManifest: %v", err)
		}
		if len(m.GPUs) != 2 || m.GPUs[0].UUID != "GPU-aaaa" || m.CUDAArchList != "86;89" {
			t.Errorf("manifest GPUs = %+v, arch list = %q", m.GPUs, m.CUDAArchList)
		}
	})

	t.Run("D19: a CUDA build that lists no CUDA device is failed_verification", func(t *testing.T) {
		t.Parallel()

		f := newFixture(t)
		// No devices.txt: the stand-in server prints an empty device list, which
		// is what a CUDA build that silently fell back to the CPU backend looks
		// like from the outside.
		req := cudaRequest(f)

		_, err := f.B.Build(t.Context(), req)
		fail := mustFailure(t, err)
		if fail.Code != CodeNoCUDADevice || fail.Phase != PhaseVerify {
			t.Fatalf("failure = %+v, want no_cuda_device in verify", fail)
		}
		if got := fail.VersionState(); got != model.VersionFailedVerification {
			t.Errorf("VersionState() = %q, want failed_verification", got)
		}
		if exists(t, f.B.Layout.VersionDir(req.VersionID)) {
			t.Error("the version directory was published despite a failed verification")
		}
	})

	t.Run("a CUDA build with no detected architecture is refused in preflight", func(t *testing.T) {
		t.Parallel()

		f := newFixture(t)
		req := cudaRequest(f)
		req.CUDAArchs = nil
		req.GPUs = nil

		_, err := f.B.Build(t.Context(), req)
		fail := mustFailure(t, err)
		if fail.Phase != PhasePreflight || fail.Code != CodeInvalidRequest {
			t.Fatalf("failure = %+v, want invalid_request in preflight", fail)
		}
		if !strings.Contains(fail.Hint, "native") {
			t.Errorf("the hint does not explain why `native` is not the answer: %q", fail.Hint)
		}
	})
}

func TestBuildVerificationFailure(t *testing.T) {
	t.Parallel()

	f := newFixture(t, "FAKE_SERVER_BROKEN=1")
	req := f.request()

	_, err := f.B.Build(t.Context(), req)
	fail := mustFailure(t, err)
	if fail.Code != CodeFailedVerification || fail.Phase != PhaseVerify {
		t.Fatalf("failure = %+v, want failed_verification in verify", fail)
	}
	if fail.ExitCode != 127 {
		t.Errorf("ExitCode = %d, want the loader's 127", fail.ExitCode)
	}
	if got := fail.FailingStep(); got != model.StepVerify {
		t.Errorf("FailingStep() = %q, want verify", got)
	}
	if exists(t, f.B.Layout.VersionDir(req.VersionID)) {
		t.Error("a build that does not run on this host was published anyway")
	}
	if !exists(t, f.B.Layout.StagingDir(req.VersionID)) {
		t.Error("the staging tree was discarded; it is the evidence for the failure")
	}
}

func TestBuildCompileFailure(t *testing.T) {
	t.Parallel()

	f := newFixture(t, "FAKE_COMPILE=fail")
	req := f.request()

	_, err := f.B.Build(t.Context(), req)
	fail := mustFailure(t, err)
	if fail.Code != CodeCompileFailed || fail.Phase != PhaseCompile {
		t.Fatalf("failure = %+v, want compile_failed in compile", fail)
	}
	if fail.VersionState() != model.VersionFailed {
		t.Errorf("VersionState() = %q, want failed", fail.VersionState())
	}
	if fail.LogLine == 0 || !strings.Contains(fail.LogExcerpt, "error:") {
		t.Errorf("no error line was attributed: line %d, %q", fail.LogLine, fail.LogExcerpt)
	}

	// D4: the build directory is KEPT, which is the whole reason Retry can be a
	// warm `cmake --build` rather than a full rebuild.
	if !exists(t, f.B.Layout.WorktreeDir(req.VersionID)) {
		t.Error("the worktree was removed after a failure; D4's warm retry is gone")
	}
	if !f.B.CanResume(req.VersionID) {
		t.Error("CanResume = false after an interrupted build with a warm cmake cache")
	}
}

func TestBuildOOMRetry(t *testing.T) {
	t.Parallel()

	t.Run("D20: one automatic retry at -j1 turns an OOM kill into a slow success", func(t *testing.T) {
		t.Parallel()

		f := newFixture(t, "FAKE_COMPILE=oom_once")
		req := f.request()

		res, err := f.B.Build(t.Context(), req)
		if err != nil {
			t.Fatalf("Build: %v\n--- log ---\n%s", err, f.buildLog(req.VersionID))
		}
		if !res.OOMRetried {
			t.Error("Result.OOMRetried = false")
		}
		if got := f.recorded("jobs"); !slices.Equal(got, []string{"8", "1"}) {
			t.Errorf("cmake --build was run with -j %v, want [8 1]", got)
		}
		if res.Jobs != 1 {
			t.Errorf("Result.Jobs = %d, want the retry's 1", res.Jobs)
		}
		if !f.Obs.has(func(p Progress) bool { return p.OOMRetry && p.Jobs == 1 }) {
			t.Error("no progress frame told the UI the build had dropped to -j1")
		}
		log := f.buildLog(req.VersionID)
		if !strings.Contains(log, "retrying once at -j 1") {
			t.Errorf("the log does not record the retry:\n%s", log)
		}
		if !strings.Contains(log, "Killed signal terminated program") {
			t.Errorf("the log does not state the evidence the retry rested on:\n%s", log)
		}
		if !strings.Contains(log, "no matching oom-kill line") {
			t.Error("the log does not admit that the kernel log did not corroborate the kill")
		}
	})

	t.Run("an OOM that survives the retry is oom_killed, and there is only one retry", func(t *testing.T) {
		t.Parallel()

		f := newFixture(t, "FAKE_COMPILE=oom")
		req := f.request()

		_, err := f.B.Build(t.Context(), req)
		fail := mustFailure(t, err)
		if fail.Code != CodeOOMKilled {
			t.Fatalf("failure = %+v, want oom_killed", fail)
		}
		if got := f.recorded("jobs"); !slices.Equal(got, []string{"8", "1"}) {
			t.Errorf("cmake --build was run with -j %v, want exactly [8 1] — one retry, not a loop", got)
		}
	})

	t.Run("a plain compile error is never mistaken for an OOM", func(t *testing.T) {
		t.Parallel()

		f := newFixture(t, "FAKE_COMPILE=fail")
		req := f.request()

		_, err := f.B.Build(t.Context(), req)
		fail := mustFailure(t, err)
		if fail.Code != CodeCompileFailed {
			t.Fatalf("failure = %+v, want compile_failed", fail)
		}
		if got := f.recorded("jobs"); !slices.Equal(got, []string{"8"}) {
			t.Errorf("cmake --build ran %v times, want one: a syntax error is not retried at -j1", got)
		}
	})
}

func TestBuildCancellation(t *testing.T) {
	t.Parallel()

	f := newFixture(t, "FAKE_COMPILE=hang")
	req := f.request()

	ctx, cancel := context.WithCancel(t.Context())
	compiling := make(chan struct{})
	var closed bool
	req.Observer = ObserverFunc(func(_ context.Context, p Progress) error {
		if p.Phase == PhaseCompile && !closed {
			closed = true
			close(compiling)
		}
		return nil
	})

	done := make(chan error, 1)
	go func() {
		_, err := f.B.Build(ctx, req)
		done <- err
	}()

	select {
	case <-compiling:
	case <-time.After(30 * time.Second):
		t.Fatal("the build never reached the compile phase")
	}
	cancel()

	var err error
	select {
	case err = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the build did not stop after the context was canceled")
	}

	fail := mustFailure(t, err)
	if fail.Code != CodeCanceled {
		t.Fatalf("failure = %+v, want canceled", fail)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err does not unwrap to context.Canceled: %v", err)
	}
	if got := fail.VersionState(); got != model.VersionCanceled {
		t.Errorf("VersionState() = %q, want canceled", got)
	}

	// Section 6.5's cancellation rule: the worktree and the partial staging
	// directory are removed — by Discard, which the worker calls only for a
	// cancel and never for a daemon restart (that is D4's case, and it keeps
	// the tree).
	if err := f.B.Discard(context.Background(), req.VersionID); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	if exists(t, f.B.Layout.WorktreeDir(req.VersionID)) {
		t.Error("Discard left the worktree behind")
	}
	if exists(t, f.B.Layout.StagingDir(req.VersionID)) {
		t.Error("Discard left the staging directory behind")
	}
}

func TestBuildResume(t *testing.T) {
	t.Parallel()

	f := newFixture(t, "FAKE_COMPILE=fail")
	req := f.request()

	if _, err := f.B.Build(t.Context(), req); err == nil {
		t.Fatal("the first build was supposed to fail")
	}
	fetches := f.countRecorded("git.args", "worktree add")
	configures := f.countRecorded("cmake.args", "-DCMAKE_BUILD_TYPE=Release")
	if fetches != 1 || configures != 1 {
		t.Fatalf("first build: %d worktree adds, %d configures", fetches, configures)
	}

	// D4: Retry re-runs `cmake --build` against the warm objects.
	f.setEnv("FAKE_COMPILE=ok")
	req.Resume = true
	res, err := f.B.Build(t.Context(), req)
	if err != nil {
		t.Fatalf("resumed build: %v\n--- log ---\n%s", err, f.buildLog(req.VersionID))
	}
	if !res.Resumed {
		t.Error("Result.Resumed = false")
	}
	if got := f.countRecorded("git.args", "worktree add"); got != fetches {
		t.Errorf("the resumed build re-fetched: %d worktree adds, want %d", got, fetches)
	}
	if got := f.countRecorded("git.args", "clone"); got != 1 {
		t.Errorf("the resumed build cloned again (%d clones total)", got)
	}
	if got := f.countRecorded("cmake.args", "-DCMAKE_BUILD_TYPE=Release"); got != configures {
		t.Errorf("the resumed build re-configured: %d configures, want %d", got, configures)
	}
	if res.ResolvedCommit != fakeSHA {
		t.Errorf("ResolvedCommit = %q; a resumed build still records the commit it is building", res.ResolvedCommit)
	}
	if !exists(t, ServerPath(res.VersionDir)) {
		t.Error("the resumed build did not publish")
	}

	// The phase machine skips fetch's work but still REPORTS the phase, so the
	// UI's step list does not jump.
	if got := f.Obs.phases()[2]; got != PhaseFetch {
		t.Errorf("third phase = %q, want fetch", got)
	}
}

func TestBuildForcedRebuildSwap(t *testing.T) {
	t.Parallel()

	t.Run("D78: an existing version directory is replaced by two renames", func(t *testing.T) {
		t.Parallel()

		f := newFixture(t)
		req := f.request()
		if _, err := f.B.Build(t.Context(), req); err != nil {
			t.Fatalf("first build: %v", err)
		}
		versionDir := f.B.Layout.VersionDir(req.VersionID)
		marker := filepath.Join(versionDir, "from-the-first-build")
		if err := os.WriteFile(marker, []byte("x"), 0o600); err != nil {
			t.Fatalf("write marker: %v", err)
		}

		res, err := f.B.Build(t.Context(), req)
		if err != nil {
			t.Fatalf("rebuild: %v\n--- log ---\n%s", err, f.buildLog(req.VersionID))
		}
		if res.VersionDir != versionDir {
			t.Errorf("VersionDir = %q, want the same directory", res.VersionDir)
		}
		if exists(t, marker) {
			t.Error("the old tree was written over in place rather than swapped out")
		}
		if !exists(t, ServerPath(versionDir)) {
			t.Error("the rebuilt binaries are not in place")
		}
		if exists(t, f.B.Layout.SupersededDir(req.VersionID)) {
			t.Error(".old survived the swap")
		}
		if exists(t, f.B.Layout.StagingDir(req.VersionID)) {
			t.Error(".staging survived the swap")
		}
	})

	t.Run("D25: the swap is refused while a live process executes out of the directory", func(t *testing.T) {
		t.Parallel()

		f := newFixture(t)
		req := f.request()
		if _, err := f.B.Build(t.Context(), req); err != nil {
			t.Fatalf("first build: %v", err)
		}
		versionDir := f.B.Layout.VersionDir(req.VersionID)
		marker := filepath.Join(versionDir, "still-running")
		if err := os.WriteFile(marker, []byte("x"), 0o600); err != nil {
			t.Fatalf("write marker: %v", err)
		}

		f.B.Guard = stubGuard{pid: 4242, inUse: true}
		_, err := f.B.Build(t.Context(), req)
		fail := mustFailure(t, err)
		if fail.Code != CodeVersionInUse || fail.Phase != PhasePublish {
			t.Fatalf("failure = %+v, want version_in_use in publish", fail)
		}
		if !strings.Contains(fail.Message, "4242") {
			t.Errorf("the refusal does not name the process: %q", fail.Message)
		}
		if !exists(t, marker) {
			t.Error("the in-use directory was disturbed anyway")
		}
		if !exists(t, f.B.Layout.StagingDir(req.VersionID)) {
			t.Error("the finished build was discarded; it should wait in the staging directory")
		}
	})
}

func TestBuildPreflightAndSpace(t *testing.T) {
	t.Parallel()

	t.Run("a missing toolchain aborts with per-distro guidance and no package-manager call", func(t *testing.T) {
		t.Parallel()

		f := newFixture(t)
		// Force the real preflight: internal/toolchain's probe, which is the
		// same one the wizard's toolchain step and the plan endpoint read.
		f.B.Tools = Tools{}
		f.B.Toolchain = fakeProbe(f, "cmake")

		_, err := f.B.Build(t.Context(), f.request())
		fail := mustFailure(t, err)
		if fail.Code != CodeToolchainMissing || fail.Phase != PhasePreflight {
			t.Fatalf("failure = %+v, want toolchain_missing in preflight", fail)
		}
		if !strings.Contains(fail.Message, "cmake") {
			t.Errorf("the message does not name the missing tool: %q", fail.Message)
		}
		// FamilyUnknown is deliberate: it is the case where the report cannot
		// name ONE package and lists every family's instead, which is the
		// widest guidance the design's "per-distro guidance" can mean.
		for _, want := range []string{"fedora: cmake", "arch: cmake"} {
			if !strings.Contains(fail.Hint, want) {
				t.Errorf("the hint is not per-distro (%q missing): %q", want, fail.Hint)
			}
		}
		for _, forbidden := range []string{"apt install", "dnf install", "pacman -S", "sudo "} {
			if strings.Contains(fail.Hint, forbidden) {
				t.Errorf("the hint contains a package-manager COMMAND (%q), which section 6.5 forbids: %q",
					forbidden, fail.Hint)
			}
		}
	})

	t.Run("a CUDA build on a host with no nvcc is refused, and the same host builds CPU", func(t *testing.T) {
		t.Parallel()

		f := newFixture(t)
		f.B.Tools = Tools{}
		f.B.Toolchain = fakeProbe(f, "nvcc", "driver")

		cuda := f.request()
		cuda.VersionID = "b10621-cuda-src"
		cuda.Backend = model.BackendCUDA
		cuda.CUDAArchs = []string{"89"}
		_, err := f.B.Build(t.Context(), cuda)
		fail := mustFailure(t, err)
		if fail.Code != CodeToolchainMissing || !strings.Contains(fail.Message, "nvcc") {
			t.Fatalf("failure = %+v, want toolchain_missing naming nvcc", fail)
		}

		// The identical host is fine for a CPU build: nvcc is CUDA-only, and a
		// probe that refused both would make this product unusable on every
		// machine with no NVIDIA card.
		if _, err := f.B.Build(t.Context(), f.request()); err != nil {
			t.Fatalf("CPU build on the same host: %v", err)
		}
	})

	t.Run("the space floor refuses before anything is fetched", func(t *testing.T) {
		t.Parallel()

		f := newFixture(t)
		f.B.FreeSpace = func(string) (uint64, error) { return 1 * GiB, nil }

		_, err := f.B.Build(t.Context(), f.request())
		fail := mustFailure(t, err)
		if fail.Code != CodeInsufficientSpace || fail.Phase != PhaseSpace {
			t.Fatalf("failure = %+v, want insufficient_space in space", fail)
		}
		if len(f.recorded("git.args")) != 0 {
			t.Error("git ran despite the space refusal")
		}
	})

	t.Run("an unmeasurable filesystem does not refuse the build", func(t *testing.T) {
		t.Parallel()

		f := newFixture(t)
		f.B.FreeSpace = func(string) (uint64, error) { return 0, errors.New("statfs: no such thing") }

		if _, err := f.B.Build(t.Context(), f.request()); err != nil {
			t.Fatalf("Build: %v", err)
		}
	})
}

func TestBuildFetchAndConfigureFailures(t *testing.T) {
	t.Parallel()

	t.Run("a clone that cannot reach the remote fails in fetch", func(t *testing.T) {
		t.Parallel()

		f := newFixture(t, "FAKE_GIT_FAIL=clone")
		_, err := f.B.Build(t.Context(), f.request())
		fail := mustFailure(t, err)
		if fail.Code != CodeFetchFailed || fail.Phase != PhaseFetch {
			t.Fatalf("failure = %+v, want fetch_failed in fetch", fail)
		}
		if fail.ExitCode != 128 {
			t.Errorf("ExitCode = %d, want git's 128", fail.ExitCode)
		}
	})

	t.Run("a configure failure carries the hint its message earned", func(t *testing.T) {
		t.Parallel()

		f := newFixture(t, "FAKE_CONFIGURE=fail")
		_, err := f.B.Build(t.Context(), f.request())
		fail := mustFailure(t, err)
		if fail.Code != CodeConfigureFailed || fail.Phase != PhaseConfigure {
			t.Fatalf("failure = %+v, want configure_failed in configure", fail)
		}
		if !strings.Contains(fail.Hint, "CUDA toolkit") {
			t.Errorf("Hint = %q, want the nvcc guidance the message earns", fail.Hint)
		}
		if fail.LogLine == 0 || !strings.Contains(strings.ToLower(fail.LogExcerpt), "cmake error") {
			t.Errorf("no error line attributed: %d %q", fail.LogLine, fail.LogExcerpt)
		}
	})

	t.Run("an install that produces no llama-bench is install_incomplete", func(t *testing.T) {
		t.Parallel()

		f := newFixture(t, "FAKE_INSTALL=missing_bench")
		_, err := f.B.Build(t.Context(), f.request())
		fail := mustFailure(t, err)
		if fail.Code != CodeInstallIncomplete || fail.Phase != PhaseInstall {
			t.Fatalf("failure = %+v, want install_incomplete in install", fail)
		}
		if !strings.Contains(fail.Message, "llama-bench") {
			t.Errorf("the message does not name the missing binary: %q", fail.Message)
		}
	})

	t.Run("an install with no lib/ is refused, because $ORIGIN/../lib would resolve to nothing", func(t *testing.T) {
		t.Parallel()

		f := newFixture(t, "FAKE_INSTALL=no_lib")
		_, err := f.B.Build(t.Context(), f.request())
		fail := mustFailure(t, err)
		if fail.Code != CodeInstallIncomplete {
			t.Fatalf("failure = %+v, want install_incomplete", fail)
		}
	})
}

func TestBuildLogRegistryExposesTheLiveSink(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	req := f.request()

	seen := make(chan struct{})
	var once bool
	req.Observer = ObserverFunc(func(_ context.Context, p Progress) error {
		if p.Phase == PhaseCompile && !once {
			once = true
			// While the build runs, `GET …/log` must be able to find its sink
			// and follow it — the design's "in-memory ring of the last 5000
			// lines with a broadcast channel".
			sink, ok := f.B.Logs.Sink(req.VersionID)
			if !ok {
				t.Error("the running build's log sink is not in the registry")
			} else if len(sink.Tail(0)) == 0 {
				t.Error("the live sink's ring is empty mid-build")
			}
			close(seen)
		}
		return nil
	})

	if _, err := f.B.Build(t.Context(), req); err != nil {
		t.Fatalf("Build: %v", err)
	}
	<-seen
	if _, ok := f.B.Logs.Sink(req.VersionID); ok {
		t.Error("the sink is still registered after the build finished")
	}
}

func TestBuildObserverErrorsDoNotFailTheBuild(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.Obs.err = errors.New("the database is busy")

	if _, err := f.B.Build(t.Context(), f.request()); err != nil {
		t.Fatalf("Build: %v — a progress write that fails must not abandon a compile", err)
	}
}
