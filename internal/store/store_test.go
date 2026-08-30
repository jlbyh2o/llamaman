package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestDSNCarriesPragmas pins the section-2 pragma set onto every connection
// string, in order, and checks that a pool's extra pragmas and driver
// parameters are appended rather than replacing them.
func TestDSNCarriesPragmas(t *testing.T) {
	got := dsn("/var/lib/llamaman/llamaman.db", nil, nil)
	want := "file:/var/lib/llamaman/llamaman.db" +
		"?_pragma=journal_mode%28WAL%29" +
		"&_pragma=foreign_keys%28ON%29" +
		"&_pragma=busy_timeout%285000%29" +
		"&_pragma=synchronous%28NORMAL%29" +
		"&_pragma=temp_store%28MEMORY%29" +
		"&_pragma=auto_vacuum%28INCREMENTAL%29"
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("dsn mismatch (-want +got):\n%s", diff)
	}

	ro := dsn("/db", []string{"query_only(1)"}, nil)
	if want := "&_pragma=query_only%281%29"; ro[len(ro)-len(want):] != want {
		t.Errorf("read pool DSN does not end with query_only: %s", ro)
	}

	rw := dsn("/db", nil, []string{"_txlock=immediate"})
	if want := "&_txlock=immediate"; rw[len(rw)-len(want):] != want {
		t.Errorf("write pool DSN does not end with _txlock=immediate: %s", rw)
	}
}

// TestOpenSmoke proves the two-pool arrangement of DESIGN section 2 actually
// holds against a real file: the write pool writes, the read pool reads the
// same data, and the read pool refuses a write.
func TestOpenSmoke(t *testing.T) {
	path := filepath.Join(t.TempDir(), "llamaman.db")

	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	if _, err := s.RW.Exec("CREATE TABLE t (a TEXT)"); err != nil {
		t.Fatalf("write pool exec: %v", err)
	}

	var n int
	if err := s.RO.QueryRow("SELECT count(*) FROM t").Scan(&n); err != nil {
		t.Fatalf("read pool query: %v", err)
	}
	if n != 0 {
		t.Errorf("count = %d, want 0", n)
	}

	if _, err := s.RO.Exec("INSERT INTO t VALUES ('x')"); err == nil {
		t.Error("read pool accepted a write; query_only is not in effect")
	}
}

// TestPragmasOnEveryConnection asserts every pragma DESIGN section 2.1 names is
// actually in effect, on BOTH pools. The DSN test above pins what is asked for;
// this one pins what the database answers, which is the only version that
// matters — auto_vacuum in particular is a header property that a pragma can
// only set while the file is still empty.
func TestPragmasOnEveryConnection(t *testing.T) {
	s := newTestStore(t)

	tests := []struct {
		pragma string
		want   string
		why    string
	}{
		{"journal_mode", "wal", "WAL permits one writer alongside readers (§2)"},
		{"foreign_keys", "1", "every REFERENCES in the schema is enforced, not decorative"},
		{"busy_timeout", "5000", "instance-exec opens its own connection from a separate process"},
		{"synchronous", "1", "NORMAL: durable enough under WAL, without an fsync per commit"},
		{"temp_store", "2", "MEMORY: no temp files in the state directory"},
		{"auto_vacuum", "2", "INCREMENTAL: the file can be reclaimed without a full VACUUM"},
	}

	pools := map[string]*sql.DB{"rw": s.RW, "ro": s.RO}

	for _, tt := range tests {
		t.Run(tt.pragma, func(t *testing.T) {
			for name, db := range pools {
				var got string
				if err := db.QueryRow("PRAGMA " + tt.pragma).Scan(&got); err != nil {
					t.Fatalf("%s pool: PRAGMA %s: %v", name, tt.pragma, err)
				}
				if got != tt.want {
					t.Errorf("%s pool: PRAGMA %s = %q, want %q (%s)",
						name, tt.pragma, got, tt.want, tt.why)
				}
			}
		})
	}
}

// TestWriteRollsBackOnError proves Store.Write does not commit a failed unit of
// work — the property every "job row and domain row in one transaction" claim in
// section 2.3a rests on.
func TestWriteRollsBackOnError(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	sentinel := errors.New("boom")

	err := s.Write(ctx, func(ctx context.Context, tx Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO settings (key, value, updated_at, updated_by)
			 VALUES ('ui.theme', '"light"', 1, 'admin')`); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Write error = %v, want %v", err, sentinel)
	}

	var n int
	if err := s.RO.QueryRow(`SELECT count(*) FROM settings`).Scan(&n); err != nil {
		t.Fatalf("count settings: %v", err)
	}
	if n != 0 {
		t.Errorf("settings rows after a rolled-back write = %d, want 0", n)
	}
}

// TestReadRunsInOneSnapshot proves the read helper works against the query_only
// pool: a multi-statement read opens a transaction, sees committed data, and
// rolls back without complaint.
func TestReadRunsInOneSnapshot(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO settings (key, value, updated_at, updated_by)
			 VALUES ('ui.theme', '"dark"', 1, 'admin')`)
		return err
	})

	var settings, migrations int
	err := s.Read(ctx, func(ctx context.Context, tx Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM settings`).Scan(&settings); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&migrations)
	})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if settings != 1 || migrations != 1 {
		t.Errorf("read %d settings and %d migrations, want 1 and 1", settings, migrations)
	}
}

// TestErrNotFoundWrapsNoRows keeps the sentinel usable by callers that test for
// either it or the driver's own error.
func TestErrNotFoundWrapsNoRows(t *testing.T) {
	s := newTestStore(t)
	_, err := s.Setting(context.Background(), s.RO, "never.set")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Setting on a missing key = %v, want ErrNotFound", err)
	}
}
