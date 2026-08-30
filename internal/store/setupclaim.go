package store

import (
	"context"
	"fmt"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// setup_claim queries (DESIGN sections 2.2 and 2.2a).
//
// The database holds only sha256(token) — precisely so `db-backups/` entries,
// VACUUM INTO snapshots and `llamaman diagnostics` bundles can never leak a live
// claim credential. The plaintext travels through a 0600 file and never through
// any method in this file. Nothing here reads or writes that file either: minting
// it, printing it and unlinking it are the composition root's, because they are
// filesystem operations whose failure modes (a wrong uid, a half-run --purge) are
// not database failures.

// SetupClaim returns the singleton row, or ErrNotFound on an install that has
// never minted a token — which is the normal state of a database whose
// `admin_account` was created before the claim ever mattered.
func (s *Store) SetupClaim(ctx context.Context, tx Tx) (model.SetupClaim, error) {
	var out model.SetupClaim
	err := tx.QueryRowContext(ctx,
		`SELECT token_hash, token_path, created_at, claimed_at, claimed_from_ip
		   FROM setup_claim WHERE id = 1`).
		Scan(&out.TokenHash, &out.TokenPath, &out.CreatedAt, &out.ClaimedAt, &out.ClaimedFromIP)
	if err != nil {
		return model.SetupClaim{}, notFound(err)
	}
	return out, nil
}

// PutSetupClaim writes the mint of §2.2a step 1 and the ROTATE of step 6 with
// one statement, because they are the same write: a fresh hash and path
// REPLACING whatever was there, with the claim columns cleared.
//
// Rotation is not an edge case to be tolerated but the designed answer to a
// missing token file while `claimed_at IS NULL` — someone deleted it, or an
// `install.sh --purge` half-ran — because a one-time credential nobody can read
// is worse than a fresh one.
func (s *Store) PutSetupClaim(ctx context.Context, tx Tx, tokenHash, tokenPath string, createdAt int64) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO setup_claim (id, token_hash, token_path, created_at, claimed_at, claimed_from_ip)
		 VALUES (1, ?, ?, ?, NULL, NULL)
		 ON CONFLICT(id) DO UPDATE SET
		   token_hash      = excluded.token_hash,
		   token_path      = excluded.token_path,
		   created_at      = excluded.created_at,
		   claimed_at      = NULL,
		   claimed_from_ip = NULL`,
		tokenHash, tokenPath, createdAt)
	if err != nil {
		return fmt.Errorf("put setup_claim: %w", err)
	}
	return nil
}

// ClaimSetup is the BURN of §2.2a step 5: stamp claimed_at and claimed_from_ip
// and set token_path to NULL. It reports whether it changed anything, so a
// second claim — a replayed request, or two browsers racing — is visible to the
// caller as false rather than silently succeeding.
//
// The caller runs this in the SAME transaction that creates `admin_account`, and
// unlinks the file immediately after the commit. A crash between the two leaves
// a file whose row is already claimed, which the next boot removes; that state is
// idempotent and is not an error.
func (s *Store) ClaimSetup(ctx context.Context, tx Tx, at int64, fromIP string) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE setup_claim
		    SET claimed_at = ?, claimed_from_ip = ?, token_path = NULL
		  WHERE id = 1 AND claimed_at IS NULL`,
		at, fromIP)
	if err != nil {
		return false, fmt.Errorf("claim setup: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("claim setup: %w", err)
	}
	return n > 0, nil
}

// ClearSetupTokenPath forgets the file without claiming the token. It is the
// repair for the crash in §2.2a step 5 — the commit landed, the unlink did not —
// and for the stale file §11.1 step 8 removes when `admin_account` exists but a
// `setup-token` file is still on disk.
func (s *Store) ClearSetupTokenPath(ctx context.Context, tx Tx) error {
	_, err := tx.ExecContext(ctx, `UPDATE setup_claim SET token_path = NULL WHERE id = 1`)
	if err != nil {
		return fmt.Errorf("clear setup token path: %w", err)
	}
	return nil
}
