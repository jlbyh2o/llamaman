package cache

import (
	"strings"

	"github.com/jlbyh2o/llamaman/internal/gguf"
	"github.com/jlbyh2o/llamaman/internal/model"
)

// Classification (DESIGN section 7.2, "Scan").
//
//	`mmproj*` filename or `clip.has_vision_encoder` → mmproj
//	an embedding architecture or a `pooling_type`   → embedding
//	otherwise                                       → text
//
// "draft" is deliberately NOT here. A draft model is a per-INSTANCE role — the
// same 0.5B text model is a draft for one instance and the served model of
// another — so classifying one at scan time would record a decision the scan is
// not entitled to make, and would then have to be un-made every time a user
// pointed an instance at it directly.

// embeddingArchitectures are the `general.architecture` values that mean "this
// produces embeddings, not tokens". It is a list rather than a rule because
// there is no rule: the architecture string is whatever the converter wrote, and
// llama.cpp's own embedding path is likewise keyed off a set of names.
//
// It is the WEAKER of the two signals. `{arch}.pooling_type` is a fact about the
// file and catches every architecture this list has not heard of, which is why
// the classifier checks it first and why an unknown embedding model still lands
// as `text` rather than as `unknown` — a wrong-but-usable answer the user can
// correct beats a state that hides the file.
var embeddingArchitectures = map[string]struct{}{
	"bert":            {},
	"nomic-bert":      {},
	"nomic-bert-moe":  {},
	"jina-bert-v2":    {},
	"jina-bert-v3":    {},
	"gte":             {},
	"xlm-roberta":     {},
	"roberta":         {},
	"t5encoder":       {},
	"gemma-embedding": {},
	"qwen3-embedding": {},
}

// Classify decides a file's `models.kind` from its name and its parsed header.
//
// shape is nil when the file did not parse. That is not `text` and not
// `embedding`: section 2.6 keeps `unknown` in the enum precisely so a scan can
// record a GGUF it cannot read rather than lose it, and the file also becomes a
// `stray_files` row with reason `unparsable` so the user is told which one.
func Classify(filename string, shape *gguf.Shape) model.ModelKind {
	// The projector test comes first and reads the header before the name: a
	// file called `model.gguf` that carries `clip.has_vision_encoder` IS a
	// projector, and treating it as weights would produce an instance that
	// loads a vision tower as a language model.
	if shape != nil && (shape.HasVision || shape.HasAudio) {
		return model.ModelMmproj
	}
	if LooksLikeMmproj(filename) {
		return model.ModelMmproj
	}
	if shape == nil {
		return model.ModelUnknown
	}
	if shape.PoolingType != nil {
		return model.ModelEmbedding
	}
	if _, ok := embeddingArchitectures[strings.ToLower(shape.Architecture)]; ok {
		return model.ModelEmbedding
	}
	return model.ModelText
}

// QuantLabel is `models.quant_label`: the short name a user recognizes a file
// by — `Q4_K_M`, `IQ3_XXS`, `F16`.
//
// The header's own answer is preferred: internal/gguf resolves
// `general.file_type` to its llama_ftype name and falls back to the dominant
// tensor type for a file whose producer omitted the key, which is already the
// honest answer for both cases. The FILE NAME is consulted only when the header
// gave nothing at all, because the convention
// `<Model>-<Quant>.gguf` is a convention and the metadata is a fact.
func QuantLabel(filename string, shape *gguf.Shape) string {
	if shape != nil && shape.Quantization != "" {
		return shape.Quantization
	}
	return quantFromName(filename)
}

// quantFromName reads the trailing `-Q4_K_M` of a conventional file name. It
// returns "" rather than guessing when the tail does not look like a quant
// label, because a wrong label is worse than none: it is what the model list
// groups by.
func quantFromName(filename string) string {
	base := strings.TrimSuffix(filename, GGUFExtension)
	if sh, ok := ParseShardName(filename); ok {
		base = sh.Base
	}
	idx := strings.LastIndexByte(base, '-')
	if idx < 0 || idx == len(base)-1 {
		return ""
	}
	tail := base[idx+1:]
	upper := strings.ToUpper(tail)
	switch {
	case strings.HasPrefix(upper, "Q") && len(upper) > 1 && upper[1] >= '0' && upper[1] <= '9':
		return upper
	case strings.HasPrefix(upper, "IQ") && len(upper) > 2:
		return upper
	case upper == "F16" || upper == "F32" || upper == "BF16":
		return upper
	}
	return ""
}
