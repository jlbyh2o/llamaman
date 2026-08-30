package api

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/jlbyh2o/llamaman/internal/api/middleware"
	"github.com/jlbyh2o/llamaman/internal/model"
)

// Auth is section 3's Auth column. It is an alias rather than a second type so
// that a route declares its authorization in the same vocabulary the gate reads
// it in, with no conversion that could quietly lose a value.
type Auth = middleware.Auth

// The four values of section 3's Auth column, re-exported so a route table
// reads as `api.AuthSession` rather than reaching into the middleware package.
const (
	AuthPublic  = middleware.AuthPublic
	AuthSession = middleware.AuthSession
	AuthSetup   = middleware.AuthSetup
	AuthToken   = middleware.AuthToken
)

// Response documents one status a route can answer with. The registry carries
// these because api/openapi.json is GENERATED from the route registry (D43) —
// the document is a projection of this table, never a parallel description of
// it, which is what makes the CI drift check meaningful.
type Response struct {
	// Status is the HTTP status.
	Status int
	// Description is the one-line explanation the document shows.
	Description string
	// Body is a zero value of the DTO this status returns; nil means no body
	// (204, or a 200 whose body is the error envelope for an error status).
	// The OpenAPI generator reflects over it.
	Body any
	// Codes lists the model.ErrorCode values this status can carry, for an
	// error response. They become the documented enum of `error.code`, which
	// is how the closed enum of section 3 reaches the generated TypeScript.
	Codes []model.ErrorCode
	// MediaType overrides application/json for a response that is not JSON —
	// the SSE stream, and the sanitized HTML of `GET /hf/card/{repo...}`.
	MediaType string
	// AltMediaTypes are the OTHER forms this same status can take, chosen by
	// the request's `Accept` header rather than by a different status.
	//
	// Two endpoints in section 3 are written that way and both say so in the
	// same words: `GET /llamacpp/versions/{id}/log` is "plain text; SSE for a
	// live tail", and `GET /system/journal` is "JSON lines, or SSE when
	// `Accept: text/event-stream`". A second Response with the same status
	// could not express it — a status appears once in an OpenAPI operation — so
	// the alternatives are listed here and become extra `content` entries.
	AltMediaTypes []string
}

// QueryParam documents one `?key=` a route reads. The registry carries these
// for the same reason it carries Response: openapi.json is a projection of this
// table, so a filter that is not declared here does not exist as far as the
// generated TypeScript is concerned.
type QueryParam struct {
	Name        string
	Description string
	// Required marks a parameter the route refuses without.
	Required bool
	// Enum closes the value set, when there is one.
	Enum []string
	// Type is the JSON Schema type; empty means "string".
	Type string
}

// Route is one row of an endpoint table in DESIGN section 3, in code. The
// registry of these is the single source of truth for three things that must
// never disagree: what the ServeMux serves, what api/openapi.json documents,
// and which middleware each request runs through.
type Route struct {
	// Method is one of the standard HTTP methods.
	Method string
	// Pattern is the path, in Go 1.22 ServeMux syntax, INCLUDING wildcards:
	// `/api/v1/models/{id}`, `/api/v1/hf/tree/{repo...}`.
	Pattern string
	// Auth is the route's row in section 3's Auth column.
	Auth Auth
	// OperationID is the stable name of this operation. It is the OpenAPI
	// operationId, the name the request log prints, and the key the
	// response-conformance checker looks its documented responses up by, so it
	// must be unique across the registry.
	OperationID string
	// Summary is the one-line description, taken from the Notes column of the
	// section 3 table this route transcribes.
	Summary string
	// Tag groups the route in the generated document; use the section name
	// ("system", "instances", "hf", …).
	Tag string
	// Handler serves the request, after the per-route chain.
	Handler http.Handler

	// Idempotent marks a job-creating POST that accepts an `Idempotency-Key`
	// header (D39/D65). It mounts the extractor layer and documents the header
	// as a parameter.
	Idempotent bool
	// RateLimit is the per-route 429 policy. Almost every route leaves it nil:
	// section 3 gives 429 exactly one meaning, and inventing a second would
	// break the contract the conformance middleware checks.
	RateLimit *middleware.RateLimit

	// Query documents the `?key=` parameters this route reads.
	Query []QueryParam
	// RequestBody is a zero value of the DTO this route accepts; nil for a
	// route with no body.
	RequestBody any
	// Success is the 2xx this route answers with.
	Success Response
	// Errors are the documented non-2xx answers, beyond the ones every route
	// shares.
	Errors []Response
}

// Registry is the route table. It is built once, at construction, and read
// afterwards by three consumers: the mux builder, the OpenAPI generator and the
// tests that assert the table itself.
type Registry struct {
	routes []Route
	byKey  map[string]struct{}
	byOp   map[string]struct{}
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{byKey: map[string]struct{}{}, byOp: map[string]struct{}{}}
}

// Add validates and appends a route.
//
// Validation is not a formality here. Go's ServeMux PANICS AT REGISTRATION on a
// multi-segment wildcard that is not the final element of the pattern — section
// 3.6 spells this out, because a repo id contains a `/` and so
// `/api/v1/hf/models/{repo...}/tree` would take the daemon down at boot rather
// than 404 at request time. Catching it here turns that class of mistake into
// an error a test reads, and section 3.6's CI test additionally builds the real
// ServeMux inside a recover() so the two checks cannot both be wrong.
func (r *Registry) Add(rt Route) error {
	if rt.Method == "" {
		return fmt.Errorf("route %q: no method", rt.Pattern)
	}
	if !knownMethod(rt.Method) {
		return fmt.Errorf("route %s %s: unknown method", rt.Method, rt.Pattern)
	}
	if rt.Handler == nil {
		return fmt.Errorf("route %s %s: no handler", rt.Method, rt.Pattern)
	}
	if rt.OperationID == "" {
		return fmt.Errorf("route %s %s: no operation id", rt.Method, rt.Pattern)
	}
	if !rt.Auth.Valid() {
		return fmt.Errorf("route %s: invalid auth %q", rt.OperationID, rt.Auth)
	}
	if rt.Auth == AuthToken {
		// Section 3: "`token` = the gateway ports, which are not this API".
		// The gateway is its own listener with its own handler (section 3.15),
		// and a token-authenticated route on the management listener would be
		// a credential this API has no verifier for.
		return fmt.Errorf("route %s: `token` auth belongs to the gateway listeners, not this API", rt.OperationID)
	}
	if err := validatePattern(rt.Pattern); err != nil {
		return fmt.Errorf("route %s: %w", rt.OperationID, err)
	}
	if rt.Idempotent && rt.Method != http.MethodPost {
		return fmt.Errorf("route %s: only a job-creating POST accepts %s",
			rt.OperationID, middleware.HeaderIdempotencyKey)
	}
	if rt.RateLimit != nil && rt.RateLimit.Code == "" {
		return fmt.Errorf("route %s: a rate-limit policy must name the error code its 429 carries", rt.OperationID)
	}
	if rt.Success.Status < 200 || rt.Success.Status > 299 {
		return fmt.Errorf("route %s: Success.Status %d is not a 2xx", rt.OperationID, rt.Success.Status)
	}

	key := rt.Method + " " + rt.Pattern
	if _, dup := r.byKey[key]; dup {
		return fmt.Errorf("route %s: %s is already registered", rt.OperationID, key)
	}
	if _, dup := r.byOp[rt.OperationID]; dup {
		return fmt.Errorf("route %s: operation id is already registered", rt.OperationID)
	}
	r.byKey[key] = struct{}{}
	r.byOp[rt.OperationID] = struct{}{}
	r.routes = append(r.routes, rt)
	return nil
}

// Routes returns the registered routes in registration order.
func (r *Registry) Routes() []Route {
	out := make([]Route, len(r.routes))
	copy(out, r.routes)
	return out
}

// Sorted returns the routes ordered by pattern then method — the order the
// generated document uses, so that regenerating it after a route is added in a
// different place produces a minimal diff.
func (r *Registry) Sorted() []Route {
	out := r.Routes()
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Pattern != out[j].Pattern {
			return out[i].Pattern < out[j].Pattern
		}
		return methodOrder(out[i].Method) < methodOrder(out[j].Method)
	})
	return out
}

// Len reports how many routes are registered.
func (r *Registry) Len() int { return len(r.routes) }

func knownMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions:
		return true
	}
	return false
}

// methodOrder sorts methods the way an endpoint table reads: read, then create,
// then modify, then remove.
func methodOrder(m string) int {
	for i, v := range []string{
		http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions,
	} {
		if m == v {
			return i
		}
	}
	return 99
}

// validatePattern enforces the subset of ServeMux pattern syntax this API uses,
// and in particular the rule of section 3.6: a `{name...}` multi-segment
// wildcard may appear only as the FINAL segment.
func validatePattern(p string) error {
	if p == "" {
		return fmt.Errorf("empty pattern")
	}
	if !strings.HasPrefix(p, "/") {
		return fmt.Errorf("pattern %q must begin with '/' (this API registers no host patterns)", p)
	}
	if strings.Contains(p, "//") {
		return fmt.Errorf("pattern %q has an empty path segment", p)
	}

	segs := strings.Split(strings.TrimPrefix(p, "/"), "/")
	names := map[string]struct{}{}
	for i, seg := range segs {
		last := i == len(segs)-1
		open := strings.IndexByte(seg, '{')
		if open < 0 {
			if strings.ContainsAny(seg, "}") {
				return fmt.Errorf("pattern %q: unbalanced '}' in segment %q", p, seg)
			}
			continue
		}
		if open != 0 || !strings.HasSuffix(seg, "}") {
			// ServeMux only accepts a wildcard that is a WHOLE segment;
			// "pre{x}" and "{x}post" are literal text containing braces, which
			// is never what a route table means.
			return fmt.Errorf("pattern %q: a wildcard must be a whole path segment, got %q", p, seg)
		}
		name := seg[1 : len(seg)-1]
		if name == "$" {
			if !last {
				return fmt.Errorf("pattern %q: {$} may only be the final segment", p)
			}
			continue
		}
		multi := strings.HasSuffix(name, "...")
		if multi {
			name = strings.TrimSuffix(name, "...")
			if !last {
				// The rule section 3.6 exists for. Registering this against a
				// real ServeMux panics, at boot, in the composition root.
				return fmt.Errorf(
					"pattern %q: the multi-segment wildcard {%s...} must be the final segment — "+
						"ServeMux panics at registration otherwise (DESIGN section 3.6); "+
						"move the sub-resource in front of it, as /api/v1/hf/tree/{repo...} does",
					p, name)
			}
		}
		if name == "" {
			return fmt.Errorf("pattern %q: a wildcard needs a name", p)
		}
		if strings.ContainsAny(name, "{}/. ") {
			return fmt.Errorf("pattern %q: invalid wildcard name %q", p, name)
		}
		if _, dup := names[name]; dup {
			return fmt.Errorf("pattern %q: duplicate wildcard name %q", p, name)
		}
		names[name] = struct{}{}
	}
	return nil
}

// PathParams returns the wildcard names in a pattern, in order, and whether
// each one is multi-segment. The OpenAPI generator turns them into `in: path`
// parameters; the multi-segment flag is what its description has to mention,
// because an OpenAPI path parameter does not otherwise admit a `/`.
func PathParams(pattern string) []PathParam {
	var out []PathParam
	for _, seg := range strings.Split(strings.TrimPrefix(pattern, "/"), "/") {
		if len(seg) < 3 || seg[0] != '{' || seg[len(seg)-1] != '}' {
			continue
		}
		name := seg[1 : len(seg)-1]
		if name == "$" {
			continue
		}
		multi := strings.HasSuffix(name, "...")
		out = append(out, PathParam{Name: strings.TrimSuffix(name, "..."), Multi: multi})
	}
	return out
}

// PathParam is one wildcard of a route pattern.
type PathParam struct {
	Name  string
	Multi bool
}

// OpenAPIPath renders a route pattern as an OpenAPI path template:
// `/api/v1/hf/tree/{repo...}` becomes `/api/v1/hf/tree/{repo}`. OpenAPI has no
// multi-segment parameter, so the `...` is dropped and the parameter's
// description says the value may contain `/` — which is exactly what
// `ui/src/api/schema.d.ts` needs in order to type the call correctly.
func OpenAPIPath(pattern string) string {
	segs := strings.Split(pattern, "/")
	for i, seg := range segs {
		if len(seg) >= 3 && seg[0] == '{' && seg[len(seg)-1] == '}' {
			name := seg[1 : len(seg)-1]
			if name == "$" {
				segs[i] = ""
				continue
			}
			segs[i] = "{" + strings.TrimSuffix(name, "...") + "}"
		}
	}
	out := strings.Join(segs, "/")
	if len(out) > 1 && strings.HasSuffix(out, "/") {
		out = strings.TrimSuffix(out, "/")
	}
	return out
}
