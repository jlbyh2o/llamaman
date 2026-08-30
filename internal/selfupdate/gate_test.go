package selfupdate

import (
	"context"
	"testing"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// The confirmation gate's three branches and its closing pass (DESIGN section
// 12.3, section 15).
//
// The comparison the whole routine turns on is `pending.target_version` against
// THIS binary's version, plus one question for systemd. Nothing here is measured
// from a clock, and the deferral case below asserts exactly that: a fake clock
// advanced by an hour does not change the answer, which is the regression test
// for measuring liveness with a clock at all.

const (
	thisVersion = "v1.2.0"
	prevVersion = "v1.1.0"
	updateID    = "01J8ZQ7X0000000000000ROW01"
	orphanID    = "01J8ZQ7X0000000000000ORPH1"
)

func marker(id, target string) Marker {
	return Marker{
		Format: MarkerFormat, SelfUpdateID: id, FromVersion: prevVersion,
		TargetVersion: target, BinaryPath: "/usr/local/bin/llamaman",
		StagedAt: 1788012345678,
	}
}

// TestGateBranch1Confirms: `pending.target_version` equals this binary's
// version, so the update took. Row and job `succeeded`, an `events` row, then
// the marker is unlinked and the scratch cleared.
func TestGateBranch1Confirms(t *testing.T) {
	t.Parallel()

	f := newGateFixture(t, thisVersion)
	f.seedUpdate(updateID, model.UpdateSwapping, model.JobInterrupted, thisVersion)
	f.writeMarker(marker(updateID, thisVersion))
	scratch := f.scratch("llamaman_v1.2.0_linux_amd64.tar.gz")

	res, err := f.gate.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Branch != BranchConfirmed {
		t.Fatalf("branch = %q, want %q", res.Branch, BranchConfirmed)
	}
	if got := f.row(updateID).State; got != model.UpdateSucceeded {
		t.Errorf("self_updates.state = %q, want succeeded", got)
	}
	if got := f.job(updateID).State; got != model.JobSucceeded {
		t.Errorf("jobs.state = %q, want succeeded", got)
	}
	if exists(f.l.PendingPath()) {
		t.Error("the marker survived a confirmed update")
	}
	if exists(scratch) {
		t.Error("the scratch survived a confirmed update")
	}
	if codes := f.notifications(); len(codes) != 0 {
		t.Errorf("a confirmed update raised %v; F24 is branch 3's card", codes)
	}
}

// TestGateBranch1IsIdempotent: "commit first and unlink second, so a kill
// between them leaves a terminal row beside a marker the next call resolves as a
// no-op — the branch is idempotent for a row already `succeeded`".
func TestGateBranch1IsIdempotent(t *testing.T) {
	t.Parallel()

	f := newGateFixture(t, thisVersion)
	f.seedUpdate(updateID, model.UpdateSwapping, model.JobInterrupted, thisVersion)
	f.writeMarker(marker(updateID, thisVersion))

	if _, err := f.gate.Resolve(context.Background()); err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	// The kill: the row committed, the unlink did not happen.
	f.writeMarker(marker(updateID, thisVersion))

	res, err := f.gate.Resolve(context.Background())
	if err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if res.Branch != BranchConfirmed {
		t.Fatalf("branch = %q, want %q", res.Branch, BranchConfirmed)
	}
	if res.RowResolved {
		t.Error("the second call moved a row that was already terminal")
	}
	if got := f.row(updateID).State; got != model.UpdateSucceeded {
		t.Errorf("self_updates.state = %q, want succeeded", got)
	}
	if exists(f.l.PendingPath()) {
		t.Error("the second call did not unlink the marker")
	}
}

// TestGateBranch2Defers: the target is not this version and
// `llamaman-selfupdate.service` is active or activating, so an actor is working.
// Row, job and marker are left untouched.
//
// The companion assertion is the one that matters most: **a fake clock advanced
// by an hour does not change the answer.** Liveness is a question for systemd,
// never for a clock (D91) — and `staged_at` is informational, so no deferral in
// this protocol can be aged out.
func TestGateBranch2Defers(t *testing.T) {
	t.Parallel()

	for _, state := range []string{"active", "activating"} {
		t.Run(state, func(t *testing.T) {
			t.Parallel()
			f := newGateFixture(t, thisVersion)
			f.units.state = state
			f.seedUpdate(updateID, model.UpdateSwapping, model.JobInterrupted, "v1.3.0")
			f.writeMarker(marker(updateID, "v1.3.0"))
			scratch := f.scratch("llamaman_v1.3.0_linux_amd64.tar.gz")

			res, err := f.gate.Resolve(context.Background())
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if res.Branch != BranchDeferred {
				t.Fatalf("branch = %q, want %q", res.Branch, BranchDeferred)
			}
			if got := f.row(updateID).State; got != model.UpdateSwapping {
				t.Errorf("the deferral moved the row to %q", got)
			}
			if got := f.job(updateID).State; got != model.JobInterrupted {
				t.Errorf("the deferral moved the job to %q", got)
			}
			if !exists(f.l.PendingPath()) {
				t.Error("the deferral deleted the marker")
			}
			if !exists(scratch) {
				t.Error("the deferral cleared the scratch the working actor is reading")
			}

			// The clock moves an hour. The answer must not.
			f.gate.now = func() time.Time { return f.now.Add(time.Hour) }
			again, err := f.gate.Resolve(context.Background())
			if err != nil {
				t.Fatalf("second Resolve: %v", err)
			}
			if again.Branch != BranchDeferred {
				t.Errorf("an hour later the branch became %q: the deferral is being measured "+
					"with a clock rather than asked of systemd (D91)", again.Branch)
			}
		})
	}
}

// TestGateDeferralIsBoundedBySystemd: "the deferral case is then re-run with the
// oneshot killed by its own TimeoutStartSec=120 to assert the bound is
// systemd's: the very next gate call takes branch 3."
func TestGateDeferralIsBoundedBySystemd(t *testing.T) {
	t.Parallel()

	f := newGateFixture(t, thisVersion)
	f.units.state = "active"
	f.seedUpdate(updateID, model.UpdateSwapping, model.JobInterrupted, "v1.3.0")
	f.writeMarker(marker(updateID, "v1.3.0"))

	if res, err := f.gate.Resolve(context.Background()); err != nil || res.Branch != BranchDeferred {
		t.Fatalf("first Resolve: branch %q, err %v", res.Branch, err)
	}

	// systemd killed the oneshot at its TimeoutStartSec=120; ExecStopPost= ran and
	// the unit is no longer active. `deactivating` is deliberately NOT in the
	// active set either — the ExecStart process is already gone.
	for _, after := range []string{"deactivating", "failed", "inactive"} {
		f.units.state = after
		res, err := f.gate.Resolve(context.Background())
		if err != nil {
			t.Fatalf("Resolve after %q: %v", after, err)
		}
		if res.Branch != BranchNotApplied {
			t.Fatalf("after %q the branch is %q, want %q", after, res.Branch, BranchNotApplied)
		}
		break
	}
	if got := f.row(updateID).State; got != model.UpdateFailed {
		t.Errorf("self_updates.state = %q, want failed", got)
	}
}

// TestGateBranch3ClosesTheUpdate: the target is not this version and no actor is
// active. Row and job `failed`/`update_not_applied`, the marker deleted, the
// scratch cleared and F24 raised.
func TestGateBranch3ClosesTheUpdate(t *testing.T) {
	t.Parallel()

	f := newGateFixture(t, thisVersion)
	f.seedUpdate(updateID, model.UpdateStaged, model.JobInterrupted, "v1.3.0")
	f.writeMarker(marker(updateID, "v1.3.0"))
	scratch := f.scratch("llamaman_v1.3.0_linux_amd64.tar.gz")

	res, err := f.gate.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Branch != BranchNotApplied {
		t.Fatalf("branch = %q, want %q", res.Branch, BranchNotApplied)
	}
	if got := f.row(updateID).State; got != model.UpdateFailed {
		t.Errorf("self_updates.state = %q, want failed", got)
	}
	job := f.job(updateID)
	if job.State != model.JobFailed {
		t.Errorf("jobs.state = %q, want failed", job.State)
	}
	if job.ErrorCode == nil || *job.ErrorCode != string(model.CodeUpdateNotApplied) {
		t.Errorf("jobs.error_code = %v, want %q", job.ErrorCode, model.CodeUpdateNotApplied)
	}
	if f.row(updateID).ErrorMessage == nil {
		t.Error("the domain row carries no message; §2.3a puts the code on the job and the message here")
	}
	if exists(f.l.PendingPath()) || exists(scratch) {
		t.Error("branch 3 left the marker or the scratch behind")
	}
	if codes := f.notifications(); len(codes) != 1 || codes[0] != string(model.CodeUpdateNotApplied) {
		t.Errorf("notifications = %v, want one %q (F24)", codes, model.CodeUpdateNotApplied)
	}
}

// TestGateSweepsAnUnreadableMarker is the property that stops a file no reader
// understands from outliving every process that does: an unreadable or
// unknown-format `pending` takes branch 3, naming the FILE rather than a version.
func TestGateSweepsAnUnreadableMarker(t *testing.T) {
	t.Parallel()

	cases := []struct{ name, body string }{
		{"a format from a release this binary predates", `{"format":99,"target_version":"v9.9.9"}`},
		{"a write cut off half way", "{not json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newGateFixture(t, thisVersion)
			writeFile(t, f.l.PendingPath(), []byte(tc.body))
			scratch := f.scratch("debris.tar.gz")

			res, err := f.gate.Resolve(context.Background())
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if res.Branch != BranchNotApplied || !res.MarkerUnreadable {
				t.Fatalf("branch = %q, unreadable = %v; want the sweep",
					res.Branch, res.MarkerUnreadable)
			}
			if exists(f.l.PendingPath()) {
				t.Error("a marker this binary cannot read was DEFERRED to rather than swept")
			}
			if exists(scratch) {
				t.Error("the sweep left the scratch behind")
			}
			if codes := f.notifications(); len(codes) != 1 {
				t.Errorf("notifications = %v, want one F24", codes)
			}
		})
	}
}

// TestGateMarkerNamingNoRowIsANoOp: "a marker whose `self_update_id` names no
// `self_updates` row is a no-op in both writing branches. The branch performs no
// domain write, still unlinks the marker, still clears `update/` scratch, and —
// in branch 3 — still raises F24, taking both version strings from the marker
// rather than from a row."
//
// That state is ordinary after the most disruptive recovery in the design: F12's
// fresh-DB arm, or a `restore-db` to a snapshot older than the update. The
// branch that runs right after it must not abort a boot.
func TestGateMarkerNamingNoRowIsANoOp(t *testing.T) {
	t.Parallel()

	t.Run("branch 1", func(t *testing.T) {
		t.Parallel()
		f := newGateFixture(t, thisVersion)
		f.writeMarker(marker("01J8ZQ7X000000000000MISSING", thisVersion))

		res, err := f.gate.Resolve(context.Background())
		if err != nil {
			t.Fatalf("Resolve aborted the boot: %v", err)
		}
		if res.Branch != BranchConfirmed || res.RowResolved {
			t.Errorf("branch = %q, row_resolved = %v; want a confirmed no-op",
				res.Branch, res.RowResolved)
		}
		if exists(f.l.PendingPath()) {
			t.Error("the marker survived")
		}
	})

	t.Run("branch 3", func(t *testing.T) {
		t.Parallel()
		f := newGateFixture(t, thisVersion)
		f.writeMarker(marker("01J8ZQ7X000000000000MISSING", "v1.3.0"))

		res, err := f.gate.Resolve(context.Background())
		if err != nil {
			t.Fatalf("Resolve aborted the boot: %v", err)
		}
		if res.Branch != BranchNotApplied || res.RowResolved {
			t.Errorf("branch = %q, row_resolved = %v; want a not-applied no-op",
				res.Branch, res.RowResolved)
		}
		if exists(f.l.PendingPath()) {
			t.Error("the marker survived")
		}
		if codes := f.notifications(); len(codes) != 1 {
			t.Errorf("notifications = %v, want F24 raised from the marker's own versions", codes)
		}
	})
}

// TestGateClosingPass: "each of the three branches is re-run with an extra
// non-terminal `self_updates` row and `interrupted` job that the surviving
// marker does not name, asserting that row closed `failed`/`daemon_restarted`
// while the branch's own row and marker are resolved exactly as above, and — for
// the deferral branch — that the row ITS marker names is left untouched."
func TestGateClosingPass(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		actorState  string
		target      string
		wantBranch  Branch
		wantOwnRow  model.SelfUpdateState
		markerStays bool
	}{
		{
			name: "beside branch 1", actorState: "inactive", target: thisVersion,
			wantBranch: BranchConfirmed, wantOwnRow: model.UpdateSucceeded,
		},
		{
			name: "beside branch 2", actorState: "active", target: "v1.3.0",
			wantBranch: BranchDeferred, wantOwnRow: model.UpdateSwapping, markerStays: true,
		},
		{
			name: "beside branch 3", actorState: "inactive", target: "v1.3.0",
			wantBranch: BranchNotApplied, wantOwnRow: model.UpdateFailed,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newGateFixture(t, thisVersion)
			f.units.state = tc.actorState
			f.seedUpdate(updateID, model.UpdateSwapping, model.JobInterrupted, tc.target)
			f.writeMarker(marker(updateID, tc.target))

			// The orphan: a row a plain restart or a database restore left behind,
			// which no marker names. Without the pass its live job would refuse
			// every future update at 409 job_in_flight with no marker for any
			// caller to find.
			f.seedUpdate(orphanID, model.UpdateVerifying, model.JobInterrupted, "v1.4.0")

			res, err := f.gate.Resolve(context.Background())
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if res.Branch != tc.wantBranch {
				t.Fatalf("branch = %q, want %q", res.Branch, tc.wantBranch)
			}
			if res.ClosedOrphans != 1 {
				t.Errorf("closed %d orphans, want 1", res.ClosedOrphans)
			}
			if got := f.row(orphanID).State; got != model.UpdateFailed {
				t.Errorf("the orphan is %q, want failed", got)
			}
			job := f.job(orphanID)
			if job.ErrorCode == nil || *job.ErrorCode != string(model.CodeDaemonRestarted) {
				t.Errorf("the orphan's job error_code = %v, want %q",
					job.ErrorCode, model.CodeDaemonRestarted)
			}
			if got := f.row(updateID).State; got != tc.wantOwnRow {
				t.Errorf("the branch's own row is %q, want %q", got, tc.wantOwnRow)
			}
			if exists(f.l.PendingPath()) != tc.markerStays {
				t.Errorf("marker present = %v, want %v",
					exists(f.l.PendingPath()), tc.markerStays)
			}
		})
	}
}

// TestGateClosingPassRunsWithNoMarker: with no marker at all the routine goes
// straight to the closing pass, which is what makes the endpoint caller useful —
// it is how a `self_update` job a restart orphaned stops refusing the next apply.
func TestGateClosingPassRunsWithNoMarker(t *testing.T) {
	t.Parallel()

	f := newGateFixture(t, thisVersion)
	f.seedUpdate(orphanID, model.UpdateDownloading, model.JobInterrupted, "v1.4.0")

	res, err := f.gate.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Branch != BranchNone {
		t.Fatalf("branch = %q, want %q", res.Branch, BranchNone)
	}
	if res.ClosedOrphans != 1 {
		t.Fatalf("closed %d orphans, want 1", res.ClosedOrphans)
	}
	if got := f.job(orphanID).State; got != model.JobFailed {
		t.Errorf("the orphan's job is %q, want failed", got)
	}
}

// TestClosingPassLeavesThisBootsOwnWork is the guard's whole point: `interrupted`
// means the lease belongs to a boot that is GONE, so the pass can never close
// work the calling process is itself performing — including a forward update
// that is `downloading` on this boot's own lease, which is exactly the state the
// endpoint caller runs against.
func TestClosingPassLeavesThisBootsOwnWork(t *testing.T) {
	t.Parallel()

	f := newGateFixture(t, thisVersion)
	f.seedUpdate(orphanID, model.UpdateDownloading, model.JobRunning, "v1.4.0")

	res, err := f.gate.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.ClosedOrphans != 0 {
		t.Fatalf("the pass closed %d live rows; it may only close `interrupted` ones",
			res.ClosedOrphans)
	}
	if got := f.row(orphanID).State; got != model.UpdateDownloading {
		t.Errorf("a running download was moved to %q", got)
	}
}

// TestDisarmBeforeMigrationIsResolvedFromMemory is D92 end to end: the marker is
// read into memory and unlinked BEFORE the first migration, and step 11's gate
// then resolves that in-memory copy exactly as if it had read the file.
//
// The two are the same input and the branches do not distinguish them — which is
// what this asserts by taking branch 1 with no marker on disk at all.
func TestDisarmBeforeMigrationIsResolvedFromMemory(t *testing.T) {
	t.Parallel()

	f := newGateFixture(t, thisVersion)
	f.seedUpdate(updateID, model.UpdateSwapping, model.JobInterrupted, thisVersion)
	f.writeMarker(marker(updateID, thisVersion))

	if err := f.gate.DisarmBeforeMigration(); err != nil {
		t.Fatalf("DisarmBeforeMigration: %v", err)
	}
	if exists(f.l.PendingPath()) {
		t.Fatal("the disarm did not unlink the marker: the judge is still armed across a migration")
	}

	res, err := f.gate.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Branch != BranchConfirmed {
		t.Fatalf("branch = %q, want %q — the in-memory copy was lost", res.Branch, BranchConfirmed)
	}
	if got := f.row(updateID).State; got != model.UpdateSucceeded {
		t.Errorf("self_updates.state = %q, want succeeded", got)
	}
}

// TestDisarmBeforeMigrationSweepsAnUnreadableMarker: "an `update/pending` this
// binary cannot parse is unlinked on the same rule and reaches the gate as the
// same in-memory fact, which is the sweep branch it would have taken from disk."
func TestDisarmBeforeMigrationSweepsAnUnreadableMarker(t *testing.T) {
	t.Parallel()

	f := newGateFixture(t, thisVersion)
	writeFile(t, f.l.PendingPath(), []byte(`{"format":99}`))

	if err := f.gate.DisarmBeforeMigration(); err != nil {
		t.Fatalf("DisarmBeforeMigration: %v", err)
	}
	if exists(f.l.PendingPath()) {
		t.Fatal("an unparsable marker survived the disarm")
	}

	res, err := f.gate.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Branch != BranchNotApplied || !res.MarkerUnreadable {
		t.Fatalf("branch = %q, unreadable = %v; want the sweep", res.Branch, res.MarkerUnreadable)
	}
}

// TestDisarmWithNoMarkerIsSilent: the overwhelmingly common case. Every ordinary
// migrating boot takes this arm, and it must neither fail nor leave the gate
// holding a phantom.
func TestDisarmWithNoMarkerIsSilent(t *testing.T) {
	t.Parallel()

	f := newGateFixture(t, thisVersion)
	if err := f.gate.DisarmBeforeMigration(); err != nil {
		t.Fatalf("DisarmBeforeMigration: %v", err)
	}
	res, err := f.gate.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Branch != BranchNone {
		t.Errorf("branch = %q, want %q", res.Branch, BranchNone)
	}
}

// thisBoot is the lease owner the fixtures below stamp on a job this daemon is
// itself running — `runtime_info.boot_id` in production (§2.3).
const thisBoot = "01J8ZQ7X00000000000000BOOT"

// TestGateStandsDownForTheSwapThisBootIsPerforming is the regression test for
// the gate closing out its own update.
//
// Section 12.1 step 7 writes `update/pending` at step 6 and only THEN drains, so
// there is a window in which the marker is on disk, names a version this binary
// is not, and `llamaman-selfupdate.service` has not been summoned yet. Both live
// callers of this routine run in that window — the 30 s ticker and
// `POST /update/apply`, which resolves before its guard — and branch 3 taken
// there is catastrophic rather than untidy: it unlinks the marker that is the
// oneshot's ONLY trigger (`ConditionPathExists=`) and the judge's second
// condition, and clears the verified tarball with it. The swap is then silently
// skipped and the update cannot be reverted.
//
// The guard is the one section 12.3 already states for the closing pass, read
// from the other side: a job leased by THIS boot is work the calling process is
// itself performing, and the gate never closes that out. It is a fact about a
// process, not a clock — the daemon holding the lease either exists, or has died
// and left the job `interrupted` for the next boot's triage.
func TestGateStandsDownForTheSwapThisBootIsPerforming(t *testing.T) {
	t.Parallel()

	f := newGateFixture(t, prevVersion)
	f.gate.Attach(Attachments{BootID: thisBoot})
	// The state section 12.1 step 7 commits: row `swapping`, job RUNNING on this
	// boot's own lease, marker on disk, no actor summoned yet.
	f.seedLeasedUpdate(updateID, model.UpdateSwapping, model.JobRunning, thisVersion, thisBoot)
	f.writeMarker(marker(updateID, thisVersion))
	scratch := f.scratch("llamaman_v1.2.0_linux_amd64.tar.gz")
	f.units.state = "inactive"

	res, err := f.gate.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Branch != BranchDeferred {
		t.Fatalf("branch = %q, want %q: the gate closed out the swap this boot is performing",
			res.Branch, BranchDeferred)
	}
	if !exists(f.l.PendingPath()) {
		t.Error("the marker was unlinked: the oneshot's only trigger and the judge's " +
			"second condition are both gone, so the swap is skipped and unrevertable")
	}
	if !exists(scratch) {
		t.Error("the verified tarball was cleared out from under the swap")
	}
	if got := f.row(updateID).State; got != model.UpdateSwapping {
		t.Errorf("self_updates.state = %q, want swapping", got)
	}
	if got := f.job(updateID).State; got != model.JobRunning {
		t.Errorf("jobs.state = %q, want running", got)
	}
	if codes := f.notifications(); len(codes) != 0 {
		t.Errorf("F24 was raised against a live swap: %v", codes)
	}
}

// TestGateClosesOutAnUpdateAnotherBootLeft is the other half of the same guard,
// and the reason it is keyed to the LEASE rather than to "a live job": once the
// daemon that staged the update is gone, boot triage has moved its job to
// `interrupted` — section 2.3's "the lease belongs to a boot that is gone" — and
// branch 3 is exactly right. Every stop-point row that reaches branch 3 has this
// shape.
func TestGateClosesOutAnUpdateAnotherBootLeft(t *testing.T) {
	t.Parallel()

	f := newGateFixture(t, prevVersion)
	f.gate.Attach(Attachments{BootID: thisBoot})
	f.seedLeasedUpdate(updateID, model.UpdateSwapping, model.JobInterrupted, thisVersion,
		"01J8ZQ7X0000000000000OLDER")
	f.writeMarker(marker(updateID, thisVersion))
	f.units.state = "inactive"

	res, err := f.gate.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Branch != BranchNotApplied {
		t.Fatalf("branch = %q, want %q", res.Branch, BranchNotApplied)
	}
	if exists(f.l.PendingPath()) {
		t.Error("the marker survived branch 3")
	}
	if got := f.row(updateID).State; got != model.UpdateFailed {
		t.Errorf("self_updates.state = %q, want failed", got)
	}
	if codes := f.notifications(); len(codes) != 1 {
		t.Errorf("notifications = %v, want exactly F24", codes)
	}
}
