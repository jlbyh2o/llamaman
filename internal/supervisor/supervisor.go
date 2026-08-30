package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jlbyh2o/llamaman/internal/hw"
	"github.com/jlbyh2o/llamaman/internal/instances"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/netutil"
	"github.com/jlbyh2o/llamaman/internal/store"
	"github.com/jlbyh2o/llamaman/internal/systemd"
)

// The reconciler's wiring: what it needs from the rest of the system, and the
// loop that drives it. The package comment in doc.go says what it is FOR; this
// file says what it is made of.
//
// Every collaborator below is an interface or a function this package declares
// rather than a concrete type it imports, and that is D49 invariant 1 in
// practice: only internal/store contains SQL, so every other package declares
// the repository interface IT needs. The same rule is why HostFacts and
// ExeResolver are functions — reading /proc is not this package's business, and
// a test that had to have a reboot to exercise the boot decision could not
// exist.

// UnitPattern is the sub-state subscription this supervisor listens on.
const UnitPattern = "llamaman-instance@*.service"

// FastPollInterval is the tick while any instance is `starting` or `loading`
// (§5.8). A model load is the one moment a user watches the state field, and a
// five-second tick makes `starting → loading → ready` look like one jump.
const FastPollInterval = time.Second

// ReadySettleSec is how long a run must stay alive after `/health` first
// answered 200 before the crash-loop window is restarted from its `ready_at`
// (D64). A minute is long enough that an instance which loads, answers once and
// dies does not clear its own history.
const ReadySettleSec = 60

// Store is the persistence this package needs. *store.Store satisfies it
// structurally — DESIGN section 1, invariant 1: only internal/store contains
// SQL, so every other package declares the repository interface it needs.
type Store interface {
	Read(ctx context.Context, fn func(context.Context, store.Tx) error) error
	Write(ctx context.Context, fn func(context.Context, store.Tx) error) error

	Instance(ctx context.Context, tx store.Tx, id string) (model.Instance, error)
	Instances(ctx context.Context, tx store.Tx, f store.InstanceFilter) ([]model.Instance, error)
	InstancePortHolders(ctx context.Context, tx store.Tx) ([]model.InstancePorts, error)
	SetInstanceDesiredState(ctx context.Context, tx store.Tx, id string, desired model.DesiredState, at int64) (bool, error)
	StampPendingStart(ctx context.Context, tx store.Tx, id string, trigger model.PendingTrigger, overrideJSON *string, at int64) (bool, error)
	ReassignInternalPort(ctx context.Context, tx store.Tx, id string, port int, at int64) (bool, error)

	InstanceStatus(ctx context.Context, tx store.Tx, instanceID string) (model.InstanceStatus, error)
	UpdateInstanceStatus(ctx context.Context, tx store.Tx, st model.InstanceStatus) (bool, error)

	// InsertFitObservation records §8.7's calibration row on a first `ready`.
	InsertFitObservation(ctx context.Context, tx store.Tx, o model.FitObservation) error

	InsertInstanceStart(ctx context.Context, tx store.Tx, r model.InstanceStart) error
	CloseInstanceStart(ctx context.Context, tx store.Tx, id string, c store.StartClosure) (bool, error)
	CloseOpenInstanceStart(ctx context.Context, tx store.Tx, instanceID string, c store.StartClosure) (bool, error)
	StampStartReady(ctx context.Context, tx store.Tx, id string, at int64) (bool, error)
	OpenInstanceStart(ctx context.Context, tx store.Tx, instanceID string) (model.InstanceStart, error)
	LastClosedInstanceStart(ctx context.Context, tx store.Tx, instanceID string) (model.InstanceStart, error)
	CountFailedStartsSince(ctx context.Context, tx store.Tx, instanceID string, after int64) (int, error)
	HasInhibitedStartSince(ctx context.Context, tx store.Tx, instanceID string, reason model.InhibitReason, after int64) (bool, error)
	InstancesWithOpenStarts(ctx context.Context, tx store.Tx) ([]string, error)

	ActiveVersion(ctx context.Context, tx store.Tx) (store.ActiveVersion, error)
	AutostartCouplings(ctx context.Context, tx store.Tx) ([]store.AutostartCoupling, error)
	RelabelBootStarts(ctx context.Context, tx store.Tx, hostBootAt, bootAt int64) (int64, error)
	RuntimeInfo(ctx context.Context, tx store.Tx) (model.RuntimeInfo, error)
	SetHostBoot(ctx context.Context, tx store.Tx, hostBootID string, hostBootAt int64) error
}

// Events is the events/SSE seam. Append belongs inside the caller's write
// transaction; Publish runs only after it commits, because a subscriber told
// about a row that then rolled back would have been told something that did not
// happen.
type Events interface {
	Append(ctx context.Context, tx store.Tx, ev model.Event) error
	Publish(ev model.Event)
}

// Settings is the typed settings this supervisor reads.
type Settings interface {
	GetInt(ctx context.Context, key string) (int64, error)
}

// HostBoot is the host's identity for this boot: the kernel's `boot_id` and the
// instant `/proc/stat`'s `btime` names.
//
// Both halves are read, and D74 explains why the instant is not the daemon's
// own `boot_at`: using the daemon start time made every ordinary daemon restart
// — including the one every self-update performs — rewrite a `systemctl start`
// typed at a shell three days ago as `autostart`.
type HostBoot struct {
	ID string
	At time.Time
}

// HostFacts answers what boot this is. It is a function rather than a package
// call so a test can simulate a reboot without one.
type HostFacts func() (HostBoot, error)

// ExeResolver answers D25's version-truth question: which llama.cpp build is
// this pid actually executing? The production implementation is a readlink of
// `/proc/<pid>/exe`.
//
// The same answer is the GC guard: a version directory is never deleted while
// any live process executes from it.
type ExeResolver func(pid int) (string, error)

// Enablement reports whether a unit is enabled, for the `autostart` ≠
// unit-enabled corrective action.
//
// It is a seam with a nil default because the control channel of §5.3 exposes
// no unit-file state: `Controller` can Enable and Disable, and reads typed
// RUNTIME properties, but nothing in its vocabulary answers "is this enabled".
// Rather than guess, a nil Enablement makes the supervisor apply the instance's
// declared autostart ONCE per daemon start, at boot reconciliation, which
// converges without issuing a D-Bus call per instance per tick — and the
// capability gate below still decides whether it may act at all.
type Enablement interface {
	Enabled(ctx context.Context, unit string) (bool, error)
}

// Config wires a Supervisor.
type Config struct {
	Store    Store
	Settings Settings
	Events   Events

	// Control is the systemd channel. Nil is a supported, documented mode —
	// `systemd_control='unavailable'` (F10, §11.1a) — in which the supervisor
	// observes nothing and starts nothing, and every instance reads `unknown`.
	// It never means the daemon spawns llama-server itself.
	Control systemd.Controller

	// Prober is the health probe. Nil uses NewHTTPProber.
	Prober Prober

	// StateDir is where `versions/active` lives, for D25's exe comparison.
	StateDir string

	// ManageUnitFiles gates the autostart corrective action on the same
	// capability `PUT /instances/{id}/autostart` is gated on (§11.1a): it is
	// false when `polkit_unit_files = 0`, when polkit denied the grant at boot
	// (F9), and when `systemd_control='unavailable'` (F10). Ungated, the action
	// was an unconditional D-Bus call that polkit would deny on every pass for
	// every instance — an error loop with no terminal state.
	ManageUnitFiles bool
	// Enablement observes unit enablement. Nil is documented above.
	Enablement Enablement

	// GPUs is D17's attribution source (§5.8's "per-instance VRAM and GPU
	// attribution", §8.6). The sampler joins
	// `--query-compute-apps=pid,gpu_uuid,used_gpu_memory` onto the unit's
	// MainPID and writes `instance_status.vram_bytes`, `gpu_uuids_json` and
	// `gpu_attribution`.
	//
	// Nil leaves all three columns alone — `gpu_attribution` stays at its schema
	// default `'unknown'` and `gpu_uuids_json` stays NULL — which is the honest
	// answer for a daemon with no prober, and which §10's guard reads as
	// "occupies every GPU it could occupy" rather than as "occupies none".
	GPUs hw.Prober

	// Journal reads an instance unit's recent output, for §5.8's fit
	// observation. Nil disables the observation entirely — no scan, no
	// `fit_report_json`, no `fit_observations` row — which is also what
	// `runtime_info.journal_read != 'ok'` produces (D77, F23).
	Journal Journal
	// Fit answers what the calculator predicted for an instance, so the
	// observation can be written beside it (§8.7). Nil still stamps
	// `fit_report_json` — D33's "reported by llama.cpp" panel — but writes no
	// calibration row, because a ratio needs both halves.
	Fit FitPredictor

	// Host reads the boot identity. Nil uses ProcHostFacts.
	Host HostFacts
	// Exe resolves a pid's executable. Nil uses ProcExe.
	Exe ExeResolver
	// Probe is the live bind check used when reassigning a port after exit 78.
	// Nil uses netutil.Free.
	Probe func(bind string, port int) bool

	// Now supplies every instant this supervisor stamps. Nil uses time.Now.
	Now func() time.Time
	// NewID mints row ids. Nil uses store.NewID.
	NewID func(time.Time) string
	// Logger receives pass failures. Nil uses slog.Default.
	Logger *slog.Logger
}

// Supervisor is the reconciler.
type Supervisor struct {
	cfg Config

	st       Store
	settings Settings
	events   Events
	control  systemd.Controller
	prober   Prober
	host     HostFacts
	exe      ExeResolver
	probe    func(string, int) bool
	now      func() time.Time
	newID    func(time.Time) string
	log      *slog.Logger

	mu sync.Mutex
	// healthFails counts CONSECUTIVE failed probes per instance, which is what
	// `ready → degraded` needs and what a ledger row must not record: a run that
	// recovers has one row, not three (§5.6's writer table).
	healthFails map[string]int
	// timedOut names the start rows this supervisor gave up on. It is memory
	// rather than a column because a start timeout is a decision about a run
	// that is still in flight, and the ledger records what HAPPENED to a run,
	// once, at its end (D63).
	timedOut map[string]bool
	// appliedAutostart is what enablement this daemon has already applied, for
	// the nil-Enablement case documented above.
	appliedAutostart map[string]bool
	// synthesized remembers which unit EXIT already has its §5.6 row, so a unit
	// that stays failed for an hour records one row rather than 720.
	synthesized map[string]int64
	// fast reports whether the last pass saw an instance still coming up, which
	// is what switches the tick to one second.
	fast bool
}

// New builds a Supervisor.
func New(cfg Config) (*Supervisor, error) {
	if cfg.Store == nil {
		return nil, errors.New("supervisor: Store is required")
	}
	s := &Supervisor{
		cfg:              cfg,
		st:               cfg.Store,
		settings:         cfg.Settings,
		events:           cfg.Events,
		control:          cfg.Control,
		prober:           cfg.Prober,
		host:             cfg.Host,
		exe:              cfg.Exe,
		probe:            cfg.Probe,
		now:              cfg.Now,
		newID:            cfg.NewID,
		log:              cfg.Logger,
		healthFails:      map[string]int{},
		timedOut:         map[string]bool{},
		appliedAutostart: map[string]bool{},
		synthesized:      map[string]int64{},
	}
	if s.prober == nil {
		s.prober = NewHTTPProber()
	}
	if s.host == nil {
		s.host = ProcHostFacts
	}
	if s.exe == nil {
		s.exe = ProcExe
	}
	if s.probe == nil {
		s.probe = netutil.Free
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.newID == nil {
		s.newID = store.NewID
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	return s, nil
}

// Run performs boot reconciliation and then reconciles until ctx is done.
//
// The loop is woken by two things and no others: systemd's sub-state signals,
// so a unit that died is observed as it happens rather than up to five seconds
// later, and a tick, so a health transition that systemd cannot see (a model
// that finished loading) is still noticed. The tick shortens to one second
// while anything is `starting` or `loading`.
func (s *Supervisor) Run(ctx context.Context) error {
	if err := s.BootReconcile(ctx); err != nil {
		return err
	}

	var events <-chan systemd.SubStateEvent
	if s.control != nil {
		ch, err := s.control.SubscribeSubState(ctx, UnitPattern)
		if err != nil {
			// A supervisor that cannot subscribe still polls. Losing the push
			// channel costs latency, not correctness — which is exactly the
			// trade §5.3 describes when the exec controller wins.
			s.log.Warn("supervisor: sub-state subscription unavailable, polling only",
				slog.String("error", err.Error()))
		} else {
			events = ch
		}
	}

	timer := time.NewTimer(s.interval(ctx))
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-events:
			if !ok {
				events = nil
				continue
			}
		case <-timer.C:
		}

		if err := s.Reconcile(ctx); err != nil && !errors.Is(err, context.Canceled) {
			s.log.Error("supervisor: reconcile pass failed", slog.String("error", err.Error()))
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(s.interval(ctx))
	}
}

// OnReconnect is the callback §5.3 requires the consumer to supply: after the
// bus connection is re-established, every managed unit's properties are
// resynchronized and reconciled against the database BEFORE event processing
// resumes. Wire it into systemd.Options.OnReconnect.
func (s *Supervisor) OnReconnect(ctx context.Context) func() {
	return func() {
		if err := s.Reconcile(ctx); err != nil {
			s.log.Error("supervisor: resynchronization after reconnect failed",
				slog.String("error", err.Error()))
		}
	}
}

// interval is the tick for the next wait.
func (s *Supervisor) interval(ctx context.Context) time.Duration {
	s.mu.Lock()
	fast := s.fast
	s.mu.Unlock()
	if fast {
		return FastPollInterval
	}
	return time.Duration(s.settingInt(ctx, "instances.health_poll_sec", 5)) * time.Second
}

// Reconcile runs one pass over the subject set.
//
// The set is every instance with `deleted_at IS NULL`, PLUS every instance with
// an open `instance_starts` row. The second term is one lookup on
// `idx_instance_starts_open` and exists so that a soft delete's own StopUnit
// (§3.10c) is ledgered by the supervisor — the only writer allowed to close
// that row — before the row is forgotten.
func (s *Supervisor) Reconcile(ctx context.Context) error {
	subjects, err := s.subjects(ctx)
	if err != nil {
		return err
	}

	active, activeErr := s.activeVersion(ctx)
	runtimeReady := activeErr == nil && active.Ready()

	fast := false
	var firstErr error
	for _, inst := range subjects {
		coming, err := s.pass(ctx, inst, active, runtimeReady)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			s.log.Error("supervisor: instance pass failed",
				slog.String("instance", inst.Name), slog.String("error", err.Error()))
			continue
		}
		fast = fast || coming
	}

	s.mu.Lock()
	s.fast = fast
	s.mu.Unlock()
	return firstErr
}

// subjects reads the reconcile set.
func (s *Supervisor) subjects(ctx context.Context) ([]model.Instance, error) {
	var out []model.Instance
	err := s.st.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		live, err := s.st.Instances(ctx, tx, store.InstanceFilter{})
		if err != nil {
			return err
		}
		seen := make(map[string]struct{}, len(live))
		for _, i := range live {
			seen[i.ID] = struct{}{}
		}
		out = live

		open, err := s.st.InstancesWithOpenStarts(ctx, tx)
		if err != nil {
			return err
		}
		for _, id := range open {
			if _, dup := seen[id]; dup {
				continue
			}
			// A soft-deleted instance with a row still open. It is in the set
			// for exactly one more pass: long enough to ledger the stop, never
			// long enough to be started again — `desired_state` was set to
			// `stopped` by the delete, and the launcher would exit 64 anyway.
			inst, err := s.st.Instance(ctx, tx, id)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					continue
				}
				return err
			}
			seen[id] = struct{}{}
			out = append(out, inst)
		}
		return nil
	})
	return out, err
}

// activeVersion reads the `is_active=1` build. A host with no active build is
// an ordinary state on a fresh install, not an error: nothing can be started,
// and the reconciler says so by taking no start action rather than by failing.
func (s *Supervisor) activeVersion(ctx context.Context) (store.ActiveVersion, error) {
	var v store.ActiveVersion
	err := s.st.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		got, err := s.st.ActiveVersion(ctx, tx)
		v = got
		return err
	})
	return v, err
}

// settingInt reads a settings key, falling back to the design's default when no
// settings source is wired or the key cannot be read. A supervisor that stopped
// reconciling because a settings lookup failed would be a daemon that takes
// inference down over a configuration read.
func (s *Supervisor) settingInt(ctx context.Context, key string, def int64) int64 {
	if s.settings == nil {
		return def
	}
	v, err := s.settings.GetInt(ctx, key)
	if err != nil || v <= 0 {
		return def
	}
	return v
}

// ProcHostFacts reads this host's boot identity from procfs: the kernel's
// `boot_id`, and `/proc/stat`'s `btime` field as the boot instant.
//
// btime is seconds since the epoch and is stored ×1000 beside `host_boot_id`
// (D74). It is the HOST boot instant, which is the only clock that makes the
// one relabel of §5.8 correct.
func ProcHostFacts() (HostBoot, error) {
	id, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return HostBoot{}, fmt.Errorf("supervisor: read boot_id: %w", err)
	}
	stat, err := os.ReadFile("/proc/stat")
	if err != nil {
		return HostBoot{}, fmt.Errorf("supervisor: read /proc/stat: %w", err)
	}
	for _, line := range strings.Split(string(stat), "\n") {
		rest, ok := strings.CutPrefix(line, "btime ")
		if !ok {
			continue
		}
		secs, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
		if err != nil {
			return HostBoot{}, fmt.Errorf("supervisor: parse btime %q: %w", rest, err)
		}
		return HostBoot{ID: strings.TrimSpace(string(id)), At: time.Unix(secs, 0)}, nil
	}
	return HostBoot{}, errors.New("supervisor: /proc/stat has no btime line")
}

// ProcExe is D25's readlink: which binary is this pid executing?
func ProcExe(pid int) (string, error) {
	return os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
}

// activeDir is the concrete directory `versions/active` resolves to, for D25's
// comparison. An unresolvable symlink is not an error the supervisor reports:
// it simply means the comparison cannot be made this pass, and
// `exe_version_id` is left as it was.
func (s *Supervisor) activeDir() string {
	if s.cfg.StateDir == "" {
		return ""
	}
	dir, err := filepath.EvalSymlinks(filepath.Join(s.cfg.StateDir, "versions", "active"))
	if err != nil {
		return ""
	}
	return dir
}

// unitName is the unit for an instance. `instances.unit_name` is authoritative
// — it is a stored column — and the template is the fallback for a row written
// before it was populated.
func unitName(inst model.Instance) string {
	if inst.UnitName != "" {
		return inst.UnitName
	}
	return instances.UnitName(inst.Name)
}
