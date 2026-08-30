package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// Store is the persistence this package needs (DESIGN section 1, invariant 1:
// only internal/store contains SQL, so every other package declares the
// repository interface it needs and *store.Store satisfies it structurally).
type Store interface {
	Setting(ctx context.Context, tx store.Tx, key string) (model.Setting, error)
	Settings(ctx context.Context, tx store.Tx) ([]model.Setting, error)
	PutSetting(ctx context.Context, tx store.Tx, v model.Setting) error
	PutSettingIfAbsent(ctx context.Context, tx store.Tx, v model.Setting) (bool, error)
	DeleteSetting(ctx context.Context, tx store.Tx, key string) error

	Write(ctx context.Context, fn func(context.Context, store.Tx) error) error
	Read(ctx context.Context, fn func(context.Context, store.Tx) error) error
}

// Cache is the read-through cache in front of Store: once a key's value has
// been resolved — from an override row, or from its registry default when no
// row exists — it is served from memory until a write invalidates it, so a
// hot path (the gateway checking gateway.idle_timeout_sec on every request,
// say) never touches the database for a setting nobody has changed (DESIGN
// section 1's package description).
type Cache struct {
	reg   *Registry
	store Store
	now   func() time.Time

	mu     sync.RWMutex
	values map[string]any // decoded native value, present only once resolved
	// gen counts invalidations. It is what makes the read-through fill in Get
	// safe against a concurrent write: a Set commits its row and then
	// invalidates, so a Get that read the OLD row through the RO pool before
	// that commit must not publish what it read afterward — the delete would
	// have found nothing to remove and the stale value would then stick until
	// the next write or a daemon restart, which §3.4's "takes effect
	// immediately" forbids. Get snapshots gen before the store read and drops
	// its fill if the snapshot no longer matches.
	gen uint64
}

// New builds a Cache over reg, backed by st. reg is typically NewRegistry();
// a caller wanting a narrower or test-only key set may build its own.
func New(reg *Registry, st Store) *Cache {
	return &Cache{
		reg:    reg,
		store:  st,
		now:    time.Now,
		values: make(map[string]any),
	}
}

// Registry returns the registry this cache reads through — e.g. so an HTTP
// handler can render the `schema` half of `GET /api/v1/settings` (DESIGN
// section 3.4) alongside the `values` half that Values returns.
func (c *Cache) Registry() *Registry { return c.reg }

// Load populates the cache from every override row in one query, then fills
// every remaining registered key with its default — the boot sequence's "load
// settings (built-in defaults plus `settings` rows)" step (DESIGN section
// 11.1 step 5). Calling it is an optimization, not a requirement: Get resolves
// and caches a key it has not seen yet on its own, lazily, one row at a time.
// A row for a key no longer in the registry (a setting a later release
// removed) is ignored rather than erroring, matching this package's
// forward-only stance on the schema.
func (c *Cache) Load(ctx context.Context) error {
	var rows []model.Setting
	if err := c.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		rows, err = c.store.Settings(ctx, tx)
		return err
	}); err != nil {
		return fmt.Errorf("settings: load: %w", err)
	}

	overrides := make(map[string]model.Setting, len(rows))
	for _, row := range rows {
		overrides[row.Key] = row
	}

	defs := c.reg.Definitions()
	values := make(map[string]any, len(defs))
	for _, def := range defs {
		if row, ok := overrides[def.Key]; ok {
			decoded, err := def.decode(json.RawMessage(row.Value))
			if err != nil {
				return fmt.Errorf("settings: load %s: %w", def.Key, err)
			}
			values[def.Key] = decoded
			continue
		}
		values[def.Key] = def.Default
	}

	c.mu.Lock()
	c.values = values
	c.gen++ // a fill that straddled this wholesale replacement must not resurrect
	c.mu.Unlock()
	return nil
}

// Get returns key's current value: the cached value if one is loaded, else
// the stored override, else the registry default — resolving and caching
// whichever it finds so a second call never re-queries the store.
func (c *Cache) Get(ctx context.Context, key string) (any, error) {
	def, ok := c.reg.Lookup(key)
	if !ok {
		return nil, unknownKeyError(key)
	}

	v, gen, ok := c.cached(key)
	if ok {
		return v, nil
	}

	var out any
	err := c.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		row, err := c.store.Setting(ctx, tx, key)
		if errors.Is(err, store.ErrNotFound) {
			out = def.Default
			return nil
		}
		if err != nil {
			return err
		}
		decoded, err := def.decode(json.RawMessage(row.Value))
		if err != nil {
			return err
		}
		out = decoded
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("settings: get %s: %w", key, err)
	}

	c.fill(key, out, gen)
	return out, nil
}

// cached returns key's cached value if it has one, together with the
// invalidation generation the lookup saw — the token fill needs to decide
// whether its read is still publishable.
func (c *Cache) cached(key string) (any, uint64, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.values[key]
	return v, c.gen, ok
}

// fill publishes a read-through result under the generation the read started
// at. An invalidation that landed in between — a Set whose row committed after
// this read took its snapshot, a Reset, a Load — bumped the generation, and the
// value read is then known to be stale: it is returned to THIS caller, whose
// read genuinely predates the write, but it is not cached, so the next Get
// re-reads and sees the new row.
func (c *Cache) fill(key string, v any, gen uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gen != gen {
		return
	}
	c.values[key] = v
}

// GetInt reads a KindInt setting. It returns an error for a key that is not
// registered or not an int — a programmer error (the wrong accessor for the
// key), never a value that failed validation, since nothing invalid is ever
// stored.
func (c *Cache) GetInt(ctx context.Context, key string) (int64, error) {
	v, err := c.Get(ctx, key)
	if err != nil {
		return 0, err
	}
	n, ok := v.(int64)
	if !ok {
		return 0, fmt.Errorf("settings: %s is not an int setting", key)
	}
	return n, nil
}

// GetBool reads a KindBool setting.
func (c *Cache) GetBool(ctx context.Context, key string) (bool, error) {
	v, err := c.Get(ctx, key)
	if err != nil {
		return false, err
	}
	b, ok := v.(bool)
	if !ok {
		return false, fmt.Errorf("settings: %s is not a bool setting", key)
	}
	return b, nil
}

// GetString reads a KindString or KindEnum setting.
func (c *Cache) GetString(ctx context.Context, key string) (string, error) {
	v, err := c.Get(ctx, key)
	if err != nil {
		return "", err
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("settings: %s is not a string setting", key)
	}
	return s, nil
}

// Set decodes raw against key's definition, validates it, persists it as the
// override (DESIGN section 2.1: a row means "a human or a named system step
// decided this"), and invalidates the cached entry so the next Get reflects
// it. It returns the decoded value on success. raw is JSON, matching the
// `settings.value` column and the shape a PATCH /api/v1/settings body already
// carries per key.
func (c *Cache) Set(ctx context.Context, key string, raw json.RawMessage, updatedBy model.SettingUpdatedBy) (any, error) {
	def, decoded, err := c.decodeAndValidate(key, raw)
	if err != nil {
		return nil, err
	}

	canonical, err := json.Marshal(decoded)
	if err != nil {
		return nil, fmt.Errorf("settings: encode %s: %w", key, err)
	}

	row := model.Setting{
		Key:       def.Key,
		Value:     string(canonical),
		UpdatedAt: c.now().UnixMilli(),
		UpdatedBy: updatedBy,
	}
	if err := c.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		return c.store.PutSetting(ctx, tx, row)
	}); err != nil {
		return nil, fmt.Errorf("settings: set %s: %w", key, err)
	}

	c.Invalidate(key)
	return decoded, nil
}

// SeedIfAbsent writes value for key only if no override row exists yet, and
// reports whether it did. This is DESIGN section 11.1 step 6b's seed rule: a
// fresh install's `serve --port N` flag becomes `ui.port_desired`, but a
// stored value — set in the UI, deliberately, by a human — always wins.
func (c *Cache) SeedIfAbsent(ctx context.Context, key string, raw json.RawMessage, updatedBy model.SettingUpdatedBy) (bool, error) {
	def, decoded, err := c.decodeAndValidate(key, raw)
	if err != nil {
		return false, err
	}

	canonical, err := json.Marshal(decoded)
	if err != nil {
		return false, fmt.Errorf("settings: encode %s: %w", key, err)
	}

	var wrote bool
	if err := c.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		wrote, err = c.store.PutSettingIfAbsent(ctx, tx, model.Setting{
			Key:       def.Key,
			Value:     string(canonical),
			UpdatedAt: c.now().UnixMilli(),
			UpdatedBy: updatedBy,
		})
		return err
	}); err != nil {
		return false, fmt.Errorf("settings: seed %s: %w", key, err)
	}
	if wrote {
		c.Invalidate(key)
	}
	return wrote, nil
}

// Reset deletes each key's override row, returning it to the registry's
// built-in default (DESIGN section 3.4: `POST /api/v1/settings/reset`).
// Deleting a key that already has no row is not an error. An unregistered key
// in keys fails the whole call before anything is deleted.
func (c *Cache) Reset(ctx context.Context, keys []string) error {
	for _, key := range keys {
		if _, ok := c.reg.Lookup(key); !ok {
			return unknownKeyError(key)
		}
	}

	if err := c.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		for _, key := range keys {
			if err := c.store.DeleteSetting(ctx, tx, key); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("settings: reset: %w", err)
	}

	c.Invalidate(keys...)
	return nil
}

// Invalidate drops each key from the cache, if present, so the next Get
// reloads it from the store. Set, SeedIfAbsent and Reset call this
// themselves; it is exported for a caller that changed a settings row through
// some path other than this Cache.
//
// It also bumps the generation counter, which is the half that matters under
// concurrency: deleting an entry a racing Get has not written yet would drop
// nothing, and that Get would then install the pre-write value as if it were
// current.
func (c *Cache) Invalidate(keys ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	for _, key := range keys {
		delete(c.values, key)
	}
}

// decodeAndValidate looks up key's definition and runs raw through decode
// then Validate, wrapping either failure as a CodeSettingInvalid model.Error
// so a handler can return it to an API caller without translation.
func (c *Cache) decodeAndValidate(key string, raw json.RawMessage) (Definition, any, error) {
	def, ok := c.reg.Lookup(key)
	if !ok {
		return Definition{}, nil, unknownKeyError(key)
	}
	decoded, err := def.decode(raw)
	if err != nil {
		return Definition{}, nil, invalidValueError(key, err)
	}
	if err := def.Validate(decoded); err != nil {
		return Definition{}, nil, invalidValueError(key, err)
	}
	return def, decoded, nil
}

func unknownKeyError(key string) error {
	return model.Error{
		Code:    model.CodeSettingInvalid,
		Message: fmt.Sprintf("unknown setting %q", key),
		Details: map[string]any{"key": key},
	}
}

func invalidValueError(key string, cause error) error {
	return model.Error{
		Code:    model.CodeSettingInvalid,
		Message: cause.Error(),
		Details: map[string]any{"key": key},
	}
}
