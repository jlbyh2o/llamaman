package store

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The migration runner (DESIGN sections 2 and 11.1 step 4).
//
// Section 14 declines a migration library on the grounds that "a ~120-line
// runner over embedded SQL has no failure modes we do not want to own". This is
// that runner, and the failure modes it owns are exactly three, all of them
// fatal and all of them named in section 11.1 step 4:
//
//  1. The database is AHEAD of this binary — MAX(schema_migrations.version) is
//     greater than the highest migration embedded here. That is the state a
//     downgrade leaves behind (§12.4, D90). The daemon does not open it for
//     writing, does not migrate and does not serve: running a v14 query set
//     against a v15 schema can corrupt data, and there is no forward-only
//     migration that undoes one.
//  2. An applied migration's checksum does not match the embedded file, or the
//     applied version has no embedded file at all. Either means the history this
//     database records is not the history this binary would have written.
//  3. A migration failed to apply, in which case its transaction is rolled back
//     and no `schema_migrations` row is written, so the next boot retries the
//     same version against the same schema.
//
// Migrations are forward-only and applied ONE PER TRANSACTION, so a failure
// leaves every earlier version committed and the failing one absent.

//go:embed migrations/*.sql
var migrationFS embed.FS

// Migration is one embedded numbered SQL file.
type Migration struct {
	Version  int    // the NNNN prefix
	Name     string // the part after the underscore, without .sql
	Checksum string // sha256 of SQL, hex
	SQL      string
}

// Filename returns the file's name as embedded, which is what a fatal error
// message names so an operator can find it.
func (m Migration) Filename() string {
	return fmt.Sprintf("%04d_%s.sql", m.Version, m.Name)
}

// AppliedMigration is one row of `schema_migrations`.
type AppliedMigration struct {
	Version   int
	Name      string
	Checksum  string
	AppliedAt int64
}

// SchemaAheadError is failure mode 1: the database was written by a newer
// release than this binary. Section 11.1 step 4 requires the daemon to log one
// journald line naming BOTH versions and then exit non-zero, so both are fields
// rather than only a message.
type SchemaAheadError struct {
	DBVersion     int // MAX(schema_migrations.version) in the file
	BinaryVersion int // the highest migration embedded in this binary
}

func (e *SchemaAheadError) Error() string {
	return fmt.Sprintf(
		"database schema version %d is newer than this binary's %d: "+
			"the database was written by a newer release and this binary must not migrate or serve it",
		e.DBVersion, e.BinaryVersion)
}

// Unwrap lets callers test with errors.Is(err, ErrSchemaAhead) without knowing
// the concrete type.
func (e *SchemaAheadError) Unwrap() error { return ErrSchemaAhead }

// ChecksumMismatchError is failure mode 2.
type ChecksumMismatchError struct {
	Version  int
	Name     string
	Applied  string // the checksum recorded in schema_migrations
	Embedded string // the checksum of the file in this binary, "" when absent
}

func (e *ChecksumMismatchError) Error() string {
	if e.Embedded == "" {
		return fmt.Sprintf(
			"migration %04d_%s.sql is recorded as applied but is not embedded in this binary "+
				"(recorded checksum %s)", e.Version, e.Name, e.Applied)
	}
	return fmt.Sprintf(
		"migration %04d_%s.sql has changed since it was applied: recorded %s, embedded %s",
		e.Version, e.Name, e.Applied, e.Embedded)
}

// Unwrap lets callers test with errors.Is(err, ErrChecksumMismatch).
func (e *ChecksumMismatchError) Unwrap() error { return ErrChecksumMismatch }

// The two sentinels the boot sequence branches on.
var (
	// ErrSchemaAhead is the downgrade refusal of §11.1 step 4. It is a start
	// failure on purpose: after StartLimitBurst attempts the unit reaches
	// `failed`, OnFailure= starts the judge, and the version that CAN open the
	// database is put back, so an accidental downgrade self-corrects (§12.2).
	ErrSchemaAhead = errors.New("store: database schema is newer than this binary")
	// ErrChecksumMismatch is fatal: an applied migration is not the file this
	// binary embeds.
	ErrChecksumMismatch = errors.New("store: applied migration checksum mismatch")
)

// Migrations returns the embedded set, ordered by version. The result is
// computed from the embedded files on every call, so a test can compare it
// against the database without reaching into package state.
func Migrations() ([]Migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}
	out := make([]Migration, 0, len(entries))
	seen := make(map[int]string, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		version, name, err := parseMigrationName(e.Name())
		if err != nil {
			return nil, err
		}
		if prev, dup := seen[version]; dup {
			return nil, fmt.Errorf(
				"embedded migrations %s and %s share version %d", prev, e.Name(), version)
		}
		seen[version] = e.Name()

		body, err := migrationFS.ReadFile(path.Join("migrations", e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", e.Name(), err)
		}
		sum := sha256.Sum256(body)
		out = append(out, Migration{
			Version:  version,
			Name:     name,
			Checksum: hex.EncodeToString(sum[:]),
			SQL:      string(body),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// parseMigrationName splits "0001_init.sql" into 1 and "init".
func parseMigrationName(filename string) (int, string, error) {
	base := strings.TrimSuffix(filename, ".sql")
	prefix, name, ok := strings.Cut(base, "_")
	if !ok || prefix == "" || name == "" {
		return 0, "", fmt.Errorf(
			"migration %q is not named <version>_<name>.sql", filename)
	}
	version, err := strconv.Atoi(prefix)
	if err != nil || version <= 0 {
		return 0, "", fmt.Errorf("migration %q has no positive integer version prefix", filename)
	}
	return version, name, nil
}

// MigrateOptions carries the two things the boot sequence needs the runner to
// do besides applying SQL.
type MigrateOptions struct {
	// Now supplies the applied_at stamp. Nil means time.Now.
	Now func() time.Time

	// Heartbeat, when set, is called every HeartbeatEvery for as long as a
	// migration is executing. §11.1 step 4: while a migration is running the
	// daemon sends EXTEND_TIMEOUT_USEC= every 10 s over $NOTIFY_SOCKET, so a
	// legitimately slow start extends TimeoutStartSec= instead of being killed
	// and judged (D88, §5.4). The store does not know what sd_notify is; it only
	// promises to call this while it is busy.
	Heartbeat func()

	// HeartbeatEvery defaults to DefaultHeartbeatEvery.
	HeartbeatEvery time.Duration

	// BeforeFirst, when set, is called once — after the schema gate and the
	// checksum verification have passed, and BEFORE the first migration is
	// applied — but only when there is at least one migration to apply. That
	// instant is D92's disarm point: applying a migration is the exact moment
	// <prefix>/llamaman.prev stops being a binary that could open this database,
	// so it is the exact moment the judge's second ConditionPathExists= must stop
	// holding. The rule fires on "about to migrate", not on "a migration
	// committed", and D92 states why that direction of trade is the right one.
	// An error returned here aborts the migration with nothing applied.
	BeforeFirst func(pending []Migration) error
}

// DefaultHeartbeatEvery is the 10 s interval §11.1 step 4 names.
const DefaultHeartbeatEvery = 10 * time.Second

// Migrate applies every embedded migration the database has not yet recorded,
// one per transaction, and returns the ones it applied. It is idempotent: a
// second call on an up-to-date database applies nothing and returns an empty
// slice.
func (s *Store) Migrate(ctx context.Context, opts MigrateOptions) ([]Migration, error) {
	now := opts.Now
	if now == nil {
		now = time.Now
	}

	embedded, err := Migrations()
	if err != nil {
		return nil, err
	}
	if len(embedded) == 0 {
		return nil, errors.New("store: no migrations are embedded in this binary")
	}

	applied, err := s.AppliedMigrations(ctx, s.RW)
	if err != nil {
		return nil, err
	}

	pending, err := planMigrations(embedded, applied)
	if err != nil {
		return nil, err
	}
	if len(pending) == 0 {
		return nil, nil
	}
	if opts.BeforeFirst != nil {
		if err := opts.BeforeFirst(pending); err != nil {
			return nil, err
		}
	}

	stop := heartbeat(opts.Heartbeat, opts.HeartbeatEvery)
	defer stop()

	done := make([]Migration, 0, len(pending))
	for _, m := range pending {
		if err := s.applyOne(ctx, m, now().UnixMilli()); err != nil {
			return done, err
		}
		done = append(done, m)
	}
	return done, nil
}

// planMigrations is the whole decision, separated from the I/O so it can be
// tested as a pure function: it enforces the schema gate, verifies every applied
// checksum, and returns what is left to do in version order.
func planMigrations(embedded []Migration, applied []AppliedMigration) ([]Migration, error) {
	byVersion := make(map[int]Migration, len(embedded))
	highest := 0
	for _, m := range embedded {
		byVersion[m.Version] = m
		if m.Version > highest {
			highest = m.Version
		}
	}

	appliedSet := make(map[int]AppliedMigration, len(applied))
	dbVersion := 0
	for _, a := range applied {
		appliedSet[a.Version] = a
		if a.Version > dbVersion {
			dbVersion = a.Version
		}
	}

	// Gate first: a database written by a newer release is refused before any
	// checksum is even consulted, because this binary's opinion about the files
	// it does not have is worthless.
	if dbVersion > highest {
		return nil, &SchemaAheadError{DBVersion: dbVersion, BinaryVersion: highest}
	}

	for _, a := range applied {
		m, ok := byVersion[a.Version]
		if !ok {
			return nil, &ChecksumMismatchError{Version: a.Version, Name: a.Name, Applied: a.Checksum}
		}
		if m.Checksum != a.Checksum {
			return nil, &ChecksumMismatchError{
				Version:  a.Version,
				Name:     m.Name,
				Applied:  a.Checksum,
				Embedded: m.Checksum,
			}
		}
	}

	pending := make([]Migration, 0, len(embedded))
	for _, m := range embedded {
		if _, done := appliedSet[m.Version]; !done {
			pending = append(pending, m)
		}
	}
	return pending, nil
}

// applyOne runs one migration and records it in the SAME transaction, so a
// crash can never leave a schema change without its row or a row without its
// schema change.
//
// `schema_migrations` is not pre-created anywhere: 0001_init.sql declares it
// like every other table, and because the recording INSERT shares this
// transaction with the CREATE that precedes it, the first migration of a fresh
// database records itself. That is why AppliedMigrations tolerates a missing
// table instead — the alternative, an IF NOT EXISTS copy of the DDL in Go, would
// be a second declaration of a table section 2.1 declares once.
func (s *Store) applyOne(ctx context.Context, m Migration, at int64) error {
	return s.Write(ctx, func(ctx context.Context, tx Tx) error {
		if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
			return fmt.Errorf("apply %s: %w", m.Filename(), err)
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, name, checksum, applied_at)
			 VALUES (?, ?, ?, ?)`,
			m.Version, m.Name, m.Checksum, at)
		if err != nil {
			return fmt.Errorf("record %s: %w", m.Filename(), err)
		}
		return nil
	})
}

// AppliedMigrations returns every recorded migration, oldest first. It tolerates
// a missing table by returning nothing, so it may be called on a fresh file.
func (s *Store) AppliedMigrations(ctx context.Context, tx Tx) ([]AppliedMigration, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT version, name, checksum, applied_at
		   FROM schema_migrations
		  ORDER BY version`)
	if err != nil {
		if isMissingTable(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("select schema_migrations: %w", err)
	}
	defer rows.Close()

	var out []AppliedMigration
	for rows.Next() {
		var a AppliedMigration
		if err := rows.Scan(&a.Version, &a.Name, &a.Checksum, &a.AppliedAt); err != nil {
			return nil, fmt.Errorf("scan schema_migrations: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SchemaVersion returns MAX(schema_migrations.version), or 0 on a database that
// has never been migrated. It is what §11.1 step 10 writes into
// `runtime_info.schema_version` and what the schema gate compares.
func (s *Store) SchemaVersion(ctx context.Context, tx Tx) (int, error) {
	var v *int
	err := tx.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v)
	if err != nil {
		if isMissingTable(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("select schema version: %w", err)
	}
	if v == nil {
		return 0, nil
	}
	return *v, nil
}

// isMissingTable reports whether err is SQLite's "no such table", which is the
// ordinary state of a database this binary has never migrated.
func isMissingTable(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such table")
}

// heartbeat starts a ticker that calls notify every d until the returned stop
// func runs. A nil notify or a non-positive interval makes it a no-op, so the
// callers need no branch.
func heartbeat(notify func(), every time.Duration) (stop func()) {
	if notify == nil {
		return func() {}
	}
	if every <= 0 {
		every = DefaultHeartbeatEvery
	}
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				notify()
			case <-done:
				return
			}
		}
	}()
	return func() {
		close(done)
		<-finished
	}
}
