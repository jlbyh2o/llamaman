package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// The `api_tokens` and `token_instances` tables (DESIGN section 2.9).
//
// This file moves HASHES. It never mints a secret, never hashes one and never
// sees one: `lm_` + base58 of 32 random bytes is minted by internal/tokens, and
// what arrives here is the sha256 of it (D37) plus the `lm_`-and-six-characters
// prefix section 2.9 stores in the clear for display and log correlation.
//
// Two properties of the table are load-bearing and are the reason the methods
// below have the shapes they do:
//
//   - `token_hash` is UNIQUE, which is what makes verification O(1): the gateway
//     hashes the presented secret once and looks the row up by that hash, on an
//     index, rather than scanning. APITokenByHash is that lookup.
//   - `revoked` is TERMINAL and the hash is RETAINED on a revoked row, so a
//     leaked secret can never be re-minted into validity. RevokeAPIToken
//     therefore stamps a state and never deletes, and there is deliberately no
//     DELETE method on this table at all — section 3.12's `DELETE
//     /api/v1/tokens/{id}` is a revoke.

// APIToken is one row of `api_tokens` (§2.9).
type APIToken struct {
	ID   string
	Name string
	// Prefix is `lm_` plus the first six characters of the secret, in the clear.
	// It is what a listing shows and what a journal line can be correlated
	// against; it is not enough to authenticate with.
	Prefix string
	// TokenHash is sha256(secret), hex. D37: a 256-bit uniformly random secret
	// has no dictionary to attack, and an argon2 hash on every inference request
	// would add ~100 ms to every call. The admin PASSWORD keeps argon2id.
	TokenHash string
	Scope     model.TokenScope
	State     model.TokenState
	// RateLimitRPM is the optional per-token token bucket, NULL for unlimited.
	RateLimitRPM *int64
	// ExpiresAt is NULL for a token that never expires.
	ExpiresAt *int64

	CreatedAt int64
	UpdatedAt int64
	RevokedAt *int64

	LastUsedAt *int64
	LastUsedIP *string
	// RequestCount is advanced by the gateway at most once per 10 s per token
	// (§9.3), so a busy token costs one UPDATE every ten seconds rather than one
	// per request through a write pool that is a single connection.
	RequestCount int64
}

// Revoked reports whether this row has reached the terminal state.
func (t APIToken) Revoked() bool { return t.State == model.TokenRevoked }

const apiTokenColumns = `id, name, prefix, token_hash, scope, state, rate_limit_rpm,
	expires_at, created_at, updated_at, revoked_at, last_used_at, last_used_ip, request_count`

// InsertAPIToken writes a freshly minted token. The caller supplies the id (a
// ULID from NewID), the prefix and the hash; the plaintext secret never reaches
// this package.
func (s *Store) InsertAPIToken(ctx context.Context, tx Tx, t APIToken) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO api_tokens (`+apiTokenColumns+`)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.Name, t.Prefix, t.TokenHash, string(t.Scope), string(t.State),
		t.RateLimitRPM, t.ExpiresAt, t.CreatedAt, t.UpdatedAt, t.RevokedAt,
		t.LastUsedAt, t.LastUsedIP, t.RequestCount)
	if err != nil {
		return fmt.Errorf("insert api token: %w", err)
	}
	return nil
}

// APIToken returns one row by id, or ErrNotFound.
func (s *Store) APIToken(ctx context.Context, tx Tx, id string) (APIToken, error) {
	return scanAPIToken(tx.QueryRowContext(ctx,
		`SELECT `+apiTokenColumns+` FROM api_tokens WHERE id = ?`, id))
}

// APITokenByHash is the gateway's verification lookup: one indexed read on the
// UNIQUE `token_hash`. ErrNotFound is the `unknown` denial reason and is an
// ordinary answer, not a failure.
//
// The row is returned WHATEVER its state — revoked, disabled and expired rows all
// come back — because the caller must be able to tell those three denials apart
// for `gateway_denials_daily`, and because a revoked hash matching is exactly the
// fact worth counting.
func (s *Store) APITokenByHash(ctx context.Context, tx Tx, hash string) (APIToken, error) {
	return scanAPIToken(tx.QueryRowContext(ctx,
		`SELECT `+apiTokenColumns+` FROM api_tokens WHERE token_hash = ?`, hash))
}

// APITokens lists every token, newest first (ids are ULIDs, so id DESC is
// creation order with a unique tiebreak). Revoked rows are included: they are
// history the listing shows, and hiding them would make a leaked-and-revoked
// token look like one that never existed.
func (s *Store) APITokens(ctx context.Context, tx Tx) ([]APIToken, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT `+apiTokenColumns+` FROM api_tokens ORDER BY id DESC`)
	if err != nil {
		return nil, fmt.Errorf("select api tokens: %w", err)
	}
	defer rows.Close()

	var out []APIToken
	for rows.Next() {
		t, err := scanAPIToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// UpdateAPIToken writes the four editable columns of `PATCH /api/v1/tokens/{id}`
// plus the state, and reports whether it changed anything.
//
// It refuses to move a row OUT of `revoked` in SQL rather than in the caller:
// `active|disabled → revoked` is terminal (§2.9), and a guard that lives in the
// statement cannot be forgotten by a second caller. A false return on a revoked
// row is what the service turns into a refusal.
func (s *Store) UpdateAPIToken(ctx context.Context, tx Tx, t APIToken) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE api_tokens
		    SET name = ?, scope = ?, state = ?, rate_limit_rpm = ?, expires_at = ?,
		        updated_at = ?, revoked_at = ?
		  WHERE id = ? AND state <> 'revoked'`,
		t.Name, string(t.Scope), string(t.State), t.RateLimitRPM, t.ExpiresAt,
		t.UpdatedAt, t.RevokedAt, t.ID)
	if err != nil {
		return false, fmt.Errorf("update api token: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("update api token: %w", err)
	}
	return n > 0, nil
}

// RevokeAPIToken moves a token to the terminal state and stamps `revoked_at`. A
// false return means the row was already revoked or never existed, which
// `DELETE /api/v1/tokens/{id}` answers as a no-op and a 404 respectively rather
// than pretending to have done something.
//
// The hash is deliberately left in place: retaining it is what makes a leaked
// secret permanently unusable instead of merely unassigned.
func (s *Store) RevokeAPIToken(ctx context.Context, tx Tx, id string, at int64) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE api_tokens SET state = 'revoked', revoked_at = ?, updated_at = ?
		  WHERE id = ? AND state <> 'revoked'`, at, at, id)
	if err != nil {
		return false, fmt.Errorf("revoke api token: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("revoke api token: %w", err)
	}
	return n > 0, nil
}

// TouchAPIToken records use: `last_used_at`, `last_used_ip` and a request-count
// increment. The gateway batches it to at most once per 10 s per token (§9.3),
// which is why `delta` is a count rather than an implied one.
func (s *Store) TouchAPIToken(ctx context.Context, tx Tx, id string,
	at int64, ip *string, delta int64) (bool, error) {

	res, err := tx.ExecContext(ctx,
		`UPDATE api_tokens
		    SET last_used_at = ?, last_used_ip = ?, request_count = request_count + ?
		  WHERE id = ?`, at, ip, delta, id)
	if err != nil {
		return false, fmt.Errorf("touch api token: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("touch api token: %w", err)
	}
	return n > 0, nil
}

// TokenInstances returns the instance ids one `scope='instances'` token reaches,
// in id order. An empty result on such a token is a token that reaches nothing,
// which is a legal and deliberately un-special state.
func (s *Store) TokenInstances(ctx context.Context, tx Tx, tokenID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT instance_id FROM token_instances WHERE token_id = ? ORDER BY instance_id`, tokenID)
	if err != nil {
		return nil, fmt.Errorf("select token instances: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan token instance: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// AllTokenInstances returns every scope row grouped by token id. The listing
// endpoint reads it once rather than making one query per token, and the gateway
// has no use for it at all — a verification reads exactly the one token it was
// presented with.
func (s *Store) AllTokenInstances(ctx context.Context, tx Tx) (map[string][]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT token_id, instance_id FROM token_instances ORDER BY token_id, instance_id`)
	if err != nil {
		return nil, fmt.Errorf("select token instances: %w", err)
	}
	defer rows.Close()

	out := map[string][]string{}
	for rows.Next() {
		var tokenID, instanceID string
		if err := rows.Scan(&tokenID, &instanceID); err != nil {
			return nil, fmt.Errorf("scan token instance: %w", err)
		}
		out[tokenID] = append(out[tokenID], instanceID)
	}
	return out, rows.Err()
}

// SetTokenInstances replaces one token's scope rows with exactly the ids given.
// It is a replace rather than an add/remove pair because that is what
// `PATCH /api/v1/tokens/{id}` sends: the client owns the whole list.
//
// A nonexistent instance id is refused by the foreign key, which is the check
// this layer wants — the alternative is a scope row pointing at nothing, which
// would silently widen or narrow a token depending on which way the reader
// resolved it.
func (s *Store) SetTokenInstances(ctx context.Context, tx Tx, tokenID string, instanceIDs []string) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM token_instances WHERE token_id = ?`, tokenID); err != nil {
		return fmt.Errorf("clear token instances: %w", err)
	}
	if len(instanceIDs) == 0 {
		return nil
	}

	// One statement, so a partial write cannot leave a token scoped to half of
	// what was asked for even if the transaction is rolled back by a later
	// failure the caller recovers from.
	args := make([]any, 0, len(instanceIDs)*2)
	values := make([]string, 0, len(instanceIDs))
	seen := map[string]struct{}{}
	for _, id := range instanceIDs {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		values = append(values, "(?, ?)")
		args = append(args, tokenID, id)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO token_instances (token_id, instance_id) VALUES `+
			strings.Join(values, ", "), args...); err != nil {
		return fmt.Errorf("set token instances: %w", err)
	}
	return nil
}

func scanAPIToken(sc rowScanner) (APIToken, error) {
	var (
		out   APIToken
		scope string
		state string
	)
	err := sc.Scan(&out.ID, &out.Name, &out.Prefix, &out.TokenHash, &scope, &state,
		&out.RateLimitRPM, &out.ExpiresAt, &out.CreatedAt, &out.UpdatedAt, &out.RevokedAt,
		&out.LastUsedAt, &out.LastUsedIP, &out.RequestCount)
	if err != nil {
		return APIToken{}, notFound(err)
	}
	out.Scope = model.TokenScope(scope)
	out.State = model.TokenState(state)
	return out, nil
}
