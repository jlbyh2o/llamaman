package fit

// Per-GPU placement (DESIGN section 8.4).
//
// The unit of this calculation is `assigned(g, n)` — what ONE device is charged
// when n of the L+1 offload steps are on the GPU. There is deliberately no
// scalar `Required`: comparing one total against Σ free VRAM says "fits" on a
// 23 GB + 4 GB pair for a model that cannot be placed on either card, because
// llama.cpp does not pool VRAM across devices. Layers land on a specific GPU and
// either fit there or fail.

// placement is one evaluation of the split: what each device holds, and what is
// left for system RAM.
type placement struct {
	// weights and kv are per device, indexed as Devices is.
	weights []uint64
	kv      []uint64
	// extra is the draft model and the multimodal projector, which llama.cpp
	// loads onto the main device rather than spreading (section 8.2's note).
	extra []uint64

	ramWeights uint64
	ramKV      uint64
	ramExtra   uint64
}

func (p placement) deviceBytes(i int) uint64 {
	return p.weights[i] + p.kv[i] + p.extra[i]
}

func (p placement) spill() uint64 { return p.ramWeights + p.ramKV + p.ramExtra }

// splitWeights normalizes `tensor_split` over the participating devices. An
// empty or all-zero split is even, which is llama.cpp's own default.
func splitWeights(n int, ts []float64) []float64 {
	out := make([]float64, n)
	var sum float64
	for i := range out {
		if i < len(ts) && ts[i] > 0 {
			out[i] = ts[i]
		}
		sum += out[i]
	}
	if sum <= 0 {
		for i := range out {
			out[i] = 1 / float64(n)
		}
		return out
	}
	for i := range out {
		out[i] /= sum
	}
	return out
}

// cumulative turns a normalized split into its running sum, which is what
// llama.cpp's layer assignment binary-searches.
func cumulative(w []float64) []float64 {
	out := make([]float64, len(w))
	var run float64
	for i, v := range w {
		run += v
		out[i] = run
	}
	if len(out) > 0 {
		// Guard the last boundary against floating-point drift, so a layer at
		// fraction 0.999999 never falls off the end of the device list.
		out[len(out)-1] = 1
	}
	return out
}

// layerDevice picks the device holding offloaded layer i of nOffload, under
// `split_mode=layer`: the first device whose cumulative share exceeds the
// layer's position. It is llama.cpp's own rule, which is why an uneven
// `tensor_split` moves whole layers rather than fractions of them.
func layerDevice(i, nOffload int, cum []float64) int {
	if len(cum) == 0 {
		return 0
	}
	if nOffload <= 0 {
		return 0
	}
	frac := float64(i) / float64(nOffload)
	for g, c := range cum {
		if frac < c {
			return g
		}
	}
	return len(cum) - 1
}

// share splits total by a normalized weight, in integers, with the remainder
// riding on the last device so the parts sum exactly to the whole.
func share(total uint64, w []float64) []uint64 {
	out := make([]uint64, len(w))
	if len(w) == 0 {
		return out
	}
	var assigned uint64
	for i := 0; i < len(w)-1; i++ {
		out[i] = uint64(float64(total) * w[i])
		assigned += out[i]
	}
	out[len(w)-1] = total - assigned
	return out
}

// place charges n offload steps across the participating devices.
//
// `split_mode=layer` (the default) deals whole layers out by `tensor_split`, and
// KV FOLLOWS ITS LAYERS — only offloaded layers hold VRAM KV, the rest is
// charged to RAM. `split_mode=row` splits each tensor's rows by `tensor_split`
// and charges the whole KV cache to `main_gpu`, which is what upstream does.
// `split_mode=none` uses one device.
func place(devs []Device, f Flags, layerBytes []uint64, other uint64, kv kvPlan, n int) placement {
	nd := len(devs)
	p := placement{
		weights: make([]uint64, nd),
		kv:      make([]uint64, nd),
		extra:   make([]uint64, nd),
	}
	nLayer := len(layerBytes)
	offloadLayers := min(n, nLayer)

	if nd == 0 {
		// No participating device: everything is RAM, and no offload is
		// possible whatever n says.
		for i := range nLayer {
			p.ramWeights += layerBytes[i]
			p.ramKV += kv.layerBytes(i)
		}
		p.ramWeights += other
		return p
	}

	main := f.MainGPU
	if main >= nd {
		main = nd - 1
	}

	switch f.SplitMode {
	case SplitRow:
		w := splitWeights(nd, f.TensorSplit)
		for i := range nLayer {
			if i < offloadLayers {
				for g, b := range share(layerBytes[i], w) {
					p.weights[g] += b
				}
				// Row split keeps the whole cache on the main device.
				p.kv[main] += kv.layerBytes(i)
				continue
			}
			p.ramWeights += layerBytes[i]
			p.ramKV += kv.layerBytes(i)
		}
		if n > nLayer {
			for g, b := range share(other, w) {
				p.weights[g] += b
			}
		} else {
			p.ramWeights += other
		}

	case SplitNone:
		for i := range nLayer {
			if i < offloadLayers {
				p.weights[main] += layerBytes[i]
				p.kv[main] += kv.layerBytes(i)
				continue
			}
			p.ramWeights += layerBytes[i]
			p.ramKV += kv.layerBytes(i)
		}
		if n > nLayer {
			p.weights[main] += other
		} else {
			p.ramWeights += other
		}

	default: // SplitLayer
		cum := cumulative(splitWeights(nd, f.TensorSplit))
		last := 0
		for i := range nLayer {
			if i < offloadLayers {
				g := layerDevice(i, offloadLayers, cum)
				last = g
				p.weights[g] += layerBytes[i]
				p.kv[g] += kv.layerBytes(i)
				continue
			}
			p.ramWeights += layerBytes[i]
			p.ramKV += kv.layerBytes(i)
		}
		if n > nLayer {
			// The output head and the token embedding follow the last offloaded
			// layer, which is where upstream puts them under a layer split.
			p.weights[last] += other
		} else {
			p.ramWeights += other
		}
	}

	return p
}

// layerBytes is kvPlan's accessor, tolerant of a plan shorter than the weight
// bucketing (a tensor index whose `blk.N.` numbering has gaps).
func (p kvPlan) layerBytes(i int) uint64 {
	if i < 0 || i >= len(p.layers) {
		return 0
	}
	return p.layers[i].bytes
}
