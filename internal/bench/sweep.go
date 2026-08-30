package bench

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/jlbyh2o/llamaman/internal/instances"
	"github.com/jlbyh2o/llamaman/internal/model"
)

// Sweep expansion (DESIGN section 10, "Expansion first").
//
// `POST /bench/runs` expands the cross-product into `bench_points` rows BEFORE
// execution, which is what buys exact progress, exact resume after a crash, and
// a duration estimate from prior runs' median seconds-per-point. Everything in
// this file is therefore pure: a Sweep and a base FlagSet in, an ordered list of
// Points out, with no clock, no database and no filesystem — so the point count
// the sweep builder previews and the point count the worker runs are computed by
// the same function.
//
// # The axes are comma-lists, and both spellings mean the same thing
//
// Section 3.13's example writes them as JSON arrays (`"n_batch":[512,2048]`),
// while llama-bench's own sweep syntax and every UI field a person types into
// are comma-separated strings (`"n_batch":"512,2048"`). Each axis accepts both
// and canonicalizes to the array, so `sweep_json` on the row is one shape
// however the request was written and "run this again" reproduces it exactly.

// Caps on an expansion. A sweep is a cross-product, so the interesting failure
// is not a big number but a small one multiplied six times: three offloads, two
// batch sizes, two ubatch sizes, two flash-attention settings, two K types and
// three tests is 144 points and reads, in a form, like six short lists.
const (
	// MaxPoints is the largest cross-product this daemon will expand. D44 sizes
	// the frontend's chart budget at "a 512-point sweep", which is the design's
	// own figure for the largest run it contemplates, so it is the number here
	// too: past it a sweep is refused with its own count rather than silently
	// truncated, because a truncated benchmark measures something nobody asked
	// for.
	MaxPoints = 512
	// MaxAxisValues caps one axis. Sixteen offloads or sixteen batch sizes is
	// already an unusual experiment; a hundred is a paste accident.
	MaxAxisValues = 16
	// MaxTests caps the `tests` list, which is an axis like any other.
	MaxTests = 16
	// MaxRepetitions caps `-r`. Every repetition is a full generation, so this
	// multiplies the wall clock of every point at once — the one knob that
	// lengthens a sweep without changing its point count.
	MaxRepetitions = 25
)

// Test is one entry of the sweep's `tests` list: a prompt length, a generation
// length, and the depth to run them at. It maps onto llama-bench's `-p`, `-n`
// and `-d`, and it is the one axis that comes from the sweep rather than from
// the FlagSet (section 10.1's last table row).
type Test struct {
	PP    *int `json:"pp,omitempty"`
	TG    *int `json:"tg,omitempty"`
	Depth *int `json:"depth,omitempty"`
}

// Kind is the `bench_results.test_kind` this test shape produces, which is also
// how llama-bench labels its own output rows.
func (t Test) Kind() model.BenchTestKind {
	switch {
	case t.PP != nil && *t.PP > 0 && t.TG != nil && *t.TG > 0:
		return model.TestPPTG
	case t.TG != nil && *t.TG > 0:
		return model.TestTG
	default:
		return model.TestPP
	}
}

// Label renders the test the way llama-bench's own table does — `pp512`,
// `tg128`, `pp512+tg128`, with `@d4096` appended for a depth.
func (t Test) Label() string {
	var parts []string
	if t.PP != nil && *t.PP > 0 {
		parts = append(parts, "pp"+strconv.Itoa(*t.PP))
	}
	if t.TG != nil && *t.TG > 0 {
		parts = append(parts, "tg"+strconv.Itoa(*t.TG))
	}
	label := strings.Join(parts, "+")
	if label == "" {
		label = "default"
	}
	if t.Depth != nil && *t.Depth > 0 {
		label += "@d" + strconv.Itoa(*t.Depth)
	}
	return label
}

// Valid reports whether this test asks for any work at all.
func (t Test) empty() bool {
	return (t.PP == nil || *t.PP <= 0) && (t.TG == nil || *t.TG <= 0)
}

// Sweep is the `bench_runs.sweep_json` document: the base configuration every
// point starts from, the axes that vary across points, and the tests each point
// runs.
//
// Base is a full model.FlagSet because a benchmark measures "the configuration
// the user would actually run" (section 10): the axes vary a handful of fields
// and everything else — cache types, device filter, threads — is held at the
// value the instance form would have saved.
type Sweep struct {
	// Base is the FlagSet each point starts from. Nil is the empty set.
	Base *model.FlagSet `json:"base,omitempty"`

	NGpuLayers  NGLAxis  `json:"n_gpu_layers,omitempty"`
	NBatch      IntAxis  `json:"n_batch,omitempty"`
	NUbatch     IntAxis  `json:"n_ubatch,omitempty"`
	Threads     IntAxis  `json:"threads,omitempty"`
	FlashAttn   BoolAxis `json:"flash_attn,omitempty"`
	TypeK       StrAxis  `json:"type_k,omitempty"`
	TypeV       StrAxis  `json:"type_v,omitempty"`
	SplitMode   StrAxis  `json:"split_mode,omitempty"`
	TensorSplit StrAxis  `json:"tensor_split,omitempty"`

	// Tests is the `-p`/`-n`/`-d` axis. An empty list runs one point per flag
	// combination with llama-bench's own defaults, which is a legal — if
	// unusual — sweep.
	Tests []Test `json:"tests,omitempty"`

	// OnConflict is §3.13's `on_conflict`, and it lives in the sweep document
	// because a DRAFT has to remember it: `sweep_json` is the only column that
	// survives from `POST /bench/runs` to a `POST …/start` days later, and a
	// policy re-guessed at start time would be a different run from the one the
	// user reviewed. Empty means ConflictAbort.
	OnConflict ConflictPolicy `json:"on_conflict,omitempty"`

	// ExtraFlags is `bench.extra_flags`: llama-bench's OWN escape hatch,
	// validated against its own forbidden list (`-m`, `-o`, `-r`) rather than
	// the server's, because these are llama-bench flags (section 10.1's last
	// row). `FlagSet.extra_flags` is dropped, loudly, by BenchIgnoredFlags.
	ExtraFlags string `json:"extra_flags,omitempty"`
}

// ConflictPolicy is §3.13's `on_conflict`: what to do when the exclusivity
// guard finds instances loaded on the target GPUs.
type ConflictPolicy string

const (
	// ConflictAbort refuses the run with `409 bench_gpu_conflict` naming the
	// instances. It is the default because stopping somebody's production
	// instance is not a thing to do by omission.
	ConflictAbort ConflictPolicy = "abort"
	// ConflictStopAndRestore stops them, records their ids in
	// `stopped_instances_json`, and restarts them from a finalizer that runs on
	// success, failure AND cancellation — and again at boot.
	ConflictStopAndRestore ConflictPolicy = "stop_and_restore"
)

// ConflictPolicyValues lists the two policies §3.13 names, in order.
func ConflictPolicyValues() []ConflictPolicy {
	return []ConflictPolicy{ConflictAbort, ConflictStopAndRestore}
}

// Valid reports whether p is one of the two.
func (p ConflictPolicy) Valid() bool {
	for _, v := range ConflictPolicyValues() {
		if p == v {
			return true
		}
	}
	return false
}

// Point is one expanded cell of the cross-product, before it becomes a
// `bench_points` row.
type Point struct {
	// Ordinal is the execution order, from 0.
	Ordinal int
	// Flags is Base with this cell's axis values applied. It is a deep copy, so
	// two points never share a pointer field.
	Flags model.FlagSet
	// Test is this cell's `-p`/`-n`/`-d`.
	Test Test
	// Label is the human-readable cell, `ngl=20 b=2048 fa=1 tg128` — the string
	// `jobs.progress_json`'s `current` carries and the results table's row
	// heading.
	Label string
}

// BenchPoint renders this point's sweep half of the llama-bench command line.
// Repetitions comes from the run rather than from the point, because `-r` is one
// number for the whole sweep.
func (p Point) BenchPoint(repetitions int, extraFlags string) instances.BenchPoint {
	bp := instances.BenchPoint{
		PromptLen:  p.Test.PP,
		GenLen:     p.Test.TG,
		Depth:      p.Test.Depth,
		ExtraFlags: extraFlags,
	}
	if repetitions > 0 {
		bp.Repetitions = &repetitions
	}
	return bp
}

// Expand computes the cross-product, in execution order.
//
// The odometer runs with the LAST axis varying fastest, and `tests` is the last
// axis, so every test shape for one flag combination runs consecutively. That
// ordering is what makes a partially completed sweep readable: point 40 of 144
// is "the first twelve flag combinations, complete", not "a third of every
// combination".
func Expand(s Sweep) ([]Point, error) {
	axes, err := s.axes()
	if err != nil {
		return nil, err
	}

	total := 1
	for _, a := range axes {
		total *= len(a.values)
	}
	if total > MaxPoints {
		return nil, errorf(CodeSweepTooLarge,
			"this sweep expands to %d points and the limit is %d; drop a value from one of "+
				"the axes — a cross-product multiplies, so one fewer batch size removes %d points",
			total, MaxPoints, total/len(axes[len(axes)-1].values))
	}

	base := model.FlagSet{}
	if s.Base != nil {
		base = s.Base.Clone()
	}

	out := make([]Point, 0, total)
	idx := make([]int, len(axes))
	for n := 0; n < total; n++ {
		p := Point{Ordinal: n, Flags: base.Clone()}
		var labels []string
		for i, a := range axes {
			v := a.values[idx[i]]
			v.apply(&p.Flags, &p.Test)
			if v.label != "" {
				labels = append(labels, v.label)
			}
		}
		p.Label = strings.Join(labels, " ")
		out = append(out, p)

		// Increment the odometer from the last axis.
		for i := len(axes) - 1; i >= 0; i-- {
			idx[i]++
			if idx[i] < len(axes[i].values) {
				break
			}
			idx[i] = 0
		}
	}
	return out, nil
}

// axisValue is one setting of one axis: what it does to the point, and how it is
// labeled.
type axisValue struct {
	label string
	apply func(*model.FlagSet, *Test)
}

// axis is one dimension of the cross-product. An axis with no values contributes
// a single no-op value, so an unset axis multiplies the total by one rather than
// by zero.
type axis struct {
	name   string
	values []axisValue
}

// axes builds the dimensions in the order section 3.13's example writes them,
// with `tests` last. The order is fixed rather than derived from the request so
// that two sweeps with the same values expand to the same ordinals whatever
// order the JSON keys arrived in.
func (s Sweep) axes() ([]axis, error) {
	all := []axis{
		{name: "n_gpu_layers", values: nglValues(s.NGpuLayers)},
		{name: "n_batch", values: intValues(s.NBatch, "b", func(f *model.FlagSet, v int) { f.BatchSize = &v })},
		{name: "n_ubatch", values: intValues(s.NUbatch, "ub", func(f *model.FlagSet, v int) { f.UbatchSize = &v })},
		{name: "threads", values: intValues(s.Threads, "t", func(f *model.FlagSet, v int) { f.Threads = &v })},
		{name: "flash_attn", values: flashAttnValues(s.FlashAttn)},
		{name: "type_k", values: strValues(s.TypeK, "ctk", func(f *model.FlagSet, v string) { f.CacheTypeK = &v })},
		{name: "type_v", values: strValues(s.TypeV, "ctv", func(f *model.FlagSet, v string) { f.CacheTypeV = &v })},
		{name: "split_mode", values: splitModeValues(s.SplitMode)},
		{name: "tensor_split", values: tensorSplitValues(s.TensorSplit)},
		{name: "tests", values: testValues(s.Tests)},
	}

	for _, a := range all {
		if len(a.values) > MaxAxisValues {
			return nil, errorf(CodeSweepTooLarge,
				"axis %q has %d values and the limit is %d", a.name, len(a.values), MaxAxisValues)
		}
	}
	if len(s.Tests) > MaxTests {
		return nil, errorf(CodeSweepTooLarge,
			"the sweep lists %d tests and the limit is %d", len(s.Tests), MaxTests)
	}

	// An axis with no values still contributes one no-op cell, so that the
	// odometer below never multiplies by zero and a sweep with no axes at all
	// expands to exactly one point.
	out := make([]axis, 0, len(all))
	for _, a := range all {
		if len(a.values) == 0 {
			a.values = []axisValue{{apply: func(*model.FlagSet, *Test) {}}}
		}
		out = append(out, a)
	}
	return out, nil
}

func nglValues(axis NGLAxis) []axisValue {
	out := make([]axisValue, 0, len(axis))
	for _, v := range axis {
		ngl := v
		label := "ngl=" + strconv.Itoa(benchNGL(ngl))
		if ngl.Mode == model.NGLAuto {
			// Section 10.1: llama-bench has no --fit, so `auto` runs as 999 and
			// the point says so — the substitution is visible in the results
			// table rather than hidden in the argv.
			label += " (auto)"
		}
		out = append(out, axisValue{label: label, apply: func(f *model.FlagSet, _ *Test) {
			copy := ngl
			f.NGpuLayers = &copy
		}})
	}
	return out
}

func intValues(axis IntAxis, prefix string, set func(*model.FlagSet, int)) []axisValue {
	out := make([]axisValue, 0, len(axis))
	for _, v := range axis {
		n := v
		out = append(out, axisValue{
			label: prefix + "=" + strconv.Itoa(n),
			apply: func(f *model.FlagSet, _ *Test) { set(f, n) },
		})
	}
	return out
}

func strValues(axis StrAxis, prefix string, set func(*model.FlagSet, string)) []axisValue {
	out := make([]axisValue, 0, len(axis))
	for _, v := range axis {
		s := v
		out = append(out, axisValue{
			label: prefix + "=" + s,
			apply: func(f *model.FlagSet, _ *Test) { set(f, s) },
		})
	}
	return out
}

func flashAttnValues(axis BoolAxis) []axisValue {
	out := make([]axisValue, 0, len(axis))
	for _, v := range axis {
		on := v
		fa := model.FlashAttnOff
		label := "fa=0"
		if on {
			fa, label = model.FlashAttnOn, "fa=1"
		}
		out = append(out, axisValue{label: label, apply: func(f *model.FlagSet, _ *Test) {
			copy := fa
			f.FlashAttn = &copy
		}})
	}
	return out
}

func splitModeValues(axis StrAxis) []axisValue {
	out := make([]axisValue, 0, len(axis))
	for _, v := range axis {
		sm := model.SplitMode(v)
		out = append(out, axisValue{label: "sm=" + v, apply: func(f *model.FlagSet, _ *Test) {
			copy := sm
			f.SplitMode = &copy
		}})
	}
	return out
}

// tensorSplitValues takes each value as a comma-separated ratio list, which is
// how llama.cpp itself spells `-ts` and how the instance form stores it.
func tensorSplitValues(axis StrAxis) []axisValue {
	out := make([]axisValue, 0, len(axis))
	for _, v := range axis {
		raw := v
		ratios := parseRatios(raw)
		out = append(out, axisValue{label: "ts=" + raw, apply: func(f *model.FlagSet, _ *Test) {
			f.TensorSplit = append([]float64(nil), ratios...)
		}})
	}
	return out
}

func testValues(tests []Test) []axisValue {
	out := make([]axisValue, 0, len(tests))
	for _, t := range tests {
		test := t
		out = append(out, axisValue{
			label: test.Label(),
			apply: func(_ *model.FlagSet, dst *Test) { *dst = test },
		})
	}
	return out
}

// benchNGL is section 10.1's `-ngl` number for a point, and it is the same
// substitution instances.RenderBenchArgv makes: all→999, none→0, count→N, and
// auto→999 because llama-bench has no --fit.
func benchNGL(ngl model.NGpuLayers) int {
	switch ngl.Mode {
	case model.NGLNone:
		return 0
	case model.NGLCount:
		if ngl.Count != nil {
			return *ngl.Count
		}
		return instances.NGLAllValue
	default:
		return instances.NGLAllValue
	}
}

// Validate checks the sweep's own values — the ones no renderer would catch,
// because a renderer is handed a FlagSet that has already been validated.
func (s Sweep) Validate() error {
	if s.Base != nil {
		if err := s.Base.Validate(); err != nil {
			return errorf(model.CodeBadFlags, "the sweep's base configuration is invalid: %s", err)
		}
	}
	for _, v := range s.SplitMode {
		if !model.SplitMode(v).Valid() {
			return errorf(model.CodeBadFlags, "split_mode %q is not one of none, layer, row", v)
		}
	}
	for _, v := range s.TensorSplit {
		if len(parseRatios(v)) == 0 {
			return errorf(model.CodeBadFlags,
				"tensor_split %q is not a comma-separated list of ratios", v)
		}
	}
	for _, name := range []struct {
		field string
		axis  IntAxis
	}{
		{"n_batch", s.NBatch}, {"n_ubatch", s.NUbatch}, {"threads", s.Threads},
	} {
		for _, v := range name.axis {
			if v <= 0 {
				return errorf(model.CodeBadFlags, "%s value %d must be greater than zero", name.field, v)
			}
		}
	}
	if s.OnConflict != "" && !s.OnConflict.Valid() {
		return errorf(model.CodeBadFlags,
			"on_conflict %q is neither %q nor %q", s.OnConflict,
			ConflictAbort, ConflictStopAndRestore)
	}
	for i, t := range s.Tests {
		if t.empty() {
			return errorf(model.CodeBadFlags,
				"tests[%d] asks for neither a prompt nor a generation; one of pp or tg is required", i)
		}
		if t.Depth != nil && *t.Depth < 0 {
			return errorf(model.CodeBadFlags, "tests[%d].depth %d is negative", i, *t.Depth)
		}
	}
	// The WORD split is checked here; the forbidden-flag list (`-m`, `-o`, `-r`)
	// is deliberately NOT re-stated. instances.RenderBenchArgv owns it, every
	// expansion renders every point through that function, and a second copy of
	// the list here is a second copy that could drift from the renderer that
	// actually enforces it (D49 invariant 3).
	if _, err := instances.SplitWords(s.ExtraFlags); err != nil {
		return errorf(model.CodeExtraFlagForbidden,
			"bench.extra_flags could not be split into words: %s", err)
	}
	return nil
}

// Canonical renders the sweep as it is stored in `sweep_json`: every axis as an
// array, whatever spelling the request used.
func (s Sweep) Canonical() (string, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("bench: canonicalize the sweep: %w", err)
	}
	return string(b), nil
}

// ParseSweep decodes a `sweep_json` column or a request body's `sweep` object.
// Unknown fields are rejected, for the reason model.ParseFlagSet rejects them: a
// key nothing expands is either a typo or a newer schema, and silently dropping
// it would make the sweep the user described and the sweep that ran disagree
// with no way to see it.
func ParseSweep(raw []byte) (Sweep, error) {
	var s Sweep
	if len(bytes.TrimSpace(raw)) == 0 {
		return s, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return Sweep{}, errorf(model.CodeBadFlags, "the sweep could not be read: %s", err)
	}
	return s, nil
}

// -----------------------------------------------------------------------------
// The axis types: an array, or the comma-list a person types
// -----------------------------------------------------------------------------

// IntAxis is `[512,2048]` or `"512,2048"`.
type IntAxis []int

// UnmarshalJSON accepts both spellings.
func (a *IntAxis) UnmarshalJSON(b []byte) error {
	tokens, err := axisTokens(b)
	if err != nil {
		return err
	}
	out := make(IntAxis, 0, len(tokens))
	for _, tok := range tokens {
		n, err := strconv.Atoi(strings.TrimSpace(strings.Trim(tok, `"`)))
		if err != nil {
			return fmt.Errorf("%q is not a whole number", tok)
		}
		out = append(out, n)
	}
	*a = dedupe(out)
	return nil
}

// StrAxis is `["f16","q8_0"]` or `"f16,q8_0"`.
type StrAxis []string

// UnmarshalJSON accepts both spellings.
func (a *StrAxis) UnmarshalJSON(b []byte) error {
	tokens, err := axisTokens(b)
	if err != nil {
		return err
	}
	out := make(StrAxis, 0, len(tokens))
	for _, tok := range tokens {
		v := strings.TrimSpace(tok)
		if strings.HasPrefix(v, `"`) {
			var s string
			if err := json.Unmarshal([]byte(v), &s); err != nil {
				return fmt.Errorf("%q is not a string", tok)
			}
			v = s
		}
		if v == "" {
			continue
		}
		out = append(out, v)
	}
	*a = dedupe(out)
	return nil
}

// BoolAxis is `[true,false]` or `"on,off"`. The string spellings are the ones a
// person writes into a field: on/off, 1/0, yes/no, true/false.
type BoolAxis []bool

// UnmarshalJSON accepts both spellings.
func (a *BoolAxis) UnmarshalJSON(b []byte) error {
	tokens, err := axisTokens(b)
	if err != nil {
		return err
	}
	out := make(BoolAxis, 0, len(tokens))
	for _, tok := range tokens {
		v := strings.ToLower(strings.TrimSpace(strings.Trim(tok, `"`)))
		switch v {
		case "true", "1", "on", "yes":
			out = append(out, true)
		case "false", "0", "off", "no":
			out = append(out, false)
		default:
			return fmt.Errorf("%q is not a yes-or-no value", tok)
		}
	}
	*a = dedupe(out)
	return nil
}

// NGLAxis is `[0,20,"all"]` or `"0,20,all"`: a mixed axis, because "twenty
// layers" and "every layer" are both answers to the same question and D51 models
// them as one field with four modes.
type NGLAxis []model.NGpuLayers

// MarshalJSON writes the axis the way a request writes it — `[0,20,"all"]` —
// rather than as the model.NGpuLayers objects it holds.
//
// It exists so `sweep_json` round trips: the canonical form is stored, read back
// by the worker's preflight and by "run this again", and a stored form the
// parser could not read would break both. The object form is still ACCEPTED on
// the way in, so a row written by anything else is not lost.
func (a NGLAxis) MarshalJSON() ([]byte, error) {
	out := make([]any, 0, len(a))
	for _, v := range a {
		if v.Mode == model.NGLCount && v.Count != nil {
			out = append(out, *v.Count)
			continue
		}
		out = append(out, string(v.Mode))
	}
	return json.Marshal(out)
}

// UnmarshalJSON accepts both spellings, and each element may be a number, one of
// the three mode names, or the `{"mode":…,"count":…}` object model.NGpuLayers
// itself marshals as.
func (a *NGLAxis) UnmarshalJSON(b []byte) error {
	tokens, err := axisTokens(b)
	if err != nil {
		return err
	}
	out := make(NGLAxis, 0, len(tokens))
	for _, tok := range tokens {
		if trimmed := strings.TrimSpace(tok); strings.HasPrefix(trimmed, "{") {
			var ngl model.NGpuLayers
			if err := json.Unmarshal([]byte(trimmed), &ngl); err != nil {
				return fmt.Errorf("%q is not an n_gpu_layers object", tok)
			}
			if !ngl.Mode.Valid() {
				return fmt.Errorf("n_gpu_layers mode %q is not one of auto, all, none, count", ngl.Mode)
			}
			out = append(out, ngl)
			continue
		}
		v := strings.ToLower(strings.TrimSpace(strings.Trim(tok, `"`)))
		switch v {
		case string(model.NGLAll):
			out = append(out, model.NGpuLayers{Mode: model.NGLAll})
		case string(model.NGLNone):
			out = append(out, model.NGpuLayers{Mode: model.NGLNone})
		case string(model.NGLAuto):
			out = append(out, model.NGpuLayers{Mode: model.NGLAuto})
		default:
			n, err := strconv.Atoi(v)
			if err != nil {
				return fmt.Errorf("%q is neither a layer count nor one of all, none, auto", tok)
			}
			if n < 0 {
				return fmt.Errorf("n_gpu_layers %d is negative", n)
			}
			count := n
			out = append(out, model.NGpuLayers{Mode: model.NGLCount, Count: &count})
		}
	}
	*a = dedupeNGL(out)
	return nil
}

// axisTokens splits an axis value into element-shaped tokens, accepting both a
// JSON array and the comma-separated string a form field produces.
//
// It splits the string form on commas WITHOUT respecting quoting, and that is
// correct for these axes: every value is a number, a mode name or a llama.cpp
// type name, none of which contains a comma. The one axis whose values do —
// `tensor_split`, whose elements are themselves comma-separated ratio lists —
// therefore takes only the array form for a multi-value sweep, which is what the
// sweep builder sends.
func axisTokens(b []byte) ([]string, error) {
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	if trimmed[0] == '[' {
		var raw []json.RawMessage
		if err := json.Unmarshal(trimmed, &raw); err != nil {
			return nil, err
		}
		out := make([]string, 0, len(raw))
		for _, el := range raw {
			out = append(out, string(el))
		}
		return out, nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return nil, err
		}
		var out []string
		for _, part := range strings.Split(s, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
		return out, nil
	}
	// A bare scalar is a one-value axis, which is what `"n_batch": 512` means.
	return []string{string(trimmed)}, nil
}

// dedupe removes repeats while keeping the first occurrence's position. A
// duplicated axis value is a paste accident that would otherwise double the
// cross-product and produce two identical points.
func dedupe[T comparable](in []T) []T {
	seen := make(map[T]struct{}, len(in))
	out := in[:0]
	for _, v := range in {
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

// dedupeNGL is dedupe for a struct with a pointer field, which is not
// comparable: two values are the same offload when they render to the same
// `-ngl` number and carry the same mode.
func dedupeNGL(in []model.NGpuLayers) []model.NGpuLayers {
	type key struct {
		mode model.NGLMode
		n    int
	}
	seen := make(map[key]struct{}, len(in))
	out := in[:0]
	for _, v := range in {
		k := key{mode: v.Mode, n: benchNGL(v)}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, v)
	}
	return out
}

// parseRatios reads a `-ts` value: "0.6,0.4" into two floats. An unparseable
// element makes the whole value empty, which Validate turns into a 422 rather
// than silently benchmarking a different split.
func parseRatios(s string) []float64 {
	parts := strings.Split(s, ",")
	out := make([]float64, 0, len(parts))
	for _, p := range parts {
		v, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil || v < 0 {
			return nil
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// sortedStrings is used by the export and the preflight to render a set in a
// stable order.
func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
