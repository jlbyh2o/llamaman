package cli

import (
	"bytes"
	"errors"
	"testing"

	"github.com/jlbyh2o/llamaman/internal/supervisor"
)

// TestUnitOnlyRefusesInteractive pins the DESIGN section 1 rule: the three
// unit-only entry points refuse an interactive terminal unless --force is given.
//
// The refusal is asserted for all three; what each does once it is PAST the
// refusal differs, so that half is a separate expectation per command.
// instance-exec is the one that takes an argument — the template unit hands it
// `%i` — so with none it reports the exit status the missing instance maps to
// rather than the "not implemented" of a stub.
func TestUnitOnlyRefusesInteractive(t *testing.T) {
	cases := []struct {
		name        string
		interactive bool
		args        []string
		// wantStub is what the two stubbed commands answer; wantExec is what
		// instance-exec answers, which is never the same error.
		wantStub error
		wantExec error
	}{
		{"unit start", false, nil, ErrNotImplemented, nil},
		{"terminal", true, nil, ErrInteractive, ErrInteractive},
		{"terminal with force", true, []string{"--force"}, ErrNotImplemented, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			env := Env{Stdout: &out, Stderr: &errOut, Interactive: tc.interactive}

			for _, cmd := range []func(Env, []string) error{SelfupdateApply, UpdateVerify} {
				if err := cmd(env, tc.args); !errors.Is(err, tc.wantStub) {
					t.Errorf("got %v, want %v", err, tc.wantStub)
				}
			}

			err := InstanceExec(env, tc.args)
			if tc.wantExec != nil {
				if !errors.Is(err, tc.wantExec) {
					t.Errorf("instance-exec: got %v, want %v", err, tc.wantExec)
				}
				return
			}
			code, ok := ExitCode(err)
			if !ok || code != supervisor.ExitInstanceMissing {
				t.Errorf("instance-exec with no name: got (%v, code %d, contract %v), "+
					"want exit %d", err, code, ok, supervisor.ExitInstanceMissing)
			}
		})
	}
}
