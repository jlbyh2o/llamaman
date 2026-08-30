package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jlbyh2o/llamaman/internal/app"
	"github.com/jlbyh2o/llamaman/internal/instances"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/netutil"
	"github.com/jlbyh2o/llamaman/internal/settings"
	"github.com/jlbyh2o/llamaman/internal/store"
	"github.com/jlbyh2o/llamaman/internal/supervisor"
)

// `llamaman instance-exec <name>` — the render step of DESIGN section 5.6.
//
// It runs in the unit's context, as the service identity, with NO D-Bus, no
// HTTP, no GPU probe and no network of any kind: its entire world is this
// binary, the database file and `%i`. That is not minimalism for its own sake.
// It is what makes section 5.6's central promise true — INSTANCES START
// CORRECTLY AT BOOT EVEN WHEN llamaman.service ITSELF IS FAILING — so the
// control plane can be broken without taking inference down. Everything this
// file does follows from it: `-ngl auto` renders no flag because resolving it
// would need live hardware state (D51), the active version is read from a table
// rather than probed, and the last statement is an execve rather than a
// supervised child.
//
// The ledger row is opened BEFORE preflight, not after it (D54). Every exit
// status below is a real failure a user must be able to see and a restart
// policy must be able to count; a row written only on the happy path would make
// a configuration that fails preflight on every attempt look like an instance
// that never tried to start — no crash-loop cutoff, no start history, infinite
// backoff retries.

// Schema-gate timings (§5.6a). They are fields on the options struct rather
// than constants so a test can exercise the wait without spending a minute in
// it, and the values here are the design's.
const (
	// schemaWaitMax is the bounded wait for a DB that is BEHIND this binary:
	// the daemon has not finished migrating, and ordering (§5.5's
	// `After=llamaman.service`) means this case is already rare.
	schemaWaitMax = 60 * time.Second
	// schemaPollEvery is how often the version is re-read during that wait.
	schemaPollEvery = 500 * time.Millisecond
)

// instanceExecOptions are the seams a test replaces. A real run leaves every
// field zero.
type instanceExecOptions struct {
	// Now supplies every instant the ledger stamps. Nil uses Env.now.
	Now func() time.Time
	// Exec is the final syscall. Nil uses syscall.Exec. A test supplies one
	// that records argv and returns, which is the only way to observe step 11
	// without replacing this process.
	Exec func(path string, argv []string, envv []string) error
	// Probe is step 8's test bind. Nil uses a real bind through netutil.
	Probe func(bind string, port int) bool
	// StateDir overrides D72's chain. Empty resolves it the way §11.1 step 1
	// does, which under the instance template means `$STATE_DIRECTORY`.
	StateDir string
	// SchemaWaitMax and SchemaPollEvery override the gate's timings. Zero uses
	// the constants above.
	SchemaWaitMax   time.Duration
	SchemaPollEvery time.Duration
	// SchemaVersion overrides "the highest migration embedded in this binary".
	// Zero reads the embedded set, which is what a real run does; a test uses
	// it to simulate a binary older or newer than the database.
	SchemaVersion int
}

// InstanceExec is the launcher named by every instance unit's ExecStart.
// Unit-only (DESIGN sections 1 and 5.6).
func InstanceExec(env Env, args []string) error {
	rest, err := unitOnly(env, "instance-exec", args)
	if err != nil {
		return err
	}
	if len(rest) != 1 || rest[0] == "" {
		fmt.Fprintf(env.Stderr,
			"llamaman instance-exec: expected exactly one instance name (the unit passes %%i), got %d\n",
			len(rest))
		return NewExitError(supervisor.ExitInstanceMissing,
			"instance-exec: no instance name given")
	}
	return runInstanceExec(env, rest[0], instanceExecOptions{})
}

// launcher is one instance-exec run: the resolved seams, the open database and
// the id of the ledger row this run owns.
type launcher struct {
	env  Env
	name string
	opts instanceExecOptions

	st *store.Store
	// startID is the `instance_starts` row opened at step 3. It is empty until
	// then, and that emptiness is exactly what §5.6's "steps 1, 1b and 2 are
	// before the row exists" means: fail() writes nothing while it is empty.
	startID string
	inst    model.Instance
	// override is the transient FlagSet patch step 3 consumed, held in memory
	// for step 4. It is kept here rather than re-read, because step 3 has
	// already CLEARED the column — that clearing is what makes safe start
	// one-shot against a crash, a reboot or a supervisor restart (D61).
	override *string
}

func runInstanceExec(env Env, name string, opts instanceExecOptions) error {
	l := &launcher{env: env, name: name, opts: opts}

	// Step 1. The database, or exit 70 with no row at all.
	closeDB, err := l.open()
	if err != nil {
		return err
	}
	defer closeDB()

	ctx := context.Background()

	// Steps 1b and 3, together and possibly more than once. The gate is
	// re-asserted inside the step-3 transaction, and a migration that lands
	// between the poll and the transaction restarts the gate rather than
	// letting the run proceed on a stale assumption.
	if err := l.openLedgerRow(ctx); err != nil {
		return err
	}

	return l.launch(ctx)
}

// open resolves the state directory and takes the short-lived read-write
// connection of §5.6 step 1.
//
// The file is stat'd before the driver is asked to open it, because SQLite
// would CREATE a missing database — and a launcher that created an empty
// database in the daemon's state directory would turn a misconfigured
// `StateDirectory=` into a corrupt-looking install rather than into the honest
// exit 70 this step promises.
func (l *launcher) open() (func(), error) {
	dir := l.opts.StateDir
	if dir == "" {
		// §11.1 step 1's chain, whose FIRST candidate is `$STATE_DIRECTORY` —
		// which the instance template's own `StateDirectory=llamaman`
		// guarantees is set (§5.5). Passing `system` as the scope only matters
		// when the environment says nothing, and a unit always says something.
		dir = app.ResolveStateDir(model.ScopeSystem, "", l.env.getenv())
	}
	path := filepath.Join(dir, app.DatabaseFileName)

	if fi, err := os.Stat(path); err != nil || !fi.Mode().IsRegular() {
		return nil, l.abort(supervisor.ExitDBUnavailable,
			"no database at %s (state directory %s)", path, dir)
	}

	st, err := store.Open(context.Background(), path)
	if err != nil {
		return nil, l.abort(supervisor.ExitDBUnavailable, "open %s: %v", path, err)
	}
	l.st = st
	return func() { st.Close() }, nil
}

// openLedgerRow runs the schema gate (§5.6a) and then steps 2 and 3 in one
// BEGIN IMMEDIATE transaction, retrying the pair when a migration lands between
// them.
func (l *launcher) openLedgerRow(ctx context.Context) error {
	want := l.opts.SchemaVersion
	if want == 0 {
		embedded, err := store.Migrations()
		if err != nil {
			return l.abort(supervisor.ExitDBUnavailable, "read embedded migrations: %v", err)
		}
		for _, m := range embedded {
			if m.Version > want {
				want = m.Version
			}
		}
	}

	// A migration landing inside the transaction restarts the gate. Bounding
	// the retries stops a host whose daemon is migrating in a tight loop from
	// spinning here forever; the gate's own wait is what absorbs the ordinary
	// case, and exit 75 is a clean, visible failure the supervisor recovers
	// from on its own.
	for attempt := 0; attempt < 4; attempt++ {
		if err := l.schemaGate(ctx, want); err != nil {
			return err
		}
		err := l.st.Write(ctx, func(ctx context.Context, tx store.Tx) error {
			return l.openRowTx(ctx, tx, want)
		})
		if err == nil {
			return nil
		}
		if errors.Is(err, errSchemaMoved) {
			continue
		}
		return err
	}
	return l.abort(supervisor.ExitSchemaMismatch,
		"the database schema changed under every attempt to open a start row")
}

// errSchemaMoved aborts the step-3 transaction when the schema version inside
// it is not the one the gate agreed on. It never escapes openLedgerRow.
var errSchemaMoved = errors.New("schema version moved inside the start transaction")

// openRowTx is §5.6 steps 2 and 3, in the caller's BEGIN IMMEDIATE transaction.
func (l *launcher) openRowTx(ctx context.Context, tx store.Tx, want int) error {
	// The step-1b check, re-asserted so the whole run is pinned to one schema
	// version.
	have, err := l.st.SchemaVersion(ctx, tx)
	if err != nil {
		return l.abort(supervisor.ExitDBUnavailable, "re-read the schema version: %v", err)
	}
	if have != want {
		return errSchemaMoved
	}

	// Step 2. A missing or soft-deleted instance is exit 64 with NO row: the
	// foreign key has no parent, and an instance the user deleted needs no
	// history. This is also the safety net behind a delete that could not
	// disable the unit (§3.10c) — the enabled unit starts this launcher, which
	// exits 64 without launching anything.
	inst, err := l.st.InstanceByName(ctx, tx, l.name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return l.abort(supervisor.ExitInstanceMissing,
				"no live instance named %q", l.name)
		}
		return l.abort(supervisor.ExitDBUnavailable, "load instance %q: %v", l.name, err)
	}
	l.inst = inst

	now := l.now()

	// Step 3, first half. Close any row a previous run left open, BEFORE the
	// insert, because `idx_instance_starts_open` is UNIQUE: a surviving open
	// row would otherwise make this insert — rather than the earlier lost write
	// — the thing that fails. `exit_code` stays NULL because nothing observed
	// how that run ended, which is exactly why the row was left open, and D64
	// excludes `launcher_superseded` from the crash-loop count for the same
	// reason.
	superseded := supervisor.ErrLauncherSuperseded
	if _, err := l.st.CloseOpenInstanceStart(ctx, tx, inst.ID, store.StartClosure{
		Outcome:   model.OutcomeFailed,
		ErrorCode: &superseded,
		EndedAt:   now.UnixMilli(),
	}); err != nil {
		return l.abort(supervisor.ExitDBUnavailable, "close a superseded start row: %v", err)
	}

	// Step 3, second half. The hand-off contract: the daemon stamps its intent
	// before StartUnit, this consumes it, and a start nobody stamped — a boot
	// start of an enabled unit, or a hand-run `systemctl start` — is honestly
	// recorded as `external` instead of guessed. Consuming the override in the
	// SAME transaction is what makes safe start one-shot (D61): a crash, a
	// reboot or a supervisor restart all find the columns already clear and
	// launch the saved configuration.
	trigger, override, err := l.st.TakePendingStart(ctx, tx, inst.ID)
	if err != nil {
		return l.abort(supervisor.ExitDBUnavailable, "consume the pending start: %v", err)
	}

	row := model.InstanceStart{
		ID:           store.NewID(now),
		InstanceID:   inst.ID,
		At:           now.UnixMilli(),
		Trigger:      model.StartByExternal,
		ConfigHash:   inst.ConfigHash,
		OverrideJSON: override,
	}
	if trigger != nil {
		row.Trigger = model.StartTrigger(*trigger)
	}
	if err := l.st.InsertInstanceStart(ctx, tx, row); err != nil {
		return l.abort(supervisor.ExitDBUnavailable, "open the start row: %v", err)
	}
	l.startID = row.ID
	l.override = override
	return nil
}

// schemaGate is §5.6a: proceed, wait, or refuse.
//
// Nothing in the launcher touches a table until this passes — INCLUDING
// `instance_starts`. Both refusals therefore record no ledger row, exactly like
// exit 70, and for the same reason: writing a row is precisely the operation
// one must not perform against a schema this binary does not understand. The
// supervisor synthesizes the row instead, which it can do safely because the
// daemon is by definition the process that owns the current schema.
func (l *launcher) schemaGate(ctx context.Context, want int) error {
	waitMax := l.opts.SchemaWaitMax
	if waitMax == 0 {
		waitMax = schemaWaitMax
	}
	poll := l.opts.SchemaPollEvery
	if poll == 0 {
		poll = schemaPollEvery
	}

	deadline := l.now().Add(waitMax)
	for {
		var have int
		err := l.st.Read(ctx, func(ctx context.Context, tx store.Tx) error {
			v, err := l.st.SchemaVersion(ctx, tx)
			have = v
			return err
		})
		if err != nil {
			return l.abort(supervisor.ExitDBUnavailable, "read the schema version: %v", err)
		}

		switch {
		case have == want:
			return nil

		case have > want:
			// A DOWNGRADE across a schema bump, until §12.4's procedure has
			// been run (D90, D94). Waiting cannot help, and running a v14 query
			// set against a v15 schema can corrupt data — so this branch does
			// not wait, and it is the whole reason the gate runs before any
			// table is touched.
			fmt.Fprintf(l.env.Stderr,
				"llamaman: schema_ahead: the database is at version %d and this binary understands %d; "+
					"a newer release wrote this database\n", have, want)
			return l.abort(supervisor.ExitSchemaMismatch,
				"database schema %d is newer than this binary's %d", have, want)

		case !l.now().Before(deadline):
			// The daemon has not migrated within the bounded wait. `Restart=no`
			// means the unit simply stays failed; the supervisor starts it as
			// soon as the daemon is up and `desired_state='running'`, so the
			// recovery is automatic and needs no user action.
			fmt.Fprintf(l.env.Stderr,
				"llamaman: schema_mismatch: the database is at version %d and this binary expects %d; "+
					"the daemon has not finished upgrading the database\n", have, want)
			return l.abort(supervisor.ExitSchemaMismatch,
				"database schema %d is behind this binary's %d after waiting %s",
				have, want, waitMax)
		}

		select {
		case <-ctx.Done():
			return l.abort(supervisor.ExitSchemaMismatch, "canceled while waiting for the schema")
		case <-time.After(poll):
		}
	}
}

// launch is steps 4 through 11: everything that happens with the ledger row
// open, and every failure among them closes it.
func (l *launcher) launch(ctx context.Context) error {
	inst := l.inst

	// Step 4. Flags, then the transient override as a shallow patch over them
	// (present keys replace). `extra_flags` is dropped for an overridden run:
	// a safe start is defined as the saved configuration minus the flags that
	// are suspect, and an escape hatch nobody re-examined is exactly the kind
	// of thing that keeps a start failing.
	flags, err := model.ParseFlagSet([]byte(inst.FlagsJSON))
	if err != nil {
		return l.fail(supervisor.ExitBadFlags, supervisor.ErrBadFlags, nil, "%v", err)
	}
	if l.override != nil {
		if flags, err = flags.ApplyOverride([]byte(*l.override)); err != nil {
			return l.fail(supervisor.ExitBadFlags, supervisor.ErrBadFlags, nil, "%v", err)
		}
		inst.ExtraFlags = ""
	}

	// The escape hatch is re-validated here and not only at save time, because
	// this renders from the STORED row: a row written before a rule existed
	// must not start with a flag the rule now forbids (§5.7).
	if _, err := instances.ParseExtraFlags(inst.ExtraFlags); err != nil {
		return l.fail(supervisor.ExitBadFlags, supervisor.ErrBadFlags, nil, "extra_flags: %v", err)
	}

	// Still step 4. A draft pairing that was checked and FAILED must not start:
	// speculative decoding across two vocabularies emits garbage rather than
	// erroring, so this is the one validation the launcher re-runs from the
	// stored row (D34, §3.10a). `deferred` is not refused — the metadata simply
	// did not exist yet, and the models service re-checks when it lands.
	if inst.DraftValidation == model.DraftMismatch {
		return l.fail(supervisor.ExitBadFlags, supervisor.ErrDraftVocabMismatch, nil,
			"the draft model's vocabulary does not match the primary model's")
	}

	// Step 5. The runtime.
	runtime, err := l.resolveRuntime(ctx)
	if err != nil {
		return err
	}

	// Step 6. The model files.
	primary, mmproj, draft, err := l.resolveModels(ctx, inst)
	if err != nil {
		return err
	}

	// Step 7. argv. Nothing here consults the GPU, the fit calculator or the
	// network, and the process remains free of D-Bus and HTTP.
	argv, err := instances.RenderArgv(inst, flags, primary, mmproj, draft, runtime)
	if err != nil {
		return l.fail(supervisor.ExitBadFlags, supervisor.ErrBadFlags, nil, "render argv: %v", err)
	}

	// Step 8. The listener. The probe is advisory the moment it returns — F6
	// exists because another process can take the port between here and
	// llama-server's own bind — but an occupied port caught now is a row the
	// supervisor can act on (it reassigns from the pool) rather than a
	// llama-server that exits with a message nobody parsed.
	if !l.probe(instances.LoopbackHost, inst.InternalPort) {
		detail := fmt.Sprintf(`{"internal_port":%d,"public_port":%d}`,
			inst.InternalPort, inst.PublicPort)
		return l.fail(supervisor.ExitPortConflict, supervisor.ErrPortConflict, &detail,
			"127.0.0.1:%d is already in use", inst.InternalPort)
	}

	// Step 9. Record what was actually rendered, and commit BEFORE the exec so
	// the row survives a crash on model load. The launcher does NOT write
	// `instance_status`: `applied_config_hash` is stamped by the supervisor
	// when `/health` first returns 200, so a process that dies during model
	// load never records a configuration that ran.
	effective := instances.ConfigHash(argv, primary, mmproj, draft, runtime.ID)
	argvJSON, err := json.Marshal(argv)
	if err != nil {
		return l.fail(supervisor.ExitBadFlags, supervisor.ErrBadFlags, nil, "encode argv: %v", err)
	}
	if err := l.st.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		_, err := l.st.UpdateStartRender(ctx, tx, l.startID,
			ptr(string(argvJSON)), &effective, &runtime.ID)
		return err
	}); err != nil {
		return l.fail(supervisor.ExitDBUnavailable, supervisor.ErrLauncherDBUnavailable, nil,
			"record the rendered argv: %v", err)
	}

	// Still step 9: the environment section 5.7 specifies, built from the same
	// pure function `GET /instances/{id}/command` renders — that endpoint
	// promises "this argv and env verbatim", which only holds while there is
	// one producer of both. It is one read of a table this process already has
	// open, and it consults no file and no hardware.
	envv, err := l.environment(ctx)
	if err != nil {
		return err
	}

	// Step 10. The canonical line, and the first thing anyone sees in the
	// unit's journal.
	fmt.Fprintf(l.env.Stderr, "llamaman: exec %s %s\n", runtime.ID, strings.Join(argv, " "))

	// Step 11. No LD_LIBRARY_PATH is set: the RPATH from D22 makes each version
	// directory self-contained, and setting one would let a library from
	// another version be picked up by a build that was linked against its own.
	server := runtime.ServerPath()
	if err := l.execve(server, argv, envv); err != nil {
		return l.fail(supervisor.ExitRuntimeMissing, supervisor.ErrRuntimeMissing, nil,
			"exec %s: %v", server, err)
	}
	return nil
}

// environment is section 5.7's environment contract, resolved from settings
// rows and applied over this process's own environment.
//
// Three of its four rules are about what is NOT there, and each is a bug this
// launcher would otherwise ship: `LLAMA_CACHE` is removed so llama.cpp's own
// `-hf` cache is never used (SPEC section 3.2), `CUDA_VISIBLE_DEVICES` is
// removed because setting it beside `--device` renumbers the devices and
// silently addresses the wrong card (D66), and no `LD_LIBRARY_PATH` is added
// (step 11). The fourth points the process at the cache root this daemon
// actually manages.
//
// A settings read that fails is exit 70 like any other unusable database: the
// alternative — exec'ing with an environment that names the wrong cache — is
// the silent kind of wrong this whole contract exists to avoid.
func (l *launcher) environment(ctx context.Context) ([]string, error) {
	var rows []model.Setting
	if err := l.st.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		rows, err = l.st.Settings(ctx, tx)
		return err
	}); err != nil {
		return nil, l.fail(supervisor.ExitDBUnavailable, supervisor.ErrLauncherDBUnavailable, nil,
			"read the settings the environment is built from: %v", err)
	}

	in := instances.EnvInput{GGML: map[string]string{}}
	for _, row := range rows {
		switch {
		case row.Key == settings.KeyHFHubDir:
			// The authoritative primary hub directory (section 7.2a). An
			// absent row means the built-in default, which is the empty
			// string: the cache root has not been resolved yet, and neither
			// HF variable is set.
			var dir string
			if err := json.Unmarshal([]byte(row.Value), &dir); err == nil {
				in.HubDir = dir
			}
		default:
			name, ok := instances.GGMLEnvName(row.Key)
			if !ok {
				continue
			}
			var v any
			if err := json.Unmarshal([]byte(row.Value), &v); err != nil {
				continue
			}
			in.GGML[name] = fmt.Sprint(v)
		}
	}
	return instances.Env(os.Environ(), in), nil
}

// resolveRuntime is step 5: `versions/active` to a concrete directory, and the
// `is_active=1` row's state.
//
// Both halves are needed and neither substitutes for the other. The symlink
// says WHICH directory; the row says whether that directory is mid-swap. A
// forced rebuild of the active version (D71/D78) moves the row out of `ready`
// for the duration and reinstalls into the directory the symlink names —
// consulting the row costs one indexed read of a table already open, and
// together with D78's staging protocol it closes both halves of the hazard:
// nothing is written into a live directory, and nothing starts against one
// mid-swap.
func (l *launcher) resolveRuntime(ctx context.Context) (instances.Runtime, error) {
	var active store.ActiveVersion
	err := l.st.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		v, err := l.st.ActiveVersion(ctx, tx)
		active = v
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return instances.Runtime{}, l.fail(supervisor.ExitRuntimeMissing,
			supervisor.ErrRuntimeMissing, nil, "no llama.cpp version is active")
	}
	if err != nil {
		return instances.Runtime{}, l.fail(supervisor.ExitDBUnavailable,
			supervisor.ErrLauncherDBUnavailable, nil, "read the active version: %v", err)
	}
	if !active.Ready() {
		return instances.Runtime{}, l.fail(supervisor.ExitRuntimeMissing,
			supervisor.ErrRuntimeRebuilding, nil,
			"the active llama.cpp version %s is in state %q, not ready", active.ID, active.State)
	}

	dir, err := filepath.EvalSymlinks(filepath.Join(l.versionsDir(), "active"))
	if err != nil {
		return instances.Runtime{}, l.fail(supervisor.ExitRuntimeMissing,
			supervisor.ErrRuntimeMissing, nil, "resolve versions/active: %v", err)
	}

	rt := instances.Runtime{ID: active.ID, Dir: dir, SupportsFit: active.SupportsFit}
	if active.HelpJSON != nil {
		// A capture that will not parse is not a reason to refuse a start: the
		// only reader of it is the flag-churn guard, which the launcher never
		// runs. An empty set makes that check unavailable, which is what a
		// missing capture already means.
		_ = json.Unmarshal([]byte(*active.HelpJSON), &rt.Help)
	}

	if fi, err := os.Stat(rt.ServerPath()); err != nil || fi.IsDir() {
		return instances.Runtime{}, l.fail(supervisor.ExitRuntimeMissing,
			supervisor.ErrRuntimeMissing, nil, "no executable at %s", rt.ServerPath())
	}
	return rt, nil
}

// resolveModels is step 6: the primary GGUF (shard 1 for a sharded set), the
// optional mmproj and the optional draft, each resolved to one absolute path
// and each stat'd. A missing file exits 72 WITH THE RESOLVED PATH in the
// message, which is what lets F4's card offer a re-download of the right thing.
func (l *launcher) resolveModels(ctx context.Context, inst model.Instance) (
	primary, mmproj, draft *instances.ModelFile, err error) {

	ids := []string{}
	for _, id := range []*string{inst.ModelID, inst.MmprojModelID, inst.DraftModelID} {
		if id != nil {
			ids = append(ids, *id)
		}
	}
	if inst.ModelID == nil {
		return nil, nil, nil, l.fail(supervisor.ExitModelMissing, supervisor.ErrModelMissing, nil,
			"this instance references no model")
	}

	var rows map[string]store.ModelPaths
	if err := l.st.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		m, err := l.st.ModelPathsByID(ctx, tx, ids)
		rows = m
		return err
	}); err != nil {
		return nil, nil, nil, l.fail(supervisor.ExitDBUnavailable,
			supervisor.ErrLauncherDBUnavailable, nil, "read the referenced models: %v", err)
	}

	resolve := func(id *string, role string) (*instances.ModelFile, error) {
		if id == nil {
			return nil, nil
		}
		row, ok := rows[*id]
		if !ok {
			return nil, l.fail(supervisor.ExitModelMissing, supervisor.ErrModelMissing, nil,
				"the %s model %s is not in the catalog", role, *id)
		}
		path := filepath.Join(row.SnapshotDir, row.PrimaryFile)
		if fi, err := os.Stat(path); err != nil || fi.IsDir() {
			return nil, l.fail(supervisor.ExitModelMissing, supervisor.ErrModelMissing, nil,
				"the %s model file %s is missing", role, path)
		}
		return &instances.ModelFile{ID: row.ID, Path: path}, nil
	}

	if primary, err = resolve(inst.ModelID, "primary"); err != nil {
		return nil, nil, nil, err
	}
	if mmproj, err = resolve(inst.MmprojModelID, "mmproj"); err != nil {
		return nil, nil, nil, err
	}
	if draft, err = resolve(inst.DraftModelID, "draft"); err != nil {
		return nil, nil, nil, err
	}
	return primary, mmproj, draft, nil
}

// fail closes the open ledger row and returns the exit status.
//
// If the closing write itself fails — a locked or unwritable database — the
// launcher STILL exits with the correct code, and the supervisor closes the row
// from the unit's ExecMainStatus. The ledger is never left open by a process
// that has exited.
func (l *launcher) fail(code int, errorCode string, detail *string, format string, args ...any) error {
	message := fmt.Sprintf(format, args...)
	fmt.Fprintf(l.env.Stderr, "llamaman instance-exec: %s: %s\n", errorCode, message)

	if l.startID != "" {
		exit := int64(code)
		closure := store.StartClosure{
			Outcome:      model.OutcomeFailed,
			ExitCode:     &exit,
			ErrorCode:    &errorCode,
			ErrorMessage: &message,
			DetailJSON:   detail,
			EndedAt:      l.now().UnixMilli(),
		}
		err := l.st.Write(context.Background(), func(ctx context.Context, tx store.Tx) error {
			_, err := l.st.CloseInstanceStart(ctx, tx, l.startID, closure)
			return err
		})
		if err != nil {
			fmt.Fprintf(l.env.Stderr,
				"llamaman instance-exec: could not close the start row (%v); "+
					"the supervisor will close it from the unit's exit status\n", err)
		}
	}
	return &ExitError{Code: code, Err: errors.New(message)}
}

// abort is fail's counterpart for the three exits that happen before the row
// exists (§5.6: 70, 75 and 64). It writes nothing, by construction rather than
// by remembering to.
func (l *launcher) abort(code int, format string, args ...any) error {
	message := fmt.Sprintf(format, args...)
	fmt.Fprintf(l.env.Stderr, "llamaman instance-exec: %s\n", message)
	return &ExitError{Code: code, Err: errors.New(message)}
}

func (l *launcher) versionsDir() string {
	dir := l.opts.StateDir
	if dir == "" {
		dir = app.ResolveStateDir(model.ScopeSystem, "", l.env.getenv())
	}
	return filepath.Join(dir, "versions")
}

func (l *launcher) now() time.Time {
	if l.opts.Now != nil {
		return l.opts.Now()
	}
	return l.env.now()
}

func (l *launcher) probe(bind string, port int) bool {
	if l.opts.Probe != nil {
		return l.opts.Probe(bind, port)
	}
	return netutil.Free(bind, port)
}

func (l *launcher) execve(path string, argv, envv []string) error {
	if l.opts.Exec != nil {
		return l.opts.Exec(path, argv, envv)
	}
	return syscall.Exec(path, argv, envv)
}

func ptr[T any](v T) *T { return &v }
