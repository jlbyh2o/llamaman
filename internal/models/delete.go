package models

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"

	"github.com/jlbyh2o/llamaman/internal/events"
	"github.com/jlbyh2o/llamaman/internal/hf/cache"
	"github.com/jlbyh2o/llamaman/internal/jobs"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// Deleting a model (D28, DESIGN sections 3.7, 7.2).
//
// Two guarantees, and they are separate:
//
//  1. THE GUARD. A model any NON-DELETED instance references is refused with
//     `409 model_in_use`, naming them. A soft-deleted instance is deliberately
//     NOT a blocker (D68) and cannot be one at the SQL level either, because
//     this path never issues a SQL DELETE: the row moves `deleting → deleted`
//     and stays, so `instances.model_id`'s ON DELETE RESTRICT is never
//     exercised and a retained instance keeps a readable record of what it once
//     pointed at. (The cache-root detach of §7.2a is the exception, and its own
//     guard counts every referencing row.)
//  2. THE PREVIEW. Blobs are refcounted across every snapshot in the repo
//     directory, and the user is shown "will free N GB, N files, keeping M
//     shared blobs" BEFORE anything is executed. A blob shared by two revisions
//     must never be removed out from under one of them.

// DeletePlan is `GET /api/v1/models/{id}/delete-preview`, and it is also what
// the delete executes. The same value answers both questions on purpose: a
// preview computed by different code from the act it previews is a preview that
// can be wrong.
type DeletePlan struct {
	ModelID string
	// Files is how many snapshot entries would be unlinked.
	Files int
	// Bytes is what would actually be freed — the blobs whose refcount reaches
	// zero.
	Bytes int64
	// BlobsSharedKept is how many blobs this deletion drops a reference to that
	// another snapshot still holds. They stay, and saying so is what makes the
	// byte count believable when it is smaller than the model's size.
	BlobsSharedKept int
	// SharedBytes is what those kept blobs occupy.
	SharedBytes int64
	// InUseBy names the non-deleted instances that block the delete. A non-empty
	// list means `DELETE` will answer `409 model_in_use`.
	InUseBy []model.InstanceRef
	// RemovesRepoDir reports that the whole `models--…` directory would go,
	// because this was its last snapshot and no blob survives.
	RemovesRepoDir bool

	plan cache.Plan
}

// DeletePreview builds the plan without touching anything.
func (s *Service) DeletePreview(ctx context.Context, id string) (DeletePlan, error) {
	subject, err := s.loadForDelete(ctx, id)
	if err != nil {
		return DeletePlan{}, err
	}
	return planFor(subject)
}

// deleteSubject is everything a plan needs, read in one snapshot of the
// database. It is a value rather than three arguments because the plan and the
// guard must be computed from the SAME read: a refs list from one moment and a
// file list from another would let a delete be authorized against a model whose
// files had since changed hands.
type deleteSubject struct {
	model model.LocalModel
	root  model.CacheRoot
	refs  []model.InstanceRef
	files []string
}

// loadForDelete reads the model, its root, the instances referencing it and its
// file names in one read transaction.
func (s *Service) loadForDelete(ctx context.Context, id string) (deleteSubject, error) {
	var out deleteSubject
	err := s.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		if out.model, err = s.store.LocalModel(ctx, tx, id); err != nil {
			return err
		}
		if out.root, err = s.store.CacheRoot(ctx, tx, out.model.RootID); err != nil {
			return err
		}
		// The guard counts NON-deleted instances only (D68) — this path never
		// issues a SQL DELETE, so ON DELETE RESTRICT is never exercised and a
		// soft-deleted instance keeping a record of what it pointed at is the
		// intended behavior.
		if out.refs, err = s.store.ModelInstanceRefs(ctx, tx, id, false); err != nil {
			return err
		}
		out.files, err = s.fileNames(ctx, tx, out.model)
		return err
	})
	return out, err
}

// planFor computes the D28 plan for one model.
//
// The files it names are this model's own — its shards, and nothing else in the
// snapshot. A snapshot legitimately holds several quants, a tokenizer and a
// README; deleting one quant may not take the others, and the empty-directory
// cleanup inside the plan is what removes the snapshot when the last one goes.
//
// It touches the FILESYSTEM — the refcount is a walk of every snapshot in the
// repository — so it is deliberately outside every transaction. Holding the
// single write connection across a walk of a multi-terabyte cache would block
// every other writer for the duration.
func planFor(sub deleteSubject) (DeletePlan, error) {
	p, err := cache.PlanDelete(sub.root.Path, sub.model.RepoID, sub.model.Revision, sub.files)
	if err != nil {
		return DeletePlan{}, err
	}
	return DeletePlan{
		ModelID:         sub.model.ID,
		Files:           p.Files(),
		Bytes:           p.Bytes(),
		BlobsSharedKept: len(p.SharedKept),
		SharedBytes:     p.SharedBytes(),
		InUseBy:         sub.refs,
		RemovesRepoDir:  p.RemoveRepoDir,
		plan:            p,
	}, nil
}

// fileNames is the model's file list, preferring the catalog and falling back to
// its primary file.
//
// The fallback matters for a row whose `model_files` are gone — an older row, or
// one whose files a partial reconciliation removed. A delete that named nothing
// would report "will free 0 bytes" and then leave 40 GB on the disk, which is
// the worst of both answers.
func (s *Service) fileNames(ctx context.Context, tx store.Tx, m model.LocalModel) ([]string, error) {
	files, err := s.store.ModelFiles(ctx, tx, m.ID)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(files))
	for _, f := range files {
		names = append(names, f.Filename)
	}
	if len(names) == 0 && m.PrimaryFile != "" {
		names = append(names, m.PrimaryFile)
	}
	return names, nil
}

// Delete is `DELETE /api/v1/models/{id}`: the guard, then the state move, then
// the job that executes the plan.
//
// The row moves to `deleting` in the SAME transaction that creates the job
// (§2.3a), so a restart between the two is impossible and the state the UI shows
// is never ahead of the work. D69 runs on that move: `state` is a `config_hash`
// input, and an instance pointed at a model that is being deleted has a stale
// hash from the moment the user clicks.
func (s *Service) Delete(ctx context.Context, id string) (DeletePlan, JobRef, error) {
	// Phase 1: read, guard, and compute the plan — none of it inside a write
	// transaction, because the plan walks the filesystem.
	sub, err := s.loadForDelete(ctx, id)
	if err != nil {
		return DeletePlan{}, JobRef{}, err
	}
	if err := guardDelete(sub); err != nil {
		return DeletePlan{}, JobRef{}, err
	}
	plan, err := planFor(sub)
	if err != nil {
		return DeletePlan{}, JobRef{}, err
	}
	var sink events.Sink

	// Phase 2: the guard is re-evaluated inside the transaction that acts on
	// it. The first evaluation is there to answer the user before doing a walk;
	// THIS one is the one that matters, because an instance can be created
	// between the two and a guard that only ran before the walk would be a
	// read-then-write race.
	//
	// The plan is not re-derived here: the worker recomputes it from the disk
	// anyway (D28), and the copy returned to the caller is the receipt for what
	// the click is expected to free.
	var ref JobRef
	err = s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		m, err := s.store.LocalModel(ctx, tx, id)
		if err != nil {
			return err
		}
		refs, err := s.store.ModelInstanceRefs(ctx, tx, id, false)
		if err != nil {
			return err
		}
		if err := guardDelete(deleteSubject{model: m, refs: refs}); err != nil {
			return err
		}

		now := s.now().UnixMilli()
		if _, err := s.store.SetLocalModelState(ctx, tx, id, model.ModelDeleting, now); err != nil {
			return err
		}
		if err := s.recomputeFor(ctx, tx, id); err != nil {
			return err
		}
		from, to := string(m.State), string(model.ModelDeleting)
		if err := s.appendEvent(ctx, tx, model.Event{
			Level: model.LevelInfo, Category: model.CategoryModel, Actor: model.ActorAdmin,
			Action: "model_deleting", SubjectID: &id, FromState: &from, ToState: &to,
			Message: "deleting " + m.RepoID + " " + m.PrimaryFile,
		}, &sink); err != nil {
			return err
		}

		ref, err = s.enqueue(ctx, tx, jobs.EnqueueParams{
			Kind:     model.JobModelDelete,
			DomainID: id,
			Params:   DeleteParams{ModelID: id},
		}, id)
		return err
	})
	if err == nil {
		s.publish(&sink)
	}
	return plan, ref, err
}

// guardDelete is §7.2's two refusals, in one place so the pre-walk answer and
// the in-transaction answer cannot disagree about what is allowed.
func guardDelete(sub deleteSubject) error {
	if len(sub.refs) > 0 {
		return model.Error{Code: model.CodeModelInUse,
			Message: "instances still use this model",
			Details: map[string]any{
				"model_id": sub.model.ID, "instances": refDetails(sub.refs),
			}}
	}
	if !deletable(sub.model.State) {
		return model.Error{Code: model.CodeJobInFlight,
			Message: "this model cannot be deleted from its current state",
			Details: map[string]any{
				"model_id": sub.model.ID, "state": string(sub.model.State),
			}}
	}
	return nil
}

// deletable is §2.6's edge into `deleting`: `ready|missing|corrupt`, plus two
// states this reading admits and states its reason for.
//
// `incomplete` is admitted because a scanned shard set missing a member lands
// there (see buildRow), and a half-downloaded model a user can see but never
// remove would be a permanent, un-clearable row occupying real disk.
//
// `deleting` is admitted because of boot triage, not because the transition
// table widened. A `model_delete` job is triaged to `failed` at boot (§2.3),
// which leaves the row in `deleting` with nothing running; re-issuing the delete
// is how a user gets out of it, and the plan is recomputed from disk so the
// retry is idempotent rather than approximate.
//
// `planned`, `downloading` and `verifying` are NOT admitted: a download owns
// those rows, and the answer there is `409 job_in_flight` naming the state.
func deletable(s model.ModelState) bool {
	switch s {
	case model.ModelReady, model.ModelMissing, model.ModelCorrupt, model.ModelIncomplete, model.ModelDeleting:
		return true
	}
	return false
}

// DeleteParams is a `model_delete` job's `params_json`.
type DeleteParams struct {
	ModelID string `json:"model_id"`
}

// DeleteWorker runs `model_delete` jobs.
type DeleteWorker struct{ svc *Service }

// NewDeleteWorker builds the worker for `jobs.Queue.Register`.
func NewDeleteWorker(s *Service) *DeleteWorker { return &DeleteWorker{svc: s} }

// Kind is `model_delete`.
func (w *DeleteWorker) Kind() model.JobKind { return model.JobModelDelete }

// Run executes the plan and moves the row to `deleted`.
//
// The plan is recomputed here rather than carried in `params_json`, and the
// reason is D28: the refcount is a fact about the disk at the moment of the
// delete, and a plan computed minutes ago — before another download landed a
// second snapshot that shares a blob — would remove content something now needs.
func (w *DeleteWorker) Run(ctx context.Context, t *jobs.Task) (jobs.Outcome, error) {
	return w.svc.ExecuteDelete(ctx, t.Job().SubjectID)
}

// ExecuteDelete is the delete worker's whole body, reachable without a leased
// job.
//
// It is exported for one reason: `jobs.Task` can only be constructed by the
// queue, so a test that drove the worker would have to run a queue to assert
// what a refcounted delete does to a disk. Splitting the work from the lease
// keeps the assertion on the behavior — and the composition root still registers
// the worker, so production goes through the same code.
func (s *Service) ExecuteDelete(ctx context.Context, id string) (jobs.Outcome, error) {
	sub, err := s.loadForDelete(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// The row is gone — a cache-root detach cascaded it away while this
			// job waited. There is nothing to delete and nothing went wrong.
			return jobs.Succeeded(nil), nil
		}
		return jobs.Outcome{}, err
	}
	m := sub.model

	plan, err := planFor(sub)
	if err != nil {
		msg := err.Error()
		return jobs.RetryableFailure(string(model.CodeDeleteIncomplete), msg, nil), nil
	}
	if err := plan.plan.Execute(); err != nil {
		msg := err.Error()
		return jobs.RetryableFailure(string(model.CodeDeleteIncomplete), msg, nil), nil
	}

	freed := plan.Bytes
	sink := &events.Sink{}
	out := jobs.Succeeded(func(ctx context.Context, tx store.Tx, _ model.JobState) error {
		now := s.now().UnixMilli()
		if _, err := s.store.SetLocalModelState(ctx, tx, id, model.ModelDeleted, now); err != nil {
			return err
		}
		if _, err := s.store.DeleteModelFilesNotIn(ctx, tx, id, nil); err != nil {
			return err
		}
		if err := s.recomputeFor(ctx, tx, id); err != nil {
			return err
		}
		from, to := string(model.ModelDeleting), string(model.ModelDeleted)
		detail, _ := json.Marshal(map[string]any{
			"bytes_freed": freed, "files": plan.Files, "blobs_shared_kept": plan.BlobsSharedKept,
		})
		ds := string(detail)
		return s.appendEvent(ctx, tx, model.Event{
			Level: model.LevelInfo, Category: model.CategoryModel, Actor: model.ActorSystem,
			Action: "model_deleted", SubjectID: &id, FromState: &from, ToState: &to,
			Message: "deleted " + m.RepoID + " " + m.PrimaryFile, DetailJSON: &ds,
		}, sink)
	})
	out.AfterCommit = func() { s.publish(sink) }
	return out, nil
}

// -----------------------------------------------------------------------------
// Strays
// -----------------------------------------------------------------------------

// Strays is `GET /api/v1/cache/strays`.
func (s *Service) Strays(ctx context.Context, rootID string) ([]model.StrayFile, error) {
	var out []model.StrayFile
	err := s.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		out, err = s.store.StrayFiles(ctx, tx, rootID, false)
		return err
	})
	return out, err
}

// DeleteStray is `DELETE /api/v1/cache/strays/{id}?delete_file=true`.
//
// The row and the file are two decisions. Forgetting the row is always safe;
// removing the file is the user's call and is refused for anything outside the
// root that reported it — a `path` column is data, and a delete that followed it
// anywhere would be a way to make this daemon unlink a file it never scanned.
func (s *Service) DeleteStray(ctx context.Context, id string, deleteFile bool) error {
	var sink events.Sink
	if err := s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		st, err := s.store.StrayFile(ctx, tx, id)
		if err != nil {
			return err
		}
		if deleteFile {
			root, err := s.store.CacheRoot(ctx, tx, st.RootID)
			if err != nil {
				return err
			}
			if !withinRoot(root.Path, st.Path) {
				return model.Error{Code: model.CodeSettingInvalid,
					Message: "that file is not inside the cache root that reported it",
					Details: map[string]any{"path": st.Path, "root": root.Path}}
			}
			if err := removeFile(st.Path); err != nil {
				return err
			}
		}
		if _, err := s.store.DeleteStrayFile(ctx, tx, id); err != nil {
			return err
		}
		return s.appendEvent(ctx, tx, model.Event{
			Level: model.LevelInfo, Category: model.CategoryModel, Actor: model.ActorAdmin,
			Action: "stray_removed", Message: "removed the stray record for " + st.Path,
		}, &sink)
	}); err != nil {
		return err
	}
	s.publish(&sink)
	return nil
}

// DismissStray is `POST /api/v1/cache/strays/{id}/dismiss`: the user has seen it
// and wants it out of the list without removing anything.
func (s *Service) DismissStray(ctx context.Context, id string) error {
	return s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		_, err := s.store.DismissStrayFile(ctx, tx, id, s.now().UnixMilli())
		return err
	})
}

// withinRoot reports whether path is at or under root, by path SEGMENT — the
// same containment test the protected-prefix check uses, and for the same
// reason: `/mnt/hub-old` is not under `/mnt/hub`.
func withinRoot(root, path string) bool {
	r := filepath.Clean(root)
	p := filepath.Clean(path)
	return p == r || strings.HasPrefix(p, r+string(filepath.Separator))
}
