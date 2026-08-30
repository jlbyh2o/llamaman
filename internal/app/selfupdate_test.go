package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jlbyh2o/llamaman/internal/buildinfo"
	"github.com/jlbyh2o/llamaman/internal/selfupdate"
)

// The two places section 11.1 puts the self-update protocol in the boot
// sequence, and the retention rule D14 protects (DESIGN sections 11.1, 12.1
// and 15).
//
// Both orderings are section 19's fourth preservation property in code, and
// neither can be asserted anywhere but here, because both are facts about the
// COMPOSITION ROOT rather than about internal/selfupdate:
//
//	step 4  the D92 disarm, through MigrateOptions.BeforeFirst — before the
//	        first migration is ATTEMPTED.
//	step 11 the confirmation gate, before READY=1.

// TestTheGateRunsBeforeReady is section 15's "a probe that watches
// $NOTIFY_SOCKET asserts `update/pending` is already gone at the instant READY=1
// is observed".
//
// The property it protects is the one section 12.2 leans on: **a daemon that
// ever signals readiness has already resolved the marker**, so the judge cannot
// be armed against a version that demonstrably booted. Moving the gate back
// after READY=1 would re-open exactly the path D92 exists to close.
func TestTheGateRunsBeforeReady(t *testing.T) {
	dir := t.TempDir()
	seedLoopback(t, dir)

	// A marker naming a version this binary is NOT, with no actor active: the
	// gate must take branch 3, close it out and unlink the marker — all before
	// READY=1 goes out.
	l := selfupdate.Layout{StateDir: dir}
	if err := l.EnsureUpdateDir(); err != nil {
		t.Fatalf("EnsureUpdateDir: %v", err)
	}
	if err := selfupdate.WriteMarker(l.PendingPath(), selfupdate.Marker{
		Format:        selfupdate.MarkerFormat,
		SelfUpdateID:  "01J8ZQ7X0000000000000GATE1",
		FromVersion:   "v0.0.1",
		TargetVersion: "v99.99.99",
		BinaryPath:    "/usr/local/bin/llamaman",
		StagedAt:      1788012345678,
	}); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}

	// markerAtReady records whether the marker was still on disk at the instant
	// READY=1 was observed. It is captured on the notifier's own goroutine, which
	// is the daemon's, so there is no race to lose.
	probe := &readyProbe{markerPath: l.PendingPath(), readySent: make(chan struct{})}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			Logger:           quiet(),
			Notifier:         probe,
			StateDirOverride: dir,
			Getenv:           func(string) string { return "" },
			ReadyHook:        func(addr string) { ready <- addr },
		})
	}()

	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("Run returned before it was listening: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the daemon never became ready")
	}
	// The listener is up before READY=1 is sent, so wait for the signal itself.
	select {
	case <-probe.readySent:
	case <-time.After(30 * time.Second):
		t.Fatal("READY=1 was never sent")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("the daemon did not shut down")
	}

	if probe.markerAtReady {
		t.Error("update/pending was still on disk at the instant READY=1 was sent: " +
			"the confirmation gate is running after readiness, which arms the judge " +
			"against a version that demonstrably booted (DESIGN section 11.1 step 11)")
	}
	if _, err := os.Stat(l.PendingPath()); err == nil {
		t.Error("the boot gate did not resolve the marker")
	}
}

// readyProbe is a Notifier that records the on-disk state at the exact instant
// READY=1 is sent.
type readyProbe struct {
	markerPath    string
	markerAtReady bool
	readySent     chan struct{}
}

func (p *readyProbe) Ready() error {
	_, err := os.Stat(p.markerPath)
	p.markerAtReady = err == nil
	close(p.readySent)
	return nil
}

func (p *readyProbe) Status(string) error               { return nil }
func (p *readyProbe) ExtendTimeout(time.Duration) error { return nil }
func (p *readyProbe) Watchdog() error                   { return nil }
func (p *readyProbe) Stopping() error                   { return nil }

// TestDisarmIsWiredToTheMigrationRunner is D92's other half: the hook the
// migration runner calls BEFORE the first migration is attempted is the gate's
// disarm, and it is wired at construction rather than later.
//
// It is asserted through the daemon's own method, because the failure this
// guards against is a wiring one: a boot that migrates with the revert still
// armed is a host that can end up with no daemon at all, and nothing about
// internal/selfupdate can notice that on its own.
func TestDisarmIsWiredToTheMigrationRunner(t *testing.T) {
	dir := t.TempDir()

	l := selfupdate.Layout{StateDir: dir}
	if err := l.EnsureUpdateDir(); err != nil {
		t.Fatalf("EnsureUpdateDir: %v", err)
	}
	if err := selfupdate.WriteMarker(l.PendingPath(), selfupdate.Marker{
		Format:        selfupdate.MarkerFormat,
		SelfUpdateID:  "01J8ZQ7X0000000000000DIS01",
		FromVersion:   "v0.0.1",
		TargetVersion: buildinfo.Version,
		BinaryPath:    "/usr/local/bin/llamaman",
		StagedAt:      1788012345678,
	}); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}

	d := &daemon{log: quiet(), stateDir: dir, swap: make(chan struct{}, 1)}
	d.updateGate = selfupdate.NewGate(selfupdate.GateConfig{
		Layout: l, Version: buildinfo.Version, Log: quiet(),
	})

	if err := d.disarmRevert(nil); err != nil {
		t.Fatalf("disarmRevert: %v", err)
	}
	if _, err := os.Stat(l.PendingPath()); err == nil {
		t.Fatal("the marker survived the disarm: a migration would run with the judge " +
			"still armed, and it would rename a binary back over a database it can no " +
			"longer open (D92)")
	}
}

// TestDisarmRefusesWithNoGate is the guard on the ordering itself: migrating
// with nothing to disarm the revert is the one ordering this design cannot
// tolerate, so it is a refusal rather than a silent skip.
func TestDisarmRefusesWithNoGate(t *testing.T) {
	d := &daemon{log: quiet(), stateDir: t.TempDir()}
	if err := d.disarmRevert(nil); err == nil {
		t.Fatal("a migration was allowed to proceed with no gate to disarm the revert")
	}
}

// TestSnapshotRetentionKeepsTheNewest is D14's arithmetic, driven with section
// 15's fixture: "eight updates in a week asserting the snapshot labeled with
// `<prefix>/llamaman.prev`'s version survives, which is the case the earlier
// 'newest for the installed version' predicate protected in name only".
//
// The predicate is "the newest", not "the newest for the version currently
// installed", and the reason is arithmetic: a snapshot is taken only immediately
// BEFORE an update and is labeled with the version being REPLACED, so the newest
// one is by construction the database as the version now at
// `<prefix>/llamaman.prev` left it — exactly the schema section 12.4's
// procedure restores. A snapshot labeled with the INSTALLED version either does
// not exist yet or carries a schema the running binary can already open.
func TestSnapshotRetentionKeepsTheNewest(t *testing.T) {
	dir := t.TempDir()
	backups := filepath.Join(dir, DBBackupsDirName)
	if err := os.MkdirAll(backups, 0o750); err != nil {
		t.Fatalf("create db-backups: %v", err)
	}

	// Eight updates in a week: v1.0.0 → v1.0.1 → … → v1.0.8. Each snapshot is
	// labeled with the version it REPLACED, so the newest names v1.0.7 — the
	// version that is now at <prefix>/llamaman.prev.
	base := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	var newest string
	for i := 0; i < 8; i++ {
		replaced := "v1.0." + itoaPlain(i)
		at := base.Add(time.Duration(i) * 20 * time.Hour)
		name := selfupdate.SnapshotName(replaced, at.Unix())
		path := filepath.Join(backups, name)
		if err := os.WriteFile(path, []byte("snapshot of "+replaced), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatalf("chtimes %s: %v", name, err)
		}
		newest = name
	}

	w := &maintenanceWorker{stateDir: dir, log: quiet()}
	removed := w.sweepDBBackups()

	if removed != 1 {
		t.Errorf("the sweep removed %d snapshots, want 1 (eight taken, %d kept)",
			removed, DBBackupsKept)
	}
	entries, err := os.ReadDir(backups)
	if err != nil {
		t.Fatalf("read db-backups: %v", err)
	}
	if len(entries) != DBBackupsKept {
		t.Errorf("%d snapshots survived, want %d", len(entries), DBBackupsKept)
	}

	kept := map[string]bool{}
	for _, e := range entries {
		kept[e.Name()] = true
	}
	if !kept[newest] {
		t.Errorf("the NEWEST snapshot (%s) was deleted — it is the database the version "+
			"now at <prefix>/llamaman.prev left behind, and the one section 12.4's "+
			"procedure restores (D14)", newest)
	}
	// The oldest goes first, which is what makes the rule "oldest deleted first"
	// rather than an arbitrary seven.
	if kept[selfupdate.SnapshotName("v1.0.0", base.Unix())] {
		t.Error("the oldest snapshot survived; the rule deletes oldest first")
	}
}

// TestSnapshotRetentionNeverDeletesTheOnlyOne is the "whatever the count is
// tuned to" half of D14: even with a keep count of zero the newest snapshot
// survives, because "the newest snapshot is the database `llamaman restore-db`
// restores" is a promise that must not depend on a constant nobody re-reads.
func TestSnapshotRetentionNeverDeletesTheOnlyOne(t *testing.T) {
	dir := t.TempDir()
	backups := filepath.Join(dir, DBBackupsDirName)
	if err := os.MkdirAll(backups, 0o750); err != nil {
		t.Fatalf("create db-backups: %v", err)
	}
	only := filepath.Join(backups, selfupdate.SnapshotName("v1.1.0", 1788012345))
	if err := os.WriteFile(only, []byte("the only snapshot"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	w := &maintenanceWorker{stateDir: dir, log: quiet()}
	if removed := w.sweepDBBackups(); removed != 0 {
		t.Errorf("the sweep removed %d of one snapshot", removed)
	}
	if _, err := os.Stat(only); err != nil {
		t.Errorf("the only snapshot was deleted: %v", err)
	}
}
