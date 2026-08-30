package middleware

import (
	"encoding/json"
	"net/http"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// Middleware is one layer of http.Handler wrapping.
type Middleware func(http.Handler) http.Handler

// Chain is an ordered list of layers, OUTERMOST FIRST. Then applies them in
// reverse so that c[0] sees the request before c[1], which is how DESIGN
// section 1 reads its own list of layers.
type Chain []Middleware

// Then wraps h in every layer of c and returns the resulting handler. A nil
// entry is skipped, which lets a caller build a chain with conditional layers
// without branching around the append.
func (c Chain) Then(h http.Handler) http.Handler {
	for i := len(c) - 1; i >= 0; i-- {
		if c[i] == nil {
			continue
		}
		h = c[i](h)
	}
	return h
}

// Append returns c with ms added at the inside of the chain, skipping nils.
func (c Chain) Append(ms ...Middleware) Chain {
	for _, m := range ms {
		if m != nil {
			c = append(c, m)
		}
	}
	return c
}

// Auth is the "Auth" column of every endpoint table in DESIGN section 3, and
// the single input the session gate branches on. It lives in this package
// rather than in internal/api because the gate is what reads it and internal/api
// imports this package (never the other way round).
type Auth string

const (
	// AuthPublic: no credential at all — /healthz, /api/v1/meta,
	// /api/v1/auth/session, /api/v1/auth/login, /api/v1/setup/state.
	AuthPublic Auth = "public"
	// AuthSession: an admin session cookie, plus the CSRF double-submit on
	// every non-GET.
	AuthSession Auth = "session"
	// AuthSetup: allowed only BEFORE the admin account exists, and then only
	// from loopback or with a valid X-Setup-Token (D38).
	AuthSetup Auth = "setup"
	// AuthToken: the per-instance gateway ports, which are not this API
	// (section 3.15). The route registry refuses it.
	AuthToken Auth = "token"
)

// AuthValues lists the four values of section 3's Auth column, in the order
// that section introduces them.
func AuthValues() []Auth { return []Auth{AuthPublic, AuthSession, AuthSetup, AuthToken} }

// Valid reports whether a is one of the four.
func (a Auth) Valid() bool {
	for _, v := range AuthValues() {
		if a == v {
			return true
		}
	}
	return false
}

// The cookie and header names DESIGN section 3 fixes. They are constants here
// because three layers (the session gate, the CSRF check and internal/auth's
// login handler) must agree on them exactly.
const (
	// CookieSession holds `<session_id>.<secret>`; HttpOnly, SameSite=Lax,
	// Path=/, Secure only over TLS (section 3).
	CookieSession = "lm_session"
	// CookieCSRF is deliberately NOT HttpOnly: the SPA reads it and echoes it
	// back in HeaderCSRF. It holds an HMAC of the session's csrf_secret, never
	// the secret.
	CookieCSRF = "lm_csrf"
	// HeaderCSRF is the double-submit echo every non-GET must carry.
	HeaderCSRF = "X-CSRF-Token"
	// HeaderSetupToken carries the one-time claim token for a non-loopback
	// caller during setup (D38).
	HeaderSetupToken = "X-Setup-Token"
	// HeaderIdempotencyKey is the optional replay key on job-creating POSTs
	// (D39/D65).
	HeaderIdempotencyKey = "Idempotency-Key"
)

// The error codes this package emits. DESIGN section 3 enumerates codes as they
// arrive with their endpoints, so — following the precedent internal/sse set
// with `invalid_topic` — the codes a middleware layer owns are declared beside
// the layer that writes them rather than in internal/model, whose catalog is
// the set of codes some COLUMN is closed by.
const (
	// CodeUnauthorized is the 401 a `session` route answers when no valid
	// session cookie accompanied the request.
	CodeUnauthorized model.ErrorCode = "unauthorized"
	// CodeCSRFFailed is the 403 for a non-GET whose double-submit token is
	// missing, mismatched, or whose Origin/Sec-Fetch-Site says cross-site.
	CodeCSRFFailed model.ErrorCode = "csrf_failed"
	// CodeSetupRequired is section 3's setup gate: until `admin_account`
	// exists, every `session` endpoint answers 409 with this code, and the SPA
	// routes to the wizard on that code alone.
	CodeSetupRequired model.ErrorCode = "setup_required"
	// CodeSetupTokenRequired is the 403 a `setup` route answers a non-loopback
	// caller that presented no valid X-Setup-Token (D38).
	CodeSetupTokenRequired model.ErrorCode = "setup_token_required"
	// CodeSetupAlreadyClaimed is the 409 a `setup` route answers once the
	// admin account exists: the claim window is closed, and answering
	// `setup_required` would tell the SPA the exact opposite of the truth.
	CodeSetupAlreadyClaimed model.ErrorCode = "setup_already_claimed"
	// CodeUnsupportedMediaType is the 415 for a request body sent as anything
	// other than JSON — including one sent with no Content-Type at all, which
	// is the shape a cross-origin request built to avoid a preflight has.
	// internal/api re-exports it: one code, whether the decoder or the setup
	// guard is the layer that answers.
	CodeUnsupportedMediaType model.ErrorCode = "unsupported_media_type"
	// CodeIdempotencyKeyInvalid is the 400 for an Idempotency-Key that is not
	// a short printable ASCII token. It is distinct from
	// model.CodeIdempotencyKeyReused, which is a 422 about a well-formed key
	// used for two different bodies.
	CodeIdempotencyKeyInvalid model.ErrorCode = "idempotency_key_invalid"
	// CodeInternalError is the 500 the recover layer writes, and the one the
	// session gate writes when the authenticator itself failed.
	CodeInternalError model.ErrorCode = "internal_error"
)

// WriteError writes the one error body DESIGN section 3 defines —
// {"error":{"code":…,"message":…,"details":{…}}} — with a matching status. It
// is exported because internal/api renders the same envelope and there must be
// exactly one writer of it: two would drift.
//
// Cache-Control: no-store is set because an error is never a cacheable
// representation of a resource, and a 401 cached by an intermediary would
// outlive the login that fixes it.
func WriteError(w http.ResponseWriter, status int, code model.ErrorCode, message string, details map[string]any) {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Del("Content-Length")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(model.ErrorEnvelope{
		Error: model.Error{Code: code, Message: message, Details: details},
	})
}
