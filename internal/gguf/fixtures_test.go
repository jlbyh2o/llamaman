package gguf_test

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/jlbyh2o/llamaman/internal/gguf"
	"github.com/jlbyh2o/llamaman/internal/gguf/ggufbuild"
)

// The fixtures.
//
// DESIGN section 15 asks for checked-in headers covering llama, qwen3, gemma3
// (per-layer KV heads plus a sliding window), an MoE, a sharded set and an
// mmproj. These are those six shapes, built rather than downloaded: every
// expected value is written down in shape_test.go beside the builder that
// produced it, which is a thing no real model file can offer, and the whole set
// is a few tens of kilobytes instead of a few gigabytes.
//
// They are checked in AND regenerable. TestFixturesMatchBuilders rebuilds every
// one and compares it byte for byte with the file on disk, so the two can never
// drift; `go test ./internal/gguf -run TestFixturesMatchBuilders -update`
// rewrites them after a deliberate change.
//
// All but one are header-only — no tensor data — which is exactly what a remote
// Range peek sees and what File.Complete reports false for. `complete.gguf`
// carries its (zeroed) data so the other side of that predicate is covered too.

var update = flag.Bool("update", false, "rewrite the GGUF fixtures in testdata/")

type fixture struct {
	name  string
	full  bool // write the tensor data, not just the header
	build func() *ggufbuild.Builder
}

func fixtures() []fixture {
	return []fixture{
		{name: "llama.gguf", build: llamaFixture},
		{name: "llama-be.gguf", build: func() *ggufbuild.Builder { return llamaFixture().BigEndian(true) }},
		{name: "qwen3.gguf", build: qwen3Fixture},
		{name: "gemma3.gguf", build: gemma3Fixture},
		{name: "moe.gguf", build: moeFixture},
		{name: "sharded-00001-of-00002.gguf", build: func() *ggufbuild.Builder { return shardFixture(1) }},
		{name: "sharded-00002-of-00002.gguf", build: func() *ggufbuild.Builder { return shardFixture(2) }},
		{name: "mmproj.gguf", build: mmprojFixture},
		{name: "alltypes.gguf", build: allTypesFixture},
		{name: "bigvocab.gguf", build: bigVocabFixture},
		{name: "complete.gguf", full: true, build: completeFixture},
	}
}

// llamaFixture is the ordinary case: a dense model with a scalar head_count_kv,
// a Q4_K_M mix, and the three non-layer tensors DESIGN section 8.2 charges to
// `W_other`.
func llamaFixture() *ggufbuild.Builder {
	b := ggufbuild.New("llama").
		Set("general.name", ggufbuild.Str("synthetic-llama")).
		Set("general.quantization_version", ggufbuild.U32(2)).
		Set("general.file_type", ggufbuild.U32(15)). // Q4_K_M
		Set("llama.block_count", ggufbuild.U32(4)).
		Set("llama.context_length", ggufbuild.U32(4096)).
		Set("llama.embedding_length", ggufbuild.U32(512)).
		Set("llama.feed_forward_length", ggufbuild.U32(1024)).
		Set("llama.attention.head_count", ggufbuild.U32(8)).
		Set("llama.attention.head_count_kv", ggufbuild.U32(2)).
		Set("tokenizer.ggml.model", ggufbuild.Str("llama")).
		Set("tokenizer.ggml.tokens", ggufbuild.Strs(tokenNames(256)...))
	b.Tensor("token_embd.weight", gguf.TypeQ4_K, 512, 256)
	b.Layers(4, 512, 1024, gguf.TypeQ4_K)
	b.Tensor("output_norm.weight", gguf.TypeF32, 512)
	b.Tensor("output.weight", gguf.TypeQ6_K, 512, 256)
	return b
}

// qwen3Fixture states key_length and value_length explicitly, so the defaulting
// path in Shape is not the only one exercised, and uses grouped-query attention.
func qwen3Fixture() *ggufbuild.Builder {
	b := ggufbuild.New("qwen3").
		Set("general.name", ggufbuild.Str("synthetic-qwen3")).
		Set("general.file_type", ggufbuild.U32(7)). // Q8_0
		Set("qwen3.block_count", ggufbuild.U32(6)).
		Set("qwen3.context_length", ggufbuild.U32(32768)).
		Set("qwen3.embedding_length", ggufbuild.U32(1024)).
		Set("qwen3.feed_forward_length", ggufbuild.U32(2560)).
		Set("qwen3.attention.head_count", ggufbuild.U32(16)).
		Set("qwen3.attention.head_count_kv", ggufbuild.U32(8)).
		Set("qwen3.attention.key_length", ggufbuild.U32(128)).
		Set("qwen3.attention.value_length", ggufbuild.U32(128)).
		Set("qwen3.vocab_size", ggufbuild.U32(151936)).
		Set("tokenizer.ggml.model", ggufbuild.Str("gpt2"))
	b.Tensor("token_embd.weight", gguf.TypeQ8_0, 1024, 512)
	b.Layers(6, 1024, 2560, gguf.TypeQ8_0)
	b.Tensor("output_norm.weight", gguf.TypeF32, 1024)
	return b
}

// gemma3Fixture is D30's case: head_count_kv as a PER-LAYER ARRAY, beside a
// sliding window and the pattern that says which layers use it. It is the
// fixture DESIGN section 8.3 names for fit.SWALayers.
func gemma3Fixture() *ggufbuild.Builder {
	b := ggufbuild.New("gemma3").
		Set("general.name", ggufbuild.Str("synthetic-gemma3")).
		Set("general.file_type", ggufbuild.U32(15)).
		Set("gemma3.block_count", ggufbuild.U32(6)).
		Set("gemma3.context_length", ggufbuild.U32(8192)).
		Set("gemma3.embedding_length", ggufbuild.U32(768)).
		Set("gemma3.feed_forward_length", ggufbuild.U32(1536)).
		Set("gemma3.attention.head_count", ggufbuild.U32(8)).
		Set("gemma3.attention.head_count_kv", ggufbuild.U32s(4, 4, 4, 2, 2, 1)).
		Set("gemma3.attention.key_length", ggufbuild.U32(256)).
		Set("gemma3.attention.value_length", ggufbuild.U32(256)).
		Set("gemma3.attention.sliding_window", ggufbuild.U32(1024)).
		Set("gemma3.attention.sliding_window_pattern", ggufbuild.U32(6)).
		Set("tokenizer.ggml.model", ggufbuild.Str("llama")).
		Set("tokenizer.ggml.tokens", ggufbuild.Strs(tokenNames(64)...))
	b.Tensor("token_embd.weight", gguf.TypeQ4_K, 768, 64)
	b.Layers(6, 768, 1536, gguf.TypeQ4_K)
	b.Tensor("output_norm.weight", gguf.TypeF32, 768)
	return b
}

// moeFixture carries the expert pair and the three-dimensional expert tensors
// that make `file_size / n_layer` averaging wrong (DESIGN section 8.2).
func moeFixture() *ggufbuild.Builder {
	const (
		layers = 4
		embd   = 512
		ff     = 768
		nexp   = 8
	)
	b := ggufbuild.New("qwen3moe").
		Set("general.name", ggufbuild.Str("synthetic-moe")).
		Set("general.file_type", ggufbuild.U32(14)). // Q4_K_S
		Set("qwen3moe.block_count", ggufbuild.U32(layers)).
		Set("qwen3moe.context_length", ggufbuild.U32(8192)).
		Set("qwen3moe.embedding_length", ggufbuild.U32(embd)).
		Set("qwen3moe.feed_forward_length", ggufbuild.U32(ff)).
		Set("qwen3moe.attention.head_count", ggufbuild.U32(8)).
		Set("qwen3moe.attention.head_count_kv", ggufbuild.U32(2)).
		Set("qwen3moe.expert_count", ggufbuild.U32(nexp)).
		Set("qwen3moe.expert_used_count", ggufbuild.U32(2)).
		Set("tokenizer.ggml.model", ggufbuild.Str("gpt2"))
	b.Tensor("token_embd.weight", gguf.TypeQ4_K, embd, 256)
	for i := 0; i < layers; i++ {
		p := "blk." + strconv.Itoa(i) + "."
		b.Tensor(p+"attn_norm.weight", gguf.TypeF32, embd)
		b.Tensor(p+"attn_qkv.weight", gguf.TypeQ4_K, embd, embd*3)
		b.Tensor(p+"ffn_gate_inp.weight", gguf.TypeF32, embd, nexp)
		b.Tensor(p+"ffn_up_exps.weight", gguf.TypeQ4_K, embd, ff, nexp)
		b.Tensor(p+"ffn_down_exps.weight", gguf.TypeQ4_K, ff, embd, nexp)
	}
	b.Tensor("output_norm.weight", gguf.TypeF32, embd)
	b.Tensor("output.weight", gguf.TypeQ6_K, embd, 256)
	return b
}

// shardFixture is one file of a two-file set, carrying the split.* keys DESIGN
// section 7.2a's scan groups shards by. Only the first shard holds the metadata
// a reader needs, which is section 7.3's "only shard 1 is needed to parse
// metadata".
func shardFixture(no int) *ggufbuild.Builder {
	b := ggufbuild.New("llama").
		Set("general.name", ggufbuild.Str("synthetic-sharded")).
		Set("split.no", ggufbuild.U16(uint16(no-1))).
		Set("split.count", ggufbuild.U16(2)).
		Set("split.tensors.count", ggufbuild.I32(10))
	if no == 1 {
		b.Set("llama.block_count", ggufbuild.U32(2)).
			Set("llama.context_length", ggufbuild.U32(2048)).
			Set("llama.embedding_length", ggufbuild.U32(512)).
			Set("llama.feed_forward_length", ggufbuild.U32(1024)).
			Set("llama.attention.head_count", ggufbuild.U32(8)).
			Set("llama.attention.head_count_kv", ggufbuild.U32(8)).
			Set("tokenizer.ggml.model", ggufbuild.Str("llama")).
			Set("tokenizer.ggml.tokens", ggufbuild.Strs(tokenNames(64)...))
		b.Tensor("token_embd.weight", gguf.TypeQ4_0, 512, 64)
		b.Layers(1, 512, 1024, gguf.TypeQ4_0)
	} else {
		b.Tensor("blk.1.attn_norm.weight", gguf.TypeF32, 512)
		b.Tensor("blk.1.attn_qkv.weight", gguf.TypeQ4_0, 512, 1536)
		b.Tensor("blk.1.ffn_up.weight", gguf.TypeQ4_0, 512, 1024)
		b.Tensor("blk.1.ffn_down.weight", gguf.TypeQ4_0, 1024, 512)
		b.Tensor("output_norm.weight", gguf.TypeF32, 512)
	}
	return b
}

// mmprojFixture is a multimodal projector: architecture "clip", the vision
// marker, and none of the language-model geometry. It is the file that proves
// Shape reports what is missing instead of failing (section 2.6's `unknown`
// kind, section 7.2a's `mmproj` classification).
func mmprojFixture() *ggufbuild.Builder {
	b := ggufbuild.New("clip").
		Set("general.name", ggufbuild.Str("synthetic-mmproj")).
		Set("general.file_type", ggufbuild.U32(1)). // F16
		Set("clip.has_vision_encoder", ggufbuild.Bool(true)).
		Set("clip.has_audio_encoder", ggufbuild.Bool(false)).
		Set("clip.projector_type", ggufbuild.Str("mlp"))
	b.Tensor("mm.0.weight", gguf.TypeF16, 1024, 512)
	b.Tensor("mm.0.bias", gguf.TypeF32, 512)
	b.Tensor("v.blk.0.attn_q.weight", gguf.TypeF16, 1024, 1024)
	return b
}

// allTypesFixture holds one key of every metadata value type, an array of each,
// and an array of arrays — the round-trip surface of the format.
func allTypesFixture() *ggufbuild.Builder {
	b := ggufbuild.New("test").
		Set("test.u8", ggufbuild.U8(200)).
		Set("test.i8", ggufbuild.I8(-100)).
		Set("test.u16", ggufbuild.U16(60000)).
		Set("test.i16", ggufbuild.I16(-30000)).
		Set("test.u32", ggufbuild.U32(4000000000)).
		Set("test.i32", ggufbuild.I32(-2000000000)).
		Set("test.f32", ggufbuild.F32(0.5)).
		Set("test.bool_true", ggufbuild.Bool(true)).
		Set("test.bool_false", ggufbuild.Bool(false)).
		Set("test.string", ggufbuild.Str("héllo, wörld")).
		Set("test.string_empty", ggufbuild.Str("")).
		Set("test.u64", ggufbuild.U64(18000000000000000000)).
		Set("test.i64", ggufbuild.I64(-9000000000000000000)).
		Set("test.f64", ggufbuild.F64(-1.25)).
		Set("test.arr_u8", ggufbuild.Arr(gguf.ValueUint8, ggufbuild.U8(1), ggufbuild.U8(2))).
		Set("test.arr_i8", ggufbuild.Arr(gguf.ValueInt8, ggufbuild.I8(-1), ggufbuild.I8(2))).
		Set("test.arr_u16", ggufbuild.Arr(gguf.ValueUint16, ggufbuild.U16(1), ggufbuild.U16(65535))).
		Set("test.arr_i16", ggufbuild.Arr(gguf.ValueInt16, ggufbuild.I16(-32768))).
		Set("test.arr_u32", ggufbuild.U32s(3, 5, 8)).
		Set("test.arr_i32", ggufbuild.Arr(gguf.ValueInt32, ggufbuild.I32(-7))).
		Set("test.arr_f32", ggufbuild.Arr(gguf.ValueFloat32, ggufbuild.F32(1.5), ggufbuild.F32(-2.5))).
		Set("test.arr_bool", ggufbuild.Arr(gguf.ValueBool, ggufbuild.Bool(true), ggufbuild.Bool(false))).
		Set("test.arr_string", ggufbuild.Strs("a", "", "cc")).
		Set("test.arr_u64", ggufbuild.Arr(gguf.ValueUint64, ggufbuild.U64(1<<40))).
		Set("test.arr_i64", ggufbuild.Arr(gguf.ValueInt64, ggufbuild.I64(-(1<<40)))).
		Set("test.arr_f64", ggufbuild.Arr(gguf.ValueFloat64, ggufbuild.F64(3.5))).
		Set("test.arr_empty", ggufbuild.Arr(gguf.ValueUint32)).
		Set("test.arr_nested", ggufbuild.Arr(gguf.ValueArray,
			ggufbuild.U32s(1, 2),
			ggufbuild.U32s(3),
		))
	b.Tensor("only.weight", gguf.TypeF32, 4)
	return b
}

// bigVocabFixture has a token array past DefaultArrayLimit, so the elision path
// of DESIGN section 8.5 — "parsed but not retained" — runs against a real file
// and n_vocab still comes out right.
func bigVocabFixture() *ggufbuild.Builder {
	b := ggufbuild.New("llama").
		Set("llama.block_count", ggufbuild.U32(1)).
		Set("llama.context_length", ggufbuild.U32(512)).
		Set("llama.embedding_length", ggufbuild.U32(256)).
		Set("llama.feed_forward_length", ggufbuild.U32(512)).
		Set("llama.attention.head_count", ggufbuild.U32(4)).
		Set("tokenizer.ggml.model", ggufbuild.Str("llama")).
		Set("tokenizer.ggml.tokens", ggufbuild.Strs(tokenNames(1500)...)).
		Set("tokenizer.ggml.scores", ggufbuild.Arr(gguf.ValueFloat32, floatVals(1500)...))
	b.Tensor("token_embd.weight", gguf.TypeQ4_K, 256, 1500)
	b.Layers(1, 256, 512, gguf.TypeQ4_K)
	return b
}

// completeFixture is the only fixture written with its tensor data, so
// File.Complete has a true case as well as ten false ones.
func completeFixture() *ggufbuild.Builder {
	b := ggufbuild.New("llama").
		Set("llama.block_count", ggufbuild.U32(1)).
		Set("llama.context_length", ggufbuild.U32(256)).
		Set("llama.embedding_length", ggufbuild.U32(256)).
		Set("llama.feed_forward_length", ggufbuild.U32(256)).
		Set("llama.attention.head_count", ggufbuild.U32(4))
	// Deliberately tiny: the point is that the tensor data is PRESENT, and a
	// checked-in fixture full of zeros should be small.
	b.Tensor("token_embd.weight", gguf.TypeQ4_0, 32, 8)
	b.Tensor("blk.0.attn_norm.weight", gguf.TypeF32, 32)
	b.Tensor("blk.0.attn_qkv.weight", gguf.TypeQ4_0, 32, 96)
	return b
}

func tokenNames(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "tok" + strconv.Itoa(i)
	}
	return out
}

func floatVals(n int) []ggufbuild.Val {
	out := make([]ggufbuild.Val, n)
	for i := range out {
		out[i] = ggufbuild.F32(float32(i) / 4)
	}
	return out
}

func fixturePath(name string) string { return filepath.Join("testdata", name) }

// TestFixturesMatchBuilders is what keeps the checked-in bytes and the builders
// above from drifting: it rebuilds every fixture and compares. Run with -update
// to rewrite them after a deliberate change.
func TestFixturesMatchBuilders(t *testing.T) {
	for _, fx := range fixtures() {
		t.Run(fx.name, func(t *testing.T) {
			b := fx.build()
			want := b.Header()
			if fx.full {
				want = b.Full()
			}
			path := fixturePath(fx.name)
			if *update {
				if err := b.WriteFile(path, fx.full); err != nil {
					t.Fatalf("write %s: %v", path, err)
				}
				return
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v (run `go test ./internal/gguf -run TestFixturesMatchBuilders -update` to create it)", path, err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("%s is %d bytes on disk and %d from the builder; they differ (run with -update to rewrite)", path, len(got), len(want))
			}
		})
	}
}

// loadFixture parses a checked-in fixture, with the package defaults.
func loadFixture(t *testing.T, name string, opts ...gguf.Option) *gguf.File {
	t.Helper()
	f, err := gguf.ParseFile(fixturePath(name), opts...)
	if err != nil {
		t.Fatalf("ParseFile(%s): %v", name, err)
	}
	return f
}

// fixtureBytes returns a fixture's bytes, for the tests that mutate them.
func fixtureBytes(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(fixturePath(name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return b
}
