package gguf_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/jlbyh2o/llamaman/internal/gguf"
	"github.com/jlbyh2o/llamaman/internal/gguf/ggufbuild"
)

// Rejection.
//
// The three failures a user can actually hit are told apart on purpose: a file
// that is not GGUF (a mislabeled download), a GGUF that was cut short (an
// interrupted one), and a GGUF whose header contradicts itself (corruption).
// Each case below asserts the sentinel, because the sentinel is what the models
// service will turn into a state — `corrupt` versus `incomplete` — and getting
// it wrong sends a user to delete a file that only needed resuming.

func TestRejectNonGGUF(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want error
	}{
		{"empty", nil, gguf.ErrTruncated},
		{"three bytes", []byte("GGU"), gguf.ErrTruncated},
		{"wrong magic", append([]byte("GGML"), make([]byte, 60)...), gguf.ErrBadMagic},
		{"a text file", []byte("this is a README, not a model, and it is long enough"), gguf.ErrBadMagic},
		{"byte-swapped magic", append([]byte("FUGG"), make([]byte, 60)...), gguf.ErrBadMagic},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := gguf.Parse(bytes.NewReader(tc.data), int64(len(tc.data)))
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestRejectTruncation walks a real fixture's length from 0 up: every prefix
// short of the whole header must be refused, and refused as truncation rather
// than as corruption, because the difference decides whether the answer is
// "resume the download" or "delete the file".
func TestRejectTruncation(t *testing.T) {
	raw := fixtureBytes(t, "llama.gguf")
	// Every offset is worth a case in principle; a stride keeps the test fast
	// while still landing inside every kind of field — a length prefix, a key's
	// bytes, a tensor's dimensions.
	for n := 0; n < len(raw); n += 7 {
		_, err := gguf.Parse(bytes.NewReader(raw[:n]), int64(n))
		if err == nil {
			t.Fatalf("a %d-byte prefix of a %d-byte header parsed", n, len(raw))
		}
		if n >= 4 && !errors.Is(err, gguf.ErrTruncated) {
			t.Fatalf("a %d-byte prefix failed with %v, want ErrTruncated", n, err)
		}
	}
	if _, err := gguf.Parse(bytes.NewReader(raw), int64(len(raw))); err != nil {
		t.Fatalf("the whole header failed: %v", err)
	}
}

// TestRejectSizeLie covers the other truncation: the bytes are there, but the
// caller says the object is shorter than it is. This is the remote path's
// failure mode — a Content-Length that disagrees with the body — and it must
// not be answered by reading past the length the caller vouched for.
func TestRejectSizeLie(t *testing.T) {
	raw := fixtureBytes(t, "llama.gguf")
	_, err := gguf.Parse(bytes.NewReader(raw), int64(len(raw))/2)
	if !errors.Is(err, gguf.ErrTruncated) {
		t.Fatalf("error = %v, want ErrTruncated", err)
	}
}

func TestRejectCorruptHeader(t *testing.T) {
	tests := []struct {
		name  string
		build func() []byte
		want  error
	}{
		{
			name: "duplicate metadata key",
			build: func() []byte {
				return ggufbuild.New("llama").
					Set("llama.block_count", ggufbuild.U32(4)).
					Set("llama.block_count", ggufbuild.U32(5)).
					Header()
			},
			want: gguf.ErrCorrupt,
		},
		{
			name: "duplicate tensor name",
			build: func() []byte {
				b := ggufbuild.New("llama")
				b.Tensor("blk.0.attn_norm.weight", gguf.TypeF32, 32)
				b.Tensor("blk.0.attn_norm.weight", gguf.TypeF32, 32)
				return b.Header()
			},
			want: gguf.ErrCorrupt,
		},
		{
			name: "tensor offset not where the layout puts it",
			build: func() []byte {
				b := ggufbuild.New("llama")
				b.Tensor("a.weight", gguf.TypeF32, 32)
				b.TensorAt("b.weight", gguf.TypeF32, 4096, 32)
				return b.Header()
			},
			want: gguf.ErrCorrupt,
		},
		{
			name: "first tensor does not start at zero",
			build: func() []byte {
				b := ggufbuild.New("llama")
				b.TensorAt("a.weight", gguf.TypeF32, 128, 32)
				return b.Header()
			},
			want: gguf.ErrCorrupt,
		},
		{
			name: "alignment is not a power of two",
			build: func() []byte {
				b := ggufbuild.New("llama").Alignment(48)
				b.Tensor("a.weight", gguf.TypeF32, 32)
				return b.Header()
			},
			want: gguf.ErrCorrupt,
		},
		{
			name: "alignment is zero",
			build: func() []byte {
				b := ggufbuild.New("llama").Alignment(0)
				b.Tensor("a.weight", gguf.TypeF32, 32)
				return b.Header()
			},
			want: gguf.ErrCorrupt,
		},
		{
			name: "too many tensor dimensions",
			build: func() []byte {
				b := ggufbuild.New("llama")
				b.Tensor("a.weight", gguf.TypeF32, 2, 2, 2, 2, 2)
				return b.Header()
			},
			want: gguf.ErrCorrupt,
		},
		{
			name: "row is not a whole number of quantization blocks",
			build: func() []byte {
				b := ggufbuild.New("llama")
				// 100 is not a multiple of q4_K's 256-element block, so this
				// tensor has no well-defined size at all.
				b.Tensor("a.weight", gguf.TypeQ4_K, 100)
				return b.Header()
			},
			want: gguf.ErrCorrupt,
		},
		{
			name: "dimension overflows int64",
			build: func() []byte {
				b := ggufbuild.New("llama")
				b.Tensor("a.weight", gguf.TypeF32, 1<<62, 1<<62)
				return b.Header()
			},
			want: gguf.ErrCorrupt,
		},
		{
			name: "unknown metadata value type",
			build: func() []byte {
				return ggufbuild.New("llama").
					Set("weird", ggufbuild.Raw(gguf.ValueType(99), []byte{1, 2, 3, 4})).
					Header()
			},
			want: gguf.ErrUnknownValueType,
		},
		{
			name: "unknown element type inside an array",
			build: func() []byte {
				return ggufbuild.New("llama").
					Set("weird", ggufbuild.Arr(gguf.ValueType(77), ggufbuild.U32(1))).
					Header()
			},
			want: gguf.ErrUnknownValueType,
		},
		{
			name: "removed tensor type",
			build: func() []byte {
				b := ggufbuild.New("llama")
				// 4 is GGML_TYPE_Q4_2, withdrawn years ago: no block geometry
				// exists for it, so no size can be computed.
				b.Tensor("a.weight", gguf.GGMLType(4), 32)
				return b.Header()
			},
			want: gguf.ErrUnknownTensorType,
		},
		{
			name: "tensor type from the future",
			build: func() []byte {
				b := ggufbuild.New("llama")
				b.Tensor("a.weight", gguf.GGMLType(500), 32)
				return b.Header()
			},
			want: gguf.ErrUnknownTensorType,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data := tc.build()
			_, err := gguf.Parse(bytes.NewReader(data), int64(len(data)))
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestRejectAbsurdCounts covers the counts in the header itself. A corrupt
// tensor or metadata count is the one field that, believed, turns into an
// allocation — so it is bounded by what could physically follow it before it is
// used for anything.
func TestRejectAbsurdCounts(t *testing.T) {
	tests := []struct {
		name   string
		offset int // where in the header the count sits
		value  uint64
	}{
		{"tensor count", 8, 1 << 40},
		{"metadata count", 16, 1 << 40},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw := bytes.Clone(fixtureBytes(t, "llama.gguf"))
			binary.LittleEndian.PutUint64(raw[tc.offset:], tc.value)
			_, err := gguf.Parse(bytes.NewReader(raw), int64(len(raw)))
			if !errors.Is(err, gguf.ErrTruncated) {
				t.Fatalf("error = %v, want ErrTruncated", err)
			}
		})
	}

	t.Run("array length", func(t *testing.T) {
		// An array claiming more elements than the file has bytes is refused
		// rather than allocated. The length is found by construction, not by
		// searching: the key is followed by its 4-byte value type, the array's
		// 4-byte element type, and then the 8-byte count.
		const key = "arraykey"
		raw := ggufbuild.New("llama").Set(key, ggufbuild.U32s(1, 2, 3)).Header()
		i := bytes.Index(raw, []byte(key))
		if i < 0 {
			t.Fatal("could not find the key in the built header")
		}
		at := i + len(key) + 4 + 4
		if got := binary.LittleEndian.Uint64(raw[at:]); got != 3 {
			t.Fatalf("expected the array count 3 at offset %d, found %d", at, got)
		}
		binary.LittleEndian.PutUint64(raw[at:], 1<<40)
		_, err := gguf.Parse(bytes.NewReader(raw), int64(len(raw)))
		if !errors.Is(err, gguf.ErrTruncated) {
			t.Fatalf("error = %v, want ErrTruncated", err)
		}
	})
}

// TestHeaderLimit covers the bound that exists because the file's own size is no
// bound at all in a 20 GB quant: a header running past the limit is refused
// distinctly, rather than read.
func TestHeaderLimit(t *testing.T) {
	raw := fixtureBytes(t, "llama.gguf")
	_, err := gguf.Parse(bytes.NewReader(raw), int64(len(raw)), gguf.WithMaxHeaderBytes(64))
	if !errors.Is(err, gguf.ErrHeaderTooLarge) {
		t.Fatalf("error = %v, want ErrHeaderTooLarge", err)
	}
	if _, err := gguf.Parse(bytes.NewReader(raw), int64(len(raw)), gguf.WithMaxHeaderBytes(0)); err != nil {
		t.Fatalf("an unbounded parse failed: %v", err)
	}
}

// TestRejectDeepNesting covers the one input shape that could recurse the
// decoder off its stack.
func TestRejectDeepNesting(t *testing.T) {
	v := ggufbuild.U32(1)
	for i := 0; i < 40; i++ {
		v = ggufbuild.Arr(gguf.ValueArray, v)
	}
	// The innermost element is a bare uint32 under an array-of-array tag, which
	// the parser will refuse either on depth or on the type mismatch; what must
	// not happen is unbounded recursion.
	data := ggufbuild.New("llama").Set("deep", v).Header()
	if _, err := gguf.Parse(bytes.NewReader(data), int64(len(data))); err == nil {
		t.Fatal("a 40-deep nested array parsed")
	}
}

// TestParseGuards covers the two arguments a caller can get wrong.
func TestParseGuards(t *testing.T) {
	if _, err := gguf.Parse(nil, 10); !errors.Is(err, gguf.ErrTruncated) {
		t.Errorf("Parse(nil) = %v, want ErrTruncated", err)
	}
	if _, err := gguf.Parse(bytes.NewReader([]byte("GGUF")), -1); !errors.Is(err, gguf.ErrTruncated) {
		t.Errorf("Parse(size -1) = %v, want ErrTruncated", err)
	}
}

// TestParseFileMissing keeps the local path's error honest: a missing file is an
// os error, not a format one.
func TestParseFileMissing(t *testing.T) {
	_, err := gguf.ParseFile(t.TempDir() + "/nope.gguf")
	if err == nil {
		t.Fatal("ParseFile on a missing path succeeded")
	}
	for _, sentinel := range []error{gguf.ErrBadMagic, gguf.ErrTruncated, gguf.ErrCorrupt} {
		if errors.Is(err, sentinel) {
			t.Errorf("a missing file reported %v", sentinel)
		}
	}
}
