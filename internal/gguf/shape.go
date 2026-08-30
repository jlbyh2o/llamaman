package gguf

import (
	"fmt"
	"strings"
)

// Metadata key names. Every key DESIGN sections 8.1 and 2.6 read is spelled once
// here, and the per-architecture ones are formatted against
// `general.architecture` — GGUF namespaces them by the architecture the file
// declares, so the same shape is `llama.block_count` in one file and
// `qwen3.block_count` in the next.
const (
	KeyArchitecture        = "general.architecture"
	KeyName                = "general.name"
	KeyFileType            = "general.file_type"
	KeyQuantizationVersion = "general.quantization_version"
	KeyAlignment           = "general.alignment"

	KeyTokenizerModel  = "tokenizer.ggml.model"
	KeyTokenizerTokens = "tokenizer.ggml.tokens"

	// The split keys a sharded set carries, which DESIGN section 7.2a's scan
	// groups shards by when the filename suffix is not enough.
	KeySplitNo      = "split.no"
	KeySplitCount   = "split.count"
	KeySplitTensors = "split.tensors.count"

	// The multimodal projector's own marker (section 7.2a's `mmproj`
	// classification).
	KeyClipHasVisionEncoder = "clip.has_vision_encoder"
	KeyClipHasAudioEncoder  = "clip.has_audio_encoder"
)

// archKey formats a per-architecture key: archKey("llama", "block_count") is
// "llama.block_count".
func archKey(arch, suffix string) string { return arch + "." + suffix }

// The per-architecture key suffixes.
const (
	subBlockCount           = "block_count"
	subContextLength        = "context_length"
	subEmbeddingLength      = "embedding_length"
	subFeedForwardLength    = "feed_forward_length"
	subVocabSize            = "vocab_size"
	subExpertCount          = "expert_count"
	subExpertUsedCount      = "expert_used_count"
	subPoolingType          = "pooling_type"
	subHeadCount            = "attention.head_count"
	subHeadCountKV          = "attention.head_count_kv"
	subKeyLength            = "attention.key_length"
	subValueLength          = "attention.value_length"
	subSlidingWindow        = "attention.sliding_window"
	subSlidingWindowPattern = "attention.sliding_window_pattern"
)

// Shape is the model geometry read out of the metadata table: the inputs DESIGN
// section 8.1 lists for the fit calculator, plus the columns section 2.6's
// `models` table stores.
//
// Absence is carried honestly. A pointer field is nil when the key was not in
// the file, because for the sliding-window pair that difference IS the
// semantics (section 8.3: a model with no `sliding_window` key has no
// sliding-window attention at all, whatever any pattern key says) and a zero
// would be read as a window of width zero. The plain integer fields are zero
// when absent, and Notes says which ones those were.
//
// Nothing here interprets: SWA layer counting, KV sizing and every default that
// depends on a flag live in internal/fit, which owns section 8.3's derivation.
// This type reports what the file says.
type Shape struct {
	// Architecture is `general.architecture` — "llama", "qwen3", "gemma3",
	// "clip" — and the namespace every key below was read from.
	Architecture string
	// Name is `general.name`, the human label the producer chose.
	Name string

	// BlockCount is `{arch}.block_count`: L, the transformer layer count
	// (`models.n_layer`).
	BlockCount int
	// ContextLength is `{arch}.context_length` (`models.n_ctx_train`).
	ContextLength int
	// EmbeddingLength is `{arch}.embedding_length` (n_embd).
	EmbeddingLength int
	// FeedForwardLength is `{arch}.feed_forward_length` (n_ff). When the file
	// gives it per layer, this is the first entry and a note says so.
	FeedForwardLength int
	// HeadCount is `{arch}.attention.head_count` (n_head), likewise.
	HeadCount int

	// HeadCountKV is `{arch}.attention.head_count_kv`, resolved to one entry per
	// layer: section 8.1 says the key is a scalar OR a per-layer array, and
	// section 8.3 indexes it per layer either way, so a scalar is broadcast to
	// BlockCount entries here and the caller never branches. When the key is
	// absent it broadcasts HeadCount, which is llama.cpp's own default.
	HeadCountKV []int
	// HeadCountKVPerLayer records that the file gave an array rather than a
	// scalar — what `models.n_head_kv_json` stores verbatim (D30).
	HeadCountKVPerLayer bool

	// KeyLength is `{arch}.attention.key_length`, defaulting to
	// EmbeddingLength/HeadCount (head_dim_k).
	KeyLength int
	// ValueLength is `{arch}.attention.value_length`, defaulting to KeyLength
	// (head_dim_v).
	ValueLength int

	// VocabSize is `{arch}.vocab_size` when present, and otherwise the LENGTH of
	// `tokenizer.ggml.tokens` — which survives that array being elided, so
	// n_vocab costs nothing to learn.
	VocabSize int
	// TokenizerModel is `tokenizer.ggml.model`, the field D34's draft-model
	// vocabulary check compares alongside VocabSize.
	TokenizerModel string

	// ExpertCount and ExpertUsedCount are the MoE pair; both are 0 on a dense
	// model, which is what section 8.4's `CB_moe` term keys off.
	ExpertCount     int
	ExpertUsedCount int

	// SlidingWindow is `{arch}.attention.sliding_window` — the window WIDTH in
	// tokens. nil means the key is absent, which section 8.3 reads as "no SWA at
	// all"; it is deliberately not 0.
	SlidingWindow *int
	// SlidingWindowPattern is `{arch}.attention.sliding_window_pattern` — the
	// PERIOD. nil means absent. Section 8.3 spells out what a window with no
	// pattern means and this package does not decide it.
	SlidingWindowPattern *int

	// PoolingType is `{arch}.pooling_type`, present on embedding models and one
	// of the two things section 7.2a's scan classifies `kind='embedding'` by.
	PoolingType *int

	// HasVision and HasAudio are the `clip.*` encoder markers that make a file
	// an mmproj projector rather than a model.
	HasVision bool
	HasAudio  bool

	// SplitNo, SplitCount and SplitTensors are the sharding keys: this file's
	// index within the set, the set's size, and the total tensor count across
	// it. SplitCount is 0 on an unsharded file.
	SplitNo      int
	SplitCount   int
	SplitTensors int

	// FileType is `general.file_type`, the llama_ftype number, or nil when the
	// key is absent.
	FileType *int
	// Quantization is the label for `models.file_type`: FileType's name when it
	// has one, and otherwise the dominant tensor type (see Sizes.DominantType),
	// so a file whose producer omitted the key still gets an honest answer.
	Quantization string

	// Sizes is the tensor index bucketed per section 8.2.
	Sizes Sizes

	// Notes records what could not be read and what had to be assumed — a
	// missing geometry key, a per-layer array whose length disagrees with
	// BlockCount. It is diagnostic text for the UI and the logs, not a control
	// signal.
	Notes []string
}

// Shape reads the model geometry out of the metadata table and the tensor index.
//
// It never fails. A file missing half its keys — an mmproj projector, a model
// from an architecture nothing here has heard of — yields a Shape with zeros and
// a Notes entry per missing key, because the models service records what it
// found either way and refusing would lose the file (section 2.6's `unknown`
// kind exists for exactly this).
func (f *File) Shape() Shape {
	s := Shape{Sizes: f.Sizes()}
	kv := f.KV

	s.Architecture, _ = kv.String(KeyArchitecture)
	if s.Architecture == "" {
		s.Notes = append(s.Notes, "general.architecture is missing")
	}
	s.Name, _ = kv.String(KeyName)
	arch := s.Architecture

	req := func(suffix string) (int, bool) {
		if arch == "" {
			return 0, false
		}
		key := archKey(arch, suffix)
		v, ok := kv.Get(key)
		if !ok {
			s.Notes = append(s.Notes, key+" is missing")
			return 0, false
		}
		// A handful of shape keys are written per layer by some producers; the
		// scalar columns take the first entry and say so.
		if v.Type == ValueArray {
			ints, ok := v.AsInts()
			if !ok || len(ints) == 0 {
				s.Notes = append(s.Notes, key+" is an array this reader could not use")
				return 0, false
			}
			s.Notes = append(s.Notes, fmt.Sprintf("%s is a per-layer array of %d; using the first entry", key, len(ints)))
			return int(ints[0]), true
		}
		n, ok := v.AsInt()
		if !ok {
			s.Notes = append(s.Notes, key+" is not a number")
			return 0, false
		}
		return int(n), true
	}
	opt := func(suffix string) *int {
		if arch == "" {
			return nil
		}
		n, ok := kv.Int(archKey(arch, suffix))
		if !ok {
			return nil
		}
		v := int(n)
		return &v
	}

	s.BlockCount, _ = req(subBlockCount)
	s.ContextLength, _ = req(subContextLength)
	s.EmbeddingLength, _ = req(subEmbeddingLength)
	s.HeadCount, _ = req(subHeadCount)
	if p := opt(subFeedForwardLength); p != nil {
		s.FeedForwardLength = *p
	} else if arch != "" {
		if v, ok := kv.Get(archKey(arch, subFeedForwardLength)); ok {
			if ints, ok := v.AsInts(); ok && len(ints) > 0 {
				s.FeedForwardLength = int(ints[0])
				s.Notes = append(s.Notes, fmt.Sprintf("%s is a per-layer array of %d; using the first entry",
					archKey(arch, subFeedForwardLength), len(ints)))
			}
		}
	}

	if p := opt(subExpertCount); p != nil {
		s.ExpertCount = *p
	}
	if p := opt(subExpertUsedCount); p != nil {
		s.ExpertUsedCount = *p
	}

	s.HeadCountKV, s.HeadCountKVPerLayer = f.headCountKV(arch, s.HeadCount, s.BlockCount, &s.Notes)

	// head_dim_k defaults to n_embd/n_head and head_dim_v to head_dim_k
	// (section 8.1). The defaults are applied here rather than left to the
	// calculator so there is one place they can be wrong.
	if p := opt(subKeyLength); p != nil {
		s.KeyLength = *p
	} else if s.HeadCount > 0 {
		s.KeyLength = s.EmbeddingLength / s.HeadCount
	}
	if p := opt(subValueLength); p != nil {
		s.ValueLength = *p
	} else {
		s.ValueLength = s.KeyLength
	}

	s.SlidingWindow = opt(subSlidingWindow)
	s.SlidingWindowPattern = opt(subSlidingWindowPattern)
	s.PoolingType = opt(subPoolingType)

	if p := opt(subVocabSize); p != nil {
		s.VocabSize = *p
	} else if v, ok := kv.Get(KeyTokenizerTokens); ok {
		// The token list is elided by default, but its declared length is kept,
		// and that length is n_vocab.
		s.VocabSize = int(v.Count())
	}
	s.TokenizerModel, _ = kv.String(KeyTokenizerModel)

	s.HasVision, _ = kv.Bool(KeyClipHasVisionEncoder)
	s.HasAudio, _ = kv.Bool(KeyClipHasAudioEncoder)

	if n, ok := kv.Int(KeySplitNo); ok {
		s.SplitNo = int(n)
	}
	if n, ok := kv.Int(KeySplitCount); ok {
		s.SplitCount = int(n)
	}
	if n, ok := kv.Int(KeySplitTensors); ok {
		s.SplitTensors = int(n)
	}

	if n, ok := kv.Uint(KeyFileType); ok {
		ft := int(n)
		s.FileType = &ft
		if name, ok := FileTypeName(n); ok {
			s.Quantization = name
		}
	}
	if s.Quantization == "" {
		if t, ok := s.Sizes.DominantType(); ok {
			s.Quantization = strings.ToUpper(t.String())
		}
	}

	return s
}

// headCountKV resolves `{arch}.attention.head_count_kv` to one entry per layer.
func (f *File) headCountKV(arch string, headCount, blockCount int, notes *[]string) ([]int, bool) {
	broadcast := func(n int) []int {
		if blockCount <= 0 {
			return nil
		}
		out := make([]int, blockCount)
		for i := range out {
			out[i] = n
		}
		return out
	}
	if arch == "" {
		return broadcast(headCount), false
	}
	key := archKey(arch, subHeadCountKV)
	v, ok := f.KV.Get(key)
	if !ok {
		// llama.cpp defaults n_head_kv to n_head, which makes the model
		// multi-head rather than grouped-query — the conservative reading, since
		// it is the larger KV cache.
		return broadcast(headCount), false
	}
	if v.Type != ValueArray {
		n, ok := v.AsInt()
		if !ok {
			*notes = append(*notes, key+" is not a number")
			return broadcast(headCount), false
		}
		return broadcast(int(n)), false
	}
	ints, ok := v.AsInts()
	if !ok {
		*notes = append(*notes, key+" is an array this reader could not use")
		return broadcast(headCount), false
	}
	out := make([]int, len(ints))
	for i, n := range ints {
		out[i] = int(n)
	}
	if blockCount > 0 && len(out) != blockCount {
		*notes = append(*notes, fmt.Sprintf("%s has %d entries for %d layers", key, len(out), blockCount))
	}
	return out, true
}
