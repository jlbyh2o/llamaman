package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// The boot integrity check (DESIGN section 11.1 step 3).
//
// Step 3 is "open or create llamaman.db, enforce mode 0600, run
// PRAGMA integrity_check". This file owns the last of those three. What happens
// on FAILURE is deliberately not here: F12's recovery — move the file aside,
// restore the newest db-backups/ entry, else start fresh, and raise a
// notification listing what was lost — is a decision about files and
// notifications that belongs to the composition root. The store's job is to give
// that decision an unambiguous answer, which means distinguishing "the check ran
// and the database is corrupt" from "the check could not run", because the first
// is F12 and the second is not.

// IntegrityError is a database that failed PRAGMA integrity_check. Lines carries
// the rows SQLite emitted, which the F12 notification quotes so a user can see
// what was lost rather than only that something was.
type IntegrityError struct {
	Lines []string
}

func (e *IntegrityError) Error() string {
	return "store: PRAGMA integrity_check failed: " + strings.Join(e.Lines, "; ")
}

// CheckOptions carries the heartbeat §11.1 step 4 requires around the slow parts
// of boot. While PRAGMA integrity_check is running the daemon sends
// EXTEND_TIMEOUT_USEC= every 10 s over $NOTIFY_SOCKET, so a legitimately slow
// start extends TimeoutStartSec= instead of being killed and judged (D88).
// A large database on slow storage is exactly the case this exists for.
type CheckOptions struct {
	// Heartbeat, when set, is called every HeartbeatEvery while the check runs.
	Heartbeat func()
	// HeartbeatEvery defaults to DefaultHeartbeatEvery.
	HeartbeatEvery time.Duration
	// MaxErrors bounds how many problems SQLite reports before it stops looking.
	// Zero means DefaultIntegrityMaxErrors. The check is for a yes/no decision
	// followed by a restore, not for a repair, so the whole list is never needed
	// and an unbounded scan of a badly damaged file is time the boot does not
	// have.
	MaxErrors int
}

// DefaultIntegrityMaxErrors bounds PRAGMA integrity_check(N).
const DefaultIntegrityMaxErrors = 100

// IntegrityCheck runs PRAGMA integrity_check against the write pool and returns
// nil when SQLite answers with the single row "ok".
//
// It runs on the write pool deliberately: that is the connection the daemon is
// about to migrate and serve on, so a file the read pool could open but the
// write pool could not would slip past a check made anywhere else.
func (s *Store) IntegrityCheck(ctx context.Context, opts CheckOptions) error {
	maxErrors := opts.MaxErrors
	if maxErrors <= 0 {
		maxErrors = DefaultIntegrityMaxErrors
	}

	stop := heartbeat(opts.Heartbeat, opts.HeartbeatEvery)
	defer stop()

	// The argument cannot be a bound parameter: PRAGMA takes a literal.
	rows, err := s.RW.QueryContext(ctx, fmt.Sprintf("PRAGMA integrity_check(%d)", maxErrors))
	if err != nil {
		return fmt.Errorf("run integrity check: %w", err)
	}
	defer rows.Close()

	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return fmt.Errorf("scan integrity check: %w", err)
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read integrity check: %w", err)
	}

	if len(lines) == 1 && lines[0] == "ok" {
		return nil
	}
	if len(lines) == 0 {
		// SQLite always returns at least one row; no rows means the check did not
		// actually run, which is not the same fact as corruption and must not be
		// reported as one.
		return fmt.Errorf("run integrity check: no result rows")
	}
	return &IntegrityError{Lines: lines}
}

// ForeignKeyCheck runs PRAGMA foreign_key_check and reports every violation as
// "<table> row <rowid> -> <parent>". Migrations run with foreign_keys=ON so
// violations cannot be introduced by ordinary writes, but `llamaman doctor` and
// the post-restore path of §12.4 both want to say so rather than assume it.
func (s *Store) ForeignKeyCheck(ctx context.Context, tx Tx) ([]string, error) {
	rows, err := tx.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return nil, fmt.Errorf("run foreign key check: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var table, parent string
		var rowid *int64
		var fkID int64
		if err := rows.Scan(&table, &rowid, &parent, &fkID); err != nil {
			return nil, fmt.Errorf("scan foreign key check: %w", err)
		}
		id := int64(-1)
		if rowid != nil {
			id = *rowid
		}
		out = append(out, fmt.Sprintf("%s row %d -> %s", table, id, parent))
	}
	return out, rows.Err()
}
