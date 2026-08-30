package bench

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/jlbyh2o/llamaman/internal/model"
)

// Sweep expansion is DESIGN section 15's named unit test for this package:
// "cross-product size and ordering". Size, because a cross-product multiplies
// and a user reads it as six short lists; ordering, because it is what makes a
// partially completed sweep readable and what makes `bench_points.ordinal` a
// stable identity across two expansions of the same sweep.

func ptr[T any](v T) *T { return &v }

// docExampleSweep is section 3.13's own POST body, field for field. It is the
// example the endpoint table shows, so its point count is a fact the design
// states rather than one this test invents: 3 offloads × 2 batch sizes × 2
// flash-attention settings × 2 K types × 3 tests.
func docExampleSweep() Sweep {
	return Sweep{
		NGpuLayers: NGLAxis{
			{Mode: model.NGLCount, Count: ptr(0)},
			{Mode: model.NGLCount, Count: ptr(20)},
			{Mode: model.NGLAll},
		},
		NBatch:    IntAxis{512, 2048},
		FlashAttn: BoolAxis{true, false},
		TypeK:     StrAxis{"f16", "q8_0"},
		Tests: []Test{
			{PP: ptr(512)},
			{TG: ptr(128)},
			{PP: ptr(512), TG: ptr(128), Depth: ptr(4096)},
		},
	}
}

func TestExpandCounts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		sweep Sweep
		want  int
	}{
		{
			name:  "no axes at all is one point",
			sweep: Sweep{},
			want:  1,
		},
		{
			name:  "tests alone",
			sweep: Sweep{Tests: []Test{{PP: ptr(512)}, {TG: ptr(128)}}},
			want:  2,
		},
		{
			name:  "one axis multiplies by its length",
			sweep: Sweep{NBatch: IntAxis{512, 1024, 2048}},
			want:  3,
		},
		{
			name:  "two axes multiply",
			sweep: Sweep{NBatch: IntAxis{512, 2048}, NUbatch: IntAxis{128, 256, 512}},
			want:  6,
		},
		{
			name:  "section 3.13's example: 3 x 2 x 2 x 2 x 3",
			sweep: docExampleSweep(),
			want:  72,
		},
		{
			name: "every axis at once",
			sweep: Sweep{
				NGpuLayers:  NGLAxis{{Mode: model.NGLAll}, {Mode: model.NGLNone}},
				NBatch:      IntAxis{512, 2048},
				NUbatch:     IntAxis{256},
				Threads:     IntAxis{8, 16},
				FlashAttn:   BoolAxis{true, false},
				TypeK:       StrAxis{"f16"},
				TypeV:       StrAxis{"q8_0"},
				SplitMode:   StrAxis{"layer"},
				TensorSplit: StrAxis{"0.6,0.4"},
				Tests:       []Test{{PP: ptr(512)}},
			},
			want: 2 * 2 * 1 * 2 * 2 * 1 * 1 * 1 * 1 * 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			points, err := Expand(tt.sweep)
			if err != nil {
				t.Fatalf("Expand: %v", err)
			}
			if len(points) != tt.want {
				t.Errorf("Expand produced %d points, want %d", len(points), tt.want)
			}
			for i, p := range points {
				if p.Ordinal != i {
					t.Errorf("point %d has ordinal %d", i, p.Ordinal)
				}
			}
		})
	}
}

// TestExpandOrdering pins the odometer: the LAST axis varies fastest, and
// `tests` is the last axis, so every test shape for one flag combination runs
// consecutively. That is what makes "point 40 of 144" mean "the first twelve
// combinations, complete" rather than "a third of every combination".
func TestExpandOrdering(t *testing.T) {
	t.Parallel()

	points, err := Expand(Sweep{
		NBatch: IntAxis{512, 2048},
		Tests:  []Test{{PP: ptr(512)}, {TG: ptr(128)}},
	})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}

	want := []string{
		"b=512 pp512",
		"b=512 tg128",
		"b=2048 pp512",
		"b=2048 tg128",
	}
	got := make([]string, 0, len(points))
	for _, p := range points {
		got = append(got, p.Label)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("expansion order (-want +got):\n%s", diff)
	}
}

// TestExpandAppliesAxesToFlags proves each cell carries its own FlagSet rather
// than a shared pointer: a benchmark measures "the configuration the user would
// actually run", and two points that shared a *int would measure one another's.
func TestExpandAppliesAxesToFlags(t *testing.T) {
	t.Parallel()

	base := model.FlagSet{CacheTypeV: ptr("q8_0"), CtxSize: ptr(8192)}
	points, err := Expand(Sweep{
		Base:      &base,
		NBatch:    IntAxis{512, 2048},
		FlashAttn: BoolAxis{true, false},
	})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(points) != 4 {
		t.Fatalf("got %d points, want 4", len(points))
	}

	for i, p := range points {
		if p.Flags.CacheTypeV == nil || *p.Flags.CacheTypeV != "q8_0" {
			t.Errorf("point %d lost the base cache_type_v", i)
		}
		if p.Flags.CtxSize == nil || *p.Flags.CtxSize != 8192 {
			t.Errorf("point %d lost the base ctx_size", i)
		}
	}
	if *points[0].Flags.BatchSize != 512 || *points[2].Flags.BatchSize != 2048 {
		t.Errorf("batch sizes did not vary: %d, %d",
			*points[0].Flags.BatchSize, *points[2].Flags.BatchSize)
	}
	if *points[0].Flags.FlashAttn != model.FlashAttnOn ||
		*points[1].Flags.FlashAttn != model.FlashAttnOff {
		t.Errorf("flash_attn did not vary: %v, %v",
			*points[0].Flags.FlashAttn, *points[1].Flags.FlashAttn)
	}

	// The base must not have been mutated by any of the four points.
	if base.BatchSize != nil || base.FlashAttn != nil {
		t.Errorf("Expand wrote through into the base FlagSet: %+v", base)
	}
}

// TestExpandCaps is the other half of section 15's "cross-product size": the
// caps, and the fact that they are refusals with a number rather than silent
// truncation. A truncated benchmark measures something nobody asked for.
func TestExpandCaps(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		sweep Sweep
		want  string
	}{
		{
			name:  "an axis past MaxAxisValues",
			sweep: Sweep{NBatch: seqIntAxis(MaxAxisValues + 1)},
			want:  "axis \"n_batch\"",
		},
		{
			name: "a cross-product past MaxPoints",
			sweep: Sweep{
				NBatch:  seqIntAxis(MaxAxisValues),
				NUbatch: seqIntAxis(MaxAxisValues),
				Threads: seqIntAxis(MaxAxisValues),
			},
			want: "expands to 4096 points",
		},
		{
			name:  "too many tests",
			sweep: Sweep{Tests: seqTests(MaxTests + 1)},
			want:  "the limit is 16",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Expand(tt.sweep)
			if err == nil {
				t.Fatal("Expand accepted a sweep past its cap")
			}
			var me model.Error
			if !errors.As(err, &me) || me.Code != CodeSweepTooLarge {
				t.Fatalf("Expand returned %v, want a %s model.Error", err, CodeSweepTooLarge)
			}
			if !strings.Contains(me.Message, tt.want) {
				t.Errorf("message %q does not mention %q", me.Message, tt.want)
			}
		})
	}
}

// TestExpandAtTheCap asserts the boundary is inclusive: exactly MaxPoints is
// accepted, and one more is not. An off-by-one here is the difference between a
// documented limit and a limit that is documented wrong.
func TestExpandAtTheCap(t *testing.T) {
	t.Parallel()

	// 16 × 16 × 2 = 512 = MaxPoints.
	at := Sweep{
		NBatch:    seqIntAxis(16),
		NUbatch:   seqIntAxis(16),
		FlashAttn: BoolAxis{true, false},
	}
	points, err := Expand(at)
	if err != nil {
		t.Fatalf("Expand refused exactly MaxPoints: %v", err)
	}
	if len(points) != MaxPoints {
		t.Fatalf("got %d points, want %d", len(points), MaxPoints)
	}

	over := at
	over.Threads = IntAxis{4, 8}
	if _, err := Expand(over); err == nil {
		t.Error("Expand accepted a sweep of 1024 points")
	}
}

// TestAxisSpellings is the comma-list half of the axis types: section 3.13
// writes them as JSON arrays and every form field a person types into produces a
// comma-separated string, and both must expand to the same points.
func TestAxisSpellings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		json string
		want Sweep
	}{
		{
			name: "arrays, as section 3.13 writes them",
			json: `{"n_batch":[512,2048],"type_k":["f16","q8_0"],"flash_attn":[true,false]}`,
			want: Sweep{
				NBatch: IntAxis{512, 2048}, TypeK: StrAxis{"f16", "q8_0"},
				FlashAttn: BoolAxis{true, false},
			},
		},
		{
			name: "comma lists, as a form field produces them",
			json: `{"n_batch":"512,2048","type_k":"f16,q8_0","flash_attn":"on,off"}`,
			want: Sweep{
				NBatch: IntAxis{512, 2048}, TypeK: StrAxis{"f16", "q8_0"},
				FlashAttn: BoolAxis{true, false},
			},
		},
		{
			name: "a bare scalar is a one-value axis",
			json: `{"n_batch":512,"type_k":"f16","flash_attn":true}`,
			want: Sweep{
				NBatch: IntAxis{512}, TypeK: StrAxis{"f16"}, FlashAttn: BoolAxis{true},
			},
		},
		{
			name: "whitespace around a comma list is trimmed",
			json: `{"n_batch":" 512 , 2048 "}`,
			want: Sweep{NBatch: IntAxis{512, 2048}},
		},
		{
			name: "a repeated value is dropped, keeping the first position",
			json: `{"n_batch":[2048,512,2048],"type_k":["f16","f16"]}`,
			want: Sweep{NBatch: IntAxis{2048, 512}, TypeK: StrAxis{"f16"}},
		},
		{
			name: "n_gpu_layers mixes counts and modes",
			json: `{"n_gpu_layers":[0,20,"all","auto","none"]}`,
			want: Sweep{NGpuLayers: NGLAxis{
				{Mode: model.NGLCount, Count: ptr(0)},
				{Mode: model.NGLCount, Count: ptr(20)},
				{Mode: model.NGLAll},
				{Mode: model.NGLAuto},
				{Mode: model.NGLNone},
			}},
		},
		{
			name: "n_gpu_layers as a comma list",
			json: `{"n_gpu_layers":"0,20,all"}`,
			want: Sweep{NGpuLayers: NGLAxis{
				{Mode: model.NGLCount, Count: ptr(0)},
				{Mode: model.NGLCount, Count: ptr(20)},
				{Mode: model.NGLAll},
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseSweep([]byte(tt.json))
			if err != nil {
				t.Fatalf("ParseSweep: %v", err)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("parsed sweep (-want +got):\n%s", diff)
			}

			// Both spellings must also expand to the same number of points,
			// which is the property the API's `points_total` depends on.
			wantPoints, err := Expand(tt.want)
			if err != nil {
				t.Fatalf("Expand(want): %v", err)
			}
			gotPoints, err := Expand(got)
			if err != nil {
				t.Fatalf("Expand(got): %v", err)
			}
			if len(wantPoints) != len(gotPoints) {
				t.Errorf("expanded to %d points, want %d", len(gotPoints), len(wantPoints))
			}
		})
	}
}

// TestSweepCanonicalRoundTrip: `sweep_json` is stored in ONE shape however the
// request was written, so "run this again" reproduces the run exactly.
func TestSweepCanonicalRoundTrip(t *testing.T) {
	t.Parallel()

	commas, err := ParseSweep([]byte(`{"n_batch":"512,2048","n_gpu_layers":"0,all"}`))
	if err != nil {
		t.Fatalf("ParseSweep: %v", err)
	}
	canonical, err := commas.Canonical()
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	if !strings.Contains(canonical, `"n_batch":[512,2048]`) {
		t.Errorf("canonical form kept the comma spelling: %s", canonical)
	}

	back, err := ParseSweep([]byte(canonical))
	if err != nil {
		t.Fatalf("ParseSweep(canonical): %v", err)
	}
	if diff := cmp.Diff(commas, back); diff != "" {
		t.Errorf("canonical form does not round trip (-first +second):\n%s", diff)
	}
}

func TestSweepValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		sweep Sweep
		code  model.ErrorCode
	}{
		{
			name:  "a split mode llama.cpp does not have",
			sweep: Sweep{SplitMode: StrAxis{"diagonal"}},
			code:  model.CodeBadFlags,
		},
		{
			name:  "a tensor split that is not ratios",
			sweep: Sweep{TensorSplit: StrAxis{"left,right"}},
			code:  model.CodeBadFlags,
		},
		{
			name:  "a batch size of zero",
			sweep: Sweep{NBatch: IntAxis{0}},
			code:  model.CodeBadFlags,
		},
		{
			name:  "a test that asks for neither pp nor tg",
			sweep: Sweep{Tests: []Test{{Depth: ptr(4096)}}},
			code:  model.CodeBadFlags,
		},
		{
			name:  "a conflict policy that is neither",
			sweep: Sweep{OnConflict: "ignore"},
			code:  model.CodeBadFlags,
		},
		{
			name:  "bench extra flags with an unterminated quote",
			sweep: Sweep{ExtraFlags: `--progress "unterminated`},
			code:  model.CodeExtraFlagForbidden,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.sweep.Validate()
			if err == nil {
				t.Fatal("Validate accepted an invalid sweep")
			}
			var me model.Error
			if !errors.As(err, &me) || me.Code != tt.code {
				t.Fatalf("Validate returned %v, want code %s", err, tt.code)
			}
		})
	}

	if err := docExampleSweep().Validate(); err != nil {
		t.Errorf("Validate rejected section 3.13's own example: %v", err)
	}
}

// TestTestKindAndLabel pins the mapping onto `bench_results.test_kind`, which is
// also how llama-bench labels its own rows.
func TestTestKindAndLabel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		test  Test
		kind  model.BenchTestKind
		label string
	}{
		{Test{PP: ptr(512)}, model.TestPP, "pp512"},
		{Test{TG: ptr(128)}, model.TestTG, "tg128"},
		{Test{PP: ptr(512), TG: ptr(128)}, model.TestPPTG, "pp512+tg128"},
		{Test{PP: ptr(512), TG: ptr(128), Depth: ptr(4096)}, model.TestPPTG, "pp512+tg128@d4096"},
	}
	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			if got := tt.test.Kind(); got != tt.kind {
				t.Errorf("Kind = %s, want %s", got, tt.kind)
			}
			if got := tt.test.Label(); got != tt.label {
				t.Errorf("Label = %q, want %q", got, tt.label)
			}
		})
	}
}

func seqIntAxis(n int) IntAxis {
	out := make(IntAxis, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, 64+i)
	}
	return out
}

func seqTests(n int) []Test {
	out := make([]Test, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, Test{PP: ptr(64 + i)})
	}
	return out
}
