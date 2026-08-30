package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
	"github.com/jlbyh2o/llamaman/internal/store/storetest"
	"github.com/jlbyh2o/llamaman/internal/supervisor"
)

// `llamaman instance-exec` — DESIGN section 5.6.
//
// Two properties are asserted over and over here and they are the whole point
// of D54: the ledger row is opened BEFORE preflight, so every failure a user
// can cause leaves a row a restart policy can count; and the three failures
// that happen before the row can exist (70, 75, 64) leave NO row, because
// writing one is either impossible or unsafe.

const testVersion = "b10621-cpu-bin"

// launcherFixture is one state directory with an instance ready to launch.
type launcherFixture struct {
	*storetest.StateDir
	inst model.Instance
	// execed is the argv the final syscall would have received. Nil means the
	// run never reached step 11.
	execed []string
	// execEnv is the environment that syscall would have received: section
	// 5.7's contract, which `GET /instances/{id}/command` promises to return
	// verbatim.
	execEnv  []string
	execArg0 string
	stderr   *bytes.Buffer
	opts     instanceExecOptions
	env      Env
}

func newLauncherFixture(t *testing.T) *launcherFixture {
	t.Helper()
	sd := storetest.NewStateDir(t, testVersion, "")
	sd.SeedModel(t, "m-1", true)

	inst := storetest.NewInstance("i-1", "qwen", "m-1", 8081, 21001)
	sd.SeedInstance(t, inst)

	f := &launcherFixture{StateDir: sd, inst: inst, stderr: &bytes.Buffer{}}
	f.env = Env{Stdout: &bytes.Buffer{}, Stderr: f.stderr, Getenv: func(string) string { return "" }}
	f.opts = instanceExecOptions{
		StateDir: sd.Dir,
		Now:      func() time.Time { return time.Unix(1700, 0) },
		// Every test that does not care about the port says it is free. The one
		// that does replaces this.
		Probe: func(string, int) bool { return true },
		Exec: func(path string, argv, envv []string) error {
			f.execArg0, f.execed, f.execEnv = path, argv, envv
			return nil
		},
	}
	return f
}

func (f *launcherFixture) run(t *testing.T) error {
	t.Helper()
	return runInstanceExec(f.env, f.inst.Name, f.opts)
}

// exitCode is the status a run asked for, and whether it asked for one. The
// distinction matters: only a status that is part of section 5.6's contract
// travels as an ExitError.
func exitCode(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	code, ok := ExitCode(err)
	if !ok {
		t.Fatalf("error carries no exit-code contract: %v", err)
	}
	return code
}

// TestLauncherOpensAndKeepsTheLedgerRow is the successful start, end to end:
// the row is opened before preflight, filled in with what was rendered, and
// left OPEN across the execve — because `outcome` is written exactly once, at
// the END of a run, and reaching execve is not the end of one (D63).
func TestLauncherOpensAndKeepsTheLedgerRow(t *testing.T) {
	f := newLauncherFixture(t)

	// The daemon stamped its intent before StartUnit. The launcher consumes it.
	mustStampPending(t, f.StateDir, f.inst.ID, model.TriggerUser, nil)

	if err := f.run(t); err != nil {
		t.Fatalf("run: %v\nstderr: %s", err, f.stderr)
	}

	starts := f.Starts(t, f.inst.ID)
	if len(starts) != 1 {
		t.Fatalf("got %d ledger rows, want exactly 1", len(starts))
	}
	row := starts[0]

	if row.Outcome != nil {
		t.Errorf("outcome = %v, want NULL — the run is still in flight (D63)", *row.Outcome)
	}
	if row.ReadyAt != nil {
		t.Error("ready_at is stamped by the SUPERVISOR at the first /health 200, not by the launcher")
	}
	if row.Trigger != model.StartByUser {
		t.Errorf("trigger = %q, want %q — the stamped trigger was not consumed",
			row.Trigger, model.StartByUser)
	}
	if row.ConfigHash != f.inst.ConfigHash {
		t.Errorf("config_hash = %q, want %q", row.ConfigHash, f.inst.ConfigHash)
	}
	if row.ArgvJSON == nil {
		t.Fatal("argv_json is NULL; step 9 must record what was rendered BEFORE the exec")
	}
	if row.LlamacppVersionID == nil || *row.LlamacppVersionID != testVersion {
		t.Errorf("llamacpp_version_id = %v, want %q", row.LlamacppVersionID, testVersion)
	}
	if row.EffectiveConfigHash == nil || *row.EffectiveConfigHash == "" {
		t.Error("effective_config_hash is NULL; the supervisor copies it into applied_config_hash")
	}
	if row.OverrideJSON != nil {
		t.Errorf("override_json = %q on an ordinary start", *row.OverrideJSON)
	}

	// The hand-off columns are cleared in the SAME transaction that consumed
	// them, which is what makes safe start one-shot against a crash or a reboot.
	after := f.Instance(t, f.inst.ID)
	if after.PendingTrigger != nil || after.PendingOverrideJSON != nil {
		t.Errorf("pending columns survived the consume: trigger=%v override=%v",
			after.PendingTrigger, after.PendingOverrideJSON)
	}

	// Step 11 reached, with the version's own binary.
	if f.execArg0 != f.ServerPath {
		t.Errorf("execed %q, want %q", f.execArg0, f.ServerPath)
	}
	var argv []string
	if err := json.Unmarshal([]byte(*row.ArgvJSON), &argv); err != nil {
		t.Fatalf("argv_json does not decode: %v", err)
	}
	if len(f.execed) == 0 || len(argv) == 0 {
		t.Fatal("argv is empty")
	}
	if got, want := len(f.execed), len(argv); got != want {
		t.Errorf("execed %d args, recorded %d — the ledger must describe the run", got, want)
	}
	// `applied_config_hash` stays untouched: a launcher that reached execve and
	// then died during model load must not clear `restart_required` for a
	// configuration that never ran.
	if st := f.Status(t, f.inst.ID); st.AppliedConfigHash != nil {
		t.Errorf("applied_config_hash = %q; only the supervisor writes it, at the first /health 200",
			*st.AppliedConfigHash)
	}
}

// TestLauncherRecordsAnUnstampedStartAsExternal pins the honest half of the
// trigger contract: a boot start of an enabled unit, or a hand-run
// `systemctl start`, is recorded as what it is rather than guessed.
func TestLauncherRecordsAnUnstampedStartAsExternal(t *testing.T) {
	f := newLauncherFixture(t)

	if err := f.run(t); err != nil {
		t.Fatalf("run: %v\nstderr: %s", err, f.stderr)
	}
	starts := f.Starts(t, f.inst.ID)
	if len(starts) != 1 || starts[0].Trigger != model.StartByExternal {
		t.Fatalf("trigger = %v, want %q", starts, model.StartByExternal)
	}
}

// TestLauncherConsumesTheSafeStartOverride is D61: a transient FlagSet patch
// for exactly the next start, applied over the parsed flags, recorded on the
// row, and cleared in the same transaction that consumed it.
func TestLauncherConsumesTheSafeStartOverride(t *testing.T) {
	f := newLauncherFixture(t)

	// The saved configuration has an escape hatch; the safe start drops it, and
	// pins the context down.
	f.Exec(t, `UPDATE instances SET extra_flags = ? WHERE id = ?`, "--log-colors", f.inst.ID)

	override := `{"ctx_size":512}`
	mustStampPending(t, f.StateDir, f.inst.ID, model.TriggerSafeStart, &override)

	if err := f.run(t); err != nil {
		t.Fatalf("run: %v\nstderr: %s", err, f.stderr)
	}

	row := f.Starts(t, f.inst.ID)[0]
	if row.Trigger != model.StartBySafeStart {
		t.Errorf("trigger = %q, want %q", row.Trigger, model.StartBySafeStart)
	}
	if row.OverrideJSON == nil || *row.OverrideJSON != override {
		t.Errorf("override_json = %v, want %q — the history must show this was not the saved configuration",
			row.OverrideJSON, override)
	}

	argv := f.execed
	if !containsPair(argv, "-c", "512") {
		t.Errorf("argv = %v, want the override's ctx_size", argv)
	}
	if contains(argv, "--log-colors") {
		t.Errorf("argv = %v, want extra_flags DROPPED for an overridden run", argv)
	}

	// One-shot: a crash, a reboot or a supervisor restart all find the columns
	// clear and launch the saved configuration.
	after := f.Instance(t, f.inst.ID)
	if after.PendingOverrideJSON != nil {
		t.Error("pending_override_json survived; safe start would repeat forever")
	}
	if after.ExtraFlags != "--log-colors" {
		t.Errorf("extra_flags = %q; the override must never touch the SAVED configuration",
			after.ExtraFlags)
	}
}

// TestLauncherClosesASupersededRow pins section 5.6 step 3's first half. A row
// survives only when a previous run's closing UPDATE could not land, or when an
// external `systemctl start` raced the supervisor — either way that run's
// outcome was never observed, which is exactly what the row must say, and why
// D64 excludes it from the crash-loop count.
func TestLauncherClosesASupersededRow(t *testing.T) {
	f := newLauncherFixture(t)

	mustInsertStart(t, f.StateDir, model.InstanceStart{
		ID:         "start-old",
		InstanceID: f.inst.ID,
		At:         1000,
		Trigger:    model.StartByExternal,
		ConfigHash: f.inst.ConfigHash,
	})

	if err := f.run(t); err != nil {
		t.Fatalf("run: %v\nstderr: %s", err, f.stderr)
	}

	starts := f.Starts(t, f.inst.ID)
	if len(starts) != 2 {
		t.Fatalf("got %d rows, want the superseded one plus the new one", len(starts))
	}
	var old *model.InstanceStart
	for i := range starts {
		if starts[i].ID == "start-old" {
			old = &starts[i]
		}
	}
	if old == nil {
		t.Fatal("the superseded row is missing")
	}
	if old.Outcome == nil || *old.Outcome != model.OutcomeFailed {
		t.Errorf("superseded outcome = %v, want failed", old.Outcome)
	}
	if old.ErrorCode == nil || *old.ErrorCode != supervisor.ErrLauncherSuperseded {
		t.Errorf("superseded error_code = %v, want %q", old.ErrorCode, supervisor.ErrLauncherSuperseded)
	}
	if old.ExitCode != nil {
		t.Errorf("superseded exit_code = %d, want NULL — nothing observed how that run ended",
			*old.ExitCode)
	}
}

// TestLauncherExitCodeContract walks section 5.6's table in full: the status,
// the `error_code` on the row, and — for the three failures that happen before
// the row can exist — the absence of a row.
func TestLauncherExitCodeContract(t *testing.T) {
	cases := []struct {
		name string
		// arrange mutates the fixture into the failure state.
		arrange  func(t *testing.T, f *launcherFixture)
		wantExit int
		// wantErrorCode is "" when no row is expected at all.
		wantErrorCode string
		wantRows      int
	}{
		{
			name: "64 instance missing",
			arrange: func(t *testing.T, f *launcherFixture) {
				f.Exec(t, `UPDATE instances SET deleted_at = 2000 WHERE id = ?`, f.inst.ID)
			},
			wantExit: supervisor.ExitInstanceMissing,
			wantRows: 0,
		},
		{
			name: "65 bad flags",
			arrange: func(t *testing.T, f *launcherFixture) {
				f.Exec(t, `UPDATE instances SET flags_json = ? WHERE id = ?`,
					`{"no_such_flag":1}`, f.inst.ID)
			},
			wantExit:      supervisor.ExitBadFlags,
			wantErrorCode: supervisor.ErrBadFlags,
			wantRows:      1,
		},
		{
			name: "65 draft vocabulary mismatch",
			arrange: func(t *testing.T, f *launcherFixture) {
				f.Exec(t, `UPDATE instances SET draft_validation = 'mismatch' WHERE id = ?`, f.inst.ID)
			},
			wantExit:      supervisor.ExitBadFlags,
			wantErrorCode: supervisor.ErrDraftVocabMismatch,
			wantRows:      1,
		},
		{
			name: "69 runtime missing",
			arrange: func(t *testing.T, f *launcherFixture) {
				if err := os.Remove(f.ServerPath); err != nil {
					t.Fatal(err)
				}
			},
			wantExit:      supervisor.ExitRuntimeMissing,
			wantErrorCode: supervisor.ErrRuntimeMissing,
			wantRows:      1,
		},
		{
			name: "69 runtime rebuilding",
			arrange: func(t *testing.T, f *launcherFixture) {
				f.SetVersionState(t, testVersion, model.VersionBuilding)
			},
			wantExit:      supervisor.ExitRuntimeMissing,
			wantErrorCode: supervisor.ErrRuntimeRebuilding,
			wantRows:      1,
		},
		{
			name: "72 model file missing",
			arrange: func(t *testing.T, f *launcherFixture) {
				var dir, file string
				row := f.QueryRow(t, `SELECT snapshot_dir, primary_file FROM models WHERE id = 'm-1'`)
				if err := row.Scan(&dir, &file); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(filepath.Join(dir, file)); err != nil {
					t.Fatal(err)
				}
			},
			wantExit:      supervisor.ExitModelMissing,
			wantErrorCode: supervisor.ErrModelMissing,
			wantRows:      1,
		},
		{
			name: "78 internal port taken",
			arrange: func(t *testing.T, f *launcherFixture) {
				f.opts.Probe = func(string, int) bool { return false }
			},
			wantExit:      supervisor.ExitPortConflict,
			wantErrorCode: supervisor.ErrPortConflict,
			wantRows:      1,
		},
		{
			name: "75 schema ahead",
			arrange: func(t *testing.T, f *launcherFixture) {
				// A downgrade across a schema bump. Waiting cannot help, so the
				// gate must not wait: a long poll here would be the test that
				// proves it does.
				f.SetSchemaVersion(t, 9999)
				f.opts.SchemaWaitMax = time.Hour
			},
			wantExit: supervisor.ExitSchemaMismatch,
			wantRows: 0,
		},
		{
			name: "75 schema behind after the bounded wait",
			arrange: func(t *testing.T, f *launcherFixture) {
				f.SetSchemaVersion(t, 0)
				f.opts.SchemaWaitMax = 20 * time.Millisecond
				f.opts.SchemaPollEvery = 5 * time.Millisecond
				// The gate measures its deadline against the clock, so a frozen
				// one would never expire.
				f.opts.Now = time.Now
			},
			wantExit: supervisor.ExitSchemaMismatch,
			wantRows: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newLauncherFixture(t)
			tc.arrange(t, f)

			err := f.run(t)
			if got := exitCode(t, err); got != tc.wantExit {
				t.Fatalf("exit = %d, want %d (stderr: %s)", got, tc.wantExit, f.stderr)
			}
			if f.execed != nil {
				t.Error("the launcher execed on a path that must fail preflight")
			}

			starts := f.Starts(t, f.inst.ID)
			if len(starts) != tc.wantRows {
				t.Fatalf("got %d ledger rows, want %d: %+v", len(starts), tc.wantRows, starts)
			}
			if tc.wantRows == 0 {
				return
			}

			row := starts[0]
			if row.Outcome == nil || *row.Outcome != model.OutcomeFailed {
				t.Errorf("outcome = %v, want failed", row.Outcome)
			}
			if row.ExitCode == nil || int(*row.ExitCode) != tc.wantExit {
				t.Errorf("exit_code = %v, want %d", row.ExitCode, tc.wantExit)
			}
			if row.ErrorCode == nil || *row.ErrorCode != tc.wantErrorCode {
				t.Errorf("error_code = %v, want %q", row.ErrorCode, tc.wantErrorCode)
			}
			if row.EndedAt == nil {
				t.Error("ended_at is NULL on a closed row")
			}
			if row.ArgvJSON != nil && tc.wantExit != supervisor.ExitPortConflict {
				t.Errorf("argv_json = %q on a run that never rendered", *row.ArgvJSON)
			}
		})
	}
}

// TestLauncherExecEnvironment is section 5.7's environment contract at the one
// place it is actually applied.
//
// Three of its four rules are about absence, and each is a silent failure
// rather than a loud one: an inherited LLAMA_CACHE gives llama.cpp a second
// model store this product does not manage (SPEC section 3.2), an inherited
// CUDA_VISIBLE_DEVICES renumbers the devices so `--device CUDA1` addresses a
// different physical card (D66), and a missing HF_HUB_CACHE points anything the
// process resolves at whatever cache the ambient environment happened to name.
func TestLauncherExecEnvironment(t *testing.T) {
	f := newLauncherFixture(t)
	f.StateDir.Exec(t,
		`INSERT INTO settings (key, value, updated_at, updated_by) VALUES (?, ?, ?, 'admin')`,
		"hf.hub_dir", `"/srv/hf/hub"`, 1700_000)
	f.env.Getenv = func(string) string { return "" }

	if err := f.run(t); err != nil {
		t.Fatalf("run: %v", err)
	}

	env := map[string]string{}
	for _, kv := range f.execEnv {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}

	for _, tc := range []struct{ key, want string }{
		{"HF_HUB_CACHE", "/srv/hf/hub"},
		// The hub directory ends in /hub, so the courtesy projection is made.
		{"HF_HOME", "/srv/hf"},
	} {
		if env[tc.key] != tc.want {
			t.Errorf("%s = %q, want %q", tc.key, env[tc.key], tc.want)
		}
	}
	for _, key := range []string{"LLAMA_CACHE", "CUDA_VISIBLE_DEVICES", "LD_LIBRARY_PATH"} {
		if v, set := env[key]; set {
			t.Errorf("%s = %q; section 5.7 leaves it unset", key, v)
		}
	}
}

// TestLauncherPortConflictCarriesBothPorts pins F5's "shown in the start
// history with both ports", which is what lets the UI's remediation card name
// the collision without a second query.
func TestLauncherPortConflictCarriesBothPorts(t *testing.T) {
	f := newLauncherFixture(t)
	f.opts.Probe = func(string, int) bool { return false }

	if got := exitCode(t, f.run(t)); got != supervisor.ExitPortConflict {
		t.Fatalf("exit = %d, want %d", got, supervisor.ExitPortConflict)
	}
	row := f.Starts(t, f.inst.ID)[0]
	if row.DetailJSON == nil {
		t.Fatal("detail_json is NULL on a port conflict")
	}
	var detail struct {
		Internal int `json:"internal_port"`
		Public   int `json:"public_port"`
	}
	if err := json.Unmarshal([]byte(*row.DetailJSON), &detail); err != nil {
		t.Fatalf("detail_json does not decode: %v", err)
	}
	if detail.Internal != f.inst.InternalPort || detail.Public != f.inst.PublicPort {
		t.Errorf("detail_json = %+v, want internal %d and public %d",
			detail, f.inst.InternalPort, f.inst.PublicPort)
	}
	// The ports live in `detail_json` and nowhere else, because step 8 runs
	// BEFORE step 9's argv write: this row is the one preflight failure whose
	// remediation needs a value the rendered command line would have carried.
	if row.ArgvJSON != nil {
		t.Errorf("argv_json = %q; step 9 has not run when step 8 fails", *row.ArgvJSON)
	}
}

// TestLauncherNoDatabaseIsExitSeventy is the first row of section 5.6's "no
// row" table: there is no working read-write connection, so writing a row is
// precisely the operation that just failed.
func TestLauncherNoDatabaseIsExitSeventy(t *testing.T) {
	var stderr bytes.Buffer
	env := Env{Stdout: &bytes.Buffer{}, Stderr: &stderr, Getenv: func(string) string { return "" }}

	empty := t.TempDir()
	err := runInstanceExec(env, "qwen", instanceExecOptions{StateDir: empty})
	if got := exitCode(t, err); got != supervisor.ExitDBUnavailable {
		t.Fatalf("exit = %d, want %d", got, supervisor.ExitDBUnavailable)
	}
	// Nothing was created: a launcher that created an empty database in the
	// daemon's state directory would turn a misconfigured StateDirectory= into
	// a corrupt-looking install.
	if _, err := os.Stat(filepath.Join(empty, "llamaman.db")); !os.IsNotExist(err) {
		t.Errorf("the launcher created a database it could not use (%v)", err)
	}
}

// TestLauncherSchemaGateProceedsWhenTheDaemonCatchesUp is the branch that makes
// the bounded wait worth having: a database BEHIND this binary is a daemon that
// has not migrated yet, and waiting is the whole remedy.
func TestLauncherSchemaGateProceedsWhenTheDaemonCatchesUp(t *testing.T) {
	f := newLauncherFixture(t)

	want := highestEmbeddedMigration(t)
	f.SetSchemaVersion(t, want-1)
	f.opts.SchemaWaitMax = 5 * time.Second
	f.opts.SchemaPollEvery = 2 * time.Millisecond
	f.opts.Now = time.Now

	// The daemon finishes migrating a moment later.
	go func() {
		time.Sleep(20 * time.Millisecond)
		f.SetSchemaVersion(t, want)
	}()

	if err := f.run(t); err != nil {
		t.Fatalf("run: %v\nstderr: %s", err, f.stderr)
	}
	if len(f.Starts(t, f.inst.ID)) != 1 {
		t.Error("the gate did not proceed once the schema agreed")
	}
}

func highestEmbeddedMigration(t *testing.T) int {
	t.Helper()
	ms, err := store.Migrations()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	high := 0
	for _, m := range ms {
		if m.Version > high {
			high = m.Version
		}
	}
	return high
}

func mustStampPending(t *testing.T, sd *storetest.StateDir, id string,
	trigger model.PendingTrigger, override *string) {
	t.Helper()
	err := sd.DB.Write(t.Context(), func(ctx context.Context, tx store.Tx) error {
		_, err := sd.DB.StampPendingStart(ctx, tx, id, trigger, override, 1500)
		return err
	})
	if err != nil {
		t.Fatalf("stamp the pending start: %v", err)
	}
}

func mustInsertStart(t *testing.T, sd *storetest.StateDir, row model.InstanceStart) {
	t.Helper()
	err := sd.DB.Write(t.Context(), func(ctx context.Context, tx store.Tx) error {
		return sd.DB.InsertInstanceStart(ctx, tx, row)
	})
	if err != nil {
		t.Fatalf("insert the start row: %v", err)
	}
}

func contains(argv []string, want string) bool {
	for _, a := range argv {
		if a == want {
			return true
		}
	}
	return false
}

func containsPair(argv []string, flag, value string) bool {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag && argv[i+1] == value {
			return true
		}
	}
	return false
}
