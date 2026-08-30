package supervisor

import (
	"context"
	"encoding/json"

	"github.com/jlbyh2o/llamaman/internal/hw"
	"github.com/jlbyh2o/llamaman/internal/model"
)

// Per-instance VRAM and GPU attribution (D17, DESIGN section 5.8's last
// paragraph and section 8.6).
//
// The sampler runs `nvidia-smi --query-compute-apps=pid,gpu_uuid,used_gpu_memory`
// and joins on the unit's `MainPID`: the `pid` column gives
// `instance_status.vram_bytes` (summed across rows) and the `gpu_uuid` column
// gives `instance_status.gpu_uuids_json` with `gpu_attribution='measured'`.
//
// It lives in the reconcile pass because the pass is the one place that already
// holds both halves of the join — the instance row and the systemd MainPID — and
// because `instance_status` has exactly one writer (section 5.6's writer table).
//
// The `gpu_uuid` field is what makes section 10's bench exclusivity guard
// implementable at all: a `pid,used_gpu_memory` query returns no GPU identity
// and can never answer "which GPU is this instance on". The three confidences
// this writes are read there, and the guard treats anything but `measured` as
// occupying every GPU the instance could occupy — so a wrong `measured` is far
// more dangerous than an honest `declared`, and nothing here upgrades a
// confidence it did not earn.

// attribute fills in the three D17 columns on next.
//
// It is called on every pass rather than only on a transition, because VRAM is a
// live number: a model that grows its KV cache as slots fill has a different
// `vram_bytes` a minute later, and the bench guard reads the UUID set at the
// moment a sweep starts, not at the moment the instance came up.
func (s *Supervisor) attribute(ctx context.Context, next *model.InstanceStatus,
	inst model.Instance, obs observation) {

	if s.cfg.GPUs == nil {
		// No prober: the columns are left EXACTLY as they were rather than
		// zeroed. `gpu_attribution` stays at its schema default `'unknown'` and
		// `gpu_uuids_json` stays NULL, which is what F14 requires and what
		// section 10's guard reads as the widest possible claim.
		return
	}

	// Nothing running holds nothing. This is the one place a NULL is written
	// deliberately rather than for want of an answer, and `unknown` is still the
	// right confidence: a stopped instance is excluded by the guard on its
	// STATE, long before its UUID list is consulted.
	if obs.unit != unitActive || obs.props.MainPID == 0 {
		next.VRAMBytes = nil
		next.GPUUUIDsJSON = nil
		next.GPUAttribution = model.AttributionUnknown
		return
	}

	apps, appsErr := s.cfg.GPUs.ComputeApps(ctx)
	if appsErr != nil {
		// F14: `nvidia-smi` failed entirely. NULL, never 0 — a fabricated zero
		// in `vram_bytes` would read as "this instance uses no VRAM", which is
		// the one thing the guard must never believe.
		s.log.Debug("supervisor: GPU attribution unavailable",
			"instance", inst.Name, "error", appsErr.Error())
		next.VRAMBytes = nil
		next.GPUUUIDsJSON = nil
		next.GPUAttribution = model.AttributionUnknown
		return
	}

	// The inventory is needed for the `declared` fallback only, so a failure
	// there is not fatal: the prober still reports the cards it knows about with
	// unknown memory, and identity is all this needs.
	gpus, _ := s.cfg.GPUs.Probe(ctx)

	var declared []string
	if flags, err := model.ParseFlagSet([]byte(inst.FlagsJSON)); err == nil {
		declared = hw.Declared(gpus, flags)
	}

	a := hw.Attribute(apps, true, int(obs.props.MainPID), declared, hw.UUIDs(gpus))

	next.GPUAttribution = a.Confidence
	next.VRAMBytes = nil
	if a.VRAMBytes != nil {
		v := int64(*a.VRAMBytes)
		next.VRAMBytes = &v
	}
	next.GPUUUIDsJSON = nil
	if len(a.GPUUUIDs) > 0 {
		if b, err := json.Marshal(a.GPUUUIDs); err == nil {
			raw := string(b)
			next.GPUUUIDsJSON = &raw
		}
	}
}
