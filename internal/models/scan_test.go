package models

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jlbyh2o/llamaman/internal/hf/cache"
	"github.com/jlbyh2o/llamaman/internal/hf/cache/cachetest"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

const (
	repo = "bartowski/Qwen3-8B-GGUF"
	revA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	revB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// TestScanGroupsShardsAndPairsMmproj is the reconciliation's headline claim:
// a five-shard set is ONE model row whose primary file is shard 1, the
// projector beside it is its own row, and the two are linked.
func TestScanGroupsShardsAndPairsMmproj(t *testing.T) {
	f := newFixture(t)

	const base = "Qwen3-8B-Q4_K_M"
	for i := 1; i <= 3; i++ {
		f.hub.Model(repo, revA, cache.ShardName(base, i, 3), cachetest.GGUF{
			Arch: "qwen3", Layers: 2, FileType: 15, // Q4_K_M
			SplitNo: uint16(i - 1), SplitCount: 3,
		})
	}
	f.hub.Model(repo, revA, "mmproj-Qwen3-f16.gguf", cachetest.GGUF{Arch: "clip", Vision: true})
	f.hub.Ref(repo, "main", revA)

	counters := f.scan()
	if counters.ModelsFound != 2 {
		t.Fatalf("models found = %d, want 2 (the shard set and the projector)", counters.ModelsFound)
	}
	if counters.ModelsAdded != 2 {
		t.Fatalf("models added = %d, want 2", counters.ModelsAdded)
	}

	rows := f.models()
	if len(rows) != 2 {
		t.Fatalf("catalog rows = %d, want 2 — three shards are ONE logical model", len(rows))
	}

	weights := f.find(cache.ShardName(base, 1, 3))
	if weights.Kind != model.ModelText {
		t.Fatalf("weights kind = %q, want text", weights.Kind)
	}
	if weights.ShardCount != 3 {
		t.Fatalf("shard count = %d, want 3", weights.ShardCount)
	}
	if weights.State != model.ModelReady {
		t.Fatalf("state = %q, want ready — every declared shard was found", weights.State)
	}
	if weights.Revision != revA {
		t.Fatalf("revision = %q, want the snapshot directory name %q", weights.Revision, revA)
	}
	if weights.RefName == nil || *weights.RefName != "main" {
		t.Fatalf("ref_name = %v, want main", weights.RefName)
	}
	if weights.MmprojModelID == nil {
		t.Fatal("the sole projector in the snapshot was not paired")
	}
	if !weights.MmprojAuto {
		t.Fatal("an automatic pairing was recorded as manual")
	}

	proj := f.find("mmproj-Qwen3-f16.gguf")
	if proj.Kind != model.ModelMmproj {
		t.Fatalf("projector kind = %q, want mmproj", proj.Kind)
	}
	if *weights.MmprojModelID != proj.ID {
		t.Fatalf("paired projector = %q, want %q", *weights.MmprojModelID, proj.ID)
	}

	// The shard set's files are all recorded, in order, with the split totals.
	d, err := f.svc.Get(context.Background(), weights.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(d.Files) != 3 {
		t.Fatalf("model_files = %d, want 3", len(d.Files))
	}
	for i, file := range d.Files {
		if file.ShardIndex != i+1 || file.ShardTotal != 3 {
			t.Fatalf("file %d = shard %d of %d, want %d of 3", i, file.ShardIndex, file.ShardTotal, i+1)
		}
		if file.State != model.FilePresent {
			t.Fatalf("file %d state = %q, want present", i, file.State)
		}
		if file.Etag == nil || file.BlobPath == nil {
			t.Fatalf("file %d did not resolve to a blob: %+v", i, file)
		}
	}
}

// TestScanThreeSnapshotsAreThreeRows is section 15's case, at the catalog level:
// N snapshot directories produce N distinct `models` rows, each labeled by its
// DIRECTORY name, and `ref_name` is set only for the one `refs/main` points at.
func TestScanThreeSnapshotsAreThreeRows(t *testing.T) {
	f := newFixture(t)

	const revC = "cccccccccccccccccccccccccccccccccccccccc"
	for _, rev := range []string{revA, revB, revC} {
		f.hub.Model(repo, rev, "model.gguf", cachetest.GGUF{Arch: "qwen3"})
	}
	f.hub.Ref(repo, "main", revB)

	f.scan()

	rows := f.models()
	if len(rows) != 3 {
		t.Fatalf("catalog rows = %d, want 3 — one per snapshot directory", len(rows))
	}

	seen := map[string]View{}
	for _, r := range rows {
		if r.RepoID != repo {
			t.Fatalf("repo id = %q, want %q", r.RepoID, repo)
		}
		seen[r.Revision] = r
	}
	for _, rev := range []string{revA, revB, revC} {
		if _, ok := seen[rev]; !ok {
			t.Fatalf("no row for revision %q — the revision must be the directory name, "+
				"never refs/main", rev)
		}
	}
	if seen[revB].RefName == nil || *seen[revB].RefName != "main" {
		t.Fatalf("revision %q ref_name = %v, want main", revB, seen[revB].RefName)
	}
	for _, rev := range []string{revA, revC} {
		if seen[rev].RefName != nil {
			t.Fatalf("revision %q ref_name = %v, want null — no ref points at it",
				rev, *seen[rev].RefName)
		}
	}
}

// TestRescanAfterExternalDeletionMarksMissing is the reconciliation contract:
// a row whose files vanished becomes `missing`, and is NEVER deleted, because a
// disk may have been unplugged and the row is how the user finds out which one.
func TestRescanAfterExternalDeletionMarksMissing(t *testing.T) {
	f := newFixture(t)

	f.hub.Model(repo, revA, "keep.gguf", cachetest.GGUF{Arch: "qwen3"})
	f.hub.Model(repo, revB, "gone.gguf", cachetest.GGUF{Arch: "qwen3"})
	f.scan()

	gone := f.find("gone.gguf")
	kept := f.find("keep.gguf")
	instance := f.seedInstance("uses-gone", gone.ID, false)

	// Somebody else removed the whole snapshot — `hf cache delete`, or an
	// unmounted drive.
	l := cache.NewLayout(f.hub.Dir)
	if err := os.RemoveAll(l.SnapshotDir(repo, revB)); err != nil {
		t.Fatal(err)
	}

	f.hash.reset()
	counters := f.scan()

	if counters.ModelsMissing != 1 {
		t.Fatalf("models missing = %d, want 1", counters.ModelsMissing)
	}

	after := f.models()
	if len(after) != 2 {
		t.Fatalf("catalog rows = %d, want 2 — a missing model is never deleted", len(after))
	}

	var found bool
	for _, r := range after {
		if r.ID != gone.ID {
			continue
		}
		found = true
		if r.State != model.ModelMissing {
			t.Fatalf("state = %q, want missing", r.State)
		}
		// The path columns are kept, which is what lets the UI say WHICH disk
		// to plug back in.
		if r.SnapshotDir == "" || r.PrimaryFile != "gone.gguf" {
			t.Fatalf("the missing row lost its path: %+v", r)
		}
	}
	if !found {
		t.Fatal("the row for the removed model was deleted rather than marked missing")
	}

	// D69: `state` moved, so every instance referencing it had its config hash
	// recomputed in that same transaction.
	var recomputed bool
	for _, id := range f.hash.ids {
		if id == instance {
			recomputed = true
		}
	}
	if !recomputed {
		t.Fatalf("RecomputeConfigHash was not called for the referencing instance "+
			"(called for %v) — the stored hash now describes a path that is gone", f.hash.ids)
	}

	// The surviving model is untouched.
	if f.find("keep.gguf").State != model.ModelReady {
		t.Fatalf("the surviving model's state changed; it should still be ready")
	}
	_ = kept
}

// TestRescanRestoresAMissingModel: a disk that comes back is a model that comes
// back, through the ordinary upsert path and with no manual step.
func TestRescanRestoresAMissingModel(t *testing.T) {
	f := newFixture(t)

	spec := cachetest.GGUF{Arch: "qwen3"}
	f.hub.Model(repo, revA, "model.gguf", spec)
	f.scan()

	l := cache.NewLayout(f.hub.Dir)
	snapshot := l.SnapshotDir(repo, revA)
	if err := os.RemoveAll(snapshot); err != nil {
		t.Fatal(err)
	}
	f.scan()
	if got := f.find("model.gguf").State; got != model.ModelMissing {
		t.Fatalf("state = %q, want missing", got)
	}

	f.hub.Link(repo, revA, "model.gguf", cachetest.FakeEtag(repo, revA, "model.gguf"))
	f.scan()

	back := f.find("model.gguf")
	if back.State != model.ModelReady {
		t.Fatalf("state = %q, want ready once the files returned", back.State)
	}
	if len(f.models()) != 1 {
		t.Fatalf("catalog rows = %d, want 1 — the returning model reused its row", len(f.models()))
	}
}

// TestRescanIsIdempotent: scanning an unchanged cache twice adds nothing. It is
// what makes a `cache_scan` job free to restart after a daemon restart.
func TestRescanIsIdempotent(t *testing.T) {
	f := newFixture(t)

	f.hub.Model(repo, revA, "model.gguf", cachetest.GGUF{Arch: "qwen3"})
	first := f.scan()
	second := f.scan()

	if first.ModelsAdded != 1 {
		t.Fatalf("first scan added %d, want 1", first.ModelsAdded)
	}
	if second.ModelsAdded != 0 {
		t.Fatalf("second scan added %d, want 0 — the identity is stable", second.ModelsAdded)
	}
	if second.ModelsFound != 1 {
		t.Fatalf("second scan found %d, want 1", second.ModelsFound)
	}
	if len(f.models()) != 1 {
		t.Fatalf("catalog rows = %d, want 1", len(f.models()))
	}
}

// TestScanRecordsGGUFGeometry pins the columns section 2.6 keeps for the fit
// calculator, and in particular that `n_head_kv_json` is the key VERBATIM (D30).
func TestScanRecordsGGUFGeometry(t *testing.T) {
	f := newFixture(t)

	f.hub.Model(repo, revA, "model.gguf", cachetest.GGUF{Arch: "qwen3", Layers: 4})
	f.scan()

	m := f.find("model.gguf")
	if m.GGUFParsedAt == nil {
		t.Fatal("gguf_parsed_at is null after a scan that parsed the header")
	}
	if m.Arch == nil || *m.Arch != "qwen3" {
		t.Fatalf("arch = %v, want qwen3", m.Arch)
	}
	if m.NLayer == nil || *m.NLayer != 4 {
		t.Fatalf("n_layer = %v, want 4", m.NLayer)
	}
	if m.TokenizerModel == nil || *m.TokenizerModel != "gpt2" {
		t.Fatalf("tokenizer_model = %v, want gpt2 — D34's draft check compares it", m.TokenizerModel)
	}
	// The fixture writes a SCALAR head_count_kv, so the column holds a number.
	// An array would be stored as an array; broadcasting one into the other is
	// what D30 forbids.
	if m.NHeadKVJSON == nil || *m.NHeadKVJSON != "2" {
		t.Fatalf("n_head_kv_json = %v, want the scalar 2 verbatim", m.NHeadKVJSON)
	}
	if m.TensorSummaryJSON == nil {
		t.Fatal("tensor_summary_json is null; section 8.2's bucketing was not recorded")
	}
	// A scan makes NO network calls and stores no card, so this stays null
	// until a model is opened.
	if m.CardFetchedAt != nil {
		t.Fatal("the scan fetched a model card; it must work offline")
	}
}

// TestScanRecordsStrays and their removal on a later pass: the row is the record
// of a problem, and it goes when the problem does.
func TestScanRecordsAndClearsStrays(t *testing.T) {
	f := newFixture(t)

	f.hub.Model(repo, revA, "model.gguf", cachetest.GGUF{Arch: "qwen3"})
	f.hub.Blob(repo, "orphan", []byte("nothing points at this"))
	counters := f.scan()

	if counters.StraysFound != 1 {
		t.Fatalf("strays found = %d, want 1", counters.StraysFound)
	}
	strays, err := f.svc.Strays(context.Background(), "")
	if err != nil {
		t.Fatalf("Strays: %v", err)
	}
	if len(strays) != 1 || strays[0].Reason != model.StrayOrphanBlob {
		t.Fatalf("strays = %+v, want one orphan blob", strays)
	}

	if err := os.Remove(cache.NewLayout(f.hub.Dir).BlobPath(repo, "orphan")); err != nil {
		t.Fatal(err)
	}
	f.scan()

	strays, err = f.svc.Strays(context.Background(), "")
	if err != nil {
		t.Fatalf("Strays: %v", err)
	}
	if len(strays) != 0 {
		t.Fatalf("strays = %+v, want none once the file is gone", strays)
	}
}

// TestScanIncompleteShardSet: a set missing a member is `incomplete`, not
// `ready`. A half-downloaded model the user can see beats one the catalog
// pretends is not there — and an instance must not be able to load it.
func TestScanIncompleteShardSet(t *testing.T) {
	f := newFixture(t)

	const base = "Model-Q4_K_M"
	for _, i := range []int{1, 2} { // shard 3 of 3 is absent
		f.hub.Model(repo, revA, cache.ShardName(base, i, 3), cachetest.GGUF{
			Arch: "llama", SplitNo: uint16(i - 1), SplitCount: 3,
		})
	}
	f.scan()

	m := f.find(cache.ShardName(base, 1, 3))
	if m.State != model.ModelIncomplete {
		t.Fatalf("state = %q, want incomplete", m.State)
	}
	if m.ShardCount != 3 {
		t.Fatalf("shard_count = %d, want the DECLARED 3, so the row records that one is missing",
			m.ShardCount)
	}
}

// TestScanKeepsAManualPairing: `mmproj_auto = 0` is a human's decision, and a
// rescan does not get to overrule it.
func TestScanKeepsAManualPairing(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.hub.Model(repo, revA, "model.gguf", cachetest.GGUF{Arch: "qwen3"})
	f.hub.Model(repo, revA, "mmproj-a-f16.gguf", cachetest.GGUF{Arch: "clip", Vision: true})
	f.scan()

	weights := f.find("model.gguf")
	if weights.MmprojModelID == nil {
		t.Fatal("the sole projector was not auto-paired")
	}

	// The user detaches it by hand.
	if _, err := f.svc.PairMmproj(ctx, weights.ID, ""); err != nil {
		t.Fatalf("PairMmproj: %v", err)
	}
	f.scan()

	after := f.find("model.gguf")
	if after.MmprojAuto {
		t.Fatal("a rescan reset mmproj_auto")
	}
	if after.MmprojModelID != nil {
		t.Fatal("a rescan re-attached a projector the user had detached")
	}
}

// TestScanSeveralProjectorsProduceAPicker: section 7.2 refuses to guess. Two
// f16 projectors are a tie, and breaking it by file name would be exactly the
// guess the rule forbids.
func TestScanSeveralProjectorsProduceAPicker(t *testing.T) {
	f := newFixture(t)

	f.hub.Model(repo, revA, "model.gguf", cachetest.GGUF{Arch: "qwen3"})
	f.hub.Model(repo, revA, "mmproj-vision-f16.gguf", cachetest.GGUF{Arch: "clip", Vision: true})
	f.hub.Model(repo, revA, "mmproj-audio-f16.gguf", cachetest.GGUF{Arch: "clip", Vision: true})
	f.scan()

	weights := f.find("model.gguf")
	if weights.MmprojModelID != nil {
		t.Fatal("a projector was guessed where the rule says to show a picker")
	}

	d, err := f.svc.Get(context.Background(), weights.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(d.MmprojCandidates) != 2 {
		t.Fatalf("picker candidates = %d, want 2", len(d.MmprojCandidates))
	}
}

// TestScanOfAnUnmountedRootIsNotAFailure: a removable drive must not fail every
// boot. The walk reports nothing and the reconciliation marks its models
// missing.
func TestScanOfAnUnmountedRootIsNotAFailure(t *testing.T) {
	f := newFixture(t)

	f.hub.Model(repo, revA, "model.gguf", cachetest.GGUF{Arch: "qwen3"})
	f.scan()
	if err := os.RemoveAll(f.hub.Dir); err != nil {
		t.Fatal(err)
	}

	counters := f.scan()
	if counters.ModelsMissing != 1 {
		t.Fatalf("models missing = %d, want 1", counters.ModelsMissing)
	}
	if f.find("model.gguf").State != model.ModelMissing {
		t.Fatal("the model of an unmounted root was not marked missing")
	}
}

func TestScanRowMovesWithTheJob(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	row, ref, err := f.svc.RequestScan(ctx, "", model.ScanTriggerBoot)
	if err != nil {
		t.Fatalf("RequestScan: %v", err)
	}
	if row.State != model.ScanQueued {
		t.Fatalf("scan state = %q, want queued", row.State)
	}
	if row.Trigger != model.ScanTriggerBoot {
		t.Fatalf("trigger = %q, want boot", row.Trigger)
	}
	// This fixture has no queue, so the receipt names the domain row and no job
	// — the honest state, rather than a synchronous fallback.
	if ref.JobID != "" || ref.SubjectID != row.ID {
		t.Fatalf("job receipt = %+v, want the subject alone", ref)
	}

	back, err := f.svc.Scan(ctx, row.ID)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if back.ID != row.ID {
		t.Fatalf("scan id = %q, want %q", back.ID, row.ID)
	}

	if _, err := f.svc.Scan(ctx, "01JNOSUCHSCAN0000000000000"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Scan of an unknown id = %v, want ErrNotFound", err)
	}
}
