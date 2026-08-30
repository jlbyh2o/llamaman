package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// TestMigrateIsIdempotent: the second boot applies nothing. Forward-only means a
// re-run must be a no-op, not a re-apply.
func TestMigrateIsIdempotent(t *testing.T) {
	s := newTestStore(t) // already migrated once
	ctx := context.Background()

	again, err := s.Migrate(ctx, MigrateOptions{})
	if err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("second Migrate applied %d migrations, want 0", len(again))
	}

	v, err := s.SchemaVersion(ctx, s.RO)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	embedded, err := Migrations()
	if err != nil {
		t.Fatalf("Migrations: %v", err)
	}
	if want := embedded[len(embedded)-1].Version; v != want {
		t.Errorf("SchemaVersion = %d, want %d", v, want)
	}
}

// TestSchemaVersionOnFreshFile: a database nobody has migrated answers 0 rather
// than failing, because AppliedMigrations and SchemaVersion are both called
// before the first migration has created their table.
func TestSchemaVersionOnFreshFile(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "llamaman.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	v, err := s.SchemaVersion(ctx, s.RO)
	if err != nil {
		t.Fatalf("SchemaVersion on a fresh file: %v", err)
	}
	if v != 0 {
		t.Errorf("SchemaVersion = %d, want 0", v)
	}
	applied, err := s.AppliedMigrations(ctx, s.RO)
	if err != nil {
		t.Fatalf("AppliedMigrations on a fresh file: %v", err)
	}
	if len(applied) != 0 {
		t.Errorf("AppliedMigrations = %v, want none", applied)
	}
}

// TestMigrateRefusesNewerSchema is §11.1 step 4's downgrade gate: a database
// written by a newer release is not opened for writing, not migrated and not
// served. The error carries both versions because the journald line the daemon
// logs before exiting names both.
func TestMigrateRefusesNewerSchema(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, name, checksum, applied_at)
			 VALUES (99, 'from_the_future', 'deadbeef', 1)`)
		return err
	})

	_, err := s.Migrate(ctx, MigrateOptions{})
	if !errors.Is(err, ErrSchemaAhead) {
		t.Fatalf("Migrate = %v, want ErrSchemaAhead", err)
	}
	var ahead *SchemaAheadError
	if !errors.As(err, &ahead) {
		t.Fatalf("error %v does not carry a *SchemaAheadError", err)
	}
	if ahead.DBVersion != 99 {
		t.Errorf("DBVersion = %d, want 99", ahead.DBVersion)
	}
	if ahead.BinaryVersion != 1 {
		t.Errorf("BinaryVersion = %d, want 1", ahead.BinaryVersion)
	}
}

// TestMigrateRefusesChecksumMismatch: an applied migration that is not the file
// this binary embeds is fatal. The history the database records must be the
// history this binary would have written, or nothing downstream can be trusted.
func TestMigrateRefusesChecksumMismatch(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE schema_migrations SET checksum = 'tampered' WHERE version = 1`)
		return err
	})

	_, err := s.Migrate(ctx, MigrateOptions{})
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("Migrate = %v, want ErrChecksumMismatch", err)
	}
	var mismatch *ChecksumMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("error %v does not carry a *ChecksumMismatchError", err)
	}
	if mismatch.Applied != "tampered" || mismatch.Embedded == "" {
		t.Errorf("mismatch = %+v, want both checksums populated", mismatch)
	}
}

// TestPlanMigrations is the runner's whole decision as a pure function, which is
// where its three failure modes are cheapest to pin down.
func TestPlanMigrations(t *testing.T) {
	a := Migration{Version: 1, Name: "init", Checksum: "aaa"}
	b := Migration{Version: 2, Name: "more", Checksum: "bbb"}
	embedded := []Migration{a, b}

	applied := func(m Migration, sum string) AppliedMigration {
		return AppliedMigration{Version: m.Version, Name: m.Name, Checksum: sum, AppliedAt: 1}
	}

	tests := []struct {
		name        string
		embedded    []Migration
		applied     []AppliedMigration
		wantPending []Migration
		wantErr     error
	}{
		{
			name:        "fresh database applies everything",
			embedded:    embedded,
			wantPending: []Migration{a, b},
		},
		{
			name:        "up to date applies nothing",
			embedded:    embedded,
			applied:     []AppliedMigration{applied(a, "aaa"), applied(b, "bbb")},
			wantPending: []Migration{},
		},
		{
			name:        "partially migrated applies the rest",
			embedded:    embedded,
			applied:     []AppliedMigration{applied(a, "aaa")},
			wantPending: []Migration{b},
		},
		{
			name:     "database ahead of the binary is refused",
			embedded: []Migration{a},
			applied:  []AppliedMigration{applied(a, "aaa"), applied(b, "bbb")},
			wantErr:  ErrSchemaAhead,
		},
		{
			name:     "changed migration is refused",
			embedded: embedded,
			applied:  []AppliedMigration{applied(a, "not-aaa")},
			wantErr:  ErrChecksumMismatch,
		},
		{
			name:     "applied migration this binary does not have is refused",
			embedded: []Migration{b},
			applied:  []AppliedMigration{applied(a, "aaa"), applied(b, "bbb")},
			wantErr:  ErrChecksumMismatch,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := planMigrations(tt.embedded, tt.applied)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("planMigrations: %v", err)
			}
			if diff := cmp.Diff(tt.wantPending, got); diff != "" {
				t.Errorf("pending mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestParseMigrationName pins the file-naming contract, since a misnamed file is
// a migration that silently never runs.
func TestParseMigrationName(t *testing.T) {
	tests := []struct {
		filename    string
		wantVersion int
		wantName    string
		wantErr     bool
	}{
		{filename: "0001_init.sql", wantVersion: 1, wantName: "init"},
		{filename: "0042_add_something.sql", wantVersion: 42, wantName: "add_something"},
		{filename: "init.sql", wantErr: true},
		{filename: "_init.sql", wantErr: true},
		{filename: "0001_.sql", wantErr: true},
		{filename: "abcd_init.sql", wantErr: true},
		{filename: "0000_init.sql", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.filename, func(t *testing.T) {
			version, name, err := parseMigrationName(tt.filename)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseMigrationName = (%d, %q), want an error", version, name)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseMigrationName: %v", err)
			}
			if version != tt.wantVersion || name != tt.wantName {
				t.Errorf("= (%d, %q), want (%d, %q)", version, name, tt.wantVersion, tt.wantName)
			}
		})
	}
}

// TestMigrateBeforeFirstRunsOnlyWhenMigrating is D92's disarm point: the rule
// fires on "about to migrate", not on "a migration committed", and it must NOT
// fire on a boot that has nothing to apply — otherwise every ordinary restart
// would consume the revert marker.
func TestMigrateBeforeFirstRunsOnlyWhenMigrating(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "llamaman.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	var calls, sawPending int
	opts := MigrateOptions{BeforeFirst: func(pending []Migration) error {
		calls++
		sawPending = len(pending)
		return nil
	}}

	if _, err := s.Migrate(ctx, opts); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	if calls != 1 {
		t.Errorf("BeforeFirst calls on the migrating boot = %d, want 1", calls)
	}
	if sawPending == 0 {
		t.Error("BeforeFirst was handed an empty pending list")
	}

	if _, err := s.Migrate(ctx, opts); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if calls != 1 {
		t.Errorf("BeforeFirst calls after a no-op boot = %d, want still 1", calls)
	}
}

// TestMigrateBeforeFirstErrorAbortsWithNothingApplied: if the disarm cannot be
// performed, no schema change is made, so the previous binary is still able to
// open this database.
func TestMigrateBeforeFirstErrorAbortsWithNothingApplied(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "llamaman.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	sentinel := errors.New("cannot unlink update/pending")
	_, err = s.Migrate(ctx, MigrateOptions{
		BeforeFirst: func([]Migration) error { return sentinel },
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Migrate = %v, want %v", err, sentinel)
	}

	v, err := s.SchemaVersion(ctx, s.RO)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if v != 0 {
		t.Errorf("schema version after an aborted migration = %d, want 0", v)
	}
}

// TestHeartbeatFiresWhileWorking covers the EXTEND_TIMEOUT_USEC= hook §11.1
// step 4 requires around migrations and around PRAGMA integrity_check: a
// legitimately slow start must extend TimeoutStartSec= rather than trip it.
func TestHeartbeatFiresWhileWorking(t *testing.T) {
	var beats atomic.Int64
	stop := heartbeat(func() { beats.Add(1) }, time.Millisecond)
	deadline := time.Now().Add(2 * time.Second)
	for beats.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	stop()

	if got := beats.Load(); got < 3 {
		t.Errorf("heartbeat fired %d times in 2s at a 1ms interval, want at least 3", got)
	}

	// stop() must be synchronous: nothing may call the notifier after it returns,
	// or a boot could send EXTEND_TIMEOUT_USEC= after READY=1.
	after := beats.Load()
	time.Sleep(20 * time.Millisecond)
	if beats.Load() != after {
		t.Error("heartbeat kept firing after stop returned")
	}
}

// TestHeartbeatWithoutNotifierIsNoOp keeps the callers branch-free.
func TestHeartbeatWithoutNotifierIsNoOp(t *testing.T) {
	stop := heartbeat(nil, time.Millisecond)
	stop()
}

// TestIntegrityCheckPasses covers the happy path with the hook wired, which is
// the only part of §11.1 step 3 that lives in this package: what to DO about a
// corrupt file (move it aside, restore db-backups/, else start fresh) is the
// composition root's decision, not the store's. A healthy database finishes far
// too fast to observe a beat, so the interval is here to prove the hook does not
// interfere rather than to count anything.
func TestIntegrityCheckPasses(t *testing.T) {
	s := newTestStore(t)
	var beats atomic.Int64

	err := s.IntegrityCheck(context.Background(), CheckOptions{
		Heartbeat:      func() { beats.Add(1) },
		HeartbeatEvery: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("IntegrityCheck: %v", err)
	}
}

// TestIntegrityErrorMessage keeps the F12 notification's raw material intact:
// the lines SQLite reported are what the user is shown, so they are carried
// rather than summarized.
func TestIntegrityErrorMessage(t *testing.T) {
	err := &IntegrityError{Lines: []string{"row 3 missing from index idx_jobs_ready", "wrong # of entries"}}
	got := err.Error()
	for _, want := range []string{"idx_jobs_ready", "wrong # of entries"} {
		if !strings.Contains(got, want) {
			t.Errorf("error %q does not carry %q", got, want)
		}
	}
}
