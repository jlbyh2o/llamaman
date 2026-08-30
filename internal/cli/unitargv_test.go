package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/systemd"
)

// The correspondence that was missing, and the reason both privileged actors of
// DESIGN section 12 were dead on every host.
//
// internal/systemd asserted that each rendered `ExecStart=` names a known
// SUBCOMMAND; internal/cli asserted that each unit-only entry point refuses a
// terminal and refuses a missing `--scope`. Neither side ever asserted the thing
// in between — that the argv one package RENDERS is argv the other package
// ACCEPTS — so `unitOnly` could reject `--scope` outright and the suite stayed
// green while:
//
//   - `llamaman-selfupdate.service` exited non-zero before section 12.2 step 0,
//     so no self-update ever swapped a binary: every apply staged the marker,
//     stopped serving, burned section 9.4 step 7's 120 s failsafe with no
//     management UI, came back on the old binary and raised F24; and
//   - `llamaman-update-verify.service` exited non-zero before the judge's check
//     1, so D88's revert — this design's ONLY automatic recovery, and the safety
//     net the `revert_unavailable` guard clause exists to guarantee — was
//     inoperative on 100% of installs.
//
// The test below closes that gap by construction: it renders the units the way
// `install-units` does and drives the real entry points with the exact arguments
// it finds in them.

// renderedExecStarts returns, per scope, the argv of every rendered unit's
// `ExecStart=` — split the way systemd splits it, into a binary path, a
// subcommand and that subcommand's own arguments.
func renderedExecStarts(t *testing.T) map[model.SystemdScope][][]string {
	t.Helper()

	specs := []systemd.Spec{
		{
			Scope: model.ScopeSystem, Identity: "llamaman", IdentityGroup: "llamaman",
			Prefix: "/usr/local/bin", UnitFilesGrant: true,
			// Pinned rather than probed: this test must render identically on a
			// container with no systemctl on it (section 12.2's two-candidate
			// probe is asserted in internal/systemd, where it belongs).
			Systemctl: "/usr/bin/systemctl",
		},
		{
			Scope: model.ScopeUser, Identity: "alice", IdentityGroup: "alice",
			Prefix: "/home/alice/.local/bin", Systemctl: "/usr/bin/systemctl --user",
		},
	}

	out := map[model.SystemdScope][][]string{}
	for _, spec := range specs {
		for _, name := range systemd.UnitNames(spec.Scope) {
			content, err := spec.RenderUnit(name)
			if err != nil {
				t.Fatalf("%s/%s: %v", spec.Scope, name, err)
			}
			for _, line := range systemd.Directives(content)["ExecStart"] {
				fields := strings.Fields(line)
				if len(fields) < 2 {
					t.Fatalf("%s/%s: ExecStart=%q names no subcommand", spec.Scope, name, line)
				}
				out[spec.Scope] = append(out[spec.Scope], fields)
			}
		}
	}
	return out
}

// TestUnitOnlyEntryPointsAcceptTheirRenderedArgv drives the shared prologue of
// both privileged actors — `unitOnly` then `parseScopeFlag`, which is literally
// the first thing SelfupdateApply and UpdateVerify each do — with the exact
// arguments `install-units` writes into their units, and requires it to yield
// the scope the unit rendered.
//
// It runs the PROLOGUE rather than the whole command deliberately: the judge's
// body ends in a rename of an installed binary, and a unit test must not be one
// `systemctl is-active` away from performing a revert on the machine it happens
// to be running on. The prologue is where the defect was, is the whole of what
// the units and the CLI have to agree about, and is the only part of either
// actor that reads argv at all.
func TestUnitOnlyEntryPointsAcceptTheirRenderedArgv(t *testing.T) {
	t.Parallel()

	// The two actors of section 12, by the subcommand token their ExecStart
	// names, and the scope each must parse out of the unit it was rendered into.
	actors := map[string]bool{"selfupdate-apply": true, "update-verify": true}

	seen := map[string]bool{}
	for scope, argvs := range renderedExecStarts(t) {
		for _, argv := range argvs {
			name, args := argv[1], argv[2:]
			if !actors[name] {
				continue
			}
			t.Run(string(scope)+"/"+name, func(t *testing.T) {
				var out, errOut bytes.Buffer
				// Interactive is false: this is how systemd starts it, with
				// stdin on /dev/null.
				env := Env{Stdout: &out, Stderr: &errOut}

				rest, err := unitOnly(env, name, args)
				if err != nil {
					t.Fatalf("unitOnly rejected the rendered ExecStart %q: %v\n%s",
						strings.Join(argv, " "), err, errOut.String())
				}
				got, err := parseScopeFlag(name, env, rest)
				if err != nil {
					t.Fatalf("the rendered ExecStart %q does not parse: %v\n%s",
						strings.Join(argv, " "), err, errOut.String())
				}
				if got != scope {
					t.Errorf("%q parsed as scope %q, want %q",
						strings.Join(argv, " "), got, scope)
				}
			})
			seen[name] = true
		}
	}

	// A rendered unit that stopped naming an actor would make the loop above
	// vacuous, which is exactly the shape of the hole this test exists to close.
	for name := range actors {
		if !seen[name] {
			t.Errorf("no rendered ExecStart names %q; this test asserted nothing about it", name)
		}
	}
}

// TestSelfupdateApplyRefusesUserScope is the other half of section 5.2a item 2,
// and it is a statement about WHO WAS SUMMONED rather than about what the swap
// sequence does.
//
// `install-units` writes `llamaman-selfupdate.service` only in system scope, and
// in the D2 topology the daemon performs section 12.2's sequence in process
// instead. So this entry point being reached with `--scope user` means something
// is wrong with the installation, and the refusal names the repair line — while
// selfupdate.Apply itself must still run in user scope, because that is the code
// the D2 daemon calls.
func TestSelfupdateApplyRefusesUserScope(t *testing.T) {
	t.Parallel()

	var out, errOut bytes.Buffer
	env := Env{Stdout: &out, Stderr: &errOut}

	err := SelfupdateApply(env, []string{"--scope", "user"})
	if err == nil {
		t.Fatal("selfupdate-apply ran in user scope")
	}
	if !strings.Contains(errOut.String(), repairUnitsLine) {
		t.Errorf("the refusal did not print the repair line; got:\n%s", errOut.String())
	}
	if errors.Is(err, ErrScopeRequired) {
		t.Error("the refusal was about a missing scope, not about the topology")
	}
}

// TestInstanceExecStillRejectsUnknownFlags: relaxing `unitOnly` so the two
// actors can own `--scope` must not turn a typo into an instance name.
// `instance-exec` takes no flags of its own — the template unit hands it `%i` —
// so anything flag-shaped is still an error with usage.
func TestInstanceExecStillRejectsUnknownFlags(t *testing.T) {
	t.Parallel()

	var out, errOut bytes.Buffer
	env := Env{Stdout: &out, Stderr: &errOut}

	if err := InstanceExec(env, []string{"--bogus", "qwen"}); err == nil {
		t.Fatal("instance-exec accepted an unknown flag")
	}
	if !strings.Contains(errOut.String(), "not defined") {
		t.Errorf("the failure did not name the undefined flag; got:\n%s", errOut.String())
	}
}
