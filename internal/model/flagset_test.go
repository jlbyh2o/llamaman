package model

import (
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// FlagSet tests (DESIGN section 15: "the safe-start override patch — that it
// changes the rendered argv, that effective_config_hash != config_hash, and
// that flags_json, config_hash and generation are all untouched by it (D61)").
//
// The first two of those three are asserted in internal/instances, where the
// renderer and the hash live. What belongs HERE is the third: that patching a
// FlagSet cannot reach back into the value it was patched from.

func TestParseFlagSet(t *testing.T) {
	// The document DESIGN section 2.8 prints, with the explicit nulls it shows.
	const doc = `{
	  "ctx_size": 8192,
	  "n_gpu_layers": {"mode":"all"},
	  "batch_size": 2048,
	  "ubatch_size": 512,
	  "parallel": 4,
	  "threads": null,
	  "threads_batch": null,
	  "flash_attn": "auto",
	  "cache_type_k": "f16",
	  "cache_type_v": "f16",
	  "split_mode": "layer",
	  "tensor_split": [0.5, 0.5],
	  "main_gpu": 0,
	  "device_filter": "CUDA0,CUDA1",
	  "device_uuids": ["GPU-a1","GPU-b2"],
	  "mlock": false, "no_mmap": false,
	  "cont_batching": true,
	  "embedding": false,
	  "pooling": null,
	  "rerank": false,
	  "alias": "qwen3-8b",
	  "chat_template": null, "chat_template_file": null, "jinja": true,
	  "rope_scaling": null, "rope_freq_base": null, "rope_freq_scale": null,
	  "yarn_ext_factor": null, "yarn_attn_factor": null,
	  "n_keep": null, "n_predict": null, "defrag_thold": null, "cache_reuse": null,
	  "numa": null, "cpu_mask": null, "prio": null,
	  "slot_save_path": null,
	  "draft": {"n_max":16,"n_min":0,"p_min":0.75,"ctx_size":null,"n_gpu_layers":null},
	  "props_endpoint": true, "slots_endpoint": true, "metrics_endpoint": true,
	  "log_verbosity": null
	}`

	f, err := ParseFlagSet([]byte(doc))
	if err != nil {
		t.Fatalf("the document section 2.8 prints does not parse: %v", err)
	}

	// A null is "do not pass the flag", which is a nil pointer and not a zero.
	if f.Threads != nil {
		t.Error(`"threads": null produced a value; null must stay nil`)
	}
	// A present false is NOT a null: `mlock: false` is a decision.
	if f.Mlock == nil || *f.Mlock {
		t.Error(`"mlock": false must parse as a present false, not as absent`)
	}
	if f.NGpuLayers == nil || f.NGpuLayers.Mode != NGLAll {
		t.Errorf("n_gpu_layers = %+v, want mode=all", f.NGpuLayers)
	}
	if f.Draft == nil || f.Draft.NMax == nil || *f.Draft.NMax != 16 {
		t.Errorf("draft = %+v, want n_max=16", f.Draft)
	}
	if diff := cmp.Diff([]float64{0.5, 0.5}, f.TensorSplit); diff != "" {
		t.Errorf("tensor_split mismatch (-want +got):\n%s", diff)
	}
}

// TestParseFlagSetRejectsUnknownFields: a key nothing renders is either a typo
// or a flag from a newer schema. Dropping it silently would make the saved
// configuration and the running configuration disagree with no way to see it —
// and this is exactly what `instance-exec` turns into exit 65.
func TestParseFlagSetRejectsUnknownFields(t *testing.T) {
	if _, err := ParseFlagSet([]byte(`{"ctx_sizee":4096}`)); err == nil {
		t.Fatal("a misspelled key was accepted")
	}
	if _, err := ParseFlagSet(nil); err != nil {
		t.Errorf("an empty flags_json is the empty FlagSet, not an error: %v", err)
	}
}

// TestApplyOverrideIsAShallowPatch is section 3.10b step 2: "present keys
// replace, absent keys are untouched".
func TestApplyOverrideIsAShallowPatch(t *testing.T) {
	saved := FlagSet{
		CtxSize:    ptrTo(8192),
		NGpuLayers: &NGpuLayers{Mode: NGLAll},
		Parallel:   ptrTo(4),
		Alias:      ptrTo("qwen3-8b"),
		FlashAttn:  ptrTo(FlashAttnOn),
	}

	// The safe-start patch DESIGN section 3.10b step 1 writes, verbatim.
	patched, err := saved.ApplyOverride(
		[]byte(`{"n_gpu_layers":{"mode":"none"},"ctx_size":2048,"parallel":1}`))
	if err != nil {
		t.Fatalf("ApplyOverride: %v", err)
	}

	if got := *patched.CtxSize; got != 2048 {
		t.Errorf("ctx_size = %d, want the override's 2048", got)
	}
	if got := patched.NGpuLayers.Mode; got != NGLNone {
		t.Errorf("n_gpu_layers.mode = %q, want the override's %q", got, NGLNone)
	}
	if got := *patched.Parallel; got != 1 {
		t.Errorf("parallel = %d, want the override's 1", got)
	}
	// Absent keys are untouched: the safe start still serves under the saved
	// alias and the saved attention setting.
	if got := *patched.Alias; got != "qwen3-8b" {
		t.Errorf("alias = %q, want the saved value — the patch did not mention it", got)
	}
	if got := *patched.FlashAttn; got != FlashAttnOn {
		t.Errorf("flash_attn = %q, want the saved value", got)
	}
}

// TestApplyOverrideNeverTouchesTheSavedFlagSet is D61's "never persisted" from
// the one angle a struct can get wrong: the receiver's pointer fields are shared
// structure, so patching a shallow copy in place would write THROUGH them into
// the saved configuration.
func TestApplyOverrideNeverTouchesTheSavedFlagSet(t *testing.T) {
	saved := FlagSet{
		CtxSize:    ptrTo(8192),
		NGpuLayers: &NGpuLayers{Mode: NGLCount, Count: ptrTo(37)},
		Draft:      &DraftFlags{NMax: ptrTo(16)},
	}
	before, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := saved.ApplyOverride(
		[]byte(`{"n_gpu_layers":{"mode":"none"},"ctx_size":2048,"draft":{"n_max":2}}`)); err != nil {
		t.Fatalf("ApplyOverride: %v", err)
	}

	after, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("applying an override mutated the saved FlagSet:\nbefore %s\nafter  %s",
			before, after)
	}
}

// TestApplyOverrideRejectsUnknownKeys: the only producer of an override today is
// safe-start, so an unrecognized key is our own bug — worth failing the start
// over rather than launching a configuration nobody asked for.
func TestApplyOverrideRejectsUnknownKeys(t *testing.T) {
	if _, err := (FlagSet{}).ApplyOverride([]byte(`{"ngl":0}`)); err == nil {
		t.Fatal("an unknown override key was accepted")
	}
	for _, empty := range []string{"", "null", "  "} {
		if _, err := (FlagSet{CtxSize: ptrTo(1)}).ApplyOverride([]byte(empty)); err != nil {
			t.Errorf("an empty override (%q) should be a plain deep copy: %v", empty, err)
		}
	}
}

// TestCloneIsDeep guards the same aliasing hazard for `pin-ngl`, which edits one
// field of a stored FlagSet.
func TestCloneIsDeep(t *testing.T) {
	original := FlagSet{NGpuLayers: &NGpuLayers{Mode: NGLAuto}}
	clone := original.Clone()
	clone.NGpuLayers.Mode = NGLCount
	clone.NGpuLayers.Count = ptrTo(37)

	if original.NGpuLayers.Mode != NGLAuto {
		t.Error("editing a clone reached back into the original")
	}
}

func TestHelpFlags(t *testing.T) {
	var absent HelpFlags
	if absent.Available() {
		t.Error("a NULL help_flags_json is not an available capture")
	}
	present := HelpFlags{"--jinja", "-ngl"}
	if !present.Available() {
		t.Error("a non-empty capture is available")
	}
	if !present.Has("-ngl") || present.Has("--nope") {
		t.Error("Has does not answer membership")
	}
}
