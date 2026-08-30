package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jlbyh2o/llamaman/internal/api/middleware"
)

// Config wires an API. Every collaborator is an interface or an http.Handler
// owned by this package, so the composition root can build the API before the
// subsystem behind a field exists (DESIGN section 1: dependencies point inward,
// and a consumer owns the interface it needs).
type Config struct {
	// Logger receives one line per request and one per 5xx. Nil uses
	// slog.Default.
	Logger *slog.Logger

	// Now supplies every instant this layer stamps or measures. Nil uses
	// time.Now.
	Now func() time.Time

	// Auth is the session, CSRF and setup-token authority. internal/auth will
	// satisfy it; until then internal/app wires middleware.Unconfigured, whose
	// SetupComplete still answers from the database so the `setup_required`
	// gate is truthful from the first boot. Nil makes every non-public route
	// answer 500, which is a construction bug rather than a request-time
	// condition — but it is a 500 rather than a panic, because taking the
	// daemon down would take the gateway listeners with it (section 9.4).
	Auth middleware.Authenticator

	// Meta answers `GET /api/v1/meta`, the endpoint install.sh polls
	// (section 3.1).
	Meta MetaProvider

	// Sessions serves the auth endpoints of section 3.1 — login, logout,
	// password change, session listing and revocation. internal/auth satisfies
	// it, and NewAuthenticator adapts the same value to Auth above, so one
	// service answers both the gate and the endpoints and the two can never
	// disagree about what a session is. Nil answers those routes 503 rather
	// than removing them: they are documented endpoints, and a build gap is
	// reported, never faked.
	Sessions SessionService

	// Setup serves the wizard endpoints of section 3.2 that exist today — the
	// state, the claim, the skip and the completion. internal/setup satisfies
	// it. Nil answers 503, for the same reason.
	Setup SetupService

	// Instances serves the instance endpoints of section 3.10 that exist today
	// — list, create, detail, patch and delete. internal/instances satisfies
	// it. Nil answers those routes 503 rather than removing them: they are
	// documented endpoints, and a build gap is reported, never faked.
	Instances InstanceService

	// Events is the SSE transport mounted at `GET /api/v1/events`
	// (section 3.14). It is internal/sse's Handler, which owns the handshake,
	// the heartbeat and Last-Event-ID replay; this package owns only the route
	// and the session gate in front of it. Nil mounts nothing, and the route
	// is absent from the registry and therefore from openapi.json — a document
	// that promised an endpoint the binary does not serve would fail the D43
	// conformance suite, correctly.
	Events http.Handler

	// Fallback serves everything this API does not: the embedded SPA and its
	// client-side routes (internal/web). Nil answers 404 in the error
	// envelope, which is the right behavior for a binary built without a UI.
	Fallback http.Handler

	// Conformance is the D43 response-conformance middleware, wrapped around
	// the mux. It is nil in production and supplied by the integration suite,
	// where an undocumented endpoint, a missing documented field or an extra
	// field fails the run.
	Conformance func(http.Handler) http.Handler
}

// API is the mounted REST surface: a route registry, a ServeMux built from it,
// and the middleware chain in front of both.
type API struct {
	cfg     Config
	reg     *Registry
	handler http.Handler
	log     *slog.Logger
	now     func() time.Time
}

// New builds the API. It returns an error rather than panicking for every
// registration mistake — a bad pattern, a duplicate operation id, a
// multi-segment wildcard that is not final — so that the composition root fails
// with a message instead of a stack trace, and so a test can assert the
// message.
func New(cfg Config) (*API, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}

	a := &API{cfg: cfg, reg: NewRegistry(), log: cfg.Logger, now: cfg.Now}

	if err := a.register(); err != nil {
		return nil, err
	}
	h, err := a.build()
	if err != nil {
		return nil, err
	}
	a.handler = h
	return a, nil
}

// Routes returns the registered route table — what the OpenAPI generator reads
// and what the routing-table test asserts.
func (a *API) Routes() []Route { return a.reg.Routes() }

// Registry returns the route registry itself.
func (a *API) Registry() *Registry { return a.reg }

// ServeHTTP serves a request through the global chain and the mux.
func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) { a.handler.ServeHTTP(w, r) }

// register declares every route this binary serves. It is one function on
// purpose: DESIGN section 3 states the API as a set of tables, and the closest
// code can come to that is a single readable list.
func (a *API) register() error {
	var errs []error
	add := func(rt Route) {
		if err := a.reg.Add(rt); err != nil {
			errs = append(errs, err)
		}
	}

	add(a.healthRoute())
	add(a.metaRoute())
	for _, rt := range a.authRoutes() {
		add(rt)
	}
	for _, rt := range a.setupRoutes() {
		add(rt)
	}
	for _, rt := range a.instanceRoutes() {
		add(rt)
	}
	if a.cfg.Events != nil {
		add(a.eventsRoute())
	}

	return errors.Join(errs...)
}

// build assembles the mux and the global chain.
//
// The chain has two halves and the split is deliberate:
//
//   - The GLOBAL half (request log, panic recovery, the D43 conformance
//     checker) wraps the mux, so it covers a request that matched no route at
//     all — a 404 is worth logging, and a panic in the fallback handler must
//     not kill the daemon either.
//   - The PER-ROUTE half (session gate, CSRF, rate limit, idempotency) wraps
//     each handler INSIDE the mux, in the order DESIGN section 1 lists them.
//     Wrapping per route rather than globally is what lets each layer read the
//     route's own declaration — its Auth column, its rate-limit policy,
//     whether it accepts an Idempotency-Key — instead of re-deriving the match
//     from the path, which is the duplication that lets a routing table and a
//     middleware disagree about which endpoint is public.
func (a *API) build() (http.Handler, error) {
	mux := http.NewServeMux()

	// pathMux carries one method-less entry per distinct pattern. It is how a
	// request that reached the fallback is told apart into 404 and 405: Go's
	// ServeMux answers 405 itself only when nothing else matches, and
	// registering a "/" fallback (which we must, to serve the SPA and to write
	// the error envelope) takes that answer away. Asking our own table instead
	// keeps the routing table the single source of truth for both statuses.
	pathMux := http.NewServeMux()
	allowed := map[string][]string{}

	for _, rt := range a.reg.Sorted() {
		allowed[rt.Pattern] = append(allowed[rt.Pattern], rt.Method)
	}
	for pattern, methods := range allowed {
		allow := strings.Join(methods, ", ")
		if err := handleSafely(pathMux, pattern, http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Allow", allow)
			})); err != nil {
			return nil, err
		}
	}

	for _, rt := range a.reg.Routes() {
		h, err := a.wrap(rt)
		if err != nil {
			return nil, err
		}
		if err := handleSafely(mux, rt.Method+" "+rt.Pattern, h); err != nil {
			return nil, err
		}
	}

	if err := handleSafely(mux, "/", a.fallback(pathMux)); err != nil {
		return nil, err
	}

	var top http.Handler = mux
	if a.cfg.Conformance != nil {
		top = a.cfg.Conformance(top)
	}

	global := middleware.Chain{
		middleware.RequestLog(a.log, a.now),
		middleware.Recover(a.log),
	}
	return global.Then(top), nil
}

// wrap applies the per-route chain, in DESIGN section 1's order: session, CSRF,
// rate limit, idempotency.
//
// Session first is not arbitrary. The setup gate lives inside it, and section 3
// says the SPA "routes to the wizard on that code alone" — so an unclaimed host
// must answer `409 setup_required` to a session route before anything else has
// a chance to answer something that means something different. CSRF second
// because it needs the session the gate resolved. Rate limit third because
// section 3 gives 429 exactly one meaning, on one authenticated endpoint, so
// there is nothing for it to protect ahead of authentication. Idempotency last
// because it hands its key straight to the handler.
func (a *API) wrap(rt Route) (http.Handler, error) {
	if rt.Handler == nil {
		return nil, fmt.Errorf("route %s: no handler", rt.OperationID)
	}

	chain := middleware.Chain{}
	chain = chain.Append(middleware.SessionGate(rt.Auth, a.cfg.Auth))
	switch rt.Auth {
	case AuthSession:
		chain = chain.Append(middleware.CSRF(a.cfg.Auth))
	case AuthSetup:
		// The same fetch-metadata checks, minus the double-submit half, which
		// has no session to key on. D38's loopback-or-token rule alone cannot
		// tell the operator's own browser from a hostile page driving it: a
		// cross-origin `no-cors` POST arrives from 127.0.0.1 like any other and
		// would claim the host with the attacker's password.
		chain = chain.Append(middleware.FetchMetadata())
	}
	chain = chain.Append(middleware.RateLimiter(rt.RateLimit, a.now))
	if rt.Idempotent {
		chain = chain.Append(middleware.IdempotencyKeyExtractor())
	}

	h := chain.Then(rt.Handler)

	// The operation id goes into the context OUTSIDE every layer above, so a
	// 401 written by the gate is still logged and conformance-checked against
	// the route it was aimed at.
	op := rt.OperationID
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(middleware.WithRoute(r.Context(), op)))
	}), nil
}

// fallback answers everything the route table did not match: `405` for a path
// that exists under other methods, the SPA for a browser navigation, and `404`
// in the error envelope for everything else.
func (a *API) fallback(pathMux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h, pattern := pathMux.Handler(r); pattern != "" {
			// The path is a route; the method is not. The pathMux handler sets
			// Allow and writes nothing, so the envelope goes out after it.
			h.ServeHTTP(w, r)
			middleware.WriteError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed,
				r.Method+" is not allowed on this path", nil)
			return
		}

		if a.cfg.Fallback != nil && !isAPIPath(r.URL.Path) {
			a.cfg.Fallback.ServeHTTP(w, r)
			return
		}

		middleware.WriteError(w, http.StatusNotFound, CodeNotFound, "not found", nil)
	})
}

// isAPIPath reports whether a path belongs to this API's namespace. A miss
// inside it is a 404 in the error envelope; a miss outside it is a client-side
// route the SPA shell should answer, because a browser reloading
// `/instances/01J…` must get the app rather than a JSON 404.
func isAPIPath(p string) bool {
	return p == "/healthz" || p == BasePath || len(p) > len(BasePath) && p[:len(BasePath)+1] == BasePath+"/"
}

// BasePath is the API's prefix (DESIGN section 3: "Base path /api/v1").
const BasePath = "/api/v1"

// handleSafely registers a pattern and converts the ServeMux registration panic
// — which section 3.6 warns takes the daemon down at boot — into an error.
// Registry.Add already rejects the shapes that cause it; this is the second
// half of that guarantee, and the reason the CI test of section 3.6 can assert
// "constructing the real mux from the registry never panics" against a code
// path that is also what production runs.
func handleSafely(mux *http.ServeMux, pattern string, h http.Handler) (err error) {
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("registering %q panicked: %v", pattern, v)
		}
	}()
	mux.Handle(pattern, h)
	return nil
}
