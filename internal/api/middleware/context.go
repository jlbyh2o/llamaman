package middleware

import (
	"context"
	"net/http"
	"sync/atomic"
	"time"
)

// Session is what the session gate resolves a `lm_session` cookie into and puts
// in the request context. It is deliberately the smallest shape a handler needs:
// internal/auth owns the row, the argon2id verification, the lockout and the
// rotation, and hands this back.
type Session struct {
	// ID is the `sessions.id` ULID — the half of the cookie that is stored in
	// the clear. The secret half is never carried here.
	ID string
	// CreatedAt and ExpiresAt are the window `GET /api/v1/auth/session`
	// reports. ExpiresAt is what the gate refuses on.
	CreatedAt time.Time
	ExpiresAt time.Time
	// IP and UserAgent are the last-seen facts `GET /api/v1/auth/sessions`
	// lists.
	IP        string
	UserAgent string
}

// contextKey is unexported so nothing outside this package can collide with,
// or forge, a value the gate put in the context.
type contextKey int

const (
	sessionKey contextKey = iota
	idempotencyKey
	routeKey
	routeSlotKey
)

// WithSession returns ctx carrying s. Only the session gate calls it.
func WithSession(ctx context.Context, s *Session) context.Context {
	return context.WithValue(ctx, sessionKey, s)
}

// SessionFrom returns the session the gate resolved for this request. The
// second result is false on a `public` route, and on any route whose gate did
// not run — which is what makes "is this request authenticated" a question a
// handler answers from the context rather than by re-reading the cookie.
func SessionFrom(ctx context.Context) (*Session, bool) {
	s, ok := ctx.Value(sessionKey).(*Session)
	return s, ok && s != nil
}

// WithIdempotencyKey returns ctx carrying the validated Idempotency-Key header.
func WithIdempotencyKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, idempotencyKey, key)
}

// IdempotencyKeyFrom returns the validated Idempotency-Key for this request, if
// the client sent one on a route that accepts it (D39/D65). A handler passes it
// straight into jobs.EnqueueParams; the 10-minute replay window and the
// `422 idempotency_key_reused` fingerprint check live in internal/jobs and
// internal/store, not here. This layer only extracts and validates the header.
func IdempotencyKeyFrom(ctx context.Context) (string, bool) {
	k, ok := ctx.Value(idempotencyKey).(string)
	return k, ok && k != ""
}

// WithRouteSlot installs an empty holder for the matched route's operation id.
//
// It exists because of an ordering that cannot be avoided: the route is not
// known until the mux has matched, but the two layers that most want to name it
// — the request log and the panic recovery — are OUTSIDE the mux, and a
// context value added by an inner layer never reaches an outer one (the inner
// layer gets a new *http.Request; the outer one still holds the old). The
// outermost layer therefore installs a settable slot on the way in, and the
// per-route wrapper fills it on the way through.
//
// Calling it twice is a no-op, so a chain that installs it in more than one
// place still has exactly one slot.
func WithRouteSlot(ctx context.Context) context.Context {
	if _, ok := ctx.Value(routeSlotKey).(*atomic.Pointer[string]); ok {
		return ctx
	}
	return context.WithValue(ctx, routeSlotKey, new(atomic.Pointer[string]))
}

// WithRoute returns ctx carrying the operation id of the matched route, and
// fills the slot an outer layer installed, if there is one.
func WithRoute(ctx context.Context, operationID string) context.Context {
	if slot, ok := ctx.Value(routeSlotKey).(*atomic.Pointer[string]); ok {
		slot.Store(&operationID)
	}
	return context.WithValue(ctx, routeKey, operationID)
}

// RouteFrom returns the operation id of the route this request matched. It is
// what the request log names, and what the D43 response-conformance checker
// looks its documented responses up by — without it the checker would have to
// re-derive the match from the path, which is exactly the duplication that lets
// a spec and a router disagree.
//
// It reads the direct value first (a handler's own context) and the slot
// second (an outer layer asking after the fact).
func RouteFrom(ctx context.Context) (string, bool) {
	if id, ok := ctx.Value(routeKey).(string); ok && id != "" {
		return id, true
	}
	if slot, ok := ctx.Value(routeSlotKey).(*atomic.Pointer[string]); ok {
		if id := slot.Load(); id != nil && *id != "" {
			return *id, true
		}
	}
	return "", false
}

// clientIP is the best available identity for rate limiting and for the
// `sessions` audit columns. It is r.RemoteAddr's host and NOTHING ELSE:
// X-Forwarded-For is attacker-controlled on a daemon that binds a LAN address
// directly (SPEC's deployment is "open the browser at http://host:port"), and
// trusting it would let any client mint an unlimited number of rate-limit
// buckets by varying one header.
func clientIP(r *http.Request) string {
	addr := r.RemoteAddr
	// Strip the port without importing net for a single split: the last colon
	// separates it, and an IPv6 literal is bracketed.
	for i := len(addr) - 1; i >= 0; i-- {
		switch addr[i] {
		case ':':
			return addr[:i]
		case ']':
			return addr
		}
	}
	return addr
}
