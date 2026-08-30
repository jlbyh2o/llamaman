package gguf_test

import (
	"bytes"
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/jlbyh2o/llamaman/internal/gguf"
	"github.com/jlbyh2o/llamaman/internal/gguf/ggufbuild"
)

// TestTruncationByteByByte is TestRejectTruncation's finer sibling: every single
// prefix of a header that holds one key of every value type. Walking one byte at
// a time lands inside every field width the format has — a bool's single byte,
// an int16's two, a float64's eight, a string's length prefix and its bytes —
// so no read path can be short-circuited without a case landing on it.
func TestTruncationByteByByte(t *testing.T) {
	raw := fixtureBytes(t, "alltypes.gguf")
	for n := 0; n < len(raw); n++ {
		_, err := gguf.Parse(bytes.NewReader(raw[:n]), int64(n))
		if err == nil {
			t.Fatalf("a %d-byte prefix of a %d-byte header parsed", n, len(raw))
		}
		if n >= 4 && !errors.Is(err, gguf.ErrTruncated) {
			t.Fatalf("a %d-byte prefix failed with %v, want ErrTruncated", n, err)
		}
	}
}

// TestAbsurdStringLength covers a length prefix that cannot be a length: a
// string claiming more bytes than an int64 can count. It is refused on the
// number, before any allocation is attempted.
func TestAbsurdStringLength(t *testing.T) {
	huge := bytes.Repeat([]byte{0xff}, 8)
	data := ggufbuild.New("llama").Set("s", ggufbuild.Raw(gguf.ValueString, huge)).Header()
	_, err := gguf.Parse(bytes.NewReader(data), int64(len(data)))
	if !errors.Is(err, gguf.ErrTruncated) {
		t.Fatalf("error = %v, want ErrTruncated", err)
	}
}

// TestElidedNestedArray covers the skip path for an array of arrays past the
// retention limit: the elements are walked rather than kept, and a truncation
// inside them is still found.
func TestElidedNestedArray(t *testing.T) {
	outer := make([]ggufbuild.Val, 8)
	for i := range outer {
		outer[i] = ggufbuild.Strs("a", "bb", "ccc")
	}
	data := ggufbuild.New("llama").
		Set("nested", ggufbuild.Arr(gguf.ValueArray, outer...)).
		Header()

	f, err := gguf.Parse(bytes.NewReader(data), int64(len(data)), gguf.WithArrayLimit(4))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	v, _ := f.KV.Get("nested")
	if !v.Array.Elided || v.Array.Len != 8 {
		t.Errorf("array = %+v; want 8 elements, elided", v.Array)
	}
	if got := v.Any(); got != nil {
		t.Errorf("Any() on an elided array = %v, want nil so an empty list is never mistaken for real data", got)
	}

	// The same file with a limit that retains everything must give the elements
	// back, nested types intact.
	f2 := parseBytes(t, data)
	v2, _ := f2.KV.Get("nested")
	if v2.Array.Elided || len(v2.Array.Values) != 8 {
		t.Fatalf("array = %+v; want 8 retained elements", v2.Array)
	}
	inner, ok := v2.Array.Values[0].AsStrings()
	if !ok {
		t.Fatal("AsStrings refused a nested string array")
	}
	if diff := cmp.Diff([]string{"a", "bb", "ccc"}, inner); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}

	// Cut the file inside the elided region.
	_, err = gguf.Parse(bytes.NewReader(data[:len(data)-4]), int64(len(data)-4), gguf.WithArrayLimit(4))
	if !errors.Is(err, gguf.ErrTruncated) {
		t.Fatalf("a truncated nested array gave %v, want ErrTruncated", err)
	}
}

// TestFloatAndStringAccessors covers the two coercions the geometry keys do not
// happen to use but the metadata map does — rope scales are floats, tokenizer
// merges are string arrays.
func TestFloatAndStringAccessors(t *testing.T) {
	f := loadFixture(t, "alltypes.gguf")

	tests := []struct {
		key  string
		want float64
		ok   bool
	}{
		{"test.f32", 0.5, true},
		{"test.f64", -1.25, true},
		{"test.u32", 4000000000, true},
		{"test.i32", -2000000000, true},
		{"test.string", 0, false},
		{"test.missing", 0, false},
	}
	for _, tc := range tests {
		got, ok := f.KV.Float(tc.key)
		if ok != tc.ok || got != tc.want {
			t.Errorf("Float(%q) = %v, %v; want %v, %v", tc.key, got, ok, tc.want, tc.ok)
		}
	}

	strs, ok := f.KV.Strings("test.arr_string")
	if !ok {
		t.Fatal("Strings refused a string array")
	}
	if diff := cmp.Diff([]string{"a", "", "cc"}, strs); diff != "" {
		t.Errorf("Strings (-want +got):\n%s", diff)
	}
	if one, ok := f.KV.Strings("test.string"); !ok || len(one) != 1 || one[0] != "héllo, wörld" {
		t.Errorf("Strings on a scalar = %v, %v; want a one-element slice", one, ok)
	}
	if _, ok := f.KV.Strings("test.arr_u32"); ok {
		t.Error("Strings accepted an integer array")
	}
	if _, ok := f.KV.Strings("test.missing"); ok {
		t.Error("Strings accepted a missing key")
	}

	// The remaining KV helpers on a missing key, which every caller hits when a
	// producer omits an optional key.
	for _, probe := range []struct {
		name string
		call func() bool
	}{
		{"String", func() bool { _, ok := f.KV.String("nope"); return ok }},
		{"Uint", func() bool { _, ok := f.KV.Uint("nope"); return ok }},
		{"Int", func() bool { _, ok := f.KV.Int("nope"); return ok }},
		{"Bool", func() bool { _, ok := f.KV.Bool("nope"); return ok }},
		{"Ints", func() bool { _, ok := f.KV.Ints("nope"); return ok }},
	} {
		if probe.call() {
			t.Errorf("KV.%s answered for a missing key", probe.name)
		}
	}
}

// TestValueCountAndAny covers the two methods the models service uses to fill
// `metadata_json` and n_vocab.
func TestValueCountAndAny(t *testing.T) {
	tests := []struct {
		name  string
		v     gguf.Value
		count uint64
		any   any
	}{
		{"scalar", gguf.Value{Type: gguf.ValueUint32, Uint: 9}, 1, uint64(9)},
		{"signed scalar", gguf.Value{Type: gguf.ValueInt32, Int: -9}, 1, int64(-9)},
		{"float", gguf.Value{Type: gguf.ValueFloat64, Float: 1.5}, 1, 1.5},
		{"bool", gguf.Value{Type: gguf.ValueBool, Bool: true}, 1, true},
		{"string", gguf.Value{Type: gguf.ValueString, String: "x"}, 1, "x"},
		{"array with no backing", gguf.Value{Type: gguf.ValueArray}, 0, nil},
		{"unknown type", gguf.Value{Type: gguf.ValueType(99)}, 1, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.v.Count(); got != tc.count {
				t.Errorf("Count() = %d, want %d", got, tc.count)
			}
			if got := tc.v.Any(); got != tc.any {
				t.Errorf("Any() = %v, want %v", got, tc.any)
			}
		})
	}

	t.Run("array renders as a list", func(t *testing.T) {
		f := loadFixture(t, "gemma3.gguf")
		v, _ := f.KV.Get("gemma3.attention.head_count_kv")
		got, ok := v.Any().([]any)
		if !ok {
			t.Fatalf("Any() = %T, want []any", v.Any())
		}
		if diff := cmp.Diff([]any{uint64(4), uint64(4), uint64(4), uint64(2), uint64(2), uint64(1)}, got); diff != "" {
			t.Errorf("(-want +got):\n%s", diff)
		}
	})

	t.Run("AsInts refuses a list of strings", func(t *testing.T) {
		f := loadFixture(t, "alltypes.gguf")
		if _, ok := f.KV.Ints("test.arr_string"); ok {
			t.Error("Ints accepted a string array")
		}
	})

	t.Run("AsStrings refuses a scalar number", func(t *testing.T) {
		v := gguf.Value{Type: gguf.ValueUint32, Uint: 1}
		if _, ok := v.AsStrings(); ok {
			t.Error("AsStrings accepted a number")
		}
	})

	t.Run("AsFloat refuses an array", func(t *testing.T) {
		v := gguf.Value{Type: gguf.ValueArray}
		if _, ok := v.AsFloat(); ok {
			t.Error("AsFloat accepted an array")
		}
	})

	t.Run("AsInts on an int64 too large for the field", func(t *testing.T) {
		v := arrayValue(gguf.ValueUint64, gguf.Value{Type: gguf.ValueUint64, Uint: math.MaxUint64})
		if _, ok := v.AsInts(); ok {
			t.Error("AsInts accepted a uint64 above MaxInt64 rather than refusing")
		}
	})
}

// TestTypeStrings covers what an unknown type number renders as, which is what
// an error message about a future file will contain.
func TestTypeStrings(t *testing.T) {
	if got := gguf.ValueType(99).String(); got != "value_type(99)" {
		t.Errorf("ValueType(99) = %q", got)
	}
	if got := gguf.ValueUint8.String(); got != "uint8" {
		t.Errorf("ValueUint8 = %q", got)
	}
	if gguf.ValueType(99).Valid() {
		t.Error("ValueType(99) reports valid")
	}
	if got := gguf.GGMLType(4).String(); got != "ggml_type(4)" {
		t.Errorf("GGMLType(4) = %q", got)
	}
	if got := gguf.GGMLType(9999).String(); got != "ggml_type(9999)" {
		t.Errorf("GGMLType(9999) = %q", got)
	}
	for _, t2 := range []gguf.GGMLType{4, 9999} {
		if t2.BitsPerWeight() != 0 || t2.BlockSize() != 0 || t2.TypeSize() != 0 || t2.Quantized() {
			t.Errorf("unknown type %v reported geometry", t2)
		}
	}
}

// TestParseFileRoundTrip covers the local path end to end, including the one
// thing ParseFile adds over Parse: the size comes from the file rather than the
// caller.
func TestParseFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "model.gguf")
	if err := llamaFixture().WriteFile(path, true); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := gguf.ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if f.FileSize != st.Size() {
		t.Errorf("FileSize = %d, want the stat size %d", f.FileSize, st.Size())
	}
	if !f.Complete() {
		t.Error("a file written with its data reports incomplete")
	}
	if diff := cmp.Diff(loadFixture(t, "llama.gguf").Shape(), f.Shape()); diff != "" {
		t.Errorf("shape differs from the header-only fixture (-header +full):\n%s", diff)
	}

	// A file whose bytes are not GGUF at all reports the path, so the log line
	// names the file rather than just the failure.
	bad := filepath.Join(dir, "README.md")
	if err := os.WriteFile(bad, []byte("not a model"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = gguf.ParseFile(bad)
	if !errors.Is(err, gguf.ErrBadMagic) {
		t.Fatalf("error = %v, want ErrBadMagic", err)
	}
	if !bytes.Contains([]byte(err.Error()), []byte("README.md")) {
		t.Errorf("error %q does not name the file", err)
	}
}

// TestReadAheadDefaulting covers the option's guard: a nonsense window falls back
// to the default rather than making every read a request.
func TestReadAheadDefaulting(t *testing.T) {
	raw := fixtureBytes(t, "llama.gguf")
	for _, n := range []int{0, -1} {
		fr := &fakeRange{data: raw}
		rr := gguf.NewRemoteReaderAt(context.Background(), fr, int64(len(raw)), gguf.WithReadAhead(n))
		if _, err := rr.ReadAt(make([]byte, 8), 0); err != nil {
			t.Fatalf("ReadAt: %v", err)
		}
		if got := fr.ranges[0][1]; got != int64(len(raw)) {
			// The window is the default 1 MiB, clamped to the object.
			t.Errorf("WithReadAhead(%d) asked for %d bytes, want the object's %d", n, got, len(raw))
		}
	}
}

// TestTruncationInsideEveryValueType is the truncation case the whole-fixture
// walk cannot reach. A header with many keys is rejected early — its metadata
// count cannot fit in the bytes that remain — so a short prefix never gets as
// far as decoding a value. A file with ONE key does, which puts a cut inside the
// read of each width the decoder has.
func TestTruncationInsideEveryValueType(t *testing.T) {
	tests := []struct {
		name string
		val  ggufbuild.Val
	}{
		{"uint8", ggufbuild.U8(1)},
		{"int8", ggufbuild.I8(-1)},
		{"bool", ggufbuild.Bool(true)},
		{"uint16", ggufbuild.U16(1)},
		{"int16", ggufbuild.I16(-1)},
		{"uint32", ggufbuild.U32(1)},
		{"int32", ggufbuild.I32(-1)},
		{"float32", ggufbuild.F32(1)},
		{"uint64", ggufbuild.U64(1)},
		{"int64", ggufbuild.I64(-1)},
		{"float64", ggufbuild.F64(1)},
		{"string", ggufbuild.Str("abc")},
		{"array", ggufbuild.U32s(1, 2, 3)},
		{"nested array", ggufbuild.Arr(gguf.ValueArray, ggufbuild.U32s(1, 2))},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data := ggufbuild.New("").Set("k", tc.val).Header()
			for _, cut := range []int{1, 2} {
				if cut >= len(data) {
					continue
				}
				short := data[:len(data)-cut]
				_, err := gguf.Parse(bytes.NewReader(short), int64(len(short)))
				if !errors.Is(err, gguf.ErrTruncated) {
					t.Fatalf("cutting %d byte(s) gave %v, want ErrTruncated", cut, err)
				}
			}
			if _, err := gguf.Parse(bytes.NewReader(data), int64(len(data))); err != nil {
				t.Fatalf("the whole header failed: %v", err)
			}
		})
	}
}

// TestNumericCoercionRefusals covers the coercions that must fail: a float is
// not a count, and every geometry key in DESIGN section 8.1 is a count.
func TestNumericCoercionRefusals(t *testing.T) {
	for _, v := range []gguf.Value{
		{Type: gguf.ValueFloat32, Float: 1.5},
		{Type: gguf.ValueFloat64, Float: 1.5},
		{Type: gguf.ValueArray},
	} {
		if _, ok := v.AsUint(); ok {
			t.Errorf("AsUint accepted a %v", v.Type)
		}
		if _, ok := v.AsInt(); ok {
			t.Errorf("AsInt accepted a %v", v.Type)
		}
	}
	if got, ok := (gguf.Value{Type: gguf.ValueBool}).AsUint(); !ok || got != 0 {
		t.Errorf("AsUint(false) = %d, %v; want 0, true", got, ok)
	}
	if got, ok := (gguf.Value{Type: gguf.ValueBool}).AsInt(); !ok || got != 0 {
		t.Errorf("AsInt(false) = %d, %v; want 0, true", got, ok)
	}
	if _, ok := (gguf.Value{Type: gguf.ValueString}).AsFloat(); ok {
		t.Error("AsFloat accepted a string")
	}
}

// TestShapeIgnoresWronglyTypedKeys covers the metadata a producer got wrong.
// None of it may fail the parse or invent a number; each case leaves the field
// zero and says so in Notes, because a scan has to record the file either way.
func TestShapeIgnoresWronglyTypedKeys(t *testing.T) {
	t.Run("no architecture at all", func(t *testing.T) {
		data := ggufbuild.New("").Set("some.key", ggufbuild.U32(1)).Header()
		s := parseBytes(t, data).Shape()
		if s.Architecture != "" || s.BlockCount != 0 || s.HeadCountKV != nil {
			t.Errorf("shape = %+v; want everything empty", s)
		}
		if !containsNote(s.Notes, "general.architecture is missing") {
			t.Errorf("Notes = %q", s.Notes)
		}
	})

	t.Run("block_count is a string", func(t *testing.T) {
		b := shapeBuilderWithout("llama.block_count")
		b.Set("llama.block_count", ggufbuild.Str("four"))
		s := parseBytes(t, b.Header()).Shape()
		if s.BlockCount != 0 {
			t.Errorf("BlockCount = %d, want 0", s.BlockCount)
		}
		if !containsNote(s.Notes, "is not a number") {
			t.Errorf("Notes = %q", s.Notes)
		}
	})

	t.Run("head_count_kv is a string", func(t *testing.T) {
		b := shapeBuilder()
		b.Set("llama.attention.head_count", ggufbuild.U32(8))
		b.Set("llama.attention.head_count_kv", ggufbuild.Str("two"))
		s := parseBytes(t, b.Header()).Shape()
		if diff := cmp.Diff([]int{8, 8, 8, 8}, s.HeadCountKV); diff != "" {
			t.Errorf("HeadCountKV should fall back to head_count (-want +got):\n%s", diff)
		}
		if !containsNote(s.Notes, "head_count_kv is not a number") {
			t.Errorf("Notes = %q", s.Notes)
		}
	})

	t.Run("head_count_kv is an array of strings", func(t *testing.T) {
		b := shapeBuilder()
		b.Set("llama.attention.head_count", ggufbuild.U32(8))
		b.Set("llama.attention.head_count_kv", ggufbuild.Strs("a", "b"))
		s := parseBytes(t, b.Header()).Shape()
		if diff := cmp.Diff([]int{8, 8, 8, 8}, s.HeadCountKV); diff != "" {
			t.Errorf("HeadCountKV should fall back to head_count (-want +got):\n%s", diff)
		}
		if !containsNote(s.Notes, "could not use") {
			t.Errorf("Notes = %q", s.Notes)
		}
	})

	t.Run("head_count is an array of strings", func(t *testing.T) {
		b := shapeBuilder()
		b.Set("llama.attention.head_count", ggufbuild.Strs("a"))
		s := parseBytes(t, b.Header()).Shape()
		if s.HeadCount != 0 {
			t.Errorf("HeadCount = %d, want 0", s.HeadCount)
		}
		if !containsNote(s.Notes, "could not use") {
			t.Errorf("Notes = %q", s.Notes)
		}
	})

	t.Run("feed_forward_length is a per-layer array", func(t *testing.T) {
		b := shapeBuilderWithout("llama.feed_forward_length")
		b.Set("llama.feed_forward_length", ggufbuild.U32s(1024, 1024, 1024, 1024))
		s := parseBytes(t, b.Header()).Shape()
		if s.FeedForwardLength != 1024 {
			t.Errorf("FeedForwardLength = %d, want the first entry 1024", s.FeedForwardLength)
		}
		if !containsNote(s.Notes, "per-layer array") {
			t.Errorf("Notes = %q", s.Notes)
		}
	})
}

// shapeBuilderWithout is shapeBuilder minus one key, so a subtest can supply
// that key in a form of its own without writing a duplicate.
func shapeBuilderWithout(omit string) *ggufbuild.Builder {
	keys := []struct {
		key string
		val ggufbuild.Val
	}{
		{"llama.block_count", ggufbuild.U32(4)},
		{"llama.context_length", ggufbuild.U32(2048)},
		{"llama.embedding_length", ggufbuild.U32(512)},
		{"llama.feed_forward_length", ggufbuild.U32(1024)},
	}
	b := ggufbuild.New("llama")
	for _, k := range keys {
		if k.key == omit {
			continue
		}
		b.Set(k.key, k.val)
	}
	b.Tensor("token_embd.weight", gguf.TypeF32, 512, 8)
	return b
}
