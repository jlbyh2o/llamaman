package cli

import (
	"bytes"
	"errors"
	"testing"
)

// TestUnitOnlyRefusesInteractive pins the DESIGN section 1 rule: the three
// unit-only entry points refuse an interactive terminal unless --force is given.
func TestUnitOnlyRefusesInteractive(t *testing.T) {
	cases := []struct {
		name        string
		interactive bool
		args        []string
		want        error
	}{
		{"unit start", false, nil, ErrNotImplemented},
		{"terminal", true, nil, ErrInteractive},
		{"terminal with force", true, []string{"--force"}, ErrNotImplemented},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			env := Env{Stdout: &out, Stderr: &errOut, Interactive: tc.interactive}
			for _, cmd := range []func(Env, []string) error{InstanceExec, SelfupdateApply, UpdateVerify} {
				if err := cmd(env, tc.args); !errors.Is(err, tc.want) {
					t.Errorf("got %v, want %v", err, tc.want)
				}
			}
		})
	}
}
