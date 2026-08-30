package fit

// The FlagSet subset section 8.1 reads, as a plain struct.
//
// It is a projection of `model.FlagSet` rather than that type itself: D49
// invariant 5 keeps this package on the standard library, and the fit route owns
// the ten-line conversion. The field names match the FlagSet's so the two cannot
// be confused at a call site, and every default that llama-server would apply is
// resolved HERE, once, by Normalize — not scattered through the formulas, where
// a forgotten zero becomes a division or a free fit.

// NGLMode mirrors the four modes of `n_gpu_layers` (D51, section 5.7).
type NGLMode string

const (
	// NGLAuto is resolved to a layer count HERE and nowhere else in the product
	// (D51): the launch path renders no `-ngl` flag at all and lets llama.cpp's
	// own `--fit` decide with the real free VRAM. This resolution is what gives
	// the UI a number to show and `pin-ngl` a value to write; it is advisory.
	NGLAuto NGLMode = "auto"
	// NGLAll is `-ngl 999`: every layer plus the output weights.
	NGLAll NGLMode = "all"
	// NGLNone is `-ngl 0`.
	NGLNone NGLMode = "none"
	// NGLCount is `-ngl N`.
	NGLCount NGLMode = "count"
)

// SplitMode mirrors `-sm none|layer|row`.
type SplitMode string

const (
	// SplitNone puts everything on the main device.
	SplitNone SplitMode = "none"
	// SplitLayer is llama.cpp's default: whole layers are dealt out across the
	// participating devices by `tensor_split`.
	SplitLayer SplitMode = "layer"
	// SplitRow splits each tensor's rows by `tensor_split` and keeps the whole
	// KV cache on the main device.
	SplitRow SplitMode = "row"
)

// FlashAttn mirrors `-fa on|off|auto`.
type FlashAttn string

const (
	FlashAttnOn   FlashAttn = "on"
	FlashAttnOff  FlashAttn = "off"
	FlashAttnAuto FlashAttn = "auto"
)

// Flags is the FlagSet subset section 8.1 names, already resolved to values.
type Flags struct {
	// NCtx is C: the TOTAL context llama-server shares across its slots. KV is
	// sized by C alone and does not scale with NParallel (section 8.3).
	NCtx int
	// NParallel is P, the slot count. It appears in CB_logits and in the derived
	// per-slot context the UI shows.
	NParallel int
	// NBatch and NUbatch are B and U. U is the one that sizes compute buffers.
	NBatch  int
	NUbatch int

	// NGL is the offload decision.
	NGL      NGLMode
	NGLCount int

	FlashAttn FlashAttn
	// TypeK and TypeV are the `-ctk`/`-ctv` cache types. Empty means f16.
	TypeK string
	TypeV string

	SplitMode SplitMode
	// TensorSplit is the per-device weighting, indexed into Devices. An empty
	// slice splits evenly.
	TensorSplit []float64
	// MainGPU indexes Devices.
	MainGPU int

	// Embedding swaps CB_logits from n_vocab to n_embd, which on a
	// large-vocabulary model is the difference between 600 MiB and 5 MiB.
	Embedding bool

	// Draft is the speculative-decoding tuning that goes with ModelShape.Draft.
	Draft DraftFlags
}

// DraftFlags is the draft model's own context and offload.
type DraftFlags struct {
	// CtxSize is `-cd`. Zero means "the primary model's context", which is what
	// llama.cpp falls back to and the larger of the two readings.
	CtxSize int
	// NGpuLayers is `-ngld`. nil means "not pinned", which upstream resolves to
	// the same offload as the primary model; this package reads it as ALL of the
	// draft model, the conservative choice, and says so in a note.
	NGpuLayers *int
}

// Defaults are the values llama-server itself applies when a flag is absent.
// They are resolved once, by Normalize, so no formula below has to branch on a
// zero that might mean "unset" or might mean "zero".
const (
	DefaultNCtx      = 4096
	DefaultNParallel = 1
	DefaultNBatch    = 2048
	DefaultNUbatch   = 512
)

// Normalize fills in llama-server's own defaults and clamps the nonsensical.
//
// It never fails. A caller that hands over a zero context gets llama-server's
// default rather than a division by zero, because a fit report that refuses to
// exist is less useful than one that says what the server would actually do.
func (f Flags) Normalize() Flags {
	if f.NCtx <= 0 {
		f.NCtx = DefaultNCtx
	}
	if f.NParallel <= 0 {
		f.NParallel = DefaultNParallel
	}
	if f.NBatch <= 0 {
		f.NBatch = DefaultNBatch
	}
	if f.NUbatch <= 0 {
		f.NUbatch = DefaultNUbatch
	}
	if f.NUbatch > f.NBatch {
		// llama.cpp clamps the micro-batch to the batch; a larger one is not an
		// error there and must not size a buffer here.
		f.NUbatch = f.NBatch
	}
	if f.NGL == "" {
		f.NGL = NGLAuto
	}
	if f.FlashAttn == "" {
		f.FlashAttn = FlashAttnAuto
	}
	if f.TypeK == "" {
		f.TypeK = CacheTypeF16
	}
	if f.TypeV == "" {
		f.TypeV = CacheTypeF16
	}
	if f.SplitMode == "" {
		f.SplitMode = SplitLayer
	}
	if f.MainGPU < 0 {
		f.MainGPU = 0
	}
	return f
}

// FlashAttnOn reports whether the attention compute buffer should be sized with
// flash attention.
//
// `auto` is read as OFF, and that asymmetry is deliberate. The non-flash
// attention buffer is the larger of the two by an order of magnitude at long
// context, and section 8.7's golden rule is that a verdict must never say "fits"
// for a load that OOMs. Guessing the smaller buffer for a tri-state we cannot
// resolve without the build's own capability report would break exactly that
// rule; the report says so in a note instead.
func (f Flags) FlashAttnOn() bool { return f.FlashAttn == FlashAttnOn }

// PerSlotCtx is C/P — the number section 8.3 says users actually get wrong.
func (f Flags) PerSlotCtx() int {
	if f.NParallel <= 0 {
		return f.NCtx
	}
	return f.NCtx / f.NParallel
}
