package instances

import (
	"reflect"
	"testing"
)

// DESIGN section 5.7's environment contract, which is as much about what is
// absent as about what is set.

func TestEnvSet(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   EnvInput
		want map[string]string
	}{
		{
			name: "a hub directory that ends in /hub gets both variables",
			in:   EnvInput{HubDir: "/home/u/.cache/huggingface/hub"},
			want: map[string]string{
				"HF_HUB_CACHE": "/home/u/.cache/huggingface/hub",
				"HF_HOME":      "/home/u/.cache/huggingface",
			},
		},
		{
			// Rule 1 of section 7.2 takes $HF_HUB_CACHE verbatim, with no
			// `/hub` appended, so the HF_HOME projection is meaningless and
			// section 5.7 says not to make it.
			name: "an HF_HUB_CACHE-style root gets HF_HUB_CACHE only",
			in:   EnvInput{HubDir: "/mnt/models"},
			want: map[string]string{"HF_HUB_CACHE": "/mnt/models"},
		},
		{
			name: "a trailing slash does not defeat the /hub suffix",
			in:   EnvInput{HubDir: "/srv/hf/hub/"},
			want: map[string]string{
				"HF_HUB_CACHE": "/srv/hf/hub/",
				"HF_HOME":      "/srv/hf",
			},
		},
		{
			// The cache root has not been resolved yet. Inventing a path would
			// point llama.cpp at a directory nobody registered.
			name: "no hub directory sets neither variable",
			in:   EnvInput{},
			want: map[string]string{},
		},
		{
			name: "GGML passthroughs are carried verbatim",
			in: EnvInput{
				HubDir: "/mnt/models",
				GGML:   map[string]string{"GGML_CUDA_ENABLE_UNIFIED_MEMORY": "1"},
			},
			want: map[string]string{
				"HF_HUB_CACHE":                    "/mnt/models",
				"GGML_CUDA_ENABLE_UNIFIED_MEMORY": "1",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := EnvSet(tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("EnvSet() = %v, want %v", got, tt.want)
			}
			if _, set := got["CUDA_VISIBLE_DEVICES"]; set {
				t.Error("CUDA_VISIBLE_DEVICES is set; D66 says the launcher sets no device environment at all")
			}
			if _, set := got["LLAMA_CACHE"]; set {
				t.Error("LLAMA_CACHE is set; section 5.7 says it is explicitly unset")
			}
		})
	}
}

// TestEnvAppliesOverABaseEnvironment is the launcher's own case: an inherited
// environment, two variables removed from it and the cache pointed at the root
// this daemon manages.
func TestEnvAppliesOverABaseEnvironment(t *testing.T) {
	t.Parallel()

	base := []string{
		"PATH=/usr/bin",
		"LLAMA_CACHE=/tmp/llama",       // must not survive (SPEC section 3.2)
		"CUDA_VISIBLE_DEVICES=1",       // must not survive (D66)
		"HF_HUB_CACHE=/somewhere/else", // must be replaced, not duplicated
		"malformed-with-no-equals",     // dropped rather than passed on
	}
	got := Env(base, EnvInput{HubDir: "/cache/hub"})

	want := []string{
		"HF_HOME=/cache",
		"HF_HUB_CACHE=/cache/hub",
		"PATH=/usr/bin",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Env() = %v, want %v", got, want)
	}
}

func TestGGMLEnvName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		key  string
		want string
		ok   bool
	}{
		{key: "ggml.cuda_enable_unified_memory", want: "GGML_CUDA_ENABLE_UNIFIED_MEMORY", ok: true},
		{key: "ggml.cuda.force_mmq", want: "GGML_CUDA_FORCE_MMQ", ok: true},
		{key: "ggml.", want: "", ok: false},
		{key: "hf.hub_dir", want: "", ok: false},
		{key: "", want: "", ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			t.Parallel()
			got, ok := GGMLEnvName(tt.key)
			if got != tt.want || ok != tt.ok {
				t.Errorf("GGMLEnvName(%q) = (%q, %v), want (%q, %v)", tt.key, got, ok, tt.want, tt.ok)
			}
		})
	}
}
