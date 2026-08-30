package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/jlbyh2o/llamaman/internal/hf/download"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// The download endpoints of DESIGN section 3.8.
//
// `POST /downloads` is the clearest example in this API of section 3's
// long-action rule: it answers `202 {"job_id","subject","model_id"}` — never the
// download itself — because the `jobs`, `downloads`, `models` and `model_files`
// rows are written in ONE transaction (section 2.7), so the job IS the receipt
// and `GET /downloads/{id}` carries the detail. An `Idempotency-Key` replay
// inside its window returns THE SAME BODY with `200` (D65), which is what makes
// a double-clicked Download a replay rather than a `409`.
//
// The two refusals both carry their numbers rather than merely refusing:
// `409 download_exists` names the download already running, and
// `409 insufficient_disk` says how much is free and how much is needed. A guard
// that will not say why is a guard a user cannot get past.

// DownloadService is everything this layer needs from internal/hf/download. The
// consumer owns the interface (DESIGN section 1); *download.Service satisfies it.
type DownloadService interface {
	Create(ctx context.Context, req download.CreateRequest) (download.CreateResult, error)
	Get(ctx context.Context, id string) (download.View, error)
	List(ctx context.Context, f store.DownloadFilter) ([]download.View, error)
	Pause(ctx context.Context, id string) error
	Resume(ctx context.Context, id string) error
	Retry(ctx context.Context, id string) error
	Cancel(ctx context.Context, id string, keepPartial bool) error
	SetPriority(ctx context.Context, id string, priority int) error
}

// DownloadDTO is one row of `GET /api/v1/downloads`.
type DownloadDTO struct {
	ID      string `json:"id"`
	ModelID string `json:"model_id"`
	// RepoID, Revision and PrimaryFile come from the model row, so a list does
	// not need one request per download to say what is being downloaded.
	RepoID      string `json:"repo_id"`
	Revision    string `json:"revision"`
	PrimaryFile string `json:"primary_file"`

	State         string `json:"state"`
	Priority      int    `json:"priority"`
	IncludeMmproj bool   `json:"include_mmproj"`

	BytesTotal int64 `json:"bytes_total"`
	BytesDone  int64 `json:"bytes_done"`
	// BytesAtStart is the offset this run resumed from. It is on the wire
	// because it is what makes `speed_bps` and `eta_sec` checkable: a client can
	// see that 38 of 40 GB were already there and that the reported rate
	// describes the two that moved.
	BytesAtStart int64  `json:"bytes_at_start"`
	SpeedBPS     int64  `json:"speed_bps"`
	ETASec       *int64 `json:"eta_sec"`

	Attempts     int     `json:"attempts"`
	ErrorCode    *string `json:"error_code"`
	ErrorMessage *string `json:"error_message"`

	CreatedAt  string  `json:"created_at"`
	StartedAt  *string `json:"started_at"`
	FinishedAt *string `json:"finished_at"`

	// Files is the per-file progress of section 7.3. A sharded download shows
	// five bars, not one: "47%" of a five-shard model tells a user nothing about
	// which shard is stuck.
	Files []DownloadFileDTO `json:"files"`
}

// DownloadFileDTO is one file's line in a download.
type DownloadFileDTO struct {
	ID          string `json:"id"`
	ModelFileID string `json:"model_file_id"`
	ModelID     string `json:"model_id"`
	Filename    string `json:"filename"`
	ShardIndex  int    `json:"shard_index"`
	ShardTotal  int    `json:"shard_total"`
	State       string `json:"state"`
	BytesTotal  int64  `json:"bytes_total"`
	BytesDone   int64  `json:"bytes_done"`
	// Etag is the BLOB NAME — for an LFS object the sha256 hex. The HTTP
	// validator is deliberately NOT on the wire: it is an internal resume
	// detail, it means nothing to a client, and publishing it would invite one
	// to send it somewhere.
	Etag      *string `json:"etag"`
	Attempts  int     `json:"attempts"`
	LastError *string `json:"last_error"`
}

// CreateDownloadRequest is the body of `POST /api/v1/downloads`.
type CreateDownloadRequest struct {
	RepoID string `json:"repo_id"`
	// Revision may be a branch, a tag or a commit; omitted means `main`. What is
	// STORED is always the resolved commit — section 2.6 forbids a branch name
	// in `models.revision`, because `main` names a different tree next week.
	Revision string `json:"revision,omitempty"`
	// Files are paths inside the repository. Naming any file of a shard set
	// expands to the whole set, and a set the repository holds only part of is
	// refused (section 7.3).
	Files []string `json:"files"`
	// IncludeMmproj defaults to true when omitted, which is why it is a pointer:
	// a plain bool cannot tell "the client said false" from "the client said
	// nothing", and those are opposite answers here.
	IncludeMmproj *bool `json:"include_mmproj,omitempty"`
	// MmprojFile picks between several projectors, for the repository that has
	// more than one and where section 7.2's preference rule declined to guess.
	MmprojFile string `json:"mmproj_file,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Priority   int    `json:"priority,omitempty"`
}

// CreateDownloadResponse is section 3.8's `202`: the job receipt plus the model
// id, so the UI can open the model page before a byte has moved (section 3.10a).
type CreateDownloadResponse struct {
	JobReceiptDTO
	ModelID string `json:"model_id"`
	// MmprojModelID is the projector's own `models` row, null when none was
	// included. It is a separate row because a projector is separately reusable
	// across quantizations (section 7.3).
	MmprojModelID *string `json:"mmproj_model_id"`
	DownloadID    string  `json:"download_id"`
	BytesTotal    int64   `json:"bytes_total"`
}

// PatchDownloadRequest is `PATCH /api/v1/downloads/{id}`: the queue reorder.
type PatchDownloadRequest struct {
	Priority int `json:"priority"`
}

func (a *API) downloadRoutes() []Route {
	return []Route{
		a.listDownloadsRoute(),
		a.createDownloadRoute(),
		a.getDownloadRoute(),
		a.patchDownloadRoute(),
		a.pauseDownloadRoute(),
		a.resumeDownloadRoute(),
		a.retryDownloadRoute(),
		a.cancelDownloadRoute(),
	}
}

func (a *API) listDownloadsRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/downloads",
		Auth:        AuthSession,
		OperationID: "listDownloads",
		Summary:     "Downloads with their per-file progress.",
		Tag:         "downloads",
		Query: []QueryParam{{
			Name: "state", Enum: []string{"active", "all"},
			Description: "`active` is everything unfinished, `paused` included; `all` is the whole " +
				"table, which is the receipt for what has landed on this disk.",
		}},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.downloads()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			f := store.DownloadFilter{ActiveOnly: r.URL.Query().Get("state") != "all"}
			views, err := svc.List(r.Context(), f)
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			items := make([]DownloadDTO, 0, len(views))
			for _, v := range views {
				items = append(items, downloadDTO(v))
			}
			if err := WriteJSON(w, http.StatusOK, NewList(items, len(items), nil)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{Status: http.StatusOK, Description: "The downloads.",
			Body: List[DownloadDTO]{}},
	}
}

func (a *API) createDownloadRoute() Route {
	return Route{
		Method:      http.MethodPost,
		Pattern:     BasePath + "/downloads",
		Auth:        AuthSession,
		OperationID: "createDownload",
		Summary: "Queue a model download. The jobs, downloads, models and model_files rows are " +
			"written in one transaction, so the job is the receipt.",
		Tag:         "downloads",
		Idempotent:  true,
		RequestBody: CreateDownloadRequest{},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.downloads()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			var body CreateDownloadRequest
			if err := DecodeJSON(w, r, &body); err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			includeMmproj := true
			if body.IncludeMmproj != nil {
				includeMmproj = *body.IncludeMmproj
			}

			res, err := svc.Create(r.Context(), download.CreateRequest{
				RepoID:        body.RepoID,
				Revision:      body.Revision,
				Files:         body.Files,
				IncludeMmproj: includeMmproj,
				MmprojFile:    body.MmprojFile,
				Kind:          model.ModelKind(body.Kind),
				Priority:      body.Priority,
				Idempotency:   idempotencyFor(r, body),
			})
			if err != nil {
				a.writeDownloadError(w, r, err)
				return
			}

			// D65: a replay inside the window answers `200` with the same body.
			// A double-clicked Download must read as neither an error nor a
			// second download.
			status := http.StatusAccepted
			if res.Job.Replayed {
				status = http.StatusOK
			}
			out := CreateDownloadResponse{
				JobReceiptDTO: downloadReceipt(res.Job),
				ModelID:       res.ModelID,
				DownloadID:    res.Download.ID,
				BytesTotal:    res.Download.BytesTotal,
			}
			if res.MmprojModelID != "" {
				id := res.MmprojModelID
				out.MmprojModelID = &id
			}
			if err := WriteJSON(w, status, out); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status:      http.StatusAccepted,
			Description: "The download was queued. Watch `job_id`.",
			Body:        CreateDownloadResponse{},
		},
		Errors: []Response{
			{
				Status:      http.StatusOK,
				Description: "An `Idempotency-Key` replay inside its window: the original download.",
				Body:        CreateDownloadResponse{},
			},
			{
				Status: http.StatusConflict,
				Description: "This model is already being downloaded (`details.download_id` names " +
					"it), or the target filesystem cannot hold it (`details` carries the numbers).",
				Codes: []model.ErrorCode{
					model.CodeDownloadExists, download.CodeInsufficientDisk, model.CodeJobInFlight,
				},
			},
			{
				Status: http.StatusForbidden,
				Description: "The repository is gated. `details` carries `repo` and `request_url`; " +
					"access grants are browser-only on the Hub's side, so the UI links out.",
				Codes: []model.ErrorCode{download.CodeHFGated, download.CodeHFPrivate},
			},
			{
				Status: http.StatusUnprocessableEntity,
				Description: "The request names files this repository does not hold at this " +
					"revision, a shard set the repository holds only part of, more than one " +
					"quantization, or a projector choice that is ambiguous.",
				Codes: []model.ErrorCode{
					download.CodeFileNotInRepo, download.CodeShardSetIncomplete,
					download.CodeNoFilesSelected, download.CodeMultipleQuants,
					download.CodeMmprojAmbiguous,
				},
			},
			{
				Status:      http.StatusBadGateway,
				Description: "The Hugging Face Hub could not be reached.",
				Codes:       []model.ErrorCode{download.CodeHFUnreachable},
			},
			{
				Status:      http.StatusServiceUnavailable,
				Description: "This daemon has no primary cache root yet.",
				Codes:       []model.ErrorCode{download.CodeNoCacheRoot},
			},
		},
	}
}

func (a *API) getDownloadRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/downloads/{id}",
		Auth:        AuthSession,
		OperationID: "getDownload",
		Summary:     "One download with its per-file progress.",
		Tag:         "downloads",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.downloads()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			v, err := svc.Get(r.Context(), r.PathValue("id"))
			if err != nil {
				a.writeDownloadError(w, r, err)
				return
			}
			if err := WriteJSON(w, http.StatusOK, downloadDTO(v)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{Status: http.StatusOK, Description: "The download.", Body: DownloadDTO{}},
		Errors: []Response{{
			Status: http.StatusNotFound, Description: "No download has this id.",
			Codes: []model.ErrorCode{CodeNotFound},
		}},
	}
}

func (a *API) patchDownloadRoute() Route {
	return Route{
		Method:      http.MethodPatch,
		Pattern:     BasePath + "/downloads/{id}",
		Auth:        AuthSession,
		OperationID: "reorderDownload",
		Summary: "Change a download's queue priority. Moves the jobs row and the downloads row " +
			"together, because the pool leases on the former.",
		Tag:         "downloads",
		RequestBody: PatchDownloadRequest{},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.downloads()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			var body PatchDownloadRequest
			if err := DecodeJSON(w, r, &body); err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if body.Priority < 0 {
				WriteError(w, r, a.log, BadRequest("priority must not be negative"))
				return
			}
			if err := svc.SetPriority(r.Context(), r.PathValue("id"), body.Priority); err != nil {
				a.writeDownloadError(w, r, err)
				return
			}
			v, err := svc.Get(r.Context(), r.PathValue("id"))
			if err != nil {
				a.writeDownloadError(w, r, err)
				return
			}
			if err := WriteJSON(w, http.StatusOK, downloadDTO(v)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{Status: http.StatusOK, Description: "The reordered download.",
			Body: DownloadDTO{}},
		Errors: []Response{{
			Status: http.StatusNotFound, Description: "No download has this id.",
			Codes: []model.ErrorCode{CodeNotFound},
		}},
	}
}

func (a *API) pauseDownloadRoute() Route {
	return a.downloadActionRoute("pause", "pauseDownload",
		"Pause a download. The job releases its lease, the running transfers unwind, and every "+
			"`.incomplete` file stands where it is for the resume to continue from.",
		func(svc DownloadService, r *http.Request, id string) error {
			return svc.Pause(r.Context(), id)
		})
}

func (a *API) resumeDownloadRoute() Route {
	return a.downloadActionRoute("resume", "resumeDownload",
		"Resume a paused download. It continues from the byte each file reached.",
		func(svc DownloadService, r *http.Request, id string) error {
			return svc.Resume(r.Context(), id)
		})
}

func (a *API) retryDownloadRoute() Route {
	return a.downloadActionRoute("retry", "retryDownload",
		"Run a failed or canceled download again, resuming from whatever is on disk.",
		func(svc DownloadService, r *http.Request, id string) error {
			return svc.Retry(r.Context(), id)
		})
}

// downloadActionRoute is the shared shape of pause, resume and retry: a `202`
// with no body, because each of them starts work the job reports on rather than
// producing a result of its own.
func (a *API) downloadActionRoute(verb, opID, summary string,
	apply func(DownloadService, *http.Request, string) error) Route {

	return Route{
		Method:      http.MethodPost,
		Pattern:     BasePath + "/downloads/{id}/" + verb,
		Auth:        AuthSession,
		OperationID: opID,
		Summary:     summary,
		Tag:         "downloads",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.downloads()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			id := r.PathValue("id")
			if err := apply(svc, r, id); err != nil {
				a.writeDownloadError(w, r, err)
				return
			}
			v, err := svc.Get(r.Context(), id)
			if err != nil {
				a.writeDownloadError(w, r, err)
				return
			}
			if err := WriteJSON(w, http.StatusAccepted, downloadDTO(v)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{Status: http.StatusAccepted, Description: "The download after the change.",
			Body: DownloadDTO{}},
		Errors: []Response{
			{Status: http.StatusNotFound, Description: "No download has this id.",
				Codes: []model.ErrorCode{CodeNotFound}},
			{Status: http.StatusConflict,
				Description: "The download's current state does not admit this change.",
				Codes:       []model.ErrorCode{download.CodeDownloadNotPausable}},
		},
	}
}

func (a *API) cancelDownloadRoute() Route {
	return Route{
		Method:      http.MethodPost,
		Pattern:     BasePath + "/downloads/{id}/cancel",
		Auth:        AuthSession,
		OperationID: "cancelDownload",
		Summary: "Cancel a download. The partial files are kept by default, so a retry resumes " +
			"rather than starting over.",
		Tag: "downloads",
		Query: []QueryParam{{
			Name: "keep_partial", Type: "boolean",
			Description: "Default true. False removes each `.incomplete` file as the transfer " +
				"that owns it releases its handle.",
		}},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.downloads()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			// The default is true and the parameter is read as "false only when
			// the client said so". A cancel that silently discarded forty
			// gigabytes because a query parameter was misspelled would be the
			// worst possible reading of an ambiguous request.
			keep := true
			if v := r.URL.Query().Get("keep_partial"); v != "" {
				parsed, err := strconv.ParseBool(v)
				if err != nil {
					WriteError(w, r, a.log, BadRequest("keep_partial must be a boolean"))
					return
				}
				keep = parsed
			}
			id := r.PathValue("id")
			if err := svc.Cancel(r.Context(), id, keep); err != nil {
				a.writeDownloadError(w, r, err)
				return
			}
			v, err := svc.Get(r.Context(), id)
			if err != nil {
				a.writeDownloadError(w, r, err)
				return
			}
			if err := WriteJSON(w, http.StatusAccepted, downloadDTO(v)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{Status: http.StatusAccepted, Description: "The canceled download.",
			Body: DownloadDTO{}},
		Errors: []Response{
			{Status: http.StatusNotFound, Description: "No download has this id.",
				Codes: []model.ErrorCode{CodeNotFound}},
			{Status: http.StatusConflict, Description: "The download has already finished.",
				Codes: []model.ErrorCode{download.CodeDownloadNotPausable}},
		},
	}
}

// writeDownloadError maps this package's codes onto the statuses section 3.8
// pairs them with.
//
// It is a local mapping rather than an addition to statusForCode because the
// codes are internal/hf/download's own: they are declared beside the code that
// returns them, and the ONE layer that knows what a 403 means is this one. Every
// code it does not recognize falls through to WriteError, which is where a
// `not_found` from the store and every unclassified error already go.
func (a *API) writeDownloadError(w http.ResponseWriter, r *http.Request, err error) {
	var me model.Error
	if errors.As(err, &me) {
		if status, ok := downloadStatus[me.Code]; ok {
			WriteError(w, r, a.log, &Error{
				Status: status, Code: me.Code, Message: me.Message, Details: me.Details, Err: err,
			})
			return
		}
	}
	WriteError(w, r, a.log, err)
}

// downloadStatus is section 3.8's code-to-status table.
//
// `hf_gated` is a 403 and not a 404 deliberately: the repository exists, the
// user can see it in a browser, and telling them it does not exist would send
// them looking for a typo instead of clicking the link this response carries.
var downloadStatus = map[model.ErrorCode]int{
	download.CodeHFGated:             http.StatusForbidden,
	download.CodeHFPrivate:           http.StatusForbidden,
	download.CodeHFUnreachable:       http.StatusBadGateway,
	download.CodeInsufficientDisk:    http.StatusConflict,
	download.CodeShardSetIncomplete:  http.StatusUnprocessableEntity,
	download.CodeFileNotInRepo:       http.StatusUnprocessableEntity,
	download.CodeNoFilesSelected:     http.StatusUnprocessableEntity,
	download.CodeMultipleQuants:      http.StatusUnprocessableEntity,
	download.CodeMmprojAmbiguous:     http.StatusUnprocessableEntity,
	download.CodeDownloadNotPausable: http.StatusConflict,
	download.CodeNoCacheRoot:         http.StatusServiceUnavailable,
	model.CodeDownloadExists:         http.StatusConflict,
}

// downloadReceipt renders the `{job_id, subject}` receipt for a download job.
// It is a second function rather than a reuse of jobReceiptDTO because that one
// takes a models.JobRef, and converting between two identical structs at every
// call site would be worse than one four-line function.
func downloadReceipt(ref download.JobRef) JobReceiptDTO {
	out := JobReceiptDTO{
		Subject: SubjectDTO{Type: string(model.SubjectDownload), ID: ref.SubjectID},
	}
	if ref.JobID != "" {
		id := ref.JobID
		out.JobID = &id
	}
	return out
}

func downloadDTO(v download.View) DownloadDTO {
	d := DownloadDTO{
		ID:            v.ID,
		ModelID:       v.ModelID,
		RepoID:        v.RepoID,
		Revision:      v.Revision,
		PrimaryFile:   v.PrimaryFile,
		State:         string(v.State),
		Priority:      v.Priority,
		IncludeMmproj: v.IncludeMmproj,
		BytesTotal:    v.BytesTotal,
		BytesDone:     v.BytesDone,
		BytesAtStart:  v.BytesAtStart,
		SpeedBPS:      v.SpeedBPS,
		ETASec:        v.ETASec,
		Attempts:      v.Attempts,
		ErrorCode:     v.ErrorCode,
		ErrorMessage:  v.ErrorMessage,
		CreatedAt:     Time(v.CreatedAt),
		StartedAt:     TimePtr(v.StartedAt),
		FinishedAt:    TimePtr(v.FinishedAt),
		Files:         make([]DownloadFileDTO, 0, len(v.Tasks)),
	}
	for _, t := range v.Tasks {
		d.Files = append(d.Files, DownloadFileDTO{
			ID:          t.ID,
			ModelFileID: t.ModelFileID,
			ModelID:     t.ModelID,
			Filename:    t.Filename,
			ShardIndex:  t.ShardIndex,
			ShardTotal:  t.ShardTotal,
			State:       string(t.State),
			BytesTotal:  t.BytesTotal,
			BytesDone:   t.BytesDone,
			Etag:        t.Etag,
			Attempts:    t.Attempts,
			LastError:   t.LastError,
		})
	}
	return d
}

func (a *API) downloads() (DownloadService, error) {
	if a.cfg.Downloads == nil {
		return nil, Errorf(http.StatusServiceUnavailable, CodeInternalError,
			"this daemon was built without a download service")
	}
	return a.cfg.Downloads, nil
}
