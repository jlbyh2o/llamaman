package model

// Rows and views for identity (DESIGN sections 2.2, 2.2a, 3.1, 3.2).
//
// The row structs mirror the four tables of §2.2 that carry an admin's identity:
// `admin_account`, `sessions`, `login_attempts` and `lockouts`. The VIEW structs
// below them are the shapes internal/auth and internal/setup hand back to
// internal/api, which converts them into wire DTOs. They live here rather than in
// either package for the reason §1 gives: dependencies point inward, so a domain
// service cannot name a type internal/api owns, and internal/api must not import
// a service to name its results.
//
// No struct here carries a secret. A session's secret half exists only in the
// cookie and as sha256 in the row; a password exists only as an argon2id encoded
// string. SessionCredential is the single exception and says so in its own doc
// comment: it is the value being handed to the browser, in flight, never stored.

// AdminAccount is the singleton `admin_account` row (§2.2). Its existence IS the
// setup gate of §3: until this row exists every `session` endpoint answers
// `409 setup_required`, and the SPA routes to the wizard on that code alone.
type AdminAccount struct {
	// PasswordHash is an argon2id encoded string — the PHC form carrying the
	// parameters and salt, so a later release can raise the cost without a
	// migration and rehash on the next successful login.
	PasswordHash  string
	PasswordSetAt int64
	UpdatedAt     int64
}

// Session is one `sessions` row (§2.2). The cookie is `<id>.<secret>`; only
// sha256(secret) is stored, so a database read — a backup, a diagnostics bundle,
// a `VACUUM INTO` snapshot — cannot mint a usable cookie.
type Session struct {
	ID         string
	TokenHash  string
	CSRFSecret string
	CreatedAt  int64
	LastSeenAt int64
	ExpiresAt  int64
	RevokedAt  *int64
	IP         *string
	UserAgent  *string
}

// Live reports whether the session may still authenticate a request at `now`:
// not revoked, and not past its absolute expiry. The IDLE timeout is not checked
// here because it is a policy read from `security.idle_timeout_hours` rather than
// a property of the row, and internal/auth owns policy.
func (s Session) Live(now int64) bool {
	return s.RevokedAt == nil && s.ExpiresAt > now
}

// LoginAttempt is one `login_attempts` row (§2.2) — the audit trail behind the
// lockout of SPEC §4. Reason is a pointer because the column is nullable and
// because a future release must be able to record a reason this one does not
// name, which is exactly why the column carries no CHECK.
type LoginAttempt struct {
	ID      string
	At      int64
	IP      string
	Success bool
	Reason  *LoginReason
}

// Lockout is one `lockouts` row (§2.2): the per-IP block SPEC §4 requires.
// Strikes counts how many times this address has exhausted its attempt budget,
// and is what lets a repeat offender be held longer than a first one.
type Lockout struct {
	IP          string
	LockedUntil int64
	Strikes     int
	UpdatedAt   int64
}

// Locked reports whether the block is still in force at `now`.
func (l Lockout) Locked(now int64) bool { return l.LockedUntil > now }

// SessionCredential is a freshly minted session on its way to the browser, and
// the ONE value in this package that carries a live secret. internal/api turns it
// into the two cookies §3 fixes — `lm_session` (HttpOnly) and `lm_csrf` (readable
// by the SPA, echoed in `X-CSRF-Token`) — and nothing persists it.
type SessionCredential struct {
	// SessionID is the row id, for logging and for "this is the current
	// session" in a session listing.
	SessionID string
	// SessionCookie is `<session_id>.<secret>`: the whole value of `lm_session`.
	SessionCookie string
	// CSRFToken is the value of `lm_csrf` AND of the `X-CSRF-Token` header the
	// double-submit compares it against.
	CSRFToken string
	// ExpiresAt is the session's absolute expiry, Unix milliseconds.
	ExpiresAt int64
}

// SessionInfo is one row of `GET /api/v1/auth/sessions` (§3.1): the active
// sessions, with the audit columns that let a human recognize a device they no
// longer use. It carries no hash and no secret.
type SessionInfo struct {
	ID         string
	IP         *string
	UserAgent  *string
	CreatedAt  int64
	LastSeenAt int64
	ExpiresAt  int64
	// Current marks the session the request itself arrived on, so the UI can
	// label it rather than inviting a human to revoke the session they are
	// looking at it through.
	Current bool
}

// SessionState is the body behind `GET /api/v1/auth/session` (§3.1). It is a
// PUBLIC endpoint, so the three fields are everything an unauthenticated caller
// may learn: whether this request carries a session, whether the host has been
// set up at all, and when the session it does carry runs out.
type SessionState struct {
	Authenticated bool
	SetupComplete bool
	// ExpiresAt is nil when Authenticated is false.
	ExpiresAt *int64
}
