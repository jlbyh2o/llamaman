package systemd

import (
	"context"
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// fakeAuthority answers CheckAuthorization from a table and records what it was
// asked.
type fakeAuthority struct {
	answers map[string]bool
	errs    map[string]error
	asked   []authQuestion
	closed  bool
}

type authQuestion struct {
	Action  string
	Details map[string]string
}

func (f *fakeAuthority) CheckAuthorization(_ context.Context, action string, details map[string]string) (bool, error) {
	f.asked = append(f.asked, authQuestion{Action: action, Details: details})
	if err := f.errs[action]; err != nil {
		return false, err
	}
	return f.answers[action], nil
}

func (f *fakeAuthority) Close() error { f.closed = true; return nil }

// TestCheckPolkitBranches: the two answers are independent, and each maps onto
// a runtime_info column and a GET /system/capabilities field the UI acts on
// before a user clicks anything.
func TestCheckPolkitBranches(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		answers map[string]bool
		want    PolkitResult
	}{
		{
			name:    "an ordinary install",
			answers: map[string]bool{ActionManageUnits: true, ActionManageUnitFiles: true},
			want: PolkitResult{
				ManageUnits: true, ManageUnitFiles: true,
				Detail: "manage-units and manage-unit-files granted",
			},
		},
		{
			// install-units --no-autostart-grant. Everything else keeps
			// working; the autostart column goes read-only from the first page
			// load rather than erroring on the first toggle.
			name:    "the autostart grant withheld",
			answers: map[string]bool{ActionManageUnits: true},
			want: PolkitResult{
				ManageUnits: true,
				Detail:      "manage-units granted; manage-unit-files withheld (autostart is read-only)",
			},
		},
		{
			// F9: a blocking notification with the --repair-polkit line, and a
			// read-only control plane, rather than a failure on the first Start.
			name:    "nothing granted",
			answers: map[string]bool{},
			want:    PolkitResult{Detail: "manage-units denied; the control plane is read-only"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			auth := &fakeAuthority{answers: tc.answers}
			got, applicable, err := checkPolkit(context.Background(), auth)
			if err != nil {
				t.Fatalf("checkPolkit: %v", err)
			}
			if !applicable {
				t.Error("the system-scope check reported itself inapplicable")
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("PolkitResult (-want +got):\n%s", diff)
			}
		})
	}
}

// TestCheckPolkitProbesTheScopedDetail is the half a naive probe gets wrong: the
// manage-units rule is name-scoped and fails closed on an undefined unit, so
// asking without the `unit` detail would exercise a branch that is deliberately
// denied and report a working host as broken. manage-unit-files, which systemd
// authorizes bus-wide, carries no detail at all.
func TestCheckPolkitProbesTheScopedDetail(t *testing.T) {
	t.Parallel()

	auth := &fakeAuthority{answers: map[string]bool{ActionManageUnits: true, ActionManageUnitFiles: true}}
	if _, _, err := checkPolkit(context.Background(), auth); err != nil {
		t.Fatalf("checkPolkit: %v", err)
	}

	want := []authQuestion{
		{Action: ActionManageUnits, Details: map[string]string{"unit": UnitInstancesTgt}},
		{Action: ActionManageUnitFiles, Details: nil},
	}
	if diff := cmp.Diff(want, auth.asked); diff != "" {
		t.Errorf("questions asked (-want +got):\n%s", diff)
	}
}

// TestCheckPolkitPropagatesErrors: a bus error is not a denial. Reporting one as
// the other would put a host into the read-only F9 state for a transient
// failure and print a repair command that fixes nothing.
func TestCheckPolkitPropagatesErrors(t *testing.T) {
	t.Parallel()

	boom := errors.New("polkitd is not running")
	auth := &fakeAuthority{errs: map[string]error{ActionManageUnits: boom}}

	_, applicable, err := checkPolkit(context.Background(), auth)
	if !errors.Is(err, boom) {
		t.Fatalf("checkPolkit = %v, want the bus error", err)
	}
	if !applicable {
		t.Error("a bus error made the check report itself inapplicable")
	}
}

// TestCheckPolkitUserScopeAsksNothing is section 5.2a: there is no polkit rule
// in the D2 topology at all, a user manager authorizes its owner
// unconditionally, and both columns stay NULL meaning "not applicable" rather
// than "denied".
func TestCheckPolkitUserScopeAsksNothing(t *testing.T) {
	t.Parallel()

	got, applicable, err := CheckPolkit(context.Background(), model.ScopeUser)
	if err != nil {
		t.Fatalf("CheckPolkit: %v", err)
	}
	if applicable {
		t.Error("the user-scope check reported itself applicable; it must make no call at all")
	}
	if diff := cmp.Diff(PolkitResult{}, got); diff != "" {
		t.Errorf("PolkitResult (-want +got):\n%s", diff)
	}
}
