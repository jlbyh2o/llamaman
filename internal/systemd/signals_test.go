package systemd

import (
	"testing"

	sddbus "github.com/coreos/go-systemd/v22/dbus"
	godbus "github.com/godbus/dbus/v5"
)

// TestUnitNameFromPath is the decoding that lets a subscriber be matched before
// a properties round trip is spent. Getting it wrong does not fail loudly — it
// silently drops every transition for units whose names contain a `-`, `@` or
// `.`, which is all of them.
func TestUnitNameFromPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path godbus.ObjectPath
		want string
	}{
		{
			name: "the daemon unit",
			path: godbus.ObjectPath(unitObjectPrefix + "llamaman_2eservice"),
			want: "llamaman.service",
		},
		{
			name: "an instance unit, every escapable character",
			path: godbus.ObjectPath(unitObjectPrefix + "llamaman_2dinstance_40qwen3_2d8b_2eservice"),
			want: "llamaman-instance@qwen3-8b.service",
		},
		{
			name: "uppercase hex is accepted",
			path: godbus.ObjectPath(unitObjectPrefix + "llamaman_2Eservice"),
			want: "llamaman.service",
		},
		{"not a unit object", godbus.ObjectPath("/org/freedesktop/systemd1/job/42"), ""},
		{"a nested path", godbus.ObjectPath(unitObjectPrefix + "a/b"), ""},
		{"an empty name", godbus.ObjectPath(unitObjectPrefix), ""},
		{"a truncated escape", godbus.ObjectPath(unitObjectPrefix + "llamaman_2"), ""},
		{"a non-hex escape", godbus.ObjectPath(unitObjectPrefix + "llamaman_zzservice"), ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := unitNameFromPath(tc.path); got != tc.want {
				t.Errorf("unitNameFromPath(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

// TestUnitNameFromPathRoundTrip checks the decoder against systemd's own
// encoder, over the unit names this design actually produces plus the awkward
// shapes an instance name may take.
func TestUnitNameFromPathRoundTrip(t *testing.T) {
	t.Parallel()

	names := []string{
		UnitDaemon, UnitInstance, UnitInstancesTgt, UnitSelfUpdate, UnitUpdateVerify,
		InstanceUnit("qwen3-8b"),
		InstanceUnit("a"),
		InstanceUnit("0123456789012345678901234567890"),
		"llamaman.slice",
		"multi-user.target",
	}

	for _, name := range names {
		path := godbus.ObjectPath(unitObjectPrefix + sddbus.PathBusEscape(name))
		if got := unitNameFromPath(path); got != name {
			t.Errorf("round trip of %q via %q = %q", name, path, got)
		}
	}
}
