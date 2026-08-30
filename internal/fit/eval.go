package fit

import "fmt"

// The verdict machinery of DESIGN section 8.7.
//
//	placeable(n) ⟺ ∀ g ∈ selected GPUs : assigned(g, n) ≤ free(g) − reserve(g)
//	fits         ⟺ placeable(L+1)
//	partial      ⟺ ∃ n ∈ [0, L] with placeable(n), and the spill fits RAM
//	wont_run     otherwise
//
// The test is per-GPU (`∀`), never a sum. `required_vram_bytes` is reported as
// the sum and is clearly labeled a total, so the UI can say "18.4 GB across 2
// GPUs" without anything computing a verdict from it.

// evaluator holds everything one placement needs, so `placeable(n)` is a method
// on a value rather than a function of eleven arguments — which is what lets the
// descending scan, the context search and the recommendation all evaluate the
// SAME function instead of three that could disagree.
type evaluator struct {
	model   ModelShape
	flags   Flags
	devices []Device

	layer []uint64
	other uint64
	kv    kvPlan

	k, v        CacheType
	calibration Calibration

	compute  uint64
	overhead uint64
	margin   uint64
	reserve  uint64
	// extra is the draft model and the multimodal projector: real bytes on the
	// main device whenever anything is offloaded at all, and RAM otherwise.
	extra uint64

	host Host
}

// evaluation is one `assigned(g, n)` pass.
type evaluation struct {
	perGPU     []DeviceReport
	required   uint64
	gpuWeights uint64
	gpuKV      uint64
	spill      uint64
	placeable  bool
	ramOK      bool
	// worst is the index of the device that is furthest over, or -1.
	worst int
}

// at charges n offload steps and tests every device.
func (e evaluator) at(n int) evaluation {
	p := place(e.devices, e.flags, e.layer, e.other, e.kv, n)

	main := e.flags.MainGPU
	if main >= len(e.devices) {
		main = len(e.devices) - 1
	}
	if e.extra > 0 {
		if n > 0 && len(e.devices) > 0 {
			p.extra[main] += e.extra
		} else {
			p.ramExtra += e.extra
		}
	}

	// `-ngl 0` puts nothing on any device, and llama.cpp allocates no CUDA
	// context, no compute buffer and no margin for a backend it never uses. The
	// three flat charges are therefore charged only when something is actually
	// offloaded — without this branch a 1 GiB card would make a CPU-only run
	// impossible, because OH_gpu plus the margin alone exceed the card.
	onGPU := n > 0

	out := evaluation{placeable: true, worst: -1}
	var worstShort uint64
	for i, d := range e.devices {
		r := DeviceReport{
			Index:        d.Index,
			UUID:         d.UUID,
			Name:         d.Name,
			WeightsBytes: p.weights[i],
			KVBytes:      p.kv[i],
			ExtraBytes:   p.extra[i],
			ReserveBytes: e.reserve,
		}
		if onGPU {
			r.OverheadBytes, r.MarginBytes = e.overhead, e.margin
			r.AssignedBytes = p.deviceBytes(i) + e.compute + e.overhead + e.margin
		}
		switch {
		case !onGPU:
			// Nothing is asked of this device, so nothing can be refused —
			// including by a device whose memory could not be read.
			r.OK = true
			if d.Known {
				free := d.FreeBytes
				r.FreeBytes = &free
				total := d.TotalBytes
				r.TotalBytes = &total
			}
		case d.Known:
			free := d.FreeBytes
			r.FreeBytes = &free
			total := d.TotalBytes
			r.TotalBytes = &total
			budget := uint64(0)
			if free > e.reserve {
				budget = free - e.reserve
			}
			r.OK = r.AssignedBytes <= budget
			if !r.OK {
				r.ShortByBytes = r.AssignedBytes - budget
			}
		}
		// A device whose memory could not be read is never OK: D16 forbids
		// treating an unknown as a zero, and it equally forbids treating it as
		// unlimited. The report's VRAMUnknown flag is what tells the UI to say
		// "unknown" rather than "won't run".
		if !r.OK {
			out.placeable = false
			if r.ShortByBytes >= worstShort {
				worstShort, out.worst = r.ShortByBytes, i
			}
		}
		out.required += r.AssignedBytes
		out.gpuWeights += p.weights[i]
		out.gpuKV += p.kv[i]
		out.perGPU = append(out.perGPU, r)
	}

	out.spill = p.spill()
	out.ramOK = e.ramFits(out.spill)
	return out
}

// ramFits is section 8.7's `W_ram + KV_ram ≤ 0.9 × free system RAM`.
//
// Unmeasured RAM is allowed through with a note rather than failing closed, and
// that asymmetry with VRAM is deliberate: /proc/meminfo is readable on every
// Linux this runs on, so an unknown here means the caller did not ask, whereas
// an unknown GPU means the driver refused to answer.
func (e evaluator) ramFits(spill uint64) bool {
	if spill == 0 {
		return true
	}
	if !e.host.RAMKnown {
		return true
	}
	return float64(spill) <= RAMHeadroom*float64(e.host.RAMFreeBytes)
}

// maxPlaceable is the largest n on [0, L+1] with placeable(n) — the descending
// scan section 8.7 permits. It is what `max_n_gpu_layers` reports and what
// `-ngl auto` resolves to (D51).
func (e evaluator) maxPlaceable() int {
	if len(e.devices) == 0 {
		return 0
	}
	for n := e.model.OffloadSteps(); n >= 0; n-- {
		if e.at(n).placeable {
			return n
		}
	}
	return 0
}

// forCtx returns the same evaluator with a different context, rebuilding the two
// terms that depend on it: the KV cache and — when flash attention is off — the
// attention compute buffer.
func (e evaluator) forCtx(c int) evaluator {
	out := e
	out.flags.NCtx = c
	out.kv = planKV(e.model, out.flags, e.k, e.v)
	out.compute = computeBuffers(e.model, out.flags, e.calibration).total()
	return out
}

// forCache returns the same evaluator with different cache types and flash
// attention, which is the second rung of section 8.7's recommendation ladder.
func (e evaluator) forCache(k, v CacheType, fa bool) evaluator {
	out := e
	out.k, out.v = k, v
	out.flags.TypeK, out.flags.TypeV = k.Name, v.Name
	if fa {
		out.flags.FlashAttn = FlashAttnOn
	} else {
		out.flags.FlashAttn = FlashAttnOff
	}
	out.kv = planKV(e.model, out.flags, k, v)
	out.compute = computeBuffers(e.model, out.flags, e.calibration).total()
	return out
}

// maxCtxAtFullOffload is the largest context, rounded down to 256, that fits
// with everything offloaded. It is a binary search over the same pure function
// — memory is monotonic in the context, so the boundary is well defined — and it
// costs about a dozen evaluations rather than the thousands a linear walk over
// 256-token steps up to 1M would.
func (e evaluator) maxCtxAtFullOffload() int {
	if len(e.devices) == 0 {
		return 0
	}
	steps := e.model.OffloadSteps()
	hi := e.model.NCtxTrain
	if hi < e.flags.NCtx {
		hi = e.flags.NCtx
	}
	if hi < KVPad {
		hi = KVPad
	}
	hi = hi / KVPad * KVPad
	if !e.forCtx(KVPad).at(steps).placeable {
		return 0
	}
	if e.forCtx(hi).at(steps).placeable {
		return hi
	}
	lo := KVPad
	// Invariant: lo fits, hi does not. Both are multiples of KVPad.
	for hi-lo > KVPad {
		mid := (lo + hi) / 2 / KVPad * KVPad
		if mid <= lo {
			break
		}
		if e.forCtx(mid).at(steps).placeable {
			lo = mid
		} else {
			hi = mid
		}
	}
	return lo
}

// verdict folds one evaluation into section 8.7's three-valued answer.
//
// The design states the rule for the default configuration — `fits ⟺
// placeable(L+1)` — and this refines it for a request that PINNED an offload:
// `fits` means the configuration under test puts everything in VRAM, `partial`
// means it runs with a spill this host's RAM can hold, and `wont_run` means it
// does not run as asked. The distinction matters because `POST /fit/estimate`
// is asked about a specific FlagSet, not only about the model.
func (e evaluator) verdict(n int, at evaluation, notes []string) (Verdict, []string) {
	steps := e.model.OffloadSteps()

	if len(e.devices) == 0 {
		notes = append(notes,
			"no GPU is selected; this is a CPU-only estimate and every layer is charged to RAM")
		if at.ramOK {
			return VerdictPartial, notes
		}
		notes = append(notes, e.ramNote(at.spill))
		return VerdictWontRun, notes
	}

	if at.placeable {
		if n >= steps {
			return VerdictFits, notes
		}
		if at.ramOK {
			return VerdictPartial, notes
		}
		notes = append(notes, e.ramNote(at.spill))
		return VerdictWontRun, notes
	}

	notes = append(notes, e.shortNote(at))

	// The requested offload does not place. A smaller one might, and section 8.7
	// says to report the largest such n rather than only refusing.
	if best := e.maxPlaceable(); best < n {
		alt := e.at(best)
		if alt.placeable && alt.ramOK {
			notes = append(notes, fmt.Sprintf(
				"the requested offload does not fit; %d of %d layers is the most these GPUs can hold",
				best, steps))
			return VerdictPartial, notes
		}
	}
	if at.spill > 0 && !at.ramOK {
		notes = append(notes, e.ramNote(at.spill))
	}
	return VerdictWontRun, notes
}

// shortNote names the device that is over, rather than reporting a total the
// user cannot act on (section 8.7).
func (e evaluator) shortNote(at evaluation) string {
	if at.worst < 0 || at.worst >= len(at.perGPU) {
		return "at least one selected GPU cannot hold its share"
	}
	r := at.perGPU[at.worst]
	if r.FreeBytes == nil {
		return fmt.Sprintf("GPU %d (%s) reported no free VRAM figure, so it cannot be placed on",
			r.Index, r.Name)
	}
	msg := fmt.Sprintf("GPU %d is short by %s", r.Index, humanBytes(r.ShortByBytes))
	if len(e.devices) > 1 && e.flags.SplitMode != SplitNone {
		msg += " — try a --tensor-split that gives it a smaller share"
	}
	return msg
}

func (e evaluator) ramNote(spill uint64) string {
	if !e.host.RAMKnown {
		return fmt.Sprintf("%s would spill to system RAM, whose free size was not measured",
			humanBytes(spill))
	}
	return fmt.Sprintf("%s would spill to system RAM, which has %s free (the calculator allows 90%% of it)",
		humanBytes(spill), humanBytes(e.host.RAMFreeBytes))
}

// recommend is section 8.7's ladder: prefer full offload; failing that try
// `type_k`/`type_v = q8_0` with `flash_attn: on` (required for a quantized V on
// most builds) before reducing layers; never recommend a context below 4096
// without saying so in the notes.
func (e evaluator) recommend(v Verdict, n, maxN int, k, kv CacheType, f Flags, notes []string) (Recommendation, []string) {
	steps := e.model.OffloadSteps()

	if e.at(steps).placeable {
		return Recommendation{
			NGpuLayers: steps,
			FlashAttn:  f.FlashAttnOn(),
			TypeK:      k.Name,
			TypeV:      kv.Name,
			Reason:     "the whole model, its cache and its buffers fit in VRAM",
		}, notes
	}

	q8, _ := LookupCacheType(CacheTypeQ8_0)
	quant := e.forCache(q8, q8, true)
	if quant.at(steps).placeable {
		return Recommendation{
			NGpuLayers: steps,
			FlashAttn:  true,
			TypeK:      CacheTypeQ8_0,
			TypeV:      CacheTypeQ8_0,
			Reason: "quantizing the KV cache to q8_0 with flash attention on makes full " +
				"offload fit",
		}, notes
	}

	best, bestFA, bestK, bestV := maxN, f.FlashAttnOn(), k.Name, kv.Name
	if qn := quant.maxPlaceable(); qn > best {
		best, bestFA, bestK, bestV = qn, true, CacheTypeQ8_0, CacheTypeQ8_0
	}
	if best > 0 {
		return Recommendation{
			NGpuLayers: best,
			FlashAttn:  bestFA,
			TypeK:      bestK,
			TypeV:      bestV,
			Reason: fmt.Sprintf("%d of %d offload steps is the most these GPUs can hold at this context",
				best, steps),
		}, notes
	}

	// Nothing offloads at this context. A smaller one may, and that is worth
	// saying — but not silently: a context below 4096 is a different product.
	if c := quant.maxCtxAtFullOffload(); c > 0 {
		rec := Recommendation{
			NGpuLayers: steps,
			FlashAttn:  true,
			TypeK:      CacheTypeQ8_0,
			TypeV:      CacheTypeQ8_0,
			NCtx:       c,
			Reason: fmt.Sprintf("full offload needs the context reduced to %d with a q8_0 cache",
				c),
		}
		if c < MinRecommendedCtx {
			notes = append(notes, fmt.Sprintf(
				"the recommended context of %d is below %d, which is short for most uses",
				c, MinRecommendedCtx))
		}
		return rec, notes
	}

	return Recommendation{
		NGpuLayers: 0,
		FlashAttn:  f.FlashAttnOn(),
		TypeK:      k.Name,
		TypeV:      kv.Name,
		Reason:     "no offload fits on the selected GPUs; this configuration runs on the CPU",
	}, notes
}

// MinRecommendedCtx is the floor section 8.7 names: a recommendation below it is
// allowed, but never silently.
const MinRecommendedCtx = 4096

// humanBytes renders a byte count for a note. It is the one place this package
// formats anything, and it exists because "GPU 1 is short by 2.1 GB" is
// actionable and "short by 2254857830" is not.
func humanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit && exp < 4; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTP"[exp])
}
