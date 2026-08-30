package tokens

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// Store is the persistence this package needs. *store.Store satisfies it
// structurally — DESIGN section 1, invariant 1: only internal/store contains
// SQL, so every other package declares the repository interface it needs.
type Store interface {
	Read(ctx context.Context, fn func(context.Context, store.Tx) error) error
	Write(ctx context.Context, fn func(context.Context, store.Tx) error) error

	InsertAPIToken(ctx context.Context, tx store.Tx, t store.APIToken) error
	APIToken(ctx context.Context, tx store.Tx, id string) (store.APIToken, error)
	APITokenByHash(ctx context.Context, tx store.Tx, hash string) (store.APIToken, error)
	APITokens(ctx context.Context, tx store.Tx) ([]store.APIToken, error)
	UpdateAPIToken(ctx context.Context, tx store.Tx, t store.APIToken) (bool, error)
	RevokeAPIToken(ctx context.Context, tx store.Tx, id string, at int64) (bool, error)
	TouchAPIToken(ctx context.Context, tx store.Tx, id string, at int64, ip *string, delta int64) (bool, error)

	TokenInstances(ctx context.Context, tx store.Tx, tokenID string) ([]string, error)
	AllTokenInstances(ctx context.Context, tx store.Tx) (map[string][]string, error)
	SetTokenInstances(ctx context.Context, tx store.Tx, tokenID string, instanceIDs []string) error

	TokenUsage(ctx context.Context, tx store.Tx, tokenID string, rng store.UsageRange) ([]store.TokenUsageRow, error)
}

// Events is the events/SSE seam. Append belongs inside the caller's write
// transaction; Publish runs only after it commits.
type Events interface {
	Append(ctx context.Context, tx store.Tx, ev model.Event) error
	Publish(ev model.Event)
}

// Config wires a Service.
type Config struct {
	Store Store
	// Events is optional. A daemon without it mints and revokes tokens exactly
	// the same way and simply logs nothing to the event stream.
	Events Events
	// Now supplies every instant this service stamps. Nil uses time.Now.
	Now func() time.Time
	// NewID mints row ids. Nil uses store.NewID.
	NewID func(time.Time) string
	// Rand is the entropy behind a minted secret. Nil uses crypto/rand, which
	// is the only correct answer outside a test.
	Rand io.Reader
}

// Service is the token store of DESIGN section 9.3: mint, verify, and the four
// state moves of section 3.12.
//
// # Why the cache exists and what makes it safe
//
// The gateway verifies a credential on EVERY proxied request, and a request is
// the hot path of the thing this daemon exists to serve. A database read per
// request through a write-serialized SQLite would put the daemon back in the
// data path that D3 and section 9.1 keep it out of. So verified tokens are
// cached — and the cache is guarded by an EPOCH COUNTER rather than by a TTL:
//
//   - Every write in this file bumps the epoch.
//   - A cached entry carries the epoch it was read at.
//   - A request whose entry is stale re-reads the row.
//
// That is what makes "revocation takes effect within one request, with no reload
// of anything" true, which is the whole reason Llama Man owns the public port
// (SPEC §1). A TTL would make the same sentence "within one request, some
// seconds from now".
//
// A hash that matches NO row is never cached. Caching negatives would let anyone
// who can reach a public port grow this map without bound by presenting random
// credentials, which is a denial of service written as an optimization.
type Service struct {
	store  Store
	events Events
	now    func() time.Time
	newID  func(time.Time) string
	rand   io.Reader

	// epoch is bumped by every token or scope write (§2.9). It is the ONE thing
	// that invalidates the cache.
	epoch atomic.Uint64
	// cache maps token_hash → *entry.
	cache sync.Map
	// limits holds the per-token buckets of `rate_limit_rpm`.
	limits *buckets

	// uses accumulates `last_used_at`/`request_count` so a busy token costs one
	// UPDATE per ten seconds rather than one per request (§9.3).
	usesMu sync.Mutex
	uses   map[string]*use
}

// entry is one verified token as the cache holds it. It carries the hash so the
// comparison can be constant-time even on a cache hit, and the instance set as a
// map so the scope check is O(1) rather than a scan of an admin-controlled slice.
type entry struct {
	epoch     uint64
	id        string
	prefix    string
	hash      string
	state     model.TokenState
	scope     model.TokenScope
	instances map[string]struct{}
	expiresAt *int64
	rateLimit *int64
}

// use is one token's pending touch.
type use struct {
	count     int64
	lastAt    int64
	lastIP    *string
	lastWrite int64
}

// TouchInterval is section 9.3's "at most once per 10 s per token".
const TouchInterval = 10 * time.Second

// New builds a Service.
func New(cfg Config) (*Service, error) {
	if cfg.Store == nil {
		return nil, errors.New("tokens: a service needs a store")
	}
	s := &Service{
		store:  cfg.Store,
		events: cfg.Events,
		now:    cfg.Now,
		newID:  cfg.NewID,
		rand:   cfg.Rand,
		limits: newBuckets(),
		uses:   map[string]*use{},
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.newID == nil {
		s.newID = store.NewID
	}
	// The epoch starts at 1 so that a zero-valued entry — which can only come
	// from a bug — is always stale rather than accidentally current.
	s.epoch.Store(1)
	return s, nil
}

// Epoch reports the current cache epoch. It exists for tests and for a caller
// that wants to prove an edit landed.
func (s *Service) Epoch() uint64 { return s.epoch.Load() }

// Invalidate bumps the epoch, which makes every cached decision stale within one
// request.
//
// It is exported for the one writer of `token_instances` that is not in this
// file: deleting an instance removes its scope rows (§3.10c step 3), and that is
// a SCOPE WRITE in section 2.9's sense even though the instance service is the
// one making it. Calling this after such a write is cheap and unconditional;
// forgetting it would leave a cached token believing in an instance that is gone.
func (s *Service) Invalidate() { s.epoch.Add(1) }

// Token is one row as the API returns it: never the secret, and the scope rows
// resolved alongside so a caller does not make a second query to learn what a
// token reaches.
type Token struct {
	ID     string
	Name   string
	Prefix string
	Scope  model.TokenScope
	State  model.TokenState
	// InstanceIDs is meaningful only for `scope='instances'`; a global token
	// reaches everything and carries none.
	InstanceIDs  []string
	RateLimitRPM *int64
	ExpiresAt    *int64

	CreatedAt int64
	UpdatedAt int64
	RevokedAt *int64

	LastUsedAt   *int64
	LastUsedIP   *string
	RequestCount int64
}

// Minted is the answer to `POST /api/v1/tokens`: the row, plus the ONE response
// in this whole API that carries a secret (§3.12).
type Minted struct {
	Token Token
	// Secret is shown once and never stored in a form anyone can reverse. The
	// caller must not log it, and nothing in this package does.
	Secret string
}

// MintParams is the body of `POST /api/v1/tokens`.
type MintParams struct {
	Name  string
	Scope model.TokenScope
	// InstanceIDs is required for `scope='instances'` and ignored otherwise.
	InstanceIDs  []string
	RateLimitRPM *int64
	ExpiresAt    *int64
}

// Mint creates a token and returns its secret exactly once (§9.3, §3.12).
func (s *Service) Mint(ctx context.Context, p MintParams) (Minted, error) {
	now := s.now()
	nowMS := now.UnixMilli()

	if p.Name == "" {
		return Minted{}, model.Error{
			Code:    CodeTokenNameRequired,
			Message: "a token needs a name; it is the only thing that will identify it later",
		}
	}
	scope := p.Scope
	if scope == "" {
		scope = model.ScopeGlobal
	}
	if !scope.Valid() {
		return Minted{}, model.Error{
			Code:    CodeTokenScopeInvalid,
			Message: fmt.Sprintf("scope %q is not one of global, instances", scope),
		}
	}
	if scope == model.ScopeInstances && len(p.InstanceIDs) == 0 {
		return Minted{}, model.Error{
			Code: CodeTokenScopeInvalid,
			Message: "a token scoped to instances must name at least one; a scope that " +
				"reaches nothing is a token that cannot be used",
		}
	}
	if err := validRateLimit(p.RateLimitRPM); err != nil {
		return Minted{}, err
	}

	secret, err := NewSecret(s.rand)
	if err != nil {
		return Minted{}, err
	}

	row := store.APIToken{
		ID:           s.newID(now),
		Name:         p.Name,
		Prefix:       DisplayPrefix(secret),
		TokenHash:    Hash(secret),
		Scope:        scope,
		State:        model.TokenActive,
		RateLimitRPM: p.RateLimitRPM,
		ExpiresAt:    p.ExpiresAt,
		CreatedAt:    nowMS,
		UpdatedAt:    nowMS,
	}

	var out Token
	err = s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		if err := s.store.InsertAPIToken(ctx, tx, row); err != nil {
			return err
		}
		if scope == model.ScopeInstances {
			if err := s.store.SetTokenInstances(ctx, tx, row.ID, p.InstanceIDs); err != nil {
				return err
			}
		}
		out = tokenOf(row, p.InstanceIDs)
		return s.event(ctx, tx, now, row, "token_created", model.LevelInfo,
			fmt.Sprintf("API token %s (%s) created", row.Name, row.Prefix))
	})
	if err != nil {
		return Minted{}, err
	}

	s.Invalidate()
	s.publish(now, row, "token_created")
	return Minted{Token: out, Secret: secret}, nil
}

// List is `GET /api/v1/tokens`. It never returns secrets — there is nothing
// stored that could be one.
func (s *Service) List(ctx context.Context) ([]Token, error) {
	var out []Token
	err := s.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		rows, err := s.store.APITokens(ctx, tx)
		if err != nil {
			return err
		}
		scopes, err := s.store.AllTokenInstances(ctx, tx)
		if err != nil {
			return err
		}
		out = make([]Token, 0, len(rows))
		for _, row := range rows {
			out = append(out, tokenOf(row, scopes[row.ID]))
		}
		return nil
	})
	return out, err
}

// Get is `GET /api/v1/tokens/{id}`.
func (s *Service) Get(ctx context.Context, id string) (Token, error) {
	var out Token
	err := s.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		row, err := s.store.APIToken(ctx, tx, id)
		if err != nil {
			return err
		}
		ids, err := s.store.TokenInstances(ctx, tx, id)
		if err != nil {
			return err
		}
		out = tokenOf(row, ids)
		return nil
	})
	return out, err
}

// Usage is `GET /api/v1/tokens/{id}/usage`: the per-token breakdown D56 writes
// beside the instance-first counters.
func (s *Service) Usage(ctx context.Context, id string, rng store.UsageRange) ([]store.TokenUsageRow, error) {
	var out []store.TokenUsageRow
	err := s.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		// The token has to exist, so an unknown id is a 404 rather than an empty
		// series that reads as "this token was never used".
		if _, err := s.store.APIToken(ctx, tx, id); err != nil {
			return err
		}
		var err error
		out, err = s.store.TokenUsage(ctx, tx, id, rng)
		return err
	})
	return out, err
}

// PatchParams is the body of `PATCH /api/v1/tokens/{id}`. A nil field is left
// alone; a non-nil one is written.
type PatchParams struct {
	Name  *string
	State *model.TokenState
	// Scope is changed implicitly by InstanceIDs: a non-empty list means
	// `instances`, an explicitly empty one means `global`. That is what the
	// section 3.12 body shape can express, and inventing a separate `scope`
	// field would let a client send a pair that contradicts itself.
	InstanceIDs *[]string
	// RateLimitRPM is a two-level pointer so the wire can distinguish "leave it"
	// (nil) from "remove the limit" (a pointer to nil).
	RateLimitRPM **int64
	ExpiresAt    **int64
}

// Patch is `PATCH /api/v1/tokens/{id}`.
//
// `revoked` is terminal, so a patch of a revoked token is refused rather than
// silently ignored: an admin who thinks they just re-enabled a leaked credential
// must be told they did not.
func (s *Service) Patch(ctx context.Context, id string, p PatchParams) (Token, error) {
	now := s.now()
	nowMS := now.UnixMilli()

	if p.State != nil {
		switch *p.State {
		case model.TokenActive, model.TokenDisabled, model.TokenRevoked:
		default:
			return Token{}, model.Error{
				Code:    CodeTokenStateInvalid,
				Message: fmt.Sprintf("state %q is not one of active, disabled, revoked", *p.State),
			}
		}
	}
	if p.RateLimitRPM != nil {
		if err := validRateLimit(*p.RateLimitRPM); err != nil {
			return Token{}, err
		}
	}

	var (
		out  Token
		from model.TokenState
	)
	err := s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		row, err := s.store.APIToken(ctx, tx, id)
		if err != nil {
			return err
		}
		from = row.State
		if row.Revoked() {
			return model.Error{
				Code: CodeTokenRevoked,
				Message: "this token is revoked, which is terminal — mint a new one rather " +
					"than restoring a secret that has already leaked",
				Details: map[string]any{"token_id": id},
			}
		}

		ids, err := s.store.TokenInstances(ctx, tx, id)
		if err != nil {
			return err
		}

		if p.Name != nil {
			if *p.Name == "" {
				return model.Error{
					Code:    CodeTokenNameRequired,
					Message: "a token needs a name",
				}
			}
			row.Name = *p.Name
		}
		if p.RateLimitRPM != nil {
			row.RateLimitRPM = *p.RateLimitRPM
		}
		if p.ExpiresAt != nil {
			row.ExpiresAt = *p.ExpiresAt
		}
		if p.InstanceIDs != nil {
			ids = *p.InstanceIDs
			if len(ids) == 0 {
				row.Scope = model.ScopeGlobal
			} else {
				row.Scope = model.ScopeInstances
			}
			if err := s.store.SetTokenInstances(ctx, tx, id, ids); err != nil {
				return err
			}
		}
		if p.State != nil {
			row.State = *p.State
			if row.State == model.TokenRevoked {
				row.RevokedAt = &nowMS
			}
		}
		row.UpdatedAt = nowMS

		changed, err := s.store.UpdateAPIToken(ctx, tx, row)
		if err != nil {
			return err
		}
		if !changed {
			// The row was revoked between the read and the write. Re-reading
			// would only tell us what we already know, and the honest answer is
			// the same refusal the guard above writes.
			return model.Error{
				Code:    CodeTokenRevoked,
				Message: "this token was revoked while the edit was in flight",
				Details: map[string]any{"token_id": id},
			}
		}
		out = tokenOf(row, ids)
		return s.event(ctx, tx, now, row, "token_updated", model.LevelInfo,
			fmt.Sprintf("API token %s (%s) updated", row.Name, row.Prefix))
	})
	if err != nil {
		return Token{}, err
	}

	// The bucket goes with the policy: an admin who just raised a limit should
	// not have to wait out the old one.
	s.limits.forget(id)
	s.Invalidate()
	if p.State != nil && *p.State != from {
		s.publish(now, store.APIToken{ID: out.ID, Name: out.Name, Prefix: out.Prefix},
			"token_"+string(*p.State))
	} else {
		s.publish(now, store.APIToken{ID: out.ID, Name: out.Name, Prefix: out.Prefix}, "token_updated")
	}
	return out, nil
}

// Revoke is `DELETE /api/v1/tokens/{id}`: a soft, terminal state change (§3.12).
// The hash is retained so a leaked secret can never be re-minted into validity.
//
// Revoking an already-revoked token is a no-op rather than an error — the caller
// asked for it to be dead and it is — while an id that names no row is
// store.ErrNotFound, which the API layer renders as 404.
func (s *Service) Revoke(ctx context.Context, id string) error {
	now := s.now()
	nowMS := now.UnixMilli()

	var row store.APIToken
	err := s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		if row, err = s.store.APIToken(ctx, tx, id); err != nil {
			return err
		}
		revoked, err := s.store.RevokeAPIToken(ctx, tx, id, nowMS)
		if err != nil {
			return err
		}
		if !revoked {
			return nil
		}
		return s.event(ctx, tx, now, row, "token_revoked", model.LevelWarn,
			fmt.Sprintf("API token %s (%s) revoked", row.Name, row.Prefix))
	})
	if err != nil {
		return err
	}

	s.limits.forget(id)
	s.Invalidate()
	s.publish(now, row, "token_revoked")
	return nil
}

// Verified is what a successful verification yields. It is deliberately small:
// the gateway needs an id to account against and a prefix to log, and nothing
// else about a credential belongs anywhere near a request path.
type Verified struct {
	TokenID string
	Prefix  string
	Scope   model.TokenScope
}

// DeniedError is a refusal carrying the reason `gateway_denials_daily` counts
// (§2.9). It is an error rather than a bool because the reason IS the product
// feature: "unauthorized attempts on port 8081" is a dashboard line, and
// "expired" and "revoked" are different stories about the same client.
type DeniedError struct {
	Reason model.DenialReason
}

func (e DeniedError) Error() string { return "tokens: denied: " + string(e.Reason) }

// Denied unwraps err into its denial reason, and reports whether it was one.
func Denied(err error) (model.DenialReason, bool) {
	var d DeniedError
	if errors.As(err, &d) {
		return d.Reason, true
	}
	return "", false
}

// Verify checks one presented secret against one instance's listener (§9.3).
//
// The order of the checks is the order of the reasons in section 2.9's comment,
// and it is not arbitrary: `unknown` before `disabled` before `expired` before
// `scope` before `rate_limited`, so a denial always names the FIRST thing wrong
// rather than whichever check happened to run first.
func (s *Service) Verify(ctx context.Context, secret, instanceID string) (Verified, error) {
	if secret == "" {
		return Verified{}, DeniedError{Reason: model.DeniedMissing}
	}

	hash := Hash(secret)
	e, err := s.entryFor(ctx, hash)
	if err != nil {
		return Verified{}, err
	}

	// Constant-time even on a cache hit. The lookup was by hash and the hash is
	// a sha256 of the presented bytes, so this comparison can only fail on a
	// bug — but a comparison that is sometimes constant-time is a comparison
	// nobody can reason about, and this one costs 32 bytes of XOR.
	if subtle.ConstantTimeCompare([]byte(e.hash), []byte(hash)) != 1 {
		return Verified{}, DeniedError{Reason: model.DeniedUnknown}
	}

	switch e.state {
	case model.TokenRevoked:
		return Verified{}, DeniedError{Reason: model.DeniedRevoked}
	case model.TokenDisabled:
		return Verified{}, DeniedError{Reason: model.DeniedDisabled}
	}

	now := s.now()
	if e.expiresAt != nil && now.UnixMilli() >= *e.expiresAt {
		return Verified{}, DeniedError{Reason: model.DeniedExpired}
	}

	if e.scope == model.ScopeInstances {
		if _, ok := e.instances[instanceID]; !ok {
			return Verified{}, DeniedError{Reason: model.DeniedScope}
		}
	}

	if e.rateLimit != nil && !s.limits.allow(e.id, *e.rateLimit, now) {
		return Verified{}, DeniedError{Reason: model.DeniedRateLimited}
	}

	return Verified{TokenID: e.id, Prefix: e.prefix, Scope: e.scope}, nil
}

// entryFor returns the cached entry for a hash, re-reading the row when the
// cached epoch is stale or nothing is cached.
func (s *Service) entryFor(ctx context.Context, hash string) (*entry, error) {
	epoch := s.epoch.Load()
	if v, ok := s.cache.Load(hash); ok {
		if e := v.(*entry); e.epoch == epoch {
			return e, nil
		}
	}

	var (
		row store.APIToken
		ids []string
	)
	err := s.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		if row, err = s.store.APITokenByHash(ctx, tx, hash); err != nil {
			return err
		}
		if row.Scope != model.ScopeInstances {
			return nil
		}
		ids, err = s.store.TokenInstances(ctx, tx, row.ID)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		// Not cached, deliberately: see the Service doc comment.
		s.cache.Delete(hash)
		return nil, DeniedError{Reason: model.DeniedUnknown}
	}
	if err != nil {
		return nil, err
	}

	e := &entry{
		epoch:     epoch,
		id:        row.ID,
		prefix:    row.Prefix,
		hash:      row.TokenHash,
		state:     row.State,
		scope:     row.Scope,
		expiresAt: row.ExpiresAt,
		rateLimit: row.RateLimitRPM,
	}
	if len(ids) > 0 {
		e.instances = make(map[string]struct{}, len(ids))
		for _, id := range ids {
			e.instances[id] = struct{}{}
		}
	}
	s.cache.Store(hash, e)
	return e, nil
}

// RecordUse notes that a token served a request. It accumulates in memory; Flush
// writes at most once per TouchInterval per token (§9.3).
//
// ip may be empty, which is what a request with no parseable remote address
// gives — a unix socket in a test, say. An empty address stores NULL rather than
// an empty string, because "we do not know" is a fact and "" is not an address.
func (s *Service) RecordUse(id string, at time.Time, ip string) {
	if id == "" {
		return
	}
	s.usesMu.Lock()
	defer s.usesMu.Unlock()

	u, ok := s.uses[id]
	if !ok {
		u = &use{}
		s.uses[id] = u
	}
	u.count++
	u.lastAt = at.UnixMilli()
	if ip != "" {
		u.lastIP = &ip
	}
}

// Flush writes the pending `last_used_at`/`request_count` updates.
//
// force ignores the ten-second floor and is what shutdown passes: the counters
// are in memory, and a process that is about to end has no later chance to write
// them. Everything else passes false, which is the rate limit section 9.3 asks
// for on a column no user is watching to the second.
func (s *Service) Flush(ctx context.Context, force bool) error {
	nowMS := s.now().UnixMilli()

	type pending struct {
		id string
		u  use
	}
	var due []pending

	s.usesMu.Lock()
	for id, u := range s.uses {
		if u.count == 0 {
			continue
		}
		if !force && nowMS-u.lastWrite < TouchInterval.Milliseconds() {
			continue
		}
		due = append(due, pending{id: id, u: *u})
		u.count = 0
		u.lastWrite = nowMS
	}
	s.usesMu.Unlock()

	if len(due) == 0 {
		return nil
	}

	err := s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		for _, p := range due {
			if _, err := s.store.TouchAPIToken(ctx, tx, p.id, p.u.lastAt, p.u.lastIP, p.u.count); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		// Put the counts back. A flush that failed has not been recorded
		// anywhere, and dropping it would make `request_count` quietly
		// under-report for the life of the token.
		s.usesMu.Lock()
		for _, p := range due {
			if u, ok := s.uses[p.id]; ok {
				u.count += p.u.count
				u.lastWrite = p.u.lastWrite - TouchInterval.Milliseconds()
			}
		}
		s.usesMu.Unlock()
		return err
	}
	return nil
}

// validRateLimit refuses a negative limit. Zero and nil both mean "no limit",
// and the two are kept distinct on the wire only because a client that sends 0
// means it.
func validRateLimit(v *int64) error {
	if v == nil || *v >= 0 {
		return nil
	}
	return model.Error{
		Code:    CodeTokenRateLimitInvalid,
		Message: "rate_limit_rpm cannot be negative; omit it or send 0 for no limit",
	}
}

func tokenOf(row store.APIToken, instanceIDs []string) Token {
	if instanceIDs == nil {
		instanceIDs = []string{}
	}
	return Token{
		ID:           row.ID,
		Name:         row.Name,
		Prefix:       row.Prefix,
		Scope:        row.Scope,
		State:        row.State,
		InstanceIDs:  instanceIDs,
		RateLimitRPM: row.RateLimitRPM,
		ExpiresAt:    row.ExpiresAt,
		CreatedAt:    row.CreatedAt,
		UpdatedAt:    row.UpdatedAt,
		RevokedAt:    row.RevokedAt,
		LastUsedAt:   row.LastUsedAt,
		LastUsedIP:   row.LastUsedIP,
		RequestCount: row.RequestCount,
	}
}

// event appends one `events` row inside the caller's transaction. The message
// carries the token's NAME and PREFIX and never anything derived from the
// secret: §2.2's rule and CLAUDE.md's are the same rule, and an event message is
// the one part of this system that is designed to be read later.
func (s *Service) event(ctx context.Context, tx store.Tx, now time.Time, row store.APIToken,
	action string, level model.EventLevel, message string) error {
	if s.events == nil {
		return nil
	}
	return s.events.Append(ctx, tx, s.newEvent(now, row, action, level, message))
}

func (s *Service) publish(now time.Time, row store.APIToken, action string) {
	if s.events == nil {
		return
	}
	s.events.Publish(s.newEvent(now, row, action, model.LevelInfo, ""))
}

func (s *Service) newEvent(now time.Time, row store.APIToken, action string,
	level model.EventLevel, message string) model.Event {
	subjectType, subjectID := "token", row.ID
	return model.Event{
		ID:          s.newID(now),
		At:          now.UnixMilli(),
		Level:       level,
		Category:    model.CategoryToken,
		SubjectType: &subjectType,
		SubjectID:   &subjectID,
		Action:      action,
		Actor:       model.ActorAdmin,
		Message:     message,
	}
}
