package hw

import (
	"slices"
	"testing"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// TestAttribute is section 8.6's confidence table, row by row. The rule the
// table encodes is that `declared` and `unknown` are NOT "no GPUs": section
// 10's bench exclusivity guard treats a non-`measured` instance as occupying
// every GPU it could occupy, so the promise fails closed.
func TestAttribute(t *testing.T) {
	const (
		gpuA = "GPU-1f0b3d92-64c1-4a8e-b0f5-7c2a91d4e630"
		gpuB = "GPU-9c47ae51-2b60-4d13-8a9f-51e0c7b83d24"
	)
	apps := []ComputeApp{
		{PID: 14211, GPUUUID: gpuA, UsedVRAMBytes: 18742 * MiB},
		{PID: 14211, GPUUUID: gpuB, UsedVRAMBytes: 11336 * MiB},
		{PID: 15980, GPUUUID: gpuA, UsedVRAMBytes: 1204 * MiB},
	}

	cases := []struct {
		name       string
		apps       []ComputeApp
		appsOK     bool
		pid        int
		declared   []string
		present    []string
		wantVRAM   *uint64
		wantUUIDs  []string
		wantConfid model.GPUAttribution
	}{
		{
			name: "measured sums every row for the pid and names both GPUs",
			apps: apps, appsOK: true, pid: 14211,
			present:    []string{gpuA, gpuB},
			wantVRAM:   ptr(uint64(18742+11336) * MiB),
			wantUUIDs:  []string{gpuA, gpuB},
			wantConfid: model.AttributionMeasured,
		},
		{
			name: "measured for the second process names only its GPU",
			apps: apps, appsOK: true, pid: 15980,
			present:    []string{gpuA, gpuB},
			wantVRAM:   ptr(uint64(1204) * MiB),
			wantUUIDs:  []string{gpuA},
			wantConfid: model.AttributionMeasured,
		},
		{
			name: "no rows yet falls back to what the instance declared",
			apps: apps, appsOK: true, pid: 22001,
			declared:   []string{gpuB},
			present:    []string{gpuA, gpuB},
			wantUUIDs:  []string{gpuB},
			wantConfid: model.AttributionDeclared,
		},
		{
			name: "no rows and no declaration is the conservative superset",
			apps: apps, appsOK: true, pid: 22001,
			present:    []string{gpuA, gpuB},
			wantUUIDs:  []string{gpuA, gpuB},
			wantConfid: model.AttributionDeclared,
		},
		{
			name:       "a failed probe is unknown, with NULL vram — never 0",
			apps:       nil,
			appsOK:     false,
			pid:        14211,
			present:    []string{gpuA, gpuB},
			wantConfid: model.AttributionUnknown,
		},
		{
			name: "no GPUs at all is unknown rather than an empty measurement",
			apps: nil, appsOK: true, pid: 14211,
			wantConfid: model.AttributionUnknown,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Attribute(tc.apps, tc.appsOK, tc.pid, tc.declared, tc.present)
			if got.Confidence != tc.wantConfid {
				t.Errorf("confidence = %q, want %q", got.Confidence, tc.wantConfid)
			}
			switch {
			case tc.wantVRAM == nil && got.VRAMBytes != nil:
				t.Errorf("vram = %d, want NULL", *got.VRAMBytes)
			case tc.wantVRAM != nil && got.VRAMBytes == nil:
				t.Errorf("vram = NULL, want %d", *tc.wantVRAM)
			case tc.wantVRAM != nil && *got.VRAMBytes != *tc.wantVRAM:
				t.Errorf("vram = %d, want %d", *got.VRAMBytes, *tc.wantVRAM)
			}
			if !slices.Equal(got.GPUUUIDs, tc.wantUUIDs) {
				t.Errorf("uuids = %v, want %v", got.GPUUUIDs, tc.wantUUIDs)
			}
		})
	}
}

// TestSelectPreservesInventoryOrder: `tensor_split` and `--device` index the
// participating list, so a selection that reordered the cards would move a
// user's split onto a different physical card (section 5.7, D66).
func TestSelectPreservesInventoryOrder(t *testing.T) {
	gpus := []GPU{
		{Index: 0, UUID: "GPU-a"},
		{Index: 1, UUID: "GPU-b"},
		{Index: 2, UUID: "GPU-c"},
	}
	got := Select(gpus, []string{"GPU-c", "GPU-a"})
	if len(got) != 2 || got[0].UUID != "GPU-a" || got[1].UUID != "GPU-c" {
		t.Fatalf("Select = %v, want inventory order a,c", got)
	}
	if all := Select(gpus, nil); len(all) != 3 {
		t.Errorf("an empty selection should select everything, got %d", len(all))
	}
	if names := UUIDs(gpus); !slices.Equal(names, []string{"GPU-a", "GPU-b", "GPU-c"}) {
		t.Errorf("UUIDs = %v", names)
	}
}

func ptr[T any](v T) *T { return &v }
