package supervisor_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jlbyh2o/llamaman/internal/hw"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
	"github.com/jlbyh2o/llamaman/internal/supervisor"
)

// D17's per-instance VRAM and GPU identity, written by the reconcile pass
// (DESIGN section 5.8's last paragraph, section 8.6's table).
//
// This is the column SPEC section 3.5's bench exclusivity guard reads through
// section 10: it lists instances whose `instance_status.gpu_uuids_json`
// intersects the GPUs a sweep would use. A daemon that never populated it would
// leave that guard able to take only the fail-closed path — every loaded
// instance treated as occupying every card — which is safe but makes the
// `measured` distinction D17 exists for unreachable, and turns "benchmark the
// idle card" into "stop everything first".

// fakeGPUs is an hw.Prober whose two answers a test writes directly. The two are
// separate because the section 8.6 table's three confidences are exactly the
// three combinations that matter: a row naming the pid (`measured`), no row for
// it (`declared`), and a query that failed (`unknown`).
type fakeGPUs struct {
	gpus    []hw.GPU
	apps    []hw.ComputeApp
	appsErr error
}

func (f fakeGPUs) Probe(context.Context) ([]hw.GPU, error) { return f.gpus, nil }
func (f fakeGPUs) ComputeApps(context.Context) ([]hw.ComputeApp, error) {
	return f.apps, f.appsErr
}

func twoCards() []hw.GPU {
	return []hw.GPU{
		{Index: 0, UUID: "GPU-aaaa", Name: "NVIDIA GeForce RTX 4090",
			VRAMTotalBytes: hw.Bytes(25769803776), VRAMFreeBytes: hw.Bytes(25000000000)},
		{Index: 1, UUID: "GPU-bbbb", Name: "NVIDIA GeForce RTX 3060",
			VRAMTotalBytes: hw.Bytes(12884901888), VRAMFreeBytes: hw.Bytes(12000000000)},
	}
}

// withGPUs rebuilds the fixture's supervisor with a prober attached. The
// fixture's own constructor leaves one out, because most of these tests are
// about the ledger rather than about VRAM.
func withGPUs(t *testing.T, f *fixture, p hw.Prober) {
	t.Helper()
	sup, err := supervisor.New(supervisor.Config{
		Store:    f.DB,
		Settings: fakeSettings{"instances.health_poll_sec": 5, "instances.start_timeout_sec": 900},
		Events:   f.ev,
		Control:  f.ctl,
		Prober:   f.probe,
		StateDir: f.Dir,
		GPUs:     p,
		Now:      f.clock.now,
		NewID:    f.ids.next,
		Host: func() (supervisor.HostBoot, error) {
			return supervisor.HostBoot{ID: "boot-1", At: f.clock.now()}, nil
		},
		Exe: func(int) (string, error) { return "", context.Canceled },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	f.sup = sup
}

func TestAttributionWritesTheColumnsTheBenchGuardReads(t *testing.T) {
	const pid = 4242

	cases := []struct {
		name  string
		flags string
		gpus  fakeGPUs

		wantConfidence model.GPUAttribution
		wantUUIDs      []string
		wantVRAM       *int64
	}{
		{
			// The normal path: the driver named this pid, so the answer is a
			// measurement and the guard can tell "loaded on the card you are
			// about to benchmark" from "loaded on the other one".
			name:  "measured from the gpu_uuid column",
			flags: `{}`,
			gpus: fakeGPUs{gpus: twoCards(), apps: []hw.ComputeApp{
				{PID: pid, GPUUUID: "GPU-bbbb", UsedVRAMBytes: 7_000_000_000},
				{PID: 99, GPUUUID: "GPU-aaaa", UsedVRAMBytes: 3_000_000_000},
			}},
			wantConfidence: model.AttributionMeasured,
			wantUUIDs:      []string{"GPU-bbbb"},
			wantVRAM:       ptr(int64(7_000_000_000)),
		},
		{
			// Early in the load, before the first allocation: the driver
			// answered and named nobody, so the instance's OWN device selection
			// is the conservative superset.
			name:           "declared from the instance's own device filter",
			flags:          `{"device_filter":"CUDA0"}`,
			gpus:           fakeGPUs{gpus: twoCards()},
			wantConfidence: model.AttributionDeclared,
			wantUUIDs:      []string{"GPU-aaaa"},
		},
		{
			// Declared nothing: every present GPU, which is the widest claim
			// and the one that makes section 10's guard fail closed.
			name:           "declared as every present GPU when nothing is selected",
			flags:          `{}`,
			gpus:           fakeGPUs{gpus: twoCards()},
			wantConfidence: model.AttributionDeclared,
			wantUUIDs:      []string{"GPU-aaaa", "GPU-bbbb"},
		},
		{
			// F14: nvidia-smi failed entirely. NULL, never 0 — a fabricated zero
			// would read as "this instance uses no VRAM".
			name:           "unknown when the query failed",
			flags:          `{}`,
			gpus:           fakeGPUs{gpus: twoCards(), appsErr: context.DeadlineExceeded},
			wantConfidence: model.AttributionUnknown,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, func(i *model.Instance) { i.FlagsJSON = tc.flags })
			withGPUs(t, f, tc.gpus)

			f.reconcile(t)
			f.launchLikeInstanceExec(t, pid)
			f.probe.set(200)
			f.reconcile(t)

			st := f.Status(t, f.inst.ID)
			if st.GPUAttribution != tc.wantConfidence {
				t.Fatalf("gpu_attribution = %q, want %q", st.GPUAttribution, tc.wantConfidence)
			}

			if len(tc.wantUUIDs) == 0 {
				if st.GPUUUIDsJSON != nil {
					t.Errorf("gpu_uuids_json = %q, want NULL", *st.GPUUUIDsJSON)
				}
				if st.VRAMBytes != nil {
					t.Errorf("vram_bytes = %d, want NULL — never 0 (F14)", *st.VRAMBytes)
				}
				return
			}
			if st.GPUUUIDsJSON == nil {
				t.Fatal("gpu_uuids_json is NULL: section 10's guard has nothing to intersect")
			}
			var got []string
			if err := json.Unmarshal([]byte(*st.GPUUUIDsJSON), &got); err != nil {
				t.Fatalf("gpu_uuids_json is not a JSON array: %v", err)
			}
			if len(got) != len(tc.wantUUIDs) {
				t.Fatalf("gpu_uuids_json = %v, want %v", got, tc.wantUUIDs)
			}
			for i := range got {
				if got[i] != tc.wantUUIDs[i] {
					t.Fatalf("gpu_uuids_json = %v, want %v", got, tc.wantUUIDs)
				}
			}
			switch {
			case tc.wantVRAM == nil && st.VRAMBytes != nil:
				t.Errorf("vram_bytes = %d, want NULL: nothing measured this instance",
					*st.VRAMBytes)
			case tc.wantVRAM != nil && (st.VRAMBytes == nil || *st.VRAMBytes != *tc.wantVRAM):
				t.Errorf("vram_bytes = %v, want %d", st.VRAMBytes, *tc.wantVRAM)
			}
		})
	}
}

// TestAttributionClearsWhenNothingIsRunning: a stopped instance holds no VRAM,
// and leaving last week's measurement in the column would make the bench guard
// refuse a sweep over a card nobody is using.
func TestAttributionClearsWhenNothingIsRunning(t *testing.T) {
	const pid = 4242
	f := newFixture(t, nil)
	withGPUs(t, f, fakeGPUs{gpus: twoCards(), apps: []hw.ComputeApp{
		{PID: pid, GPUUUID: "GPU-aaaa", UsedVRAMBytes: 5_000_000_000},
	}})

	f.reconcile(t)
	f.launchLikeInstanceExec(t, pid)
	f.probe.set(200)
	f.reconcile(t)
	if f.Status(t, f.inst.ID).GPUUUIDsJSON == nil {
		t.Fatal("the running instance recorded no GPU identity")
	}

	// The unit goes away.
	if err := f.DB.Write(context.Background(), func(ctx context.Context, tx store.Tx) error {
		_, err := f.DB.SetInstanceDesiredState(ctx, tx, f.inst.ID, model.DesiredStopped,
			f.clock.now().UnixMilli())
		return err
	}); err != nil {
		t.Fatalf("stop the instance: %v", err)
	}
	f.ctl.setUnit(f.unit, unitDead())
	f.reconcile(t)

	st := f.Status(t, f.inst.ID)
	if st.GPUUUIDsJSON != nil {
		t.Errorf("a stopped instance still claims %s", *st.GPUUUIDsJSON)
	}
	if st.VRAMBytes != nil {
		t.Errorf("a stopped instance still reports %d bytes of VRAM", *st.VRAMBytes)
	}
}
