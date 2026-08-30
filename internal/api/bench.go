package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/jlbyh2o/llamaman/internal/bench"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// The benchmark endpoints of DESIGN section 3.13 — all twelve rows of that
// table: the run listing, the create with its sweep expansion, the preflight,
// start, cancel, the detail, rename/annotate, delete, the flattened results, the
// three-format export, the comparison and the history series.
//
// Two of them are mechanisms rather than conveniences, and both are named
// elsewhere in the design:
//
//   - `GET /bench/preflight` is where §10.1's dropped flags become visible.
//     "Every dropped field is dropped LOUDLY" is a promise this endpoint keeps,
//     and it is what answers "why is my benchmark not measuring my 32k context"
//     before the run rather than after it.
//   - `POST /bench/runs` is §10's "expansion first": the cross-product becomes
//     `bench_points` rows BEFORE anything executes, which is what makes progress
//     exact, resume exact and the duration estimate possible.

// BenchService is everything this layer needs from internal/bench. The consumer
// owns the interface (DESIGN section 1); *bench.Service satisfies it.
type BenchService interface {
	List(ctx context.Context, f store.BenchRunFilter) ([]store.BenchRun, error)
	Get(ctx context.Context, id string) (bench.View, error)
	Create(ctx context.Context, req bench.CreateRequest) (bench.CreateResult, error)
	Start(ctx context.Context, id string) (model.Job, error)
	Cancel(ctx context.Context, id string) (model.Job, error)
	Annotate(ctx context.Context, id, name string, notes *string) (store.BenchRun, error)
	Delete(ctx context.Context, id string) error

	Results(ctx context.Context, id string) ([]bench.ResultRow, error)
	Export(ctx context.Context, id string, format bench.Format) (bench.Export, error)
	Preflight(ctx context.Context, req bench.PreflightRequest) (bench.Preflight, error)
	Compare(ctx context.Context, q store.BenchCompareQuery) ([]store.BenchComparePoint, error)
	Series(ctx context.Context, q store.BenchSeriesQuery) ([]store.BenchSeriesPoint, error)
}

// BenchRunDTO is one row of `GET /api/v1/bench/runs`.
//
// The model and llama.cpp identity travel DENORMALIZED, exactly as they are
// stored: a benchmark is history, and `model_label`/`llamacpp_tag` stay readable
// after the model file is deleted and the build is superseded.
type BenchRunDTO struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"`

	ModelID    *string `json:"model_id"`
	ModelLabel string  `json:"model_label"`
	ModelPath  string  `json:"model_path"`
	QuantLabel *string `json:"quant_label"`

	LlamacppVersionID *string `json:"llamacpp_version_id"`
	LlamacppTag       string  `json:"llamacpp_tag"`
	LlamacppCommit    *string `json:"llamacpp_commit"`
	LlamacppBackend   string  `json:"llamacpp_backend"`

	Repetitions  int `json:"repetitions"`
	PointsTotal  int `json:"points_total"`
	PointsDone   int `json:"points_done"`
	PointsFailed int `json:"points_failed"`

	// StoppedInstances and RestoreDone are the stop-and-restore protocol on the
	// wire. They are separate fields for the reason they are separate columns:
	// `state` says what the benchmark did, `restore_done` says what the host is
	// still owed, and a UI that showed only the first would have no way to warn
	// that two production instances are down.
	StoppedInstances []string `json:"stopped_instances"`
	RestoreDone      bool     `json:"restore_done"`

	ErrorMessage *string `json:"error_message"`
	Notes        *string `json:"notes"`

	CreatedAt  string  `json:"created_at"`
	StartedAt  *string `json:"started_at"`
	FinishedAt *string `json:"finished_at"`
}

// BenchPointDTO is one expanded cell.
type BenchPointDTO struct {
	ID      string `json:"id"`
	Ordinal int    `json:"ordinal"`
	State   string `json:"state"`
	// Label is the human-readable cell — `ngl=20 b=2048 fa=1` — derived from the
	// axis columns rather than stored, so a column added later shows up here
	// without a migration.
	Label string `json:"label"`
	// Argv is the exact `llama-bench` command line this point runs, which is
	// what "exact resume" means: the same argv, not one rebuilt from inputs that
	// may have moved.
	Argv []string `json:"argv"`

	NGpuLayers  *int64  `json:"n_gpu_layers"`
	NBatch      *int64  `json:"n_batch"`
	NUbatch     *int64  `json:"n_ubatch"`
	NThreads    *int64  `json:"n_threads"`
	FlashAttn   *bool   `json:"flash_attn"`
	TypeK       *string `json:"type_k"`
	TypeV       *string `json:"type_v"`
	SplitMode   *string `json:"split_mode"`
	TensorSplit *string `json:"tensor_split"`
	NDepth      *int64  `json:"n_depth"`

	StartedAt    *string `json:"started_at"`
	FinishedAt   *string `json:"finished_at"`
	ErrorMessage *string `json:"error_message"`
}

// BenchProgressDTO is `jobs.progress_json` for a sweep, section 10's three
// fields, lifted onto the run detail so a client polling one endpoint sees both.
type BenchProgressDTO struct {
	PointsDone  int    `json:"points_done"`
	PointsTotal int    `json:"points_total"`
	Current     string `json:"current"`
}

// BenchRunDetailDTO is `GET /api/v1/bench/runs/{id}`: the run, its points, the
// environment capture and the live job's progress.
type BenchRunDetailDTO struct {
	Run    BenchRunDTO     `json:"run"`
	Points []BenchPointDTO `json:"points"`

	// Sweep, GPU and Host are the three captured documents. Cross-version
	// comparisons are meaningless without the last two (section 10), so they are
	// on the run rather than looked up when a chart is drawn — the card may not
	// be in the host any more by then.
	Sweep map[string]any   `json:"sweep"`
	GPU   []map[string]any `json:"gpu"`
	Host  map[string]any   `json:"host"`

	JobID    *string           `json:"job_id"`
	JobState *string           `json:"job_state"`
	Progress *BenchProgressDTO `json:"progress"`
}

// BenchResultRowDTO is one flattened row of `GET …/results` — the point's axes
// beside the result's numbers, which is the shape the table and the CSV both
// want.
type BenchResultRowDTO struct {
	PointID string `json:"point_id"`
	Ordinal int    `json:"ordinal"`
	Label   string `json:"label"`

	NGpuLayers  *int64  `json:"n_gpu_layers"`
	NBatch      *int64  `json:"n_batch"`
	NUbatch     *int64  `json:"n_ubatch"`
	NThreads    *int64  `json:"n_threads"`
	FlashAttn   *bool   `json:"flash_attn"`
	TypeK       *string `json:"type_k"`
	TypeV       *string `json:"type_v"`
	SplitMode   *string `json:"split_mode"`
	TensorSplit *string `json:"tensor_split"`

	TestKind string `json:"test_kind"`
	NPrompt  int64  `json:"n_prompt"`
	NGen     int64  `json:"n_gen"`
	NDepth   int64  `json:"n_depth"`

	// AvgTS is tokens per second and StddevTS its spread. The pair travels
	// together everywhere: a throughput with no spread invites a reader to
	// believe a difference that is inside the noise.
	AvgTS    float64 `json:"avg_ts"`
	StddevTS float64 `json:"stddev_ts"`
	AvgNS    int64   `json:"avg_ns"`
	StddevNS int64   `json:"stddev_ns"`
	// Samples is how many repetitions the mean was taken over.
	Samples int `json:"samples"`
}

// CreateBenchRunRequest is the body of `POST /api/v1/bench/runs`
// (section 3.13's example, field for field).
type CreateBenchRunRequest struct {
	Name    string `json:"name,omitempty"`
	ModelID string `json:"model_id"`
	// Repetitions is llama-bench's `-r`. Zero means
	// `settings.bench.default_repetitions`.
	Repetitions int `json:"repetitions,omitempty"`
	// Sweep is the cross-product definition. Every axis accepts either a JSON
	// array (`[512,2048]`) or the comma-list a form field produces
	// (`"512,2048"`).
	Sweep json.RawMessage `json:"sweep,omitempty"`
	// OnConflict is `abort` or `stop_and_restore`. Empty means `abort`, because
	// stopping somebody's production instance is not a thing to do by omission.
	OnConflict string `json:"on_conflict,omitempty"`
	// Draft creates the run without queueing it — section 3.13's `201 draft`,
	// which is the sweep builder saving work in progress.
	Draft bool `json:"draft,omitempty"`
}

// PatchBenchRunRequest is the body of `PATCH /api/v1/bench/runs/{id}`: rename
// and annotate, and nothing else. A benchmark's inputs are immutable once
// expanded — the points are written and the results measured — so there is no
// edit here that could make the row disagree with what ran.
type PatchBenchRunRequest struct {
	Name  string  `json:"name,omitempty"`
	Notes *string `json:"notes,omitempty"`
}

// CreateBenchRunResponse embeds section 3's long-action receipt beside the run,
// so a client is not left to guess which domain row the SSE frames belong to.
// `job_id` is null for a draft, where nothing was queued.
type CreateBenchRunResponse struct {
	JobReceiptDTO
	Run    BenchRunDTO     `json:"run"`
	Points []BenchPointDTO `json:"points"`
}

// BenchConflictDTO is one instance the exclusivity guard found on the target
// GPUs.
type BenchConflictDTO struct {
	InstanceID string   `json:"instance_id"`
	Name       string   `json:"name"`
	State      string   `json:"state"`
	GPUUUIDs   []string `json:"gpu_uuids"`
	// Attribution is how `gpu_uuids` was obtained, and Assumed reports the
	// FAIL-CLOSED inclusion: an instance whose attribution is `declared` or
	// `unknown` is treated as occupying every GPU it could occupy, so a bench is
	// never launched into a collision merely because attribution was
	// unavailable (section 10, D17).
	Attribution string `json:"attribution"`
	Assumed     bool   `json:"assumed"`
}

// BenchIgnoredFlagDTO is one entry of section 10.1's loud drop list.
type BenchIgnoredFlagDTO struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

// BenchPreflightDTO is `GET /api/v1/bench/preflight`.
type BenchPreflightDTO struct {
	ModelID    string `json:"model_id"`
	ModelLabel string `json:"model_label"`
	ModelPath  string `json:"model_path"`

	LlamacppVersionID string `json:"llamacpp_version_id"`
	LlamacppTag       string `json:"llamacpp_tag"`
	RuntimeReady      bool   `json:"runtime_ready"`

	PointsTotal int `json:"points_total"`
	Repetitions int `json:"repetitions"`
	// EstimatedSec is `points_total × median seconds-per-point`, and
	// EstimateFromHistory says whether that median came from prior runs against
	// this model or from a built-in default. An estimate and a guess deserve
	// different words on screen.
	EstimatedSec        int  `json:"estimated_sec"`
	EstimateFromHistory bool `json:"estimate_from_history"`

	ExclusiveGPU bool     `json:"exclusive_gpu"`
	TargetGPUs   []string `json:"target_gpus"`
	// GPUIdentityKnown reports whether this host could enumerate its GPUs at
	// all. When it is false every entry in `conflicts` is `assumed`.
	GPUIdentityKnown bool               `json:"gpu_identity_known"`
	Conflicts        []BenchConflictDTO `json:"conflicts"`
	// FreeVRAMBytes is keyed by GPU UUID. A card whose memory the driver did not
	// report is ABSENT rather than zero (D16): "unknown" and "none" are
	// different answers.
	FreeVRAMBytes map[string]uint64 `json:"free_vram_bytes"`

	IgnoredFlags []BenchIgnoredFlagDTO `json:"ignored_flags"`
	Notes        []string              `json:"notes"`
}

// CompareBenchRequest is the body of `POST /api/v1/bench/compare`.
type CompareBenchRequest struct {
	RunIDs []string `json:"run_ids,omitempty"`
	// X is the horizontal axis, Series splits the lines, Y is the measured
	// value. Filters are equality constraints keyed by axis —
	// `{"test_kind":"tg"}` being the commonest, since a chart mixing prompt and
	// generation throughput is not a chart.
	X       string            `json:"x"`
	Y       string            `json:"y"`
	Series  string            `json:"series,omitempty"`
	Filters map[string]string `json:"filters,omitempty"`
}

// BenchComparePointDTO is one aggregated cell. Samples is on the wire because a
// mean over one sample and a mean over nine are different claims.
type BenchComparePointDTO struct {
	X       string  `json:"x"`
	Series  string  `json:"series"`
	Value   float64 `json:"value"`
	Samples int     `json:"samples"`
}

// BenchCompareDTO is the chart-ready answer.
type BenchCompareDTO struct {
	X      string                 `json:"x"`
	Y      string                 `json:"y"`
	Series string                 `json:"series"`
	Points []BenchComparePointDTO `json:"points"`
}

// BenchSeriesPointDTO is one point of a history line.
type BenchSeriesPointDTO struct {
	RunID   string  `json:"run_id"`
	RunName string  `json:"run_name"`
	Group   string  `json:"group"`
	At      string  `json:"at"`
	Value   float64 `json:"value"`
	Samples int     `json:"samples"`
}

// BenchSeriesDTO is `GET /api/v1/bench/series`: history across llama.cpp
// versions, oldest first.
type BenchSeriesDTO struct {
	Metric string                `json:"metric"`
	Group  string                `json:"group"`
	Points []BenchSeriesPointDTO `json:"points"`
}

func (a *API) benchRoutes() []Route {
	return []Route{
		a.listBenchRunsRoute(),
		a.createBenchRunRoute(),
		a.benchPreflightRoute(),
		a.getBenchRunRoute(),
		a.patchBenchRunRoute(),
		a.deleteBenchRunRoute(),
		a.startBenchRunRoute(),
		a.cancelBenchRunRoute(),
		a.benchResultsRoute(),
		a.benchExportRoute(),
		a.benchCompareRoute(),
		a.benchSeriesRoute(),
	}
}

func (a *API) listBenchRunsRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/bench/runs",
		Auth:        AuthSession,
		OperationID: "listBenchRuns",
		Summary:     "Every benchmark run, newest first, with its summary counters.",
		Tag:         "bench",
		Query: []QueryParam{
			{Name: "state", Description: "Keep only runs in this state.",
				Enum: benchRunStates()},
			{Name: "model_id", Description: "Keep only runs against this model."},
			{Name: "limit", Description: "Maximum runs to return. Default 200.", Type: "integer"},
		},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.bench()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			f := store.BenchRunFilter{
				ModelID: r.URL.Query().Get("model_id"),
				Limit:   int(queryInt64(r, "limit", 0)),
			}
			if st := r.URL.Query().Get("state"); st != "" {
				f.States = []model.BenchRunState{model.BenchRunState(st)}
			}
			runs, err := svc.List(r.Context(), f)
			if err != nil {
				a.writeBenchError(w, r, err)
				return
			}
			items := make([]BenchRunDTO, 0, len(runs))
			for _, run := range runs {
				items = append(items, benchRunDTO(run))
			}
			if err := WriteJSON(w, http.StatusOK, NewList(items, len(items), nil)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status:      http.StatusOK,
			Description: "The runs, newest first.",
			Body:        List[BenchRunDTO]{},
		},
	}
}

func (a *API) createBenchRunRoute() Route {
	return Route{
		Method:      http.MethodPost,
		Pattern:     BasePath + "/bench/runs",
		Auth:        AuthSession,
		OperationID: "createBenchRun",
		Summary: "Expand a sweep into its points and queue it. The cross-product becomes " +
			"`bench_points` rows BEFORE anything executes, which is what makes progress and " +
			"resume exact.",
		Tag:         "bench",
		Idempotent:  true,
		RequestBody: CreateBenchRunRequest{},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.bench()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			var body CreateBenchRunRequest
			if err := DecodeJSON(w, r, &body); err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			sweep, err := bench.ParseSweep(body.Sweep)
			if err != nil {
				a.writeBenchError(w, r, err)
				return
			}
			if body.OnConflict != "" {
				sweep.OnConflict = bench.ConflictPolicy(body.OnConflict)
			}

			res, err := svc.Create(r.Context(), bench.CreateRequest{
				Name:        body.Name,
				ModelID:     body.ModelID,
				Repetitions: body.Repetitions,
				Sweep:       sweep,
				Draft:       body.Draft,
				Idempotency: idempotencyFor(r, body),
			})
			if err != nil {
				a.writeBenchError(w, r, err)
				return
			}

			// `201` for a draft — a row was created and nothing was queued —
			// and `202` for a sweep that is now waiting for the bench lease.
			// An Idempotency-Key replay answers `200` with the original run,
			// which is what makes a double-clicked Run a replay rather than a
			// second sweep.
			status := http.StatusAccepted
			switch {
			case res.Replayed:
				status = http.StatusOK
			case body.Draft:
				status = http.StatusCreated
			}
			if err := WriteJSON(w, status, CreateBenchRunResponse{
				JobReceiptDTO: benchReceipt(res.Job, res.Run.ID),
				Run:           benchRunDTO(res.Run),
				Points:        benchPointDTOs(res.Points),
			}); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status:      http.StatusAccepted,
			Description: "The sweep was expanded and queued. Watch `job_id`.",
			Body:        CreateBenchRunResponse{},
		},
		Errors: []Response{
			{
				Status:      http.StatusOK,
				Description: "An `Idempotency-Key` replay inside its window.",
				Body:        CreateBenchRunResponse{},
			},
			{
				Status:      http.StatusCreated,
				Description: "`draft: true` — the run and its points exist, and nothing was queued.",
				Body:        CreateBenchRunResponse{},
			},
			{
				Status: http.StatusConflict,
				Description: "Instances are loaded on the GPUs this benchmark would use and " +
					"`on_conflict` is `abort`; `details.instances` names them, including any " +
					"included because per-GPU attribution was unavailable. Or no llama.cpp " +
					"build is active, so there is no llama-bench to run.",
				Codes: []model.ErrorCode{
					bench.CodeBenchGPUConflict,
					bench.CodeBenchNoRuntime,
					model.CodeJobInFlight,
				},
			},
			{
				Status: http.StatusUnprocessableEntity,
				Description: "The sweep expands past the point limit, an axis is malformed, " +
					"`on_conflict` is not one of the two policies, or the model has no file " +
					"on disk to benchmark.",
				Codes: []model.ErrorCode{
					bench.CodeSweepTooLarge,
					model.CodeBadFlags,
					model.CodeModelMissing,
					model.CodeExtraFlagForbidden,
					model.CodeIdempotencyKeyReused,
				},
			},
		},
	}
}

func (a *API) benchPreflightRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/bench/preflight",
		Auth:        AuthSession,
		OperationID: "benchPreflight",
		Summary: "What this sweep would do before it is committed: GPU conflicts, free VRAM, " +
			"the point count, a duration estimate, and every FlagSet field llama-bench has no " +
			"equivalent for.",
		Tag: "bench",
		Query: []QueryParam{
			{Name: "model_id", Description: "The model to benchmark.", Required: true},
			{
				Name: "sweep",
				Description: "The sweep document as JSON, url-encoded. Absent means a " +
					"single-point sweep, which is what the estimate for \"just run it once\" is.",
			},
			{Name: "repetitions", Description: "llama-bench's `-r`.", Type: "integer"},
		},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.bench()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			sweep, err := bench.ParseSweep([]byte(r.URL.Query().Get("sweep")))
			if err != nil {
				a.writeBenchError(w, r, err)
				return
			}
			out, err := svc.Preflight(r.Context(), bench.PreflightRequest{
				ModelID:     r.URL.Query().Get("model_id"),
				Sweep:       sweep,
				Repetitions: int(queryInt64(r, "repetitions", 0)),
			})
			if err != nil {
				a.writeBenchError(w, r, err)
				return
			}
			if err := WriteJSON(w, http.StatusOK, benchPreflightDTO(out)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status:      http.StatusOK,
			Description: "What would happen, and what would be ignored.",
			Body:        BenchPreflightDTO{},
		},
		Errors: []Response{{
			Status: http.StatusUnprocessableEntity,
			Description: "No `model_id`, a model that does not exist, or a sweep that is " +
				"malformed or expands past the point limit.",
			Codes: []model.ErrorCode{
				model.CodeModelMissing, model.CodeBadFlags, bench.CodeSweepTooLarge,
			},
		}},
	}
}

func (a *API) getBenchRunRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/bench/runs/{id}",
		Auth:        AuthSession,
		OperationID: "getBenchRun",
		Summary: "One run: its points, the captured environment, and the live job's " +
			"`{points_done, points_total, current}` progress.",
		Tag: "bench",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.bench()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			v, err := svc.Get(r.Context(), r.PathValue("id"))
			if err != nil {
				a.writeBenchError(w, r, err)
				return
			}
			if err := WriteJSON(w, http.StatusOK, benchDetailDTO(v)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status:      http.StatusOK,
			Description: "The run.",
			Body:        BenchRunDetailDTO{},
		},
		Errors: []Response{{
			Status:      http.StatusNotFound,
			Description: "No run has this id.",
			Codes:       []model.ErrorCode{CodeNotFound},
		}},
	}
}

func (a *API) patchBenchRunRoute() Route {
	return Route{
		Method:      http.MethodPatch,
		Pattern:     BasePath + "/bench/runs/{id}",
		Auth:        AuthSession,
		OperationID: "patchBenchRun",
		Summary:     "Rename or annotate a run. Its inputs and results are immutable.",
		Tag:         "bench",
		RequestBody: PatchBenchRunRequest{},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.bench()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			var body PatchBenchRunRequest
			if err := DecodeJSON(w, r, &body); err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			run, err := svc.Annotate(r.Context(), r.PathValue("id"), body.Name, body.Notes)
			if err != nil {
				a.writeBenchError(w, r, err)
				return
			}
			if err := WriteJSON(w, http.StatusOK, benchRunDTO(run)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status:      http.StatusOK,
			Description: "The updated run.",
			Body:        BenchRunDTO{},
		},
		Errors: []Response{{
			Status:      http.StatusNotFound,
			Description: "No run has this id.",
			Codes:       []model.ErrorCode{CodeNotFound},
		}},
	}
}

func (a *API) deleteBenchRunRoute() Route {
	return Route{
		Method:      http.MethodDelete,
		Pattern:     BasePath + "/bench/runs/{id}",
		Auth:        AuthSession,
		OperationID: "deleteBenchRun",
		Summary: "Delete a run and its points and results. Refused while its job is live " +
			"or while it still owes stopped instances a restart.",
		Tag: "bench",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.bench()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := svc.Delete(r.Context(), r.PathValue("id")); err != nil {
				a.writeBenchError(w, r, err)
				return
			}
			WriteNoContent(w)
		}),
		Success: Response{
			Status:      http.StatusNoContent,
			Description: "The run is gone.",
		},
		Errors: []Response{
			{
				Status:      http.StatusNotFound,
				Description: "No run has this id.",
				Codes:       []model.ErrorCode{CodeNotFound},
			},
			{
				Status: http.StatusConflict,
				Description: "The run's job is live, or it stopped production instances it " +
					"has not restarted yet — deleting the row would lose the list the boot " +
					"restore reads.",
				Codes: []model.ErrorCode{bench.CodeBenchRunning},
			},
		},
	}
}

func (a *API) startBenchRunRoute() Route {
	return Route{
		Method:      http.MethodPost,
		Pattern:     BasePath + "/bench/runs/{id}/start",
		Auth:        AuthSession,
		OperationID: "startBenchRun",
		Summary:     "Queue a draft run. It waits for the bench lease, one sweep at a time.",
		Tag:         "bench",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.bench()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			id := r.PathValue("id")
			job, err := svc.Start(r.Context(), id)
			if err != nil {
				a.writeBenchError(w, r, err)
				return
			}
			if err := WriteJSON(w, http.StatusAccepted, benchReceipt(job, id)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status:      http.StatusAccepted,
			Description: "The sweep was queued. Watch `job_id`.",
			Body:        JobReceiptDTO{},
		},
		Errors: []Response{
			{
				Status:      http.StatusNotFound,
				Description: "No run has this id.",
				Codes:       []model.ErrorCode{CodeNotFound},
			},
			{
				Status:      http.StatusConflict,
				Description: "This run is not a draft, or a job already holds it.",
				Codes:       []model.ErrorCode{bench.CodeBenchNotStartable, model.CodeJobInFlight},
			},
		},
	}
}

func (a *API) cancelBenchRunRoute() Route {
	return Route{
		Method:      http.MethodPost,
		Pattern:     BasePath + "/bench/runs/{id}/cancel",
		Auth:        AuthSession,
		OperationID: "cancelBenchRun",
		Summary: "Stop a running sweep. The process group is signaled, the remaining points " +
			"are marked `skipped`, and the stop-and-restore finalizer restarts every " +
			"instance the run stopped.",
		Tag: "bench",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.bench()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			id := r.PathValue("id")
			job, err := svc.Cancel(r.Context(), id)
			if err != nil {
				a.writeBenchError(w, r, err)
				return
			}
			if err := WriteJSON(w, http.StatusAccepted, benchReceipt(job, id)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status: http.StatusAccepted,
			Description: "The cancel was requested. The sweep stops after the current point " +
				"and the instances it stopped are restarted.",
			Body: JobReceiptDTO{},
		},
		Errors: []Response{
			{
				Status:      http.StatusNotFound,
				Description: "No run has this id.",
				Codes:       []model.ErrorCode{CodeNotFound},
			},
			{
				Status:      http.StatusConflict,
				Description: "No job for this run is live, so there is nothing to cancel.",
				Codes:       []model.ErrorCode{bench.CodeBenchNotCancelable},
			},
		},
	}
}

func (a *API) benchResultsRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/bench/runs/{id}/results",
		Auth:        AuthSession,
		OperationID: "getBenchRunResults",
		Summary:     "The run's results, flattened one row per measurement with all axes as columns.",
		Tag:         "bench",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.bench()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			rows, err := svc.Results(r.Context(), r.PathValue("id"))
			if err != nil {
				a.writeBenchError(w, r, err)
				return
			}
			items := make([]BenchResultRowDTO, 0, len(rows))
			for _, row := range rows {
				items = append(items, benchResultRowDTO(row))
			}
			if err := WriteJSON(w, http.StatusOK, NewList(items, len(items), nil)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status:      http.StatusOK,
			Description: "The flattened results, in point order.",
			Body:        List[BenchResultRowDTO]{},
		},
		Errors: []Response{{
			Status:      http.StatusNotFound,
			Description: "No run has this id.",
			Codes:       []model.ErrorCode{CodeNotFound},
		}},
	}
}

func (a *API) benchExportRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/bench/runs/{id}/export",
		Auth:        AuthSession,
		OperationID: "exportBenchRun",
		Summary: "Export a run: `json` (run + points + results, self-describing), `csv` " +
			"(one row per result), or `md` (a provenance header plus a table, ready to " +
			"paste into an issue). Each carries a `Content-Disposition` filename.",
		Tag: "bench",
		Query: []QueryParam{{
			Name:        "format",
			Description: "json, csv or md. Default json.",
			Enum:        benchFormats(),
		}},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.bench()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			out, err := svc.Export(r.Context(), r.PathValue("id"),
				bench.Format(r.URL.Query().Get("format")))
			if err != nil {
				a.writeBenchError(w, r, err)
				return
			}
			h := w.Header()
			h.Set("Content-Type", out.MediaType)
			h.Set("Cache-Control", "no-store")
			h.Set("Content-Disposition", `attachment; filename="`+out.Filename+`"`)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(out.Body)
		}),
		Success: Response{
			Status: http.StatusOK,
			Description: "The export, in the requested format, with a `Content-Disposition` " +
				"filename derived from the run's name.",
			MediaType:     "application/json",
			AltMediaTypes: []string{"text/csv", "text/markdown"},
		},
		Errors: []Response{
			{
				Status:      http.StatusNotFound,
				Description: "No run has this id.",
				Codes:       []model.ErrorCode{CodeNotFound},
			},
			{
				Status:      http.StatusUnprocessableEntity,
				Description: "`?format=` is not one of json, csv, md.",
				Codes:       []model.ErrorCode{model.CodeBadFlags},
			},
		},
	}
}

func (a *API) benchCompareRoute() Route {
	return Route{
		Method:      http.MethodPost,
		Pattern:     BasePath + "/bench/compare",
		Auth:        AuthSession,
		OperationID: "compareBenchRuns",
		Summary: "Chart-ready series across runs: one grouped query over " +
			"`bench_points ⋈ bench_results` with the sweep axes as columns.",
		Tag:         "bench",
		RequestBody: CompareBenchRequest{},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.bench()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			var body CompareBenchRequest
			if err := DecodeJSON(w, r, &body); err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			q := store.BenchCompareQuery{
				RunIDs: body.RunIDs,
				X:      store.BenchAxis(body.X),
				Series: store.BenchAxis(body.Series),
				Y:      store.BenchMetric(body.Y),
			}
			if len(body.Filters) > 0 {
				q.Filters = make(map[store.BenchAxis]string, len(body.Filters))
				for k, v := range body.Filters {
					q.Filters[store.BenchAxis(k)] = v
				}
			}
			points, err := svc.Compare(r.Context(), q)
			if err != nil {
				a.writeBenchError(w, r, err)
				return
			}
			out := BenchCompareDTO{
				X: body.X, Y: body.Y, Series: body.Series,
				Points: make([]BenchComparePointDTO, 0, len(points)),
			}
			for _, p := range points {
				out.Points = append(out.Points, BenchComparePointDTO{
					X: p.X, Series: p.Series, Value: p.Value, Samples: p.Samples,
				})
			}
			if err := WriteJSON(w, http.StatusOK, out); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status:      http.StatusOK,
			Description: "The aggregated series.",
			Body:        BenchCompareDTO{},
		},
		Errors: []Response{{
			Status:      http.StatusUnprocessableEntity,
			Description: "`x`, `series` or a filter key is not a comparable axis, or `y` is not a measured metric.",
			Codes:       []model.ErrorCode{model.CodeBadFlags},
		}},
	}
}

func (a *API) benchSeriesRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/bench/series",
		Auth:        AuthSession,
		OperationID: "benchSeries",
		Summary:     "History for one model across llama.cpp versions, oldest first.",
		Tag:         "bench",
		Query: []QueryParam{
			{Name: "model_id", Description: "Restrict to one model."},
			{Name: "test", Description: "The test shape to plot.", Enum: benchTestKinds()},
			{Name: "metric", Description: "The measured value. Default avg_ts.", Enum: benchMetrics()},
			{Name: "group", Description: "What each line is labeled by. Default llamacpp_tag.",
				Enum: benchAxes()},
			{Name: "limit", Description: "Maximum points to return. Default 200.", Type: "integer"},
		},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.bench()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			q := r.URL.Query()
			query := store.BenchSeriesQuery{
				ModelID: q.Get("model_id"),
				Test:    model.BenchTestKind(q.Get("test")),
				Metric:  store.BenchMetric(q.Get("metric")),
				Group:   store.BenchAxis(q.Get("group")),
				Limit:   int(queryInt64(r, "limit", 0)),
			}
			points, err := svc.Series(r.Context(), query)
			if err != nil {
				a.writeBenchError(w, r, err)
				return
			}
			out := BenchSeriesDTO{
				Metric: orDefault(q.Get("metric"), string(store.MetricAvgTS)),
				Group:  orDefault(q.Get("group"), string(store.AxisLlamacppTag)),
				Points: make([]BenchSeriesPointDTO, 0, len(points)),
			}
			for _, p := range points {
				out.Points = append(out.Points, BenchSeriesPointDTO{
					RunID: p.RunID, RunName: p.RunName, Group: p.Group,
					At: Time(p.At), Value: p.Value, Samples: p.Samples,
				})
			}
			if err := WriteJSON(w, http.StatusOK, out); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status:      http.StatusOK,
			Description: "The history, oldest first.",
			Body:        BenchSeriesDTO{},
		},
		Errors: []Response{{
			Status:      http.StatusUnprocessableEntity,
			Description: "`metric`, `group` or `test` is not one this API can plot.",
			Codes:       []model.ErrorCode{model.CodeBadFlags},
		}},
	}
}

// bench returns the service, or the 503 a build without one answers with.
func (a *API) bench() (BenchService, error) {
	if a.cfg.Bench == nil {
		return nil, Errorf(http.StatusServiceUnavailable, CodeInternalError,
			"this daemon was built without a bench runner")
	}
	return a.cfg.Bench, nil
}

// writeBenchError maps this domain's codes onto the statuses section 3.13 pairs
// them with, then hands the rest to WriteError.
func (a *API) writeBenchError(w http.ResponseWriter, r *http.Request, err error) {
	var me model.Error
	if errors.As(err, &me) {
		if status, ok := bench.Statuses()[me.Code]; ok {
			WriteError(w, r, a.log, &Error{
				Status:  status,
				Code:    me.Code,
				Message: me.Message,
				Details: me.Details,
				Err:     err,
			})
			return
		}
	}
	WriteError(w, r, a.log, err)
}

// benchReceipt renders section 3's long-action shape. The subject is the RUN,
// which is also `jobs.subject_id` for a `bench_run` (section 2.3a), so a draft
// with no job still names the row the client should watch.
func benchReceipt(j model.Job, runID string) JobReceiptDTO {
	out := JobReceiptDTO{
		Subject: SubjectDTO{Type: string(model.SubjectBenchRun), ID: runID},
	}
	if j.ID != "" {
		id := j.ID
		out.JobID = &id
		out.Subject = SubjectDTO{Type: string(j.SubjectType), ID: j.SubjectID}
	}
	return out
}

func benchRunDTO(run store.BenchRun) BenchRunDTO {
	dto := BenchRunDTO{
		ID:                run.ID,
		Name:              run.Name,
		State:             string(run.State),
		ModelID:           run.ModelID,
		ModelLabel:        run.ModelLabel,
		ModelPath:         run.ModelPath,
		QuantLabel:        run.QuantLabel,
		LlamacppVersionID: run.LlamacppVersionID,
		LlamacppTag:       run.LlamacppTag,
		LlamacppCommit:    run.LlamacppCommit,
		LlamacppBackend:   run.LlamacppBackend,
		Repetitions:       run.Repetitions,
		PointsTotal:       run.PointsTotal,
		PointsDone:        run.PointsDone,
		PointsFailed:      run.PointsFailed,
		StoppedInstances:  []string{},
		RestoreDone:       run.RestoreDone,
		ErrorMessage:      run.ErrorMessage,
		Notes:             run.Notes,
		CreatedAt:         Time(run.CreatedAt),
		StartedAt:         TimePtr(run.StartedAt),
		FinishedAt:        TimePtr(run.FinishedAt),
	}
	if run.StoppedInstancesJSON != nil {
		var ids []string
		if err := json.Unmarshal([]byte(*run.StoppedInstancesJSON), &ids); err == nil && ids != nil {
			dto.StoppedInstances = ids
		}
	}
	return dto
}

func benchPointDTOs(points []store.BenchPoint) []BenchPointDTO {
	out := make([]BenchPointDTO, 0, len(points))
	for _, p := range points {
		out = append(out, benchPointDTO(p))
	}
	return out
}

func benchPointDTO(p store.BenchPoint) BenchPointDTO {
	dto := BenchPointDTO{
		ID:           p.ID,
		Ordinal:      p.Ordinal,
		State:        string(p.State),
		Label:        bench.PointLabel(p),
		Argv:         []string{},
		NGpuLayers:   p.NGpuLayers,
		NBatch:       p.NBatch,
		NUbatch:      p.NUbatch,
		NThreads:     p.NThreads,
		FlashAttn:    p.FlashAttn,
		TypeK:        p.TypeK,
		TypeV:        p.TypeV,
		SplitMode:    p.SplitMode,
		TensorSplit:  p.TensorSplit,
		NDepth:       p.NDepth,
		StartedAt:    TimePtr(p.StartedAt),
		FinishedAt:   TimePtr(p.FinishedAt),
		ErrorMessage: p.ErrorMessage,
	}
	var argv []string
	if err := json.Unmarshal([]byte(p.ArgsJSON), &argv); err == nil && argv != nil {
		dto.Argv = argv
	}
	return dto
}

func benchDetailDTO(v bench.View) BenchRunDetailDTO {
	out := BenchRunDetailDTO{
		Run:    benchRunDTO(v.Run),
		Points: benchPointDTOs(v.Points),
		Sweep:  map[string]any{},
		GPU:    []map[string]any{},
		Host:   map[string]any{},
	}
	_ = json.Unmarshal([]byte(v.Run.SweepJSON), &out.Sweep)
	_ = json.Unmarshal([]byte(v.Run.GPUJSON), &out.GPU)
	_ = json.Unmarshal([]byte(v.Run.HostJSON), &out.Host)
	if out.GPU == nil {
		out.GPU = []map[string]any{}
	}

	if v.Job != nil {
		id, state := v.Job.ID, string(v.Job.State)
		out.JobID, out.JobState = &id, &state
		if v.Job.ProgressJSON != nil {
			var p BenchProgressDTO
			if err := json.Unmarshal([]byte(*v.Job.ProgressJSON), &p); err == nil {
				out.Progress = &p
			}
		}
	}
	return out
}

func benchResultRowDTO(r bench.ResultRow) BenchResultRowDTO {
	return BenchResultRowDTO{
		PointID:     r.PointID,
		Ordinal:     r.Ordinal,
		Label:       r.Label,
		NGpuLayers:  r.NGpuLayers,
		NBatch:      r.NBatch,
		NUbatch:     r.NUbatch,
		NThreads:    r.NThreads,
		FlashAttn:   r.FlashAttn,
		TypeK:       r.TypeK,
		TypeV:       r.TypeV,
		SplitMode:   r.SplitMode,
		TensorSplit: r.TensorSplit,
		TestKind:    string(r.TestKind),
		NPrompt:     r.NPrompt,
		NGen:        r.NGen,
		NDepth:      r.NDepth,
		AvgTS:       r.AvgTS,
		StddevTS:    r.StddevTS,
		AvgNS:       r.AvgNS,
		StddevNS:    r.StddevNS,
		Samples:     r.Samples,
	}
}

func benchPreflightDTO(p bench.Preflight) BenchPreflightDTO {
	out := BenchPreflightDTO{
		ModelID:             p.ModelID,
		ModelLabel:          p.ModelLabel,
		ModelPath:           p.ModelPath,
		LlamacppVersionID:   p.LlamacppVersionID,
		LlamacppTag:         p.LlamacppTag,
		RuntimeReady:        p.RuntimeReady,
		PointsTotal:         p.PointsTotal,
		Repetitions:         p.Repetitions,
		EstimatedSec:        int(p.Estimate.Seconds()),
		EstimateFromHistory: p.EstimateFromHistory,
		ExclusiveGPU:        p.ExclusiveGPU,
		TargetGPUs:          p.TargetGPUs,
		GPUIdentityKnown:    p.GPUIdentityKnown,
		Conflicts:           make([]BenchConflictDTO, 0, len(p.Conflicts)),
		FreeVRAMBytes:       p.FreeVRAMBytes,
		IgnoredFlags:        make([]BenchIgnoredFlagDTO, 0, len(p.IgnoredFlags)),
		Notes:               p.Notes,
	}
	if out.TargetGPUs == nil {
		out.TargetGPUs = []string{}
	}
	if out.FreeVRAMBytes == nil {
		out.FreeVRAMBytes = map[string]uint64{}
	}
	if out.Notes == nil {
		out.Notes = []string{}
	}
	for _, c := range p.Conflicts {
		gpus := c.GPUUUIDs
		if gpus == nil {
			gpus = []string{}
		}
		out.Conflicts = append(out.Conflicts, BenchConflictDTO{
			InstanceID:  c.InstanceID,
			Name:        c.Name,
			State:       string(c.State),
			GPUUUIDs:    gpus,
			Attribution: string(c.Attribution),
			Assumed:     c.Assumed,
		})
	}
	for _, f := range p.IgnoredFlags {
		out.IgnoredFlags = append(out.IgnoredFlags,
			BenchIgnoredFlagDTO{Field: f.Field, Reason: f.Reason})
	}
	return out
}

// The four enum lists the route table documents. Each is derived from the
// closed set that already exists rather than restated, so a value added to one
// of those sets appears in openapi.json — and therefore in the generated
// TypeScript — in the same commit.
func benchRunStates() []string {
	out := make([]string, 0, len(model.BenchRunStateValues()))
	for _, v := range model.BenchRunStateValues() {
		out = append(out, string(v))
	}
	return out
}

func benchTestKinds() []string {
	out := make([]string, 0, len(model.BenchTestKindValues()))
	for _, v := range model.BenchTestKindValues() {
		out = append(out, string(v))
	}
	return out
}

func benchFormats() []string {
	out := make([]string, 0, len(bench.FormatValues()))
	for _, v := range bench.FormatValues() {
		out = append(out, string(v))
	}
	return out
}

func benchMetrics() []string {
	out := make([]string, 0, len(store.BenchMetrics()))
	for _, v := range store.BenchMetrics() {
		out = append(out, string(v))
	}
	return out
}

func benchAxes() []string {
	out := make([]string, 0, len(store.BenchAxes()))
	for _, v := range store.BenchAxes() {
		out = append(out, string(v))
	}
	return out
}

// orDefault echoes a query parameter back with the service's own fallback
// applied, so the response says which metric was actually plotted rather than
// leaving the client to re-derive the default.
func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
