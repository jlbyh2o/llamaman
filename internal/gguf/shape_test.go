package gguf_test

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/jlbyh2o/llamaman/internal/gguf"
	"github.com/jlbyh2o/llamaman/internal/gguf/ggufbuild"
)

func ptr(n int) *int { return &n }

// TestShape is the geometry DESIGN section 8.1 lists, read off each fixture and
// compared with values written by hand beside the builder that produced them.
// Everything the fit calculator and the `models` columns of section 2.6 need is
// in one comparison, so a key read from the wrong namespace or defaulted the
// wrong way cannot hide.
func TestShape(t *testing.T) {
	tests := []struct {
		fixture string
		want    gguf.Shape
		notes   []string // substrings every Notes entry set must contain
	}{
		{
			fixture: "llama.gguf",
			want: gguf.Shape{
				Architecture:      "llama",
				Name:              "synthetic-llama",
				BlockCount:        4,
				ContextLength:     4096,
				EmbeddingLength:   512,
				FeedForwardLength: 1024,
				HeadCount:         8,
				HeadCountKV:       []int{2, 2, 2, 2},
				// key_length is absent, so it defaults to n_embd/n_head, and
				// value_length to key_length (section 8.1).
				KeyLength:      64,
				ValueLength:    64,
				VocabSize:      256,
				TokenizerModel: "llama",
				FileType:       ptr(15),
				Quantization:   "Q4_K_M",
			},
		},
		{
			fixture: "qwen3.gguf",
			want: gguf.Shape{
				Architecture:      "qwen3",
				Name:              "synthetic-qwen3",
				BlockCount:        6,
				ContextLength:     32768,
				EmbeddingLength:   1024,
				FeedForwardLength: 2560,
				HeadCount:         16,
				HeadCountKV:       []int{8, 8, 8, 8, 8, 8},
				// Stated explicitly, and NOT 1024/16 = 64: a model whose head
				// dimension is not n_embd/n_head is exactly why the key exists.
				KeyLength:   128,
				ValueLength: 128,
				// From `qwen3.vocab_size`, which wins over the token array.
				VocabSize:      151936,
				TokenizerModel: "gpt2",
				FileType:       ptr(7),
				Quantization:   "Q8_0",
			},
		},
		{
			fixture: "gemma3.gguf",
			want: gguf.Shape{
				Architecture:      "gemma3",
				Name:              "synthetic-gemma3",
				BlockCount:        6,
				ContextLength:     8192,
				EmbeddingLength:   768,
				FeedForwardLength: 1536,
				HeadCount:         8,
				// The D30 case: a per-layer array, kept per layer.
				HeadCountKV:         []int{4, 4, 4, 2, 2, 1},
				HeadCountKVPerLayer: true,
				KeyLength:           256,
				ValueLength:         256,
				VocabSize:           64,
				TokenizerModel:      "llama",
				// Present, so section 8.3 has a window to reason about; the
				// pattern is reported verbatim and interpreted in internal/fit.
				SlidingWindow:        ptr(1024),
				SlidingWindowPattern: ptr(6),
				FileType:             ptr(15),
				Quantization:         "Q4_K_M",
			},
		},
		{
			fixture: "moe.gguf",
			want: gguf.Shape{
				Architecture:      "qwen3moe",
				Name:              "synthetic-moe",
				BlockCount:        4,
				ContextLength:     8192,
				EmbeddingLength:   512,
				FeedForwardLength: 768,
				HeadCount:         8,
				HeadCountKV:       []int{2, 2, 2, 2},
				KeyLength:         64,
				ValueLength:       64,
				TokenizerModel:    "gpt2",
				ExpertCount:       8,
				ExpertUsedCount:   2,
				FileType:          ptr(14),
				Quantization:      "Q4_K_S",
			},
		},
		{
			fixture: "sharded-00001-of-00002.gguf",
			want: gguf.Shape{
				Architecture:      "llama",
				Name:              "synthetic-sharded",
				BlockCount:        2,
				ContextLength:     2048,
				EmbeddingLength:   512,
				FeedForwardLength: 1024,
				HeadCount:         8,
				HeadCountKV:       []int{8, 8},
				KeyLength:         64,
				ValueLength:       64,
				VocabSize:         64,
				TokenizerModel:    "llama",
				SplitNo:           0,
				SplitCount:        2,
				SplitTensors:      10,
				// No general.file_type, so the label falls back to the dominant
				// tensor type.
				Quantization: "Q4_0",
			},
		},
		{
			fixture: "mmproj.gguf",
			want: gguf.Shape{
				Architecture: "clip",
				Name:         "synthetic-mmproj",
				HasVision:    true,
				FileType:     ptr(1),
				Quantization: "F16",
			},
			// A projector has none of the language-model geometry, and saying so
			// is the point: section 2.6's `unknown` kind exists because a scan
			// must record a file it cannot classify rather than lose it.
			notes: []string{
				"clip.block_count is missing",
				"clip.context_length is missing",
				"clip.embedding_length is missing",
				"clip.attention.head_count is missing",
			},
		},
		{
			fixture: "bigvocab.gguf",
			want: gguf.Shape{
				Architecture:      "llama",
				BlockCount:        1,
				ContextLength:     512,
				EmbeddingLength:   256,
				FeedForwardLength: 512,
				HeadCount:         4,
				// head_count_kv is absent, so it broadcasts head_count — the
				// larger KV cache, which is llama.cpp's own default and the
				// conservative reading.
				HeadCountKV: []int{4},
				KeyLength:   64,
				ValueLength: 64,
				// Read off the LENGTH of an array that was never retained.
				VocabSize:      1500,
				TokenizerModel: "llama",
				Quantization:   "Q4_K",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.fixture, func(t *testing.T) {
			f := loadFixture(t, tc.fixture)
			got := f.Shape()

			notes := got.Notes
			got.Notes = nil
			// Sizes has its own test; comparing it here would restate every
			// tensor of every fixture.
			got.Sizes = gguf.Sizes{}

			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("Shape (-want +got):\n%s", diff)
			}
			for _, want := range tc.notes {
				if !containsNote(notes, want) {
					t.Errorf("Notes %q does not mention %q", notes, want)
				}
			}
			if len(tc.notes) == 0 && len(notes) != 0 {
				t.Errorf("Notes = %q, want none", notes)
			}
		})
	}
}

func containsNote(notes []string, want string) bool {
	for _, n := range notes {
		if strings.Contains(n, want) {
			return true
		}
	}
	return false
}

// TestShapeSlidingWindowAbsence is the distinction D30 turns on, asserted as a
// property rather than as a number: an absent `sliding_window` key is nil here,
// never zero, because internal/fit reads nil as "this model has no
// sliding-window attention at all" and would read zero as a window of width
// zero — which would size the whole KV cache to nothing.
func TestShapeSlidingWindowAbsence(t *testing.T) {
	plain := loadFixture(t, "llama.gguf").Shape()
	if plain.SlidingWindow != nil || plain.SlidingWindowPattern != nil {
		t.Errorf("a model with no SWA keys reported window %v pattern %v, want nil and nil",
			plain.SlidingWindow, plain.SlidingWindowPattern)
	}

	swa := loadFixture(t, "gemma3.gguf").Shape()
	if swa.SlidingWindow == nil || *swa.SlidingWindow != 1024 {
		t.Errorf("SlidingWindow = %v, want 1024", swa.SlidingWindow)
	}
	if swa.SlidingWindowPattern == nil || *swa.SlidingWindowPattern != 6 {
		t.Errorf("SlidingWindowPattern = %v, want 6", swa.SlidingWindowPattern)
	}
}

// TestShapeSurvivesNonsense covers the promise that Shape never fails. A models
// service that must record what a scan found cannot have a metadata reader that
// refuses.
func TestShapeSurvivesNonsense(t *testing.T) {
	t.Run("an architecture with no geometry keys", func(t *testing.T) {
		// alltypes.gguf declares the architecture "test", whose namespace holds
		// no geometry keys at all — the shape a file from an architecture this
		// build has never heard of has.
		s := loadFixture(t, "alltypes.gguf").Shape()
		if s.Architecture != "test" {
			t.Fatalf("Architecture = %q, want \"test\"", s.Architecture)
		}
		if s.BlockCount != 0 || s.HeadCountKV != nil {
			t.Errorf("geometry was invented: BlockCount %d, HeadCountKV %v", s.BlockCount, s.HeadCountKV)
		}
		if len(s.Notes) == 0 {
			t.Error("Notes is empty for a file with no geometry keys")
		}
	})

	t.Run("head_count_kv array of the wrong length", func(t *testing.T) {
		b := shapeBuilder()
		b.Set("llama.attention.head_count_kv", u32Array(1, 2))
		s := parseBytes(t, b.Header()).Shape()
		if diff := cmp.Diff([]int{1, 2}, s.HeadCountKV); diff != "" {
			t.Errorf("HeadCountKV (-want +got):\n%s", diff)
		}
		if !containsNote(s.Notes, "has 2 entries for 4 layers") {
			t.Errorf("Notes = %q, want a length disagreement", s.Notes)
		}
	})

	t.Run("head_count as a per-layer array", func(t *testing.T) {
		b := shapeBuilder()
		b.Set("llama.attention.head_count", u32Array(8, 8, 8, 8))
		s := parseBytes(t, b.Header()).Shape()
		if s.HeadCount != 8 {
			t.Errorf("HeadCount = %d, want the first entry 8", s.HeadCount)
		}
		if !containsNote(s.Notes, "per-layer array") {
			t.Errorf("Notes = %q, want a note about the array", s.Notes)
		}
	})

	t.Run("head_count zero does not divide", func(t *testing.T) {
		b := shapeBuilder()
		b.Set("llama.attention.head_count", u32Val(0))
		s := parseBytes(t, b.Header()).Shape()
		if s.KeyLength != 0 || s.ValueLength != 0 {
			t.Errorf("KeyLength/ValueLength = %d/%d, want 0/0 rather than a division by zero", s.KeyLength, s.ValueLength)
		}
	})
}

// shapeBuilder is a llama-shaped header carrying every geometry key EXCEPT the
// attention head counts, so each subtest above can supply that one key in the
// form it is testing without writing a duplicate.
func shapeBuilder() *ggufbuild.Builder {
	b := ggufbuild.New("llama").
		Set("llama.block_count", ggufbuild.U32(4)).
		Set("llama.context_length", ggufbuild.U32(2048)).
		Set("llama.embedding_length", ggufbuild.U32(512)).
		Set("llama.feed_forward_length", ggufbuild.U32(1024))
	b.Tensor("token_embd.weight", gguf.TypeF32, 512, 8)
	return b
}

func u32Val(v uint32) ggufbuild.Val      { return ggufbuild.U32(v) }
func u32Array(v ...uint32) ggufbuild.Val { return ggufbuild.U32s(v...) }
