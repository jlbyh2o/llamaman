package api

import (
	"context"
	"net/http"

	"github.com/jlbyh2o/llamaman/internal/buildinfo"
	"github.com/jlbyh2o/llamaman/internal/model"
)

// Health is the body of `GET /healthz` (DESIGN section 3.1): "liveness only, no
// state". It touches nothing — no database, no bus, no filesystem — because a
// liveness probe that can be made to fail by a slow dependency is a probe that
// restarts a healthy daemon.
type Health struct {
	Status  string `json:"status"`
	Version string `json:"version"`
}

// Meta is the body of `GET /api/v1/meta` (section 3.1), the endpoint
// `install.sh` polls at step 9 while it waits for the daemon to come up.
//
// It is public and it is the ONLY public endpoint that reads state, which
// bounds what it may say: a version, a commit, two booleans about whether this
// host has been claimed, and the port the walk landed on. Nothing here helps an
// unauthenticated caller do anything except decide whether to open the wizard.
type Meta struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	// SetupComplete is "the wizard has been finished". The SPA uses it to
	// decide between the wizard and the dashboard.
	SetupComplete bool `json:"setup_complete"`
	// Claimed is "`setup_claim.claimed_at` is stamped" — the one-time token has
	// been burned and an admin account exists. A host can be claimed without
	// being complete: that is a wizard interrupted after the password step.
	Claimed bool `json:"claimed"`
	// UIPort is `runtime_info.ui_port`, the port the walk of section 11.1 step
	// 7 ACTUALLY landed on, which is not necessarily `ui.port_desired`.
	UIPort int `json:"ui_port"`
}

// MetaProvider answers `GET /api/v1/meta`. internal/app implements it over the
// store and this boot's resolved port; the interface is declared here because
// the consumer owns it (DESIGN section 1).
type MetaProvider interface {
	Meta(ctx context.Context) (Meta, error)
}

func (a *API) healthRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     "/healthz",
		Auth:        AuthPublic,
		OperationID: "getHealth",
		Summary:     "Liveness only, no state.",
		Tag:         "health",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if err := WriteJSON(w, http.StatusOK, Health{
				Status:  "ok",
				Version: buildinfo.Version,
			}); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status:      http.StatusOK,
			Description: "The daemon is alive.",
			Body:        Health{},
		},
	}
}

func (a *API) metaRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/meta",
		Auth:        AuthPublic,
		OperationID: "getMeta",
		Summary:     "Version, claim state and the port the management listener landed on — what install.sh polls.",
		Tag:         "meta",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if a.cfg.Meta == nil {
				WriteError(w, r, a.log, Errorf(http.StatusServiceUnavailable, CodeInternalError,
					"this daemon was built without a meta provider"))
				return
			}
			m, err := a.cfg.Meta.Meta(r.Context())
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := WriteJSON(w, http.StatusOK, m); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status:      http.StatusOK,
			Description: "This daemon's identity and claim state.",
			Body:        Meta{},
		},
		Errors: []Response{{
			Status:      http.StatusServiceUnavailable,
			Description: "The daemon is up but has not finished resolving its own identity.",
			Codes:       []model.ErrorCode{CodeInternalError},
		}},
	}
}
