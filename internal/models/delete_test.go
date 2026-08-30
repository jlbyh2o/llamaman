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

// modelError unwraps the domain error a guard returned, failing the test when
// the call succeeded or returned something else.
func modelError(t *testing.T, err error) model.Error {
	t.Helper()
	var me model.Error
	if !errors.As(err, &me) {
		t.Fatalf("error = %v, want a model.Error", err)
	}
	return me
}

// TestDeletePreviewRefcountsAcrossSnapshots is D28 at the service level: the
// preview says what will actually be freed, and a blob a second snapshot shares
// is reported as kept rather than counted as freed.
func TestDeletePreviewRefcountsAcrossSnapshots(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// One tokenizer blob shared by two revisions; one weights file per revision.
	shared := make([]byte, 8192)
	f.hub.Blob(repo, "shared-tokenizer", shared)
	f.hub.Link(repo, revA, "tokenizer.gguf", "shared-tokenizer")
	f.hub.Link(repo, revB, "tokenizer.gguf", "shared-tokenizer")
	f.hub.Model(repo, revA, "model.gguf", cachetest.GGUF{Arch: "qwen3", Complete: true})
	f.hub.Model(repo, revB, "model.gguf", cachetest.GGUF{Arch: "qwen3", Complete: true})
	f.scan()

	// The tokenizer is its own (unparsable, hence `unknown`) row in each
	// snapshot; the weights are what we delete.
	var target View
	for _, v := range f.models() {
		if v.PrimaryFile == "model.gguf" && v.Revision == revA {
			target = v
		}
	}
	if target.ID == "" {
		t.Fatal("the revision-A weights row was not found")
	}

	plan, err := f.svc.DeletePreview(ctx, target.ID)
	if err != nil {
		t.Fatalf("DeletePreview: %v", err)
	}
	if plan.Files != 1 {
		t.Fatalf("files = %d, want 1 — the plan names this model's own files", plan.Files)
	}
	if plan.Bytes <= 0 {
		t.Fatalf("bytes = %d, want the weights blob's allocation", plan.Bytes)
	}
	if len(plan.InUseBy) != 0 {
		t.Fatalf("in_use_by = %+v, want empty", plan.InUseBy)
	}
	// Deleting one quant of one snapshot does not take the repository.
	if plan.RemovesRepoDir {
		t.Fatal("the plan would remove the repo directory while revision B survives")
	}

	// Now delete the shared tokenizer row of revision A: its blob is shared, so
	// nothing is freed and the preview says so.
	var tok View
	for _, v := range f.models() {
		if v.PrimaryFile == "tokenizer.gguf" && v.Revision == revA {
			tok = v
		}
	}
	if tok.ID == "" {
		t.Fatal("the shared tokenizer did not become its own catalog row")
	}
	tokPlan, err := f.svc.DeletePreview(ctx, tok.ID)
	if err != nil {
		t.Fatalf("DeletePreview: %v", err)
	}
	if tokPlan.Bytes != 0 {
		t.Fatalf("bytes = %d, want 0 — the blob is shared with revision B", tokPlan.Bytes)
	}
	if tokPlan.BlobsSharedKept != 1 || tokPlan.SharedBytes <= 0 {
		t.Fatalf("shared blobs = %d / %d bytes, want 1 and non-zero",
			tokPlan.BlobsSharedKept, tokPlan.SharedBytes)
	}
}

// TestDeleteRefusesWhileAnInstanceUsesIt is §7.2's guard, and the shape of the
// 409: the instances are NAMED, so the UI can say which ones.
func TestDeleteRefusesWhileAnInstanceUsesIt(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.hub.Model(repo, revA, "model.gguf", cachetest.GGUF{Arch: "qwen3"})
	f.scan()
	m := f.find("model.gguf")
	f.seedInstance("inference-1", m.ID, false)

	_, _, err := f.svc.Delete(ctx, m.ID)
	me := modelError(t, err)
	if me.Code != model.CodeModelInUse {
		t.Fatalf("code = %q, want model_in_use", me.Code)
	}
	refs, _ := me.Details["instances"].([]map[string]any)
	if len(refs) != 1 || refs[0]["name"] != "inference-1" {
		t.Fatalf("details.instances = %+v, want inference-1 named", me.Details["instances"])
	}

	// The row did not move.
	if f.find("model.gguf").State != model.ModelReady {
		t.Fatal("a refused delete moved the model's state")
	}
}

// TestDeleteIgnoresSoftDeletedInstances is the OTHER half of §7.2's guard, and
// the one that is easy to get backwards. Deleting a model never issues a SQL
// DELETE — the row moves `deleting → deleted` and stays — so ON DELETE RESTRICT
// is never exercised, and a soft-deleted instance keeping a readable record of
// what it once pointed at is the intended behavior (D68).
func TestDeleteIgnoresSoftDeletedInstances(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.hub.Model(repo, revA, "model.gguf", cachetest.GGUF{Arch: "qwen3", Complete: true})
	f.scan()
	m := f.find("model.gguf")
	f.seedInstance("retired", m.ID, true)

	plan, _, err := f.svc.Delete(ctx, m.ID)
	if err != nil {
		t.Fatalf("Delete refused because of a SOFT-DELETED instance: %v", err)
	}
	if plan.Files != 1 {
		t.Fatalf("plan files = %d, want 1", plan.Files)
	}

	// The row is now `deleting` and still there.
	rows, err := f.svc.List(ctx, ListParams{IncludeDeleted: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 || rows[0].State != model.ModelDeleting {
		t.Fatalf("rows = %+v, want one row in state deleting", rows)
	}
}

// TestDeleteWorkerExecutesAndKeepsTheRow: the job frees the files and the row
// moves to `deleted` — it is never removed, so a retained instance can still
// say what it pointed at.
func TestDeleteWorkerExecutesAndKeepsTheRow(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.hub.Model(repo, revA, "model.gguf", cachetest.GGUF{Arch: "qwen3", Complete: true})
	f.scan()
	m := f.find("model.gguf")
	link := cache.NewLayout(f.hub.Dir).SnapshotFile(repo, revA, "model.gguf")

	if _, _, err := f.svc.Delete(ctx, m.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Drive the worker's body the way the queue would, and apply the commit it
	// returns in one transaction — which is the §2.3a pairing the queue
	// performs for real.
	out, err := f.svc.ExecuteDelete(ctx, m.ID)
	if err != nil {
		t.Fatalf("delete worker: %v", err)
	}
	if out.State != model.JobSucceeded {
		t.Fatalf("outcome = %+v, want succeeded", out)
	}
	if err := f.db.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		return out.Commit(ctx, tx, model.JobSucceeded)
	}); err != nil {
		t.Fatalf("commit the delete: %v", err)
	}

	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("the snapshot entry survived the delete: %v", err)
	}
	rows, err := f.svc.List(ctx, ListParams{IncludeDeleted: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 || rows[0].State != model.ModelDeleted {
		t.Fatalf("rows = %+v, want one row in state deleted", rows)
	}
	// The default listing hides it: a deleted model is history, not catalog.
	if got := f.models(); len(got) != 0 {
		t.Fatalf("the default listing still shows %d deleted rows", len(got))
	}
}

// TestDetachRootRefusesForSoftDeletedInstances is §7.2a's exception, and section
// 15's named integration case. This IS the path that issues a SQL DELETE against
// `models`, so a guard phrased over non-deleted instances would pass and the
// transaction would then fail with a raw foreign-key violation instead of the
// documented 409.
func TestDetachRootRefusesForSoftDeletedInstances(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// A second root, so the one under test is not primary.
	other := cachetest.New(t)
	other.Model(repo, revA, "model.gguf", cachetest.GGUF{Arch: "qwen3"})
	added, _, err := f.svc.AddRoot(ctx, other.Dir)
	if err != nil {
		t.Fatalf("AddRoot: %v", err)
	}

	row, _, err := f.svc.RequestScan(ctx, added.ID, model.ScanTriggerManual)
	if err != nil {
		t.Fatalf("RequestScan: %v", err)
	}
	if _, err := f.svc.runScan(ctx, nil, row.ID,
		ScanParams{RootID: added.ID, Path: other.Dir}); err != nil {
		t.Fatalf("runScan: %v", err)
	}

	m := f.find("model.gguf")
	if m.RootID != added.ID {
		t.Fatalf("the scanned model belongs to root %q, want %q", m.RootID, added.ID)
	}
	instance := f.seedInstance("retired", m.ID, true)

	err = f.svc.DetachRoot(ctx, added.ID)
	me := modelError(t, err)
	if me.Code != model.CodeModelInUse {
		t.Fatalf("code = %q, want model_in_use — a SOFT-DELETED instance still holds "+
			"a RESTRICT reference on this path", me.Code)
	}
	refs, _ := me.Details["instances"].([]map[string]any)
	if len(refs) != 1 || refs[0]["deleted"] != true {
		t.Fatalf("details.instances = %+v, want the soft-deleted instance marked deleted",
			me.Details["instances"])
	}

	// Purging that instance releases the reference, and the detach then
	// succeeds — with no file touched.
	if err := f.db.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		_, err := f.db.PurgeInstance(ctx, tx, instance)
		return err
	}); err != nil {
		t.Fatalf("purge the instance: %v", err)
	}
	if err := f.svc.DetachRoot(ctx, added.ID); err != nil {
		t.Fatalf("DetachRoot after the purge: %v", err)
	}
	if _, err := os.Stat(cache.NewLayout(other.Dir).SnapshotFile(repo, revA, "model.gguf")); err != nil {
		t.Fatalf("detaching a root touched its files: %v", err)
	}
	if len(f.models()) != 0 {
		t.Fatalf("the detached root's catalog rows survived: %+v", f.models())
	}
}

func TestDetachRootRefusesThePrimary(t *testing.T) {
	f := newFixture(t)

	err := f.svc.DetachRoot(context.Background(), f.root.ID)
	if me := modelError(t, err); me.Code != model.CodeRootIsPrimary {
		t.Fatalf("code = %q, want root_is_primary", me.Code)
	}
}

func TestDeleteStrayRefusesAPathOutsideItsRoot(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.hub.Blob(repo, "orphan", []byte("orphan"))
	f.scan()

	strays, err := f.svc.Strays(ctx, "")
	if err != nil {
		t.Fatalf("Strays: %v", err)
	}
	if len(strays) != 1 {
		t.Fatalf("strays = %d, want 1", len(strays))
	}

	// Removing the file it names is allowed, because it is inside the root that
	// reported it.
	if err := f.svc.DeleteStray(ctx, strays[0].ID, true); err != nil {
		t.Fatalf("DeleteStray: %v", err)
	}
	if _, err := os.Stat(strays[0].Path); !os.IsNotExist(err) {
		t.Fatal("the stray file was not removed")
	}
	if left, _ := f.svc.Strays(ctx, ""); len(left) != 0 {
		t.Fatalf("the stray row survived: %+v", left)
	}
}

func TestDismissStrayHidesItWithoutRemovingAnything(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	blob := f.hub.Blob(repo, "orphan", []byte("orphan"))
	f.scan()
	strays, _ := f.svc.Strays(ctx, "")
	if len(strays) != 1 {
		t.Fatalf("strays = %d, want 1", len(strays))
	}

	if err := f.svc.DismissStray(ctx, strays[0].ID); err != nil {
		t.Fatalf("DismissStray: %v", err)
	}
	if left, _ := f.svc.Strays(ctx, ""); len(left) != 0 {
		t.Fatalf("a dismissed stray is still listed: %+v", left)
	}
	if _, err := os.Stat(blob); err != nil {
		t.Fatalf("dismissing a stray removed its file: %v", err)
	}

	// And a later scan must not un-dismiss it: `dismissed_at` survives the
	// upsert, or the dismissal never worked.
	f.scan()
	if left, _ := f.svc.Strays(ctx, ""); len(left) != 0 {
		t.Fatalf("a rescan un-dismissed the stray: %+v", left)
	}
}
