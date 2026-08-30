package app

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jlbyh2o/llamaman/internal/hf/cache"
	"github.com/jlbyh2o/llamaman/internal/jobs"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/settings"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// The nightly `maintenance` job (DESIGN sections 2.11 and 11.1 step 12) and the
// clock that enqueues it.
//
// It lives in the composition root rather than in a domain package because it is
// the one job kind §2.3a says has NO domain row — "the job row IS the record" —
// and because its work spans tables that belong to four different subsystems.
// Putting the sweep beside any one of them would have made that one the owner of
// everybody else's retention policy.
//
// Every rule below is §2.11's, and every one of them is expressed as "delete
// what is outside the window" rather than "keep N", because an append-only table
// swept nightly has no other honest shape.

const (
	// MaintenanceHour and MaintenanceMinute are when the pass runs, in local
	// time. DESIGN says "nightly" and does not pin an hour, so this is policy
	// rather than contract — chosen late enough to be quiet and early enough
	// that a host powered down overnight still catches it on the next start,
	// since the scheduler runs a pass at boot when one is overdue.
	MaintenanceHour   = 3
	MaintenanceMinute = 30

	// LoginAttemptRetention is §2.11's `login_attempts` window. It has no
	// setting in the registry, so the design's number is the number.
	LoginAttemptRetention = 30 * 24 * time.Hour
	// SessionGrace is §2.11's "sessions past `expires_at + 7d`": an expired
	// session is already unusable, and the extra week is what lets "you were
	// signed out" be answerable rather than merely true.
	SessionGrace = 7 * 24 * time.Hour
	// IdempotencyGrace is D65's "past `expires_at + 24h`", well beyond the
	// ten-minute replay window, so a late replay of a just-expired key still
	// finds the row that explains it rather than silently creating a second job.
	IdempotencyGrace = 24 * time.Hour

	// InstanceStartsKept is §2.11's "`instance_starts` 500 per instance", and
	// PER INSTANCE is the load-bearing half: a global cap would let one
	// crash-looping instance erase every other instance's history overnight.
	InstanceStartsKept = 500
	// FitObservationsKept is §2.11's "`fit_observations` 2000 rows" — §8.7's
	// calibration corpus, which is a rolling window rather than an archive.
	FitObservationsKept = 2000
	// TerminalJobRetention is §2.11's "`jobs` in a terminal state 90 days".
	// Live and `interrupted` rows are excluded in the query, not here.
	TerminalJobRetention = 90 * 24 * time.Hour
	// NotificationRetention is §2.11's "`notifications` dismissed + 30 days".
	// An undismissed notification is never swept: it is still asking for
	// something.
	NotificationRetention = 30 * 24 * time.Hour
	// UsageDailyRetention is §2.11's "`instance_usage_daily` and
	// `token_usage_daily` 400 days" — a year plus enough margin that a
	// year-over-year comparison never falls off the end mid-comparison.
	UsageDailyRetention = 400 * 24 * time.Hour
	// SupersededDBRetention is §2.11's "`llamaman.db.superseded-*` 30 days"
	// (§12.4): the database a downgrade set aside, kept long enough to be
	// noticed and no longer.
	SupersededDBRetention = 30 * 24 * time.Hour

	// DBBackupsKept is §2.11's "`db-backups/` the newest 7, oldest deleted
	// first, and the newest snapshot is never deleted whatever the count is
	// tuned to".
	//
	// The exception is not decoration. A snapshot is taken only immediately
	// BEFORE an update and is labeled with the version it replaces, so the
	// newest one is by construction the database as the binary now at
	// `<prefix>/llamaman.prev` left it — which is exactly the file §12.4's
	// downgrade procedure restores (D14). Deleting it would leave `llamaman
	// restore-db` with nothing to restore on the one host that needs it.
	DBBackupsKept = 7
)

// The two directory names the file sweeps below act on, relative to the resolved
// state directory (D72 — never a literal /var/lib/llamaman).
const (
	// DBBackupsDirName holds the D14 pre-update snapshots (§12.1).
	DBBackupsDirName = "db-backups"
	// SupersededDBPrefix is what §12.4's downgrade renames the database to when
	// a newer schema is set aside.
	SupersededDBPrefix = "llamaman.db.superseded-"
)

// maintenanceWorker runs `maintenance`.
type maintenanceWorker struct {
	store    *store.Store
	settings *settings.Cache
	now      func() time.Time
	log      *slog.Logger
	// stateDir is the resolved state directory (D72). It is what the two FILE
	// sweeps act on — `db-backups/` and `llamaman.db.superseded-*` — and an
	// empty value skips them rather than guessing at a path.
	stateDir string
}

// Kind implements jobs.Worker.
func (w *maintenanceWorker) Kind() model.JobKind { return model.JobMaintenance }

// sweepResult counts what one pass removed, for the job's `progress_json` and
// for one log line. Every field is one row of §2.11's retention list.
type sweepResult struct {
	EventsByAge     int64 `json:"events_by_age"`
	EventsByRows    int64 `json:"events_by_rows"`
	LoginAttempts   int64 `json:"login_attempts"`
	Sessions        int64 `json:"sessions"`
	IdempotencyKeys int64 `json:"idempotency_keys"`
	InstanceStarts  int64 `json:"instance_starts"`
	FitObservations int64 `json:"fit_observations"`
	Jobs            int64 `json:"jobs"`
	Notifications   int64 `json:"notifications"`
	UsageDaily      int64 `json:"usage_daily"`
	// The three that are not table rows.
	DBBackups     int `json:"db_backups"`
	SupersededDBs int `json:"superseded_dbs"`
	StaleLocks    int `json:"stale_locks"`
}

// Run implements jobs.Worker.
//
// The sweeps share ONE write transaction. That is not for speed: it is so a pass
// interrupted half way leaves the retention windows consistent with each other,
// and so the next pass — which is a full pass, not a resume — has nothing to
// reconcile. Every statement is a bounded DELETE over an indexed column, so the
// transaction is short even on a database that has been running for a year.
func (w *maintenanceWorker) Run(ctx context.Context, t *jobs.Task) (jobs.Outcome, error) {
	now := w.now()
	var res sweepResult

	err := w.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error

		days := w.settingInt(ctx, "retention.events_days", 90)
		cutoff := now.Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()
		if res.EventsByAge, err = w.store.DeleteEventsBefore(ctx, tx, cutoff); err != nil {
			return err
		}

		// The row cap is checked before it is applied: a COUNT over an
		// append-only table is one index scan, and the DELETE it avoids on the
		// overwhelmingly common night is a sort of the whole table.
		maxRows := w.settingInt(ctx, "retention.events_rows", 200_000)
		count, err := w.store.CountEvents(ctx, tx)
		if err != nil {
			return err
		}
		if count > maxRows {
			if res.EventsByRows, err = w.store.TrimEventsToRows(ctx, tx, maxRows); err != nil {
				return err
			}
		}

		if res.LoginAttempts, err = w.store.DeleteLoginAttemptsBefore(ctx, tx,
			now.Add(-LoginAttemptRetention).UnixMilli()); err != nil {
			return err
		}
		if res.Sessions, err = w.store.DeleteExpiredSessions(ctx, tx,
			now.Add(-SessionGrace).UnixMilli()); err != nil {
			return err
		}
		if res.IdempotencyKeys, err = w.store.DeleteIdempotencyKeysBefore(ctx, tx,
			now.Add(-IdempotencyGrace).UnixMilli()); err != nil {
			return err
		}

		// The per-instance cap, closed rows only: an open row (`outcome IS
		// NULL`) is a run still in flight and the supervisor reads it.
		if res.InstanceStarts, err = w.store.TrimInstanceStartsPerInstance(ctx, tx,
			InstanceStartsKept); err != nil {
			return err
		}
		if res.FitObservations, err = w.store.TrimFitObservationsToRows(ctx, tx,
			FitObservationsKept); err != nil {
			return err
		}
		// Terminal jobs only. `interrupted` is deliberately not terminal (§2.3):
		// D4 keeps the build directory warm and that row is what a retry
		// resumes from.
		if res.Jobs, err = w.store.DeleteTerminalJobsBefore(ctx, tx,
			now.Add(-TerminalJobRetention).UnixMilli()); err != nil {
			return err
		}
		if res.Notifications, err = w.store.DeleteDismissedNotificationsBefore(ctx, tx,
			now.Add(-NotificationRetention).UnixMilli()); err != nil {
			return err
		}
		if res.UsageDaily, err = w.store.DeleteUsageDailyBefore(ctx, tx,
			now.Add(-UsageDailyRetention).UTC().Format(usageDayFormat)); err != nil {
			return err
		}

		// Benchmarks are deliberately absent from this list: §2.11's closing
		// line is "**Benchmarks are never auto-deleted** — they are the
		// product."
		return nil
	})
	if err != nil {
		// A sweep that could not run is worth retrying: the next attempt is a
		// full pass over the same windows, so nothing compounds.
		return jobs.RetryableFailure(jobs.CodeInternalError, err.Error(), nil), nil
	}

	// The three sweeps that are not SQL. They run AFTER the transaction, not
	// inside it: a `.lock` file removal and an `unlink` are not transactional,
	// and holding a write transaction open across a filesystem walk of the model
	// cache would block every other writer for its duration. Each reports its
	// own failure and none of them fails the pass — a directory that could not
	// be swept tonight is swept tomorrow, and losing the row retention with it
	// would be the worse trade.
	res.DBBackups, res.SupersededDBs = w.sweepStateDir(now)
	res.StaleLocks = w.sweepStaleLocks(ctx, now)

	if err := t.SetProgress(ctx, res); err != nil {
		w.log.Debug("could not record the maintenance sweep counts", "error", err)
	}
	w.log.Info("nightly maintenance swept the retention windows",
		"events_by_age", res.EventsByAge, "events_by_rows", res.EventsByRows,
		"login_attempts", res.LoginAttempts, "sessions", res.Sessions,
		"idempotency_keys", res.IdempotencyKeys, "instance_starts", res.InstanceStarts,
		"fit_observations", res.FitObservations, "jobs", res.Jobs,
		"notifications", res.Notifications, "usage_daily", res.UsageDaily,
		"db_backups", res.DBBackups, "superseded_dbs", res.SupersededDBs,
		"stale_locks", res.StaleLocks)

	// No Commit and no DomainWriter: `maintenance` is the one kind §2.3a gives
	// no domain row, so the job row moves alone and that is correct.
	return jobs.Succeeded(nil), nil
}

// usageDayFormat is the 'YYYY-MM-DD' UTC string `instance_usage_daily.day` and
// `token_usage_daily.day` hold (§2.9).
const usageDayFormat = "2006-01-02"

// sweepStateDir applies §2.11's two file rules under the resolved state
// directory: `db-backups/` keeps the newest DBBackupsKept, and
// `llamaman.db.superseded-*` files older than SupersededDBRetention go.
//
// It returns what it removed and reports its own failures. An unreadable
// directory, or one that does not exist yet — which is every host that has never
// self-updated — is not an error: there is simply nothing to sweep.
func (w *maintenanceWorker) sweepStateDir(now time.Time) (backups, superseded int) {
	if w.stateDir == "" {
		return 0, 0
	}
	return w.sweepDBBackups(), w.sweepSupersededDBs(now)
}

// sweepDBBackups keeps the newest DBBackupsKept snapshots, oldest deleted first.
//
// The newest is never deleted whatever the count is tuned to (§2.11), which the
// loop below gets for free by keeping at least one — but the guard is written
// out anyway, because "the newest snapshot is the database `llamaman restore-db`
// restores" (D14) is a promise that must not depend on a constant nobody
// re-reads.
func (w *maintenanceWorker) sweepDBBackups() int {
	dir := filepath.Join(w.stateDir, DBBackupsDirName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			w.log.Warn("could not read the database backup directory", "dir", dir, "error", err)
		}
		return 0
	}

	type snapshot struct {
		name string
		mod  time.Time
	}
	var snaps []snapshot
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		snaps = append(snaps, snapshot{name: e.Name(), mod: info.ModTime()})
	}
	// Newest first, so everything past the keep count is the tail.
	sort.Slice(snaps, func(i, j int) bool {
		if snaps[i].mod.Equal(snaps[j].mod) {
			return snaps[i].name > snaps[j].name
		}
		return snaps[i].mod.After(snaps[j].mod)
	})

	keep := DBBackupsKept
	if keep < 1 {
		keep = 1 // the newest is never deleted
	}
	removed := 0
	for i := keep; i < len(snaps); i++ {
		path := filepath.Join(dir, snaps[i].name)
		if err := os.Remove(path); err != nil {
			w.log.Warn("could not remove an old database backup", "path", path, "error", err)
			continue
		}
		removed++
	}
	return removed
}

// sweepSupersededDBs removes `llamaman.db.superseded-*` files older than
// SupersededDBRetention (§2.11, §12.4).
func (w *maintenanceWorker) sweepSupersededDBs(now time.Time) int {
	entries, err := os.ReadDir(w.stateDir)
	if err != nil {
		w.log.Warn("could not read the state directory", "dir", w.stateDir, "error", err)
		return 0
	}
	cutoff := now.Add(-SupersededDBRetention)
	removed := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), SupersededDBPrefix) {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		path := filepath.Join(w.stateDir, e.Name())
		if err := os.Remove(path); err != nil {
			w.log.Warn("could not remove a superseded database", "path", path, "error", err)
			continue
		}
		removed++
	}
	return removed
}

// sweepStaleLocks is §7.2a's other half of "the file is not removed on release":
// "The nightly maintenance pass removes `.lock` files older than 7 days with no
// holder."
//
// It runs over EVERY registered cache root, not only the primary. A non-primary
// root is scan-and-serve, but `hf download` run by hand against it leaves locks
// there exactly as it does anywhere else, and a lock directory nobody sweeps
// grows one file per blob forever.
func (w *maintenanceWorker) sweepStaleLocks(ctx context.Context, now time.Time) int {
	var roots []model.CacheRoot
	if err := w.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		roots, err = w.store.CacheRoots(ctx, tx)
		return err
	}); err != nil {
		w.log.Warn("could not list the cache roots to sweep their locks", "error", err)
		return 0
	}

	removed := 0
	for _, r := range roots {
		n, err := cache.SweepStaleLocks(r.Path, now)
		removed += n
		if err != nil {
			// A root on an unplugged disk is the ordinary reason, and it is not
			// a reason to fail the pass.
			w.log.Warn("could not sweep stale interop locks", "root", r.Path, "error", err)
		}
	}
	return removed
}

// settingInt reads a retention knob, falling back to the design's own number
// when the settings cache cannot answer. A sweep that refused to run because a
// setting would not load would let the table it was meant to bound grow without
// limit, which is the failure the setting exists to prevent.
func (w *maintenanceWorker) settingInt(ctx context.Context, key string, def int64) int64 {
	if w.settings == nil {
		return def
	}
	v, err := w.settings.GetInt(ctx, key)
	if err != nil || v <= 0 {
		if err != nil {
			w.log.Warn("could not read a retention setting; using the default",
				"key", key, "default", def, "error", err)
		}
		return def
	}
	return v
}

// scheduleMaintenance is the "nightly maintenance" of §11.1 step 12's background
// workers: one goroutine that enqueues a `maintenance` job at MaintenanceHour
// every day, and once at startup when the pass is overdue.
//
// It enqueues rather than sweeping directly, which is what makes the pass
// observable in `GET /jobs`, cancelable, and — through
// `idx_jobs_one_live_per_subject` on the fixed synthetic subject
// `('system','maintenance')` — impossible to run twice at once. A host that was
// powered down at 03:30 still gets its sweep, because the boot pass is
// unconditional rather than a catch-up computation nobody could test.
func (d *daemon) scheduleMaintenance(ctx context.Context) {
	d.enqueueMaintenance(ctx)

	for {
		wait := untilNextMaintenance(d.opts.Now())
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			d.enqueueMaintenance(ctx)
		}
	}
}

// enqueueMaintenance queues one pass. A live pass already holding the subject is
// the ordinary answer to "the previous one is still running", not an error.
func (d *daemon) enqueueMaintenance(ctx context.Context) {
	_, err := d.queue.Enqueue(ctx, jobs.EnqueueParams{Kind: model.JobMaintenance})
	switch {
	case err == nil:
		return
	case errors.Is(err, context.Canceled):
		return
	}
	var me model.Error
	if errors.As(err, &me) && me.Code == model.CodeJobInFlight {
		d.log.Debug("a maintenance pass is already live; skipping this one")
		return
	}
	d.log.Warn("could not enqueue the nightly maintenance pass", "error", err)
}

// untilNextMaintenance is how long to sleep before the next run, in local time
// so that "03:30" means what a human reading the journal expects it to mean.
func untilNextMaintenance(now time.Time) time.Duration {
	next := time.Date(now.Year(), now.Month(), now.Day(),
		MaintenanceHour, MaintenanceMinute, 0, 0, now.Location())
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next.Sub(now)
}
