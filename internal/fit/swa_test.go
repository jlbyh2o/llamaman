package fit

import "testing"

// SWALayers against the upstream convention (DESIGN section 8.3, D30, and the
// case section 15 names): pattern 6 over 26 layers, a window with no pattern
// (all layers full, note emitted), and a pattern with no window (SWA off).
//
// The middle case is the one this test exists for. Reading the schema default of
// 1 as "all layers are sliding" would under-count KV by up to an order of
// magnitude and let the calculator promise a fit that OOMs — the single failure
// mode section 8.7 forbids outright.
func TestSWALayers(t *testing.T) {
	cases := []struct {
		name    string
		layers  int
		window  *int
		pattern *int
		wantSWA int
		wantOK  bool
	}{
		{
			name:   "gemma 3: pattern 6 over 26 layers is five local, one global",
			layers: 26, window: intp(1024), pattern: intp(6),
			// L_swa = L − floor(L/N): the layers whose 1-based index is a
			// multiple of 6 are the global ones — 6, 12, 18, 24 — so four are
			// full and twenty-two slide.
			wantSWA: 22, wantOK: true,
		},
		{
			name:   "a pattern that divides the layer count exactly",
			layers: 24, window: intp(4096), pattern: intp(6),
			wantSWA: 20, wantOK: true,
		},
		{
			name:   "a window with NO pattern is read as period 1: every layer is full",
			layers: 26, window: intp(1024), pattern: nil,
			wantSWA: 0, wantOK: false,
		},
		{
			name:   "an explicit pattern of 1 means the same thing",
			layers: 26, window: intp(1024), pattern: intp(1),
			wantSWA: 0, wantOK: false,
		},
		{
			name:   "a pattern with NO window is meaningless metadata: SWA is off",
			layers: 26, window: nil, pattern: intp(6),
			wantSWA: 0, wantOK: false,
		},
		{
			name:   "a zero-width window is not a window",
			layers: 26, window: intp(0), pattern: intp(6),
			wantSWA: 0, wantOK: false,
		},
		{
			name:   "no layers, nothing to count",
			layers: 0, window: intp(1024), pattern: intp(6),
			wantSWA: 0, wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := SWALayers(tc.layers, tc.window, tc.pattern)
			if got != tc.wantSWA || ok != tc.wantOK {
				t.Fatalf("SWALayers(%d, %v, %v) = %d, %v; want %d, %v",
					tc.layers, deref(tc.window), deref(tc.pattern), got, ok, tc.wantSWA, tc.wantOK)
			}
			// IsSWALayer must agree with the count, layer by layer: they are two
			// readings of one rule and a disagreement would size the cache with
			// one and report it with the other.
			var counted int
			for i := range tc.layers {
				if IsSWALayer(i, tc.window, tc.pattern) {
					counted++
				}
			}
			if counted != tc.wantSWA {
				t.Errorf("IsSWALayer counted %d sliding layers, SWALayers said %d",
					counted, tc.wantSWA)
			}
		})
	}
}

// TestPadCtx: llama.cpp pads the KV cache up to 256 tokens, so a context of
// 4097 costs the same as one of 4352.
func TestPadCtx(t *testing.T) {
	cases := []struct{ in, want int }{
		{0, 0}, {1, 256}, {256, 256}, {257, 512}, {4096, 4096}, {4097, 4352}, {8192, 8192},
	}
	for _, tc := range cases {
		if got := PadCtx(tc.in); got != tc.want {
			t.Errorf("PadCtx(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func intp(v int) *int { return &v }

func deref(p *int) any {
	if p == nil {
		return "nil"
	}
	return *p
}
