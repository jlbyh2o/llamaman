package store

import (
	"context"
	"database/sql"
	"errors"
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

// ErrNotFound is returned by every query method whose row is absent. It wraps
// sql.ErrNoRows so a caller may test for either, but it is the one this package
// returns: "no such row" is a domain answer here, not a driver detail, and a
// handler mapping it to 404 should not have to import database/sql to do so.
var ErrNotFound = fmt.Errorf("store: row not found: %w", sql.ErrNoRows)

// Store owns the two pools over the single database file at Path. The write
// pool is capped at one connection — WAL permits a single writer, and
// serializing in Go turns SQLITE_BUSY into a queue — while the read pool is
// sized to GOMAXPROCS.
type Store struct {
	// Path is the resolved database file, <state_dir>/llamaman.db. The state
	// directory is resolved per D72 and is never a literal.
	Path string

	// RW is the read-write pool, MaxOpenConns(1), whose transactions are
	// BEGIN IMMEDIATE.
	RW *sql.DB
	// RO is the read pool, sized to GOMAXPROCS and held to query_only.
	RO *sql.DB
}

// Tx is the transactional surface every query method in this package takes
// (DESIGN section 1: "methods take ctx + *Tx; no business logic"). Both *sql.DB
// and *sql.Tx satisfy it, which is what lets a caller compose several writes
// into ONE transaction — the design asks for that on nearly every path: a job
// row and its domain row are written together, a setup claim is stamped in the
// same transaction that creates the admin account, an idempotency key is
// inserted alongside the job it names.
//
// Store never opens a transaction on a caller's behalf inside a query method.
// Transaction boundaries are the caller's, because only the caller knows which
// writes have to commit together.
type Tx interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Open opens both pools against path and verifies each is reachable. It does not
// run migrations or the integrity check; those are separate steps the boot
// sequence orders around its readiness notification (DESIGN section 11.1).
func Open(ctx context.Context, path string) (*Store, error) {
	rw, err := sql.Open("sqlite", dsn(path, nil, []string{"_txlock=immediate"}))
	if err != nil {
		return nil, fmt.Errorf("open read-write pool: %w", err)
	}
	rw.SetMaxOpenConns(1)

	ro, err := sql.Open("sqlite", dsn(path, []string{"query_only(1)"}, nil))
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

// Write runs fn inside one BEGIN IMMEDIATE transaction on the write pool and
// commits when it returns nil. The write lock is taken at BEGIN rather than at
// the first write (the pool's DSN carries _txlock=immediate), which is what D97
// asks for wherever a guard is evaluated and acted on in the same transaction:
// a deferred transaction would read its guard under a shared lock and could be
// beaten to the write.
//
// A panic inside fn rolls back and re-panics, so a bug can never leave a
// transaction open on the one write connection.
func (s *Store) Write(ctx context.Context, fn func(context.Context, Tx) error) (err error) {
	tx, err := s.RW.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin write transaction: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			tx.Rollback()
			panic(p)
		}
		if err != nil {
			tx.Rollback()
		}
	}()
	if err = fn(ctx, tx); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// Read runs fn inside one deferred transaction on the read pool, so a multi-
// statement read sees one consistent snapshot, and always rolls back — the pool
// is query_only, so there is nothing to commit.
func (s *Store) Read(ctx context.Context, fn func(context.Context, Tx) error) error {
	tx, err := s.RO.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin read transaction: %w", err)
	}
	defer tx.Rollback()
	return fn(ctx, tx)
}

// dsn renders a modernc.org/sqlite DSN carrying the section-2 pragmas plus any
// extra pragmas and driver parameters this pool adds.
func dsn(path string, extraPragmas, extraParams []string) string {
	q := make([]string, 0, len(pragmas)+len(extraPragmas)+len(extraParams))
	for _, p := range append(append([]string{}, pragmas...), extraPragmas...) {
		q = append(q, "_pragma="+url.QueryEscape(p))
	}
	q = append(q, extraParams...)
	return "file:" + path + "?" + strings.Join(q, "&")
}

// notFound maps the driver's sql.ErrNoRows onto this package's sentinel and
// leaves every other error alone.
func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
