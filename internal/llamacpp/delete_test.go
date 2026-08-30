package llamacpp

import (
	"context"
	"os"
	"testing"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// TestDeleteGuards is §3.5's three documented refusals, and the third of them —
// D25's — is the one that is answered from `/proc` rather than from a row,
// because "database bookkeeping alone is not trusted for this".
func TestDeleteGuards(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		setup    func(*fixture)
		wantCode model.ErrorCode
	}{
		{
			name: "the active build",
			setup: func(f *fixture) {
				f.registerActivate(&fakeRoller{failAt: -1})
				f.activate(f.t, "b10621-cpu-src", RestartNone)
			},
			wantCode: CodeVersionActive,
		},
		{
			name: "the retained rollback target",
			setup: func(f *fixture) {
				f.registerActivate(&fakeRoller{failAt: -1})
				f.seedVersion("b10700-cpu-src", model.VersionReady)
				f.activate(f.t, "b10621-cpu-src", RestartNone)
				f.activate(f.t, "b10700-cpu-src", RestartNone)
			},
			wantCode: CodeVersionIsRollbackTarget,
		},
		{
			name: "a live process is executing out of it",
			setup: func(f *fixture) {
				f.guard.inUse, f.guard.pid = true, 9182
			},
			wantCode: CodeVersionInUse,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, nil)
			f.seedVersion("b10621-cpu-src", model.VersionReady)
			tc.setup(f)

			_, err := f.svc.Delete(context.Background(), "b10621-cpu-src")
			assertCode(t, err, tc.wantCode)

			// A refused delete leaves the row exactly where it was.
			if got := f.version("b10621-cpu-src").State; got != model.VersionReady {
				t.Errorf("version state after a refusal = %q, want ready", got)
			}
		})
	}
}

// TestDeleteRemovesTheDirectory is the happy path of §2.5's `deleting → deleted`
// edge, and it asserts the state pairs §2.3a gives `llamacpp_delete`.
func TestDeleteRemovesTheDirectory(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil)
	f.registerDelete()
	f.seedVersion("b10621-cpu-src", model.VersionReady)
	dir := f.svc.Layout().VersionDir("b10621-cpu-src")

	job, err := f.svc.Delete(context.Background(), "b10621-cpu-src")
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// `queued` pairs with `ready`: nothing on disk is at risk until the worker
	// starts.
	if got := f.version("b10621-cpu-src").State; got != model.VersionReady {
		t.Errorf("state while queued = %q, want ready", got)
	}

	f.runOne()

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the version directory survived the delete: %v", err)
	}
	if got := f.version("b10621-cpu-src").State; got != model.VersionDeleted {
		t.Errorf("state = %q, want deleted", got)
	}
	if got := f.job(job.ID).State; got != model.JobSucceeded {
		t.Errorf("job state = %q, want succeeded", got)
	}
}

// TestDeleteWorkerRechecksTheGuard is why D25 is asked twice: a delete job can
// wait minutes behind a build, and an instance can start in that window. The
// version must come back to `ready` — usable again, and back in the list — and
// nothing may be removed.
func TestDeleteWorkerRechecksTheGuard(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil)
	f.registerDelete()
	f.seedVersion("b10621-cpu-src", model.VersionReady)
	dir := f.svc.Layout().VersionDir("b10621-cpu-src")

	job, err := f.svc.Delete(context.Background(), "b10621-cpu-src")
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Between the request and the worker, an instance starts on this build.
	f.guard.inUse, f.guard.pid = true, 5150

	f.runOne()

	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the version directory was removed while a process was executing from it: %v", err)
	}
	if got := f.version("b10621-cpu-src").State; got != model.VersionReady {
		t.Errorf("state = %q, want ready — the version is usable again", got)
	}
	if got := f.job(job.ID); got.State != model.JobFailed {
		t.Errorf("job state = %q, want failed", got.State)
	} else if got.ErrorCode == nil || *got.ErrorCode != string(CodeVersionInUse) {
		t.Errorf("job error_code = %v, want %q", got.ErrorCode, CodeVersionInUse)
	}
}

// TestDeleteWorkerRefusesAVersionActivatedWhileQueued: the row guards are
// re-read by the worker for the same reason the process guard is. Deleting the
// build every instance is about to start from is the one mistake it must not
// make.
func TestDeleteWorkerRefusesAVersionActivatedWhileQueued(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil)
	f.registerDelete()
	f.registerActivate(&fakeRoller{failAt: -1})
	f.seedVersion("b10621-cpu-src", model.VersionReady)
	dir := f.svc.Layout().VersionDir("b10621-cpu-src")

	if _, err := f.svc.Delete(context.Background(), "b10621-cpu-src"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// An activation cannot be enqueued while the delete holds the subject, so
	// the flags are moved the way a race would leave them.
	f.activateRowDirectly(t, "b10621-cpu-src")

	f.runOne()

	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the active build's directory was removed: %v", err)
	}
	if got := f.version("b10621-cpu-src").State; got != model.VersionReady {
		t.Errorf("state = %q, want ready", got)
	}
}
