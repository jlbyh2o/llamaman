package fit

// Compute buffers and overhead (DESIGN section 8.4, D31).
//
//	CB_logits = (embedding ? n_embd : n_vocab) × max(U, P) × 4
//	CB_act    = k_act × U × n_embd × 4
//	CB_attn   = flash_attn ? 2 × U × n_head × head_dim_k × 4
//	                       : n_head × U × min(kv_ctx, 4096) × 4
//	CB_moe    = n_expert_used × U × n_ff × 4
//	CB        = CB_logits + CB_act + CB_attn + CB_moe
//	OH_gpu    = 400 MiB, PER GPU
//
// Every term is a graph allocation in f32, which is where the ×4 comes from.
// CB_moe is a first-class term rather than a fudge factor because the routing
// and expert scratch of a 128-expert model is hundreds of megabytes that appear
// on no other line.

// DefaultKAct is the activation multiplier of CB_act. Six is the value section
// 8.4 states, and it is the one thing in this whole model that is genuinely
// empirical — which is why D32's calibration corrects it (and OH_gpu) from this
// host's own loads rather than leaving it a hard-coded constant forever.
const DefaultKAct = 6.0

// OverheadPerGPUBytes is OH_gpu: the CUDA context plus cuBLAS workspaces, per
// participating device.
const OverheadPerGPUBytes uint64 = 400 << 20

// AttnCtxCap is the `min(kv_ctx, 4096)` of the non-flash attention term: the
// unfused attention graph materializes a score matrix over a bounded slice of
// the context rather than all of it.
const AttnCtxCap = 4096

// kActFor is section 8.4's "per-arch table". It has one entry today — the
// default — and it exists as a named seam rather than an inline 6 so that the
// first architecture that genuinely needs a different multiplier is a line here
// and a golden test, not a rewrite of the term.
func kActFor(arch string) float64 {
	if k, ok := kActByArch[arch]; ok {
		return k
	}
	return DefaultKAct
}

var kActByArch = map[string]float64{}

// computeParts is CB split into its four terms, so the report can show where a
// gigabyte went and a calibration observation can be compared term by term.
type computeParts struct {
	logits uint64
	act    uint64
	attn   uint64
	moe    uint64
}

func (c computeParts) total() uint64 { return c.logits + c.act + c.attn + c.moe }

// computeBuffers sizes CB. cal scales the empirical half — k_act — and is the
// identity when no calibration is in effect.
func computeBuffers(m ModelShape, f Flags, cal Calibration) computeParts {
	const f32 = 4

	width := m.NVocab
	if f.Embedding {
		// An embedding server never materializes vocabulary logits; on a
		// 150k-token vocabulary that single branch is most of a gigabyte.
		width = m.NEmbd
	}
	logitsRows := f.NUbatch
	if f.NParallel > logitsRows {
		logitsRows = f.NParallel
	}

	kAct := cal.ApplyKAct(kActFor(m.Arch))

	var parts computeParts
	parts.logits = mul(uint64(width), uint64(logitsRows), f32)
	parts.act = uint64(kAct * float64(f.NUbatch) * float64(m.NEmbd) * f32)
	if f.FlashAttnOn() {
		parts.attn = mul(2, uint64(f.NUbatch), uint64(m.NHead), uint64(m.HeadDimK), f32)
	} else {
		span := PadCtx(f.NCtx)
		if span > AttnCtxCap {
			span = AttnCtxCap
		}
		parts.attn = mul(uint64(m.NHead), uint64(f.NUbatch), uint64(span), f32)
	}
	if m.NExpert > 0 {
		parts.moe = mul(uint64(m.NExpertUsed), uint64(f.NUbatch), uint64(m.NFF), f32)
	}
	return parts
}

// mul multiplies a list of counts. It is a helper rather than an expression so
// the terms above read as the formulas section 8.4 writes them.
func mul(vs ...uint64) uint64 {
	out := uint64(1)
	for _, v := range vs {
		out *= v
	}
	return out
}
