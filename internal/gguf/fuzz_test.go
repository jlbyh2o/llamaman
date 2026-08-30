package gguf_test

import (
	"bytes"
	"os"
	"testing"

	"github.com/jlbyh2o/llamaman/internal/gguf"
)

// FuzzParseHeader is the guard DESIGN section 8.5 asks for.
//
// The parser's input is attacker-adjacent by construction: a GGUF header comes
// from a stranger's repository on the hub, and section 8.5's remote peek reads
// one before anything has been verified or even downloaded. The property is the
// only one worth asserting at this layer — no panic, no unbounded allocation, no
// hang — and it is asserted for EVERY input, including the ones that parse: a
// header that survives must also be self-consistent, because a caller that got a
// *File back will go on to read sizes off it.
func FuzzParseHeader(f *testing.F) {
	for _, fx := range fixtures() {
		if b, err := os.ReadFile(fixturePath(fx.name)); err == nil {
			f.Add(b)
		}
	}
	f.Add([]byte(nil))
	f.Add([]byte("GGUF"))
	f.Add([]byte("GGUF\x03\x00\x00\x00"))
	f.Add(append([]byte("GGUF\x03\x00\x00\x00"), bytes.Repeat([]byte{0xff}, 16)...))

	f.Fuzz(func(t *testing.T, data []byte) {
		// The bound is what makes this test terminate on a header claiming a
		// gigabyte-long string, rather than allocating one.
		file, err := gguf.Parse(bytes.NewReader(data), int64(len(data)), gguf.WithMaxHeaderBytes(1<<20))
		if err != nil {
			return
		}

		if file.Version != 2 && file.Version != 3 {
			t.Fatalf("accepted version %d", file.Version)
		}
		if file.Alignment == 0 || file.Alignment&(file.Alignment-1) != 0 {
			t.Fatalf("accepted alignment %d", file.Alignment)
		}
		if uint64(len(file.Tensors)) != file.TensorCount {
			t.Fatalf("TensorCount %d but %d tensors", file.TensorCount, len(file.Tensors))
		}
		if file.HeaderBytes > int64(len(data)) {
			t.Fatalf("HeaderBytes %d exceeds the %d bytes given", file.HeaderBytes, len(data))
		}
		if file.DataOffset < file.HeaderBytes || file.DataSize < 0 {
			t.Fatalf("data region [%d,+%d) is behind the %d-byte header", file.DataOffset, file.DataSize, file.HeaderBytes)
		}

		// Every accessor a caller reaches for must survive whatever got through.
		var total uint64
		for _, ti := range file.Tensors {
			if !ti.Type.Valid() {
				t.Fatalf("tensor %q kept type %v", ti.Name, ti.Type)
			}
			total += ti.Bytes()
			ti.LayerIndex()
		}
		sizes := file.Sizes()
		if sizes.Total != total {
			t.Fatalf("Sizes().Total = %d, tensors sum to %d", sizes.Total, total)
		}
		sizes.DominantType()
		file.Shape()
		file.KV.Map()
	})
}
