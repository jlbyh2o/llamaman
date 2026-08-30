package instances

import (
	"sort"
	"strings"
)

// The environment contract of DESIGN section 5.7, beside the argv contract and
// for the same reason: `GET /instances/{id}/command` returns "this argv and env
// verbatim, so what the UI shows is what runs", which means the launcher and
// that endpoint must produce both from one function. A second implementation of
// the environment would be a second answer to the question the endpoint exists
// to answer truthfully.
//
// Like RenderArgv, this is PURE: it takes values and returns strings, reads no
// file, no clock and no hardware. Everything it needs — the primary hub
// directory and the GGML passthroughs — is a settings value the caller has
// already read.

// The four variables section 5.7 names, as constants because three components
// have to agree on the spelling: the launcher, the command endpoint and the
// tests that assert the contract.
const (
	// EnvHFHubCache is set ALWAYS, from the primary hub directory.
	EnvHFHubCache = "HF_HUB_CACHE"
	// EnvHFHome is set only when the hub directory is literally
	// `<something>/hub` — the projection section 7.2a calls a courtesy, and the
	// one that is meaningless for an `HF_HUB_CACHE`-style root.
	EnvHFHome = "HF_HOME"
	// EnvLlamaCache is explicitly UNSET: llama.cpp's own `-hf` cache is never
	// used (SPEC section 3.2), and an inherited value would give the process a
	// second model store this product does not manage.
	EnvLlamaCache = "LLAMA_CACHE"
	// EnvCUDAVisibleDevices is never set, and an inherited one is removed
	// (D66). Setting it beside `--device` is a silent wrong-GPU bug rather than
	// a crash: it RENUMBERS the devices llama.cpp sees, so a `--device CUDA1`
	// rendered from the user's second GPU addresses a different physical card
	// after masking, and `--main-gpu`/`--tensor-split` indices shift with it.
	EnvCUDAVisibleDevices = "CUDA_VISIBLE_DEVICES"
)

// GGMLSettingPrefix is the settings-key prefix whose rows are passed through as
// `GGML_*` environment variables: `ggml.foo_bar` becomes `GGML_FOO_BAR`.
//
// The v1 registry (section 2.1) defines no key under it, so the passthrough is
// empty on every stock install — which is the correct behavior, not a gap: the
// settings service rejects a key the registry does not declare, so the only way
// a row can exist here is a registry that grew one, and then it passes through
// without this function changing.
const GGMLSettingPrefix = "ggml."

// EnvInput is everything section 5.7's environment depends on.
type EnvInput struct {
	// HubDir is `settings['hf.hub_dir']`, the authoritative primary hub
	// directory (section 7.2a). Empty means the cache root has not been
	// resolved yet, in which case neither HF variable is set — inventing a path
	// would point the process at a directory nobody registered.
	HubDir string
	// GGML are the `GGML_*` passthroughs, already mapped from their settings
	// keys by GGMLEnvName.
	GGML map[string]string
}

// EnvSet is what section 5.7 SETS, as a map so a caller rendering
// `GET /instances/{id}/command` can show it as an object.
func EnvSet(in EnvInput) map[string]string {
	out := make(map[string]string, len(in.GGML)+2)
	for k, v := range in.GGML {
		out[k] = v
	}
	if in.HubDir != "" {
		out[EnvHFHubCache] = in.HubDir
		// "HF_HOME only when the hub directory ends in /hub" — rule 1 of
		// section 7.2 produces a root that does not, and projecting one anyway
		// would name a directory the cache layout does not use.
		if home, ok := hfHomeOf(in.HubDir); ok {
			out[EnvHFHome] = home
		}
	}
	return out
}

// EnvUnset is what section 5.7 REMOVES from the inherited environment.
func EnvUnset() []string {
	return []string{EnvLlamaCache, EnvCUDAVisibleDevices}
}

// Env applies both to a base environment — `os.Environ()` in the launcher — and
// returns the `KEY=VALUE` slice `execve` takes.
//
// The result is sorted, so two runs of the same configuration produce the same
// environment in the same order: an env that reordered itself would make the
// command endpoint's output churn and a golden test meaningless.
func Env(base []string, in EnvInput) []string {
	set := EnvSet(in)
	drop := make(map[string]struct{}, len(EnvUnset()))
	for _, k := range EnvUnset() {
		drop[k] = struct{}{}
	}

	out := make([]string, 0, len(base)+len(set))
	for _, kv := range base {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if _, dropped := drop[name]; dropped {
			continue
		}
		if _, replaced := set[name]; replaced {
			continue
		}
		out = append(out, kv)
	}
	for k, v := range set {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// GGMLEnvName maps a settings key under GGMLSettingPrefix to its environment
// variable name, and reports whether the key is one at all.
func GGMLEnvName(key string) (string, bool) {
	rest, ok := strings.CutPrefix(key, GGMLSettingPrefix)
	if !ok || rest == "" {
		return "", false
	}
	return "GGML_" + strings.ToUpper(strings.ReplaceAll(rest, ".", "_")), true
}

// hfHomeOf is section 7.2a's courtesy projection: the hub directory minus a
// trailing `/hub`, and nothing at all when it does not end in one.
func hfHomeOf(hubDir string) (string, bool) {
	trimmed := strings.TrimRight(hubDir, "/")
	home, ok := strings.CutSuffix(trimmed, "/hub")
	if !ok || home == "" {
		return "", false
	}
	return home, true
}
