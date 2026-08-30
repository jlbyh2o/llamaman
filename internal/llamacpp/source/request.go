package source

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jlbyh2o/llamaman/internal/hw"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/toolchain"
)

// DefaultGitURL is upstream, and the default of `llamacpp_versions.git_url`
// (section 2.5). A `channel='custom'` row carries its own.
const DefaultGitURL = "https://github.com/ggml-org/llama.cpp"

// Request is one source build: everything section 6.5 needs that this package
// cannot discover for itself.
//
// It carries no database handle and no settings reader. Both are the caller's:
// the `llamacpp_install` worker resolves the row, reads
// `llamacpp.build_jobs`/`cuda_arch_list`/`extra_cmake_flags`, probes the GPUs
// through hw.Prober and hands the answers here. That is what keeps this package
// testable against a fake toolchain and keeps D49's "only internal/store writes
// SQL" true without a rule about it.
type Request struct {
	// VersionID is `llamacpp_versions.id` — `<tag>-<backend>-<acq>` (D60) — and
	// is also every directory name this build touches.
	VersionID string

	// Tag is the `tag` column ('b10621', 'v0.3.0', 'fork-<urlhash6>-<short>'),
	// recorded in manifest.json so a version directory identifies itself
	// without the database.
	Tag string

	// BuildTag is the `b#####` a stable release pinned through nightly-tag.txt
	// (section 6.2), and Channel is which of the three the request came from.
	// Both exist only to be recorded: the manifest a prebuilt install writes
	// carries them, and one reader has to decode both files.
	BuildTag string
	Channel  model.LlamacppChannel

	// GitURL is the remote to clone or fetch from; empty means DefaultGitURL.
	GitURL string

	// GitRef is the tag, branch or 40-hex commit to build. Empty builds the
	// remote's default branch tip, which is only ever right for a manual
	// experiment — every row this daemon creates names a ref.
	GitRef string

	// Backend selects the CUDA flags (section 6.5's configure block).
	Backend model.Backend

	// CUDAArchs are the compute capabilities to compile for, as cmake spells
	// them ("89", "86") — from detection, NEVER `native` and never `all` (D21).
	// Empty on a CUDA build makes the preflight fail rather than guess.
	CUDAArchs []string

	// GPUs is the device set those architectures came from. Only the UUIDs and
	// compute capabilities are recorded (manifest.json and
	// `llamacpp_versions.gpu_uuids_json`), which is what lets the UI say
	// "rebuild recommended" when the live GPU set no longer matches.
	GPUs []hw.GPU

	// ExtraCMakeFlags is `settings.llamacpp.extra_cmake_flags` followed by the
	// request's own `cmake_extra` (section 3.5), passed through verbatim and
	// last so a user can override anything above them.
	ExtraCMakeFlags []string

	// Jobs overrides D20's computed parallelism
	// (`settings.llamacpp.build_jobs`; 0 = auto).
	Jobs int

	// Resume asks for D4's warm rerun: when the worktree and the cmake cache
	// from a previous attempt are both present, skip `fetch` and `configure`
	// and re-run `cmake --build` against the warm objects. When they are not,
	// the full pipeline runs and Result.Resumed says so.
	Resume bool

	// Observer receives one Progress per phase, plus the compile phase's
	// counters as they are parsed out of the build output. It is how
	// `jobs.progress_json` is written (section 6.5's opening line); nil is a
	// build nobody is watching.
	Observer Observer
}

// GitURLOrDefault is the remote this build fetches from.
func (r Request) GitURLOrDefault() string {
	if r.GitURL == "" {
		return DefaultGitURL
	}
	return r.GitURL
}

// Validate rejects a request this package cannot act on. It is deliberately
// strict about the CUDA architecture list: D21's whole point is that `native`
// and `all` are both wrong answers, so neither may arrive here disguised as a
// detected capability.
func (r Request) Validate() error {
	if err := ValidateVersionID(r.VersionID); err != nil {
		return err
	}
	if !r.Backend.Valid() {
		return fmt.Errorf("source: backend %q is not one of %v", r.Backend, model.BackendValues())
	}
	// The exec-safety half of the git-URL rule (giturl.go). The scheme
	// allowlist is applied one layer up, where a user's request becomes a row
	// and a rejection can still be a 422; what a package that hands strings to
	// `exec` must check for itself is argv injection, the `::` transport escape
	// and embedded credentials.
	if err := validateGitURLSafety(r.GitURLOrDefault()); err != nil {
		return err
	}
	for _, a := range r.CUDAArchs {
		if err := validateCUDAArch(a); err != nil {
			return err
		}
	}
	if r.Jobs < 0 {
		return fmt.Errorf("source: build jobs %d is negative", r.Jobs)
	}
	return nil
}

// CUDAArchList renders `CMAKE_CUDA_ARCHITECTURES`: the detected capabilities,
// deduplicated, ascending, `;`-separated.
func (r Request) CUDAArchList() string { return strings.Join(r.CUDAArchs, ";") }

// GPUUUIDs is the device identity recorded with the build
// (`llamacpp_versions.gpu_uuids_json`).
func (r Request) GPUUUIDs() []string {
	out := make([]string, 0, len(r.GPUs))
	for _, g := range r.GPUs {
		if g.UUID != "" {
			out = append(out, g.UUID)
		}
	}
	return out
}

// validateCUDAArch accepts what cmake accepts in CMAKE_CUDA_ARCHITECTURES for a
// concrete device — "89", "86", and the `-real`/`-virtual` suffixes — and
// refuses the two magic words D21 exists to rule out.
func validateCUDAArch(a string) error {
	switch strings.ToLower(a) {
	case "":
		return fmt.Errorf("source: empty CUDA architecture")
	case "native", "all", "all-major":
		return fmt.Errorf("source: CUDA architecture %q is forbidden (D21): "+
			"compile for the capabilities actually detected, so the build still runs if the GPU set changes", a)
	}
	digits := strings.TrimSuffix(strings.TrimSuffix(a, "-real"), "-virtual")
	if digits == "" {
		return fmt.Errorf("source: CUDA architecture %q has no compute capability", a)
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return fmt.Errorf("source: CUDA architecture %q is not a compute capability", a)
		}
	}
	return nil
}

// CUDAArchsFromGPUs turns detected compute capabilities into the cmake list
// D21 requires: "8.9" becomes "89", duplicates collapse, and the result is
// sorted so two probes of the same host in a different order produce the same
// `cuda_arch_list` — which matters, because D71 compares that string when it
// decides whether a re-post is a reuse or a `409 version_options_differ`.
//
// A GPU whose capability the driver did not report is skipped rather than
// guessed at; when that leaves the list empty on a CUDA build, the preflight
// says so instead of compiling for an architecture nobody asked for.
func CUDAArchsFromGPUs(gpus []hw.GPU) []string {
	seen := make(map[string]struct{}, len(gpus))
	var out []string
	for _, g := range gpus {
		a := archFromComputeCap(g.ComputeCap)
		if a == "" {
			continue
		}
		if _, dup := seen[a]; dup {
			continue
		}
		seen[a] = struct{}{}
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// ParseCUDAArchList parses the `llamacpp.cuda_arch_list` setting, whose value is
// a free-form `;`- or `,`-separated list the settings registry deliberately does
// not police (section 2.1). Empty means "auto-detect", and is not an error.
func ParseCUDAArchList(s string) ([]string, error) {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ';' || r == ',' || r == ' ' || r == '\t'
	})
	var out []string
	seen := make(map[string]struct{}, len(fields))
	for _, f := range fields {
		a := archFromComputeCap(f)
		if a == "" {
			a = f
		}
		if err := validateCUDAArch(a); err != nil {
			return nil, err
		}
		if _, dup := seen[a]; dup {
			continue
		}
		seen[a] = struct{}{}
		out = append(out, a)
	}
	sort.Strings(out)
	return out, nil
}

// archFromComputeCap turns a driver-reported capability into a cmake entry.
//
// The "8.9" → "89" conversion is internal/toolchain's — it is the same function
// `GET /api/v1/llamacpp/plan` reports `cuda_arch` with, and two spellings of
// that mapping would be two answers to D21. What this adds is the already-cmake
// form: a `llamacpp.cuda_arch_list` setting a person typed reads "89", which is
// not a version and which toolchain's parser therefore declines.
func archFromComputeCap(cc string) string {
	cc = strings.TrimSpace(cc)
	if cc == "" {
		return ""
	}
	if a := toolchain.ArchFromComputeCap(cc); a != "" {
		return a
	}
	for _, r := range cc {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return cc
}
