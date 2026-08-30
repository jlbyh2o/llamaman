package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/jlbyh2o/llamaman/internal/api"
	"github.com/jlbyh2o/llamaman/internal/fit"
	"github.com/jlbyh2o/llamaman/internal/hw"
	"github.com/jlbyh2o/llamaman/internal/instances"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/models"
	"github.com/jlbyh2o/llamaman/internal/store"
	"github.com/jlbyh2o/llamaman/internal/systemd"
)

// The composition root's answer to the supervision half of DESIGN section 3.10
// (api.InstanceControlService).
//
// Three collaborators meet here and nowhere else, which is why this adapter
// exists rather than more methods on *instances.Service: the instance service
// owns the guarded transitions, internal/systemd owns the control channel and
// the journal, and the fit calculator owns `-ngl auto`'s resolved count. Section
// 1's invariant 2 — no package outside internal/systemd may implement or bypass
// the Controller — is what keeps the first of those from simply calling the
// second.
//
// The degraded modes of section 11.1a are enforced HERE, once, in
// requireControl and requireUnitFiles. Section 3.10 gives each control verb a
// `409 systemd_unavailable` and the autostart toggle a `409
// autostart_unavailable`, and each carries the exact command to run by hand:
// F9 and F10 are supported installs, so "you cannot do this here, do this
// instead" is the contract, not an error path.

// instanceControlAPI implements api.InstanceControlService.
type instanceControlAPI struct{ d *daemon }

/* -- guards ----------------------------------------------------------------- */

// requireControl is F10. Without a manager there is nothing to ask, and saying
// so with the manual command beats writing a desired state nobody will act on.
func (c *instanceControlAPI) requireControl(unit string) error {
	if c.d.systemd.Control != nil {
		return nil
	}
	return model.Error{
		Code: model.CodeSystemdUnavailable,
		Message: "no service manager is reachable from this daemon, so instances cannot be " +
			"controlled from here",
		Details: map[string]any{"unit": unit, "hints": []string{
			"sudo systemctl start " + unit,
			"sudo llamaman install-units --identity " + currentIdentity(),
		}},
	}
}

// requireUnitFiles is F9's narrower refusal: starting and stopping still work,
// and only enablement is withheld.
func (c *instanceControlAPI) requireUnitFiles(unit string) error {
	if c.d.systemd.Control == nil {
		return c.requireControl(unit)
	}
	if c.d.systemd.ManageUnitFiles(c.d.scope) {
		return nil
	}
	return model.Error{
		Code: model.CodeAutostartUnavailable,
		Message: "this daemon may not enable or disable unit files on this host, so the " +
			"autostart switch cannot take effect",
		Details: map[string]any{"unit": unit, "hints": []string{
			"sudo systemctl enable " + unit,
			"sudo llamaman install-units --repair-polkit",
		}},
	}
}

// instance reads one non-deleted row, or reports 404.
func (c *instanceControlAPI) instance(ctx context.Context, id string) (instances.View, error) {
	return c.d.instances.Get(ctx, id)
}

// kick asks the supervisor for a pass now rather than at its next tick.
//
// Every control verb writes the DESIRED axis and lets the supervisor act, which
// is what makes an instance that crashed while the daemon was down get restarted
// when the daemon returns. But a user who clicks Start and watches a spinner for
// the length of a poll interval reasonably concludes the button is broken, so
// the write is followed by a nudge. The nudge is best-effort and detached: it is
// an optimization of latency, never the mechanism.
func (c *instanceControlAPI) kick() {
	sup := c.d.supervisor
	if sup == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), supervisorKickTimeout)
		defer cancel()
		if err := sup.Reconcile(ctx); err != nil {
			c.d.log.Debug("the post-control reconcile pass failed", "error", err)
		}
	}()
}

const supervisorKickTimeout = 30 * time.Second

/* -- the five control verbs ------------------------------------------------- */

func (c *instanceControlAPI) Start(ctx context.Context, id string) (api.InstanceControlDTO, error) {
	view, err := c.instance(ctx, id)
	if err != nil {
		return api.InstanceControlDTO{}, err
	}
	if err := c.requireControl(instances.UnitName(view.Name)); err != nil {
		return api.InstanceControlDTO{}, err
	}
	out, err := c.d.instances.SetDesiredState(ctx, id, model.DesiredRunning, model.TriggerUser)
	if err != nil {
		return api.InstanceControlDTO{}, err
	}
	c.kick()
	return api.InstanceControlDTO{
		DesiredState: string(out.DesiredState), Hints: []string{},
	}, nil
}

func (c *instanceControlAPI) Stop(ctx context.Context, id string, drainSec int) (
	api.InstanceControlDTO, error) {

	view, err := c.instance(ctx, id)
	if err != nil {
		return api.InstanceControlDTO{}, err
	}
	if err := c.requireControl(instances.UnitName(view.Name)); err != nil {
		return api.InstanceControlDTO{}, err
	}
	out, err := c.d.instances.SetDesiredState(ctx, id, model.DesiredStopped, "")
	if err != nil {
		return api.InstanceControlDTO{}, err
	}
	c.kick()

	dto := api.InstanceControlDTO{DesiredState: string(out.DesiredState), Hints: []string{}}
	if out.Autostart {
		// Section 2.8's hint. Stopping an autostart instance is a request about
		// NOW, not about the next boot, and a user who does not know that comes
		// back to a running instance and mistrusts the button.
		dto.Hint = ptrTo("will_start_at_boot")
	}
	return dto, nil
}

// RestartInstance is the one verb that reaches systemd directly, and the reason
// is that the supervisor cannot see it.
//
// The supervisor acts on a DIFFERENCE — desired versus actual, or
// `config_hash` versus `applied_config_hash`. A restart of an instance whose
// configuration has not changed is neither, so writing a desired state would
// produce no action at all: the row already says `running` and the unit already
// is. The restart is therefore asked for, detached so the 202 is not held for
// the length of a model load.
func (c *instanceControlAPI) RestartInstance(ctx context.Context, id string) (
	api.InstanceControlDTO, error) {

	view, err := c.instance(ctx, id)
	if err != nil {
		return api.InstanceControlDTO{}, err
	}
	unit := instances.UnitName(view.Name)
	if err := c.requireControl(unit); err != nil {
		return api.InstanceControlDTO{}, err
	}
	out, err := c.d.instances.SetDesiredState(ctx, id, model.DesiredRunning, model.TriggerUser)
	if err != nil {
		return api.InstanceControlDTO{}, err
	}

	control := c.d.systemd.Control
	go func() {
		rctx, cancel := context.WithTimeout(context.Background(), instanceJobTimeout)
		defer cancel()
		if _, err := control.Restart(rctx, unit); err != nil {
			c.d.log.Warn("the requested instance restart failed", "unit", unit, "error", err)
		}
	}()

	return api.InstanceControlDTO{
		DesiredState: string(out.DesiredState), Hints: []string{},
	}, nil
}

// instanceJobTimeout bounds a detached start or restart job. It is longer than a
// health probe and shorter than forever, because a model that takes ten minutes
// to load is a real configuration and a wedged job is not.
const instanceJobTimeout = 10 * time.Minute

func (c *instanceControlAPI) SafeStart(ctx context.Context, id string) (
	api.InstanceControlDTO, error) {

	view, err := c.instance(ctx, id)
	if err != nil {
		return api.InstanceControlDTO{}, err
	}
	if err := c.requireControl(instances.UnitName(view.Name)); err != nil {
		return api.InstanceControlDTO{}, err
	}
	out, err := c.d.instances.SafeStart(ctx, id)
	if err != nil {
		return api.InstanceControlDTO{}, err
	}
	c.kick()
	return api.InstanceControlDTO{
		DesiredState: string(out.DesiredState),
		Hint:         ptrTo("safe_mode"),
		Hints:        []string{},
	}, nil
}

// ResetFailed clears the database latch first and the unit's failed state
// second.
//
// The order is not cosmetic: clearing the unit before the row agrees would let
// the supervisor's next pass observe a healthy unit against a still-latched
// `crash-looping` row and re-derive the latch it was just asked to drop.
func (c *instanceControlAPI) ResetFailed(ctx context.Context, id string) (
	api.InstanceControlDTO, error) {

	view, err := c.instance(ctx, id)
	if err != nil {
		return api.InstanceControlDTO{}, err
	}
	unit := instances.UnitName(view.Name)

	out, err := c.d.instances.ResetFailed(ctx, id)
	if err != nil {
		return api.InstanceControlDTO{}, err
	}

	dto := api.InstanceControlDTO{DesiredState: string(out.DesiredState), Hints: []string{}}
	if control := c.d.systemd.Control; control != nil {
		if err := control.ResetFailed(ctx, unit); err != nil &&
			!errors.Is(err, systemd.ErrNoSuchUnit) {
			// The database half landed, which is the half that governs the
			// supervisor. Report the manual line for the other half rather than
			// failing a reset that mostly worked.
			dto.Hints = append(dto.Hints, "sudo systemctl reset-failed "+unit)
		}
	} else {
		dto.Hints = append(dto.Hints, "sudo systemctl reset-failed "+unit)
	}
	c.kick()
	return dto, nil
}

/* -- autostart and pin-ngl -------------------------------------------------- */

func (c *instanceControlAPI) SetAutostart(ctx context.Context, id string, enabled bool) (
	api.AutostartDTO, error) {

	view, err := c.instance(ctx, id)
	if err != nil {
		return api.AutostartDTO{}, err
	}
	unit := instances.UnitName(view.Name)
	if err := c.requireUnitFiles(unit); err != nil {
		return api.AutostartDTO{}, err
	}

	out, err := c.d.instances.SetAutostart(ctx, id, enabled)
	if err != nil {
		return api.AutostartDTO{}, err
	}

	dto := api.AutostartDTO{Enabled: out.Autostart, Hints: []string{}}
	control := c.d.systemd.Control
	if enabled {
		err = control.Enable(ctx, []string{unit})
	} else {
		err = control.Disable(ctx, []string{unit})
	}
	if err != nil && !errors.Is(err, systemd.ErrNoSuchUnit) {
		dto.Hints = append(dto.Hints,
			"sudo systemctl "+map[bool]string{true: "enable", false: "disable"}[enabled]+" "+unit)
	}

	// Section 2.8's other hint: enabling autostart on a stopped instance says
	// nothing about now, and a user who expected it to start needs telling.
	if enabled && out.Status.State != model.InstanceReady {
		dto.Hint = ptrTo("start_now")
	}
	return dto, nil
}

// PinNGL is D51: turn the calculator's advisory into a saved count.
//
// It refuses on a configuration that is not `auto`, because pinning a count that
// is already pinned would silently overwrite a number the user chose — and it
// refuses when no estimate can be made, because writing a guess as an explicit
// setting is exactly the thing `auto` exists to avoid.
func (c *instanceControlAPI) PinNGL(ctx context.Context, id string) (api.PinNGLDTO, error) {
	view, err := c.instance(ctx, id)
	if err != nil {
		return api.PinNGLDTO{}, err
	}
	if view.Flags.NGpuLayers == nil || view.Flags.NGpuLayers.Mode != model.NGLAuto {
		return api.PinNGLDTO{}, model.Error{
			Code: model.CodeBadFlags,
			Message: "this instance's GPU offload is not set to auto, so there is no advisory " +
				"to pin",
		}
	}

	report, ok, err := c.d.estimateFor(ctx, view.ModelID, view.Flags)
	if err != nil {
		return api.PinNGLDTO{}, err
	}
	if !ok {
		return api.PinNGLDTO{}, model.Error{
			Code: model.CodeModelMissing,
			Message: "this model's GGUF header has not been parsed yet, so no offload can be " +
				"estimated — pin a count once the download finishes",
		}
	}

	count := report.NGpuLayers
	flags := view.Flags
	flags.NGpuLayers = &model.NGpuLayers{Mode: model.NGLCount, Count: &count}

	updated, err := c.d.instances.Patch(ctx, id, instances.PatchParams{
		Generation: view.Generation,
		Flags:      &flags,
	})
	if err != nil {
		return api.PinNGLDTO{}, err
	}
	return api.PinNGLDTO{
		Instance:     api.InstanceDTOOf(updated),
		PinnedLayers: count,
		Warnings:     api.WarningDTOs(updated.Warnings),
	}, nil
}

/* -- reads ------------------------------------------------------------------ */

func (c *instanceControlAPI) InstanceStatus(ctx context.Context, id string) (
	api.InstanceLiveStatusDTO, error) {

	view, err := c.instance(ctx, id)
	if err != nil {
		return api.InstanceLiveStatusDTO{}, err
	}
	out := api.InstanceLiveStatusDTO{
		Status: api.InstanceStatusDTOOf(view.Status),
		HealthURL: "http://" + net.JoinHostPort("127.0.0.1",
			strconv.Itoa(view.InternalPort)) + "/health",
	}

	// A live unit read, or nothing. Section 11.1a's rule: an unavailable manager
	// reports NULL, never a fabricated `inactive` — which a screen would render
	// as "stopped" and a user would read as an answer.
	if control := c.d.systemd.Control; control != nil {
		props, err := control.Props(ctx, instances.UnitName(view.Name))
		if err == nil {
			live := &api.UnitLiveDTO{
				ActiveState: props.ActiveState,
				SubState:    props.SubState,
				Result:      props.Result,
				NRestarts:   int64(props.NRestarts),
			}
			if props.MainPID != 0 {
				live.MainPID = ptrTo(int64(props.MainPID))
			}
			if props.MemoryCurrent != 0 {
				live.MemoryBytes = ptrTo(int64(props.MemoryCurrent))
			}
			if !props.ExecMainExitTimestamp.IsZero() {
				live.SinceAt = ptrTo(api.Time(props.ExecMainExitTimestamp.UnixMilli()))
			}
			out.Unit = live
		}
	}
	return out, nil
}

func (c *instanceControlAPI) Usage(ctx context.Context, id, from, to string) (
	api.InstanceUsageDTO, error) {

	view, err := c.instance(ctx, id)
	if err != nil {
		return api.InstanceUsageDTO{}, err
	}

	var rows []store.InstanceUsageRow
	if err := c.d.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		rows, err = c.d.store.InstanceUsage(ctx, tx, id, store.UsageRange{From: from, To: to})
		return err
	}); err != nil {
		return api.InstanceUsageDTO{}, err
	}

	out := api.InstanceUsageDTO{Items: make([]api.InstanceUsageDayDTO, 0, len(rows))}
	for _, r := range rows {
		out.Items = append(out.Items, api.InstanceUsageDayDTO{
			Day:        r.Day,
			AuthMode:   string(r.AuthMode),
			Requests:   r.Requests,
			Errors:     r.Errors,
			BytesIn:    r.BytesIn,
			BytesOut:   r.BytesOut,
			DurationMS: r.DurationMS,
			// Every row of `instance_usage_daily` is the gateway's own ledger;
			// llama-server's counter is the scalar beside the list, not a row.
			Source: "gateway",
		})
	}
	out.Total = len(out.Items)
	out.RequestsServed = view.Status.RequestsServed
	return out, nil
}

func (c *instanceControlAPI) Starts(ctx context.Context, id string, limit int) (
	[]api.InstanceStartDTO, error) {

	if _, err := c.instance(ctx, id); err != nil {
		return nil, err
	}
	var rows []model.InstanceStart
	if err := c.d.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		rows, err = c.d.store.InstanceStarts(ctx, tx, id, limit)
		return err
	}); err != nil {
		return nil, err
	}
	return api.InstanceStartDTOs(rows), nil
}

func (c *instanceControlAPI) Command(ctx context.Context, id string) (
	api.InstanceCommandDTO, error) {

	view, err := c.instance(ctx, id)
	if err != nil {
		return api.InstanceCommandDTO{}, err
	}

	out := api.InstanceCommandDTO{
		Argv:         view.Argv,
		Unit:         instances.UnitName(view.Name),
		UnknownFlags: view.UnknownFlags,
		Env:          map[string]string{},
	}
	if out.Argv == nil {
		out.Argv = []string{}
	}
	if out.UnknownFlags == nil {
		out.UnknownFlags = []string{}
	}

	hubDir := ""
	if c.d.settings != nil {
		hubDir, _ = c.d.settings.GetString(ctx, "hf.hub_dir")
	}
	out.Env = instances.EnvSet(instances.EnvInput{HubDir: hubDir})
	return out, nil
}

func (c *instanceControlAPI) Logs(ctx context.Context, id string, lines int, since string) (
	[]api.JournalLineDTO, error) {

	view, err := c.instance(ctx, id)
	if err != nil {
		return nil, err
	}
	if c.d.systemd.Journal != model.JournalOK {
		return nil, model.Error{
			Code: model.CodeJournalUnavailable,
			Message: "this daemon's identity cannot read the journal, so this instance's log " +
				"cannot be shown — which is not the same as the instance being quiet",
			Details: map[string]any{"journal_read": string(c.d.systemd.Journal), "hints": []string{
				"sudo usermod -aG systemd-journal " + currentIdentity(),
				"sudo systemctl restart llamaman.service",
			}},
		}
	}
	if lines <= 0 || lines > 5000 {
		lines = 500
	}

	opts := systemd.JournalOptions{
		Scope: c.d.scope,
		Units: []string{instances.UnitName(view.Name)},
		Lines: lines,
	}
	if since != "" {
		// A journald cursor is opaque and a timestamp is not; the two are told
		// apart by whether the value parses as one. A value that is neither is
		// ignored rather than refused: it is a resume hint, and refusing to show
		// a log because a cursor went stale is the wrong trade.
		if at, err := time.Parse(time.RFC3339, since); err == nil {
			opts.Since = at
		} else {
			opts.Cursor = since
		}
	}

	entries, err := systemd.Tail(ctx, opts)
	if err != nil {
		if errors.Is(err, systemd.ErrJournalUnavailable) {
			return nil, model.Error{
				Code:    model.CodeJournalUnavailable,
				Message: "journalctl is not available on this host",
			}
		}
		return nil, err
	}
	return journalDTOs(entries), nil
}

/* -- validate, ports and the proxies ---------------------------------------- */

func (c *instanceControlAPI) Validate(ctx context.Context, body api.ValidateInstanceRequest) (
	api.ValidateInstanceDTO, error) {

	params := instances.DryRunParams{
		InstanceID:    body.InstanceID,
		ModelID:       body.ModelID,
		MmprojModelID: body.MmprojModelID,
		DraftModelID:  body.DraftModelID,
	}
	if body.Flags != nil {
		params.Flags = *body.Flags
	}
	if body.ExtraFlags != nil {
		params.ExtraFlags = *body.ExtraFlags
	}

	view, err := c.d.instances.DryRun(ctx, params)
	if err != nil {
		return api.ValidateInstanceDTO{}, err
	}

	out := api.ValidateInstanceDTO{
		Argv:            view.Argv,
		UnknownFlags:    view.UnknownFlags,
		DraftValidation: string(view.DraftValidation),
		Warnings:        api.WarningDTOs(view.Warnings),
	}
	if out.Argv == nil {
		out.Argv = []string{}
	}
	if out.UnknownFlags == nil {
		out.UnknownFlags = []string{}
	}

	// A fit estimate when there is one to make. An unparsed GGUF — the ordinary
	// state of a model that is still downloading — is a null `fit` rather than a
	// zeroed report, for the reason F14 gives everywhere else: a number with no
	// basis reads as an answer.
	if report, ok, err := c.d.estimateFor(ctx, ptrOrEmpty(params.ModelID), params.Flags); err == nil && ok {
		out.Fit = ptrTo(api.FitReportDTOOf(report))
	}
	return out, nil
}

func (c *instanceControlAPI) SuggestPort(ctx context.Context, kind string) (
	api.PortSuggestionDTO, error) {

	port, err := c.d.instances.SuggestPort(ctx, instances.PortKind(kind))
	if err != nil {
		return api.PortSuggestionDTO{}, err
	}
	return api.PortSuggestionDTO{Port: port, Kind: kind}, nil
}

// Proxy forwards one of llama-server's own read-only endpoints.
//
// It talks to `127.0.0.1:<internal_port>` — the loopback address the instance
// binds, never the public gateway port — so the answer describes the process
// rather than whatever the gateway would have done with the request. An instance
// that is not running has no upstream, and saying so beats a connection-refused
// error the user has to decode.
func (c *instanceControlAPI) Proxy(ctx context.Context, id, what string) (api.UpstreamBodyDTO, error) {
	view, err := c.instance(ctx, id)
	if err != nil {
		return api.UpstreamBodyDTO{}, err
	}
	if view.Status.State != model.InstanceReady {
		return api.UpstreamBodyDTO{}, model.Error{
			Code:    model.CodeSystemdUnavailable,
			Message: "this instance is not running, so there is nothing to ask",
			Details: map[string]any{"state": view.Status.State},
		}
	}

	url := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(view.InternalPort)) + "/" + what
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return api.UpstreamBodyDTO{}, err
	}
	resp, err := proxyClient.Do(req)
	if err != nil {
		return api.UpstreamBodyDTO{}, model.Error{
			Code:    model.CodeSystemdUnavailable,
			Message: "this instance did not answer on its internal port",
			Details: map[string]any{"port": view.InternalPort},
		}
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxProxyBody))
	if err != nil {
		return api.UpstreamBodyDTO{}, err
	}
	if resp.StatusCode >= 400 {
		return api.UpstreamBodyDTO{}, model.Error{
			Code:    model.CodeSystemdUnavailable,
			Message: fmt.Sprintf("llama-server answered %d for /%s", resp.StatusCode, what),
		}
	}

	// `/props` and `/slots` are JSON; `/metrics` is Prometheus exposition text.
	// The wrapper carries whichever one arrived in its own field, so a client
	// never has to guess which it is holding.
	var parsed any
	if err := json.Unmarshal(body, &parsed); err == nil {
		return api.UpstreamBodyDTO{JSON: parsed}, nil
	}
	return api.UpstreamBodyDTO{Text: ptrTo(string(body))}, nil
}

// maxProxyBody bounds what an upstream can hand back. `/slots` on a
// heavily-loaded server carries every slot's prompt, which is the only one of
// the three that can get large.
const maxProxyBody = 4 << 20

var proxyClient = &http.Client{Timeout: 5 * time.Second}

/* -- the fit estimate the two callers above share --------------------------- */

// estimateFor runs the calculator for one model and FlagSet, reporting `ok=false`
// when there is nothing to estimate from.
//
// A model whose GGUF has not been parsed is the ordinary case that returns
// false: it is a supported state — this design deliberately allows configuring
// an instance against a model that is still downloading — and an estimate built
// from a missing shape would be a number with no basis, which is worse than no
// number at all (F14).
func (d *daemon) estimateFor(ctx context.Context, modelID *string, flags model.FlagSet) (
	fit.Report, bool, error) {

	if modelID == nil || *modelID == "" {
		return fit.Report{}, false, nil
	}

	var local model.LocalModel
	if err := d.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		local, err = d.store.LocalModel(ctx, tx, *modelID)
		if errors.Is(err, store.ErrNotFound) {
			local = model.LocalModel{}
			return nil
		}
		return err
	}); err != nil {
		return fit.Report{}, false, err
	}
	if local.ID == "" {
		return fit.Report{}, false, nil
	}

	shape, ok := models.FitShape(models.View{LocalModel: local})
	if !ok {
		return fit.Report{}, false, nil
	}

	var gpus []hw.GPU
	if d.gpus != nil {
		gpus, _ = d.gpus.Probe(ctx)
	}
	selected := hw.Select(gpus, hw.Declared(gpus, flags))
	devices := make([]fit.Device, 0, len(selected))
	for i, g := range selected {
		dev := fit.Device{Index: i, UUID: g.UUID, Name: g.Name}
		if g.VRAMKnown() {
			dev.TotalBytes, dev.FreeBytes, dev.Known = *g.VRAMTotalBytes, *g.VRAMFreeBytes, true
		}
		devices = append(devices, dev)
	}

	return fit.Estimate(fit.Request{
		Model:   shape,
		Flags:   models.FitFlags(flags),
		Devices: devices,
	}), true, nil
}

func ptrOrEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func ptrTo[T any](v T) *T { return &v }
