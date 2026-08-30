package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/jlbyh2o/llamaman/internal/api/middleware"
	"github.com/jlbyh2o/llamaman/internal/jobs"
	"github.com/jlbyh2o/llamaman/internal/llamacpp"
	"github.com/jlbyh2o/llamaman/internal/model"
)

// The llama.cpp lifecycle endpoints of DESIGN section 3.5 — all twelve rows of
// that table: the active build, the version list and one version, the install
// POST with all five of D71's branches behind it, cancel, retry, activate,
// delete, rollback, the build log, the release listing and the acquisition plan.
//
// Two of them are named mechanisms elsewhere in the design rather than
// conveniences, which is why they are not the last to arrive:
//
//   - `GET /llamacpp/plan` is the whole subject of section 6.3 — "the difference
//     between 'build failed after four minutes' and 'install cmake first'".
//   - `POST …/{id}/retry` is the operation section 2.5's reuse-and-reset row and
//     D4 both name as the way an `interrupted` build resumes against warm
//     objects.

// LlamacppService is everything this layer needs from internal/llamacpp. The
// consumer owns the interface (DESIGN section 1); *llamacpp.Service satisfies it.
type LlamacppService interface {
	List(ctx context.Context) ([]llamacpp.View, error)
	Get(ctx context.Context, id string) (llamacpp.View, error)
	Active(ctx context.Context) (llamacpp.View, error)
	Install(ctx context.Context, req llamacpp.InstallRequest) (llamacpp.InstallResult, error)
	Cancel(ctx context.Context, id string) (model.Job, error)
	Retry(ctx context.Context, id string) (model.Job, error)
	Activate(ctx context.Context, id string, req llamacpp.ActivateRequest) (model.Job, error)
	Rollback(ctx context.Context, req llamacpp.ActivateRequest) (model.Job, error)
	Delete(ctx context.Context, id string) (model.Job, error)

	// Log is one page of the durable build log; FollowLog and LogTail are the
	// live half — the in-memory ring section 6.5 keeps and the broadcast the SSE
	// tail reads.
	Log(ctx context.Context, id string, offset, limit int64) (llamacpp.LogChunk, error)
	FollowLog(id string) (lines <-chan string, stop func(), ok bool)
	LogTail(id string, n int) []string

	Releases(ctx context.Context, channel model.LlamacppChannel) (llamacpp.ReleaseList, error)
	PlanInstall(ctx context.Context, req llamacpp.PlanRequest) (llamacpp.Plan, error)
}

// LlamacppVersionDTO is one row of `GET /api/v1/llamacpp/versions` (section 3.5).
//
// The three identity components are on the wire beside the id because the UI
// never shows the raw id when it can help it: it renders "b10621 · CUDA ·
// source" from `tag`, `backend` and `acquisition` (section 6.2).
type LlamacppVersionDTO struct {
	ID          string  `json:"id"`
	Channel     string  `json:"channel"`
	Tag         string  `json:"tag"`
	BuildTag    *string `json:"build_tag"`
	Backend     string  `json:"backend"`
	Acquisition string  `json:"acquisition"`

	GitURL         string  `json:"git_url"`
	GitRef         *string `json:"git_ref"`
	ResolvedCommit *string `json:"resolved_commit"`

	State          string `json:"state"`
	IsActive       bool   `json:"is_active"`
	PreviousActive bool   `json:"previous_active"`
	// SupersededBy links a `failed_verification` prebuilt to the source build
	// D18 enqueued in its place, which is what lets the UI say "prebuilt
	// rejected — built from source instead" in one line.
	SupersededBy *string `json:"superseded_by"`

	SizeBytes   *int64 `json:"size_bytes"`
	SupportsFit bool   `json:"supports_fit"`
	// InUseBy names the instances whose live process is executing out of this
	// version's directory (D25). A non-empty list is why a delete is refused.
	InUseBy []string `json:"in_use_by"`

	ErrorCode    *string `json:"error_code"`
	ErrorMessage *string `json:"error_message"`
	FailingStep  *string `json:"failing_step"`

	// The four timestamps are RFC 3339 UTC strings, like every other timestamp
	// on this API (section 3's conventions). The storage form is Unix
	// milliseconds and the conversion lives in this layer and nowhere else —
	// dto.go's Time and TimePtr are it.
	CreatedAt   string  `json:"created_at"`
	StartedAt   *string `json:"started_at"`
	FinishedAt  *string `json:"finished_at"`
	ActivatedAt *string `json:"activated_at"`
}

// LlamacppVersionDetailDTO is `GET /api/v1/llamacpp/versions/{id}`: the row plus
// the three columns a list does not carry because they are large.
type LlamacppVersionDetailDTO struct {
	Version LlamacppVersionDTO `json:"version"`
	// BuildOptions is `build_options_json` verbatim — the flags this build was
	// configured with, and what D71 compares a re-post against.
	BuildOptions *string `json:"build_options"`
	// Binaries is `binaries_json`: the tool names found under bin/.
	Binaries *string `json:"binaries"`
	// DevicesOutput is `llama-server --list-devices`, verbatim (D19).
	DevicesOutput *string `json:"devices_output"`
	CUDAArchList  *string `json:"cuda_arch_list"`
	HostCPUFlags  *string `json:"host_cpu_flags"`
	LogPath       *string `json:"log_path"`
}

// InstallLlamacppRequest is the body of `POST /api/v1/llamacpp/versions`.
type InstallLlamacppRequest struct {
	Channel string `json:"channel"`
	// Tag pins a release. Ignored on the stable channel, where "stable" means
	// whatever `releases/latest` says.
	Tag    string `json:"tag,omitempty"`
	GitURL string `json:"git_url,omitempty"`
	GitRef string `json:"git_ref,omitempty"`
	// Backend is cpu or cuda; empty means cpu.
	Backend string `json:"backend,omitempty"`
	// ForceSource takes section 6.3's "otherwise" branch whatever the asset
	// lookup would have said.
	ForceSource bool `json:"force_source,omitempty"`
	// CMakeExtra is appended after `settings.llamacpp.extra_cmake_flags`.
	CMakeExtra []string `json:"cmake_extra,omitempty"`
	// ForceRebuild is D71's override for a `ready` id: reuse-and-reset the row
	// and build it again, refused only when a live process is executing out of
	// its directory.
	ForceRebuild bool `json:"force_rebuild,omitempty"`
}

// InstallLlamacppResponse is what that POST answers with: `202` for a build that
// was queued, `200` for a reuse or an Idempotency-Key replay.
//
// It embeds section 3's long-action receipt — `{"job_id","subject"}` — for the
// same reason `POST /downloads` does: the subject is what the SSE stream is
// keyed on, and a client given only a job id has to guess which domain row the
// frames belong to. `job_id` is null on D71's reuse branch, where the row
// already exists and nothing was queued.
type InstallLlamacppResponse struct {
	JobReceiptDTO
	Version LlamacppVersionDTO `json:"version"`
	// Reused is D71's third branch: already installed, nothing rebuilt.
	Reused bool `json:"reused"`
}

// ActivateLlamacppRequest is the body of the activate and rollback POSTs.
type ActivateLlamacppRequest struct {
	// RestartInstances is "none" or "rolling" (section 6.6 step 5).
	RestartInstances string `json:"restart_instances,omitempty"`
	// CanaryInstanceID is the instance the roll gates on. Empty picks the first
	// running instance in creation order.
	CanaryInstanceID string `json:"canary_instance_id,omitempty"`
}

// llamacppReceipt renders section 3's long-action shape for a job this package
// created: `{"job_id":"…","subject":{…}}`.
//
// The subject comes off the job row itself (`jobs.subject_type` /
// `jobs.subject_id`, section 2.3a) rather than being reconstructed from the
// route's path parameter. Those two are the same string today, and the row is
// the one that stays right: a rollback's subject is the version it ACTIVATES,
// which the request never named.
func llamacppReceipt(j model.Job) JobReceiptDTO {
	out := JobReceiptDTO{
		Subject: SubjectDTO{Type: string(j.SubjectType), ID: j.SubjectID},
	}
	if j.ID != "" {
		id := j.ID
		out.JobID = &id
	}
	return out
}

func (a *API) llamacppRoutes() []Route {
	return []Route{
		a.activeLlamacppRoute(),
		a.listLlamacppVersionsRoute(),
		a.getLlamacppVersionRoute(),
		a.installLlamacppVersionRoute(),
		a.cancelLlamacppVersionRoute(),
		a.retryLlamacppVersionRoute(),
		a.llamacppVersionLogRoute(),
		a.activateLlamacppVersionRoute(),
		a.rollbackLlamacppRoute(),
		a.deleteLlamacppVersionRoute(),
		a.llamacppReleasesRoute(),
		a.llamacppPlanRoute(),
	}
}

func (a *API) activeLlamacppRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/llamacpp/active",
		Auth:        AuthSession,
		OperationID: "getActiveLlamacpp",
		Summary:     "The active build, with its build options and the devices it can see.",
		Tag:         "llamacpp",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.llamacpp()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			v, err := svc.Active(r.Context())
			if err != nil {
				a.writeLlamacppError(w, r, err)
				return
			}
			if err := WriteJSON(w, http.StatusOK, llamacppDetailDTO(v)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status:      http.StatusOK,
			Description: "The active build.",
			Body:        LlamacppVersionDetailDTO{},
		},
		Errors: []Response{{
			Status: http.StatusNotFound,
			Description: "No build is active. This is the ordinary state on a fresh install, " +
				"not an error condition of this endpoint.",
			Codes: []model.ErrorCode{CodeNotFound},
		}},
	}
}

func (a *API) listLlamacppVersionsRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/llamacpp/versions",
		Auth:        AuthSession,
		OperationID: "listLlamacppVersions",
		Summary: "Every llama.cpp build this host has, with its state, size, dates, " +
			"`is_active`, `previous_active` and `in_use_by`.",
		Tag: "llamacpp",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.llamacpp()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			views, err := svc.List(r.Context())
			if err != nil {
				a.writeLlamacppError(w, r, err)
				return
			}
			items := make([]LlamacppVersionDTO, 0, len(views))
			for _, v := range views {
				items = append(items, llamacppVersionDTO(v))
			}
			if err := WriteJSON(w, http.StatusOK, NewList(items, len(items), nil)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status:      http.StatusOK,
			Description: "Every version row, newest first.",
			Body:        List[LlamacppVersionDTO]{},
		},
	}
}

func (a *API) getLlamacppVersionRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/llamacpp/versions/{id}",
		Auth:        AuthSession,
		OperationID: "getLlamacppVersion",
		Summary:     "One version, including its build options, failing step and device list.",
		Tag:         "llamacpp",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.llamacpp()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			v, err := svc.Get(r.Context(), r.PathValue("id"))
			if err != nil {
				a.writeLlamacppError(w, r, err)
				return
			}
			if err := WriteJSON(w, http.StatusOK, llamacppDetailDTO(v)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status:      http.StatusOK,
			Description: "The version.",
			Body:        LlamacppVersionDetailDTO{},
		},
		Errors: []Response{{
			Status:      http.StatusNotFound,
			Description: "No version has this id.",
			Codes:       []model.ErrorCode{CodeNotFound},
		}},
	}
}

func (a *API) installLlamacppVersionRoute() Route {
	return Route{
		Method:      http.MethodPost,
		Pattern:     BasePath + "/llamacpp/versions",
		Auth:        AuthSession,
		OperationID: "installLlamacppVersion",
		Summary: "Install a llama.cpp build. The request resolves to a three-part id before " +
			"anything is inserted, and then takes exactly one of D71's five branches.",
		Tag:         "llamacpp",
		Idempotent:  true,
		RequestBody: InstallLlamacppRequest{},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.llamacpp()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			var body InstallLlamacppRequest
			if err := DecodeJSON(w, r, &body); err != nil {
				WriteError(w, r, a.log, err)
				return
			}

			res, err := svc.Install(r.Context(), llamacpp.InstallRequest{
				Channel:      model.LlamacppChannel(body.Channel),
				Tag:          body.Tag,
				GitURL:       body.GitURL,
				GitRef:       body.GitRef,
				Backend:      model.Backend(body.Backend),
				ForceSource:  body.ForceSource,
				CMakeExtra:   body.CMakeExtra,
				ForceRebuild: body.ForceRebuild,
				Idempotency:  idempotencyFor(r, body),
			})
			if err != nil {
				a.writeLlamacppError(w, r, err)
				return
			}

			// 200 for a reuse or a replay, 202 for work that was queued. The
			// distinction is the whole point of D71's third branch and D65's
			// window: a double-clicked Build must not read as an error, and it
			// must not read as a second build either.
			status := http.StatusAccepted
			if res.Reused || res.Replayed {
				status = http.StatusOK
			}
			if err := WriteJSON(w, status, InstallLlamacppResponse{
				JobReceiptDTO: llamacppReceipt(res.Job),
				Version:       llamacppVersionDTO(llamacpp.View{Version: res.Version}),
				Reused:        res.Reused,
			}); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status:      http.StatusAccepted,
			Description: "The build was queued. Watch `job_id`.",
			Body:        InstallLlamacppResponse{},
		},
		Errors: []Response{
			{
				Status: http.StatusOK,
				Description: "Already installed with these options (`reused`), or an " +
					"`Idempotency-Key` replay inside its window.",
				Body: InstallLlamacppResponse{},
			},
			{
				Status: http.StatusConflict,
				Description: "This id is already being built, or it is installed with " +
					"different build options, or a live process is executing out of the " +
					"directory a forced rebuild would replace.",
				Codes: []model.ErrorCode{
					llamacpp.CodeBuildInFlight,
					llamacpp.CodeVersionOptionsDiffer,
					llamacpp.CodeVersionInUse,
				},
			},
			{
				Status: http.StatusUnprocessableEntity,
				Description: "The request could not be resolved to a version: an unknown " +
					"channel or backend, a custom build with no usable git ref, or a channel " +
					"lookup that failed.",
				Codes: []model.ErrorCode{
					model.CodeBadFlags,
					llamacpp.CodeResolveFailed,
					model.CodeIdempotencyKeyReused,
				},
			},
		},
	}
}

func (a *API) activateLlamacppVersionRoute() Route {
	return Route{
		Method:      http.MethodPost,
		Pattern:     BasePath + "/llamacpp/versions/{id}/activate",
		Auth:        AuthSession,
		OperationID: "activateLlamacppVersion",
		Summary: "Make this build active: flip `is_active`, recompute every instance's " +
			"`config_hash`, move the symlink, and optionally run the canary-gated " +
			"rolling restart.",
		Tag:         "llamacpp",
		Idempotent:  true,
		RequestBody: ActivateLlamacppRequest{},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.llamacpp()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			var body ActivateLlamacppRequest
			if err := DecodeJSON(w, r, &body); err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			job, err := svc.Activate(r.Context(), r.PathValue("id"), llamacpp.ActivateRequest{
				RestartInstances: body.RestartInstances,
				CanaryInstanceID: body.CanaryInstanceID,
				Idempotency:      idempotencyFor(r, body),
			})
			if err != nil {
				a.writeLlamacppError(w, r, err)
				return
			}
			if err := WriteJSON(w, http.StatusAccepted, llamacppReceipt(job)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status:      http.StatusAccepted,
			Description: "The activation was queued. Watch `job_id` for the canary roll.",
			Body:        JobReceiptDTO{},
		},
		Errors: []Response{
			{
				Status:      http.StatusNotFound,
				Description: "No version has this id.",
				Codes:       []model.ErrorCode{CodeNotFound},
			},
			{
				Status: http.StatusConflict,
				Description: "The version is not ready, another activation is already " +
					"running, or a benchmark is live (section 6.6 step 1).",
				Codes: []model.ErrorCode{
					llamacpp.CodeVersionNotReady,
					llamacpp.CodeActivationInFlight,
					llamacpp.CodeBenchInFlight,
					model.CodeJobInFlight,
				},
			},
			{
				Status:      http.StatusUnprocessableEntity,
				Description: "`restart_instances` is neither \"none\" nor \"rolling\".",
				Codes:       []model.ErrorCode{model.CodeBadFlags},
			},
		},
	}
}

func (a *API) rollbackLlamacppRoute() Route {
	return Route{
		Method:      http.MethodPost,
		Pattern:     BasePath + "/llamacpp/rollback",
		Auth:        AuthSession,
		OperationID: "rollbackLlamacpp",
		Summary: "Activate the retained previous build. This is the activation routine " +
			"with `previous_active` as the target, revert path included.",
		Tag:         "llamacpp",
		Idempotent:  true,
		RequestBody: ActivateLlamacppRequest{},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.llamacpp()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			var body ActivateLlamacppRequest
			if err := DecodeJSON(w, r, &body); err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			job, err := svc.Rollback(r.Context(), llamacpp.ActivateRequest{
				RestartInstances: body.RestartInstances,
				CanaryInstanceID: body.CanaryInstanceID,
				Idempotency:      idempotencyFor(r, body),
			})
			if err != nil {
				a.writeLlamacppError(w, r, err)
				return
			}
			if err := WriteJSON(w, http.StatusAccepted, llamacppReceipt(job)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status:      http.StatusAccepted,
			Description: "The rollback was queued.",
			Body:        JobReceiptDTO{},
		},
		Errors: []Response{{
			Status: http.StatusConflict,
			Description: "There is no retained previous build — nothing has been replaced " +
				"yet, or `llamacpp.keep_previous` is off — or the same guards the activate " +
				"endpoint applies refused.",
			Codes: []model.ErrorCode{
				llamacpp.CodeNoRollbackTarget,
				llamacpp.CodeVersionNotReady,
				llamacpp.CodeActivationInFlight,
				llamacpp.CodeBenchInFlight,
				model.CodeJobInFlight,
			},
		}},
	}
}

func (a *API) deleteLlamacppVersionRoute() Route {
	return Route{
		Method:      http.MethodDelete,
		Pattern:     BasePath + "/llamacpp/versions/{id}",
		Auth:        AuthSession,
		OperationID: "deleteLlamacppVersion",
		Summary: "Remove a build's directory. Refused for the active build, for the " +
			"rollback target, and for any directory a live process is executing out of (D25).",
		Tag: "llamacpp",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.llamacpp()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			job, err := svc.Delete(r.Context(), r.PathValue("id"))
			if err != nil {
				a.writeLlamacppError(w, r, err)
				return
			}
			if err := WriteJSON(w, http.StatusAccepted, llamacppReceipt(job)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status:      http.StatusAccepted,
			Description: "The deletion was queued.",
			Body:        JobReceiptDTO{},
		},
		Errors: []Response{
			{
				Status:      http.StatusNotFound,
				Description: "No version has this id.",
				Codes:       []model.ErrorCode{CodeNotFound},
			},
			{
				Status: http.StatusConflict,
				Description: "The version is active, is the retained rollback target, or a " +
					"live process is still executing out of its directory.",
				Codes: []model.ErrorCode{
					llamacpp.CodeVersionActive,
					llamacpp.CodeVersionIsRollbackTarget,
					llamacpp.CodeVersionInUse,
					model.CodeJobInFlight,
				},
			},
		},
	}
}

// llamacpp returns the service, or the 503 a build without one answers with.
func (a *API) llamacpp() (LlamacppService, error) {
	if a.cfg.Llamacpp == nil {
		return nil, Errorf(http.StatusServiceUnavailable, CodeInternalError,
			"this daemon was built without a llama.cpp lifecycle service")
	}
	return a.cfg.Llamacpp, nil
}

// writeLlamacppError maps this domain's codes onto the statuses section 3.5
// pairs them with, then hands the rest to WriteError.
//
// The map lives in internal/llamacpp beside the codes, for the same reason
// internal/api and internal/api/middleware own theirs: the package that decides
// to return a code is the one that knows what it meant by it. A code with no
// entry falls through to WriteError's own mapping, which answers 500 — the
// honest reading of "this layer has not been taught what the domain meant".
func (a *API) writeLlamacppError(w http.ResponseWriter, r *http.Request, err error) {
	var me model.Error
	if errors.As(err, &me) {
		if status, ok := llamacpp.Statuses()[me.Code]; ok {
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

// llamacppVersionDTO projects a row onto the wire.
func llamacppVersionDTO(v llamacpp.View) LlamacppVersionDTO {
	row := v.Version
	dto := LlamacppVersionDTO{
		ID:          row.ID,
		Channel:     string(row.Channel),
		Tag:         row.Tag,
		BuildTag:    row.BuildTag,
		Backend:     string(row.Backend),
		Acquisition: string(row.Acquisition),
		// Redacted on the way out: a row written before the git-URL validation
		// landed, or one edited into the database by hand, must not publish a
		// credential through this field (DESIGN sections 2.2 and 7.1).
		GitURL:         llamacpp.RedactGitURL(row.GitURL),
		GitRef:         row.GitRef,
		ResolvedCommit: row.ResolvedCommit,
		State:          string(row.State),
		IsActive:       row.IsActive,
		PreviousActive: row.PreviousActive,
		SupersededBy:   row.SupersededBy,
		SizeBytes:      row.SizeBytes,
		SupportsFit:    row.SupportsFit,
		InUseBy:        v.InUseBy,
		ErrorCode:      row.ErrorCode,
		ErrorMessage:   row.ErrorMessage,
		CreatedAt:      Time(row.CreatedAt),
		StartedAt:      TimePtr(row.StartedAt),
		FinishedAt:     TimePtr(row.FinishedAt),
		ActivatedAt:    TimePtr(row.ActivatedAt),
	}
	if dto.InUseBy == nil {
		dto.InUseBy = []string{}
	}
	if row.FailingStep != nil {
		step := string(*row.FailingStep)
		dto.FailingStep = &step
	}
	return dto
}

func llamacppDetailDTO(v llamacpp.View) LlamacppVersionDetailDTO {
	return LlamacppVersionDetailDTO{
		Version:       llamacppVersionDTO(v),
		BuildOptions:  v.Version.BuildOptionsJSON,
		Binaries:      v.Version.BinariesJSON,
		DevicesOutput: v.Version.DevicesOutput,
		CUDAArchList:  v.Version.CUDAArchList,
		HostCPUFlags:  v.Version.HostCPUFlags,
		LogPath:       v.Version.LogPath,
	}
}

// idempotencyFor builds D65's three inputs from a request that carries an
// `Idempotency-Key`, or nil when it does not. The middleware has already
// validated the header's shape and put it on the context; the route pattern
// comes from the same place, so a key replayed on a DIFFERENT route is the 422
// section 2.3 says it is rather than a silent replay.
//
// The fingerprint is taken over the DECODED body re-marshaled, not over the raw
// bytes, which is what "canonicalized request body" has to mean for a JSON API:
// two requests that differ only in whitespace or key order are the same request,
// and a client that reformatted its body between retries must not get a 422.
func idempotencyFor(r *http.Request, body any) *jobs.Idempotency {
	key, ok := middleware.IdempotencyKeyFrom(r.Context())
	if !ok || key == "" {
		return nil
	}
	route, _ := middleware.RouteFrom(r.Context())
	b, err := json.Marshal(body)
	if err != nil {
		b = nil
	}
	sum := sha256.Sum256(b)
	return &jobs.Idempotency{
		Key:                key,
		Route:              r.Method + " " + route,
		RequestFingerprint: hex.EncodeToString(sum[:]),
	}
}

// -----------------------------------------------------------------------------
// Cancel, retry and the build log
// -----------------------------------------------------------------------------

func (a *API) cancelLlamacppVersionRoute() Route {
	return Route{
		Method:      http.MethodPost,
		Pattern:     BasePath + "/llamacpp/versions/{id}/cancel",
		Auth:        AuthSession,
		OperationID: "cancelLlamacppVersion",
		Summary: "Stop a build that is running. Section 2.5's cancel edge: the process " +
			"group is signaled and the partial directories are removed by the worker.",
		Tag: "llamacpp",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.llamacpp()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			job, err := svc.Cancel(r.Context(), r.PathValue("id"))
			if err != nil {
				a.writeLlamacppError(w, r, err)
				return
			}
			if err := WriteJSON(w, http.StatusAccepted, llamacppReceipt(job)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status: http.StatusAccepted,
			Description: "The cancel was requested. The build stops at its next checkpoint " +
				"and the job reports the outcome.",
			Body: JobReceiptDTO{},
		},
		Errors: []Response{
			{
				Status:      http.StatusNotFound,
				Description: "No version has this id.",
				Codes:       []model.ErrorCode{CodeNotFound},
			},
			{
				Status:      http.StatusConflict,
				Description: "No job for this version is running, so there is nothing to cancel.",
				Codes:       []model.ErrorCode{llamacpp.CodeBuildNotCancelable},
			},
		},
	}
}

func (a *API) retryLlamacppVersionRoute() Route {
	return Route{
		Method:      http.MethodPost,
		Pattern:     BasePath + "/llamacpp/versions/{id}/retry",
		Auth:        AuthSession,
		OperationID: "retryLlamacppVersion",
		Summary: "Run a stopped build again. An `interrupted` build resumes against the warm " +
			"worktree and cmake cache (D4); a failed or canceled one takes section 2.5's " +
			"reuse-and-reset.",
		Tag: "llamacpp",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.llamacpp()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			job, err := svc.Retry(r.Context(), r.PathValue("id"))
			if err != nil {
				a.writeLlamacppError(w, r, err)
				return
			}
			if err := WriteJSON(w, http.StatusAccepted, llamacppReceipt(job)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status:      http.StatusAccepted,
			Description: "The build was queued again. Watch `job_id`.",
			Body:        JobReceiptDTO{},
		},
		Errors: []Response{
			{
				Status:      http.StatusNotFound,
				Description: "No version has this id.",
				Codes:       []model.ErrorCode{CodeNotFound},
			},
			{
				Status: http.StatusConflict,
				Description: "This version's last install is not in one of the three states a " +
					"retry acts on (`failed`, `canceled`, `interrupted`).",
				Codes: []model.ErrorCode{llamacpp.CodeBuildNotRetryable},
			},
		},
	}
}

// LogPageDTO is the JSON envelope `GET …/log` answers with when the client asks
// for JSON rather than plain text.
//
// The offsets are BYTE offsets into the log file, which is what makes paging a
// growing file a subtraction rather than a line count that shifts under the
// reader.
type LogPageDTO struct {
	VersionID  string `json:"version_id"`
	Text       string `json:"text"`
	Offset     int64  `json:"offset"`
	NextOffset int64  `json:"next_offset"`
	Size       int64  `json:"size"`
	// Live reports that a build is writing this log now, so a client that
	// reached the end should follow it with `Accept: text/event-stream` rather
	// than poll.
	Live bool `json:"live"`
}

func (a *API) llamacppVersionLogRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/llamacpp/versions/{id}/log",
		Auth:        AuthSession,
		OperationID: "getLlamacppVersionLog",
		Summary: "The build log: `?offset=&limit=` plain text by default, the JSON envelope " +
			"with `Accept: application/json`, and a live SSE tail with " +
			"`Accept: text/event-stream`.",
		Tag: "llamacpp",
		Query: []QueryParam{
			{
				Name: "offset",
				Description: "Byte offset to read from. The previous page's `next_offset`, " +
					"or 0 for the beginning.",
				Type: "integer",
			},
			{
				Name:        "limit",
				Description: "Maximum bytes to return. Default 262144, maximum 4194304.",
				Type:        "integer",
			},
			{
				Name: "tail",
				Description: "On the SSE stream, how many buffered lines to replay before " +
					"following. Default 200, capped by the 5000-line ring section 6.5 keeps.",
				Type: "integer",
			},
		},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.llamacpp()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			id := r.PathValue("id")
			if wantsEventStream(r) {
				a.streamLlamacppLog(w, r, svc, id)
				return
			}

			offset := queryInt64(r, "offset", 0)
			limit := queryInt64(r, "limit", 0)
			chunk, err := svc.Log(r.Context(), id, offset, limit)
			if err != nil {
				a.writeLlamacppError(w, r, err)
				return
			}
			if wantsJSON(r) {
				if err := WriteJSON(w, http.StatusOK, LogPageDTO{
					VersionID: id, Text: chunk.Text, Offset: chunk.Offset,
					NextOffset: chunk.NextOffset, Size: chunk.Size, Live: chunk.Live,
				}); err != nil {
					WriteError(w, r, a.log, err)
				}
				return
			}

			h := w.Header()
			h.Set("Content-Type", "text/plain; charset=utf-8")
			h.Set("Cache-Control", "no-store")
			// The paging cursor travels in headers for the plain-text form, so a
			// `curl | less` reader gets the bytes and a script still gets the
			// offsets without asking for a second representation.
			h.Set("X-Log-Offset", strconv.FormatInt(chunk.Offset, 10))
			h.Set("X-Log-Next-Offset", strconv.FormatInt(chunk.NextOffset, 10))
			h.Set("X-Log-Size", strconv.FormatInt(chunk.Size, 10))
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, chunk.Text)
		}),
		Success: Response{
			Status: http.StatusOK,
			Description: "A page of the build log as plain text, the same page as JSON when " +
				"`Accept: application/json`, or a live `event: line` stream when " +
				"`Accept: text/event-stream`.",
			Body:          LogPageDTO{},
			MediaType:     "application/json",
			AltMediaTypes: []string{"text/plain", "text/event-stream"},
		},
		Errors: []Response{{
			Status:      http.StatusNotFound,
			Description: "No version has this id.",
			Codes:       []model.ErrorCode{CodeNotFound},
		}},
	}
}

// streamLlamacppLog serves the live tail.
//
// A build that is NOT running gets the buffered tail and an immediate close
// rather than a connection held open forever: there is nothing more to send, and
// an EventSource that never ends is an EventSource a browser keeps alive across
// a page the user has left.
func (a *API) streamLlamacppLog(w http.ResponseWriter, r *http.Request, svc LlamacppService, id string) {
	if _, err := svc.Log(r.Context(), id, 0, 1); err != nil {
		a.writeLlamacppError(w, r, err)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	rc := http.NewResponseController(w)
	send := func(line string) bool {
		// One `data:` per line, and a line that somehow contains a newline is
		// split across several — which is what the SSE framing requires and the
		// one way a build log can break a naive writer.
		for _, part := range strings.Split(line, "\n") {
			if _, err := fmt.Fprintf(w, "data: %s\n", part); err != nil {
				return false
			}
		}
		if _, err := io.WriteString(w, "\n"); err != nil {
			return false
		}
		return rc.Flush() == nil
	}

	tail := int(queryInt64(r, "tail", 200))
	for _, line := range svc.LogTail(id, tail) {
		if !send(line) {
			return
		}
	}

	lines, stop, live := svc.FollowLog(id)
	if !live {
		// Not running. Say so and close, rather than holding the connection.
		_, _ = io.WriteString(w, "event: end\ndata: {\"live\":false}\n\n")
		_ = rc.Flush()
		return
	}
	defer stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case line, ok := <-lines:
			if !ok {
				_, _ = io.WriteString(w, "event: end\ndata: {\"live\":false}\n\n")
				_ = rc.Flush()
				return
			}
			if !send(line) {
				return
			}
		}
	}
}

// -----------------------------------------------------------------------------
// The release listing and the acquisition plan
// -----------------------------------------------------------------------------

// ReleaseDTO is one row of `GET /api/v1/llamacpp/releases`.
type ReleaseDTO struct {
	Tag        string `json:"tag"`
	Name       string `json:"name"`
	Prerelease bool   `json:"prerelease"`
	// PublishedAt is an RFC 3339 UTC string like every other timestamp here.
	PublishedAt string `json:"published_at"`
	// BodyHTML is the changelog RENDERED AND SANITIZED SERVER-SIDE (D35), and
	// BodyMarkdown is the source behind the "view source" toggle.
	BodyHTML     string `json:"body_html"`
	BodyMarkdown string `json:"body_markdown"`
	// NightlyTag is the `b#####` a stable release pins through its
	// nightly-tag.txt asset — what is actually fetched or built (section 6.2).
	NightlyTag string `json:"nightly_tag"`
	// AssetName and AssetSize describe the prebuilt for THIS host's
	// architecture; null when the release publishes none, which is the fact
	// that sends section 6.3's decision to a source build.
	AssetName *string `json:"asset_name"`
	AssetSize *int64  `json:"asset_size"`
	Installed bool    `json:"installed"`
}

// RateLimitDTO is api.github.com's budget as of the last call (section 6.2).
type RateLimitDTO struct {
	Remaining int    `json:"remaining"`
	Limit     int    `json:"limit"`
	ResetAt   string `json:"reset_at"`
	// Authenticated says whether a GitHub token was used. It is the difference
	// between 60 requests an hour and 5000, and it is what makes "why is the
	// nightly list stale" answerable on screen.
	Authenticated bool `json:"authenticated"`
	// Known is false until a request has actually been made.
	Known bool `json:"known"`
}

// ReleaseListDTO is `GET /api/v1/llamacpp/releases`.
type ReleaseListDTO struct {
	Channel  string       `json:"channel"`
	Releases []ReleaseDTO `json:"releases"`
	// Stale reports a body served from the cache because the network or the
	// rate limit would not produce a fresh one.
	Stale     bool         `json:"stale"`
	FetchedAt *string      `json:"fetched_at"`
	RateLimit RateLimitDTO `json:"rate_limit"`
}

func (a *API) llamacppReleasesRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/llamacpp/releases",
		Auth:        AuthSession,
		OperationID: "listLlamacppReleases",
		Summary: "The releases of a channel, with the changelog rendered and sanitized " +
			"server-side (D35), the resolved `nightly_tag`, and the api.github.com " +
			"rate-limit headroom.",
		Tag: "llamacpp",
		Query: []QueryParam{{
			Name:        "channel",
			Description: "Which channel to list. `custom` has no listing: it is a git URL and a ref.",
			Enum:        []string{string(model.ChannelStable), string(model.ChannelNightly)},
		}},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.llamacpp()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			list, err := svc.Releases(r.Context(),
				model.LlamacppChannel(r.URL.Query().Get("channel")))
			if err != nil {
				a.writeLlamacppError(w, r, err)
				return
			}
			if err := WriteJSON(w, http.StatusOK, releaseListDTO(list)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status:      http.StatusOK,
			Description: "The channel's releases, newest first.",
			Body:        ReleaseListDTO{},
		},
		Errors: []Response{
			{
				Status: http.StatusUnprocessableEntity,
				Description: "`?channel=` is not `stable` or `nightly`. The custom channel " +
					"resolves through `git ls-remote` and has no listing.",
				Codes: []model.ErrorCode{model.CodeBadFlags},
			},
			{
				Status: http.StatusConflict,
				Description: "The channel could not be listed: GitHub was unreachable and " +
					"nothing usable was cached.",
				Codes: []model.ErrorCode{llamacpp.CodeResolveFailed},
			},
		},
	}
}

// PlanDTO is `GET /api/v1/llamacpp/plan` (section 6.3).
type PlanDTO struct {
	VersionID   string `json:"version_id"`
	Acquisition string `json:"acquisition"`
	Backend     string `json:"backend"`
	Channel     string `json:"channel"`
	Tag         string `json:"tag"`
	BuildTag    string `json:"build_tag"`
	// Reason is section 6.3's sentence: why this branch and not the other.
	Reason    string  `json:"reason"`
	AssetName *string `json:"asset_name"`

	EstimatedMinutes int      `json:"estimated_minutes"`
	MissingTools     []string `json:"missing_tools"`
	CUDAArch         []string `json:"cuda_arch"`
	FreeSpaceOK      bool     `json:"free_space_ok"`
	// FreeSpaceKnown reports whether the statfs behind FreeBytes ran at all. A
	// failed probe reports `free_space_known: false`, and the UI must render
	// "unknown" rather than the zero in FreeBytes — "Free space 0 B of 3 GiB
	// needed" for a disk with 360 GiB free tells the user to fix something that
	// is not broken and disables the button that finishes setup.
	FreeSpaceKnown bool  `json:"free_space_known"`
	FreeBytes      int64 `json:"free_bytes"`
	RequiredBytes  int64 `json:"required_bytes"`
	// CanProceed folds the checks above, so the UI can enable or disable the
	// Install button without re-deriving the rule.
	CanProceed bool `json:"can_proceed"`
}

func (a *API) llamacppPlanRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/llamacpp/plan",
		Auth:        AuthSession,
		OperationID: "planLlamacppInstall",
		Summary: "What installing this would do, before committing: the acquisition decision " +
			"with its reason, the detected CUDA architectures, the missing toolchain items " +
			"and a free-space check (section 6.3).",
		Tag: "llamacpp",
		Query: []QueryParam{
			{
				Name:        "channel",
				Description: "stable, nightly or custom. Empty means stable.",
				Enum: []string{
					string(model.ChannelStable), string(model.ChannelNightly),
					string(model.ChannelCustom),
				},
			},
			{Name: "tag", Description: "Pin a release, as the install POST does."},
			{
				Name:        "backend",
				Description: "cpu or cuda. Empty means cpu.",
				Enum:        []string{string(model.BackendCPU), string(model.BackendCUDA)},
			},
			{Name: "git_url", Description: "The remote of a custom build."},
			{Name: "git_ref", Description: "The tag, branch or 40-hex commit of a custom build."},
			{
				Name:        "force_source",
				Description: "Plan section 6.3's \"otherwise\" branch whatever the asset lookup says.",
				Type:        "boolean",
			},
		},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.llamacpp()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			q := r.URL.Query()
			plan, err := svc.PlanInstall(r.Context(), llamacpp.PlanRequest{
				Channel:     model.LlamacppChannel(q.Get("channel")),
				Tag:         q.Get("tag"),
				Backend:     model.Backend(q.Get("backend")),
				GitURL:      q.Get("git_url"),
				GitRef:      q.Get("git_ref"),
				ForceSource: queryBool(r, "force_source"),
			})
			if err != nil {
				a.writeLlamacppError(w, r, err)
				return
			}
			if err := WriteJSON(w, http.StatusOK, planDTO(plan)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status:      http.StatusOK,
			Description: "What would happen, and whether it can.",
			Body:        PlanDTO{},
		},
		Errors: []Response{{
			Status: http.StatusUnprocessableEntity,
			Description: "The request could not be resolved to a version: an unknown channel " +
				"or backend, a git URL this daemon will not clone, or a channel lookup " +
				"that failed.",
			Codes: []model.ErrorCode{model.CodeBadFlags, llamacpp.CodeResolveFailed},
		}},
	}
}

func releaseListDTO(l llamacpp.ReleaseList) ReleaseListDTO {
	out := ReleaseListDTO{
		Channel:  string(l.Channel),
		Releases: make([]ReleaseDTO, 0, len(l.Releases)),
		Stale:    l.Stale,
		RateLimit: RateLimitDTO{
			Remaining:     l.RateLimit.Remaining,
			Limit:         l.RateLimit.Limit,
			Authenticated: l.RateLimit.Authenticated,
			Known:         l.RateLimit.Known,
		},
	}
	if !l.FetchedAt.IsZero() {
		out.FetchedAt = TimePtr(ptrOf(l.FetchedAt.UnixMilli()))
	}
	if !l.RateLimit.ResetAt.IsZero() {
		out.RateLimit.ResetAt = Time(l.RateLimit.ResetAt.UnixMilli())
	}
	for _, r := range l.Releases {
		v := ReleaseDTO{
			Tag: r.Tag, Name: r.Name, Prerelease: r.Prerelease,
			PublishedAt:  Time(r.PublishedAt.UnixMilli()),
			BodyHTML:     r.BodyHTML,
			BodyMarkdown: r.BodyMarkdown,
			NightlyTag:   r.NightlyTag,
			Installed:    r.Installed,
		}
		if r.AssetName != "" {
			name, size := r.AssetName, r.AssetSize
			v.AssetName, v.AssetSize = &name, &size
		}
		out.Releases = append(out.Releases, v)
	}
	return out
}

func planDTO(p llamacpp.Plan) PlanDTO {
	out := PlanDTO{
		VersionID:        p.VersionID,
		Acquisition:      string(p.Acquisition),
		Backend:          string(p.Backend),
		Channel:          string(p.Channel),
		Tag:              p.Tag,
		BuildTag:         p.BuildTag,
		Reason:           p.Reason,
		EstimatedMinutes: p.EstimatedMinutes,
		MissingTools:     p.MissingTools,
		CUDAArch:         p.CUDAArch,
		FreeSpaceOK:      p.FreeSpaceOK,
		FreeSpaceKnown:   p.FreeSpaceKnown,
		FreeBytes:        p.FreeBytes,
		RequiredBytes:    p.RequiredBytes,
		CanProceed:       p.CanProceed,
	}
	if p.AssetName != "" {
		name := p.AssetName
		out.AssetName = &name
	}
	if out.MissingTools == nil {
		out.MissingTools = []string{}
	}
	if out.CUDAArch == nil {
		out.CUDAArch = []string{}
	}
	return out
}
