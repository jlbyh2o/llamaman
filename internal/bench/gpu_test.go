package bench

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/jlbyh2o/llamaman/internal/hw"
	"github.com/jlbyh2o/llamaman/internal/model"
)

// The exclusivity guard (DESIGN section 10, SPEC section 3.5).
//
// Two properties are asserted here and they pull in opposite directions, which
// is why both need saying:
//
//   - With a MEASURED attribution the guard is precise. That is what `gpu_uuid`
//     is for (D17): without per-GPU identity it could not tell "loaded on the
//     GPU you are about to benchmark" from "loaded on the other one", and SPEC
//     section 3.5's promise would be unkeepable on a multi-GPU host.
//   - With anything else the guard FAILS CLOSED. An instance whose attribution
//     is `declared` or `unknown` occupies every GPU it could occupy, so a bench
//     is never launched into a collision merely because attribution was
//     unavailable — and `Assumed` says which instances were included that way,
//     because "we stopped your instance on a guess" is a thing a user is
//     entitled to see beforehand.

func inventory() GPUInventory { return NewGPUInventory(twoGPUs()) }

func TestGPUInventoryResolve(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		flags model.FlagSet
		want  []string
	}{
		{
			name:  "no device selection at all is every present GPU",
			flags: model.FlagSet{},
			want:  []string{"GPU-aaaa", "GPU-bbbb"},
		},
		{
			name:  "--device names one card",
			flags: model.FlagSet{DeviceFilter: ptr("CUDA1")},
			want:  []string{"GPU-bbbb"},
		},
		{
			name:  "--device names two",
			flags: model.FlagSet{DeviceFilter: ptr("CUDA0,CUDA1")},
			want:  []string{"GPU-aaaa", "GPU-bbbb"},
		},
		{
			name:  "a tensor split names the devices it puts weight on",
			flags: model.FlagSet{TensorSplit: []float64{0.6, 0.4}},
			want:  []string{"GPU-aaaa", "GPU-bbbb"},
		},
		{
			name: "a zero ratio is not an occupancy claim",
			// "put nothing on device 1" is a statement that it is NOT used.
			flags: model.FlagSet{TensorSplit: []float64{1.0, 0}},
			want:  []string{"GPU-aaaa"},
		},
		{
			name:  "--main-gpu alone",
			flags: model.FlagSet{MainGPU: ptr(1)},
			want:  []string{"GPU-bbbb"},
		},
		{
			name: "an index this host does not have contributes nothing, " +
				"so the selection falls back to every GPU",
			flags: model.FlagSet{DeviceFilter: ptr("CUDA7")},
			want:  []string{"GPU-aaaa", "GPU-bbbb"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := inventory().Resolve(tt.flags)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("Resolve (-want +got):\n%s", diff)
			}
		})
	}
}

func TestDeviceIndex(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want int
	}{
		{"CUDA0", 0},
		{"CUDA1", 1},
		{" CUDA12 ", 12},
		{"ROCm0", 0},
		{"Vulkan3", 3},
		{"CUDA", -1},
		{"", -1},
	}
	for _, tt := range tests {
		if got := deviceIndex(tt.in); got != tt.want {
			t.Errorf("deviceIndex(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

// TestOccupancyFailsClosed is the guard's central rule. The same instance,
// loaded on GPU 1 and about to be benchmarked around on GPU 0, is a conflict or
// not depending ENTIRELY on whether the attribution was measured.
func TestOccupancyFailsClosed(t *testing.T) {
	t.Parallel()

	onGPU1 := `["GPU-bbbb"]`

	tests := []struct {
		name        string
		attribution model.GPUAttribution
		uuids       *string
		flagsJSON   string
		wantGPUs    []string
		wantAssumed bool
		// wantConflict is whether this instance collides with a benchmark
		// targeting GPU 0. It is a SEPARATE expectation from wantAssumed: a
		// fail-closed inclusion widens the claim to what the CONFIGURATION
		// allows, which is not always every card.
		wantConflict bool
	}{
		{
			name:        "measured: exactly what the driver reported",
			attribution: model.AttributionMeasured,
			uuids:       &onGPU1,
			flagsJSON:   `{"device_filter":"CUDA1"}`,
			wantGPUs:    []string{"GPU-bbbb"},
		},
		{
			name:         "declared: the device_filter set, not the measurement",
			attribution:  model.AttributionDeclared,
			uuids:        &onGPU1,
			flagsJSON:    `{"device_filter":"CUDA0,CUDA1"}`,
			wantGPUs:     []string{"GPU-aaaa", "GPU-bbbb"},
			wantAssumed:  true,
			wantConflict: true,
		},
		{
			name:         "unknown with no device selection: every present GPU",
			attribution:  model.AttributionUnknown,
			flagsJSON:    `{"ctx_size":4096}`,
			wantGPUs:     []string{"GPU-aaaa", "GPU-bbbb"},
			wantAssumed:  true,
			wantConflict: true,
		},
		{
			// Assumed, but still not a collision: the widest claim the
			// CONFIGURATION supports is CUDA1, and inventing a claim the
			// configuration rules out would refuse benchmarks that are fine.
			name:        "measured but the JSON will not parse is not a measurement",
			attribution: model.AttributionMeasured,
			uuids:       ptr(`{"not":"a list"}`),
			flagsJSON:   `{"device_filter":"CUDA1"}`,
			wantGPUs:    []string{"GPU-bbbb"},
			wantAssumed: true,
		},
		{
			name:         "a configuration this daemon cannot read is the widest claim",
			attribution:  model.AttributionDeclared,
			flagsJSON:    `{"nonsense_field":1}`,
			wantGPUs:     []string{"GPU-aaaa", "GPU-bbbb"},
			wantAssumed:  true,
			wantConflict: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := model.InstanceView{
				Instance: model.Instance{ID: "01INST", Name: "busy", FlagsJSON: tt.flagsJSON},
				Status: model.InstanceStatus{
					State:          model.InstanceReady,
					GPUAttribution: tt.attribution,
					GPUUUIDsJSON:   tt.uuids,
				},
			}
			occ := OccupancyOf(v, inventory())
			if diff := cmp.Diff(tt.wantGPUs, occ.GPUUUIDs); diff != "" {
				t.Errorf("occupied GPUs (-want +got):\n%s", diff)
			}
			if occ.Assumed != tt.wantAssumed {
				t.Errorf("Assumed = %v, want %v", occ.Assumed, tt.wantAssumed)
			}

			// The consequence: a bench on GPU 0 collides with an instance whose
			// claim — measured or assumed — includes GPU 0, and with no other.
			conflicts := Conflicts([]string{"GPU-aaaa"}, []model.InstanceView{v}, inventory())
			if got := len(conflicts) > 0; got != tt.wantConflict {
				t.Errorf("conflict on GPU 0 = %v, want %v", got, tt.wantConflict)
			}
		})
	}
}

// TestConflictsSkipsInstancesThatOccupyNothing: a stopped or failed instance
// holds no VRAM, and refusing a benchmark because of one would make the guard
// useless on the host it matters most on.
func TestConflictsSkipsInstancesThatOccupyNothing(t *testing.T) {
	t.Parallel()

	states := map[model.InstanceState]bool{
		model.InstanceReady:        true,
		model.InstanceDegraded:     true,
		model.InstanceLoading:      true,
		model.InstanceStarting:     true,
		model.InstanceStopped:      false,
		model.InstanceStopping:     false,
		model.InstanceFailed:       false,
		model.InstanceCrashLooping: false,
		model.InstanceUnknown:      false,
	}
	for state, wantConflict := range states {
		t.Run(string(state), func(t *testing.T) {
			v := model.InstanceView{
				Instance: model.Instance{ID: "01INST", Name: "busy", FlagsJSON: `{}`},
				Status: model.InstanceStatus{
					State: state, GPUAttribution: model.AttributionUnknown,
				},
			}
			got := len(Conflicts([]string{"GPU-aaaa"}, []model.InstanceView{v}, inventory())) > 0
			if got != wantConflict {
				t.Errorf("conflict for a %s instance = %v, want %v", state, got, wantConflict)
			}
		})
	}

	// A soft-deleted instance is not a conflict either: nothing is running.
	deleted := model.InstanceView{
		Instance: model.Instance{
			ID: "01INST", Name: "gone", FlagsJSON: `{}`, DeletedAt: ptr(int64(1000)),
		},
		Status: model.InstanceStatus{
			State: model.InstanceReady, GPUAttribution: model.AttributionUnknown,
		},
	}
	if n := len(Conflicts([]string{"GPU-aaaa"}, []model.InstanceView{deleted}, inventory())); n != 0 {
		t.Errorf("a soft-deleted instance produced %d conflicts", n)
	}
}

// TestUnknownInventoryStillFailsClosed: a host whose GPUs could not be
// enumerated has no identity to intersect, so a bench must not be waved through.
//
// The empty target set makes every intersection empty, which read naively says
// "no conflicts" for a host that is entirely loaded — the fail-OPEN section 10
// forbids in as many words: "a bench is never launched into a collision merely
// because attribution was unavailable". So an unknown inventory returns every
// loaded instance, Assumed.
func TestUnknownInventoryStillFailsClosed(t *testing.T) {
	t.Parallel()

	unknown := UnknownGPUInventory()
	if unknown.Known() {
		t.Error("an inventory that could not be enumerated reports Known")
	}
	if got := unknown.Resolve(model.FlagSet{}); len(got) != 0 {
		t.Errorf("Resolve on an unknown inventory = %v, want nothing", got)
	}

	loaded := model.InstanceView{
		Instance: model.Instance{ID: "01INST", Name: "busy", FlagsJSON: "{}"},
		Status: model.InstanceStatus{
			State: model.InstanceReady, GPUAttribution: model.AttributionUnknown,
		},
	}
	conflicts := Conflicts(unknown.Resolve(model.FlagSet{}),
		[]model.InstanceView{loaded}, unknown)
	if len(conflicts) != 1 {
		t.Fatalf("an unenumerable host produced %d conflicts, want the loaded instance; "+
			"a driver hiccup must not launch a bench into a loaded instance's VRAM",
			len(conflicts))
	}
	if !conflicts[0].Assumed {
		t.Error("the conflict does not say it was included on a fail-closed basis")
	}
}

// TestEmptyProbeIsNotUnknown: the other half of the same distinction. A probe
// that ANSWERED with no cards is a CPU-only host, which genuinely has nothing to
// be exclusive about — refusing every bench there would make the guard useless
// rather than safe.
func TestEmptyProbeIsNotUnknown(t *testing.T) {
	t.Parallel()

	cpuOnly := NewGPUInventory(nil)
	if !cpuOnly.Known() {
		t.Fatal("a successful probe that found no GPU reports the inventory unknown")
	}
	loaded := model.InstanceView{
		Instance: model.Instance{ID: "01INST", Name: "busy", FlagsJSON: "{}"},
		Status: model.InstanceStatus{
			State: model.InstanceReady, GPUAttribution: model.AttributionUnknown,
		},
	}
	if n := len(Conflicts(nil, []model.InstanceView{loaded}, cpuOnly)); n != 0 {
		t.Errorf("a CPU-only host produced %d conflicts", n)
	}
}

// TestProbeWithNoUUIDsIsUnknown: cards without identity are cards the guard
// cannot reason about (D17), so a probe that found devices and named none of
// them is unknown rather than empty.
func TestProbeWithNoUUIDsIsUnknown(t *testing.T) {
	t.Parallel()

	inv := NewGPUInventory([]hw.GPU{{Index: 0, UUID: ""}, {Index: 1, UUID: ""}})
	if inv.Known() {
		t.Error("a probe that named no card reports the inventory known")
	}
}

func TestConflictDetails(t *testing.T) {
	t.Parallel()

	details := ConflictDetails([]Occupancy{{
		InstanceID:  "01INST",
		Name:        "busy",
		State:       model.InstanceReady,
		GPUUUIDs:    []string{"GPU-aaaa"},
		Attribution: model.AttributionDeclared,
		Assumed:     true,
	}})
	items, ok := details["instances"].([]map[string]any)
	if !ok || len(items) != 1 {
		t.Fatalf("details = %#v", details)
	}
	for _, key := range []string{"instance_id", "name", "state", "gpu_uuids", "attribution", "assumed"} {
		if _, present := items[0][key]; !present {
			t.Errorf("the 409 details omit %q", key)
		}
	}
	if items[0]["assumed"] != true {
		t.Error("the details do not say the instance was included on a fail-closed basis")
	}
}

func TestNewGPUInventorySkipsCardsWithNoUUID(t *testing.T) {
	t.Parallel()

	// A probe that reported a card with no UUID gives the guard nothing to
	// intersect for that card. Including it under an empty key would make every
	// instance look like it was on it.
	inv := NewGPUInventory([]hw.GPU{
		{Index: 0, UUID: "GPU-aaaa"},
		{Index: 1, UUID: ""},
	})
	if diff := cmp.Diff([]string{"GPU-aaaa"}, inv.All()); diff != "" {
		t.Errorf("All (-want +got):\n%s", diff)
	}
}
