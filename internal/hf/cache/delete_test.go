package cache_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jlbyh2o/llamaman/internal/hf/cache"
	"github.com/jlbyh2o/llamaman/internal/hf/cache/cachetest"
)

// D28's whole claim in one test: a blob two snapshots share survives the
// deletion of either, and the preview says so before anything is executed.
func TestPlanDeleteKeepsSharedBlobs(t *testing.T) {
	t.Parallel()

	h := cachetest.New(t)
	// `shared` is linked from both revisions — a tokenizer, in practice, or a
	// shard that did not change between two `main` fetches. `onlyA` belongs to
	// revision A alone.
	h.Blob(repo, "shared", make([]byte, 8192))
	h.Blob(repo, "onlyA", make([]byte, 4096))
	h.Link(repo, revA, "tokenizer.gguf", "shared")
	h.Link(repo, revA, "model.gguf", "onlyA")
	h.Link(repo, revB, "tokenizer.gguf", "shared")

	plan, err := cache.PlanDelete(h.Dir, repo, revA, []string{"tokenizer.gguf", "model.gguf"})
	if err != nil {
		t.Fatalf("PlanDelete: %v", err)
	}

	if plan.Files() != 2 {
		t.Fatalf("files = %d, want 2", plan.Files())
	}
	if len(plan.Blobs) != 1 || plan.Blobs[0].Etag != "onlyA" {
		t.Fatalf("blobs to remove = %+v, want only onlyA", plan.Blobs)
	}
	if len(plan.SharedKept) != 1 || plan.SharedKept[0].Etag != "shared" {
		t.Fatalf("shared blobs kept = %+v, want shared", plan.SharedKept)
	}
	if plan.SharedKept[0].Refs != 2 || plan.SharedKept[0].Dropped != 1 {
		t.Fatalf("shared refcount = %d refs, %d dropped; want 2 and 1",
			plan.SharedKept[0].Refs, plan.SharedKept[0].Dropped)
	}
	// The whole repo directory must not go: revision B is still there.
	if plan.RemoveRepoDir {
		t.Fatal("the plan would remove the repo directory while another snapshot survives")
	}

	if err := plan.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	l := cache.NewLayout(h.Dir)
	if _, err := os.Stat(l.BlobPath(repo, "onlyA")); !os.IsNotExist(err) {
		t.Error("the unshared blob survived the delete")
	}
	if _, err := os.Stat(l.BlobPath(repo, "shared")); err != nil {
		t.Errorf("THE SHARED BLOB WAS REMOVED out from under revision B: %v", err)
	}
	// And revision B still resolves through it, which is the property the
	// refcount exists to protect.
	if _, err := os.Stat(l.SnapshotFile(repo, revB, "tokenizer.gguf")); err != nil {
		t.Errorf("revision B's snapshot entry no longer resolves: %v", err)
	}
	if _, err := os.Stat(l.SnapshotDir(repo, revA)); !os.IsNotExist(err) {
		t.Error("the emptied snapshot directory was left behind")
	}
}

func TestPlanDeleteLastSnapshotRemovesTheRepoDirectory(t *testing.T) {
	t.Parallel()

	h := cachetest.New(t)
	h.Blob(repo, "only", make([]byte, 4096))
	h.Link(repo, revA, "model.gguf", "only")

	plan, err := cache.PlanDelete(h.Dir, repo, revA, nil)
	if err != nil {
		t.Fatalf("PlanDelete: %v", err)
	}
	if !plan.RemoveRepoDir {
		t.Fatal("the last snapshot's delete does not remove the repo directory")
	}
	if err := plan.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, err := os.Stat(cache.NewLayout(h.Dir).RepoDir(repo)); !os.IsNotExist(err) {
		t.Fatal("the repo directory survived a delete that emptied it")
	}
}

func TestPlanDeleteBytesAreAllocationNotLogicalSize(t *testing.T) {
	t.Parallel()

	h := cachetest.New(t)
	blob := h.Blob(repo, "e1", make([]byte, 12345))
	h.Link(repo, revA, "model.gguf", "e1")

	plan, err := cache.PlanDelete(h.Dir, repo, revA, []string{"model.gguf"})
	if err != nil {
		t.Fatalf("PlanDelete: %v", err)
	}
	st, err := os.Stat(blob)
	if err != nil {
		t.Fatal(err)
	}
	// "Will free N GB" is a claim about the disk, so it has to be the
	// allocation. A logical size would over-promise on a sparse file and
	// under-promise on a small one.
	if want := cache.AllocatedBytes(st); plan.Bytes() != want {
		t.Fatalf("plan bytes = %d, want the allocated %d", plan.Bytes(), want)
	}
}

func TestPlanDeleteLeavesOtherQuantsInTheSameSnapshot(t *testing.T) {
	t.Parallel()

	// One snapshot legitimately holds several quants. Deleting one may not take
	// the others, and the snapshot directory must survive.
	h := cachetest.New(t)
	h.Blob(repo, "q4", make([]byte, 4096))
	h.Blob(repo, "q8", make([]byte, 8192))
	h.Link(repo, revA, "Model-Q4_K_M.gguf", "q4")
	h.Link(repo, revA, "Model-Q8_0.gguf", "q8")

	plan, err := cache.PlanDelete(h.Dir, repo, revA, []string{"Model-Q4_K_M.gguf"})
	if err != nil {
		t.Fatalf("PlanDelete: %v", err)
	}
	if plan.RemoveRepoDir || len(plan.SnapshotDirs) != 0 {
		t.Fatalf("the plan would remove directories another quant still occupies: %+v", plan)
	}
	if err := plan.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	l := cache.NewLayout(h.Dir)
	if _, err := os.Stat(l.SnapshotFile(repo, revA, "Model-Q8_0.gguf")); err != nil {
		t.Errorf("the other quant was removed: %v", err)
	}
	if _, err := os.Stat(l.BlobPath(repo, "q8")); err != nil {
		t.Errorf("the other quant's blob was removed: %v", err)
	}
}

func TestPlanDeleteIgnoresIncompleteFilesOfOtherTransfers(t *testing.T) {
	t.Parallel()

	h := cachetest.New(t)
	h.Blob(repo, "mine", make([]byte, 4096))
	h.Link(repo, revA, "model.gguf", "mine")
	// Another tool's transfer in progress, for a blob this delete does not
	// touch. D26 keeps it resumable by huggingface_hub itself, and removing it
	// would discard someone else's work.
	other := h.Incomplete(repo, "someone-elses", []byte("partial"))

	plan, err := cache.PlanDelete(h.Dir, repo, revA, []string{"model.gguf"})
	if err != nil {
		t.Fatalf("PlanDelete: %v", err)
	}
	if err := plan.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("an unrelated .incomplete file was removed: %v", err)
	}
}

func TestPlanDeleteMissingFileIsSkippedNotRefused(t *testing.T) {
	t.Parallel()

	// A catalog one file ahead of the disk. Refusing would leave a model row
	// nothing can remove; skipping deletes what is actually there.
	h := cachetest.New(t)
	h.Blob(repo, "e1", make([]byte, 4096))
	h.Link(repo, revA, "present.gguf", "e1")

	plan, err := cache.PlanDelete(h.Dir, repo, revA, []string{"present.gguf", "gone.gguf"})
	if err != nil {
		t.Fatalf("PlanDelete: %v", err)
	}
	if plan.Files() != 1 {
		t.Fatalf("files = %d, want 1 — the missing name is skipped", plan.Files())
	}
}

func TestPlanDeleteOnAnAbsentRepoIsEmpty(t *testing.T) {
	t.Parallel()

	plan, err := cache.PlanDelete(t.TempDir(), "org/never-fetched", revA, nil)
	if err != nil {
		t.Fatalf("PlanDelete: %v", err)
	}
	if plan.Files() != 0 || plan.Bytes() != 0 {
		t.Fatalf("plan over a repository that is not there = %+v, want empty", plan)
	}
}

// TestExecuteRemovesLinksBeforeBlobs pins the ordering that makes an
// interrupted delete leave a consistent cache: links first, so a blob is never
// removed while an entry still points at it.
func TestExecuteIsSafeToRepeat(t *testing.T) {
	t.Parallel()

	h := cachetest.New(t)
	h.Blob(repo, "e1", make([]byte, 4096))
	h.Link(repo, revA, "model.gguf", "e1")

	plan, err := cache.PlanDelete(h.Dir, repo, revA, []string{"model.gguf"})
	if err != nil {
		t.Fatalf("PlanDelete: %v", err)
	}
	if err := plan.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// A `model_delete` job is triaged to `failed` at boot (section 2.3), which
	// leaves the row in `deleting`; re-issuing the delete is how a user gets
	// out of it, so executing a plan twice has to be a no-op rather than an
	// error.
	if err := plan.Execute(); err != nil {
		t.Fatalf("second Execute = %v, want a no-op", err)
	}
}

func TestPlanDeleteAcrossSubdirectories(t *testing.T) {
	t.Parallel()

	// A repository whose GGUFs live in a subdirectory. The relative symlink is
	// one `../` deeper, and the refcount has to resolve it just the same.
	h := cachetest.New(t)
	h.Blob(repo, "e1", make([]byte, 4096))
	h.Link(repo, revA, "gguf/model.gguf", "e1")

	plan, err := cache.PlanDelete(h.Dir, repo, revA, []string{"gguf/model.gguf"})
	if err != nil {
		t.Fatalf("PlanDelete: %v", err)
	}
	if len(plan.Blobs) != 1 {
		t.Fatalf("blobs = %+v, want the nested entry's blob to be resolved", plan.Blobs)
	}
	if err := plan.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cache.NewLayout(h.Dir).SnapshotDir(repo, revA), "gguf")); !os.IsNotExist(err) {
		t.Fatal("the emptied subdirectory was left behind")
	}
}

// TestExecuteRecountsBlobsUnderTheInteropLock is the race section 7.2 calls "the
// one failure this design will not tolerate".
//
// A plan is computed, shown to the user, and executed when they confirm. A
// download of a SECOND revision that shares one of its blobs can complete in
// between: the write path renames `.incomplete` onto `blobs/<etag>` and then
// creates `snapshots/<other>/<file> -> ../../blobs/<etag>`. A delete that
// trusted its stale refcount would unlink that blob and leave a dangling
// snapshot symlink under a `models` row still marked `ready`.
func TestExecuteRecountsBlobsUnderTheInteropLock(t *testing.T) {
	t.Parallel()

	h := cachetest.New(t)
	h.Blob(repo, "e1", make([]byte, 4096))
	h.Link(repo, revA, "model.gguf", "e1")

	plan, err := cache.PlanDelete(h.Dir, repo, revA, []string{"model.gguf"})
	if err != nil {
		t.Fatalf("PlanDelete: %v", err)
	}
	if len(plan.Blobs) != 1 || plan.Blobs[0].Etag != "e1" {
		t.Fatalf("blobs = %+v, want e1 at refcount zero", plan.Blobs)
	}

	// Between the plan and the execution, another revision links the same blob.
	h.Link(repo, revB, "model.gguf", "e1")

	if err := plan.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	l := cache.NewLayout(h.Dir)
	if _, err := os.Stat(l.BlobPath(repo, "e1")); err != nil {
		t.Fatalf("THE BLOB WAS REMOVED out from under the snapshot that linked it while "+
			"the plan was being confirmed: %v", err)
	}
	// And the link that arrived in the window still resolves.
	if _, err := os.Stat(filepath.Join(l.SnapshotDir(repo, revB), "model.gguf")); err != nil {
		t.Errorf("the new snapshot entry dangles: %v", err)
	}
}

// TestExecuteLeavesABlobAnotherToolIsWriting is the other half of the same
// interlock: the D27 lock is held by a transfer that is between its rename and
// its symlink, so this delete has no way to know whether the blob is about to be
// referenced. It leaves it alone, which the next scan reports as an orphan blob
// — the benign outcome, and the same one an interrupted delete already produces.
func TestExecuteLeavesABlobAnotherToolIsWriting(t *testing.T) {
	t.Parallel()

	h := cachetest.New(t)
	h.Blob(repo, "e1", make([]byte, 4096))
	h.Link(repo, revA, "model.gguf", "e1")

	plan, err := cache.PlanDelete(h.Dir, repo, revA, []string{"model.gguf"})
	if err != nil {
		t.Fatalf("PlanDelete: %v", err)
	}

	lock, err := cache.Acquire(cache.LockPath(h.Dir, repo, "e1"))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = lock.Release() }()

	if err := plan.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	l := cache.NewLayout(h.Dir)
	if _, err := os.Stat(l.BlobPath(repo, "e1")); err != nil {
		t.Errorf("a blob whose interop lock another process holds was removed: %v", err)
	}
	// The snapshot entry still goes: unlinking it is never in doubt.
	if _, err := os.Lstat(filepath.Join(l.SnapshotDir(repo, revA), "model.gguf")); !os.IsNotExist(err) {
		t.Error("the snapshot entry survived the delete")
	}
}

// TestExecuteRefusesAPlanWithNoLockPath states the failure mode of a Plan
// assembled by hand: with no hub and no repository id there is no D27 lock path
// to take, and an uninterlocked blob removal is precisely what Execute exists to
// avoid.
func TestExecuteRefusesAPlanWithNoLockPath(t *testing.T) {
	t.Parallel()

	h := cachetest.New(t)
	h.Blob(repo, "e1", make([]byte, 4096))
	h.Link(repo, revA, "model.gguf", "e1")

	plan, err := cache.PlanDelete(h.Dir, repo, revA, nil)
	if err != nil {
		t.Fatalf("PlanDelete: %v", err)
	}
	plan.Hub, plan.RepoID = "", ""
	if err := plan.Execute(); err == nil {
		t.Fatal("a plan with no lock path removed blobs anyway")
	}
	if _, err := os.Stat(cache.NewLayout(h.Dir).BlobPath(repo, "e1")); err != nil {
		t.Errorf("the blob was removed without the interop lock: %v", err)
	}
}
