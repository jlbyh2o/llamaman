package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jlbyh2o/llamaman/internal/app"
	"github.com/jlbyh2o/llamaman/internal/selfupdate"
	"github.com/jlbyh2o/llamaman/internal/store"
	"golang.org/x/sys/unix"
)

// `llamaman restore-db` (DESIGN section 12.4, section 15).
//
// Section 15 asks for "each precondition asserted to refuse — the lock held
// (naming the PID), a snapshot outside `db-backups/`, one failing
// `integrity_check`, one whose schema is newer than the binary — the
// '`<prefix>/llamaman` will migrate this forward' warning asserted to be printed
// when the installed binary's schema is newer than the snapshot's and ABSENT
// when it is not, and the confirmation asserted to be required (a non-TTY
// without `--yes` refuses)".
//
// The property behind all of them is D94's: **this command on its own does not
// complete a downgrade.** Run without steps 1 and 2 of the five it is a
// destructive no-op that the newer binary migrates straight back — so every
// refusal here is a refusal to do something irreversible for a reason the
// operator has not been told.

// restoreFixture is a state directory with a real database and a real snapshot
// under db-backups/.
type restoreFixture struct {
	t        *testing.T
	dir      string
	dbPath   string
	snapshot string
	env      Env
	out      *bytes.Buffer
	errOut   *bytes.Buffer
}

func newRestoreFixture(t *testing.T) *restoreFixture {
	t.Helper()
	dir := t.TempDir()
	ctx := context.Background()

	dbPath := filepath.Join(dir, app.DatabaseFileName)
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open the database: %v", err)
	}
	if _, err := db.Migrate(ctx, store.MigrateOptions{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// The D14 snapshot, taken exactly the way section 12.1 step 4 takes it.
	backups := filepath.Join(dir, selfupdate.DBBackupsDirName)
	if err := os.MkdirAll(backups, 0o750); err != nil {
		t.Fatalf("create db-backups: %v", err)
	}
	snapshot := filepath.Join(backups, selfupdate.SnapshotName("v1.1.0", 1788012345))
	if err := db.VacuumInto(ctx, snapshot); err != nil {
		t.Fatalf("VACUUM INTO: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	return &restoreFixture{
		t: t, dir: dir, dbPath: dbPath, snapshot: snapshot,
		out: out, errOut: errOut,
		env: Env{
			Stdout: out, Stderr: errOut,
			// A non-TTY: the confirmation must be supplied with --yes.
			Interactive: false,
			Getenv: func(k string) string {
				if k == "STATE_DIRECTORY" {
					return dir
				}
				return ""
			},
		},
	}
}

// TestRestoreDBRefusesWhileTheDaemonHoldsTheLock is the first precondition, and
// the one with a command attached: the daemon must be down, because overwriting
// a live WAL database out from under a running process corrupts it twice over.
func TestRestoreDBRefusesWhileTheDaemonHoldsTheLock(t *testing.T) {
	f := newRestoreFixture(t)

	// A real flock on the real lock file, which is what a running daemon holds —
	// the question restore-db asks is asked of the LOCK, because a pid file is
	// only its label and a stale one is ordinary after a crash.
	holdLock(t, filepath.Join(f.dir, app.LockFileName))

	err := RestoreDB(f.env, []string{f.snapshot, "--yes"})
	if err == nil {
		t.Fatal("restore-db proceeded while the daemon held the lock")
	}
	msg := err.Error()
	if !strings.Contains(msg, "systemctl stop "+selfupdate.DaemonUnit) {
		t.Errorf("the refusal does not print the stop command: %s", msg)
	}
	if !strings.Contains(msg, "pid") {
		t.Errorf("the refusal does not name the holding pid: %s", msg)
	}
}

// TestRestoreDBRefusesASnapshotOutsideDBBackups: this command runs as root and
// copies whatever it is pointed at over the database, so the directory is the
// whole of its authority over what it will restore.
func TestRestoreDBRefusesASnapshotOutsideDBBackups(t *testing.T) {
	f := newRestoreFixture(t)

	elsewhere := filepath.Join(t.TempDir(), "somebody-elses.db")
	if err := os.WriteFile(elsewhere, mustReadFile(t, f.snapshot), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	err := RestoreDB(f.env, []string{elsewhere, "--yes"})
	if err == nil {
		t.Fatal("restore-db accepted a snapshot from outside db-backups/")
	}
	if !strings.Contains(err.Error(), selfupdate.DBBackupsDirName) {
		t.Errorf("the refusal does not name db-backups/: %v", err)
	}
}

// TestRestoreDBRefusesACorruptSnapshot: the snapshot must pass
// `PRAGMA integrity_check` before anything is touched.
func TestRestoreDBRefusesACorruptSnapshot(t *testing.T) {
	f := newRestoreFixture(t)

	corrupt := filepath.Join(f.dir, selfupdate.DBBackupsDirName, "llamaman-v1.0.0-1788000000.db")
	if err := os.WriteFile(corrupt, []byte("this is not a database"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := RestoreDB(f.env, []string{corrupt, "--yes"}); err == nil {
		t.Fatal("restore-db accepted a snapshot that is not a database")
	}
	// Nothing was touched: the live database still opens.
	assertDatabaseOpens(t, f.dbPath)
}

// TestRestoreDBRequiresConfirmation: "a non-TTY without --yes refuses". The
// operator types the snapshot's file name to proceed, and `--yes` supplies it
// for the one scripted case the F24 card documents.
func TestRestoreDBRequiresConfirmation(t *testing.T) {
	f := newRestoreFixture(t)

	err := RestoreDB(f.env, []string{f.snapshot})
	if err == nil {
		t.Fatal("restore-db proceeded on a non-TTY with no --yes")
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Errorf("the refusal does not name --yes: %v", err)
	}
	assertDatabaseOpens(t, f.dbPath)
}

// TestRestoreDBPrintsBothDatabasesAndTheLoss is the report section 12.4 requires
// BEFORE anything happens: five facts per database, then the loss, one line per
// table.
func TestRestoreDBPrintsBothDatabasesAndTheLoss(t *testing.T) {
	f := newRestoreFixture(t)

	if err := RestoreDB(f.env, []string{f.snapshot, "--yes"}); err != nil {
		t.Fatalf("RestoreDB: %v", err)
	}
	printed := f.out.String()
	for _, want := range []string{"Snapshot", "Current", "schema v", "discard"} {
		if !strings.Contains(printed, want) {
			t.Errorf("the report does not contain %q:\n%s", want, printed)
		}
	}
	for _, table := range store.LossTables() {
		if !strings.Contains(printed, table) {
			t.Errorf("the loss summary has no line for %q:\n%s", table, printed)
		}
	}
	// The database this replaced is kept, and its path is printed so the operator
	// can keep it — re-running the command against IT is the undo.
	if !strings.Contains(printed, app.SupersededDBPrefix) {
		t.Errorf("the report does not name the superseded database:\n%s", printed)
	}
}

// TestRestoreDBLeavesAWorkingDatabase walks the five steps' outcome: after the
// rename the live database is the snapshot, its sidecars are gone, and it opens.
func TestRestoreDBLeavesAWorkingDatabase(t *testing.T) {
	f := newRestoreFixture(t)

	if err := RestoreDB(f.env, []string{f.snapshot, "--yes"}); err != nil {
		t.Fatalf("RestoreDB: %v", err)
	}
	assertDatabaseOpens(t, f.dbPath)

	for _, sidecar := range []string{f.dbPath + "-wal", f.dbPath + "-shm"} {
		if _, err := os.Stat(sidecar); err == nil {
			t.Errorf("%s survived the restore: it is a WAL from a different database", sidecar)
		}
	}
	if _, err := os.Stat(f.dbPath + ".restore"); err == nil {
		t.Error("the staging file survived the rename")
	}

	// The superseded copy is there, and re-running the command against it is the
	// documented undo — which means it must itself be a restorable snapshot.
	entries, err := os.ReadDir(f.dir)
	if err != nil {
		t.Fatalf("read the state directory: %v", err)
	}
	found := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), app.SupersededDBPrefix) {
			found = true
			assertDatabaseOpens(t, filepath.Join(f.dir, e.Name()))
		}
	}
	if !found {
		t.Error("no llamaman.db.superseded-* was written")
	}
}

// TestRestoreDBIsIdempotent is the crash-safety claim, exercised the way an
// operator would after a kill: re-running the command completes.
func TestRestoreDBIsIdempotent(t *testing.T) {
	f := newRestoreFixture(t)

	for i := 0; i < 2; i++ {
		f.out.Reset()
		if err := RestoreDB(f.env, []string{f.snapshot, "--yes"}); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
		assertDatabaseOpens(t, f.dbPath)
	}
}

// holdLock takes the same non-blocking exclusive flock the boot sequence takes,
// and writes this process's pid into the file the way the daemon does, so the
// refusal has a pid to name.
func holdLock(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatalf("open the lock file: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatalf("flock: %v", err)
	}
	if _, err := f.WriteString(strconv.Itoa(os.Getpid()) + "\n"); err != nil {
		t.Fatalf("write the pid: %v", err)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

// assertDatabaseOpens is the promise every crash window of section 12.4 makes:
// "every window lands on a database that opens".
func assertDatabaseOpens(t *testing.T, path string) {
	t.Helper()
	ctx := context.Background()
	db, err := store.OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatalf("the database at %s does not open: %v", path, err)
	}
	defer db.Close()
	if err := db.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		_, err := db.SchemaVersion(ctx, tx)
		return err
	}); err != nil {
		t.Fatalf("the database at %s does not answer: %v", path, err)
	}
}

// TestTheMigratesForwardWarning is section 15's "the '`<prefix>/llamaman` will
// migrate this forward' warning asserted to be printed when the installed
// binary's schema is newer than the snapshot's and ABSENT when it is not".
//
// It is D94's destructive no-op made visible: run at the moment the F24 card
// appears, `restore-db` passes its own precondition trivially — the snapshot's
// schema is not newer than the NEWER binary now running — restores the old
// database, and is immediately undone when that binary migrates it forward
// again. The only lasting effect would be the data loss. Printing the line is
// what turns that into an informed choice rather than a silent one.
func TestTheMigratesForwardWarning(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name                   string
		binarySchema, snapshot int
		want                   bool
	}{
		{
			name:         "the newer binary is still installed — D94's destructive no-op",
			binarySchema: 15, snapshot: 14, want: true,
		},
		{
			name:         "step 3 of the five commands: the OLDER binary is running it",
			binarySchema: 14, snapshot: 14, want: false,
		},
		{
			name:         "a snapshot newer than this binary is refused before the warning matters",
			binarySchema: 14, snapshot: 15, want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := migratesForward(tc.binarySchema, tc.snapshot); got != tc.want {
				t.Errorf("migratesForward(%d, %d) = %v, want %v",
					tc.binarySchema, tc.snapshot, got, tc.want)
			}
		})
	}
}

// TestRestoreDBOmitsTheWarningOnAMatchingSchema is the same assertion end to
// end, against the printed report: the fixture's snapshot was taken by THIS
// binary, so nothing would migrate forward and the line must not appear.
func TestRestoreDBOmitsTheWarningOnAMatchingSchema(t *testing.T) {
	f := newRestoreFixture(t)

	if err := RestoreDB(f.env, []string{f.snapshot, "--yes"}); err != nil {
		t.Fatalf("RestoreDB: %v", err)
	}
	printed := f.out.String()
	if strings.Contains(printed, "will migrate this database forward") {
		t.Errorf("the warning was printed for a snapshot at this binary's own schema:\n%s", printed)
	}
	// And with no warning there is no five-command procedure either: nothing
	// prints the restore-db line alone, and nothing prints the procedure when it
	// is not the thing the operator needs.
	if strings.Contains(printed, "To complete a downgrade") {
		t.Errorf("the downgrade procedure was printed for a same-version restore:\n%s", printed)
	}
}
