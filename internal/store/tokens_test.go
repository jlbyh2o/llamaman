package store

import (
	"context"
	"errors"
	"testing"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// The `api_tokens`, `token_instances` and the three accounting tables of DESIGN
// section 2.9, against a real migrated database — because what is being asserted
// here is what SQLite does: a UNIQUE index, a terminal-state guard written into
// an UPDATE, cascading foreign keys, and the additive upsert that D56's flusher
// depends on.

func seedToken(t *testing.T, s *Store, id, hash string, scope model.TokenScope) APIToken {
	t.Helper()
	tok := APIToken{
		ID:        id,
		Name:      "token " + id,
		Prefix:    "lm_" + hash[:6],
		TokenHash: hash,
		Scope:     scope,
		State:     model.TokenActive,
		CreatedAt: 1000,
		UpdatedAt: 1000,
	}
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		return s.InsertAPIToken(ctx, tx, tok)
	})
	return tok
}

// TestAPITokenRoundTrip covers insert, both lookups and the listing order.
func TestAPITokenRoundTrip(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	ctx := context.Background()

	rpm := int64(120)
	expires := int64(1_700_000_000_000)
	want := APIToken{
		ID:           "01JTOKEN00000000000000000A",
		Name:         "laptop",
		Prefix:       "lm_abc123",
		TokenHash:    "a1b2c3",
		Scope:        model.ScopeInstances,
		State:        model.TokenActive,
		RateLimitRPM: &rpm,
		ExpiresAt:    &expires,
		CreatedAt:    10,
		UpdatedAt:    10,
	}
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		return s.InsertAPIToken(ctx, tx, want)
	})

	got, err := s.APIToken(ctx, s.RO, want.ID)
	if err != nil {
		t.Fatalf("APIToken: %v", err)
	}
	if got.Name != want.Name || got.Prefix != want.Prefix || got.TokenHash != want.TokenHash {
		t.Errorf("round trip lost a field: %+v", got)
	}
	if got.Scope != model.ScopeInstances || got.State != model.TokenActive {
		t.Errorf("scope/state = %q/%q", got.Scope, got.State)
	}
	if got.RateLimitRPM == nil || *got.RateLimitRPM != rpm {
		t.Errorf("rate_limit_rpm = %v, want %d", got.RateLimitRPM, rpm)
	}
	if got.RequestCount != 0 {
		t.Errorf("request_count = %d on a fresh token", got.RequestCount)
	}

	byHash, err := s.APITokenByHash(ctx, s.RO, want.TokenHash)
	if err != nil {
		t.Fatalf("APITokenByHash: %v", err)
	}
	if byHash.ID != want.ID {
		t.Errorf("APITokenByHash found %q, want %q", byHash.ID, want.ID)
	}

	if _, err := s.APIToken(ctx, s.RO, "01JNOSUCH0000000000000000A"); !errors.Is(err, ErrNotFound) {
		t.Errorf("an unknown id = %v, want ErrNotFound", err)
	}
	if _, err := s.APITokenByHash(ctx, s.RO, "nothing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("an unknown hash = %v, want ErrNotFound", err)
	}
}

// TestTokenHashIsUnique: `token_hash` is UNIQUE, which is what makes
// verification one indexed read rather than a scan.
func TestTokenHashIsUnique(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	seedToken(t, s, "01JTOKEN00000000000000000A", "samehash", model.ScopeGlobal)

	err := s.Write(context.Background(), func(ctx context.Context, tx Tx) error {
		return s.InsertAPIToken(ctx, tx, APIToken{
			ID: "01JTOKEN00000000000000000B", Name: "b", Prefix: "lm_b",
			TokenHash: "samehash", Scope: model.ScopeGlobal, State: model.TokenActive,
			CreatedAt: 1, UpdatedAt: 1,
		})
	})
	if err == nil {
		t.Fatal("two tokens were stored under one hash")
	}
}

// TestRevokeIsTerminalInSQL: the guard lives in the statement, not in the
// caller, so a second caller cannot forget it.
func TestRevokeIsTerminalInSQL(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	ctx := context.Background()
	tok := seedToken(t, s, "01JTOKEN00000000000000000A", "hash-a", model.ScopeGlobal)

	var revoked bool
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		var err error
		revoked, err = s.RevokeAPIToken(ctx, tx, tok.ID, 2000)
		return err
	})
	if !revoked {
		t.Fatal("the first revoke reported no change")
	}

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		var err error
		revoked, err = s.RevokeAPIToken(ctx, tx, tok.ID, 3000)
		return err
	})
	if revoked {
		t.Error("revoking twice reported a second change")
	}

	// An UPDATE cannot bring it back.
	tok.State = model.TokenActive
	tok.UpdatedAt = 4000
	var changed bool
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		var err error
		changed, err = s.UpdateAPIToken(ctx, tx, tok)
		return err
	})
	if changed {
		t.Error("UpdateAPIToken moved a token out of `revoked`, which is terminal")
	}

	got, err := s.APIToken(ctx, s.RO, tok.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != model.TokenRevoked {
		t.Errorf("state = %q, want revoked", got.State)
	}
	if got.RevokedAt == nil || *got.RevokedAt != 2000 {
		t.Errorf("revoked_at = %v, want the FIRST revoke's instant", got.RevokedAt)
	}
	// The hash is retained: a leaked secret can never be re-minted into validity.
	if got.TokenHash != "hash-a" {
		t.Errorf("token_hash = %q; it must be retained on a revoked row", got.TokenHash)
	}
}

// TestTouchAccumulates: the gateway batches to at most once per 10 s per token,
// so the delta is a count rather than an implied one.
func TestTouchAccumulates(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	ctx := context.Background()
	tok := seedToken(t, s, "01JTOKEN00000000000000000A", "hash-a", model.ScopeGlobal)

	ip := "192.0.2.10"
	for _, delta := range []int64{12, 30} {
		mustWrite(t, s, func(ctx context.Context, tx Tx) error {
			ok, err := s.TouchAPIToken(ctx, tx, tok.ID, 5000, &ip, delta)
			if err == nil && !ok {
				t.Error("TouchAPIToken reported no row")
			}
			return err
		})
	}

	got, err := s.APIToken(ctx, s.RO, tok.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RequestCount != 42 {
		t.Errorf("request_count = %d, want 42", got.RequestCount)
	}
	if got.LastUsedIP == nil || *got.LastUsedIP != ip {
		t.Errorf("last_used_ip = %v, want %q", got.LastUsedIP, ip)
	}
}

// TestSetTokenInstancesReplaces: `PATCH /api/v1/tokens/{id}` sends the whole
// list, so the write is a replace.
func TestSetTokenInstancesReplaces(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	ctx := context.Background()
	const a, b, c = "01JINST0000000000000000001", "01JINST0000000000000000002", "01JINST0000000000000000003"
	seedInstance(t, s, newInstance(a, "inst1", 8081, 21001))
	seedInstance(t, s, newInstance(b, "inst2", 8082, 21002))
	seedInstance(t, s, newInstance(c, "inst3", 8083, 21003))
	tok := seedToken(t, s, "01JTOKEN00000000000000000A", "hash-a", model.ScopeInstances)

	set := func(ids ...string) {
		t.Helper()
		mustWrite(t, s, func(ctx context.Context, tx Tx) error {
			return s.SetTokenInstances(ctx, tx, tok.ID, ids)
		})
	}
	read := func() []string {
		t.Helper()
		ids, err := s.TokenInstances(ctx, s.RO, tok.ID)
		if err != nil {
			t.Fatal(err)
		}
		return ids
	}

	// A duplicate in the request is not a primary-key violation: the client
	// owns the list and may repeat itself.
	set(a, b, a)
	if got := read(); len(got) != 2 || got[0] != a || got[1] != b {
		t.Errorf("scope = %v, want [%s %s]", got, a, b)
	}

	set(c)
	if got := read(); len(got) != 1 || got[0] != c {
		t.Errorf("scope = %v, want [%s] — the write is a replace", got, c)
	}

	set()
	if got := read(); len(got) != 0 {
		t.Errorf("scope = %v, want empty", got)
	}

	// A scope row for an instance that does not exist is refused by the foreign
	// key, so a token can never be scoped to nothing-in-particular.
	err := s.Write(ctx, func(ctx context.Context, tx Tx) error {
		return s.SetTokenInstances(ctx, tx, tok.ID, []string{"01JNOSUCHINSTANCE000000000"})
	})
	if err == nil {
		t.Error("a scope row naming no instance was accepted")
	}
}

// TestAllTokenInstancesGroups is the listing endpoint's one query.
func TestAllTokenInstancesGroups(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	ctx := context.Background()
	const a, b = "01JINST0000000000000000001", "01JINST0000000000000000002"
	seedInstance(t, s, newInstance(a, "inst1", 8081, 21001))
	seedInstance(t, s, newInstance(b, "inst2", 8082, 21002))
	one := seedToken(t, s, "01JTOKEN00000000000000000A", "hash-a", model.ScopeInstances)
	two := seedToken(t, s, "01JTOKEN00000000000000000B", "hash-b", model.ScopeInstances)
	seedToken(t, s, "01JTOKEN00000000000000000C", "hash-c", model.ScopeGlobal)

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		if err := s.SetTokenInstances(ctx, tx, one.ID, []string{a, b}); err != nil {
			return err
		}
		return s.SetTokenInstances(ctx, tx, two.ID, []string{b})
	})

	got, err := s.AllTokenInstances(ctx, s.RO)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("groups = %d, want 2 — a global token has no scope rows", len(got))
	}
	if len(got[one.ID]) != 2 || len(got[two.ID]) != 1 {
		t.Errorf("groups = %v", got)
	}

	// Listing order: newest first, so a fresh token is at the top of the screen.
	list, err := s.APITokens(ctx, s.RO)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 || list[0].ID != "01JTOKEN00000000000000000C" {
		t.Errorf("listing is not newest-first: %v", idsOf(list))
	}
}

func idsOf(rows []APIToken) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}

// TestDeletingATokenCascadesItsScopeAndUsage: both usage tables and the scope
// rows reference `api_tokens(id) ON DELETE CASCADE`. Nothing in this design
// deletes a token — revoke is soft — but the schema says cascade, and a schema
// claim nobody tests is a schema claim nobody can rely on.
func TestDeletingATokenCascadesItsScopeAndUsage(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	ctx := context.Background()
	const inst = "01JINST0000000000000000001"
	seedInstance(t, s, newInstance(inst, "inst1", 8081, 21001))
	tok := seedToken(t, s, "01JTOKEN00000000000000000A", "hash-a", model.ScopeInstances)

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		if err := s.SetTokenInstances(ctx, tx, tok.ID, []string{inst}); err != nil {
			return err
		}
		return s.AddTokenUsage(ctx, tx, TokenUsageDelta{
			TokenID: tok.ID, InstanceID: inst, Day: "2026-03-01", Requests: 1,
		})
	})

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM api_tokens WHERE id = ?`, tok.ID)
		return err
	})

	ids, err := s.TokenInstances(ctx, s.RO, tok.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Errorf("token_instances survived the delete: %v", ids)
	}
	rows, err := s.TokenUsage(ctx, s.RO, tok.ID, UsageRange{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("token_usage_daily survived the delete: %v", rows)
	}
}
