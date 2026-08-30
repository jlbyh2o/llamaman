package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/jlbyh2o/llamaman/internal/api/middleware"
	"github.com/jlbyh2o/llamaman/internal/auth"
	"github.com/jlbyh2o/llamaman/internal/model"
)

// The setup-wizard endpoints of DESIGN section 3.2 that this binary can serve
// today: the state, the claim, the skip and the completion.
//
// The four rows of section 3.2 that are absent — `setup/toolchain`,
// `setup/llamacpp`, `setup/hf-token`, `setup/cache/scan` — are absent
// DELIBERATELY rather than stubbed. Each delegates to a subsystem that does not
// exist yet, and D43's conformance suite is only meaningful if a documented
// endpoint is one the binary actually serves: a route registered here appears in
// api/openapi.json, and a 501 behind it would be a promise in the contract that
// the daemon cannot keep. They join the registry with the subsystems they call.

// SetupService is everything this layer needs from internal/setup. As with
// SessionService, it speaks domain types and strings only — the loopback question
// is answered here, where the request is, and passed in as a bool.
type SetupService interface {
	// State answers the public `GET /api/v1/setup/state`.
	State(ctx context.Context, loopback bool) (model.SetupState, error)
	// ClaimPassword is section 2.2a's burn plus the wizard's first step: it
	// creates `admin_account`, stamps the claim and mints the session.
	ClaimPassword(ctx context.Context, password, ip, userAgent string) (model.SessionCredential, error)
	// Skip marks a skippable step skipped, refusing one that section 11.2 marks
	// otherwise.
	Skip(ctx context.Context, step model.WizardStep) error
	// Finish marks the wizard done.
	Finish(ctx context.Context) error
}

// SetupStateDTO is the body of `GET /api/v1/setup/state` (section 3.2).
type SetupStateDTO struct {
	Claimed  bool `json:"claimed"`
	Complete bool `json:"complete"`
	// TokenRequired is false for loopback callers (D38).
	TokenRequired bool `json:"token_required"`
	// ActiveStep is the step a resuming browser should open, or null once every
	// step is finished. It is derived from the stored rows, so a refresh or a
	// daemon restart mid-wizard resumes in the right place (section 11.2).
	ActiveStep *string         `json:"active_step"`
	Steps      []WizardStepDTO `json:"steps"`
}

// WizardStepDTO is one entry of section 3.2's `steps` array.
type WizardStepDTO struct {
	Step  string `json:"step"`
	State string `json:"state"`
	// Skippable is section 11.2's "skippable" column.
	Skippable bool `json:"skippable"`
	// Blocked reports that an earlier non-skippable step is unfinished. The
	// gate is enforced server-side; this only lets the UI say so before the
	// click.
	Blocked bool `json:"blocked"`
}

// SetupPasswordRequest is the body of `POST /api/v1/setup/password`.
type SetupPasswordRequest struct {
	Password string `json:"password"`
}

// SetupSkipRequest is the body of `POST /api/v1/setup/skip`.
type SetupSkipRequest struct {
	Step string `json:"step"`
}

func (a *API) setupRoutes() []Route {
	return []Route{
		a.setupStateRoute(),
		a.setupPasswordRoute(),
		a.setupSkipRoute(),
		a.setupCompleteRoute(),
	}
}

func (a *API) setupStateRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/setup/state",
		Auth:        AuthPublic,
		OperationID: "getSetupState",
		Summary:     "Claim state, whether this caller needs a setup token, and where the wizard stands.",
		Tag:         "setup",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.setup()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			st, err := svc.State(r.Context(), middleware.IsLoopback(r))
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := WriteJSON(w, http.StatusOK, setupStateDTO(st)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status:      http.StatusOK,
			Description: "The wizard's state for this caller.",
			Body:        SetupStateDTO{},
		},
	}
}

func (a *API) setupPasswordRoute() Route {
	return Route{
		Method:      http.MethodPost,
		Pattern:     BasePath + "/setup/password",
		Auth:        AuthSetup,
		OperationID: "setupPassword",
		Summary: "Claim this host: create the admin account, burn the one-time token and " +
			"log the browser in. Loopback callers need no `X-Setup-Token` (D38).",
		Tag:         "setup",
		RequestBody: SetupPasswordRequest{},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.setup()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			var req SetupPasswordRequest
			if err := DecodeJSON(w, r, &req); err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			cred, err := svc.ClaimPassword(r.Context(), req.Password, requestIP(r), r.UserAgent())
			if err != nil {
				if errors.Is(err, auth.ErrAlreadyClaimed) {
					// The loser of section 2.2a's claim race, and the replayed
					// request. It is the same answer the session gate gives a
					// `setup` route once the account exists, so the SPA has one
					// code to react to however it lost.
					WriteError(w, r, a.log, Conflict(middleware.CodeSetupAlreadyClaimed,
						"this host has already been claimed"))
					return
				}
				WriteError(w, r, a.log, err)
				return
			}
			a.setSessionCookies(w, r, cred)
			WriteNoContent(w)
		}),
		Success: Response{
			Status:      http.StatusNoContent,
			Description: "The host is claimed and the caller is logged in.",
		},
		Errors: []Response{
			{
				Status:      http.StatusBadRequest,
				Description: "The password does not meet the minimum length.",
				Codes:       []model.ErrorCode{model.CodePasswordInvalid},
			},
			{
				Status:      http.StatusConflict,
				Description: "Another caller claimed this host first.",
				Codes:       []model.ErrorCode{middleware.CodeSetupAlreadyClaimed},
			},
		},
	}
}

func (a *API) setupSkipRoute() Route {
	return Route{
		Method:      http.MethodPost,
		Pattern:     BasePath + "/setup/skip",
		Auth:        AuthSession,
		OperationID: "skipSetupStep",
		Summary:     "Skip a wizard step that section 11.2 marks skippable.",
		Tag:         "setup",
		RequestBody: SetupSkipRequest{},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.setup()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			var req SetupSkipRequest
			if err := DecodeJSON(w, r, &req); err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := svc.Skip(r.Context(), model.WizardStep(req.Step)); err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			WriteNoContent(w)
		}),
		Success: Response{
			Status:      http.StatusNoContent,
			Description: "The step is marked skipped.",
		},
		Errors: []Response{
			{
				Status:      http.StatusBadRequest,
				Description: "No such wizard step.",
				Codes:       []model.ErrorCode{model.CodeWizardStepUnknown},
			},
			{
				Status:      http.StatusConflict,
				Description: "The step is not skippable, or an earlier step is unfinished.",
				Codes:       []model.ErrorCode{model.CodeWizardStepLocked},
			},
		},
	}
}

func (a *API) setupCompleteRoute() Route {
	return Route{
		Method:      http.MethodPost,
		Pattern:     BasePath + "/setup/complete",
		Auth:        AuthSession,
		OperationID: "completeSetup",
		Summary:     "Finish the wizard. Refused while a non-skippable step is unfinished.",
		Tag:         "setup",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.setup()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := svc.Finish(r.Context()); err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			WriteNoContent(w)
		}),
		Success: Response{
			Status:      http.StatusNoContent,
			Description: "The wizard is complete.",
		},
		Errors: []Response{{
			Status:      http.StatusConflict,
			Description: "A non-skippable step is still unfinished.",
			Codes:       []model.ErrorCode{model.CodeWizardStepLocked},
		}},
	}
}

func (a *API) setup() (SetupService, error) {
	if a.cfg.Setup == nil {
		return nil, Errorf(http.StatusServiceUnavailable, CodeInternalError,
			"this daemon was built without a setup service")
	}
	return a.cfg.Setup, nil
}

func setupStateDTO(st model.SetupState) SetupStateDTO {
	steps := make([]WizardStepDTO, 0, len(st.Steps))
	for _, v := range st.Steps {
		steps = append(steps, WizardStepDTO{
			Step:      string(v.Step),
			State:     string(v.State),
			Skippable: v.Skippable,
			Blocked:   v.Blocked,
		})
	}
	out := SetupStateDTO{
		Claimed:       st.Claimed,
		Complete:      st.Complete,
		TokenRequired: st.TokenRequired,
		Steps:         steps,
	}
	if step, ok := st.ActiveStep(); ok {
		s := string(step)
		out.ActiveStep = &s
	}
	return out
}
