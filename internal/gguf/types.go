package gguf

import "strconv"

// ValueType is the tag on a metadata value — the thirteen types the GGUF
// specification defines, written as a uint32 before every value and before
// every array's elements.
type ValueType uint32

// The metadata value types, in the specification's own numbering. The gaps that
// exist among ggml TENSOR types (below) have no counterpart here: this list is
// dense and closed.
const (
	ValueUint8   ValueType = 0
	ValueInt8    ValueType = 1
	ValueUint16  ValueType = 2
	ValueInt16   ValueType = 3
	ValueUint32  ValueType = 4
	ValueInt32   ValueType = 5
	ValueFloat32 ValueType = 6
	ValueBool    ValueType = 7
	ValueString  ValueType = 8
	ValueArray   ValueType = 9
	ValueUint64  ValueType = 10
	ValueInt64   ValueType = 11
	ValueFloat64 ValueType = 12
)

// valueTypeNames is indexed by ValueType; a value outside it is unknown.
var valueTypeNames = [...]string{
	ValueUint8:   "uint8",
	ValueInt8:    "int8",
	ValueUint16:  "uint16",
	ValueInt16:   "int16",
	ValueUint32:  "uint32",
	ValueInt32:   "int32",
	ValueFloat32: "float32",
	ValueBool:    "bool",
	ValueString:  "string",
	ValueArray:   "array",
	ValueUint64:  "uint64",
	ValueInt64:   "int64",
	ValueFloat64: "float64",
}

// Valid reports whether t is one of the thirteen defined value types.
func (t ValueType) Valid() bool { return int(t) < len(valueTypeNames) }

// String names the type, or reports the raw number when it is not one of the
// thirteen — which is what a corrupt or future file looks like.
func (t ValueType) String() string {
	if !t.Valid() {
		return "value_type(" + strconv.FormatUint(uint64(t), 10) + ")"
	}
	return valueTypeNames[t]
}

// wireSize is the number of bytes a value of this type occupies when its length
// is fixed, and 0 for the two variable-length types (string and array). It is
// both the decoder's read width and the bound that stops a declared array
// length from being believed: an array of n elements cannot be shorter than
// n×wireSize bytes, so an n larger than the file is rejected before anything is
// allocated.
func (t ValueType) wireSize() uint64 {
	switch t {
	case ValueUint8, ValueInt8, ValueBool:
		return 1
	case ValueUint16, ValueInt16:
		return 2
	case ValueUint32, ValueInt32, ValueFloat32:
		return 4
	case ValueUint64, ValueInt64, ValueFloat64:
		return 8
	default:
		return 0
	}
}

// minWireSize is wireSize for the fixed-width types and the smallest possible
// encoding for the other two: a string is at least its 8-byte length prefix, an
// array at least its 4-byte element type plus its 8-byte count.
func (t ValueType) minWireSize() uint64 {
	switch t {
	case ValueString:
		return 8
	case ValueArray:
		return 12
	default:
		return t.wireSize()
	}
}

// GGMLType is a tensor's element type: the `ggml_type` enum, whose numbering is
// upstream's and has permanent gaps where formats were removed (Q4_2, Q4_3, the
// repacked Q4_0_4_4 family). The gaps are preserved rather than compacted,
// because the number is what is written in the file.
type GGMLType uint32

// The ggml tensor types, mirroring `enum ggml_type` name for name so that a
// reader with upstream open can check this table against it.
const (
	TypeF32     GGMLType = 0
	TypeF16     GGMLType = 1
	TypeQ4_0    GGMLType = 2
	TypeQ4_1    GGMLType = 3
	TypeQ5_0    GGMLType = 6
	TypeQ5_1    GGMLType = 7
	TypeQ8_0    GGMLType = 8
	TypeQ8_1    GGMLType = 9
	TypeQ2_K    GGMLType = 10
	TypeQ3_K    GGMLType = 11
	TypeQ4_K    GGMLType = 12
	TypeQ5_K    GGMLType = 13
	TypeQ6_K    GGMLType = 14
	TypeQ8_K    GGMLType = 15
	TypeIQ2_XXS GGMLType = 16
	TypeIQ2_XS  GGMLType = 17
	TypeIQ3_XXS GGMLType = 18
	TypeIQ1_S   GGMLType = 19
	TypeIQ4_NL  GGMLType = 20
	TypeIQ3_S   GGMLType = 21
	TypeIQ2_S   GGMLType = 22
	TypeIQ4_XS  GGMLType = 23
	TypeI8      GGMLType = 24
	TypeI16     GGMLType = 25
	TypeI32     GGMLType = 26
	TypeI64     GGMLType = 27
	TypeF64     GGMLType = 28
	TypeIQ1_M   GGMLType = 29
	TypeBF16    GGMLType = 30
	TypeTQ1_0   GGMLType = 34
	TypeTQ2_0   GGMLType = 35
	TypeMXFP4   GGMLType = 39
)

// tensorTraits is one row of the ggml type table: how many elements share a
// quantization block, and how many bytes that block occupies on disk. A row
// with blockSize 0 is a hole — a removed type, or a number this build does not
// know — and sizing a tensor of that type fails rather than guessing.
type tensorTraits struct {
	name      string
	blockSize uint64
	typeSize  uint64
}

// tensorTypeTraits is `ggml_type_traits[]`, indexed by the type number, with the
// removed types left as holes. Each typeSize is the `sizeof(block_*)` upstream
// asserts, and the bytes-per-weight it implies is in the comment so a wrong
// entry is visible without a compiler: these numbers multiply every weight
// figure DESIGN section 8.2 produces, and an error here would be a silent,
// plausible-looking mis-estimate rather than a crash.
var tensorTypeTraits = [...]tensorTraits{
	TypeF32:  {"f32", 1, 4},
	TypeF16:  {"f16", 1, 2},
	TypeQ4_0: {"q4_0", 32, 18},  // 2 + 16      = 4.50 bpw
	TypeQ4_1: {"q4_1", 32, 20},  // 2 + 2 + 16  = 5.00 bpw
	TypeQ5_0: {"q5_0", 32, 22},  // 2 + 4 + 16  = 5.50 bpw
	TypeQ5_1: {"q5_1", 32, 24},  // 2+2 + 4 +16 = 6.00 bpw
	TypeQ8_0: {"q8_0", 32, 34},  // 2 + 32      = 8.50 bpw
	TypeQ8_1: {"q8_1", 32, 36},  // 2+2 + 32    = 9.00 bpw
	TypeQ2_K: {"q2_K", 256, 84}, // 16+64+2+2   = 2.625 bpw
	TypeQ3_K: {"q3_K", 256, 110},
	TypeQ4_K: {"q4_K", 256, 144}, // 2+2+12+128 = 4.50 bpw
	TypeQ5_K: {"q5_K", 256, 176},
	TypeQ6_K: {"q6_K", 256, 210}, // 128+64+16+2 = 6.5625 bpw
	TypeQ8_K: {"q8_K", 256, 292}, // intermediate type; never appears in a file
	// The i-quants. Each typeSize is sizeof(block_iq*) and the comment is the
	// advertised bits per weight, which is typeSize*8/blockSize.
	TypeIQ2_XXS: {"iq2_xxs", 256, 66}, // 2.0625 bpw
	TypeIQ2_XS:  {"iq2_xs", 256, 74},  // 2.3125 bpw
	TypeIQ3_XXS: {"iq3_xxs", 256, 98}, // 3.0625 bpw
	TypeIQ1_S:   {"iq1_s", 256, 50},   // 1.5625 bpw
	TypeIQ4_NL:  {"iq4_nl", 32, 18},   // 4.50   bpw
	TypeIQ3_S:   {"iq3_s", 256, 110},  // 3.4375 bpw
	TypeIQ2_S:   {"iq2_s", 256, 82},   // 2.5625 bpw
	TypeIQ4_XS:  {"iq4_xs", 256, 136}, // 4.25   bpw
	TypeI8:      {"i8", 1, 1},
	TypeI16:     {"i16", 1, 2},
	TypeI32:     {"i32", 1, 4},
	TypeI64:     {"i64", 1, 8},
	TypeF64:     {"f64", 1, 8},
	TypeIQ1_M:   {"iq1_m", 256, 56}, // 1.75 bpw
	TypeBF16:    {"bf16", 1, 2},
	TypeTQ1_0:   {"tq1_0", 256, 54}, // 1.6875 bpw ternary
	TypeTQ2_0:   {"tq2_0", 256, 66}, // 2.0625 bpw ternary
	TypeMXFP4:   {"mxfp4", 32, 17},  // 1 shared exponent + 16 = 4.25 bpw
}

func (t GGMLType) traits() (tensorTraits, bool) {
	if int(t) >= len(tensorTypeTraits) {
		return tensorTraits{}, false
	}
	tr := tensorTypeTraits[t]
	if tr.blockSize == 0 {
		return tensorTraits{}, false
	}
	return tr, true
}

// Valid reports whether this build knows the type's block geometry, and
// therefore whether a tensor of this type can be sized at all.
func (t GGMLType) Valid() bool { _, ok := t.traits(); return ok }

// String is upstream's `type_name` — "q4_K", "iq4_xs", "bf16" — or the raw
// number for a type this build does not know.
func (t GGMLType) String() string {
	if tr, ok := t.traits(); ok {
		return tr.name
	}
	return "ggml_type(" + strconv.FormatUint(uint64(t), 10) + ")"
}

// BlockSize is how many elements share one quantization block: 1 for the plain
// float and integer types, 32 for the legacy quants and 256 for the K- and
// i-quants. A tensor's first dimension must be a multiple of it.
func (t GGMLType) BlockSize() uint64 { tr, _ := t.traits(); return tr.blockSize }

// TypeSize is the on-disk size of one block, in bytes.
func (t GGMLType) TypeSize() uint64 { tr, _ := t.traits(); return tr.typeSize }

// BitsPerWeight is TypeSize×8/BlockSize — the number quant names advertise
// ("4.25 bpw"). It is reporting only; every byte figure in this package is
// computed from BlockSize and TypeSize in integers, never from this float.
func (t GGMLType) BitsPerWeight() float64 {
	tr, ok := t.traits()
	if !ok {
		return 0
	}
	return float64(tr.typeSize) * 8 / float64(tr.blockSize)
}

// Quantized reports whether the type stores more than one element per block —
// that is, whether it is a quantization rather than a plain float or integer
// array. It is what separates the tensors that carry a file's quant identity
// from the norms and biases that are f32 in every quant of the same model.
func (t GGMLType) Quantized() bool { return t.BlockSize() > 1 }

// fileTypeNames maps `general.file_type` — the `llama_ftype` enum, which names
// a whole FILE's quantization mix rather than one tensor's type — to the label
// users know it by. The gaps are the ftypes upstream removed.
//
// The distinction matters because the two answers differ: a Q4_K_M file is a
// mixture whose tensors are q4_K, q6_K and f32, and no single tensor type is
// called "Q4_K_M". This table is therefore the preferred source for
// `models.file_type`, with the dominant tensor type as the fallback when the key
// is absent or holds a number this table does not know (see Shape.Quantization).
var fileTypeNames = map[uint64]string{
	0:  "F32",
	1:  "F16",
	2:  "Q4_0",
	3:  "Q4_1",
	7:  "Q8_0",
	8:  "Q5_0",
	9:  "Q5_1",
	10: "Q2_K",
	11: "Q3_K_S",
	12: "Q3_K_M",
	13: "Q3_K_L",
	14: "Q4_K_S",
	15: "Q4_K_M",
	16: "Q5_K_S",
	17: "Q5_K_M",
	18: "Q6_K",
	19: "IQ2_XXS",
	20: "IQ2_XS",
	21: "Q2_K_S",
	22: "IQ3_XS",
	23: "IQ3_XXS",
	24: "IQ1_S",
	25: "IQ4_NL",
	26: "IQ3_S",
	27: "IQ3_M",
	28: "IQ2_S",
	29: "IQ2_M",
	30: "IQ4_XS",
	31: "IQ1_M",
	32: "BF16",
	36: "TQ1_0",
	37: "TQ2_0",
	38: "MXFP4_MOE",
}

// FileTypeName maps a `general.file_type` value to its label ("Q4_K_M"), and
// reports false for a number this build does not know — in which case the
// caller should fall back to the dominant tensor type rather than invent a name.
func FileTypeName(ft uint64) (string, bool) {
	name, ok := fileTypeNames[ft]
	return name, ok
}
