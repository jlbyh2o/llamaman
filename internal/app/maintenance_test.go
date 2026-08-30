package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jlbyh2o/llamaman/internal/hf/cache"
	"github.com/jlbyh2o/llamaman/internal/jobs"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/settings"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// The nightly sweep, against a real database.
//
// Every one of these windows is a DELETE with a cutoff, and the only thing worth
// asserting about a DELETE with a cutoff is that it removes what is outside the
// window and nothing that is inside it. A fake store would assert that the test
// computed the same cutoff twice.

func newSweepStore(t *testing.T) (*store.Store, *settings.Cache) {
	t.Helper()
	ctx := context.Background()

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "llamaman.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.Migrate(ctx, store.MigrateOptions{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	set := settings.New(settings.NewRegistry(), st)
	if err := set.Load(ctx); err != nil {
		t.Fatalf("load settings: %v", err)
	}
	return st, set
}

// TestMaintenanceSweepsEveryWindow is §2.11's retention table, one row at a
// time: an old row on each side of each cutoff, and only the old one goes.
func TestMaintenanceSweepsEveryWindow(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st, set := newSweepStore(t)
	now := time.Unix(1_800_000_000, 0).UTC()

	// One row comfortably outside each window and one comfortably inside it.
	old := now.Add(-200 * 24 * time.Hour)
	recent := now.Add(-time.Hour)

	seed := func(at time.Time, tag string) {
		t.Helper()
		if err := st.Write(ctx, func(ctx context.Context, tx store.Tx) error {
			if err := st.AppendEvent(ctx, tx, model.Event{
				ID: store.NewID(at), At: at.UnixMilli(), Level: model.LevelInfo,
				Category: model.CategorySystem, Action: "test_" + tag,
				Actor: model.ActorSystem, Message: tag,
			}); err != nil {
				return err
			}
			if err := st.InsertLoginAttempt(ctx, tx, model.LoginAttempt{
				ID: store.NewID(at), At: at.UnixMilli(), IP: "192.0.2.1", Success: true,
			}); err != nil {
				return err
			}
			// A session's window is `expires_at + 7d`, so the expiry is what
			// decides, not the creation time.
			return st.InsertSession(ctx, tx, model.Session{
				ID:         store.NewID(at),
				TokenHash:  "hash-" + tag,
				CSRFSecret: "csrf-" + tag,
				CreatedAt:  at.UnixMilli(),
				LastSeenAt: at.UnixMilli(),
				ExpiresAt:  at.UnixMilli(),
			})
		}); err != nil {
			t.Fatalf("seed %s rows: %v", tag, err)
		}
	}
	seed(old, "old")
	seed(recent, "recent")

	// An idempotency key needs a job to reference.
	if err := st.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		j := model.Job{
			ID: store.NewID(old), Kind: model.JobMaintenance,
			SubjectType: model.SubjectSystem, SubjectID: model.SubjectIDMaintenance,
			State: model.JobSucceeded, Priority: jobs.DefaultPriority,
			RunAfter: old.UnixMilli(), MaxAttempts: 1, CreatedAt: old.UnixMilli(),
		}
		if err := st.InsertJob(ctx, tx, j); err != nil {
			return err
		}
		for _, k := range []struct {
			key string
			at  time.Time
		}{{"old-key", old}, {"recent-key", recent}} {
			if err := st.InsertIdempotencyKey(ctx, tx, model.IdempotencyKey{
				Key: k.key, Route: "POST /api/v1/llamacpp/versions",
				RequestFingerprint: "f", JobID: j.ID,
				CreatedAt: k.at.UnixMilli(),
				ExpiresAt: k.at.Add(model.IdempotencyWindow).UnixMilli(),
			}, k.at.UnixMilli()); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed idempotency keys: %v", err)
	}

	w := &maintenanceWorker{store: st, settings: set, now: func() time.Time { return now },
		log: quietLogger()}
	runMaintenance(t, st, w, now)

	cases := []struct {
		table string
		want  int64
	}{
		{"events", 1},
		{"login_attempts", 1},
		{"sessions", 1},
		{"idempotency_keys", 1},
	}
	for _, tc := range cases {
		if got := countRows(t, st, tc.table); got != tc.want {
			t.Errorf("%s has %d rows after the sweep, want %d — "+
				"the row outside its window should be gone and the one inside it kept",
				tc.table, got, tc.want)
		}
	}
}

// TestMaintenanceTrimsEventsToTheRowCap is §2.11's second events rule: "90 days
// OR 200k rows", whichever bites first. The cap is lowered so the test does not
// have to write two hundred thousand rows to observe it.
func TestMaintenanceTrimsEventsToTheRowCap(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st, set := newSweepStore(t)
	now := time.Unix(1_800_000_000, 0).UTC()

	if _, err := set.Set(ctx, "retention.events_rows", []byte("5"),
		model.UpdatedByAdmin); err != nil {
		t.Fatalf("set retention.events_rows: %v", err)
	}

	if err := st.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		for i := range 20 {
			at := now.Add(-time.Duration(i) * time.Minute)
			if err := st.AppendEvent(ctx, tx, model.Event{
				ID: store.NewID(at), At: at.UnixMilli(), Level: model.LevelInfo,
				Category: model.CategorySystem, Action: "test",
				Actor: model.ActorSystem, Message: fmt.Sprintf("event %d", i),
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed events: %v", err)
	}

	w := &maintenanceWorker{store: st, settings: set, now: func() time.Time { return now },
		log: quietLogger()}
	runMaintenance(t, st, w, now)

	if got := countRows(t, st, "events"); got != 5 {
		t.Errorf("events = %d after the trim, want the newest 5", got)
	}
}

// TestUntilNextMaintenance: the pass runs at a fixed local hour, and the wait is
// to the NEXT occurrence of it whichever side of the hour the clock is on.
func TestUntilNextMaintenance(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		now  time.Time
		want time.Duration
	}{
		{
			name: "before the hour, later today",
			now:  time.Date(2026, 3, 1, 1, 30, 0, 0, time.Local),
			want: 2 * time.Hour,
		},
		{
			name: "after the hour, tomorrow",
			now:  time.Date(2026, 3, 1, 4, 30, 0, 0, time.Local),
			want: 23 * time.Hour,
		},
		{
			name: "exactly on the hour, tomorrow",
			now:  time.Date(2026, 3, 1, MaintenanceHour, MaintenanceMinute, 0, 0, time.Local),
			want: 24 * time.Hour,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := untilNextMaintenance(tc.now); got != tc.want {
				t.Errorf("untilNextMaintenance(%s) = %s, want %s", tc.now, got, tc.want)
			}
		})
	}
}

// runMaintenance drives one pass through the real queue, because the worker's
// Run takes a *jobs.Task and only the queue makes one.
func runMaintenance(t *testing.T, st *store.Store, w jobs.Worker, now time.Time) {
	t.Helper()
	ctx := context.Background()

	q, err := jobs.New(st, jobs.Options{
		BootID: "01SWEEPSWEEPSWEEPSWEEPSWEEP",
		Now:    func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("jobs.New: %v", err)
	}
	if err := q.Register(w); err != nil {
		t.Fatalf("register the maintenance worker: %v", err)
	}
	res, err := q.Enqueue(ctx, jobs.EnqueueParams{Kind: model.JobMaintenance})
	if err != nil {
		t.Fatalf("enqueue the maintenance pass: %v", err)
	}
	ran, err := q.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if !ran {
		t.Fatal("RunOnce found no maintenance job")
	}
	j, err := q.Job(ctx, res.Job.ID)
	if err != nil {
		t.Fatalf("read the maintenance job: %v", err)
	}
	if j.State != model.JobSucceeded {
		t.Fatalf("maintenance job state = %q (%v), want succeeded", j.State, j.ErrorMessage)
	}
}

func countRows(t *testing.T, st *store.Store, table string) int64 {
	t.Helper()
	var n int64
	if err := st.RO.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// TestMaintenanceSweepsTheStateDirectory is §2.11's two FILE rules: `db-backups/`
// keeps the newest seven, oldest deleted first and the newest never deleted
// whatever the count, and `llamaman.db.superseded-*` goes after thirty days.
//
// The newest-is-never-deleted rule is the one worth a test of its own: a
// snapshot is taken only immediately before an update and labeled with the
// version it replaces, so the newest one IS the database `llamaman restore-db`
// would restore (D14, §12.4).
func TestMaintenanceSweepsTheStateDirectory(t *testing.T) {
	t.Parallel()

	st, set := newSweepStore(t)
	now := time.Unix(1_800_000_000, 0).UTC()
	dir := t.TempDir()

	backups := filepath.Join(dir, DBBackupsDirName)
	if err := os.MkdirAll(backups, 0o750); err != nil {
		t.Fatal(err)
	}
	// Ten snapshots, newest first by modification time.
	var names []string
	for i := range 10 {
		name := fmt.Sprintf("llamaman-%02d.db", i)
		names = append(names, name)
		p := filepath.Join(backups, name)
		if err := os.WriteFile(p, []byte("db"), 0o600); err != nil {
			t.Fatal(err)
		}
		mod := now.Add(-time.Duration(i) * time.Hour)
		if err := os.Chtimes(p, mod, mod); err != nil {
			t.Fatal(err)
		}
	}

	// Two superseded databases, one on each side of the thirty-day window.
	for _, tc := range []struct {
		name string
		age  time.Duration
	}{
		{SupersededDBPrefix + "old", 60 * 24 * time.Hour},
		{SupersededDBPrefix + "recent", time.Hour},
	} {
		p := filepath.Join(dir, tc.name)
		if err := os.WriteFile(p, []byte("db"), 0o600); err != nil {
			t.Fatal(err)
		}
		mod := now.Add(-tc.age)
		if err := os.Chtimes(p, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	// A file that merely lives beside them is not this sweep's business.
	if err := os.WriteFile(filepath.Join(dir, "llamaman.db"), []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}

	w := &maintenanceWorker{store: st, settings: set, now: func() time.Time { return now },
		log: quietLogger(), stateDir: dir}
	runMaintenance(t, st, w, now)

	kept, err := os.ReadDir(backups)
	if err != nil {
		t.Fatalf("read the backup directory: %v", err)
	}
	if len(kept) != DBBackupsKept {
		t.Errorf("%d backups remain, want the newest %d", len(kept), DBBackupsKept)
	}
	if _, err := os.Stat(filepath.Join(backups, names[0])); err != nil {
		t.Errorf("THE NEWEST SNAPSHOT WAS DELETED — it is the one `llamaman restore-db` "+
			"restores after a downgrade: %v", err)
	}
	if _, err := os.Stat(filepath.Join(backups, names[9])); !os.IsNotExist(err) {
		t.Error("the oldest snapshot survived the cap")
	}

	if _, err := os.Stat(filepath.Join(dir, SupersededDBPrefix+"old")); !os.IsNotExist(err) {
		t.Error("a superseded database older than thirty days survived")
	}
	if _, err := os.Stat(filepath.Join(dir, SupersededDBPrefix+"recent")); err != nil {
		t.Errorf("a superseded database inside its window was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "llamaman.db")); err != nil {
		t.Errorf("THE LIVE DATABASE WAS REMOVED: %v", err)
	}
}

// TestMaintenanceSweepsStaleInteropLocks is §7.2a's other half of "the file is
// not removed on release": "The nightly maintenance pass removes `.lock` files
// older than 7 days with no holder."
//
// It runs over every registered root, not only the primary: `hf download` run by
// hand against a scan-and-serve root leaves locks there exactly as it does
// anywhere else.
func TestMaintenanceSweepsStaleInteropLocks(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st, set := newSweepStore(t)
	now := time.Unix(1_800_000_000, 0).UTC()

	const repo = "bartowski/Test-Model-GGUF"
	hubs := map[string]bool{ // path -> is primary
		filepath.Join(t.TempDir(), "primary-hub"):   true,
		filepath.Join(t.TempDir(), "secondary-hub"): false,
	}
	for path, primary := range hubs {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			etag string
			age  time.Duration
		}{
			{"stale", 30 * 24 * time.Hour},
			{"fresh", time.Hour},
		} {
			p := cache.LockPath(path, repo, tc.etag)
			if err := os.MkdirAll(filepath.Dir(p), cache.DirMode); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, nil, cache.FileMode); err != nil {
				t.Fatal(err)
			}
			mod := now.Add(-tc.age)
			if err := os.Chtimes(p, mod, mod); err != nil {
				t.Fatal(err)
			}
		}
		if err := st.Write(ctx, func(ctx context.Context, tx store.Tx) error {
			return st.InsertCacheRoot(ctx, tx, model.CacheRoot{
				ID: store.NewID(now), Path: path, IsPrimary: primary,
				Writable: true, SymlinksOK: true, CreatedAt: now.UnixMilli(),
			})
		}); err != nil {
			t.Fatalf("seed the cache root: %v", err)
		}
	}

	// A stale lock another process still holds is left alone: "with no holder"
	// is half the rule, and it is established by taking the lock rather than by
	// reading a timestamp.
	var heldPath string
	for path := range hubs {
		heldPath = cache.LockPath(path, repo, "stale")
		break
	}
	held, err := cache.Acquire(heldPath)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = held.Release() }()

	w := &maintenanceWorker{store: st, settings: set, now: func() time.Time { return now },
		log: quietLogger(), stateDir: t.TempDir()}
	runMaintenance(t, st, w, now)

	for path := range hubs {
		fresh := cache.LockPath(path, repo, "fresh")
		if _, err := os.Stat(fresh); err != nil {
			t.Errorf("a lock inside its window was removed from %s: %v", path, err)
		}
		stale := cache.LockPath(path, repo, "stale")
		_, err := os.Stat(stale)
		if stale == heldPath {
			if err != nil {
				t.Errorf("a stale lock ANOTHER PROCESS HOLDS was removed: %v", err)
			}
			continue
		}
		if !os.IsNotExist(err) {
			t.Errorf("a stale unheld lock survived in %s", path)
		}
	}
}
