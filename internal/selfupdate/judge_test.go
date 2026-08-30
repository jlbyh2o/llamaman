package selfupdate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// The revert's trigger truth table (DESIGN section 15, section 12.2).
//
// "With `update/pending` present and `<prefix>/llamaman.prev` present, the judge
// is invoked by hand against each of `active`, `activating`, `inactive`,
// `deactivating` and `failed`, and asserts it renames on **`failed` alone** and
// exits 0 having touched nothing in the other four … plus a sixth run where the
// stub is absent entirely, asserting the judge does nothing and exits 0."
//
// Two properties are asserted on every row, because both are structural:
//
//   - The verdict is read from STDOUT, never from the exit status. `is-active`
//     exits **3** for any unit that is not active while printing the state on
//     stdout, so a judge that treated a non-zero exit as an error would be
//     inverted on the one input that matters. Every row here is driven through a
//     probe that returns a state AND would have failed, exactly as the real
//     command does.
//   - A directory diff after every run asserts the judge created no
//     `llamaman.db-wal` or `-shm` — that it never opened the database even to
//     read a schema version (section 12.2, section 11.3).

// judgeHost is a host with a retained previous binary beside the installed one,
// which is the state the judge's unit's two ConditionPathExists= lines describe.
func judgeHost(t *testing.T) *host {
	t.Helper()
	h := newHost(t)
	h.stage("v1.2.0")
	writeFile(t, h.layout.RetainedPath(), []byte("#!/bin/sh\necho v1.1.0\n"))
	if err := os.Chmod(h.layout.RetainedPath(), 0o755); err != nil {
		t.Fatalf("chmod the retained binary: %v", err)
	}
	// The installed binary is the NEW one at this point: the swap happened and
	// the daemon may or may not have come back.
	writeFile(t, h.layout.InstalledPath(), []byte("#!/bin/sh\necho v1.2.0\n"))
	if err := os.Chmod(h.layout.InstalledPath(), 0o755); err != nil {
		t.Fatalf("chmod the installed binary: %v", err)
	}
	h.installed = mustRead(t, h.layout.InstalledPath())
	return h
}

// TestJudgeTriggerTruthTable is the six rows.
func TestJudgeTriggerTruthTable(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		// state is what `systemctl is-active` prints on stdout. probeErr
		// simulates the sixth row: the stub is absent entirely.
		state    string
		probeErr error
		// wantRevert is true for exactly one row.
		wantRevert bool
	}{
		{name: "active — a daemon is running and this is not the judge's business", state: "active"},
		{name: "activating — a start is still in progress", state: "activating"},
		{name: "inactive — a human or a shutdown stopped it deliberately", state: "inactive"},
		{name: "deactivating — the service is stopping", state: "deactivating"},
		{name: "an unrecognized word", state: "something-else"},
		{name: "systemctl could not be run at all", probeErr: errors.New("exec: no systemctl")},
		{name: "failed — the ONE state that authorizes the rename", state: "failed", wantRevert: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := judgeHost(t)

			state, probeErr := tc.state, tc.probeErr
			v, err := Verify(context.Background(), VerifyOptions{
				Scope:   model.ScopeSystem,
				Layout:  h.layout,
				SelfExe: h.layout.RetainedPath(),
				IsActive: func(context.Context, model.SystemdScope) (string, error) {
					// The real exec ignores a non-zero exit status and reads
					// stdout; this stands in for exactly that behavior, and the
					// error arm is the "could not run it at all" row.
					return state, probeErr
				},
			})
			if err != nil {
				t.Fatalf("Verify returned an error for %q: %v", tc.state, err)
			}
			if v.Reverted != tc.wantRevert {
				t.Errorf("reverted = %v, want %v (state %q)", v.Reverted, tc.wantRevert, tc.state)
			}

			installed := mustRead(t, h.layout.InstalledPath())
			retainedStillThere := exists(h.layout.RetainedPath())
			if tc.wantRevert {
				if retainedStillThere {
					t.Error("the rename did not consume <prefix>/llamaman.prev")
				}
				if string(installed) != "#!/bin/sh\necho v1.1.0\n" {
					t.Errorf("the installed binary is %q, want the retained one", installed)
				}
			} else {
				h.assertInstalledUnchanged()
				if !retainedStillThere {
					t.Error("the judge consumed <prefix>/llamaman.prev without reverting")
				}
			}

			// The judge never writes a marker, and never removes one either:
			// deleting it is the GATE's job, in branch 3.
			if !exists(h.layout.PendingPath()) {
				t.Error("the judge removed update/pending; only the gate does that")
			}
			h.assertNoDatabaseFiles()
		})
	}
}

// TestJudgeRefusesMismatchedOwnership is check 1: refuse unless this process's
// own image and `<prefix>/llamaman` are owned by the same uid and the image is
// not group- or world-writable.
//
// The uid half cannot be exercised without two uids, so what is asserted here is
// the half that can be: a retained binary anyone may write is one the judge will
// not exec a revert from.
func TestJudgeRefusesAWorldWritableImage(t *testing.T) {
	t.Parallel()

	h := judgeHost(t)
	if err := os.Chmod(h.layout.RetainedPath(), 0o777); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	_, err := Verify(context.Background(), VerifyOptions{
		Scope:   model.ScopeSystem,
		Layout:  h.layout,
		SelfExe: h.layout.RetainedPath(),
		IsActive: func(context.Context, model.SystemdScope) (string, error) {
			return "failed", nil
		},
	})
	if !errors.Is(err, ErrJudgeRefused) {
		t.Fatalf("got %v, want ErrJudgeRefused", err)
	}
	// A refusal changes nothing: the host is exactly as the judge found it.
	h.assertInstalledUnchanged()
	if !exists(h.layout.RetainedPath()) {
		t.Error("a refusing judge consumed the retained binary")
	}
	h.assertNoDatabaseFiles()
}

// TestJudgeRefusesAnUnparsableScope is section 12.2's "a missing or unparsable
// `--scope` is a refusal, not a guess". The actor leaves everything in place and
// exits non-zero; the unit's ExecStopPost= still runs reset-failed and start.
func TestJudgeRefusesAnUnparsableScope(t *testing.T) {
	t.Parallel()

	h := judgeHost(t)
	_, err := Verify(context.Background(), VerifyOptions{
		Scope:   model.SystemdScope("neither"),
		Layout:  h.layout,
		SelfExe: h.layout.RetainedPath(),
		IsActive: func(context.Context, model.SystemdScope) (string, error) {
			t.Error("the judge asked systemd for a state before parsing its own scope")
			return "failed", nil
		},
	})
	if err == nil {
		t.Fatal("an unparsable scope was accepted")
	}
	h.assertInstalledUnchanged()
	if !exists(h.layout.RetainedPath()) {
		t.Error("a refusing judge consumed the retained binary")
	}
}

// TestJudgeRenameFailureNamesTheManualCommand is stop-point row 12: "a human
// reading `journalctl -u llamaman-update-verify.service` gets the exact manual
// line, which is the same one rename".
func TestJudgeRenameFailureNamesTheManualCommand(t *testing.T) {
	t.Parallel()

	h := judgeHost(t)
	// Make the rename fail by removing the retained binary between the checks and
	// the rename — the same shape as a read-only <prefix>, without needing one.
	self := filepath.Join(t.TempDir(), "llamaman.prev")
	writeFile(t, self, []byte("#!/bin/sh\n"))
	if err := os.Chmod(self, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := os.Remove(h.layout.RetainedPath()); err != nil {
		t.Fatalf("remove the retained binary: %v", err)
	}

	_, err := Verify(context.Background(), VerifyOptions{
		Scope:   model.ScopeSystem,
		Layout:  h.layout,
		SelfExe: self,
		IsActive: func(context.Context, model.SystemdScope) (string, error) {
			return "failed", nil
		},
	})
	if err == nil {
		t.Fatal("a rename that could not happen reported success")
	}
	for _, want := range []string{"sudo mv", "reset-failed", "systemctl start"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure does not name %q: %v", want, err)
		}
	}
	h.assertInstalledUnchanged()
}
