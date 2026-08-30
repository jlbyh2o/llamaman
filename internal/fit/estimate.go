package fit

import "fmt"

// Estimate is the whole calculator (DESIGN section 8): a pure function from a
// model shape, a FlagSet subset, the visible devices and a calibration table to
// a report saying whether the configuration will run and where its memory goes.
//
// No I/O, no clock, no database. The design writes the signature as
// `Estimate(ModelShape, FlagSet, []Device, Calibration) → Report`; it is a
// single Request struct here because section 8.1 lists three more host inputs —
// free system RAM, `fit.margin_mib`, and the caller's own reserve — that the
// arrow notation leaves out, and four bare arguments plus three more positional
// ones is how a caller eventually passes the margin as the reserve.

// Request is one estimate's inputs.
type Request struct {
	Model ModelShape
	Flags Flags
	// Devices are the PARTICIPATING GPUs, already filtered by the request's
	// `gpus` list. An empty slice is a CPU-only estimate, not an error.
	Devices []Device
	Host    Host
	// Calibration is the correction for this `(arch, backend, llamacpp_tag)`.
	// The zero value is the identity and makes the report `modeled`.
	Calibration Calibration
	// ReserveBytesPerGPU is section 3.9's `reserve_bytes_per_gpu`: the caller's
	// own headroom, charged to EVERY participating device exactly like margin
	// and OH_gpu, never divided among them.
	ReserveBytesPerGPU uint64
	// MarginMiB is `fit.margin_mib` (section 8.1), charged PER GPU.
	//
	// It is a POINTER because the setting's own registry entry says "0 is a
	// legitimate 'no margin'": a plain int could not tell an operator who
	// deliberately set the knob to zero from a caller who never set it at all,
	// and the zero-means-default reading would silently charge that operator a
	// gibibyte per card they had explicitly declined. Nil means "not supplied"
	// and uses DefaultMarginMiB; a negative value is treated as nil, since there
	// is no such thing as a negative reservation.
	MarginMiB *int
}

// MiB returns a pointer to n, for Request.MarginMiB. It exists so a call site
// can write `MarginMiB: fit.MiB(2048)` without a temporary.
func MiB(n int) *int { return &n }

// Estimate produces the report.
func Estimate(req Request) Report {
	f := req.Flags.Normalize()
	m := req.Model
	var notes []string
	modeled := false

	k, okK := LookupCacheType(f.TypeK)
	if !okK {
		notes = append(notes, fmt.Sprintf(
			"cache type %q is not one this build knows; sized as f16", f.TypeK))
		k, _ = LookupCacheType(CacheTypeF16)
		modeled = true
	}
	v, okV := LookupCacheType(f.TypeV)
	if !okV {
		notes = append(notes, fmt.Sprintf(
			"cache type %q is not one this build knows; sized as f16", f.TypeV))
		v, _ = LookupCacheType(CacheTypeF16)
		modeled = true
	}
	if v.Quantized() && !f.FlashAttnOn() {
		notes = append(notes, "V-cache quantization requires flash attention on most builds")
	}
	if f.FlashAttn == FlashAttnAuto {
		notes = append(notes,
			"flash_attn is `auto`; the attention buffer is sized as if it were off, "+
				"which is the larger of the two")
	}

	layerBytes, other, exact := m.Layers()
	if !exact {
		notes = append(notes,
			"the tensor index is not available, so the weights are the file size split "+
				"evenly over the layers; the estimate is approximate for MoE models and "+
				"large output heads")
		modeled = true
	}

	kv := planKV(m, f, k, v)
	if m.SWAWindow != nil && *m.SWAWindow > 0 && !kv.windowApplied {
		notes = append(notes, fmt.Sprintf(
			"the model declares a sliding window of %d with no sliding_window_pattern; "+
				"the window was ignored and every layer is sized as full attention",
			*m.SWAWindow))
		modeled = true
	}

	cb := computeBuffers(m, f, req.Calibration)
	overhead := req.Calibration.ApplyOverhead(OverheadPerGPUBytes)

	marginMiB := DefaultMarginMiB
	if req.MarginMiB != nil && *req.MarginMiB >= 0 {
		marginMiB = *req.MarginMiB
	}
	margin := uint64(marginMiB) << 20

	devs, devNotes := participating(req.Devices, f)
	notes = append(notes, devNotes...)
	unknown := false
	for _, d := range devs {
		if !d.Known {
			unknown = true
		}
	}
	if unknown {
		notes = append(notes,
			"at least one selected GPU's free VRAM could not be read; no verdict can be "+
				"given for it and it is treated as unplaceable")
		modeled = true
	}

	extra, extraNotes := extraCharge(m, f, k, v)
	notes = append(notes, extraNotes...)

	ev := evaluator{
		model:       m,
		flags:       f,
		devices:     devs,
		layer:       layerBytes,
		other:       other,
		kv:          kv,
		k:           k,
		v:           v,
		calibration: req.Calibration,
		compute:     cb.total(),
		overhead:    overhead,
		margin:      margin,
		reserve:     req.ReserveBytesPerGPU,
		extra:       extra,
		host:        req.Host,
	}

	steps := m.OffloadSteps() // L+1
	maxN := ev.maxPlaceable()

	n, autoResolved := resolveNGL(f, steps, maxN)
	if autoResolved {
		notes = append(notes, fmt.Sprintf(
			"`-ngl auto` renders no -ngl flag at all; %d is this calculator's advisory "+
				"prediction of what llama.cpp's own --fit will choose", n))
	}

	at := ev.at(n)
	rep := Report{
		Inputs: Inputs{
			Arch: m.Arch, NLayer: m.NLayer, NLayerSWA: kv.swaLayers,
			NHeadKV: headCounts(m), HeadDimK: m.HeadDimK, HeadDimV: m.HeadDimV,
			NCtx: f.NCtx, KVCtx: PadCtx(f.NCtx), NUbatch: f.NUbatch, NBatch: f.NBatch,
			NParallel: f.NParallel, TypeK: k.Name, TypeV: v.Name,
			FlashAttn: f.FlashAttnOn(), NExpert: m.NExpert, NExpertUsed: m.NExpertUsed,
			NVocab: m.NVocab, NEmbd: m.NEmbd, NFF: m.NFF, NHead: m.NHead,
		},
		WeightsBytes:          totalOf(layerBytes) + other,
		WeightsOffloadedBytes: at.gpuWeights,
		KVBytes:               kv.full,
		KVSWABytes:            kv.swa,
		KVOffloadedBytes:      at.gpuKV,
		ComputeBytes:          cb.total(),
		ComputeLogitsBytes:    cb.logits,
		ComputeActBytes:       cb.act,
		ComputeAttnBytes:      cb.attn,
		ComputeMoEBytes:       cb.moe,
		BackendOverheadBytes:  overhead,
		MarginBytesPerGPU:     margin,
		MarginBytes:           margin * uint64(len(devs)),
		ReserveBytesPerGPU:    req.ReserveBytesPerGPU,
		ReserveBytes:          req.ReserveBytesPerGPU * uint64(len(devs)),
		RequiredVRAMBytes:     at.required,
		PerGPU:                at.perGPU,
		SpillToRAMBytes:       at.spill,
		SystemRAMFreeBytes:    req.Host.RAMFreeBytes,
		SystemRAMKnown:        req.Host.RAMKnown,
		NGpuLayers:            n,
		MaxNGpuLayers:         maxN,
		MaxCtxAtFullOffload:   ev.maxCtxAtFullOffload(),
		PerSlotCtx:            f.PerSlotCtx(),
		Calibration:           req.Calibration,
		VRAMUnknown:           unknown,
	}

	rep.Verdict, notes = ev.verdict(n, at, notes)
	rep.Recommendation, notes = ev.recommend(rep.Verdict, n, maxN, k, v, f, notes)

	rep.Confidence = ConfidenceModeled
	if req.Calibration.Applied && !modeled {
		rep.Confidence = ConfidenceCalibrated
	}
	rep.Notes = notes
	return rep
}

// participating narrows the device list to the ones this split actually uses.
// `split_mode=none` uses one device, so charging OH_gpu and the margin to the
// others would invent gigabytes of pressure on cards llama.cpp will not touch.
func participating(devs []Device, f Flags) ([]Device, []string) {
	if len(devs) == 0 || f.SplitMode != SplitNone {
		return devs, nil
	}
	main := f.MainGPU
	if main >= len(devs) {
		main = len(devs) - 1
	}
	d := devs[main]
	d.Index = 0
	return []Device{d}, []string{
		"split_mode is `none`, so only the main device participates",
	}
}

// resolveNGL turns the FlagSet's four modes into a layer count on [0, L+1].
// `auto` is resolved HERE and nowhere else in the product (D51).
func resolveNGL(f Flags, steps, maxN int) (n int, auto bool) {
	switch f.NGL {
	case NGLAll:
		return steps, false
	case NGLNone:
		return 0, false
	case NGLCount:
		if f.NGLCount > steps {
			return steps, false
		}
		if f.NGLCount < 0 {
			return 0, false
		}
		return f.NGLCount, false
	default:
		return maxN, true
	}
}

func headCounts(m ModelShape) []int {
	if m.NLayer <= 0 {
		return nil
	}
	out := make([]int, m.NLayer)
	for i := range out {
		out[i] = m.HeadCountKV(i)
	}
	return out
}

func totalOf(vs []uint64) uint64 {
	var out uint64
	for _, v := range vs {
		out += v
	}
	return out
}
