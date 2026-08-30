package cache_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jlbyh2o/llamaman/internal/hf/cache"
	"github.com/jlbyh2o/llamaman/internal/hf/cache/cachetest"
	"github.com/jlbyh2o/llamaman/internal/model"
)

const (
	repo = "bartowski/Qwen3-8B-GGUF"
	// Three snapshots of one repository, which is the case section 7.2 spells
	// out: a pinned revision, an older `main` left behind by a previous fetch,
	// and a branch. They must produce three DISTINCT rows.
	revA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	revB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	revC = "cccccccccccccccccccccccccccccccccccccccc"
)

// threeSnapshotHub is section 15's case: one repo directory with three snapshot
// directories, and `refs/main` pointing at exactly one of them.
func threeSnapshotHub(t *testing.T) *cachetest.Hub {
	t.Helper()
	h := cachetest.New(t)
	for _, rev := range []string{revA, revB, revC} {
		h.Model(repo, rev, "Qwen3-8B-Q4_K_M.gguf", cachetest.GGUF{Arch: "qwen3", Layers: 2})
	}
	h.Ref(repo, "main", revB)
	// A negative-cache marker, which the walk must skip entirely.
	h.NoExist(repo, revA, "config.json")
	return h
}

func TestScanThreeSnapshotsAreThreeModels(t *testing.T) {
	t.Parallel()
	h := threeSnapshotHub(t)

	res, err := cache.Scan(context.Background(), h.Dir, cache.ScanOptions{})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(res.Repos) != 1 {
		t.Fatalf("repos = %d, want 1", len(res.Repos))
	}
	r := res.Repos[0]
	if r.RepoID != repo {
		t.Fatalf("repo id = %q, want %q", r.RepoID, repo)
	}
	if len(r.Snapshots) != 3 {
		t.Fatalf("snapshots = %d, want 3 — N snapshot directories are N distinct models", len(r.Snapshots))
	}

	// THE REVISION IS THE DIRECTORY NAME. Taking it from refs/main would stamp
	// revB onto all three and collapse them onto one identity.
	for i, want := range []string{revA, revB, revC} {
		if r.Snapshots[i].Revision != want {
			t.Fatalf("snapshot %d revision = %q, want %q", i, r.Snapshots[i].Revision, want)
		}
	}

	// `refs/` fills the DISPLAY field, and only for the snapshot it points at.
	for _, s := range r.Snapshots {
		switch s.Revision {
		case revB:
			if len(s.RefNames) != 1 || s.RefNames[0] != "main" {
				t.Fatalf("snapshot %s ref names = %v, want [main]", s.Revision, s.RefNames)
			}
		default:
			if len(s.RefNames) != 0 {
				t.Fatalf("snapshot %s ref names = %v, want none — no ref points at it",
					s.Revision, s.RefNames)
			}
		}
	}

	// `.no_exist/` is a negative cache, not snapshots.
	for _, s := range r.Snapshots {
		for _, f := range s.Files {
			if filepath.Base(f.Name) == "config.json" {
				t.Fatalf("the walk entered .no_exist and reported %q", f.Path)
			}
		}
	}
}

func TestScanParsesResolvesAndClassifies(t *testing.T) {
	t.Parallel()

	h := cachetest.New(t)
	h.Model(repo, revA, "Qwen3-8B-Q4_K_M.gguf", cachetest.GGUF{Arch: "qwen3", Layers: 3})
	h.Model(repo, revA, "mmproj-Qwen3-f16.gguf", cachetest.GGUF{Arch: "clip", Vision: true})

	res, err := cache.Scan(context.Background(), h.Dir, cache.ScanOptions{})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	files := res.Repos[0].Snapshots[0].Files
	if len(files) != 2 {
		t.Fatalf("files = %d, want 2", len(files))
	}

	byName := map[string]cache.FileEntry{}
	for _, f := range files {
		byName[f.Name] = f
	}

	weights := byName["Qwen3-8B-Q4_K_M.gguf"]
	if weights.Shape == nil {
		t.Fatalf("the weights file did not parse: %v", weights.ParseErr)
	}
	if weights.Shape.Architecture != "qwen3" || weights.Shape.BlockCount != 3 {
		t.Fatalf("shape = %+v, want qwen3 with 3 blocks", weights.Shape)
	}
	// The link was resolved into `blobs/`, which is what makes the refcount and
	// the shared-cache short circuit possible at all.
	if weights.Etag == "" || weights.BlobPath == "" {
		t.Fatalf("the snapshot entry did not resolve to a blob: %+v", weights)
	}
	if got := cache.Classify(weights.Name, weights.Shape); got != model.ModelText {
		t.Fatalf("classified the weights as %q, want text", got)
	}

	proj := byName["mmproj-Qwen3-f16.gguf"]
	if got := cache.Classify(proj.Name, proj.Shape); got != model.ModelMmproj {
		t.Fatalf("classified the projector as %q, want mmproj", got)
	}
}

func TestClassify(t *testing.T) {
	t.Parallel()

	pooling := uint32(1)
	cases := []struct {
		name     string
		filename string
		spec     *cachetest.GGUF
		want     model.ModelKind
	}{
		{"metadata beats the name", "model.gguf",
			&cachetest.GGUF{Arch: "clip", Vision: true}, model.ModelMmproj},
		{"the name alone is enough", "mmproj-model-f16.gguf",
			&cachetest.GGUF{Arch: "llama"}, model.ModelMmproj},
		{"pooling_type means embedding", "e5.gguf",
			&cachetest.GGUF{Arch: "llama", Pooling: &pooling}, model.ModelEmbedding},
		{"a known embedding architecture", "bge.gguf",
			&cachetest.GGUF{Arch: "bert"}, model.ModelEmbedding},
		{"anything else is text", "Qwen3-8B-Q4_K_M.gguf",
			&cachetest.GGUF{Arch: "qwen3"}, model.ModelText},
		// A file that did not parse is `unknown`, not `text`: section 2.6 keeps
		// the member so a scan can record a GGUF it cannot read rather than
		// lose it.
		{"unparsable is unknown", "broken.gguf", nil, model.ModelUnknown},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.spec == nil {
				if got := cache.Classify(tc.filename, nil); got != tc.want {
					t.Fatalf("Classify(%q, nil) = %q, want %q", tc.filename, got, tc.want)
				}
				return
			}
			h := cachetest.New(t)
			h.Model("org/name", revA, tc.filename, *tc.spec)
			res, err := cache.Scan(context.Background(), h.Dir, cache.ScanOptions{})
			if err != nil {
				t.Fatalf("Scan: %v", err)
			}
			f := res.Repos[0].Snapshots[0].Files[0]
			if got := cache.Classify(f.Name, f.Shape); got != tc.want {
				t.Fatalf("Classify(%q) = %q, want %q", tc.filename, got, tc.want)
			}
		})
	}
}

func TestScanStrays(t *testing.T) {
	t.Parallel()

	h := cachetest.New(t)
	etag := h.Model(repo, revA, "Qwen3-8B-Q4_K_M.gguf", cachetest.GGUF{Arch: "qwen3"})

	// An orphan blob: content no snapshot points at.
	h.Blob(repo, "orphan-blob", []byte("forty gigabytes, notionally"))
	// A transfer in progress. It is NOT a stray — it may belong to a concurrent
	// `hf download` (D26).
	h.Incomplete(repo, "in-flight", []byte("partial"))
	// A GGUF outside any snapshot directory.
	if err := os.WriteFile(filepath.Join(h.Dir, "loose.gguf"), []byte("GGUF"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A broken link: the snapshot entry survives its blob.
	h.Model(repo, revB, "Qwen3-8B-Q8_0.gguf", cachetest.GGUF{Arch: "qwen3"})
	h.BreakLink(repo, cachetest.FakeEtag(repo, revB, "Qwen3-8B-Q8_0.gguf"))

	res, err := cache.Scan(context.Background(), h.Dir, cache.ScanOptions{})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	got := map[model.StrayReason]int{}
	for _, s := range res.Strays {
		got[s.Reason]++
		if filepath.Ext(s.Path) == cache.IncompleteSuffix {
			t.Fatalf("an .incomplete file was reported as a stray: %s", s.Path)
		}
	}

	for reason, want := range map[model.StrayReason]int{
		model.StrayOrphanBlob:      1,
		model.StrayOutsideSnapshot: 1,
		model.StrayBrokenSymlink:   1,
	} {
		if got[reason] != want {
			t.Errorf("strays with reason %q = %d, want %d (all: %+v)", reason, got[reason], want, res.Strays)
		}
	}

	// The live blob is not an orphan.
	for _, s := range res.Strays {
		if filepath.Base(s.Path) == etag {
			t.Fatalf("the blob a live snapshot points at was reported as an orphan")
		}
	}
}

func TestScanCountsSharedBlobsOnce(t *testing.T) {
	t.Parallel()

	// Two snapshots linking ONE blob. The disk holds the content once, and a
	// byte total that doubled it would make every storage number wrong on a
	// cache that has ever re-fetched `main`.
	h := cachetest.New(t)
	content := cachetest.BuildGGUF(cachetest.GGUF{Arch: "qwen3", Layers: 2})
	h.Blob(repo, "shared", content)
	h.Link(repo, revA, "model.gguf", "shared")
	h.Link(repo, revB, "model.gguf", "shared")

	res, err := cache.Scan(context.Background(), h.Dir, cache.ScanOptions{})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	blob := filepath.Join(h.Dir, cache.RepoFolderName(repo), "blobs", "shared")
	st, err := os.Stat(blob)
	if err != nil {
		t.Fatal(err)
	}
	if want := cache.AllocatedBytes(st); res.BytesTotal != want {
		t.Fatalf("bytes total = %d, want %d — a blob two snapshots share occupies the disk once",
			res.BytesTotal, want)
	}
}

func TestScanUnparsableGGUFIsAStrayNotADrop(t *testing.T) {
	t.Parallel()

	h := cachetest.New(t)
	h.Blob(repo, "junk", []byte("this is not a GGUF file at all"))
	h.Link(repo, revA, "broken.gguf", "junk")

	res, err := cache.Scan(context.Background(), h.Dir, cache.ScanOptions{})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	files := res.Repos[0].Snapshots[0].Files
	if len(files) != 1 {
		t.Fatalf("files = %d, want the unparsable file to still be reported", len(files))
	}
	if files[0].Shape != nil || files[0].ParseErr == nil {
		t.Fatalf("expected a parse error and no shape, got %+v", files[0])
	}
	var found bool
	for _, s := range res.Strays {
		if s.Reason == model.StrayUnparsable {
			found = true
		}
	}
	if !found {
		t.Fatalf("no `unparsable` stray was recorded: %+v", res.Strays)
	}
}

func TestScanOfAMissingRootIsNotAFailure(t *testing.T) {
	t.Parallel()

	// A registered root whose disk is not mounted. The reconciliation that
	// follows marks its models `missing`; the scan itself must not error, or a
	// removable drive would fail every boot.
	res, err := cache.Scan(context.Background(), filepath.Join(t.TempDir(), "gone"), cache.ScanOptions{})
	if err != nil {
		t.Fatalf("Scan of a missing root = %v, want no error", err)
	}
	if len(res.Repos) != 0 {
		t.Fatalf("repos = %d, want 0", len(res.Repos))
	}
}

func TestScanCopyModeFallback(t *testing.T) {
	t.Parallel()

	// F17: on a filesystem that refuses symlinks the snapshot entry is a plain
	// file. It has no etag and no blob path, and it is its own content — so it
	// always counts toward the byte total.
	h := cachetest.New(t)
	h.File(repo, revA, "model.gguf", cachetest.BuildGGUF(cachetest.GGUF{Arch: "llama"}))

	res, err := cache.Scan(context.Background(), h.Dir, cache.ScanOptions{})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	f := res.Repos[0].Snapshots[0].Files[0]
	if f.Etag != "" || f.BlobPath != "" {
		t.Fatalf("a plain file reported a blob: %+v", f)
	}
	if f.Shape == nil {
		t.Fatalf("the copy-mode file did not parse: %v", f.ParseErr)
	}
	if res.BytesTotal == 0 {
		t.Fatal("a copy-mode file contributed nothing to the byte total")
	}
}

func TestScanProgress(t *testing.T) {
	t.Parallel()

	h := threeSnapshotHub(t)
	var calls int
	_, err := cache.Scan(context.Background(), h.Dir, cache.ScanOptions{
		Progress: func(cache.Progress) { calls++ },
		// A clock that always answers the same instant would throttle every
		// report away; the final report is forced, so at least one arrives.
	})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if calls == 0 {
		t.Fatal("no progress was reported")
	}
}

func TestScanRespectsCancellation(t *testing.T) {
	t.Parallel()

	h := threeSnapshotHub(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := cache.Scan(ctx, h.Dir, cache.ScanOptions{}); err == nil {
		t.Fatal("a canceled scan returned no error")
	}
}
