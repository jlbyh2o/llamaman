package gguf

import (
	"sort"
	"strconv"
	"strings"
)

// TensorInfo is one entry of the tensor index: the name a layer is found by,
// the shape, the element type and the offset within the tensor-data region.
//
// This is the input to DESIGN section 8.2's weight bucketing, and the reason
// that section forbids `file_size / n_layer` averaging: the answer is here, per
// tensor, exactly, and averaging is wrong precisely for the models — MoE, large
// output heads — where the answer matters.
type TensorInfo struct {
	Name string
	// Dims is the shape in ggml order, fastest-varying first, so Dims[0] is the
	// row length and the dimension quantization blocks run along.
	Dims   []uint64
	Type   GGMLType
	Offset uint64 // relative to File.DataOffset
}

// Elements is the product of the dimensions. A tensor with no dimensions has
// one element, which is ggml's own convention for a scalar.
func (t TensorInfo) Elements() uint64 {
	n := uint64(1)
	for _, d := range t.Dims {
		n *= d
	}
	return n
}

// Bytes is the tensor's size on disk: DESIGN section 8.2's
// `(numel(t) / block_size(type)) × type_size(type)`, in integers.
//
// The division is exact because Parse rejects a tensor whose first dimension is
// not a multiple of its block size, and the remaining dimensions only multiply
// whole rows. A type this build does not know returns 0, which Parse also makes
// unreachable by refusing such a file outright.
func (t TensorInfo) Bytes() uint64 {
	tr, ok := t.Type.traits()
	if !ok {
		return 0
	}
	return t.Elements() / tr.blockSize * tr.typeSize
}

// LayerIndex reports the transformer block a tensor belongs to, by the `blk.N.`
// prefix llama.cpp's own naming convention gives every per-layer tensor. It is
// the `^blk\.i\.` match of DESIGN section 8.2, done without a regexp because it
// runs once per tensor per parse.
//
// Everything else — `token_embd`, `output_norm`, `output` — is not in any layer,
// which is what makes `W_other` a separate term and what makes `-ngl all`
// differ from `-ngl L` by more than a rounding error.
func (t TensorInfo) LayerIndex() (int, bool) {
	rest, ok := strings.CutPrefix(t.Name, "blk.")
	if !ok {
		return 0, false
	}
	digits, _, ok := strings.Cut(rest, ".")
	if !ok || digits == "" || len(digits) > 9 {
		return 0, false
	}
	n, err := strconv.Atoi(digits)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// TypeUsage is one row of a file's quantization mix: how many tensors, how many
// weights and how many bytes are held in one ggml type.
type TypeUsage struct {
	Type     GGMLType `json:"type"`
	Name     string   `json:"name"`
	Tensors  int      `json:"tensors"`
	Elements uint64   `json:"elements"`
	Bytes    uint64   `json:"bytes"`
}

// Sizes is DESIGN section 8.2's bucketing of the tensor index: the total, the
// per-layer split, the remainder, and the type mix underneath both.
//
// It is JSON-tagged because it is what `models.tensor_summary_json` stores and
// what the section 3.6 peek response returns as `tensor_summary`.
type Sizes struct {
	// TensorCount is len(File.Tensors).
	TensorCount int `json:"tensor_count"`
	// Total is `W_total`: every tensor's bytes, summed.
	Total uint64 `json:"total_bytes"`
	// Layer is `W_layer[i]`, indexed by block. Its length is one past the
	// highest `blk.N.` seen, so a file whose layers are numbered without gaps
	// gives a slice of exactly n_layer entries.
	Layer []uint64 `json:"layer_bytes"`
	// Other is `W_other` — Total minus the sum of Layer. It is the token
	// embedding, the output norm and the output head, and section 8.2 keeps it
	// separate because on a large vocabulary it can exceed a gigabyte on its own.
	Other uint64 `json:"other_bytes"`
	// ByType is the quantization mix, ordered by ggml type number.
	ByType []TypeUsage `json:"by_type"`
}

// Sizes buckets the tensor index. It is a pure function of File.Tensors and is
// recomputed rather than cached, because it costs one pass over a few hundred
// entries.
func (f *File) Sizes() Sizes {
	s := Sizes{TensorCount: len(f.Tensors)}
	byType := make(map[GGMLType]TypeUsage)
	maxLayer := -1
	for _, t := range f.Tensors {
		if i, ok := t.LayerIndex(); ok && i > maxLayer {
			maxLayer = i
		}
	}
	if maxLayer >= 0 {
		s.Layer = make([]uint64, maxLayer+1)
	}
	var layerTotal uint64
	for _, t := range f.Tensors {
		b := t.Bytes()
		s.Total += b
		if i, ok := t.LayerIndex(); ok {
			s.Layer[i] += b
			layerTotal += b
		}
		u := byType[t.Type]
		u.Type = t.Type
		u.Name = t.Type.String()
		u.Tensors++
		u.Elements += t.Elements()
		u.Bytes += b
		byType[t.Type] = u
	}
	s.Other = s.Total - layerTotal

	s.ByType = make([]TypeUsage, 0, len(byType))
	for _, u := range byType {
		s.ByType = append(s.ByType, u)
	}
	sort.Slice(s.ByType, func(i, j int) bool { return s.ByType[i].Type < s.ByType[j].Type })
	return s
}

// DominantType is the quantized type holding the most bytes — the type a file's
// quant name is built around. Norms and biases stay f32 in every quant of the
// same model, so they are excluded; a file with no quantized tensors at all (an
// f16 or bf16 conversion) falls back to the plain type holding the most bytes,
// which is the right answer for exactly that case.
//
// It reports false only for a file with no tensors.
func (s Sizes) DominantType() (GGMLType, bool) {
	var best TypeUsage
	var found bool
	for _, u := range s.ByType {
		if !u.Type.Quantized() {
			continue
		}
		if !found || u.Bytes > best.Bytes {
			best, found = u, true
		}
	}
	if found {
		return best.Type, true
	}
	for _, u := range s.ByType {
		if !found || u.Bytes > best.Bytes {
			best, found = u, true
		}
	}
	return best.Type, found
}
