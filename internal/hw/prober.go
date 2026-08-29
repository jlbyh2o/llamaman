package hw

import "context"

// GPU is one device as the driver reports it. Every memory field is in BYTES:
// nvidia-smi emits MiB and the conversion happens exactly once, inside
// NvidiaSMIProber's parser, so nothing downstream ever has to ask (DESIGN
// section 8.6).
//
// The three VRAM fields are pointers because "unknown" and "zero" are different
// answers and the design forbids confusing them (D16, section 8.6: a probe
// failure marks GPUs unknown, never zero; `vram_bytes` is NULL, never 0). A nil
// pointer is unknown — the driver was unreachable or the field was absent from
// its output — and a non-nil pointer is a value the driver actually reported. A
// consumer that read a plain 0 for free VRAM would turn every fit verdict into
// wont_run without ever saying why, so callers must branch on nil rather than
// dereference blind.
type GPU struct {
	Index          int
	UUID           string
	Name           string
	VRAMTotalBytes *uint64 // nil = unknown; never 0 to mean unknown
	VRAMUsedBytes  *uint64 // nil = unknown; never 0 to mean unknown
	VRAMFreeBytes  *uint64 // nil = unknown; never 0 to mean unknown
	UtilizationPct int     // percent
	TemperatureC   int     // degrees Celsius
	PowerDrawWatts float64 // watts; absent on cards without a sensor
	ComputeCap     string  // "major.minor"
	DriverVersion  string
}

// VRAMKnown reports whether this device's memory figures came from the driver.
// A GPU whose memory is unknown is still a GPU the daemon lists; it is only its
// fit verdicts that must say "unknown" instead of "won't run" (D16).
func (g GPU) VRAMKnown() bool {
	return g.VRAMTotalBytes != nil && g.VRAMUsedBytes != nil && g.VRAMFreeBytes != nil
}

// Bytes is a small constructor for the VRAM fields, so a parser that has a
// value can write hw.Bytes(n) instead of taking the address of a loop variable.
func Bytes(n uint64) *uint64 { return &n }

// ComputeApp is one process holding memory on a GPU, as reported by
// nvidia-smi --query-compute-apps. UsedVRAMBytes is in bytes, converted at the
// same boundary as the fields above.
type ComputeApp struct {
	PID           int
	GPUUUID       string
	UsedVRAMBytes uint64
}

// Prober is the seam behind which GPU inventory lives (D16). v1 ships one
// implementation, NvidiaSMIProber; the ROCm and Vulkan detection SPEC section 6
// defers drops in here without touching a caller.
type Prober interface {
	// Probe returns the current GPU inventory. An implementation that cannot
	// reach the driver reports the GPUs it knows about with unknown memory —
	// nil VRAM pointers — rather than reporting zero VRAM or an empty list.
	Probe(ctx context.Context) ([]GPU, error)

	// ComputeApps returns the processes currently holding VRAM, which is how
	// the fit calculator accounts for memory this daemon did not allocate.
	ComputeApps(ctx context.Context) ([]ComputeApp, error)
}
