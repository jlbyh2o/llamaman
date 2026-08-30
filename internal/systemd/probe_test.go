package systemd

import (
	"context"
	"errors"
	"testing"

	sddbus "github.com/coreos/go-systemd/v22/dbus"

	"github.com/jlbyh2o/llamaman/internal/model"
)

type fakeLister struct {
	units  []sddbus.UnitStatus
	err    error
	closed bool
}

func (f *fakeLister) ListUnitsByNamesContext(context.Context, []string) ([]sddbus.UnitStatus, error) {
	return f.units, f.err
}

func (f *fakeLister) Close() { f.closed = true }

// TestScopeProbe is section 11.1 step 1's fallback rule, and both halves matter.
//
// A bare user-bus connection is NOT enough: a developer running `llamaman serve`
// from a desktop session has a perfectly good user bus and is not in the D2
// topology at all. The probe answers `user` only when that manager also knows
// llamaman.service.
func TestScopeProbe(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		dial  func(context.Context) (unitLister, error)
		want  model.SystemdScope
		wantK bool
	}{
		{
			name: "the user manager knows the unit",
			dial: func(context.Context) (unitLister, error) {
				return &fakeLister{units: []sddbus.UnitStatus{
					{Name: UnitDaemon, LoadState: "loaded"},
				}}, nil
			},
			want: model.ScopeUser, wantK: true,
		},
		{
			// A manager that has never heard of the unit answers with a row
			// carrying LoadState=not-found rather than omitting it, so the
			// row's presence alone proves nothing.
			name: "a user bus that does not know the unit",
			dial: func(context.Context) (unitLister, error) {
				return &fakeLister{units: []sddbus.UnitStatus{
					{Name: UnitDaemon, LoadState: "not-found"},
				}}, nil
			},
			wantK: false,
		},
		{
			name: "no user bus at all",
			dial: func(context.Context) (unitLister, error) { return nil, errors.New("no bus") },
			want: "", wantK: false,
		},
		{
			name: "the list call fails",
			dial: func(context.Context) (unitLister, error) {
				return &fakeLister{err: errors.New("timed out")}, nil
			},
			wantK: false,
		},
		{
			name:  "an empty answer",
			dial:  func(context.Context) (unitLister, error) { return &fakeLister{}, nil },
			wantK: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := scopeProbe(context.Background(), tc.dial)
			if ok != tc.wantK || got != tc.want {
				t.Errorf("scopeProbe = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.wantK)
			}
		})
	}
}

// TestScopeProbeClosesItsConnection: the probe runs once at boot and must not
// leave a bus connection behind for the rest of the daemon's life.
func TestScopeProbeClosesItsConnection(t *testing.T) {
	t.Parallel()

	lister := &fakeLister{units: []sddbus.UnitStatus{{Name: UnitDaemon, LoadState: "loaded"}}}
	scopeProbe(context.Background(), func(context.Context) (unitLister, error) { return lister, nil })
	if !lister.closed {
		t.Error("the probe left its connection open")
	}
}
