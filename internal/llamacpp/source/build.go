package source

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jlbyh2o/llamaman/internal/buildinfo"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/procx"
	"github.com/jlbyh2o/llamaman/internal/toolchain"
	"golang.org/x/sys/unix"
)

// BuildOptions is what went into `llamacpp_versions.build_options_json`: the
// flags this build was configured with, which is what D71 compares when a
// re-post of a `ready` id must decide between "already installed" and
// `409 version_options_differ`.
type BuildOptions struct {
	Generator       string   `json:"generator"`
	CMakeFlags      []string `json:"cmake_flags"`
	ExtraCMakeFlags []string `json:"extra_cmake_flags,omitempty"`
	CUDAArchList    string   `json:"cuda_arch_list,omitempty"`
	CCache          bool     `json:"ccache"`
	Jobs            int      `json:"jobs"`
}

// Result is everything the `llamacpp_install` worker needs to move the version
// row to `ready` (section 2.5's `verifying → ready` edge).
type Result struct {
	VersionID string
	Backend   model.Backend

	// ResolvedCommit is the 40-hex the ref resolved to
	// (`llamacpp_versions.resolved_commit`).
	ResolvedCommit string

	// VersionDir is the published directory the staging tree was renamed into.
	VersionDir string

	// Binaries and SizeBytes fill `binaries_json` and `size_bytes`.
	Binaries  []string
	SizeBytes int64

	// CUDAArchList, GPUUUIDs and HostCPUFlags fill the columns of the same
	// names, which is what makes section 6.5's "rebuild recommended" checks
	// possible later.
	CUDAArchList string
	GPUUUIDs     []string
	HostCPUFlags string

	// Verification carries `devices_output`, `help_flags_json` and
	// `supports_fit`.
	Verification Verification

	BuildOptions BuildOptions
	ManifestPath string
	LogPath      string

	// Jobs is the parallelism the compile actually ran at, and OOMRetried says
	// whether D20's automatic -j1 retry was used — the reason the UI states.
	Jobs       int
	OOMRetried bool

	// Resumed reports that D4's warm rerun happened: `fetch` and `configure`
	// were skipped because a previous attempt's worktree and cmake cache were
	// both present.
	Resumed bool

	StartedAt  time.Time
	FinishedAt time.Time
}

// Duration is how long the build took.
func (r *Result) Duration() time.Duration { return r.FinishedAt.Sub(r.StartedAt) }

// Builder runs DESIGN section 6.5's pipeline. Every piece of the host it
// depends on is a field, so the whole phase machine can be exercised against a
// fake toolchain — which is exactly what section 15 asks for ("a fake source
// tree whose cmake is a shell script … this exercises the whole phase machine …
// without compiling llama.cpp, and it is why this area can be tested at all").
//
// A Builder is a value: Build copies it before it mutates anything, so one
// Builder can serve every build the daemon runs. That is belt and braces —
// D70's `build_lease` already makes two concurrent builds impossible — but it
// means no field here is a shared mutable.
type Builder struct {
	// Layout resolves every path under the resolved state directory (D72).
	Layout Layout

	// Logs, when set, holds the live sink of the running build so the log
	// endpoint can follow it (section 3.5's `GET …/log`).
	Logs *LogRegistry

	// Tools presets the toolchain instead of probing for it. Empty means the
	// preflight phase asks internal/toolchain, which is the same probe the
	// wizard's toolchain step and `GET /api/v1/llamacpp/plan` read.
	Tools Tools

	// Toolchain configures that probe. The zero value probes this host.
	Toolchain toolchain.Options

	// Guard answers D25's "is a live process executing out of this directory",
	// asked again immediately before a forced rebuild's swap. Nil means
	// ProcExeGuard over /proc.
	Guard DirGuard

	// OOM corroborates a suspected OOM kill against the kernel log (D20). Nil
	// means KmsgWatcher over /dev/kmsg; a host where that cannot be read still
	// gets the retry, with the reason saying the corroboration was unavailable.
	OOM OOMWatcher

	// NumCPU and MemAvailable are D20's two inputs. Nil means runtime.NumCPU
	// and /proc/meminfo.
	NumCPU       func() int
	MemAvailable func() (uint64, error)

	// FreeSpace reports the bytes available on the filesystem holding a path
	// (the `space` phase). Nil means statfs.
	FreeSpace func(path string) (uint64, error)

	// CPUFlags reads the host's CPU feature set for the GGML_NATIVE
	// rebuild-recommended check. Nil means /proc/cpuinfo.
	CPUFlags func() (string, error)

	// Env is added to every child process's environment.
	Env []string

	// Grace is how long a child has to exit after SIGTERM before SIGKILL.
	// Zero means procx.DefaultGrace, which is section 6.5's ten seconds.
	Grace time.Duration

	// RingLines caps the in-memory log tail; zero means DefaultRingLines.
	RingLines int

	// Now is the clock; nil means time.Now.
	Now func() time.Time

	// Logger is the daemon's slog; nil discards.
	Logger *slog.Logger

	// Per-build state, set by Build on its own copy.
	tools   Tools
	scanner logScanner
}

// Build runs the pipeline for one version and returns what the version row
// needs. Every failure is a *Failure naming the phase, the code, the exit
// status and the first line of the log that explains it.
func (b *Builder) Build(ctx context.Context, req Request) (*Result, error) {
	if err := req.Validate(); err != nil {
		return nil, &Failure{
			Phase: PhasePreflight, Code: CodeInvalidRequest,
			Message: err.Error(), cause: err,
		}
	}

	// A per-build copy: the Builder the caller holds stays immutable.
	r := *b
	r.tools = b.Tools

	res := &Result{
		VersionID:    req.VersionID,
		Backend:      req.Backend,
		CUDAArchList: req.CUDAArchList(),
		GPUUUIDs:     req.GPUUUIDs(),
		StartedAt:    r.now(),
		LogPath:      r.Layout.LogPath(req.VersionID),
	}

	sink, err := OpenLog(res.LogPath, r.RingLines)
	if err != nil {
		return nil, &Failure{
			Phase: PhasePreflight, Code: CodeInternalError,
			Message: "cannot open the build log", cause: err,
		}
	}
	r.Logs.put(req.VersionID, sink)
	defer func() {
		r.Logs.drop(req.VersionID)
		if err := sink.Close(); err != nil {
			r.log().Warn("closing the build log failed", "version_id", req.VersionID, "error", err)
		}
	}()

	sink.SetPhase(PhasePreflight)
	sink.Printf("=== llamaman source build of %s started %s",
		req.VersionID, res.StartedAt.UTC().Format(time.RFC3339))
	sink.Printf("=== %s at %s, backend %s",
		RedactGitURL(req.GitURLOrDefault()), displayRef(req.GitRef), req.Backend)

	err = r.pipeline(ctx, req, res, sink)
	res.FinishedAt = r.now()

	if err != nil {
		f := r.attribute(ctx, err)
		sink.Printf("=== FAILED in %s after %s: %s", f.Phase, res.Duration().Round(time.Second), f.Message)
		if f.Hint != "" {
			sink.Printf("=== hint: %s", f.Hint)
		}
		return res, f
	}
	sink.Printf("=== ready in %s: %s", res.Duration().Round(time.Second), res.VersionDir)
	return res, nil
}

func (b *Builder) pipeline(ctx context.Context, req Request, res *Result, sink *LogSink) error {
	if err := b.preflight(ctx, req, sink); err != nil {
		return err
	}
	if err := b.space(ctx, req, sink); err != nil {
		return err
	}

	resume := req.Resume && b.CanResume(req.VersionID)
	res.Resumed = resume

	if resume {
		sink.SetPhase(PhaseFetch)
		b.progress(ctx, req, Progress{Phase: PhaseFetch, Message: "resuming against the existing worktree"})
		sink.Printf("resuming: the worktree and cmake cache from the interrupted build are both present, " +
			"so fetch and configure are skipped and the build re-runs against warm objects (D4)")
		out, err := b.capture(ctx, nil, b.tools.Git, "-C", b.Layout.WorktreeDir(req.VersionID), "rev-parse", "HEAD")
		if err == nil {
			res.ResolvedCommit = strings.TrimSpace(out)
		}
	} else {
		sink.SetPhase(PhaseFetch)
		b.progress(ctx, req, Progress{Phase: PhaseFetch, Message: "fetching sources"})
		commit, err := b.fetch(ctx, req, sink)
		if err != nil {
			return err
		}
		res.ResolvedCommit = commit
	}

	opts := ConfigureOptions{
		SourceDir:     b.Layout.WorktreeDir(req.VersionID),
		BuildDir:      b.Layout.CMakeDir(req.VersionID),
		InstallPrefix: b.Layout.StagingDir(req.VersionID),
		Generator:     b.tools.Generator(),
		Backend:       req.Backend,
		CUDAArchs:     req.CUDAArchs,
		CCache:        b.tools.CCache,
		ExtraFlags:    req.ExtraCMakeFlags,
	}
	args := ConfigureArgs(opts)
	res.BuildOptions = BuildOptions{
		Generator:       opts.Generator,
		CMakeFlags:      args,
		ExtraCMakeFlags: req.ExtraCMakeFlags,
		CUDAArchList:    req.CUDAArchList(),
		CCache:          b.tools.CCache != "",
	}

	if !resume {
		sink.SetPhase(PhaseConfigure)
		b.progress(ctx, req, Progress{Phase: PhaseConfigure, Message: "running cmake"})
		sink.Printf("+ %s %s", b.tools.CMake, strings.Join(args, " "))
		if _, err := b.exec(ctx, sink, nil, b.tools.CMake, args...); err != nil {
			return &Failure{
				Phase: PhaseConfigure, Code: CodeConfigureFailed,
				Message: "cmake could not configure the build", ExitCode: exitCodeOf(err), cause: err,
			}
		}
	}

	if err := b.compile(ctx, req, res, sink); err != nil {
		return err
	}
	if err := b.install(ctx, req, sink); err != nil {
		return err
	}

	sink.SetPhase(PhaseVerify)
	b.progress(ctx, req, Progress{Phase: PhaseVerify, Message: "checking the binaries on this host"})
	staging := b.Layout.StagingDir(req.VersionID)
	v, err := b.verify(ctx, req, staging, sink)
	res.Verification = v
	if err != nil {
		return err
	}
	return b.publish(ctx, req, res, sink)
}

// preflight is section 6.5's first row: probe the toolchain and abort with
// per-distro guidance — never a package-manager call — when a piece is missing.
func (b *Builder) preflight(ctx context.Context, req Request, sink *LogSink) error {
	sink.SetPhase(PhasePreflight)
	b.progress(ctx, req, Progress{Phase: PhasePreflight, Message: "probing the toolchain"})

	if b.tools.CMake == "" || b.tools.Git == "" {
		tools, report, err := probeToolchain(ctx, b.Toolchain, req.Backend)
		b.tools = tools
		sink.Printf("toolchain probe: %s", report.Summary)
		if err != nil {
			return err
		}
	}
	sink.Printf("toolchain: git=%s cmake=%s (%s) generator=%s ccache=%s nvcc=%s",
		b.tools.Git, b.tools.CMake, b.tools.CMakeVersion, b.tools.Generator(),
		orNone(b.tools.CCache), orNone(b.tools.NVCC))

	if req.Backend == model.BackendCUDA && len(req.CUDAArchs) == 0 {
		return &Failure{
			Phase: PhasePreflight,
			Code:  CodeInvalidRequest,
			Message: "no CUDA compute capability was detected, so there is nothing to set " +
				"CMAKE_CUDA_ARCHITECTURES to",
			Hint: "Check that nvidia-smi reports a GPU, or set the architectures explicitly in " +
				"Settings → Builds. `native` and `all` are deliberately not accepted (D21): the first " +
				"produces a binary that silently will not run if the GPU set changes, and the second " +
				"multiplies compile time.",
		}
	}
	if req.Backend == model.BackendCUDA {
		sink.Printf("CUDA architectures: %s", req.CUDAArchList())
	}
	return nil
}

// space is section 6.5's second row: 12 GiB free for CUDA, 3 GiB for CPU.
func (b *Builder) space(ctx context.Context, req Request, sink *LogSink) error {
	sink.SetPhase(PhaseSpace)
	b.progress(ctx, req, Progress{Phase: PhaseSpace, Message: "checking free space"})

	// The floor lives in internal/toolchain because `GET /api/v1/llamacpp/plan`
	// reports `free_space_ok` from the same number before any build exists, and
	// two copies of a threshold is one copy too many.
	need := uint64(toolchain.RequiredFreeBytes(req.Backend))
	free, err := b.freeSpace()(b.Layout.StateDir)
	if err != nil {
		// Not knowing is not a refusal: a filesystem that cannot answer statfs
		// still builds, and the compile's own "No space left on device" is a
		// perfectly clear failure with a hint attached (see knownHints).
		sink.Printf("note: could not measure free space on %s (%v); continuing", b.Layout.StateDir, err)
		return nil
	}
	sink.Printf("free space: %.1f GiB available, %.1f GiB required for a %s build",
		float64(free)/GiB, float64(need)/GiB, req.Backend)
	if free < need {
		return &Failure{
			Phase: PhaseSpace,
			Code:  CodeInsufficientSpace,
			Message: fmt.Sprintf("%.1f GiB free on %s, but a %s build needs %.1f GiB",
				float64(free)/GiB, b.Layout.StateDir, req.Backend, float64(need)/GiB),
			Hint: "Free space in the state directory's filesystem — old llama.cpp versions under " +
				"versions/ and finished build directories under build/ are the usual candidates — and retry.",
		}
	}
	return nil
}

// compile is section 6.5's `compile` row plus D20's automatic retry.
func (b *Builder) compile(ctx context.Context, req Request, res *Result, sink *LogSink) error {
	sink.SetPhase(PhaseCompile)

	jobs := req.Jobs
	source := "settings.llamacpp.build_jobs"
	if jobs <= 0 {
		mem, err := b.memAvailable()()
		if err != nil {
			sink.Printf("note: could not read MemAvailable (%v); sizing the build from the CPU count alone", err)
		}
		jobs = BuildJobs(b.numCPU()(), mem)
		source = fmt.Sprintf("min(NumCPU=%d, max(2, MemAvailable=%.1f GiB / 2))", b.numCPU()(), float64(mem)/GiB)
	}
	res.Jobs = jobs
	res.BuildOptions.Jobs = jobs
	sink.Printf("compiling with -j %d (%s)", jobs, source)

	// Only this phase's output may be read as OOM evidence: a line that matched
	// during fetch or configure must not make the first compile failure look
	// like a memory kill.
	b.scanner.resetOOM()
	started := b.now()
	err := b.runCompile(ctx, req, res, sink, jobs, false)
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return err
	}

	suspicion := suspectOOM(resultOf(err), b.scanner.oomLine, b.kernelVerdict(ctx, started))
	if !suspicion.Suspected || jobs <= 1 {
		code := CodeCompileFailed
		if suspicion.Suspected {
			code = CodeOOMKilled
		}
		return &Failure{
			Phase: PhaseCompile, Code: code,
			Message:  compileMessage(code, jobs, suspicion),
			ExitCode: exitCodeOf(err), cause: err,
		}
	}

	// D20's one automatic retry. It converts the most common workstation build
	// failure from a hard error into a slow success, and the UI states why.
	res.OOMRetried = true
	res.Jobs, res.BuildOptions.Jobs = 1, 1
	b.scanner.resetOOM()
	sink.Printf("=== the compile was killed and it looks like the kernel ran out of memory: %s", suspicion.Reason)
	sink.Printf("=== retrying once at -j 1 (D20); this is slower but survives a memory-starved host")

	retryStarted := b.now()
	if err := b.runCompile(ctx, req, res, sink, 1, true); err != nil {
		if ctx.Err() != nil {
			return err
		}
		retry := suspectOOM(resultOf(err), b.scanner.oomLine, b.kernelVerdict(ctx, retryStarted))
		code := CodeCompileFailed
		if retry.Suspected {
			code = CodeOOMKilled
		}
		return &Failure{
			Phase: PhaseCompile, Code: code,
			Message:  compileMessage(code, 1, retry),
			ExitCode: exitCodeOf(err), cause: err,
		}
	}
	return nil
}

func compileMessage(code string, jobs int, s OOMSuspicion) string {
	if code == CodeOOMKilled {
		return fmt.Sprintf("the compile was killed for lack of memory even at -j %d: %s", jobs, s.Reason)
	}
	return fmt.Sprintf("the compile failed at -j %d", jobs)
}

// runCompile executes one `cmake --build` and streams ninja's counters into
// jobs.progress_json.
func (b *Builder) runCompile(ctx context.Context, req Request, res *Result, sink *LogSink, jobs int, retry bool) error {
	args := BuildArgs(b.Layout.CMakeDir(req.VersionID), jobs)
	sink.Printf("+ %s %s", b.tools.CMake, strings.Join(args, " "))

	var (
		lastPct  = -1
		lastSent time.Time
	)
	b.progress(ctx, req, Progress{Phase: PhaseCompile, Jobs: jobs, OOMRetry: retry})

	onLine := func(l procx.Line) {
		done, total, pct, ok := parseBuildProgress(l.Text)
		if !ok {
			return
		}
		now := b.now()
		// One progress write per percent, and never more than one every 500 ms:
		// a large build emits thousands of these lines and every one of them
		// would otherwise be a database write.
		if pct == lastPct && now.Sub(lastSent) < 500*time.Millisecond {
			return
		}
		lastPct, lastSent = pct, now
		p := pct
		b.progress(ctx, req, Progress{
			Phase: PhaseCompile, Pct: &p, Done: done, Total: total,
			Jobs: jobs, OOMRetry: retry,
		})
	}

	_, err := b.exec(ctx, sink, onLine, b.tools.CMake, args...)
	return err
}

// install is section 6.5's `install` row: into `versions/<id>.staging`, never
// into `versions/<id>` (D78).
func (b *Builder) install(ctx context.Context, req Request, sink *LogSink) error {
	sink.SetPhase(PhaseInstall)
	b.progress(ctx, req, Progress{Phase: PhaseInstall, Message: "installing into the staging directory"})

	staging := b.Layout.StagingDir(req.VersionID)
	// A staging tree left by an earlier attempt must not contribute files to
	// this one: a stale binary that this build no longer produces would
	// otherwise be published as if it had just been built.
	if err := os.RemoveAll(staging); err != nil {
		return &Failure{
			Phase: PhaseInstall, Code: CodeInstallIncomplete,
			Message: fmt.Sprintf("cannot clear the staging directory %s", staging), cause: err,
		}
	}

	args := InstallArgs(b.Layout.CMakeDir(req.VersionID))
	sink.Printf("+ %s %s", b.tools.CMake, strings.Join(args, " "))
	if _, err := b.exec(ctx, sink, nil, b.tools.CMake, args...); err != nil {
		return &Failure{
			Phase: PhaseInstall, Code: CodeInstallIncomplete,
			Message: "cmake --install failed", ExitCode: exitCodeOf(err), cause: err,
		}
	}
	return assertInstalled(staging)
}

// publish is section 6.5's last row: manifest, build log, then the rename that
// makes the tree a version (D78).
func (b *Builder) publish(ctx context.Context, req Request, res *Result, sink *LogSink) error {
	sink.SetPhase(PhasePublish)
	b.progress(ctx, req, Progress{Phase: PhasePublish, Message: "publishing the build"})

	staging := b.Layout.StagingDir(req.VersionID)
	binaries, size, err := dirStats(staging)
	if err != nil {
		return &Failure{
			Phase: PhasePublish, Code: CodePublishFailed,
			Message: "cannot measure the staging tree", cause: err,
		}
	}
	res.Binaries, res.SizeBytes = binaries, size

	if flags, err := b.cpuFlags()(); err != nil {
		sink.Printf("note: could not read the host CPU flags (%v)", err)
	} else {
		res.HostCPUFlags = flags
	}

	m := Manifest{
		ManifestVersion: ManifestVersion,
		VersionID:       req.VersionID,
		Tag:             req.Tag,
		BuildTag:        req.BuildTag,
		Channel:         string(req.Channel),
		Acquisition:     acquisition,
		Backend:         string(req.Backend),
		// Redacted: manifest.json is a durable file inside the version
		// directory, and DESIGN sections 2.2/7.1 keep a credential out of every
		// such place (giturl.go).
		GitURL:         RedactGitURL(req.GitURLOrDefault()),
		GitRef:         req.GitRef,
		ResolvedCommit: res.ResolvedCommit,
		BuiltAt:        b.now().UTC(),
		BuiltBy:        buildinfo.Version,
		Generator:      res.BuildOptions.Generator,
		CMakeVersion:   b.tools.CMakeVersion,
		CCache:         res.BuildOptions.CCache,
		Jobs:           res.Jobs,
		OOMRetried:     res.OOMRetried,
		CMakeFlags:     res.BuildOptions.CMakeFlags,
		CUDAArchList:   res.CUDAArchList,
		GPUs:           manifestGPUs(req),
		HostCPUFlags:   res.HostCPUFlags,
		Binaries:       binaries,
		SizeBytes:      size,
		ServerHelp:     res.Verification.HelpOutput,
		HelpFlags:      res.Verification.HelpFlags,
		SupportsFit:    res.Verification.SupportsFit,
		DevicesOutput:  res.Verification.DevicesOutput,
		VersionOutput:  res.Verification.VersionOutput,
	}
	if err := WriteManifest(staging, m); err != nil {
		return &Failure{
			Phase: PhasePublish, Code: CodePublishFailed,
			Message: "cannot write manifest.json", cause: err,
		}
	}

	// The durable log is copied INTO the staging tree before the rename, so the
	// published directory carries its own build.log (section 6.5's destination
	// (b)) without anything ever being written into a directory
	// `versions/active` can resolve into.
	if err := sink.CopyTo(filepath.Join(staging, BuildLogName)); err != nil {
		sink.Printf("note: could not copy the build log into the version directory (%v)", err)
	}

	if err := b.publishDir(ctx, req.VersionID); err != nil {
		return err
	}
	res.VersionDir = b.Layout.VersionDir(req.VersionID)
	res.ManifestPath = ManifestPath(res.VersionDir)
	return nil
}

// CanResume reports whether D4's warm rerun is available for a version id: an
// interrupted build left both its worktree and its cmake cache behind.
func (b *Builder) CanResume(versionID string) bool {
	if err := ValidateVersionID(versionID); err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(b.Layout.WorktreeDir(versionID), ".git")); err != nil {
		return false
	}
	_, err := os.Stat(filepath.Join(b.Layout.CMakeDir(versionID), "CMakeCache.txt"))
	return err == nil
}

// Discard removes what a CANCELED build leaves behind: the worktree and the
// partial staging directory (section 6.5's cancellation rule).
//
// It is deliberately not called on every failure. D4 keeps the build directory
// exactly so that Retry re-runs `cmake --build` against warm objects — minutes
// rather than a full CUDA rebuild — so the worker calls this only when the job
// was canceled by a user, and never when the daemon merely restarted.
func (b *Builder) Discard(ctx context.Context, versionID string) error {
	if err := ValidateVersionID(versionID); err != nil {
		return err
	}
	r := *b
	r.tools = b.Tools
	if r.tools.Git == "" {
		if p, err := r.lookPath()("git"); err == nil {
			r.tools.Git = p
		}
	}

	var errs []error
	if err := r.removeWorktree(ctx, versionID); err != nil {
		errs = append(errs, err)
	}
	staging := r.Layout.StagingDir(versionID)
	if err := os.RemoveAll(staging); err != nil {
		errs = append(errs, fmt.Errorf("source: remove %s: %w", staging, err))
	}
	return errors.Join(errs...)
}

// attribute turns any error the pipeline returned into a *Failure carrying the
// log line the viewer should scroll to and the hint the failure has earned
// (section 6.5's "failure attribution").
func (b *Builder) attribute(ctx context.Context, err error) *Failure {
	f, ok := AsFailure(err)
	if !ok {
		f = &Failure{
			Phase: PhaseCompile, Code: CodeInternalError,
			Message: err.Error(), ExitCode: exitCodeOf(err), cause: err,
		}
	}
	// A canceled build is not a failed one, and only the context can say which
	// happened: a `cmake --build` killed by the cancellation exits non-zero
	// exactly like one that hit a compile error.
	if ctxErr := ctx.Err(); ctxErr != nil {
		f.Code = CodeCanceled
		f.Message = "the build was stopped"
		f.Hint = ""
		if f.cause == nil {
			f.cause = ctxErr
		}
		return f
	}
	if f.LogLine == 0 {
		f.LogLine, f.LogExcerpt = b.scanner.errLine, b.scanner.errText
	}
	if f.Hint == "" {
		f.Hint = b.scanner.hint
	}
	return f
}

// exec runs one child, streaming its merged output into the build log, into the
// failure scanner, and into onLine when the caller wants the lines too.
func (b *Builder) exec(ctx context.Context, sink *LogSink, onLine func(procx.Line), path string, args ...string) (procx.Result, error) {
	if path == "" {
		return procx.Result{}, errors.New("source: no binary to run (the toolchain was not resolved)")
	}
	cmd := procx.Cmd{
		Path:     path,
		Args:     args,
		ExtraEnv: append(append([]string{}, gitEnv...), b.Env...),
		Grace:    b.Grace,
		Now:      b.Now,
		OnLine: func(l procx.Line) {
			sink.Line(l)
			b.scanner.observe(l.Text)
			if onLine != nil {
				onLine(l)
			}
		},
	}
	return procx.Run(ctx, cmd)
}

// capture runs one child and returns its merged output, still streaming it into
// the build log when a sink is given — which is what puts `cmake --version` and
// `llama-server --list-devices` in build.log above the step that used them.
func (b *Builder) capture(ctx context.Context, sink *LogSink, path string, args ...string) (string, error) {
	var out strings.Builder
	_, err := b.exec(ctx, sink, func(l procx.Line) {
		out.WriteString(l.Text)
		out.WriteByte('\n')
	}, path, args...)
	return out.String(), err
}

func (b *Builder) progress(ctx context.Context, req Request, p Progress) {
	if req.Observer == nil {
		return
	}
	if err := req.Observer.Progress(ctx, p); err != nil {
		// A progress write that fails is a database problem, and abandoning a
		// CUDA compile eight minutes in because of one would be the wrong
		// trade.
		b.log().Warn("writing build progress failed",
			"version_id", req.VersionID, "phase", p.Phase, "error", err)
	}
}

func (b *Builder) kernelVerdict(ctx context.Context, since time.Time) kernelVerdict {
	w := b.OOM
	if w == nil {
		w = KmsgWatcher{}
	}
	confirmed, err := w.OOMKillSince(ctx, since)
	return kernelVerdict{confirmed: confirmed, err: err}
}

func (b *Builder) guard() DirGuard {
	if b.Guard != nil {
		return b.Guard
	}
	return ProcExeGuard{}
}

// lookPath resolves a binary the way the toolchain probe would, so a test that
// substitutes one substitutes both.
func (b *Builder) lookPath() func(string) (string, error) {
	if b.Toolchain.LookPath != nil {
		return b.Toolchain.LookPath
	}
	return exec.LookPath
}

func (b *Builder) numCPU() func() int {
	if b.NumCPU != nil {
		return b.NumCPU
	}
	return runtime.NumCPU
}

func (b *Builder) memAvailable() func() (uint64, error) {
	if b.MemAvailable != nil {
		return b.MemAvailable
	}
	return func() (uint64, error) { return MemAvailableBytes("/proc/meminfo") }
}

func (b *Builder) freeSpace() func(string) (uint64, error) {
	if b.FreeSpace != nil {
		return b.FreeSpace
	}
	return FreeSpaceBytes
}

func (b *Builder) cpuFlags() func() (string, error) {
	if b.CPUFlags != nil {
		return b.CPUFlags
	}
	return func() (string, error) { return HostCPUFlags("/proc/cpuinfo") }
}

func (b *Builder) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

func (b *Builder) log() *slog.Logger {
	if b.Logger != nil {
		return b.Logger
	}
	return slog.New(slog.DiscardHandler)
}

// FreeSpaceBytes reports the bytes available to an unprivileged writer on the
// filesystem holding path — Bavail rather than Bfree, because the blocks
// reserved for root are not blocks this daemon can use.
func FreeSpaceBytes(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("source: statfs %s: %w", path, err)
	}
	return st.Bavail * uint64(st.Bsize), nil
}

func exitCodeOf(err error) int {
	var ee *procx.ExitError
	if errors.As(err, &ee) {
		return ee.Result.ExitCode
	}
	return 0
}

func resultOf(err error) procx.Result {
	var ee *procx.ExitError
	if errors.As(err, &ee) {
		return ee.Result
	}
	return procx.Result{}
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
