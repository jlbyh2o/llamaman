package fit

// The calculator's inputs (DESIGN section 8.1), as plain structs.
//
// Nothing here imports internal/model or internal/gguf, and that is D49
// invariant 5 in practice: this package is arithmetic, and arithmetic that can
// be handed a struct literal in a test is arithmetic whose expected values can
// be written down beside it. The caller — the fit route, the instance detail
// endpoint, the bench planner — reads a `models` row or a GGUF header and fills
// these in.

// ModelShape is the model geometry section 8.1 lists, plus the per-tensor byte
// buckets section 8.2 requires.
//
// Absence is carried honestly, because for the sliding-window pair the
// difference IS the semantics (section 8.3): a nil SWAWindow means the model has
// no sliding-window attention at all, and a zero would mean a window of width
// zero.
type ModelShape struct {
	// Arch is `general.architecture`. It selects the per-architecture k_act
	// entry (section 8.4) and is half of the calibration key (D32).
	Arch string

	// NLayer is L, the transformer block count. The offload axis runs over
	// [0, L+1]: L layers plus the non-layer weights (section 8.2's W_other).
	NLayer int
	// NEmbd, NFF, NHead are n_embd, n_ff and n_head.
	NEmbd int
	NFF   int
	NHead int
	// NHeadKV is `attention.head_count_kv` resolved to ONE ENTRY PER LAYER
	// (D30). A scalar is broadcast by the caller; a shorter slice is read by
	// clamping to its last entry, and a nil slice falls back to NHead, which is
	// llama.cpp's own default and the larger cache.
	NHeadKV []int
	// HeadDimK and HeadDimV are `attention.key_length` and
	// `attention.value_length`, already defaulted by the caller
	// (n_embd/n_head and head_dim_k respectively).
	HeadDimK int
	HeadDimV int

	// NCtxTrain is `context_length`, the trained maximum. It is advisory here:
	// llama.cpp clamps to it, and the recommendation never proposes more.
	NCtxTrain int
	// NVocab is the output head's width, which dominates CB_logits on a
	// large-vocabulary model.
	NVocab int

	// NExpert and NExpertUsed are the MoE pair. Both are 0 on a dense model,
	// which is what zeroes section 8.4's CB_moe term.
	NExpert     int
	NExpertUsed int

	// SWAWindow is `{arch}.attention.sliding_window` — the window WIDTH in
	// tokens. nil means the key was ABSENT, which section 8.3 reads as "no SWA
	// at all", whatever any pattern says.
	SWAWindow *int
	// SWAPattern is `{arch}.attention.sliding_window_pattern` — the PERIOD,
	// verbatim. nil means absent, and section 8.3 spells out that a window with
	// no pattern is read as period 1, i.e. every layer full-attention.
	SWAPattern *int

	// LayerBytes is W_layer[i]: the summed size of every tensor matching
	// `^blk\.i\.` (D29). Its length is normally NLayer.
	//
	// An EMPTY slice selects section 8.2's pre-download fallback, which needs
	// FileBytes and stamps the report `modeled`. `file_size / n_layer` averaging
	// is forbidden as the primary path precisely because MoE models and large
	// output heads are mis-sized by it — which is why the fallback is a named,
	// reported degradation rather than the default.
	LayerBytes []uint64
	// OtherBytes is W_other — token_embd, output_norm and the output head. It is
	// separate because `-ngl all` and `-ngl L` differ by it alone, and on a large
	// vocabulary it can exceed a gigabyte.
	OtherBytes uint64
	// FileBytes is the model's on-disk size, used only by the fallback above.
	FileBytes uint64

	// MmprojBytes is the paired multimodal projector's size, 0 when there is
	// none. llama.cpp puts the projector on a GPU whenever anything is
	// offloaded, so it is charged to the main device in that case and to RAM
	// otherwise.
	MmprojBytes uint64

	// Draft is the speculative-decoding draft model's own shape, or nil. Its
	// weights and its KV cache are real VRAM on the same devices, and an
	// estimate that ignored them would promise a fit that OOMs the moment
	// `draft_model_id` is set.
	Draft *ModelShape
}

// Layers returns the effective per-layer weight buckets and W_other, together
// with whether they are exact.
//
// The inexact branch is section 8.2's pre-download fallback: with only HF's
// `gguf` summary in hand there is no tensor index, so every one of the L+1
// buckets is FileBytes/(L+1). That is exactly `W_gpu ≈ file_bytes × n/(L+1)`
// expressed per bucket, which lets the per-GPU placement below run unchanged
// instead of growing a second code path that could disagree with the first.
func (m ModelShape) Layers() (layer []uint64, other uint64, exact bool) {
	if len(m.LayerBytes) > 0 {
		out := make([]uint64, m.NLayer)
		for i := range out {
			out[i] = m.layerBytesAt(i)
		}
		return out, m.OtherBytes, true
	}
	buckets := m.NLayer + 1
	if buckets <= 0 {
		return nil, m.FileBytes, false
	}
	per := m.FileBytes / uint64(buckets)
	out := make([]uint64, m.NLayer)
	for i := range out {
		out[i] = per
	}
	// The remainder rides with W_other so the buckets still sum to FileBytes; a
	// report whose parts do not add up to the file it describes is a report
	// nobody can check.
	return out, m.FileBytes - per*uint64(m.NLayer), false
}

func (m ModelShape) layerBytesAt(i int) uint64 {
	if len(m.LayerBytes) == 0 {
		return 0
	}
	if i < 0 {
		i = 0
	}
	if i >= len(m.LayerBytes) {
		i = len(m.LayerBytes) - 1
	}
	return m.LayerBytes[i]
}

// TotalBytes is W_total: every layer plus W_other, from whichever bucketing
// Layers chose.
func (m ModelShape) TotalBytes() uint64 {
	layer, other, _ := m.Layers()
	total := other
	for _, b := range layer {
		total += b
	}
	return total
}

// HeadCountKV is n_head_kv for one layer, with the two documented fallbacks: a
// slice shorter than L clamps to its last entry, and an absent slice falls back
// to NHead — llama.cpp's own default, and the larger cache of the two readings.
func (m ModelShape) HeadCountKV(i int) int {
	if len(m.NHeadKV) == 0 {
		return m.NHead
	}
	if i < 0 {
		i = 0
	}
	if i >= len(m.NHeadKV) {
		i = len(m.NHeadKV) - 1
	}
	return m.NHeadKV[i]
}

// OffloadSteps is the number of positions on the offload axis: L+1, because
// section 8.2 counts the non-layer weights as one more step past the last layer.
func (m ModelShape) OffloadSteps() int { return m.NLayer + 1 }
