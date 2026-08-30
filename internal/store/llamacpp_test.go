package store

import (
	"context"
	"errors"
	"testing"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// seedLlamacpp inserts one row in a chosen state.
func seedLlamacpp(t *testing.T, s *Store, id string, state model.VersionState) LlamacppVersion {
	t.Helper()
	row := LlamacppVersion{
		ID:          id,
		Channel:     model.ChannelNightly,
		Tag:         id,
		Acquisition: model.AcquisitionSource,
		Backend:     model.BackendCPU,
		DirName:     id,
		State:       state,
		SupportsFit: true,
		CreatedAt:   1_700_000_000_000,
	}
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		return s.InsertLlamacppVersion(ctx, tx, row)
	})
	return row
}

func readLlamacpp(t *testing.T, s *Store, id string) LlamacppVersion {
	t.Helper()
	var row LlamacppVersion
	if err := s.Read(context.Background(), func(ctx context.Context, tx Tx) error {
		var err error
		row, err = s.LlamacppVersion(ctx, tx, id)
		return err
	}); err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	return row
}

// TestLlamacppStateStamps asserts the two timestamp rules §2.5 attaches to the
// state column, which is why moving a version is a method rather than an UPDATE
// each caller writes: `started_at` on the first move out of `pending`, and
// `finished_at` on a terminal state — but never on `deleting`, which is a live
// state whose own edges decide what the row becomes.
func TestLlamacppStateStamps(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		state        model.VersionState
		wantStarted  bool
		wantFinished bool
	}{
		{name: "resolving starts the clock", state: model.VersionResolving, wantStarted: true},
		{name: "building is still running", state: model.VersionBuilding, wantStarted: true},
		{
			name: "ready is terminal", state: model.VersionReady,
			wantStarted: true, wantFinished: true,
		},
		{
			name: "failed is terminal", state: model.VersionFailed,
			wantStarted: true, wantFinished: true,
		},
		{
			name: "deleting is not terminal", state: model.VersionDeleting,
			wantStarted: true, wantFinished: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			seedLlamacpp(t, s, "b10621-cpu-src", model.VersionPending)

			mustWrite(t, s, func(ctx context.Context, tx Tx) error {
				ok, err := s.SetLlamacppVersionState(ctx, tx, "b10621-cpu-src", tc.state, 42)
				if err == nil && !ok {
					t.Fatal("the update matched no row")
				}
				return err
			})

			row := readLlamacpp(t, s, "b10621-cpu-src")
			if got := row.StartedAt != nil; got != tc.wantStarted {
				t.Errorf("started_at set = %v, want %v", got, tc.wantStarted)
			}
			if got := row.FinishedAt != nil; got != tc.wantFinished {
				t.Errorf("finished_at set = %v, want %v", got, tc.wantFinished)
			}
		})
	}
}

// TestResetLlamacppVersion is D71's reuse-and-reset: every trace of the previous
// attempt's OUTCOME is cleared and nothing else is, because the outcome itself
// survives in `events` and in the rotated build log.
func TestResetLlamacppVersion(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	seedLlamacpp(t, s, "b10621-cpu-bin", model.VersionPending)
	seedLlamacpp(t, s, "b10621-cpu-src", model.VersionPending)

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		if _, err := s.FailLlamacppVersion(ctx, tx, "b10621-cpu-bin", LlamacppFailure{
			State:        model.VersionFailedVerification,
			FailingStep:  ptr(model.StepVerify),
			ErrorCode:    ptr("verification_failed"),
			ErrorMessage: ptr("requires GLIBC_2.38, host has 2.36"),
			ExitCode:     ptr(int64(127)),
		}, 100); err != nil {
			return err
		}
		_, err := s.SetLlamacppSupersededBy(ctx, tx, "b10621-cpu-bin", "b10621-cpu-src")
		return err
	})

	failed := readLlamacpp(t, s, "b10621-cpu-bin")
	if failed.SupersededBy == nil || failed.ErrorCode == nil {
		t.Fatalf("the failure was not recorded: %+v", failed)
	}

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		ok, err := s.ResetLlamacppVersion(ctx, tx, "b10621-cpu-bin", 200)
		if err == nil && !ok {
			t.Fatal("reset matched no row")
		}
		return err
	})

	row := readLlamacpp(t, s, "b10621-cpu-bin")
	switch {
	case row.State != model.VersionPending:
		t.Errorf("state = %q, want pending", row.State)
	case row.ErrorCode != nil, row.ErrorMessage != nil, row.FailingStep != nil,
		row.ExitCode != nil, row.SupersededBy != nil:
		t.Errorf("the reset row still carries the previous outcome: %+v", row)
	case row.StartedAt != nil, row.FinishedAt != nil:
		t.Errorf("the reset row still carries the previous attempt's clock: %+v", row)
	}
}

// TestActivateAndRestoreFlags is §6.6 steps 2 and 3 and D24's revert, at the one
// level where the two UNIQUE partial indexes are real: the flags must never be
// held by two rows at once, in either direction.
func TestActivateAndRestoreFlags(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := newTestStore(t)
	seedLlamacpp(t, s, "b10500-cpu-src", model.VersionReady)
	seedLlamacpp(t, s, "b10600-cpu-src", model.VersionReady)
	seedLlamacpp(t, s, "b10621-cpu-src", model.VersionReady)

	activate := func(id string, keepPrevious bool) Activation {
		t.Helper()
		var act Activation
		mustWrite(t, s, func(ctx context.Context, tx Tx) error {
			var err error
			act, err = s.ActivateLlamacppVersion(ctx, tx, id, keepPrevious, 1)
			return err
		})
		return act
	}

	activate("b10500-cpu-src", true)
	activate("b10600-cpu-src", true)
	third := activate("b10621-cpu-src", true)

	if third.OutgoingID != "b10600-cpu-src" {
		t.Errorf("outgoing = %q, want b10600-cpu-src", third.OutgoingID)
	}
	// Rollback depth is one by construction, so the build that just lost the
	// slot is the one nothing references any more.
	if third.DeletionCandidateID != "b10500-cpu-src" {
		t.Errorf("deletion candidate = %q, want b10500-cpu-src", third.DeletionCandidateID)
	}
	if !readLlamacpp(t, s, "b10621-cpu-src").IsActive {
		t.Error("the target is not active")
	}
	if !readLlamacpp(t, s, "b10600-cpu-src").PreviousActive {
		t.Error("the outgoing build did not take the rollback slot")
	}

	// The revert: back to exactly the flags the activation found.
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		return s.RestoreLlamacppFlags(ctx, tx, third.Before)
	})
	if !readLlamacpp(t, s, "b10600-cpu-src").IsActive {
		t.Error("the revert did not restore the previous active build")
	}
	if readLlamacpp(t, s, "b10621-cpu-src").IsActive {
		t.Error("the reverted build is still active — the next boot would re-point " +
			"versions/active at it")
	}
	if !readLlamacpp(t, s, "b10500-cpu-src").PreviousActive {
		t.Error("the revert did not restore the rollback slot")
	}

	// And the flags are still single-holder, which the partial indexes enforce.
	var active, previous int64
	if err := s.Read(ctx, func(ctx context.Context, tx Tx) error {
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM llamacpp_versions WHERE is_active = 1`).Scan(&active); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM llamacpp_versions WHERE previous_active = 1`).Scan(&previous)
	}); err != nil {
		t.Fatalf("count the flags: %v", err)
	}
	if active != 1 || previous != 1 {
		t.Errorf("is_active=%d previous_active=%d, want exactly one of each", active, previous)
	}
}

// TestActivateWithoutKeepPrevious is §6.6 step 2's second behavior: the outgoing
// build has no slot to fall into, so it is what gets queued for deletion and
// there is no rollback target at all.
func TestActivateWithoutKeepPrevious(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	seedLlamacpp(t, s, "b10600-cpu-src", model.VersionReady)
	seedLlamacpp(t, s, "b10621-cpu-src", model.VersionReady)

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		_, err := s.ActivateLlamacppVersion(ctx, tx, "b10600-cpu-src", false, 1)
		return err
	})
	var act Activation
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		var err error
		act, err = s.ActivateLlamacppVersion(ctx, tx, "b10621-cpu-src", false, 2)
		return err
	})

	if act.DeletionCandidateID != "b10600-cpu-src" {
		t.Errorf("deletion candidate = %q, want the outgoing build", act.DeletionCandidateID)
	}
	if readLlamacpp(t, s, "b10600-cpu-src").PreviousActive {
		t.Error("keep_previous is off but a rollback target was retained")
	}
	err := s.Read(context.Background(), func(ctx context.Context, tx Tx) error {
		_, err := s.LlamacppVersionByFlag(ctx, tx, true)
		return err
	})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("previous lookup error = %v, want ErrNotFound", err)
	}
}

// TestActivateRefusesAVersionThatIsNotReady: a build still compiling has no
// binaries to point `versions/active` at.
func TestActivateRefusesAVersionThatIsNotReady(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	seedLlamacpp(t, s, "b10621-cpu-src", model.VersionBuilding)

	err := s.Write(context.Background(), func(ctx context.Context, tx Tx) error {
		_, err := s.ActivateLlamacppVersion(ctx, tx, "b10621-cpu-src", true, 1)
		return err
	})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound for a version that is not ready", err)
	}
}

// TestBuildLease is D70's conditional UPDATE: a free lease is taken, a held one
// refuses a second holder, the owner may re-take its own, and a lapsed one is
// reclaimable.
func TestBuildLease(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	seedLlamacpp(t, s, "b10621-cpu-src", model.VersionPending)
	seedJobForLease(t, s, "job-a")
	seedJobForLease(t, s, "job-b")

	take := func(jobID, owner string, now, expires int64) bool {
		t.Helper()
		var ok bool
		mustWrite(t, s, func(ctx context.Context, tx Tx) error {
			var err error
			ok, err = s.AcquireBuildLease(ctx, tx, jobID, "b10621-cpu-src", owner, now, expires)
			return err
		})
		return ok
	}

	if !take("job-a", "boot-1", 100, 1_000) {
		t.Fatal("a free lease was refused")
	}
	if take("job-b", "boot-2", 200, 1_100) {
		t.Error("a held lease was handed to a second builder — D70's whole point")
	}
	if !take("job-a", "boot-1", 300, 1_200) {
		t.Error("the holding boot could not extend its own lease")
	}
	// A lapsed lease belongs to a daemon that is gone.
	if !take("job-b", "boot-2", 2_000, 3_000) {
		t.Error("a lapsed lease was not reclaimable")
	}

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		ok, err := s.ReleaseForeignBuildLease(ctx, tx, "boot-3")
		if err == nil && !ok {
			t.Error("a lease owned by another boot was not released")
		}
		return err
	})
	var lease BuildLease
	if err := s.Read(context.Background(), func(ctx context.Context, tx Tx) error {
		var err error
		lease, err = s.BuildLease(ctx, tx)
		return err
	}); err != nil {
		t.Fatalf("read the lease: %v", err)
	}
	if lease.Held() {
		t.Errorf("the lease is still held: %+v", lease)
	}
}

// seedJobForLease inserts the `jobs` row `build_lease.job_id` references.
func seedJobForLease(t *testing.T, s *Store, id string) {
	t.Helper()
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		return s.InsertJob(ctx, tx, model.Job{
			ID: id, Kind: model.JobLlamacppInstall,
			SubjectType: model.SubjectLlamacppVersion, SubjectID: id,
			State: model.JobSucceeded, Priority: 100, RunAfter: 1,
			MaxAttempts: 1, CreatedAt: 1,
		})
	})
}
