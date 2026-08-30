package middleware

import (
	"context"
	"errors"
	"net"
	"net/http"
)

// Errors an Authenticator returns. They are sentinels rather than typed values
// because the gate's only decision is which status to write.
var (
	// ErrNoSession means no valid session accompanied the request: no cookie,
	// a malformed one, an unknown id, a wrong secret, or an expired row. The
	// four are deliberately indistinguishable on the wire — telling them apart
	// would turn the endpoint into a session-id oracle — so the gate answers
	// 401 `unauthorized` for all of them.
	ErrNoSession = errors.New("middleware: no valid session")
	// ErrSetupTokenRequired means a `setup` route was reached from a
	// non-loopback address without a valid X-Setup-Token (D38).
	ErrSetupTokenRequired = errors.New("middleware: setup token required")
)

// Authenticator is everything the session and CSRF layers need from
// internal/auth (DESIGN section 1: dependencies point inward, and a consumer
// owns the interface it needs). internal/auth will satisfy it; until then
// internal/app wires the Unconfigured implementation below, whose SetupComplete
// still answers from the database so `setup_required` is honest from the first
// boot.
type Authenticator interface {
	// SetupComplete reports whether `admin_account` exists. It is the setup
	// gate of section 3: until it is true, every `session` endpoint answers
	// 409 setup_required, and the SPA routes to the wizard on that code alone.
	SetupComplete(ctx context.Context) (bool, error)

	// Authenticate resolves the `lm_session` cookie. It returns ErrNoSession
	// for every failure mode, and on success a Session the gate puts in the
	// request context.
	Authenticate(ctx context.Context, r *http.Request) (*Session, error)

	// AuthorizeSetup decides whether a `setup` route may proceed: loopback
	// callers always may, everyone else must present a valid X-Setup-Token
	// (D38). It returns ErrSetupTokenRequired otherwise.
	AuthorizeSetup(ctx context.Context, r *http.Request) error

	// VerifyCSRF checks the double-submit pair for s in constant time: cookie
	// is the `lm_csrf` value the browser sent back, header is X-CSRF-Token.
	// Only internal/auth knows the HMAC key, which is why this is not a plain
	// string comparison in the CSRF layer.
	VerifyCSRF(ctx context.Context, s *Session, cookie, header string) bool
}

// SessionGate is the first layer of the per-route chain (DESIGN section 1:
// "session, CSRF, rate limit, idempotency"). It branches on nothing but the
// route's own Auth column, which is what makes the routing table the single
// statement of who may call what.
//
// The order of the two checks inside AuthSession is not interchangeable. The
// setup gate runs FIRST, so a browser that has never claimed this host gets
// `409 setup_required` and routes to the wizard, rather than a `401
// unauthorized` that would send it to a login form for an account that does not
// exist.
func SessionGate(auth Auth, a Authenticator) Middleware {
	if auth == AuthPublic {
		// No layer at all rather than a pass-through wrapper: a public route
		// should cost nothing and should not appear in a stack trace.
		return nil
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			if a == nil {
				WriteError(w, http.StatusInternalServerError, CodeInternalError,
					"the API was built without an authenticator", nil)
				return
			}

			complete, err := a.SetupComplete(ctx)
			if err != nil {
				WriteError(w, http.StatusInternalServerError, CodeInternalError,
					"could not determine whether this host has been claimed", nil)
				return
			}

			switch auth {
			case AuthSession:
				if !complete {
					WriteError(w, http.StatusConflict, CodeSetupRequired,
						"this host has not been set up yet", nil)
					return
				}
				sess, err := a.Authenticate(ctx, r)
				if err != nil || sess == nil {
					if err != nil && !errors.Is(err, ErrNoSession) {
						WriteError(w, http.StatusInternalServerError, CodeInternalError,
							"could not verify the session", nil)
						return
					}
					WriteError(w, http.StatusUnauthorized, CodeUnauthorized,
						"a valid admin session is required", nil)
					return
				}
				r = r.WithContext(WithSession(ctx, sess))

			case AuthSetup:
				if complete {
					WriteError(w, http.StatusConflict, CodeSetupAlreadyClaimed,
						"this host has already been claimed", nil)
					return
				}
				if err := a.AuthorizeSetup(ctx, r); err != nil {
					if !errors.Is(err, ErrSetupTokenRequired) {
						WriteError(w, http.StatusInternalServerError, CodeInternalError,
							"could not verify the setup token", nil)
						return
					}
					WriteError(w, http.StatusForbidden, CodeSetupTokenRequired,
						"a setup token is required from a non-loopback address", nil)
					return
				}

			default:
				// AuthToken is section 3.15's gateway credential, which is not
				// this API at all, and the route registry refuses it at
				// registration. Reaching here is a construction bug.
				WriteError(w, http.StatusInternalServerError, CodeInternalError,
					"route declared an authentication mode this API does not serve", nil)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// Unconfigured is the Authenticator the composition root wires before
// internal/auth exists: it answers SetupComplete from a callback the caller
// supplies (so the `setup_required` gate is truthful from the first boot) and
// denies everything else.
//
// It is not a test double. It is the honest behavior of a daemon whose
// authentication subsystem has not been built yet: `session` routes answer
// `setup_required` or `unauthorized`, and `setup` routes admit loopback — which
// is exactly D38's rule and the only part of the claim flow that does not need
// a password hasher.
type Unconfigured struct {
	// Complete answers SetupComplete. Nil means "never claimed".
	Complete func(ctx context.Context) (bool, error)
}

// SetupComplete implements Authenticator.
func (u Unconfigured) SetupComplete(ctx context.Context) (bool, error) {
	if u.Complete == nil {
		return false, nil
	}
	return u.Complete(ctx)
}

// Authenticate implements Authenticator: there are no sessions yet.
func (u Unconfigured) Authenticate(context.Context, *http.Request) (*Session, error) {
	return nil, ErrNoSession
}

// AuthorizeSetup implements D38's loopback rule, which needs no secret.
func (u Unconfigured) AuthorizeSetup(_ context.Context, r *http.Request) error {
	if IsLoopback(r) {
		return nil
	}
	return ErrSetupTokenRequired
}

// VerifyCSRF implements Authenticator: with no sessions there is no pair to
// verify, and a false here can never reject a request the gate above did not
// already reject with 401.
func (u Unconfigured) VerifyCSRF(context.Context, *Session, string, string) bool { return false }

// IsLoopback reports whether the request arrived from this machine, which is
// D38's whole first-run rule: "a request from loopback may claim the daemon
// with no token at all". It reads r.RemoteAddr and nothing else, for the same
// reason clientIP does.
func IsLoopback(r *http.Request) bool {
	host := clientIP(r)
	host = trimBrackets(host)
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func trimBrackets(s string) string {
	if len(s) >= 2 && s[0] == '[' && s[len(s)-1] == ']' {
		return s[1 : len(s)-1]
	}
	return s
}
