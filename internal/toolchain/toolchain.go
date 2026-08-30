package toolchain

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/procx"
)

// The probe set (DESIGN section 6.5 `preflight`, section 3.3
// `GET /api/v1/system/toolchain`, section 11.2's `toolchain` wizard step).
//
// Everything here is READ-ONLY and takes at most a few hundred milliseconds:
// each tool is looked up on PATH and asked for its version, and that is the
// whole mechanism. Nothing is installed, no package manager is invoked, and no
// state outside the returned Report is written — the report's consumer decides
// whether to persist it as a `toolchain_probes` row (section 2.4).
//
// The report answers three different questions with one pass, which is why it
// carries both per-tool detail and two summary booleans:
//
//   - the wizard's `toolchain` step renders one card per tool with its version,
//     what is needed, and a distro-specific hint (section 11.2);
//   - `GET /api/v1/llamacpp/plan` reports `missing_tools` before a user commits
//     to a build (section 6.3);
//   - the source pipeline's `preflight` phase aborts on the same set, so a
//     build never fails four minutes in on something the plan already knew
//     (section 6.5).

// Tool names. These are the `name` field of section 3.3's per-tool object and
// the strings that appear in a plan's `missing_tools`, so they are a closed
// vocabulary rather than incidental strings.
const (
	ToolGCC    = "gcc"
	ToolGXX    = "g++"
	ToolCMake  = "cmake"
	ToolNinja  = "ninja"
	ToolMake   = "make"
	ToolGit    = "git"
	ToolCcache = "ccache"
	ToolNvcc   = "nvcc"
	ToolDriver = "driver"
	ToolGlibc  = "glibc"
)

// MinCMake is the only minimum version DESIGN states (section 6.5: "cmake
// (>= 3.14)"). No other tool is version-gated here on purpose: inventing a
// minimum the design does not name would turn a host that builds llama.cpp
// perfectly well into one this product refuses to build on.
const MinCMake = "3.14"

// Tool is one probe result. The JSON tags are section 3.3's per-tool shape —
// `{name,found,path,version,min_version,ok,note,docs_url}` — plus `optional`,
// which this package adds because a report that cannot distinguish "ninja is
// missing and that is fine" from "cmake is missing and nothing will build"
// cannot be rendered. The HTTP DTO is a projection of this struct and owns its
// own field set; this shape is what goes into `toolchain_probes.result_json`.
type Tool struct {
	Name string `json:"name"`
	// Found reports whether the binary was resolved on PATH at all.
	Found bool   `json:"found"`
	Path  string `json:"path,omitempty"`
	// Version is the dotted numeric version, or empty when the tool ran but
	// printed something this package does not parse. Empty is not a failure.
	Version string `json:"version,omitempty"`
	// MinVersion is the minimum DESIGN requires, or empty when it requires none.
	MinVersion string `json:"min_version,omitempty"`
	// OK reports whether this tool satisfies its requirement. An OPTIONAL tool
	// that is absent is OK — its requirement is trivially satisfied — so a
	// renderer must read `found` for presence and `ok` for "does this block a
	// build".
	OK bool `json:"ok"`
	// Optional marks a tool whose absence never blocks a build (ninja, ccache).
	Optional bool `json:"optional"`
	// Note is the one-line human answer: why it is not OK, and what to install.
	// It never contains a command to run as root (section 6.5).
	Note    string `json:"note,omitempty"`
	DocsURL string `json:"docs_url,omitempty"`
	// CUDAOnly marks a tool a CPU build does not need, so `ok_cpu` can be true
	// on a host with no NVIDIA anything.
	CUDAOnly bool `json:"cuda_only,omitempty"`
}

// Report is one full probe: what section 2.4's `toolchain_probes` row stores in
// `result_json`, with `ok_cpu`/`ok_cuda`/`summary` lifted into its own columns.
type Report struct {
	At     time.Time `json:"at"`
	Family Family    `json:"family"`
	Tools  []Tool    `json:"tools"`
	// Libc is the host's C library, which is the fact DESIGN's D18 acceptance
	// check reports a prebuilt against ("requires GLIBC_2.38, host has 2.36").
	Libc Libc `json:"libc"`
	// CUDAArch is the compute capabilities of the GPUs present, in the form
	// `CMAKE_CUDA_ARCHITECTURES` wants ("89", "86") — D21's detected list, and
	// the plan endpoint's `cuda_arch`.
	CUDAArch []string `json:"cuda_arch,omitempty"`
	// DriverVersion is the NVIDIA driver version, empty when there is none.
	DriverVersion string `json:"driver_version,omitempty"`
	// OKCPU and OKCUDA are `toolchain_probes.ok_cpu` / `ok_cuda`: can this host
	// complete a CPU source build, and a CUDA one.
	OKCPU  bool `json:"ok_cpu"`
	OKCUDA bool `json:"ok_cuda"`
	// Summary is `toolchain_probes.summary`: one line for a list view.
	Summary string `json:"summary"`
}

// Tool returns one probe result by name.
func (r Report) Tool(name string) (Tool, bool) {
	for _, t := range r.Tools {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

// Missing lists the tools that block a build of this backend, in probe order.
// It is the plan endpoint's `missing_tools` (section 6.3) and the source
// pipeline's abort list (section 6.5 `preflight`).
func (r Report) Missing(backend model.Backend) []string {
	var out []string
	for _, t := range r.Tools {
		if t.OK || t.Optional {
			continue
		}
		if t.CUDAOnly && backend != model.BackendCUDA {
			continue
		}
		out = append(out, t.Name)
	}
	return out
}

// CanBuild reports whether a source build of this backend can proceed.
func (r Report) CanBuild(backend model.Backend) bool {
	if backend == model.BackendCUDA {
		return r.OKCUDA
	}
	return r.OKCPU
}

// Runner executes one probe command and returns its MERGED output. It is the
// seam every test in this package substitutes: a real probe of a host with no
// nvcc, no ninja and a musl libc would otherwise be untestable anywhere but on
// that host.
//
// The streams are merged rather than kept apart because these tools disagree
// about which one a version banner belongs on — `ldd --version` writes stdout,
// several `--version` implementations write stderr — and a parser that has to
// know which is a parser that breaks on the next tool.
//
// It returns an error only when the process could not be started at all; a
// non-zero exit is `code`, because several of these tools exit non-zero while
// still printing the version we asked for.
type Runner func(ctx context.Context, name string, args ...string) (output string, code int, err error)

// Options configures a probe. The zero value probes this host.
type Options struct {
	// Run executes a command. Nil uses internal/procx.
	Run Runner
	// LookPath resolves a binary on PATH. Nil uses exec.LookPath.
	LookPath func(file string) (string, error)
	// OSRelease is the os-release file the distro family is read from. Empty
	// uses /etc/os-release.
	OSRelease string
	// Family overrides distro detection outright. Empty detects.
	Family Family
	// Now supplies the report timestamp. Nil uses time.Now.
	Now func() time.Time
	// Timeout bounds one probe command. Zero uses 5 s — every command here
	// prints a version and exits, and one that does not is a hung host, not a
	// slow one.
	Timeout time.Duration
}

func (o Options) run() Runner {
	if o.Run != nil {
		return o.Run
	}
	return execRun
}

func (o Options) lookPath() func(string) (string, error) {
	if o.LookPath != nil {
		return o.LookPath
	}
	return exec.LookPath
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now().UTC()
}

func (o Options) timeout() time.Duration {
	if o.Timeout > 0 {
		return o.Timeout
	}
	return 5 * time.Second
}

// execRun is the real Runner. It goes through internal/procx like every other
// child process in this project, which is what guarantees that a probe against
// a hung binary is killed with its whole process group when the context ends
// rather than leaking a process into the daemon's lifetime.
func execRun(ctx context.Context, name string, args ...string) (string, int, error) {
	// A probe must never inherit an interactive environment's surprises; the
	// only variable any of these tools needs is PATH, which the process already
	// has, and LC_ALL keeps the banners parseable on a localized host.
	out, res, err := procx.Capture(ctx, procx.Cmd{Path: name, Args: args, ExtraEnv: []string{"LC_ALL=C"}})
	var ee *procx.ExitError
	if err != nil && !errors.As(err, &ee) {
		// The process could not be started at all — not the same thing as a
		// tool that ran and exited non-zero, which several of these do while
		// printing exactly what was asked for.
		return out, -1, err
	}
	return out, res.ExitCode, nil
}

// spec is one entry in the probe table.
type spec struct {
	name     string
	bin      string
	args     []string
	parse    func(output string) (Version, bool)
	min      string
	optional bool
	cudaOnly bool
}

// specs is the probe table, in the order the report and the UI list them.
// `driver` and `glibc` are not on it: they are not "a binary with a --version",
// and each has its own probe below.
var specs = []spec{
	{name: ToolGCC, bin: "gcc", args: []string{"--version"}, parse: parseFirstLineVersion},
	{name: ToolGXX, bin: "g++", args: []string{"--version"}, parse: parseFirstLineVersion},
	{name: ToolCMake, bin: "cmake", args: []string{"--version"}, parse: parseFirstLineVersion, min: MinCMake},
	{name: ToolNinja, bin: "ninja", args: []string{"--version"}, parse: parseFirstLineVersion, optional: true},
	{name: ToolMake, bin: "make", args: []string{"--version"}, parse: parseFirstLineVersion},
	{name: ToolGit, bin: "git", args: []string{"--version"}, parse: parseFirstLineVersion},
	{name: ToolCcache, bin: "ccache", args: []string{"--version"}, parse: parseFirstLineVersion, optional: true},
	{name: ToolNvcc, bin: "nvcc", args: []string{"--version"}, parse: ParseNvccVersion, cudaOnly: true},
}

// Probe runs the whole probe set and returns the report. It never returns an
// error: a host missing every tool is a report, not a failure, and that is
// exactly the host the wizard exists to talk to.
func Probe(ctx context.Context, opts Options) Report {
	fam := opts.Family
	if fam == "" {
		fam = DetectFamily(opts.OSRelease)
	}

	r := Report{At: opts.now(), Family: fam}
	for _, s := range specs {
		r.Tools = append(r.Tools, probeOne(ctx, opts, fam, s))
	}

	libc, libcTool := probeLibc(ctx, opts, fam)
	r.Libc = libc
	r.Tools = append(r.Tools, libcTool)

	driver, driverTool := probeDriver(ctx, opts, fam)
	r.DriverVersion = driver.Version
	r.CUDAArch = driver.Architectures()
	r.Tools = append(r.Tools, driverTool)

	r.OKCPU = ok(r, ToolGCC, ToolGXX, ToolCMake, ToolGit) && okGenerator(r)
	r.OKCUDA = r.OKCPU && ok(r, ToolNvcc, ToolDriver)
	r.Summary = summarize(r)
	return r
}

// probeOne resolves and interrogates a single binary from the table.
func probeOne(ctx context.Context, opts Options, fam Family, s spec) Tool {
	t := Tool{Name: s.name, MinVersion: s.min, Optional: s.optional, CUDAOnly: s.cudaOnly}
	if g, okg := GuidanceFor(s.name); okg {
		t.DocsURL = g.DocsURL
	}

	path, err := opts.lookPath()(s.bin)
	if err != nil {
		t.OK = s.optional
		t.Note = notFoundNote(s.name, fam)
		return t
	}
	t.Found = true
	t.Path = path

	cctx, cancel := context.WithTimeout(ctx, opts.timeout())
	defer cancel()
	out, code, runErr := opts.run()(cctx, path, s.args...)
	if runErr != nil {
		t.Note = fmt.Sprintf("found at %s but could not be run: %v", path, runErr)
		return t
	}
	v, parsed := s.parse(out)
	if parsed {
		t.Version = v.String()
	}
	if code != 0 && !parsed {
		t.Note = fmt.Sprintf("found at %s but `%s %s` exited %d", path, s.bin, strings.Join(s.args, " "), code)
		return t
	}
	if s.min != "" && parsed && !v.AtLeast(MustParseVersion(s.min)) {
		t.Note = fmt.Sprintf("version %s is older than the required %s; %s",
			v.String(), s.min, noteFor(s.name, fam, false))
		return t
	}
	t.OK = true
	if s.optional {
		t.Note = optionalPresentNote(s.name)
	}
	return t
}

// okGenerator is the one requirement that is a choice rather than a tool:
// section 6.5 configures with Ninja when it is present and Unix Makefiles
// otherwise, so a host needs ONE of them, not both.
func okGenerator(r Report) bool {
	return ok(r, ToolNinja) || ok(r, ToolMake)
}

func ok(r Report, names ...string) bool {
	for _, n := range names {
		t, found := r.Tool(n)
		if !found || !t.OK || !t.Found {
			return false
		}
	}
	return true
}

func notFoundNote(tool string, fam Family) string {
	return "not found on PATH — " + noteFor(tool, fam, true)
}

func noteFor(tool string, fam Family, missing bool) string {
	g, okg := GuidanceFor(tool)
	if !okg {
		return "no guidance recorded for this tool"
	}
	return g.note(fam, missing)
}

func optionalPresentNote(tool string) string {
	switch tool {
	case ToolNinja:
		return "used as the CMake generator"
	case ToolCcache:
		return "used as the compiler launcher, which makes rebuilds of nearby commits fast"
	}
	return ""
}

// parseFirstLineVersion is the parser for every tool that prints its version on
// the first line it emits. The first line that CONTAINS a version wins, rather
// than strictly the first line, so a tool that greets before it answers is
// still read correctly.
func parseFirstLineVersion(out string) (Version, bool) {
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if v, okv := ParseVersion(line); okv {
			return v, true
		}
	}
	return Version{}, false
}

// summarize renders `toolchain_probes.summary`: one line a list view can show
// without expanding anything.
func summarize(r Report) string {
	var missing []string
	for _, t := range r.Tools {
		if !t.OK && !t.Optional && !t.CUDAOnly && t.Name != ToolNinja && t.Name != ToolMake {
			missing = append(missing, t.Name)
		}
	}
	if !okGenerator(r) {
		missing = append(missing, "ninja or make")
	}
	switch {
	case len(missing) > 0:
		sort.Strings(missing)
		return "cannot build from source: missing " + strings.Join(missing, ", ")
	case r.OKCUDA:
		arch := "no compute capability detected"
		if len(r.CUDAArch) > 0 {
			arch = "CUDA arch " + strings.Join(r.CUDAArch, ";")
		}
		return "ready for CPU and CUDA builds (" + arch + ")"
	default:
		miss := r.Missing(model.BackendCUDA)
		if len(miss) == 0 {
			return "ready for CPU builds"
		}
		return "ready for CPU builds; CUDA needs " + strings.Join(miss, ", ")
	}
}

// RequiredFreeBytes is section 6.5's `space` phase: 12 GiB for a CUDA build,
// 3 GiB for a CPU one. It lives here rather than in the build pipeline because
// `GET /api/v1/llamacpp/plan` reports `free_space_ok` from the same number
// before any build exists, and two copies of a threshold is one copy too many.
// The free-space measurement itself belongs to internal/hw (Statfs); this is
// only the number to compare it against.
func RequiredFreeBytes(backend model.Backend) int64 {
	const giB = 1 << 30
	if backend == model.BackendCUDA {
		return 12 * giB
	}
	return 3 * giB
}

// sortedUnique is shared by the CUDA architecture list.
func sortedUnique(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := slices.Clone(in)
	sort.Strings(out)
	return slices.Compact(out)
}
