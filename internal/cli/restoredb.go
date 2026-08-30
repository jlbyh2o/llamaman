package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jlbyh2o/llamaman/internal/app"
	"github.com/jlbyh2o/llamaman/internal/selfupdate"
	"github.com/jlbyh2o/llamaman/internal/store"
	"golang.org/x/sys/unix"
)

// `llamaman restore-db <snapshot> [--yes] [--json]` — DESIGN section 12.4.
//
// **The only database restore in this design, it never runs by itself, and on
// its own it does not complete a downgrade.** It is step 3 of a five-command
// procedure, and run without steps 1 and 2 it is a destructive no-op that the
// newer binary migrates straight back (D94):
//
//	sudo systemctl stop llamaman.service
//	install.sh --version <older> --no-start
//	sudo llamaman restore-db <snapshot>          <- this command
//	sudo systemctl reset-failed llamaman.service
//	sudo systemctl start llamaman.service
//
// Run at the moment the F24 card appears it passes its own precondition
// trivially — the snapshot's schema is not newer than the NEWER binary now
// running — restores the old database, and is immediately undone: the newer
// binary starts, section 11.1 step 4 migrates the restored database forward, and
// the only lasting effect is that every instance, token, benchmark run,
// download, event and notification created since the snapshot is gone. That is
// why the warning line below exists, and why it is a WARNING rather than a
// refusal: restoring an older snapshot on the same version is a legitimate
// data-recovery operation and stays available.
//
// It is one of the two commands deliberately outside section 11.3's
// "create nothing under the state directory" rule, because its whole purpose is
// to WRITE the database with the daemon down. Run as root, every file it creates
// is chowned to the database's uid/gid and chmodded 0600 before it exits: a
// root-owned database, or a root-owned sidecar beside one, is a database the
// service identity can never write again.

// RestoreDB is that command.
func RestoreDB(env Env, args []string) error {
	fs := flag.NewFlagSet("restore-db", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	yes := fs.Bool("yes", false,
		"supply the confirmation non-interactively, for the one scripted case the F24 card documents")
	asJSON := fs.Bool("json", false, "emit the report as JSON")
	fs.Usage = func() {
		fmt.Fprintf(env.Stderr, "Usage: llamaman restore-db <snapshot> [--yes] [--json]\n\n")
		fmt.Fprintf(env.Stderr,
			"Restores a db-backups/ snapshot over llamaman.db, with the daemon stopped.\n"+
				"On its own this does NOT complete a downgrade — see DESIGN section 12.4.\n\n")
		fs.PrintDefaults()
	}
	// The flags are recognized WHEREVER they appear, and the positional is
	// whatever is left. Go's flag package stops parsing at the first non-flag
	// argument, and `restore-db <snapshot> --yes` is exactly what an operator
	// types — reading it as two positionals would refuse the one invocation the
	// F24 card documents. Neither flag takes a value, so splitting on the leading
	// dash is unambiguous.
	var flagArgs, positional []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			flagArgs = append(flagArgs, a)
			continue
		}
		positional = append(positional, a)
	}
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	if len(positional) != 1 {
		fs.Usage()
		return NewExitError(2, "restore-db takes exactly one argument: the snapshot to restore")
	}

	r := restorer{env: env, snapshot: positional[0], yes: *yes, json: *asJSON}
	return r.run(context.Background())
}

type restorer struct {
	env      Env
	snapshot string
	yes      bool
	json     bool
}

// report is what the command prints before doing anything — and, with --json,
// the whole of its output.
type report struct {
	Snapshot dbFacts  `json:"snapshot"`
	Current  dbFacts  `json:"current"`
	Loss     []loss   `json:"loss"`
	Warning  string   `json:"warning,omitempty"`
	Actions  []string `json:"actions,omitempty"`
}

// dbFacts is the five facts section 12.4 requires for BOTH databases.
type dbFacts struct {
	Path          string `json:"path"`
	SizeBytes     int64  `json:"size_bytes"`
	ModifiedAt    string `json:"modified_at"`
	SchemaVersion int    `json:"schema_version"`
	DaemonVersion string `json:"daemon_version"`
}

// loss is one line of the loss summary.
type loss struct {
	Table string `json:"table"`
	Rows  int    `json:"rows"`
}

func (r restorer) run(ctx context.Context) error {
	paths := resolvePaths(r.env)
	if !paths.Exists {
		return NewExitError(1, "no database at %s — there is nothing to restore over", paths.DBPath)
	}

	// --- privilege: root or the database's owner, the euid checked against
	// stat, exactly like reset-password (§11.3).
	uid, gid, err := dbOwner(paths.DBPath)
	if err != nil {
		return err
	}
	owner := fileOwner{uid: uid, gid: gid}
	if os.Geteuid() != 0 && os.Geteuid() != uid {
		return NewExitError(2,
			"restore-db must run as root or as %s, the owner of %s",
			identityName(uid), paths.DBPath)
	}

	// --- precondition: `llamaman.lock` is free. The daemon must be down:
	// overwriting a live WAL database out from under a running process corrupts
	// it twice over.
	//
	// The question is asked of the LOCK rather than of the pid file, because the
	// lock is the fact and the pid file is only its label: a stale pid file is
	// ordinary after a crash, and refusing on it would refuse a restore the
	// operator is entitled to. The pid is read afterwards, for the message.
	if pid, held := lockHeld(paths); held {
		return NewExitError(2,
			"the daemon is running as pid %d and holds %s\n"+
				"stop it first:  sudo systemctl stop %s",
			pid, filepath.Join(paths.StateDir, app.LockFileName), selfupdate.DaemonUnit)
	}

	// --- precondition: the snapshot is under db-backups/.
	snapshot, err := r.resolveSnapshot(paths)
	if err != nil {
		return err
	}

	// --- precondition: it opens, passes integrity_check, and its schema is not
	// newer than this binary understands.
	snapFacts, err := readDBFacts(ctx, snapshot, true)
	if err != nil {
		return err
	}
	highest, err := highestMigrationVersion()
	if err != nil {
		return err
	}
	if snapFacts.SchemaVersion > highest {
		return NewExitError(2,
			"%s was written at schema version %d and this binary understands %d — "+
				"restoring a schema this binary still cannot open would only move the failure",
			snapshot, snapFacts.SchemaVersion, highest)
	}

	currentFacts, err := readDBFacts(ctx, paths.DBPath, false)
	if err != nil {
		return err
	}

	rep := report{Snapshot: snapFacts, Current: currentFacts}
	rep.Loss, err = computeLoss(ctx, paths.DBPath, snapshot)
	if err != nil {
		return err
	}

	// --- the warning, printed whenever the binary that will start next would
	// migrate the restore straight back forward. That is the exact shape of
	// D94's destructive no-op, and printing it is what turns it into an informed
	// choice rather than a silent one.
	if migratesForward(highest, snapFacts.SchemaVersion) {
		rep.Warning = fmt.Sprintf(
			"%s is %s; it will migrate this database forward on its next start. "+
				"If you meant to downgrade, install the older release first — see DESIGN section 12.4.",
			installedBinaryPath(), installedBinaryVersion())
		rep.Actions = selfupdate.DowngradeProcedure(
			selfupdate.Layout{StateDir: paths.StateDir}, "<older tag>", snapshot)
	}

	r.printReport(rep)

	// --- the confirmation: the operator types the snapshot's file name.
	if err := r.confirm(filepath.Base(snapshot)); err != nil {
		return err
	}

	superseded, err := r.restore(ctx, paths.DBPath, snapshot, owner)
	if err != nil {
		return err
	}

	fmt.Fprintf(r.env.Stdout, "\nRestored %s over %s.\n", snapshot, paths.DBPath)
	fmt.Fprintf(r.env.Stdout,
		"The database this replaced is kept at %s; re-run this command against it to undo.\n"+
			"It is pruned by the nightly maintenance pass after 30 days.\n", superseded)
	return nil
}

// lockHeld reports whether a daemon holds `<state_dir>/llamaman.lock`, and the
// pid it recorded there.
//
// It asks the kernel by trying the same non-blocking flock the boot sequence
// takes, which is the only answer that cannot be stale. Two details matter:
//
//   - The file is opened WITHOUT O_CREATE. This command may run as root, and a
//     root-created file in the state directory is exactly what section 11.3
//     forbids — an absent lock file means the daemon has never run here, which is
//     a free lock.
//   - A lock this process acquires is released immediately. Holding it would
//     make the restore itself the thing that blocks a daemon start, and the
//     restore is deliberately an offline operation rather than a lease.
func lockHeld(p paths) (int, bool) {
	path := filepath.Join(p.StateDir, app.LockFileName)
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return 0, false
	}
	defer f.Close()

	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		pid := 0
		if b, readErr := os.ReadFile(path); readErr == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		return pid, true
	}
	_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
	return 0, false
}

// resolveSnapshot enforces "the snapshot exists under `db-backups/`".
//
// The check is on the RESOLVED path, so a symlink or a `..` cannot walk out of
// the directory: this command runs as root and copies whatever it is pointed at
// over the database.
func (r restorer) resolveSnapshot(p paths) (string, error) {
	abs, err := filepath.Abs(r.snapshot)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", NewExitError(2, "%s: %v", r.snapshot, err)
	}
	backups := filepath.Join(p.StateDir, selfupdate.DBBackupsDirName)
	if realBackups, err := filepath.EvalSymlinks(backups); err == nil {
		backups = realBackups
	}
	if filepath.Dir(resolved) != backups {
		return "", NewExitError(2,
			"%s is not under %s — restore-db only restores snapshots this daemon wrote",
			resolved, backups)
	}
	return resolved, nil
}

// readDBFacts opens a database read-only and reads the five facts section 12.4
// prints for each of them. `check` additionally runs PRAGMA integrity_check,
// which is a precondition for the snapshot and not for the live database — the
// live one is about to be replaced.
func readDBFacts(ctx context.Context, path string, check bool) (dbFacts, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return dbFacts{}, NewExitError(2, "%s: %v", path, err)
	}
	out := dbFacts{
		Path:       path,
		SizeBytes:  fi.Size(),
		ModifiedAt: fi.ModTime().UTC().Format(time.RFC3339),
	}

	s, err := store.OpenReadOnly(ctx, path)
	if err != nil {
		return dbFacts{}, NewExitError(2, "%s: %v", path, err)
	}
	defer s.Close()

	err = s.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		if check {
			if err := s.IntegrityCheckRead(ctx, tx, 0); err != nil {
				return err
			}
		}
		v, err := s.SchemaVersion(ctx, tx)
		if err != nil {
			return err
		}
		out.SchemaVersion = v

		ri, err := s.RuntimeInfo(ctx, tx)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		out.DaemonVersion = ri.DaemonVersion
		return nil
	})
	if err != nil {
		return dbFacts{}, NewExitError(2, "%s: %v", path, err)
	}
	return out, nil
}

// computeLoss is the set difference section 12.4 asks for: rows present in the
// current database and ABSENT from the snapshot, one line per table.
func computeLoss(ctx context.Context, currentPath, snapshotPath string) ([]loss, error) {
	current, err := store.OpenReadOnly(ctx, currentPath)
	if err != nil {
		return nil, err
	}
	defer current.Close()
	snap, err := store.OpenReadOnly(ctx, snapshotPath)
	if err != nil {
		return nil, err
	}
	defer snap.Close()

	var out []loss
	for _, table := range store.LossTables() {
		var have, kept []string
		if err := current.Read(ctx, func(ctx context.Context, tx store.Tx) error {
			var err error
			have, err = current.RowIDs(ctx, tx, table)
			return err
		}); err != nil {
			return nil, err
		}
		if err := snap.Read(ctx, func(ctx context.Context, tx store.Tx) error {
			var err error
			kept, err = snap.RowIDs(ctx, tx, table)
			return err
		}); err != nil {
			return nil, err
		}

		inSnapshot := make(map[string]struct{}, len(kept))
		for _, id := range kept {
			inSnapshot[id] = struct{}{}
		}
		n := 0
		for _, id := range have {
			if _, ok := inSnapshot[id]; !ok {
				n++
			}
		}
		out = append(out, loss{Table: table, Rows: n})
	}
	return out, nil
}

func (r restorer) printReport(rep report) {
	if r.json {
		enc := json.NewEncoder(r.env.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rep)
		return
	}

	w := r.env.Stdout
	fmt.Fprintf(w, "Snapshot   %s\n", rep.Snapshot.Path)
	fmt.Fprintf(w, "           %d bytes, modified %s, schema v%d, written by %s\n",
		rep.Snapshot.SizeBytes, rep.Snapshot.ModifiedAt, rep.Snapshot.SchemaVersion,
		orUnknown(rep.Snapshot.DaemonVersion))
	fmt.Fprintf(w, "Current    %s\n", rep.Current.Path)
	fmt.Fprintf(w, "           %d bytes, modified %s, schema v%d, written by %s\n",
		rep.Current.SizeBytes, rep.Current.ModifiedAt, rep.Current.SchemaVersion,
		orUnknown(rep.Current.DaemonVersion))

	fmt.Fprintf(w, "\nThis will discard, permanently:\n")
	for _, l := range rep.Loss {
		fmt.Fprintf(w, "  %-14s %d\n", l.Table, l.Rows)
	}
	if rep.Warning != "" {
		fmt.Fprintf(w, "\nWARNING: %s\n", rep.Warning)
		if len(rep.Actions) > 0 {
			fmt.Fprintf(w, "\nTo complete a downgrade, all five of these, in order:\n")
			for _, a := range rep.Actions {
				fmt.Fprintf(w, "  %s\n", a)
			}
		}
	}
}

func orUnknown(v string) string {
	if v == "" {
		return "an unrecorded version"
	}
	return v
}

// confirm requires the operator to type the snapshot's file name. `--yes`
// supplies it for the one scripted case the F24 card documents; a non-TTY
// without `--yes` refuses rather than proceeding on a pipe.
func (r restorer) confirm(name string) error {
	if r.yes {
		return nil
	}
	if !r.env.Interactive {
		return NewExitError(2,
			"restore-db needs a confirmation and stdin is not a terminal; "+
				"pass --yes to supply it non-interactively")
	}
	fmt.Fprintf(r.env.Stdout, "\nType the snapshot's file name to proceed (%s): ", name)

	line, err := bufio.NewReader(r.env.stdin()).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if strings.TrimSpace(line) != name {
		return NewExitError(2, "that is not %s — nothing was changed", name)
	}
	return nil
}

// restore is section 12.4's five steps, in the order that makes every crash
// window land on a database that opens:
//
//	(1) checkpoint the live database TRUNCATE, so the main file is complete and
//	    its sidecars are redundant;
//	(2) VACUUM INTO llamaman.db.superseded-<ts> beside it, so the discarded
//	    database is recoverable by re-running this command against IT;
//	(3) unlink llamaman.db-wal and llamaman.db-shm;
//	(4) copy the snapshot to llamaman.db.restore IN THE SAME DIRECTORY, chown it
//	    to the current database's uid/gid, chmod 0600, fsync it and the directory;
//	(5) rename it over llamaman.db.
//
// Steps 4 and 5 are the D14 ownership rule: a root-created 0600 file the service
// identity cannot write is a database that never opens again.
func (r restorer) restore(ctx context.Context, dbPath, snapshot string, owner fileOwner) (string, error) {
	dir := filepath.Dir(dbPath)
	superseded := filepath.Join(dir,
		fmt.Sprintf("%s%d", app.SupersededDBPrefix, r.env.now().Unix()))

	// (1) and (2) need a write connection to the live database.
	s, err := store.Open(ctx, dbPath)
	if err != nil {
		return "", err
	}
	if err := s.CheckpointTruncate(ctx); err != nil {
		s.Close()
		return "", err
	}
	if err := os.Remove(superseded); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.Close()
		return "", err
	}
	if err := s.VacuumInto(ctx, superseded); err != nil {
		s.Close()
		return "", err
	}
	if err := s.Close(); err != nil {
		return "", err
	}
	if err := chownAndChmod(superseded, owner); err != nil {
		return "", err
	}

	// (3) the sidecars are redundant after the checkpoint, and leaving them
	// beside a restored main file would be a WAL from a different database.
	for _, sidecar := range []string{dbPath + "-wal", dbPath + "-shm"} {
		if err := os.Remove(sidecar); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("remove %s: %w", sidecar, err)
		}
	}

	// (4) copy into the same directory, so (5) is a rename that cannot EXDEV.
	staging := dbPath + ".restore"
	if err := copyFile(snapshot, staging); err != nil {
		return "", err
	}
	if err := chownAndChmod(staging, owner); err != nil {
		return "", err
	}
	if err := fsyncPath(dir); err != nil {
		return "", err
	}

	// (5)
	if err := os.Rename(staging, dbPath); err != nil {
		return "", fmt.Errorf("rename %s over %s: %w", staging, dbPath, err)
	}
	if err := fsyncPath(dir); err != nil {
		return "", err
	}
	return superseded, nil
}

// fileOwner is the uid/gid every file this command creates is given. It is read
// from the database itself through resetpassword.go's dbOwner, which is the same
// stat both write-side commands check their euid against (§11.3).
type fileOwner struct {
	uid, gid int
}

func chownAndChmod(path string, owner fileOwner) error {
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	if os.Geteuid() != 0 {
		return nil
	}
	if err := os.Chown(path, owner.uid, owner.gid); err != nil {
		return fmt.Errorf("chown %s to %d:%d: %w", path, owner.uid, owner.gid, err)
	}
	return nil
}

func copyFile(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close()

	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", dest, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return fmt.Errorf("copy %s to %s: %w", src, dest, err)
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return fmt.Errorf("fsync %s: %w", dest, err)
	}
	return out.Close()
}

func fsyncPath(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s to fsync it: %w", path, err)
	}
	defer f.Close()
	// A filesystem that refuses fsync on a directory is not a reason to fail a
	// restore: the rename itself is still atomic.
	_ = f.Sync()
	return nil
}

// migratesForward is the predicate behind section 12.4's warning line, and it is
// its own function because the sentence it decides is the difference between a
// data recovery and D94's destructive no-op.
//
// It is true exactly when the binary that will start next would migrate the
// restored database straight back forward — that is, when this binary's highest
// embedded migration EXCEEDS the snapshot's `MAX(schema_migrations.version)`.
// Equality is the case the five-command procedure produces at its third step,
// where the binary running this command IS the older one, and there the warning
// is correctly absent.
//
// It is a WARNING and not a refusal: restoring an older snapshot on the same
// version is a legitimate data-recovery operation and stays available.
func migratesForward(binarySchema, snapshotSchema int) bool {
	return binarySchema > snapshotSchema
}

// highestMigrationVersion is what this binary understands — the right-hand side
// of the schema precondition and of the "will migrate this forward" warning.
func highestMigrationVersion() (int, error) {
	ms, err := store.Migrations()
	if err != nil {
		return 0, err
	}
	highest := 0
	for _, m := range ms {
		if m.Version > highest {
			highest = m.Version
		}
	}
	return highest, nil
}

// installedBinaryPath and installedBinaryVersion name `<prefix>/llamaman` for
// the warning line. Section 12.4 is explicit that this command "never MODIFIES
// `<prefix>/llamaman`, `<prefix>/llamaman.prev` or any unit — it reads the
// installed binary's version, and nothing else about it, only to print that
// warning".
func installedBinaryPath() string {
	prefix, err := selfupdate.ResolvePrefix()
	if err != nil {
		return "the installed llamaman"
	}
	return filepath.Join(prefix, selfupdate.BinaryName)
}

func installedBinaryVersion() string {
	path := installedBinaryPath()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "version").Output()
	if err != nil {
		return "a version it would not report"
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return "a version it would not report"
	}
	return fields[0]
}
