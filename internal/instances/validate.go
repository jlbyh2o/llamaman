package instances

import (
	"fmt"
	"regexp"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// Validation (DESIGN sections 2.8, 3.10a, 5.7; D11, D34).

// NamePattern is D11's instance-name grammar. The same rule is enforced in
// three places and that is not redundancy: this string becomes a systemd unit
// instance id (`llamaman-instance@<name>.service`), so the SQL `CHECK` keeps a
// bad name out of the table even when a code path is wrong, and the polkit rule
// matches the same regex to keep the daemon's unit-name grant from widening.
//
// The base proposal's `GLOB '[a-z0-9]*'` constrained only the FIRST character,
// which is why the schema carries the extra `NOT GLOB '*[^a-z0-9-]*'` clause
// beside it.
const NamePattern = `^[a-z0-9][a-z0-9-]{0,31}$`

var nameRE = regexp.MustCompile(NamePattern)

// UnitTemplate is the content-free instance template of section 5.5. It receives
// nothing but `%i`, which is why safe start needs a database hand-off column
// rather than an argv or environment channel (D61).
const UnitTemplate = "llamaman-instance@%s.service"

// UnitName renders the systemd unit name for an instance name.
func UnitName(name string) string { return fmt.Sprintf(UnitTemplate, name) }

// DisableCommand is the exact line `DELETE /instances/{id}` hands back when it
// could not disable the unit itself (section 3.10c, F9/F10).
func DisableCommand(name string) string {
	return "sudo systemctl disable " + UnitName(name)
}

// ValidateName enforces D11.
func ValidateName(name string) error {
	if nameRE.MatchString(name) {
		return nil
	}
	return model.Error{
		Code: model.CodeInstanceNameInvalid,
		Message: "an instance name must be 1-32 characters of lowercase letters, digits and " +
			"hyphens, starting with a letter or digit — it becomes a systemd unit name",
		Details: map[string]any{"name": name, "pattern": NamePattern},
	}
}

// ValidateFlags is the save-time half of section 5.7: the FlagSet's own value
// rules, plus the one cross-field guard D51 names.
func ValidateFlags(flags model.FlagSet) error {
	if err := flags.Validate(); err != nil {
		return model.Error{Code: model.CodeBadFlags, Message: err.Error()}
	}
	// `auto` is rejected together with an explicit tensor_split, because
	// --tensor-split also disables --fit upstream — leaving `auto` meaning
	// nothing at all rather than meaning "llama.cpp chooses".
	if flags.NGpuLayers != nil && flags.NGpuLayers.Mode == model.NGLAuto && len(flags.TensorSplit) > 0 {
		return model.Error{
			Code: model.CodeNGLAutoConflict,
			Message: "n_gpu_layers `auto` cannot be combined with an explicit tensor_split: " +
				"llama.cpp disables --fit when either is pinned, so `auto` would decide nothing",
		}
	}
	return nil
}

// DraftPair is the two sides of D34's three-valued check.
type DraftPair struct {
	// Primary and Draft are the models being paired. A zero ModelMeta means
	// "no such reference", which is not a mismatch — it is nothing to check.
	Primary ModelMeta
	Draft   ModelMeta
}

// ModelMeta is the GGUF metadata the draft check reads. `Parsed` is
// `models.gguf_parsed_at IS NOT NULL`: the two fields below exist only after a
// parse, and the design deliberately supports configuring an instance against a
// model that is still downloading.
type ModelMeta struct {
	ID             string
	Parsed         bool
	TokenizerModel *string
	NVocab         *int64
}

// ValidateDraft is section 3.10a's table, three-valued by design (D34).
//
//	both sides parsed and matching  → ok, save succeeds
//	both sides parsed and differing → 422 draft_vocab_mismatch, nothing is saved
//	either side unparsed            → deferred, save succeeds with a warning
//
// A hard reject on NULL metadata would break this design's own "queue the
// download, configure the instance while it runs" flow; a silent accept would
// evaporate the guarantee. The deferred state is recorded in
// `instances.draft_validation` and re-checked in exactly two later moments — by
// the models service when the metadata lands, and by the launcher's preflight,
// which refuses to start on `mismatch` (exit 65).
func ValidateDraft(p DraftPair) (model.DraftValidation, *model.Warning, error) {
	if p.Draft.ID == "" || p.Primary.ID == "" {
		return model.DraftOK, nil, nil
	}
	if !p.Primary.Parsed || !p.Draft.Parsed {
		unparsed := p.Draft.ID
		if !p.Primary.Parsed {
			unparsed = p.Primary.ID
		}
		return model.DraftDeferred, &model.Warning{
			Code: model.WarnDraftVocabUnverified,
			Message: "the draft model's vocabulary will be checked against the primary model " +
				"when the GGUF metadata is available",
			Details: map[string]any{"unparsed_model_id": unparsed},
		}, nil
	}
	if sameString(p.Primary.TokenizerModel, p.Draft.TokenizerModel) &&
		sameInt64(p.Primary.NVocab, p.Draft.NVocab) {
		return model.DraftOK, nil, nil
	}
	return model.DraftMismatch, nil, model.Error{
		Code: model.CodeDraftVocabMismatch,
		Message: "the draft model's tokenizer does not match the primary model's; " +
			"speculative decoding would produce garbage output",
		Details: map[string]any{
			"primary": map[string]any{
				"model_id":        p.Primary.ID,
				"tokenizer_model": p.Primary.TokenizerModel,
				"n_vocab":         p.Primary.NVocab,
			},
			"draft": map[string]any{
				"model_id":        p.Draft.ID,
				"tokenizer_model": p.Draft.TokenizerModel,
				"n_vocab":         p.Draft.NVocab,
			},
		},
	}
}

func sameString(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func sameInt64(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
