// Package ggufbuild writes small, valid GGUF files for tests.
//
// DESIGN section 15 asks internal/gguf to be tested against checked-in headers
// for llama, qwen3, gemma3, an MoE, a sharded set and an mmproj. Real ones would
// have to be downloaded — gigabytes, gated repos, and a fixture nobody can
// reproduce or explain. Synthetic ones are better on every axis that matters
// here: the parser's job is the FORMAT, every fixture's expected values are
// written down beside it, and a shape that does not exist yet (a per-layer
// head_count_kv array beside a sliding window, a nested array) can be built the
// moment it is worth a test.
//
// The files this writes are byte-for-byte what the reference writer produces for
// the same input, including the big-endian convention: the magic is always the
// byte sequence "GGUF" and every other field follows the chosen order.
//
// It lives under internal/gguf/ because that is the tree it serves, next to the
// parser it feeds, and it imports internal/gguf for the two enums and the size
// arithmetic that decide the tensor layout. That last import means a round-trip
// test alone would not catch a wrong entry in the parser's ggml type table — the
// same wrong number would lay the file out — so the type table is pinned
// separately by a test with hand-written literals.
//
// It is an ordinary package rather than a test file so that other packages' tests
// can use it too: internal/fit's SWA cases (DESIGN section 8.3) and
// internal/models' scan cases need GGUF files with chosen metadata, and building
// one in the test that needs it beats reaching across the tree for a fixture
// whose expected values live somewhere else.
package ggufbuild

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"strconv"

	"github.com/jlbyh2o/llamaman/internal/gguf"
)

// Val is a metadata value to be written. Build one with the constructors below;
// the zero Val is a uint8 zero.
type Val struct {
	typ      gguf.ValueType
	u        uint64
	i        int64
	f        float64
	b        bool
	s        string
	elemType gguf.ValueType
	elems    []Val
	raw      []byte
	isRaw    bool
}

// The scalar constructors, one per GGUF metadata type.
func U8(v uint8) Val   { return Val{typ: gguf.ValueUint8, u: uint64(v)} }
func U16(v uint16) Val { return Val{typ: gguf.ValueUint16, u: uint64(v)} }
func U32(v uint32) Val { return Val{typ: gguf.ValueUint32, u: uint64(v)} }
func U64(v uint64) Val { return Val{typ: gguf.ValueUint64, u: v} }
func I8(v int8) Val    { return Val{typ: gguf.ValueInt8, i: int64(v)} }
func I16(v int16) Val  { return Val{typ: gguf.ValueInt16, i: int64(v)} }
func I32(v int32) Val  { return Val{typ: gguf.ValueInt32, i: int64(v)} }
func I64(v int64) Val  { return Val{typ: gguf.ValueInt64, i: v} }
func F32(v float32) Val {
	return Val{typ: gguf.ValueFloat32, f: float64(v)}
}
func F64(v float64) Val { return Val{typ: gguf.ValueFloat64, f: v} }
func Bool(v bool) Val   { return Val{typ: gguf.ValueBool, b: v} }
func Str(v string) Val  { return Val{typ: gguf.ValueString, s: v} }

// Arr writes an array of elemType. The elements' own types are not written —
// GGUF arrays are homogeneous — so a caller that mixes types produces a file
// that decodes as elemType, which is a corruption case worth being able to
// build.
func Arr(elemType gguf.ValueType, elems ...Val) Val {
	return Val{typ: gguf.ValueArray, elemType: elemType, elems: elems}
}

// U32s is Arr(gguf.ValueUint32, …) for the common per-layer integer array.
func U32s(vs ...uint32) Val {
	elems := make([]Val, len(vs))
	for i, v := range vs {
		elems[i] = U32(v)
	}
	return Arr(gguf.ValueUint32, elems...)
}

// Strs is Arr(gguf.ValueString, …) — a tokenizer vocabulary, usually.
func Strs(vs ...string) Val {
	elems := make([]Val, len(vs))
	for i, v := range vs {
		elems[i] = Str(v)
	}
	return Arr(gguf.ValueString, elems...)
}

// Raw writes payload verbatim under the type tag t, which may be a type the
// format does not define. It is how a test builds the malformed input the parser
// has to refuse.
func Raw(t gguf.ValueType, payload []byte) Val {
	return Val{typ: t, raw: payload, isRaw: true}
}

type kvEntry struct {
	key string
	val Val
}

type tensorEntry struct {
	name      string
	typ       gguf.GGMLType
	dims      []uint64
	offset    uint64
	overrides bool
}

// Builder accumulates metadata and tensors and writes them out. The zero value
// is not usable; call New.
type Builder struct {
	version   uint32
	bigEndian bool
	alignment uint64
	kv        []kvEntry
	tensors   []tensorEntry
}

// New starts a version 3, little-endian, 32-byte-aligned file. arch, when
// non-empty, is written as general.architecture — the key every other key is
// namespaced under.
func New(arch string) *Builder {
	b := &Builder{version: 3, alignment: 32}
	if arch != "" {
		b.Set(gguf.KeyArchitecture, Str(arch))
	}
	return b
}

// Version overrides the header version. Values other than 2 and 3 are written
// as given, which is how the unsupported-version case is built.
func (b *Builder) Version(v uint32) *Builder { b.version = v; return b }

// BigEndian writes every field except the magic in big-endian order.
func (b *Builder) BigEndian(v bool) *Builder { b.bigEndian = v; return b }

// Alignment sets the tensor-data alignment and writes general.alignment. A
// caller wanting the key absent — the default-32 case — simply does not call
// this.
func (b *Builder) Alignment(a uint64) *Builder {
	b.alignment = a
	return b.Set(gguf.KeyAlignment, U32(uint32(a)))
}

// Set appends a metadata pair. Keys are written in call order and are not
// deduplicated, so a duplicate can be built on purpose.
func (b *Builder) Set(key string, v Val) *Builder {
	b.kv = append(b.kv, kvEntry{key: key, val: v})
	return b
}

// Tensor appends a tensor descriptor. Its offset is computed from the tensors
// already added — each one's size padded up to the alignment — which is the
// consecutive layout the format requires.
func (b *Builder) Tensor(name string, t gguf.GGMLType, dims ...uint64) *Builder {
	b.tensors = append(b.tensors, tensorEntry{name: name, typ: t, dims: dims})
	return b
}

// TensorAt appends a tensor with an offset of the caller's choosing, for the
// test that a tensor index which does not lay its data out consecutively is
// refused.
func (b *Builder) TensorAt(name string, t gguf.GGMLType, offset uint64, dims ...uint64) *Builder {
	b.tensors = append(b.tensors, tensorEntry{name: name, typ: t, dims: dims, offset: offset, overrides: true})
	return b
}

// Layers is the shorthand every fixture uses: for each of n blocks, the four
// tensors a transformer layer always has, sized off the embedding length. It
// keeps the fixtures short enough to read while still producing a tensor index
// that DESIGN section 8.2's per-layer bucketing has real work to do on.
func (b *Builder) Layers(n int, embd uint64, ff uint64, t gguf.GGMLType) *Builder {
	for i := 0; i < n; i++ {
		p := "blk." + strconv.Itoa(i) + "."
		b.Tensor(p+"attn_norm.weight", gguf.TypeF32, embd)
		b.Tensor(p+"attn_qkv.weight", t, embd, embd*3)
		b.Tensor(p+"ffn_up.weight", t, embd, ff)
		b.Tensor(p+"ffn_down.weight", t, ff, embd)
	}
	return b
}

func (b *Builder) order() binary.ByteOrder {
	if b.bigEndian {
		return binary.BigEndian
	}
	return binary.LittleEndian
}

// DataSize is the length of the tensor-data region the current tensor list
// implies: each tensor's byte size padded up to the alignment, summed.
func (b *Builder) DataSize() uint64 {
	var running uint64
	for _, t := range b.tensors {
		running += padUp(b.bytesOf(t), b.alignment)
	}
	return running
}

func (b *Builder) bytesOf(t tensorEntry) uint64 {
	return gguf.TensorInfo{Name: t.name, Dims: t.dims, Type: t.typ}.Bytes()
}

func padUp(n, align uint64) uint64 {
	if align == 0 {
		return n
	}
	return (n + align - 1) / align * align
}

// Header writes the file up to the end of the tensor index, with no padding and
// no tensor data. This is the "truncated header" of DESIGN section 15: a few
// kilobytes that parse completely, and the exact shape a remote Range peek sees.
func (b *Builder) Header() []byte {
	var buf bytes.Buffer
	order := b.order()

	// The magic is a byte sequence, not an integer: it reads "GGUF" in both byte
	// orders, which is why the version below is what discloses the order.
	buf.WriteString("GGUF")
	writeU32(&buf, order, b.version)
	writeU64(&buf, order, uint64(len(b.tensors)))
	writeU64(&buf, order, uint64(len(b.kv)))

	for _, e := range b.kv {
		writeString(&buf, order, e.key)
		writeU32(&buf, order, uint32(e.val.typ))
		writeValue(&buf, order, e.val)
	}

	var running uint64
	for _, t := range b.tensors {
		writeString(&buf, order, t.name)
		writeU32(&buf, order, uint32(len(t.dims)))
		for _, d := range t.dims {
			writeU64(&buf, order, d)
		}
		writeU32(&buf, order, uint32(t.typ))
		off := running
		if t.overrides {
			off = t.offset
		}
		writeU64(&buf, order, off)
		running += padUp(b.bytesOf(t), b.alignment)
	}
	return buf.Bytes()
}

// Full writes the header, the alignment padding, and the tensor data as zeros —
// a complete file, for the test that File.Complete tells the two apart.
func (b *Builder) Full() []byte {
	h := b.Header()
	total := padUp(uint64(len(h)), b.alignment) + b.DataSize()
	out := make([]byte, total)
	copy(out, h)
	return out
}

// WriteFile writes Header (or Full) to path, creating parent directories.
func (b *Builder) WriteFile(path string, full bool) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data := b.Header()
	if full {
		data = b.Full()
	}
	return os.WriteFile(path, data, 0o644)
}

func writeU16(w *bytes.Buffer, o binary.ByteOrder, v uint16) {
	var p [2]byte
	o.PutUint16(p[:], v)
	w.Write(p[:])
}

func writeU32(w *bytes.Buffer, o binary.ByteOrder, v uint32) {
	var p [4]byte
	o.PutUint32(p[:], v)
	w.Write(p[:])
}

func writeU64(w *bytes.Buffer, o binary.ByteOrder, v uint64) {
	var p [8]byte
	o.PutUint64(p[:], v)
	w.Write(p[:])
}

func writeString(w *bytes.Buffer, o binary.ByteOrder, s string) {
	writeU64(w, o, uint64(len(s)))
	w.WriteString(s)
}

// writeValue writes a value's payload; its type tag is written by the caller,
// because an array writes its elements' tag once rather than per element.
func writeValue(w *bytes.Buffer, o binary.ByteOrder, v Val) {
	if v.isRaw {
		w.Write(v.raw)
		return
	}
	switch v.typ {
	case gguf.ValueUint8:
		w.WriteByte(byte(v.u))
	case gguf.ValueInt8:
		w.WriteByte(byte(int8(v.i)))
	case gguf.ValueUint16:
		writeU16(w, o, uint16(v.u))
	case gguf.ValueInt16:
		writeU16(w, o, uint16(int16(v.i)))
	case gguf.ValueUint32:
		writeU32(w, o, uint32(v.u))
	case gguf.ValueInt32:
		writeU32(w, o, uint32(int32(v.i)))
	case gguf.ValueFloat32:
		writeU32(w, o, math.Float32bits(float32(v.f)))
	case gguf.ValueBool:
		if v.b {
			w.WriteByte(1)
		} else {
			w.WriteByte(0)
		}
	case gguf.ValueString:
		writeString(w, o, v.s)
	case gguf.ValueArray:
		writeU32(w, o, uint32(v.elemType))
		writeU64(w, o, uint64(len(v.elems)))
		for _, e := range v.elems {
			writeValue(w, o, e)
		}
	case gguf.ValueUint64:
		writeU64(w, o, v.u)
	case gguf.ValueInt64:
		writeU64(w, o, uint64(v.i))
	case gguf.ValueFloat64:
		writeU64(w, o, math.Float64bits(v.f))
	}
}
