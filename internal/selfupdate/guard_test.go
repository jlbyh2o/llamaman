package selfupdate

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jlbyh2o/llamaman/internal/jobs"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// The update guard (DESIGN section 12.1 step 1, section 3.14, section 15).
//
// "A table-driven case over (job state × `systemd_control` ×
// `llamaman-selfupdate.service` present/absent/masked × `OnFailure=` present)
// asserting the four clauses and their four codes, run against both the endpoint
// and the prose's enumeration — the two lists are generated from ONE TABLE in
// the test, so they cannot drift into disagreeing about how many clauses there
// are."
//
// The `selfupdate_unsupported` axis is asserted to be a fact about the INSTALLED
// UNIT, not about the running binary: a case with every unit present asserts the
// clause never fires, which no self-referential "this binary has no
// selfupdate-apply subcommand" predicate could ever have failed — it is a
// compile-time constant evaluated by the very binary that would have to contain
// the endpoint to evaluate it.

// guardClauses is the one table. Both the endpoint's behavior and the count the
// prose commits to are read off it.
var guardClauses = []struct {
	code model.ErrorCode
	what string
}{
	{model.CodeJobInFlight, "a build is running, or any self_update job is live (`interrupted` counts)"},
	{CodeSelfUpdateUnavailable, "systemd_control='unavailable'"},
	{CodeSelfUpdateUnsupported, "the swap actor cannot be summoned"},
	{CodeRevertUnavailable, "there is no working revert"},
}

// TestGuardHasExactlyFourClauses is the drift check between the code and the
// three places the design enumerates them.
func TestGuardHasExactlyFourClauses(t *testing.T) {
	t.Parallel()
	if len(guardClauses) != 4 {
		var listed []string
		for _, c := range guardClauses {
			listed = append(listed, string(c.code)+" ("+c.what+")")
		}
		t.Fatalf("the guard has %d clauses — %s; section 12.1 step 1, section 3.14 and this "+
			"fixture all say exactly four", len(guardClauses), strings.Join(listed, "; "))
	}
	seen := map[model.ErrorCode]bool{}
	for _, c := range guardClauses {
		if seen[c.code] {
			t.Errorf("%q appears twice: the four clauses have four distinct codes", c.code)
		}
		seen[c.code] = true
	}
}

// healthyUnits is a host with every unit installed and the revert wired.
func healthyUnits() fakeUnitFiles {
	return fakeUnitFiles{
		DaemonUnit: {Present: true, Content: "[Unit]\nOnFailure=" + JudgeUnit + "\n"},
		SwapUnit:   {Present: true, Content: "[Service]\nType=oneshot\n"},
		JudgeUnit:  {Present: true, Content: "[Service]\nType=oneshot\n"},
	}
}

// guardFixture is a service over a real store, with everything the guard reads
// injectable.
type guardFixture struct {
	t     *testing.T
	svc   *Service
	store *store.Store
	units fakeUnitFiles
	f     *gateFixture
}

func newGuardFixture(t *testing.T, scope model.SystemdScope) *guardFixture {
	t.Helper()
	f := newGateFixture(t, thisVersion)
	units := healthyUnits()

	q, err := jobs.New(f.store, jobs.Options{BootID: "01J8ZQ7X0000000000000BOOT1"})
	if err != nil {
		t.Fatalf("build the job queue: %v", err)
	}
	svc, err := New(Config{
		Store: f.store, Jobs: q, Gate: f.gate, Layout: f.l, Scope: scope,
		Version: thisVersion, Units: units, Keys: KeySet{}, GOARCH: hostArch,
		Now: func() time.Time { return f.now },
	})
	if err != nil {
		t.Fatalf("build the service: %v", err)
	}
	return &guardFixture{t: t, svc: svc, store: f.store, units: units, f: f}
}

// TestGuardClauses is the table.
func TestGuardClauses(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		scope model.SystemdScope
		// setUp puts the host into the state under test.
		setUp func(t *testing.T, g *guardFixture)
		// wantCode is "" for a case that must be ACCEPTED.
		wantCode model.ErrorCode
	}{
		{
			name:  "every unit present and nothing live — accepted",
			scope: model.ScopeSystem,
		},
		{
			name:  "a llama.cpp build is running",
			scope: model.ScopeSystem,
			setUp: func(t *testing.T, g *guardFixture) {
				g.seedJob(model.JobLlamacppInstall, model.SubjectLlamacppVersion,
					"b10621-cpu-src", model.JobRunning)
			},
			wantCode: model.CodeJobInFlight,
		},
		{
			name:  "another self-update is live",
			scope: model.ScopeSystem,
			setUp: func(t *testing.T, g *guardFixture) {
				g.f.seedUpdate(orphanID, model.UpdateDownloading, model.JobRunning, "v1.4.0")
			},
			wantCode: model.CodeJobInFlight,
		},
		{
			name:  "an INTERRUPTED self-update counts as live",
			scope: model.ScopeSystem,
			setUp: func(t *testing.T, g *guardFixture) {
				// The gate's closing pass runs FIRST and would close an orphan
				// this apply does not conflict with — so the row is given a
				// surviving marker, which is precisely the deferral case the pass
				// must not touch.
				g.f.seedUpdate(orphanID, model.UpdateSwapping, model.JobInterrupted, "v1.4.0")
				g.f.writeMarker(marker(orphanID, "v1.4.0"))
				g.f.units.state = "active"
			},
			wantCode: model.CodeJobInFlight,
		},
		{
			name:  "systemd is unavailable",
			scope: model.ScopeSystem,
			setUp: func(t *testing.T, g *guardFixture) {
				g.setSystemdControl(model.ControlUnavailable)
			},
			wantCode: CodeSelfUpdateUnavailable,
		},
		{
			name:  "the swap actor's unit is absent",
			scope: model.ScopeSystem,
			setUp: func(t *testing.T, g *guardFixture) {
				delete(g.units, SwapUnit)
			},
			wantCode: CodeSelfUpdateUnsupported,
		},
		{
			name:  "the swap actor's unit is masked",
			scope: model.ScopeSystem,
			setUp: func(t *testing.T, g *guardFixture) {
				g.units[SwapUnit] = UnitFile{Present: true, Masked: true}
			},
			wantCode: CodeSelfUpdateUnsupported,
		},
		{
			name: "in USER scope the swap-actor clause is inapplicable and never fires",
			// There is no oneshot in the D2 topology: the daemon performs the swap
			// in process (§5.2a item 2).
			scope: model.ScopeUser,
			setUp: func(t *testing.T, g *guardFixture) {
				delete(g.units, SwapUnit)
			},
		},
		{
			name:  "llamaman.service carries no OnFailure=",
			scope: model.ScopeSystem,
			setUp: func(t *testing.T, g *guardFixture) {
				g.units[DaemonUnit] = UnitFile{Present: true, Content: "[Unit]\nDescription=x\n"}
			},
			wantCode: CodeRevertUnavailable,
		},
		{
			name:  "the judge's unit is absent",
			scope: model.ScopeSystem,
			setUp: func(t *testing.T, g *guardFixture) {
				delete(g.units, JudgeUnit)
			},
			wantCode: CodeRevertUnavailable,
		},
		{
			name:  "the judge's unit is masked",
			scope: model.ScopeSystem,
			setUp: func(t *testing.T, g *guardFixture) {
				g.units[JudgeUnit] = UnitFile{Present: true, Masked: true}
			},
			wantCode: CodeRevertUnavailable,
		},
		{
			name:  "a unit that is byte-different from the template but still carries OnFailure=",
			scope: model.ScopeSystem,
			setUp: func(t *testing.T, g *guardFixture) {
				// D95: the clause is computed from the unit's own DIRECTIVES, never
				// from a template hash, so a `drift: stale` host — the ordinary
				// state after a self-update across a release that changed a
				// template — still updates.
				g.units[DaemonUnit] = UnitFile{
					Present: true,
					Content: "# llamaman-units: 0\n[Unit]\nDescription=an older render\n" +
						"OnFailure=" + JudgeUnit + "\n",
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := newGuardFixture(t, tc.scope)
			if tc.setUp != nil {
				tc.setUp(t, g)
			}

			_, err := g.svc.Apply(context.Background(), ApplyRequest{Tag: "v1.3.0"})
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("the guard refused a host it should have accepted: %v", err)
				}
				return
			}

			var me model.Error
			if !errors.As(err, &me) {
				t.Fatalf("got %v, want a model.Error carrying %q", err, tc.wantCode)
			}
			if me.Code != tc.wantCode {
				t.Fatalf("code = %q, want %q", me.Code, tc.wantCode)
			}
			// Every refusal names a command, because a refusal a human cannot act
			// on is a dead end.
			if tc.wantCode != model.CodeJobInFlight {
				if me.Details["command"] == nil {
					t.Errorf("%q carries no command for the operator to run", me.Code)
				}
			}
		})
	}
}

// TestGuardRefusesBeforeAnythingIsStaged is section 15's "with the state
// directory diffed to prove `update/` was never touched".
func TestGuardRefusesBeforeAnythingIsStaged(t *testing.T) {
	t.Parallel()

	g := newGuardFixture(t, model.ScopeSystem)
	delete(g.units, SwapUnit)
	debris := g.f.scratch("a-previous-tarball.tar.gz")

	if _, err := g.svc.Apply(context.Background(), ApplyRequest{Tag: "v1.3.0"}); err == nil {
		t.Fatal("the guard staged an update it should have refused")
	}
	if !exists(debris) {
		t.Error("a refused apply emptied update/; only an ACCEPTED one may, and only " +
			"after its transaction commits")
	}
	if exists(g.f.l.PendingPath()) {
		t.Error("a refused apply wrote a marker")
	}
}

// TestApplyStagesAndClearsScratch is the accepted path: the row and its job are
// inserted in one transaction, and `update/` is emptied only AFTER that
// transaction commits (D97).
func TestApplyStagesAndClearsScratch(t *testing.T) {
	t.Parallel()

	g := newGuardFixture(t, model.ScopeSystem)
	debris := g.f.scratch("a-previous-tarball.tar.gz")

	res, err := g.svc.Apply(context.Background(), ApplyRequest{Tag: "v1.3.0"})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.SelfUpdateID == "" || res.Job.ID == "" {
		t.Fatal("Apply returned no row or no job")
	}
	if exists(debris) {
		t.Error("update/ was not emptied after the transaction committed")
	}
	if res.SchemaWarning {
		t.Error("a forward update carries the downgrade warning")
	}

	row := g.f.row(res.SelfUpdateID)
	if row.State != model.UpdatePlanned {
		t.Errorf("self_updates.state = %q, want planned", row.State)
	}
	if row.ToVersion != "v1.3.0" || row.FromVersion != thisVersion {
		t.Errorf("row versions = %s, want %s", fmtVersions(row.FromVersion, row.ToVersion),
			fmtVersions(thisVersion, "v1.3.0"))
	}
}

// TestApplyToAnOlderTagCarriesTheFiveCommands is D90 plus D94: a downgrade is
// this same pipeline pointed at an older tag, and the response carries section
// 12.4's warning AND its five-command procedure — never the `restore-db` line
// alone, and never without its `reset-failed` step.
func TestApplyToAnOlderTagCarriesTheFiveCommands(t *testing.T) {
	t.Parallel()

	g := newGuardFixture(t, model.ScopeSystem)
	res, err := g.svc.Apply(context.Background(), ApplyRequest{Tag: prevVersion})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !res.SchemaWarning {
		t.Error("a downgrade carries no schema warning")
	}
	if len(res.Procedure) != 5 {
		t.Fatalf("the procedure has %d commands, want 5 (D94)", len(res.Procedure))
	}
	wantIn := []string{"systemctl stop", "install.sh", "restore-db", "reset-failed", "systemctl start"}
	for i, want := range wantIn {
		if !strings.Contains(res.Procedure[i], want) {
			t.Errorf("command %d is %q, want it to contain %q", i+1, res.Procedure[i], want)
		}
	}
}

// TestCancelCutOff is D96: a cancel is honored while the row is
// `planned`/`downloading`/`verifying`, and refused `409
// selfupdate_not_cancelable` at or after the `staged` commit — because from that
// instant the marker is on disk and the swap is a unit systemd owns.
func TestCancelCutOff(t *testing.T) {
	t.Parallel()

	cases := []struct {
		state      model.SelfUpdateState
		cancelable bool
	}{
		{model.UpdatePlanned, true},
		{model.UpdateDownloading, true},
		{model.UpdateVerifying, true},
		{model.UpdateStaged, false},
		{model.UpdateSwapping, false},
	}

	for _, tc := range cases {
		t.Run(string(tc.state), func(t *testing.T) {
			t.Parallel()
			g := newGuardFixture(t, model.ScopeSystem)
			g.f.seedUpdate(updateID, tc.state, model.JobRunning, "v1.3.0")

			var err error
			readErr := g.store.Read(context.Background(), func(ctx context.Context, tx store.Tx) error {
				err = g.svc.CheckCancel(ctx, tx, model.Job{SubjectID: updateID})
				return nil
			})
			if readErr != nil {
				t.Fatalf("read: %v", readErr)
			}

			if tc.cancelable {
				if err != nil {
					t.Fatalf("a cancel before the `staged` commit was refused: %v", err)
				}
				return
			}
			var me model.Error
			if !errors.As(err, &me) || me.Code != model.CodeSelfUpdateNotCancelable {
				t.Fatalf("got %v, want %q", err, model.CodeSelfUpdateNotCancelable)
			}
		})
	}
}

// seedJob inserts a job with no domain row, for the build-in-flight clause.
func (g *guardFixture) seedJob(kind model.JobKind, subjectType model.JobSubjectType,
	subjectID string, state model.JobState) {

	g.t.Helper()
	err := g.store.Write(context.Background(), func(ctx context.Context, tx store.Tx) error {
		return g.store.InsertJob(ctx, tx, model.Job{
			ID: "job-" + subjectID, Kind: kind, SubjectType: subjectType,
			SubjectID: subjectID, State: state, Priority: 100,
			RunAfter: g.f.now.UnixMilli(), MaxAttempts: 1, CreatedAt: g.f.now.UnixMilli(),
		})
	})
	if err != nil {
		g.t.Fatalf("seed a %s job: %v", kind, err)
	}
}

// setSystemdControl writes the one `runtime_info` column clause 2 reads.
func (g *guardFixture) setSystemdControl(control model.SystemdControl) {
	g.t.Helper()
	err := g.store.Write(context.Background(), func(ctx context.Context, tx store.Tx) error {
		c := control
		return g.store.PutRuntimeInfo(ctx, tx, model.RuntimeInfo{
			DaemonVersion:  thisVersion,
			SystemdControl: &c,
		})
	})
	if err != nil {
		g.t.Fatalf("set systemd_control: %v", err)
	}
}
