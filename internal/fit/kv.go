package fit

// The KV cache, including sliding window (DESIGN section 8.3, D30).

// KVPad is the boundary llama.cpp pads the KV cache up to.
const KVPad = 256

// PadCtx is `kv_ctx = round_up(C, 256)`.
func PadCtx(c int) int {
	if c <= 0 {
		return 0
	}
	return (c + KVPad - 1) / KVPad * KVPad
}

// SWALayers is section 8.3's derivation, extracted because D30 exists to get
// exactly this right and because a rule this counter-intuitive deserves one
// implementation and one table test.
//
// Upstream's convention is that `sliding_window_pattern = N` means a repeating
// group of N layers in which the LAST uses full attention and the other N−1 use
// the window — so Gemma 3's pattern of 6 is "five local, one global" and
// `L_swa = L − floor(L/N)` when N divides L.
//
// Two defaults matter as much as the formula:
//
//   - `sliding_window` ABSENT → the model has no SWA at all, whatever the
//     pattern says. A pattern without a window is meaningless metadata.
//   - `sliding_window` present, `sliding_window_pattern` ABSENT → the period is
//     1, which under the rule above makes EVERY layer full-attention. This is
//     the opposite of the naive reading, and deliberately so: reading a default
//     of 1 as "all layers are sliding" would under-count KV by up to an order of
//     magnitude and let this calculator promise a fit that OOMs — the one
//     failure mode section 8.7 forbids outright. `ok` reports false for that
//     case so the caller can add its note and drop to `modeled`.
//
// ok is true when the window was actually applied.
func SWALayers(l int, window, pattern *int) (swa int, ok bool) {
	if l <= 0 || window == nil || *window <= 0 {
		return 0, false
	}
	p := 1
	applied := false
	if pattern != nil {
		p = *pattern
		applied = true
	}
	if p <= 1 {
		// Period 1: every layer's index satisfies (i+1) mod 1 == 0, so every
		// layer is a full-attention layer.
		return 0, applied && p > 1
	}
	for i := range l {
		if (i+1)%p != 0 {
			swa++
		}
	}
	return swa, true
}

// IsSWALayer reports whether layer i uses the sliding window, under the same
// rule SWALayers counts with.
func IsSWALayer(i int, window, pattern *int) bool {
	if window == nil || *window <= 0 || pattern == nil || *pattern <= 1 {
		return false
	}
	return (i+1)%*pattern != 0
}

// kvLayer is one layer's KV cost, split into the two terms the report shows
// separately.
type kvLayer struct {
	bytes uint64
	swa   bool
}

// kvPlan is the whole cache, per layer, so placement can charge each layer's KV
// to the device that holds it — "KV follows its layers" (section 8.3).
type kvPlan struct {
	layers []kvLayer
	// full and swa are the two reported totals over ALL layers, offloaded or
	// not: `kv_bytes` and `kv_swa_bytes` of section 3.9.
	full uint64
	swa  uint64
	// swaLayers is `n_layer_swa` in the report's inputs.
	swaLayers int
	// windowApplied reports whether the sliding window was used at all.
	windowApplied bool
}

func (p kvPlan) total() uint64 { return p.full + p.swa }

// planKV sizes the cache per layer.
//
//	kv_ctx     = round_up(C, 256)
//	per_tok(i) = n_head_kv[i] × (head_dim_k × bpe(type_k) + head_dim_v × bpe(type_v))
//	KV_full    = kv_ctx × Σ per_tok(i) over full-attention layers
//	KV_swa     = min(kv_ctx, W_swa + U) × Σ per_tok(i) over sliding-window layers
//
// The SWA term's `+ U` is not decoration: llama.cpp keeps the window plus one
// micro-batch of lookahead, so a cache sized at exactly W_swa is short by U
// tokens on every local layer.
func planKV(m ModelShape, f Flags, k, v CacheType) kvPlan {
	kvCtx := PadCtx(f.NCtx)
	swaSpan := kvCtx
	if m.SWAWindow != nil && *m.SWAWindow > 0 {
		if s := *m.SWAWindow + f.NUbatch; s < swaSpan {
			swaSpan = s
		}
	}

	_, applied := SWALayers(m.NLayer, m.SWAWindow, m.SWAPattern)
	p := kvPlan{layers: make([]kvLayer, m.NLayer), windowApplied: applied}
	for i := range m.NLayer {
		heads := m.HeadCountKV(i)
		perTok := rowBytes(heads*m.HeadDimK, k) + rowBytes(heads*m.HeadDimV, v)
		isSWA := applied && IsSWALayer(i, m.SWAWindow, m.SWAPattern)
		span := kvCtx
		if isSWA {
			span = swaSpan
			p.swaLayers++
		}
		b := perTok * uint64(span)
		p.layers[i] = kvLayer{bytes: b, swa: isSWA}
		if isSWA {
			p.swa += b
		} else {
			p.full += b
		}
	}
	return p
}
