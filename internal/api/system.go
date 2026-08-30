package api

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// The eleven rows of DESIGN section 3.3.
//
// Every fact this group serves is a HOST fact — the systemd control channel, the
// polkit answers, the GPU inventory, the toolchain probe, the unit-file drift,
// the journal — and none of them belongs to a domain service. So the interface
// below is satisfied by the composition root itself rather than by a package
// under internal/: internal/app is the only place that holds the boot probe's
// results, and section 1's rule that dependencies point inward is what keeps
// internal/api free of an import of internal/systemd, internal/hw and
// internal/toolchain all at once.
//
// The DTOs are this package's, and the service returns them directly. That is
// the precedent section 3.6's two credential triples already set with
// TokenStatus: when the "service" is the composition root, a second set of
// structs to convert between would be two spellings of one shape with a mapping
// function whose only job is to let them drift.
//
// `GET /system/capabilities` is the load-bearing one. Section 3.3 calls it "the
// single object the UI reads to decide which controls to disable and which
// explanatory copy to show", and section 11.1a enumerates every degraded
// combination it can report. A daemon that detects F9 or F10 correctly and
// cannot say so is a daemon whose UI lies about the host, which is why this
// route answers even when every other one in the group is degraded.

// SystemService is everything this layer needs to serve section 3.3.
// internal/app satisfies it.
type SystemService interface {
	Info(ctx context.Context) (SystemInfoDTO, error)
	Capabilities(ctx context.Context) (CapabilitiesDTO, error)
	Toolchain(ctx context.Context) ([]ToolchainCheckDTO, error)
	// ProbeToolchain re-runs the probe. It is a long action and answers 202
	// with a receipt, per section 3's "long actions never block".
	ProbeToolchain(ctx context.Context) (JobReceiptDTO, error)
	GPUs(ctx context.Context) ([]GPUDTO, error)
	Disk(ctx context.Context) ([]DiskUsageDTO, error)
	Units(ctx context.Context) ([]UnitStatusDTO, error)
	// Journal returns the tail. It returns model.CodeJournalUnavailable when
	// `runtime_info.journal_read != 'ok'` (D77) — an empty stream and a denied
	// one must not look alike.
	Journal(ctx context.Context, p JournalParams) ([]JournalLineDTO, error)
	Notifications(ctx context.Context) ([]NotificationDTO, error)
	DismissNotification(ctx context.Context, id string) error
	// Restart is section 3.3's ordered restart. It returns the four guard
	// refusals — restart_unavailable, systemd_denied, job_in_flight and D93's
	// restart_rate_limited — as model.Error values.
	Restart(ctx context.Context) (RestartDTO, error)
	// Diagnostics writes the D50 support bundle and returns the file to stream
	// back. Section 11.3 defines the artifact; section 4 screen 16 puts the
	// button on the System screen.
	Diagnostics(ctx context.Context) (DiagnosticsBundle, error)
}

// SystemdControl, SystemdScope, JournalRead and ListenerContinuity reach the
// wire as the strings internal/model closes their columns with.

// CapabilitiesDTO is `GET /api/v1/system/capabilities` (section 3.3).
//
// The three nullable booleans are nullable for one reason and it matters:
// section 11.1a says a polkit answer that was never asked for — user scope,
// where a user manager authorizes its owner unconditionally — is "not
// applicable", NEVER false. A UI that renders a missing answer as a denial
// tells a working host it is broken.
type CapabilitiesDTO struct {
	SystemdControl string `json:"systemd_control"`
	SystemdScope   string `json:"systemd_scope"`
	// PolkitOK and PolkitUnitFiles are null in user scope and on a host whose
	// bus could not be reached at all.
	PolkitOK           *bool  `json:"polkit_ok"`
	PolkitUnitFiles    *bool  `json:"polkit_unit_files"`
	ListenerContinuity string `json:"listener_continuity"`
	// InstanceControl is whether start, stop and restart can reach a manager.
	InstanceControl bool `json:"instance_control"`
	// AutostartControl is whether unit files may be enabled and disabled.
	AutostartControl bool `json:"autostart_control"`
	// SelfUpdate and SelfUpdateRevert are answered by READING THE INSTALLED
	// UNITS' OWN DIRECTIVES (D95) — `OnFailure=` on llamaman.service, and the
	// presence and mask state of llamaman-update-verify.service — and never by
	// "the drift check reports no drift", which would turn every unit-template
	// change into a permanent refusal.
	SelfUpdate       bool   `json:"self_update"`
	SelfUpdateRevert bool   `json:"self_update_revert"`
	JournalRead      string `json:"journal_read"`
	// Degraded lists the section 11.1a failure ids in effect (`F9`, `F10`, …),
	// so the UI's banner can name the mode rather than infer it from a
	// combination of five booleans.
	Degraded []DegradedModeDTO `json:"degraded"`
}

// DegradedModeDTO is one entry of section 11.1a's enumeration: what is
// unavailable, why, and the command that fixes it.
type DegradedModeDTO struct {
	// ID is the failure id of section 11.1a / section 17 (`F9`, `F10`, `F16`).
	ID string `json:"id"`
	// Summary is one sentence a banner can render verbatim.
	Summary string `json:"summary"`
	// Hints are the exact commands, in the order to run them.
	Hints []string `json:"hints"`
}

// SystemInfoDTO is `GET /api/v1/system/info`.
type SystemInfoDTO struct {
	Version string  `json:"version"`
	Commit  *string `json:"commit"`
	// UptimeSec is this daemon's, not the host's.
	UptimeSec int64 `json:"uptime_sec"`
	// Identity is the user the service runs as.
	Identity       string `json:"identity"`
	SystemdScope   string `json:"systemd_scope"`
	SystemdControl string `json:"systemd_control"`
	PolkitOK       *bool  `json:"polkit_ok"`
	UIPort         int    `json:"ui_port"`
	UIURL          string `json:"ui_url"`
	StateDir       string `json:"state_dir"`
	HFHome         string `json:"hf_home"`
	Kernel         string `json:"kernel"`
	CPUModel       string `json:"cpu_model"`
	CPUCount       int    `json:"cpu_count"`
	RAMTotalBytes  int64  `json:"ram_total_bytes"`
	RAMFreeBytes   int64  `json:"ram_free_bytes"`
}

// ToolchainCheckDTO is one row of `GET /api/v1/system/toolchain` — one tool, its
// verdict, and the guidance section 11.1a owes a user whose build will fail.
type ToolchainCheckDTO struct {
	Name    string  `json:"name"`
	Found   bool    `json:"found"`
	Path    *string `json:"path"`
	Version *string `json:"version"`
	// MinVersion is what this tool must be at least, null for a tool with no
	// floor (git) and for the two that are facts rather than tools (glibc, free
	// space).
	MinVersion *string `json:"min_version"`
	OK         bool    `json:"ok"`
	Note       *string `json:"note"`
	DocsURL    *string `json:"docs_url"`
}

// GPUDTO is one row of `GET /api/v1/system/gpus`.
//
// Every telemetry field is nullable and `state` says which reading this is. A
// probe that failed reports `state:"unknown"` with nulls rather than zeros: a
// card showing 0 MiB free reads as "full", which is the opposite of "we could
// not ask" (F14).
type GPUDTO struct {
	ID    string `json:"id"`
	Index int    `json:"index"`
	Name  string `json:"name"`
	UUID  string `json:"uuid"`

	VRAMTotal *int64 `json:"vram_total"`
	VRAMUsed  *int64 `json:"vram_used"`
	VRAMFree  *int64 `json:"vram_free"`

	UtilPct *int    `json:"util_pct"`
	TempC   *int    `json:"temp_c"`
	Driver  string  `json:"driver"`
	CUDA    *string `json:"cuda"`
	// ComputeCap is the `major.minor` capability the source build's
	// CMAKE_CUDA_ARCHITECTURES is derived from (section 6.5).
	ComputeCap *string `json:"compute_cap"`
	State      string  `json:"state"`
}

// DiskUsageDTO is one row of `GET /api/v1/system/disk`: per cache root and for
// the state directory.
type DiskUsageDTO struct {
	Path string `json:"path"`
	// Kind is `cache_root` or `state_dir`.
	Kind string `json:"kind"`
	// Primary is set only for a cache root.
	Primary    *bool `json:"primary"`
	TotalBytes int64 `json:"total_bytes"`
	FreeBytes  int64 `json:"free_bytes"`
	UsedBytes  int64 `json:"used_bytes"`
	// ModelBytes is what this root's models occupy, and VersionBytes what the
	// llama.cpp builds under the state directory do. Each is null on the kind it
	// does not apply to — never a zero, which would read as "nothing there".
	ModelBytes   *int64 `json:"model_bytes"`
	VersionBytes *int64 `json:"version_bytes"`
}

// UnitStatusDTO is one row of `GET /api/v1/system/units` (section 5.4a).
//
// `stale` — an older or absent stamp — is the ORDINARY state of a host that has
// self-updated across a release which changed a template, and it blocks nothing
// (D95). `missing`, and a hash mismatch at the current stamp, are F16.
type UnitStatusDTO struct {
	Unit string `json:"unit"`
	// InstalledStamp is the `# llamaman-units: <N>` line `install-units` wrote,
	// null when the file carries none.
	InstalledStamp *int64 `json:"installed_stamp"`
	// TemplateStamp is what the running binary would render.
	TemplateStamp int64 `json:"template_stamp"`
	// Drift is `none`, `stale` or `missing`.
	Drift string `json:"drift"`
	// The F21 diff: units this one wants, requires, or that are masked, which
	// the installed file and the rendered template disagree about.
	WantsDiff    []string `json:"wants_diff"`
	RequiresDiff []string `json:"requires_diff"`
	MaskedDiff   []string `json:"masked_diff"`
	// RepairCommand is the exact line to run, null when there is nothing to
	// repair. Read-only: the daemon cannot write /etc.
	RepairCommand *string `json:"repair_command"`
}

// JournalParams is `GET /api/v1/system/journal`'s query.
type JournalParams struct {
	Unit  string
	Lines int
}

// JournalLineDTO is one journal entry.
type JournalLineDTO struct {
	At   string  `json:"at"`
	Unit *string `json:"unit"`
	// Priority is the syslog level, so the viewer can color an error line.
	Priority *int   `json:"priority"`
	Message  string `json:"message"`
}

// NotificationDTO is one row of `GET /api/v1/system/notifications` — section
// 2.11's much smaller table beside `events`: things that need a human.
type NotificationDTO struct {
	ID       string `json:"id"`
	Severity string `json:"severity"`
	// Code maps to a remediation card (section 17).
	Code    string `json:"code"`
	Title   string `json:"title"`
	Message string `json:"message"`
	// Hints are the remediation commands the card prints.
	Hints     []string    `json:"hints"`
	Subject   *SubjectDTO `json:"subject"`
	CreatedAt string      `json:"created_at"`
	// DismissedAt is null on every row this endpoint returns by default; it is
	// on the wire so a client that asked for dismissed rows can tell them apart.
	DismissedAt *string `json:"dismissed_at"`
}

// RestartDTO is the 202 of `POST /api/v1/system/restart` (section 3.3, section 9.4).
type RestartDTO struct {
	// JobID is null: the restart is not a job, because the process that would
	// report its progress is the one going away.
	JobID *string `json:"job_id"`
	Unit  string  `json:"unit"`
	// ListenerContinuity is whether the gateway ports survive the swap through
	// the fd store, or drop and are rebound (section 9.4).
	ListenerContinuity string `json:"listener_continuity"`
	DrainSec           int    `json:"drain_sec"`
}

// DiagnosticsBundle is the D50 support archive: a path on disk plus the name to
// offer it under. It is streamed rather than buffered because it carries the
// journal tail.
type DiagnosticsBundle struct {
	// Path is the file to stream. The handler removes it after writing.
	Path string
	// Filename is the `Content-Disposition` name.
	Filename string
}

// DiagnosticsDTO documents the download for the generated schema. The response
// itself is an archive, not JSON; this shape exists so the operation appears in
// openapi.json with a body rather than as a bare 200.
type DiagnosticsDTO struct {
	// Filename is the name the archive is offered under.
	Filename string `json:"filename"`
}

func (a *API) systemRoutes() []Route {
	return []Route{
		a.systemInfoRoute(),
		a.systemCapabilitiesRoute(),
		a.systemToolchainRoute(),
		a.systemToolchainProbeRoute(),
		a.systemGPUsRoute(),
		a.systemDiskRoute(),
		a.systemUnitsRoute(),
		a.systemJournalRoute(),
		a.systemNotificationsRoute(),
		a.systemDismissNotificationRoute(),
		a.systemDiagnosticsRoute(),
		a.systemRestartRoute(),
	}
}

// system resolves the service or reports the build gap, exactly as every other
// group in this package does.
func (a *API) system() (SystemService, error) {
	if a.cfg.System == nil {
		return nil, Errorf(http.StatusServiceUnavailable, CodeInternalError,
			"this daemon was built without a system service")
	}
	return a.cfg.System, nil
}

func (a *API) systemInfoRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/system/info",
		Auth:        AuthSession,
		OperationID: "getSystemInfo",
		Summary:     "Version, uptime, service identity, systemd facts, ports, paths and host hardware.",
		Tag:         "system",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.system()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			info, err := svc.Info(r.Context())
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := WriteJSON(w, http.StatusOK, info); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{Status: http.StatusOK, Description: "This daemon and this host.",
			Body: SystemInfoDTO{}},
	}
}

func (a *API) systemCapabilitiesRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/system/capabilities",
		Auth:        AuthSession,
		OperationID: "getSystemCapabilities",
		Summary: "The single object the UI reads to decide which controls to disable and which " +
			"explanatory copy to show, so it never has to guess.",
		Tag: "system",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.system()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			caps, err := svc.Capabilities(r.Context())
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := WriteJSON(w, http.StatusOK, caps); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{Status: http.StatusOK,
			Description: "What this host lets the daemon do, and which degraded modes are in effect.",
			Body:        CapabilitiesDTO{}},
	}
}

func (a *API) systemToolchainRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/system/toolchain",
		Auth:        AuthSession,
		OperationID: "getSystemToolchain",
		Summary:     "The latest probe: per-tool found/version/verdict with fix guidance.",
		Tag:         "system",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.system()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			checks, err := svc.Toolchain(r.Context())
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := WriteJSON(w, http.StatusOK, NewList(checks, len(checks), nil)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{Status: http.StatusOK, Description: "One row per tool.",
			Body: List[ToolchainCheckDTO]{}},
	}
}

func (a *API) systemToolchainProbeRoute() Route {
	return Route{
		Method:      http.MethodPost,
		Pattern:     BasePath + "/system/toolchain/probe",
		Auth:        AuthSession,
		OperationID: "probeSystemToolchain",
		Summary:     "Re-run the toolchain probe.",
		Tag:         "system",
		Idempotent:  true,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.system()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			receipt, err := svc.ProbeToolchain(r.Context())
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := WriteJSON(w, http.StatusAccepted, receipt); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{Status: http.StatusAccepted, Description: "The probe was queued.",
			Body: JobReceiptDTO{}},
	}
}

func (a *API) systemGPUsRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/system/gpus",
		Auth:        AuthSession,
		OperationID: "listSystemGPUs",
		Summary:     "The GPU inventory with live VRAM, utilization and temperature.",
		Tag:         "system",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.system()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			gpus, err := svc.GPUs(r.Context())
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := WriteJSON(w, http.StatusOK, NewList(gpus, len(gpus), nil)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{Status: http.StatusOK,
			Description: "One row per device. An empty list means no supported device was found, " +
				"which is an answer rather than a failure.",
			Body: List[GPUDTO]{}},
	}
}

func (a *API) systemDiskRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/system/disk",
		Auth:        AuthSession,
		OperationID: "getSystemDisk",
		Summary:     "Total, free and used per cache root and for the state directory.",
		Tag:         "system",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.system()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			rows, err := svc.Disk(r.Context())
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := WriteJSON(w, http.StatusOK, NewList(rows, len(rows), nil)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{Status: http.StatusOK, Description: "One row per filesystem of interest.",
			Body: List[DiskUsageDTO]{}},
	}
}

func (a *API) systemUnitsRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/system/units",
		Auth:        AuthSession,
		OperationID: "listSystemUnits",
		Summary: "Installed unit files versus what this binary would render, with the exact " +
			"repair command. Read-only — the daemon cannot write /etc.",
		Tag: "system",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.system()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			units, err := svc.Units(r.Context())
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := WriteJSON(w, http.StatusOK, NewList(units, len(units), nil)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{Status: http.StatusOK, Description: "One row per unit.",
			Body: List[UnitStatusDTO]{}},
	}
}

func (a *API) systemJournalRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/system/journal",
		Auth:        AuthSession,
		OperationID: "getSystemJournal",
		Summary:     "The journal tail for a unit. SSE when Accept: text/event-stream.",
		Tag:         "system",
		Query: []QueryParam{
			{Name: "unit", Description: "The unit to read; defaults to the daemon's own."},
			{Name: "lines", Description: "How many entries to return.", Type: "integer"},
		},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.system()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			lines, err := svc.Journal(r.Context(), JournalParams{
				Unit:  r.URL.Query().Get("unit"),
				Lines: int(queryInt64(r, "lines", 500)),
			})
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
		Errors: []Response{{
			Status: http.StatusConflict,
			Description: "This daemon's identity cannot read the journal (D77). An empty stream " +
				"and a denied one must not look alike, so this is refused rather than answered " +
				"with nothing; `details.hints` names the group to add.",
			Codes: []model.ErrorCode{model.CodeJournalUnavailable},
		}},
	}
}

func (a *API) systemNotificationsRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/system/notifications",
		Auth:        AuthSession,
		OperationID: "listSystemNotifications",
		Summary:     "Undismissed notifications with their remediation actions.",
		Tag:         "system",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.system()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			rows, err := svc.Notifications(r.Context())
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := WriteJSON(w, http.StatusOK, NewList(rows, len(rows), nil)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{Status: http.StatusOK, Description: "The outstanding cards.",
			Body: List[NotificationDTO]{}},
	}
}

func (a *API) systemDismissNotificationRoute() Route {
	return Route{
		Method:      http.MethodPost,
		Pattern:     BasePath + "/system/notifications/{id}/dismiss",
		Auth:        AuthSession,
		OperationID: "dismissSystemNotification",
		Summary:     "Clear one card. The row is stamped, not deleted.",
		Tag:         "system",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.system()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := svc.DismissNotification(r.Context(), r.PathValue("id")); err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			WriteNoContent(w)
		}),
		Success: Response{Status: http.StatusNoContent, Description: "The card was dismissed."},
		Errors: []Response{{Status: http.StatusNotFound,
			Description: "No notification has this id.",
			Codes:       []model.ErrorCode{CodeNotFound}}},
	}
}

func (a *API) systemDiagnosticsRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/system/diagnostics",
		Auth:        AuthSession,
		OperationID: "downloadDiagnostics",
		Summary: "The D50 support bundle: configuration, unit files, the journal tail and the " +
			"toolchain report, redacted of every secret.",
		Tag: "system",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.system()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			bundle, err := svc.Diagnostics(r.Context())
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			a.serveDiagnostics(w, r, bundle)
		}),
		Success: Response{Status: http.StatusOK,
			Description: "The archive, as a download.",
			Body:        DiagnosticsDTO{},
			MediaType:   "application/gzip"},
	}
}

func (a *API) systemRestartRoute() Route {
	return Route{
		Method:      http.MethodPost,
		Pattern:     BasePath + "/system/restart",
		Auth:        AuthSession,
		OperationID: "restartSystem",
		Summary: "Restart llamaman.service: commit, flush this 202, drain, hand the listeners to " +
			"the fd store, then a non-blocking RestartNoWait. Instances are untouched.",
		Tag: "system",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.system()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			out, err := svc.Restart(r.Context())
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := WriteJSON(w, http.StatusAccepted, out); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{Status: http.StatusAccepted,
			Description: "The restart was accepted and begins after this response is flushed.",
			Body:        RestartDTO{}},
		Errors: []Response{
			{Status: http.StatusConflict,
				Description: "A build or a self-update is live; no service manager is reachable; " +
					"or the name-scoped manage-units grant on llamaman.service was refused. Each " +
					"carries the manual command in `details.hints`.",
				Codes: []model.ErrorCode{
					model.CodeJobInFlight,
					model.CodeRestartUnavailable,
					model.CodeSystemdDenied,
				}},
			{Status: http.StatusTooManyRequests,
				Description: "D93: this boot has not yet cleared its unit's start-limit counter. " +
					"`details.retry_after_ms` is how long the UI disables the button for, rather " +
					"than spending a start the revert deadline needs.",
				Codes: []model.ErrorCode{model.CodeRestartRateLimited}},
		},
	}
}

// serveDiagnostics streams the bundle and removes it.
//
// It is streamed from a file rather than buffered because the archive carries
// the journal tail, which on a host that has been crash-looping is the largest
// thing this API ever sends. The file is removed after the write either way: a
// support bundle left in the state directory is a redacted-but-still-sensitive
// artifact nobody asked to keep.
func (a *API) serveDiagnostics(w http.ResponseWriter, r *http.Request, b DiagnosticsBundle) {
	defer func() { _ = os.Remove(b.Path) }()

	f, err := os.Open(b.Path)
	if err != nil {
		WriteError(w, r, a.log, err)
		return
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		WriteError(w, r, a.log, err)
		return
	}

	name := b.Filename
	if name == "" {
		name = filepath.Base(b.Path)
	}
	h := w.Header()
	h.Set("Content-Type", "application/gzip")
	h.Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	h.Set("Cache-Control", "no-store")
	// The name is ours — it is built from the daemon's version and the clock,
	// never from anything a request carried — so it needs no quoting dance.
	h.Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f)
}
