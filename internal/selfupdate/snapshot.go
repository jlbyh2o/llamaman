package selfupdate

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/jlbyh2o/llamaman/internal/store"
)

// The D14 pre-update database snapshot (DESIGN section 12.1 step 4).
//
// `VACUUM INTO db-backups/llamaman-<version-being-replaced>-<ts>.db`, taken by
// the daemon immediately before an update and by nothing else. It is the only
// thing that makes a downgrade across a schema bump possible at all, because
// migrations are forward-only.
//
// **Nothing restores it automatically.** Not the judge, not the confirmation
// gate, not `POST /update/apply`, not F12's boot recovery — which is a different
// path, for a CORRUPT database, and restores the newest snapshot rather than one
// the operator chose. Restoring this file is `llamaman restore-db`, it is step 3
// of section 12.4's five-command procedure, and it is a human's decision (D90,
// D94): overwriting a live WAL database out from under a running process
// corrupts it twice over, and discarding every instance, token, benchmark and
// event created since the snapshot is a judgment only the operator can make.
//
// **Retention is stated once and lives in the nightly maintenance job**
// (section 2.11): the newest 7 are kept, oldest deleted first, and the newest
// snapshot is NEVER deleted whatever the count is tuned to. The predicate is
// "the newest" and not "the newest for the version currently installed" because
// of how these files are produced — a snapshot is written only here, immediately
// before an update, and is labeled with the version being REPLACED, so the
// newest one is by construction the database as the version now at
// `<prefix>/llamaman.prev` left it. That is exactly the schema section 12.4's
// procedure needs, and a snapshot labeled with the INSTALLED version either does
// not exist yet or carries a schema the running binary can already open.

// Snapshot writes the pre-update snapshot and returns its path.
//
// `VACUUM INTO` is used rather than a file copy for the reason SQLite documents:
// it produces a consistent database from a live one with a WAL, without holding
// a lock for the length of a copy and without any risk of capturing a torn page.
func Snapshot(ctx context.Context, s *store.Store, l Layout, fromVersion string,
	now time.Time) (string, error) {

	if err := os.MkdirAll(l.BackupsDir(), 0o750); err != nil {
		return "", fmt.Errorf("selfupdate: create %s: %w", l.BackupsDir(), err)
	}
	path := l.BackupsDir() + string(os.PathSeparator) + SnapshotName(fromVersion, now.Unix())

	// A leftover from an interrupted run would make VACUUM INTO fail outright —
	// it refuses to write to a file that exists — and stop-point row 1 already
	// names an ENOSPC here as a state the closing pass exits from. Removing it
	// first keeps a retry from inheriting the first attempt's debris.
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("selfupdate: remove a stale snapshot at %s: %w", path, err)
	}

	// The statement itself lives in internal/store, which is the only package in
	// this repository that writes SQL (D49's first invariant).
	if err := s.VacuumInto(ctx, path); err != nil {
		return "", fmt.Errorf("selfupdate: %w", err)
	}

	// The snapshot inherits the process umask rather than the database's 0600.
	// The database is 0600 because it holds sealed secrets, and so must every
	// copy of it: `db-backups/` is one of the artifacts D46 names as something
	// this design creates and must therefore protect.
	if err := os.Chmod(path, 0o600); err != nil {
		return "", fmt.Errorf("selfupdate: chmod %s: %w", path, err)
	}
	return path, nil
}
