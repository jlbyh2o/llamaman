package hw

import (
	"sort"
	"strconv"
	"strings"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// Per-instance VRAM and GPU identity (D17, DESIGN section 8.6).
//
// `instance_status.vram_bytes` needs only a pid and a size, but
// `instance_status.gpu_uuids_json` — which SPEC section 3.5's bench exclusivity
// guard reads through section 10 — has to know WHICH GPU each row belongs to. So
// the join produces both, and `instance_status.gpu_attribution` records HOW the
// answer was obtained, so every consumer can see its own confidence:
//
//	measured | the gpu_uuid column, joined on MainPID           | the normal path
//	measured | the per-GPU -i loop, identity from the loop      | drivers with no gpu_uuid field
//	declared | the instance's own device_filter/tensor_split/   | the process is up but has
//	         | main_gpu, else EVERY present GPU                 | allocated nothing yet
//	unknown  | —                                                | nvidia-smi failed entirely
//
// Consumers must not treat `declared` or `unknown` as "no GPUs": section 10's
// guard treats a non-`measured` instance as occupying every GPU it could
// occupy, so the exclusivity promise fails closed.

// Attribution is one instance's answer.
//
// VRAMBytes is a pointer because `unknown` writes NULL and never 0 (F14): an
// instance whose GPU memory could not be measured has not been measured, and a
// zero in that column would be read as "this instance uses no VRAM".
type Attribution struct {
	VRAMBytes  *uint64
	GPUUUIDs   []string
	Confidence model.GPUAttribution
}

// Attribute joins compute-app rows onto a MainPID.
//
// appsOK is false when the nvidia-smi call failed outright, which is the only
// case that yields `unknown`. declared is the instance's own device selection —
// the UUIDs behind its `device_filter`/`tensor_split`/`main_gpu` — and present
// is every GPU on the host, the conservative superset used when the instance
// declared nothing.
func Attribute(apps []ComputeApp, appsOK bool, mainPID int, declared, present []string) Attribution {
	if !appsOK {
		return Attribution{Confidence: model.AttributionUnknown}
	}

	var total uint64
	seen := map[string]struct{}{}
	var uuids []string
	found := false
	for _, a := range apps {
		if a.PID != mainPID || mainPID == 0 {
			continue
		}
		found = true
		total += a.UsedVRAMBytes
		if a.GPUUUID == "" {
			continue
		}
		if _, dup := seen[a.GPUUUID]; dup {
			continue
		}
		seen[a.GPUUUID] = struct{}{}
		uuids = append(uuids, a.GPUUUID)
	}
	if found {
		sort.Strings(uuids)
		return Attribution{
			VRAMBytes:  &total,
			GPUUUIDs:   uuids,
			Confidence: model.AttributionMeasured,
		}
	}

	// The process is running and the driver answered, but no row names it: it is
	// early in the load, before the first cudaMalloc. The conservative superset
	// is what the guard needs, and `declared` is what says so.
	out := declared
	if len(out) == 0 {
		out = present
	}
	if len(out) == 0 {
		return Attribution{Confidence: model.AttributionUnknown}
	}
	uuids = append([]string(nil), out...)
	sort.Strings(uuids)
	return Attribution{GPUUUIDs: uuids, Confidence: model.AttributionDeclared}
}

// UUIDs is every GPU in an inventory, in index order — the "every present GPU"
// superset of the `declared` row above.
func UUIDs(gpus []GPU) []string {
	out := make([]string, 0, len(gpus))
	for _, g := range gpus {
		if g.UUID != "" {
			out = append(out, g.UUID)
		}
	}
	return out
}

// Select narrows an inventory to a UUID list, preserving the inventory's own
// order — which is the order `--device` and `tensor_split` index (section 5.7).
// An empty want selects everything.
func Select(gpus []GPU, want []string) []GPU {
	if len(want) == 0 {
		return gpus
	}
	set := make(map[string]struct{}, len(want))
	for _, u := range want {
		set[u] = struct{}{}
	}
	out := make([]GPU, 0, len(want))
	for _, g := range gpus {
		if _, ok := set[g.UUID]; ok {
			out = append(out, g)
		}
	}
	return out
}

// Declared resolves an instance's OWN device selection against the present
// inventory — the `declared` row of section 8.6's table, and the argument
// Attribute takes when the driver answered but named no process.
//
// The three selectors compose rather than override, exactly as section 10 reads
// them: `--device CUDA0,CUDA1` names two cards, `--tensor-split 0.6,0.4` names
// the first two, `--main-gpu 1` names the second. A selector naming an index
// this host does not have contributes nothing, which is the honest reading — the
// instance would not start on that device either.
//
// An empty result means the instance declared nothing, NOT that it uses no GPU.
// Attribute's caller substitutes the every-present-GPU superset in that case,
// which is what makes the section 10 guard fail closed.
//
// The index is nvidia-smi's, which is also llama.cpp's `CUDA<n>` and also
// `gpus.gpu_index`, and those three are one stable mapping precisely because the
// launcher never sets `CUDA_VISIBLE_DEVICES` (D66).
func Declared(gpus []GPU, flags model.FlagSet) []string {
	byIndex := make(map[int]string, len(gpus))
	for _, g := range gpus {
		if g.UUID != "" {
			byIndex[g.Index] = g.UUID
		}
	}

	seen := map[string]struct{}{}
	var out []string
	add := func(i int) {
		uuid, ok := byIndex[i]
		if !ok {
			return
		}
		if _, dup := seen[uuid]; dup {
			return
		}
		seen[uuid] = struct{}{}
		out = append(out, uuid)
	}

	if flags.DeviceFilter != nil {
		for _, dev := range strings.Split(*flags.DeviceFilter, ",") {
			if i := deviceIndex(dev); i >= 0 {
				add(i)
			}
		}
	}
	for i, ratio := range flags.TensorSplit {
		// A zero ratio means "put nothing on this device", so it is not a claim.
		if ratio > 0 {
			add(i)
		}
	}
	if flags.MainGPU != nil {
		add(*flags.MainGPU)
	}

	sort.Strings(out)
	return out
}

// deviceIndex reads the trailing integer of a llama.cpp device name — `CUDA1` is
// 1, `ROCm0` is 0 — and returns -1 for anything with no trailing digits.
func deviceIndex(dev string) int {
	dev = strings.TrimSpace(dev)
	i := len(dev)
	for i > 0 && dev[i-1] >= '0' && dev[i-1] <= '9' {
		i--
	}
	if i == len(dev) {
		return -1
	}
	n, err := strconv.Atoi(dev[i:])
	if err != nil {
		return -1
	}
	return n
}
