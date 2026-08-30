package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/jlbyh2o/llamaman/internal/fit"
	"github.com/jlbyh2o/llamaman/internal/hw"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/models"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// The fit calculator endpoints of DESIGN section 3.9.
//
// This file is glue, deliberately and on purpose. `internal/fit` is a pure
// function of plain structs (D49 invariant 5) and `internal/hw` is a probe, so
// SOMETHING has to resolve a model source into a shape, a FlagSet into the
// calculator's flags, and a GPU inventory into devices. That resolution is what
// the endpoint IS, so it lives with the endpoint: three converters, one request
// assembler, and a DTO.
//
// The rules it must not lose in translation, all from section 8:
//
//   - `required_vram_bytes` is a REPORTING TOTAL. The verdict is `∀ g:
//     per_gpu[g].ok`, never a comparison against a sum, because llama.cpp does
//     not pool VRAM across devices.
//   - `reserve_bytes_per_gpu` is per participating GPU and is never divided.
//   - a GPU whose memory could not be read reports `free_bytes: null`, not 0.

// FitHardware is the live host state the calculator needs. internal/hw's
// NvidiaSMIProber and Meminfo satisfy it.
//
// It is optional: a nil Hardware answers with a CPU-only estimate rather than a
// 503, because "what would this cost in RAM" is a real question on a host with
// no NVIDIA card, and refusing it would make the quant picker useless there.
type FitHardware interface {
	// Probe returns the GPU inventory. A device whose memory could not be read
	// comes back with nil VRAM pointers, which this layer carries through to a
	// null `free_bytes` rather than collapsing to zero (D16, F14).
	Probe(ctx context.Context) ([]hw.GPU, error)
	// Memory reports system RAM, whose free figure section 8.7's `partial`
	// verdict tests the spill against.
	Memory() (hw.Memory, error)
}

// Settings is the typed settings this layer reads. *settings.Cache satisfies
// it, and the consumer owns the interface (DESIGN section 1) so internal/api
// keeps no dependency on the cache's own type.
type Settings interface {
	GetInt(ctx context.Context, key string) (int64, error)
}

// FitCalibrationSource supplies D32's learned correction for the active build.
//
// It is optional in the same way and for the same reason: with no source, every
// report is `confidence: "modeled"`, which is exactly what a fresh install
// should say.
type FitCalibrationSource interface {
	// ActiveRuntime is the other half of the calibration key: the llama.cpp tag
	// and backend a load would actually use.
	ActiveRuntime(ctx context.Context) (tag string, backend model.Backend, err error)
	// Observations returns at most limit rows for a key, newest first.
	Observations(ctx context.Context, key model.FitCalibrationKey, limit int) ([]fit.Observation, error)
}

// FitSourceDTO is section 3.9's `source`: either a model this host already
// holds, or a repository file it does not — the pre-download peek that lets a
// quant be measured before 20 GB are downloaded.
type FitSourceDTO struct {
	ModelID string `json:"model_id,omitempty"`
	RepoID  string `json:"repo_id,omitempty"`
	File    string `json:"file,omitempty"`
	// Revision defaults to `main` for a repository source.
	Revision string `json:"revision,omitempty"`
}

// FitEstimateRequest is the body of `POST /api/v1/fit/estimate`.
type FitEstimateRequest struct {
	Source FitSourceDTO `json:"source"`
	// Flags is a FlagSet subset. It is the same document `instances.flags_json`
	// carries, so the estimate on the instance form and the estimate on the
	// instance page cannot disagree about what a flag means.
	Flags model.FlagSet `json:"flags"`
	// GPUs selects participating devices by UUID. Empty means every present GPU.
	GPUs []string `json:"gpus,omitempty"`
	// ReserveBytesPerGPU is the caller's own headroom, charged to EVERY selected
	// device exactly like the margin and the backend overhead — never divided
	// among them (section 8.7's `reserve(g)`).
	ReserveBytesPerGPU uint64 `json:"reserve_bytes_per_gpu,omitempty"`
}

// FitBatchRequest is the body of `POST /api/v1/fit/estimate-batch`: one report
// per quantization, which is what drives the quant picker.
type FitBatchRequest struct {
	RepoID   string        `json:"repo_id"`
	Revision string        `json:"revision,omitempty"`
	Files    []string      `json:"files"`
	Flags    model.FlagSet `json:"flags"`
	GPUs     []string      `json:"gpus,omitempty"`

	ReserveBytesPerGPU uint64 `json:"reserve_bytes_per_gpu,omitempty"`
}

// FitInputsDTO echoes what the estimate was made from (section 3.9's `inputs`).
type FitInputsDTO struct {
	Arch        string `json:"arch"`
	NLayer      int    `json:"n_layer"`
	NLayerSWA   int    `json:"n_layer_swa"`
	NHeadKV     []int  `json:"n_head_kv"`
	HeadDimK    int    `json:"head_dim_k"`
	HeadDimV    int    `json:"head_dim_v"`
	NCtx        int    `json:"n_ctx"`
	KVCtx       int    `json:"kv_ctx"`
	NUbatch     int    `json:"n_ubatch"`
	NBatch      int    `json:"n_batch"`
	NParallel   int    `json:"n_parallel"`
	TypeK       string `json:"type_k"`
	TypeV       string `json:"type_v"`
	FlashAttn   bool   `json:"flash_attn"`
	NExpert     int    `json:"n_expert"`
	NExpertUsed int    `json:"n_expert_used"`
	NVocab      int    `json:"n_vocab"`
	NEmbd       int    `json:"n_embd"`
	NFF         int    `json:"n_ff"`
	NHead       int    `json:"n_head"`
}

// FitDeviceDTO is one row of `per_gpu`.
//
// FreeBytes and TotalBytes are nullable for the reason D16 gives: a GPU whose
// driver could not be read has an UNKNOWN figure, and a UI that rendered
// "0 bytes free" would be reporting a measurement nobody made.
type FitDeviceDTO struct {
	Index         int     `json:"index"`
	UUID          string  `json:"uuid"`
	Name          string  `json:"name"`
	FreeBytes     *uint64 `json:"free_bytes"`
	TotalBytes    *uint64 `json:"total_bytes"`
	AssignedBytes uint64  `json:"assigned_bytes"`
	OK            bool    `json:"ok"`
	ShortByBytes  uint64  `json:"short_by_bytes"`

	WeightsBytes  uint64 `json:"weights_bytes"`
	KVBytes       uint64 `json:"kv_bytes"`
	ExtraBytes    uint64 `json:"extra_bytes"`
	OverheadBytes uint64 `json:"backend_overhead_bytes"`
	MarginBytes   uint64 `json:"margin_bytes"`
	ReserveBytes  uint64 `json:"reserve_bytes"`
}

// FitRecommendationDTO is section 3.9's `recommendation`.
type FitRecommendationDTO struct {
	NGpuLayers int    `json:"n_gpu_layers"`
	FlashAttn  bool   `json:"flash_attn"`
	TypeK      string `json:"type_k"`
	TypeV      string `json:"type_v"`
	// NCtx is present only when the recommendation had to reduce the context.
	NCtx   int    `json:"n_ctx,omitempty"`
	Reason string `json:"reason"`
}

// FitReportDTO is section 3.9's response body.
type FitReportDTO struct {
	// Source echoes which file this report is about, which is what makes the
	// batch response readable.
	ModelID string `json:"model_id,omitempty"`
	File    string `json:"file,omitempty"`

	Inputs FitInputsDTO `json:"inputs"`

	WeightsBytes          uint64 `json:"weights_bytes"`
	WeightsOffloadedBytes uint64 `json:"weights_offloaded_bytes"`
	KVBytes               uint64 `json:"kv_bytes"`
	KVSWABytes            uint64 `json:"kv_swa_bytes"`
	KVOffloadedBytes      uint64 `json:"kv_offloaded_bytes"`
	ComputeBytes          uint64 `json:"compute_bytes"`
	ComputeLogitsBytes    uint64 `json:"compute_logits_bytes"`
	ComputeActBytes       uint64 `json:"compute_act_bytes"`
	ComputeAttnBytes      uint64 `json:"compute_attn_bytes"`
	ComputeMoEBytes       uint64 `json:"compute_moe_bytes"`

	// BackendOverheadBytes is OH_gpu, PER participating GPU.
	BackendOverheadBytes uint64 `json:"backend_overhead_bytes"`
	// MarginBytesPerGPU is `fit.margin_mib`; MarginBytes is it times the
	// participating GPUs, a reporting total.
	MarginBytesPerGPU uint64 `json:"margin_bytes_per_gpu"`
	MarginBytes       uint64 `json:"margin_bytes"`
	// ReserveBytesPerGPU is echoed from the request; ReserveBytes is its total.
	ReserveBytesPerGPU uint64 `json:"reserve_bytes_per_gpu"`
	ReserveBytes       uint64 `json:"reserve_bytes"`

	// RequiredVRAMBytes is Σ per_gpu[].assigned_bytes — a TOTAL, never the test.
	RequiredVRAMBytes uint64         `json:"required_vram_bytes"`
	PerGPU            []FitDeviceDTO `json:"per_gpu"`

	SpillToRAMBytes    uint64 `json:"spill_to_ram_bytes"`
	SystemRAMFreeBytes uint64 `json:"system_ram_free_bytes"`
	SystemRAMKnown     bool   `json:"system_ram_known"`

	Verdict             string `json:"verdict"`
	NGpuLayers          int    `json:"n_gpu_layers"`
	MaxNGpuLayers       int    `json:"max_n_gpu_layers"`
	MaxCtxAtFullOffload int    `json:"max_ctx_at_full_offload"`
	PerSlotCtx          int    `json:"per_slot_ctx"`

	Recommendation FitRecommendationDTO `json:"recommendation"`
	Confidence     string               `json:"confidence"`
	// CalibrationSamples says how many observations the correction came from,
	// so "modeled" can be explained rather than merely asserted.
	CalibrationSamples int  `json:"calibration_samples"`
	CalibrationClamped bool `json:"calibration_clamped"`
	// VRAMUnknown reports that at least one selected device's memory could not
	// be read. A consumer must render "unknown" rather than "won't run".
	VRAMUnknown bool     `json:"vram_unknown"`
	Notes       []string `json:"notes"`
}

// FitBatchReportDTO is the batch response: one report per quant plus the pick.
type FitBatchReportDTO struct {
	Reports []FitReportDTO `json:"reports"`
	// RecommendedFile is the largest quantization that still fits, which is the
	// choice the quant picker defaults to. Empty when none of them do.
	RecommendedFile string `json:"recommended_file"`
}

// CodeFitUnavailable is the 409 for a source this daemon cannot measure: a model
// whose GGUF header has not been parsed yet, or a repository file the Hub would
// not serve a header for. It is a 409 rather than a 422 because the request is
// well-formed and the answer may exist later.
const CodeFitUnavailable model.ErrorCode = "fit_unavailable"

func (a *API) fitRoutes() []Route {
	return []Route{a.fitEstimateRoute(), a.fitEstimateBatchRoute()}
}

func (a *API) fitEstimateRoute() Route {
	return Route{
		Method:      http.MethodPost,
		Pattern:     BasePath + "/fit/estimate",
		Auth:        AuthSession,
		OperationID: "estimateFit",
		Summary: "Will this model and these flags run on these GPUs, and where does the " +
			"memory go: per-GPU placement, the KV and compute breakdown, the `-ngl auto` " +
			"advisory, and a recommendation (section 8).",
		Tag:         "fit",
		RequestBody: FitEstimateRequest{},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req FitEstimateRequest
			if err := DecodeJSON(w, r, &req); err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			rep, err := a.estimateFit(r.Context(), req)
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := WriteJSON(w, http.StatusOK, rep); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status:      http.StatusOK,
			Description: "The estimate. `verdict` is exactly `∀ g: per_gpu[g].ok`.",
			Body:        FitReportDTO{},
		},
		Errors: []Response{
			{
				Status:      http.StatusUnprocessableEntity,
				Description: "The source names neither a model nor a repository file, or the flags are invalid.",
				Codes:       []model.ErrorCode{model.CodeBadFlags},
			},
			{
				Status:      http.StatusNotFound,
				Description: "`source.model_id` names no model on this host.",
				Codes:       []model.ErrorCode{CodeNotFound},
			},
			{
				Status: http.StatusConflict,
				Description: "The source cannot be measured yet: its GGUF header has not been " +
					"parsed, or the Hub would not serve one.",
				Codes: []model.ErrorCode{CodeFitUnavailable},
			},
		},
	}
}

func (a *API) fitEstimateBatchRoute() Route {
	return Route{
		Method:      http.MethodPost,
		Pattern:     BasePath + "/fit/estimate-batch",
		Auth:        AuthSession,
		OperationID: "estimateFitBatch",
		Summary: "One report per quantization of a repository, plus the largest one that " +
			"fits — this is what drives the quant picker (section 3.9).",
		Tag:         "fit",
		RequestBody: FitBatchRequest{},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req FitBatchRequest
			if err := DecodeJSON(w, r, &req); err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			out, err := a.estimateFitBatch(r.Context(), req)
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := WriteJSON(w, http.StatusOK, out); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status:      http.StatusOK,
			Description: "One report per file, in the order they were asked for.",
			Body:        FitBatchReportDTO{},
		},
		Errors: []Response{
			{
				Status:      http.StatusUnprocessableEntity,
				Description: "No repository, no files, or invalid flags.",
				Codes:       []model.ErrorCode{model.CodeBadFlags},
			},
			{
				Status:      http.StatusServiceUnavailable,
				Description: "This build has no Hugging Face client, so a remote peek is impossible.",
				Codes:       []model.ErrorCode{CodeInternalError},
			},
		},
	}
}

// estimateFit assembles the calculator's inputs and runs it.
func (a *API) estimateFit(ctx context.Context, req FitEstimateRequest) (FitReportDTO, error) {
	if err := req.Flags.Validate(); err != nil {
		return FitReportDTO{}, Errorf(http.StatusUnprocessableEntity, model.CodeBadFlags,
			"%s", err.Error())
	}

	shape, modelID, file, err := a.fitShape(ctx, req.Source)
	if err != nil {
		return FitReportDTO{}, err
	}

	base, err := a.fitRequest(ctx, shape, req.Flags, req.GPUs, req.ReserveBytesPerGPU)
	if err != nil {
		return FitReportDTO{}, err
	}
	out := fitReportDTO(fit.Estimate(base))
	out.ModelID, out.File = modelID, file
	return out, nil
}

// estimateFitBatch measures every quantization of one repository against the
// same flags and the same devices.
//
// The devices and the calibration are resolved ONCE for the whole batch, not per
// file: a quant picker with twelve rows would otherwise fork nvidia-smi twelve
// times, and — worse — could produce rows measured against different free VRAM,
// so a user comparing them would be comparing answers to different questions.
func (a *API) estimateFitBatch(ctx context.Context, req FitBatchRequest) (FitBatchReportDTO, error) {
	if strings.TrimSpace(req.RepoID) == "" || len(req.Files) == 0 {
		return FitBatchReportDTO{}, Errorf(http.StatusUnprocessableEntity, model.CodeBadFlags,
			"a repository and at least one file are required")
	}
	if err := req.Flags.Validate(); err != nil {
		return FitBatchReportDTO{}, Errorf(http.StatusUnprocessableEntity, model.CodeBadFlags,
			"%s", err.Error())
	}
	if a.cfg.HF == nil {
		return FitBatchReportDTO{}, Errorf(http.StatusServiceUnavailable, CodeInternalError,
			"this build has no Hugging Face client")
	}

	devices, host, err := a.fitHost(ctx, req.GPUs)
	if err != nil {
		return FitBatchReportDTO{}, err
	}
	// One read for the whole batch: `fit.margin_mib` cannot change between two
	// quants of the same picker, and reading it per file would put the settings
	// cache inside a loop that already forks nothing.
	margin := a.fitMargin(ctx)

	out := FitBatchReportDTO{Reports: make([]FitReportDTO, 0, len(req.Files))}
	var (
		bestBytes uint64
		bestFile  string
	)
	for _, f := range req.Files {
		shape, err := a.peekShape(ctx, req.RepoID, req.Revision, f)
		if err != nil {
			// One unreadable quant must not sink the picker: the row is reported
			// with the reason and the rest are still measured.
			out.Reports = append(out.Reports, FitReportDTO{
				File:    f,
				Verdict: string(fit.VerdictWontRun),
				Notes:   []string{fitReason(err)},
			})
			continue
		}
		cal, err := a.fitCalibration(ctx, shape.Arch)
		if err != nil {
			return FitBatchReportDTO{}, err
		}
		rep := fit.Estimate(fit.Request{
			Model: shape, Flags: models.FitFlags(req.Flags), Devices: devices, Host: host,
			Calibration: cal, ReserveBytesPerGPU: req.ReserveBytesPerGPU,
			MarginMiB: margin,
		})
		dto := fitReportDTO(rep)
		dto.File = f
		out.Reports = append(out.Reports, dto)

		// The recommendation is the LARGEST quantization that still fits: bigger
		// weights mean a better quant, and "fits" is the only bar that matters.
		if rep.Verdict == fit.VerdictFits && rep.WeightsBytes > bestBytes {
			bestBytes, bestFile = rep.WeightsBytes, f
		}
	}
	out.RecommendedFile = bestFile
	return out, nil
}

// fitShape resolves section 3.9's `source`.
func (a *API) fitShape(ctx context.Context, src FitSourceDTO) (fit.ModelShape, string, string, error) {
	switch {
	case strings.TrimSpace(src.ModelID) != "":
		if a.cfg.Models == nil {
			return fit.ModelShape{}, "", "", Errorf(http.StatusServiceUnavailable,
				CodeInternalError, "this build has no model catalog")
		}
		detail, err := a.cfg.Models.Get(ctx, src.ModelID)
		if err != nil {
			// WriteError maps store.ErrNotFound to a 404 in the envelope, which
			// is the answer for an id that names no row; anything else is the
			// service's own error and keeps its own status.
			if errors.Is(err, store.ErrNotFound) {
				return fit.ModelShape{}, "", "", NotFound("no model %q", src.ModelID)
			}
			return fit.ModelShape{}, "", "", err
		}
		shape, ok := models.FitShape(detail.View)
		if !ok {
			return fit.ModelShape{}, "", "", Conflict(CodeFitUnavailable,
				"%s has not had its GGUF header parsed yet", detail.ID)
		}
		return shape, detail.ID, detail.PrimaryFile, nil

	case strings.TrimSpace(src.RepoID) != "" && strings.TrimSpace(src.File) != "":
		if a.cfg.HF == nil {
			return fit.ModelShape{}, "", "", Errorf(http.StatusServiceUnavailable,
				CodeInternalError, "this build has no Hugging Face client")
		}
		shape, err := a.peekShape(ctx, src.RepoID, src.Revision, src.File)
		if err != nil {
			return fit.ModelShape{}, "", "", err
		}
		return shape, "", src.File, nil

	default:
		return fit.ModelShape{}, "", "", Errorf(http.StatusUnprocessableEntity,
			model.CodeBadFlags,
			"source must carry either `model_id` or both `repo_id` and `file`")
	}
}

// peekShape reads a repository file's header over an HTTP Range request — the
// capability section 8.5 built the GGUF reader for, and what lets a quant be
// measured before 20 GB are downloaded.
func (a *API) peekShape(ctx context.Context, repo, revision, file string) (fit.ModelShape, error) {
	if revision == "" {
		revision = "main"
	}
	f, err := a.cfg.HF.Peek(ctx, repo, revision, file)
	if err != nil {
		return fit.ModelShape{}, Conflict(CodeFitUnavailable,
			"%s could not be measured: %s", file, err.Error())
	}
	return models.FitShapeFromGGUF(f), nil
}

// fitRequest assembles everything but the shape.
func (a *API) fitRequest(ctx context.Context, shape fit.ModelShape, flags model.FlagSet,
	gpus []string, reserve uint64) (fit.Request, error) {

	devices, host, err := a.fitHost(ctx, gpus)
	if err != nil {
		return fit.Request{}, err
	}
	cal, err := a.fitCalibration(ctx, shape.Arch)
	if err != nil {
		return fit.Request{}, err
	}
	return fit.Request{
		Model: shape, Flags: models.FitFlags(flags), Devices: devices, Host: host,
		Calibration: cal, ReserveBytesPerGPU: reserve,
		MarginMiB: a.fitMargin(ctx),
	}, nil
}

// fitMargin reads `fit.margin_mib` — section 8.1's third host input, and the one
// the settings table of section 2.1 exposes as a user-editable knob.
//
// It is a POINTER answer because the registry entry allows 0 and means it: an
// operator who sets the margin to zero on a card they are deliberately filling
// must get zero, not the default gibibyte. Nil — no settings source, or a read
// that failed — leaves fit.Estimate to substitute DefaultMarginMiB, which is the
// same number the registry defaults to, so the two can never disagree.
func (a *API) fitMargin(ctx context.Context) *int {
	if a.cfg.Settings == nil {
		return nil
	}
	v, err := a.cfg.Settings.GetInt(ctx, "fit.margin_mib")
	if err != nil {
		a.log.Warn("could not read fit.margin_mib; estimating with the default margin",
			"error", err, "default_mib", fit.DefaultMarginMiB)
		return nil
	}
	if v < 0 {
		return nil
	}
	return fit.MiB(int(v))
}

// fitHost probes the machine. A probe FAILURE is not an error here: D16's whole
// point is that an unreadable driver yields devices with unknown memory, and the
// calculator reports that honestly instead of refusing to answer.
func (a *API) fitHost(ctx context.Context, want []string) ([]fit.Device, fit.Host, error) {
	if a.cfg.Hardware == nil {
		return nil, fit.Host{}, nil
	}

	gpus, err := a.cfg.Hardware.Probe(ctx)
	if err != nil && len(gpus) == 0 {
		// Nothing at all came back — not even an inventory with unknown memory.
		// A CPU-only estimate is still a useful answer.
		gpus = nil
	}
	selected := hw.Select(gpus, want)

	devices := make([]fit.Device, 0, len(selected))
	for i, g := range selected {
		d := fit.Device{Index: i, UUID: g.UUID, Name: g.Name}
		if g.VRAMKnown() {
			d.TotalBytes, d.FreeBytes, d.Known = *g.VRAMTotalBytes, *g.VRAMFreeBytes, true
		}
		devices = append(devices, d)
	}

	var host fit.Host
	if mem, err := a.cfg.Hardware.Memory(); err == nil {
		host = fit.Host{
			RAMFreeBytes:  mem.AvailableBytes,
			RAMTotalBytes: mem.TotalBytes,
			RAMKnown:      mem.Known,
		}
	}
	return devices, host, nil
}

// fitCalibration looks up D32's correction for this architecture on the active
// build. A missing source, an unknown runtime or too few samples all produce the
// zero Calibration, which is the identity and reports `modeled`.
func (a *API) fitCalibration(ctx context.Context, arch string) (fit.Calibration, error) {
	if a.cfg.FitCalibration == nil || arch == "" {
		return fit.Calibration{}, nil
	}
	tag, backend, err := a.cfg.FitCalibration.ActiveRuntime(ctx)
	if err != nil || tag == "" {
		return fit.Calibration{}, nil
	}
	obs, err := a.cfg.FitCalibration.Observations(ctx,
		model.FitCalibrationKey{Arch: arch, Backend: backend, LlamacppTag: tag},
		fit.CalibrationWindow)
	if err != nil {
		return fit.Calibration{}, nil
	}
	return fit.NewCalibration(obs), nil
}

func fitReportDTO(r fit.Report) FitReportDTO {
	out := FitReportDTO{
		Inputs: FitInputsDTO{
			Arch: r.Inputs.Arch, NLayer: r.Inputs.NLayer, NLayerSWA: r.Inputs.NLayerSWA,
			NHeadKV: r.Inputs.NHeadKV, HeadDimK: r.Inputs.HeadDimK, HeadDimV: r.Inputs.HeadDimV,
			NCtx: r.Inputs.NCtx, KVCtx: r.Inputs.KVCtx, NUbatch: r.Inputs.NUbatch,
			NBatch: r.Inputs.NBatch, NParallel: r.Inputs.NParallel,
			TypeK: r.Inputs.TypeK, TypeV: r.Inputs.TypeV, FlashAttn: r.Inputs.FlashAttn,
			NExpert: r.Inputs.NExpert, NExpertUsed: r.Inputs.NExpertUsed,
			NVocab: r.Inputs.NVocab, NEmbd: r.Inputs.NEmbd, NFF: r.Inputs.NFF,
			NHead: r.Inputs.NHead,
		},
		WeightsBytes:          r.WeightsBytes,
		WeightsOffloadedBytes: r.WeightsOffloadedBytes,
		KVBytes:               r.KVBytes,
		KVSWABytes:            r.KVSWABytes,
		KVOffloadedBytes:      r.KVOffloadedBytes,
		ComputeBytes:          r.ComputeBytes,
		ComputeLogitsBytes:    r.ComputeLogitsBytes,
		ComputeActBytes:       r.ComputeActBytes,
		ComputeAttnBytes:      r.ComputeAttnBytes,
		ComputeMoEBytes:       r.ComputeMoEBytes,
		BackendOverheadBytes:  r.BackendOverheadBytes,
		MarginBytesPerGPU:     r.MarginBytesPerGPU,
		MarginBytes:           r.MarginBytes,
		ReserveBytesPerGPU:    r.ReserveBytesPerGPU,
		ReserveBytes:          r.ReserveBytes,
		RequiredVRAMBytes:     r.RequiredVRAMBytes,
		SpillToRAMBytes:       r.SpillToRAMBytes,
		SystemRAMFreeBytes:    r.SystemRAMFreeBytes,
		SystemRAMKnown:        r.SystemRAMKnown,
		Verdict:               string(r.Verdict),
		NGpuLayers:            r.NGpuLayers,
		MaxNGpuLayers:         r.MaxNGpuLayers,
		MaxCtxAtFullOffload:   r.MaxCtxAtFullOffload,
		PerSlotCtx:            r.PerSlotCtx,
		Recommendation: FitRecommendationDTO{
			NGpuLayers: r.Recommendation.NGpuLayers,
			FlashAttn:  r.Recommendation.FlashAttn,
			TypeK:      r.Recommendation.TypeK,
			TypeV:      r.Recommendation.TypeV,
			NCtx:       r.Recommendation.NCtx,
			Reason:     r.Recommendation.Reason,
		},
		Confidence:         string(r.Confidence),
		CalibrationSamples: r.Calibration.Samples,
		CalibrationClamped: r.Calibration.Clamped,
		VRAMUnknown:        r.VRAMUnknown,
		Notes:              r.Notes,
	}
	if out.Notes == nil {
		out.Notes = []string{}
	}
	out.PerGPU = make([]FitDeviceDTO, 0, len(r.PerGPU))
	for _, g := range r.PerGPU {
		out.PerGPU = append(out.PerGPU, FitDeviceDTO{
			Index: g.Index, UUID: g.UUID, Name: g.Name,
			FreeBytes: g.FreeBytes, TotalBytes: g.TotalBytes,
			AssignedBytes: g.AssignedBytes, OK: g.OK, ShortByBytes: g.ShortByBytes,
			WeightsBytes: g.WeightsBytes, KVBytes: g.KVBytes, ExtraBytes: g.ExtraBytes,
			OverheadBytes: g.OverheadBytes, MarginBytes: g.MarginBytes,
			ReserveBytes: g.ReserveBytes,
		})
	}
	return out
}

// fitReason renders a per-file failure for the batch response without leaking an
// internal error's text into a document the UI renders verbatim.
func fitReason(err error) string {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		return apiErr.Message
	}
	return "this file could not be measured"
}
