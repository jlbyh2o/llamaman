package hw

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(b)
}

// scriptRunner answers each invocation from a table keyed by a substring of the
// first argument, and records every argv it saw. It is the seam of section 8.6:
// a probe of a host with two cards, an old driver and a running llama-server
// would otherwise be testable only on that host.
type scriptRunner struct {
	mu    sync.Mutex
	calls [][]string
	// answers maps a substring of args[0] to an output/exit pair, in order.
	answers  []scriptAnswer
	startErr error
}

type scriptAnswer struct {
	match string
	out   string
	code  int
	// once limits an answer to one use, so a test can make the first call fail
	// and the second succeed.
	once bool
	used bool
}

func (s *scriptRunner) run(_ context.Context, name string, args ...string) (string, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, append([]string{name}, args...))
	if s.startErr != nil {
		return "", -1, s.startErr
	}
	joined := strings.Join(args, " ")
	for i := range s.answers {
		a := &s.answers[i]
		if a.once && a.used {
			continue
		}
		if strings.Contains(joined, a.match) {
			a.used = true
			return a.out, a.code, nil
		}
	}
	return "", 2, nil
}

func (s *scriptRunner) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *scriptRunner) argv(i int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[i]
}

func proberFor(t *testing.T, r *scriptRunner, opts Options) *NvidiaSMIProber {
	t.Helper()
	opts.Run = r.run
	opts.LookPath = func(string) (string, error) { return "/usr/bin/nvidia-smi", nil }
	return NewNvidiaSMIProber(opts)
}

// TestParseGPUsConvertsMiBToBytes is the assertion DESIGN section 8.6 names
// outright: a 24576-MiB card must become 25769803776 bytes. The conversion
// appears once in the codebase and this is what pins it — wiring the parser
// straight into a byte column is a factor-of-2²⁰ error that does not crash, it
// silently turns every fit verdict into `wont_run`.
func TestParseGPUsConvertsMiBToBytes(t *testing.T) {
	gpus, err := ParseGPUs(fixture(t, "nvidia-smi/query-gpu-single.txt"))
	if err != nil {
		t.Fatalf("ParseGPUs: %v", err)
	}
	if len(gpus) != 1 {
		t.Fatalf("got %d GPUs, want 1", len(gpus))
	}
	g := gpus[0]
	if !g.VRAMKnown() {
		t.Fatal("VRAM should be known for a line that carried all three figures")
	}
	// 24576 MiB × 1048576 = 25769803776.
	if got := *g.VRAMTotalBytes; got != 25769803776 {
		t.Errorf("vram_total_bytes = %d, want 25769803776", got)
	}
	if got := *g.VRAMFreeBytes; got != 23468*MiB {
		t.Errorf("vram_free_bytes = %d, want %d", got, uint64(23468)*MiB)
	}
	if got := *g.VRAMUsedBytes; got != 1108*MiB {
		t.Errorf("vram_used_bytes = %d, want %d", got, uint64(1108)*MiB)
	}
	// The other columns keep their own units and must NOT be scaled.
	if g.UtilizationPct != 3 || g.TemperatureC != 38 {
		t.Errorf("utilization/temperature = %d/%d, want 3/38", g.UtilizationPct, g.TemperatureC)
	}
	if g.PowerDrawWatts != 32.11 {
		t.Errorf("power.draw = %v, want 32.11", g.PowerDrawWatts)
	}
	if g.ComputeCap != "8.6" || g.DriverVersion != "580.82.07" {
		t.Errorf("compute_cap/driver = %q/%q, want 8.6/580.82.07", g.ComputeCap, g.DriverVersion)
	}
}

// TestParseGPUsTable covers the shapes a driver actually prints.
func TestParseGPUsTable(t *testing.T) {
	cases := []struct {
		name    string
		file    string
		want    int
		check   func(t *testing.T, gpus []GPU)
		wantErr bool
	}{
		{
			name: "dual",
			file: "nvidia-smi/query-gpu-dual.txt",
			want: 2,
			check: func(t *testing.T, gpus []GPU) {
				if gpus[0].Index != 0 || gpus[1].Index != 1 {
					t.Errorf("indices = %d,%d, want 0,1", gpus[0].Index, gpus[1].Index)
				}
				if *gpus[1].VRAMTotalBytes != 15360*MiB {
					t.Errorf("second card total = %d, want %d",
						*gpus[1].VRAMTotalBytes, uint64(15360)*MiB)
				}
				if gpus[1].Name != "Tesla T4" {
					t.Errorf("second card name = %q", gpus[1].Name)
				}
			},
		},
		{
			name: "not supported placeholders are blanks, not values",
			file: "nvidia-smi/query-gpu-not-supported.txt",
			want: 1,
			check: func(t *testing.T, gpus []GPU) {
				g := gpus[0]
				if g.ComputeCap != "" {
					t.Errorf("compute_cap = %q, want empty for [Not Supported]", g.ComputeCap)
				}
				if g.PowerDrawWatts != 0 {
					t.Errorf("power.draw = %v, want 0 for [N/A]", g.PowerDrawWatts)
				}
				// The memory columns were real, so they are still known.
				if !g.VRAMKnown() || *g.VRAMTotalBytes != 5120*MiB {
					t.Errorf("memory should still parse: %+v", g)
				}
			},
		},
		{
			name:    "a truncated line is an error, not a partial GPU",
			file:    "nvidia-smi/driver-mismatch.txt",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gpus, err := ParseGPUs(fixture(t, tc.file))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want an error, got %d GPUs", len(gpus))
				}
				if !errors.Is(err, ErrProbeFailed) {
					t.Errorf("error should wrap ErrProbeFailed: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseGPUs: %v", err)
			}
			if len(gpus) != tc.want {
				t.Fatalf("got %d GPUs, want %d", len(gpus), tc.want)
			}
			tc.check(t, gpus)
		})
	}
}

// TestParseComputeApps covers both column shapes and the empty answer.
func TestParseComputeApps(t *testing.T) {
	cases := []struct {
		name     string
		file     string
		fallback string
		want     []ComputeApp
	}{
		{
			name: "three columns carry their own identity",
			file: "nvidia-smi/compute-apps-dual.txt",
			want: []ComputeApp{
				{PID: 14211, GPUUUID: "GPU-1f0b3d92-64c1-4a8e-b0f5-7c2a91d4e630", UsedVRAMBytes: 18742 * MiB},
				{PID: 14211, GPUUUID: "GPU-9c47ae51-2b60-4d13-8a9f-51e0c7b83d24", UsedVRAMBytes: 11336 * MiB},
				{PID: 15980, GPUUUID: "GPU-1f0b3d92-64c1-4a8e-b0f5-7c2a91d4e630", UsedVRAMBytes: 1204 * MiB},
			},
		},
		{
			name:     "two columns take identity from the loop variable",
			file:     "nvidia-smi/compute-apps-nouuid.txt",
			fallback: "GPU-1f0b3d92-64c1-4a8e-b0f5-7c2a91d4e630",
			want: []ComputeApp{
				{PID: 14211, GPUUUID: "GPU-1f0b3d92-64c1-4a8e-b0f5-7c2a91d4e630", UsedVRAMBytes: 18742 * MiB},
				{PID: 15980, GPUUUID: "GPU-1f0b3d92-64c1-4a8e-b0f5-7c2a91d4e630", UsedVRAMBytes: 1204 * MiB},
			},
		},
		{
			name: "no running processes is not a parse failure",
			file: "nvidia-smi/compute-apps-none.txt",
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseComputeApps(fixture(t, tc.file), tc.fallback)
			if err != nil {
				t.Fatalf("ParseComputeApps: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %d rows, want %d: %+v", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("row %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestProbeFailureMarksUnknownNeverZero is D16 and F14 as an executable rule.
// The first probe succeeds, so the identity of both cards is known; the second
// fails, and every memory field must come back nil rather than 0 — a fabricated
// "0 MiB free" would make the fit calculator confidently wrong.
func TestProbeFailureMarksUnknownNeverZero(t *testing.T) {
	r := &scriptRunner{answers: []scriptAnswer{
		{match: "--query-gpu", out: fixture(t, "nvidia-smi/query-gpu-dual.txt"), code: 0, once: true},
		{match: "--query-gpu", out: fixture(t, "nvidia-smi/driver-mismatch.txt"), code: 9},
	}}
	p := proberFor(t, r, Options{TTL: -1})

	first, err := p.Probe(context.Background())
	if err != nil {
		t.Fatalf("first probe: %v", err)
	}
	if len(first) != 2 || !first[0].VRAMKnown() {
		t.Fatalf("first probe should report two measured GPUs, got %+v", first)
	}

	second, err := p.Probe(context.Background())
	if err == nil {
		t.Fatal("a driver-mismatch exit must be reported as an error")
	}
	if !errors.Is(err, ErrProbeFailed) {
		t.Errorf("error should wrap ErrProbeFailed: %v", err)
	}
	if len(second) != 2 {
		t.Fatalf("a failed probe must still list the GPUs it knows about, got %d", len(second))
	}
	for _, g := range second {
		if g.VRAMKnown() {
			t.Errorf("GPU %d still reports known VRAM after a failed probe", g.Index)
		}
		if g.VRAMTotalBytes != nil || g.VRAMUsedBytes != nil || g.VRAMFreeBytes != nil {
			t.Errorf("GPU %d has non-nil VRAM after a failure: %+v", g.Index, g)
		}
		if g.UUID == "" || g.Name == "" {
			t.Errorf("GPU %d lost its identity: %+v", g.Index, g)
		}
	}
}

// TestProbeCachesForTTL: every consumer reads through the ~2 s cache, so a page
// with four live panels forks nvidia-smi once rather than four times a second.
func TestProbeCachesForTTL(t *testing.T) {
	now := time.Unix(1788042587, 0)
	r := &scriptRunner{answers: []scriptAnswer{
		{match: "--query-gpu", out: fixture(t, "nvidia-smi/query-gpu-single.txt")},
	}}
	p := proberFor(t, r, Options{Now: func() time.Time { return now }})

	for range 4 {
		if _, err := p.Probe(context.Background()); err != nil {
			t.Fatalf("probe: %v", err)
		}
	}
	if got := r.count(); got != 1 {
		t.Fatalf("nvidia-smi ran %d times inside the TTL, want 1", got)
	}

	now = now.Add(CacheTTL + time.Millisecond)
	if _, err := p.Probe(context.Background()); err != nil {
		t.Fatalf("probe after TTL: %v", err)
	}
	if got := r.count(); got != 2 {
		t.Fatalf("nvidia-smi ran %d times across the TTL boundary, want 2", got)
	}
}

// TestComputeAppsFallsBackPerGPU is the second `measured` row of section 8.6's
// table: a driver whose nvidia-smi rejects `gpu_uuid` is detected once, by the
// non-zero exit plus its "not a valid field" message, and the loop takes GPU
// identity from `-i <index>` instead.
func TestComputeAppsFallsBackPerGPU(t *testing.T) {
	r := &scriptRunner{answers: []scriptAnswer{
		{match: "pid,gpu_uuid", out: fixture(t, "nvidia-smi/field-not-valid.txt"), code: 6},
		{match: "--query-gpu", out: fixture(t, "nvidia-smi/query-gpu-dual.txt")},
		{match: "pid,used_gpu_memory", out: fixture(t, "nvidia-smi/compute-apps-nouuid.txt")},
	}}
	p := proberFor(t, r, Options{TTL: -1})

	apps, err := p.ComputeApps(context.Background())
	if err != nil {
		t.Fatalf("ComputeApps: %v", err)
	}
	// Two GPUs × two rows in the fixture.
	if len(apps) != 4 {
		t.Fatalf("got %d rows, want 4: %+v", len(apps), apps)
	}
	uuids := map[string]int{}
	for _, a := range apps {
		if a.GPUUUID == "" {
			t.Fatalf("the fallback must supply identity from the loop variable: %+v", a)
		}
		uuids[a.GPUUUID]++
	}
	if len(uuids) != 2 {
		t.Errorf("rows should name both GPUs, got %v", uuids)
	}

	// The rejection is remembered for the process lifetime: a second call must
	// not re-run the three-column query.
	before := r.count()
	if _, err := p.ComputeApps(context.Background()); err != nil {
		t.Fatalf("second ComputeApps: %v", err)
	}
	for _, call := range r.calls[before:] {
		if strings.Contains(strings.Join(call, " "), "pid,gpu_uuid") {
			t.Fatal("the rejected gpu_uuid query was retried; the detection is not remembered")
		}
	}
}

// TestQueryArgvIsPinned: the two queries are a contract with section 8.6, and a
// silently reordered column list is a silently reordered parser.
func TestQueryArgvIsPinned(t *testing.T) {
	r := &scriptRunner{answers: []scriptAnswer{
		{match: "--query-gpu", out: fixture(t, "nvidia-smi/query-gpu-single.txt")},
		{match: "pid,gpu_uuid", out: fixture(t, "nvidia-smi/compute-apps-dual.txt")},
	}}
	p := proberFor(t, r, Options{TTL: -1})
	if _, err := p.Probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if _, err := p.ComputeApps(context.Background()); err != nil {
		t.Fatalf("compute apps: %v", err)
	}

	want := [][]string{
		{
			"/usr/bin/nvidia-smi",
			"--query-gpu=index,uuid,name,memory.total,memory.used,memory.free," +
				"utilization.gpu,temperature.gpu,power.draw,compute_cap,driver_version",
			"--format=csv,noheader,nounits",
		},
		{
			"/usr/bin/nvidia-smi",
			"--query-compute-apps=pid,gpu_uuid,used_gpu_memory",
			"--format=csv,noheader,nounits",
		},
	}
	for i, w := range want {
		got := r.argv(i)
		if len(got) != len(w) {
			t.Fatalf("call %d argv = %v, want %v", i, got, w)
		}
		for j := range w {
			if got[j] != w[j] {
				t.Errorf("call %d arg %d = %q, want %q", i, j, got[j], w[j])
			}
		}
	}
}

// TestNoNvidiaSMIIsNotAFailure: a host with no NVIDIA driver has no GPUs, which
// is an ordinary state. Returning an error there would put every CPU-only
// install into a permanent degraded mode.
func TestNoNvidiaSMIIsNotAFailure(t *testing.T) {
	p := NewNvidiaSMIProber(Options{
		LookPath: func(string) (string, error) { return "", errors.New("not found") },
		Run: func(context.Context, string, ...string) (string, int, error) {
			t.Fatal("nvidia-smi must not be run when it is not on PATH")
			return "", 0, nil
		},
	})
	gpus, err := p.Probe(context.Background())
	if err != nil || len(gpus) != 0 {
		t.Fatalf("Probe = %v, %v; want an empty inventory and no error", gpus, err)
	}
	if p.Available() {
		t.Error("Available should be false with no binary on PATH")
	}
}
