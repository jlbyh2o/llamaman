package source

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// Verification is what the `verify` phase learned by running the binaries it
// just installed — against the STAGING tree, before anything is renamed into
// place (section 6.5, D78).
type Verification struct {
	// VersionOutput is `llama-server --version`, which D18 requires to exit 0
	// ON THIS HOST. For a source build that check is nearly free and still
	// worth doing: it catches a build that linked against a library the install
	// tree does not carry, which is exactly what D22's RPATH exists to prevent
	// and therefore exactly what a broken RPATH looks like.
	VersionOutput string

	// DevicesOutput is `llama-server --list-devices`, verbatim, for
	// `llamacpp_versions.devices_output` (section 2.5).
	DevicesOutput string

	// CUDADevices is how many CUDA devices that output named. D19 requires at
	// least one for a CUDA build: a CUDA build that silently fell back to CPU
	// is worse than no build.
	CUDADevices int

	// HelpOutput is the verbatim `llama-server --help` capture that goes into
	// manifest.json.
	HelpOutput string

	// HelpFlags is that capture parsed into the set of flag names, which is
	// `llamacpp_versions.help_flags_json` — the queryable projection that keeps
	// the flag-churn guard (section 5.7) a pure function over ROWS instead of a
	// file read inside RenderArgv.
	HelpFlags model.HelpFlags

	// SupportsFit is `llamacpp_versions.supports_fit`: does this build know
	// `--fit`? RenderArgv reads that column to decide how `-ngl auto` renders
	// (D51), which is what keeps the renderer pure.
	SupportsFit bool

	// BenchOutput is `llama-bench -h`, kept because llama-bench's presence is
	// the whole reason D23 turns LLAMA_BUILD_TOOLS on.
	BenchOutput string
}

// cudaDeviceRe matches the device lines `llama-server --list-devices` prints:
//
//	Available devices:
//	  CUDA0: NVIDIA GeForce RTX 4090 (24210 MiB, 23900 MiB free)
var cudaDeviceRe = regexp.MustCompile(`(?im)^\s*CUDA(\d+)\s*:`)

// cudaFoundRe matches the backend's own init line, which older builds print
// instead: "ggml_cuda_init: found 2 CUDA devices:".
var cudaFoundRe = regexp.MustCompile(`(?i)found\s+(\d+)\s+CUDA\s+device`)

// CountCUDADevices reports how many distinct CUDA devices a `--list-devices`
// capture names. It is the whole of D19's test, kept as a pure function so the
// several output formats upstream has shipped can be pinned in a table.
func CountCUDADevices(out string) int {
	seen := make(map[string]struct{})
	for _, m := range cudaDeviceRe.FindAllStringSubmatch(out, -1) {
		seen[m[1]] = struct{}{}
	}
	if len(seen) > 0 {
		return len(seen)
	}
	if m := cudaFoundRe.FindStringSubmatch(out); m != nil {
		n := 0
		for _, r := range m[1] {
			n = n*10 + int(r-'0')
		}
		return n
	}
	return 0
}

// flagRe is one flag name as it appears in a help line's leading cluster.
var flagRe = regexp.MustCompile(`^--?[A-Za-z0-9][A-Za-z0-9-]*$`)

// ParseHelpFlags turns a `llama-server --help` capture into the set of flag
// names it advertises, sorted and deduplicated.
//
// The shape it reads is upstream's own: a leading cluster of comma-separated
// flags, each optionally followed by an argument placeholder, then a wide gap,
// then the description —
//
//	-h,    --help, --usage       print usage and exit
//	-c,    --ctx-size N          size of the prompt context
//	       --fit                 let llama.cpp choose the offload
//
// The cluster ends at the first token that is neither a flag, a placeholder, nor
// a comma — which is what keeps a flag NAMED in a description ("see --fit")
// from being read as a flag the build SUPPORTS. That distinction is the whole
// value of the column: an over-generous parser would make section 5.7's
// unknown-flag guard useless by declaring every flag known.
func ParseHelpFlags(out string) model.HelpFlags {
	seen := make(map[string]struct{})
	var flags []string
	add := func(tok string) {
		if _, dup := seen[tok]; dup {
			return
		}
		seen[tok] = struct{}{}
		flags = append(flags, tok)
	}
	for line := range strings.Lines(out) {
		trimmed := strings.TrimRight(line, "\r\n")
		// A deeply indented line is a continuation of the previous
		// description, never a flag cluster.
		if lead := len(trimmed) - len(strings.TrimLeft(trimmed, " \t")); lead > 8 {
			continue
		}
		parseHelpLine(trimmed, add)
	}
	sort.Strings(flags)
	return flags
}

// parseHelpLine walks one help line's leading flag cluster.
func parseHelpLine(line string, add func(string)) {
	i, n := 0, len(line)
	skipSpace := func() {
		for i < n && (line[i] == ' ' || line[i] == '\t') {
			i++
		}
	}
	word := func(stopAtComma bool) string {
		start := i
		for i < n && line[i] != ' ' && line[i] != '\t' && (!stopAtComma || line[i] != ',') {
			i++
		}
		return line[start:i]
	}

	for i < n {
		skipSpace()
		if i >= n || line[i] != '-' {
			return
		}
		start := i
		for i < n && line[i] != ' ' && line[i] != '\t' && line[i] != ',' && line[i] != '=' {
			i++
		}
		tok := line[start:i]
		if !flagRe.MatchString(tok) {
			return
		}
		add(tok)

		if i < n && line[i] == '=' {
			// "--fit=BOOL": drop the placeholder with its '='.
			word(true)
		}
		if i < n && line[i] == ',' {
			i++
			continue
		}
		skipSpace()
		if i >= n {
			return
		}
		// An argument placeholder may sit between the flag and the comma that
		// introduces its alias: "--ctx-size N, -c N".
		if word(true) == "" {
			return
		}
		if i < n && line[i] == ',' {
			i++
			continue
		}
		return
	}
}

// verify runs the D18 and D19 checks against dir, which is always the staging
// tree.
//
// The four probes below are the only place outside internal/instances where a
// flag is handed to a `versions/*/bin/*` binary, and they are not the thing
// D49's third invariant forbids. That rule is about RENDERING a llama.cpp
// command line — turning a model.FlagSet into argv, which exactly two functions
// in internal/instances may do (D62). These are acceptance probes with no
// configuration in them at all: `--version` is D18's, `--list-devices` is
// D19's, `--help` is the capture section 2.5's `help_flags_json` column is
// parsed from, and `-h` is how section 6.4 step 3 identifies llama-bench.
// Sections 6.4 and 6.5 name all four literally, so an import-graph test that
// enforces the invariant has to allow the acquisition pipelines these strings —
// they are how a build is accepted, not how an instance is configured.
func (b *Builder) verify(ctx context.Context, req Request, dir string, sink *LogSink) (Verification, error) {
	var v Verification
	server := ServerPath(dir)

	out, err := b.capture(ctx, sink, server, "--version")
	v.VersionOutput = out
	if err != nil {
		return v, &Failure{
			Phase:   PhaseVerify,
			Code:    CodeFailedVerification,
			Message: fmt.Sprintf("the build finished but `%s --version` does not run on this host", server),
			Hint: "The binaries are in the staging directory and the build log is kept; " +
				"a missing shared library here usually means the install RPATH was overridden by an extra cmake flag.",
			ExitCode: exitCodeOf(err),
			cause:    err,
		}
	}

	// --list-devices is captured on both backends: for CUDA it is D19's test,
	// and for CPU it is still the honest record of what the build can see, which
	// is what `llamacpp_versions.devices_output` is for.
	devices, devErr := b.capture(ctx, sink, server, "--list-devices")
	v.DevicesOutput = devices
	v.CUDADevices = CountCUDADevices(devices)
	if req.Backend == model.BackendCUDA {
		if devErr != nil {
			return v, &Failure{
				Phase:    PhaseVerify,
				Code:     CodeNoCUDADevice,
				Message:  "`llama-server --list-devices` failed on a CUDA build",
				ExitCode: exitCodeOf(devErr),
				cause:    devErr,
			}
		}
		if v.CUDADevices < 1 {
			return v, &Failure{
				Phase: PhaseVerify,
				Code:  CodeNoCUDADevice,
				Message: "this CUDA build reports no CUDA device, which means it fell back to the CPU backend " +
					"while claiming to be a CUDA build",
				Hint: "Check that the NVIDIA driver is loaded and that nvcc and the driver agree on a CUDA " +
					"version, then retry. The build log records the cmake output that chose the backend.",
			}
		}
	}

	help, err := b.capture(ctx, sink, server, "--help")
	if err != nil {
		// A non-zero --help is not fatal on its own; the capture is what
		// matters, and section 5.7 already models an absent one as "the flag
		// check is unavailable for this build" rather than as a failure.
		sink.Printf("note: `llama-server --help` exited non-zero; recording the capture anyway (%v)", err)
	}
	v.HelpOutput = help
	v.HelpFlags = ParseHelpFlags(help)
	v.SupportsFit = v.HelpFlags.Has("--fit")

	// llama-bench has its own argument parser and exits non-zero on a flag it
	// does not know, so its acceptance test is presence plus a `-h` whose output
	// names it — never an exit status (section 6.4 step 3).
	bench := BenchPath(dir)
	if _, err := os.Stat(bench); err != nil {
		return v, &Failure{
			Phase: PhaseVerify,
			Code:  CodeInstallIncomplete,
			Message: "llama-bench is missing from the build, so the benchmark feature would be absent: " +
				"the build did not honor -DLLAMA_BUILD_TOOLS=ON",
			cause: err,
		}
	}
	benchOut, _ := b.capture(ctx, sink, bench, "-h")
	v.BenchOutput = benchOut
	if !strings.Contains(strings.ToLower(benchOut), "llama-bench") {
		return v, &Failure{
			Phase:   PhaseVerify,
			Code:    CodeFailedVerification,
			Message: "`llama-bench -h` did not identify itself, so the installed binary is not llama-bench",
		}
	}
	return v, nil
}
