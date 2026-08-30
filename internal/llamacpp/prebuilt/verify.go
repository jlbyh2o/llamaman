package prebuilt

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/procx"
	"github.com/jlbyh2o/llamaman/internal/toolchain"
)

// The acceptance test (DESIGN section 6.4 step 3, D18 and D19).
//
// A tarball is `ready` only after `bin/llama-server --version` exits 0 ON THIS
// HOST. Nothing else is a substitute: the file exists, it is the right
// architecture, its permissions are right, and it still does not run, because
// upstream built it against a newer glibc than this distribution ships. That
// single execution is the difference between SPEC section 3.7's distro-agnostic
// promise and a product that works on Ubuntu.
//
// The three sub-checks, and why each is shaped the way it is:
//
//	llama-server --version   MUST exit 0 (D18). On failure the ELF is diagnosed
//	                         and the row goes to `failed_verification`, with a
//	                         SOURCE build of the same tag enqueued in its place.
//	llama-bench              is asserted by `stat` plus a `-h` run whose output
//	                         mentions `llama-bench`. It is deliberately NOT
//	                         required to exit 0: llama-bench has its own
//	                         argument parser and an unrecognized flag exits
//	                         non-zero, so a hard exit-status check here would
//	                         reject working builds (section 6.4 step 3).
//	llama-server --list-devices  must report at least one CUDA device for a CUDA
//	                         build (D19). A CUDA build that silently fell back
//	                         to CPU is worse than no build.
//
// The `--help` capture happens here too, because this is the one moment the
// binaries are known-good and already being executed. Section 5.7 turns it into
// two COLUMNS — `help_flags_json` and `supports_fit` — precisely so that
// RenderArgv stays pure and never opens `manifest.json`.
//
// **On D49's third invariant.** This file execs a `versions/*/bin/*` binary
// with flags, which is the shape that invariant polices — "only
// internal/instances turns a FlagSet into a llama.cpp command line". These four
// invocations are not that, and the distinction is exactly the one the
// invariant is drawing: `--version`, `--help`, `--list-devices` and `-h` are
// FIXED probe arguments that section 6.4 step 3 names literally, carry no user
// configuration, and produce no instance. Nothing here reads a FlagSet, a
// model path or an instance row; a build that could not answer these four
// questions could not be installed at all. An import-graph test written against
// that invariant should scope itself to argv derived from `model.FlagSet`, not
// to every `-` in an exec slice, or it will flag this file, its counterpart in
// internal/llamacpp/source, and nothing that is actually a rendering.

// Binary names inside a version directory.
const (
	BinServer = "llama-server"
	BinBench  = "llama-bench"
	BinCLI    = "llama-cli"
)

// FitFlag is the flag whose presence in `llama-server --help` sets
// `supports_fit` (section 2.5, D51). A build that predates it renders
// `-ngl auto` as `-ngl 999` instead of omitting the flag.
const FitFlag = "--fit"

// VerifyOptions configures the acceptance test.
type VerifyOptions struct {
	// Root is the version directory — the STAGING directory during an install,
	// which is the whole point of D78: nothing is verified in a directory
	// `versions/active` can resolve into.
	Root string
	// Backend decides whether D19's CUDA device check runs.
	Backend model.Backend
	// HostLibc is this host's C library, used for the glibc diagnosis. The zero
	// value probes it.
	HostLibc toolchain.Libc
	// Run executes one binary. Nil uses internal/procx. The seam exists so the whole
	// acceptance path — including the failure that triggers the source-build
	// fallback — is testable on a host with no llama.cpp at all.
	Run Runner
	// Timeout bounds one probe. Zero uses 60 s: `--version` is instant, but a
	// first run of a CUDA binary initializes the driver and can take seconds.
	Timeout time.Duration
}

// Runner executes one command and returns its merged output and exit status. An
// error is returned only when the process could not be started.
type Runner func(ctx context.Context, name string, args ...string) (output string, code int, err error)

// VerifyResult is the acceptance test's verdict and everything it learned. The
// fields map onto section 2.5's columns: `devices_output`, `help_flags_json`,
// `supports_fit`, and — on failure — `error_code`, `error_message` and the
// diagnosis carried into the replacement row's `params_json`.
type VerifyResult struct {
	OK bool
	// VersionOutput is `llama-server --version` verbatim.
	VersionOutput string
	// HelpOutput is the `llama-server --help` capture section 2.5 stores in
	// manifest.json verbatim.
	HelpOutput string
	// HelpFlags is the parsed flag set that becomes `help_flags_json`.
	HelpFlags model.HelpFlags
	// SupportsFit is derived from HelpFlags: does this build know `--fit`.
	SupportsFit bool
	// DevicesOutput is `llama-server --list-devices` verbatim, section 2.5's
	// `devices_output` (D19). Captured for every backend, because "which
	// devices did this build see" is worth having on a CPU build too.
	DevicesOutput string
	// Binaries are the tool names found under bin/, sorted.
	Binaries []string
	// FailingCheck names which check failed: "execute", "llama-bench",
	// "cuda-devices", or empty on success.
	FailingCheck string
	// Diagnosis is filled when the execute check failed.
	Diagnosis *Diagnosis
	// SourceFallback is D18's signal: this failure is one a source build of the
	// same tag and backend is expected to fix, so the caller inserts a NEW row
	// `<tag>-<backend>-src` beside this one and links them through
	// `superseded_by`. It is false for a failure a rebuild cannot fix — a
	// CUDA build that lists no device is terminal (D19), because rebuilding it
	// on the same host would list no device either.
	SourceFallback bool
	// Err is the reason, suitable for `error_message`.
	Err error
}

// ErrVerification is the class of every acceptance failure.
var ErrVerification = errors.New("prebuilt: verification failed")

// Verify runs the acceptance test against a version tree.
//
// It returns an error only for something that stopped the test from running at
// all. A binary that ran and failed is a RESULT — `OK: false` with a diagnosis
// — because that outcome has a defined place in the state machine
// (`failed_verification` plus a source build) and turning it into a Go error
// would lose the diagnosis on the way up.
func Verify(ctx context.Context, opts VerifyOptions) (VerifyResult, error) {
	if opts.Root == "" {
		return VerifyResult{}, errors.New("prebuilt: Verify needs a root directory")
	}
	run := opts.Run
	if run == nil {
		run = execRun
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	hostLibc := opts.HostLibc
	if hostLibc.Kind == "" {
		hostLibc = toolchain.Glibc(ctx)
	}

	var res VerifyResult
	bins, err := listBinaries(opts.Root)
	if err != nil {
		return res, err
	}
	res.Binaries = bins

	server := filepath.Join(opts.Root, "bin", BinServer)
	if _, err := os.Stat(server); err != nil {
		res.FailingCheck = "execute"
		res.Err = fmt.Errorf("%w: %s is missing from the archive", ErrVerification, filepath.Join("bin", BinServer))
		// A tarball with no server binary is not something a source build of
		// the same tag would fail at; it is a broken or wrong download.
		return res, nil
	}

	// --- D18: it must execute on THIS host -----------------------------------
	out, code, err := runWithTimeout(ctx, run, timeout, server, "--version")
	res.VersionOutput = out
	if err != nil || code != 0 {
		d := Diagnose(server, hostLibc)
		res.Diagnosis = &d
		res.FailingCheck = "execute"
		res.SourceFallback = true
		res.Err = fmt.Errorf("%w: `%s --version` %s: %s",
			ErrVerification, filepath.Join("bin", BinServer), exitDescription(code, err), d.Summary)
		return res, nil
	}

	// --- the help capture, section 5.7's two columns --------------------------
	help, _, err := runWithTimeout(ctx, run, timeout, server, "--help")
	if err == nil {
		res.HelpOutput = help
		res.HelpFlags = ParseHelpFlags(help)
		res.SupportsFit = res.HelpFlags.Has(FitFlag)
	}

	// --- devices, and D19 for a CUDA build ------------------------------------
	devices, devCode, devErr := runWithTimeout(ctx, run, timeout, server, "--list-devices")
	if devErr == nil {
		res.DevicesOutput = devices
	}
	if opts.Backend == model.BackendCUDA {
		switch {
		case devErr != nil || devCode != 0:
			res.FailingCheck = "cuda-devices"
			res.Err = fmt.Errorf("%w: `%s --list-devices` %s, so the CUDA devices could not be confirmed",
				ErrVerification, filepath.Join("bin", BinServer), exitDescription(devCode, devErr))
			return res, nil
		case !HasCUDADevice(devices):
			// Terminal (D19): rebuilding on this host would list no device
			// either, so there is no source fallback to offer.
			res.FailingCheck = "cuda-devices"
			res.Err = fmt.Errorf("%w: this CUDA build reports no CUDA device, so it would run on the CPU",
				ErrVerification)
			return res, nil
		}
	}

	// --- llama-bench, by presence and self-identification ---------------------
	bench := filepath.Join(opts.Root, "bin", BinBench)
	if _, err := os.Stat(bench); err != nil {
		res.FailingCheck = BinBench
		res.SourceFallback = true
		res.Err = fmt.Errorf("%w: %s is missing, so the benchmark feature would be absent",
			ErrVerification, filepath.Join("bin", BinBench))
		return res, nil
	}
	// Deliberately NOT gated on the exit status: llama-bench's own parser exits
	// non-zero on an unrecognized flag, and `-h` is one on some builds.
	benchOut, _, benchErr := runWithTimeout(ctx, run, timeout, bench, "-h")
	if benchErr != nil || !strings.Contains(benchOut, BinBench) {
		res.FailingCheck = BinBench
		res.SourceFallback = true
		res.Err = fmt.Errorf("%w: `%s -h` did not identify itself as %s",
			ErrVerification, filepath.Join("bin", BinBench), BinBench)
		return res, nil
	}

	res.OK = true
	return res, nil
}

// HasCUDADevice reads `llama-server --list-devices` output and reports whether
// any CUDA device is listed (D19).
//
// The output llama.cpp prints is a header line followed by one indented line
// per device:
//
//	Available devices:
//	  CUDA0: NVIDIA GeForce RTX 4090 (24210 MiB, 23890 MiB free)
//
// A build that fell back to CPU prints the header and nothing under it, or a
// `CPU` entry alone — which is exactly the silent failure D19 exists to catch.
func HasCUDADevice(out string) bool {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		name, _, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if strings.HasPrefix(name, "CUDA") && name != "CUDA" {
			return true
		}
	}
	return false
}

// ParseHelpFlags extracts the set of flag names from a `llama-server --help`
// capture — section 5.7's `help_flags_json`, the queryable projection of a
// capture the renderer may not read.
//
// llama.cpp's help lists aliases together and then the value placeholder:
//
//	-ngl, --gpu-layers, --n-gpu-layers N     number of layers to offload
//	     --fit [on|off]                      automatically fit to memory
//
// so a flag ends at whitespace, a comma, an `=`, or the start of a placeholder.
// Only tokens at the START of a line (after indentation) or in a comma-run
// following one are taken: prose in a description mentioning `--foo` is a
// mention, not an advertised flag, and treating it as one would silence the
// flag-churn guard exactly when it matters.
func ParseHelpFlags(help string) model.HelpFlags {
	seen := map[string]bool{}
	var out []string

	for _, line := range strings.Split(help, "\n") {
		trimmed := strings.TrimLeft(line, " \t")
		if trimmed == "" || !strings.HasPrefix(trimmed, "-") {
			continue
		}
		// Walk the comma-separated run of flag tokens at the start of the line.
		rest := trimmed
		for {
			flag, remainder, ok := cutFlag(rest)
			if !ok {
				break
			}
			if !seen[flag] {
				seen[flag] = true
				out = append(out, flag)
			}
			remainder = strings.TrimLeft(remainder, " \t")
			if !strings.HasPrefix(remainder, ",") {
				break
			}
			rest = strings.TrimLeft(remainder[1:], " \t")
			if !strings.HasPrefix(rest, "-") {
				break
			}
		}
	}
	sort.Strings(out)
	return model.HelpFlags(out)
}

// cutFlag reads one flag token from the start of s.
func cutFlag(s string) (flag, rest string, ok bool) {
	if !strings.HasPrefix(s, "-") {
		return "", s, false
	}
	i := 0
	for i < len(s) {
		c := s[i]
		if c == ' ' || c == '\t' || c == ',' || c == '=' || c == '[' || c == '<' || c == '(' {
			break
		}
		i++
	}
	flag = s[:i]
	// `-` alone, or `--`, is not a flag name.
	if flag == "-" || flag == "--" || len(flag) < 2 {
		return "", s, false
	}
	// A flag name is letters, digits and dashes. Anything else is prose.
	for _, r := range flag[1:] {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return "", s, false
		}
	}
	return flag, s[i:], true
}

// listBinaries returns the tool names under bin/, sorted.
func listBinaries(root string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(root, "bin"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("prebuilt: reading bin/: %w", err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out, nil
}

func runWithTimeout(ctx context.Context, run Runner, d time.Duration, name string, args ...string) (string, int, error) {
	cctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	return run(cctx, name, args...)
}

// execRun is the real Runner. It goes through internal/procx, like every other
// child process in this project: the probes here are the FIRST execution of a
// binary this host has never run, and one that hangs — a CUDA init against a
// wedged driver is the realistic case — must be killed with its process group
// when the timeout fires rather than outliving the install job.
//
// No LD_LIBRARY_PATH is set, and none is needed: every version tree is
// self-contained through its `$ORIGIN/../lib` RPATH (D22). If that RPATH is
// wrong, this is exactly where it must show up.
func execRun(ctx context.Context, name string, args ...string) (string, int, error) {
	out, res, err := procx.Capture(ctx, procx.Cmd{Path: name, Args: args})
	var ee *procx.ExitError
	if err != nil && !errors.As(err, &ee) {
		return out, -1, err
	}
	return out, res.ExitCode, nil
}

func exitDescription(code int, err error) string {
	if err != nil {
		return fmt.Sprintf("could not be run (%v)", err)
	}
	return fmt.Sprintf("exited %d", code)
}
