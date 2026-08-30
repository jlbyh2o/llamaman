package fit

import "strings"

// The `-ctk`/`-ctv` bytes-per-element table of DESIGN section 8.3 (D30).
//
// The point of the table is that block-quantized cache types are FRACTIONAL:
// q8_0 is 34 bytes per 32 elements, which is 1.0625 bytes each. A calculator
// that rounded that to 1 would under-size a 32k KV cache by 6%, and one that
// rounded it to 2 would refuse loads that fit. The arithmetic below is done in
// integers over whole blocks so neither happens.

// Cache type names, as llama.cpp spells them on the command line.
const (
	CacheTypeF32    = "f32"
	CacheTypeF16    = "f16"
	CacheTypeBF16   = "bf16"
	CacheTypeQ8_0   = "q8_0"
	CacheTypeQ5_1   = "q5_1"
	CacheTypeQ5_0   = "q5_0"
	CacheTypeQ4_1   = "q4_1"
	CacheTypeQ4_0   = "q4_0"
	CacheTypeIQ4_NL = "iq4_nl"
)

// CacheType is one row of section 8.3's table: how many bytes a block of
// BlockSize elements occupies.
type CacheType struct {
	Name      string
	BlockSize int
	TypeSize  int
}

// BytesPerElement is the table's last column, for display and for tests that
// want to check the table itself rather than a derived size.
func (c CacheType) BytesPerElement() float64 {
	if c.BlockSize == 0 {
		return 0
	}
	return float64(c.TypeSize) / float64(c.BlockSize)
}

// Quantized reports whether this type is block-quantized, which is what makes a
// V-cache require flash attention on most builds (section 8.7's note).
func (c CacheType) Quantized() bool { return c.BlockSize > 1 }

// cacheTypes is section 8.3's table verbatim. It is deliberately CLOSED: an
// unknown `-ctk` value is not silently sized as f16, because a build that grew a
// new cache type would then be mis-estimated with no sign that anything was
// guessed.
var cacheTypes = map[string]CacheType{
	CacheTypeF32:    {Name: CacheTypeF32, BlockSize: 1, TypeSize: 4},
	CacheTypeF16:    {Name: CacheTypeF16, BlockSize: 1, TypeSize: 2},
	CacheTypeBF16:   {Name: CacheTypeBF16, BlockSize: 1, TypeSize: 2},
	CacheTypeQ8_0:   {Name: CacheTypeQ8_0, BlockSize: 32, TypeSize: 34},
	CacheTypeQ5_1:   {Name: CacheTypeQ5_1, BlockSize: 32, TypeSize: 24},
	CacheTypeQ5_0:   {Name: CacheTypeQ5_0, BlockSize: 32, TypeSize: 22},
	CacheTypeQ4_1:   {Name: CacheTypeQ4_1, BlockSize: 32, TypeSize: 20},
	CacheTypeQ4_0:   {Name: CacheTypeQ4_0, BlockSize: 32, TypeSize: 18},
	CacheTypeIQ4_NL: {Name: CacheTypeIQ4_NL, BlockSize: 32, TypeSize: 18},
}

// LookupCacheType resolves a `-ctk`/`-ctv` value. An empty name is f16,
// llama.cpp's own default; an unrecognized one reports false and the caller adds
// a note and falls back to f16 rather than pretending to know.
func LookupCacheType(name string) (CacheType, bool) {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		return cacheTypes[CacheTypeF16], true
	}
	c, ok := cacheTypes[n]
	return c, ok
}

// CacheTypeNames lists the table's rows in the order section 8.3 prints them —
// widest first — so a UI picker and this table cannot drift.
func CacheTypeNames() []string {
	return []string{
		CacheTypeF32, CacheTypeF16, CacheTypeBF16, CacheTypeQ8_0,
		CacheTypeQ5_1, CacheTypeQ5_0, CacheTypeQ4_1, CacheTypeQ4_0, CacheTypeIQ4_NL,
	}
}

// rowBytes is the size of a KV row of n elements in type c: whole blocks,
// rounded up.
//
// Rounding up rather than truncating matters for exactly one reason and it is
// the golden rule of section 8.7: a partial block still occupies a whole block
// in ggml, and a calculator that truncated would report a cache smaller than the
// one allocated. Every realistic head dimension is a multiple of 32, so the
// rounding is a no-op on real models and a safety margin on the ones it is not.
func rowBytes(n int, c CacheType) uint64 {
	if n <= 0 || c.BlockSize <= 0 {
		return 0
	}
	blocks := (n + c.BlockSize - 1) / c.BlockSize
	return uint64(blocks) * uint64(c.TypeSize)
}
