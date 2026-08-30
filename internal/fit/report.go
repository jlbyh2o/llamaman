package fit

// The report shape of DESIGN section 3.9, as domain values.
//
// The HTTP DTO is a projection of this — the API layer owns its own field names
// and its own JSON — but the arithmetic and the vocabulary are here, so a
// consumer that is not HTTP (the instance detail page's advisory, the bench
// planner, a CLI `doctor` line) reads the same numbers.

// Verdict is section 8.7's three-valued answer.
type Verdict string

const (
	// VerdictFits means every offload step is placeable on the selected devices:
	// the whole model, its cache and its buffers are in VRAM.
	VerdictFits Verdict = "fits"
	// VerdictPartial means it runs, with some layers on the CPU, and the spill
	// is within RAMHeadroom of free system RAM.
	VerdictPartial Verdict = "partial"
	// VerdictWontRun means neither.
	VerdictWontRun Verdict = "wont_run"
)

// DeviceReport is one row of section 3.9's `per_gpu`.
//
// FreeBytes is a pointer for the reason D16 gives: a GPU whose driver could not
// be read has an UNKNOWN free figure, not a zero one, and a UI that rendered
// "0 bytes free" would be reporting a measurement nobody made.
type DeviceReport struct {
	Index         int
	UUID          string
	Name          string
	FreeBytes     *uint64
	TotalBytes    *uint64
	AssignedBytes uint64
	// OK is `assigned ≤ free − reserve` for this device. The verdict is exactly
	// the conjunction of these, which is why the sum below is labeled a total and
	// never used as a test.
	OK bool
	// ShortByBytes is how much this device is over, 0 when it is not. It is what
	// lets `notes` say "GPU 1 is short by 2.1 GB" instead of reporting a total
	// the user cannot act on.
	ShortByBytes uint64
	// WeightsBytes, KVBytes and ExtraBytes break `assigned` down, so the UI can
	// show where the VRAM went without re-deriving the split.
	WeightsBytes uint64
	KVBytes      uint64
	ExtraBytes   uint64
	// OverheadBytes, MarginBytes and ReserveBytes are the three flat per-device
	// charges: OH_gpu, `fit.margin_mib` and the request's own headroom.
	OverheadBytes uint64
	MarginBytes   uint64
	ReserveBytes  uint64
}

// Inputs echoes what the estimate was made FROM — section 3.9's `inputs` object.
// It exists so a report can be read a week later without the request beside it.
type Inputs struct {
	Arch        string
	NLayer      int
	NLayerSWA   int
	NHeadKV     []int
	HeadDimK    int
	HeadDimV    int
	NCtx        int
	KVCtx       int
	NUbatch     int
	NBatch      int
	NParallel   int
	TypeK       string
	TypeV       string
	FlashAttn   bool
	NExpert     int
	NExpertUsed int
	NVocab      int
	NEmbd       int
	NFF         int
	NHead       int
}

// Recommendation is section 8.7's suggested configuration: prefer full offload,
// then quantized cache with flash attention, then fewer layers.
type Recommendation struct {
	NGpuLayers int
	FlashAttn  bool
	TypeK      string
	TypeV      string
	// NCtx is present only when the recommendation had to reduce the context.
	NCtx int
	// Reason is one sentence naming what the change buys.
	Reason string
}

// Report is one estimate.
type Report struct {
	Inputs Inputs

	// WeightsBytes is W_total and WeightsOffloadedBytes is W_gpu(n).
	WeightsBytes          uint64
	WeightsOffloadedBytes uint64
	// KVBytes is the full-attention term and KVSWABytes the sliding-window one,
	// both over ALL layers; KVOffloadedBytes is the part that is in VRAM.
	KVBytes          uint64
	KVSWABytes       uint64
	KVOffloadedBytes uint64
	// ComputeBytes is CB, charged to every participating device.
	ComputeBytes uint64
	// ComputeLogitsBytes and friends break CB into section 8.4's four terms.
	ComputeLogitsBytes uint64
	ComputeActBytes    uint64
	ComputeAttnBytes   uint64
	ComputeMoEBytes    uint64

	// BackendOverheadBytes is OH_gpu, PER participating GPU.
	BackendOverheadBytes uint64
	// MarginBytesPerGPU is `fit.margin_mib`; MarginBytes is it times the number
	// of participating GPUs — a reporting total, never a test.
	MarginBytesPerGPU uint64
	MarginBytes       uint64
	// ReserveBytesPerGPU is echoed from the request; ReserveBytes is its total.
	ReserveBytesPerGPU uint64
	ReserveBytes       uint64

	// RequiredVRAMBytes is Σ per_gpu[].assigned_bytes. It is a TOTAL and is
	// never the test — section 8.7 is explicit that a Σ-free-VRAM comparison
	// says "fits" for a model that cannot be placed on any single card.
	RequiredVRAMBytes uint64
	PerGPU            []DeviceReport

	// SpillToRAMBytes is what is not in VRAM: weights, cache and extras.
	SpillToRAMBytes uint64
	// SystemRAMFreeBytes is MemAvailable, or 0 with RAMKnown false.
	SystemRAMFreeBytes uint64
	SystemRAMKnown     bool

	Verdict Verdict
	// NGpuLayers is the offload this report was evaluated at — the resolution of
	// `auto` when the request said auto (D51), and otherwise what was asked for.
	NGpuLayers int
	// MaxNGpuLayers is what we predict llama.cpp's `--fit` will choose for
	// `-ngl auto`, and the value `POST /instances/{id}/pin-ngl` writes. It is
	// ADVISORY and is never rendered into argv (D51).
	MaxNGpuLayers int
	// MaxCtxAtFullOffload is the largest context, rounded down to 256, that fits
	// with everything offloaded. Zero when nothing fits fully offloaded.
	MaxCtxAtFullOffload int
	// PerSlotCtx is C/P, the derived number section 8.3 says users get wrong.
	PerSlotCtx int

	Recommendation Recommendation
	Confidence     Confidence
	Calibration    Calibration
	// VRAMUnknown reports that at least one selected device's memory could not
	// be read (F14). A consumer must render "unknown" rather than "won't run"
	// when this is set: the verdict below fails closed, but it is a statement
	// about a measurement that does not exist.
	VRAMUnknown bool
	Notes       []string
}
