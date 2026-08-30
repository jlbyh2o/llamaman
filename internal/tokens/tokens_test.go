package tokens

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// fakeStore is the row store. Only internal/store contains SQL (D49 invariant
// 1), so this package's tests fake it rather than carrying an INSERT.
type fakeStore struct {
	mu     sync.Mutex
	rows   map[string]store.APIToken
	byHash map[string]string
	scopes map[string][]string

	hashReads int
	touches   []touch
	writeErr  error
}

type touch struct {
	id    string
	at    int64
	ip    *string
	delta int64
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		rows:   map[string]store.APIToken{},
		byHash: map[string]string{},
		scopes: map[string][]string{},
	}
}

func (f *fakeStore) Read(ctx context.Context, fn func(context.Context, store.Tx) error) error {
	return fn(ctx, nil)
}

func (f *fakeStore) Write(ctx context.Context, fn func(context.Context, store.Tx) error) error {
	f.mu.Lock()
	err := f.writeErr
	f.mu.Unlock()
	if err != nil {
		return err
	}
	return fn(ctx, nil)
}

func (f *fakeStore) InsertAPIToken(_ context.Context, _ store.Tx, t store.APIToken) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, dup := f.byHash[t.TokenHash]; dup {
		return errors.New("UNIQUE constraint failed: api_tokens.token_hash")
	}
	f.rows[t.ID] = t
	f.byHash[t.TokenHash] = t.ID
	return nil
}

func (f *fakeStore) APIToken(_ context.Context, _ store.Tx, id string) (store.APIToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.rows[id]
	if !ok {
		return store.APIToken{}, store.ErrNotFound
	}
	return t, nil
}

func (f *fakeStore) APITokenByHash(_ context.Context, _ store.Tx, hash string) (store.APIToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hashReads++
	id, ok := f.byHash[hash]
	if !ok {
		return store.APIToken{}, store.ErrNotFound
	}
	return f.rows[id], nil
}

func (f *fakeStore) APITokens(_ context.Context, _ store.Tx) ([]store.APIToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.APIToken, 0, len(f.rows))
	for _, t := range f.rows {
		out = append(out, t)
	}
	return out, nil
}

func (f *fakeStore) UpdateAPIToken(_ context.Context, _ store.Tx, t store.APIToken) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cur, ok := f.rows[t.ID]
	if !ok || cur.State == model.TokenRevoked {
		return false, nil
	}
	f.rows[t.ID] = t
	return true, nil
}

func (f *fakeStore) RevokeAPIToken(_ context.Context, _ store.Tx, id string, at int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.rows[id]
	if !ok || t.State == model.TokenRevoked {
		return false, nil
	}
	t.State, t.RevokedAt, t.UpdatedAt = model.TokenRevoked, &at, at
	f.rows[id] = t
	return true, nil
}

func (f *fakeStore) TouchAPIToken(_ context.Context, _ store.Tx, id string,
	at int64, ip *string, delta int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touches = append(f.touches, touch{id: id, at: at, ip: ip, delta: delta})
	t, ok := f.rows[id]
	if !ok {
		return false, nil
	}
	t.LastUsedAt, t.LastUsedIP = &at, ip
	t.RequestCount += delta
	f.rows[id] = t
	return true, nil
}

func (f *fakeStore) TokenInstances(_ context.Context, _ store.Tx, tokenID string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.scopes[tokenID], nil
}

func (f *fakeStore) AllTokenInstances(_ context.Context, _ store.Tx) (map[string][]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string][]string{}
	for k, v := range f.scopes {
		out[k] = append([]string(nil), v...)
	}
	return out, nil
}

func (f *fakeStore) SetTokenInstances(_ context.Context, _ store.Tx, tokenID string, ids []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(ids) == 0 {
		delete(f.scopes, tokenID)
		return nil
	}
	f.scopes[tokenID] = append([]string(nil), ids...)
	return nil
}

func (f *fakeStore) TokenUsage(_ context.Context, _ store.Tx, _ string,
	_ store.UsageRange) ([]store.TokenUsageRow, error) {
	return nil, nil
}

func (f *fakeStore) touchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.touches)
}

// clock is a hand-advanced clock, so the ten-second touch floor and the token
// bucket can be tested without sleeping.
type clock struct {
	mu sync.Mutex
	at time.Time
}

func newClock() *clock { return &clock{at: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

func newService(t *testing.T, st *fakeStore, clk *clock) *Service {
	t.Helper()
	cfg := Config{Store: st}
	if clk != nil {
		cfg.Now = clk.now
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("tokens.New: %v", err)
	}
	return s
}

// TestSecretFormat is section 9.3's format sentence, asserted field by field:
// `lm_` + base58 of 32 crypto/rand bytes, with the prefix stored in the clear
// and the hash a sha256 of the WHOLE secret (D37).
func TestSecretFormat(t *testing.T) {
	t.Parallel()

	secret, err := NewSecret(nil)
	if err != nil {
		t.Fatalf("NewSecret: %v", err)
	}

	if !strings.HasPrefix(secret, Prefix) {
		t.Errorf("secret %q does not start with %q", DisplayPrefix(secret), Prefix)
	}
	body := strings.TrimPrefix(secret, Prefix)
	if len(body) < 40 || len(body) > 44 {
		t.Errorf("base58 body is %d characters; 32 bytes encodes to 40-44", len(body))
	}
	// Base58 exists so the secret survives a double-click, a URL and a hand
	// transcription: no +, /, =, and none of the confusable characters.
	for _, r := range body {
		if !strings.ContainsRune(base58Alphabet, r) {
			t.Errorf("secret contains %q, which is not in the base58 alphabet", r)
		}
	}

	if got, want := DisplayPrefix(secret), Prefix+body[:6]; got != want {
		t.Errorf("DisplayPrefix = %q, want %q", got, want)
	}
	if len(DisplayPrefix(secret)) != len(Prefix)+6 {
		t.Errorf("the stored prefix is %d characters; §2.9 says `lm_` plus six",
			len(DisplayPrefix(secret)))
	}

	sum := sha256.Sum256([]byte(secret))
	if got, want := Hash(secret), hex.EncodeToString(sum[:]); got != want {
		t.Errorf("Hash is not sha256 of the whole secret")
	}
	if strings.Contains(Hash(secret), body) {
		t.Error("the hash contains the secret")
	}
}

// TestSecretsAreDistinct is the cheap sanity check that the entropy source is
// actually being read: a mint that returned a constant would pass every other
// test in this file.
func TestSecretsAreDistinct(t *testing.T) {
	t.Parallel()

	seen := map[string]struct{}{}
	for range 200 {
		s, err := NewSecret(nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, dup := seen[s]; dup {
			t.Fatal("two mints produced the same secret")
		}
		seen[s] = struct{}{}
	}
}

// TestShortEntropyIsAnError: there is no such thing as a token that is nearly
// random enough.
func TestShortEntropyIsAnError(t *testing.T) {
	t.Parallel()

	if _, err := NewSecret(bytes.NewReader([]byte("too short"))); err == nil {
		t.Fatal("NewSecret accepted a short read")
	}
}

// TestBase58PreservesLeadingZeroBytes: leading zero bytes encode as leading
// '1's, which is what keeps the encoding injective. Without it, one secret in
// 256 would collide with a shorter one.
func TestBase58PreservesLeadingZeroBytes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   []byte
		want string
	}{
		{in: []byte{0}, want: "1"},
		{in: []byte{0, 0}, want: "11"},
		{in: []byte{0, 1}, want: "12"},
		{in: []byte{1}, want: "2"},
		{in: []byte{57}, want: "z"},
		{in: []byte{58}, want: "21"},
	}
	for _, tc := range cases {
		if got := base58Encode(tc.in); got != tc.want {
			t.Errorf("base58Encode(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestVerifyDeniesInTheDocumentedOrder walks section 2.9's reason list. The
// ORDER matters: a denial must name the first thing wrong, so "expired" is not
// reported for a revoked token and "scope" is not reported for a disabled one.
func TestVerifyDeniesInTheDocumentedOrder(t *testing.T) {
	t.Parallel()

	const instanceID = "01JINSTANCE0000000000000001"
	clk := newClock()

	cases := []struct {
		name    string
		setup   func(t *testing.T, s *Service) string
		present string
		want    model.DenialReason
	}{
		{
			name:    "no credential",
			setup:   func(*testing.T, *Service) string { return "" },
			present: "",
			want:    model.DeniedMissing,
		},
		{
			name:  "a secret no row matches",
			setup: func(*testing.T, *Service) string { return "lm_nothing" },
			want:  model.DeniedUnknown,
		},
		{
			name: "a disabled token",
			setup: func(t *testing.T, s *Service) string {
				m := mustMint(t, s, MintParams{Name: "d"})
				disabled := model.TokenDisabled
				if _, err := s.Patch(context.Background(), m.Token.ID,
					PatchParams{State: &disabled}); err != nil {
					t.Fatal(err)
				}
				return m.Secret
			},
			want: model.DeniedDisabled,
		},
		{
			name: "a revoked token",
			setup: func(t *testing.T, s *Service) string {
				m := mustMint(t, s, MintParams{Name: "r"})
				if err := s.Revoke(context.Background(), m.Token.ID); err != nil {
					t.Fatal(err)
				}
				return m.Secret
			},
			want: model.DeniedRevoked,
		},
		{
			name: "an expired token",
			setup: func(t *testing.T, s *Service) string {
				past := clk.now().Add(-time.Minute).UnixMilli()
				return mustMint(t, s, MintParams{Name: "e", ExpiresAt: &past}).Secret
			},
			want: model.DeniedExpired,
		},
		{
			name: "a token scoped to another instance",
			setup: func(t *testing.T, s *Service) string {
				return mustMint(t, s, MintParams{
					Name: "s", Scope: model.ScopeInstances,
					InstanceIDs: []string{"01JINSTANCE0000000000000099"},
				}).Secret
			},
			want: model.DeniedScope,
		},
		{
			name: "a revoked token that is ALSO expired names the revocation",
			setup: func(t *testing.T, s *Service) string {
				past := clk.now().Add(-time.Minute).UnixMilli()
				m := mustMint(t, s, MintParams{Name: "re", ExpiresAt: &past})
				if err := s.Revoke(context.Background(), m.Token.ID); err != nil {
					t.Fatal(err)
				}
				return m.Secret
			},
			want: model.DeniedRevoked,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newService(t, newFakeStore(), clk)
			secret := tc.setup(t, s)

			_, err := s.Verify(context.Background(), secret, instanceID)
			reason, ok := Denied(err)
			if !ok {
				t.Fatalf("Verify returned %v, want a denial", err)
			}
			if reason != tc.want {
				t.Errorf("reason = %q, want %q", reason, tc.want)
			}
		})
	}
}

// TestGlobalAndScopedTokensPass is the positive half of SPEC §3.4's scoping.
func TestGlobalAndScopedTokensPass(t *testing.T) {
	t.Parallel()

	const here = "01JINSTANCE0000000000000001"
	s := newService(t, newFakeStore(), nil)

	global := mustMint(t, s, MintParams{Name: "global"})
	scoped := mustMint(t, s, MintParams{
		Name: "scoped", Scope: model.ScopeInstances, InstanceIDs: []string{here},
	})

	for _, tc := range []struct {
		name   string
		secret string
		id     string
	}{
		{"a global token reaches this instance", global.Secret, here},
		{"a global token reaches any instance", global.Secret, "01JINSTANCE0000000000000002"},
		{"a scoped token reaches the one it names", scoped.Secret, here},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, err := s.Verify(context.Background(), tc.secret, tc.id)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if v.TokenID == "" || v.Prefix == "" {
				t.Errorf("Verified is incomplete: %+v", v)
			}
		})
	}
}

// TestEpochInvalidatesTheCacheWithoutARestart is section 9.3's epoch counter,
// tested at the level it lives: a cache HIT costs no read, and any write makes
// the very next request re-read.
func TestEpochInvalidatesTheCacheWithoutARestart(t *testing.T) {
	t.Parallel()

	const instanceID = "01JINSTANCE0000000000000001"
	st := newFakeStore()
	s := newService(t, st, nil)
	m := mustMint(t, s, MintParams{Name: "cached"})

	if _, err := s.Verify(context.Background(), m.Secret, instanceID); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	reads := st.hashReads
	for range 20 {
		if _, err := s.Verify(context.Background(), m.Secret, instanceID); err != nil {
			t.Fatalf("Verify: %v", err)
		}
	}
	if st.hashReads != reads {
		t.Errorf("%d database reads for 20 cache hits; the cache is not being used",
			st.hashReads-reads)
	}

	// Any write bumps the epoch, and the next request re-reads. This is what
	// makes revocation take effect within ONE request.
	name := "renamed"
	if _, err := s.Patch(context.Background(), m.Token.ID, PatchParams{Name: &name}); err != nil {
		t.Fatalf("Patch: %v", err)
	}
	if _, err := s.Verify(context.Background(), m.Secret, instanceID); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if st.hashReads != reads+1 {
		t.Errorf("the stale entry was not re-read: %d reads, want %d", st.hashReads, reads+1)
	}

	// Invalidate is the hook for the one scope write that happens elsewhere:
	// deleting an instance removes its `token_instances` rows.
	before := s.Epoch()
	s.Invalidate()
	if s.Epoch() == before {
		t.Error("Invalidate did not bump the epoch")
	}
}

// TestUnknownHashesAreNeverCached: caching negatives would let anyone who can
// reach a public port grow the map without bound by presenting random
// credentials, which is a denial of service written as an optimization.
func TestUnknownHashesAreNeverCached(t *testing.T) {
	t.Parallel()

	st := newFakeStore()
	s := newService(t, st, nil)

	for i := range 5 {
		_, err := s.Verify(context.Background(), fmt.Sprintf("lm_unknown%d", i), "x")
		if reason, ok := Denied(err); !ok || reason != model.DeniedUnknown {
			t.Fatalf("Verify(%d) = %v, want an `unknown` denial", i, err)
		}
	}

	cached := 0
	s.cache.Range(func(any, any) bool { cached++; return true })
	if cached != 0 {
		t.Errorf("%d unknown hashes were cached", cached)
	}
	if st.hashReads != 5 {
		t.Errorf("hash reads = %d, want 5 — an unknown secret must not be served from a cache",
			st.hashReads)
	}
}

// TestRateLimitBucketRefills covers `rate_limit_rpm`: the burst is one minute's
// worth, and it refills continuously rather than resetting on a window boundary
// — which is what stops a client spending two minutes' budget in two
// milliseconds either side of a boundary.
func TestRateLimitBucketRefills(t *testing.T) {
	t.Parallel()

	const instanceID = "01JINSTANCE0000000000000001"
	clk := newClock()
	s := newService(t, newFakeStore(), clk)

	limit := int64(60)
	m := mustMint(t, s, MintParams{Name: "limited", RateLimitRPM: &limit})

	for i := range 60 {
		if _, err := s.Verify(context.Background(), m.Secret, instanceID); err != nil {
			t.Fatalf("call %d was refused: %v", i, err)
		}
	}
	_, err := s.Verify(context.Background(), m.Secret, instanceID)
	if reason, ok := Denied(err); !ok || reason != model.DeniedRateLimited {
		t.Fatalf("the 61st call = %v, want a rate_limited denial", err)
	}

	// One second buys exactly one more request at 60/min.
	clk.advance(time.Second)
	if _, err := s.Verify(context.Background(), m.Secret, instanceID); err != nil {
		t.Errorf("after a second of refill: %v", err)
	}
	if _, err := s.Verify(context.Background(), m.Secret, instanceID); err == nil {
		t.Error("the bucket handed out more than it had refilled")
	}
}

// TestNoRateLimitMeansNoLimit: nil and 0 are both "unlimited", and only the wire
// distinguishes them.
func TestNoRateLimitMeansNoLimit(t *testing.T) {
	t.Parallel()

	clk := newClock()
	s := newService(t, newFakeStore(), clk)
	zero := int64(0)

	for _, limit := range []*int64{nil, &zero} {
		m := mustMint(t, s, MintParams{Name: "unlimited", RateLimitRPM: limit})
		for i := range 500 {
			if _, err := s.Verify(context.Background(), m.Secret, "x"); err != nil {
				t.Fatalf("call %d refused with limit %v: %v", i, limit, err)
			}
		}
	}
}

// TestRevokeIsTerminal is section 2.9's transition table: active ⇄ disabled, and
// active|disabled → revoked, which is terminal. The hash is RETAINED so a leaked
// secret can never be re-minted into validity.
func TestRevokeIsTerminal(t *testing.T) {
	t.Parallel()

	st := newFakeStore()
	s := newService(t, st, nil)
	m := mustMint(t, s, MintParams{Name: "leaked"})

	if err := s.Revoke(context.Background(), m.Token.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	// A second revoke is a no-op, not an error: the caller asked for it to be
	// dead and it is.
	if err := s.Revoke(context.Background(), m.Token.ID); err != nil {
		t.Errorf("revoking twice: %v", err)
	}

	active := model.TokenActive
	_, err := s.Patch(context.Background(), m.Token.ID, PatchParams{State: &active})
	var me model.Error
	if !errors.As(err, &me) || me.Code != CodeTokenRevoked {
		t.Fatalf("re-enabling a revoked token = %v, want %s", err, CodeTokenRevoked)
	}

	row, err := st.APIToken(context.Background(), nil, m.Token.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.TokenHash == "" {
		t.Error("the hash was cleared on revoke; a leaked secret could then be re-minted")
	}
	if row.RevokedAt == nil {
		t.Error("revoked_at was not stamped")
	}

	if err := s.Revoke(context.Background(), "01JNOSUCHTOKEN00000000000000"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("revoking a token that does not exist = %v, want ErrNotFound", err)
	}
}

// TestMintRefusals is the 422 table of section 3.12.
func TestMintRefusals(t *testing.T) {
	t.Parallel()

	negative := int64(-1)
	cases := []struct {
		name   string
		params MintParams
		want   model.ErrorCode
	}{
		{"no name", MintParams{}, CodeTokenNameRequired},
		{"an unknown scope", MintParams{Name: "x", Scope: "everything"}, CodeTokenScopeInvalid},
		{
			"an instances scope naming nothing",
			MintParams{Name: "x", Scope: model.ScopeInstances},
			CodeTokenScopeInvalid,
		},
		{"a negative rate limit", MintParams{Name: "x", RateLimitRPM: &negative},
			CodeTokenRateLimitInvalid},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newService(t, newFakeStore(), nil)
			_, err := s.Mint(context.Background(), tc.params)
			var me model.Error
			if !errors.As(err, &me) {
				t.Fatalf("Mint = %v, want a model.Error", err)
			}
			if me.Code != tc.want {
				t.Errorf("code = %q, want %q", me.Code, tc.want)
			}
		})
	}
}

// TestPatchRescopesThroughInstanceIDs: a non-empty list makes the token
// instance-scoped and an explicitly empty one makes it global, which is what the
// section 3.12 body shape can express without a `scope` field that could
// contradict it.
func TestPatchRescopesThroughInstanceIDs(t *testing.T) {
	t.Parallel()

	const a, b = "01JINSTANCE000000000000000A", "01JINSTANCE000000000000000B"
	s := newService(t, newFakeStore(), nil)
	m := mustMint(t, s, MintParams{Name: "rescope"})

	ids := []string{a, b}
	tok, err := s.Patch(context.Background(), m.Token.ID, PatchParams{InstanceIDs: &ids})
	if err != nil {
		t.Fatalf("Patch: %v", err)
	}
	if tok.Scope != model.ScopeInstances {
		t.Errorf("scope = %q, want instances", tok.Scope)
	}
	if _, err := s.Verify(context.Background(), m.Secret, a); err != nil {
		t.Errorf("the token no longer reaches %s: %v", a, err)
	}
	if _, err := s.Verify(context.Background(), m.Secret, "01JINSTANCE000000000000000C"); err == nil {
		t.Error("the token still reaches an instance it does not name")
	}

	empty := []string{}
	tok, err = s.Patch(context.Background(), m.Token.ID, PatchParams{InstanceIDs: &empty})
	if err != nil {
		t.Fatalf("Patch: %v", err)
	}
	if tok.Scope != model.ScopeGlobal {
		t.Errorf("scope = %q, want global", tok.Scope)
	}
	if _, err := s.Verify(context.Background(), m.Secret, "01JINSTANCE000000000000000C"); err != nil {
		t.Errorf("a global token was refused: %v", err)
	}
}

// TestUseStampsAreBatched is section 9.3's "`api_tokens.last_used_at` /
// `request_count` update at most once per 10 s per token": a busy token costs
// one UPDATE every ten seconds rather than one per request, through a write pool
// that is a single connection.
func TestUseStampsAreBatched(t *testing.T) {
	t.Parallel()

	clk := newClock()
	st := newFakeStore()
	s := newService(t, st, clk)
	m := mustMint(t, s, MintParams{Name: "busy"})

	for range 100 {
		s.RecordUse(m.Token.ID, clk.now(), "10.0.0.5")
	}
	if err := s.Flush(context.Background(), false); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if st.touchCount() != 1 {
		t.Fatalf("touches = %d, want 1 for 100 requests", st.touchCount())
	}
	row, _ := st.APIToken(context.Background(), nil, m.Token.ID)
	if row.RequestCount != 100 {
		t.Errorf("request_count = %d, want 100 — the batching must not lose counts",
			row.RequestCount)
	}
	if row.LastUsedIP == nil || *row.LastUsedIP != "10.0.0.5" {
		t.Errorf("last_used_ip = %v, want 10.0.0.5", row.LastUsedIP)
	}

	// Inside the ten seconds, nothing more is written.
	s.RecordUse(m.Token.ID, clk.now(), "10.0.0.5")
	if err := s.Flush(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if st.touchCount() != 1 {
		t.Errorf("touches = %d; a second flush inside the floor must write nothing",
			st.touchCount())
	}

	// A shutdown forces it: the counters are in memory and there is no later
	// chance to write them.
	if err := s.Flush(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if st.touchCount() != 2 {
		t.Errorf("touches = %d after a forced flush, want 2", st.touchCount())
	}

	// And past the floor, an ordinary flush writes again.
	clk.advance(2 * TouchInterval)
	s.RecordUse(m.Token.ID, clk.now(), "10.0.0.5")
	if err := s.Flush(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if st.touchCount() != 3 {
		t.Errorf("touches = %d past the floor, want 3", st.touchCount())
	}
}

// TestFailedFlushKeepsItsCounts: a flush that failed has not been recorded
// anywhere, and dropping it would make `request_count` quietly under-report for
// the life of the token.
func TestFailedFlushKeepsItsCounts(t *testing.T) {
	t.Parallel()

	clk := newClock()
	st := newFakeStore()
	s := newService(t, st, clk)
	m := mustMint(t, s, MintParams{Name: "unlucky"})

	for range 7 {
		s.RecordUse(m.Token.ID, clk.now(), "")
	}
	st.mu.Lock()
	st.writeErr = errors.New("database is locked")
	st.mu.Unlock()

	if err := s.Flush(context.Background(), true); err == nil {
		t.Fatal("Flush reported success against a failing store")
	}

	st.mu.Lock()
	st.writeErr = nil
	st.mu.Unlock()
	if err := s.Flush(context.Background(), true); err != nil {
		t.Fatalf("the retry failed: %v", err)
	}
	row, _ := st.APIToken(context.Background(), nil, m.Token.ID)
	if row.RequestCount != 7 {
		t.Errorf("request_count = %d, want 7 — the failed flush lost its counts",
			row.RequestCount)
	}
}

// TestConcurrentVerifyIsSafe exercises the sync.Map and the bucket under the
// race detector, which is the only way to prove a lock-free hot path.
func TestConcurrentVerifyIsSafe(t *testing.T) {
	t.Parallel()

	s := newService(t, newFakeStore(), nil)
	m := mustMint(t, s, MintParams{Name: "hot"})

	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for range 50 {
				if _, err := s.Verify(context.Background(), m.Secret, "x"); err != nil {
					t.Errorf("goroutine %d: %v", i, err)
					return
				}
				s.RecordUse(m.Token.ID, time.Now(), "127.0.0.1")
			}
		}(i)
	}
	// Writers racing readers is the case the epoch exists for.
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name := "hot"
			if _, err := s.Patch(context.Background(), m.Token.ID, PatchParams{Name: &name}); err != nil {
				t.Errorf("Patch: %v", err)
			}
		}()
	}
	wg.Wait()
}

func mustMint(t *testing.T, s *Service, p MintParams) Minted {
	t.Helper()
	m, err := s.Mint(context.Background(), p)
	if err != nil {
		t.Fatalf("Mint(%q): %v", p.Name, err)
	}
	return m
}
