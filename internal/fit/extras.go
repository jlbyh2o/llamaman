package fit

import "fmt"

// The draft model and the multimodal projector.
//
// Neither appears in section 8's formulas, and both are real VRAM the moment a
// user sets `draft_model_id` or pairs an mmproj: a 0.5B draft at f16 with its
// own KV cache is well over a gigabyte, and a projector is a few hundred
// megabytes. An estimate that ignored them would promise a fit that OOMs on
// exactly the configurations this product makes easy to build, so they are
// charged here — to the MAIN device, because llama.cpp loads both onto one
// device rather than splitting them across the tensor split.

// extraCharge is the draft plus projector bytes, and the notes that explain
// them.
func extraCharge(m ModelShape, f Flags, k, v CacheType) (uint64, []string) {
	var total uint64
	var notes []string

	if m.MmprojBytes > 0 {
		total += m.MmprojBytes
		notes = append(notes, fmt.Sprintf(
			"the paired multimodal projector adds %s, charged to the main device",
			humanBytes(m.MmprojBytes)))
	}

	d := m.Draft
	if d == nil {
		return total, notes
	}

	layer, other, _ := d.Layers()
	steps := d.OffloadSteps()
	n := steps
	pinned := false
	if f.Draft.NGpuLayers != nil {
		n = *f.Draft.NGpuLayers
		pinned = true
		if n > steps {
			n = steps
		}
		if n < 0 {
			n = 0
		}
	}

	var weights uint64
	for i := 0; i < len(layer) && i < n; i++ {
		weights += layer[i]
	}
	if n > len(layer) {
		weights += other
	}

	df := f
	df.NCtx = f.Draft.CtxSize
	if df.NCtx <= 0 {
		// `-cd` unset: llama.cpp falls back to the primary model's context,
		// which is also the larger of the two readings.
		df.NCtx = f.NCtx
	}
	dkv := planKV(*d, df, k, v)
	var kvBytes uint64
	for i := 0; i < len(dkv.layers) && i < n; i++ {
		kvBytes += dkv.layers[i].bytes
	}

	total += weights + kvBytes
	if !pinned {
		notes = append(notes, fmt.Sprintf(
			"the draft model has no -ngld, so all of it is charged to VRAM: %s of weights "+
				"and %s of cache at a draft context of %d",
			humanBytes(weights), humanBytes(kvBytes), df.NCtx))
	} else {
		notes = append(notes, fmt.Sprintf(
			"the draft model adds %s of weights and %s of cache at -ngld %d",
			humanBytes(weights), humanBytes(kvBytes), n))
	}
	return total, notes
}
