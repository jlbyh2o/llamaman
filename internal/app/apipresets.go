package app

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/jlbyh2o/llamaman/internal/api"
	"github.com/jlbyh2o/llamaman/internal/instances"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// The composition root's answer to DESIGN section 3.11 (api.PresetService).
//
// A preset is `flags_json` plus `extra_flags` and nothing else. That omission is
// the design: everything a preset carries is a TUNING decision, and everything
// it leaves out — the model, the ports, the name — is an IDENTITY decision. It
// is what makes "apply this preset to these five instances" a sentence that
// means something rather than five instances that are now the same instance.
//
// Apply is per-key, and a per-instance failure is reported INSIDE the 200. An
// apply over five instances that stops at the second leaves the user with no way
// to know which two moved, so each row carries its own outcome and the request
// as a whole succeeds.

type presetAPI struct{ d *daemon }

func (p *presetAPI) Presets(ctx context.Context) ([]api.PresetDTO, error) {
	var rows []store.FlagPreset
	if err := p.d.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		rows, err = p.d.store.FlagPresets(ctx, tx)
		return err
	}); err != nil {
		return nil, err
	}
	out := make([]api.PresetDTO, 0, len(rows))
	for _, row := range rows {
		dto, err := presetDTO(row)
		if err != nil {
			// A row whose flags no longer parse is a row this binary cannot
			// apply. Skipping it beats failing the list: the other presets still
			// work, and a picker that shows nothing because one entry is corrupt
			// is worse than one that is missing an entry.
			p.d.log.Warn("skipping a flag preset whose flags will not parse",
				"preset", row.ID, "error", err)
			continue
		}
		out = append(out, dto)
	}
	return out, nil
}

func (p *presetAPI) Preset(ctx context.Context, id string) (api.PresetDTO, error) {
	var row store.FlagPreset
	if err := p.d.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		row, err = p.d.store.FlagPreset(ctx, tx, id)
		return err
	}); err != nil {
		return api.PresetDTO{}, err
	}
	return presetDTO(row)
}

func (p *presetAPI) CreatePreset(ctx context.Context, in api.PresetInput) (api.PresetDTO, error) {
	name := strings.TrimSpace(derefOr(in.Name, ""))
	if name == "" {
		return api.PresetDTO{}, model.Error{
			Code: model.CodeBadFlags, Message: "a preset needs a name",
		}
	}
	flags := model.FlagSet{}
	if in.Flags != nil {
		flags = *in.Flags
	}
	if err := instances.ValidateFlags(flags); err != nil {
		return api.PresetDTO{}, err
	}

	blob, err := json.Marshal(flags)
	if err != nil {
		return api.PresetDTO{}, err
	}
	now := p.d.opts.Now()
	row := store.FlagPreset{
		ID:          store.NewID(now),
		Name:        name,
		Description: in.Description,
		FlagsJSON:   string(blob),
		ExtraFlags:  derefOr(in.ExtraFlags, ""),
		CreatedAt:   now.UnixMilli(),
		UpdatedAt:   now.UnixMilli(),
	}
	if err := p.d.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		return p.d.store.InsertFlagPreset(ctx, tx, row)
	}); err != nil {
		return api.PresetDTO{}, presetNameConflict(name, err)
	}
	return presetDTO(row)
}

func (p *presetAPI) PatchPreset(ctx context.Context, id string, in api.PresetInput) (
	api.PresetDTO, error) {

	var out store.FlagPreset
	err := p.d.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		row, err := p.d.store.FlagPreset(ctx, tx, id)
		if err != nil {
			return err
		}
		if row.Builtin {
			return builtinRefusal(row.Name)
		}
		if in.Name != nil {
			row.Name = strings.TrimSpace(*in.Name)
		}
		if in.Description != nil {
			row.Description = in.Description
		}
		if in.Flags != nil {
			if err := instances.ValidateFlags(*in.Flags); err != nil {
				return err
			}
			blob, err := json.Marshal(*in.Flags)
			if err != nil {
				return err
			}
			row.FlagsJSON = string(blob)
		}
		if in.ExtraFlags != nil {
			row.ExtraFlags = *in.ExtraFlags
		}
		row.UpdatedAt = p.d.opts.Now().UnixMilli()

		if _, err := p.d.store.UpdateFlagPreset(ctx, tx, row); err != nil {
			return presetNameConflict(row.Name, err)
		}
		out = row
		return nil
	})
	if err != nil {
		return api.PresetDTO{}, err
	}
	return presetDTO(out)
}

func (p *presetAPI) DeletePreset(ctx context.Context, id string) error {
	return p.d.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		row, err := p.d.store.FlagPreset(ctx, tx, id)
		if err != nil {
			return err
		}
		if row.Builtin {
			return builtinRefusal(row.Name)
		}
		_, err = p.d.store.DeleteFlagPreset(ctx, tx, id)
		return err
	})
}

// PresetFromInstance captures an instance's tuning, and only its tuning.
func (p *presetAPI) PresetFromInstance(ctx context.Context, instanceID string,
	in api.PresetInput) (api.PresetDTO, error) {

	view, err := p.d.instances.Get(ctx, instanceID)
	if err != nil {
		return api.PresetDTO{}, err
	}

	flags := view.Flags
	extra := view.ExtraFlags
	name := strings.TrimSpace(derefOr(in.Name, ""))
	if name == "" {
		name = view.Name
	}
	return p.CreatePreset(ctx, api.PresetInput{
		Name:        &name,
		Description: in.Description,
		Flags:       &flags,
		ExtraFlags:  &extra,
	})
}

// ApplyPreset writes the preset's flags onto each named instance, key by key.
//
// `overwrite` is the allow-list of keys the preset may replace, and an EMPTY
// list means every key the preset sets. That is the destructive reading on
// purpose: the UI always sends the list the user checked, and a caller that
// sends nothing has asked for the whole preset.
func (p *presetAPI) ApplyPreset(ctx context.Context, id string,
	instanceIDs, overwrite []string) (api.PresetApplyDTO, error) {

	preset, err := p.Preset(ctx, id)
	if err != nil {
		return api.PresetApplyDTO{}, err
	}

	allow := map[string]struct{}{}
	for _, key := range overwrite {
		allow[key] = struct{}{}
	}

	out := api.PresetApplyDTO{Items: make([]api.PresetApplyEntryDTO, 0, len(instanceIDs))}
	for _, instanceID := range instanceIDs {
		entry := api.PresetApplyEntryDTO{InstanceID: instanceID, Changed: []string{}}

		view, err := p.d.instances.Get(ctx, instanceID)
		if err != nil {
			entry.Error = errorDetail(err)
			out.Items = append(out.Items, entry)
			continue
		}
		entry.Name = view.Name

		merged, changed, err := mergeFlags(view.Flags, preset.Flags, allow)
		if err != nil {
			entry.Error = errorDetail(err)
			out.Items = append(out.Items, entry)
			continue
		}
		entry.Changed = changed

		if len(changed) == 0 {
			entry.RestartRequired = view.Derived.RestartRequired
			out.Items = append(out.Items, entry)
			continue
		}

		updated, err := p.d.instances.Patch(ctx, instanceID, instances.PatchParams{
			Generation: view.Generation,
			Flags:      &merged,
		})
		if err != nil {
			entry.Error = errorDetail(err)
			out.Items = append(out.Items, entry)
			continue
		}
		entry.RestartRequired = updated.Derived.RestartRequired
		out.Items = append(out.Items, entry)
	}
	out.Total = len(out.Items)
	return out, nil
}

/* -- helpers ---------------------------------------------------------------- */

// mergeFlags overlays the preset's set keys onto the instance's, restricted to
// the allow-list, and names the keys that actually moved.
//
// It works over the JSON projection rather than over the struct's fields by
// reflection, and the reason is D41: `flags_json` is ONE JSON column whose Go
// type is model.FlagSet, so "the keys a preset sets" is exactly "the keys
// present in its JSON object". A field-by-field merge would need editing every
// time llama.cpp grows a flag, which is the maintenance burden D41 exists to
// remove.
func mergeFlags(current, preset model.FlagSet, allow map[string]struct{}) (
	model.FlagSet, []string, error) {

	currentJSON, err := json.Marshal(current)
	if err != nil {
		return model.FlagSet{}, nil, err
	}
	presetJSON, err := json.Marshal(preset)
	if err != nil {
		return model.FlagSet{}, nil, err
	}

	var base, patch map[string]json.RawMessage
	if err := json.Unmarshal(currentJSON, &base); err != nil {
		return model.FlagSet{}, nil, err
	}
	if err := json.Unmarshal(presetJSON, &patch); err != nil {
		return model.FlagSet{}, nil, err
	}

	changed := []string{}
	for key, value := range patch {
		if len(allow) > 0 {
			if _, ok := allow[key]; !ok {
				continue
			}
		}
		if prev, ok := base[key]; ok && string(prev) == string(value) {
			continue
		}
		base[key] = value
		changed = append(changed, key)
	}
	sort.Strings(changed)

	mergedJSON, err := json.Marshal(base)
	if err != nil {
		return model.FlagSet{}, nil, err
	}
	merged, err := model.ParseFlagSet(mergedJSON)
	if err != nil {
		return model.FlagSet{}, nil, err
	}
	return merged, changed, nil
}

func presetDTO(row store.FlagPreset) (api.PresetDTO, error) {
	flags, err := model.ParseFlagSet([]byte(row.FlagsJSON))
	if err != nil {
		return api.PresetDTO{}, model.Error{Code: model.CodeBadFlags, Message: err.Error()}
	}
	return api.PresetDTO{
		ID:          row.ID,
		Name:        row.Name,
		Description: row.Description,
		Flags:       flags,
		ExtraFlags:  row.ExtraFlags,
		Builtin:     row.Builtin,
		CreatedAt:   api.Time(row.CreatedAt),
		UpdatedAt:   api.Time(row.UpdatedAt),
	}, nil
}

// builtinRefusal is section 3.11's one guard: a shipped preset is a constant, so
// that a user who has overwritten a favorite locally still has the original.
func builtinRefusal(name string) error {
	return model.Error{
		Code:    model.CodeConflictGeneration,
		Message: "\"" + name + "\" is a built-in preset; copy it under a new name to change it",
	}
}

// presetNameConflict turns the UNIQUE(name) violation into section 3's 409
// rather than a 500 with a SQL string in the log.
func presetNameConflict(name string, err error) error {
	if err == nil {
		return nil
	}
	var me model.Error
	if errors.As(err, &me) {
		return err
	}
	if strings.Contains(err.Error(), "UNIQUE") {
		return model.Error{
			Code:    model.CodeInstanceNameTaken,
			Message: "a preset named \"" + name + "\" already exists",
		}
	}
	return err
}

func errorDetail(err error) *api.ErrorDetailDTO {
	var me model.Error
	if errors.As(err, &me) {
		return &api.ErrorDetailDTO{Code: string(me.Code), Message: me.Message}
	}
	if errors.Is(err, store.ErrNotFound) {
		return &api.ErrorDetailDTO{Code: "not_found", Message: "no instance has this id"}
	}
	return &api.ErrorDetailDTO{Code: "internal_error", Message: "this instance could not be updated"}
}

func derefOr(p *string, def string) string {
	if p == nil {
		return def
	}
	return *p
}
