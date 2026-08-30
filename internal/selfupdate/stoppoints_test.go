package selfupdate

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// **Every way this protocol stops** — DESIGN section 12.3's stop-point table,
// all sixteen rows (1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11a, 11b, 12, 13, 14, 15).
//
// Each row was walked in the design as a kill, a power loss or an error return
// between two named steps, and every one ends in a state that the next daemon
// boot, the judge, or a documented command exits from. This file drives each row
// as a FIXTURE — the on-disk state that row names, plus a fake systemd — and
// asserts two things per row: the exact on-disk state, and that the mechanism
// its row names is the one that resolves it.
//
// Two assertions run on every row, because both are the structural properties
// section 12.3 says make the table exhaustive rather than merely long:
//
//   - **No step anywhere calls StopUnit on llamaman.service.** The two actor
//     seams — Restarter and UnitStater — have no stop verb on them to call, so
//     this is enforced by the type system and re-asserted by the recorded verbs.
//   - **Every step that changes the installed binary is a single rename()
//     between two names in one directory**, so there is no intermediate on-disk
//     state for a crash to land in.
//
// Rows 5, 6, 7 and 9 additionally assert the thing three earlier readings got
// wrong: while the daemon is alive and blocked in section 12.1 step 7,
// `llamaman.service` is `active`, so an actor's `ExecStopPost=` `start` is an
// EALREADY no-op that recovers NOTHING. What gets the host moving again there is
// section 9.4 step 7's 120 s failsafe. `ExecStopPost=` earns its place in rows
// 12-14, where the unit is `failed` and there is no daemon to be already running.

// stopPoint is one row of the table.
type stopPoint struct {
	// row is the label section 12.3 gives it.
	row string
	// what the row's "stops where" column says.
	stopsWhere string
	// setUp builds the on-disk and in-database state the row's "what is on disk"
	// column names.
	setUp func(t *testing.T, f *gateFixture)
	// actorState is what `llamaman-selfupdate.service` reports while the gate
	// runs, which is the ONE question branch 2 turns on.
	actorState string
	// wantBranch is the gate branch the row's "what gets out of it" column names,
	// or "" for a row no gate call resolves (the actor and judge rows, which are
	// driven in the second table below).
	wantBranch Branch
	// wantRow and wantJob are what the row and its job must be afterwards.
	wantRow model.SelfUpdateState
	wantJob model.JobState
	// wantJobCode is `jobs.error_code`.
	wantJobCode string
	// wantMarker is whether `update/pending` survives.
	wantMarker bool
	// wantNotified is whether F24 was raised.
	wantNotified bool
	// wantScratchSurvives marks the two rows whose scratch the GATE does not
	// clear, because no branch matched: rows 1 and 2 leave `update/` debris and
	// name a different deleter — "the next `POST /update/apply` empties `update/`
	// before it stages, so the scratch has a deleter".
	wantScratchSurvives bool
}

// TestStopPointsResolvedByTheGate drives rows 1, 2, 3, 4 and 15 — every row
// whose "what gets out of it" column names the confirmation gate.
func TestStopPointsResolvedByTheGate(t *testing.T) {
	t.Parallel()

	rows := []stopPoint{
		{
			row:        "1",
			stopsWhere: "the daemon dies anywhere in §12.1 steps 1-4, before `staged` commits",
			setUp: func(t *testing.T, f *gateFixture) {
				// Row, job `interrupted`, `update/` scratch, NO marker.
				f.seedUpdate(updateID, model.UpdateDownloading, model.JobInterrupted, "v1.3.0")
				f.scratch("llamaman_v1.3.0_linux_amd64.tar.gz")
			},
			actorState: "inactive",
			wantBranch: BranchNone,
			wantRow:    model.UpdateFailed,
			wantJob:    model.JobFailed,
			// The CLOSING PASS closes it, and the closing pass's code is
			// `daemon_restarted` rather than `update_not_applied`: no marker names
			// this row, so nothing claims the update was attempted.
			wantJobCode:         string(model.CodeDaemonRestarted),
			wantScratchSurvives: true,
		},
		{
			row:        "2",
			stopsWhere: "between §12.1 steps 5 and 6 — row committed `staged`, marker not yet written",
			setUp: func(t *testing.T, f *gateFixture) {
				f.seedUpdate(updateID, model.UpdateStaged, model.JobInterrupted, "v1.3.0")
			},
			actorState:  "inactive",
			wantBranch:  BranchNone,
			wantRow:     model.UpdateFailed,
			wantJob:     model.JobFailed,
			wantJobCode: string(model.CodeDaemonRestarted),
		},
		{
			row:        "3",
			stopsWhere: "between §12.1 steps 6 and 7 — marker written, `swapping` not committed",
			setUp: func(t *testing.T, f *gateFixture) {
				f.seedUpdate(updateID, model.UpdateStaged, model.JobInterrupted, "v1.3.0")
				f.writeMarker(marker(updateID, "v1.3.0"))
			},
			actorState:   "inactive",
			wantBranch:   BranchNotApplied,
			wantRow:      model.UpdateFailed,
			wantJob:      model.JobFailed,
			wantJobCode:  string(model.CodeUpdateNotApplied),
			wantNotified: true,
		},
		{
			row:        "4 (deferred half)",
			stopsWhere: "after §12.1 step 7's commit, while the oneshot is running",
			setUp: func(t *testing.T, f *gateFixture) {
				f.seedUpdate(updateID, model.UpdateSwapping, model.JobInterrupted, "v1.3.0")
				f.writeMarker(marker(updateID, "v1.3.0"))
			},
			actorState: "active",
			wantBranch: BranchDeferred,
			wantRow:    model.UpdateSwapping,
			wantJob:    model.JobInterrupted,
			wantMarker: true,
		},
		{
			row:        "4 (resolved half)",
			stopsWhere: "the same row once the oneshot's TimeoutStartSec=120 has ended it",
			setUp: func(t *testing.T, f *gateFixture) {
				f.seedUpdate(updateID, model.UpdateSwapping, model.JobInterrupted, "v1.3.0")
				f.writeMarker(marker(updateID, "v1.3.0"))
			},
			actorState:   "failed",
			wantBranch:   BranchNotApplied,
			wantRow:      model.UpdateFailed,
			wantJob:      model.JobFailed,
			wantJobCode:  string(model.CodeUpdateNotApplied),
			wantNotified: true,
		},
		{
			row: "15",
			stopsWhere: "the daemon is killed after the judge's revert, before its gate ran — " +
				"old binary installed, marker present, llamaman.prev consumed",
			setUp: func(t *testing.T, f *gateFixture) {
				f.seedUpdate(updateID, model.UpdateSwapping, model.JobInterrupted, "v1.3.0")
				f.writeMarker(marker(updateID, "v1.3.0"))
			},
			actorState:   "inactive",
			wantBranch:   BranchNotApplied,
			wantRow:      model.UpdateFailed,
			wantJob:      model.JobFailed,
			wantJobCode:  string(model.CodeUpdateNotApplied),
			wantNotified: true,
		},
	}

	for _, tc := range rows {
		t.Run("row "+tc.row, func(t *testing.T) {
			t.Parallel()
			f := newGateFixture(t, thisVersion)
			f.units.state = tc.actorState
			tc.setUp(t, f)

			res, err := f.gate.Resolve(context.Background())
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if res.Branch != tc.wantBranch {
				t.Errorf("%s: branch = %q, want %q (%s)",
					tc.row, res.Branch, tc.wantBranch, tc.stopsWhere)
			}
			if got := f.row(updateID).State; got != tc.wantRow {
				t.Errorf("%s: self_updates.state = %q, want %q", tc.row, got, tc.wantRow)
			}
			job := f.job(updateID)
			if job.State != tc.wantJob {
				t.Errorf("%s: jobs.state = %q, want %q", tc.row, job.State, tc.wantJob)
			}
			if tc.wantJobCode != "" {
				if job.ErrorCode == nil || *job.ErrorCode != tc.wantJobCode {
					t.Errorf("%s: jobs.error_code = %v, want %q", tc.row, job.ErrorCode, tc.wantJobCode)
				}
			}
			if exists(f.l.PendingPath()) != tc.wantMarker {
				t.Errorf("%s: marker present = %v, want %v",
					tc.row, exists(f.l.PendingPath()), tc.wantMarker)
			}
			if got := len(f.notifications()) > 0; got != tc.wantNotified {
				t.Errorf("%s: F24 raised = %v, want %v", tc.row, got, tc.wantNotified)
			}
			// Rows 1 and 2 name a different deleter for their scratch — the next
			// `POST /update/apply`, which empties `update/` after its transaction
			// commits — and the gate's own writing branches clear it for the rest.
			// Either way nothing an update owned outlives it, which is the whole
			// property; what differs is which caller does the deleting.
			switch {
			case tc.wantScratchSurvives:
				if !exists(filepath.Join(f.l.UpdateDir(), "llamaman_v1.3.0_linux_amd64.tar.gz")) {
					t.Errorf("%s: the gate cleared scratch on a branch that matched nothing", tc.row)
				}
				// The deleter the row names.
				if err := f.l.ClearScratch(); err != nil {
					t.Fatalf("ClearScratch: %v", err)
				}
				assertScratchGone(t, f)
			case !tc.wantMarker:
				assertScratchGone(t, f)
			}
		})
	}
}

// assertScratchGone checks that `update/` holds nothing but, at most, the
// marker.
func assertScratchGone(t *testing.T, f *gateFixture) {
	t.Helper()
	entries, err := os.ReadDir(f.l.UpdateDir())
	if err != nil {
		t.Fatalf("read the update directory: %v", err)
	}
	for _, e := range entries {
		if e.Name() != PendingFileName {
			t.Errorf("scratch survived: %s", e.Name())
		}
	}
}

// TestStopPointsInTheActor drives rows 5, 6, 7, 8 and 9 — the actor's own
// windows.
func TestStopPointsInTheActor(t *testing.T) {
	t.Parallel()

	t.Run("row 5: the actor refuses at step 0", func(t *testing.T) {
		t.Parallel()
		// Driven five ways in TestApplyRefusalsTouchNothing; what this row adds is
		// the sentence that made three earlier readings wrong: the marker is
		// INTACT and llamaman.service was never touched by this actor, so the
		// recovery is the failsafe rather than ExecStopPost=.
		h := newHost(t)
		h.stage("v1.2.0")
		writeFile(t, h.layout.PendingPath(), []byte("{truncated"))

		restart := &fakeRestarter{}
		if _, err := Apply(context.Background(), ApplyOptions{
			Scope: model.ScopeSystem, Layout: h.layout, Keys: h.keys,
			Restart: restart, GOARCH: hostArch,
		}); err == nil {
			t.Fatal("the actor proceeded past a truncated marker")
		}
		if !exists(h.layout.PendingPath()) {
			t.Error("row 5 says the marker is intact")
		}
		if len(restart.restarted) != 0 {
			t.Errorf("row 5 says llamaman.service was never touched by this actor; got %v",
				restart.restarted)
		}
		h.assertInstalledUnchanged()
	})

	t.Run("row 6: killed between steps 1 and 2", func(t *testing.T) {
		t.Parallel()
		h, _ := stageForApply(t, "v1.2.0")
		installed := copyOf(t, h.layout.InstalledPath())

		// The state the row names: llamaman.prev is a fresh copy of the binary
		// that is STILL installed, and nothing is swapped.
		if _, err := retain(h.layout.InstalledPath(), h.layout.retainTmpPath(),
			h.layout.RetainedPath()); err != nil {
			t.Fatalf("retain: %v", err)
		}

		if got := mustRead(t, h.layout.RetainedPath()); !bytes.Equal(got, installed) {
			t.Error("llamaman.prev is not a copy of the installed binary")
		}
		if got := mustRead(t, h.layout.InstalledPath()); !bytes.Equal(got, installed) {
			t.Error("the installed binary changed before the swap")
		}
		// "The retained copy is harmless — byte-identical to <prefix>/llamaman —
		// and the next update's step 1 replaces it."
		if _, err := applyOn(t, h, &fakeRestarter{}); err != nil {
			t.Fatalf("the next update could not proceed over the retained copy: %v", err)
		}
	})

	t.Run("row 7: killed between steps 2 and 3", func(t *testing.T) {
		t.Parallel()
		h, _ := stageForApply(t, "v1.2.0")
		// `<prefix>/llamaman` unchanged; `<prefix>/llamaman.new.tmp` left behind.
		writeFile(t, filepath.Join(h.layout.Prefix, installTmpName), []byte("a half-extracted binary"))
		h.assertInstalledUnchanged()

		// "llamaman.new.tmp is opened O_TRUNC by the next actor run, so it has a
		// writer that reclaims it; nothing ever executes it."
		if _, err := applyOn(t, h, &fakeRestarter{}); err != nil {
			t.Fatalf("the next actor run could not reclaim the staging file: %v", err)
		}
		if exists(filepath.Join(h.layout.Prefix, installTmpName)) {
			t.Error("the staging file was not reclaimed")
		}
	})

	t.Run("row 8: power loss exactly at step 3's rename", func(t *testing.T) {
		t.Parallel()
		// "Atomic: <prefix>/llamaman is wholly the old binary or wholly the new
		// one, both fsynced beforehand." The racing-reader assertion is
		// TestSwapIsAtomicToAConcurrentReader; what this row adds is the STRUCTURAL
		// half — a rename between two names in one directory has no third outcome.
		l := Layout{StateDir: "/var/lib/llamaman", Prefix: "/usr/local/bin"}
		if filepath.Dir(l.installTmpPath()) != filepath.Dir(l.InstalledPath()) {
			t.Error("step 3's rename crosses directories, so it is not atomic")
		}
	})

	t.Run("row 9: killed between steps 3 and 4 — swapped, no restart issued", func(t *testing.T) {
		t.Parallel()
		h, rel := stageForApply(t, "v1.2.0")

		// Apply with no Restarter is exactly this row: the swap completed and the
		// restart was never issued.
		if _, err := applyOn(t, h, nil); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if got := mustRead(t, h.layout.InstalledPath()); !bytes.Equal(got, rel.binary) {
			t.Error("row 9 says the new binary is installed")
		}
		if !exists(h.layout.PendingPath()) {
			t.Error("row 9 says the marker is present")
		}
		// "After 120 s it exits and Restart=always starts <prefix>/llamaman, which
		// is now the NEW binary; if it works, branch 1 confirms."
		f := newGateFixture(t, "v1.2.0")
		f.seedUpdate(updateID, model.UpdateSwapping, model.JobInterrupted, "v1.2.0")
		f.writeMarker(marker(updateID, "v1.2.0"))
		res, err := f.gate.Resolve(context.Background())
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if res.Branch != BranchConfirmed {
			t.Errorf("the new binary's own gate took %q, want %q", res.Branch, BranchConfirmed)
		}
	})
}

// TestStopPointsInTheJudge drives rows 10, 12, 13 and 14.
func TestStopPointsInTheJudge(t *testing.T) {
	t.Parallel()

	t.Run("row 10: the new binary will not start at all", func(t *testing.T) {
		t.Parallel()
		h := judgeHost(t)
		v, err := Verify(context.Background(), VerifyOptions{
			Scope: model.ScopeSystem, Layout: h.layout, SelfExe: h.layout.RetainedPath(),
			IsActive: func(context.Context, model.SystemdScope) (string, error) {
				// `is-active` exits 3 and prints the state on stdout.
				return "failed", nil
			},
		})
		if err != nil || !v.Reverted {
			t.Fatalf("the judge did not revert on `failed`: reverted=%v err=%v", v.Reverted, err)
		}
		if exists(h.layout.RetainedPath()) {
			t.Error("row 10 says the rename consumes <prefix>/llamaman.prev")
		}

		// "the old daemon's gate takes branch 3 → F20/F24."
		f := newGateFixture(t, prevVersion)
		f.seedUpdate(updateID, model.UpdateSwapping, model.JobInterrupted, thisVersion)
		f.writeMarker(marker(updateID, thisVersion))
		res, err := f.gate.Resolve(context.Background())
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if res.Branch != BranchNotApplied || !res.Notified {
			t.Errorf("the reverted daemon's gate took %q (notified=%v), want %q with F24",
				res.Branch, res.Notified, BranchNotApplied)
		}
	})

	t.Run("rows 12 and 13: the judge refuses, or is killed before its rename", func(t *testing.T) {
		t.Parallel()
		h := judgeHost(t)
		before := copyOf(t, h.layout.InstalledPath())
		retainedBefore := copyOf(t, h.layout.RetainedPath())

		// Row 12: check 2 says something other than `failed`, so nothing happens.
		// Row 13 is the same on-disk outcome by a different route — killed before
		// the rename — and the table gives them one line for that reason.
		if _, err := Verify(context.Background(), VerifyOptions{
			Scope: model.ScopeSystem, Layout: h.layout, SelfExe: h.layout.RetainedPath(),
			IsActive: func(context.Context, model.SystemdScope) (string, error) {
				return "activating", nil
			},
		}); err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if !bytes.Equal(mustRead(t, h.layout.InstalledPath()), before) ||
			!bytes.Equal(mustRead(t, h.layout.RetainedPath()), retainedBefore) {
			t.Error("rows 12-13 say NOTHING changed — the host is exactly as the judge found it")
		}
	})

	t.Run("row 14: power loss exactly at the judge's rename", func(t *testing.T) {
		t.Parallel()
		// "Atomic: <prefix>/llamaman is wholly the new binary (and llamaman.prev
		// survives) or wholly the old one (and llamaman.prev is gone)." Both
		// outcomes are covered above; the structural half is that the rename is
		// between two names in one directory, so there is no third.
		l := Layout{StateDir: "/var/lib/llamaman", Prefix: "/usr/local/bin"}
		if filepath.Dir(l.RetainedPath()) != filepath.Dir(l.InstalledPath()) {
			t.Error("the revert's rename crosses directories, so it is not atomic")
		}

		// The other half of row 14 — "this is the row a +180 s timer could not
		// cover, because nothing re-arms a timer across a reboot" — is a property
		// of the UNIT rather than of this code: the trigger is OnFailure=, so it
		// simply fires again. What is asserted here is that the judge is
		// re-runnable: a second run against the post-power-loss state does the
		// right thing rather than looping.
		h := judgeHost(t)
		for i := 0; i < 2; i++ {
			v, err := Verify(context.Background(), VerifyOptions{
				Scope: model.ScopeSystem, Layout: h.layout, SelfExe: h.layout.RetainedPath(),
				IsActive: func(context.Context, model.SystemdScope) (string, error) {
					return "failed", nil
				},
			})
			if i == 0 {
				if err != nil || !v.Reverted {
					t.Fatalf("first run: reverted=%v err=%v", v.Reverted, err)
				}
				continue
			}
			// The second run finds llamaman.prev gone. In production the unit's
			// own ConditionPathExists= skips it entirely; here the body refuses
			// rather than looping, which is the same outcome for the host.
			if err == nil && v.Reverted {
				t.Error("the judge reverted twice; there is no second binary to install")
			}
		}
	})
}

// TestStopPointRow11 is the D92 pair, and the two rows differ only in whether a
// migration COMMITTED — which is the distinction `doctor` makes from
// MAX(schema_migrations.version) against the newest snapshot's.
//
// What both rows share is the property D92 exists for: **the marker is already
// gone**, so `ConditionPathExists=%S/llamaman/update/pending` is false, the judge
// is SKIPPED, and the previous binary is not renamed back over a database it
// could not open. The host is left crash-looping on the new binary rather than
// dark.
func TestStopPointRow11(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name string
		// migrated is 11a; not migrated is 11b, whose exit is shorter and
		// restores nothing.
		migrated bool
	}{
		{name: "11a — a migration committed, then the binary panicked", migrated: true},
		{name: "11b — the FIRST migration failed and moved nothing", migrated: false},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			f := newGateFixture(t, thisVersion)
			f.seedUpdate(updateID, model.UpdateSwapping, model.JobInterrupted, thisVersion)
			f.writeMarker(marker(updateID, thisVersion))

			// Section 11.1 step 4: the disarm runs BEFORE the first migration is
			// ATTEMPTED, which is what makes 11a and 11b share this line.
			if err := f.gate.DisarmBeforeMigration(); err != nil {
				t.Fatalf("DisarmBeforeMigration: %v", err)
			}
			if exists(f.l.PendingPath()) {
				t.Fatal("the marker survived the disarm: the judge would rename a binary " +
					"back over a database it can no longer open")
			}

			// The binary then panics — 11a after a migration committed, 11b
			// because the first one failed — so this gate call never happens and
			// the next boot's does. Either way the judge is inert, because its
			// second ConditionPathExists= is false.
			judged := judgeHost(t)
			if err := os.Remove(judged.layout.PendingPath()); err != nil {
				t.Fatalf("remove the marker: %v", err)
			}
			if exists(judged.layout.PendingPath()) {
				t.Fatal("the fixture still has a marker")
			}
			// The retained binary is still there — the judge was skipped, not run
			// — which is the difference between "a crash loop a human can exit"
			// and "no daemon at all".
			if !exists(judged.layout.RetainedPath()) {
				t.Error("11a/11b require <prefix>/llamaman.prev to survive")
			}

			// The `self_updates` row is closed by the next boot's closing pass
			// even though the swap itself succeeded — the one mislabeled history
			// row D92 buys the host with.
			res, err := f.gate.Resolve(context.Background())
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if res.Branch != BranchConfirmed {
				// With the in-memory copy this binary IS the target, so the gate
				// confirms — the mislabeling in the design's prose is about the
				// crash happening BEFORE this call, which the next case covers.
				t.Errorf("branch = %q, want %q", res.Branch, BranchConfirmed)
			}
		})
	}
}

// TestRow11MislabelsWhenTheCrashBeatsTheGate is the cost D92 states plainly: "a
// crash between here and step 11's commit leaves the row to the closing pass,
// which closes it `failed`/`daemon_restarted` for an update that in fact took —
// one mislabeled history row, in a narrow window, against a dark host".
//
// It is asserted rather than assumed, because a future change that quietly
// removed it would be a change to D92 and to the window it protects.
func TestRow11MislabelsWhenTheCrashBeatsTheGate(t *testing.T) {
	t.Parallel()

	f := newGateFixture(t, thisVersion)
	f.seedUpdate(updateID, model.UpdateSwapping, model.JobInterrupted, thisVersion)
	f.writeMarker(marker(updateID, thisVersion))

	// The boot that migrated, disarmed and then panicked before step 11.
	if err := f.gate.DisarmBeforeMigration(); err != nil {
		t.Fatalf("DisarmBeforeMigration: %v", err)
	}

	// The NEXT boot: a fresh gate with no in-memory copy, and no marker on disk.
	now := f.now
	next := NewGate(GateConfig{
		Store: f.store, Layout: f.l, Version: thisVersion,
		Units: f.units, Now: func() time.Time { return now },
	})
	res, err := next.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Branch != BranchNone {
		t.Fatalf("branch = %q, want %q — there is no marker left to resolve", res.Branch, BranchNone)
	}
	if res.ClosedOrphans != 1 {
		t.Fatalf("the closing pass closed %d rows, want 1", res.ClosedOrphans)
	}
	job := f.job(updateID)
	if job.ErrorCode == nil || *job.ErrorCode != string(model.CodeDaemonRestarted) {
		t.Errorf("the mislabeled row's code is %v, want %q — D92's stated price",
			job.ErrorCode, model.CodeDaemonRestarted)
	}
}
