package source

import (
	"errors"
	"fmt"
	"strings"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// The values written to `llamacpp_versions.error_code` and `jobs.error_code`
// when a source build ends badly. They are plain strings rather than
// model.ErrorCode constants because model's enum is the API's closed error
// catalog and these are domain codes the llama.cpp service maps onto it; the
// service is the one place that knows which of them is a 409 and which is only
// a row.
const (
	// CodeInvalidRequest is a request this package refused before doing
	// anything — an unusable version id, a forbidden CUDA architecture (D21).
	CodeInvalidRequest = "invalid_request"
	// CodeToolchainMissing is the `preflight` refusal: a required tool is
	// absent or too old. The Failure's Hint carries per-distro package names
	// and NEVER a package-manager call (section 6.5).
	CodeToolchainMissing = "toolchain_missing"
	// CodeInsufficientSpace is the `space` refusal: 12 GiB for CUDA, 3 GiB for
	// CPU.
	CodeInsufficientSpace = "insufficient_space"
	// CodeFetchFailed covers clone, fetch, ref resolution, worktree and
	// submodules.
	CodeFetchFailed = "fetch_failed"
	// CodeConfigureFailed is a non-zero `cmake -S … -B …`.
	CodeConfigureFailed = "configure_failed"
	// CodeCompileFailed is a non-zero `cmake --build`.
	CodeCompileFailed = "compile_failed"
	// CodeOOMKilled is a compile the kernel killed, AFTER D20's automatic
	// single retry at -j1 also failed. A first OOM is not an error at all — it
	// is the retry.
	CodeOOMKilled = "oom_killed"
	// CodeInstallIncomplete is a staging tree missing one of the binaries
	// section 6.5's `install` phase asserts.
	CodeInstallIncomplete = "install_incomplete"
	// CodeFailedVerification is D18: the binaries do not execute on this host.
	CodeFailedVerification = "failed_verification"
	// CodeNoCUDADevice is D19: a CUDA build whose `--list-devices` reports no
	// CUDA device. A build that silently fell back to CPU is worse than no
	// build, so this is terminal.
	CodeNoCUDADevice = "no_cuda_device"
	// CodeVersionInUse is publish's refusal to swap a directory a live process
	// is executing from (D25).
	CodeVersionInUse = "version_in_use"
	// CodePublishFailed is a manifest write or a rename that failed.
	CodePublishFailed = "publish_failed"
	// CodeCanceled is the build the user stopped, or the daemon shutting down.
	// Which of the two it was is the worker's question — `cancel_requested`
	// answers it — and D4 turns on the difference: a cancel removes the
	// worktree, a daemon restart keeps it.
	CodeCanceled = "canceled"
	// CodeInternalError is a failure with no better name.
	CodeInternalError = "internal_error"
)

// Failure is how every build ends badly: the phase, the code, a message, the
// child's exit status, and the first line of the log that explains it.
//
// Section 6.5's "failure attribution" paragraph is this type: each phase records
// its exit code, the log viewer scrolls to the first line matching `error:` /
// `CMake Error` / `fatal:`, and known failures carry an actionable hint.
type Failure struct {
	// Phase is where it stopped.
	Phase Phase
	// Code is one of the constants above.
	Code string
	// Message is one sentence for a human.
	Message string
	// Hint is the actionable remedy, when this failure has a known one.
	Hint string
	// ExitCode is the failing child's status, or 0 when no child ran.
	ExitCode int
	// LogLine is the 1-based line number, within the entries this build wrote,
	// of the first line matching the error patterns — what the log viewer
	// scrolls to. Zero when nothing matched.
	LogLine int
	// LogExcerpt is that line.
	LogExcerpt string

	cause error
}

// Error renders the failure with its phase and exit status.
func (f *Failure) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "source: %s failed", f.Phase)
	if f.ExitCode != 0 {
		fmt.Fprintf(&b, " (exit %d)", f.ExitCode)
	}
	if f.Message != "" {
		b.WriteString(": ")
		b.WriteString(f.Message)
	}
	if f.LogExcerpt != "" {
		fmt.Fprintf(&b, ": %s", f.LogExcerpt)
	}
	return b.String()
}

// Unwrap exposes the underlying error, which for a canceled build is the
// context's — so errors.Is(err, context.Canceled) answers "did the user stop
// this" without inspecting Code.
func (f *Failure) Unwrap() error { return f.cause }

// FailingStep is the value for `llamacpp_versions.failing_step`.
func (f *Failure) FailingStep() model.FailingStep { return f.Phase.FailingStep() }

// VersionState is the state the version row moves to, and it is deliberately
// computed HERE rather than in the worker: section 2.5's table gives
// `failed_verification` its own edge for exactly two outcomes — a build that
// does not execute (D18) and a CUDA build that lists no CUDA device (D19) — and
// spreading that mapping over two packages is how the two of them come to
// disagree.
func (f *Failure) VersionState() model.VersionState {
	switch f.Code {
	case CodeFailedVerification, CodeNoCUDADevice:
		return model.VersionFailedVerification
	case CodeCanceled:
		return model.VersionCanceled
	default:
		return model.VersionFailed
	}
}

// AsFailure extracts a *Failure from an error chain.
func AsFailure(err error) (*Failure, bool) {
	var f *Failure
	ok := errors.As(err, &f)
	return f, ok
}

// errorLinePatterns are the three section 6.5 names, lowercased for comparison.
var errorLinePatterns = []string{"error:", "cmake error", "fatal:", "fatal error"}

// isErrorLine reports whether a line is the kind the log viewer scrolls to.
func isErrorLine(text string) bool {
	low := strings.ToLower(text)
	for _, p := range errorLinePatterns {
		if strings.Contains(low, p) {
			return true
		}
	}
	return false
}

// logScanner is section 6.5's failure attribution done as the log STREAMS
// rather than by re-reading it afterwards.
//
// The three things attribution needs — the first error line and its number, the
// actionable hint, and the line that looks like an OOM kill — are each the FIRST
// match of a predicate, so each can be decided when the line arrives and none of
// them needs the log kept in memory. That matters at the size builds actually
// reach: a CUDA build emits hundreds of thousands of lines, and re-scanning a
// 5000-line ring would in any case have found the first match only if the
// failure happened near the end.
type logScanner struct {
	lines   int
	errLine int
	errText string
	hint    string
	oomLine string
}

func (s *logScanner) observe(text string) {
	s.lines++
	if s.errLine == 0 && isErrorLine(text) {
		s.errLine, s.errText = s.lines, strings.TrimSpace(text)
	}
	if s.hint == "" {
		s.hint = hintForLine(text)
	}
	if s.oomLine == "" && isOOMLine(text) {
		s.oomLine = strings.TrimSpace(text)
	}
}

// resetOOM clears the OOM evidence before D20's retry, so the second compile is
// judged on its own output rather than on the first one's.
func (s *logScanner) resetOOM() { s.oomLine = "" }

// knownHints are section 6.5's "known failures get actionable hints" table.
// Each entry is a lowercased substring of the build output and the sentence a
// user can act on. Order matters: the first match wins, so the specific
// patterns come before the general ones.
var knownHints = []struct {
	match string
	hint  string
}{
	{
		"unsupported gpu architecture",
		"This CUDA toolkit does not support one of the detected compute capabilities. " +
			"Set the architectures explicitly in Settings → Builds, or install a newer CUDA toolkit.",
	},
	{
		"no cmake_cuda_compiler could be found",
		"The CUDA compiler (nvcc) was not found by cmake. Install the CUDA toolkit — " +
			"Debian/Ubuntu: nvidia-cuda-toolkit; Fedora: cuda-nvcc; Arch: cuda — and retry.",
	},
	{
		"could not find nvcc",
		"The CUDA compiler (nvcc) was not found by cmake. Install the CUDA toolkit — " +
			"Debian/Ubuntu: nvidia-cuda-toolkit; Fedora: cuda-nvcc; Arch: cuda — and retry.",
	},
	{
		"unsupported gnu version",
		"This CUDA toolkit refuses the host compiler's version. Install a supported gcc/g++ " +
			"alongside it and point the build at it with -DCMAKE_CUDA_HOST_COMPILER in " +
			"Settings → Builds → extra cmake flags.",
	},
	{
		"could not find curl",
		"llama.cpp asked for libcurl even though the build sets -DLLAMA_CURL=OFF; check the extra " +
			"cmake flags in Settings → Builds for a flag that turns it back on.",
	},
	{
		"c++ compiler is not able to compile",
		"The C++ compiler cannot build a test program. Install a working toolchain — " +
			"Debian/Ubuntu: build-essential; Fedora: gcc-c++; Arch: base-devel.",
	},
	{
		"no space left on device",
		"The state directory's filesystem filled up during the build. Free space and retry; " +
			"the build directory is kept, so the retry resumes against warm objects.",
	},
}

// hintForLine returns the actionable hint one line has earned, or "".
func hintForLine(text string) string {
	low := strings.ToLower(text)
	for _, h := range knownHints {
		if strings.Contains(low, h.match) {
			return h.hint
		}
	}
	return ""
}
