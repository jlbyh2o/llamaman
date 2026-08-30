package models

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"

	"github.com/jlbyh2o/llamaman/internal/hf/cache"
	"github.com/jlbyh2o/llamaman/internal/hf/cache/cachetest"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// applyVerify runs the verify worker's body and commits what it returns, the way
// the queue would.
func (f *fixture) applyVerify(id string, digest bool) {
	f.t.Helper()
	ctx := context.Background()

	w := NewVerifyWorker(f.svc, func(context.Context) bool { return digest })
	out, err := w.runFor(ctx, id)
	if err != nil {
		f.t.Fatalf("verify worker: %v", err)
	}
	if out.Commit == nil {
		return
	}
	if err := f.db.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		return out.Commit(ctx, tx, model.JobSucceeded)
	}); err != nil {
		f.t.Fatalf("commit the verify: %v", err)
	}
}

func TestVerifyMovesReadyToMissing(t *testing.T) {
	f := newFixture(t)

	f.hub.Model(repo, revA, "model.gguf", cachetest.GGUF{Arch: "qwen3", Complete: true})
	f.scan()
	m := f.find("model.gguf")
	instance := f.seedInstance("uses-it", m.ID, false)

	if err := os.Remove(cache.NewLayout(f.hub.Dir).SnapshotFile(repo, revA, "model.gguf")); err != nil {
		t.Fatal(err)
	}
	f.hash.reset()
	f.applyVerify(m.ID, false)

	after := f.find("model.gguf")
	if after.State != model.ModelMissing {
		t.Fatalf("state = %q, want missing", after.State)
	}

	// D69 again: `state` moved, so the referencing instance's config hash was
	// recomputed in that same transaction.
	var recomputed bool
	for _, id := range f.hash.ids {
		if id == instance {
			recomputed = true
		}
	}
	if !recomputed {
		t.Fatalf("RecomputeConfigHash was not called for %s (called for %v)", instance, f.hash.ids)
	}
}

// TestVerifyChecksumMismatchIsCorrupt: the digest is checkable only because the
// blob's NAME is its sha256 for an LFS object (§7.2) — the expected value is
// already on disk as a file name.
func TestVerifyChecksumMismatchIsCorrupt(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	content := cachetest.BuildGGUF(cachetest.GGUF{Arch: "qwen3", Complete: true})
	sum := sha256.Sum256(content)
	etag := hex.EncodeToString(sum[:])

	blob := f.hub.Blob(repo, etag, content)
	f.hub.Link(repo, revA, "model.gguf", etag)
	f.scan()
	m := f.find("model.gguf")

	// A correct file verifies clean and records that the digest was checked.
	f.applyVerify(m.ID, true)
	if got := f.find("model.gguf").State; got != model.ModelReady {
		t.Fatalf("state = %q, want ready", got)
	}
	d, err := f.svc.Get(ctx, m.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(d.Files) != 1 || !d.Files[0].ChecksumVerified {
		t.Fatalf("checksum_verified = %+v, want true after a digest pass", d.Files)
	}

	// Corrupt one byte in place, keeping the length, so only the digest can
	// catch it — which is the whole reason the digest half exists.
	corrupted := append([]byte(nil), content...)
	corrupted[len(corrupted)-1] ^= 0xFF
	if err := os.WriteFile(blob, corrupted, 0o644); err != nil {
		t.Fatal(err)
	}

	f.applyVerify(m.ID, true)
	if got := f.find("model.gguf").State; got != model.ModelCorrupt {
		t.Fatalf("state = %q, want corrupt", got)
	}

	// And with checksums off the same file passes: a stat cannot see it. That
	// is the honest cost of the setting, and it is why the two halves are
	// separate.
	f.applyVerify(m.ID, false)
	if got := f.find("model.gguf").State; got != model.ModelReady {
		t.Fatalf("state = %q, want ready — a stat cannot detect an in-place corruption", got)
	}
}

func TestVerifySizeMismatchIsCorrupt(t *testing.T) {
	f := newFixture(t)

	f.hub.Model(repo, revA, "model.gguf", cachetest.GGUF{Arch: "qwen3", Complete: true})
	f.scan()
	m := f.find("model.gguf")

	blob := cache.NewLayout(f.hub.Dir).BlobPath(repo, cachetest.FakeEtag(repo, revA, "model.gguf"))
	if err := os.WriteFile(blob, []byte("truncated"), 0o644); err != nil {
		t.Fatal(err)
	}

	f.applyVerify(m.ID, false)
	if got := f.find("model.gguf").State; got != model.ModelCorrupt {
		t.Fatalf("state = %q, want corrupt — the recorded size no longer matches", got)
	}
}

func TestMetadataReadsTheFileAndFallsBack(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.hub.Model(repo, revA, "model.gguf", cachetest.GGUF{Arch: "qwen3", Layers: 3})
	f.scan()
	m := f.find("model.gguf")

	kv, err := f.svc.Metadata(ctx, m.ID)
	if err != nil {
		t.Fatalf("Metadata: %v", err)
	}
	if kv["general.architecture"] != "qwen3" {
		t.Fatalf("general.architecture = %v, want qwen3", kv["general.architecture"])
	}
	// The tokenizer table is in the file and therefore in this answer — and it
	// is NOT in the row, which is why the endpoint re-reads rather than serving
	// a column a 300-model scan would have had to fill.
	if _, ok := kv["tokenizer.ggml.tokens"]; !ok {
		t.Fatalf("the metadata map is missing the tokenizer table: %v", keysOf(kv))
	}
	if m.MetadataJSON != nil {
		t.Fatal("the scan stored a metadata blob; it re-reads the file instead")
	}

	// With the file gone and nothing recorded, the endpoint says so rather than
	// returning an empty map that reads as an answer.
	if err := os.RemoveAll(cache.NewLayout(f.hub.Dir).RepoDir(repo)); err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.Metadata(ctx, m.ID)
	if me := modelError(t, err); me.Code != model.CodeModelMissing {
		t.Fatalf("code = %q, want model_missing", me.Code)
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
