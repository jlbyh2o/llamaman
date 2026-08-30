package fit

import (
	"math"
	"testing"
)

// The `-ctk`/`-ctv` table of DESIGN section 8.3, pinned with hand-written
// literals rather than derived from the map it checks — a table that tested
// itself would agree with any typo in it.
func TestCacheTypeTable(t *testing.T) {
	cases := []struct {
		name       string
		block      int
		typeSize   int
		bytesPerEl float64
	}{
		{CacheTypeF32, 1, 4, 4.0},
		{CacheTypeF16, 1, 2, 2.0},
		{CacheTypeBF16, 1, 2, 2.0},
		{CacheTypeQ8_0, 32, 34, 1.0625},
		{CacheTypeQ5_1, 32, 24, 0.75},
		{CacheTypeQ5_0, 32, 22, 0.6875},
		{CacheTypeQ4_1, 32, 20, 0.625},
		{CacheTypeQ4_0, 32, 18, 0.5625},
		{CacheTypeIQ4_NL, 32, 18, 0.5625},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, ok := LookupCacheType(tc.name)
			if !ok {
				t.Fatalf("%s is not in the table", tc.name)
			}
			if c.BlockSize != tc.block || c.TypeSize != tc.typeSize {
				t.Errorf("%s = %d bytes per %d elements, want %d per %d",
					tc.name, c.TypeSize, c.BlockSize, tc.typeSize, tc.block)
			}
			if math.Abs(c.BytesPerElement()-tc.bytesPerEl) > 1e-9 {
				t.Errorf("%s bytes/elem = %v, want %v", tc.name, c.BytesPerElement(), tc.bytesPerEl)
			}
		})
	}
	if len(CacheTypeNames()) != len(cases) {
		t.Errorf("CacheTypeNames lists %d rows, the table has %d", len(CacheTypeNames()), len(cases))
	}
}

// TestLookupCacheTypeDefaultsAndUnknowns: an empty value is f16 (llama.cpp's own
// default) and an unrecognized one reports false rather than being silently
// sized as f16 — a build that grew a new cache type must be visible, not
// guessed at.
func TestLookupCacheTypeDefaultsAndUnknowns(t *testing.T) {
	if c, ok := LookupCacheType(""); !ok || c.Name != CacheTypeF16 {
		t.Errorf("empty = %+v, %v; want f16, true", c, ok)
	}
	if c, ok := LookupCacheType("Q8_0"); !ok || c.Name != CacheTypeQ8_0 {
		t.Errorf("case should not matter: %+v, %v", c, ok)
	}
	if _, ok := LookupCacheType("q3_k_xl"); ok {
		t.Error("an unknown cache type must report false")
	}
}

// TestRowBytes: the fractional types are why this is integer arithmetic over
// whole blocks. A 128-element row of q8_0 is four blocks of 34 bytes — 136, not
// 128 and not 256.
func TestRowBytes(t *testing.T) {
	q8, _ := LookupCacheType(CacheTypeQ8_0)
	f16, _ := LookupCacheType(CacheTypeF16)
	cases := []struct {
		name  string
		elems int
		typ   CacheType
		want  uint64
	}{
		{"f16 is two bytes an element", 128, f16, 256},
		{"q8_0 rounds up to whole blocks", 128, q8, 136},
		{"a partial q8_0 block still costs a whole one", 33, q8, 68},
		{"zero elements cost nothing", 0, q8, 0},
	}
	for _, tc := range cases {
		if got := rowBytes(tc.elems, tc.typ); got != tc.want {
			t.Errorf("%s: rowBytes(%d) = %d, want %d", tc.name, tc.elems, got, tc.want)
		}
	}
}
