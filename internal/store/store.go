package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"runtime"
	"strings"

	_ "modernc.org/sqlite" // pure-Go driver; keeps CGO_ENABLED=0 (DESIGN section 2)
)

// pragmas are applied to every connection in both pools (DESIGN section 2).
// busy_timeout matters even though the write pool serializes in Go, because
// `llamaman instance-exec` opens its own short-lived read-write connection from
// a separate process.
var pragmas = []string{
	"journal_mode(WAL)",
	"foreign_keys(ON)",
	"busy_timeout(5000)",
	"synchronous(NORMAL)",
	"temp_store(MEMORY)",
	"auto_vacuum(INCREMENTAL)",
}

// Store owns the two pools over the single database file at Path. The write
// pool is capped at one connection — WAL permits a single writer, and
// serializing in Go turns SQLITE_BUSY into a queue — while the read pool is
// sized to GOMAXPROCS.
type Store struct {
	// Path is the resolved database file, <state_dir>/llamaman.db. The state
	// directory is resolved per D72 and is never a literal.
	Path string

	// RW is the read-write pool, MaxOpenConns(1).
	RW *sql.DB
	// RO is the read pool, sized to GOMAXPROCS and held to query_only.
	RO *sql.DB
}

// Open opens both pools against path and verifies each is reachable. It does not
// run migrations or the integrity check; those are separate steps the boot
// sequence orders around its readiness notification (DESIGN section 11.1).
func Open(ctx context.Context, path string) (*Store, error) {
	rw, err := sql.Open("sqlite", dsn(path, nil))
	if err != nil {
		return nil, fmt.Errorf("open read-write pool: %w", err)
	}
	rw.SetMaxOpenConns(1)

	ro, err := sql.Open("sqlite", dsn(path, []string{"query_only(1)"}))
	if err != nil {
		rw.Close()
		return nil, fmt.Errorf("open read pool: %w", err)
	}
	ro.SetMaxOpenConns(runtime.GOMAXPROCS(0))

	s := &Store{Path: path, RW: rw, RO: ro}
	if err := rw.PingContext(ctx); err != nil {
		s.Close()
		return nil, fmt.Errorf("ping read-write pool: %w", err)
	}
	if err := ro.PingContext(ctx); err != nil {
		s.Close()
		return nil, fmt.Errorf("ping read pool: %w", err)
	}
	return s, nil
}

// Close closes both pools.
func (s *Store) Close() error {
	var first error
	for _, db := range []*sql.DB{s.RO, s.RW} {
		if db == nil {
			continue
		}
		if err := db.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// dsn renders a modernc.org/sqlite DSN carrying the section-2 pragmas plus any
// extras this pool adds.
func dsn(path string, extra []string) string {
	q := make([]string, 0, len(pragmas)+len(extra))
	for _, p := range append(append([]string{}, pragmas...), extra...) {
		q = append(q, "_pragma="+url.QueryEscape(p))
	}
	return "file:" + path + "?" + strings.Join(q, "&")
}
