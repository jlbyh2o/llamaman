// Package storetest seeds a real database for the two components that cannot
// be tested against a fake one: `instance-exec` (DESIGN section 5.6) and the
// supervisor (section 5.8).
//
// Every other consumer of internal/store fakes it, and that is the right rule —
// only internal/store contains SQL (D49 invariant 1), so a test elsewhere must
// not carry an INSERT to satisfy a foreign key. The launcher breaks the rule's
// PREMISE rather than the rule: it is a separate process whose first act is to
// open the database file itself, with its own short-lived read-write
// connection, so "fake the store" is not available — there is nothing to
// inject. Its preflight then reads `llamacpp_versions` and `models`, two tables
// whose services do not exist yet and therefore have no INSERT methods to
// borrow.
//
// So the fixtures live HERE: under the store tree, where SQL belongs, in a
// package whose name says it is for tests and which nothing in the product
// imports. The alternative was exported production INSERTs with no production
// caller, which would have been the same SQL in a worse place.
package storetest

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// StateDir is a fully populated state directory: a migrated database, and the
// `versions/` layout of section 6.1 with `active` pointing at a real directory.
type StateDir struct {
	// Dir is the state directory itself — what `$STATE_DIRECTORY` names.
	Dir string
	// DB is the open store over `<Dir>/llamaman.db`.
	DB *store.Store
	// VersionID is the `llamacpp_versions.id` seeded active.
	VersionID string
	// ServerPath is `<Dir>/versions/<VersionID>/bin/llama-server`.
	ServerPath string
}

// NewStateDir builds the whole fixture: a migrated database in a temp
// directory, one `ready` active llama.cpp version, and an executable at that
// version's `bin/llama-server`.
//
// server is copied to that path. Passing "" writes a stub shell script instead,
// which is enough for every test that only needs the preflight to find an
// executable and never actually execs it.
func NewStateDir(t *testing.T, versionID, server string) *StateDir {
	t.Helper()
	dir := t.TempDir()

	db, err := store.Open(context.Background(), filepath.Join(dir, "llamaman.db"))
	if err != nil {
		t.Fatalf("open the fixture database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Migrate(context.Background(), store.MigrateOptions{}); err != nil {
		t.Fatalf("migrate the fixture database: %v", err)
	}

	bin := filepath.Join(dir, "versions", versionID, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatalf("create the version directory: %v", err)
	}
	serverPath := filepath.Join(bin, "llama-server")
	if server == "" {
		// `#!/bin/sh` + `exit 0` is a real executable that a stat and an execve
		// both accept, which is all a preflight test needs.
		if err := os.WriteFile(serverPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatalf("write the stub server: %v", err)
		}
	} else {
		b, err := os.ReadFile(server)
		if err != nil {
			t.Fatalf("read %s: %v", server, err)
		}
		if err := os.WriteFile(serverPath, b, 0o755); err != nil {
			t.Fatalf("install the stub server: %v", err)
		}
	}

	// `versions/active` is a SYMLINK, and it is the only activation mechanism
	// (section 6.1). Tests that resolve it are testing the real thing.
	if err := os.Symlink(filepath.Join(dir, "versions", versionID),
		filepath.Join(dir, "versions", "active")); err != nil {
		t.Fatalf("link versions/active: %v", err)
	}

	sd := &StateDir{Dir: dir, DB: db, VersionID: versionID, ServerPath: serverPath}
	sd.SeedVersion(t, versionID, model.VersionReady, true)
	return sd
}

// SeedVersion inserts (or replaces) the active `llamacpp_versions` row.
//
// state is a parameter because the launcher's exit 69 `runtime_rebuilding`
// branch and the supervisor's "no start is attempted at all" branch both turn
// on a row that is active and NOT ready — the D78 state a forced rebuild puts
// the active version into.
func (sd *StateDir) SeedVersion(t *testing.T, id string, state model.VersionState, active bool) {
	t.Helper()
	sd.exec(t,
		`INSERT INTO llamacpp_versions
		   (id, channel, tag, acquisition, backend, dir_name, state, is_active,
		    supports_fit, created_at)
		 VALUES (?, 'stable', ?, 'prebuilt', 'cpu', ?, ?, ?, 1, 1000)
		 ON CONFLICT(id) DO UPDATE SET state = excluded.state, is_active = excluded.is_active`,
		id, id, id, string(state), boolInt(active))
}

// SetVersionState moves the active row's state, which is how a test simulates a
// forced rebuild starting and finishing.
func (sd *StateDir) SetVersionState(t *testing.T, id string, state model.VersionState) {
	t.Helper()
	sd.exec(t, `UPDATE llamacpp_versions SET state = ? WHERE id = ?`, string(state), id)
}

// SeedModel writes a `models` row and creates the file it points at, so the
// launcher's step-6 stat succeeds.
//
// Passing exists=false writes the row and NOT the file, which is exactly F4's
// condition: a model the catalog knows about and the disk does not.
func (sd *StateDir) SeedModel(t *testing.T, id string, exists bool) string {
	t.Helper()

	root := "root-" + id
	sd.exec(t,
		`INSERT INTO hf_cache_roots (id, path, is_primary, writable, created_at)
		 VALUES (?, ?, 1, 1, 1000)
		 ON CONFLICT(id) DO NOTHING`,
		root, filepath.Join(sd.Dir, "hub", id))

	snapshot := filepath.Join(sd.Dir, "hub", id, "snapshots", "deadbeef")
	primary := "model.gguf"
	if exists {
		if err := os.MkdirAll(snapshot, 0o755); err != nil {
			t.Fatalf("create the snapshot directory: %v", err)
		}
		if err := os.WriteFile(filepath.Join(snapshot, primary), []byte("GGUF"), 0o644); err != nil {
			t.Fatalf("write the model file: %v", err)
		}
	}

	sd.exec(t,
		`INSERT INTO models
		   (id, root_id, repo_id, revision, kind, state, origin, snapshot_dir,
		    primary_file, created_at, updated_at)
		 VALUES (?, ?, ?, 'deadbeef', 'text', 'ready', 'llamaman', ?, ?, 1000, 1000)`,
		id, root, "acme/"+id, snapshot, primary)

	return filepath.Join(snapshot, primary)
}

// SeedInstance writes a config row and its status row in one transaction, which
// is the pairing section 2.8 requires of every creator — the status row has
// three NOT NULL columns and cannot spring into existence lazily, which is what
// lets every reader use an inner join.
func (sd *StateDir) SeedInstance(t *testing.T, inst model.Instance) {
	t.Helper()
	err := sd.DB.Write(context.Background(), func(ctx context.Context, tx store.Tx) error {
		if err := sd.DB.InsertInstance(ctx, tx, inst); err != nil {
			return err
		}
		return sd.DB.InsertInstanceStatus(ctx, tx, model.InstanceStatus{
			InstanceID:     inst.ID,
			State:          model.InstanceUnknown,
			LastChangeAt:   inst.CreatedAt,
			GPUAttribution: model.AttributionUnknown,
		})
	})
	if err != nil {
		t.Fatalf("seed instance %s: %v", inst.Name, err)
	}
}

// NewInstance builds a config row with the defaults the schema declares, so a
// test names only the columns it is about.
func NewInstance(id, name, modelID string, public, internal int) model.Instance {
	return model.Instance{
		ID:               id,
		Name:             name,
		ModelID:          &modelID,
		PublicPort:       public,
		InternalPort:     internal,
		AuthMode:         model.AuthToken,
		RestartPolicy:    model.RestartOnFailure,
		RestartMax:       5,
		RestartWindowSec: 600,
		FlagsJSON:        `{"ctx_size":4096}`,
		ConfigHash:       "hash-" + id,
		DesiredState:     model.DesiredStopped,
		DraftValidation:  model.DraftOK,
		UnitName:         "llamaman-instance@" + name + ".service",
		Generation:       1,
		CreatedAt:        1000,
		UpdatedAt:        1000,
	}
}

// SetSchemaVersion rewrites `schema_migrations` so the launcher's gate sees a
// database that is BEHIND or AHEAD of this binary (section 5.6a).
//
// It is the only way to exercise either branch: both compare against the
// migration set compiled into the binary under test, which a test cannot
// change.
func (sd *StateDir) SetSchemaVersion(t *testing.T, version int) {
	t.Helper()
	sd.exec(t, `DELETE FROM schema_migrations`)
	sd.exec(t,
		`INSERT INTO schema_migrations (version, name, checksum, applied_at)
		 VALUES (?, 'fixture', 'fixture', 1000)`, version)
}

// Starts returns the ledger for an instance, newest first.
func (sd *StateDir) Starts(t *testing.T, instanceID string) []model.InstanceStart {
	t.Helper()
	var out []model.InstanceStart
	err := sd.DB.Read(context.Background(), func(ctx context.Context, tx store.Tx) error {
		got, err := sd.DB.InstanceStarts(ctx, tx, instanceID, 0)
		out = got
		return err
	})
	if err != nil {
		t.Fatalf("read the ledger of %s: %v", instanceID, err)
	}
	return out
}

// Instance re-reads a config row, for assertions about the hand-off columns.
func (sd *StateDir) Instance(t *testing.T, id string) model.Instance {
	t.Helper()
	var out model.Instance
	err := sd.DB.Read(context.Background(), func(ctx context.Context, tx store.Tx) error {
		got, err := sd.DB.Instance(ctx, tx, id)
		out = got
		return err
	})
	if err != nil {
		t.Fatalf("read instance %s: %v", id, err)
	}
	return out
}

// Status re-reads a status row.
func (sd *StateDir) Status(t *testing.T, id string) model.InstanceStatus {
	t.Helper()
	var out model.InstanceStatus
	err := sd.DB.Read(context.Background(), func(ctx context.Context, tx store.Tx) error {
		got, err := sd.DB.InstanceStatus(ctx, tx, id)
		out = got
		return err
	})
	if err != nil {
		t.Fatalf("read the status of %s: %v", id, err)
	}
	return out
}

// SeedRuntimeInfo writes the singleton row the supervisor's boot decision
// compares against. hostBootID may be empty, which is the state of a database
// no supervisor has ever run against.
func (sd *StateDir) SeedRuntimeInfo(t *testing.T, hostBootID string, hostBootAt, bootAt int64) {
	t.Helper()
	info := model.RuntimeInfo{DaemonVersion: "test", DaemonCommit: "test", BootAt: &bootAt}
	if hostBootID != "" {
		info.HostBootID = &hostBootID
		info.HostBootAt = &hostBootAt
	}
	err := sd.DB.Write(context.Background(), func(ctx context.Context, tx store.Tx) error {
		return sd.DB.PutRuntimeInfo(ctx, tx, info)
	})
	if err != nil {
		t.Fatalf("seed runtime_info: %v", err)
	}
}

// Exec runs one statement against the fixture. It is exported for the handful
// of assertions that need a column no query method returns.
func (sd *StateDir) Exec(t *testing.T, query string, args ...any) {
	t.Helper()
	sd.exec(t, query, args...)
}

// QueryRow runs one scalar query against the fixture.
func (sd *StateDir) QueryRow(t *testing.T, query string, args ...any) *sql.Row {
	t.Helper()
	return sd.DB.RW.QueryRow(query, args...)
}

func (sd *StateDir) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := sd.DB.RW.Exec(query, args...); err != nil {
		t.Fatalf("fixture statement failed: %v\n%s", err, query)
	}
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
