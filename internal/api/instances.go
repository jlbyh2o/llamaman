package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/jlbyh2o/llamaman/internal/instances"
	"github.com/jlbyh2o/llamaman/internal/model"
)

// The instance endpoints of DESIGN section 3.10 this binary serves today:
// list, create, detail, patch and delete.
//
// The rest of section 3.10's table — start, stop, restart, reset-failed,
// safe-start, autostart, pin-ngl, status, usage, starts, command, logs, the
// llama-server proxies and `POST /instances/validate` — are absent
// DELIBERATELY rather than stubbed, for the reason section 3.2's handlers give:
// each needs the supervisor, the gateway or the fit calculator, and a route
// registered here appears in api/openapi.json, where a 501 behind it would be a
// promise in the contract the daemon cannot keep. They join the registry with
// the subsystems they call.

// InstanceService is everything this layer needs from internal/instances. The
// consumer owns the interface (DESIGN section 1); *instances.Service satisfies
// it.
type InstanceService interface {
	List(ctx context.Context, includeDeleted bool) ([]instances.View, error)
	Get(ctx context.Context, id string) (instances.View, error)
	Create(ctx context.Context, p instances.CreateParams) (instances.View, error)
	Patch(ctx context.Context, id string, p instances.PatchParams) (instances.View, error)
	Delete(ctx context.Context, id string, p instances.DeleteParams) (instances.DeleteResult, error)
}

// InstanceDTO is one row of `GET /api/v1/instances`: config ⋈ status with the
// four derived flags beside `state` (section 2.8).
//
// `flags` is a model.FlagSet rather than a hand-copied DTO, and it is the one
// deliberate exception to "a DTO never embeds a model struct". The rule exists
// because a column changes for storage reasons while a field changes for client
// reasons — but D41 makes those the SAME reason here: `flags_json` is one JSON
// column typed in Go as this struct, so a new upstream flag is a struct field
// and a golden argv test, never a migration and never a wire change the storage
// layer did not also make. Duplicating 43 optional fields would create exactly
// the drift the generated document exists to prevent.
type InstanceDTO struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	DisplayName *string `json:"display_name"`
	Description *string `json:"description"`

	ModelID       *string `json:"model_id"`
	MmprojModelID *string `json:"mmproj_model_id"`
	DraftModelID  *string `json:"draft_model_id"`

	PublicPort   int `json:"public_port"`
	InternalPort int `json:"internal_port"`

	AuthMode         string `json:"auth_mode"`
	Autostart        bool   `json:"autostart"`
	RestartPolicy    string `json:"restart_policy"`
	RestartMax       int    `json:"restart_max"`
	RestartWindowSec int    `json:"restart_window_sec"`

	Flags      model.FlagSet `json:"flags"`
	ExtraFlags string        `json:"extra_flags"`

	ConfigHash      string `json:"config_hash"`
	DesiredState    string `json:"desired_state"`
	DraftValidation string `json:"draft_validation"`
	UnitName        string `json:"unit_name"`
	// Generation is the value a PATCH must echo. A mismatch is
	// `409 conflict_generation`, and it always means a human edited the
	// configuration — none of section 2.8's exceptional writers bumps it.
	Generation int64 `json:"generation"`

	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
	DeletedAt *string `json:"deleted_at"`

	Status InstanceStatusDTO `json:"status"`

	// The four derived flags of section 2.8: computed on read, never stored,
	// and never states — which is what lets an instance be `ready` and flagged
	// at the same time.
	RestartRequired bool    `json:"restart_required"`
	StaleVersion    bool    `json:"stale_version"`
	Inhibited       bool    `json:"inhibited"`
	InhibitReason   *string `json:"inhibit_reason"`
	DraftUnverified bool    `json:"draft_unverified"`
}

// InstanceStatusDTO is the observed half. Every nullable column stays nullable
// on the wire: `requests_served` is null when the metrics endpoint is off, and
// the UI must say "metrics disabled" rather than show a zero that reads as an
// answer.
type InstanceStatusDTO struct {
	State             string  `json:"state"`
	SystemdActive     *string `json:"systemd_active"`
	SystemdSub        *string `json:"systemd_sub"`
	SystemdResult     *string `json:"systemd_result"`
	MainPID           *int64  `json:"main_pid"`
	ExeVersionID      *string `json:"exe_version_id"`
	AppliedConfigHash *string `json:"applied_config_hash"`
	ReadyAt           *string `json:"ready_at"`
	LastChangeAt      string  `json:"last_change_at"`
	LastHealthAt      *string `json:"last_health_at"`
	HealthCode        *int64  `json:"health_code"`
	SlotsTotal        *int64  `json:"slots_total"`
	SlotsBusy         *int64  `json:"slots_busy"`
	CtxSize           *int64  `json:"ctx_size"`
	RequestsServed    *int64  `json:"requests_served"`
	RSSBytes          *int64  `json:"rss_bytes"`
	VRAMBytes         *int64  `json:"vram_bytes"`
	GPUAttribution    string  `json:"gpu_attribution"`
	LastExitCode      *int64  `json:"last_exit_code"`
	LastError         *string `json:"last_error"`
}

// InstanceDetailDTO is `GET /api/v1/instances/{id}`: the row plus the rendered
// argv and the last five starts (section 3.10).
type InstanceDetailDTO struct {
	Instance InstanceDTO `json:"instance"`
	// Argv is the command line this instance would run, null while a model is
	// still downloading or no llama.cpp build is active — showing a half-real
	// command line would be worse than showing none.
	Argv []string `json:"argv"`
	// ActiveVersionID is the build `argv` was rendered against and the one the
	// `stale_version` flag was computed against.
	ActiveVersionID string `json:"active_version_id"`
	// UnknownFlags is section 5.7's flag-churn guard. It is empty both when
	// every flag is known and when the build recorded no help capture at all;
	// `warnings` says which.
	UnknownFlags []string           `json:"unknown_flags"`
	Starts       []InstanceStartDTO `json:"starts"`
	Warnings     []WarningDTO       `json:"warnings"`
}

// InstanceStartDTO is one row of the start ledger (section 2.8).
type InstanceStartDTO struct {
	ID      string `json:"id"`
	At      string `json:"at"`
	Trigger string `json:"trigger"`

	ConfigHash string `json:"config_hash"`
	// EffectiveConfigHash is what was ACTUALLY rendered: equal to config_hash
	// normally, and the override hash for a safe start (D61).
	EffectiveConfigHash *string `json:"effective_config_hash"`
	// Override is the transient FlagSet patch this run consumed, shown inline
	// so "it only works with -ngl 0" is a fact in the history rather than
	// something a user has to remember (section 3.10b).
	Override map[string]any `json:"override"`
	Argv     []string       `json:"argv"`

	LlamacppVersionID *string `json:"llamacpp_version_id"`
	ReadyAt           *string `json:"ready_at"`
	// Outcome is null while the run is in flight, and is written exactly once
	// at the end of a run (D63). `ready` is deliberately not one of its values.
	Outcome      *string        `json:"outcome"`
	ExitCode     *int64         `json:"exit_code"`
	ErrorCode    *string        `json:"error_code"`
	ErrorMessage *string        `json:"error_message"`
	Detail       map[string]any `json:"detail"`
	EndedAt      *string        `json:"ended_at"`
}

// WarningDTO is one entry of a `warnings` array: the request succeeded, and
// there is something the client should show (section 3.10a).
type WarningDTO struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// CreateInstanceResponse is `POST /api/v1/instances`. The warnings travel
// beside the created row because a deferred draft check is a successful save
// that owes a check (D34, section 3.10a).
type CreateInstanceResponse struct {
	Instance InstanceDTO  `json:"instance"`
	Warnings []WarningDTO `json:"warnings"`
}

// CreateInstanceRequest is the body of `POST /api/v1/instances`.
type CreateInstanceRequest struct {
	Name        string  `json:"name"`
	DisplayName *string `json:"display_name,omitempty"`
	Description *string `json:"description,omitempty"`

	ModelID       string  `json:"model_id"`
	MmprojModelID *string `json:"mmproj_model_id,omitempty"`
	DraftModelID  *string `json:"draft_model_id,omitempty"`

	// PublicPort and InternalPort are auto-allocated when omitted, against the
	// same rules a supplied one is validated by (section 2.8).
	PublicPort   *int `json:"public_port,omitempty"`
	InternalPort *int `json:"internal_port,omitempty"`

	AuthMode         *string `json:"auth_mode,omitempty"`
	Autostart        *bool   `json:"autostart,omitempty"`
	RestartPolicy    *string `json:"restart_policy,omitempty"`
	RestartMax       *int    `json:"restart_max,omitempty"`
	RestartWindowSec *int    `json:"restart_window_sec,omitempty"`

	Flags      *model.FlagSet `json:"flags,omitempty"`
	ExtraFlags *string        `json:"extra_flags,omitempty"`
}

// PatchInstanceRequest is the body of `PATCH /api/v1/instances/{id}`: partial,
// plus the `generation` the client read.
//
// An omitted field is left alone. An EMPTY STRING clears a nullable one —
// `"draft_model_id": ""` detaches the draft model — which is how a partial
// update expresses "remove this" without a second null-versus-absent encoding
// that no generated client would type correctly.
//
// `autostart` and `desired_state` are deliberately not here: autostart is
// `PUT /instances/{id}/autostart`, which only enables or disables the unit, and
// desired state belongs to the start/stop endpoints. Folding either into a
// config PATCH would let an edit to an unrelated field silently change what
// happens at the next boot.
type PatchInstanceRequest struct {
	Generation int64 `json:"generation"`

	Name        *string `json:"name,omitempty"`
	DisplayName *string `json:"display_name,omitempty"`
	Description *string `json:"description,omitempty"`

	ModelID       *string `json:"model_id,omitempty"`
	MmprojModelID *string `json:"mmproj_model_id,omitempty"`
	DraftModelID  *string `json:"draft_model_id,omitempty"`

	PublicPort   *int `json:"public_port,omitempty"`
	InternalPort *int `json:"internal_port,omitempty"`

	AuthMode         *string `json:"auth_mode,omitempty"`
	RestartPolicy    *string `json:"restart_policy,omitempty"`
	RestartMax       *int    `json:"restart_max,omitempty"`
	RestartWindowSec *int    `json:"restart_window_sec,omitempty"`

	Flags      *model.FlagSet `json:"flags,omitempty"`
	ExtraFlags *string        `json:"extra_flags,omitempty"`
}

// PatchInstanceResponse carries the updated row and, per section 3.10,
// `restart_required` — which is already one of the four flags on the row
// itself, so the field here is the row.
type PatchInstanceResponse struct {
	Instance InstanceDTO  `json:"instance"`
	Warnings []WarningDTO `json:"warnings"`
}

// DeleteInstanceResponse is the 202 of `DELETE /api/v1/instances/{id}`.
type DeleteInstanceResponse struct {
	// Purged distinguishes the soft delete from `?purge=true`.
	Purged bool `json:"purged"`
	// Hints carries the manual commands for anything the daemon could not do
	// itself — the `unit_still_enabled` case, where `DisableUnitFiles` was
	// skipped or denied and the exact `sudo systemctl disable …` line is what
	// the user needs (section 3.10c step 1).
	Hints []string `json:"hints"`
}

func (a *API) instanceRoutes() []Route {
	return []Route{
		a.listInstancesRoute(),
		a.createInstanceRoute(),
		a.getInstanceRoute(),
		a.patchInstanceRoute(),
		a.deleteInstanceRoute(),
	}
}

func (a *API) listInstancesRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/instances",
		Auth:        AuthSession,
		OperationID: "listInstances",
		Summary: "Config joined with status, plus the four derived flags. " +
			"Soft-deleted instances are excluded unless ?include_deleted=true.",
		Tag: "instances",
		Query: []QueryParam{{
			Name: "include_deleted",
			Description: "Include soft-deleted instances. They keep their start history and " +
				"accounting but hold neither their name nor their ports (D68).",
			Type: "boolean",
		}},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.instances()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			views, err := svc.List(r.Context(), queryBool(r, "include_deleted"))
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			items := make([]InstanceDTO, 0, len(views))
			for _, v := range views {
				items = append(items, instanceDTO(v))
			}
			if err := WriteJSON(w, http.StatusOK, NewList(items, len(items), nil)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status:      http.StatusOK,
			Description: "Every instance this host manages.",
			Body:        List[InstanceDTO]{},
		},
	}
}

func (a *API) createInstanceRoute() Route {
	return Route{
		Method:      http.MethodPost,
		Pattern:     BasePath + "/instances",
		Auth:        AuthSession,
		OperationID: "createInstance",
		Summary: "Create an instance. Ports are auto-allocated when omitted and validated " +
			"against the section 2.8 rules.",
		Tag:         "instances",
		RequestBody: CreateInstanceRequest{},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.instances()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			var body CreateInstanceRequest
			if err := DecodeJSON(w, r, &body); err != nil {
				WriteError(w, r, a.log, err)
				return
			}

			v, err := svc.Create(r.Context(), instances.CreateParams{
				Name:             body.Name,
				DisplayName:      body.DisplayName,
				Description:      body.Description,
				ModelID:          body.ModelID,
				MmprojModelID:    body.MmprojModelID,
				DraftModelID:     body.DraftModelID,
				PublicPort:       body.PublicPort,
				InternalPort:     body.InternalPort,
				AuthMode:         enumPtr[model.AuthMode](body.AuthMode),
				Autostart:        body.Autostart,
				RestartPolicy:    enumPtr[model.RestartPolicy](body.RestartPolicy),
				RestartMax:       body.RestartMax,
				RestartWindowSec: body.RestartWindowSec,
				Flags:            flagsOf(body.Flags),
				ExtraFlags:       stringOr(body.ExtraFlags, ""),
			})
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := WriteJSON(w, http.StatusCreated, CreateInstanceResponse{
				Instance: instanceDTO(v),
				Warnings: warningDTOs(v.Warnings),
			}); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status:      http.StatusCreated,
			Description: "The instance was created. `warnings` may carry a deferred draft check.",
			Body:        CreateInstanceResponse{},
		},
		Errors: []Response{
			{
				Status:      http.StatusConflict,
				Description: "Another live instance already has this name.",
				Codes:       []model.ErrorCode{model.CodeInstanceNameTaken},
			},
			{
				Status: http.StatusUnprocessableEntity,
				Description: "The configuration was refused: a name that is not a legal unit " +
					"id, a port that breaks one of the section 2.8 rules, a draft model whose " +
					"vocabulary differs, `-ngl auto` with an explicit tensor split, an " +
					"`extra_flags` override of a flag the renderer owns, or a model that does " +
					"not exist.",
				Codes: []model.ErrorCode{
					model.CodeInstanceNameInvalid,
					model.CodePortUnavailable,
					model.CodeDraftVocabMismatch,
					model.CodeNGLAutoConflict,
					model.CodeExtraFlagForbidden,
					model.CodeModelMissing,
					model.CodeBadFlags,
				},
			},
		},
	}
}

func (a *API) getInstanceRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/instances/{id}",
		Auth:        AuthSession,
		OperationID: "getInstance",
		Summary:     "One instance, with its rendered argv and its last five starts.",
		Tag:         "instances",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.instances()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			v, err := svc.Get(r.Context(), r.PathValue("id"))
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := WriteJSON(w, http.StatusOK, instanceDetailDTO(v)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status:      http.StatusOK,
			Description: "The instance.",
			Body:        InstanceDetailDTO{},
		},
		Errors: []Response{{
			Status:      http.StatusNotFound,
			Description: "No instance has this id.",
			Codes:       []model.ErrorCode{CodeNotFound},
		}},
	}
}

func (a *API) patchInstanceRoute() Route {
	return Route{
		Method:      http.MethodPatch,
		Pattern:     BasePath + "/instances/{id}",
		Auth:        AuthSession,
		OperationID: "patchInstance",
		Summary:     "Edit an instance. The body must carry the `generation` the client read.",
		Tag:         "instances",
		RequestBody: PatchInstanceRequest{},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.instances()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			var body PatchInstanceRequest
			if err := DecodeJSON(w, r, &body); err != nil {
				WriteError(w, r, a.log, err)
				return
			}

			v, err := svc.Patch(r.Context(), r.PathValue("id"), instances.PatchParams{
				Generation:       body.Generation,
				Name:             body.Name,
				DisplayName:      clearable(body.DisplayName),
				Description:      clearable(body.Description),
				ModelID:          body.ModelID,
				MmprojModelID:    clearable(body.MmprojModelID),
				DraftModelID:     clearable(body.DraftModelID),
				PublicPort:       body.PublicPort,
				InternalPort:     body.InternalPort,
				AuthMode:         enumPtr[model.AuthMode](body.AuthMode),
				RestartPolicy:    enumPtr[model.RestartPolicy](body.RestartPolicy),
				RestartMax:       body.RestartMax,
				RestartWindowSec: body.RestartWindowSec,
				Flags:            body.Flags,
				ExtraFlags:       body.ExtraFlags,
			})
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := WriteJSON(w, http.StatusOK, PatchInstanceResponse{
				Instance: instanceDTO(v),
				Warnings: warningDTOs(v.Warnings),
			}); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status:      http.StatusOK,
			Description: "The updated instance, including `restart_required`.",
			Body:        PatchInstanceResponse{},
		},
		Errors: []Response{
			{
				Status:      http.StatusNotFound,
				Description: "No instance has this id.",
				Codes:       []model.ErrorCode{CodeNotFound},
			},
			{
				Status: http.StatusConflict,
				Description: "The instance was edited by someone else, or the requested name " +
					"is taken by another live instance.",
				Codes: []model.ErrorCode{
					model.CodeConflictGeneration,
					model.CodeInstanceNameTaken,
				},
			},
			{
				Status:      http.StatusUnprocessableEntity,
				Description: "The edited configuration was refused; see createInstance for the codes.",
				Codes: []model.ErrorCode{
					model.CodeInstanceNameInvalid,
					model.CodePortUnavailable,
					model.CodeDraftVocabMismatch,
					model.CodeNGLAutoConflict,
					model.CodeExtraFlagForbidden,
					model.CodeModelMissing,
					model.CodeBadFlags,
				},
			},
		},
	}
}

func (a *API) deleteInstanceRoute() Route {
	return Route{
		Method:      http.MethodDelete,
		Pattern:     BasePath + "/instances/{id}",
		Auth:        AuthSession,
		OperationID: "deleteInstance",
		Summary: "Soft delete: stop and disable the unit, close the listener, stamp " +
			"deleted_at, keep every row. ?purge=true is the explicit hard delete.",
		Tag: "instances",
		Query: []QueryParam{
			{
				Name: "purge",
				Description: "Hard delete: the row and all of its history and accounting " +
					"cascade away. That history is the one thing in this system that cannot " +
					"be recomputed.",
				Type: "boolean",
			},
			{
				Name:        "keep_tokens",
				Description: "Leave this instance's `token_instances` scope rows in place.",
				Type:        "boolean",
			},
		},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.instances()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			result, err := svc.Delete(r.Context(), r.PathValue("id"), instances.DeleteParams{
				Purge:      queryBool(r, "purge"),
				KeepTokens: queryBool(r, "keep_tokens"),
			})
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			hints := result.Hints
			if hints == nil {
				hints = []string{}
			}
			if err := WriteJSON(w, http.StatusAccepted, DeleteInstanceResponse{
				Purged: result.Purged,
				Hints:  hints,
			}); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status: http.StatusAccepted,
			Description: "The instance was deleted. `hints` carries any command the daemon " +
				"could not run itself.",
			Body: DeleteInstanceResponse{},
		},
		Errors: []Response{{
			Status:      http.StatusNotFound,
			Description: "No instance has this id.",
			Codes:       []model.ErrorCode{CodeNotFound},
		}},
	}
}

func (a *API) instances() (InstanceService, error) {
	if a.cfg.Instances == nil {
		return nil, Errorf(http.StatusServiceUnavailable, CodeInternalError,
			"this daemon was built without an instance service")
	}
	return a.cfg.Instances, nil
}

func instanceDTO(v instances.View) InstanceDTO {
	return InstanceDTO{
		ID:               v.ID,
		Name:             v.Name,
		DisplayName:      v.DisplayName,
		Description:      v.Description,
		ModelID:          v.ModelID,
		MmprojModelID:    v.MmprojModelID,
		DraftModelID:     v.DraftModelID,
		PublicPort:       v.PublicPort,
		InternalPort:     v.InternalPort,
		AuthMode:         string(v.AuthMode),
		Autostart:        v.Autostart,
		RestartPolicy:    string(v.RestartPolicy),
		RestartMax:       v.RestartMax,
		RestartWindowSec: v.RestartWindowSec,
		Flags:            v.Flags,
		ExtraFlags:       v.ExtraFlags,
		ConfigHash:       v.ConfigHash,
		DesiredState:     string(v.DesiredState),
		DraftValidation:  string(v.DraftValidation),
		UnitName:         v.UnitName,
		Generation:       v.Generation,
		CreatedAt:        Time(v.CreatedAt),
		UpdatedAt:        Time(v.UpdatedAt),
		DeletedAt:        TimePtr(v.DeletedAt),
		Status:           instanceStatusDTO(v.Status),
		RestartRequired:  v.Derived.RestartRequired,
		StaleVersion:     v.Derived.StaleVersion,
		Inhibited:        v.Derived.Inhibited,
		InhibitReason:    enumString(v.Derived.InhibitReason),
		DraftUnverified:  v.Derived.DraftUnverified,
	}
}

func instanceStatusDTO(st model.InstanceStatus) InstanceStatusDTO {
	return InstanceStatusDTO{
		State:             string(st.State),
		SystemdActive:     st.SystemdActive,
		SystemdSub:        st.SystemdSub,
		SystemdResult:     st.SystemdResult,
		MainPID:           st.MainPID,
		ExeVersionID:      st.ExeVersionID,
		AppliedConfigHash: st.AppliedConfigHash,
		ReadyAt:           TimePtr(st.ReadyAt),
		LastChangeAt:      Time(st.LastChangeAt),
		LastHealthAt:      TimePtr(st.LastHealthAt),
		HealthCode:        st.HealthCode,
		SlotsTotal:        st.SlotsTotal,
		SlotsBusy:         st.SlotsBusy,
		CtxSize:           st.CtxSize,
		RequestsServed:    st.RequestsServed,
		RSSBytes:          st.RSSBytes,
		VRAMBytes:         st.VRAMBytes,
		GPUAttribution:    string(st.GPUAttribution),
		LastExitCode:      st.LastExitCode,
		LastError:         st.LastError,
	}
}

func instanceDetailDTO(v instances.View) InstanceDetailDTO {
	starts := make([]InstanceStartDTO, 0, len(v.Starts))
	for _, r := range v.Starts {
		starts = append(starts, instanceStartDTO(r))
	}
	unknown := v.UnknownFlags
	if unknown == nil {
		unknown = []string{}
	}
	return InstanceDetailDTO{
		Instance:        instanceDTO(v),
		Argv:            v.Argv,
		ActiveVersionID: v.ActiveVersionID,
		UnknownFlags:    unknown,
		Starts:          starts,
		Warnings:        warningDTOs(v.Warnings),
	}
}

func instanceStartDTO(r model.InstanceStart) InstanceStartDTO {
	return InstanceStartDTO{
		ID:                  r.ID,
		At:                  Time(r.At),
		Trigger:             string(r.Trigger),
		ConfigHash:          r.ConfigHash,
		EffectiveConfigHash: r.EffectiveConfigHash,
		Override:            jsonObject(r.OverrideJSON),
		Argv:                jsonStrings(r.ArgvJSON),
		LlamacppVersionID:   r.LlamacppVersionID,
		ReadyAt:             TimePtr(r.ReadyAt),
		Outcome:             enumString(r.Outcome),
		ExitCode:            r.ExitCode,
		ErrorCode:           r.ErrorCode,
		ErrorMessage:        r.ErrorMessage,
		Detail:              jsonObject(r.DetailJSON),
		EndedAt:             TimePtr(r.EndedAt),
	}
}

func warningDTOs(warnings []model.Warning) []WarningDTO {
	out := make([]WarningDTO, 0, len(warnings))
	for _, w := range warnings {
		out = append(out, WarningDTO{Code: string(w.Code), Message: w.Message, Details: w.Details})
	}
	return out
}

// queryBool reads a `?flag=true` parameter. Anything strconv.ParseBool rejects
// is false rather than an error: a delete that failed because `?purge=yes` did
// not parse would be a worse answer than a delete that was not a purge.
func queryBool(r *http.Request, name string) bool {
	v, err := strconv.ParseBool(r.URL.Query().Get(name))
	return err == nil && v
}

// clearable turns the wire's "empty string clears it" into the service's
// explicit two-level pointer, where the outer nil means "leave it alone" and
// the inner nil means "set it to NULL".
func clearable(v *string) **string {
	if v == nil {
		return nil
	}
	if *v == "" {
		var null *string
		return &null
	}
	return &v
}

// enumPtr narrows an optional wire string into an optional domain enum. The
// value is NOT validated here — the service owns that, and duplicating the
// check would let the two disagree about what is legal.
func enumPtr[T ~string](v *string) *T {
	if v == nil {
		return nil
	}
	out := T(*v)
	return &out
}

// enumString widens an optional domain enum for the wire.
func enumString[T ~string](v *T) *string {
	if v == nil {
		return nil
	}
	out := string(*v)
	return &out
}

func stringOr(v *string, def string) string {
	if v == nil {
		return def
	}
	return *v
}

func flagsOf(v *model.FlagSet) model.FlagSet {
	if v == nil {
		return model.FlagSet{}
	}
	return *v
}

// jsonObject decodes a stored JSON column for display. A column that does not
// parse comes back nil rather than failing the response: the row is history, and
// a listing that 500s because one old row is malformed is worse than one field
// reading null.
func jsonObject(raw *string) map[string]any {
	if raw == nil {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(*raw), &out); err != nil {
		return nil
	}
	return out
}

func jsonStrings(raw *string) []string {
	if raw == nil {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(*raw), &out); err != nil {
		return nil
	}
	return out
}
