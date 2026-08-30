package model

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// FlagSet is the typed form of an instance's `flags_json` column (D41): one
// struct field per llama.cpp flag we expose, so a new upstream flag is a field
// and a golden argv test rather than a migration. A nil field means "do not pass
// the flag", which is distinct from passing its zero value — `parallel: null`
// lets llama-server choose, `parallel: 0` would be an argument.
//
// The field list is DESIGN section 2.8's `flags_json` document, and the mapping
// onto argv is section 5.7 (llama-server) and section 10.1 (llama-bench). Both
// renderers live in internal/instances and nowhere else (D49 invariant 3, D62);
// this file only says what a flag IS, never how it is spelled on a command line.
//
// Anything not modeled here goes in `instances.extra_flags` (SPEC section 3.3's
// escape hatch), so no upstream flag is ever unreachable and no upstream flag
// addition is a migration.
type FlagSet struct {
	// CtxSize is the TOTAL context, shared across the -np slots.
	CtxSize *int `json:"ctx_size,omitempty"`
	// NGpuLayers is the offload decision, and `auto` is a real member rather
	// than a synonym for "all" — see NGpuLayers's own doc comment (D51).
	NGpuLayers *NGpuLayers `json:"n_gpu_layers,omitempty"`

	BatchSize    *int `json:"batch_size,omitempty"`
	UbatchSize   *int `json:"ubatch_size,omitempty"`
	Parallel     *int `json:"parallel,omitempty"`
	Threads      *int `json:"threads,omitempty"`
	ThreadsBatch *int `json:"threads_batch,omitempty"`

	FlashAttn  *FlashAttn `json:"flash_attn,omitempty"`
	CacheTypeK *string    `json:"cache_type_k,omitempty"`
	CacheTypeV *string    `json:"cache_type_v,omitempty"`

	SplitMode *SplitMode `json:"split_mode,omitempty"`
	// TensorSplit indices are into the --device list, not into nvidia-smi's
	// ordering (section 5.7).
	TensorSplit []float64 `json:"tensor_split,omitempty"`
	// MainGPU is likewise an index into the --device list.
	MainGPU *int `json:"main_gpu,omitempty"`
	// DeviceFilter is `--device`, the ONLY device selector (D66). It is
	// rendered verbatim, and the launcher sets no CUDA_VISIBLE_DEVICES: setting
	// both renumbers the devices llama.cpp sees, so `--device CUDA1` would
	// address a different physical card than the one the user picked.
	DeviceFilter *string `json:"device_filter,omitempty"`
	// DeviceUUIDs is the PROVENANCE of DeviceFilter: the GPU UUIDs the user
	// actually picked, resolved to CUDA<n> at save time. It is never rendered
	// into argv — the supervisor compares it against the live map and raises
	// F22 when the ordering changed under the instance (section 5.7).
	DeviceUUIDs []string `json:"device_uuids,omitempty"`

	Mlock  *bool `json:"mlock,omitempty"`
	NoMmap *bool `json:"no_mmap,omitempty"`
	// ContBatching is tri-state on the wire and two-valued in argv: true is
	// `-cb`, false is `-nocb`, and nil passes neither.
	ContBatching *bool `json:"cont_batching,omitempty"`

	Embedding *bool   `json:"embedding,omitempty"`
	Pooling   *string `json:"pooling,omitempty"`
	Rerank    *bool   `json:"rerank,omitempty"`

	Alias            *string `json:"alias,omitempty"`
	ChatTemplate     *string `json:"chat_template,omitempty"`
	ChatTemplateFile *string `json:"chat_template_file,omitempty"`
	Jinja            *bool   `json:"jinja,omitempty"`

	RopeScaling    *string  `json:"rope_scaling,omitempty"`
	RopeFreqBase   *float64 `json:"rope_freq_base,omitempty"`
	RopeFreqScale  *float64 `json:"rope_freq_scale,omitempty"`
	YarnExtFactor  *float64 `json:"yarn_ext_factor,omitempty"`
	YarnAttnFactor *float64 `json:"yarn_attn_factor,omitempty"`

	NKeep       *int     `json:"n_keep,omitempty"`
	NPredict    *int     `json:"n_predict,omitempty"`
	DefragThold *float64 `json:"defrag_thold,omitempty"`
	CacheReuse  *int     `json:"cache_reuse,omitempty"`

	Numa    *string `json:"numa,omitempty"`
	CPUMask *string `json:"cpu_mask,omitempty"`
	Prio    *int    `json:"prio,omitempty"`

	SlotSavePath *string `json:"slot_save_path,omitempty"`

	// Draft is speculative decoding. The draft MODEL is an instance column
	// (`draft_model_id`), not a flag; this is the tuning beside it.
	Draft *DraftFlags `json:"draft,omitempty"`

	// The three server endpoints the gateway and the supervisor read. They are
	// flags rather than constants because `instance_status.requests_served` is
	// NULL when the metrics endpoint is off (section 2.9), which the UI shows
	// as "metrics disabled" rather than as zero.
	PropsEndpoint   *bool `json:"props_endpoint,omitempty"`
	SlotsEndpoint   *bool `json:"slots_endpoint,omitempty"`
	MetricsEndpoint *bool `json:"metrics_endpoint,omitempty"`

	LogVerbosity *int `json:"log_verbosity,omitempty"`
}

// NGLMode is the mode of the `n_gpu_layers` object (D51, section 5.7).
type NGLMode string

const (
	// NGLAuto renders NO -ngl flag at all, which is precisely what leaves
	// llama.cpp's own --fit enabled to choose the offload. It is not resolved
	// anywhere in the launch path: resolving it would need live VRAM, which
	// would make internal/instances impure, would make `config_hash` move
	// because free memory moved, and would disable the --fit projection D33
	// uses as ground truth.
	NGLAuto NGLMode = "auto"
	// NGLAll is `-ngl 999`.
	NGLAll NGLMode = "all"
	// NGLNone is `-ngl 0`.
	NGLNone NGLMode = "none"
	// NGLCount is `-ngl <count>`, the mode POST /instances/{id}/pin-ngl writes
	// when a user pins the calculator's advisory number.
	NGLCount NGLMode = "count"
)

// NGLModeValues lists the four modes, in the order section 2.8 writes them.
func NGLModeValues() []NGLMode { return []NGLMode{NGLAuto, NGLAll, NGLNone, NGLCount} }

// Valid reports whether m is one of the four modes. NGLMode is closed by this
// package alone — `flags_json` is one JSON column, so no SQL CHECK closes it
// and it is deliberately absent from ClosedEnums.
func (m NGLMode) Valid() bool { return valid(m, NGLModeValues()) }

// NGpuLayers is the `n_gpu_layers` object: `{"mode":"auto"|"all"|"none"|"count","count":N}`.
type NGpuLayers struct {
	Mode NGLMode `json:"mode"`
	// Count is read only when Mode is NGLCount.
	Count *int `json:"count,omitempty"`
}

// FlashAttn is `-fa on|off|auto`. llama-server takes the tri-state; llama-bench
// takes `-fa 0|1` and section 10.1 pins `auto` there to 1 with a recorded note,
// which is exactly the kind of divergence D62 splits the two renderers over.
type FlashAttn string

const (
	FlashAttnOn   FlashAttn = "on"
	FlashAttnOff  FlashAttn = "off"
	FlashAttnAuto FlashAttn = "auto"
)

// FlashAttnValues lists the three values, in the order section 2.8 writes them.
func FlashAttnValues() []FlashAttn { return []FlashAttn{FlashAttnOn, FlashAttnOff, FlashAttnAuto} }

// Valid reports whether f is one of the three values.
func (f FlashAttn) Valid() bool { return valid(f, FlashAttnValues()) }

// SplitMode is `-sm none|layer|row`.
type SplitMode string

const (
	SplitNone  SplitMode = "none"
	SplitLayer SplitMode = "layer"
	SplitRow   SplitMode = "row"
)

// SplitModeValues lists the three values, in the order section 2.8 writes them.
func SplitModeValues() []SplitMode { return []SplitMode{SplitNone, SplitLayer, SplitRow} }

// Valid reports whether s is one of the three values.
func (s SplitMode) Valid() bool { return valid(s, SplitModeValues()) }

// DraftFlags is the `draft` object of section 2.8's `flags_json`. Every field is
// nullable for the same reason every flag is: null means "do not pass it".
type DraftFlags struct {
	NMax *int     `json:"n_max,omitempty"`
	NMin *int     `json:"n_min,omitempty"`
	PMin *float64 `json:"p_min,omitempty"`
	// CtxSize is the draft model's own context (-cd), independent of the
	// primary model's -c.
	CtxSize *int `json:"ctx_size,omitempty"`
	// NGpuLayers is the draft model's offload (-ngld). It is a plain count
	// rather than an NGpuLayers object: --fit governs the primary model, and a
	// three-way "auto" for the draft would have nothing to resolve against.
	NGpuLayers *int `json:"n_gpu_layers,omitempty"`
}

// ParseFlagSet decodes a `flags_json` column. Unknown fields are REJECTED: a
// key nothing renders is either a typo or a flag from a newer schema, and
// silently dropping it would make the saved configuration and the running
// configuration disagree with no way to see it. `instance-exec` turns this
// error into exit 65 (`bad_flags`, section 5.6 step 4).
func ParseFlagSet(raw []byte) (FlagSet, error) {
	var f FlagSet
	if len(bytes.TrimSpace(raw)) == 0 {
		return f, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return FlagSet{}, fmt.Errorf("parse flags_json: %w", err)
	}
	return f, nil
}

// ApplyOverride returns f patched by a transient start override
// (`instances.pending_override_json`, D61 / section 3.10b): present keys
// replace, absent keys are untouched.
//
// It never mutates f, and that is not a style preference. The receiver's
// pointer fields are shared structure — patching a copy of the struct in place
// would write through them into the caller's own FlagSet, so a safe start would
// silently rewrite the saved configuration it is defined never to touch. The
// marshal/unmarshal round trip below is a deep copy first and a shallow merge
// second, which is exactly the two properties D61 needs.
//
// Unknown keys are rejected here too: the only producer of an override today is
// safe-start (section 3.10b), so an unrecognized key is our own bug and is
// worth failing the start over rather than launching a configuration nobody
// asked for.
func (f FlagSet) ApplyOverride(raw []byte) (FlagSet, error) {
	deep, err := json.Marshal(f)
	if err != nil {
		return FlagSet{}, fmt.Errorf("copy flags: %w", err)
	}
	var out FlagSet
	if err := json.Unmarshal(deep, &out); err != nil {
		return FlagSet{}, fmt.Errorf("copy flags: %w", err)
	}

	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return out, nil
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return FlagSet{}, fmt.Errorf("apply start override: %w", err)
	}
	return out, nil
}

// Clone returns a deep copy of f, so a caller may edit one field of a stored
// FlagSet — `pin-ngl` rewriting `auto` to a count (section 3.10) — without
// reaching through a shared pointer into the value it was read from.
func (f FlagSet) Clone() FlagSet {
	out, err := f.ApplyOverride(nil)
	if err != nil {
		// ApplyOverride can only fail on a value json cannot round-trip, and
		// every field of FlagSet is a scalar, pointer or slice of scalars. A
		// future field that breaks that returns the zero value rather than
		// taking the caller down.
		return FlagSet{}
	}
	return out
}

// Validate checks the enum-valued flags. It is the value-level half of what
// `POST /instances` and `PATCH /instances/{id}` refuse with `422 bad_flags`;
// the flag-NAME half is internal/instances.UnknownFlags against the active
// build's `help_flags_json` (section 5.7), which is a warning rather than an
// error because llama.cpp ships ~10 nightlies a day.
//
// Values this design does not close — `pooling`, `numa`, `rope_scaling`,
// `cache_type_k`/`cache_type_v` — are deliberately not checked here. Their
// vocabularies belong to llama.cpp and change with it, so pinning them in Go
// would reject a build's own new option; the launcher's exit code and the flag
// warning are what report those.
func (f FlagSet) Validate() error {
	if f.NGpuLayers != nil {
		ngl := *f.NGpuLayers
		if !ngl.Mode.Valid() {
			return fmt.Errorf("n_gpu_layers.mode %q is not one of auto, all, none, count", ngl.Mode)
		}
		if ngl.Mode == NGLCount {
			if ngl.Count == nil {
				return fmt.Errorf("n_gpu_layers.mode is %q but no count was given", NGLCount)
			}
			if *ngl.Count < 0 {
				return fmt.Errorf("n_gpu_layers.count %d is negative", *ngl.Count)
			}
		}
	}
	if f.FlashAttn != nil && !f.FlashAttn.Valid() {
		return fmt.Errorf("flash_attn %q is not one of on, off, auto", *f.FlashAttn)
	}
	if f.SplitMode != nil && !f.SplitMode.Valid() {
		return fmt.Errorf("split_mode %q is not one of none, layer, row", *f.SplitMode)
	}
	for i, v := range f.TensorSplit {
		if v < 0 {
			return fmt.Errorf("tensor_split[%d] is negative", i)
		}
	}
	if f.MainGPU != nil && *f.MainGPU < 0 {
		return fmt.Errorf("main_gpu %d is negative", *f.MainGPU)
	}
	for _, v := range []struct {
		name  string
		value *int
	}{
		{"ctx_size", f.CtxSize},
		{"batch_size", f.BatchSize},
		{"ubatch_size", f.UbatchSize},
		{"parallel", f.Parallel},
	} {
		if v.value != nil && *v.value <= 0 {
			return fmt.Errorf("%s must be greater than zero, got %d", v.name, *v.value)
		}
	}
	return nil
}

// HelpFlags is the parsed `llamacpp_versions.help_flags_json` (section 5.7): the
// set of flag names the active build's own `llama-server --help` advertises.
//
// It is a COLUMN rather than a file for one reason — RenderArgv is pure and may
// not open `manifest.json` — and it is nullable, which this type models as an
// empty value rather than as an empty set. A build whose capture is missing (a
// row migrated in from an older schema) makes the check unavailable, not
// universally failing: see instances.UnknownFlags.
type HelpFlags []string

// Available reports whether this build recorded a help capture at all. When it
// is false the flag-churn guard says "flag check unavailable for this build"
// rather than flagging every flag as unknown.
func (h HelpFlags) Available() bool { return len(h) > 0 }

// Has reports whether name — a flag as it appears in argv, `--jinja` or `-ngl`
// — is one the build advertises.
func (h HelpFlags) Has(name string) bool {
	for _, v := range h {
		if v == name {
			return true
		}
	}
	return false
}
