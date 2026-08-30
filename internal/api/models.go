package api

import (
	"context"
	"net/http"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/models"
)

// The local models and cache endpoints of DESIGN section 3.7.
//
// Everything section 3.7's table names is here except the two rows that belong
// to another subsystem's transaction: `POST /cache/roots/{id}/promote` shares
// its write path with `PATCH /settings {"hf.hub_dir"}` (§7.2a) and is
// registered here, while the download-side endpoints of §3.8 are internal/hf's.
//
// Three long actions — scan, verify and delete — answer `202` with a job id, per
// section 3's rule that anything starting work returns a receipt and reports
// over SSE. A delete is a job rather than a synchronous unlink because the plan
// it executes is refcounted across a whole repository (D28) and because the row
// must move `deleting → deleted` under a job that survives a restart.

// ModelService is everything this layer needs from internal/models. The consumer
// owns the interface (DESIGN section 1); *models.Service satisfies it.
type ModelService interface {
	List(ctx context.Context, p models.ListParams) ([]models.View, error)
	Get(ctx context.Context, id string) (models.Detail, error)
	Metadata(ctx context.Context, id string) (map[string]any, error)
	DeletePreview(ctx context.Context, id string) (models.DeletePlan, error)
	Delete(ctx context.Context, id string) (models.DeletePlan, models.JobRef, error)
	Verify(ctx context.Context, id string) (models.JobRef, error)
	PairMmproj(ctx context.Context, id, mmprojID string) (models.View, error)

	Roots(ctx context.Context) ([]models.RootView, error)
	AddRoot(ctx context.Context, path string) (models.RootView, models.JobRef, error)
	PromoteRoot(ctx context.Context, id string) (models.RootView, models.JobRef, error)
	DetachRoot(ctx context.Context, id string) error

	RequestScan(ctx context.Context, rootID string, trigger model.CacheScanTrigger) (model.CacheScan, models.JobRef, error)
	Scan(ctx context.Context, id string) (model.CacheScan, error)

	Strays(ctx context.Context, rootID string) ([]model.StrayFile, error)
	DeleteStray(ctx context.Context, id string, deleteFile bool) error
	DismissStray(ctx context.Context, id string) error
}

// ModelDTO is one row of `GET /api/v1/models`.
//
// The GGUF geometry stays nullable on the wire, all of it. A model whose header
// has not been parsed — one that is still downloading — has NULL for every
// field, and the fit panel must say "not yet known" rather than show a zero that
// reads as an answer (F14).
type ModelDTO struct {
	ID       string `json:"id"`
	RootID   string `json:"root_id"`
	RootPath string `json:"root_path"`
	RepoID   string `json:"repo_id"`
	// Revision is the resolved commit sha — the snapshot DIRECTORY name, never
	// a branch (§7.2).
	Revision string `json:"revision"`
	// RefName is a display field: the branch a `refs/` entry points at this
	// revision with, null for a snapshot no ref names.
	RefName    *string `json:"ref_name"`
	QuantLabel *string `json:"quant_label"`
	Kind       string  `json:"kind"`
	State      string  `json:"state"`
	Origin     string  `json:"origin"`

	SnapshotDir string `json:"snapshot_dir"`
	PrimaryFile string `json:"primary_file"`
	// Path is the resolved file llama.cpp would be handed, so a client does not
	// have to join two fields and get the separator wrong.
	Path        string `json:"path"`
	ShardCount  int    `json:"shard_count"`
	TotalBytes  int64  `json:"total_bytes"`
	BytesOnDisk int64  `json:"bytes_on_disk"`

	MmprojModelID *string `json:"mmproj_model_id"`
	MmprojAuto    bool    `json:"mmproj_auto"`
	HasVision     bool    `json:"has_vision"`

	Arch           *string `json:"arch"`
	NLayer         *int64  `json:"n_layer"`
	NCtxTrain      *int64  `json:"n_ctx_train"`
	NEmbd          *int64  `json:"n_embd"`
	NFF            *int64  `json:"n_ff"`
	NHead          *int64  `json:"n_head"`
	NVocab         *int64  `json:"n_vocab"`
	NExpert        *int64  `json:"n_expert"`
	NExpertUsed    *int64  `json:"n_expert_used"`
	HeadDimK       *int64  `json:"head_dim_k"`
	HeadDimV       *int64  `json:"head_dim_v"`
	SWAWindow      *int64  `json:"swa_window"`
	SWAPattern     *int64  `json:"swa_pattern"`
	TokenizerModel *string `json:"tokenizer_model"`
	FileType       *string `json:"file_type"`
	// NHeadKV is the `n_head_kv_json` column decoded: a number when the file
	// gave a scalar and an array when it gave one per layer (D30). Both shapes
	// reach the client, because §8.3 sizes the KV cache per layer and an
	// averaged scalar is wrong exactly where the answer matters.
	NHeadKV any `json:"n_head_kv"`
	// TensorSummary is section 8.2's bucketing of the tensor index.
	TensorSummary any `json:"tensor_summary"`

	GGUFParsedAt   *string `json:"gguf_parsed_at"`
	LastVerifiedAt *string `json:"last_verified_at"`
	CreatedAt      string  `json:"created_at"`
	UpdatedAt      string  `json:"updated_at"`

	// InUseBy names the non-deleted instances referencing this model, so the UI
	// disables Delete rather than letting the click return a 409.
	InUseBy []InstanceRefDTO `json:"in_use_by"`
}

// InstanceRefDTO names one instance that references a model.
type InstanceRefDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Role is `model`, `mmproj` or `draft` — "used as the draft model by
	// inference-1" is a materially different sentence from "used by inference-1".
	Role string `json:"role"`
	// Deleted marks a soft-deleted instance. It is never true in a model
	// delete's 409 (a soft-deleted instance does not block one) and may be true
	// in a cache-root detach's, where it is the whole reason for the refusal
	// (§7.2a).
	Deleted bool `json:"deleted"`
}

// ModelFileDTO is one row of `model_files`.
type ModelFileDTO struct {
	ID               string  `json:"id"`
	Filename         string  `json:"filename"`
	ShardIndex       int     `json:"shard_index"`
	ShardTotal       int     `json:"shard_total"`
	SizeBytes        int64   `json:"size_bytes"`
	BytesOnDisk      int64   `json:"bytes_on_disk"`
	Etag             *string `json:"etag"`
	BlobPath         *string `json:"blob_path"`
	LinkPath         *string `json:"link_path"`
	State            string  `json:"state"`
	ChecksumVerified bool    `json:"checksum_verified"`
}

// ModelDetailDTO is `GET /api/v1/models/{id}`.
type ModelDetailDTO struct {
	Model ModelDTO       `json:"model"`
	Files []ModelFileDTO `json:"files"`
	// Mmproj is the paired projector row, null when none is paired.
	Mmproj *ModelDTO `json:"mmproj"`
	// MmprojCandidates is §7.2's picker: the projectors in this repo+revision
	// the auto-pairing rule declined to choose between. It is populated even
	// when one is already paired, because changing a wrong automatic pairing is
	// what the endpoint is for.
	MmprojCandidates []ModelDTO `json:"mmproj_candidates"`
}

// DeletePreviewDTO is `GET /api/v1/models/{id}/delete-preview` (D28).
type DeletePreviewDTO struct {
	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`
	// BlobsSharedKept is how many blobs another snapshot still references, and
	// SharedBytes what they occupy. Both are reported because they explain a
	// `bytes` smaller than the model's own size — a blob shared by two
	// revisions must never be removed out from under one of them.
	BlobsSharedKept int   `json:"blobs_shared_kept"`
	SharedBytes     int64 `json:"shared_bytes"`
	// RemovesRepoDir reports that the whole `models--…` directory would go.
	RemovesRepoDir bool             `json:"removes_repo_dir"`
	InUseBy        []InstanceRefDTO `json:"in_use_by"`
}

// JobReceiptDTO is section 3's long-action shape: `{"job_id", "subject"}`.
type JobReceiptDTO struct {
	// JobID is null on a daemon built without a job queue, where the domain row
	// exists and nothing is scheduled.
	JobID   *string    `json:"job_id"`
	Subject SubjectDTO `json:"subject"`
}

// SubjectDTO names the domain row a job acts on (§2.3a).
type SubjectDTO struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// DeleteModelResponse is the 202 of `DELETE /api/v1/models/{id}`: the receipt,
// plus the plan that is being executed so the UI can show what it freed without
// a second request.
type DeleteModelResponse struct {
	JobReceiptDTO
	Plan DeletePreviewDTO `json:"plan"`
}

// CacheRootDTO is one row of `GET /api/v1/cache/roots`.
type CacheRootDTO struct {
	ID string `json:"id"`
	// Path is the HUB directory itself and need not end in `/hub` (§7.2 rule 1).
	Path string `json:"path"`
	// HFHome is the courtesy projection, empty when the path has no `/hub`
	// suffix. The Storage form shows it beneath the editable hub field only
	// when one exists.
	HFHome     string  `json:"hf_home"`
	IsPrimary  bool    `json:"is_primary"`
	Writable   bool    `json:"writable"`
	SymlinksOK bool    `json:"symlinks_ok"`
	DetectedAs *string `json:"detected_from"`
	FSType     *string `json:"fs_type"`
	TotalBytes *int64  `json:"total_bytes"`
	FreeBytes  *int64  `json:"free_bytes"`
	Models     int64   `json:"models"`
	// BytesOnDisk is what this root's models occupy — deliberately shown beside
	// FreeBytes rather than instead of it, because "our models take 400 GB" and
	// "the disk has 20 GB left" are different facts and the user needs both.
	BytesOnDisk int64   `json:"bytes_on_disk"`
	LastScanAt  *string `json:"last_scan_at"`
	CreatedAt   string  `json:"created_at"`
}

// AddCacheRootRequest is the body of `POST /api/v1/cache/roots`.
type AddCacheRootRequest struct {
	Path string `json:"path"`
}

// AddCacheRootResponse carries the new root and the scan queued for it. A new
// root is never primary (§3.7).
type AddCacheRootResponse struct {
	Root CacheRootDTO   `json:"root"`
	Scan *JobReceiptDTO `json:"scan"`
}

// PromoteCacheRootResponse is the 202 of the promote endpoint.
type PromoteCacheRootResponse struct {
	Root CacheRootDTO   `json:"root"`
	Scan *JobReceiptDTO `json:"scan"`
	// RestartRequired is always false and is stated rather than omitted:
	// relocating the cache touches no unit file (D57) and no listener, and the
	// UI should not offer a restart button for it (§7.2a).
	RestartRequired bool `json:"restart_required"`
}

// PairMmprojRequest is the body of `POST /api/v1/models/{id}/pair-mmproj`. An
// empty id detaches the projector, manually — either way `mmproj_auto` becomes
// false and no later scan overrules the choice.
type PairMmprojRequest struct {
	MmprojModelID string `json:"mmproj_model_id"`
}

// ScanRequest is the body of `POST /api/v1/cache/scan`. An omitted root scans
// the primary one.
type ScanRequest struct {
	RootID string `json:"root_id,omitempty"`
}

// CacheScanDTO is `GET /api/v1/cache/scans/{id}`.
type CacheScanDTO struct {
	ID      string `json:"id"`
	RootID  string `json:"root_id"`
	State   string `json:"state"`
	Trigger string `json:"trigger"`

	DirsSeen      int64 `json:"dirs_seen"`
	FilesSeen     int64 `json:"files_seen"`
	ModelsFound   int64 `json:"models_found"`
	ModelsAdded   int64 `json:"models_added"`
	ModelsMissing int64 `json:"models_missing"`
	StraysFound   int64 `json:"strays_found"`
	BytesTotal    int64 `json:"bytes_total"`

	ErrorMessage *string `json:"error_message"`
	StartedAt    *string `json:"started_at"`
	FinishedAt   *string `json:"finished_at"`
	CreatedAt    string  `json:"created_at"`
}

// StrayFileDTO is one row of `GET /api/v1/cache/strays`.
type StrayFileDTO struct {
	ID        string `json:"id"`
	RootID    string `json:"root_id"`
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
	// Reason is `outside_snapshot`, `orphan_blob`, `broken_symlink` or
	// `unparsable`.
	Reason      string  `json:"reason"`
	FirstSeenAt string  `json:"first_seen_at"`
	LastSeenAt  string  `json:"last_seen_at"`
	DismissedAt *string `json:"dismissed_at"`
}

// MetadataDTO is `GET /api/v1/models/{id}/metadata`: the full GGUF key/value
// map, re-read from the file.
type MetadataDTO struct {
	ModelID string         `json:"model_id"`
	KV      map[string]any `json:"kv"`
}

func (a *API) modelRoutes() []Route {
	return []Route{
		a.listModelsRoute(),
		a.getModelRoute(),
		a.modelMetadataRoute(),
		a.deletePreviewRoute(),
		a.deleteModelRoute(),
		a.verifyModelRoute(),
		a.pairMmprojRoute(),

		a.listCacheRootsRoute(),
		a.addCacheRootRoute(),
		a.promoteCacheRootRoute(),
		a.detachCacheRootRoute(),

		a.cacheScanRoute(),
		a.getCacheScanRoute(),
		a.listStraysRoute(),
		a.deleteStrayRoute(),
		a.dismissStrayRoute(),
	}
}

func (a *API) listModelsRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/models",
		Auth:        AuthSession,
		OperationID: "listModels",
		Summary:     "The local model catalog, with the instances using each row.",
		Tag:         "models",
		Query: []QueryParam{
			{Name: "state", Description: "Filter by model state; repeatable as a comma-separated list.",
				Enum: enumStrings(model.ModelStateValues())},
			{Name: "kind", Description: "Filter by model kind; repeatable as a comma-separated list.",
				Enum: enumStrings(model.ModelKindValues())},
			{Name: "q", Description: "Case-insensitive substring of the repo id or the primary file."},
			{Name: "sort", Description: "Ordering.", Enum: []string{"repo", "size", "recent"}},
			{Name: "root_id", Description: "Restrict to one cache root."},
			{Name: "include_deleted", Type: "boolean",
				Description: "Include rows in state `deleted`. Deleting a model never removes its " +
					"row (§7.2), so those rows are history rather than catalog."},
		},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.models()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			views, err := svc.List(r.Context(), models.ListParams{
				State:          enumList[model.ModelState](r, "state"),
				Kind:           enumList[model.ModelKind](r, "kind"),
				Query:          r.URL.Query().Get("q"),
				Sort:           r.URL.Query().Get("sort"),
				RootID:         r.URL.Query().Get("root_id"),
				IncludeDeleted: queryBool(r, "include_deleted"),
			})
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			items := make([]ModelDTO, 0, len(views))
			for _, v := range views {
				items = append(items, modelDTO(v))
			}
			if err := WriteJSON(w, http.StatusOK, NewList(items, len(items), nil)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{Status: http.StatusOK, Description: "The catalog.", Body: List[ModelDTO]{}},
	}
}

func (a *API) getModelRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/models/{id}",
		Auth:        AuthSession,
		OperationID: "getModel",
		Summary:     "One model with its files, its paired projector and the projector picker.",
		Tag:         "models",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.models()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			d, err := svc.Get(r.Context(), r.PathValue("id"))
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := WriteJSON(w, http.StatusOK, modelDetailDTO(d)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{Status: http.StatusOK, Description: "The model.", Body: ModelDetailDTO{}},
		Errors: []Response{{
			Status: http.StatusNotFound, Description: "No model has this id.",
			Codes: []model.ErrorCode{CodeNotFound},
		}},
	}
}

func (a *API) modelMetadataRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/models/{id}/metadata",
		Auth:        AuthSession,
		OperationID: "getModelMetadata",
		Summary: "The full GGUF key/value map, re-read from the file so a scan never has to " +
			"retain a tokenizer table it will be asked about once.",
		Tag: "models",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.models()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			id := r.PathValue("id")
			kv, err := svc.Metadata(r.Context(), id)
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := WriteJSON(w, http.StatusOK, MetadataDTO{ModelID: id, KV: kv}); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{Status: http.StatusOK, Description: "The metadata table.", Body: MetadataDTO{}},
		Errors: []Response{
			{Status: http.StatusNotFound, Description: "No model has this id.",
				Codes: []model.ErrorCode{CodeNotFound}},
			{Status: http.StatusUnprocessableEntity,
				Description: "The file is not readable and no metadata was recorded.",
				Codes:       []model.ErrorCode{model.CodeModelMissing}},
		},
	}
}

func (a *API) deletePreviewRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/models/{id}/delete-preview",
		Auth:        AuthSession,
		OperationID: "previewModelDelete",
		Summary: "What deleting this model would free, with blobs refcounted across every " +
			"snapshot in the repository (D28).",
		Tag: "models",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.models()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			plan, err := svc.DeletePreview(r.Context(), r.PathValue("id"))
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := WriteJSON(w, http.StatusOK, deletePreviewDTO(plan)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{Status: http.StatusOK, Description: "The plan.", Body: DeletePreviewDTO{}},
		Errors: []Response{{
			Status: http.StatusNotFound, Description: "No model has this id.",
			Codes: []model.ErrorCode{CodeNotFound},
		}},
	}
}

func (a *API) deleteModelRoute() Route {
	return Route{
		Method:      http.MethodDelete,
		Pattern:     BasePath + "/models/{id}",
		Auth:        AuthSession,
		OperationID: "deleteModel",
		Summary: "Execute the delete preview. The row moves deleting → deleted and is kept; " +
			"no SQL DELETE is ever issued against a model row.",
		Tag: "models",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.models()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			id := r.PathValue("id")
			plan, ref, err := svc.Delete(r.Context(), id)
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := WriteJSON(w, http.StatusAccepted, DeleteModelResponse{
				JobReceiptDTO: jobReceiptDTO(ref, string(model.SubjectModel)),
				Plan:          deletePreviewDTO(plan),
			}); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status:      http.StatusAccepted,
			Description: "The delete was accepted; `plan` is what the job is executing.",
			Body:        DeleteModelResponse{},
		},
		Errors: []Response{
			{Status: http.StatusNotFound, Description: "No model has this id.",
				Codes: []model.ErrorCode{CodeNotFound}},
			{Status: http.StatusConflict,
				Description: "Instances still use this model, or a job already holds it. " +
					"`details.instances` names them; a soft-deleted instance is never one of them.",
				Codes: []model.ErrorCode{model.CodeModelInUse, model.CodeJobInFlight}},
		},
	}
}

func (a *API) verifyModelRoute() Route {
	return Route{
		Method:      http.MethodPost,
		Pattern:     BasePath + "/models/{id}/verify",
		Auth:        AuthSession,
		OperationID: "verifyModel",
		Summary: "Re-stat every file and, when hf.verify_checksums is on, re-hash the ones whose " +
			"blob name is a sha256.",
		Tag:        "models",
		Idempotent: true,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.models()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			ref, err := svc.Verify(r.Context(), r.PathValue("id"))
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := WriteJSON(w, http.StatusAccepted,
				jobReceiptDTO(ref, string(model.SubjectModel))); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{Status: http.StatusAccepted, Description: "The verify job was queued.",
			Body: JobReceiptDTO{}},
		Errors: []Response{
			{Status: http.StatusNotFound, Description: "No model has this id.",
				Codes: []model.ErrorCode{CodeNotFound}},
			{Status: http.StatusConflict, Description: "A job already holds this model.",
				Codes: []model.ErrorCode{model.CodeJobInFlight}},
		},
	}
}

func (a *API) pairMmprojRoute() Route {
	return Route{
		Method:      http.MethodPost,
		Pattern:     BasePath + "/models/{id}/pair-mmproj",
		Auth:        AuthSession,
		OperationID: "pairModelMmproj",
		Summary: "Attach or detach a multimodal projector. Sets mmproj_auto = 0, so no later " +
			"scan overrules the choice.",
		Tag:         "models",
		RequestBody: PairMmprojRequest{},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.models()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			var body PairMmprojRequest
			if err := DecodeJSON(w, r, &body); err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			v, err := svc.PairMmproj(r.Context(), r.PathValue("id"), body.MmprojModelID)
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := WriteJSON(w, http.StatusOK, modelDTO(v)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{Status: http.StatusOK, Description: "The updated model.", Body: ModelDTO{}},
		Errors: []Response{
			{Status: http.StatusNotFound, Description: "No model has this id.",
				Codes: []model.ErrorCode{CodeNotFound}},
			{Status: http.StatusUnprocessableEntity,
				Description: "The named model is not a multimodal projector.",
				Codes:       []model.ErrorCode{model.CodeModelMissing}},
		},
	}
}

func (a *API) listCacheRootsRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/cache/roots",
		Auth:        AuthSession,
		OperationID: "listCacheRoots",
		Summary:     "Every known hub directory, primary first.",
		Tag:         "models",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.models()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			roots, err := svc.Roots(r.Context())
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			items := make([]CacheRootDTO, 0, len(roots))
			for _, root := range roots {
				items = append(items, cacheRootDTO(root))
			}
			if err := WriteJSON(w, http.StatusOK, NewList(items, len(items), nil)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{Status: http.StatusOK, Description: "The cache roots.",
			Body: List[CacheRootDTO]{}},
	}
}

func (a *API) addCacheRootRoute() Route {
	return Route{
		Method:      http.MethodPost,
		Pattern:     BasePath + "/cache/roots",
		Auth:        AuthSession,
		OperationID: "addCacheRoot",
		Summary: "Register an existing hub directory as scan-and-serve. A new root is never " +
			"primary; promote it to make downloads land there.",
		Tag:         "models",
		RequestBody: AddCacheRootRequest{},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.models()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			var body AddCacheRootRequest
			if err := DecodeJSON(w, r, &body); err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			root, scan, err := svc.AddRoot(r.Context(), body.Path)
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := WriteJSON(w, http.StatusCreated, AddCacheRootResponse{
				Root: cacheRootDTO(root),
				Scan: optionalReceipt(scan, string(model.SubjectCacheScan)),
			}); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{Status: http.StatusCreated,
			Description: "The root was registered and a scan of it was queued.",
			Body:        AddCacheRootResponse{}},
		Errors: []Response{{
			Status: http.StatusUnprocessableEntity,
			Description: "The path is not usable as a cache root: it is under a prefix the unit " +
				"mounts read-only, it is not an absolute directory, or it is already registered.",
			Codes: []model.ErrorCode{model.CodeRootPathProtected, model.CodeSettingInvalid},
		}},
	}
}

func (a *API) promoteCacheRootRoute() Route {
	return Route{
		Method:      http.MethodPost,
		Pattern:     BasePath + "/cache/roots/{id}/promote",
		Auth:        AuthSession,
		OperationID: "promoteCacheRoot",
		Summary: "Make this root primary — the single write path shared with " +
			"PATCH /settings {hf.hub_dir}. Nothing is moved or copied.",
		Tag: "models",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.models()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			root, scan, err := svc.PromoteRoot(r.Context(), r.PathValue("id"))
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := WriteJSON(w, http.StatusAccepted, PromoteCacheRootResponse{
				Root:            cacheRootDTO(root),
				Scan:            optionalReceipt(scan, string(model.SubjectCacheScan)),
				RestartRequired: false,
			}); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{Status: http.StatusAccepted,
			Description: "The root is now primary and a scan of it was queued.",
			Body:        PromoteCacheRootResponse{}},
		Errors: []Response{
			{Status: http.StatusNotFound, Description: "No cache root has this id.",
				Codes: []model.ErrorCode{CodeNotFound}},
			{Status: http.StatusUnprocessableEntity,
				Description: "The root is not writable, so it can never receive downloads.",
				Codes:       []model.ErrorCode{model.CodeRootNotWritable, model.CodeRootPathProtected}},
		},
	}
}

func (a *API) detachCacheRootRoute() Route {
	return Route{
		Method:      http.MethodDelete,
		Pattern:     BasePath + "/cache/roots/{id}",
		Auth:        AuthSession,
		OperationID: "detachCacheRoot",
		Summary: "Detach a root: its catalog rows are removed and no file is touched. Refused " +
			"while ANY instance references one of its models, soft-deleted ones included.",
		Tag: "models",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.models()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := svc.DetachRoot(r.Context(), r.PathValue("id")); err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			WriteNoContent(w)
		}),
		Success: Response{Status: http.StatusNoContent, Description: "The root was detached."},
		Errors: []Response{
			{Status: http.StatusNotFound, Description: "No cache root has this id.",
				Codes: []model.ErrorCode{CodeNotFound}},
			{Status: http.StatusConflict,
				Description: "The root is primary, or instances still reference its models. " +
					"`details.instances` marks which of them are soft-deleted; purging those " +
					"instances is the stated remedy.",
				Codes: []model.ErrorCode{model.CodeRootIsPrimary, model.CodeModelInUse}},
		},
	}
}

func (a *API) cacheScanRoute() Route {
	return Route{
		Method:      http.MethodPost,
		Pattern:     BasePath + "/cache/scan",
		Auth:        AuthSession,
		OperationID: "scanCache",
		Summary:     "Walk a cache root and reconcile the catalog against it. Makes no network calls.",
		Tag:         "models",
		RequestBody: ScanRequest{},
		Idempotent:  true,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.models()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			var body ScanRequest
			if r.ContentLength != 0 {
				if err := DecodeJSON(w, r, &body); err != nil {
					WriteError(w, r, a.log, err)
					return
				}
			}
			_, ref, err := svc.RequestScan(r.Context(), body.RootID, model.ScanTriggerManual)
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := WriteJSON(w, http.StatusAccepted,
				jobReceiptDTO(ref, string(model.SubjectCacheScan))); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{Status: http.StatusAccepted, Description: "The scan was queued.",
			Body: JobReceiptDTO{}},
		Errors: []Response{{
			Status: http.StatusNotFound, Description: "No cache root has this id, or none is registered.",
			Codes: []model.ErrorCode{CodeNotFound},
		}},
	}
}

func (a *API) getCacheScanRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/cache/scans/{id}",
		Auth:        AuthSession,
		OperationID: "getCacheScan",
		Summary:     "One scan's progress and results.",
		Tag:         "models",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.models()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			sc, err := svc.Scan(r.Context(), r.PathValue("id"))
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := WriteJSON(w, http.StatusOK, cacheScanDTO(sc)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{Status: http.StatusOK, Description: "The scan.", Body: CacheScanDTO{}},
		Errors: []Response{{
			Status: http.StatusNotFound, Description: "No scan has this id.",
			Codes: []model.ErrorCode{CodeNotFound},
		}},
	}
}

func (a *API) listStraysRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/cache/strays",
		Auth:        AuthSession,
		OperationID: "listStrays",
		Summary:     "Files in a cache root that belong to no model, largest first.",
		Tag:         "models",
		Query:       []QueryParam{{Name: "root_id", Description: "Restrict to one cache root."}},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.models()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			rows, err := svc.Strays(r.Context(), r.URL.Query().Get("root_id"))
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			items := make([]StrayFileDTO, 0, len(rows))
			for _, st := range rows {
				items = append(items, strayDTO(st))
			}
			if err := WriteJSON(w, http.StatusOK, NewList(items, len(items), nil)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{Status: http.StatusOK, Description: "The strays.", Body: List[StrayFileDTO]{}},
	}
}

func (a *API) deleteStrayRoute() Route {
	return Route{
		Method:      http.MethodDelete,
		Pattern:     BasePath + "/cache/strays/{id}",
		Auth:        AuthSession,
		OperationID: "deleteStray",
		Summary:     "Forget a stray, and optionally remove the file it names.",
		Tag:         "models",
		Query: []QueryParam{{
			Name: "delete_file", Type: "boolean",
			Description: "Also unlink the file. Refused for a path outside the cache root that " +
				"reported it.",
		}},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.models()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := svc.DeleteStray(r.Context(), r.PathValue("id"), queryBool(r, "delete_file")); err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			WriteNoContent(w)
		}),
		Success: Response{Status: http.StatusNoContent, Description: "The stray was removed."},
		Errors: []Response{
			{Status: http.StatusNotFound, Description: "No stray has this id.",
				Codes: []model.ErrorCode{CodeNotFound}},
			{Status: http.StatusBadRequest,
				Description: "The file is not inside the cache root that reported it.",
				Codes:       []model.ErrorCode{model.CodeSettingInvalid}},
		},
	}
}

func (a *API) dismissStrayRoute() Route {
	return Route{
		Method:      http.MethodPost,
		Pattern:     BasePath + "/cache/strays/{id}/dismiss",
		Auth:        AuthSession,
		OperationID: "dismissStray",
		Summary:     "Hide a stray from the list without removing anything.",
		Tag:         "models",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.models()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := svc.DismissStray(r.Context(), r.PathValue("id")); err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			WriteNoContent(w)
		}),
		Success: Response{Status: http.StatusNoContent, Description: "The stray was dismissed."},
		Errors: []Response{{
			Status: http.StatusNotFound, Description: "No stray has this id.",
			Codes: []model.ErrorCode{CodeNotFound},
		}},
	}
}

func (a *API) models() (ModelService, error) {
	if a.cfg.Models == nil {
		return nil, Errorf(http.StatusServiceUnavailable, CodeInternalError,
			"this daemon was built without a model service")
	}
	return a.cfg.Models, nil
}
