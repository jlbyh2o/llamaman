package settings

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
)

// Kind is the closed set of value types a setting may hold — the "type"
// column of DESIGN section 2.1's settings table, plus `enum`, which that same
// table also uses (`ui.theme`, `llamacpp.channel`, `update.channel`,
// `log.level`).
type Kind string

const (
	KindInt    Kind = "int"
	KindBool   Kind = "bool"
	KindString Kind = "string"
	KindEnum   Kind = "enum"
)

// Definition is one entry of the settings registry: a key, its type, its
// built-in default, and the rules a candidate value must pass before it is
// stored (DESIGN section 1's package description: "typed registry (key, type,
// default, validator)").
type Definition struct {
	// Key is the dotted settings key, e.g. "ui.port_desired".
	Key string
	// Kind is this key's value type.
	Kind Kind
	// Default is the built-in value used whenever no override row exists,
	// typed to match Kind (int64 / bool / string).
	Default any

	// Enum lists the allowed values for Kind == KindEnum. Unused otherwise.
	Enum []string
	// Min and Max bound a KindInt value; either may be nil for "unbounded on
	// that side". Unused for other kinds.
	Min, Max *int64

	// Group is the settings-table section this key belongs to — the part of
	// Key before its first '.' — filled in by the registry at registration
	// time so callers building the UI's grouped forms (DESIGN section 4,
	// "General, Network & Ports, Hugging Face, Builds, …") never have to
	// parse the key themselves.
	Group string
	// RestartRequired mirrors DESIGN section 3.4: the running daemon holds
	// the old value until `POST /system/restart` swaps it in. Named there for
	// exactly four keys: ui.port_desired, ui.bind, gateway.bind, log.level.
	RestartRequired bool

	// Extra is an additional, key-specific check run after the structural
	// Kind/Min/Max/Enum checks pass — e.g. that `ui.bind` parses as an IP
	// address, or that `hf.endpoint` is an absolute http(s) URL. Nil means no
	// extra check.
	Extra func(v any) error
}

// Validate reports whether v — a value already of the Go type Kind implies
// (int64, bool or string) — satisfies this definition: the type itself (as a
// defense against a caller that bypassed decode), the Min/Max bounds for
// KindInt, enum membership for KindEnum, and finally Extra.
func (d Definition) Validate(v any) error {
	switch d.Kind {
	case KindInt:
		n, ok := v.(int64)
		if !ok {
			return fmt.Errorf("%s: want an integer, got %T", d.Key, v)
		}
		if d.Min != nil && n < *d.Min {
			return fmt.Errorf("%s: %d is below the minimum %d", d.Key, n, *d.Min)
		}
		if d.Max != nil && n > *d.Max {
			return fmt.Errorf("%s: %d is above the maximum %d", d.Key, n, *d.Max)
		}
	case KindBool:
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("%s: want a boolean, got %T", d.Key, v)
		}
	case KindString:
		if _, ok := v.(string); !ok {
			return fmt.Errorf("%s: want a string, got %T", d.Key, v)
		}
	case KindEnum:
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("%s: want a string, got %T", d.Key, v)
		}
		if !containsString(d.Enum, s) {
			return fmt.Errorf("%s: %q is not one of %s", d.Key, s, strings.Join(d.Enum, ", "))
		}
	default:
		return fmt.Errorf("%s: unregistered kind %q", d.Key, d.Kind)
	}
	if d.Extra != nil {
		if err := d.Extra(v); err != nil {
			return fmt.Errorf("%s: %w", d.Key, err)
		}
	}
	return nil
}

// decode unmarshals raw — a `settings.value` JSON payload — into the Go type
// Kind implies, without validating it. Every settings.value the store returns
// is JSON by the column's own CHECK, so a decode failure here means the row
// was written by a definition of a different Kind, which is a registry bug
// (a key reused with a new type) rather than a value a user typed.
func (d Definition) decode(raw json.RawMessage) (any, error) {
	switch d.Kind {
	case KindInt:
		var n int64
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, fmt.Errorf("%s: not a JSON integer: %w", d.Key, err)
		}
		return n, nil
	case KindBool:
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, fmt.Errorf("%s: not a JSON boolean: %w", d.Key, err)
		}
		return b, nil
	case KindString, KindEnum:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("%s: not a JSON string: %w", d.Key, err)
		}
		return s, nil
	default:
		return nil, fmt.Errorf("%s: unregistered kind %q", d.Key, d.Kind)
	}
}

func containsString(vs []string, v string) bool {
	for _, m := range vs {
		if m == v {
			return true
		}
	}
	return false
}

func groupOf(key string) string {
	if i := strings.IndexByte(key, '.'); i >= 0 {
		return key[:i]
	}
	return key
}

func intPtr(v int64) *int64 { return &v }

// Registry is the closed, immutable set of every settings key DESIGN section
// 2.1 names. It holds no store reference and does no I/O — Cache (settings.go)
// is what layers a read-through cache and store-backed persistence over it —
// which is what makes type safety, validator rejection and default values
// testable without a database.
type Registry struct {
	defs map[string]Definition
	keys []string // insertion order; Keys()/Definitions() sort it
}

// NewRegistry builds the registry DESIGN section 2.1 specifies. It panics on
// a duplicate key, which would be a bug in this file, not in caller input —
// the panic is what the D49-style import-graph tests in this package's own
// test file turn into a normal test failure via TestNewRegistry.
func NewRegistry() *Registry {
	r := &Registry{defs: make(map[string]Definition, len(definitions))}
	for _, d := range definitions {
		if d.Group == "" {
			d.Group = groupOf(d.Key)
		}
		if _, exists := r.defs[d.Key]; exists {
			panic("settings: duplicate key " + d.Key)
		}
		r.defs[d.Key] = d
		r.keys = append(r.keys, d.Key)
	}
	return r
}

// Lookup returns the definition for key and whether it is registered.
func (r *Registry) Lookup(key string) (Definition, bool) {
	d, ok := r.defs[key]
	return d, ok
}

// Keys returns every registered key, sorted.
func (r *Registry) Keys() []string {
	out := append([]string(nil), r.keys...)
	sort.Strings(out)
	return out
}

// Definitions returns every definition, ordered by key — what a `GET
// /api/v1/settings` handler walks to build the `schema` array (DESIGN section
// 3.4).
func (r *Registry) Definitions() []Definition {
	keys := r.Keys()
	out := make([]Definition, len(keys))
	for i, k := range keys {
		out[i] = r.defs[k]
	}
	return out
}

// validIPAddress is the Extra check for the two bind-address settings.
func validIPAddress(v any) error {
	s := v.(string) //nolint:forcetypeassert // Kind==KindString already checked
	if net.ParseIP(s) == nil {
		return fmt.Errorf("%q is not a valid IP address", s)
	}
	return nil
}

// validHTTPURL is the Extra check for hf.endpoint: an absolute http(s) URL.
func validHTTPURL(v any) error {
	s := v.(string) //nolint:forcetypeassert // Kind==KindString already checked
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("%q is not a valid URL: %w", s, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%q must be an http or https URL", s)
	}
	if u.Host == "" {
		return fmt.Errorf("%q has no host", s)
	}
	return nil
}

// KeyHFHubDir is `settings['hf.hub_dir']`, the authoritative primary hub
// directory of section 7.2a.
const KeyHFHubDir = "hf.hub_dir"

// definitions is every key DESIGN section 2.1 names, in the order that table
// lists them. Each comment cites the table row it transcribes.
var definitions = []Definition{
	// --- ui.* --------------------------------------------------------------
	{
		// Seeded once from `serve --port N` when the row is absent (§11.1
		// step 6b); thereafter the stored value always wins. 1024-65535
		// matches the floor the schema itself enforces on `public_port` /
		// `internal_port` (§2.4's CHECK) — a management UI has no business on
		// a privileged port that needs root to bind.
		Key: "ui.port_desired", Kind: KindInt, Default: int64(5526),
		Min: intPtr(1024), Max: intPtr(65535), RestartRequired: true,
	},
	{
		Key: "ui.bind", Kind: KindString, Default: "0.0.0.0",
		Extra: validIPAddress, RestartRequired: true,
	},
	{
		Key: "ui.theme", Kind: KindEnum, Default: "dark",
		Enum: []string{"dark", "light", "system"},
	},

	// --- security.* ----------------------------------------------------------
	{Key: "security.session_ttl_hours", Kind: KindInt, Default: int64(720), Min: intPtr(1)},
	{Key: "security.idle_timeout_hours", Kind: KindInt, Default: int64(168), Min: intPtr(1)},
	{Key: "security.login_max_attempts", Kind: KindInt, Default: int64(8), Min: intPtr(1)},
	{Key: "security.login_window_sec", Kind: KindInt, Default: int64(300), Min: intPtr(1)},
	{Key: "security.lockout_sec", Kind: KindInt, Default: int64(900), Min: intPtr(1)},

	// --- hf.* ----------------------------------------------------------------
	//
	// KeyHFHubDir is spelled as a constant because a second reader of it exists
	// outside this package and outside the daemon process: `instance-exec`
	// builds section 5.7's `HF_HUB_CACHE`/`HF_HOME` from this exact row, with
	// no settings cache and no HTTP. A literal in both places is two spellings
	// of one authority.
	{
		Key: "hf.endpoint", Kind: KindString, Default: "https://huggingface.co",
		Extra: validHTTPURL,
	},
	{
		// The real default is resolved by the §7.2 six-rule chain
		// ($HF_HUB_CACHE first, legacy variables next, then $HF_HOME/hub, the
		// dedicated-user path, $XDG_CACHE_HOME, the built-in fallback) and
		// written by the boot probe (§11.1 step 6) before this key is ever
		// read in production. This registry's own Default ("") is the value
		// Get would return on a database no boot step has touched yet — a
		// fresh test database, not a running daemon. Writing this key is
		// accepted here (any non-empty string decodes and passes the generic
		// String check), but §7.2a names `cache.SetPrimaryRoot` — owned by
		// internal/hf/cache — as the one path that ALSO validates the
		// filesystem (absolute, creatable, writable, symlink probe) and keeps
		// `hf_cache_roots`, this key, `hf.home` and `runtime_info` in
		// agreement; a caller changing the primary cache root must go through
		// it rather than through Cache.Set directly.
		Key: KeyHFHubDir, Kind: KindString, Default: "",
	},
	{
		// Courtesy projection of hf.hub_dir (§7.2a): `hub_dir` minus a
		// trailing "/hub", else "". Writing it is translated to
		// `SetPrimaryRoot(value + "/hub")` by that same service method, not
		// by this registry — see hf.hub_dir above.
		Key: "hf.home", Kind: KindString, Default: "",
	},
	{Key: "hf.download_concurrency", Kind: KindInt, Default: int64(3), Min: intPtr(1)},
	{
		// 0 = unlimited (§2.1).
		Key: "hf.rate_limit_bytes_sec", Kind: KindInt, Default: int64(0), Min: intPtr(0),
	},
	{Key: "hf.verify_checksums", Kind: KindBool, Default: true},

	// --- llamacpp.* ------------------------------------------------------------
	{
		Key: "llamacpp.channel", Kind: KindEnum, Default: "stable",
		Enum: []string{"stable", "nightly", "custom"},
	},
	{
		// 0 = auto, meaning min(NumCPU, max(2, MemAvailableGiB/2)) (§2.1);
		// the arithmetic is internal/llamacpp/source's, not this package's.
		Key: "llamacpp.build_jobs", Kind: KindInt, Default: int64(0), Min: intPtr(0),
	},
	{
		// "" = auto-detect (D21). Free-form otherwise (a `;`-separated CUDA
		// compute-capability list, e.g. "89;86") — validated by the build
		// pipeline that consumes it, not here.
		Key: "llamacpp.cuda_arch_list", Kind: KindString, Default: "",
	},
	{Key: "llamacpp.prefer_prebuilt_cpu", Kind: KindBool, Default: true},
	{
		// Free-form extra cmake flags, passed through verbatim (§6.4); no
		// syntax this package can usefully police.
		Key: "llamacpp.extra_cmake_flags", Kind: KindString, Default: "",
	},
	{
		// A BOOL, not a depth: SPEC §5.6 keeps exactly the previous build,
		// never N (§2.1, §6.6).
		Key: "llamacpp.keep_previous", Kind: KindBool, Default: true,
	},

	// --- instances.* -----------------------------------------------------------
	{
		// Reserved pool floor/ceiling (§2.8): no `public_port` may fall
		// inside [min, max]. Bounded the same way the schema bounds a port
		// column (§2.4's CHECK, 1024-65535); min < max is a cross-key
		// invariant the API layer enforces on a combined PATCH, not
		// something one key's validator alone can express.
		Key: "instances.internal_port_min", Kind: KindInt, Default: int64(21000),
		Min: intPtr(1024), Max: intPtr(65535),
	},
	{
		Key: "instances.internal_port_max", Kind: KindInt, Default: int64(21999),
		Min: intPtr(1024), Max: intPtr(65535),
	},
	{Key: "instances.health_poll_sec", Kind: KindInt, Default: int64(5), Min: intPtr(1)},
	{Key: "instances.start_timeout_sec", Kind: KindInt, Default: int64(900), Min: intPtr(1)},

	// --- gateway.* --------------------------------------------------------------
	{
		Key: "gateway.bind", Kind: KindString, Default: "0.0.0.0",
		Extra: validIPAddress, RestartRequired: true,
	},
	{
		// 0 = never cap a generation (§2.1, §9.2).
		Key: "gateway.request_timeout_sec", Kind: KindInt, Default: int64(0), Min: intPtr(0),
	},
	{Key: "gateway.idle_timeout_sec", Kind: KindInt, Default: int64(300), Min: intPtr(1)},
	{
		// Request bodies only; responses are never buffered (§2.1).
		Key: "gateway.max_body_mb", Kind: KindInt, Default: int64(256), Min: intPtr(1),
	},
	{
		// 0 disables usage parsing entirely (§9.3).
		Key: "gateway.usage_parse_kb", Kind: KindInt, Default: int64(64), Min: intPtr(0),
	},
	{
		// How long a restart drains in-flight proxied requests (§9.4). 0 is a
		// legitimate "close immediately, drain nothing".
		Key: "gateway.drain_sec", Kind: KindInt, Default: int64(20), Min: intPtr(0),
	},

	// --- gpu.* --------------------------------------------------------------
	{Key: "gpu.poll_active_sec", Kind: KindInt, Default: int64(2), Min: intPtr(1)},
	{Key: "gpu.poll_idle_sec", Kind: KindInt, Default: int64(30), Min: intPtr(1)},

	// --- fit.* --------------------------------------------------------------
	{
		// Per participating GPU, matching llama.cpp's own `--fit-target`
		// (§8.3). 0 is a legitimate "no margin".
		Key: "fit.margin_mib", Kind: KindInt, Default: int64(1024), Min: intPtr(0),
	},
	{Key: "fit.use_calibration", Kind: KindBool, Default: true},

	// --- bench.* --------------------------------------------------------------
	{Key: "bench.exclusive_gpu", Kind: KindBool, Default: true},
	{Key: "bench.default_repetitions", Kind: KindInt, Default: int64(3), Min: intPtr(1)},

	// --- update.* --------------------------------------------------------------
	{
		Key: "update.channel", Kind: KindEnum, Default: "stable",
		Enum: []string{"stable", "prerelease"},
	},
	{Key: "update.auto_check", Kind: KindBool, Default: true},
	{Key: "update.check_interval_hours", Kind: KindInt, Default: int64(24), Min: intPtr(1)},

	// --- retention.* --------------------------------------------------------------
	{Key: "retention.events_days", Kind: KindInt, Default: int64(90), Min: intPtr(1)},
	{Key: "retention.events_rows", Kind: KindInt, Default: int64(200000), Min: intPtr(1)},

	// --- log.* --------------------------------------------------------------
	{
		// DESIGN section 2.1 names this an enum but does not spell out its
		// members; these are slog's own level names, which internal/logx (a
		// "slog handler tuned for journald", DESIGN section 1) already speaks.
		Key: "log.level", Kind: KindEnum, Default: "info",
		Enum: []string{"debug", "info", "warn", "error"}, RestartRequired: true,
	},
}
