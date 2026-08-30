package models

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/jlbyh2o/llamaman/internal/hf/cache/cachetest"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// setting reads one settings row, which is how the §7.2a assertions check the
// projections without reaching for SQL.
func (f *fixture) setting(key string) string {
	f.t.Helper()
	var row model.Setting
	if err := f.db.Read(context.Background(), func(ctx context.Context, tx store.Tx) error {
		var err error
		row, err = f.db.Setting(ctx, tx, key)
		return err
	}); err != nil {
		f.t.Fatalf("read setting %q: %v", key, err)
	}
	var v string
	if err := json.Unmarshal([]byte(row.Value), &v); err != nil {
		f.t.Fatalf("decode setting %q (%q): %v", key, row.Value, err)
	}
	return v
}

// TestSetPrimaryRootWritesAllFourRepresentations is §7.2a's central claim.
// The location is visible in four places and they would drift in a week if they
// were four independent facts; SetPrimaryRoot is the only writer of all four.
func TestSetPrimaryRootWritesAllFourRepresentations(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// newFixture already promoted `<tmp>/hub`, which HAS the suffix, so
	// `hf.home` is its parent.
	if got := f.setting(KeyHubDir); got != f.hub.Dir {
		t.Fatalf("settings[hf.hub_dir] = %q, want %q", got, f.hub.Dir)
	}
	if got, want := f.setting(KeyHFHome), filepath.Dir(f.hub.Dir); got != want {
		t.Fatalf("settings[hf.home] = %q, want %q", got, want)
	}

	roots, err := f.svc.Roots(ctx)
	if err != nil {
		t.Fatalf("Roots: %v", err)
	}
	if len(roots) != 1 || !roots[0].IsPrimary || roots[0].Path != f.hub.Dir {
		t.Fatalf("roots = %+v, want one primary at %q", roots, f.hub.Dir)
	}
	if !roots[0].Writable || !roots[0].SymlinksOK {
		t.Fatalf("the measured facts were not recorded: %+v", roots[0])
	}
	if roots[0].TotalBytes == nil || roots[0].FreeBytes == nil {
		t.Fatal("statfs was not recorded on the root row")
	}
}

// TestSetPrimaryRootWithNoHubSuffix is rule 1 of §7.2, which is what makes
// "nothing may assume the /hub suffix" load-bearing: `hf.home` is EMPTY, not
// invented.
func TestSetPrimaryRootWithNoHubSuffix(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	flat := cachetest.At(t, filepath.Join(t.TempDir(), "models-disk"))
	view, _, err := f.svc.SetPrimaryRoot(ctx, flat.Dir, model.DetectedFromHFHubCache)
	if err != nil {
		t.Fatalf("SetPrimaryRoot: %v", err)
	}

	if view.Path != flat.Dir {
		t.Fatalf("the hub directory was rewritten: %q, want %q", view.Path, flat.Dir)
	}
	if view.HFHome != "" {
		t.Fatalf("HFHome = %q, want empty — there is no /hub suffix to strip", view.HFHome)
	}
	if got := f.setting(KeyHubDir); got != flat.Dir {
		t.Fatalf("settings[hf.hub_dir] = %q, want the value verbatim %q", got, flat.Dir)
	}
	if got := f.setting(KeyHFHome); got != "" {
		t.Fatalf("settings[hf.home] = %q, want empty", got)
	}
}

// TestSetPrimaryRootKeepsTheOldRoot: relocating the cache is a statement about
// where NEW downloads go, not a migration. Nothing is moved, copied or deleted,
// and the old root's models keep resolving.
func TestSetPrimaryRootKeepsTheOldRoot(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.hub.Model(repo, revA, "model.gguf", cachetest.GGUF{Arch: "qwen3"})
	f.scan()
	before := f.find("model.gguf")

	newHub := cachetest.New(t)
	if _, _, err := f.svc.SetPrimaryRoot(ctx, newHub.Dir, model.DetectedFromSetting); err != nil {
		t.Fatalf("SetPrimaryRoot: %v", err)
	}

	roots, err := f.svc.Roots(ctx)
	if err != nil {
		t.Fatalf("Roots: %v", err)
	}
	if len(roots) != 2 {
		t.Fatalf("roots = %d, want 2 — the old root is kept as scan-and-serve", len(roots))
	}
	var oldRoot, primary RootView
	for _, r := range roots {
		if r.Path == f.hub.Dir {
			oldRoot = r
		}
		if r.IsPrimary {
			primary = r
		}
	}
	if oldRoot.IsPrimary {
		t.Fatal("the old root is still primary")
	}
	if primary.Path != newHub.Dir {
		t.Fatalf("primary = %q, want %q", primary.Path, newHub.Dir)
	}

	// `models.root_id` is NEVER rewritten by a relocation.
	after := f.find("model.gguf")
	if after.RootID != before.RootID {
		t.Fatalf("root_id moved from %q to %q — a model belongs to the root whose "+
			"filesystem actually holds it", before.RootID, after.RootID)
	}
	if after.State != model.ModelReady {
		t.Fatalf("state = %q, want ready — the files did not move", after.State)
	}
}

// TestPromoteRefusesANonWritableRoot: a read-only library is served forever and
// can never be the root downloads land in.
func TestPromoteRefusesANonWritableRoot(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if isRoot() {
		t.Skip("root ignores the mode bits, so a non-writable directory cannot be produced")
	}

	dir := filepath.Join(t.TempDir(), "readonly-hub")
	other := cachetest.At(t, dir)
	other.Model(repo, revA, "model.gguf", cachetest.GGUF{Arch: "qwen3"})

	added, _, err := f.svc.AddRoot(ctx, other.Dir)
	if err != nil {
		t.Fatalf("AddRoot: %v", err)
	}
	if added.IsPrimary {
		t.Fatal("a newly added root is primary; §3.7 says it is scan-and-serve only")
	}

	// Make it read-only and re-measure through a rescan, which is what refreshes
	// the root's facts.
	if err := chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { chmod(dir, 0o700) })

	row, _, err := f.svc.RequestScan(ctx, added.ID, model.ScanTriggerManual)
	if err != nil {
		t.Fatalf("RequestScan: %v", err)
	}
	if _, err := f.svc.runScan(ctx, nil, row.ID, ScanParams{RootID: added.ID, Path: dir}); err != nil {
		t.Fatalf("runScan: %v", err)
	}

	_, _, err = f.svc.PromoteRoot(ctx, added.ID)
	if me := modelError(t, err); me.Code != model.CodeRootNotWritable {
		t.Fatalf("code = %q, want root_not_writable", me.Code)
	}
}

func TestAddRootRefusesADuplicate(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if _, _, err := f.svc.AddRoot(ctx, f.hub.Dir); err == nil {
		t.Fatal("AddRoot accepted a path that is already registered")
	}
}

func TestAddRootRefusesAMissingDirectory(t *testing.T) {
	f := newFixture(t)

	_, _, err := f.svc.AddRoot(context.Background(), filepath.Join(t.TempDir(), "not-there"))
	if me := modelError(t, err); me.Code != model.CodeSettingInvalid {
		t.Fatalf("code = %q, want setting_invalid", me.Code)
	}
}

// TestSetPrimaryRootRefusesAProtectedPath is D57's registration-time refusal:
// `ProtectSystem=full` mounts /usr, /boot, /efi and /etc read-only, so the
// daemon could not write there whatever the mode says.
func TestSetPrimaryRootRefusesAProtectedPath(t *testing.T) {
	f := newFixture(t)

	_, _, err := f.svc.SetPrimaryRoot(context.Background(), "/usr/share/hf/hub",
		model.DetectedFromManual)
	me := modelError(t, err)
	if me.Code != model.CodeRootPathProtected {
		t.Fatalf("code = %q, want root_path_protected", me.Code)
	}
	if _, ok := me.Details["protected_prefixes"]; !ok {
		t.Fatalf("details = %+v, want the prefixes named so the message can explain itself", me.Details)
	}
}

// TestUsageReportsCatalogAndFilesystemSeparately: "our models take 400 GB" and
// "the disk has 20 GB left" are different facts and the storage view needs both.
func TestUsageReportsCatalogAndFilesystemSeparately(t *testing.T) {
	f := newFixture(t)

	f.hub.Model(repo, revA, "model.gguf", cachetest.GGUF{Arch: "qwen3", Complete: true})
	f.scan()

	usage, err := f.svc.Usage(context.Background())
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if len(usage) != 1 {
		t.Fatalf("usage rows = %d, want 1", len(usage))
	}
	u := usage[0]
	if u.Models != 1 {
		t.Fatalf("models = %d, want 1", u.Models)
	}
	if u.BytesOnDisk <= 0 || u.TotalBytes == nil || *u.TotalBytes <= 0 {
		t.Fatalf("usage = %+v, want both the catalog's bytes and the filesystem's", u)
	}
	if !u.IsPrimary {
		t.Fatal("the primary flag was not carried through")
	}
}
