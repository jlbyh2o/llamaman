package api

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// The three rows of DESIGN section 3.4.
//
// Two rules from that section shape this file, and both are about what is NOT
// here.
//
// **Secrets are not settings.** The Hugging Face token and the optional GitHub
// token never appear in `GET /api/v1/settings`, because a settings value is
// returned in the clear and these must not be. Each has its own validating
// triple under section 3.6, returning presence, hint and validity only. The
// Settings UI renders them inside the groups they belong to — the HF token under
// Hugging Face, the GitHub token under Builds — so a user finds them where they
// expect to, while the transport stays the secret-shaped one.
//
// **The schema travels with the values.** A settings screen that hard-codes
// which keys exist, what type each is and what bounds it has is a screen that
// drifts from the registry the daemon validates against — and under SPEC section
// 3.9's zero-config mandate the UI is the ONLY way to set any of this, so a
// drifted screen is an unreachable setting. `GET` therefore answers
// `{"values":{…},"schema":[…]}` and the form is generated from the second half.

// SettingsService is section 3.4 as this layer needs it. internal/app satisfies
// it over *settings.Cache: the registry lives there, and so does the knowledge
// of which values the RUNNING daemon still holds, which is what makes
// `restart_required` answerable at all.
type SettingsService interface {
	// Settings is `GET /api/v1/settings`: the current values and the schema
	// that describes them.
	Settings(ctx context.Context) (SettingsDTO, error)
	// PatchSettings applies a partial update. A key that fails its definition
	// is `400 setting_invalid` naming that key, and NOTHING is written — a
	// half-applied form is worse than a refused one.
	PatchSettings(ctx context.Context, values map[string]json.RawMessage) (PatchSettingsDTO, error)
	// ResetSettings deletes the named override rows so the built-in defaults
	// resume.
	ResetSettings(ctx context.Context, keys []string) (SettingsDTO, error)
}

// SettingKindValues lists the `type` values a SettingDefDTO can carry. It is a
// closed set so the generated TypeScript can switch on it exhaustively and the
// form knows it has a widget for every case.
func SettingKindValues() []string { return []string{"int", "bool", "string", "enum"} }

// SettingDefDTO is one entry of the schema half of `GET /api/v1/settings`.
type SettingDefDTO struct {
	Key string `json:"key"`
	// Type is one of SettingKindValues.
	Type string `json:"type"`
	// Default is the built-in value, typed to match Type.
	Default any `json:"default"`
	// Min and Max bound an int; null on either side means unbounded there.
	Min *int64 `json:"min"`
	Max *int64 `json:"max"`
	// Enum closes the value set for an enum, null otherwise.
	Enum []string `json:"enum"`
	// Label is the human name the form renders. Never null: a key with no
	// authored label falls back to a readable rendering of the key itself,
	// because a blank field label is worse than an imperfect one.
	Label string `json:"label"`
	// Group is the section of the settings screen this key belongs to — the
	// part of the key before its first dot.
	Group string `json:"group"`
	// RestartRequired means the running daemon still holds the old value, so
	// the UI shows "Restart to apply" wired to POST /system/restart.
	RestartRequired bool `json:"restart_required"`
	// UnitChangeRequired means the change needs the installer rather than the
	// daemon (section 5.4a). No setting in the v1 registry carries it; the
	// field exists so the UI can render that affordance if one ever does.
	UnitChangeRequired bool `json:"unit_change_required"`
}

// SettingsDTO is the body of `GET /api/v1/settings` and of the reset.
type SettingsDTO struct {
	// Values is every registered key's CURRENT value — the stored override
	// where one exists, the built-in default otherwise. A key is never absent:
	// a form that has to distinguish "unset" from "defaulted" is a form that
	// will render an empty field for a setting that has a value.
	Values map[string]any  `json:"values"`
	Schema []SettingDefDTO `json:"schema"`
}

// PatchSettingsDTO is the body of `PATCH /api/v1/settings`.
type PatchSettingsDTO struct {
	// Values is the full post-patch value map, not just the keys that moved, so
	// one response re-seeds the whole form.
	Values map[string]any `json:"values"`
	// RestartRequired is true when any key this patch touched is one the running
	// daemon still holds the old value for. The UI offers POST /system/restart.
	RestartRequired bool `json:"restart_required"`
	// RestartKeys names them, so the button can say which settings are waiting
	// rather than "some settings".
	RestartKeys []string `json:"restart_keys"`
}

// ResetSettingsRequest is the body of `POST /api/v1/settings/reset`.
type ResetSettingsRequest struct {
	Keys []string `json:"keys"`
}

func (a *API) settingsRoutes() []Route {
	return []Route{
		a.getSettingsRoute(),
		a.patchSettingsRoute(),
		a.resetSettingsRoute(),
	}
}

func (a *API) settingsAdmin() (SettingsService, error) {
	if a.cfg.SettingsAdmin == nil {
		return nil, Errorf(http.StatusServiceUnavailable, CodeInternalError,
			"this daemon was built without a settings service")
	}
	return a.cfg.SettingsAdmin, nil
}

func (a *API) getSettingsRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/settings",
		Auth:        AuthSession,
		OperationID: "getSettings",
		Summary: "Every registered key's current value, plus the schema the form is generated " +
			"from. Secrets are never here — they have their own validating endpoints.",
		Tag: "settings",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.settingsAdmin()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			out, err := svc.Settings(r.Context())
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := WriteJSON(w, http.StatusOK, out); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{Status: http.StatusOK, Description: "The values and their schema.",
			Body: SettingsDTO{}},
	}
}

func (a *API) patchSettingsRoute() Route {
	return Route{
		Method:      http.MethodPatch,
		Pattern:     BasePath + "/settings",
		Auth:        AuthSession,
		OperationID: "patchSettings",
		Summary: "Partial update. A value that fails its definition refuses the WHOLE patch — a " +
			"half-applied settings form is worse than a refused one.",
		Tag: "settings",
		// The body is an open map of key to value, so there is no struct to
		// reflect. RequestBody stays nil and the summary carries the shape;
		// every key's type is already in the schema half of the GET.
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.settingsAdmin()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			var body map[string]json.RawMessage
			if err := DecodeJSON(w, r, &body); err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if len(body) == 0 {
				WriteError(w, r, a.log, BadRequest("the patch names no settings"))
				return
			}
			out, err := svc.PatchSettings(r.Context(), body)
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := WriteJSON(w, http.StatusOK, out); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{Status: http.StatusOK,
			Description: "The full post-patch value map, and whether a restart is owed.",
			Body:        PatchSettingsDTO{}},
		Errors: []Response{{Status: http.StatusBadRequest,
			Description: "A key is not in the registry, or its value fails that key's type, " +
				"bounds, enum or extra check. `details.key` names it and nothing was written.",
			Codes: []model.ErrorCode{model.CodeSettingInvalid}}},
	}
}

func (a *API) resetSettingsRoute() Route {
	return Route{
		Method:      http.MethodPost,
		Pattern:     BasePath + "/settings/reset",
		Auth:        AuthSession,
		OperationID: "resetSettings",
		Summary:     "Delete the named override rows; the built-in defaults resume.",
		Tag:         "settings",
		RequestBody: ResetSettingsRequest{},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.settingsAdmin()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			var body ResetSettingsRequest
			if err := DecodeJSON(w, r, &body); err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			out, err := svc.ResetSettings(r.Context(), body.Keys)
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := WriteJSON(w, http.StatusOK, out); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{Status: http.StatusOK,
			Description: "The values after the reset, and the unchanged schema.",
			Body:        SettingsDTO{}},
		Errors: []Response{{Status: http.StatusBadRequest,
			Description: "A key is not in the registry. Nothing was deleted.",
			Codes:       []model.ErrorCode{model.CodeSettingInvalid}}},
	}
}
