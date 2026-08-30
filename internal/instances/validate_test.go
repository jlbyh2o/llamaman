package instances

import (
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/jlbyh2o/llamaman/internal/model"
)

// codeOf returns the model.ErrorCode an error carries, or "" when it carries
// none. Every refusal in this package is a model.Error, so a test asserting the
// CODE is asserting what the API will actually answer.
func codeOf(err error) model.ErrorCode {
	var me model.Error
	if errors.As(err, &me) {
		return me.Code
	}
	return ""
}

// TestValidateName is D11's grammar, which is enforced in three places because
// this string becomes a systemd unit instance id.
func TestValidateName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		ok   bool
		why  string
	}{
		{"a plain name", "qwen", true, ""},
		{"digits and hyphens", "qwen3-8b-q4", true, ""},
		{"a single character", "a", true, ""},
		{"a leading digit", "3b-model", true, "the first character may be a digit"},
		{"32 characters", "abcdefghij0123456789abcdefghij12", true, "the limit is inclusive"},
		{"33 characters", "abcdefghij0123456789abcdefghij123", false, "one over the limit"},
		{"empty", "", false, "a unit name cannot be empty"},
		{"a leading hyphen", "-qwen", false,
			"the base proposal's GLOB constrained only the first character, which is why " +
				"this case exists at all"},
		{"an uppercase letter", "Qwen", false, "unit names are lowercase here"},
		{"an underscore", "qwen_8b", false, "not in the grammar"},
		{"a dot", "qwen.8b", false, "a dot would split the unit name"},
		{"a slash", "qwen/8b", false, "a slash would escape the unit namespace"},
		{"a space", "qwen 8b", false, "not in the grammar"},
		{"an at sign", "qwen@8b", false, "@ is the template separator itself"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateName(tt.in)
			if tt.ok && err != nil {
				t.Fatalf("ValidateName(%q) = %v, want nil (%s)", tt.in, err, tt.why)
			}
			if !tt.ok {
				if err == nil {
					t.Fatalf("ValidateName(%q) was accepted — %s", tt.in, tt.why)
				}
				if got := codeOf(err); got != model.CodeInstanceNameInvalid {
					t.Errorf("code = %q, want %q", got, model.CodeInstanceNameInvalid)
				}
			}
		})
	}
}

func TestUnitName(t *testing.T) {
	if got, want := UnitName("qwen"), "llamaman-instance@qwen.service"; got != want {
		t.Errorf("UnitName = %q, want %q", got, want)
	}
	if got, want := DisableCommand("qwen"),
		"sudo systemctl disable llamaman-instance@qwen.service"; got != want {
		t.Errorf("DisableCommand = %q, want %q", got, want)
	}
}

// TestValidateFlags covers the value rules and the one cross-field guard
// section 5.7 names.
func TestValidateFlags(t *testing.T) {
	tests := []struct {
		name  string
		flags model.FlagSet
		want  model.ErrorCode
	}{
		{"empty", model.FlagSet{}, ""},
		{"the section 5.7 example", docExampleFlags(), ""},
		{
			name:  "auto with a tensor split",
			flags: model.FlagSet{NGpuLayers: &model.NGpuLayers{Mode: model.NGLAuto}, TensorSplit: []float64{0.5, 0.5}},
			want:  model.CodeNGLAutoConflict,
		},
		{
			// The same split is fine once the offload is pinned: --fit is off
			// either way, so nothing is being contradicted.
			name:  "a pinned count with a tensor split",
			flags: model.FlagSet{NGpuLayers: &model.NGpuLayers{Mode: model.NGLCount, Count: ptr(20)}, TensorSplit: []float64{0.5, 0.5}},
			want:  "",
		},
		{
			name:  "count with no count",
			flags: model.FlagSet{NGpuLayers: &model.NGpuLayers{Mode: model.NGLCount}},
			want:  model.CodeBadFlags,
		},
		{
			name:  "an unknown ngl mode",
			flags: model.FlagSet{NGpuLayers: &model.NGpuLayers{Mode: "some"}},
			want:  model.CodeBadFlags,
		},
		{
			name:  "an unknown flash_attn",
			flags: model.FlagSet{FlashAttn: ptr(model.FlashAttn("yes"))},
			want:  model.CodeBadFlags,
		},
		{
			name:  "an unknown split_mode",
			flags: model.FlagSet{SplitMode: ptr(model.SplitMode("column"))},
			want:  model.CodeBadFlags,
		},
		{
			name:  "a zero context",
			flags: model.FlagSet{CtxSize: ptr(0)},
			want:  model.CodeBadFlags,
		},
		{
			// A vocabulary this design does not close is passed through: it
			// belongs to llama.cpp and changes with it, so rejecting it here
			// would refuse a build's own new option.
			name:  "an unrecognized cache type is not our business",
			flags: model.FlagSet{CacheTypeK: ptr("q6_0_experimental")},
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := codeOf(ValidateFlags(tt.flags)); got != tt.want {
				t.Errorf("ValidateFlags code = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestValidateDraft is section 3.10a's three-valued table (D34).
func TestValidateDraft(t *testing.T) {
	parsed := func(id, tokenizer string, vocab int64) ModelMeta {
		return ModelMeta{ID: id, Parsed: true, TokenizerModel: &tokenizer, NVocab: &vocab}
	}

	tests := []struct {
		name     string
		pair     DraftPair
		want     model.DraftValidation
		wantWarn bool
		wantErr  model.ErrorCode
	}{
		{
			name: "no draft model at all",
			pair: DraftPair{Primary: parsed("m1", "gpt2", 151936)},
			want: model.DraftOK,
		},
		{
			name: "both parsed and matching",
			pair: DraftPair{
				Primary: parsed("m1", "gpt2", 151936),
				Draft:   parsed("m2", "gpt2", 151936),
			},
			want: model.DraftOK,
		},
		{
			name: "both parsed, different vocab size",
			pair: DraftPair{
				Primary: parsed("m1", "gpt2", 151936),
				Draft:   parsed("m2", "gpt2", 32000),
			},
			want: model.DraftMismatch, wantErr: model.CodeDraftVocabMismatch,
		},
		{
			name: "both parsed, different tokenizer",
			pair: DraftPair{
				Primary: parsed("m1", "gpt2", 151936),
				Draft:   parsed("m2", "llama", 151936),
			},
			want: model.DraftMismatch, wantErr: model.CodeDraftVocabMismatch,
		},
		{
			// The whole "queue the download, configure the instance while it
			// runs" flow: a hard reject here would break it.
			name: "the draft model has not been parsed yet",
			pair: DraftPair{
				Primary: parsed("m1", "gpt2", 151936),
				Draft:   ModelMeta{ID: "m2"},
			},
			want: model.DraftDeferred, wantWarn: true,
		},
		{
			name: "the primary model has not been parsed yet",
			pair: DraftPair{
				Primary: ModelMeta{ID: "m1"},
				Draft:   parsed("m2", "gpt2", 151936),
			},
			want: model.DraftDeferred, wantWarn: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, warn, err := ValidateDraft(tt.pair)
			if got != tt.want {
				t.Errorf("validation = %q, want %q", got, tt.want)
			}
			if (warn != nil) != tt.wantWarn {
				t.Errorf("warning = %v, want one: %v", warn, tt.wantWarn)
			}
			if warn != nil && warn.Code != model.WarnDraftVocabUnverified {
				t.Errorf("warning code = %q, want %q", warn.Code, model.WarnDraftVocabUnverified)
			}
			if got := codeOf(err); got != tt.wantErr {
				t.Errorf("error code = %q, want %q", got, tt.wantErr)
			}
		})
	}
}

// TestParseExtraFlags is section 5.7's escape hatch: POSIX word rules, no shell,
// and five refusals.
func TestParseExtraFlags(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
		code model.ErrorCode
	}{
		{"empty", "", nil, ""},
		{"plain words", "--log-colors --verbose", []string{"--log-colors", "--verbose"}, ""},
		{"collapsing whitespace", "  -a   b\t\tc ", []string{"-a", "b", "c"}, ""},
		{
			name: "a quoted path with a space",
			in:   `--lora "/models/adapter one.gguf"`,
			want: []string{"--lora", "/models/adapter one.gguf"},
		},
		{
			name: "single quotes are literal",
			in:   `--chat-template '{{ "hello" }}'`,
			want: []string{"--chat-template", `{{ "hello" }}`},
		},
		{
			name: "a backslash escape",
			in:   `--lora /models/adapter\ one.gguf`,
			want: []string{"--lora", "/models/adapter one.gguf"},
		},
		{
			// Not a shell: `$HOME`, a semicolon and a backtick are ordinary
			// text, which is exactly what makes this field safe to hand to
			// syscall.Exec.
			name: "shell metacharacters are ordinary text",
			in:   "--tag $HOME;reboot`id`",
			want: []string{"--tag", "$HOME;reboot`id`"},
		},
		{"an unterminated quote", `--lora "/models/x`, nil, model.CodeExtraFlagForbidden},
		{"a trailing backslash", `--lora \`, nil, model.CodeExtraFlagForbidden},
		{"--host", "--host 0.0.0.0", nil, model.CodeExtraFlagForbidden},
		{"--port", "--port 9999", nil, model.CodeExtraFlagForbidden},
		{"-m", "-m /other/model.gguf", nil, model.CodeExtraFlagForbidden},
		{"--model", "--model /other/model.gguf", nil, model.CodeExtraFlagForbidden},
		{"--api-key", "--api-key lm_XXXX", nil, model.CodeExtraFlagForbidden},
		{
			// The `--flag=value` spelling has to be caught too, or the rule is
			// one string comparison away from being decorative.
			name: "--host=... in the joined spelling",
			in:   "--host=0.0.0.0", code: model.CodeExtraFlagForbidden,
		},
		{
			// A flag that merely starts with a forbidden one is fine.
			name: "--model-draft is not --model",
			in:   "--model-draft /d.gguf", want: []string{"--model-draft", "/d.gguf"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseExtraFlags(tt.in)
			if code := codeOf(err); code != tt.code {
				t.Fatalf("code = %q, want %q (err %v)", code, tt.code, err)
			}
			if tt.code != "" {
				return
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("words mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestBenchExtraFlagsHaveTheirOwnForbiddenList: they are llama-bench flags, so
// `--host` is allowed through (it means nothing there) while `-o` is not
// (the JSON parser depends on it).
func TestBenchExtraFlagsHaveTheirOwnForbiddenList(t *testing.T) {
	for _, bad := range []string{"-m /x.gguf", "-o md", "-r 9"} {
		if _, err := RenderBenchArgv(model.FlagSet{}, qwenModel(), cudaRuntime(),
			BenchPoint{ExtraFlags: bad}); codeOf(err) != model.CodeExtraFlagForbidden {
			t.Errorf("bench.extra_flags %q was accepted", bad)
		}
	}
	if _, err := RenderBenchArgv(model.FlagSet{}, qwenModel(), cudaRuntime(),
		BenchPoint{ExtraFlags: "--progress"}); err != nil {
		t.Errorf("bench.extra_flags --progress was refused: %v", err)
	}
}
