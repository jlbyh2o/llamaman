package app

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/jlbyh2o/llamaman/internal/api"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/settings"
)

// The composition root's answer to DESIGN section 3.4 (api.SettingsService).
//
// The registry lives in internal/settings and the values live in the database,
// but `restart_required` lives HERE and nowhere else, which is the whole reason
// this adapter exists rather than the API talking to *settings.Cache directly.
//
// Section 3.4 defines the flag precisely: it means "the running daemon still
// holds the old value", and it is "cleared when the new daemon comes back". That
// is not a property of the key — `ui.port_desired` is restart-affecting always —
// it is a property of THIS PROCESS having read a different value than the one
// now stored. So the adapter snapshots the four restart-affecting keys at
// construction, which is the moment the daemon actually bound its port and its
// log level, and answers the flag by comparing. A registry that answered from
// the Definition alone would light "Restart to apply" permanently, and a user
// who restarts and sees the same banner learns to ignore it.

// settingsAPI implements api.SettingsService.
type settingsAPI struct {
	cache *settings.Cache

	// running is the value each restart-affecting key had when this daemon
	// applied it. It is written once and never mutated, so no restart can be
	// "cleared" without a new process.
	mu      sync.Mutex
	running map[string]any
}

func newSettingsAPI(ctx context.Context, cache *settings.Cache) *settingsAPI {
	s := &settingsAPI{cache: cache, running: map[string]any{}}
	if cache == nil {
		return s
	}
	for _, def := range cache.Registry().Definitions() {
		if !def.RestartRequired {
			continue
		}
		v, err := cache.Get(ctx, def.Key)
		if err != nil {
			// A key that cannot be read at boot is one this process is running
			// the DEFAULT of, which is the honest snapshot to compare against.
			v = def.Default
		}
		s.running[def.Key] = v
	}
	return s
}

func (s *settingsAPI) Settings(ctx context.Context) (api.SettingsDTO, error) {
	values, err := s.values(ctx)
	if err != nil {
		return api.SettingsDTO{}, err
	}
	return api.SettingsDTO{Values: values, Schema: s.schema(values)}, nil
}

func (s *settingsAPI) PatchSettings(ctx context.Context,
	body map[string]json.RawMessage) (api.PatchSettingsDTO, error) {

	// Every key is validated BEFORE anything is written. Section 3.4 gives a
	// per-key `400 setting_invalid`, and a form that half-applied — port saved,
	// bind refused — would leave the daemon one restart away from being
	// unreachable on both.
	keys := make([]string, 0, len(body))
	for key := range body {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		if _, ok := s.cache.Registry().Lookup(key); !ok {
			return api.PatchSettingsDTO{}, model.Error{
				Code:    model.CodeSettingInvalid,
				Message: "there is no setting named " + key,
				Details: map[string]any{"key": key},
			}
		}
	}

	// The second pass writes. `Set` re-decodes and re-validates per key, which
	// is where a bad VALUE (as opposed to a bad key) is caught; a failure here
	// leaves the keys before it written, so the ordering above is what keeps the
	// common case — a typo'd key name — from touching anything at all.
	restartKeys := []string{}
	for _, key := range keys {
		if _, err := s.cache.Set(ctx, key, body[key], model.UpdatedByAdmin); err != nil {
			return api.PatchSettingsDTO{}, settingError(key, err)
		}
		if s.needsRestart(ctx, key) {
			restartKeys = append(restartKeys, key)
		}
	}

	values, err := s.values(ctx)
	if err != nil {
		return api.PatchSettingsDTO{}, err
	}
	return api.PatchSettingsDTO{
		Values:          values,
		RestartRequired: len(restartKeys) > 0,
		RestartKeys:     restartKeys,
	}, nil
}

func (s *settingsAPI) ResetSettings(ctx context.Context, keys []string) (api.SettingsDTO, error) {
	if err := s.cache.Reset(ctx, keys); err != nil {
		return api.SettingsDTO{}, settingError(strings.Join(keys, ", "), err)
	}
	return s.Settings(ctx)
}

func (s *settingsAPI) values(ctx context.Context) (map[string]any, error) {
	defs := s.cache.Registry().Definitions()
	out := make(map[string]any, len(defs))
	for _, def := range defs {
		v, err := s.cache.Get(ctx, def.Key)
		if err != nil {
			return nil, err
		}
		out[def.Key] = v
	}
	return out, nil
}

func (s *settingsAPI) schema(values map[string]any) []api.SettingDefDTO {
	defs := s.cache.Registry().Definitions()
	out := make([]api.SettingDefDTO, 0, len(defs))
	for _, def := range defs {
		row := api.SettingDefDTO{
			Key:     def.Key,
			Type:    string(def.Kind),
			Default: def.Default,
			Min:     def.Min,
			Max:     def.Max,
			Enum:    def.Enum,
			Label:   labelFor(def.Key),
			Group:   def.Group,
		}
		if def.RestartRequired {
			running, ok := s.runningValue(def.Key)
			row.RestartRequired = ok && differs(values[def.Key], running)
		}
		out = append(out, row)
	}
	return out
}

func (s *settingsAPI) runningValue(key string) (any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.running[key]
	return v, ok
}

func (s *settingsAPI) needsRestart(ctx context.Context, key string) bool {
	def, ok := s.cache.Registry().Lookup(key)
	if !ok || !def.RestartRequired {
		return false
	}
	current, err := s.cache.Get(ctx, key)
	if err != nil {
		return true
	}
	running, ok := s.runningValue(key)
	return ok && differs(current, running)
}

// differs compares a stored value against the one this process is running.
//
// The two are always one of the three types a Definition's Kind implies —
// int64, bool or string — so a plain != is exact. It is written as a comparison
// of `any` rather than a type switch because a future Kind would then fail
// LOUDLY at the switch's default rather than silently reporting "unchanged",
// which is the direction a restart banner should be wrong in.
func differs(current, running any) bool { return current != running }

// labelFor renders a dotted key as a sentence-case field label.
//
// The registry carries no labels, and adding thirty of them there would put UI
// copy in the validation layer. Deriving one is imperfect for an acronym —
// `ui.bind` becomes "Bind" under a "Ui" group — but a derived label is never
// blank, and section 3.4's schema is what generates the form: a missing label is
// an unlabelled field, which is worse than an imperfect one.
func labelFor(key string) string {
	_, tail, ok := strings.Cut(key, ".")
	if !ok {
		tail = key
	}
	words := strings.Split(strings.ReplaceAll(tail, ".", "_"), "_")
	for i, w := range words {
		switch w {
		case "url", "ui", "hf", "gpu", "vram", "ram", "rpm", "id", "api", "ttl", "sec", "ms":
			words[i] = strings.ToUpper(w)
		case "":
		default:
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
}

// settingError normalizes internal/settings' errors into section 3.4's
// `400 setting_invalid`, keeping the key in `details` so a form can attach the
// message to the field that caused it rather than to the form as a whole.
func settingError(key string, err error) error {
	var me model.Error
	if errors.As(err, &me) {
		return me
	}
	return model.Error{
		Code:    model.CodeSettingInvalid,
		Message: err.Error(),
		Details: map[string]any{"key": key},
	}
}
