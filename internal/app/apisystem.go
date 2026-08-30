package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/jlbyh2o/llamaman/internal/api"
	"github.com/jlbyh2o/llamaman/internal/buildinfo"
	"github.com/jlbyh2o/llamaman/internal/diagnostics"
	"github.com/jlbyh2o/llamaman/internal/hw"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/selfupdate"
	"github.com/jlbyh2o/llamaman/internal/store"
	"github.com/jlbyh2o/llamaman/internal/systemd"
	"github.com/jlbyh2o/llamaman/internal/toolchain"
)

// The composition root's answer to DESIGN section 3.3 (api.SystemService).
//
// It lives here rather than in a package of its own because every fact section
// 3.3 serves was learned by the boot sequence in this package: the control
// channel and the two polkit answers (step 6), the resolved port and bind, the
// state directory D72 chose, the GPU prober the supervisor and the fit
// calculator share. A `internal/system` package would be a second copy of the
// daemon struct with a different name.
//
// `Capabilities` is the one method here whose absence was a real defect rather
// than a missing feature, and it is worth naming why. Section 11.1a defines
// degraded modes this daemon is expected to SERVE in — F9, a denied polkit
// grant; F10, no reachable service manager — and the boot log already reports
// them correctly. But a UI that cannot ask "what can this host do" has no choice
// but to render every control as if it worked, so a host in F10 showed a
// Dashboard, an Instances table and a System screen with no banner, no
// explanation and no manual command anywhere. Detecting a degraded mode and
// being unable to say so is indistinguishable, from the user's chair, from not
// detecting it at all.

// systemAPI implements api.SystemService over the daemon.
type systemAPI struct {
	d *daemon

	// toolchain caches the probe. The probe shells out to gcc, cmake, ninja,
	// git and nvcc, so a Settings screen that mounted it on every render would
	// fork a dozen processes per paint. `POST /system/toolchain/probe` clears
	// it, which is what that endpoint IS.
	mu        sync.Mutex
	report    *toolchain.Report
	reportAt  time.Time
	probeTTL  time.Duration
	startedAt time.Time
}

// toolchainTTL is how long a probe is reused. It is minutes rather than seconds
// because the answer changes only when someone installs a compiler, and the
// button that says "check again" is right there.
const toolchainTTL = 5 * time.Minute

func newSystemAPI(d *daemon) *systemAPI {
	return &systemAPI{d: d, probeTTL: toolchainTTL, startedAt: d.opts.Now()}
}

/* -- info ------------------------------------------------------------------- */

func (s *systemAPI) Info(ctx context.Context) (api.SystemInfoDTO, error) {
	d := s.d
	out := api.SystemInfoDTO{
		Version:        buildinfo.Version,
		UptimeSec:      int64(d.opts.Now().Sub(s.startedAt).Seconds()),
		Identity:       currentIdentity(),
		SystemdScope:   string(d.scope),
		SystemdControl: string(d.systemd.ControlKind),
		UIPort:         d.uiPort,
		UIURL:          "http://" + net.JoinHostPort(displayHost(d.uiBind), strconv.Itoa(d.uiPort)),
		StateDir:       d.stateDir,
		Kernel:         kernelRelease(),
	}
	if buildinfo.Commit != "" {
		out.Commit = &buildinfo.Commit
	}
	if p := d.systemd.Polkit; p != nil {
		ok := p.ManageUnits
		out.PolkitOK = &ok
	}

	// The hub directory is the fact a user actually needs — it is where models
	// land — and `hf.home` is the courtesy projection of it. Reporting the one
	// the daemon resolved beats reporting an environment variable that may not
	// have been what won.
	if err := d.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		ri, err := d.store.RuntimeInfo(ctx, tx)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if ri.HFHome != nil {
			out.HFHome = *ri.HFHome
		} else if ri.HFHubDir != nil {
			out.HFHome = *ri.HFHubDir
		}
		if ri.ServiceUser != nil && *ri.ServiceUser != "" {
			out.Identity = *ri.ServiceUser
		}
		return nil
	}); err != nil {
		return api.SystemInfoDTO{}, err
	}

	if cpu, err := hw.Cpuinfo(""); err == nil {
		out.CPUModel = cpu.Model
		out.CPUCount = cpu.Threads
	}
	if mem, err := hw.Meminfo(""); err == nil && mem.Known {
		out.RAMTotalBytes = int64(mem.TotalBytes)
		// MemAvailable, not MemFree: on a host that has read a 40 GB model once,
		// MemFree is a small number that would make every partial offload look
		// impossible (section 8.6).
		out.RAMFreeBytes = int64(mem.AvailableBytes)
	}
	return out, nil
}

func currentIdentity() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return strconv.Itoa(os.Geteuid())
}

func kernelRelease() string {
	var un unix.Utsname
	if err := unix.Uname(&un); err != nil {
		return runtime.GOOS
	}
	return nullTerminated(un.Release[:]) + " " + nullTerminated(un.Machine[:])
}

func nullTerminated(b []byte) string {
	if i := indexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

func indexByte(b []byte, c byte) int {
	for i, v := range b {
		if v == c {
			return i
		}
	}
	return -1
}

/* -- capabilities ----------------------------------------------------------- */

// Capabilities is section 3.3's "single object the UI reads to decide which
// controls to disable and which explanatory copy to show".
//
// `self_update` and `self_update_revert` are read off the INSTALLED UNITS' OWN
// DIRECTIVES and never off the drift check (D95). A host that self-updated
// across a release which changed a template legitimately differs from what this
// binary would render; it is `drift: stale`, it blocks nothing, and answering
// "no revert" for it would make every unit-template change a permanent refusal
// to update.
func (s *systemAPI) Capabilities(ctx context.Context) (api.CapabilitiesDTO, error) {
	d := s.d
	out := api.CapabilitiesDTO{
		SystemdControl:     string(d.systemd.ControlKind),
		SystemdScope:       string(d.scope),
		ListenerContinuity: string(model.ContinuityNone),
		JournalRead:        string(d.systemd.Journal),
		InstanceControl:    d.systemd.Control != nil,
		AutostartControl:   d.systemd.ManageUnitFiles(d.scope),
		Degraded:           []api.DegradedModeDTO{},
	}
	if d.gateway != nil {
		out.ListenerContinuity = string(d.gateway.Continuity())
	}
	if p := d.systemd.Polkit; p != nil {
		units, files := p.ManageUnits, p.ManageUnitFiles
		out.PolkitOK = &units
		out.PolkitUnitFiles = &files
	}

	// Section 5.2a: in user scope the daemon performs the swap in process, so
	// there is no actor unit to require and `self_update` is a property of
	// having a manager at all.
	units := selfupdate.DiskUnits{Scope: d.scope}
	swapOK := d.scope == model.ScopeUser
	if d.scope == model.ScopeSystem {
		if u, err := units.Unit(selfupdate.SwapUnit); err == nil {
			swapOK = u.Present && !u.Masked
		}
	}
	out.SelfUpdate = d.systemd.ControlKind != model.ControlUnavailable && swapOK

	revertOK := false
	if judge, err := units.Unit(selfupdate.JudgeUnit); err == nil && judge.Present && !judge.Masked {
		if daemonUnit, err := units.Unit(selfupdate.DaemonUnit); err == nil && daemonUnit.Present {
			revertOK = systemd.HasDirective(daemonUnit.Content, "OnFailure", selfupdate.JudgeUnit)
		}
	}
	out.SelfUpdateRevert = revertOK

	out.Degraded = degradedModes(out)
	return out, nil
}

// degradedModes turns the capability booleans into section 11.1a's named modes,
// each carrying the command that lifts it.
//
// The UI could derive these itself, and that is exactly why it should not: five
// booleans have thirty-two combinations, section 11.1a names the handful that
// mean something, and a banner that re-derives them is a second implementation
// of a table that lives in one document.
func degradedModes(c api.CapabilitiesDTO) []api.DegradedModeDTO {
	out := []api.DegradedModeDTO{}

	if c.SystemdControl == string(model.ControlUnavailable) {
		out = append(out, api.DegradedModeDTO{
			ID: "F10",
			Summary: "No service manager is reachable from this daemon, so instances cannot be " +
				"started, stopped or restarted from this screen. Everything else — the model " +
				"catalog, downloads, builds and benchmarks — works normally.",
			Hints: []string{
				"sudo systemctl start llamaman-instance@<name>.service",
				"sudo llamaman install-units --identity <user>",
			},
		})
		return out
	}

	if c.PolkitOK != nil && !*c.PolkitOK {
		out = append(out, api.DegradedModeDTO{
			ID: "F9",
			Summary: "This host's polkit rules do not let the daemon manage its units, so " +
				"starting, stopping and restarting must be done by hand.",
			Hints: []string{"sudo llamaman install-units --repair-polkit"},
		})
	}
	if !c.AutostartControl {
		out = append(out, api.DegradedModeDTO{
			ID: "F9",
			Summary: "The daemon may not enable or disable unit files on this host, so the " +
				"autostart switch cannot take effect. An instance's start-at-boot setting must " +
				"be applied with systemctl.",
			Hints: []string{"sudo systemctl enable llamaman-instance@<name>.service"},
		})
	}
	if c.JournalRead != string(model.JournalOK) {
		out = append(out, api.DegradedModeDTO{
			ID: "F23",
			Summary: "This daemon's identity cannot read the journal, so the log panes are " +
				"empty rather than quiet. Adding the service user to systemd-journal fixes it.",
			Hints: []string{
				"sudo usermod -aG systemd-journal <user>",
				"sudo systemctl restart llamaman.service",
			},
		})
	}
	if !c.SelfUpdateRevert {
		out = append(out, api.DegradedModeDTO{
			ID: "F20",
			Summary: "The automatic revert is not installed, so self-update is refused: no " +
				"update is ever staged without a working way back.",
			Hints: []string{"sudo llamaman install-units --identity <user>"},
		})
	}
	return out
}

/* -- toolchain -------------------------------------------------------------- */

func (s *systemAPI) Toolchain(ctx context.Context) ([]api.ToolchainCheckDTO, error) {
	rep := s.toolchainReport(ctx)
	out := make([]api.ToolchainCheckDTO, 0, len(rep.Tools))
	for _, t := range rep.Tools {
		row := api.ToolchainCheckDTO{Name: t.Name, Found: t.Found, OK: t.OK}
		if t.Path != "" {
			row.Path = &t.Path
		}
		if t.Version != "" {
			row.Version = &t.Version
		}
		if t.MinVersion != "" {
			row.MinVersion = &t.MinVersion
		}
		if t.Note != "" {
			row.Note = &t.Note
		}
		if t.DocsURL != "" {
			row.DocsURL = &t.DocsURL
		}
		out = append(out, row)
	}
	return out, nil
}

// ProbeToolchain drops the cache and re-probes.
//
// The receipt carries a null `job_id` truthfully: there is no queued work here.
// A probe is five `--version` invocations and completes inside the request, so
// minting a job row for it would put a row in `jobs` that is finished before any
// client could read it — section 3's long-action rule exists for work that
// outlives a request, and this is not that.
func (s *systemAPI) ProbeToolchain(ctx context.Context) (api.JobReceiptDTO, error) {
	s.mu.Lock()
	s.report = nil
	s.mu.Unlock()

	rep := s.toolchainReport(ctx)
	return api.JobReceiptDTO{
		Subject: api.SubjectDTO{Type: "toolchain", ID: string(rep.Family)},
	}, nil
}

func (s *systemAPI) toolchainReport(ctx context.Context) toolchain.Report {
	now := s.d.opts.Now()

	s.mu.Lock()
	if s.report != nil && now.Sub(s.reportAt) < s.probeTTL {
		rep := *s.report
		s.mu.Unlock()
		return rep
	}
	s.mu.Unlock()

	rep := toolchain.Probe(ctx, toolchain.Options{})

	s.mu.Lock()
	s.report, s.reportAt = &rep, now
	s.mu.Unlock()
	return rep
}

/* -- GPUs ------------------------------------------------------------------- */

func (s *systemAPI) GPUs(ctx context.Context) ([]api.GPUDTO, error) {
	if s.d.gpus == nil {
		return []api.GPUDTO{}, nil
	}
	devices, err := s.d.gpus.Probe(ctx)
	if err != nil {
		// A probe failure is not a 500. The design is explicit that a failed
		// poll marks devices unknown rather than absent (D16) — but a host with
		// no NVIDIA anything reaches here too, and an empty list is the honest
		// answer for it. Either way the screen shows what is true.
		return []api.GPUDTO{}, nil
	}

	out := make([]api.GPUDTO, 0, len(devices))
	for _, g := range devices {
		row := api.GPUDTO{
			ID:     g.UUID,
			Index:  g.Index,
			Name:   g.Name,
			UUID:   g.UUID,
			Driver: g.DriverVersion,
			State:  "unknown",
		}
		if row.ID == "" {
			row.ID = strconv.Itoa(g.Index)
		}
		if g.VRAMKnown() {
			row.State = "ok"
			row.VRAMTotal = int64Ptr(*g.VRAMTotalBytes)
			row.VRAMUsed = int64Ptr(*g.VRAMUsedBytes)
			row.VRAMFree = int64Ptr(*g.VRAMFreeBytes)
			util, temp := g.UtilizationPct, g.TemperatureC
			row.UtilPct, row.TempC = &util, &temp
		}
		if g.ComputeCap != "" {
			cc := g.ComputeCap
			row.ComputeCap = &cc
		}
		out = append(out, row)
	}
	return out, nil
}

func int64Ptr(v uint64) *int64 {
	n := int64(v)
	return &n
}

/* -- disk ------------------------------------------------------------------- */

func (s *systemAPI) Disk(ctx context.Context) ([]api.DiskUsageDTO, error) {
	d := s.d
	out := []api.DiskUsageDTO{}

	var (
		roots []model.CacheRoot
		usage map[string]int64
	)
	if err := d.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		roots, err = d.store.CacheRoots(ctx, tx)
		if err != nil {
			return err
		}
		rows, err := d.store.ModelDiskUsage(ctx, tx)
		if err != nil {
			return err
		}
		usage = make(map[string]int64, len(rows))
		for _, r := range rows {
			usage[r.RootID] = r.BytesOnDisk
		}
		return nil
	}); err != nil {
		return nil, err
	}

	for _, root := range roots {
		row := api.DiskUsageDTO{Path: root.Path, Kind: "cache_root"}
		primary := root.IsPrimary
		row.Primary = &primary
		if u, err := hw.DiskUsage(root.Path); err == nil {
			row.TotalBytes = int64(u.TotalBytes)
			row.FreeBytes = int64(u.FreeBytes)
			row.UsedBytes = int64(u.UsedBytes)
		}
		if b, ok := usage[root.ID]; ok {
			row.ModelBytes = &b
		}
		out = append(out, row)
	}

	state := api.DiskUsageDTO{Path: d.stateDir, Kind: "state_dir"}
	if u, err := hw.DiskUsage(d.stateDir); err == nil {
		state.TotalBytes = int64(u.TotalBytes)
		state.FreeBytes = int64(u.FreeBytes)
		state.UsedBytes = int64(u.UsedBytes)
	}
	if b, err := dirBytes(filepath.Join(d.stateDir, "versions")); err == nil {
		state.VersionBytes = &b
	}
	out = append(out, state)
	return out, nil
}

// dirBytes sums a directory tree's apparent size. It walks rather than shelling
// out to du, and it never follows a symlink out of the tree — a versions
// directory containing a link to /usr would otherwise report the whole system.
func dirBytes(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, entry os.DirEntry, err error) error {
		if err != nil {
			// A directory that vanished mid-walk (a delete running beside us) is
			// not a reason to fail the whole measurement.
			return nil
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total, err
}

/* -- units ------------------------------------------------------------------ */

// Units is section 5.4a's drift check, read-only.
//
// `stale` is the ORDINARY state of a host that has self-updated across a release
// which changed a template, and it blocks nothing (D95). Only `missing` and a
// hash mismatch at the CURRENT stamp are F16, and only those get a repair
// command — offering one for a stale stamp would send users to run install-units
// after every release for no reason.
func (s *systemAPI) Units(ctx context.Context) ([]api.UnitStatusDTO, error) {
	d := s.d
	spec := systemd.Spec{Scope: d.scope, Identity: currentIdentity()}
	names := systemd.UnitNames(d.scope)
	dir := systemd.UnitDir(d.scope)

	out := make([]api.UnitStatusDTO, 0, len(names))
	for _, name := range names {
		rendered, err := spec.RenderUnit(name)
		if err != nil {
			// A template this binary cannot render is a build bug, not a host
			// condition; report the unit as unknown rather than dropping it, so
			// the screen shows that something is wrong with it.
			out = append(out, api.UnitStatusDTO{Unit: name, Drift: "missing",
				WantsDiff: []string{}, RequiresDiff: []string{}, MaskedDiff: []string{}})
			continue
		}

		row := api.UnitStatusDTO{
			Unit: name, WantsDiff: []string{}, RequiresDiff: []string{}, MaskedDiff: []string{},
		}
		if stamp, ok := systemd.Stamp(rendered); ok {
			row.TemplateStamp = int64(stamp)
		}

		content, found := readUnit(filepath.Join(dir, name))
		if found {
			if stamp, ok := systemd.Stamp(content); ok {
				n := int64(stamp)
				row.InstalledStamp = &n
			}
		}
		row.Drift = string(systemd.Classify(content, found, rendered))
		row.WantsDiff = directiveDiff(content, rendered, "Wants")
		row.RequiresDiff = directiveDiff(content, rendered, "Requires")
		if !found {
			row.MaskedDiff = []string{}
		} else if isMasked(filepath.Join(dir, name)) {
			row.MaskedDiff = []string{name}
		}

		// Only an actionable drift gets a command. `stale` is not actionable —
		// see this method's doc comment.
		if row.Drift == string(systemd.DriftMissing) ||
			(row.Drift != string(systemd.DriftNone) && row.Drift != string(systemd.DriftStale)) {
			cmd := "sudo llamaman install-units --identity " + currentIdentity()
			row.RepairCommand = &cmd
		}
		out = append(out, row)
	}
	return out, nil
}

func readUnit(path string) (string, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return string(b), true
}

func isMasked(path string) bool {
	fi, err := os.Lstat(path)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		return false
	}
	target, err := os.Readlink(path)
	return err == nil && target == os.DevNull
}

// directiveDiff names the values of one directive the installed file and the
// rendered template disagree about, in both directions — F21's "*.wants /
// *.requires / masked-unit diff".
func directiveDiff(installed, rendered, key string) []string {
	have := systemd.Directives(installed)[key]
	want := systemd.Directives(rendered)[key]

	set := func(vs []string) map[string]struct{} {
		m := make(map[string]struct{}, len(vs))
		for _, v := range vs {
			for _, part := range strings.Fields(v) {
				m[part] = struct{}{}
			}
		}
		return m
	}
	a, b := set(have), set(want)

	diff := []string{}
	for v := range a {
		if _, ok := b[v]; !ok {
			diff = append(diff, "-"+v)
		}
	}
	for v := range b {
		if _, ok := a[v]; !ok {
			diff = append(diff, "+"+v)
		}
	}
	sort.Strings(diff)
	return diff
}

/* -- journal ---------------------------------------------------------------- */

// Journal is D77's refusal-or-answer. An empty stream and a denied one must not
// look alike, so a host whose identity cannot read the journal is told so,
// with the group to add, rather than shown a blank pane it will read as "quiet".
func (s *systemAPI) Journal(ctx context.Context, p api.JournalParams) ([]api.JournalLineDTO, error) {
	d := s.d
	if d.systemd.Journal != model.JournalOK {
		return nil, model.Error{
			Code: model.CodeJournalUnavailable,
			Message: "this daemon's identity cannot read the journal, so there is nothing to " +
				"show — which is not the same as there being nothing to see",
			Details: map[string]any{"journal_read": string(d.systemd.Journal), "hints": []string{
				"sudo usermod -aG systemd-journal " + currentIdentity(),
				"sudo systemctl restart llamaman.service",
			}},
		}
	}

	unit := strings.TrimSpace(p.Unit)
	if unit == "" {
		unit = selfupdate.DaemonUnit
	}
	lines := p.Lines
	if lines <= 0 || lines > 5000 {
		lines = 500
	}

	entries, err := systemd.Tail(ctx, systemd.JournalOptions{
		Scope: d.scope, Units: []string{unit}, Lines: lines,
	})
	if err != nil {
		if errors.Is(err, systemd.ErrJournalUnavailable) {
			return nil, model.Error{
				Code:    model.CodeJournalUnavailable,
				Message: "journalctl is not available on this host",
				Details: map[string]any{"unit": unit},
			}
		}
		return nil, err
	}
	return journalDTOs(entries), nil
}

func journalDTOs(entries []systemd.Entry) []api.JournalLineDTO {
	out := make([]api.JournalLineDTO, 0, len(entries))
	for _, e := range entries {
		row := api.JournalLineDTO{At: api.Time(e.Realtime.UnixMilli()), Message: e.Message}
		if e.Unit != "" {
			u := e.Unit
			row.Unit = &u
		}
		pri := e.Priority
		row.Priority = &pri
		out = append(out, row)
	}
	return out
}

/* -- notifications ---------------------------------------------------------- */

func (s *systemAPI) Notifications(ctx context.Context) ([]api.NotificationDTO, error) {
	var rows []store.Notification
	if err := s.d.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		rows, err = s.d.store.Notifications(ctx, tx, store.NotificationFilter{})
		return err
	}); err != nil {
		return nil, err
	}

	out := make([]api.NotificationDTO, 0, len(rows))
	for _, n := range rows {
		row := api.NotificationDTO{
			ID:        n.ID,
			Severity:  string(n.Severity),
			Code:      n.Code,
			Title:     n.Title,
			Message:   n.Body,
			Hints:     notificationHints(n),
			CreatedAt: api.Time(n.At),
		}
		if n.SubjectType != nil && n.SubjectID != nil {
			row.Subject = &api.SubjectDTO{Type: *n.SubjectType, ID: *n.SubjectID}
		}
		row.DismissedAt = api.TimePtr(n.DismissedAt)
		out = append(out, row)
	}
	return out, nil
}

// notificationHints pulls the commands out of `action_json`.
//
// The column is a free-form blob written by several producers, so this reads the
// two shapes they use — a `commands` array and a single `command` string — and
// returns an empty list for anything else rather than guessing. A card with no
// commands is a card that is only telling you something, which is a legitimate
// kind of card.
func notificationHints(n store.Notification) []string {
	out := []string{}
	if n.ActionJSON == nil {
		return out
	}
	var payload struct {
		Command  string   `json:"command"`
		Commands []string `json:"commands"`
		Hints    []string `json:"hints"`
	}
	if err := json.Unmarshal([]byte(*n.ActionJSON), &payload); err != nil {
		return out
	}
	if payload.Command != "" {
		out = append(out, payload.Command)
	}
	out = append(out, payload.Commands...)
	out = append(out, payload.Hints...)
	return out
}

func (s *systemAPI) DismissNotification(ctx context.Context, id string) error {
	now := s.d.opts.Now().UnixMilli()
	return s.d.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		if _, err := s.d.store.Notification(ctx, tx, id); err != nil {
			return err
		}
		_, err := s.d.store.DismissNotification(ctx, tx, id, now)
		return err
	})
}

/* -- restart ---------------------------------------------------------------- */

// RestartMinUptime is D93's window: this boot must have been ready for at least
// this long before a restart is spent, because `StartLimitBurst=` counts starts
// and the revert deadline needs one left.
const RestartMinUptime = 60 * time.Second

// RestartDrainSec is how long section 9.4 drains in-flight gateway requests.
const RestartDrainSec = 20

func (s *systemAPI) Restart(ctx context.Context) (api.RestartDTO, error) {
	d := s.d

	// The 409s are evaluated first and the 429 last, which is section 3.3's
	// explicit ordering: "The 409 wins, and the 429 has exactly one reason." A
	// host that refuses the grant refuses the restart too and never reaches the
	// rate-limit branch, because there is nothing for the endpoint to spend.
	if d.systemd.ControlKind == model.ControlUnavailable || d.systemd.Control == nil {
		return api.RestartDTO{}, model.Error{
			Code:    model.CodeRestartUnavailable,
			Message: "no service manager is reachable, so this daemon cannot restart itself",
			Details: map[string]any{"hints": []string{
				"sudo systemctl restart " + selfupdate.DaemonUnit}},
		}
	}
	if p := d.systemd.Polkit; p != nil && !p.ManageUnits {
		return api.RestartDTO{}, model.Error{
			Code: model.CodeSystemdDenied,
			Message: "this host's polkit rules refuse the daemon permission to restart its own " +
				"unit",
			Details: map[string]any{"hints": []string{
				"sudo systemctl restart " + selfupdate.DaemonUnit,
				"sudo llamaman install-units --repair-polkit",
			}},
		}
	}

	live, err := d.liveJobBlocking(ctx)
	if err != nil {
		return api.RestartDTO{}, err
	}
	if live != "" {
		return api.RestartDTO{}, model.Error{
			Code:    model.CodeJobInFlight,
			Message: "a " + live + " job is running; restarting now would interrupt it",
			Details: map[string]any{"job_kind": live},
		}
	}

	if uptime := d.opts.Now().Sub(s.startedAt); uptime < RestartMinUptime {
		remaining := RestartMinUptime - uptime
		return api.RestartDTO{}, model.Error{
			Code: model.CodeRestartRateLimited,
			Message: fmt.Sprintf("this daemon has been ready for %ds; restarting again now "+
				"could exhaust the unit's start limit and leave the host with no daemon",
				int(uptime.Seconds())),
			Details: map[string]any{"retry_after_ms": remaining.Milliseconds()},
		}
	}

	continuity := model.ContinuityNone
	if d.gateway != nil {
		continuity = d.gateway.Continuity()
	}

	// The order of section 9.4: this response is flushed FIRST, and the restart
	// is asked for afterwards from a goroutine, because RestartNoWait on our own
	// unit means systemd may kill this process before a synchronous handler
	// could write anything at all.
	go d.restartSelf()

	return api.RestartDTO{
		Unit:               selfupdate.DaemonUnit,
		ListenerContinuity: string(continuity),
		DrainSec:           RestartDrainSec,
	}, nil
}

/* -- diagnostics ------------------------------------------------------------ */

// Diagnostics writes the D50 bundle into the state directory's `tmp` and hands
// back the path. The handler streams it and removes it.
func (s *systemAPI) Diagnostics(ctx context.Context) (api.DiagnosticsBundle, error) {
	d := s.d
	now := d.opts.Now()

	opt := diagnostics.Options{
		Now:           now,
		DB:            d.store,
		Scope:         d.scope,
		Identity:      currentIdentity(),
		BuildLogDir:   filepath.Join(d.stateDir, "logs", "build"),
		JournalTail:   systemd.Tail,
		DaemonVersion: buildinfo.Version,
		DaemonCommit:  buildinfo.Commit,
		DaemonChannel: buildinfo.Channel,
	}
	if d.secrets != nil {
		opt.Secrets = d.secrets
	}
	if d.settings != nil {
		opt.Registry = d.settings.Registry()
	}

	files, err := diagnostics.Build(ctx, opt)
	if err != nil {
		return api.DiagnosticsBundle{}, err
	}

	dir := filepath.Join(d.stateDir, "tmp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return api.DiagnosticsBundle{}, fmt.Errorf("create %s: %w", dir, err)
	}
	name := fmt.Sprintf("llamaman-diagnostics-%s.tar.gz", now.UTC().Format("20060102-150405"))
	path := filepath.Join(dir, name)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return api.DiagnosticsBundle{}, fmt.Errorf("create %s: %w", path, err)
	}
	if err := diagnostics.WriteTarGz(f, files, now); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return api.DiagnosticsBundle{}, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return api.DiagnosticsBundle{}, err
	}
	return api.DiagnosticsBundle{Path: path, Filename: name}, nil
}

/* -- the restart's two halves ----------------------------------------------- */

// restartSelf is the signal `POST /api/v1/system/restart` sends after its 202
// has been written.
//
// It only ASKS. The serve loop owns the listeners section 9.4 pauses, drains and
// hands to the fd store, so a handler that called RestartNoWait itself would
// drop every gateway port the moment systemd sent SIGTERM — the exact
// discontinuity section 9.4 exists to prevent. The send is non-blocking, so a
// second click while a restart is already in flight is a no-op rather than a
// second drain.
func (d *daemon) restartSelf() {
	select {
	case d.restart <- struct{}{}:
	default:
	}
}

// runRestart is section 9.4's ordered stop followed by a non-blocking
// RestartUnit on our own unit.
//
// RestartNoWait, not Restart: a start job on this unit does not complete until
// this process has died and the new one is ready, so a synchronous call would
// wait on itself until systemd's job timeout. Instances are untouched
// throughout — `llamaman-instance@.service` units are independent of this one
// (section 5.5), which is what makes a management restart something a user can
// do while models are serving traffic.
func (d *daemon) runRestart(ctx context.Context, errc <-chan error) error {
	err := d.shutdown(errc)

	if d.systemd.Control == nil {
		// The endpoint's own guard makes this unreachable in practice; it is
		// here because a control channel that dropped between the guard and the
		// drain must exit rather than hang, and the unit's Restart=always is
		// what brings the daemon back.
		d.log.Error("no service manager to restart this unit", "unit", systemd.UnitDaemon)
		return err
	}

	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), swapSummonTimeout)
	defer cancel()
	if _, restartErr := d.systemd.Control.RestartNoWait(rctx, systemd.UnitDaemon); restartErr != nil {
		d.log.Error("could not restart this unit", "unit", systemd.UnitDaemon, "error", restartErr)
		return err
	}
	d.log.Info("restart requested; exiting for the new process", "unit", systemd.UnitDaemon)
	return err
}

// liveJobBlocking names the job kind that makes a restart unsafe right now, or
// the empty string.
//
// It is exactly D4's pair: a llama.cpp build and a self-update. Both survive a
// restart as `interrupted` rather than being lost — that is what section 2.3's
// boot triage is for — but a build interrupted at minute eight of nine is nine
// minutes a user did not mean to spend, and a self-update interrupted mid-swap
// is the one case the revert deadline exists for. Neither is worth a click.
func (d *daemon) liveJobBlocking(ctx context.Context) (string, error) {
	if d.store == nil {
		return "", nil
	}
	var kind string
	err := d.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		for _, k := range []model.JobKind{model.JobLlamacppInstall, model.JobSelfUpdate} {
			rows, err := d.store.Jobs(ctx, tx, store.JobFilter{
				Kinds: []model.JobKind{k}, States: model.LiveJobStates(),
			})
			if err != nil {
				return err
			}
			if len(rows) > 0 {
				kind = string(k)
				return nil
			}
		}
		return nil
	})
	return kind, err
}
