package supervisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jlbyh2o/llamaman/internal/model"
)

const mib = uint64(1) << 20

func journalFixture(t *testing.T, name string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "journal", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

// ParseFitReport against recorded llama-server output (DESIGN section 5.8).
//
// The two properties that carry the weight: device and host buffers are kept
// apart — summing `CPU_Mapped` into the VRAM figure would teach the calibration
// a correction of about two — and a scan that found nothing reports Found false
// rather than a report full of zeros.
func TestParseFitReport(t *testing.T) {
	cases := []struct {
		name        string
		file        string
		wantWeights uint64
		wantKV      uint64
		wantCompute uint64
		wantHostW   uint64
		wantHostC   uint64
		wantOOM     bool
		wantFound   bool
		wantFit     int // 0 means "no projection expected"
	}{
		{
			name: "a CUDA load reports three device buffers and two host ones",
			file: "load-cuda.txt",
			// 4156 MiB, 896 MiB, 304 MiB on the device; 282 MiB and 24 MiB in RAM.
			wantWeights: 4156 * mib, wantKV: 896 * mib, wantCompute: 304 * mib,
			wantHostW: 282 * mib, wantHostC: 24 * mib,
			wantFound: true, wantFit: 33,
		},
		{
			name: "two devices are summed",
			file: "load-dual-gpu.txt",
			// 6144 + 2048 = 8192 MiB of weights, 512 + 256 = 768 of KV,
			// 160 + 160 = 320 of compute.
			wantWeights: 8192 * mib, wantKV: 768 * mib, wantCompute: 320 * mib,
			wantHostW: 128 * mib, wantHostC: 16 * mib,
			wantFound: true,
		},
		{
			name:        "a load that died in cudaMalloc is marked oom",
			file:        "load-oom.txt",
			wantWeights: 12288 * mib,
			wantOOM:     true, wantFound: true,
		},
		{
			name: "a CPU-only load charges nothing to a device",
			file: "load-cpu.txt",
			// Every buffer is a host buffer, so all three device totals stay 0.
			wantHostW: 3891 * mib, wantHostC: 296 * mib,
			wantFound: true,
		},
		{
			name:      "a window with no buffer report yields nothing",
			file:      "no-buffer-lines.txt",
			wantFound: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseFitReport(journalFixture(t, tc.file))
			if got.Found != tc.wantFound {
				t.Fatalf("found = %v, want %v (%s)", got.Found, tc.wantFound, got)
			}
			if got.WeightsBytes != tc.wantWeights {
				t.Errorf("weights = %d, want %d", got.WeightsBytes, tc.wantWeights)
			}
			if got.KVBytes != tc.wantKV {
				t.Errorf("kv = %d, want %d", got.KVBytes, tc.wantKV)
			}
			if got.ComputeBytes != tc.wantCompute {
				t.Errorf("compute = %d, want %d", got.ComputeBytes, tc.wantCompute)
			}
			if got.WeightsHostBytes != tc.wantHostW {
				t.Errorf("host weights = %d, want %d", got.WeightsHostBytes, tc.wantHostW)
			}
			if got.ComputeHostBytes != tc.wantHostC {
				t.Errorf("host compute = %d, want %d", got.ComputeHostBytes, tc.wantHostC)
			}
			if got.OOM != tc.wantOOM {
				t.Errorf("oom = %v, want %v", got.OOM, tc.wantOOM)
			}
			switch {
			case tc.wantFit == 0 && got.FitLayers != nil:
				t.Errorf("fit_layers = %d, want none — the build printed no projection",
					*got.FitLayers)
			case tc.wantFit != 0 && got.FitLayers == nil:
				t.Errorf("fit_layers = nil, want %d", tc.wantFit)
			case tc.wantFit != 0 && *got.FitLayers != tc.wantFit:
				t.Errorf("fit_layers = %d, want %d", *got.FitLayers, tc.wantFit)
			}
		})
	}
}

// TestParseFitReportTotals: `actual_total_bytes` is the DEVICE sum. A host
// buffer is real memory and is reported, but it is not VRAM and must not be
// added to the figure the calibration compares against a VRAM prediction.
func TestParseFitReportTotals(t *testing.T) {
	r := ParseFitReport(journalFixture(t, "load-cuda.txt"))
	if got, want := r.TotalBytes(), (4156+896+304)*mib; got != want {
		t.Errorf("total = %d, want %d", got, want)
	}
	if len(r.Buffers) != 5 {
		t.Fatalf("got %d buffers, want 5", len(r.Buffers))
	}
	byDevice := map[string]FitBuffer{}
	for _, b := range r.Buffers {
		byDevice[string(b.Kind)+"/"+b.Device] = b
	}
	if b, ok := byDevice["model/CUDA0"]; !ok || b.Host {
		t.Errorf("CUDA0 model buffer = %+v, want a device buffer", b)
	}
	if b, ok := byDevice["model/CPU_Mapped"]; !ok || !b.Host {
		t.Errorf("CPU_Mapped model buffer = %+v, want a host buffer", b)
	}
	if b, ok := byDevice["compute/CUDA_Host"]; !ok || !b.Host {
		t.Errorf("CUDA_Host compute buffer = %+v, want a host buffer", b)
	}
}

// TestParseSizeAfterEquals honors the unit rather than assuming MiB: llama.cpp
// prints GiB for a large mapped model, and a parser that assumed MiB would
// under-report it by a factor of 1024.
func TestParseSizeAfterEquals(t *testing.T) {
	cases := []struct {
		line string
		want uint64
		ok   bool
	}{
		{"load_tensors: CUDA0 model buffer size =  4156.00 MiB", 4156 * mib, true},
		{"load_tensors: CUDA0 model buffer size =  12.50 GiB", uint64(12.5 * float64(uint64(1)<<30)), true},
		{"load_tensors: CUDA0 model buffer size =  512.00 KiB", 512 * 1024, true},
		{"load_tensors: CUDA0 model buffer size =  4096 B", 4096, true},
		{"load_tensors: CUDA0 model buffer size =  1.00 PiB", 0, false},
		{"load_tensors: CUDA0 model buffer size is unknown", 0, false},
	}
	for _, tc := range cases {
		got, ok := parseSizeAfterEquals(tc.line)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("parseSizeAfterEquals(%q) = %d, %v; want %d, %v",
				tc.line, got, ok, tc.want, tc.ok)
		}
	}
}

// TestParseFitProjection is D33's ground truth: llama.cpp's own answer to the
// question `-ngl auto` asks, parsed loosely enough to survive upstream rewording
// and strictly enough not to fire on an unrelated layer line.
func TestParseFitProjection(t *testing.T) {
	cases := []struct {
		line     string
		n, total int
		ok       bool
	}{
		{"llama_model_load: --fit: projecting 33 of 33 layers on the GPU", 33, 33, true},
		{"llama_model_load: fit: 28 layers", 28, 0, true},
		{"load_tensors: offloaded 33/33 layers to GPU", 0, 0, false},
		{"llama_context: n_ctx = 8192", 0, 0, false},
		{"llama_model_load: --fit: could not project any layers", 0, 0, false},
	}
	for _, tc := range cases {
		n, total, ok := parseFitProjection(tc.line)
		if ok != tc.ok || n != tc.n || total != tc.total {
			t.Errorf("parseFitProjection(%q) = %d, %d, %v; want %d, %d, %v",
				tc.line, n, total, ok, tc.n, tc.total, tc.ok)
		}
	}
}

// TestFitObservationNeedsBothHalves: the table's purpose is a ratio, and half a
// ratio is not a weaker signal — it is no signal. An unparsed buffer stays NULL
// rather than becoming a zero that would drag every median down.
func TestFitObservationNeedsBothHalves(t *testing.T) {
	rep := ParseFitReport(journalFixture(t, "load-cuda.txt"))
	pred := FitPrediction{
		Arch: "llama", Backend: model.BackendCUDA, LlamacppTag: "b10621",
		GPUName: "NVIDIA GeForce RTX 3090", PredictedComputeBytes: 280 * int64(mib),
		NCtx: ptr(int64(8192)), FlashAttn: ptr(true),
	}

	if _, ok := fitObservation("01J", 42, pred, false, rep); ok {
		t.Error("no prediction means no row")
	}
	if _, ok := fitObservation("01J", 42, FitPrediction{}, true, rep); ok {
		t.Error("a zero prediction means no row")
	}

	o, ok := fitObservation("01J", 42, pred, true, rep)
	if !ok {
		t.Fatal("a prediction and a parsed actual must produce a row")
	}
	if o.PredictedBytes != 280*int64(mib) {
		t.Errorf("predicted = %d, want %d", o.PredictedBytes, 280*int64(mib))
	}
	if o.ActualComputeBytes == nil || *o.ActualComputeBytes != 304*int64(mib) {
		t.Errorf("actual compute = %v, want %d", o.ActualComputeBytes, 304*int64(mib))
	}
	if o.ActualTotalBytes == nil || *o.ActualTotalBytes != (4156+896+304)*int64(mib) {
		t.Errorf("actual total = %v", o.ActualTotalBytes)
	}
	if o.Source != model.FitFromInstanceStart {
		t.Errorf("source = %q, want instance_start", o.Source)
	}
	if o.OOM {
		t.Error("a successful load is not an OOM")
	}

	// A CPU-only load has no device compute buffer, so that column stays NULL.
	cpu := ParseFitReport(journalFixture(t, "load-cpu.txt"))
	o, ok = fitObservation("01J", 42, pred, true, cpu)
	if !ok {
		t.Fatal("a CPU load still records what it did report")
	}
	if o.ActualComputeBytes != nil {
		t.Errorf("actual compute = %d, want NULL for a load with no device buffer",
			*o.ActualComputeBytes)
	}
	if o.ActualWeightsBytes != nil {
		t.Errorf("actual weights = %d, want NULL", *o.ActualWeightsBytes)
	}
}
