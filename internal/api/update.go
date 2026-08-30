package api

import (
	"context"
	"net/http"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/selfupdate"
)

// The self-update endpoints of DESIGN section 3.14.
//
// Four rows: the status, the check, the release listing and the apply. The apply
// is the one that carries protocol rather than plumbing — it runs the section
// 12.3 resolver first, then a guard of **exactly four clauses**, all of them
// evaluated inside the one BEGIN IMMEDIATE transaction that inserts the
// `self_updates` row and its job (D97).
//
// The four clauses are enumerated identically in section 12.1 step 1, in section
// 3.14 and in section 15's table-driven fixture, so the three can never drift —
// and the codes below are that enumeration in code:
//
//	409 job_in_flight            a build is running, or any self_update job is live
//	409 selfupdate_unavailable   systemd_control='unavailable'
//	409 selfupdate_unsupported   the swap actor cannot be summoned
//	409 revert_unavailable       there is no working revert
//
// The last two are read off the INSTALLED UNITS' own directives, never off a
// template hash (D95): a host that self-updated across a release which changed a
// unit template is `drift: stale` and must still be able to update.

// The three codes section 12.1's guard adds to section 3's catalog. They are
// re-exported from internal/selfupdate, which is where the guard that returns
// them lives, so the endpoint and the service cannot disagree about their
// spelling.
const (
	CodeSelfUpdateUnavailable = selfupdate.CodeSelfUpdateUnavailable
	CodeSelfUpdateUnsupported = selfupdate.CodeSelfUpdateUnsupported
	CodeRevertUnavailable     = selfupdate.CodeRevertUnavailable
)

// UpdateService is everything this layer needs from internal/selfupdate. The
// consumer owns the interface (DESIGN section 1); *selfupdate.Service satisfies
// it.
type UpdateService interface {
	Status(ctx context.Context) (selfupdate.Status, error)
	Releases(ctx context.Context) ([]selfupdate.Release, error)
	Check(ctx context.Context) error
	Apply(ctx context.Context, req selfupdate.ApplyRequest) (selfupdate.StageResult, error)
}

// UpdateStatusDTO is `GET /api/v1/update/status`.
type UpdateStatusDTO struct {
	CurrentVersion string `json:"current_version"`
	// LatestVersion is the newest release this host knows about, which is not
	// necessarily the newest that exists: the cache is refreshed by
	// `POST /update/check` and by an apply, never on a timer.
	LatestVersion   string `json:"latest_version"`
	UpdateAvailable bool   `json:"update_available"`
	// LastCheckedAt is when the release cache was last refreshed.
	LastCheckedAt *string `json:"last_checked_at"`
	// InFlight is the non-terminal `self_updates` row, if there is one.
	InFlight *SelfUpdateDTO `json:"in_flight"`
	// Pending is the ONE self-update fact the section 12.3 gate last computed:
	// null on a settled host, otherwise the marker plus `actor_active` — which is
	// `systemctl is-active llamaman-selfupdate.service`, the same fact the gate
	// itself defers on (D91), so the UI renders "a swap is in flight" and the F24
	// card from the fact the daemon acted on.
	Pending *UpdatePendingDTO `json:"pending"`
}

// UpdatePendingDTO is `status.pending`.
type UpdatePendingDTO struct {
	SelfUpdateID  string `json:"self_update_id"`
	FromVersion   string `json:"from_version"`
	TargetVersion string `json:"target_version"`
	StagedAt      string `json:"staged_at"`
	ActorActive   bool   `json:"actor_active"`
}

// SelfUpdateDTO is one `self_updates` row.
type SelfUpdateDTO struct {
	ID          string `json:"id"`
	FromVersion string `json:"from_version"`
	ToVersion   string `json:"to_version"`
	State       string `json:"state"`
	// SignatureOK is null until the verification step has run.
	SignatureOK *bool `json:"signature_ok"`
	// DBBackupPath is the D14 snapshot taken before this update. Nothing restores
	// it automatically; it is step 3 of section 12.4's five-command procedure.
	DBBackupPath *string `json:"db_backup_path"`
	ErrorMessage *string `json:"error_message"`
	CreatedAt    string  `json:"created_at"`
	FinishedAt   *string `json:"finished_at"`
}

// UpdateReleaseDTO is one row of `GET /api/v1/update/releases`.
type UpdateReleaseDTO struct {
	Tag         string  `json:"tag"`
	Name        string  `json:"name"`
	PublishedAt *string `json:"published_at"`
	// BodyHTML is the changelog, rendered server-side with goldmark and
	// sanitized with bluemonday (D35). The raw markdown never reaches a browser.
	BodyHTML string `json:"body_html"`
	// HasAsset reports whether this release carries a tarball for this host's
	// architecture.
	HasAsset bool `json:"has_asset"`
	// Newer, Same and Older place the release against the running version, which
	// is what the update dialog needs in order to say "downgrade".
	Newer bool `json:"newer"`
	Same  bool `json:"same"`
	Older bool `json:"older"`
}

// ApplyUpdateRequest is the body of `POST /api/v1/update/apply`.
type ApplyUpdateRequest struct {
	// Tag is any tag `/update/releases` lists, NEWER OR OLDER: a downgrade is
	// this same pipeline pointed at an older tag (D90).
	Tag string `json:"tag"`
}

// ApplyUpdateResponse is its 202.
type ApplyUpdateResponse struct {
	SelfUpdateID string `json:"self_update_id"`
	JobID        string `json:"job_id"`
	// SchemaWarning is true for a target older than the running version. The
	// dialog shows section 12.4's warning: the one-click downgrade will
	// self-correct and consume the retained binary, and completing it takes the
	// five commands below.
	SchemaWarning bool `json:"schema_warning"`
	// Procedure is those five commands, verbatim and in order. Nothing prints the
	// `restore-db` line alone, and nothing prints the procedure without its
	// `reset-failed` step (D94).
	Procedure []string `json:"procedure,omitempty"`
}

func (a *API) updateRoutes() []Route {
	return []Route{
		a.updateStatusRoute(),
		a.updateCheckRoute(),
		a.updateReleasesRoute(),
		a.updateApplyRoute(),
	}
}

func (a *API) updateStatusRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/update/status",
		Auth:        AuthSession,
		OperationID: "getUpdateStatus",
		Summary: "Current version, latest known, the in-flight row, and the one self-update " +
			"fact the confirmation gate last computed.",
		Tag: "update",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.updateService()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			st, err := svc.Status(r.Context())
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := WriteJSON(w, http.StatusOK, updateStatusDTO(st)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status:      http.StatusOK,
			Description: "This host's update state.",
			Body:        UpdateStatusDTO{},
		},
	}
}

func (a *API) updateCheckRoute() Route {
	return Route{
		Method:      http.MethodPost,
		Pattern:     BasePath + "/update/check",
		Auth:        AuthSession,
		OperationID: "checkUpdates",
		Summary:     "Refresh the release feed.",
		Tag:         "update",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.updateService()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			// Accepted, not performed: the refresh talks to api.github.com, and a
			// handler that blocked on it would hold a session request open for as
			// long as GitHub takes. The result lands in `release_cache`, which
			// `GET /update/releases` reads.
			go func() {
				ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()),
					updateCheckTimeout)
				defer cancel()
				if err := svc.Check(ctx); err != nil {
					a.log.Warn("could not refresh the llamaman release feed", "error", err)
				}
			}()
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusAccepted)
		}),
		Success: Response{
			Status:      http.StatusAccepted,
			Description: "The refresh was accepted; the result lands in the release listing.",
		},
	}
}

// updateCheckTimeout bounds the detached refresh. It is generous because the
// client is the section 6.2 one, which retries a 5xx three times with a bounded
// backoff before it gives up and serves stale.
const updateCheckTimeout = 2 * time.Minute

func (a *API) updateReleasesRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/update/releases",
		Auth:        AuthSession,
		OperationID: "listUpdateReleases",
		Summary:     "Every release, with the changelog rendered server-side.",
		Tag:         "update",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.updateService()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			list, err := svc.Releases(r.Context())
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			items := make([]UpdateReleaseDTO, 0, len(list))
			for _, rel := range list {
				items = append(items, updateReleaseDTO(rel))
			}
			if err := WriteJSON(w, http.StatusOK, NewList(items, len(items), nil)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status:      http.StatusOK,
			Description: "The cached release listing.",
			Body:        List[UpdateReleaseDTO]{},
		},
	}
}

func (a *API) updateApplyRoute() Route {
	return Route{
		Method:      http.MethodPost,
		Pattern:     BasePath + "/update/apply",
		Auth:        AuthSession,
		OperationID: "applyUpdate",
		Summary: "Stage an update to any listed tag, newer or older. Four guard clauses, " +
			"all evaluated inside the transaction that inserts the row and its job.",
		Tag:         "update",
		Idempotent:  true,
		RequestBody: ApplyUpdateRequest{},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.updateService()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			var body ApplyUpdateRequest
			if err := DecodeJSON(w, r, &body); err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if body.Tag == "" {
				WriteError(w, r, a.log, BadRequest("a release tag is required"))
				return
			}

			res, err := svc.Apply(r.Context(), selfupdate.ApplyRequest{
				Tag:         body.Tag,
				Idempotency: idempotencyFor(r, body),
			})
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}

			status := http.StatusAccepted
			if res.Replayed {
				// D65's replay: the same job, answered 200 rather than the 202 a
				// fresh one gets, which is what makes a double-clicked Update a
				// replay instead of a 409.
				status = http.StatusOK
			}
			if err := WriteJSON(w, status, ApplyUpdateResponse{
				SelfUpdateID:  res.SelfUpdateID,
				JobID:         res.Job.ID,
				SchemaWarning: res.SchemaWarning,
				Procedure:     res.Procedure,
			}); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status:      http.StatusAccepted,
			Description: "The update was staged and its job created.",
			Body:        ApplyUpdateResponse{},
		},
		Errors: []Response{
			{
				Status:      http.StatusOK,
				Description: "An Idempotency-Key replay: the same job, not a second one.",
				Body:        ApplyUpdateResponse{},
			},
			{
				Status: http.StatusConflict,
				Description: "One of the four guard clauses refused: a build or another " +
					"self-update is live, this host has no service manager, the swap actor " +
					"cannot be summoned, or there is no working revert.",
				Codes: []model.ErrorCode{
					model.CodeJobInFlight,
					CodeSelfUpdateUnavailable,
					CodeSelfUpdateUnsupported,
					CodeRevertUnavailable,
				},
			},
			{
				Status:      http.StatusUnprocessableEntity,
				Description: "The Idempotency-Key was reused with a different body.",
				Codes:       []model.ErrorCode{model.CodeIdempotencyKeyReused},
			},
		},
	}
}

// updateService is the nil check every route in this file shares. A daemon built
// without the subsystem answers 503 rather than dropping the routes: they are
// documented endpoints, and a build gap is reported, never faked.
func (a *API) updateService() (UpdateService, error) {
	if a.cfg.Update == nil {
		return nil, Errorf(http.StatusServiceUnavailable, CodeInternalError,
			"this daemon was built without the self-update subsystem")
	}
	return a.cfg.Update, nil
}

func updateStatusDTO(s selfupdate.Status) UpdateStatusDTO {
	out := UpdateStatusDTO{
		CurrentVersion:  s.CurrentVersion,
		LatestVersion:   s.LatestVersion,
		UpdateAvailable: s.UpdateAvailable,
		LastCheckedAt:   TimePtr(s.LastCheckedAt),
	}
	if s.InFlight != nil {
		row := SelfUpdateDTO{
			ID:           s.InFlight.ID,
			FromVersion:  s.InFlight.FromVersion,
			ToVersion:    s.InFlight.ToVersion,
			State:        string(s.InFlight.State),
			SignatureOK:  s.InFlight.SignatureOK,
			DBBackupPath: s.InFlight.DBBackupPath,
			ErrorMessage: s.InFlight.ErrorMessage,
			CreatedAt:    Time(s.InFlight.CreatedAt),
			FinishedAt:   TimePtr(s.InFlight.FinishedAt),
		}
		out.InFlight = &row
	}
	if s.Pending != nil {
		out.Pending = &UpdatePendingDTO{
			SelfUpdateID:  s.Pending.SelfUpdateID,
			FromVersion:   s.Pending.FromVersion,
			TargetVersion: s.Pending.TargetVersion,
			StagedAt:      Time(s.Pending.StagedAt),
			ActorActive:   s.Pending.ActorActive,
		}
	}
	return out
}

func updateReleaseDTO(r selfupdate.Release) UpdateReleaseDTO {
	return UpdateReleaseDTO{
		Tag:         r.Tag,
		Name:        r.Name,
		PublishedAt: TimePtr(r.PublishedAt),
		BodyHTML:    r.BodyHTML,
		HasAsset:    r.HasAsset,
		Newer:       r.Newer,
		Same:        r.Same,
		Older:       r.Older,
	}
}
