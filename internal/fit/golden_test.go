package fit_test

import (
	"bytes"
	"strconv"
	"testing"

	"github.com/jlbyh2o/llamaman/internal/fit"
	"github.com/jlbyh2o/llamaman/internal/gguf"
	"github.com/jlbyh2o/llamaman/internal/gguf/ggufbuild"
)

// Golden verdicts over the four synthetic GGUF shapes DESIGN section 15 names —
// llama, gemma3 (per-layer KV heads plus a sliding window), an MoE, and qwen3 —
// at several context lengths and cache types, against hand-computed expected
// bytes whose arithmetic is written out beside each case.
//
// This is an EXTERNAL test package. internal/fit itself imports nothing outside
// the standard library (D49 invariant 5), and building a real GGUF here rather
// than hand-writing a ModelShape is what makes these cases end-to-end: the
// geometry comes out of a parsed file, exactly as the fit route's does.

// shapeOf maps parsed GGUF metadata onto the calculator's inputs. It is the
// mapping section 8.1 describes, spelled out here rather than shared, because in
// a test the point of a conversion is that you can read it.
func shapeOf(f *gguf.File) fit.ModelShape {
	sh := f.Shape()
	sizes := sh.Sizes
	return fit.ModelShape{
		Arch:        sh.Architecture,
		NLayer:      sh.BlockCount,
		NEmbd:       sh.EmbeddingLength,
		NFF:         sh.FeedForwardLength,
		NHead:       sh.HeadCount,
		NHeadKV:     sh.HeadCountKV,
		HeadDimK:    sh.KeyLength,
		HeadDimV:    sh.ValueLength,
		NCtxTrain:   sh.ContextLength,
		NVocab:      sh.VocabSize,
		NExpert:     sh.ExpertCount,
		NExpertUsed: sh.ExpertUsedCount,
		SWAWindow:   sh.SlidingWindow,
		SWAPattern:  sh.SlidingWindowPattern,
		LayerBytes:  sizes.Layer,
		OtherBytes:  sizes.Other,
		FileBytes:   sizes.Total,
	}
}

func parse(t *testing.T, b *ggufbuild.Builder) *gguf.File {
	t.Helper()
	data := b.Header()
	f, err := gguf.Parse(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return f
}

func tokenNames(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "t" + strconv.Itoa(i)
	}
	return out
}

// llamaFixture: a dense model with a scalar head_count_kv.
//
//	L = 4, n_embd = 512, n_ff = 1024, n_head = 8, n_head_kv = 2
//	head_dim_k = head_dim_v = n_embd/n_head = 64, n_vocab = 256
func llamaFixture() *ggufbuild.Builder {
	b := ggufbuild.New("llama").
		Set("general.name", ggufbuild.Str("synthetic-llama")).
		Set("general.file_type", ggufbuild.U32(15)).
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

// qwen3Fixture: grouped-query attention with explicit key/value lengths and a
// 151936-token vocabulary, which is what makes CB_logits the dominant term.
func qwen3Fixture() *ggufbuild.Builder {
	b := ggufbuild.New("qwen3").
		Set("general.name", ggufbuild.Str("synthetic-qwen3")).
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

// gemma3Fixture is D30's case: head_count_kv as a per-layer array beside a
// sliding window and the pattern that says which layers use it.
func gemma3Fixture() *ggufbuild.Builder {
	b := ggufbuild.New("gemma3").
		Set("general.name", ggufbuild.Str("synthetic-gemma3")).
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
// that make `file_size / n_layer` averaging wrong (D29).
func moeFixture() *ggufbuild.Builder {
	const (
		layers = 4
		embd   = 512
		ff     = 768
		nexp   = 8
	)
	b := ggufbuild.New("qwen3moe").
		Set("general.name", ggufbuild.Str("synthetic-moe")).
		Set("qwen3moe.block_count", ggufbuild.U32(layers)).
		Set("qwen3moe.context_length", ggufbuild.U32(8192)).
		Set("qwen3moe.embedding_length", ggufbuild.U32(embd)).
		Set("qwen3moe.feed_forward_length", ggufbuild.U32(ff)).
		Set("qwen3moe.attention.head_count", ggufbuild.U32(8)).
		Set("qwen3moe.attention.head_count_kv", ggufbuild.U32(2)).
		Set("qwen3moe.expert_count", ggufbuild.U32(nexp)).
		Set("qwen3moe.expert_used_count", ggufbuild.U32(2)).
		Set("qwen3moe.vocab_size", ggufbuild.U32(151936)).
		Set("tokenizer.ggml.model", ggufbuild.Str("gpt2"))
	b.Tensor("token_embd.weight", gguf.TypeQ4_K, embd, 256)
	for i := range layers {
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

const gib = uint64(1) << 30

func devices(free uint64) []fit.Device {
	return []fit.Device{{
		Index: 0, UUID: "GPU-golden", Name: "Golden GPU",
		TotalBytes: free, FreeBytes: free, Known: true,
	}}
}

func baseFlags(ctx int, k, v string, fa fit.FlashAttn) fit.Flags {
	return fit.Flags{
		NCtx: ctx, NUbatch: 512, NBatch: 2048, NParallel: 1,
		NGL: fit.NGLAll, FlashAttn: fa, TypeK: k, TypeV: v,
		SplitMode: fit.SplitLayer,
	}
}

// TestGoldenKVAndCompute is the byte-for-byte table: four architectures, three
// contexts, two cache types, with every expected figure's arithmetic in the
// case's own comment.
func TestGoldenKVAndCompute(t *testing.T) {
	cases := []struct {
		name    string
		build   func() *ggufbuild.Builder
		ctx     int
		typeK   string
		typeV   string
		fa      fit.FlashAttn
		wantKV  uint64 // full-attention term
		wantSWA uint64 // sliding-window term
		wantCB  uint64
	}{
		{
			// llama, f16, C = 4096.
			//   per_tok = 2 heads × (64×2 + 64×2)          =        512
			//   KV      = 4096 × 4 layers × 512            =    8388608
			//   logits  = 256 vocab × 512 × 4              =     524288
			//   act     = 6 × 512 × 512 × 4                =    6291456
			//   attn    = 8 × 512 × min(4096,4096) × 4     =   67108864   (FA off)
			//   CB                                         =   73924608
			name:  "llama f16 4096 no flash attention",
			build: llamaFixture, ctx: 4096,
			typeK: fit.CacheTypeF16, typeV: fit.CacheTypeF16, fa: fit.FlashAttnOff,
			wantKV: 8388608, wantCB: 73924608,
		},
		{
			// The same with flash attention: attn = 2 × 512 × 8 × 64 × 4 = 2097152,
			// so CB = 524288 + 6291456 + 2097152 = 8912896. Thirty-three times
			// smaller — this is why section 8.4 keeps the branch.
			name:  "llama f16 4096 with flash attention",
			build: llamaFixture, ctx: 4096,
			typeK: fit.CacheTypeF16, typeV: fit.CacheTypeF16, fa: fit.FlashAttnOn,
			wantKV: 8388608, wantCB: 8912896,
		},
		{
			// Halving the context halves the cache exactly: 2048 × 4 × 512.
			name:  "llama f16 2048",
			build: llamaFixture, ctx: 2048,
			typeK: fit.CacheTypeF16, typeV: fit.CacheTypeF16, fa: fit.FlashAttnOn,
			wantKV: 4194304, wantCB: 8912896,
		},
		{
			// q8_0 is 34 bytes per 32 elements, so a 128-element row is 136:
			//   per_tok = 136 + 136                        =        272
			//   KV      = 4096 × 4 × 272                   =    4456448
			// A calculator that rounded 1.0625 down to 1 would say 4194304.
			name:  "llama q8_0 4096",
			build: llamaFixture, ctx: 4096,
			typeK: fit.CacheTypeQ8_0, typeV: fit.CacheTypeQ8_0, fa: fit.FlashAttnOn,
			wantKV: 4456448, wantCB: 8912896,
		},
		{
			// qwen3, f16, C = 8192, GQA 16/8 with 128-wide heads.
			//   per_tok = 8 × 128 × 2 × 2                  =       4096
			//   KV      = 8192 × 6 × 4096                  =  201326592
			//   logits  = 151936 × 512 × 4                 =  311164928
			//   act     = 6 × 512 × 1024 × 4               =   12582912
			//   attn    = 2 × 512 × 16 × 128 × 4           =    8388608
			//   CB                                         =  332136448
			name:  "qwen3 f16 8192",
			build: qwen3Fixture, ctx: 8192,
			typeK: fit.CacheTypeF16, typeV: fit.CacheTypeF16, fa: fit.FlashAttnOn,
			wantKV: 201326592, wantCB: 332136448,
		},
		{
			// qwen3 at its trained maximum: 32768 × 6 × 4096.
			name:  "qwen3 f16 32768",
			build: qwen3Fixture, ctx: 32768,
			typeK: fit.CacheTypeF16, typeV: fit.CacheTypeF16, fa: fit.FlashAttnOn,
			wantKV: 805306368, wantCB: 332136448,
		},
		{
			// qwen3 with a q8_0 cache: a 1024-element row is 32 blocks × 34 = 1088.
			//   per_tok = 2176, KV = 8192 × 6 × 2176       =  106954752
			name:  "qwen3 q8_0 8192",
			build: qwen3Fixture, ctx: 8192,
			typeK: fit.CacheTypeQ8_0, typeV: fit.CacheTypeQ8_0, fa: fit.FlashAttnOn,
			wantKV: 106954752, wantCB: 332136448,
		},
		{
			// gemma3, f16, C = 8192, U = 512. head_count_kv is [4,4,4,2,2,1] and
			// pattern 6 makes layers 0-4 sliding and layer 5 global.
			//   per_tok(i) = h × 256 × 2 × 2 = h × 1024
			//              = [4096, 4096, 4096, 2048, 2048, 1024]
			//   swa span   = min(8192, 1024 + 512)         =       1536
			//   KV_swa     = 1536 × (4096+4096+4096+2048+2048) = 25165824
			//   KV_full    = 8192 × 1024                   =    8388608
			//   logits     = 64 × 512 × 4                  =     131072
			//   act        = 6 × 512 × 768 × 4             =    9437184
			//   attn       = 2 × 512 × 8 × 256 × 4         =    8388608
			//   CB                                         =   17956864
			name:  "gemma3 f16 8192 sliding window",
			build: gemma3Fixture, ctx: 8192,
			typeK: fit.CacheTypeF16, typeV: fit.CacheTypeF16, fa: fit.FlashAttnOn,
			wantKV: 8388608, wantSWA: 25165824, wantCB: 17956864,
		},
		{
			// At C = 2048 the window is unchanged — it is already narrower than
			// the context — so only the one global layer shrinks:
			//   KV_full = 2048 × 1024 = 2097152, KV_swa unchanged at 25165824.
			name:  "gemma3 f16 2048 sliding window",
			build: gemma3Fixture, ctx: 2048,
			typeK: fit.CacheTypeF16, typeV: fit.CacheTypeF16, fa: fit.FlashAttnOn,
			wantKV: 2097152, wantSWA: 25165824, wantCB: 17956864,
		},
		{
			// MoE. head_count_kv 2, head_dim 64, so the cache is llama-shaped:
			//   per_tok = 512, KV = 4096 × 4 × 512         =    8388608
			//   logits  = 151936 × 512 × 4                 =  311164928
			//   act     = 6 × 512 × 512 × 4                =    6291456
			//   attn    = 2 × 512 × 8 × 64 × 4             =    2097152
			//   moe     = 2 used × 512 × 768 × 4           =    3145728
			//   CB                                         =  322699264
			name:  "moe f16 4096",
			build: moeFixture, ctx: 4096,
			typeK: fit.CacheTypeF16, typeV: fit.CacheTypeF16, fa: fit.FlashAttnOn,
			wantKV: 8388608, wantCB: 322699264,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := shapeOf(parse(t, tc.build()))
			rep := fit.Estimate(fit.Request{
				Model:   m,
				Flags:   baseFlags(tc.ctx, tc.typeK, tc.typeV, tc.fa),
				Devices: devices(8 * gib),
				Host:    fit.Host{RAMFreeBytes: 32 * gib, RAMKnown: true},
			})
			if rep.KVBytes != tc.wantKV {
				t.Errorf("kv_bytes = %d, want %d", rep.KVBytes, tc.wantKV)
			}
			if rep.KVSWABytes != tc.wantSWA {
				t.Errorf("kv_swa_bytes = %d, want %d", rep.KVSWABytes, tc.wantSWA)
			}
			if rep.ComputeBytes != tc.wantCB {
				t.Errorf("compute_bytes = %d, want %d (logits %d, act %d, attn %d, moe %d)",
					rep.ComputeBytes, tc.wantCB, rep.ComputeLogitsBytes,
					rep.ComputeActBytes, rep.ComputeAttnBytes, rep.ComputeMoEBytes)
			}
		})
	}
}

// TestGoldenVerdicts pins the three-valued answer for each fixture against a
// card sized to make the boundary interesting.
func TestGoldenVerdicts(t *testing.T) {
	cases := []struct {
		name  string
		build func() *ggufbuild.Builder
		ctx   int
		free  uint64
		ram   uint64
		want  fit.Verdict
	}{
		{
			name:  "llama on an 8 GiB card fits with room to spare",
			build: llamaFixture, ctx: 4096, free: 8 * gib, ram: 32 * gib,
			want: fit.VerdictFits,
		},
		{
			// 400 MiB of backend overhead plus a 1 GiB margin is 1.4 GiB before
			// a single weight; a 1 GiB card cannot take the model at all.
			name:  "llama on a 1 GiB card will not run there",
			build: llamaFixture, ctx: 4096, free: 1 * gib, ram: 32 * gib,
			want: fit.VerdictPartial,
		},
		{
			name:  "llama on a 1 GiB card with no RAM either will not run",
			build: llamaFixture, ctx: 4096, free: 1 * gib, ram: 1 << 20,
			want: fit.VerdictWontRun,
		},
		{
			// qwen3's 768 MiB cache at 32k plus a 311 MiB logits buffer is what
			// pushes this over a 2 GiB card.
			name:  "qwen3 at 32k does not fit on 2 GiB",
			build: qwen3Fixture, ctx: 32768, free: 2 * gib, ram: 32 * gib,
			want: fit.VerdictPartial,
		},
		{
			name:  "qwen3 at 32k fits on 8 GiB",
			build: qwen3Fixture, ctx: 32768, free: 8 * gib, ram: 32 * gib,
			want: fit.VerdictFits,
		},
		{
			name:  "gemma3 with its window fits at 8k on 2 GiB",
			build: gemma3Fixture, ctx: 8192, free: 2 * gib, ram: 32 * gib,
			want: fit.VerdictFits,
		},
		{
			name:  "moe fits at 4k on 4 GiB",
			build: moeFixture, ctx: 4096, free: 4 * gib, ram: 32 * gib,
			want: fit.VerdictFits,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := shapeOf(parse(t, tc.build()))
			rep := fit.Estimate(fit.Request{
				Model:   m,
				Flags:   baseFlags(tc.ctx, fit.CacheTypeF16, fit.CacheTypeF16, fit.FlashAttnOn),
				Devices: devices(tc.free),
				Host:    fit.Host{RAMFreeBytes: tc.ram, RAMTotalBytes: tc.ram, RAMKnown: true},
			})
			if rep.Verdict != tc.want {
				t.Errorf("verdict = %q, want %q (assigned %d of %d free; notes %v)",
					rep.Verdict, tc.want, rep.RequiredVRAMBytes, tc.free, rep.Notes)
			}
		})
	}
}

// TestSlidingWindowIsNotOptional quantifies D30's claim: without the SWA term a
// Gemma-3-class model is mis-sized by an order of magnitude, and the mis-sizing
// is in the direction that promises a fit which OOMs.
func TestSlidingWindowIsNotOptional(t *testing.T) {
	withWindow := shapeOf(parse(t, gemma3Fixture()))
	withoutWindow := withWindow
	withoutWindow.SWAWindow = nil

	flags := baseFlags(32768, fit.CacheTypeF16, fit.CacheTypeF16, fit.FlashAttnOn)
	req := fit.Request{Devices: devices(8 * gib), Flags: flags,
		Host: fit.Host{RAMFreeBytes: 32 * gib, RAMKnown: true}}

	req.Model = withWindow
	sized := fit.Estimate(req)
	req.Model = withoutWindow
	naive := fit.Estimate(req)

	sizedTotal := sized.KVBytes + sized.KVSWABytes
	naiveTotal := naive.KVBytes + naive.KVSWABytes
	if naiveTotal <= sizedTotal*4 {
		t.Fatalf("the window should save most of the cache: %d with it, %d without",
			sizedTotal, naiveTotal)
	}
	if sized.Inputs.NLayerSWA != 5 {
		t.Errorf("n_layer_swa = %d, want 5", sized.Inputs.NLayerSWA)
	}
	if naive.Inputs.NLayerSWA != 0 {
		t.Errorf("without a window there are no sliding layers, got %d", naive.Inputs.NLayerSWA)
	}
}

// TestMoEDefeatsAveraging is D29's reason for per-layer bucketing: on an MoE
// model the expert tensors make a layer several times the average of the file,
// so `file_size / n_layer` is wrong exactly where the answer matters.
func TestMoEDefeatsAveraging(t *testing.T) {
	exact := shapeOf(parse(t, moeFixture()))
	if len(exact.LayerBytes) != exact.NLayer {
		t.Fatalf("the tensor index gave %d buckets for %d layers",
			len(exact.LayerBytes), exact.NLayer)
	}

	averaged := exact
	averaged.LayerBytes, averaged.OtherBytes = nil, 0 // force the fallback

	flags := baseFlags(4096, fit.CacheTypeF16, fit.CacheTypeF16, fit.FlashAttnOn)
	flags.NGL, flags.NGLCount = fit.NGLCount, 1 // one layer only, where the gap shows

	req := fit.Request{Devices: devices(8 * gib), Flags: flags,
		Host: fit.Host{RAMFreeBytes: 32 * gib, RAMKnown: true}}
	req.Model = exact
	got := fit.Estimate(req)
	req.Model = averaged
	guess := fit.Estimate(req)

	if got.WeightsOffloadedBytes == guess.WeightsOffloadedBytes {
		t.Fatal("the averaged fallback happened to agree; this fixture is not exercising D29")
	}
	if guess.Confidence != fit.ConfidenceModeled {
		t.Errorf("the fallback must report `modeled`, got %q", guess.Confidence)
	}
	// The exact figure is the one to trust, and it is the bigger one here: a
	// single MoE layer carries eight experts' worth of tensors.
	if got.WeightsOffloadedBytes <= guess.WeightsOffloadedBytes {
		t.Errorf("exact bucketing charged %d for one layer, averaging charged %d; "+
			"the average understates the expert layers",
			got.WeightsOffloadedBytes, guess.WeightsOffloadedBytes)
	}
}

// TestNGLAllVersusLayersOnRealFixtures: `-ngl all` and `-ngl L` differ by the
// output head alone, which is why W_other is a separate bucket.
func TestNGLAllVersusLayersOnRealFixtures(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func() *ggufbuild.Builder
	}{
		{"llama", llamaFixture},
		{"qwen3", qwen3Fixture},
		{"gemma3", gemma3Fixture},
		{"moe", moeFixture},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := shapeOf(parse(t, tc.build()))
			base := fit.Request{
				Model: m, Devices: devices(8 * gib),
				Host: fit.Host{RAMFreeBytes: 32 * gib, RAMKnown: true},
			}

			base.Flags = baseFlags(4096, fit.CacheTypeF16, fit.CacheTypeF16, fit.FlashAttnOn)
			all := fit.Estimate(base)

			base.Flags.NGL, base.Flags.NGLCount = fit.NGLCount, m.NLayer
			layers := fit.Estimate(base)

			diff := all.WeightsOffloadedBytes - layers.WeightsOffloadedBytes
			if diff != m.OtherBytes {
				t.Errorf("-ngl all minus -ngl L = %d, want W_other = %d", diff, m.OtherBytes)
			}
			if layers.SpillToRAMBytes != m.OtherBytes {
				t.Errorf("-ngl L spill = %d, want W_other = %d",
					layers.SpillToRAMBytes, m.OtherBytes)
			}
		})
	}
}

// recordedLoad is a hand-written load, kept beside the corpus rather than in it.
//
// The corpus in `testdata/fit/` (corpus_test.go) is section 8.7's twenty rows
// and carries the ±10% accuracy assertions. These four are the cases whose
// REASONING is worth reading in Go — each name says which term made the load
// impossible — so that a failure here points at a formula rather than at a
// number in a data file.
type recordedLoad struct {
	name  string
	build func() *ggufbuild.Builder
	ctx   int
	typeK string
	typeV string
	fa    fit.FlashAttn
	free  uint64
	oom   bool
}

func TestNeverSaysFitsForARecordedOOM(t *testing.T) {
	corpus := []recordedLoad{
		{
			name:  "qwen3 at 32k on a 2 GiB card: the cache alone is 768 MiB",
			build: qwen3Fixture, ctx: 32768,
			typeK: fit.CacheTypeF16, typeV: fit.CacheTypeF16, fa: fit.FlashAttnOn,
			free: 2 * gib, oom: true,
		},
		{
			name:  "llama at 4k on a 1 GiB card: the backend overhead does not fit",
			build: llamaFixture, ctx: 4096,
			typeK: fit.CacheTypeF16, typeV: fit.CacheTypeF16, fa: fit.FlashAttnOn,
			free: 1 * gib, oom: true,
		},
		{
			name:  "llama at 4k with a non-flash attention buffer on 1.5 GiB",
			build: llamaFixture, ctx: 4096,
			typeK: fit.CacheTypeF16, typeV: fit.CacheTypeF16, fa: fit.FlashAttnOff,
			free: gib + gib/4, oom: true,
		},
		{
			name:  "gemma3 at 8k on 4 GiB: this one loaded",
			build: gemma3Fixture, ctx: 8192,
			typeK: fit.CacheTypeF16, typeV: fit.CacheTypeF16, fa: fit.FlashAttnOn,
			free: 4 * gib, oom: false,
		},
	}

	for _, tc := range corpus {
		t.Run(tc.name, func(t *testing.T) {
			rep := fit.Estimate(fit.Request{
				Model:   shapeOf(parse(t, tc.build())),
				Flags:   baseFlags(tc.ctx, tc.typeK, tc.typeV, tc.fa),
				Devices: devices(tc.free),
				Host:    fit.Host{RAMFreeBytes: 32 * gib, RAMTotalBytes: 64 * gib, RAMKnown: true},
			})
			if tc.oom && rep.Verdict == fit.VerdictFits {
				t.Fatalf("this load OOM'd and the calculator said `fits` (assigned %d of %d free)",
					rep.RequiredVRAMBytes, tc.free)
			}
			if !tc.oom && rep.Verdict == fit.VerdictWontRun {
				t.Errorf("this load succeeded and the calculator said `wont_run`: %v", rep.Notes)
			}
		})
	}
}
