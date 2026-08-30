package store

import (
	"context"
	"fmt"
)

// The retention sweeps §2.11 gives the nightly `maintenance` job that no other
// file already owns.
//
// `sessions`, `login_attempts` and `idempotency_keys` have theirs beside the
// queries that write them, because those tables have one writer each and the
// sweep is a line in that writer's story. `events` does not: every subsystem in
// this design appends to it, so its two rules live here, together, where the
// pair can be read as the one policy it is — "90 days OR 200k rows", whichever
// bites first (§2.11).
//
// Both are deliberately expressed as "delete what is outside the window" rather
// than "keep N": an append-only table swept by a nightly job has no other honest
// shape, and a cap enforced on every append would put a DELETE inside every
// transition in the system.

// DeleteEventsBefore is the age half of §2.11's events rule: rows older than the
// caller's cutoff, which is `now - retention.events_days`.
func (s *Store) DeleteEventsBefore(ctx context.Context, tx Tx, before int64) (int64, error) {
	res, err := tx.ExecContext(ctx, `DELETE FROM events WHERE at < ?`, before)
	if err != nil {
		return 0, fmt.Errorf("sweep events by age: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("sweep events by age: %w", err)
	}
	return n, nil
}

// TrimEventsToRows is the size half: whatever survived the age sweep is trimmed
// to the newest `max` rows.
//
// The ordering is by `id`, not by `at`. The id is a ULID minted at append time,
// so it sorts by creation with no ties to break — and it is also the SSE
// `Last-Event-ID` cursor (§2.11), which means trimming by id is trimming
// exactly the tail a reconnecting client can no longer ask for. Two events
// written in the same millisecond would tie on `at` and could be dropped in
// either order, leaving a cursor pointing into the middle of a deleted pair.
func (s *Store) TrimEventsToRows(ctx context.Context, tx Tx, max int64) (int64, error) {
	if max <= 0 {
		return 0, nil
	}
	res, err := tx.ExecContext(ctx,
		`DELETE FROM events
		  WHERE id NOT IN (SELECT id FROM events ORDER BY id DESC LIMIT ?)`, max)
	if err != nil {
		return 0, fmt.Errorf("trim events to %d rows: %w", max, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("trim events to %d rows: %w", max, err)
	}
	return n, nil
}

// CountEvents is what makes the trim skippable on the overwhelmingly common
// night where the table is nowhere near the cap: a COUNT over an append-only
// table is one index scan, and the DELETE it avoids is a sort of the whole
// table.
func (s *Store) CountEvents(ctx context.Context, tx Tx) (int64, error) {
	var n int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM events`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count events: %w", err)
	}
	return n, nil
}

// TrimInstanceStartsPerInstance is §2.11's "`instance_starts` 500 per instance
// (**closed rows only** — an open row is never pruned out from under the
// supervisor)".
//
// PER INSTANCE, not globally: a fleet of twenty instances keeps twenty
// histories, and a global cap would let one crash-looping instance erase every
// other instance's record of itself in a night.
//
// "Closed" is `outcome IS NOT NULL` (§2.8): a NULL outcome is a run still in
// flight, and the supervisor reads that row to decide what to do next. Deleting
// one would not merely lose history — it would lose the run.
func (s *Store) TrimInstanceStartsPerInstance(ctx context.Context, tx Tx, keep int64) (int64, error) {
	if keep <= 0 {
		return 0, nil
	}
	res, err := tx.ExecContext(ctx, `
		DELETE FROM instance_starts
		 WHERE outcome IS NOT NULL
		   AND id NOT IN (
		         SELECT id FROM instance_starts AS keeper
		          WHERE keeper.instance_id = instance_starts.instance_id
		            AND keeper.outcome IS NOT NULL
		          ORDER BY keeper.at DESC, keeper.id DESC
		          LIMIT ?)`, keep)
	if err != nil {
		return 0, fmt.Errorf("trim instance_starts to %d per instance: %w", keep, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("trim instance_starts to %d per instance: %w", keep, err)
	}
	return n, nil
}

// TrimFitObservationsToRows is §2.11's "`fit_observations` 2000 rows".
//
// Newest first by id, for the same reason the events trim orders by id: the id
// is a ULID minted at insert, so it sorts by creation with no ties to break,
// while two observations recorded in the same millisecond would tie on `at`.
func (s *Store) TrimFitObservationsToRows(ctx context.Context, tx Tx, max int64) (int64, error) {
	if max <= 0 {
		return 0, nil
	}
	res, err := tx.ExecContext(ctx,
		`DELETE FROM fit_observations
		  WHERE id NOT IN (SELECT id FROM fit_observations ORDER BY id DESC LIMIT ?)`, max)
	if err != nil {
		return 0, fmt.Errorf("trim fit_observations to %d rows: %w", max, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("trim fit_observations to %d rows: %w", max, err)
	}
	return n, nil
}

// DeleteTerminalJobsBefore is §2.11's "`jobs` in a terminal state
// (`succeeded`/`failed`/`canceled`) 90 days, never a live or `interrupted` row".
//
// The state list is spelled out rather than expressed as "not live", because
// `interrupted` is neither: §2.3 keeps an interrupted row precisely so a retry
// can resume against warm objects, and a sweep that treated it as finished would
// delete the one row that makes D4's warm rerun possible.
//
// `finished_at` is the cutoff column, and a terminal row with none is kept: the
// window is "90 days after it ended", and a row that cannot say when it ended is
// not a row to guess about.
func (s *Store) DeleteTerminalJobsBefore(ctx context.Context, tx Tx, before int64) (int64, error) {
	res, err := tx.ExecContext(ctx, `
		DELETE FROM jobs
		 WHERE state IN ('succeeded','failed','canceled')
		   AND finished_at IS NOT NULL
		   AND finished_at < ?`, before)
	if err != nil {
		return 0, fmt.Errorf("sweep terminal jobs: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("sweep terminal jobs: %w", err)
	}
	return n, nil
}

// DeleteDismissedNotificationsBefore is §2.11's "`notifications` dismissed + 30
// days". An UNDISMISSED notification is never swept whatever its age: it is
// still asking a human for something.
func (s *Store) DeleteDismissedNotificationsBefore(ctx context.Context, tx Tx, before int64) (int64, error) {
	res, err := tx.ExecContext(ctx,
		`DELETE FROM notifications WHERE dismissed_at IS NOT NULL AND dismissed_at < ?`, before)
	if err != nil {
		return 0, fmt.Errorf("sweep dismissed notifications: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("sweep dismissed notifications: %w", err)
	}
	return n, nil
}

// DeleteUsageDailyBefore is §2.11's "`instance_usage_daily` and
// `token_usage_daily` 400 days".
//
// `day` is a 'YYYY-MM-DD' UTC string in both tables, and it is compared as a
// string: that format sorts lexicographically in date order, which is the whole
// reason §2.9 chose it, and comparing it as text keeps the sweep an index-free
// scan of two small tables rather than 400 date conversions.
func (s *Store) DeleteUsageDailyBefore(ctx context.Context, tx Tx, day string) (int64, error) {
	var total int64
	for _, table := range []string{"instance_usage_daily", "token_usage_daily"} {
		res, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE day < ?`, day)
		if err != nil {
			return total, fmt.Errorf("sweep %s: %w", table, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("sweep %s: %w", table, err)
		}
		total += n
	}
	return total, nil
}
