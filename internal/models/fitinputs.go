package models

import (
	"encoding/json"
	"strings"

	"github.com/jlbyh2o/llamaman/internal/fit"
	"github.com/jlbyh2o/llamaman/internal/gguf"
	"github.com/jlbyh2o/llamaman/internal/model"
)

// The calculator's inputs, resolved from the rows this daemon stores (DESIGN
// sections 8.1 and 8.2).
//
// `internal/fit` is a pure function of plain structs and imports nothing outside
// the standard library (D49 invariant 5), so SOMETHING has to turn a `models`
// row into a `fit.ModelShape` and a `model.FlagSet` into `fit.Flags`. That
// conversion lives HERE, beside the service that owns those rows and the GGUF
// parse that filled them, rather than in either consumer — because there are
// two consumers and they must not drift:
//
//   - `POST /api/v1/fit/estimate` (section 3.9), the estimate a user reads
//     before creating an instance;
//   - the supervisor's fit PREDICTOR (section 8.7, D32), which records what was
//     predicted beside what llama.cpp actually reported on the first `ready`.
//
// D32 learns the ratio between those two numbers. If the endpoint and the
// predictor built their shapes differently, the calibration would be learning
// the difference between two converters rather than the error in the model, and
// the whole mechanism would be silently worthless.

// FitFlags projects a FlagSet onto the calculator's inputs.
//
// It is a projection rather than a shared type because D49 keeps internal/fit on
// the standard library. Every default llama-server would apply is left to
// fit.Flags.Normalize, so there is exactly one place they can be wrong.
func FitFlags(f model.FlagSet) fit.Flags {
	out := fit.Flags{
		NCtx:        intOrZero(f.CtxSize),
		NParallel:   intOrZero(f.Parallel),
		NBatch:      intOrZero(f.BatchSize),
		NUbatch:     intOrZero(f.UbatchSize),
		TypeK:       strOrEmpty(f.CacheTypeK),
		TypeV:       strOrEmpty(f.CacheTypeV),
		TensorSplit: f.TensorSplit,
		MainGPU:     intOrZero(f.MainGPU),
		Embedding:   f.Embedding != nil && *f.Embedding,
	}
	if f.NGpuLayers != nil {
		out.NGL = fit.NGLMode(f.NGpuLayers.Mode)
		out.NGLCount = intOrZero(f.NGpuLayers.Count)
	}
	if f.FlashAttn != nil {
		out.FlashAttn = fit.FlashAttn(*f.FlashAttn)
	}
	if f.SplitMode != nil {
		out.SplitMode = fit.SplitMode(*f.SplitMode)
	}
	if f.Draft != nil {
		out.Draft = fit.DraftFlags{
			CtxSize:    intOrZero(f.Draft.CtxSize),
			NGpuLayers: f.Draft.NGpuLayers,
		}
	}
	return out
}

// FitShape builds the calculator's inputs from a `models` row.
//
// This is the exact-after-download path of section 8.2: the tensor summary
// carries the per-layer bucketing, so no averaging is involved.
//
// ok is false when the row's GGUF header has not been parsed yet. It is a bool
// rather than an error because the two callers answer it differently and both
// answers are correct: the endpoint turns it into a 409 `fit_unavailable`, and
// the supervisor's predictor simply writes no observation — there is nothing to
// calibrate against, and guessing a shape would poison the median D32 learns.
// section 8.2 forbids the obvious guess in as many words: `file_size / n_layer`
// averaging is wrong exactly where the answer matters.
func FitShape(v View) (fit.ModelShape, bool) {
	m := v.LocalModel
	if m.GGUFParsedAt == nil || m.Arch == nil || m.NLayer == nil {
		return fit.ModelShape{}, false
	}

	shape := fit.ModelShape{
		Arch:        *m.Arch,
		NLayer:      int(*m.NLayer),
		NEmbd:       intFrom(m.NEmbd),
		NFF:         intFrom(m.NFF),
		NHead:       intFrom(m.NHead),
		HeadDimK:    intFrom(m.HeadDimK),
		HeadDimV:    intFrom(m.HeadDimV),
		NCtxTrain:   intFrom(m.NCtxTrain),
		NVocab:      intFrom(m.NVocab),
		NExpert:     intFrom(m.NExpert),
		NExpertUsed: intFrom(m.NExpertUsed),
		SWAWindow:   intPtrFrom(m.SWAWindow),
		SWAPattern:  intPtrFrom(m.SWAPattern),
		FileBytes:   uint64(m.TotalBytes),
	}
	shape.NHeadKV = headCountKV(m.NHeadKVJSON, shape.NHead, shape.NLayer)

	if m.TensorSummaryJSON != nil {
		var sizes gguf.Sizes
		if err := json.Unmarshal([]byte(*m.TensorSummaryJSON), &sizes); err == nil {
			shape.LayerBytes = sizes.Layer
			shape.OtherBytes = sizes.Other
		}
	}
	if v.Mmproj != nil {
		shape.MmprojBytes = uint64(v.Mmproj.TotalBytes)
	}
	return shape, true
}

// FitShapeFromGGUF builds the calculator's inputs from a parsed header — the
// pre-download peek.
func FitShapeFromGGUF(f *gguf.File) fit.ModelShape {
	sh := f.Shape()
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
		LayerBytes:  sh.Sizes.Layer,
		OtherBytes:  sh.Sizes.Other,
		FileBytes:   sh.Sizes.Total,
	}
}

// headCountKV reads `models.n_head_kv_json`, which stores the metadata VERBATIM
// (D30): a scalar or a per-layer array. A scalar is broadcast; anything
// unreadable falls back to n_head, llama.cpp's own default and the larger cache.
func headCountKV(raw *string, nHead, nLayer int) []int {
	broadcast := func(n int) []int {
		if nLayer <= 0 {
			return nil
		}
		out := make([]int, nLayer)
		for i := range out {
			out[i] = n
		}
		return out
	}
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return broadcast(nHead)
	}
	var arr []int
	if err := json.Unmarshal([]byte(*raw), &arr); err == nil && len(arr) > 0 {
		return arr
	}
	var scalar int
	if err := json.Unmarshal([]byte(*raw), &scalar); err == nil && scalar > 0 {
		return broadcast(scalar)
	}
	return broadcast(nHead)
}

func intOrZero(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func strOrEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func intFrom(p *int64) int {
	if p == nil {
		return 0
	}
	return int(*p)
}

func intPtrFrom(p *int64) *int {
	if p == nil {
		return nil
	}
	v := int(*p)
	return &v
}
