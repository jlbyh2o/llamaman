package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// Identity queries: `admin_account`, `sessions`, `login_attempts`, `lockouts`
// (DESIGN section 2.2).
//
// Everything here is mechanical, per §1: no policy, no clock, no hashing. The
// argon2id parameters, the session lifetime, the idle window and the lockout
// arithmetic all belong to internal/auth; what belongs here is that each
// statement is one atomic move a caller can compose with another in ONE
// transaction — which the claim path needs literally, since §2.2a step 5 stamps
// `setup_claim` in the same transaction that creates `admin_account`.

// AdminAccount returns the singleton row, or ErrNotFound when this host has
// never been claimed. That absence IS §3's setup gate, so a caller reads
// ErrNotFound as "not set up" rather than as a failure.
func (s *Store) AdminAccount(ctx context.Context, tx Tx) (model.AdminAccount, error) {
	var out model.AdminAccount
	err := tx.QueryRowContext(ctx,
		`SELECT password_hash, password_set_at, updated_at FROM admin_account WHERE id = 1`).
		Scan(&out.PasswordHash, &out.PasswordSetAt, &out.UpdatedAt)
	if err != nil {
		return model.AdminAccount{}, notFound(err)
	}
	return out, nil
}

// AdminAccountExists answers the setup gate without carrying a hash out of the
// database. It is the query the session middleware makes on every non-public
// request, so it reads one integer rather than a row.
func (s *Store) AdminAccountExists(ctx context.Context, tx Tx) (bool, error) {
	var n int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM admin_account WHERE id = 1`).Scan(&n); err != nil {
		return false, fmt.Errorf("count admin_account: %w", err)
	}
	return n > 0, nil
}

// CreateAdminAccount inserts the singleton row and reports whether it did.
//
// A false return is the LOSER of §2.2a's claim race: two browsers posting
// `/setup/password` at once, or a replayed request. It is deliberately not an
// error — the caller turns it into `409 setup_already_claimed`, which is what
// §3's Auth column says a `setup` route answers once the account exists — and it
// is decided by the database rather than by a check-then-insert, so the two
// callers cannot both read "absent" and both proceed.
func (s *Store) CreateAdminAccount(ctx context.Context, tx Tx, a model.AdminAccount) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`INSERT INTO admin_account (id, password_hash, password_set_at, updated_at)
		 VALUES (1, ?, ?, ?)
		 ON CONFLICT(id) DO NOTHING`,
		a.PasswordHash, a.PasswordSetAt, a.UpdatedAt)
	if err != nil {
		return false, fmt.Errorf("create admin_account: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("create admin_account: %w", err)
	}
	return n > 0, nil
}

// SetAdminPassword replaces the hash. It is what `POST /api/v1/auth/password`
// and `llamaman reset-password` both write, and it never creates the row: a host
// with no account is claimed through the setup flow, not through a password
// change.
func (s *Store) SetAdminPassword(ctx context.Context, tx Tx, hash string, at int64) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE admin_account SET password_hash = ?, password_set_at = ?, updated_at = ?
		  WHERE id = 1`, hash, at, at)
	if err != nil {
		return false, fmt.Errorf("set admin password: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("set admin password: %w", err)
	}
	return n > 0, nil
}

const sessionColumns = `id, token_hash, csrf_secret, created_at, last_seen_at,
	expires_at, revoked_at, ip, user_agent`

// InsertSession writes a freshly minted session. The caller supplies the id (a
// ULID from NewID) and the sha256 of the secret half; the plaintext secret never
// reaches this package.
func (s *Store) InsertSession(ctx context.Context, tx Tx, v model.Session) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO sessions (`+sessionColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		v.ID, v.TokenHash, v.CSRFSecret, v.CreatedAt, v.LastSeenAt,
		v.ExpiresAt, v.RevokedAt, v.IP, v.UserAgent)
	if err != nil {
		return fmt.Errorf("insert session: %w", err)
	}
	return nil
}

// Session returns one row by id, or ErrNotFound. The caller compares the hash
// itself, in constant time: a lookup BY hash would still be a lookup, and the
// design's cookie carries the id precisely so the row can be found without one.
func (s *Store) Session(ctx context.Context, tx Tx, id string) (model.Session, error) {
	var out model.Session
	err := tx.QueryRowContext(ctx,
		`SELECT `+sessionColumns+` FROM sessions WHERE id = ?`, id).
		Scan(&out.ID, &out.TokenHash, &out.CSRFSecret, &out.CreatedAt, &out.LastSeenAt,
			&out.ExpiresAt, &out.RevokedAt, &out.IP, &out.UserAgent)
	if err != nil {
		return model.Session{}, notFound(err)
	}
	return out, nil
}

// ActiveSessions lists the sessions `GET /api/v1/auth/sessions` shows: not
// revoked, not expired at `now`, newest first (ids are ULIDs, so id DESC is
// creation order with a unique tiebreak).
func (s *Store) ActiveSessions(ctx context.Context, tx Tx, now int64) ([]model.Session, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT `+sessionColumns+` FROM sessions
		  WHERE revoked_at IS NULL AND expires_at > ?
		  ORDER BY id DESC`, now)
	if err != nil {
		return nil, fmt.Errorf("select sessions: %w", err)
	}
	defer rows.Close()

	var out []model.Session
	for rows.Next() {
		var v model.Session
		if err := rows.Scan(&v.ID, &v.TokenHash, &v.CSRFSecret, &v.CreatedAt, &v.LastSeenAt,
			&v.ExpiresAt, &v.RevokedAt, &v.IP, &v.UserAgent); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// TouchSession records that a session was used: the sliding half of the session
// window, plus the two audit columns a session listing shows. internal/auth
// rate-limits the call so an authenticated request does not write on every hit.
func (s *Store) TouchSession(ctx context.Context, tx Tx, id string, at int64, ip, userAgent *string) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE sessions SET last_seen_at = ?, ip = ?, user_agent = ?
		  WHERE id = ? AND revoked_at IS NULL`, at, ip, userAgent, id)
	if err != nil {
		return fmt.Errorf("touch session: %w", err)
	}
	return nil
}

// RevokeSession stamps `revoked_at` and reports whether it changed anything. A
// false is a session that was already revoked or never existed, which `DELETE
// /api/v1/auth/sessions/{id}` answers as 404 rather than pretending to succeed.
func (s *Store) RevokeSession(ctx context.Context, tx Tx, id string, at int64) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE sessions SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, at, id)
	if err != nil {
		return false, fmt.Errorf("revoke session: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("revoke session: %w", err)
	}
	return n > 0, nil
}

// RevokeSessionsExcept revokes every live session but one, and returns how many
// it revoked. `POST /api/v1/auth/password` calls it with the caller's own id —
// "revokes all other sessions" (§3.1) — and `llamaman reset-password` calls it
// with the empty string, which revokes every one of them.
func (s *Store) RevokeSessionsExcept(ctx context.Context, tx Tx, keepID string, at int64) (int64, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE sessions SET revoked_at = ?
		  WHERE revoked_at IS NULL AND id <> ?`, at, keepID)
	if err != nil {
		return 0, fmt.Errorf("revoke sessions: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("revoke sessions: %w", err)
	}
	return n, nil
}

// DeleteExpiredSessions is the retention sweep of §2.11: rows past
// `expires_at + 7d`. The cutoff is the caller's, because the retention window is
// a setting.
func (s *Store) DeleteExpiredSessions(ctx context.Context, tx Tx, before int64) (int64, error) {
	res, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ?`, before)
	if err != nil {
		return 0, fmt.Errorf("delete expired sessions: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete expired sessions: %w", err)
	}
	return n, nil
}

// InsertLoginAttempt appends one audit row. Every branch of the login path
// writes one — success, wrong password, locked, no account, bad setup token —
// because the lockout counts them and because "who tried to get in" is the one
// security question this daemon can answer after the fact.
func (s *Store) InsertLoginAttempt(ctx context.Context, tx Tx, a model.LoginAttempt) error {
	var reason any
	if a.Reason != nil {
		reason = string(*a.Reason)
	}
	success := 0
	if a.Success {
		success = 1
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO login_attempts (id, at, ip, success, reason) VALUES (?, ?, ?, ?, ?)`,
		a.ID, a.At, a.IP, success, reason)
	if err != nil {
		return fmt.Errorf("insert login attempt: %w", err)
	}
	return nil
}

// CountFailedLoginAttempts counts the failures from one address at or after
// `since` — the sliding window SPEC §4's "login rate-limited with lockout" is
// measured over. `idx_login_attempts_ip_at` is the index it reads.
//
// `reasons` restricts the count, and the caller always supplies one: not every
// failed row is a GUESS. A `locked` row records a request that was refused
// without the password ever being checked, and counting those would let a client
// that keeps knocking while blocked spend a budget it is not being given the
// chance to spend — the next real attempt after the block expired would re-lock
// instantly. An empty `reasons` counts every failure, which is what a retention
// or reporting caller wants.
func (s *Store) CountFailedLoginAttempts(ctx context.Context, tx Tx, ip string,
	since int64, reasons []model.LoginReason) (int, error) {

	query := `SELECT COUNT(*) FROM login_attempts WHERE ip = ? AND at >= ? AND success = 0`
	args := []any{ip, since}
	if len(reasons) > 0 {
		ph := make([]string, len(reasons))
		for i, r := range reasons {
			ph[i] = "?"
			args = append(args, string(r))
		}
		query += ` AND reason IN (` + strings.Join(ph, ",") + `)`
	}

	var n int
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count login attempts: %w", err)
	}
	return n, nil
}

// HasLoginAttemptSince reports whether one address already has a row with this
// reason at or after `since`. It reads the same `idx_login_attempts_ip_at`
// index the count above does.
//
// It exists for one caller and one property: the `locked` row. A request
// refused because the address is already blocked has nothing to record that the
// `lockouts` row does not already say, and writing one per request would let an
// unauthenticated flood insert a row per request through a write pool that is a
// single connection (§2) — serializing ahead of supervisor status updates,
// ledger closures and job leases. Asking first caps the audit trail at one row
// per lockout EPISODE, which is the same shape §5.8 gives its `inhibited` rows
// through HasInhibitedStartSince, and for the same reason: the refusal must not
// bury the history it is part of.
func (s *Store) HasLoginAttemptSince(ctx context.Context, tx Tx, ip string,
	reason model.LoginReason, since int64) (bool, error) {

	var found int
	err := tx.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM login_attempts
		                WHERE ip = ? AND at >= ? AND reason = ?)`,
		ip, since, string(reason)).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("look for a %s login attempt: %w", reason, err)
	}
	return found != 0, nil
}

// LastSuccessfulLoginAt returns when this address last logged in successfully,
// and whether it ever has.
//
// The lockout counter needs it: a successful login is proof that the operator is
// the operator, so the failures BEFORE it must not be counted against the
// attempts after it. Without this bound, a login that cleared a block would still
// leave the earlier failures inside the sliding window, and the very next typo
// would re-lock the account the operator just got back into.
func (s *Store) LastSuccessfulLoginAt(ctx context.Context, tx Tx, ip string) (int64, bool, error) {
	var at *int64
	if err := tx.QueryRowContext(ctx,
		`SELECT MAX(at) FROM login_attempts WHERE ip = ? AND success = 1`, ip).Scan(&at); err != nil {
		return 0, false, fmt.Errorf("last successful login: %w", err)
	}
	if at == nil {
		return 0, false, nil
	}
	return *at, true, nil
}

// DeleteLoginAttemptsBefore is §2.11's retention sweep for the audit trail: 30
// days by default.
func (s *Store) DeleteLoginAttemptsBefore(ctx context.Context, tx Tx, before int64) (int64, error) {
	res, err := tx.ExecContext(ctx, `DELETE FROM login_attempts WHERE at < ?`, before)
	if err != nil {
		return 0, fmt.Errorf("delete login attempts: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete login attempts: %w", err)
	}
	return n, nil
}

// Lockout returns one address's block, or ErrNotFound when it has never been
// locked out.
func (s *Store) Lockout(ctx context.Context, tx Tx, ip string) (model.Lockout, error) {
	var out model.Lockout
	err := tx.QueryRowContext(ctx,
		`SELECT ip, locked_until, strikes, updated_at FROM lockouts WHERE ip = ?`, ip).
		Scan(&out.IP, &out.LockedUntil, &out.Strikes, &out.UpdatedAt)
	if err != nil {
		return model.Lockout{}, notFound(err)
	}
	return out, nil
}

// PutLockout upserts an address's block. The strike count and the deadline are
// computed by internal/auth, which owns the escalation policy; this method only
// stores what it decided.
func (s *Store) PutLockout(ctx context.Context, tx Tx, v model.Lockout) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO lockouts (ip, locked_until, strikes, updated_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(ip) DO UPDATE SET
		   locked_until = excluded.locked_until,
		   strikes      = excluded.strikes,
		   updated_at   = excluded.updated_at`,
		v.IP, v.LockedUntil, v.Strikes, v.UpdatedAt)
	if err != nil {
		return fmt.Errorf("put lockout: %w", err)
	}
	return nil
}

// DeleteLockout clears an address's block, which a successful login does: the
// strike history exists to slow an attacker down, not to punish the operator who
// finally remembered their password.
func (s *Store) DeleteLockout(ctx context.Context, tx Tx, ip string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM lockouts WHERE ip = ?`, ip); err != nil {
		return fmt.Errorf("delete lockout: %w", err)
	}
	return nil
}
