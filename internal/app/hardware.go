package app

import "github.com/jlbyh2o/llamaman/internal/hw"

// Step 6's hardware half (DESIGN section 8.6, D16): one nvidia-smi prober,
// shared by everything that needs to know what this host's GPUs are.
//
// It is ONE value rather than one per consumer because the prober carries the
// ~2 s sample cache section 8.6 specifies. Three probers would fork nvidia-smi
// three times as often for the same answer, and — worse — would each learn the
// `gpu_uuid`-field fallback separately, so a driver that rejects that field
// would be re-detected once per consumer instead of once per process.
//
// Its three consumers are the three places GPU identity is load-bearing:
//
//   - the supervisor's D17 attribution, which writes
//     `instance_status.gpu_uuids_json` on every pass;
//   - the bench exclusivity guard, which INTERSECTS that column with the GPUs a
//     sweep would use (section 10) — and which fails closed when the probe
//     cannot answer, treating every loaded instance as a conflict;
//   - the fit calculator's host inputs (section 8.1), where an unreadable card
//     reports `free_bytes: null` rather than 0.
//
// A host with no nvidia-smi on PATH is NOT a failure and must not be confused
// with one: the prober answers with an empty inventory and no error, which is
// "this host has no NVIDIA GPU" — an ordinary state that yields a CPU-only fit
// estimate and a bench guard with nothing to be exclusive about.
func (d *daemon) buildHardware() {
	p := hw.NewNvidiaSMIProber(hw.Options{})
	d.gpus = p
	if p.Available() {
		d.log.Info("probed the GPU inventory source", "tool", "nvidia-smi")
		return
	}
	d.log.Info("nvidia-smi is not on PATH; this host is treated as having no NVIDIA GPU",
		"consequence", "fit estimates are CPU-only and the bench exclusivity guard has "+
			"no per-GPU identity to intersect")
}

// hostProbe adapts the prober to api.FitHardware, whose second half is system
// RAM rather than VRAM. The two live in one interface because section 8.7's
// `partial` verdict tests the spill against free RAM, so an estimate that had
// GPUs but no RAM figure could report `partial` for a model the host cannot hold.
type hostProbe struct{ *hw.NvidiaSMIProber }

// Memory reads /proc/meminfo. It is read per request rather than cached: it is
// one small file, and a stale free-RAM figure is exactly the kind of wrong
// answer section 8.7 forbids.
func (hostProbe) Memory() (hw.Memory, error) { return hw.Meminfo("") }
