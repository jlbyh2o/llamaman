package settings

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// newTestCache opens a fresh migrated database and returns a Cache over the
// full registry — the same pairing the boot sequence builds (DESIGN section
// 11.1 step 5).
func newTestCache(t *testing.T) *Cache {
	t.Helper()
	ctx := context.Background()

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "llamaman.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	if _, err := st.Migrate(ctx, store.MigrateOptions{}); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	return New(NewRegistry(), st)
}

func TestCache_Get_DefaultWhenUnset(t *testing.T) {
	c := newTestCache(t)
	ctx := context.Background()

	got, err := c.GetInt(ctx, "security.session_ttl_hours")
	if err != nil {
		t.Fatalf("GetInt: %v", err)
	}
	if got != 720 {
		t.Errorf("security.session_ttl_hours = %d, want the registered default 720", got)
	}

	gotBool, err := c.GetBool(ctx, "hf.verify_checksums")
	if err != nil {
		t.Fatalf("GetBool: %v", err)
	}
	if !gotBool {
		t.Error("hf.verify_checksums default = false, want true")
	}

	gotStr, err := c.GetString(ctx, "ui.theme")
	if err != nil {
		t.Fatalf("GetString: %v", err)
	}
	if gotStr != "dark" {
		t.Errorf("ui.theme = %q, want the registered default %q", gotStr, "dark")
	}
}

func TestCache_Get_UnknownKey(t *testing.T) {
	c := newTestCache(t)
	_, err := c.Get(context.Background(), "no.such.key")
	if err == nil {
		t.Fatal("Get(unknown key): want error, got nil")
	}
	var merr model.Error
	if !errors.As(err, &merr) {
		t.Fatalf("Get(unknown key): error is not a model.Error: %v", err)
	}
	if merr.Code != model.CodeSettingInvalid {
		t.Errorf("Get(unknown key): code = %q, want %q", merr.Code, model.CodeSettingInvalid)
	}
}

func TestCache_Set_ValidatorRejection(t *testing.T) {
	c := newTestCache(t)
	ctx := context.Background()

	tests := []struct {
		name string
		key  string
		raw  string
	}{
		{"int below min", "security.session_ttl_hours", `0`},
		{"int wrong json type", "security.session_ttl_hours", `"720"`},
		{"bool wrong json type", "hf.verify_checksums", `"true"`},
		{"enum not a member", "ui.theme", `"neon"`},
		{"bad ip", "ui.bind", `"not-an-ip"`},
		{"bad url", "hf.endpoint", `"not a url"`},
		{"port below floor", "ui.port_desired", `80`},
		{"port above ceiling", "ui.port_desired", `70000`},
		{"unknown key", "no.such.key", `1`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := c.Set(ctx, tt.key, json.RawMessage(tt.raw), model.UpdatedByAdmin)
			if err == nil {
				t.Fatalf("Set(%s, %s): want a validation error, got nil", tt.key, tt.raw)
			}
			var merr model.Error
			if !errors.As(err, &merr) {
				t.Fatalf("Set(%s, %s): error is not a model.Error: %v", tt.key, tt.raw, err)
			}
			if merr.Code != model.CodeSettingInvalid {
				t.Errorf("Set(%s, %s): code = %q, want %q", tt.key, tt.raw, merr.Code, model.CodeSettingInvalid)
			}
		})
	}
}

func TestCache_Set_ThenGet_CacheInvalidation(t *testing.T) {
	c := newTestCache(t)
	ctx := context.Background()

	// Warm the cache with the default so the next Set must invalidate it,
	// not just happen to write a value nothing had read yet.
	before, err := c.GetInt(ctx, "security.session_ttl_hours")
	if err != nil {
		t.Fatalf("GetInt (warm): %v", err)
	}
	if before != 720 {
		t.Fatalf("GetInt (warm) = %d, want 720", before)
	}

	if _, err := c.Set(ctx, "security.session_ttl_hours", json.RawMessage(`48`), model.UpdatedByAdmin); err != nil {
		t.Fatalf("Set: %v", err)
	}

	after, err := c.GetInt(ctx, "security.session_ttl_hours")
	if err != nil {
		t.Fatalf("GetInt (after Set): %v", err)
	}
	if after != 48 {
		t.Errorf("GetInt (after Set) = %d, want 48 — Set did not invalidate the cached default", after)
	}
}

func TestCache_Invalidate_ForcesReload(t *testing.T) {
	c := newTestCache(t)
	ctx := context.Background()

	if _, err := c.GetInt(ctx, "gateway.drain_sec"); err != nil {
		t.Fatalf("GetInt: %v", err)
	}

	// Write an override row directly through the store, bypassing Cache.Set,
	// to prove Invalidate — not just Set's own bookkeeping — is what makes a
	// stale cached value visible again.
	raw, err := json.Marshal(int64(99))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		return c.store.PutSetting(ctx, tx, model.Setting{
			Key: "gateway.drain_sec", Value: string(raw), UpdatedAt: 1, UpdatedBy: model.UpdatedByAdmin,
		})
	}); err != nil {
		t.Fatalf("PutSetting: %v", err)
	}

	stillCached, err := c.GetInt(ctx, "gateway.drain_sec")
	if err != nil {
		t.Fatalf("GetInt (before Invalidate): %v", err)
	}
	if stillCached != 20 {
		t.Fatalf("GetInt (before Invalidate) = %d, want the still-cached default 20", stillCached)
	}

	c.Invalidate("gateway.drain_sec")

	reloaded, err := c.GetInt(ctx, "gateway.drain_sec")
	if err != nil {
		t.Fatalf("GetInt (after Invalidate): %v", err)
	}
	if reloaded != 99 {
		t.Errorf("GetInt (after Invalidate) = %d, want 99", reloaded)
	}
}

func TestCache_Load_PopulatesOverridesAndDefaults(t *testing.T) {
	c := newTestCache(t)
	ctx := context.Background()

	if _, err := c.Set(ctx, "log.level", json.RawMessage(`"debug"`), model.UpdatedByAdmin); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// A fresh Cache over the same store, never touched, must reflect the
	// override once Load runs — this is the boot-time bulk load, distinct
	// from Get's lazy per-key path.
	c2 := New(NewRegistry(), c.store)
	if err := c2.Load(ctx); err != nil {
		t.Fatalf("Load: %v", err)
	}

	got, err := c2.GetString(ctx, "log.level")
	if err != nil {
		t.Fatalf("GetString: %v", err)
	}
	if got != "debug" {
		t.Errorf("log.level after Load = %q, want %q", got, "debug")
	}

	// A key nobody ever wrote should be filled from its registry default.
	gotDefault, err := c2.GetInt(ctx, "gpu.poll_active_sec")
	if err != nil {
		t.Fatalf("GetInt: %v", err)
	}
	if gotDefault != 2 {
		t.Errorf("gpu.poll_active_sec after Load = %d, want default 2", gotDefault)
	}
}

func TestCache_Reset_RestoresDefault(t *testing.T) {
	c := newTestCache(t)
	ctx := context.Background()

	if _, err := c.Set(ctx, "bench.default_repetitions", json.RawMessage(`7`), model.UpdatedByAdmin); err != nil {
		t.Fatalf("Set: %v", err)
	}
	changed, err := c.GetInt(ctx, "bench.default_repetitions")
	if err != nil {
		t.Fatalf("GetInt: %v", err)
	}
	if changed != 7 {
		t.Fatalf("GetInt (after Set) = %d, want 7", changed)
	}

	if err := c.Reset(ctx, []string{"bench.default_repetitions"}); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	back, err := c.GetInt(ctx, "bench.default_repetitions")
	if err != nil {
		t.Fatalf("GetInt (after Reset): %v", err)
	}
	if back != 3 {
		t.Errorf("bench.default_repetitions after Reset = %d, want default 3", back)
	}
}

func TestCache_Reset_UnknownKeyFailsAtomically(t *testing.T) {
	c := newTestCache(t)
	ctx := context.Background()

	if _, err := c.Set(ctx, "bench.default_repetitions", json.RawMessage(`7`), model.UpdatedByAdmin); err != nil {
		t.Fatalf("Set: %v", err)
	}

	err := c.Reset(ctx, []string{"bench.default_repetitions", "no.such.key"})
	if err == nil {
		t.Fatal("Reset with an unknown key: want error, got nil")
	}

	// The known key's override must survive — the whole call fails before
	// anything is deleted.
	still, err := c.GetInt(ctx, "bench.default_repetitions")
	if err != nil {
		t.Fatalf("GetInt: %v", err)
	}
	if still != 7 {
		t.Errorf("bench.default_repetitions after a failed Reset = %d, want the surviving override 7", still)
	}
}

func TestCache_SeedIfAbsent(t *testing.T) {
	c := newTestCache(t)
	ctx := context.Background()

	wrote, err := c.SeedIfAbsent(ctx, "ui.port_desired", json.RawMessage(`8080`), model.UpdatedBySystem)
	if err != nil {
		t.Fatalf("SeedIfAbsent (first): %v", err)
	}
	if !wrote {
		t.Error("SeedIfAbsent (first): wrote = false, want true on an absent key")
	}
	got, err := c.GetInt(ctx, "ui.port_desired")
	if err != nil {
		t.Fatalf("GetInt: %v", err)
	}
	if got != 8080 {
		t.Errorf("ui.port_desired after seed = %d, want 8080", got)
	}

	// A second seed must not clobber a value a human (or a prior seed) chose.
	wrote2, err := c.SeedIfAbsent(ctx, "ui.port_desired", json.RawMessage(`9090`), model.UpdatedBySystem)
	if err != nil {
		t.Fatalf("SeedIfAbsent (second): %v", err)
	}
	if wrote2 {
		t.Error("SeedIfAbsent (second): wrote = true, want false — a stored value must win")
	}
	still, err := c.GetInt(ctx, "ui.port_desired")
	if err != nil {
		t.Fatalf("GetInt: %v", err)
	}
	if still != 8080 {
		t.Errorf("ui.port_desired after a second seed = %d, want the first seed's 8080", still)
	}
}

func TestCache_SeedIfAbsent_InvalidValueRejected(t *testing.T) {
	c := newTestCache(t)
	ctx := context.Background()

	_, err := c.SeedIfAbsent(ctx, "ui.port_desired", json.RawMessage(`70000`), model.UpdatedBySystem)
	if err == nil {
		t.Fatal("SeedIfAbsent with an out-of-range port: want error, got nil")
	}
}

func TestCache_TypedAccessors_WrongKindError(t *testing.T) {
	c := newTestCache(t)
	ctx := context.Background()

	if _, err := c.GetInt(ctx, "ui.theme"); err == nil {
		t.Error("GetInt(\"ui.theme\") on a string setting: want error, got nil")
	}
	if _, err := c.GetBool(ctx, "ui.theme"); err == nil {
		t.Error("GetBool(\"ui.theme\") on a string setting: want error, got nil")
	}
	if _, err := c.GetString(ctx, "security.session_ttl_hours"); err == nil {
		t.Error("GetString(\"security.session_ttl_hours\") on an int setting: want error, got nil")
	}
}

func TestCache_Registry(t *testing.T) {
	c := newTestCache(t)
	if c.Registry() == nil {
		t.Fatal("Registry() returned nil")
	}
	if _, ok := c.Registry().Lookup("ui.theme"); !ok {
		t.Fatal("Registry().Lookup(\"ui.theme\"): not found")
	}
}

// pausingStore wraps a Store and blocks inside the first Setting call until it
// is released, which turns the read-through fill's window — between "the row
// has been read" and "the value is cached" — into a place a test can put a
// whole Set.
type pausingStore struct {
	Store
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *pausingStore) Setting(ctx context.Context, tx store.Tx, key string) (model.Setting, error) {
	v, err := p.Store.Setting(ctx, tx, key)
	p.once.Do(func() {
		close(p.entered)
		<-p.release
	})
	return v, err
}

// TestCache_Get_FillDoesNotOverwriteAConcurrentInvalidation pins the ordering
// §3.4's "a non-restart_required setting takes effect immediately" depends on:
// a Get that took a cache miss must not publish what it read once a Set has
// committed a new row underneath it. The invalidation that Set issues finds
// nothing to delete — the racing Get has not stored yet — so without the
// generation check the pre-write value would be installed afterward and stick
// until the next write or a daemon restart.
func TestCache_Get_FillDoesNotOverwriteAConcurrentInvalidation(t *testing.T) {
	ctx := context.Background()

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "llamaman.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.Migrate(ctx, store.MigrateOptions{}); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	paused := &pausingStore{
		Store:   st,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	c := New(NewRegistry(), paused)

	const key = "gateway.drain_sec"
	if _, err := c.Set(ctx, key, json.RawMessage("111"), model.UpdatedByAdmin); err != nil {
		t.Fatalf("Set(111): %v", err)
	}

	// (A) a Get that misses, reads 111 through the read pool, and stops there.
	type result struct {
		v   int64
		err error
	}
	got := make(chan result, 1)
	go func() {
		v, err := c.GetInt(ctx, key)
		got <- result{v, err}
	}()
	<-paused.entered

	// (B) a Set that commits 222 and invalidates while (A) is still paused.
	if _, err := c.Set(ctx, key, json.RawMessage("222"), model.UpdatedByAdmin); err != nil {
		t.Fatalf("Set(222): %v", err)
	}

	close(paused.release)
	if r := <-got; r.err != nil {
		t.Fatalf("concurrent GetInt: %v", r.err)
	} else if r.v != 111 {
		t.Errorf("the racing GetInt returned %d, want the 111 its read predates", r.v)
	}

	after, err := c.GetInt(ctx, key)
	if err != nil {
		t.Fatalf("GetInt after the race: %v", err)
	}
	if after != 222 {
		t.Errorf("GetInt after the race = %d, want 222 — the stale fill stuck", after)
	}
}
