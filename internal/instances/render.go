package instances

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// argv rendering (DESIGN section 5.7 and section 10.1).
//
// This file is the ONLY place in the codebase that turns a model.FlagSet into a
// llama.cpp command line (D49 invariant 3). It exports exactly two renderers
// (D62) — RenderArgv for `llama-server` and RenderBenchArgv for `llama-bench`,
// whose incompatible CLI section 10.1 maps field by field — and they share one
// FlagSet so the bench runner, the fit calculator and the "show me the command
// line" endpoint can never disagree about what a flag means.
//
// # Both renderers are PURE
//
// They take rows and return strings. They do not import internal/fit or
// internal/hw, do not read live VRAM, do not touch the clock, DO NOT OPEN A
// FILE, and produce identical output for identical rows on any host. That is
// what lets `instance-exec` — a DB-read-only process with no D-Bus, no HTTP and
// no GPU probe — call RenderArgv, and what makes the golden tests beside this
// file mean something.
//
// The two build-capability rules that need to know what `llama-server --help`
// says are therefore COLUMNS, not files: `llamacpp_versions.supports_fit` and
// `help_flags_json` are parsed once at install time and arrive here inside
// Runtime (section 5.7).

// ServerBinary and BenchBinary are the executables inside a version directory.
const (
	ServerBinary = "llama-server"
	BenchBinary  = "llama-bench"
)

// LoopbackHost is the address llama-server always binds. The gateway is the
// front door (SPEC section 1), so an instance never listens on a routable
// address and `--host` is never user-editable.
const LoopbackHost = "127.0.0.1"

// NGLAllValue is what `all` renders as. 999 is llama.cpp's own idiom for "every
// layer": the runtime clamps it to the model's layer count.
const NGLAllValue = 999

// Runtime is the `llamacpp_versions` row as the renderers read it. It is
// declared here rather than imported from a llama.cpp package because the
// consumer owns the interface it needs (DESIGN section 1) — and because these
// four fields are the entire dependency, which is what keeps the renderers pure.
type Runtime struct {
	// ID is `llamacpp_versions.id` (`b10621-cuda-src`). It is folded into
	// `config_hash` (D52), which is what makes a version flip raise
	// `restart_required` for every instance at once (D69).
	ID string
	// Dir is the absolute directory the binaries live in — `versions/<id>` or
	// the `versions/active` symlink, already resolved by the caller. The
	// renderers only join a name onto it; they never stat it.
	Dir string
	// SupportsFit is `llamacpp_versions.supports_fit`. When it is false this
	// build predates `--fit`, so `-ngl auto` renders as `-ngl 999` and the
	// instance form says "this build predates --fit; auto behaves as all"
	// (section 5.7).
	SupportsFit bool
	// Help is `help_flags_json`, the flag names this build advertises. It is
	// read only by UnknownFlags — never by a renderer, which returns argv and
	// nothing else.
	Help model.HelpFlags
}

// ServerPath and BenchPath are the two executables, without touching the disk.
func (r Runtime) ServerPath() string { return filepath.Join(r.Dir, "bin", ServerBinary) }

// BenchPath is `bin/llama-bench` inside this version directory (D23 is why it
// exists at all: `-DLLAMA_BUILD_TOOLS=ON`).
func (r Runtime) BenchPath() string { return filepath.Join(r.Dir, "bin", BenchBinary) }

// ModelFile is a resolved `models` row as the renderers read it: an id for the
// hash and one absolute path for argv. For a sharded set the path is shard 1,
// which is the file llama.cpp is given and from which it finds the rest.
type ModelFile struct {
	ID   string
	Path string
}

// RenderArgv renders the `llama-server` command line for one instance
// (section 5.7).
//
// The FlagSet is passed in rather than read from inst precisely so a caller can
// hand it the override-patched set of a safe start (section 3.10b) without the
// override ever touching `instances.flags_json`.
//
// The order is fixed, and it is fixed so that `config_hash` is stable and diffs
// are readable: model paths → context → offload → batching → attention and
// cache → devices → server/network → mode flags → draft → the always-appended
// listener block → extra_flags. The draft MODEL path travels with the rest of
// the draft group rather than with `-m`, so that an argv diff shows speculative
// decoding as one contiguous block; section 5.7 fixes the groups, not the
// placement of `-md` within them.
func RenderArgv(
	inst model.Instance,
	flags model.FlagSet,
	primary, mmproj, draft *ModelFile,
	version Runtime,
) ([]string, error) {
	if primary == nil || primary.Path == "" {
		return nil, model.Error{
			Code:    model.CodeModelMissing,
			Message: "this instance has no resolved model file to serve",
		}
	}

	extra, err := ParseExtraFlags(inst.ExtraFlags)
	if err != nil {
		return nil, err
	}

	a := &argv{out: []string{version.ServerPath()}}

	// Model paths.
	a.pair("-m", primary.Path)
	if mmproj != nil && mmproj.Path != "" {
		a.pair("--mmproj", mmproj.Path)
	}

	// Context.
	a.intFlag("-c", flags.CtxSize)

	// Offload (D51). `auto` emits NOTHING, which is exactly what leaves
	// llama.cpp's own --fit enabled to choose the offload — unless this build
	// predates --fit, in which case `auto` behaves as `all` and says so.
	if n, ok := renderNGL(flags.NGpuLayers, version.SupportsFit); ok {
		a.pair("-ngl", strconv.Itoa(n))
	}

	// Batching.
	a.intFlag("-b", flags.BatchSize)
	a.intFlag("-ub", flags.UbatchSize)
	a.intFlag("-np", flags.Parallel)
	a.intFlag("-t", flags.Threads)
	a.intFlag("-tb", flags.ThreadsBatch)

	// Attention and cache.
	if flags.FlashAttn != nil {
		a.pair("-fa", string(*flags.FlashAttn))
	}
	a.strFlag("-ctk", flags.CacheTypeK)
	a.strFlag("-ctv", flags.CacheTypeV)

	// Devices. `--device` is the only device selector (D66); the launcher sets
	// no CUDA_VISIBLE_DEVICES, which is what keeps nvidia-smi index ==
	// gpus.gpu_index == llama.cpp's CUDA<n> a single stable mapping.
	a.strFlag("--device", flags.DeviceFilter)
	if flags.SplitMode != nil {
		a.pair("-sm", string(*flags.SplitMode))
	}
	if len(flags.TensorSplit) > 0 {
		a.pair("-ts", joinFloats(flags.TensorSplit))
	}
	a.intFlag("-mg", flags.MainGPU)

	// Server/network.
	a.strFlag("--alias", flags.Alias)

	// Mode flags.
	a.boolFlag("--jinja", flags.Jinja)
	a.strFlag("--chat-template", flags.ChatTemplate)
	a.strFlag("--chat-template-file", flags.ChatTemplateFile)
	a.boolFlag("--embedding", flags.Embedding)
	a.strFlag("--pooling", flags.Pooling)
	a.boolFlag("--reranking", flags.Rerank)
	a.boolFlag("--mlock", flags.Mlock)
	a.boolFlag("--no-mmap", flags.NoMmap)
	if flags.ContBatching != nil {
		// The one tri-state that renders as two different flags rather than as
		// one flag or nothing: llama-server defaults continuous batching ON, so
		// "false" has to be said out loud.
		if *flags.ContBatching {
			a.flag("-cb")
		} else {
			a.flag("-nocb")
		}
	}
	a.strFlag("--rope-scaling", flags.RopeScaling)
	a.floatFlag("--rope-freq-base", flags.RopeFreqBase)
	a.floatFlag("--rope-freq-scale", flags.RopeFreqScale)
	a.floatFlag("--yarn-ext-factor", flags.YarnExtFactor)
	a.floatFlag("--yarn-attn-factor", flags.YarnAttnFactor)
	a.intFlag("--keep", flags.NKeep)
	a.intFlag("--predict", flags.NPredict)
	a.floatFlag("--defrag-thold", flags.DefragThold)
	a.intFlag("--cache-reuse", flags.CacheReuse)
	a.strFlag("--numa", flags.Numa)
	a.strFlag("-C", flags.CPUMask)
	a.intFlag("--prio", flags.Prio)
	a.strFlag("--slot-save-path", flags.SlotSavePath)
	a.intFlag("--verbosity", flags.LogVerbosity)

	// Draft (speculative decoding). The model path heads the group so the whole
	// feature reads as one block.
	if draft != nil && draft.Path != "" {
		a.pair("-md", draft.Path)
	}
	if d := flags.Draft; d != nil {
		a.intFlag("--draft-max", d.NMax)
		a.intFlag("--draft-min", d.NMin)
		a.floatFlag("--draft-p-min", d.PMin)
		a.intFlag("-cd", d.CtxSize)
		a.intFlag("-ngld", d.NGpuLayers)
	}

	// Always appended and never user-editable (section 5.7). `--no-webui`
	// because the gateway is the front door; the three endpoint flags because
	// the supervisor and the gateway read them.
	a.pair("--host", LoopbackHost)
	a.pair("--port", strconv.Itoa(inst.InternalPort))
	a.flag("--no-webui")
	a.boolFlag("--props", flags.PropsEndpoint)
	a.boolFlag("--slots", flags.SlotsEndpoint)
	a.boolFlag("--metrics", flags.MetricsEndpoint)

	// The escape hatch, last, after the validation ParseExtraFlags performed.
	a.out = append(a.out, extra...)
	return a.out, nil
}

// BenchPoint is the part of a `llama-bench` command line that comes from the
// SWEEP rather than from the FlagSet (section 10.1): prompt length, generation
// length, depth and repetitions. Nil fields are omitted, so llama-bench applies
// its own defaults.
type BenchPoint struct {
	PromptLen   *int // -p
	GenLen      *int // -n
	Depth       *int // -d
	Repetitions *int // -r
	// ExtraFlags is `bench.extra_flags`, llama-bench's own escape hatch. It is
	// validated against its own forbidden list (`-m`, `-o`, `-r`) — NOT against
	// the server's — because these are llama-bench flags.
	ExtraFlags string
}

// RenderBenchArgv renders the `llama-bench` command line for one sweep point
// (D62, section 10.1).
//
// It is a separate function rather than a mode of RenderArgv because llama-bench
// is a different program with a different parser: no `-c` (context is derived
// from -p/-n/-d, and passing it is an unrecognized-argument exit), no `--alias`,
// no `--host`/`--port`, `-fa 0|1` rather than `on|off|auto`, and a sweep-list
// syntax. Every field section 10.1 marks dropped is dropped here, and
// BenchIgnoredFlags is what makes each drop visible in `GET /bench/preflight`
// rather than silent.
func RenderBenchArgv(
	flags model.FlagSet,
	primary *ModelFile,
	version Runtime,
	point BenchPoint,
) ([]string, error) {
	if primary == nil || primary.Path == "" {
		return nil, model.Error{
			Code:    model.CodeModelMissing,
			Message: "this benchmark has no resolved model file to run against",
		}
	}

	extra, err := parseWords(point.ExtraFlags, forbiddenBenchFlags,
		"bench.extra_flags may not override %s: llama-bench's model, output format and "+
			"repetition count come from the sweep")
	if err != nil {
		return nil, err
	}

	a := &argv{out: []string{version.BenchPath()}}
	a.pair("-m", primary.Path)

	// `auto` has no meaning here: llama-bench has no --fit, so the substitution
	// to 999 is made explicitly and BenchNotes records it for the results table.
	a.pair("-ngl", strconv.Itoa(benchNGL(flags.NGpuLayers)))

	a.intFlag("-b", flags.BatchSize)
	a.intFlag("-ub", flags.UbatchSize)
	a.intFlag("-t", flags.Threads)
	a.intFlag("-tb", flags.ThreadsBatch)

	if flags.FlashAttn != nil {
		// llama-bench takes no tri-state: on→1, off→0, auto→1 (section 10.1).
		a.pair("-fa", benchFlashAttn(*flags.FlashAttn))
	}
	a.strFlag("-ctk", flags.CacheTypeK)
	a.strFlag("-ctv", flags.CacheTypeV)

	a.strFlag("--device", flags.DeviceFilter)
	if flags.SplitMode != nil {
		a.pair("-sm", string(*flags.SplitMode))
	}
	if len(flags.TensorSplit) > 0 {
		a.pair("-ts", joinFloats(flags.TensorSplit))
	}
	a.intFlag("-mg", flags.MainGPU)

	a.boolFlag("--mlock", flags.Mlock)
	a.boolFlag("--no-mmap", flags.NoMmap)
	a.strFlag("--numa", flags.Numa)
	a.strFlag("-C", flags.CPUMask)
	a.intFlag("--prio", flags.Prio)

	// The sweep point, then the output format the parser depends on.
	a.intFlag("-p", point.PromptLen)
	a.intFlag("-n", point.GenLen)
	a.intFlag("-d", point.Depth)
	a.intFlag("-r", point.Repetitions)
	a.pair("-o", "json")

	a.out = append(a.out, extra...)
	return a.out, nil
}

// BenchNotes are the substitutions RenderBenchArgv had to make, for the `notes`
// of a bench run. Today there is exactly one: llama-bench takes no tri-state
// flash-attention, so `auto` becomes `-fa 1`.
func BenchNotes(flags model.FlagSet) []string {
	var notes []string
	if flags.FlashAttn != nil && *flags.FlashAttn == model.FlashAttnAuto {
		notes = append(notes, `flash_attn "auto" ran as -fa 1: llama-bench takes no tri-state`)
	}
	if flags.NGpuLayers != nil && flags.NGpuLayers.Mode == model.NGLAuto {
		notes = append(notes, fmt.Sprintf("ngl=%d (auto): llama-bench has no --fit, so "+
			"\"let llama.cpp decide\" has no meaning here", NGLAllValue))
	}
	return notes
}

// IgnoredFlag is one entry of `GET /bench/preflight`'s `ignored_flags`.
type IgnoredFlag struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

// BenchIgnoredFlags lists the FlagSet fields RenderBenchArgv dropped, with the
// reason section 10.1 gives for each.
//
// Every dropped field is dropped LOUDLY: the sweep builder renders these as a
// dismissible note above the estimate, so "why is my benchmark not measuring my
// 32k context" is answered before the run rather than after it.
func BenchIgnoredFlags(flags model.FlagSet, extraFlags string) []IgnoredFlag {
	var out []IgnoredFlag
	add := func(set bool, field, reason string) {
		if set {
			out = append(out, IgnoredFlag{Field: field, Reason: reason})
		}
	}

	add(flags.CtxSize != nil, "ctx_size",
		"llama-bench has no -c; context is derived from -p/-n/-d")

	const serverOnly = "server-only concept; llama-bench has no server"
	add(flags.Alias != nil, "alias", serverOnly)
	add(flags.Parallel != nil, "parallel", serverOnly)
	add(flags.ContBatching != nil, "cont_batching", serverOnly)
	add(flags.Embedding != nil, "embedding", serverOnly)
	add(flags.Pooling != nil, "pooling", serverOnly)
	add(flags.Rerank != nil, "rerank", serverOnly)
	add(flags.Jinja != nil, "jinja", serverOnly)
	add(flags.ChatTemplate != nil, "chat_template", serverOnly)
	add(flags.ChatTemplateFile != nil, "chat_template_file", serverOnly)
	add(flags.NKeep != nil, "n_keep", serverOnly)
	add(flags.NPredict != nil, "n_predict", serverOnly)
	add(flags.DefragThold != nil, "defrag_thold", serverOnly)
	add(flags.CacheReuse != nil, "cache_reuse", serverOnly)
	add(flags.SlotSavePath != nil, "slot_save_path", serverOnly)
	add(flags.PropsEndpoint != nil, "props_endpoint", serverOnly)
	add(flags.SlotsEndpoint != nil, "slots_endpoint", serverOnly)
	add(flags.MetricsEndpoint != nil, "metrics_endpoint", serverOnly)
	add(flags.LogVerbosity != nil, "log_verbosity", serverOnly)

	const ropeYarn = "server-only in this design; the sweep measures a fixed rope configuration"
	add(flags.RopeScaling != nil, "rope_scaling", ropeYarn)
	add(flags.RopeFreqBase != nil, "rope_freq_base", ropeYarn)
	add(flags.RopeFreqScale != nil, "rope_freq_scale", ropeYarn)
	add(flags.YarnExtFactor != nil, "yarn_ext_factor", ropeYarn)
	add(flags.YarnAttnFactor != nil, "yarn_attn_factor", ropeYarn)

	add(flags.Draft != nil, "draft",
		"speculative decoding is a server feature; a draft-model bench is out of v1 scope")
	add(strings.TrimSpace(extraFlags) != "", "extra_flags",
		"they are llama-server flags; use bench.extra_flags for llama-bench's own escape hatch")
	return out
}

// UnknownFlags diffs the flags in argv against what a build advertises
// (section 5.7's flag-churn guard).
//
// It is a sibling of the renderers rather than part of them: RenderArgv returns
// argv and nothing else, and only its CALLERS — the instance form's
// `POST /instances/validate`, `GET /instances/{id}/command` and the
// version-activation preflight — run this check. The launcher never does: it has
// no user to warn and no reason to spend the work.
//
// A build with no help capture makes the check UNAVAILABLE, not universally
// failing: help.Available() is false and this returns nothing, so the UI says
// "flag check unavailable for this build" rather than flagging every flag.
func UnknownFlags(argv []string, help model.HelpFlags) []string {
	if !help.Available() {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, tok := range argv {
		name, ok := flagName(tok)
		if !ok || seen[name] || help.Has(name) {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// flagName reports whether tok is a flag, and its name with any `=value`
// removed. argv[0] is a path and every value is either a number, a path or a
// word, so "starts with a dash and then a letter" is the whole test — the same
// shape the D49 import-graph test looks for.
func flagName(tok string) (string, bool) {
	if len(tok) < 2 || tok[0] != '-' {
		return "", false
	}
	rest := strings.TrimLeft(tok, "-")
	if rest == "" || !isFlagLetter(rest[0]) {
		return "", false
	}
	if i := strings.IndexByte(tok, '='); i >= 0 {
		tok = tok[:i]
	}
	return tok, true
}

func isFlagLetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// renderNGL is D51's table, and the one place `-ngl auto` is interpreted.
//
//	all   → -ngl 999
//	none  → -ngl 0
//	count → -ngl <count>
//	auto  → nothing at all, unless this build predates --fit
//
// The nil FlagSet field behaves as `auto`: a configuration that never mentioned
// the offload must not pin it, because pinning it is what turns --fit off.
func renderNGL(ngl *model.NGpuLayers, supportsFit bool) (int, bool) {
	mode := model.NGLAuto
	var count *int
	if ngl != nil {
		mode, count = ngl.Mode, ngl.Count
	}
	switch mode {
	case model.NGLAll:
		return NGLAllValue, true
	case model.NGLNone:
		return 0, true
	case model.NGLCount:
		if count == nil {
			// Validate rejects this at save time; a stored row that predates a
			// validator behaves as `all` rather than as `-ngl <nothing>`, which
			// would be an unparsable command line.
			return NGLAllValue, true
		}
		return *count, true
	default:
		if !supportsFit {
			// Section 5.7: this build predates --fit, so `auto` behaves as
			// `all` and the instance form says so.
			return NGLAllValue, true
		}
		return 0, false
	}
}

// benchNGL is the same table with one substitution: llama-bench has no --fit,
// so `auto` becomes 999 rather than "omit the flag" (section 10.1).
func benchNGL(ngl *model.NGpuLayers) int {
	if n, ok := renderNGL(ngl, false); ok {
		return n
	}
	return NGLAllValue
}

// benchFlashAttn is section 10.1's mapping: on→1, off→0, auto→1.
func benchFlashAttn(f model.FlashAttn) string {
	if f == model.FlashAttnOff {
		return "0"
	}
	return "1"
}

// joinFloats renders a tensor split as llama.cpp reads it: comma-separated, not
// repeated.
func joinFloats(vs []float64) string {
	parts := make([]string, len(vs))
	for i, v := range vs {
		parts[i] = formatFloat(v)
	}
	return strings.Join(parts, ",")
}

// formatFloat renders a float flag value in decimal, never in exponent form.
// `1e+06` is valid input for llama.cpp's own `std::stof`, but it is not what a
// user typed into the rope-frequency field and it is not what they want to read
// back out of `GET /instances/{id}/command`. The shortest decimal that round
// trips is both.
func formatFloat(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// argv accumulates a command line. Every helper is "nil means do not pass the
// flag", which is the FlagSet's central rule expressed once instead of at forty
// call sites.
type argv struct{ out []string }

func (a *argv) flag(name string)        { a.out = append(a.out, name) }
func (a *argv) pair(name, value string) { a.out = append(a.out, name, value) }
func (a *argv) strFlag(name string, v *string) {
	if v != nil {
		a.pair(name, *v)
	}
}

func (a *argv) intFlag(name string, v *int) {
	if v != nil {
		a.pair(name, strconv.Itoa(*v))
	}
}

func (a *argv) floatFlag(name string, v *float64) {
	if v != nil {
		a.pair(name, formatFloat(*v))
	}
}

// boolFlag renders a flag that has no negative form: true passes it, false and
// nil do not. `cont_batching` is the exception and is written out inline.
func (a *argv) boolFlag(name string, v *bool) {
	if v != nil && *v {
		a.flag(name)
	}
}
