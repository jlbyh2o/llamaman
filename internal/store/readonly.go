package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"runtime"
	"strings"
)

// OpenReadOnly opens an EXISTING database for reading only, exactly as DESIGN
// section 11.3 requires of `llamaman status` and `llamaman doctor`:
// `file:<path>?mode=ro&_pragma=query_only(1)`.
//
// Two properties matter more than the connection itself, and both are why this
// is a separate constructor rather than a flag on Open:
//
//   - It CREATES NOTHING. `mode=ro` means SQLite will not create the database,
//     and the write-side pragmas are not applied — `journal_mode=WAL` in
//     particular writes to the file header, and `auto_vacuum` and `synchronous`
//     are settings for a connection that will write. A root `llamaman status`
//     that created `<state_dir>/llamaman.db`, or a root-owned `-wal`/`-shm`
//     beside one, would leave a database the service identity can never write
//     again — which §11.3 forbids for every root-invocable subcommand, and which
//     a CI test asserts with a directory diff.
//   - The returned Store has NO write pool. Write panics on it, deliberately:
//     "read-only" is enforced by the absence of the connection rather than by a
//     convention a later caller could forget.
//
// The caller must have checked that the file exists; §11.3's three cases — file
// absent, file present with no sidecars and a caller who is not the owner, file
// present with the daemon running — are policy the CLI owns, because only it
// knows the euid it is running under and what to print.
func OpenReadOnly(ctx context.Context, path string) (*Store, error) {
	ro, err := sql.Open("sqlite", readOnlyDSN(path))
	if err != nil {
		return nil, fmt.Errorf("open read-only pool: %w", err)
	}
	ro.SetMaxOpenConns(runtime.GOMAXPROCS(0))

	s := &Store{Path: path, RO: ro}
	if err := ro.PingContext(ctx); err != nil {
		ro.Close()
		return nil, fmt.Errorf("open %s read-only: %w", path, err)
	}
	return s, nil
}

// IntegrityCheckRead runs `PRAGMA integrity_check(N)` over a caller's
// transaction, which is what `llamaman status` and `llamaman doctor` need:
// IntegrityCheck runs on the WRITE pool, deliberately — it is the connection the
// daemon is about to migrate and serve on — and a Store from OpenReadOnly does
// not have one.
//
// The two are the same question asked of two different connections, and both are
// worth having: the daemon must know that the connection it will WRITE through is
// sound, while the CLI must be able to ask without opening one at all (§11.3).
func (s *Store) IntegrityCheckRead(ctx context.Context, tx Tx, maxErrors int) error {
	if maxErrors <= 0 {
		maxErrors = DefaultIntegrityMaxErrors
	}
	// The argument cannot be a bound parameter: PRAGMA takes a literal.
	rows, err := tx.QueryContext(ctx, fmt.Sprintf("PRAGMA integrity_check(%d)", maxErrors))
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
		return fmt.Errorf("run integrity check: no result rows")
	}
	return &IntegrityError{Lines: lines}
}

// readOnlyPragmas is the section-2 pragma list minus every member that writes or
// only means something for a writing connection. It is a function rather than a
// literal so that a pragma added to `pragmas` is inherited here unless it is
// named below, which is the direction that fails safe.
func readOnlyPragmas() []string {
	skip := map[string]bool{
		"journal_mode(WAL)":        true, // writes the file header
		"auto_vacuum(INCREMENTAL)": true, // a property of a writer
		"synchronous(NORMAL)":      true, // a property of a writer
	}
	out := make([]string, 0, len(pragmas)+1)
	for _, p := range pragmas {
		if !skip[p] {
			out = append(out, p)
		}
	}
	return append(out, "query_only(1)")
}

// readOnlyDSN renders §11.3's connection string.
func readOnlyDSN(path string) string {
	ps := readOnlyPragmas()
	q := make([]string, 0, len(ps)+1)
	for _, p := range ps {
		q = append(q, "_pragma="+url.QueryEscape(p))
	}
	q = append(q, "mode=ro")
	return "file:" + path + "?" + strings.Join(q, "&")
}
