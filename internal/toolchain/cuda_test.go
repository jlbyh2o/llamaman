package toolchain

import (
	"slices"
	"strings"
	"testing"
)

func TestParseNvidiaSMI(t *testing.T) {
	tests := []struct {
		name      string
		fixture   string
		present   bool
		driver    string
		caps      []string
		wantArchs []string
	}{
		{
			name: "one card", fixture: "nvidia-smi-single.txt", present: true,
			driver: "610.57.04", caps: []string{"8.9"}, wantArchs: []string{"89"},
		},
		{
			name: "two cards of different generations", fixture: "nvidia-smi-dual.txt", present: true,
			driver: "560.35.03", caps: []string{"8.9", "8.6"}, wantArchs: []string{"86", "89"},
		},
		{
			name: "a driver too old to report the capability", fixture: "nvidia-smi-not-supported.txt",
			present: true, driver: "470.256.02", caps: nil, wantArchs: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := ParseNvidiaSMI(fixture(t, tc.fixture))
			if d.Present != tc.present {
				t.Fatalf("present = %v, want %v", d.Present, tc.present)
			}
			if d.Version != tc.driver {
				t.Errorf("driver = %q, want %q", d.Version, tc.driver)
			}
			if !slices.Equal(d.ComputeCaps, tc.caps) {
				t.Errorf("compute caps = %v, want %v", d.ComputeCaps, tc.caps)
			}
			if got := d.Architectures(); !slices.Equal(got, tc.wantArchs) {
				t.Errorf("architectures = %v, want %v", got, tc.wantArchs)
			}
		})
	}
}

func TestParseNvidiaSMIEmpty(t *testing.T) {
	// nvidia-smi present, exit 0, no cards: a driver installed on a host whose
	// GPU was removed. D19 would reject the resulting build, so the probe must
	// not claim a driver is usable.
	d := ParseNvidiaSMI("\n\n")
	if d.Present {
		t.Error("no output reported as a present driver")
	}
}

func TestArchFromComputeCap(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{in: "8.9", want: "89"},
		{in: "8.6", want: "86"},
		{in: "12.0", want: "120"},
		{in: "7.5", want: "75"},
		{in: " 9.0 ", want: "90"},
		{in: "[Not Supported]", want: ""},
		{in: "", want: ""},
		{in: "8", want: ""},   // no minor: not an architecture
		{in: "x.y", want: ""}, // not numeric
		{in: "0.0", want: ""}, // a zero major is not a capability
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			if got := ArchFromComputeCap(tc.in); got != tc.want {
				t.Errorf("ArchFromComputeCap(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseNvccVersion(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		want    string
	}{
		{name: "modern toolkit prints the patch level", fixture: "nvcc-12.6.txt", want: "12.6.85"},
		{name: "older toolkit prints only the release", fixture: "nvcc-11.5-no-vfield.txt", want: "11.5"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, ok := ParseNvccVersion(fixture(t, tc.fixture))
			if !ok {
				t.Fatalf("no version parsed from %s", tc.fixture)
			}
			if v.String() != tc.want {
				t.Errorf("version = %q, want %q", v.String(), tc.want)
			}
		})
	}
}

func TestProbeDriverUsesTheNarrowestQuery(t *testing.T) {
	// The driver probe answers a BUILD question and must not turn into a second
	// GPU inventory: internal/hw owns that (D16). Asserting the query keeps the
	// two from drifting into each other.
	h := fullHost(t)
	Probe(t.Context(), h.options())

	var found bool
	for _, call := range h.calls {
		if call == "/usr/bin/nvidia-smi --query-gpu=driver_version,compute_cap --format=csv,noheader" {
			found = true
		}
		if strings.Contains(call, "memory.total") {
			t.Errorf("the toolchain probe queried VRAM (%q); that is internal/hw's job", call)
		}
	}
	if !found {
		t.Errorf("driver probe query not seen; calls were %v", h.calls)
	}
}
