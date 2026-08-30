package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"testing"

	"github.com/jlbyh2o/llamaman/internal/api/middleware"
	"github.com/jlbyh2o/llamaman/internal/fit"
	"github.com/jlbyh2o/llamaman/internal/gguf"
	"github.com/jlbyh2o/llamaman/internal/gguf/ggufbuild"
	"github.com/jlbyh2o/llamaman/internal/hf"
	"github.com/jlbyh2o/llamaman/internal/hw"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/models"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// Handler tests for the fit endpoints of DESIGN section 3.9.
//
// What this layer owns is the wire contract, and three parts of it would be
// worst to get wrong:
//
//   - `required_vram_bytes` is Σ `per_gpu[].assigned_bytes` and the verdict is
//     `∀ g: per_gpu[g].ok` — never a comparison against that sum;
//   - `reserve_bytes_per_gpu` is echoed per device and `reserve_bytes` is the
//     total, so nothing downstream has to infer the unit;
//   - a GPU whose memory could not be read serializes `free_bytes: null`, never
//     0, because a 0 would make the UI say "won't run" about a measurement
//     nobody made.

const testMiB = uint64(1) << 20

// stubModels is a ModelService whose only interesting method is Get.
type stubModels struct {
	detail models.Detail
	err    error
	gotID  string
}

func (s *stubModels) List(context.Context, models.ListParams) ([]models.View, error) {
	return nil, nil
}

func (s *stubModels) Get(_ context.Context, id string) (models.Detail, error) {
	s.gotID = id
	return s.detail, s.err
}

func (s *stubModels) Metadata(context.Context, string) (map[string]any, error) { return nil, nil }

func (s *stubModels) DeletePreview(context.Context, string) (models.DeletePlan, error) {
	return models.DeletePlan{}, nil
}

func (s *stubModels) Delete(context.Context, string) (models.DeletePlan, models.JobRef, error) {
	return models.DeletePlan{}, models.JobRef{}, nil
}

func (s *stubModels) Verify(context.Context, string) (models.JobRef, error) {
	return models.JobRef{}, nil
}

func (s *stubModels) PairMmproj(context.Context, string, string) (models.View, error) {
	return models.View{}, nil
}

func (s *stubModels) Roots(context.Context) ([]models.RootView, error) { return nil, nil }

func (s *stubModels) AddRoot(context.Context, string) (models.RootView, models.JobRef, error) {
	return models.RootView{}, models.JobRef{}, nil
}

func (s *stubModels) PromoteRoot(context.Context, string) (models.RootView, models.JobRef, error) {
	return models.RootView{}, models.JobRef{}, nil
}

func (s *stubModels) DetachRoot(context.Context, string) error { return nil }

func (s *stubModels) RequestScan(context.Context, string, model.CacheScanTrigger) (
	model.CacheScan, models.JobRef, error) {
	return model.CacheScan{}, models.JobRef{}, nil
}

func (s *stubModels) Scan(context.Context, string) (model.CacheScan, error) {
	return model.CacheScan{}, nil
}

func (s *stubModels) Strays(context.Context, string) ([]model.StrayFile, error) { return nil, nil }

func (s *stubModels) DeleteStray(context.Context, string, bool) error { return nil }

func (s *stubModels) DismissStray(context.Context, string) error { return nil }

// stubHardware is the host probe.
type stubHardware struct {
	gpus []hw.GPU
	mem  hw.Memory
	err  error
}

func (s stubHardware) Probe(context.Context) ([]hw.GPU, error) { return s.gpus, s.err }

func (s stubHardware) Memory() (hw.Memory, error) { return s.mem, nil }

// stubCalibration is D32's observation source.
type stubCalibration struct {
	tag     string
	backend model.Backend
	obs     []fit.Observation
	gotKey  model.FitCalibrationKey
}

func (s *stubCalibration) ActiveRuntime(context.Context) (string, model.Backend, error) {
	return s.tag, s.backend, nil
}

func (s *stubCalibration) Observations(_ context.Context, key model.FitCalibrationKey,
	_ int) ([]fit.Observation, error) {
	s.gotKey = key
	return s.obs, nil
}

// fitModel is the `models` row every case below measures: four 1 MiB layers, a
// 2 MiB output head, and the geometry the golden arithmetic in internal/fit is
// written against.
func fitModel() models.Detail {
	sizes := gguf.Sizes{
		TensorCount: 20,
		Total:       6 * testMiB,
		Layer:       []uint64{testMiB, testMiB, testMiB, testMiB},
		Other:       2 * testMiB,
	}
	blob, _ := json.Marshal(sizes)
	summary := string(blob)
	parsed := int64(1788042587000)

	return models.Detail{View: models.View{LocalModel: model.LocalModel{
		ID: "m-1", RepoID: "acme/tiny-GGUF", PrimaryFile: "tiny-Q4_K_M.gguf",
		GGUFParsedAt: &parsed,
		Arch:         ptrOf("llama"),
		NLayer:       ptrOf(int64(4)),
		NEmbd:        ptrOf(int64(256)),
		NFF:          ptrOf(int64(512)),
		NHead:        ptrOf(int64(8)),
		NHeadKVJSON:  ptrOf("8"),
		HeadDimK:     ptrOf(int64(32)),
		HeadDimV:     ptrOf(int64(32)),
		NCtxTrain:    ptrOf(int64(4096)),
		NVocab:       ptrOf(int64(1000)),
		TotalBytes:   int64(6 * testMiB),

		TensorSummaryJSON: &summary,
	}}}
}

func gpu(index int, uuid, name string, total, free uint64) hw.GPU {
	return hw.GPU{
		Index: index, UUID: uuid, Name: name,
		VRAMTotalBytes: hw.Bytes(total),
		VRAMUsedBytes:  hw.Bytes(total - free),
		VRAMFreeBytes:  hw.Bytes(free),
	}
}

func fitAPI(t *testing.T, cfg Config) *API {
	t.Helper()
	cfg.Auth = stubAuth{
		complete: true,
		session:  &middleware.Session{ID: "s-1"},
		csrfOK:   true,
	}
	return newTestAPI(t, cfg)
}

func decodeFit(t *testing.T, body []byte) FitReportDTO {
	t.Helper()
	var out FitReportDTO
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode report: %v\n%s", err, body)
	}
	return out
}

// TestFitEstimateForALocalModel is the happy path, and it asserts the two
// invariants a consumer must be able to rely on: the total is a sum of the rows,
// and the verdict is the conjunction of their `ok`.
func TestFitEstimateForALocalModel(t *testing.T) {
	t.Parallel()

	ms := &stubModels{detail: fitModel()}
	a := fitAPI(t, Config{
		Models: ms,
		Hardware: stubHardware{
			gpus: []hw.GPU{gpu(0, "GPU-a", "Test GPU", 8<<30, 8<<30)},
			mem:  hw.Memory{TotalBytes: 32 << 30, AvailableBytes: 24 << 30, Known: true},
		},
	})

	w := do(t, a, http.MethodPost, "/api/v1/fit/estimate", `{
		"source": {"model_id": "m-1"},
		"flags": {"ctx_size": 1024, "ubatch_size": 64, "parallel": 1,
		          "n_gpu_layers": {"mode": "all"}, "flash_attn": "off",
		          "cache_type_k": "f16", "cache_type_v": "f16"}
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body)
	}
	if ms.gotID != "m-1" {
		t.Errorf("the handler asked for %q", ms.gotID)
	}

	rep := decodeFit(t, w.Body.Bytes())
	if rep.ModelID != "m-1" {
		t.Errorf("model_id = %q, want the source echoed back", rep.ModelID)
	}
	if rep.Verdict != string(fit.VerdictFits) {
		t.Fatalf("verdict = %q, want fits (%v)", rep.Verdict, rep.Notes)
	}

	// The arithmetic itself is pinned in internal/fit; what this asserts is
	// that the DTO carries it through unmangled.
	if rep.WeightsBytes != 6*testMiB || rep.KVBytes != 4*testMiB {
		t.Errorf("weights/kv = %d/%d, want %d/%d",
			rep.WeightsBytes, rep.KVBytes, 6*testMiB, 4*testMiB)
	}
	if rep.ComputeBytes != 2746368 {
		t.Errorf("compute = %d, want 2746368", rep.ComputeBytes)
	}
	if rep.Inputs.NLayer != 4 || len(rep.Inputs.NHeadKV) != 4 {
		t.Errorf("inputs = %+v, want the model's geometry echoed", rep.Inputs)
	}
	if rep.Inputs.KVCtx != 1024 {
		t.Errorf("kv_ctx = %d, want 1024", rep.Inputs.KVCtx)
	}
	if rep.PerSlotCtx != 1024 {
		t.Errorf("per_slot_ctx = %d, want 1024", rep.PerSlotCtx)
	}

	if len(rep.PerGPU) != 1 {
		t.Fatalf("per_gpu has %d rows", len(rep.PerGPU))
	}
	g := rep.PerGPU[0]
	if !g.OK {
		t.Errorf("per_gpu[0].ok = false on an 8 GiB card: %+v", g)
	}
	if g.FreeBytes == nil || *g.FreeBytes != 8<<30 {
		t.Errorf("free_bytes = %v, want the probed figure", g.FreeBytes)
	}
	if rep.RequiredVRAMBytes != g.AssignedBytes {
		t.Errorf("required_vram_bytes = %d but the only row is assigned %d; "+
			"the total must be Σ per_gpu[].assigned_bytes",
			rep.RequiredVRAMBytes, g.AssignedBytes)
	}
	if rep.SystemRAMFreeBytes != 24<<30 || !rep.SystemRAMKnown {
		t.Errorf("system RAM = %d (known %v), want the probed figure",
			rep.SystemRAMFreeBytes, rep.SystemRAMKnown)
	}
	if rep.Confidence != string(fit.ConfidenceModeled) {
		t.Errorf("confidence = %q, want modeled with no observations", rep.Confidence)
	}
}

// TestFitReserveIsEchoedPerGPUAndAsATotal is section 3.9's naming contract: the
// reserve is per participating GPU and is never divided, and both forms are on
// the wire so nothing has to infer the unit.
func TestFitReserveIsEchoedPerGPUAndAsATotal(t *testing.T) {
	t.Parallel()

	a := fitAPI(t, Config{
		Models: &stubModels{detail: fitModel()},
		Hardware: stubHardware{
			gpus: []hw.GPU{
				gpu(0, "GPU-a", "A", 8<<30, 8<<30),
				gpu(1, "GPU-b", "B", 8<<30, 8<<30),
			},
			mem: hw.Memory{TotalBytes: 32 << 30, AvailableBytes: 24 << 30, Known: true},
		},
	})

	const reserve = 512 * (1 << 20)
	w := do(t, a, http.MethodPost, "/api/v1/fit/estimate", `{
		"source": {"model_id": "m-1"},
		"flags": {"ctx_size": 1024, "n_gpu_layers": {"mode": "all"}},
		"reserve_bytes_per_gpu": 536870912
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body)
	}
	rep := decodeFit(t, w.Body.Bytes())

	if rep.ReserveBytesPerGPU != reserve {
		t.Errorf("reserve_bytes_per_gpu = %d, want %d", rep.ReserveBytesPerGPU, reserve)
	}
	if rep.ReserveBytes != 2*reserve {
		t.Errorf("reserve_bytes = %d, want %d (× participating GPUs)",
			rep.ReserveBytes, 2*reserve)
	}
	if rep.MarginBytes != 2*rep.MarginBytesPerGPU {
		t.Errorf("margin_bytes = %d, want twice the per-GPU figure %d",
			rep.MarginBytes, rep.MarginBytesPerGPU)
	}
	for _, g := range rep.PerGPU {
		if g.ReserveBytes != reserve {
			t.Errorf("GPU %d was charged %d of reserve, want the undivided %d",
				g.Index, g.ReserveBytes, reserve)
		}
	}
}

// TestFitUnknownVRAMSerializesAsNull is F14 at the wire boundary. A fabricated
// zero here is the failure D16 exists to prevent: every verdict becomes
// `wont_run` with nothing to say that no measurement was made.
func TestFitUnknownVRAMSerializesAsNull(t *testing.T) {
	t.Parallel()

	a := fitAPI(t, Config{
		Models: &stubModels{detail: fitModel()},
		Hardware: stubHardware{
			gpus: []hw.GPU{{Index: 0, UUID: "GPU-a", Name: "Unreadable"}},
			mem:  hw.Memory{TotalBytes: 32 << 30, AvailableBytes: 24 << 30, Known: true},
		},
	})

	w := do(t, a, http.MethodPost, "/api/v1/fit/estimate",
		`{"source": {"model_id": "m-1"}, "flags": {"ctx_size": 1024}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body)
	}

	var raw struct {
		VRAMUnknown bool `json:"vram_unknown"`
		PerGPU      []struct {
			FreeBytes  *uint64 `json:"free_bytes"`
			TotalBytes *uint64 `json:"total_bytes"`
			OK         bool    `json:"ok"`
		} `json:"per_gpu"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !raw.VRAMUnknown {
		t.Error("vram_unknown must be set so the UI can say `unknown` rather than `won't run`")
	}
	if len(raw.PerGPU) != 1 {
		t.Fatalf("per_gpu has %d rows", len(raw.PerGPU))
	}
	if raw.PerGPU[0].FreeBytes != nil || raw.PerGPU[0].TotalBytes != nil {
		t.Errorf("free/total = %v/%v, want null — never 0",
			raw.PerGPU[0].FreeBytes, raw.PerGPU[0].TotalBytes)
	}
}

// TestFitSourceValidation covers the three ways a source can be unusable.
func TestFitSourceValidation(t *testing.T) {
	t.Parallel()

	unparsed := fitModel()
	unparsed.GGUFParsedAt = nil

	cases := []struct {
		name     string
		cfg      Config
		body     string
		want     int
		wantCode string
	}{
		{
			name: "neither a model nor a repository file",
			cfg:  Config{Models: &stubModels{detail: fitModel()}},
			body: `{"source": {}, "flags": {}}`,
			want: http.StatusUnprocessableEntity, wantCode: string(model.CodeBadFlags),
		},
		{
			name: "a model id that names no row",
			cfg:  Config{Models: &stubModels{err: store.ErrNotFound}},
			body: `{"source": {"model_id": "nope"}, "flags": {}}`,
			want: http.StatusNotFound, wantCode: string(CodeNotFound),
		},
		{
			name: "a model whose header has not been parsed cannot be measured",
			cfg:  Config{Models: &stubModels{detail: unparsed}},
			body: `{"source": {"model_id": "m-1"}, "flags": {}}`,
			want: http.StatusConflict, wantCode: string(CodeFitUnavailable),
		},
		{
			name: "flags that fail their own validation",
			cfg:  Config{Models: &stubModels{detail: fitModel()}},
			body: `{"source": {"model_id": "m-1"}, "flags": {"ctx_size": -1}}`,
			want: http.StatusUnprocessableEntity, wantCode: string(model.CodeBadFlags),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := fitAPI(t, tc.cfg)
			w := do(t, a, http.MethodPost, "/api/v1/fit/estimate", tc.body)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d; body %s", w.Code, tc.want, w.Body)
			}
			if got := errorCode(t, w); got != tc.wantCode {
				t.Errorf("code = %q, want %q", got, tc.wantCode)
			}
		})
	}
}

// TestFitCalibrationReachesTheCalculator is D32 through the wire: the key is
// `(arch, backend, llamacpp_tag)`, and a correction in force makes the report
// say `calibrated` rather than `modeled`.
func TestFitCalibrationReachesTheCalculator(t *testing.T) {
	t.Parallel()

	cal := &stubCalibration{
		tag: "b10621", backend: model.BackendCUDA,
		obs: []fit.Observation{
			{PredictedBytes: 1000, ActualBytes: 1500},
			{PredictedBytes: 1000, ActualBytes: 1500},
			{PredictedBytes: 1000, ActualBytes: 1500},
		},
	}
	a := fitAPI(t, Config{
		Models: &stubModels{detail: fitModel()},
		Hardware: stubHardware{
			gpus: []hw.GPU{gpu(0, "GPU-a", "A", 8<<30, 8<<30)},
			mem:  hw.Memory{TotalBytes: 32 << 30, AvailableBytes: 24 << 30, Known: true},
		},
		FitCalibration: cal,
	})

	w := do(t, a, http.MethodPost, "/api/v1/fit/estimate", `{
		"source": {"model_id": "m-1"},
		"flags": {"ctx_size": 1024, "ubatch_size": 64, "n_gpu_layers": {"mode": "all"},
		          "flash_attn": "off"}
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body)
	}
	rep := decodeFit(t, w.Body.Bytes())

	if cal.gotKey != (model.FitCalibrationKey{
		Arch: "llama", Backend: model.BackendCUDA, LlamacppTag: "b10621",
	}) {
		t.Errorf("calibration key = %+v", cal.gotKey)
	}
	if rep.Confidence != string(fit.ConfidenceCalibrated) {
		t.Fatalf("confidence = %q, want calibrated", rep.Confidence)
	}
	if rep.CalibrationSamples != 3 {
		t.Errorf("calibration_samples = %d, want 3", rep.CalibrationSamples)
	}
	// k_act 6 → 9 at a ratio of 1.5: CB_act 393216 → 589824.
	if rep.ComputeActBytes != 589824 {
		t.Errorf("CB_act = %d, want the corrected 589824", rep.ComputeActBytes)
	}
	if rep.BackendOverheadBytes != 600*testMiB {
		t.Errorf("OH_gpu = %d, want the corrected %d", rep.BackendOverheadBytes, 600*testMiB)
	}
}

// TestFitEstimateBatchDrivesTheQuantPicker: one report per file, and the
// recommendation is the LARGEST quantization that still fits.
func TestFitEstimateBatchDrivesTheQuantPicker(t *testing.T) {
	t.Parallel()

	hub := &stubPeekHub{files: map[string]*gguf.File{
		"tiny-Q4_K_M.gguf": buildPeek(t, 4, 1<<20),
		"tiny-Q8_0.gguf":   buildPeek(t, 4, 4<<20),
		"tiny-F16.gguf":    buildPeek(t, 4, 4<<30),
	}}
	a := fitAPI(t, Config{
		HF: hub,
		Hardware: stubHardware{
			gpus: []hw.GPU{gpu(0, "GPU-a", "A", 8<<30, 8<<30)},
			mem:  hw.Memory{TotalBytes: 8 << 30, AvailableBytes: 4 << 30, Known: true},
		},
	})

	w := do(t, a, http.MethodPost, "/api/v1/fit/estimate-batch", `{
		"repo_id": "acme/tiny-GGUF",
		"files": ["tiny-Q4_K_M.gguf", "tiny-Q8_0.gguf", "tiny-F16.gguf"],
		"flags": {"ctx_size": 1024, "n_gpu_layers": {"mode": "all"}}
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body)
	}

	var out FitBatchReportDTO
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Reports) != 3 {
		t.Fatalf("got %d reports, want one per file", len(out.Reports))
	}
	for i, want := range []string{"tiny-Q4_K_M.gguf", "tiny-Q8_0.gguf", "tiny-F16.gguf"} {
		if out.Reports[i].File != want {
			t.Errorf("report %d is for %q, want %q — the order must be the request's",
				i, out.Reports[i].File, want)
		}
	}
	if out.RecommendedFile != "tiny-Q8_0.gguf" {
		t.Errorf("recommended_file = %q, want the largest quant that fits", out.RecommendedFile)
	}
	if out.Reports[2].Verdict == string(fit.VerdictFits) {
		t.Error("a 16 GiB model must not fit on an 8 GiB card")
	}
	if hub.gotRevision != "main" {
		t.Errorf("revision = %q, want the default `main`", hub.gotRevision)
	}
}

// TestFitEstimateBatchSurvivesOneUnreadableFile: a quant picker with twelve rows
// must not go blank because the Hub refused one header.
func TestFitEstimateBatchSurvivesOneUnreadableFile(t *testing.T) {
	t.Parallel()

	hub := &stubPeekHub{files: map[string]*gguf.File{
		"good.gguf": buildPeek(t, 4, 1<<20),
	}}
	a := fitAPI(t, Config{HF: hub})

	w := do(t, a, http.MethodPost, "/api/v1/fit/estimate-batch", `{
		"repo_id": "acme/tiny-GGUF",
		"files": ["good.gguf", "missing.gguf"],
		"flags": {"ctx_size": 1024}
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body)
	}
	var out FitBatchReportDTO
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Reports) != 2 {
		t.Fatalf("got %d reports, want 2", len(out.Reports))
	}
	if out.Reports[1].Verdict != string(fit.VerdictWontRun) || len(out.Reports[1].Notes) == 0 {
		t.Errorf("the unreadable file should be reported with a reason: %+v", out.Reports[1])
	}
}

// TestFitWithNoHardwareIsACPUEstimate: on a host with no NVIDIA card the answer
// is "what would this cost in RAM", not a 503.
func TestFitWithNoHardwareIsACPUEstimate(t *testing.T) {
	t.Parallel()

	a := fitAPI(t, Config{Models: &stubModels{detail: fitModel()}})
	w := do(t, a, http.MethodPost, "/api/v1/fit/estimate",
		`{"source": {"model_id": "m-1"}, "flags": {"ctx_size": 1024}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body)
	}
	rep := decodeFit(t, w.Body.Bytes())
	if len(rep.PerGPU) != 0 || rep.RequiredVRAMBytes != 0 {
		t.Errorf("a CPU-only host must report no VRAM requirement: %+v", rep.PerGPU)
	}
	if rep.SpillToRAMBytes == 0 {
		t.Error("everything is in RAM on a CPU-only host")
	}
	if rep.Verdict != string(fit.VerdictPartial) {
		t.Errorf("verdict = %q, want partial", rep.Verdict)
	}
}

// stubPeekHub is an HFService whose only interesting method is the header peek —
// section 8.5's reason for building a GGUF reader over HTTP Range at all.
type stubPeekHub struct {
	files       map[string]*gguf.File
	gotRepo     string
	gotRevision string
}

func (s *stubPeekHub) Search(context.Context, hf.SearchParams) (hf.SearchPage, error) {
	return hf.SearchPage{}, nil
}

func (s *stubPeekHub) Model(context.Context, string) (hf.ModelInfo, error) {
	return hf.ModelInfo{}, nil
}

func (s *stubPeekHub) Tree(context.Context, string, string) ([]hf.TreeEntry, error) {
	return nil, nil
}

func (s *stubPeekHub) Card(context.Context, string, string) (string, error) { return "", nil }

func (s *stubPeekHub) Peek(_ context.Context, repo, revision, file string, _ ...gguf.Option) (
	*gguf.File, error) {
	s.gotRepo, s.gotRevision = repo, revision
	f, ok := s.files[file]
	if !ok {
		return nil, errors.New("no such file in this repository")
	}
	return f, nil
}

// buildPeek writes a header with `layers` blocks of exactly bytesPerLayer, plus
// one non-layer tensor of the same size, so a test can put a chosen number of
// bytes in front of the calculator without a gigabyte on disk.
func buildPeek(t *testing.T, layers int, bytesPerLayer uint64) *gguf.File {
	t.Helper()
	b := ggufbuild.New("llama").
		Set("llama.block_count", ggufbuild.U32(uint32(layers))).
		Set("llama.context_length", ggufbuild.U32(4096)).
		Set("llama.embedding_length", ggufbuild.U32(256)).
		Set("llama.feed_forward_length", ggufbuild.U32(512)).
		Set("llama.attention.head_count", ggufbuild.U32(8)).
		Set("llama.attention.head_count_kv", ggufbuild.U32(8)).
		Set("llama.vocab_size", ggufbuild.U32(1000))
	elems := bytesPerLayer / 4 // f32
	for i := range layers {
		b.Tensor("blk."+strconv.Itoa(i)+".attn_qkv.weight", gguf.TypeF32, elems)
	}
	b.Tensor("output.weight", gguf.TypeF32, elems)

	data := b.Header()
	f, err := gguf.Parse(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("build peek fixture: %v", err)
	}
	return f
}

// stubSettings answers one key, which is all this layer reads.
type stubSettings map[string]int64

func (s stubSettings) GetInt(_ context.Context, key string) (int64, error) {
	v, ok := s[key]
	if !ok {
		return 0, errors.New("no such setting: " + key)
	}
	return v, nil
}

// TestFitMarginIsTheSetting: `fit.margin_mib` is section 8.1's third host input
// and section 2.1 exposes it as a user-editable knob.
//
// A registered setting nothing reads is a knob that lies, and under SPEC section
// 3.9's zero-config mandate — where the UI is the ONLY way to change anything —
// that is worse than no knob at all: an operator raises the margin to be
// conservative on a shared card, the number on the screen does not move, and
// every `placeable(n)` test still uses the default.
func TestFitMarginIsTheSetting(t *testing.T) {
	t.Parallel()

	body := `{
		"source": {"model_id": "m-1"},
		"flags": {"ctx_size": 1024, "ubatch_size": 64, "parallel": 1,
		          "n_gpu_layers": {"mode": "all"}, "flash_attn": "off",
		          "cache_type_k": "f16", "cache_type_v": "f16"}
	}`

	cases := []struct {
		name     string
		settings Settings
		want     uint64
	}{
		{
			name: "no settings source falls back to the registry default",
			want: fit.DefaultMarginMiB * testMiB,
		},
		{
			name:     "a raised margin reaches the calculator",
			settings: stubSettings{"fit.margin_mib": 2048},
			want:     2048 * testMiB,
		},
		{
			// The registry entry says so in as many words: "0 is a legitimate
			// 'no margin'". A zero-means-default reading would charge an
			// operator who explicitly declined the margin a gibibyte per card.
			name:     "zero is a real answer, not an absent one",
			settings: stubSettings{"fit.margin_mib": 0},
			want:     0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := fitAPI(t, Config{
				Models:   &stubModels{detail: fitModel()},
				Settings: tc.settings,
				Hardware: stubHardware{
					gpus: []hw.GPU{gpu(0, "GPU-a", "Test GPU", 8<<30, 8<<30)},
					mem:  hw.Memory{TotalBytes: 32 << 30, AvailableBytes: 24 << 30, Known: true},
				},
			})

			w := do(t, a, http.MethodPost, "/api/v1/fit/estimate", body)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, body %s", w.Code, w.Body)
			}
			rep := decodeFit(t, w.Body.Bytes())
			if rep.MarginBytesPerGPU != tc.want {
				t.Errorf("margin_bytes_per_gpu = %d, want %d",
					rep.MarginBytesPerGPU, tc.want)
			}
			// One participating GPU, so the reported total is the per-GPU
			// figure. Section 8.4 keeps both so nothing has to infer the unit.
			if rep.MarginBytes != tc.want {
				t.Errorf("margin_bytes = %d, want %d", rep.MarginBytes, tc.want)
			}
			if len(rep.PerGPU) != 1 || rep.PerGPU[0].MarginBytes != tc.want {
				t.Errorf("per_gpu margin = %+v, want %d charged to the device",
					rep.PerGPU, tc.want)
			}
		})
	}
}
