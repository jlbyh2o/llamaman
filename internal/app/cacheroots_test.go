package app

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// The boot sequence must REGISTER the Hugging Face cache root, not merely be
// able to (DESIGN §11.1 step 6, §7.2; SPEC §3.2).
//
// internal/hf/cache's six-rule chain and models.Service.DetectRoots were both
// implemented, unit-tested and never called, so `hf_cache_roots` stayed empty on
// every fresh host: the wizard's Hugging Face step reported "no cache root is
// registered" and models already on disk were never surfaced. A unit test of the
// chain cannot catch that — only a test of the WIRING can, which is why this one
// boots the real daemon and reads the row out of the real database.
func TestBootRegistersTheHFCacheRoot(t *testing.T) {
	// The chain reads the process environment (cache.Detect defaults to
	// os.Getenv), so every variable ahead of the one under test is pinned empty
	// to keep the winner deterministic on a developer machine that exports one.
	for _, k := range []string{"HF_HUB_CACHE", "HUGGINGFACE_HUB_CACHE", "TRANSFORMERS_CACHE"} {
		t.Setenv(k, "")
	}
	hfHome := t.TempDir()
	hub := filepath.Join(hfHome, "hub")
	if err := os.MkdirAll(hub, 0o755); err != nil {
		t.Fatalf("create the hub directory: %v", err)
	}
	t.Setenv("HF_HOME", hfHome)

	dir := t.TempDir()
	startDaemon(t, dir)

	// Rule 3 names an existing directory, so it wins outright and becomes the
	// PRIMARY row — the one every later read of the cache location resolves
	// through (§7.2a).
	path, from := primaryCacheRoot(t, filepath.Join(dir, "llamaman.db"))
	if path != hub {
		t.Errorf("primary cache root = %q, want %q", path, hub)
	}
	if from != "HF_HOME" {
		t.Errorf("detected_from = %q, want HF_HOME", from)
	}
}

// primaryCacheRoot reads the primary `hf_cache_roots` row straight out of the
// database, which is the authority the UI and the wizard both project from.
//
// It polls because detection is part of boot but the row is written in its own
// transaction; ReadyHook fires on the listener, not on the last write.
func primaryCacheRoot(t *testing.T, dbPath string) (path, detectedFrom string) {
	t.Helper()

	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		t.Fatalf("open the database: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for {
		row := db.QueryRowContext(ctx,
			`SELECT path, COALESCE(detected_from, '') FROM hf_cache_roots WHERE is_primary = 1`)
		switch err := row.Scan(&path, &detectedFrom); {
		case err == nil:
			return path, detectedFrom
		case err == sql.ErrNoRows:
			// Not written yet.
		default:
			t.Fatalf("read the primary cache root: %v", err)
		}
		select {
		case <-ctx.Done():
			t.Fatal("boot never registered a primary cache root")
		case <-time.After(50 * time.Millisecond):
		}
	}
}
