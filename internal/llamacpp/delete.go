package llamacpp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jlbyh2o/llamaman/internal/jobs"
	"github.com/jlbyh2o/llamaman/internal/llamacpp/prebuilt"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// The `llamacpp_delete` worker: garbage collection under D25's guard.
//
// "Only `active` and `previous` are retained. Before deleting any version
// directory, `readlink /proc/<MainPID>/exe` is resolved for every running
// instance and any directory in that set is skipped. Database bookkeeping alone
// is not trusted for this."
//
// The guard is asked TWICE and that is deliberate: once by the service, so the
// user's DELETE gets a 409 with a pid in it rather than a job that fails a
// second later, and once here, immediately before the removal, because a job
// queued behind a build can wait minutes and an instance can start in that
// window.
//
// The two edges out of `deleting` are §2.5's, and both are recoveries rather
// than failures: a directory that is still complete and whose `bin/llama-server`
// still executes goes back to `ready` — the version is usable again and
// reappears in the list — and only an incomplete tree is `failed`, with
// `failing_step='delete'`, which the UI offers to retry.

// DeleteWorker runs `llamacpp_delete`.
type DeleteWorker struct {
	svc *Service
	// verify runs `bin/llama-server --version` against a tree the removal left
	// behind. Nil uses prebuilt.Verify's own runner.
	verify func(ctx context.Context, dir string) bool
}

// NewDeleteWorker builds the worker.
func (s *Service) NewDeleteWorker() *DeleteWorker { return &DeleteWorker{svc: s} }

// Kind implements jobs.Worker.
func (w *DeleteWorker) Kind() model.JobKind { return model.JobLlamacppDelete }

// Start implements jobs.Starter: the row moves to `deleting` in the same
// transaction that moves the job to `running` — §2.3a's delete column, and the
// first moment anything on disk is at risk.
func (w *DeleteWorker) Start(ctx context.Context, tx store.Tx, j model.Job) error {
	var p deleteParams
	if err := decodeParams(j.ParamsJSON, &p); err != nil {
		return err
	}
	_, err := w.svc.store.SetLlamacppVersionState(ctx, tx, p.VersionID,
		model.VersionDeleting, w.svc.now().UnixMilli())
	return err
}

// SetDomainState implements jobs.DomainWriter. A delete owes nothing durable, so
// §2.3 triages it straight to `failed` at boot — and the domain row is resolved
// beside it by the SAME §2.5 edge the worker would have taken: `ready` when the
// directory is still complete, `failed` when it is not. Re-checking the tree is
// cheaper than guessing, and guessing is the only alternative.
func (w *DeleteWorker) SetDomainState(ctx context.Context, tx store.Tx, j model.Job,
	state model.JobState) error {

	var p deleteParams
	if err := decodeParams(j.ParamsJSON, &p); err != nil {
		return err
	}
	now := w.svc.now().UnixMilli()

	switch state {
	case model.JobSucceeded:
		_, err := w.svc.store.SetLlamacppVersionState(ctx, tx, p.VersionID,
			model.VersionDeleted, now)
		return err
	case model.JobQueued:
		_, err := w.svc.store.SetLlamacppVersionState(ctx, tx, p.VersionID,
			model.VersionReady, now)
		return err
	case model.JobFailed, model.JobCanceled:
		return w.resolveAfterFailure(ctx, tx, p.VersionID, now)
	}
	return nil
}

// Run implements jobs.Worker.
func (w *DeleteWorker) Run(ctx context.Context, t *jobs.Task) (jobs.Outcome, error) {
	var p deleteParams
	if err := decodeParams(t.Job().ParamsJSON, &p); err != nil {
		return jobs.Outcome{}, err
	}
	now := w.svc.now()

	// The row guards. They are re-read here rather than trusted from the
	// request because an activation can have happened while this job waited,
	// and deleting the build every instance is about to start from is the one
	// mistake this worker must not make.
	var row store.LlamacppVersion
	if err := w.svc.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		row, err = w.svc.store.LlamacppVersion(ctx, tx, p.VersionID)
		return err
	}); err != nil {
		return jobs.Outcome{}, err
	}
	switch {
	case row.IsActive:
		return w.refused(p.VersionID, CodeVersionActive,
			fmt.Sprintf("%s became the active build while its deletion was queued",
				p.VersionID)), nil
	case row.PreviousActive:
		return w.refused(p.VersionID, CodeVersionIsRollbackTarget,
			fmt.Sprintf("%s is the retained rollback target", p.VersionID)), nil
	}

	// D25, immediately before the removal.
	dir := w.svc.layout.VersionDir(p.VersionID)
	pid, inUse, err := w.svc.guard.InUse(ctx, dir)
	if err != nil {
		return w.refused(p.VersionID, CodeVersionInUse,
			fmt.Sprintf("could not check whether %s is in use: %v", p.VersionID, err)), nil
	}
	if inUse {
		return w.refused(p.VersionID, CodeVersionInUse,
			fmt.Sprintf("pid %d is still executing out of %s — it keeps the stale_version "+
				"badge until it restarts", pid, p.VersionID)), nil
	}

	if err := os.RemoveAll(dir); err != nil {
		return w.removalFailed(p.VersionID, err), nil
	}
	// The staging tree and the build log's own copy go with it; leaving either
	// behind would make a reinstall of the same id resume from a tree nothing
	// describes.
	_ = prebuilt.CleanStaging(w.svc.layout.StagingDir(p.VersionID))

	return jobs.Succeeded(func(ctx context.Context, tx store.Tx, _ model.JobState) error {
		if _, err := w.svc.store.SetLlamacppVersionState(ctx, tx, p.VersionID,
			model.VersionDeleted, now.UnixMilli()); err != nil {
			return err
		}
		return w.svc.event(ctx, tx, now, p.VersionID, "llamacpp_version_deleted",
			model.LevelInfo, fmt.Sprintf("llama.cpp %s was deleted", p.VersionID),
			ptr(string(model.VersionDeleting)), ptr(string(model.VersionDeleted)))
	}), nil
}

// refused closes the job `failed` and puts the version back where it was. It is
// §2.5's first edge out of `deleting`: nothing was removed, so the version is
// usable again and reappears in the list, and the reason is in `events` and in
// the job row.
func (w *DeleteWorker) refused(id string, code model.ErrorCode, message string) jobs.Outcome {
	now := w.svc.now()
	return jobs.Failed(string(code), message, func(ctx context.Context, tx store.Tx,
		_ model.JobState) error {

		if _, err := w.svc.store.SetLlamacppVersionState(ctx, tx, id,
			model.VersionReady, now.UnixMilli()); err != nil {
			return err
		}
		return w.svc.event(ctx, tx, now, id, "llamacpp_version_delete_refused",
			model.LevelWarn, message, ptr(string(model.VersionDeleting)),
			ptr(string(model.VersionReady)))
	})
}

// removalFailed is §2.5's second and third edges out of `deleting`, decided by
// looking at what is left on disk rather than by guessing from the error.
func (w *DeleteWorker) removalFailed(id string, cause error) jobs.Outcome {
	now := w.svc.now()
	message := fmt.Sprintf("could not remove %s: %v", id, cause)
	return jobs.Failed(string(model.CodeDeleteIncomplete), message,
		func(ctx context.Context, tx store.Tx, _ model.JobState) error {
			if err := w.resolveAfterFailure(ctx, tx, id, now.UnixMilli()); err != nil {
				return err
			}
			return w.svc.event(ctx, tx, now, id, "llamacpp_version_delete_failed",
				model.LevelError, message, ptr(string(model.VersionDeleting)), nil)
		})
}

// resolveAfterFailure walks §2.5's two edges: a tree that still carries an
// executable `bin/llama-server` is usable, so the version goes back to `ready`;
// anything else is an incomplete removal, which is `failed` with
// `failing_step='delete'` and a Delete button that retries it.
func (w *DeleteWorker) resolveAfterFailure(ctx context.Context, tx store.Tx, id string,
	now int64) error {

	if w.intact(ctx, id) {
		_, err := w.svc.store.SetLlamacppVersionState(ctx, tx, id, model.VersionReady, now)
		return err
	}
	_, err := w.svc.store.FailLlamacppVersion(ctx, tx, id, store.LlamacppFailure{
		State:        model.VersionFailed,
		FailingStep:  ptr(model.StepDelete),
		ErrorCode:    strPtr(string(model.CodeDeleteIncomplete)),
		ErrorMessage: strPtr("the version directory was left incomplete; delete it again"),
	}, now)
	return err
}

// intact reports whether the version tree still has a `bin/llama-server` that
// runs. The check is deliberately the same one D18 makes at install time: a
// directory is usable exactly when its server binary executes on this host.
func (w *DeleteWorker) intact(ctx context.Context, id string) bool {
	dir := w.svc.layout.VersionDir(id)
	if w.verify != nil {
		return w.verify(ctx, dir)
	}
	server := filepath.Join(dir, "bin", "llama-server")
	info, err := os.Stat(server)
	if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		return false
	}
	res, err := prebuilt.Verify(ctx, prebuilt.VerifyOptions{Root: dir})
	if err != nil {
		return false
	}
	return res.OK
}
