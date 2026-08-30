package store

import (
	"context"
	"fmt"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// idempotency_keys queries (DESIGN section 2.3, D65).
//
// The window, not the key, is the design. A unique index on
// `jobs.idempotency_key` would be permanent and global, which cannot express ten
// minutes: after the window the same key must be allowed to create a NEW job,
// and a client reusing one fixed key across days would collide forever. So the
// key lives in its own table with an expiry, and the middleware's three answers
// fall straight out of the two lookups below:
//
//	hit, route and fingerprint match      → 200 with the recorded job (a replay)
//	hit, fingerprint differs              → 422 idempotency_key_reused (a client bug)
//	miss                                  → delete any expired row with this key,
//	                                        then insert alongside the new job
//
// All of it runs inside the handler's own transaction, so the primary key still
// makes a concurrent double-submit impossible inside the window.

// LiveIdempotencyKey returns an unexpired row for key, or ErrNotFound. `now` is
// compared against `expires_at` so an expired row reads as a miss without having
// been swept yet.
func (s *Store) LiveIdempotencyKey(ctx context.Context, tx Tx, key string, now int64) (model.IdempotencyKey, error) {
	var out model.IdempotencyKey
	err := tx.QueryRowContext(ctx,
		`SELECT key, route, request_fingerprint, job_id, created_at, expires_at
		   FROM idempotency_keys
		  WHERE key = ? AND expires_at > ?`, key, now).
		Scan(&out.Key, &out.Route, &out.RequestFingerprint, &out.JobID, &out.CreatedAt, &out.ExpiresAt)
	if err != nil {
		return model.IdempotencyKey{}, notFound(err)
	}
	return out, nil
}

// InsertIdempotencyKey records a key beside the job it created. It first deletes
// any EXPIRED row with the same key, which is what makes a key reusable the
// moment its window closes; a live row with the same key is left alone, so the
// insert fails on the primary key and the caller's concurrent double-submit is
// refused rather than silently overwritten.
//
// job_id REFERENCES jobs(id) ON DELETE CASCADE, so the job must be inserted
// first in the same transaction.
func (s *Store) InsertIdempotencyKey(ctx context.Context, tx Tx, k model.IdempotencyKey, now int64) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM idempotency_keys WHERE key = ? AND expires_at <= ?`, k.Key, now); err != nil {
		return fmt.Errorf("sweep expired idempotency key %q: %w", k.Key, err)
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO idempotency_keys (key, route, request_fingerprint, job_id, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		k.Key, k.Route, k.RequestFingerprint, k.JobID, k.CreatedAt, k.ExpiresAt)
	if err != nil {
		return fmt.Errorf("insert idempotency key %q: %w", k.Key, err)
	}
	return nil
}

// DeleteIdempotencyKeysBefore is the nightly maintenance sweep: §2.3 keeps rows
// until `expires_at < now - 24h`, well past the window, so a late replay of a
// just-expired key still finds the row that explains it rather than silently
// creating a second job.
func (s *Store) DeleteIdempotencyKeysBefore(ctx context.Context, tx Tx, cutoff int64) (int64, error) {
	res, err := tx.ExecContext(ctx, `DELETE FROM idempotency_keys WHERE expires_at < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("sweep idempotency keys: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("sweep idempotency keys: %w", err)
	}
	return n, nil
}
