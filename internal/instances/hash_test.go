package instances

import (
	"testing"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// config_hash tests (DESIGN section 15: "`config_hash` stability under JSON key
// reordering AND under a changed `internal_port` (D52)").

func hashOf(t *testing.T, inst model.Instance, flags model.FlagSet, version Runtime) string {
	t.Helper()
	h, err := ConfigHashFor(inst, flags, qwenModel(), nil, nil, version)
	if err != nil {
		t.Fatalf("ConfigHashFor: %v", err)
	}
	return h
}

// TestConfigHashIgnoresJSONKeyOrder: `flags_json` is a document, and two
// documents that mean the same thing must hash the same. They do here by
// construction — the hash is over the RENDERED ARGV, not over the JSON — and
// this test is what keeps that construction from being replaced by something
// that hashes the column.
func TestConfigHashIgnoresJSONKeyOrder(t *testing.T) {
	a, err := model.ParseFlagSet([]byte(`{"ctx_size":8192,"parallel":4,"flash_attn":"on"}`))
	if err != nil {
		t.Fatalf("ParseFlagSet: %v", err)
	}
	b, err := model.ParseFlagSet([]byte(`{"flash_attn":"on","ctx_size":8192,"parallel":4}`))
	if err != nil {
		t.Fatalf("ParseFlagSet: %v", err)
	}

	if got, want := hashOf(t, baseInstance(), b, cudaRuntime()), hashOf(t, baseInstance(), a, cudaRuntime()); got != want {
		t.Errorf("reordering the JSON keys moved config_hash:\n got %s\nwant %s", got, want)
	}
}

// TestConfigHashIgnoresTheInternalPort is D52's central claim. The supervisor
// may reassign the internal port after an exit 78 (F5) with no user action, and
// a hash that moved with it would raise `restart_required` on an instance whose
// configuration nobody touched.
func TestConfigHashIgnoresTheInternalPort(t *testing.T) {
	flags := docExampleFlags()

	before := hashOf(t, baseInstance(), flags, cudaRuntime())

	reassigned := baseInstance()
	reassigned.InternalPort = 21099
	after := hashOf(t, reassigned, flags, cudaRuntime())

	if before != after {
		t.Errorf("reassigning the internal port moved config_hash:\n%s\n%s", before, after)
	}
}

// TestConfigHashMovesWithItsThreeInputs: everything a user can actually change
// IS in the hash, and so are the two inputs D69's maintainers move.
func TestConfigHashMovesWithItsThreeInputs(t *testing.T) {
	base := hashOf(t, baseInstance(), docExampleFlags(), cudaRuntime())

	t.Run("a flag", func(t *testing.T) {
		flags := docExampleFlags()
		flags.CtxSize = ptr(4096)
		if got := hashOf(t, baseInstance(), flags, cudaRuntime()); got == base {
			t.Error("changing ctx_size left config_hash alone")
		}
	})

	t.Run("extra_flags", func(t *testing.T) {
		inst := baseInstance()
		inst.ExtraFlags = "--log-colors"
		if got := hashOf(t, inst, docExampleFlags(), cudaRuntime()); got == base {
			t.Error("changing extra_flags left config_hash alone")
		}
	})

	t.Run("the resolved model path", func(t *testing.T) {
		other := &ModelFile{ID: "m-qwen", Path: "/somewhere/else/Qwen3-8B-Q4_K_M.gguf"}
		got, err := ConfigHashFor(baseInstance(), docExampleFlags(), other, nil, nil, cudaRuntime())
		if err != nil {
			t.Fatalf("ConfigHashFor: %v", err)
		}
		if got == base {
			t.Error("re-pointing the model left config_hash alone — D69's models-service " +
				"recomputation would then be a no-op")
		}
	})

	t.Run("the active version id", func(t *testing.T) {
		next := cudaRuntime()
		next.ID = "b10700-cuda-src"
		if got := hashOf(t, baseInstance(), docExampleFlags(), next); got == base {
			t.Error("activating a new llama.cpp version left config_hash alone — " +
				"restart_required would never fire after a version flip (D69)")
		}
	})

	t.Run("an mmproj that was not there before", func(t *testing.T) {
		mmproj := &ModelFile{ID: "m-mmproj", Path: "/cache/mmproj-f16.gguf"}
		got, err := ConfigHashFor(baseInstance(), docExampleFlags(), qwenModel(), mmproj, nil, cudaRuntime())
		if err != nil {
			t.Fatalf("ConfigHashFor: %v", err)
		}
		if got == base {
			t.Error("adding an mmproj left config_hash alone")
		}
	})
}

// TestElideListener pins exactly what D52 removes: `--host` with its value, and
// the VALUE of `--port` — the marker stays.
func TestElideListener(t *testing.T) {
	argv := []string{"/v/bin/llama-server", "-m", "/m.gguf", "--host", "127.0.0.1",
		"--port", "21001", "--no-webui"}
	got := elideListener(argv)
	want := []string{"/v/bin/llama-server", "-m", "/m.gguf", "--port", "--no-webui"}

	if len(got) != len(want) {
		t.Fatalf("elideListener = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("elideListener = %v, want %v", got, want)
		}
	}
}

// TestSafeStartOverrideMovesTheEffectiveHash is D61 from the renderer's side:
// the same instance rendered with the safe-start patch produces a DIFFERENT
// hash, which is what `instance_starts.effective_config_hash` records and what
// makes `restart_required` true for as long as the safe start is the running
// configuration.
func TestSafeStartOverrideMovesTheEffectiveHash(t *testing.T) {
	saved := docExampleFlags()
	stored := hashOf(t, baseInstance(), saved, cudaRuntime())

	patched, err := saved.ApplyOverride([]byte(
		`{"n_gpu_layers":{"mode":"none"},"ctx_size":2048,"parallel":1}`))
	if err != nil {
		t.Fatalf("ApplyOverride: %v", err)
	}
	effective := hashOf(t, baseInstance(), patched, cudaRuntime())

	if effective == stored {
		t.Fatal("the safe-start override did not change the effective hash")
	}
	// And the saved configuration is untouched: ApplyOverride deep-copies, so
	// the value the caller read from the row still hashes to what it did.
	if again := hashOf(t, baseInstance(), saved, cudaRuntime()); again != stored {
		t.Error("applying an override reached back into the saved FlagSet")
	}
}
