package bench

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/jlbyh2o/llamaman/internal/hw"
	"github.com/jlbyh2o/llamaman/internal/model"
)

// The exclusivity guard (DESIGN section 10, SPEC section 3.5): "refuses to
// launch a bench while it would collide with a loaded instance on the same GPU
// (or offers to stop/restart around it)".
//
// # Why this needs per-GPU identity, and why it fails closed
//
// The preflight lists instances whose `instance_status.gpu_uuids_json`
// intersects the target GPUs. That column is populated from the
// `pid,gpu_uuid,used_gpu_memory` attribution query of section 8.6 — the
// `gpu_uuid` field is load-bearing, because without per-GPU identity the guard
// has no way to distinguish "loaded on the GPU you are about to benchmark" from
// "loaded on the other one", and SPEC section 3.5's promise cannot be kept on a
// multi-GPU host (D17).
//
// An instance whose `gpu_attribution` is `declared` or `unknown` is therefore
// treated as occupying EVERY GPU it could occupy — its
// `device_filter`/`tensor_split`/`main_gpu` set, else all present GPUs — so a
// bench is never launched into a collision merely because attribution was
// unavailable. Assumed says which instances were included on that basis, and the
// preflight response carries it, because "we stopped your instance on a guess"
// is a thing a user is entitled to see before it happens.
//
// # The inventory itself can be unavailable, and that is not "no GPUs"
//
// The same rule has to hold one level up. "`nvidia-smi` failed" and "this host
// has no GPU" produce the same empty slice, and reading the first as the second
// is precisely the fail-OPEN that F14 and D16 forbid: an empty inventory makes
// every target set empty, every intersection empty, and every conflict
// invisible, so a bench would launch straight into a loaded instance's VRAM the
// one time the driver hiccuped. GPUInventory therefore carries whether the
// enumeration ANSWERED, separately from what it answered, and Conflicts treats
// an unanswered inventory as "every loaded instance is a conflict" — the widest
// possible claim, which is the only honest one when identity is unknown.

// Occupancy is one instance's claim on the GPUs, as the guard reads it.
type Occupancy struct {
	InstanceID string
	Name       string
	State      model.InstanceState
	// GPUUUIDs is what this instance occupies. For a `measured` attribution it
	// is what the driver actually reported; otherwise it is the fail-closed
	// superset.
	GPUUUIDs []string
	// Attribution is how GPUUUIDs was obtained (section 2.8, section 8.6).
	Attribution model.GPUAttribution
	// Assumed reports that this instance was included on the fail-closed basis
	// rather than from a measurement.
	Assumed bool
}

// GPUInventory maps what the driver reports into the two lookups this guard
// needs: index → UUID, for resolving a `--device CUDA1` or a `--main-gpu 1`, and
// the full UUID set for the "else all present GPUs" fallback.
//
// The index IS `nvidia-smi`'s index, which is also llama.cpp's `CUDA<n>` and
// also `gpus.gpu_index`, and those three are one stable mapping precisely
// because the launcher never sets `CUDA_VISIBLE_DEVICES` (D66). This type is the
// place that assumption is written down.
type GPUInventory struct {
	byIndex map[int]string
	all     []string
	// known says the enumeration ANSWERED. An empty `all` with known=true is a
	// CPU-only host, which genuinely has nothing to be exclusive about; an empty
	// `all` with known=false is a driver this daemon could not reach, which has
	// everything to be exclusive about and no way to say which card.
	known bool
}

// NewGPUInventory builds the lookups from a SUCCESSFUL probe. Pass the probe's
// result only when the probe itself succeeded; use UnknownGPUInventory when it
// did not, because the two produce the same empty slice and mean opposite
// things.
func NewGPUInventory(gpus []hw.GPU) GPUInventory {
	inv := GPUInventory{byIndex: make(map[int]string, len(gpus))}
	for _, g := range gpus {
		if g.UUID == "" {
			continue
		}
		inv.byIndex[g.Index] = g.UUID
		inv.all = append(inv.all, g.UUID)
	}
	inv.all = sortedStrings(inv.all)
	// Cards reported with no UUID are cards with no identity: the guard cannot
	// tell "loaded on the one you are benchmarking" from "loaded on the other
	// one", which is the whole reason D17 makes `gpu_uuid` non-optional. A probe
	// that found devices but named none of them is therefore an UNKNOWN
	// inventory, not an empty one.
	inv.known = len(gpus) == 0 || len(inv.all) > 0
	return inv
}

// UnknownGPUInventory is the inventory of a host whose GPUs could not be
// enumerated at all — no prober wired, or `nvidia-smi` exited non-zero (F14).
// It resolves to no targets and makes every loaded instance a conflict.
func UnknownGPUInventory() GPUInventory {
	return GPUInventory{byIndex: map[int]string{}}
}

// All returns every GPU UUID this host reports, sorted.
func (inv GPUInventory) All() []string { return append([]string(nil), inv.all...) }

// Known reports whether this host could enumerate its GPUs at all. False means
// the guard has no identity to intersect and every conflict it reports is
// Assumed; it does NOT mean the host has no GPU — see NewGPUInventory.
func (inv GPUInventory) Known() bool { return inv.known }

// Resolve turns a FlagSet's device selection into the UUID set it addresses,
// falling back to every present GPU when the FlagSet names none.
//
// The three selectors are read in the order section 10 lists them, and they
// compose rather than override: `--device CUDA0,CUDA1` names two cards,
// `--tensor-split 0.6,0.4` names the first two, `--main-gpu 1` names the second.
// A selector naming an index this host does not have contributes nothing, which
// is the honest reading — the instance would not start on that device either.
func (inv GPUInventory) Resolve(flags model.FlagSet) []string {
	set := map[string]struct{}{}

	if flags.DeviceFilter != nil {
		for _, dev := range strings.Split(*flags.DeviceFilter, ",") {
			if uuid, ok := inv.byIndex[deviceIndex(dev)]; ok && deviceIndex(dev) >= 0 {
				set[uuid] = struct{}{}
			}
		}
	}
	for i, ratio := range flags.TensorSplit {
		// A zero ratio means "put nothing on this device", so it is not an
		// occupancy claim. A non-zero one is.
		if ratio <= 0 {
			continue
		}
		if uuid, ok := inv.byIndex[i]; ok {
			set[uuid] = struct{}{}
		}
	}
	if flags.MainGPU != nil {
		if uuid, ok := inv.byIndex[*flags.MainGPU]; ok {
			set[uuid] = struct{}{}
		}
	}

	if len(set) == 0 {
		return inv.All()
	}
	out := make([]string, 0, len(set))
	for uuid := range set {
		out = append(out, uuid)
	}
	return sortedStrings(out)
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

// OccupancyOf reads one instance's claim on the GPUs, fail-closed.
func OccupancyOf(v model.InstanceView, inv GPUInventory) Occupancy {
	occ := Occupancy{
		InstanceID:  v.ID,
		Name:        v.Name,
		State:       v.Status.State,
		Attribution: v.Status.GPUAttribution,
	}

	if v.Status.GPUAttribution == model.AttributionMeasured && v.Status.GPUUUIDsJSON != nil {
		var uuids []string
		if err := json.Unmarshal([]byte(*v.Status.GPUUUIDsJSON), &uuids); err == nil {
			occ.GPUUUIDs = sortedStrings(uuids)
			return occ
		}
		// A `measured` attribution whose JSON will not parse is not a
		// measurement. Falling through to the superset is the fail-closed
		// answer, and Assumed below is what says so.
	}

	// `declared` or `unknown`, or a measurement that could not be read: every
	// GPU this instance could occupy.
	occ.Assumed = true
	flags, err := model.ParseFlagSet([]byte(v.FlagsJSON))
	if err != nil {
		// A configuration this daemon cannot read is one whose device selection
		// is unknown, which is the widest possible claim.
		occ.GPUUUIDs = inv.All()
		return occ
	}
	occ.GPUUUIDs = inv.Resolve(flags)
	return occ
}

// Loaded reports whether an instance is one that could be holding VRAM right
// now. A stopped or failed instance occupies nothing, and refusing a benchmark
// because of one would make the guard useless on exactly the host it matters
// most on — the one whose instances are usually up.
func Loaded(s model.InstanceState) bool {
	switch s {
	case model.InstanceReady, model.InstanceDegraded, model.InstanceLoading,
		model.InstanceStarting:
		return true
	}
	return false
}

// Conflicts returns the loaded instances whose GPU claim intersects target, in
// creation order.
//
// When the inventory could not be enumerated there is no intersection to take:
// the target set is empty, every occupancy set is empty, and an intersection
// test would answer "no conflicts" for a host that is entirely loaded. Section
// 10 forbids exactly that answer — "a bench is never launched into a collision
// merely because attribution was unavailable" — so every loaded instance is
// returned, marked Assumed, and the caller's `on_conflict` policy decides what
// to do about it.
func Conflicts(target []string, views []model.InstanceView, inv GPUInventory) []Occupancy {
	want := make(map[string]struct{}, len(target))
	for _, uuid := range target {
		want[uuid] = struct{}{}
	}

	var out []Occupancy
	for _, v := range views {
		if v.Deleted() || !Loaded(v.Status.State) {
			continue
		}
		occ := OccupancyOf(v, inv)
		if !inv.Known() {
			occ.Assumed = true
			out = append(out, occ)
			continue
		}
		for _, uuid := range occ.GPUUUIDs {
			if _, hit := want[uuid]; hit {
				out = append(out, occ)
				break
			}
		}
	}
	return out
}

// ConflictDetails renders the conflicting instances into the `details` object
// section 3.13's `409 bench_gpu_conflict` carries.
func ConflictDetails(conflicts []Occupancy) map[string]any {
	items := make([]map[string]any, 0, len(conflicts))
	for _, c := range conflicts {
		items = append(items, map[string]any{
			"instance_id": c.InstanceID,
			"name":        c.Name,
			"state":       string(c.State),
			"gpu_uuids":   c.GPUUUIDs,
			"attribution": string(c.Attribution),
			"assumed":     c.Assumed,
		})
	}
	return map[string]any{"instances": items}
}
