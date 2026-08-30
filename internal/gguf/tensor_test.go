package gguf_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/jlbyh2o/llamaman/internal/gguf"
)

// TestTensorTypeTable pins the ggml type table with hand-written literals.
//
// It is deliberately not derived from anything: the fixture builder computes its
// tensor layout with the same table the parser uses, so a wrong entry there
// would lay a file out wrongly AND read it back consistently, and every
// round-trip test would pass while every weight figure in DESIGN section 8.2 was
// off. Only literals written from `sizeof(block_*)` catch that, so here they
// are, with the bits-per-weight each implies as a second reading of the same
// number.
func TestTensorTypeTable(t *testing.T) {
	tests := []struct {
		typ   gguf.GGMLType
		name  string
		block uint64
		size  uint64
		bpw   float64
	}{
		{gguf.TypeF32, "f32", 1, 4, 32},
		{gguf.TypeF16, "f16", 1, 2, 16},
		{gguf.TypeBF16, "bf16", 1, 2, 16},
		{gguf.TypeF64, "f64", 1, 8, 64},
		{gguf.TypeI8, "i8", 1, 1, 8},
		{gguf.TypeI16, "i16", 1, 2, 16},
		{gguf.TypeI32, "i32", 1, 4, 32},
		{gguf.TypeI64, "i64", 1, 8, 64},

		// The legacy 32-element quants. DESIGN section 8.3's bytes-per-element
		// table for KV cache types is the same arithmetic seen per element:
		// q8_0 is 34/32 = 1.0625, q4_0 is 18/32 = 0.5625.
		{gguf.TypeQ4_0, "q4_0", 32, 18, 4.5},
		{gguf.TypeQ4_1, "q4_1", 32, 20, 5},
		{gguf.TypeQ5_0, "q5_0", 32, 22, 5.5},
		{gguf.TypeQ5_1, "q5_1", 32, 24, 6},
		{gguf.TypeQ8_0, "q8_0", 32, 34, 8.5},
		{gguf.TypeQ8_1, "q8_1", 32, 36, 9},
		{gguf.TypeIQ4_NL, "iq4_nl", 32, 18, 4.5},
		{gguf.TypeMXFP4, "mxfp4", 32, 17, 4.25},

		// The 256-element K-quants.
		{gguf.TypeQ2_K, "q2_K", 256, 84, 2.625},
		{gguf.TypeQ3_K, "q3_K", 256, 110, 3.4375},
		{gguf.TypeQ4_K, "q4_K", 256, 144, 4.5},
		{gguf.TypeQ5_K, "q5_K", 256, 176, 5.5},
		{gguf.TypeQ6_K, "q6_K", 256, 210, 6.5625},
		{gguf.TypeQ8_K, "q8_K", 256, 292, 9.125},

		// The i-quants, whose advertised names ARE their bits per weight.
		{gguf.TypeIQ1_S, "iq1_s", 256, 50, 1.5625},
		{gguf.TypeIQ1_M, "iq1_m", 256, 56, 1.75},
		{gguf.TypeIQ2_XXS, "iq2_xxs", 256, 66, 2.0625},
		{gguf.TypeIQ2_XS, "iq2_xs", 256, 74, 2.3125},
		{gguf.TypeIQ2_S, "iq2_s", 256, 82, 2.5625},
		{gguf.TypeIQ3_XXS, "iq3_xxs", 256, 98, 3.0625},
		{gguf.TypeIQ3_S, "iq3_s", 256, 110, 3.4375},
		{gguf.TypeIQ4_XS, "iq4_xs", 256, 136, 4.25},

		// The ternary quants.
		{gguf.TypeTQ1_0, "tq1_0", 256, 54, 1.6875},
		{gguf.TypeTQ2_0, "tq2_0", 256, 66, 2.0625},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.typ.Valid() {
				t.Fatalf("type %d is not in the table", tc.typ)
			}
			if got := tc.typ.String(); got != tc.name {
				t.Errorf("String() = %q, want %q", got, tc.name)
			}
			if got := tc.typ.BlockSize(); got != tc.block {
				t.Errorf("BlockSize() = %d, want %d", got, tc.block)
			}
			if got := tc.typ.TypeSize(); got != tc.size {
				t.Errorf("TypeSize() = %d, want %d", got, tc.size)
			}
			if got := tc.typ.BitsPerWeight(); got != tc.bpw {
				t.Errorf("BitsPerWeight() = %v, want %v", got, tc.bpw)
			}
			if want := tc.block > 1; tc.typ.Quantized() != want {
				t.Errorf("Quantized() = %v, want %v", tc.typ.Quantized(), want)
			}
		})
	}

	// The holes are holes on purpose: 4 and 5 were Q4_2 and Q4_3, 31 to 33 the
	// repacked Q4_0 variants, 36 to 38 the repacked IQ4_NL ones. A file naming
	// one cannot be sized and must not be guessed at.
	for _, n := range []uint32{4, 5, 31, 32, 33, 36, 37, 38, 40, 1 << 20} {
		if gguf.GGMLType(n).Valid() {
			t.Errorf("ggml type %d reports valid; it is a removed or unknown type", n)
		}
	}
}

// TestTensorSizeArithmetic is DESIGN section 8.2's
// `bytes(t) = (numel(t) / block_size(type)) × type_size(type)`, in cases chosen
// so the block division actually does something.
func TestTensorSizeArithmetic(t *testing.T) {
	tests := []struct {
		name     string
		info     gguf.TensorInfo
		elements uint64
		bytes    uint64
	}{
		{
			name:     "f32 vector",
			info:     gguf.TensorInfo{Dims: []uint64{512}, Type: gguf.TypeF32},
			elements: 512, bytes: 2048,
		},
		{
			name:     "q4_K matrix",
			info:     gguf.TensorInfo{Dims: []uint64{512, 1536}, Type: gguf.TypeQ4_K},
			elements: 786432, bytes: 786432 / 256 * 144,
		},
		{
			name:     "q6_K output head",
			info:     gguf.TensorInfo{Dims: []uint64{4096, 128256}, Type: gguf.TypeQ6_K},
			elements: 4096 * 128256, bytes: 4096 * 128256 / 256 * 210,
		},
		{
			name:     "three-dimensional expert tensor",
			info:     gguf.TensorInfo{Dims: []uint64{512, 768, 8}, Type: gguf.TypeQ4_K},
			elements: 512 * 768 * 8, bytes: 512 * 768 * 8 / 256 * 144,
		},
		{
			name:     "scalar",
			info:     gguf.TensorInfo{Dims: nil, Type: gguf.TypeF32},
			elements: 1, bytes: 4,
		},
		{
			name:     "unknown type has no size",
			info:     gguf.TensorInfo{Dims: []uint64{32}, Type: gguf.GGMLType(4)},
			elements: 32, bytes: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.info.Elements(); got != tc.elements {
				t.Errorf("Elements() = %d, want %d", got, tc.elements)
			}
			if got := tc.info.Bytes(); got != tc.bytes {
				t.Errorf("Bytes() = %d, want %d", got, tc.bytes)
			}
		})
	}
}

// TestLayerIndex covers the `^blk\.i\.` match that separates a layer's weights
// from the token embedding and the output head — the split DESIGN section 8.2
// needs because `-ngl all` and `-ngl L` differ by exactly the tensors it puts on
// the other side.
func TestLayerIndex(t *testing.T) {
	tests := []struct {
		name  string
		want  int
		found bool
	}{
		{"blk.0.attn_norm.weight", 0, true},
		{"blk.7.ffn_down.weight", 7, true},
		{"blk.123.attn_q.weight", 123, true},
		{"token_embd.weight", 0, false},
		{"output_norm.weight", 0, false},
		{"output.weight", 0, false},
		{"blk.weight", 0, false},
		{"blk..weight", 0, false},
		{"blk.x.weight", 0, false},
		{"blk.-1.weight", 0, false},
		{"blk.0", 0, false},
		{"v.blk.0.attn_q.weight", 0, false},
		{"blk.99999999999999999999.weight", 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := gguf.TensorInfo{Name: tc.name}.LayerIndex()
			if ok != tc.found {
				t.Fatalf("LayerIndex() found = %v, want %v", ok, tc.found)
			}
			if ok && got != tc.want {
				t.Errorf("LayerIndex() = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestSizesBucketing is section 8.2's bucketing against the llama fixture, with
// every expected number computed by hand from the tensor list rather than from
// the code under test.
//
//	per layer: attn_norm  f32  512            =        2 048
//	           attn_qkv   q4_K 512 x 1536     =      442 368
//	           ffn_up     q4_K 512 x 1024     =      294 912
//	           ffn_down   q4_K 1024 x 512     =      294 912
//	                                            ------------
//	                                                1 034 240   x 4 layers
//	other:     token_embd q4_K 512 x 256      =       73 728
//	           output_norm f32 512            =        2 048
//	           output     q6_K 512 x 256      =      107 520
//	                                            ------------
//	                                                  183 296
func TestSizesBucketing(t *testing.T) {
	const (
		perLayer = 2048 + 442368 + 294912 + 294912
		other    = 73728 + 2048 + 107520
		total    = 4*perLayer + other
	)

	f := loadFixture(t, "llama.gguf")
	s := f.Sizes()

	if s.TensorCount != 19 {
		t.Errorf("TensorCount = %d, want 19 (1 embedding + 4x4 layer + 2 head)", s.TensorCount)
	}
	if s.Total != total {
		t.Errorf("Total = %d, want %d", s.Total, total)
	}
	if s.Other != other {
		t.Errorf("Other = %d, want %d", s.Other, other)
	}
	want := []uint64{perLayer, perLayer, perLayer, perLayer}
	if diff := cmp.Diff(want, s.Layer); diff != "" {
		t.Errorf("Layer (-want +got):\n%s", diff)
	}
	if sum := sumOf(s.Layer) + s.Other; sum != s.Total {
		t.Errorf("layers %d + other %d = %d, but Total is %d", sumOf(s.Layer), s.Other, sum, s.Total)
	}

	// The type mix, ordered by ggml type number: f32 first, then q4_K, then q6_K.
	byType := map[gguf.GGMLType]gguf.TypeUsage{}
	for _, u := range s.ByType {
		byType[u.Type] = u
	}
	if got := byType[gguf.TypeF32].Tensors; got != 5 {
		t.Errorf("f32 tensors = %d, want 5 (4 attn_norm + output_norm)", got)
	}
	if got := byType[gguf.TypeQ4_K].Tensors; got != 13 {
		t.Errorf("q4_K tensors = %d, want 13 (token_embd + 3 per layer)", got)
	}
	if got := byType[gguf.TypeQ6_K].Bytes; got != 107520 {
		t.Errorf("q6_K bytes = %d, want 107520", got)
	}
	for i := 1; i < len(s.ByType); i++ {
		if s.ByType[i-1].Type >= s.ByType[i].Type {
			t.Fatalf("ByType is not ordered by type number: %v", s.ByType)
		}
	}

	// DataSize is the same tensors, each padded up to the alignment. With
	// 32-byte alignment and sizes that are already multiples of 32, it equals
	// the total exactly — the case worth stating, because it is what makes an
	// unpadded total a safe number to show a user.
	if int64(total) != f.DataSize {
		t.Errorf("DataSize = %d, want %d", f.DataSize, total)
	}
}

func sumOf(xs []uint64) uint64 {
	var n uint64
	for _, x := range xs {
		n += x
	}
	return n
}

// TestDominantType covers the quant identification that has to work when
// `general.file_type` is absent: the answer is the quantized type holding the
// most bytes, not the type holding the most TENSORS — a model has more f32
// norms than anything else and they are not what it is called.
func TestDominantType(t *testing.T) {
	tests := []struct {
		fixture string
		want    gguf.GGMLType
	}{
		{"llama.gguf", gguf.TypeQ4_K},
		{"qwen3.gguf", gguf.TypeQ8_0},
		{"gemma3.gguf", gguf.TypeQ4_K},
		{"moe.gguf", gguf.TypeQ4_K},
		// The projector is f16 throughout, with no quantized tensor at all, so
		// the fallback answers with the plain type rather than nothing.
		{"mmproj.gguf", gguf.TypeF16},
	}
	for _, tc := range tests {
		t.Run(tc.fixture, func(t *testing.T) {
			got, ok := loadFixture(t, tc.fixture).Sizes().DominantType()
			if !ok {
				t.Fatal("DominantType found nothing")
			}
			if got != tc.want {
				t.Errorf("DominantType() = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("no tensors", func(t *testing.T) {
		if _, ok := (gguf.Sizes{}).DominantType(); ok {
			t.Error("DominantType found a type in an empty index")
		}
	})
}

// TestFileTypeNames covers the llama_ftype labels, which are what
// `models.file_type` shows and which are NOT tensor type names: a Q4_K_M file is
// a mixture, and no tensor in it has that type.
func TestFileTypeNames(t *testing.T) {
	tests := []struct {
		ft   uint64
		want string
		ok   bool
	}{
		{0, "F32", true},
		{1, "F16", true},
		{7, "Q8_0", true},
		{15, "Q4_K_M", true},
		{18, "Q6_K", true},
		{30, "IQ4_XS", true},
		{32, "BF16", true},
		{4, "", false},  // removed
		{33, "", false}, // removed
		{9999, "", false},
	}
	for _, tc := range tests {
		got, ok := gguf.FileTypeName(tc.ft)
		if ok != tc.ok || got != tc.want {
			t.Errorf("FileTypeName(%d) = %q, %v; want %q, %v", tc.ft, got, ok, tc.want, tc.ok)
		}
	}
}
