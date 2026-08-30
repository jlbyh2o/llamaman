package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// The retention sweeps of DESIGN section 2.11 that the nightly `maintenance`
// pass calls.
//
// Each one is a DELETE with a boundary, and the only thing worth asserting about
// a DELETE with a boundary is that it removes what is outside and keeps what is
// inside — including the exceptions the design writes out in prose, which are
// the whole reason these are not one generic sweep: an OPEN `instance_starts`
// row, an `interrupted` job, an undismissed notification.

func retentionNow() time.Time { return time.Unix(1_800_000_000, 0).UTC() }

func countTable(t *testing.T, s *Store, table string) int64 {
	t.Helper()
	var n int64
	if err := s.Read(context.Background(), func(ctx context.Context, tx Tx) error {
		return tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n)
	}); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// seedRetentionInstance inserts the parent row `instance_starts` and the usage
// tables need, reusing the pairing helper instances_test.go already owns.
func seedRetentionInstance(t *testing.T, s *Store, id string, port int) {
	t.Helper()
	seedInstance(t, s, newInstance(id, id, port, port+10_000))
}

// TestTrimInstanceStartsPerInstance is section 2.11's "`instance_starts` 500 per
// instance (closed rows only)". Both halves of that sentence are load-bearing:
// the cap is per instance, and an open row is never pruned out from under the
// supervisor.
func TestTrimInstanceStartsPerInstance(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	now := retentionNow()
	seedRetentionInstance(t, s, "inst-a", 8081)
	seedRetentionInstance(t, s, "inst-b", 8082)

	// Twelve closed runs each, plus one run still in flight on inst-a.
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		for _, inst := range []string{"inst-a", "inst-b"} {
			for i := range 12 {
				at := now.Add(-time.Duration(i) * time.Hour)
				row := model.InstanceStart{
					ID: NewID(at.Add(time.Duration(i) * time.Millisecond)), InstanceID: inst,
					At: at.UnixMilli(), Trigger: model.StartByUser, ConfigHash: "h",
					Outcome: ptr(model.OutcomeStopped), EndedAt: ptr(at.UnixMilli()),
				}
				if err := s.InsertInstanceStart(ctx, tx, row); err != nil {
					return err
				}
			}
		}
		return s.InsertInstanceStart(ctx, tx, model.InstanceStart{
			ID: NewID(now.Add(time.Hour)), InstanceID: "inst-a",
			At: now.Add(-100 * time.Hour).UnixMilli(), Trigger: model.StartByUser,
			ConfigHash: "h", // no outcome: this run is still in flight
		})
	})

	// A cap of 5 per instance leaves 5 closed rows each, plus the open one.
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		n, err := s.TrimInstanceStartsPerInstance(ctx, tx, 5)
		if err != nil {
			return err
		}
		if n != 14 {
			t.Errorf("trimmed %d rows, want 14 (7 closed per instance)", n)
		}
		return nil
	})

	if got := countTable(t, s, "instance_starts"); got != 11 {
		t.Errorf("%d rows remain, want 11 (5 closed per instance plus the open one)", got)
	}

	var open int64
	if err := s.Read(context.Background(), func(ctx context.Context, tx Tx) error {
		return tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM instance_starts WHERE outcome IS NULL`).Scan(&open)
	}); err != nil {
		t.Fatalf("count open rows: %v", err)
	}
	if open != 1 {
		t.Errorf("the OPEN run was pruned out from under the supervisor (%d remain)", open)
	}

	// Per instance, not globally: inst-b's history is untouched by inst-a's.
	for _, inst := range []string{"inst-a", "inst-b"} {
		var n int64
		if err := s.Read(context.Background(), func(ctx context.Context, tx Tx) error {
			return tx.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM instance_starts WHERE instance_id = ? AND outcome IS NOT NULL`,
				inst).Scan(&n)
		}); err != nil {
			t.Fatalf("count %s: %v", inst, err)
		}
		if n != 5 {
			t.Errorf("%s kept %d closed rows, want 5", inst, n)
		}
	}
}

// TestDeleteTerminalJobsBefore is section 2.11's "`jobs` in a terminal state 90
// days, never a live or `interrupted` row". `interrupted` is the case worth
// naming: section 2.3 keeps that row precisely so a retry can resume against
// warm objects (D4), so a sweep that treated it as finished would delete the one
// row that makes the warm rerun possible.
func TestDeleteTerminalJobsBefore(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	now := retentionNow()
	old := now.Add(-200 * 24 * time.Hour)
	recent := now.Add(-time.Hour)

	type seed struct {
		state    model.JobState
		at       time.Time
		finished bool
	}
	seeds := []seed{
		{model.JobSucceeded, old, true},
		{model.JobFailed, old, true},
		{model.JobCanceled, old, true},
		{model.JobInterrupted, old, true},
		{model.JobQueued, old, false},
		{model.JobRunning, old, false},
		{model.JobPaused, old, false},
		{model.JobSucceeded, recent, true},
		// A terminal row that never recorded when it ended: the window is "90
		// days after it ended", so there is nothing to measure and it stays.
		{model.JobSucceeded, old, false},
	}

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		for i, sd := range seeds {
			j := model.Job{
				ID: NewID(sd.at.Add(time.Duration(i) * time.Millisecond)),
				// One kind per subject id, so the one-live-per-subject index
				// cannot refuse a second live row.
				Kind: model.JobMaintenance, SubjectType: model.SubjectSystem,
				SubjectID:   fmt.Sprintf("%s-%d", model.SubjectIDMaintenance, i),
				State:       sd.state,
				Priority:    100,
				RunAfter:    sd.at.UnixMilli(),
				MaxAttempts: 1,
				CreatedAt:   sd.at.UnixMilli(),
			}
			if sd.finished {
				j.FinishedAt = ptr(sd.at.UnixMilli())
			}
			if err := s.InsertJob(ctx, tx, j); err != nil {
				return err
			}
		}
		return nil
	})

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		n, err := s.DeleteTerminalJobsBefore(ctx, tx, now.Add(-90*24*time.Hour).UnixMilli())
		if err != nil {
			return err
		}
		if n != 3 {
			t.Errorf("deleted %d jobs, want the 3 old terminal ones", n)
		}
		return nil
	})

	for _, state := range []model.JobState{
		model.JobInterrupted, model.JobQueued, model.JobRunning, model.JobPaused,
	} {
		var n int64
		if err := s.Read(context.Background(), func(ctx context.Context, tx Tx) error {
			return tx.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM jobs WHERE state = ?`, string(state)).Scan(&n)
		}); err != nil {
			t.Fatalf("count %s: %v", state, err)
		}
		if n != 1 {
			t.Errorf("a %s job was swept; only terminal rows may be", state)
		}
	}
	if got := countTable(t, s, "jobs"); got != 6 {
		t.Errorf("%d jobs remain, want 6", got)
	}
}

// TestTrimFitObservationsToRows is section 2.11's "`fit_observations` 2000
// rows", exercised at a smaller cap so the test does not write two thousand.
func TestTrimFitObservationsToRows(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	now := retentionNow()

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		for i := range 20 {
			at := now.Add(-time.Duration(i) * time.Minute)
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO fit_observations (id, at, arch, llamacpp_tag, backend,
				  predicted_bytes, oom, source)
				VALUES (?, ?, 'llama', 'b1', 'cuda', 1, 0, 'instance_start')`,
				NewID(at), at.UnixMilli()); err != nil {
				return err
			}
		}
		return nil
	})

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		n, err := s.TrimFitObservationsToRows(ctx, tx, 5)
		if err != nil {
			return err
		}
		if n != 15 {
			t.Errorf("trimmed %d rows, want 15", n)
		}
		return nil
	})
	if got := countTable(t, s, "fit_observations"); got != 5 {
		t.Errorf("%d observations remain, want the newest 5", got)
	}

	// A cap of zero means "no cap", not "delete everything": a knob read as 0
	// from an unset setting must never empty the calibration corpus.
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		n, err := s.TrimFitObservationsToRows(ctx, tx, 0)
		if err != nil {
			return err
		}
		if n != 0 {
			t.Errorf("a zero cap deleted %d rows", n)
		}
		return nil
	})
}

// TestDeleteDismissedNotificationsBefore is section 2.11's "`notifications`
// dismissed + 30 days". An undismissed notification is never swept whatever its
// age: it is still asking a human for something.
func TestDeleteDismissedNotificationsBefore(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	now := retentionNow()
	old := now.Add(-200 * 24 * time.Hour)
	recent := now.Add(-time.Hour)

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		rows := []struct {
			id        string
			dismissed *int64
		}{
			{"n-old-dismissed", ptr(old.UnixMilli())},
			{"n-recent-dismissed", ptr(recent.UnixMilli())},
			{"n-old-open", nil},
		}
		for _, r := range rows {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO notifications (id, at, severity, code, title, body, dismissed_at)
				VALUES (?, ?, 'warn', 'canary_failed', 't', 'b', ?)`,
				r.id, old.UnixMilli(), r.dismissed); err != nil {
				return err
			}
		}
		return nil
	})

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		n, err := s.DeleteDismissedNotificationsBefore(ctx, tx,
			now.Add(-30*24*time.Hour).UnixMilli())
		if err != nil {
			return err
		}
		if n != 1 {
			t.Errorf("deleted %d notifications, want only the old dismissed one", n)
		}
		return nil
	})

	var open int64
	if err := s.Read(context.Background(), func(ctx context.Context, tx Tx) error {
		return tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM notifications WHERE dismissed_at IS NULL`).Scan(&open)
	}); err != nil {
		t.Fatalf("count undismissed: %v", err)
	}
	if open != 1 {
		t.Error("an undismissed notification was swept; it is still asking for something")
	}
}

// TestDeleteUsageDailyBefore is section 2.11's "`instance_usage_daily` and
// `token_usage_daily` 400 days". `day` is a 'YYYY-MM-DD' UTC string, which sorts
// lexicographically in date order — the reason it is compared as text.
func TestDeleteUsageDailyBefore(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	now := retentionNow()
	seedRetentionInstance(t, s, "inst-a", 8081)

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO api_tokens (id, name, token_hash, prefix, scope, created_at, updated_at)
			VALUES ('tok-1', 'test', 'hash', 'lm_ab', 'global', ?, ?)`,
			now.UnixMilli(), now.UnixMilli()); err != nil {
			return err
		}
		for _, day := range []string{"2023-01-01", "2026-01-01"} {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO instance_usage_daily (instance_id, day, auth_mode)
				VALUES ('inst-a', ?, 'none')`, day); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO token_usage_daily (token_id, instance_id, day)
				VALUES ('tok-1', 'inst-a', ?)`, day); err != nil {
				return err
			}
		}
		return nil
	})

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		n, err := s.DeleteUsageDailyBefore(ctx, tx, "2025-01-01")
		if err != nil {
			return err
		}
		if n != 2 {
			t.Errorf("deleted %d usage rows, want one from each table", n)
		}
		return nil
	})
	if got := countTable(t, s, "instance_usage_daily"); got != 1 {
		t.Errorf("instance_usage_daily has %d rows, want 1", got)
	}
	if got := countTable(t, s, "token_usage_daily"); got != 1 {
		t.Errorf("token_usage_daily has %d rows, want 1", got)
	}
}
