package fit

import (
	"strings"
	"testing"
)

const (
	kib = uint64(1) << 10
	mib = uint64(1) << 20
	gib = uint64(1) << 30
)

// tinyShape is the model every arithmetic assertion below is hand-computed
// against. Its numbers are chosen so the products are exact and short:
//
//	L = 4, n_embd = 256, n_ff = 512, n_head = 8, n_head_kv = 8,
//	head_dim_k = head_dim_v = 32, n_vocab = 1000
//	W_layer[i] = 1 MiB each, W_other = 2 MiB, so W_total = 6 MiB
func tinyShape() ModelShape {
	return ModelShape{
		Arch: "llama", NLayer: 4, NEmbd: 256, NFF: 512, NHead: 8,
		NHeadKV: []int{8, 8, 8, 8}, HeadDimK: 32, HeadDimV: 32,
		NVocab: 1000, NCtxTrain: 4096,
		LayerBytes: []uint64{mib, mib, mib, mib},
		OtherBytes: 2 * mib,
	}
}

// tinyFlags is C = 1024, U = 64, P = 1, f16 cache, flash attention off.
func tinyFlags() Flags {
	return Flags{
		NCtx: 1024, NUbatch: 64, NParallel: 1,
		NGL: NGLAll, FlashAttn: FlashAttnOff,
		TypeK: CacheTypeF16, TypeV: CacheTypeF16, SplitMode: SplitLayer,
	}
}

func oneDevice(free uint64) []Device {
	return []Device{{Index: 0, UUID: "GPU-a", Name: "Test GPU", TotalBytes: free, FreeBytes: free, Known: true}}
}

// The three flat per-device charges, at the margin the tests use.
//
//	OH_gpu = 400 MiB = 419430400
//	margin = 1 MiB   =   1048576   (MarginMiB: 1, so the model's own bytes show)
const (
	testMarginMiB   = 1
	testMarginBytes = mib
)

// TestEstimateTinyModelTermByTerm is the whole of sections 8.2, 8.3 and 8.4 on
// one model, with every expected number's arithmetic written out.
func TestEstimateTinyModelTermByTerm(t *testing.T) {
	rep := Estimate(Request{
		Model:     tinyShape(),
		Flags:     tinyFlags(),
		Devices:   oneDevice(gib),
		Host:      Host{RAMFreeBytes: 16 * gib, RAMTotalBytes: 32 * gib, RAMKnown: true},
		MarginMiB: MiB(testMarginMiB),
	})

	// Weights (section 8.2): -ngl all offloads L layers PLUS W_other.
	if rep.WeightsBytes != 6*mib {
		t.Errorf("W_total = %d, want %d", rep.WeightsBytes, 6*mib)
	}
	if rep.WeightsOffloadedBytes != 6*mib {
		t.Errorf("W_gpu(L+1) = %d, want %d", rep.WeightsOffloadedBytes, 6*mib)
	}

	// KV (section 8.3): kv_ctx = round_up(1024, 256) = 1024.
	// per_tok(i) = n_head_kv × (head_dim_k × 2 + head_dim_v × 2)
	//            = 8 × (32×2 + 32×2) = 1024 bytes
	// KV_full    = 1024 × 4 layers × 1024 = 4194304 = 4 MiB
	if rep.KVBytes != 4*mib {
		t.Errorf("KV = %d, want %d", rep.KVBytes, 4*mib)
	}
	if rep.KVSWABytes != 0 {
		t.Errorf("a model with no sliding window must have KV_swa = 0, got %d", rep.KVSWABytes)
	}
	if rep.KVOffloadedBytes != 4*mib {
		t.Errorf("KV_gpu = %d, want %d", rep.KVOffloadedBytes, 4*mib)
	}

	// Compute (section 8.4), flash attention OFF:
	//   CB_logits = 1000 × max(64, 1) × 4 =    256000
	//   CB_act    = 6 × 64 × 256 × 4      =    393216
	//   CB_attn   = 8 × 64 × min(1024, 4096) × 4 = 2097152
	//   CB_moe    = 0 (dense)
	//   CB        =                          2746368
	if rep.ComputeLogitsBytes != 256000 {
		t.Errorf("CB_logits = %d, want 256000", rep.ComputeLogitsBytes)
	}
	if rep.ComputeActBytes != 393216 {
		t.Errorf("CB_act = %d, want 393216", rep.ComputeActBytes)
	}
	if rep.ComputeAttnBytes != 2097152 {
		t.Errorf("CB_attn = %d, want 2097152", rep.ComputeAttnBytes)
	}
	if rep.ComputeMoEBytes != 0 {
		t.Errorf("CB_moe = %d, want 0 on a dense model", rep.ComputeMoEBytes)
	}
	if rep.ComputeBytes != 2746368 {
		t.Errorf("CB = %d, want 2746368", rep.ComputeBytes)
	}

	// assigned(g, L+1) = W_gpu + KV_gpu + CB + OH_gpu + margin
	//                  = 6291456 + 4194304 + 2746368 + 419430400 + 1048576
	//                  = 433711104
	const wantAssigned = 433711104
	if len(rep.PerGPU) != 1 {
		t.Fatalf("want one per_gpu row, got %d", len(rep.PerGPU))
	}
	if rep.PerGPU[0].AssignedBytes != wantAssigned {
		t.Errorf("assigned = %d, want %d", rep.PerGPU[0].AssignedBytes, wantAssigned)
	}
	if rep.RequiredVRAMBytes != wantAssigned {
		t.Errorf("required (Σ assigned) = %d, want %d", rep.RequiredVRAMBytes, wantAssigned)
	}
	if rep.BackendOverheadBytes != OverheadPerGPUBytes {
		t.Errorf("OH_gpu = %d, want %d", rep.BackendOverheadBytes, OverheadPerGPUBytes)
	}
	if rep.MarginBytesPerGPU != testMarginBytes || rep.MarginBytes != testMarginBytes {
		t.Errorf("margin = %d per GPU / %d total, want %d / %d",
			rep.MarginBytesPerGPU, rep.MarginBytes, testMarginBytes, testMarginBytes)
	}
	if rep.Verdict != VerdictFits {
		t.Errorf("verdict = %q, want fits (%v)", rep.Verdict, rep.Notes)
	}
	if rep.SpillToRAMBytes != 0 {
		t.Errorf("spill = %d, want 0 at full offload", rep.SpillToRAMBytes)
	}
	if rep.Confidence != ConfidenceModeled {
		t.Errorf("confidence = %q, want modeled with no calibration", rep.Confidence)
	}
}

// TestFlashAttentionChangesTheAttentionBuffer is section 8.4's "why FA matters"
// comment, made executable.
func TestFlashAttentionChangesTheAttentionBuffer(t *testing.T) {
	f := tinyFlags()
	f.FlashAttn = FlashAttnOn
	rep := Estimate(Request{
		Model: tinyShape(), Flags: f, Devices: oneDevice(gib), MarginMiB: MiB(testMarginMiB),
	})
	// CB_attn = 2 × U × n_head × head_dim_k × 4 = 2 × 64 × 8 × 32 × 4 = 131072
	if rep.ComputeAttnBytes != 131072 {
		t.Errorf("CB_attn with flash attention = %d, want 131072", rep.ComputeAttnBytes)
	}
	if !rep.Inputs.FlashAttn {
		t.Error("inputs should echo flash_attn true")
	}
}

// TestFlashAttnAutoIsSizedAsOff: `auto` cannot be resolved without the build's
// own capability report, and section 8.7's golden rule forbids guessing the
// smaller buffer.
func TestFlashAttnAutoIsSizedAsOff(t *testing.T) {
	f := tinyFlags()
	f.FlashAttn = FlashAttnAuto
	rep := Estimate(Request{
		Model: tinyShape(), Flags: f, Devices: oneDevice(gib), MarginMiB: MiB(testMarginMiB),
	})
	if rep.ComputeAttnBytes != 2097152 {
		t.Errorf("CB_attn for `auto` = %d, want the non-flash figure 2097152", rep.ComputeAttnBytes)
	}
	if !hasNote(rep.Notes, "flash_attn is `auto`") {
		t.Errorf("the assumption must be reported: %v", rep.Notes)
	}
}

// TestNGLAllVersusNGLLayers is D29's reason for keeping W_other separate: the
// two differ by the output head alone, which on a real model can exceed a
// gigabyte.
func TestNGLAllVersusNGLLayers(t *testing.T) {
	base := tinyFlags()

	all := base
	all.NGL = NGLAll
	repAll := Estimate(Request{
		Model: tinyShape(), Flags: all, Devices: oneDevice(gib),
		Host: Host{RAMFreeBytes: 16 * gib, RAMKnown: true}, MarginMiB: MiB(testMarginMiB),
	})

	layers := base
	layers.NGL, layers.NGLCount = NGLCount, 4 // exactly L
	repL := Estimate(Request{
		Model: tinyShape(), Flags: layers, Devices: oneDevice(gib),
		Host: Host{RAMFreeBytes: 16 * gib, RAMKnown: true}, MarginMiB: MiB(testMarginMiB),
	})

	if repAll.WeightsOffloadedBytes-repL.WeightsOffloadedBytes != 2*mib {
		t.Errorf("-ngl all offloads %d, -ngl L offloads %d; the difference must be W_other = %d",
			repAll.WeightsOffloadedBytes, repL.WeightsOffloadedBytes, 2*mib)
	}
	if repL.SpillToRAMBytes != 2*mib {
		t.Errorf("-ngl L must leave W_other in RAM: spill = %d, want %d", repL.SpillToRAMBytes, 2*mib)
	}
	if repAll.Verdict != VerdictFits {
		t.Errorf("-ngl all verdict = %q, want fits", repAll.Verdict)
	}
	if repL.Verdict != VerdictPartial {
		t.Errorf("-ngl L verdict = %q, want partial — the output head is on the CPU", repL.Verdict)
	}
}

// TestPerLayerHeadCountKV is D30's per-layer array: a model whose later layers
// have fewer KV heads costs less than n_head_kv[0] broadcast would say.
func TestPerLayerHeadCountKV(t *testing.T) {
	m := tinyShape()
	m.NHeadKV = []int{8, 8, 2, 2}
	rep := Estimate(Request{
		Model: m, Flags: tinyFlags(), Devices: oneDevice(gib), MarginMiB: MiB(testMarginMiB),
	})
	// per_tok = [1024, 1024, 256, 256]; KV = 1024 × 2560 = 2621440
	if rep.KVBytes != 2621440 {
		t.Errorf("KV with a per-layer array = %d, want 2621440", rep.KVBytes)
	}
	if len(rep.Inputs.NHeadKV) != 4 || rep.Inputs.NHeadKV[3] != 2 {
		t.Errorf("inputs.n_head_kv = %v, want the per-layer array", rep.Inputs.NHeadKV)
	}
}

// TestMoEComputeTerm is D31: expert scratch is a first-class term, not a fudge
// factor, and it is zero on a dense model.
func TestMoEComputeTerm(t *testing.T) {
	m := tinyShape()
	m.Arch, m.NExpert, m.NExpertUsed = "qwen3moe", 8, 2
	rep := Estimate(Request{
		Model: m, Flags: tinyFlags(), Devices: oneDevice(gib), MarginMiB: MiB(testMarginMiB),
	})
	// CB_moe = n_expert_used × U × n_ff × 4 = 2 × 64 × 512 × 4 = 262144
	if rep.ComputeMoEBytes != 262144 {
		t.Errorf("CB_moe = %d, want 262144", rep.ComputeMoEBytes)
	}
	if rep.ComputeBytes != 2746368+262144 {
		t.Errorf("CB = %d, want the dense figure plus CB_moe", rep.ComputeBytes)
	}
}

// TestSlidingWindowShrinksTheCache: a Gemma-3-shaped model is mis-sized by an
// order of magnitude without the SWA term (D30).
func TestSlidingWindowShrinksTheCache(t *testing.T) {
	m := tinyShape()
	m.Arch = "gemma3"
	m.NLayer = 6
	m.NHeadKV = []int{8, 8, 8, 8, 8, 8}
	m.LayerBytes = []uint64{mib, mib, mib, mib, mib, mib}
	m.SWAWindow, m.SWAPattern = intp(512), intp(6)

	f := tinyFlags()
	f.NCtx = 8192
	f.NUbatch = 64

	rep := Estimate(Request{
		Model: m, Flags: f, Devices: oneDevice(4 * gib),
		Host: Host{RAMFreeBytes: 16 * gib, RAMKnown: true}, MarginMiB: MiB(testMarginMiB),
	})

	// per_tok = 1024 bytes on every layer. Layers 0..4 slide, layer 5 is global.
	//   KV_full = 8192 × 1 × 1024                   =  8388608
	//   KV_swa  = min(8192, 512 + 64) × 5 × 1024    =  2949120
	if rep.KVBytes != 8388608 {
		t.Errorf("KV_full = %d, want 8388608", rep.KVBytes)
	}
	if rep.KVSWABytes != 2949120 {
		t.Errorf("KV_swa = %d, want 2949120", rep.KVSWABytes)
	}
	if rep.Inputs.NLayerSWA != 5 {
		t.Errorf("n_layer_swa = %d, want 5", rep.Inputs.NLayerSWA)
	}

	// Without the SWA term the same model would be charged 6 × 8388608 =
	// 50331648 bytes of cache — six times what it really needs.
	if total := rep.KVBytes + rep.KVSWABytes; total != 11337728 {
		t.Errorf("KV total = %d, want 11337728", total)
	}
}

// TestWindowWithNoPatternIsSizedAsFullAttention is the conservative reading of
// section 8.3, and the report says so rather than quietly assuming.
func TestWindowWithNoPatternIsSizedAsFullAttention(t *testing.T) {
	m := tinyShape()
	m.SWAWindow, m.SWAPattern = intp(512), nil

	rep := Estimate(Request{
		Model: m, Flags: tinyFlags(), Devices: oneDevice(gib), MarginMiB: MiB(testMarginMiB),
	})
	if rep.KVSWABytes != 0 || rep.KVBytes != 4*mib {
		t.Errorf("KV = %d full / %d swa, want the full-attention figure %d / 0",
			rep.KVBytes, rep.KVSWABytes, 4*mib)
	}
	if !hasNote(rep.Notes, "no sliding_window_pattern") {
		t.Errorf("the ignored window must be reported: %v", rep.Notes)
	}
	if rep.Confidence != ConfidenceModeled {
		t.Errorf("confidence = %q, want modeled", rep.Confidence)
	}
}

// TestPerGPUVerdictOnAnAsymmetricPair is section 8.7's headline case, and the
// reason there is no scalar `Required`: 23 GB + 4 GB has enough VRAM in TOTAL
// for this model and cannot hold it, because llama.cpp does not pool VRAM
// across devices.
func TestPerGPUVerdictOnAnAsymmetricPair(t *testing.T) {
	layers := make([]uint64, 20)
	for i := range layers {
		layers[i] = gib
	}
	m := ModelShape{
		Arch: "llama", NLayer: 20, NEmbd: 4096, NFF: 11008, NHead: 32,
		NHeadKV: repeatInt(8, 20), HeadDimK: 128, HeadDimV: 128,
		NVocab: 32000, NCtxTrain: 8192,
		LayerBytes: layers, OtherBytes: gib,
	}
	f := Flags{
		NCtx: 4096, NUbatch: 512, NParallel: 1, NGL: NGLAll,
		FlashAttn: FlashAttnOn, TypeK: CacheTypeF16, TypeV: CacheTypeF16,
		SplitMode: SplitLayer,
	}
	devs := []Device{
		{Index: 0, UUID: "GPU-big", Name: "24 GB card", TotalBytes: 24 * gib, FreeBytes: 23 * gib, Known: true},
		{Index: 1, UUID: "GPU-small", Name: "4 GB card", TotalBytes: 4 * gib, FreeBytes: 4 * gib, Known: true},
	}

	rep := Estimate(Request{
		Model: m, Flags: f, Devices: devs,
		Host: Host{RAMFreeBytes: 2 * gib, RAMTotalBytes: 4 * gib, RAMKnown: true},
	})

	if rep.Verdict == VerdictFits {
		t.Fatalf("a Σ-free-VRAM test would say fits here; the per-GPU test must not")
	}
	if rep.Verdict != VerdictWontRun {
		t.Errorf("verdict = %q, want wont_run: nothing places and the spill will not fit in 2 GiB RAM",
			rep.Verdict)
	}
	if len(rep.PerGPU) != 2 {
		t.Fatalf("want two per_gpu rows, got %d", len(rep.PerGPU))
	}
	if !rep.PerGPU[0].OK {
		t.Errorf("the 23 GB card should hold its share: %+v", rep.PerGPU[0])
	}
	if rep.PerGPU[1].OK {
		t.Errorf("the 4 GB card cannot hold its share: %+v", rep.PerGPU[1])
	}
	if rep.PerGPU[1].ShortByBytes == 0 {
		t.Error("a device that is over must report by how much")
	}
	// The proof that the sum would have lied: Σ assigned ≤ Σ free.
	sumFree := 23*gib + 4*gib
	if rep.RequiredVRAMBytes > sumFree {
		t.Fatalf("this case only bites when Σ assigned (%d) ≤ Σ free (%d)",
			rep.RequiredVRAMBytes, sumFree)
	}
	if !hasNote(rep.Notes, "GPU 1 is short by") {
		t.Errorf("notes must name the device that is short: %v", rep.Notes)
	}
	if !hasNote(rep.Notes, "--tensor-split") {
		t.Errorf("notes should suggest the split that would fix it: %v", rep.Notes)
	}
}

// TestAutoResolvesToMaxPlaceable is D51: `auto` is resolved to a layer count
// HERE and nowhere else, so the UI has a number and `pin-ngl` has a value — and
// the report says outright that no -ngl flag is rendered.
func TestAutoResolvesToMaxPlaceable(t *testing.T) {
	layers := make([]uint64, 20)
	for i := range layers {
		layers[i] = 512 * mib
	}
	m := tinyShape()
	m.NLayer, m.LayerBytes = 20, layers
	m.NHeadKV = repeatInt(8, 20)

	f := tinyFlags()
	f.NGL = NGLAuto

	rep := Estimate(Request{
		Model: m, Flags: f,
		Devices:   oneDevice(6 * gib),
		Host:      Host{RAMFreeBytes: 64 * gib, RAMKnown: true},
		MarginMiB: MiB(testMarginMiB),
	})

	if rep.MaxNGpuLayers <= 0 || rep.MaxNGpuLayers >= m.OffloadSteps() {
		t.Fatalf("max_n_gpu_layers = %d, want a partial offload on a 6 GiB card",
			rep.MaxNGpuLayers)
	}
	if rep.NGpuLayers != rep.MaxNGpuLayers {
		t.Errorf("auto resolved to %d but max is %d", rep.NGpuLayers, rep.MaxNGpuLayers)
	}
	if !hasNote(rep.Notes, "renders no -ngl flag") {
		t.Errorf("the advisory nature of auto must be stated: %v", rep.Notes)
	}
	// One more layer must genuinely not place — otherwise the "max" is not one.
	over := f
	over.NGL, over.NGLCount = NGLCount, rep.MaxNGpuLayers+1
	worse := Estimate(Request{
		Model: m, Flags: over, Devices: oneDevice(6 * gib),
		Host: Host{RAMFreeBytes: 64 * gib, RAMKnown: true}, MarginMiB: MiB(testMarginMiB),
	})
	for _, g := range worse.PerGPU {
		if g.OK {
			t.Errorf("max_n_gpu_layers + 1 still places: %+v", g)
		}
	}
}

// TestReserveIsPerGPUAndNeverDivided is section 3.9's contract: the reserve is
// charged to EVERY selected device, exactly like margin and OH_gpu.
func TestReserveIsPerGPUAndNeverDivided(t *testing.T) {
	devs := []Device{
		{Index: 0, UUID: "GPU-a", TotalBytes: gib, FreeBytes: gib, Known: true},
		{Index: 1, UUID: "GPU-b", TotalBytes: gib, FreeBytes: gib, Known: true},
	}
	const reserve = 256 * 1024 * 1024

	rep := Estimate(Request{
		Model: tinyShape(), Flags: tinyFlags(), Devices: devs,
		Host:      Host{RAMFreeBytes: 16 * gib, RAMKnown: true},
		MarginMiB: MiB(testMarginMiB), ReserveBytesPerGPU: reserve,
	})

	if rep.ReserveBytesPerGPU != reserve {
		t.Errorf("reserve_bytes_per_gpu = %d, want %d", rep.ReserveBytesPerGPU, reserve)
	}
	if rep.ReserveBytes != 2*reserve {
		t.Errorf("reserve_bytes = %d, want %d (× participating GPUs)", rep.ReserveBytes, 2*reserve)
	}
	for _, g := range rep.PerGPU {
		if g.ReserveBytes != reserve {
			t.Errorf("GPU %d was charged %d of reserve, want the full %d", g.Index, g.ReserveBytes, reserve)
		}
	}
	// The reserve is subtracted from FREE, not added to `assigned` (section
	// 8.4's assigned has five terms and the reserve is not one of them). The
	// test for that is a device whose free VRAM covers `assigned` but not
	// `assigned + reserve`: it must fail.
	single := Estimate(Request{
		Model: tinyShape(), Flags: tinyFlags(), Devices: oneDevice(gib),
		Host: Host{RAMFreeBytes: 16 * gib, RAMKnown: true}, MarginMiB: MiB(testMarginMiB),
	})
	tight := single.PerGPU[0].AssignedBytes
	edge := Estimate(Request{
		Model: tinyShape(), Flags: tinyFlags(),
		Devices: []Device{{
			Index: 0, UUID: "GPU-a",
			TotalBytes: tight + reserve/2, FreeBytes: tight + reserve/2, Known: true,
		}},
		Host:      Host{RAMFreeBytes: 16 * gib, RAMKnown: true},
		MarginMiB: MiB(testMarginMiB), ReserveBytesPerGPU: reserve,
	})
	if edge.PerGPU[0].OK {
		t.Errorf("assigned %d fits in %d free only if the reserve of %d was ignored",
			tight, tight+reserve/2, reserve)
	}
	if edge.PerGPU[0].AssignedBytes != tight {
		t.Errorf("assigned = %d, want %d — the reserve must not be folded into it",
			edge.PerGPU[0].AssignedBytes, tight)
	}
}

// TestUnknownVRAMIsNeverTreatedAsZeroOrUnlimited is D16 at the calculator's end
// of the boundary: a device whose driver could not be read is unplaceable AND
// flagged, so the UI says "unknown" rather than "won't run".
func TestUnknownVRAMIsNeverTreatedAsZeroOrUnlimited(t *testing.T) {
	devs := []Device{{Index: 0, UUID: "GPU-a", Name: "Unreadable", Known: false}}
	rep := Estimate(Request{
		Model: tinyShape(), Flags: tinyFlags(), Devices: devs,
		Host: Host{RAMFreeBytes: 16 * gib, RAMKnown: true}, MarginMiB: MiB(testMarginMiB),
	})
	if !rep.VRAMUnknown {
		t.Fatal("the report must flag that a device's memory was not measured")
	}
	if rep.PerGPU[0].OK {
		t.Error("an unmeasured device must not be reported as placeable")
	}
	if rep.PerGPU[0].FreeBytes != nil {
		t.Error("free_bytes must be null, never 0, for an unmeasured device")
	}
	if rep.Confidence != ConfidenceModeled {
		t.Errorf("confidence = %q, want modeled", rep.Confidence)
	}
	if !hasNote(rep.Notes, "could not be read") {
		t.Errorf("notes must explain: %v", rep.Notes)
	}
}

// TestCPUOnlyEstimate: no GPU is an ordinary configuration, not an error, and
// `fits` must not be reachable by a `∀` over an empty device set.
func TestCPUOnlyEstimate(t *testing.T) {
	f := tinyFlags()
	f.NGL = NGLAll
	rep := Estimate(Request{
		Model: tinyShape(), Flags: f, Devices: nil,
		Host: Host{RAMFreeBytes: 16 * gib, RAMKnown: true},
	})
	if rep.Verdict != VerdictPartial {
		t.Errorf("verdict = %q, want partial for a CPU-only host", rep.Verdict)
	}
	if rep.SpillToRAMBytes != 6*mib+4*mib {
		t.Errorf("spill = %d, want the whole model and its cache (%d)",
			rep.SpillToRAMBytes, 10*mib)
	}
	if rep.RequiredVRAMBytes != 0 || len(rep.PerGPU) != 0 {
		t.Errorf("no GPU means no VRAM requirement: %d / %d rows",
			rep.RequiredVRAMBytes, len(rep.PerGPU))
	}
	if rep.MarginBytes != 0 {
		t.Errorf("margin_bytes = %d, want 0 with no participating GPU", rep.MarginBytes)
	}
}

// TestSpillMustFitRAM: `partial` requires the spill to be within 90% of free
// system RAM. Beyond that the honest answer is wont_run, not "it will swap".
func TestSpillMustFitRAM(t *testing.T) {
	m := tinyShape()
	f := tinyFlags()
	f.NGL = NGLNone

	ample := Estimate(Request{
		Model: m, Flags: f, Devices: oneDevice(gib),
		Host: Host{RAMFreeBytes: 16 * gib, RAMKnown: true}, MarginMiB: MiB(testMarginMiB),
	})
	if ample.Verdict != VerdictPartial {
		t.Errorf("with 16 GiB free the spill fits: verdict = %q", ample.Verdict)
	}

	tight := Estimate(Request{
		Model: m, Flags: f, Devices: oneDevice(gib),
		Host: Host{RAMFreeBytes: 4 * mib, RAMKnown: true}, MarginMiB: MiB(testMarginMiB),
	})
	if tight.Verdict != VerdictWontRun {
		t.Errorf("with 4 MiB free a 10 MiB spill cannot run: verdict = %q", tight.Verdict)
	}
	if !hasNote(tight.Notes, "spill to system RAM") {
		t.Errorf("notes must explain the RAM refusal: %v", tight.Notes)
	}
}

// TestCalibrationChangesTheEstimateAndTheConfidence is D32 end to end.
func TestCalibrationChangesTheEstimateAndTheConfidence(t *testing.T) {
	base := Estimate(Request{
		Model: tinyShape(), Flags: tinyFlags(), Devices: oneDevice(gib),
		MarginMiB: MiB(testMarginMiB),
	})
	cal := NewCalibration([]Observation{
		{PredictedBytes: 1000, ActualBytes: 1500},
		{PredictedBytes: 1000, ActualBytes: 1500},
		{PredictedBytes: 1000, ActualBytes: 1500},
	})
	tuned := Estimate(Request{
		Model: tinyShape(), Flags: tinyFlags(), Devices: oneDevice(gib),
		MarginMiB: MiB(testMarginMiB), Calibration: cal,
	})

	// k_act 6 → 9: CB_act 393216 → 589824. The other three terms are unchanged.
	if tuned.ComputeActBytes != 589824 {
		t.Errorf("calibrated CB_act = %d, want 589824", tuned.ComputeActBytes)
	}
	if tuned.ComputeLogitsBytes != base.ComputeLogitsBytes {
		t.Error("only the activation term is scaled by k_act")
	}
	// OH_gpu 400 MiB → 600 MiB.
	if tuned.BackendOverheadBytes != 600*mib {
		t.Errorf("calibrated OH_gpu = %d, want %d", tuned.BackendOverheadBytes, 600*mib)
	}
	if tuned.Confidence != ConfidenceCalibrated {
		t.Errorf("confidence = %q, want calibrated", tuned.Confidence)
	}
	if base.Confidence != ConfidenceModeled {
		t.Errorf("uncalibrated confidence = %q, want modeled", base.Confidence)
	}
}

// TestModeledAssumptionDefeatsCalibratedConfidence: a report that had to guess
// at its own inputs is not "calibrated", whatever the observation table says.
func TestModeledAssumptionDefeatsCalibratedConfidence(t *testing.T) {
	m := tinyShape()
	m.LayerBytes, m.FileBytes = nil, 6*mib // the pre-download fallback
	cal := Calibration{Ratio: 1.2, Applied: true, Samples: 5}

	rep := Estimate(Request{
		Model: m, Flags: tinyFlags(), Devices: oneDevice(gib),
		MarginMiB: MiB(testMarginMiB), Calibration: cal,
	})
	if rep.Confidence != ConfidenceModeled {
		t.Errorf("confidence = %q, want modeled after the averaging fallback", rep.Confidence)
	}
	if !hasNote(rep.Notes, "tensor index is not available") {
		t.Errorf("the fallback must be reported: %v", rep.Notes)
	}
	// W_gpu ≈ file_bytes × n/(L+1): five buckets of 6 MiB / 5.
	if rep.WeightsBytes != 6*mib {
		t.Errorf("W_total = %d, want the file size %d", rep.WeightsBytes, 6*mib)
	}
}

// TestMaxCtxAtFullOffload is a binary search over the same pure function, and
// its answer must be a real boundary: the returned context places, the next
// 256-token step does not.
func TestMaxCtxAtFullOffload(t *testing.T) {
	m := tinyShape()
	m.NCtxTrain = 131072
	f := tinyFlags()
	f.NGL = NGLAll

	// A card with room for the model and a bounded amount of cache.
	rep := Estimate(Request{
		Model: m, Flags: f, Devices: oneDevice(512*mib + OverheadPerGPUBytes),
		Host: Host{RAMFreeBytes: 16 * gib, RAMKnown: true}, MarginMiB: MiB(testMarginMiB),
	})
	c := rep.MaxCtxAtFullOffload
	if c <= 0 {
		t.Fatalf("max_ctx_at_full_offload = %d, want a positive boundary", c)
	}
	if c%KVPad != 0 {
		t.Errorf("max_ctx_at_full_offload = %d, want a multiple of %d", c, KVPad)
	}

	at := f
	at.NCtx = c
	if v := Estimate(Request{
		Model: m, Flags: at, Devices: oneDevice(512*mib + OverheadPerGPUBytes),
		Host: Host{RAMFreeBytes: 16 * gib, RAMKnown: true}, MarginMiB: MiB(testMarginMiB),
	}); v.Verdict != VerdictFits {
		t.Errorf("the reported context %d does not actually fit: %q %v", c, v.Verdict, v.Notes)
	}
	if c < m.NCtxTrain {
		over := f
		over.NCtx = c + KVPad
		if v := Estimate(Request{
			Model: m, Flags: over, Devices: oneDevice(512*mib + OverheadPerGPUBytes),
			Host: Host{RAMFreeBytes: 16 * gib, RAMKnown: true}, MarginMiB: MiB(testMarginMiB),
		}); v.Verdict == VerdictFits {
			t.Errorf("context %d also fits, so %d is not the maximum", c+KVPad, c)
		}
	}
}

// TestRecommendationLadder walks section 8.7's order: full offload first, then
// a q8_0 cache with flash attention, then fewer layers.
func TestRecommendationLadder(t *testing.T) {
	m := tinyShape()
	m.NHeadKV = []int{8, 8, 8, 8}

	t.Run("everything fits: recommend full offload as configured", func(t *testing.T) {
		rep := Estimate(Request{
			Model: m, Flags: tinyFlags(), Devices: oneDevice(gib), MarginMiB: MiB(testMarginMiB),
		})
		if rep.Recommendation.NGpuLayers != m.OffloadSteps() {
			t.Errorf("recommendation = %+v, want full offload", rep.Recommendation)
		}
		if rep.Recommendation.TypeK != CacheTypeF16 {
			t.Errorf("nothing needs changing, so the cache type should be left alone: %+v",
				rep.Recommendation)
		}
	})

	t.Run("a quantized cache is tried before dropping layers", func(t *testing.T) {
		big := m
		big.NHeadKV = repeatInt(64, 4)
		f := tinyFlags()
		f.NCtx = 32768
		// Sized so f16 does not fit and q8_0 does.
		//   per_tok(f16)  = 64 × (32×2 + 32×2)          = 8192
		//   KV(f16)       = 32768 × 4 × 8192            = 1073741824 = 1 GiB
		//   per_tok(q8_0) = 2 × ceil(2048/32) × 34      = 4352
		//   KV(q8_0)      = 32768 × 4 × 4352            =  570425344
		rep := Estimate(Request{
			Model: big, Flags: f,
			Devices:   oneDevice(OverheadPerGPUBytes + 800*mib),
			Host:      Host{RAMFreeBytes: 64 * gib, RAMKnown: true},
			MarginMiB: MiB(testMarginMiB),
		})
		r := rep.Recommendation
		if r.TypeK != CacheTypeQ8_0 || r.TypeV != CacheTypeQ8_0 || !r.FlashAttn {
			t.Fatalf("recommendation = %+v, want q8_0 with flash attention", r)
		}
		if r.NGpuLayers != big.OffloadSteps() {
			t.Errorf("the quantized cache should buy FULL offload here: %+v", r)
		}
	})
}

func hasNote(notes []string, want string) bool {
	for _, n := range notes {
		if strings.Contains(n, want) {
			return true
		}
	}
	return false
}

func repeatInt(v, n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = v
	}
	return out
}

// TestMmprojIsChargedToTheMainDevice: a paired projector is real VRAM the moment
// a user pairs it, and an estimate that ignored it would promise a fit that OOMs
// on exactly the configuration this product makes easy to build.
func TestMmprojIsChargedToTheMainDevice(t *testing.T) {
	m := tinyShape()
	m.MmprojBytes = 64 * mib

	base := Estimate(Request{
		Model: tinyShape(), Flags: tinyFlags(), Devices: oneDevice(gib),
		Host: Host{RAMFreeBytes: 16 * gib, RAMKnown: true}, MarginMiB: MiB(testMarginMiB),
	})
	with := Estimate(Request{
		Model: m, Flags: tinyFlags(), Devices: oneDevice(gib),
		Host: Host{RAMFreeBytes: 16 * gib, RAMKnown: true}, MarginMiB: MiB(testMarginMiB),
	})

	if got := with.PerGPU[0].AssignedBytes - base.PerGPU[0].AssignedBytes; got != 64*mib {
		t.Errorf("the projector added %d, want %d", got, 64*mib)
	}
	if with.PerGPU[0].ExtraBytes != 64*mib {
		t.Errorf("extra_bytes = %d, want the projector's %d",
			with.PerGPU[0].ExtraBytes, 64*mib)
	}
	if !hasNote(with.Notes, "multimodal projector") {
		t.Errorf("the charge must be explained: %v", with.Notes)
	}

	// With nothing offloaded the projector is in RAM, not on a device.
	cpu := tinyFlags()
	cpu.NGL = NGLNone
	onCPU := Estimate(Request{
		Model: m, Flags: cpu, Devices: oneDevice(gib),
		Host: Host{RAMFreeBytes: 16 * gib, RAMKnown: true}, MarginMiB: MiB(testMarginMiB),
	})
	if onCPU.PerGPU[0].ExtraBytes != 0 {
		t.Errorf("extra_bytes = %d on a CPU-only offload, want 0", onCPU.PerGPU[0].ExtraBytes)
	}
	if onCPU.SpillToRAMBytes < 64*mib {
		t.Errorf("the projector should be charged to RAM instead: spill = %d",
			onCPU.SpillToRAMBytes)
	}
}

// TestDraftModelIsChargedToo: speculative decoding loads a second model with a
// second cache, and both are VRAM on the same devices.
func TestDraftModelIsChargedToo(t *testing.T) {
	draft := tinyShape()
	draft.NLayer = 2
	draft.LayerBytes = []uint64{mib / 2, mib / 2}
	draft.OtherBytes = mib / 2
	draft.NHeadKV = []int{8, 8}

	m := tinyShape()
	m.Draft = &draft

	base := Estimate(Request{
		Model: tinyShape(), Flags: tinyFlags(), Devices: oneDevice(gib),
		Host: Host{RAMFreeBytes: 16 * gib, RAMKnown: true}, MarginMiB: MiB(testMarginMiB),
	})

	f := tinyFlags()
	// -ngld unset: the whole draft model is charged, which is the conservative
	// reading and the one the report explains.
	with := Estimate(Request{
		Model: m, Flags: f, Devices: oneDevice(gib),
		Host: Host{RAMFreeBytes: 16 * gib, RAMKnown: true}, MarginMiB: MiB(testMarginMiB),
	})

	// Draft weights: two layers of 512 KiB plus a 512 KiB head = 1.5 MiB.
	// Draft cache: per_tok 1024 × kv_ctx 1024 × 2 layers = 2 MiB.
	const wantDraft = 3*mib/2 + 2*mib
	if got := with.PerGPU[0].AssignedBytes - base.PerGPU[0].AssignedBytes; got != wantDraft {
		t.Errorf("the draft model added %d, want %d", got, wantDraft)
	}
	if !hasNote(with.Notes, "draft model") {
		t.Errorf("the charge must be explained: %v", with.Notes)
	}

	// Pinning -ngld 0 keeps the draft model off the GPU entirely.
	pinned := f
	zero := 0
	pinned.Draft.NGpuLayers = &zero
	none := Estimate(Request{
		Model: m, Flags: pinned, Devices: oneDevice(gib),
		Host: Host{RAMFreeBytes: 16 * gib, RAMKnown: true}, MarginMiB: MiB(testMarginMiB),
	})
	if none.PerGPU[0].AssignedBytes != base.PerGPU[0].AssignedBytes {
		t.Errorf("with -ngld 0 the draft model must cost no VRAM: %d vs %d",
			none.PerGPU[0].AssignedBytes, base.PerGPU[0].AssignedBytes)
	}
}
