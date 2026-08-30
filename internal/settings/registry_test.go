package settings

import (
	"testing"
)

// designKeys is every key DESIGN section 2.1's settings table names, in the
// order that table lists them. This test's job is to catch drift in either
// direction: a key the table names that the registry forgot, or a key the
// registry carries that the table does not.
var designKeys = []string{
	"ui.port_desired",
	"ui.bind",
	"ui.theme",
	"security.session_ttl_hours",
	"security.idle_timeout_hours",
	"security.login_max_attempts",
	"security.login_window_sec",
	"security.lockout_sec",
	"hf.endpoint",
	"hf.hub_dir",
	"hf.home",
	"hf.download_concurrency",
	"hf.rate_limit_bytes_sec",
	"hf.verify_checksums",
	"llamacpp.channel",
	"llamacpp.build_jobs",
	"llamacpp.cuda_arch_list",
	"llamacpp.prefer_prebuilt_cpu",
	"llamacpp.extra_cmake_flags",
	"llamacpp.keep_previous",
	"instances.internal_port_min",
	"instances.internal_port_max",
	"instances.health_poll_sec",
	"instances.start_timeout_sec",
	"gateway.bind",
	"gateway.request_timeout_sec",
	"gateway.idle_timeout_sec",
	"gateway.max_body_mb",
	"gateway.usage_parse_kb",
	"gateway.drain_sec",
	"gpu.poll_active_sec",
	"gpu.poll_idle_sec",
	"fit.margin_mib",
	"fit.use_calibration",
	"bench.exclusive_gpu",
	"bench.default_repetitions",
	"update.channel",
	"update.auto_check",
	"update.check_interval_hours",
	"retention.events_days",
	"retention.events_rows",
	"log.level",
}

func TestNewRegistry_MatchesDesign(t *testing.T) {
	r := NewRegistry()

	got := make(map[string]bool, len(r.Keys()))
	for _, k := range r.Keys() {
		got[k] = true
	}
	want := make(map[string]bool, len(designKeys))
	for _, k := range designKeys {
		want[k] = true
	}

	for k := range want {
		if !got[k] {
			t.Errorf("DESIGN section 2.1 names %q but the registry does not carry it", k)
		}
	}
	for k := range got {
		if !want[k] {
			t.Errorf("registry carries %q, which DESIGN section 2.1 does not name", k)
		}
	}
	if len(designKeys) != len(r.Keys()) {
		t.Errorf("key count mismatch: DESIGN names %d, registry has %d", len(designKeys), len(r.Keys()))
	}
}

func TestNewRegistry_GroupDerivedFromKey(t *testing.T) {
	r := NewRegistry()
	tests := []struct{ key, wantGroup string }{
		{"ui.port_desired", "ui"},
		{"hf.hub_dir", "hf"},
		{"llamacpp.keep_previous", "llamacpp"},
		{"retention.events_days", "retention"},
	}
	for _, tt := range tests {
		def, ok := r.Lookup(tt.key)
		if !ok {
			t.Fatalf("Lookup(%q): not found", tt.key)
		}
		if def.Group != tt.wantGroup {
			t.Errorf("Lookup(%q).Group = %q, want %q", tt.key, def.Group, tt.wantGroup)
		}
	}
}

func TestNewRegistry_RestartRequired(t *testing.T) {
	// DESIGN section 3.4: exactly these four keys carry restart_required.
	want := map[string]bool{
		"ui.port_desired": true,
		"ui.bind":         true,
		"gateway.bind":    true,
		"log.level":       true,
	}
	r := NewRegistry()
	for _, def := range r.Definitions() {
		if def.RestartRequired != want[def.Key] {
			t.Errorf("%s: RestartRequired = %v, want %v", def.Key, def.RestartRequired, want[def.Key])
		}
	}
}

func TestRegistry_Definitions_SortedByKey(t *testing.T) {
	r := NewRegistry()
	defs := r.Definitions()
	for i := 1; i < len(defs); i++ {
		if defs[i-1].Key > defs[i].Key {
			t.Fatalf("Definitions() not sorted: %q before %q", defs[i-1].Key, defs[i].Key)
		}
	}
}

func TestRegistry_Lookup_UnknownKey(t *testing.T) {
	r := NewRegistry()
	if _, ok := r.Lookup("no.such.key"); ok {
		t.Fatal("Lookup(\"no.such.key\") reported found")
	}
}

func TestNewRegistry_DuplicateKeyPanics(t *testing.T) {
	orig := definitions
	defer func() { definitions = orig }()
	definitions = append(append([]Definition(nil), orig...), Definition{
		Key: orig[0].Key, Kind: KindString, Default: "",
	})

	defer func() {
		if recover() == nil {
			t.Fatal("NewRegistry did not panic on a duplicate key")
		}
	}()
	NewRegistry()
}

func TestDefinition_Validate_Int(t *testing.T) {
	d := Definition{Key: "k", Kind: KindInt, Min: intPtr(1), Max: intPtr(10)}
	tests := []struct {
		name    string
		v       any
		wantErr bool
	}{
		{"in range", int64(5), false},
		{"at min", int64(1), false},
		{"at max", int64(10), false},
		{"below min", int64(0), true},
		{"above max", int64(11), true},
		{"wrong type", "5", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := d.Validate(tt.v)
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate(%v) error = %v, wantErr %v", tt.v, err, tt.wantErr)
			}
		})
	}
}

func TestDefinition_Validate_IntUnboundedNil(t *testing.T) {
	d := Definition{Key: "k", Kind: KindInt}
	if err := d.Validate(int64(-1000)); err != nil {
		t.Errorf("Validate: unexpected error for an unbounded int setting: %v", err)
	}
	if err := d.Validate(int64(1000000)); err != nil {
		t.Errorf("Validate: unexpected error for an unbounded int setting: %v", err)
	}
}

func TestDefinition_Validate_Bool(t *testing.T) {
	d := Definition{Key: "k", Kind: KindBool}
	if err := d.Validate(true); err != nil {
		t.Errorf("Validate(true): %v", err)
	}
	if err := d.Validate("true"); err == nil {
		t.Error("Validate(\"true\") on a bool setting: want error, got nil")
	}
}

func TestDefinition_Validate_String(t *testing.T) {
	d := Definition{Key: "k", Kind: KindString}
	if err := d.Validate("anything"); err != nil {
		t.Errorf("Validate(\"anything\"): %v", err)
	}
	if err := d.Validate(int64(1)); err == nil {
		t.Error("Validate(1) on a string setting: want error, got nil")
	}
}

func TestDefinition_Validate_Enum(t *testing.T) {
	d := Definition{Key: "k", Kind: KindEnum, Enum: []string{"a", "b"}}
	if err := d.Validate("a"); err != nil {
		t.Errorf("Validate(\"a\"): %v", err)
	}
	if err := d.Validate("c"); err == nil {
		t.Error("Validate(\"c\") not in enum: want error, got nil")
	}
	if err := d.Validate(int64(1)); err == nil {
		t.Error("Validate(1) on an enum setting: want error, got nil")
	}
}

func TestDefinition_Validate_UnregisteredKind(t *testing.T) {
	d := Definition{Key: "k", Kind: Kind("bogus")}
	if err := d.Validate("x"); err == nil {
		t.Error("Validate with an unregistered Kind: want error, got nil")
	}
}

func TestDefinition_Validate_Extra(t *testing.T) {
	d := Definition{
		Key: "k", Kind: KindString,
		Extra: func(v any) error {
			if v.(string) == "bad" {
				return errBad
			}
			return nil
		},
	}
	if err := d.Validate("good"); err != nil {
		t.Errorf("Validate(\"good\"): %v", err)
	}
	if err := d.Validate("bad"); err == nil {
		t.Error("Validate(\"bad\"): want error from Extra, got nil")
	}
}

var errBad = errValidation("bad value")

type errValidation string

func (e errValidation) Error() string { return string(e) }

func TestValidIPAddress(t *testing.T) {
	tests := []struct {
		v       string
		wantErr bool
	}{
		{"0.0.0.0", false},
		{"127.0.0.1", false},
		{"::1", false},
		{"not-an-ip", true},
		{"", true},
	}
	for _, tt := range tests {
		err := validIPAddress(tt.v)
		if (err != nil) != tt.wantErr {
			t.Errorf("validIPAddress(%q) error = %v, wantErr %v", tt.v, err, tt.wantErr)
		}
	}
}

func TestValidHTTPURL(t *testing.T) {
	tests := []struct {
		v       string
		wantErr bool
	}{
		{"https://huggingface.co", false},
		{"http://example.com", false},
		{"ftp://example.com", true},
		{"not a url", true},
		{"https://", true},
	}
	for _, tt := range tests {
		err := validHTTPURL(tt.v)
		if (err != nil) != tt.wantErr {
			t.Errorf("validHTTPURL(%q) error = %v, wantErr %v", tt.v, err, tt.wantErr)
		}
	}
}

// TestDefinitions_UIBindAndGatewayBindUseIPValidator confirms the two bind
// settings actually wire up the Extra check DESIGN's bind-address prose
// implies, rather than accepting any string.
func TestDefinitions_BindSettingsRejectNonIP(t *testing.T) {
	r := NewRegistry()
	for _, key := range []string{"ui.bind", "gateway.bind"} {
		def, ok := r.Lookup(key)
		if !ok {
			t.Fatalf("Lookup(%q): not found", key)
		}
		if err := def.Validate("not-an-ip"); err == nil {
			t.Errorf("%s: Validate(\"not-an-ip\") = nil, want error", key)
		}
		if err := def.Validate(def.Default); err != nil {
			t.Errorf("%s: Validate(default %v) = %v, want nil", key, def.Default, err)
		}
	}
}

func TestDefinitions_HFEndpointRejectsNonHTTPURL(t *testing.T) {
	r := NewRegistry()
	def, ok := r.Lookup("hf.endpoint")
	if !ok {
		t.Fatal("Lookup(\"hf.endpoint\"): not found")
	}
	if err := def.Validate("not a url"); err == nil {
		t.Error("hf.endpoint: Validate(\"not a url\") = nil, want error")
	}
	if err := def.Validate(def.Default); err != nil {
		t.Errorf("hf.endpoint: Validate(default) = %v, want nil", err)
	}
}

func TestDefinitions_EveryDefaultValidatesAgainstItsOwnDefinition(t *testing.T) {
	r := NewRegistry()
	for _, def := range r.Definitions() {
		if err := def.Validate(def.Default); err != nil {
			t.Errorf("%s: its own registered Default %v fails Validate: %v", def.Key, def.Default, err)
		}
	}
}

func TestDefinition_Decode(t *testing.T) {
	tests := []struct {
		name    string
		def     Definition
		raw     string
		wantErr bool
	}{
		{"int ok", Definition{Kind: KindInt}, `42`, false},
		{"int bad json", Definition{Kind: KindInt}, `"forty-two"`, true},
		{"bool ok", Definition{Kind: KindBool}, `true`, false},
		{"bool bad json", Definition{Kind: KindBool}, `"true"`, true},
		{"string ok", Definition{Kind: KindString}, `"hi"`, false},
		{"enum ok", Definition{Kind: KindEnum}, `"stable"`, false},
		{"unregistered kind", Definition{Kind: Kind("bogus")}, `1`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.def.decode([]byte(tt.raw))
			if (err != nil) != tt.wantErr {
				t.Errorf("decode(%s) error = %v, wantErr %v", tt.raw, err, tt.wantErr)
			}
		})
	}
}
