package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/jlbyh2o/llamaman/internal/fit"
	"github.com/jlbyh2o/llamaman/internal/instances"
	"github.com/jlbyh2o/llamaman/internal/model"
)

// The supervision half of DESIGN section 3.10, plus section 3.10b's safe start,
// section 3.11's presets and `GET /ports/suggest`.
//
// These are the rows internal/api/instances.go deliberately left out while the
// subsystems behind them were being built: "each needs the supervisor, the
// gateway or the fit calculator, and a route registered here appears in
// api/openapi.json, where a 501 behind it would be a promise in the contract the
// daemon cannot keep. They join the registry with the subsystems they call."
// They are joining now.
//
// Two properties of the group are worth stating once rather than at each route.
//
// **Nothing here calls systemd directly, and start/stop/restart do not call it
// at all.** They write the DESIRED axis and return `202`; the supervisor reads
// `(desired, actual)` on its next pass and takes at most one corrective action.
// That split is what makes an instance which crashed while the daemon was down
// get restarted when the daemon returns — a handler that shelled out to
// `systemctl start` and returned success would have no such property.
//
// **Every refusal names the manual command.** Section 11.1a's degraded modes are
// modes this daemon SERVES in: F9 (a denied polkit grant) and F10 (no reachable
// manager) are not errors to be swallowed or crashed on, and a 409 whose
// `details.hints` carries `sudo systemctl start llamaman-instance@qwen.service`
// is the difference between a broken product and a documented one.

// InstanceControlService is the supervision surface. internal/app satisfies it:
// the calls need the instance service, the supervisor, the systemd controller,
// the journal reader and the fit calculator at once, and the composition root is
// the only place that holds all five.
type InstanceControlService interface {
	// The five control verbs. Each writes desired state (and, for a safe start,
	// the transient override of section 3.10b) and answers 202.
	Start(ctx context.Context, id string) (InstanceControlDTO, error)
	Stop(ctx context.Context, id string, drainSec int) (InstanceControlDTO, error)
	RestartInstance(ctx context.Context, id string) (InstanceControlDTO, error)
	SafeStart(ctx context.Context, id string) (InstanceControlDTO, error)
	ResetFailed(ctx context.Context, id string) (InstanceControlDTO, error)

	// SetAutostart enables or disables the unit file and NOTHING else — it
	// never starts or stops.
	SetAutostart(ctx context.Context, id string, enabled bool) (AutostartDTO, error)
	// PinNGL rewrites `n_gpu_layers` from `auto` to a count using the current
	// fit estimate (D51). It is an explicit config edit and bumps `generation`.
	PinNGL(ctx context.Context, id string) (PinNGLDTO, error)

	InstanceStatus(ctx context.Context, id string) (InstanceLiveStatusDTO, error)
	Usage(ctx context.Context, id, from, to string) (InstanceUsageDTO, error)
	Starts(ctx context.Context, id string, limit int) ([]InstanceStartDTO, error)
	Command(ctx context.Context, id string) (InstanceCommandDTO, error)
	Logs(ctx context.Context, id string, lines int, since string) ([]JournalLineDTO, error)
	// Validate is the dry run. It never answers 422 — it RETURNS the verdict,
	// including `draft_validation:"mismatch"`, because a dry run that refused
	// would make the form unable to show what is wrong (section 3.10a).
	Validate(ctx context.Context, body ValidateInstanceRequest) (ValidateInstanceDTO, error)
	// SuggestPort is `GET /api/v1/ports/suggest`.
	SuggestPort(ctx context.Context, kind string) (PortSuggestionDTO, error)
	// Proxy forwards `props`, `slots` or `metrics` to llama-server and returns
	// its answer wrapped in UpstreamBodyDTO.
	Proxy(ctx context.Context, id, what string) (UpstreamBodyDTO, error)
}

// PresetService is section 3.11. internal/app satisfies it over the store.
type PresetService interface {
	Presets(ctx context.Context) ([]PresetDTO, error)
	Preset(ctx context.Context, id string) (PresetDTO, error)
	CreatePreset(ctx context.Context, in PresetInput) (PresetDTO, error)
	PatchPreset(ctx context.Context, id string, in PresetInput) (PresetDTO, error)
	DeletePreset(ctx context.Context, id string) error
	PresetFromInstance(ctx context.Context, instanceID string, in PresetInput) (PresetDTO, error)
	ApplyPreset(ctx context.Context, id string, instanceIDs, overwrite []string) (PresetApplyDTO, error)
}

/* -- DTOs ------------------------------------------------------------------- */

// InstanceControlDTO is the 202 of the five control verbs.
type InstanceControlDTO struct {
	// JobID is null: none of these is a job. They move the desired axis and the
	// supervisor acts, which is a different mechanism from the job queue and is
	// reported as one rather than dressed up as a job with no progress.
	JobID *string `json:"job_id"`
	// DesiredState is what the instance is now wanted to be.
	DesiredState string `json:"desired_state"`
	// Hint is section 2.8's one-word note: `will_start_at_boot` on stopping an
	// autostart instance, `start_now` on enabling autostart while stopped.
	Hint *string `json:"hint"`
	// Hints carries the manual commands for anything the daemon could not do
	// itself, exactly as the delete response does.
	Hints []string `json:"hints"`
}

// AutostartDTO is the body of `PUT /api/v1/instances/{id}/autostart`.
type AutostartDTO struct {
	Enabled bool     `json:"enabled"`
	Hint    *string  `json:"hint"`
	Hints   []string `json:"hints"`
}

// PinNGLDTO is the body of `POST /api/v1/instances/{id}/pin-ngl` (D51).
type PinNGLDTO struct {
	Instance InstanceDTO `json:"instance"`
	// PinnedLayers is the count `auto` resolved to, which is the number the UI
	// echoes back so the user can see what was decided on their behalf.
	PinnedLayers int          `json:"pinned_layers"`
	Warnings     []WarningDTO `json:"warnings"`
}

// InstanceLiveStatusDTO is `GET /api/v1/instances/{id}/status`: the stored
// status row plus what the unit and the health probe say right now.
type InstanceLiveStatusDTO struct {
	Status InstanceStatusDTO `json:"status"`
	// Unit is what the manager reports, null in the F10 degraded mode — never a
	// fabricated "inactive", which would read as an answer.
	Unit *UnitLiveDTO `json:"unit"`
	// HealthURL is the endpoint the supervisor polls, so the detail screen can
	// show what it is checking.
	HealthURL string `json:"health_url"`
}

// UnitLiveDTO is one live read of a unit's properties (section 5.3).
type UnitLiveDTO struct {
	ActiveState string  `json:"active_state"`
	SubState    string  `json:"sub_state"`
	Result      string  `json:"result"`
	MainPID     *int64  `json:"main_pid"`
	NRestarts   int64   `json:"n_restarts"`
	MemoryBytes *int64  `json:"memory_bytes"`
	SinceAt     *string `json:"since_at"`
}

// InstanceUsageDayDTO is one row of `instance_usage_daily` (section 2.9).
type InstanceUsageDayDTO struct {
	Day      string `json:"day"`
	AuthMode string `json:"auth_mode"`
	Requests int64  `json:"requests"`
	Errors   int64  `json:"errors"`
	BytesIn  int64  `json:"bytes_in"`
	BytesOut int64  `json:"bytes_out"`
	// DurationMS is the summed upstream time. Milliseconds are the storage form
	// and the wire form; the `_ms` suffix is the documentation.
	DurationMS int64 `json:"duration_ms"`
	// Source labels which side of section 2.9 this row came from: the gateway's
	// own ledger, or llama-server's `/metrics`.
	Source string `json:"source"`
}

// InstanceUsageDTO is `GET /api/v1/instances/{id}/usage`.
type InstanceUsageDTO struct {
	Items []InstanceUsageDayDTO `json:"items"`
	Total int                   `json:"total"`
	// RequestsServed is llama-server's own counter, NULL when the metrics
	// endpoint is off. The UI must say "metrics disabled" rather than show a
	// zero that reads as an answer (section 2.9).
	RequestsServed *int64 `json:"requests_served"`
}

// InstanceCommandDTO is `GET /api/v1/instances/{id}/command` — copyable and
// auditable, which is the whole reason it exists as an endpoint rather than as
// a field nobody can find.
type InstanceCommandDTO struct {
	Argv []string          `json:"argv"`
	Env  map[string]string `json:"env"`
	Unit string            `json:"unit"`
	// UnknownFlags is section 5.7's flag-churn guard against the active build's
	// help capture; empty also means the check could not run.
	UnknownFlags []string `json:"unknown_flags"`
}

// ValidateInstanceRequest is the body of `POST /api/v1/instances/validate`.
//
// It is the create body minus the identity fields: a dry run renders argv and
// checks conflicts, and a name or a port would only make it refuse things that
// are not what it was asked about.
type ValidateInstanceRequest struct {
	// InstanceID scopes port and name checks to "everything except this row",
	// which is what an EDIT needs — without it, editing an instance reports its
	// own ports as taken.
	InstanceID string `json:"instance_id,omitempty"`

	ModelID       string  `json:"model_id"`
	MmprojModelID *string `json:"mmproj_model_id,omitempty"`
	DraftModelID  *string `json:"draft_model_id,omitempty"`

	Flags      *model.FlagSet `json:"flags,omitempty"`
	ExtraFlags *string        `json:"extra_flags,omitempty"`
}

// ValidateInstanceDTO is the dry run's answer.
type ValidateInstanceDTO struct {
	Argv         []string `json:"argv"`
	UnknownFlags []string `json:"unknown_flags"`
	// DraftValidation is section 3.10a's three-valued result: `ok`, `deferred`
	// or `mismatch`. A dry run reports `mismatch` rather than refusing with it.
	DraftValidation string       `json:"draft_validation"`
	Warnings        []WarningDTO `json:"warnings"`
	// Fit is the estimate for this configuration, null when no model is named
	// or its GGUF has not been parsed yet — which is the ordinary case for a
	// model that is still downloading, not a failure.
	Fit *FitReportDTO `json:"fit"`
}

// UpstreamBodyDTO wraps what llama-server answered on one of the three
// pass-through routes.
//
// The wrapper exists so this API does not have to claim a shape it does not own.
// llama.cpp ships nightlies; `/props` and `/slots` change with them, and a
// generated client typed against today's fields would break on a build the user
// installed themselves. So the JSON travels as a free-form value the client
// narrows, and the document says exactly that rather than describing a string —
// which is what a raw byte slice would have generated, and would have been a
// straightforward lie about the wire.
type UpstreamBodyDTO struct {
	// JSON is the parsed body when upstream answered with JSON: an object for
	// `/props`, an array for `/slots`.
	JSON any `json:"json"`
	// Text is the body when upstream answered with something that is not JSON,
	// which is `/metrics` — Prometheus exposition format, not an object.
	Text *string `json:"text"`
}

// PortSuggestionDTO is `GET /api/v1/ports/suggest`.
type PortSuggestionDTO struct {
	Port int    `json:"port"`
	Kind string `json:"kind"`
}

// PresetDTO is one `flag_presets` row (sections 2.8, 3.11).
type PresetDTO struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Description *string `json:"description"`
	// Flags is the FlagSet itself, for the same reason InstanceDTO embeds one:
	// `flags_json` is one JSON column typed in Go as that struct, so a new
	// upstream flag is a struct field rather than a wire change.
	Flags      model.FlagSet `json:"flags"`
	ExtraFlags string        `json:"extra_flags"`
	// Builtin marks a shipped preset: appliable like any other, and never
	// editable or deletable, so an overwritten favorite can always be recovered.
	Builtin   bool   `json:"builtin"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// PresetInput is the body of the preset writes. Every field is optional on a
// PATCH; `name` and `flags` are required on a create.
type PresetInput struct {
	Name        *string        `json:"name,omitempty"`
	Description *string        `json:"description,omitempty"`
	Flags       *model.FlagSet `json:"flags,omitempty"`
	ExtraFlags  *string        `json:"extra_flags,omitempty"`
}

// PresetFromInstanceRequest is the body of `POST /api/v1/presets/from-instance`:
// which instance to capture, plus the optional name and description to file it
// under.
type PresetFromInstanceRequest struct {
	InstanceID string `json:"instance_id"`
	PresetInput
}

// ApplyPresetRequest is the body of `POST /api/v1/presets/{id}/apply`.
type ApplyPresetRequest struct {
	InstanceIDs []string `json:"instance_ids"`
	// Overwrite names the keys the preset is allowed to replace. An empty list
	// means every key the preset sets — which is the destructive reading, so the
	// UI always sends the list the user checked.
	Overwrite []string `json:"overwrite"`
}

// PresetApplyDTO is the per-instance diff section 3.11 asks for.
type PresetApplyDTO struct {
	Items []PresetApplyEntryDTO `json:"items"`
	Total int                   `json:"total"`
}

// PresetApplyEntryDTO is one instance's outcome.
type PresetApplyEntryDTO struct {
	InstanceID string `json:"instance_id"`
	Name       string `json:"name"`
	// Changed names the keys that actually moved on this instance.
	Changed []string `json:"changed"`
	// RestartRequired is the instance's derived flag after the write.
	RestartRequired bool `json:"restart_required"`
	// Error is set when this instance alone refused — a per-row failure never
	// fails the whole apply, because a preset over five instances that stops at
	// the second leaves the user with no way to know which two moved.
	Error *ErrorDetailDTO `json:"error"`
}

// ErrorDetailDTO is a per-row failure inside a 200: the same `{code, message}`
// pair the error envelope carries, in a body that succeeded.
type ErrorDetailDTO struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

/* -- routes ----------------------------------------------------------------- */

func (a *API) instanceControlRoutes() []Route {
	verbs := []struct {
		path, op, summary string
		call              func(svc InstanceControlService, r *http.Request, id string) (InstanceControlDTO, error)
	}{
		{"start", "startInstance",
			"Set desired_state=running and stamp pending_trigger='user'. The supervisor starts it.",
			func(svc InstanceControlService, r *http.Request, id string) (InstanceControlDTO, error) {
				return svc.Start(r.Context(), id)
			}},
		{"stop", "stopInstance",
			"Set desired_state=stopped. Answers hint=will_start_at_boot when autostart is on.",
			func(svc InstanceControlService, r *http.Request, id string) (InstanceControlDTO, error) {
				return svc.Stop(r.Context(), id, int(queryInt64(r, "drain_sec", 30)))
			}},
		{"restart", "restartInstance",
			"Stop and start in one request.",
			func(svc InstanceControlService, r *http.Request, id string) (InstanceControlDTO, error) {
				return svc.RestartInstance(r.Context(), id)
			}},
		{"safe-start", "safeStartInstance",
			"One-shot start with -ngl 0 -c 2048 to isolate GPU from model problems. Never persisted.",
			func(svc InstanceControlService, r *http.Request, id string) (InstanceControlDTO, error) {
				return svc.SafeStart(r.Context(), id)
			}},
		{"reset-failed", "resetFailedInstance",
			"Clear the crash-loop latch and the backoff, start the crash-loop window over, and " +
				"call systemd ResetFailed.",
			func(svc InstanceControlService, r *http.Request, id string) (InstanceControlDTO, error) {
				return svc.ResetFailed(r.Context(), id)
			}},
	}

	out := make([]Route, 0, len(verbs)+12)
	for _, v := range verbs {
		call := v.call
		rt := Route{
			Method:      http.MethodPost,
			Pattern:     BasePath + "/instances/{id}/" + v.path,
			Auth:        AuthSession,
			OperationID: v.op,
			Summary:     v.summary,
			Tag:         "instances",
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				svc, err := a.instanceControl()
				if err != nil {
					WriteError(w, r, a.log, err)
					return
				}
				out, err := call(svc, r, r.PathValue("id"))
				if err != nil {
					WriteError(w, r, a.log, err)
					return
				}
				if err := WriteJSON(w, http.StatusAccepted, out); err != nil {
					WriteError(w, r, a.log, err)
				}
			}),
			Success: Response{Status: http.StatusAccepted,
				Description: "The desired state was written; the supervisor acts on its next pass.",
				Body:        InstanceControlDTO{}},
			Errors: []Response{
				{Status: http.StatusNotFound, Description: "No instance has this id.",
					Codes: []model.ErrorCode{CodeNotFound}},
				{Status: http.StatusConflict,
					Description: "No service manager is reachable on this host (F10), so there is " +
						"nothing to ask. `details.hints` carries the manual systemctl command.",
					Codes: []model.ErrorCode{model.CodeSystemdUnavailable, model.CodeSystemdDenied}},
			},
		}
		if v.path == "stop" {
			rt.Query = []QueryParam{{Name: "drain_sec",
				Description: "How long the gateway drains in-flight requests before the stop.",
				Type:        "integer"}}
		}
		out = append(out, rt)
	}

	out = append(out,
		a.autostartRoute(),
		a.pinNGLRoute(),
		a.instanceStatusRoute(),
		a.instanceUsageRoute(),
		a.instanceStartsRoute(),
		a.instanceCommandRoute(),
		a.instanceLogsRoute(),
		a.validateInstanceRoute(),
		a.suggestPortRoute(),
	)
	out = append(out, a.instanceProxyRoutes()...)
	return out
}

func (a *API) instanceControl() (InstanceControlService, error) {
	if a.cfg.InstanceControl == nil {
		return nil, Errorf(http.StatusServiceUnavailable, CodeInternalError,
			"this daemon was built without instance supervision")
	}
	return a.cfg.InstanceControl, nil
}

func (a *API) autostartRoute() Route {
	type body struct {
		Enabled bool `json:"enabled"`
	}
	return Route{
		Method:      http.MethodPut,
		Pattern:     BasePath + "/instances/{id}/autostart",
		Auth:        AuthSession,
		OperationID: "setInstanceAutostart",
		Summary: "Enable or disable the unit file, and nothing else — it never starts or stops. " +
			"Answers hint=start_now when enabling on a stopped instance.",
		Tag:         "instances",
		RequestBody: body{},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.instanceControl()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			var in body
			if err := DecodeJSON(w, r, &in); err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			out, err := svc.SetAutostart(r.Context(), r.PathValue("id"), in.Enabled)
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := WriteJSON(w, http.StatusOK, out); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{Status: http.StatusOK, Description: "The unit's enablement.",
			Body: AutostartDTO{}},
		Errors: []Response{
			{Status: http.StatusNotFound, Description: "No instance has this id.",
				Codes: []model.ErrorCode{CodeNotFound}},
			{Status: http.StatusConflict,
				Description: "The manage-unit-files grant was withheld, or no manager is reachable. " +
					"`details.hints` carries `sudo systemctl enable llamaman-instance@<name>.service`.",
				Codes: []model.ErrorCode{
					model.CodeAutostartUnavailable, model.CodeSystemdUnavailable}},
		},
	}
}

func (a *API) pinNGLRoute() Route {
	return Route{
		Method:      http.MethodPost,
		Pattern:     BasePath + "/instances/{id}/pin-ngl",
		Auth:        AuthSession,
		OperationID: "pinInstanceNGL",
		Summary: "Rewrite n_gpu_layers from auto to a count using the current fit estimate. An " +
			"explicit config edit: it bumps generation and config_hash.",
		Tag: "instances",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.instanceControl()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			out, err := svc.PinNGL(r.Context(), r.PathValue("id"))
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := WriteJSON(w, http.StatusOK, out); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{Status: http.StatusOK, Description: "The updated instance.",
			Body: PinNGLDTO{}},
		Errors: []Response{
			{Status: http.StatusNotFound, Description: "No instance has this id.",
				Codes: []model.ErrorCode{CodeNotFound}},
			{Status: http.StatusUnprocessableEntity,
				Description: "This instance's n_gpu_layers is not `auto`, or no estimate can be " +
					"made because the model's GGUF has not been parsed yet.",
				Codes: []model.ErrorCode{model.CodeBadFlags, model.CodeModelMissing}},
		},
	}
}

func (a *API) instanceStatusRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/instances/{id}/status",
		Auth:        AuthSession,
		OperationID: "getInstanceStatus",
		Summary:     "The instance_status row plus a live read of the unit and the health probe.",
		Tag:         "instances",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.instanceControl()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			out, err := svc.InstanceStatus(r.Context(), r.PathValue("id"))
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := WriteJSON(w, http.StatusOK, out); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{Status: http.StatusOK, Description: "Stored and observed status.",
			Body: InstanceLiveStatusDTO{}},
		Errors: []Response{{Status: http.StatusNotFound,
			Description: "No instance has this id.", Codes: []model.ErrorCode{CodeNotFound}}},
	}
}

func (a *API) instanceUsageRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/instances/{id}/usage",
		Auth:        AuthSession,
		OperationID: "getInstanceUsage",
		Summary: "instance_usage_daily rows for both auth modes, plus llama-server's own counter, " +
			"each labeled with its source.",
		Tag: "instances",
		Query: []QueryParam{
			{Name: "from", Description: "Inclusive first day, as YYYY-MM-DD."},
			{Name: "to", Description: "Inclusive last day, as YYYY-MM-DD."},
		},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.instanceControl()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			q := r.URL.Query()
			out, err := svc.Usage(r.Context(), r.PathValue("id"), q.Get("from"), q.Get("to"))
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if out.Items == nil {
				out.Items = []InstanceUsageDayDTO{}
			}
			if err := WriteJSON(w, http.StatusOK, out); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{Status: http.StatusOK, Description: "The daily rollups.",
			Body: InstanceUsageDTO{}},
		Errors: []Response{{Status: http.StatusNotFound,
			Description: "No instance has this id.", Codes: []model.ErrorCode{CodeNotFound}}},
	}
}

func (a *API) instanceStartsRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/instances/{id}/starts",
		Auth:        AuthSession,
		OperationID: "listInstanceStarts",
		Summary: "The start ledger: trigger, outcome, exit code, detail and argv — including " +
			"preflight failures that never reached execve.",
		Tag:   "instances",
		Query: []QueryParam{{Name: "limit", Description: "How many rows.", Type: "integer"}},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.instanceControl()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			rows, err := svc.Starts(r.Context(), r.PathValue("id"), int(queryInt64(r, "limit", 100)))
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := WriteJSON(w, http.StatusOK, NewList(rows, len(rows), nil)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{Status: http.StatusOK, Description: "The ledger, newest first.",
			Body: List[InstanceStartDTO]{}},
		Errors: []Response{{Status: http.StatusNotFound,
			Description: "No instance has this id.", Codes: []model.ErrorCode{CodeNotFound}}},
	}
}

func (a *API) instanceCommandRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/instances/{id}/command",
		Auth:        AuthSession,
		OperationID: "getInstanceCommand",
		Summary:     "The argv, environment and unit name — copyable and auditable.",
		Tag:         "instances",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.instanceControl()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			out, err := svc.Command(r.Context(), r.PathValue("id"))
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := WriteJSON(w, http.StatusOK, out); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{Status: http.StatusOK, Description: "The rendered command.",
			Body: InstanceCommandDTO{}},
		Errors: []Response{{Status: http.StatusNotFound,
			Description: "No instance has this id.", Codes: []model.ErrorCode{CodeNotFound}}},
	}
}

func (a *API) instanceLogsRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/instances/{id}/logs",
		Auth:        AuthSession,
		OperationID: "getInstanceLogs",
		Summary:     "The unit's journal. SSE when Accept: text/event-stream.",
		Tag:         "instances",
		Query: []QueryParam{
			{Name: "lines", Description: "How many entries.", Type: "integer"},
			{Name: "since", Description: "A journald cursor or timestamp to resume from."},
			{Name: "follow", Description: "Ask for the live tail.", Type: "boolean"},
		},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.instanceControl()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			lines, err := svc.Logs(r.Context(), r.PathValue("id"),
				int(queryInt64(r, "lines", 500)), r.URL.Query().Get("since"))
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := WriteJSON(w, http.StatusOK, NewList(lines, len(lines), nil)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{Status: http.StatusOK, Description: "The requested entries.",
			Body: List[JournalLineDTO]{}, AltMediaTypes: []string{"text/event-stream"}},
		Errors: []Response{
			{Status: http.StatusNotFound, Description: "No instance has this id.",
				Codes: []model.ErrorCode{CodeNotFound}},
			{Status: http.StatusConflict,
				Description: "This daemon's identity cannot read the journal (D77).",
				Codes:       []model.ErrorCode{model.CodeJournalUnavailable}},
		},
	}
}

func (a *API) validateInstanceRoute() Route {
	return Route{
		Method:      http.MethodPost,
		Pattern:     BasePath + "/instances/validate",
		Auth:        AuthSession,
		OperationID: "validateInstance",
		Summary: "Dry-run a FlagSet: render argv, check conflicts, return the three-valued draft " +
			"verdict and a fit estimate. Never a 422 — it reports rather than refuses.",
		Tag:         "instances",
		RequestBody: ValidateInstanceRequest{},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.instanceControl()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			var body ValidateInstanceRequest
			if err := DecodeJSON(w, r, &body); err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			out, err := svc.Validate(r.Context(), body)
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := WriteJSON(w, http.StatusOK, out); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{Status: http.StatusOK, Description: "The dry run's verdict.",
			Body: ValidateInstanceDTO{}},
	}
}

func (a *API) suggestPortRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/ports/suggest",
		Auth:        AuthSession,
		OperationID: "suggestPort",
		Summary:     "The next free port not in the database and not bound.",
		Tag:         "instances",
		Query: []QueryParam{{Name: "kind", Required: true,
			Description: "Which pool to draw from.", Enum: []string{"public", "internal"}}},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.instanceControl()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			kind := strings.TrimSpace(r.URL.Query().Get("kind"))
			if kind == "" {
				kind = "public"
			}
			out, err := svc.SuggestPort(r.Context(), kind)
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := WriteJSON(w, http.StatusOK, out); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{Status: http.StatusOK, Description: "A port that was free when asked.",
			Body: PortSuggestionDTO{}},
		Errors: []Response{{Status: http.StatusUnprocessableEntity,
			Description: "`kind` is not one of public, internal, or the pool is exhausted.",
			Codes:       []model.ErrorCode{model.CodePortUnavailable}}},
	}
}

// instanceProxyRoutes are section 3.10's three llama-server passthroughs. They
// are three routes rather than one `{what}` wildcard so the generated document
// names what each returns, and so a typo in a client's URL is a 404 rather than
// a proxied request to a path llama-server does not serve either.
func (a *API) instanceProxyRoutes() []Route {
	kinds := []struct{ what, op, summary string }{
		{"props", "getInstanceProps", "llama-server's /props, proxied."},
		{"slots", "getInstanceSlots", "llama-server's /slots, proxied."},
		{"metrics", "getInstanceMetrics", "llama-server's /metrics, proxied."},
	}
	out := make([]Route, 0, len(kinds))
	for _, k := range kinds {
		what := k.what
		out = append(out, Route{
			Method:      http.MethodGet,
			Pattern:     BasePath + "/instances/{id}/" + what,
			Auth:        AuthSession,
			OperationID: k.op,
			Summary:     k.summary,
			Tag:         "instances",
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				svc, err := a.instanceControl()
				if err != nil {
					WriteError(w, r, a.log, err)
					return
				}
				out, err := svc.Proxy(r.Context(), r.PathValue("id"), what)
				if err != nil {
					WriteError(w, r, a.log, err)
					return
				}
				if err := WriteJSON(w, http.StatusOK, out); err != nil {
					WriteError(w, r, a.log, err)
				}
			}),
			// The body inside the wrapper is llama-server's, not ours: it changes
			// upstream, and documenting a shape we do not own would be a promise
			// this API cannot keep.
			Success: Response{Status: http.StatusOK,
				Description: "The upstream answer, wrapped.", Body: UpstreamBodyDTO{}},
			Errors: []Response{
				{Status: http.StatusNotFound, Description: "No instance has this id.",
					Codes: []model.ErrorCode{CodeNotFound}},
				{Status: http.StatusConflict,
					Description: "The instance is not running, so there is no upstream to ask.",
					Codes:       []model.ErrorCode{model.CodeSystemdUnavailable}},
			},
		})
	}
	return out
}

/* -- presets ---------------------------------------------------------------- */

func (a *API) presets() (PresetService, error) {
	if a.cfg.Presets == nil {
		return nil, Errorf(http.StatusServiceUnavailable, CodeInternalError,
			"this daemon was built without a preset service")
	}
	return a.cfg.Presets, nil
}

func (a *API) presetRoutes() []Route {
	return []Route{
		{
			Method:      http.MethodGet,
			Pattern:     BasePath + "/presets",
			Auth:        AuthSession,
			OperationID: "listPresets",
			Summary:     "Every flag preset, built-ins first.",
			Tag:         "presets",
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				svc, err := a.presets()
				if err != nil {
					WriteError(w, r, a.log, err)
					return
				}
				rows, err := svc.Presets(r.Context())
				if err != nil {
					WriteError(w, r, a.log, err)
					return
				}
				if err := WriteJSON(w, http.StatusOK, NewList(rows, len(rows), nil)); err != nil {
					WriteError(w, r, a.log, err)
				}
			}),
			Success: Response{Status: http.StatusOK, Description: "The presets.",
				Body: List[PresetDTO]{}},
		},
		{
			Method:      http.MethodPost,
			Pattern:     BasePath + "/presets",
			Auth:        AuthSession,
			OperationID: "createPreset",
			Summary:     "Save a FlagSet and extra_flags under a name.",
			Tag:         "presets",
			RequestBody: PresetInput{},
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				svc, err := a.presets()
				if err != nil {
					WriteError(w, r, a.log, err)
					return
				}
				var in PresetInput
				if err := DecodeJSON(w, r, &in); err != nil {
					WriteError(w, r, a.log, err)
					return
				}
				out, err := svc.CreatePreset(r.Context(), in)
				if err != nil {
					WriteError(w, r, a.log, err)
					return
				}
				if err := WriteJSON(w, http.StatusCreated, out); err != nil {
					WriteError(w, r, a.log, err)
				}
			}),
			Success: Response{Status: http.StatusCreated, Description: "The saved preset.",
				Body: PresetDTO{}},
			Errors: []Response{
				{Status: http.StatusUnprocessableEntity,
					Description: "The name is empty, or the FlagSet fails section 5.7's rules.",
					Codes:       []model.ErrorCode{model.CodeBadFlags}},
				{Status: http.StatusConflict, Description: "Another preset already has this name.",
					Codes: []model.ErrorCode{model.CodeInstanceNameTaken}},
			},
		},
		{
			Method:      http.MethodGet,
			Pattern:     BasePath + "/presets/{id}",
			Auth:        AuthSession,
			OperationID: "getPreset",
			Summary:     "One preset.",
			Tag:         "presets",
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				svc, err := a.presets()
				if err != nil {
					WriteError(w, r, a.log, err)
					return
				}
				out, err := svc.Preset(r.Context(), r.PathValue("id"))
				if err != nil {
					WriteError(w, r, a.log, err)
					return
				}
				if err := WriteJSON(w, http.StatusOK, out); err != nil {
					WriteError(w, r, a.log, err)
				}
			}),
			Success: Response{Status: http.StatusOK, Description: "The preset.", Body: PresetDTO{}},
			Errors: []Response{{Status: http.StatusNotFound,
				Description: "No preset has this id.", Codes: []model.ErrorCode{CodeNotFound}}},
		},
		{
			Method:      http.MethodPatch,
			Pattern:     BasePath + "/presets/{id}",
			Auth:        AuthSession,
			OperationID: "patchPreset",
			Summary:     "Rename or re-tune a preset. Built-ins refuse.",
			Tag:         "presets",
			RequestBody: PresetInput{},
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				svc, err := a.presets()
				if err != nil {
					WriteError(w, r, a.log, err)
					return
				}
				var in PresetInput
				if err := DecodeJSON(w, r, &in); err != nil {
					WriteError(w, r, a.log, err)
					return
				}
				out, err := svc.PatchPreset(r.Context(), r.PathValue("id"), in)
				if err != nil {
					WriteError(w, r, a.log, err)
					return
				}
				if err := WriteJSON(w, http.StatusOK, out); err != nil {
					WriteError(w, r, a.log, err)
				}
			}),
			Success: Response{Status: http.StatusOK, Description: "The updated preset.",
				Body: PresetDTO{}},
			Errors: []Response{
				{Status: http.StatusNotFound, Description: "No preset has this id.",
					Codes: []model.ErrorCode{CodeNotFound}},
				{Status: http.StatusConflict,
					Description: "This is a built-in preset, or another preset has that name.",
					Codes: []model.ErrorCode{
						model.CodeInstanceNameTaken, model.CodeConflictGeneration}},
			},
		},
		{
			Method:      http.MethodDelete,
			Pattern:     BasePath + "/presets/{id}",
			Auth:        AuthSession,
			OperationID: "deletePreset",
			Summary:     "Remove a preset. Built-ins refuse.",
			Tag:         "presets",
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				svc, err := a.presets()
				if err != nil {
					WriteError(w, r, a.log, err)
					return
				}
				if err := svc.DeletePreset(r.Context(), r.PathValue("id")); err != nil {
					WriteError(w, r, a.log, err)
					return
				}
				WriteNoContent(w)
			}),
			Success: Response{Status: http.StatusNoContent, Description: "The preset is gone."},
			Errors: []Response{
				{Status: http.StatusNotFound, Description: "No preset has this id.",
					Codes: []model.ErrorCode{CodeNotFound}},
				{Status: http.StatusConflict, Description: "This is a built-in preset.",
					Codes: []model.ErrorCode{model.CodeConflictGeneration}},
			},
		},
		{
			Method:      http.MethodPost,
			Pattern:     BasePath + "/presets/from-instance",
			Auth:        AuthSession,
			OperationID: "createPresetFromInstance",
			// Section 3.11 spells this `POST /presets/from-instance/{id}`, and
			// that spelling cannot be served: net/http's ServeMux — which
			// section 3 mandates — reports it as a genuine conflict with
			// `POST /presets/{id}/apply`, because "/presets/from-instance/apply"
			// matches both and neither pattern is more specific. Between the
			// two, `{id}/apply` keeps the documented path: it is the one a user
			// reaches repeatedly, and the one whose id is a PRESET id like every
			// other `/presets/{id}` route. The capture takes its instance id in
			// the body instead, which is the same information by a different
			// route.
			Summary: "Capture an instance's FlagSet and extra_flags as a new preset. The " +
				"instance is named in the body rather than in the path.",
			Tag:         "presets",
			RequestBody: PresetFromInstanceRequest{},
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				svc, err := a.presets()
				if err != nil {
					WriteError(w, r, a.log, err)
					return
				}
				var in PresetFromInstanceRequest
				if err := DecodeJSON(w, r, &in); err != nil {
					WriteError(w, r, a.log, err)
					return
				}
				out, err := svc.PresetFromInstance(r.Context(), in.InstanceID, in.PresetInput)
				if err != nil {
					WriteError(w, r, a.log, err)
					return
				}
				if err := WriteJSON(w, http.StatusCreated, out); err != nil {
					WriteError(w, r, a.log, err)
				}
			}),
			Success: Response{Status: http.StatusCreated, Description: "The captured preset.",
				Body: PresetDTO{}},
			Errors: []Response{
				{Status: http.StatusNotFound, Description: "No instance has this id.",
					Codes: []model.ErrorCode{CodeNotFound}},
				{Status: http.StatusConflict, Description: "Another preset already has this name.",
					Codes: []model.ErrorCode{model.CodeInstanceNameTaken}},
			},
		},
		{
			Method:      http.MethodPost,
			Pattern:     BasePath + "/presets/{id}/apply",
			Auth:        AuthSession,
			OperationID: "applyPreset",
			Summary: "Apply a preset to instances, key by key, and report the per-instance diff " +
				"with each one's restart_required.",
			Tag:         "presets",
			RequestBody: ApplyPresetRequest{},
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				svc, err := a.presets()
				if err != nil {
					WriteError(w, r, a.log, err)
					return
				}
				var in ApplyPresetRequest
				if err := DecodeJSON(w, r, &in); err != nil {
					WriteError(w, r, a.log, err)
					return
				}
				out, err := svc.ApplyPreset(r.Context(), r.PathValue("id"),
					in.InstanceIDs, in.Overwrite)
				if err != nil {
					WriteError(w, r, a.log, err)
					return
				}
				if out.Items == nil {
					out.Items = []PresetApplyEntryDTO{}
				}
				if err := WriteJSON(w, http.StatusOK, out); err != nil {
					WriteError(w, r, a.log, err)
				}
			}),
			Success: Response{Status: http.StatusOK,
				Description: "One entry per instance. A per-row refusal is reported inside the " +
					"200 rather than failing the whole apply.",
				Body: PresetApplyDTO{}},
			Errors: []Response{{Status: http.StatusNotFound,
				Description: "No preset has this id.", Codes: []model.ErrorCode{CodeNotFound}}},
		},
	}
}

/* -- exported conversions --------------------------------------------------- */

// The four conversions below are the unexported ones this package already uses,
// exported for the composition root.
//
// internal/app implements InstanceControlService and PresetService, so it has to
// BUILD these DTOs — and the alternative to exporting the converters is either a
// second implementation of each in internal/app (two mappings of one shape, the
// exact drift the generated document exists to prevent) or moving the DTOs out
// of this package (which would leave the route registry describing types it does
// not own). Exporting the function is the smallest of the three.

// InstanceDTOOf renders one instance view.
func InstanceDTOOf(v instances.View) InstanceDTO { return instanceDTO(v) }

// InstanceStatusDTOOf renders one observed-status row.
func InstanceStatusDTOOf(st model.InstanceStatus) InstanceStatusDTO { return instanceStatusDTO(st) }

// InstanceStartDTOs renders a start ledger, newest first, normalizing nil to an
// empty slice so the wire form is `[]`.
func InstanceStartDTOs(rows []model.InstanceStart) []InstanceStartDTO {
	out := make([]InstanceStartDTO, 0, len(rows))
	for _, r := range rows {
		out = append(out, instanceStartDTO(r))
	}
	return out
}

// WarningDTOs renders a `warnings` array.
func WarningDTOs(warnings []model.Warning) []WarningDTO { return warningDTOs(warnings) }

// FitReportDTOOf renders a fit report, for the estimate `POST /instances/validate`
// carries beside its argv.
func FitReportDTOOf(r fit.Report) FitReportDTO { return fitReportDTO(r) }
