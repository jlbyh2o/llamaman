package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/jlbyh2o/llamaman/internal/api"
	"github.com/jlbyh2o/llamaman/internal/auth"
	"github.com/jlbyh2o/llamaman/internal/bench"
	"github.com/jlbyh2o/llamaman/internal/buildinfo"
	"github.com/jlbyh2o/llamaman/internal/events"
	"github.com/jlbyh2o/llamaman/internal/gateway"
	"github.com/jlbyh2o/llamaman/internal/hf"
	"github.com/jlbyh2o/llamaman/internal/hf/download"
	"github.com/jlbyh2o/llamaman/internal/hw"
	"github.com/jlbyh2o/llamaman/internal/instances"
	"github.com/jlbyh2o/llamaman/internal/jobs"
	"github.com/jlbyh2o/llamaman/internal/llamacpp"
	"github.com/jlbyh2o/llamaman/internal/llamacpp/github"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/models"
	"github.com/jlbyh2o/llamaman/internal/netutil"
	"github.com/jlbyh2o/llamaman/internal/secrets"
	"github.com/jlbyh2o/llamaman/internal/selfupdate"
	"github.com/jlbyh2o/llamaman/internal/settings"
	"github.com/jlbyh2o/llamaman/internal/setup"
	"github.com/jlbyh2o/llamaman/internal/sse"
	"github.com/jlbyh2o/llamaman/internal/store"
	"github.com/jlbyh2o/llamaman/internal/supervisor"
	"github.com/jlbyh2o/llamaman/internal/tokens"
	"github.com/jlbyh2o/llamaman/internal/web"
)

// daemon is everything one `llamaman serve` owns. It is assembled by boot in
// section 11.1's order and torn down by close in the reverse of it.
type daemon struct {
	opts Options
	log  *slog.Logger

	stateDir string
	scope    model.SystemdScope
	bootID   string

	releaseLock func() error
	store       *store.Store
	settings    *settings.Cache
	hub         *events.Hub
	recorder    *events.Recorder
	queue       *jobs.Queue
	secrets     *secrets.Service
	hfClient    *hf.Client
	auth        *auth.Service
	setup       *setup.Service
	instances   *instances.Service
	models      *models.Service
	downloads   *download.Service
	supervisor  *supervisor.Supervisor
	llamacpp    *llamacpp.Service
	bench       *bench.Service
	tokens      *tokens.Service
	gateway     *gateway.Gateway
	releases    *github.Client

	// updateGate is section 12.3's ResolveUpdateMarkers, built at step 4 —
	// BEFORE the migrations, because D92's disarm runs from the migration
	// runner's BeforeFirst hook and the gate that unlinked the marker is the one
	// that must resolve the in-memory copy of it at step 11.
	updateGate *selfupdate.Gate
	// selfupdate is the daemon's half of section 12: the apply endpoint's guard
	// and the pipeline behind it.
	selfupdate *selfupdate.Service
	// swap is section 12.1 step 7's signal from the worker to the serve loop. It
	// is buffered by one and the send is non-blocking, so it is the whole state
	// machine: a second BeginSwap on a daemon that is already swapping is a
	// no-op rather than a second drain.
	swap chan struct{}
	// restart is `POST /api/v1/system/restart`'s signal to the serve loop
	// (section 3.3, section 9.4). It is the same shape as swap and for the same
	// reason: the loop owns the listeners the ordered stop pauses, drains and
	// hands to the fd store, so the endpoint can only ASK. Buffered by one with
	// a non-blocking send, so a second click while a restart is already in
	// flight is a no-op rather than a second drain.
	restart chan struct{}

	// gpus is step 6's GPU probe, shared by the supervisor's D17 attribution,
	// the bench exclusivity guard and the fit calculator's host inputs. See
	// hardware.go for why there is exactly one.
	gpus *hw.NvidiaSMIProber

	// systemd is what step 6's probe learned: the control channel, the two
	// polkit answers and journal readability. Its zero value is the F10
	// degraded mode, which is a mode this daemon serves in rather than refuses
	// to start in (section 11.1a).
	systemd SystemdEnv
	// resync is the control channel's reconnect callback, filled in once the
	// supervisor exists (section 5.3).
	resync resyncSlot

	listener net.Listener
	uiPort   int
	uiBind   string
	server   *http.Server
}

// boot runs steps 1 through 10 of DESIGN section 11.1 for the subsystems that
// exist. The steps that belong to subsystems still to be built are marked and
// skipped rather than faked, and each one says which package will own it:
//
//	step 5  secret.key                 internal/secrets
//	step 6  environment probe          internal/systemd, internal/hw, internal/toolchain, internal/hf/cache
//	step 7  port-walk exclusions       internal/netutil (the walk itself is here, without the instance exclusions)
//	step 8  setup claim                internal/auth
//	step 9  host boot id               internal/supervisor (this step reads; section 5.8 writes)
//	step 10 fd-store adoption          internal/systemd (listener_continuity is recorded as 'none' until then)
//	step 11 self-update gate           internal/selfupdate (the D92 disarm hook is wired here already)
//	step 12 workers and ResetFailed    the subsystems that own each worker
func boot(ctx context.Context, opts Options, f flags) (*daemon, error) {
	log := opts.Logger
	d := &daemon{opts: opts, log: log,
		swap: make(chan struct{}, 1), restart: make(chan struct{}, 1)}

	// --- step 1: scope, then state directory. The order matters: the
	// state-directory fallback chain branches on the scope.
	scope, err := resolveScope(f.scope, opts.ScopeProbe)
	if err != nil {
		return nil, err
	}
	d.scope = scope
	d.stateDir = resolveStateDir(scope, opts.StateDirOverride, opts.Getenv)
	if err := ensureStateDir(d.stateDir); err != nil {
		return nil, err
	}
	log.Info("resolved runtime paths", "scope", scope, "state_dir", d.stateDir)

	// A resolved scope of `user` with euid 0, or `system` with a state
	// directory under $HOME, is not fatal but is reported: it almost always
	// means the units and the binary came from different installs. Recording
	// it in runtime_info.polkit_detail belongs to the step 6 probe; saying it
	// out loud costs nothing and is useful now.
	if mismatch := scopeMismatch(scope, d.stateDir, opts.Getenv); mismatch != "" {
		log.Warn("systemd scope and state directory disagree", "detail", mismatch)
	}

	// --- step 2: flock the state directory, before anything opens a file in it.
	release, err := lockStateDir(d.stateDir)
	if err != nil {
		return nil, err
	}
	d.releaseLock = release

	// --- step 3: open the database, enforce 0600, integrity_check.
	dbPath := filepath.Join(d.stateDir, DatabaseFileName)
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		d.close()
		return nil, fmt.Errorf("open %s: %w", dbPath, err)
	}
	d.store = st
	if err := enforceDBMode(dbPath); err != nil {
		d.close()
		return nil, fmt.Errorf("enforce 0600 on %s: %w", dbPath, err)
	}

	// The heartbeat is the reason a slow integrity check or migration extends
	// TimeoutStartSec= instead of tripping it (D88, section 5.4). It is wired
	// here and at the migration below, which are the two places section 11.1
	// step 4 names.
	heartbeat := func() {
		if err := opts.Notifier.ExtendTimeout(2 * store.DefaultHeartbeatEvery); err != nil {
			log.Debug("could not extend the start timeout", "error", err)
		}
	}
	if err := st.IntegrityCheck(ctx, store.CheckOptions{Heartbeat: heartbeat}); err != nil {
		// F12's recovery — move the file aside, restore the newest
		// db-backups/ entry, else start fresh, and raise a notification
		// listing what was lost — needs the backup rotation that internal/app
		// does not yet write. Refusing to start on a corrupt database is the
		// safe half of that behavior and is what the revert of section 12.2
		// expects to see; the recovery arm lands with db-backups/.
		d.close()
		return nil, fmt.Errorf("integrity check failed on %s: %w", dbPath, err)
	}

	// --- step 4: the schema gate, the D92 disarm, then the migrations.
	//
	// The gate is constructed FIRST, and it is the one subsystem that has to be:
	// BeforeFirst below is the D92 disarm, and there is no ordering in which a
	// migration may run before the thing that unlinks `update/pending` exists.
	// It needs nothing but the store and the state directory, both of which are
	// resolved by now; everything else about self-update is built with the rest
	// of the services further down.
	d.updateGate = selfupdate.NewGate(selfupdate.GateConfig{
		Store:   st,
		Layout:  d.updateLayout(),
		Version: buildinfo.Version,
		Now:     opts.Now,
		Log:     log,
	})

	applied, err := st.Migrate(ctx, store.MigrateOptions{
		Now:         opts.Now,
		Heartbeat:   heartbeat,
		BeforeFirst: d.disarmRevert,
	})
	if err != nil {
		d.close()
		var ahead *store.SchemaAheadError
		if errors.As(err, &ahead) {
			// Section 11.1 step 4: one journald line naming both versions, and
			// a non-zero exit. Running a v14 query set against a v15 schema can
			// corrupt data, and there is no forward-only migration that undoes
			// one.
			log.Error("refusing to open a database written by a newer release",
				"database_version", ahead.DBVersion, "binary_version", ahead.BinaryVersion,
				"remediation", "reinstall the newer binary, or complete the downgrade with `llamaman restore-db` (DESIGN section 12.4)")
		}
		return nil, err
	}
	if len(applied) > 0 {
		log.Info("applied migrations", "count", len(applied),
			"through", applied[len(applied)-1].Filename())
	}

	// --- step 5 (settings half): built-in defaults plus `settings` rows.
	d.settings = settings.New(settings.NewRegistry(), st)
	if err := d.settings.Load(ctx); err != nil {
		d.close()
		return nil, fmt.Errorf("load settings: %w", err)
	}

	// --- step 5 (secrets half): `<state_dir>/secret.key` and the sealed
	// credential store over it, then the Hugging Face client that reads the
	// token through it on every request rather than capturing one here.
	// Settings first, because the client's endpoint is one.
	if err := d.buildSecrets(); err != nil {
		d.close()
		return nil, err
	}
	d.buildHub()

	// --- step 5 (identity half): the authentication authority and the wizard.
	// They are constructed before the port walk because step 8's setup claim
	// needs the first one, and because `GET /api/v1/meta` — which install.sh
	// polls from the moment the listener is up — reads the second.
	authSvc, err := auth.New(auth.Config{
		Repo:     st,
		Settings: d.settings,
		StateDir: d.stateDir,
		Now:      opts.Now,
		Logger:   log,
	})
	if err != nil {
		d.close()
		return nil, err
	}
	d.auth = authSvc

	setupSvc, err := setup.New(setup.Config{Repo: st, Claimer: authSvc, Now: opts.Now})
	if err != nil {
		d.close()
		return nil, err
	}
	d.setup = setupSvc

	// --- step 6: the environment probe. internal/hw, internal/toolchain and
	// internal/hf/cache own the GPU, toolchain and cache-root halves; the
	// systemd half is here.
	//
	// The scope was settled in step 1 and is NOT re-derived. What this learns —
	// the control channel, whether `manage-units` and `manage-unit-files` were
	// granted, and whether this identity can read the journal — is persisted at
	// step 10 and read thereafter from `runtime_info` by sections 3.3, 5.8 and
	// 11.1a. An unreachable systemd is step 6a's degraded mode, never a refusal
	// to start.
	if opts.Systemd != nil {
		d.systemd = opts.Systemd(ctx, SystemdOptions{
			Scope:       scope,
			Logger:      log,
			OnReconnect: d.resynchronize,
		})
		log.Info("probed the service manager",
			"systemd_control", string(d.systemd.ControlKind),
			"journal_read", string(d.systemd.Journal),
			"polkit", d.systemd.PolkitDetail)
	}

	// The GPU half of the same probe. It is built here, with the rest of step 6,
	// so that every subsystem constructed below shares one sample cache and one
	// memory of whether this driver's nvidia-smi accepts `gpu_uuid` (section
	// 8.6).
	d.buildHardware()

	// --- step 6b: port preference from the unit, resolved once. The flag is a
	// SEED, never an override, so the database stays the single source of
	// truth SPEC section 3.9 requires.
	if err := d.seedPortFromFlag(ctx, f.port); err != nil {
		d.close()
		return nil, err
	}

	// --- step 7: the port walk.
	if err := d.walkPort(ctx); err != nil {
		d.close()
		return nil, err
	}

	// --- step 8: the setup claim (§2.2a, D38/D59). internal/auth mints the
	// token, writes the 0600 file, removes a stale one and rotates a missing
	// one; this step's own job is the announcement, which happens exactly once,
	// on a mint, at info.
	if err := d.announceSetupToken(ctx); err != nil {
		d.close()
		return nil, err
	}

	// --- step 9: the host boot id is read by internal/supervisor, which is
	// also the ONE writer of those two columns (D53, section 5.8). This step
	// deliberately does nothing.

	// --- step 10: adopt the listeners systemd held in its fd store. Until
	// internal/systemd lands sd_notify FDSTORE=1, every boot rebinds, and that
	// is recorded honestly rather than assumed away: `listener_continuity` is
	// 'none' and GET /system/capabilities will say so (section 9.4, "nothing
	// silently degrades").
	if err := d.writeRuntimeInfo(ctx); err != nil {
		d.close()
		return nil, err
	}

	// --- construct the services.
	d.hub = events.NewHub(0)
	d.recorder = events.NewRecorder(st, d.hub)

	// The instance service, and the two collaborators it declares. Its argv
	// renderer is pure, so the version directory is resolved from the row's
	// `dir_name` under `<state_dir>/versions` rather than from a symlink read.
	instSvc, err := instances.New(instances.Config{
		Store:    st,
		Settings: d.settings,
		Resolver: modelResolver{st: st, versionsDir: filepath.Join(d.stateDir, VersionsDirName)},
		Runtime:  st,
		Events:   d.recorder,
		Deactivator: deactivator{
			control:         d.systemd.Control,
			manageUnitFiles: d.systemd.ManageUnitFiles(d.scope),
		},
		Now: opts.Now,
	})
	if err != nil {
		d.close()
		return nil, err
	}
	d.instances = instSvc

	// The supervisor (section 5.8). It is constructed even in the F10 degraded
	// mode: a nil Control is a documented, supported configuration in which it
	// observes nothing and starts nothing — and its BOOT reconciliation is
	// still the only writer of `runtime_info.host_boot_id`/`host_boot_at` and
	// the only place D53's autostart coupling fires.
	sup, err := supervisor.New(supervisor.Config{
		Store:           st,
		Settings:        d.settings,
		Events:          d.recorder,
		Control:         d.systemd.Control,
		StateDir:        d.stateDir,
		ManageUnitFiles: d.systemd.ManageUnitFiles(d.scope),
		// D17's attribution source. Without it `instance_status.gpu_uuids_json`
		// is never populated and section 10's bench exclusivity guard has
		// nothing to intersect — it would still fail closed, but it could never
		// take the `measured` path D17 exists for.
		GPUs: d.gpus,
		// Section 5.8's fit observation (D33) and D32's calibration input. The
		// journal is what llama.cpp's own buffer sizes are read from, and the
		// predictor is what they are recorded BESIDE; a nil journal writes
		// neither `fit_report_json` nor a `fit_observations` row, so the
		// ground-truth panel is blank and the calibration never reaches its
		// three-sample floor.
		//
		// A nil Journal here is the honest F10/F23 answer rather than a gap: the
		// probe reports `journal_read` and observeFit re-checks it per
		// observation, so an identity that cannot read the journal degrades
		// loudly instead of calibrating from a scan of nothing.
		Journal: journalTail{scope: scope},
		Fit:     fitPredictor{st: st, gpus: d.gpus},
		Now:     opts.Now,
		Logger:  log,
	})
	if err != nil {
		d.close()
		return nil, err
	}
	d.supervisor = sup

	bootID, err := d.currentBootID(ctx)
	if err != nil {
		d.close()
		return nil, err
	}
	d.bootID = bootID

	q, err := jobs.New(st, jobs.Options{
		BootID: bootID, Now: opts.Now, Logger: log,
		// Section 3.14's `jobs` topic. Without a publisher every screen that
		// narrates work — the wizard's llama.cpp step, the Downloads queue, the
		// build log, the dashboard's active-jobs strip — shows whatever it read
		// when it mounted, for as long as it stays open.
		Publisher: jobPublisher{rec: d.recorder},
	})
	if err != nil {
		d.close()
		return nil, err
	}
	d.queue = q

	// The llama.cpp lifecycle service and its three workers, plus the nightly
	// maintenance worker. They are registered HERE rather than in serve()
	// because §2.3's boot triage looks a worker up in the registry to move its
	// domain row in the same transaction as the job row: a build interrupted by
	// the previous boot must find its DomainWriter already in place.
	// The bench runner is constructed BEFORE the llama.cpp service, because
	// §6.6 step 1's activation guard reads it: `llamacpp.Config.Bench` is the
	// BenchGuard that answers "is a bench live", and a daemon that built the two
	// the other way round would have to pass nil and mean it.
	// The model catalog and the downloader, with their four workers. They come
	// BEFORE the bench and llama.cpp services because both of those read models,
	// and before the API because five of section 3.7's endpoints and all of
	// section 3.8's answer from them (see catalog.go).
	if err := d.buildCatalog(); err != nil {
		d.close()
		return nil, err
	}

	if err := d.buildBench(); err != nil {
		d.close()
		return nil, err
	}

	if err := d.buildLlamacpp(); err != nil {
		d.close()
		return nil, err
	}

	// The inference gateway and the token store behind it (section 9). It is
	// built after the instance service, whose rows its first reconcile reads,
	// and before the API, which answers `GET /api/v1/gateway/denials` from it.
	if err := d.buildGateway(); err != nil {
		d.close()
		return nil, err
	}

	// Section 12's daemon half. It is registered HERE, with the other workers,
	// for the reason §2.3's boot triage gives: an `interrupted` self_update must
	// find its DomainWriter already in place, so that the row and the job move
	// in one transaction. The gate itself was built at step 4.
	if err := d.buildSelfUpdate(); err != nil {
		d.close()
		return nil, err
	}

	apiHandler, err := d.buildAPI(ctx)
	if err != nil {
		d.close()
		return nil, err
	}

	d.server = &http.Server{
		Handler: apiHandler,
		// No ReadTimeout or WriteTimeout: an SSE stream and a journal follow
		// are both long-lived by design (section 3.14, 3.3), and a write
		// deadline would cut them off mid-stream. ReadHeaderTimeout still
		// bounds the one thing a slow client can hold open for free.
		ReadHeaderTimeout: 20 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	return d, nil
}

// seedPortFromFlag is step 6b's three-way branch.
func (d *daemon) seedPortFromFlag(ctx context.Context, port int) error {
	const key = "ui.port_desired"
	if port == 0 {
		return nil
	}

	raw, err := json.Marshal(port)
	if err != nil {
		return err
	}
	seeded, err := d.settings.SeedIfAbsent(ctx, key, raw, model.UpdatedBySystem)
	if err != nil {
		return fmt.Errorf("seed %s from --port: %w", key, err)
	}
	if seeded {
		d.log.Info("seeded the management port from the unit", "port", port)
		return nil
	}

	stored, err := d.settings.GetInt(ctx, key)
	if err != nil {
		return err
	}
	if stored == int64(port) {
		return nil
	}
	// The stored setting wins: it was set in the UI, deliberately, by a human.
	// Nothing is broken by the divergence — it is cosmetic — and the message
	// says so, along with the one command that realigns the unit.
	d.log.Warn("the unit's --port differs from the stored setting; the stored setting wins",
		"unit_flag", port, "ui.port_desired", stored,
		"remediation", fmt.Sprintf("sudo llamaman install-units --port %d", stored))
	return nil
}

// walkPort is step 7.
//
// The full candidate set of section 2.8 — [ui.port_desired, +20] minus every
// instance's public_port and internal_port and minus the internal pool — needs
// the instances table and internal/netutil's allocator, neither of which
// exists yet. What is implemented here is the walk's SHAPE and its two
// load-bearing promises: it never refuses to start over a busy port, and the
// port it landed on is what gets recorded and advertised, not the one it wanted.
//
// The exclusion set is the part that must not be guessed at: taking a port an
// instance owns and discovering the theft only when that instance failed to
// bind is F6. Until the exclusions can be computed, the walk stays inside the
// same [desired, desired+20] window section 2.8 defines, so adding them later
// narrows the candidate set rather than moving it.
func (d *daemon) walkPort(ctx context.Context) error {
	desired, err := d.settings.GetInt(ctx, "ui.port_desired")
	if err != nil {
		return err
	}
	bind, err := d.settings.GetString(ctx, "ui.bind")
	if err != nil {
		return err
	}
	d.uiBind = bind

	ln, port, err := netutil.Walk(netutil.WalkOptions{
		Bind:    bind,
		Desired: int(desired),
		Window:  netutil.DefaultWindow,
		// Excluded is nil until the instances table has rows to exclude. The
		// walk stays inside the same [desired, desired+20] window section 2.8
		// defines, so adding the exclusions later NARROWS the candidate set
		// rather than moving it.
	})
	switch {
	case err == nil:
		d.listener, d.uiPort = ln, port
		if port != int(desired) {
			d.log.Warn("the desired management port was taken; the walk moved on",
				"desired", desired, "actual", port)
		}
		d.announce()
		return nil

	case errors.Is(err, netutil.ErrExhausted):
		// Every candidate is excluded or occupied: bind an ephemeral port
		// rather than refusing to start, and raise ui_port_exhausted. The
		// notification belongs to the `notifications` table; the log line is
		// what exists today, and it carries the same fact.
		ln, port, ephErr := netutil.Ephemeral(bind)
		if ephErr != nil {
			return fmt.Errorf("every candidate port was taken (%v) and an ephemeral bind failed too: %w",
				err, ephErr)
		}
		d.listener, d.uiPort = ln, port
		d.log.Error("ui_port_exhausted: no candidate port was free; bound an ephemeral one",
			"desired", desired, "window", netutil.DefaultWindow, "actual", port)
		d.announce()
		return nil

	default:
		return err
	}
}

// announceSetupToken is section 11.1 step 8's announcement, and step 2 of
// section 2.2a.
//
// The token is logged EXACTLY ONCE, on the boot that minted it, and journald is
// explicitly "a convenience, never the recovery path": the recovery path is
// `llamaman status`, which reads the same 0600 file. A boot that found an
// existing unclaimed token says only that the file is there, so an operator
// tailing the journal is not shown a live credential on every restart.
func (d *daemon) announceSetupToken(ctx context.Context) error {
	tok, err := d.auth.EnsureSetupToken(ctx)
	if err != nil {
		return fmt.Errorf("resolve the setup token: %w", err)
	}
	switch {
	case tok.Claimed:
		return nil
	case tok.Minted:
		url := fmt.Sprintf("http://%s", net.JoinHostPort(displayHost(d.uiBind), strconv.Itoa(d.uiPort)))
		d.log.Info(fmt.Sprintf(
			"SETUP: open %s — setup token %s (not needed from this machine)", url, tok.Token))
	default:
		d.log.Info("this host has not been claimed yet; the setup token is on disk",
			"path", tok.Path, "hint", "run `llamaman status` as root or the service identity to print it")
	}
	return nil
}

// announce publishes the port the walk actually landed on, to journald at
// `info` and through sd_notify STATUS=, so `systemctl status llamaman` shows
// the truth (D9/D24).
func (d *daemon) announce() {
	url := fmt.Sprintf("http://%s", net.JoinHostPort(displayHost(d.uiBind), strconv.Itoa(d.uiPort)))
	d.log.Info("llamaman listening", "url", url)
	if err := d.opts.Notifier.Status("listening on " + url); err != nil {
		d.log.Debug("could not publish the listening status", "error", err)
	}
	if d.opts.ReadyHook != nil {
		d.opts.ReadyHook(d.listener.Addr().String())
	}
}

// displayHost turns a wildcard bind into something a human can paste. The
// primary-IPv4 lookup section 11.1 step 7 describes belongs with the rest of
// the host probe; until then a wildcard is shown as localhost, which is at
// least always correct for the person at the console — the case SPEC section
// 3.9's acceptance test is written about.
func displayHost(bind string) string {
	switch bind {
	case "", "0.0.0.0", "::", "[::]":
		return "localhost"
	default:
		return bind
	}
}

// writeRuntimeInfo persists what this boot resolved, so `llamaman status` and
// `doctor` can read it without an HTTP call (section 2.1).
//
// It carries HostBootID and HostBootAt FORWARD from the existing row rather
// than writing them: PutRuntimeInfo's own doc comment is explicit that those
// two columns have exactly one writer, supervisor boot reconciliation step 1,
// and that a boot-time caller must not touch them — persisting the new value
// here would make the supervisor always find equality, the D53 autostart
// coupling would never fire, and autostart would be broken in both directions.
func (d *daemon) writeRuntimeInfo(ctx context.Context) error {
	now := d.opts.Now()
	bootID := store.NewID(now)
	pid := int64(os.Getpid())
	bootAt := now.UnixMilli()
	port := int64(d.uiPort)
	scope := d.scope
	// D58's honest answer, read from the gateway rather than asserted here: it
	// is `fdstore` only when this daemon actually has somewhere to hand its
	// sockets, and it degrades to `none` the moment a store refuses. A literal
	// would make `GET /system/capabilities` promise an uninterrupted restart on
	// a host that cannot deliver one — which is the one thing section 9.4 says
	// must never happen silently.
	continuity := model.ContinuityNone
	if d.gateway != nil {
		continuity = d.gateway.Continuity()
	}
	stateDir := d.stateDir
	bind := d.uiBind
	url := fmt.Sprintf("http://%s", net.JoinHostPort(displayHost(d.uiBind), strconv.Itoa(d.uiPort)))

	binary := ""
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			binary = resolved
		} else {
			binary = exe
		}
	}

	return d.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		prev, err := d.store.RuntimeInfo(ctx, tx)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}

		version, err := d.store.SchemaVersion(ctx, tx)
		if err != nil {
			return err
		}
		schemaVersion := int64(version)

		row := model.RuntimeInfo{
			DaemonVersion: buildinfo.Version,
			DaemonCommit:  buildinfo.Commit,
			PID:           &pid,
			BootID:        &bootID,
			BootAt:        &bootAt,

			// Carried forward, never written here (D53).
			HostBootID: prev.HostBootID,
			HostBootAt: prev.HostBootAt,

			UIBindAddr: &bind,
			UIPort:     &port,
			UIURLHint:  &url,

			SystemdScope:       &scope,
			ListenerContinuity: &continuity,

			StateDir:      &stateDir,
			SchemaVersion: &schemaVersion,
		}

		// What step 6's probe learned. Each column is left NULL when the probe
		// did not run or could not reach an answer, because "not learned" and
		// "denied" are different facts and section 11.1a's table reads them
		// differently: `polkit_ok` is NULL in user scope meaning "not
		// applicable", never 0.
		if d.opts.Systemd != nil {
			control := d.systemd.ControlKind
			journal := d.systemd.Journal
			row.SystemdControl = &control
			row.JournalRead = &journal
			if detail := d.systemd.PolkitDetail; detail != "" {
				row.PolkitDetail = &detail
			}
			if p := d.systemd.Polkit; p != nil {
				units, files := p.ManageUnits, p.ManageUnitFiles
				row.PolkitOK = &units
				row.PolkitUnitFiles = &files
			}
		}
		if binary != "" {
			row.BinaryPath = &binary
		}
		return d.store.PutRuntimeInfo(ctx, tx, row)
	})
}

// currentBootID reads back the ULID writeRuntimeInfo minted. The job queue's
// leases are owned by it, and reading it from the row rather than keeping a
// second copy in memory is what makes "this lease belongs to a boot that is
// gone" a string comparison against the one authoritative value (section 2.3).
func (d *daemon) currentBootID(ctx context.Context) (string, error) {
	var id string
	err := d.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		ri, err := d.store.RuntimeInfo(ctx, tx)
		if err != nil {
			return err
		}
		if ri.BootID == nil {
			return errors.New("runtime_info.boot_id is NULL immediately after this boot wrote it")
		}
		id = *ri.BootID
		return nil
	})
	return id, err
}

// buildAPI wires the HTTP surface: the SSE transport over the hub and the
// store, the SPA behind it, and the route registry in front of both.
func (d *daemon) buildAPI(ctx context.Context) (http.Handler, error) {
	ui, err := web.Handler()
	if err != nil {
		return nil, fmt.Errorf("mount the embedded UI: %w", err)
	}

	stream := sse.NewHandler(sse.Config{
		Hub:    d.hub,
		Replay: d.store,
		Logger: d.log,
	})

	// One service answers both the gate and the endpoints. api.NewAuthenticator
	// is the adapter that turns a request into the strings internal/auth takes,
	// which is what lets that package stay free of every import under
	// internal/api (section 1, invariant 4).
	a, err := api.New(api.Config{
		Logger:    d.log,
		Now:       d.opts.Now,
		Auth:      api.NewAuthenticator(d.auth),
		Sessions:  d.auth,
		Setup:     d.setup,
		Instances: d.instances,
		// The supervision half of section 3.10, plus section 3.11 and
		// `GET /ports/suggest`. It is a separate interface from Instances
		// because it needs the supervisor, the systemd controller, the journal
		// and the fit calculator, none of which the instance service owns.
		InstanceControl: &instanceControlAPI{d: d},
		Presets:         &presetAPI{d: d},
		// Section 3.7's catalog and section 3.8's queue. Both were built above;
		// a nil here is nine endpoints answering 503, which on a fresh install
		// means the wizard cannot be completed at all.
		Models:      d.models,
		Downloads:   d.downloads,
		Llamacpp:    d.llamacpp,
		Bench:       d.bench,
		HF:          d.hfClient,
		LocalModels: localIndex{st: d.store},
		HFToken: hfTokenService{
			secrets: d.secrets, endpoint: d.hubEndpoint(), log: UserAgent,
		},
		GitHubToken: githubTokenService{
			secrets: d.secrets, client: d.releases, agent: UserAgent,
		},
		APITokens: d.tokens,
		Gateway:   d.gateway,
		// Section 3.14's four self-update rows. The service is what evaluates the
		// guard's four clauses inside the one transaction that stages an update
		// (D97); this layer only carries the request to it and renders the 409s.
		Update: d.selfupdate,
		// Section 3.14's job rows. The queue is what enforces the two cancel
		// cut-offs, through the CancelGuard each owning worker registers — which
		// is the only way `POST /jobs/{id}/cancel` can reach D96's refusal for a
		// `self_update` past its `staged` commit.
		Jobs: d.queue,
		// Section 3.14's `GET /events/log`: the durable read of the same table
		// the SSE stream above replays from.
		EventLog: eventLog{st: d.store},
		// Section 8.6's host half. A nil Hardware is a CPU-only estimate rather
		// than a 503, which is a real answer on a host with no NVIDIA card —
		// but this host has a prober either way, and one that found no card
		// answers with an empty inventory rather than an error.
		Hardware: hostProbe{NvidiaSMIProber: d.gpus},
		// D32's learned correction. Without it every report is permanently
		// `confidence: "modeled"`, however many observations the supervisor
		// wrote.
		FitCalibration: fitCalibration{st: d.store},
		// `fit.margin_mib` (section 8.1). A registered setting nothing reads is
		// a knob that lies, which SPEC section 3.9's zero-config mandate makes
		// worse rather than better.
		Settings: d.settings,
		// Section 3.3 and section 3.4. `GET /system/capabilities` is the one the
		// UI reads before it renders any control at all — without it every
		// screen asserts a healthy host, including the ones section 11.1a says
		// must explain themselves.
		System:        newSystemAPI(d),
		SettingsAdmin: newSettingsAPI(ctx, d.settings),
		Meta:          d,
		Events:        stream,
		Fallback:      ui,
		Conformance:   d.opts.Conformance,
	})
	if err != nil {
		return nil, fmt.Errorf("build the API: %w", err)
	}
	d.log.Info("mounted the API", "routes", len(a.Routes()))
	return a, nil
}

// Meta implements api.MetaProvider — the endpoint install.sh polls.
//
// The two booleans are deliberately different questions, and answering them from
// two different places is the point. `claimed` is "`admin_account` exists": the
// one-time token has been burned. `setup_complete` is "the wizard's `done` step
// is complete". A host can be claimed without being complete — that is a wizard
// interrupted after the password step, which section 11.2 requires be resumable —
// and collapsing the two would send such a browser to a dashboard it is not ready
// for.
func (d *daemon) Meta(ctx context.Context) (api.Meta, error) {
	claimed, err := d.setup.Claimed(ctx)
	if err != nil {
		return api.Meta{}, err
	}
	complete, err := d.setup.Complete(ctx)
	if err != nil {
		return api.Meta{}, err
	}
	return api.Meta{
		Version:       buildinfo.Version,
		Commit:        buildinfo.Commit,
		SetupComplete: complete,
		Claimed:       claimed,
		UIPort:        d.uiPort,
	}, nil
}

// scopeMismatch names the two combinations section 11.1 step 1 says are "not
// fatal but reported", because each almost always means the units and the
// binary came from different installs.
func scopeMismatch(scope model.SystemdScope, stateDir string, env func(string) string) string {
	if scope == model.ScopeUser && os.Geteuid() == 0 {
		return "scope resolved to `user` but this process is running as root"
	}
	if scope == model.ScopeSystem {
		if home := env("HOME"); home != "" && home != "/" && isUnder(stateDir, home) {
			return "scope resolved to `system` but the state directory is under $HOME: " + stateDir
		}
	}
	return ""
}

func isUnder(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel != ".." && !filepath.IsAbs(rel) &&
		(len(rel) < 3 || rel[:3] != "../")
}
