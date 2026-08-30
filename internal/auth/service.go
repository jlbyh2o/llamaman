package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// Service is the admin authentication authority: the password, the sessions, the
// lockout, the CSRF derivation and the one-time setup token (DESIGN §2.2, §2.2a,
// §3.1, §11.1 step 8).
//
// It speaks no HTTP. Every method takes and returns domain values — a cookie
// STRING, an IP string, a model.SessionCredential — so internal/api can adapt it
// to the middleware.Authenticator interface without this package importing
// anything under internal/api (§1, invariant 4).

// Repo is the persistence this package needs (§1, invariant 1: SQL lives only in
// internal/store, so every consumer declares the repository interface it uses and
// *store.Store satisfies it structurally).
type Repo interface {
	AdminAccount(ctx context.Context, tx store.Tx) (model.AdminAccount, error)
	AdminAccountExists(ctx context.Context, tx store.Tx) (bool, error)
	CreateAdminAccount(ctx context.Context, tx store.Tx, a model.AdminAccount) (bool, error)
	SetAdminPassword(ctx context.Context, tx store.Tx, hash string, at int64) (bool, error)

	InsertSession(ctx context.Context, tx store.Tx, v model.Session) error
	Session(ctx context.Context, tx store.Tx, id string) (model.Session, error)
	ActiveSessions(ctx context.Context, tx store.Tx, now int64) ([]model.Session, error)
	TouchSession(ctx context.Context, tx store.Tx, id string, at int64, ip, userAgent *string) error
	RevokeSession(ctx context.Context, tx store.Tx, id string, at int64) (bool, error)
	RevokeSessionsExcept(ctx context.Context, tx store.Tx, keepID string, at int64) (int64, error)

	InsertLoginAttempt(ctx context.Context, tx store.Tx, a model.LoginAttempt) error
	CountFailedLoginAttempts(ctx context.Context, tx store.Tx, ip string, since int64,
		reasons []model.LoginReason) (int, error)
	LastSuccessfulLoginAt(ctx context.Context, tx store.Tx, ip string) (int64, bool, error)
	HasLoginAttemptSince(ctx context.Context, tx store.Tx, ip string,
		reason model.LoginReason, since int64) (bool, error)

	Lockout(ctx context.Context, tx store.Tx, ip string) (model.Lockout, error)
	PutLockout(ctx context.Context, tx store.Tx, v model.Lockout) error
	DeleteLockout(ctx context.Context, tx store.Tx, ip string) error

	SetupClaim(ctx context.Context, tx store.Tx) (model.SetupClaim, error)
	PutSetupClaim(ctx context.Context, tx store.Tx, tokenHash, tokenPath string, createdAt int64) error
	ClaimSetup(ctx context.Context, tx store.Tx, at int64, fromIP string) (bool, error)
	ClearSetupTokenPath(ctx context.Context, tx store.Tx) error

	AppendEvent(ctx context.Context, tx store.Tx, e model.Event) error

	Read(ctx context.Context, fn func(context.Context, store.Tx) error) error
	Write(ctx context.Context, fn func(context.Context, store.Tx) error) error
}

// Settings is the read-through settings cache as this package needs it. The five
// keys it reads are `security.session_ttl_hours`, `security.idle_timeout_hours`,
// `security.login_max_attempts`, `security.login_window_sec` and
// `security.lockout_sec` — all of them in internal/settings' registry, all of
// them editable in the UI (SPEC §3.9).
type Settings interface {
	GetInt(ctx context.Context, key string) (int64, error)
}

// Config constructs a Service.
type Config struct {
	// Repo is required.
	Repo Repo
	// Settings supplies the five `security.*` keys. Nil uses the registry
	// defaults, which is what a test wants and what a daemon whose settings
	// failed to load would fall back to anyway.
	Settings Settings
	// StateDir is the resolved state directory (D72). The setup-token file
	// lives directly inside it.
	StateDir string
	// Params are the argon2id cost parameters. The zero value takes
	// DefaultParams; a test lowers them.
	Params Params
	// Now supplies every instant. Nil uses time.Now.
	Now func() time.Time
	// Logger is used for the SETUP: announcement of §2.2a step 2 and for
	// anything that fails without failing the request. Nil uses slog.Default.
	Logger *slog.Logger
	// TouchInterval bounds how often an authenticated request rewrites
	// `sessions.last_seen_at`. Zero uses DefaultTouchInterval.
	TouchInterval time.Duration
}

// DefaultTouchInterval keeps the sliding half of the session window from putting
// a database write on every authenticated request: the row is refreshed at most
// this often, which is precise enough for an idle timeout measured in days and
// for a "last seen" column a human reads.
const DefaultTouchInterval = time.Minute

// Service implements DESIGN §3.1 and the authentication half of §3.2.
type Service struct {
	repo     Repo
	settings Settings
	stateDir string
	params   Params
	now      func() time.Time
	log      *slog.Logger
	touch    time.Duration
}

// New builds a Service.
func New(cfg Config) (*Service, error) {
	if cfg.Repo == nil {
		return nil, errors.New("auth: a repository is required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.TouchInterval <= 0 {
		cfg.TouchInterval = DefaultTouchInterval
	}
	return &Service{
		repo:     cfg.Repo,
		settings: cfg.Settings,
		stateDir: cfg.StateDir,
		params:   cfg.Params,
		now:      cfg.Now,
		log:      cfg.Logger,
		touch:    cfg.TouchInterval,
	}, nil
}

// Sentinels the API adapter maps onto statuses. They are errors rather than
// codes because the four session failures — no cookie, a malformed one, an
// unknown id, a wrong secret, an expired row — must be indistinguishable on the
// wire, and one sentinel is the simplest way to make that true by construction.
var (
	// ErrNoSession means no valid session accompanied the request.
	ErrNoSession = errors.New("auth: no valid session")
	// ErrSetupTokenRequired means a `setup` route was reached from a
	// non-loopback address without a valid X-Setup-Token (D38).
	ErrSetupTokenRequired = errors.New("auth: setup token required")
)

// SetupComplete reports whether `admin_account` exists — §3's setup gate. Until
// it is true every `session` endpoint answers `409 setup_required`, and the SPA
// routes to the wizard on that code alone.
func (s *Service) SetupComplete(ctx context.Context) (bool, error) {
	var exists bool
	err := s.repo.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		exists, err = s.repo.AdminAccountExists(ctx, tx)
		return err
	})
	return exists, err
}

// policy is the five `security.*` settings, resolved once per call that needs
// them. A read failure falls back to the registry default rather than failing the
// request: refusing a login because a setting could not be read would be a worse
// outcome than logging in under the documented default.
type policy struct {
	sessionTTL  time.Duration
	idleTimeout time.Duration
	maxAttempts int
	window      time.Duration
	lockout     time.Duration
}

func (s *Service) policy(ctx context.Context) policy {
	p := policy{
		sessionTTL:  720 * time.Hour,
		idleTimeout: 168 * time.Hour,
		maxAttempts: 8,
		window:      300 * time.Second,
		lockout:     900 * time.Second,
	}
	if s.settings == nil {
		return p
	}
	get := func(key string, fallback int64) int64 {
		v, err := s.settings.GetInt(ctx, key)
		if err != nil {
			s.log.Warn("could not read a security setting; using the built-in default",
				"key", key, "error", err, "default", fallback)
			return fallback
		}
		return v
	}
	p.sessionTTL = time.Duration(get("security.session_ttl_hours", 720)) * time.Hour
	p.idleTimeout = time.Duration(get("security.idle_timeout_hours", 168)) * time.Hour
	p.maxAttempts = int(get("security.login_max_attempts", 8))
	p.window = time.Duration(get("security.login_window_sec", 300)) * time.Second
	p.lockout = time.Duration(get("security.lockout_sec", 900)) * time.Second
	return p
}

// ResolveSession turns an `lm_session` cookie value into the session row it
// names, or ErrNoSession.
//
// Five failures collapse into that one sentinel — no cookie, a malformed one, an
// unknown id, a wrong secret, an expired or revoked row — because distinguishing
// them would turn the endpoint into an oracle for which session ids exist.
//
// It also slides the session window: a session that is used stays alive, and one
// that is not falls out after `security.idle_timeout_hours` even though its
// absolute expiry is further away. The write is throttled to TouchInterval so an
// authenticated burst does not write once per request.
func (s *Service) ResolveSession(ctx context.Context, cookie, ip, userAgent string) (model.Session, error) {
	if cookie == "" {
		return model.Session{}, ErrNoSession
	}
	id, secret, err := splitCookie(cookie)
	if err != nil {
		return model.Session{}, ErrNoSession
	}

	now := s.now()
	nowMS := now.UnixMilli()
	pol := s.policy(ctx)

	var sess model.Session
	err = s.repo.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		sess, err = s.repo.Session(ctx, tx, id)
		return err
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return model.Session{}, ErrNoSession
		}
		return model.Session{}, err
	}

	if !equalHash(hashSecret(secret), sess.TokenHash) {
		return model.Session{}, ErrNoSession
	}
	if !sess.Live(nowMS) {
		return model.Session{}, ErrNoSession
	}
	if idle := pol.idleTimeout; idle > 0 && sess.LastSeenAt+idle.Milliseconds() <= nowMS {
		return model.Session{}, ErrNoSession
	}

	if nowMS-sess.LastSeenAt >= s.touch.Milliseconds() {
		ipp, uap := optional(ip), optional(userAgent)
		if err := s.repo.Write(ctx, func(ctx context.Context, tx store.Tx) error {
			return s.repo.TouchSession(ctx, tx, sess.ID, nowMS, ipp, uap)
		}); err != nil {
			// A session that could not be touched is still a valid session.
			// Failing the request over the audit column would be the wrong
			// trade on a database that is momentarily busy.
			s.log.Warn("could not record session activity", "session", sess.ID, "error", err)
		} else {
			sess.LastSeenAt = nowMS
			sess.IP, sess.UserAgent = ipp, uap
		}
	}
	return sess, nil
}

// VerifyCSRF is the double-submit check the CSRF middleware delegates: only this
// package can recompute the HMAC, because only it reads `sessions.csrf_secret`.
func (s *Service) VerifyCSRF(ctx context.Context, sessionID, cookie, header string) bool {
	if sessionID == "" || cookie == "" || header == "" {
		return false
	}
	var sess model.Session
	err := s.repo.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		sess, err = s.repo.Session(ctx, tx, sessionID)
		return err
	})
	if err != nil {
		return false
	}
	return verifyCSRF(sess.ID, sess.CSRFSecret, cookie, header)
}

// SessionState answers the public `GET /api/v1/auth/session` (§3.1). A cookie
// that does not resolve is simply "not authenticated": this endpoint is the one
// the SPA calls before it has any credential, and an error there would send a
// first-time visitor to an error page instead of the wizard.
func (s *Service) SessionState(ctx context.Context, cookie string) (model.SessionState, error) {
	complete, err := s.SetupComplete(ctx)
	if err != nil {
		return model.SessionState{}, err
	}
	out := model.SessionState{SetupComplete: complete}

	sess, err := s.ResolveSession(ctx, cookie, "", "")
	if err != nil {
		if errors.Is(err, ErrNoSession) {
			return out, nil
		}
		return model.SessionState{}, err
	}
	expires := sess.ExpiresAt
	out.Authenticated = true
	out.ExpiresAt = &expires
	return out, nil
}

// Login is `POST /api/v1/auth/login` (§3.1): `204` plus cookies,
// `401 bad_credentials`, or `429 locked_out` with `retry_after_sec`.
//
// The order of operations is what makes the lockout meaningful and the hash cost
// bearable at the same time:
//
//  1. Read the lockout and the account in ONE read transaction. A blocked
//     address never reaches the hasher, so a lockout is also a CPU guard.
//  2. Verify the password OUTSIDE any transaction. argon2id deliberately takes
//     tens of milliseconds, and the write pool is a single connection (§2) —
//     hashing inside it would let one login attempt stall every other writer in
//     the daemon.
//  3. Record the attempt and its consequence in one write transaction.
func (s *Service) Login(ctx context.Context, password, ip, userAgent string) (model.SessionCredential, error) {
	now := s.now()
	nowMS := now.UnixMilli()
	pol := s.policy(ctx)

	var (
		lock     model.Lockout
		haveLock bool
		recorded bool
		acct     model.AdminAccount
		haveAcct bool
	)
	if err := s.repo.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		l, err := s.repo.Lockout(ctx, tx, ip)
		switch {
		case err == nil:
			lock, haveLock = l, true
		case errors.Is(err, store.ErrNotFound):
		default:
			return err
		}
		if haveLock && lock.Locked(nowMS) {
			// Whether this episode has already been audited, asked in the read
			// transaction that is happening anyway. See the branch below.
			recorded, err = s.repo.HasLoginAttemptSince(ctx, tx, ip, model.LoginLocked, lock.UpdatedAt)
			if err != nil {
				return err
			}
		}
		a, err := s.repo.AdminAccount(ctx, tx)
		switch {
		case err == nil:
			acct, haveAcct = a, true
		case errors.Is(err, store.ErrNotFound):
		default:
			return err
		}
		return nil
	}); err != nil {
		return model.SessionCredential{}, err
	}

	if haveLock && lock.Locked(nowMS) {
		retry := time.Duration(lock.LockedUntil-nowMS) * time.Millisecond
		// AT MOST ONE audit row per lockout episode. A blocked address that
		// keeps knocking has nothing to record that the `lockouts` row does not
		// already say, and this endpoint is unauthenticated: writing a row per
		// request would let a flood from the LAN insert one per request through
		// a write pool that is a single connection (§2), serializing ahead of
		// every other writer in the daemon — supervisor status updates, ledger
		// closures, job leases. The first refusal of an episode is audited; the
		// rest spend a read and nothing else. This is the same cap §5.8 puts on
		// its `inhibited` rows, and for the same reason.
		if !recorded {
			if err := s.recordAttempt(ctx, ip, nowMS, false, model.LoginLocked); err != nil {
				return model.SessionCredential{}, err
			}
		}
		return model.SessionCredential{}, lockedOutError(retry)
	}

	if !haveAcct {
		// No account exists: this host has not been claimed. The answer is the
		// same `bad_credentials` a wrong password gets, so that login is not an
		// oracle for the claim state — `GET /api/v1/meta` answers that question
		// for callers who are entitled to ask it.
		return model.SessionCredential{}, s.failLogin(ctx, ip, nowMS, pol, model.LoginNoAccount)
	}

	rehash, err := s.params.Verify(acct.PasswordHash, password)
	if err != nil {
		if !errors.Is(err, ErrPasswordMismatch) {
			// A hash this binary cannot parse is a corrupted or hand-edited
			// row. It is not the caller's fault and it is not a wrong password:
			// say so in the log and answer the same 401, because there is no
			// password that could succeed against it.
			s.log.Error("the stored password hash could not be parsed", "error", err)
		}
		return model.SessionCredential{}, s.failLogin(ctx, ip, nowMS, pol, model.LoginBadPassword)
	}

	var cred model.SessionCredential
	err = s.repo.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		if err := s.repo.InsertLoginAttempt(ctx, tx, s.attempt(nowMS, ip, true, model.LoginOK)); err != nil {
			return err
		}
		if err := s.repo.DeleteLockout(ctx, tx, ip); err != nil {
			return err
		}
		if rehash {
			// The stored hash was made with parameters weaker than this
			// binary's. A successful login is the only moment the plaintext is
			// available to upgrade it, so it is taken.
			if next, err := s.params.Hash(password); err == nil {
				if _, err := s.repo.SetAdminPassword(ctx, tx, next, nowMS); err != nil {
					return err
				}
			} else {
				s.log.Warn("could not rehash the password at the current cost", "error", err)
			}
		}
		var err error
		cred, err = s.mintSession(ctx, tx, now, pol, ip, userAgent)
		return err
	})
	if err != nil {
		return model.SessionCredential{}, err
	}
	return cred, nil
}

// failLogin records one failed attempt and applies the lockout when this failure
// exhausted the address's budget. It returns the error the caller answers with:
// `bad_credentials` normally, and `locked_out` when this was the last strike —
// telling the caller immediately that further attempts are pointless is both
// kinder and less work than letting them discover it one request later.
func (s *Service) failLogin(ctx context.Context, ip string, nowMS int64, pol policy, reason model.LoginReason) error {
	var locked time.Duration
	err := s.repo.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		if err := s.repo.InsertLoginAttempt(ctx, tx, s.attempt(nowMS, ip, false, reason)); err != nil {
			return err
		}

		// Count only the failures since the window opened — and never further
		// back than two other events, each of which ends the budget that was
		// being spent before it:
		//
		//   - the last lockout this address served. The strikes already paid
		//     for must not be counted twice, or the first failure after a block
		//     expired would re-lock instantly.
		//   - the last SUCCESSFUL login from this address. A success is proof
		//     that the operator is the operator; counting the failures that
		//     preceded it would re-lock the account they just got back into on
		//     the next typo.
		since := nowMS - pol.window.Milliseconds()
		strikes := 0
		prev, err := s.repo.Lockout(ctx, tx, ip)
		switch {
		case err == nil:
			strikes = prev.Strikes
			if prev.UpdatedAt >= since {
				since = prev.UpdatedAt + 1
			}
		case errors.Is(err, store.ErrNotFound):
		default:
			return err
		}
		if at, ok, err := s.repo.LastSuccessfulLoginAt(ctx, tx, ip); err != nil {
			return err
		} else if ok && at >= since {
			since = at + 1
		}

		n, err := s.repo.CountFailedLoginAttempts(ctx, tx, ip, since, guessReasons())
		if err != nil {
			return err
		}
		if pol.maxAttempts <= 0 || n < pol.maxAttempts {
			return nil
		}

		strikes++
		locked = lockoutFor(pol.lockout, strikes)
		return s.repo.PutLockout(ctx, tx, model.Lockout{
			IP:          ip,
			LockedUntil: nowMS + locked.Milliseconds(),
			Strikes:     strikes,
			UpdatedAt:   nowMS,
		})
	})
	if err != nil {
		return err
	}
	if locked > 0 {
		return lockedOutError(locked)
	}
	return model.Error{
		Code:    model.CodeBadCredentials,
		Message: "the password is not correct",
	}
}

// guessReasons are the failure reasons that count toward the lockout budget: an
// actual attempt to present a credential. A `locked` row is a refusal, not a
// guess, and counting it would make the next attempt after a block expired
// re-lock instantly on a budget the caller never got to spend.
func guessReasons() []model.LoginReason {
	return []model.LoginReason{model.LoginBadPassword, model.LoginNoAccount, model.LoginBadSetupToken}
}

// MaxLockout caps the escalation. A block longer than a day would lock an
// operator out of their own host for a working day over a forgotten password,
// which is a worse outcome than the marginal slowdown it buys against an attacker
// who is already down to one guess per hour.
const MaxLockout = 24 * time.Hour

// lockoutFor doubles the block with each strike, capped at MaxLockout: 15 min,
// 30, 60, … The `lockouts.strikes` column exists for exactly this, and the
// escalation is what makes a distributed guessing attack from one address
// pointless without punishing the first mistake.
func lockoutFor(base time.Duration, strikes int) time.Duration {
	if base <= 0 {
		base = 900 * time.Second
	}
	d := base
	for i := 1; i < strikes && d < MaxLockout; i++ {
		d *= 2
	}
	if d > MaxLockout {
		d = MaxLockout
	}
	return d
}

func lockedOutError(retry time.Duration) error {
	secs := int((retry + time.Second - 1) / time.Second)
	if secs < 1 {
		secs = 1
	}
	return model.Error{
		Code:    model.CodeLockedOut,
		Message: fmt.Sprintf("too many failed attempts; try again in %d seconds", secs),
		Details: map[string]any{"retry_after_sec": secs},
	}
}

// Logout revokes one session — `POST /api/v1/auth/logout` (§3.1).
func (s *Service) Logout(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return ErrNoSession
	}
	return s.repo.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		_, err := s.repo.RevokeSession(ctx, tx, sessionID, s.now().UnixMilli())
		return err
	})
}

// ChangePassword is `POST /api/v1/auth/password` (§3.1): verify the current
// password, store a fresh hash, and revoke every OTHER session — a password
// change is how an operator evicts a device they no longer control, so it has to
// take effect everywhere except here.
func (s *Service) ChangePassword(ctx context.Context, sessionID, current, next string) error {
	if err := ValidatePassword(next); err != nil {
		return err
	}

	var acct model.AdminAccount
	if err := s.repo.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		acct, err = s.repo.AdminAccount(ctx, tx)
		return err
	}); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return model.Error{Code: model.CodeBadCredentials, Message: "this host has no admin account"}
		}
		return err
	}

	if _, err := s.params.Verify(acct.PasswordHash, current); err != nil {
		if errors.Is(err, ErrPasswordMismatch) {
			return model.Error{Code: model.CodeBadCredentials, Message: "the current password is not correct"}
		}
		return err
	}

	hash, err := s.params.Hash(next)
	if err != nil {
		return err
	}
	nowMS := s.now().UnixMilli()

	return s.repo.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		if _, err := s.repo.SetAdminPassword(ctx, tx, hash, nowMS); err != nil {
			return err
		}
		revoked, err := s.repo.RevokeSessionsExcept(ctx, tx, sessionID, nowMS)
		if err != nil {
			return err
		}
		return s.repo.AppendEvent(ctx, tx, model.Event{
			ID:       store.NewID(s.now()),
			At:       nowMS,
			Level:    model.LevelWarn,
			Category: model.CategoryAuth,
			Action:   "password_changed",
			Actor:    model.ActorAdmin,
			Message:  fmt.Sprintf("the admin password was changed; %d other session(s) were revoked", revoked),
		})
	})
}

// Sessions lists the active sessions — `GET /api/v1/auth/sessions` (§3.1) —
// marking the one the request arrived on so the UI can label it rather than
// inviting a human to revoke the session they are looking through.
func (s *Service) Sessions(ctx context.Context, currentID string) ([]model.SessionInfo, error) {
	var rows []model.Session
	if err := s.repo.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		rows, err = s.repo.ActiveSessions(ctx, tx, s.now().UnixMilli())
		return err
	}); err != nil {
		return nil, err
	}

	out := make([]model.SessionInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, model.SessionInfo{
			ID:         r.ID,
			IP:         r.IP,
			UserAgent:  r.UserAgent,
			CreatedAt:  r.CreatedAt,
			LastSeenAt: r.LastSeenAt,
			ExpiresAt:  r.ExpiresAt,
			Current:    r.ID == currentID,
		})
	}
	return out, nil
}

// RevokeSession is `DELETE /api/v1/auth/sessions/{id}` (§3.1). Revoking one's
// own session is allowed and is simply a logout — refusing it would be a rule the
// UI would have to know about for no security gain.
func (s *Service) RevokeSession(ctx context.Context, id string) error {
	var changed bool
	err := s.repo.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		changed, err = s.repo.RevokeSession(ctx, tx, id, s.now().UnixMilli())
		return err
	})
	if err != nil {
		return err
	}
	if !changed {
		return store.ErrNotFound
	}
	return nil
}

// mintSession writes one session row and returns the credential for the browser.
// It must be called inside a write transaction: on the claim path the session and
// the `admin_account` row are created together (§2.2a step 5).
func (s *Service) mintSession(ctx context.Context, tx store.Tx, now time.Time, pol policy,
	ip, userAgent string) (model.SessionCredential, error) {

	secret, err := randomSecret(secretLen)
	if err != nil {
		return model.SessionCredential{}, err
	}
	csrfSecret, err := randomSecret(secretLen)
	if err != nil {
		return model.SessionCredential{}, err
	}

	nowMS := now.UnixMilli()
	id := store.NewID(now)
	row := model.Session{
		ID:         id,
		TokenHash:  hashSecret(secret),
		CSRFSecret: csrfSecret,
		CreatedAt:  nowMS,
		LastSeenAt: nowMS,
		ExpiresAt:  nowMS + pol.sessionTTL.Milliseconds(),
		IP:         optional(ip),
		UserAgent:  optional(userAgent),
	}
	if err := s.repo.InsertSession(ctx, tx, row); err != nil {
		return model.SessionCredential{}, err
	}
	return model.SessionCredential{
		SessionID:     id,
		SessionCookie: composeCookie(id, secret),
		CSRFToken:     CSRFToken(id, csrfSecret),
		ExpiresAt:     row.ExpiresAt,
	}, nil
}

func (s *Service) attempt(at int64, ip string, success bool, reason model.LoginReason) model.LoginAttempt {
	r := reason
	return model.LoginAttempt{
		ID:      store.NewID(time.UnixMilli(at)),
		At:      at,
		IP:      ip,
		Success: success,
		Reason:  &r,
	}
}

// recordAttempt writes one audit row on a path that has nothing else to write.
func (s *Service) recordAttempt(ctx context.Context, ip string, at int64, success bool, reason model.LoginReason) error {
	return s.repo.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		return s.repo.InsertLoginAttempt(ctx, tx, s.attempt(at, ip, success, reason))
	})
}

// optional turns "" into a NULL column, because §2's F14 rule is that a fact
// nobody learned is NULL rather than a zero value that reads as an answer.
func optional(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}
