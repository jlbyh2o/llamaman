package gguf_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"strconv"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/jlbyh2o/llamaman/internal/gguf"
	"github.com/jlbyh2o/llamaman/internal/gguf/ggufbuild"
)

// TestValueRoundTrip is the round trip DESIGN section 15 asks for: one key of
// every metadata value type, written by the builder and read back by the parser,
// compared exactly. The two halves are independent implementations of the same
// wire format, so a disagreement about a width, a sign or a byte order shows up
// here rather than as a wrong number in a fit report.
func TestValueRoundTrip(t *testing.T) {
	f := loadFixture(t, "alltypes.gguf")

	tests := []struct {
		key  string
		want gguf.Value
	}{
		{"test.u8", gguf.Value{Type: gguf.ValueUint8, Uint: 200}},
		{"test.i8", gguf.Value{Type: gguf.ValueInt8, Int: -100}},
		{"test.u16", gguf.Value{Type: gguf.ValueUint16, Uint: 60000}},
		{"test.i16", gguf.Value{Type: gguf.ValueInt16, Int: -30000}},
		{"test.u32", gguf.Value{Type: gguf.ValueUint32, Uint: 4000000000}},
		{"test.i32", gguf.Value{Type: gguf.ValueInt32, Int: -2000000000}},
		{"test.f32", gguf.Value{Type: gguf.ValueFloat32, Float: 0.5}},
		{"test.bool_true", gguf.Value{Type: gguf.ValueBool, Bool: true}},
		{"test.bool_false", gguf.Value{Type: gguf.ValueBool, Bool: false}},
		{"test.string", gguf.Value{Type: gguf.ValueString, String: "héllo, wörld"}},
		{"test.string_empty", gguf.Value{Type: gguf.ValueString, String: ""}},
		{"test.u64", gguf.Value{Type: gguf.ValueUint64, Uint: 18000000000000000000}},
		{"test.i64", gguf.Value{Type: gguf.ValueInt64, Int: -9000000000000000000}},
		{"test.f64", gguf.Value{Type: gguf.ValueFloat64, Float: -1.25}},
		{"test.arr_u8", arrayValue(gguf.ValueUint8,
			gguf.Value{Type: gguf.ValueUint8, Uint: 1},
			gguf.Value{Type: gguf.ValueUint8, Uint: 2})},
		{"test.arr_i8", arrayValue(gguf.ValueInt8,
			gguf.Value{Type: gguf.ValueInt8, Int: -1},
			gguf.Value{Type: gguf.ValueInt8, Int: 2})},
		{"test.arr_u16", arrayValue(gguf.ValueUint16,
			gguf.Value{Type: gguf.ValueUint16, Uint: 1},
			gguf.Value{Type: gguf.ValueUint16, Uint: 65535})},
		{"test.arr_i16", arrayValue(gguf.ValueInt16,
			gguf.Value{Type: gguf.ValueInt16, Int: -32768})},
		{"test.arr_u32", arrayValue(gguf.ValueUint32,
			gguf.Value{Type: gguf.ValueUint32, Uint: 3},
			gguf.Value{Type: gguf.ValueUint32, Uint: 5},
			gguf.Value{Type: gguf.ValueUint32, Uint: 8})},
		{"test.arr_i32", arrayValue(gguf.ValueInt32,
			gguf.Value{Type: gguf.ValueInt32, Int: -7})},
		{"test.arr_f32", arrayValue(gguf.ValueFloat32,
			gguf.Value{Type: gguf.ValueFloat32, Float: 1.5},
			gguf.Value{Type: gguf.ValueFloat32, Float: -2.5})},
		{"test.arr_bool", arrayValue(gguf.ValueBool,
			gguf.Value{Type: gguf.ValueBool, Bool: true},
			gguf.Value{Type: gguf.ValueBool, Bool: false})},
		{"test.arr_string", arrayValue(gguf.ValueString,
			gguf.Value{Type: gguf.ValueString, String: "a"},
			gguf.Value{Type: gguf.ValueString, String: ""},
			gguf.Value{Type: gguf.ValueString, String: "cc"})},
		{"test.arr_u64", arrayValue(gguf.ValueUint64,
			gguf.Value{Type: gguf.ValueUint64, Uint: 1 << 40})},
		{"test.arr_i64", arrayValue(gguf.ValueInt64,
			gguf.Value{Type: gguf.ValueInt64, Int: -(1 << 40)})},
		{"test.arr_f64", arrayValue(gguf.ValueFloat64,
			gguf.Value{Type: gguf.ValueFloat64, Float: 3.5})},
		{"test.arr_empty", arrayValue(gguf.ValueUint32)},
		{"test.arr_nested", arrayValue(gguf.ValueArray,
			arrayValue(gguf.ValueUint32,
				gguf.Value{Type: gguf.ValueUint32, Uint: 1},
				gguf.Value{Type: gguf.ValueUint32, Uint: 2}),
			arrayValue(gguf.ValueUint32,
				gguf.Value{Type: gguf.ValueUint32, Uint: 3}))},
	}

	for _, tc := range tests {
		t.Run(tc.key, func(t *testing.T) {
			got, ok := f.KV.Get(tc.key)
			if !ok {
				t.Fatalf("key %q is missing", tc.key)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("value mismatch (-want +got):\n%s", diff)
			}
		})
	}

	// Every key in the file is one of the cases above, plus the architecture the
	// builder writes. A key added to the fixture without a case here would go
	// untested, so the count is asserted rather than assumed.
	if want := len(tests) + 1; f.KV.Len() != want {
		t.Errorf("the fixture has %d metadata keys and this test covers %d", f.KV.Len(), want)
	}
}

func arrayValue(elem gguf.ValueType, vals ...gguf.Value) gguf.Value {
	a := &gguf.Array{Type: elem, Len: uint64(len(vals))}
	a.Values = append(a.Values, vals...)
	if len(vals) == 0 {
		a.Values = []gguf.Value{}
	}
	return gguf.Value{Type: gguf.ValueArray, Array: a}
}

// TestBigEndianMatchesLittleEndian is DESIGN section 8.5's big-endian handling,
// stated as the only property that matters: the same model written in the other
// byte order must parse to the same numbers.
//
// The two fixtures are the same builder with one flag flipped, so any field this
// parser reads in the wrong order shows up as a difference — and the magic,
// which is a byte sequence rather than an integer and therefore identical in
// both files, is asserted separately because getting it wrong would make every
// big-endian file simply "not a GGUF file".
func TestBigEndianMatchesLittleEndian(t *testing.T) {
	le := loadFixture(t, "llama.gguf")
	be := loadFixture(t, "llama-be.gguf")

	if le.BigEndian {
		t.Error("llama.gguf reports big-endian")
	}
	if !be.BigEndian {
		t.Fatal("llama-be.gguf reports little-endian: the version-field probe did not fire")
	}

	raw := fixtureBytes(t, "llama-be.gguf")
	if got := string(raw[:4]); got != "GGUF" {
		t.Errorf("big-endian magic = %q, want %q — the magic is a byte sequence and never swaps", got, "GGUF")
	}
	if got := binary.BigEndian.Uint32(raw[4:8]); got != 3 {
		t.Errorf("big-endian version field = %d, want 3", got)
	}

	if diff := cmp.Diff(le.KV.All(), be.KV.All()); diff != "" {
		t.Errorf("metadata differs between byte orders (-little +big):\n%s", diff)
	}
	if diff := cmp.Diff(le.Tensors, be.Tensors); diff != "" {
		t.Errorf("tensor index differs between byte orders (-little +big):\n%s", diff)
	}
	if diff := cmp.Diff(le.Shape(), be.Shape()); diff != "" {
		t.Errorf("shape differs between byte orders (-little +big):\n%s", diff)
	}
	if le.HeaderBytes != be.HeaderBytes || le.DataOffset != be.DataOffset || le.DataSize != be.DataSize {
		t.Errorf("geometry differs: little %+v, big %+v",
			[]int64{le.HeaderBytes, le.DataOffset, le.DataSize},
			[]int64{be.HeaderBytes, be.DataOffset, be.DataSize})
	}
}

// TestArrayElision covers section 8.5's "huge token arrays are parsed but not
// retained": past the limit the elements are dropped, the declared length
// survives — which is where n_vocab comes from — and the bytes are still walked,
// so a truncation inside an elided array is still caught.
func TestArrayElision(t *testing.T) {
	t.Run("default limit elides a vocabulary", func(t *testing.T) {
		f := loadFixture(t, "bigvocab.gguf")
		v, ok := f.KV.Get(gguf.KeyTokenizerTokens)
		if !ok {
			t.Fatal("tokenizer.ggml.tokens is missing")
		}
		if !v.Array.Elided {
			t.Errorf("an array of %d was retained; DefaultArrayLimit is %d", v.Array.Len, gguf.DefaultArrayLimit)
		}
		if len(v.Array.Values) != 0 {
			t.Errorf("elided array kept %d elements", len(v.Array.Values))
		}
		if v.Count() != 1500 {
			t.Errorf("Count() = %d, want 1500 — the length must survive elision", v.Count())
		}
		if got := f.Shape().VocabSize; got != 1500 {
			t.Errorf("VocabSize = %d, want 1500", got)
		}
		if _, ok := v.AsInts(); ok {
			t.Error("AsInts succeeded on an elided array; it must refuse rather than answer short")
		}
	})

	t.Run("limit retains what fits", func(t *testing.T) {
		f := loadFixture(t, "bigvocab.gguf", gguf.WithArrayLimit(-1))
		v, _ := f.KV.Get(gguf.KeyTokenizerTokens)
		if v.Array.Elided {
			t.Fatal("a negative limit must retain everything")
		}
		if len(v.Array.Values) != 1500 {
			t.Errorf("retained %d elements, want 1500", len(v.Array.Values))
		}
	})

	t.Run("zero limit elides everything", func(t *testing.T) {
		f := loadFixture(t, "gemma3.gguf", gguf.WithArrayLimit(0))
		v, _ := f.KV.Get("gemma3.attention.head_count_kv")
		if !v.Array.Elided {
			t.Error("a zero limit must elide even a six-element array")
		}
	})

	t.Run("a truncation inside an elided array is caught", func(t *testing.T) {
		raw := fixtureBytes(t, "bigvocab.gguf")
		_, err := gguf.Parse(bytes.NewReader(raw[:len(raw)/2]), int64(len(raw)/2))
		if !errors.Is(err, gguf.ErrTruncated) {
			t.Fatalf("error = %v, want ErrTruncated", err)
		}
	})
}

// TestHeaderGeometry pins the three offsets a caller reasons about — where the
// header ended, where the data starts, how long it is — and the predicate that
// separates a header-only fixture from a whole file.
func TestHeaderGeometry(t *testing.T) {
	t.Run("header only", func(t *testing.T) {
		f := loadFixture(t, "llama.gguf")
		raw := fixtureBytes(t, "llama.gguf")
		if f.HeaderBytes != int64(len(raw)) {
			t.Errorf("HeaderBytes = %d, want %d: a header-only fixture is exactly its header", f.HeaderBytes, len(raw))
		}
		if f.Alignment != gguf.DefaultAlignment {
			t.Errorf("Alignment = %d, want the %d default", f.Alignment, gguf.DefaultAlignment)
		}
		if f.DataOffset%int64(f.Alignment) != 0 || f.DataOffset < f.HeaderBytes {
			t.Errorf("DataOffset = %d is not HeaderBytes %d rounded up to %d", f.DataOffset, f.HeaderBytes, f.Alignment)
		}
		if f.Complete() {
			t.Error("Complete() is true for a header with no tensor data")
		}
	})

	t.Run("whole file", func(t *testing.T) {
		f := loadFixture(t, "complete.gguf")
		if !f.Complete() {
			t.Errorf("Complete() is false: size %d, data at %d + %d", f.FileSize, f.DataOffset, f.DataSize)
		}
		if want := f.DataOffset + f.DataSize; f.FileSize != want {
			t.Errorf("FileSize = %d, want exactly %d", f.FileSize, want)
		}
	})

	t.Run("explicit alignment", func(t *testing.T) {
		b := ggufbuild.New("llama").Alignment(64)
		b.Tensor("a.weight", gguf.TypeF32, 3)
		b.Tensor("b.weight", gguf.TypeF32, 3)
		f := parseBytes(t, b.Header())
		if f.Alignment != 64 {
			t.Fatalf("Alignment = %d, want 64", f.Alignment)
		}
		// 3 f32 is 12 bytes, padded to 64, so the second tensor sits at 64 and
		// the region is 128 long.
		if f.Tensors[1].Offset != 64 {
			t.Errorf("second tensor offset = %d, want 64", f.Tensors[1].Offset)
		}
		if f.DataSize != 128 {
			t.Errorf("DataSize = %d, want 128", f.DataSize)
		}
	})
}

// TestVersions covers the two supported versions and the refusal of everything
// else. Version 2 differs from 3 only in ways this parser does not depend on, so
// the same builder output is valid under both stamps.
func TestVersions(t *testing.T) {
	for _, v := range []uint32{2, 3} {
		t.Run("v"+strconv.Itoa(int(v)), func(t *testing.T) {
			b := llamaFixture().Version(v)
			f := parseBytes(t, b.Header())
			if f.Version != v {
				t.Errorf("Version = %d, want %d", f.Version, v)
			}
		})
	}
	for _, v := range []uint32{0, 1, 4, 1000} {
		t.Run("reject v"+strconv.Itoa(int(v)), func(t *testing.T) {
			b := llamaFixture().Version(v)
			_, err := gguf.Parse(bytes.NewReader(b.Header()), int64(len(b.Header())))
			if !errors.Is(err, gguf.ErrUnsupportedVersion) {
				t.Fatalf("error = %v, want ErrUnsupportedVersion", err)
			}
		})
	}
}

// TestValueAccessors covers the coercions Value offers, which exist because GGUF
// producers disagree about the width of the same logical key.
func TestValueAccessors(t *testing.T) {
	tests := []struct {
		name string
		v    gguf.Value
		uint any // uint64, or nil for "refused"
		int  any
		str  any
		bool any
	}{
		{
			name: "uint32",
			v:    gguf.Value{Type: gguf.ValueUint32, Uint: 7},
			uint: uint64(7), int: int64(7), str: nil, bool: true,
		},
		{
			name: "negative int32",
			v:    gguf.Value{Type: gguf.ValueInt32, Int: -7},
			uint: nil, int: int64(-7), str: nil, bool: true,
		},
		{
			name: "uint64 above MaxInt64",
			v:    gguf.Value{Type: gguf.ValueUint64, Uint: math.MaxUint64},
			uint: uint64(math.MaxUint64), int: nil, str: nil, bool: true,
		},
		{
			name: "bool",
			v:    gguf.Value{Type: gguf.ValueBool, Bool: true},
			uint: uint64(1), int: int64(1), str: nil, bool: true,
		},
		{
			name: "string is never coerced to a number",
			v:    gguf.Value{Type: gguf.ValueString, String: "12"},
			uint: nil, int: nil, str: "12", bool: nil,
		},
		{
			name: "zero integer reads as false",
			v:    gguf.Value{Type: gguf.ValueUint8, Uint: 0},
			uint: uint64(0), int: int64(0), str: nil, bool: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			checkAccessor(t, "AsUint", tc.uint, func() (any, bool) { v, ok := tc.v.AsUint(); return v, ok })
			checkAccessor(t, "AsInt", tc.int, func() (any, bool) { v, ok := tc.v.AsInt(); return v, ok })
			checkAccessor(t, "AsString", tc.str, func() (any, bool) { v, ok := tc.v.AsString(); return v, ok })
			checkAccessor(t, "AsBool", tc.bool, func() (any, bool) { v, ok := tc.v.AsBool(); return v, ok })
		})
	}

	t.Run("AsInts broadcasts a scalar", func(t *testing.T) {
		v := gguf.Value{Type: gguf.ValueUint32, Uint: 8}
		got, ok := v.AsInts()
		if !ok || len(got) != 1 || got[0] != 8 {
			t.Fatalf("AsInts() = %v, %v; want [8], true", got, ok)
		}
	})

	t.Run("AsInts reads an array", func(t *testing.T) {
		f := loadFixture(t, "gemma3.gguf")
		got, ok := f.KV.Ints("gemma3.attention.head_count_kv")
		if !ok {
			t.Fatal("Ints refused a per-layer array")
		}
		if diff := cmp.Diff([]int64{4, 4, 4, 2, 2, 1}, got); diff != "" {
			t.Errorf("(-want +got):\n%s", diff)
		}
	})
}

func checkAccessor(t *testing.T, name string, want any, call func() (any, bool)) {
	t.Helper()
	got, ok := call()
	if want == nil {
		if ok {
			t.Errorf("%s succeeded with %v; want a refusal", name, got)
		}
		return
	}
	if !ok {
		t.Errorf("%s refused; want %v", name, want)
		return
	}
	if got != want {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}

// TestKVOrderAndLookup asserts the table keeps the file's own order, which is
// what makes `models.metadata_json` reproducible.
func TestKVOrderAndLookup(t *testing.T) {
	f := loadFixture(t, "llama.gguf")
	pairs := f.KV.All()
	if len(pairs) != f.KV.Len() {
		t.Fatalf("All() has %d pairs, Len() says %d", len(pairs), f.KV.Len())
	}
	if pairs[0].Key != gguf.KeyArchitecture {
		t.Errorf("first key = %q, want %q — the builder writes it first", pairs[0].Key, gguf.KeyArchitecture)
	}
	if !f.KV.Has("llama.block_count") {
		t.Error("Has(llama.block_count) = false")
	}
	if f.KV.Has("llama.attention.sliding_window") {
		t.Error("Has reports a key the fixture does not have; absence is load-bearing for SWA")
	}
	if _, ok := f.KV.Get("nope"); ok {
		t.Error("Get returned a missing key")
	}

	m := f.KV.Map()
	if m[gguf.KeyArchitecture] != "llama" {
		t.Errorf("Map()[architecture] = %v, want \"llama\"", m[gguf.KeyArchitecture])
	}
	if len(m) != f.KV.Len() {
		t.Errorf("Map() has %d entries, the table has %d", len(m), f.KV.Len())
	}
}

func parseBytes(t *testing.T, b []byte) *gguf.File {
	t.Helper()
	f, err := gguf.Parse(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return f
}
