package models

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jlbyh2o/llamaman/internal/hf/cache/cachetest"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// The models tests run against a REAL SQLite file rather than a fake store, and
// that is deliberate. The guards this package exists for are guards about
// foreign keys and cascades — `instances.model_id` is ON DELETE RESTRICT and
// `models.root_id` is ON DELETE CASCADE — and §7.2a's warning is precisely that
// a guard written against a fake would pass while the real transaction failed
// with a raw foreign-key violation. Section 15 says the same thing about the
// cache-root detach case: "written against a real SQLite file with
// foreign_keys=ON, because the bug it guards is precisely a RESTRICT the guard
// did not anticipate".
//
// No test here contains SQL. Seeding an instance goes through the store's own
// exported InsertInstance/InsertInstanceStatus, which is what D49's invariant
// asks: only internal/store contains SQL, and everything else calls it.

// fixture is a service over a temp database and a temp hub cache.
type fixture struct {
	t     *testing.T
	svc   *Service
	db    *store.Store
	hub   *cachetest.Hub
	root  RootView
	hash  *fakeHashes
	clock int64
}

// fakeHashes records D69's recompute calls. It is the only way to assert that
// the models service told the instance service its resolved paths moved, since
// that call is the entire mechanism keeping `config_hash` from going stale.
type fakeHashes struct{ ids []string }

func (f *fakeHashes) RecomputeConfigHash(_ context.Context, _ store.Tx, ids ...string) error {
	f.ids = append(f.ids, ids...)
	return nil
}

func (f *fakeHashes) reset() { f.ids = nil }

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()

	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "llamaman.db"))
	if err != nil {
		t.Fatalf("open the database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Migrate(ctx, store.MigrateOptions{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	f := &fixture{t: t, db: db, hub: cachetest.New(t), hash: &fakeHashes{}, clock: 1_700_000_000_000}
	svc, err := New(Config{
		Store:  db,
		Hashes: f.hash,
		Now:    func() time.Time { f.clock += 1000; return time.UnixMilli(f.clock) },
	})
	if err != nil {
		t.Fatalf("build the service: %v", err)
	}
	f.svc = svc

	root, _, err := svc.SetPrimaryRoot(ctx, f.hub.Dir, model.DetectedFromManual)
	if err != nil {
		t.Fatalf("SetPrimaryRoot: %v", err)
	}
	f.root = root
	return f
}

// scan runs one full reconciliation of the primary root, the way the
// `cache_scan` worker would, and returns the counters.
//
// It calls the service's own scan body rather than driving a queue: the worker
// wrapper is job bookkeeping, and what these tests are about is what the
// reconciliation writes.
func (f *fixture) scan() model.CacheScan {
	f.t.Helper()
	ctx := context.Background()

	row, _, err := f.svc.RequestScan(ctx, "", model.ScanTriggerManual)
	if err != nil {
		f.t.Fatalf("RequestScan: %v", err)
	}
	counters, err := f.svc.runScan(ctx, nil, row.ID, ScanParams{RootID: row.RootID, Path: f.hub.Dir})
	if err != nil {
		f.t.Fatalf("runScan: %v", err)
	}
	counters.ID = row.ID
	return counters
}

// models lists the catalog, newest identity order, excluding deleted rows.
func (f *fixture) models() []View {
	f.t.Helper()
	views, err := f.svc.List(context.Background(), ListParams{})
	if err != nil {
		f.t.Fatalf("List: %v", err)
	}
	return views
}

// find returns the one catalog row whose primary file matches, failing when
// there is not exactly one.
func (f *fixture) find(primaryFile string) View {
	f.t.Helper()
	var out []View
	for _, v := range f.models() {
		if v.PrimaryFile == primaryFile {
			out = append(out, v)
		}
	}
	if len(out) != 1 {
		f.t.Fatalf("found %d catalog rows with primary file %q, want 1", len(out), primaryFile)
	}
	return out[0]
}

// isRoot reports whether this process ignores the mode bits, which is what
// makes the non-writable-root cases unproducible under `sudo go test`.
func isRoot() bool { return os.Geteuid() == 0 }

// chmod is os.Chmod, named here so the read-only-directory cases read as one
// step rather than as a filesystem call in the middle of an assertion.
func chmod(path string, mode os.FileMode) error { return os.Chmod(path, mode) }

// seedInstance writes an instance referencing a model, through the store's own
// API. deleted stamps `deleted_at`, which is the D68 soft delete the two guards
// read differently.
func (f *fixture) seedInstance(name, modelID string, deleted bool) string {
	f.t.Helper()

	id := store.NewID(time.UnixMilli(f.clock))
	inst := model.Instance{
		ID: id, Name: name, ModelID: &modelID,
		PublicPort: 18000 + len(name), InternalPort: 19000 + len(name),
		AuthMode: model.AuthToken, RestartPolicy: model.RestartOnFailure,
		RestartMax: 5, RestartWindowSec: 600,
		FlagsJSON: `{}`, ConfigHash: "hash-" + id,
		DesiredState: model.DesiredStopped, DraftValidation: model.DraftOK,
		UnitName:   "llamaman-instance@" + name + ".service",
		Generation: 1, CreatedAt: f.clock, UpdatedAt: f.clock,
	}
	err := f.db.Write(context.Background(), func(ctx context.Context, tx store.Tx) error {
		if err := f.db.InsertInstance(ctx, tx, inst); err != nil {
			return err
		}
		if err := f.db.InsertInstanceStatus(ctx, tx, model.InstanceStatus{
			InstanceID: id, State: model.InstanceUnknown,
			LastChangeAt: f.clock, GPUAttribution: model.AttributionUnknown,
		}); err != nil {
			return err
		}
		if deleted {
			_, err := f.db.SoftDeleteInstance(ctx, tx, id, f.clock)
			return err
		}
		return nil
	})
	if err != nil {
		f.t.Fatalf("seed instance %s: %v", name, err)
	}
	return id
}
