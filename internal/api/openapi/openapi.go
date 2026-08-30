package openapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"github.com/jlbyh2o/llamaman/internal/api"
	"github.com/jlbyh2o/llamaman/internal/api/middleware"
	"github.com/jlbyh2o/llamaman/internal/model"
)

// Version is the OpenAPI version the generated document declares. 3.1.0 is
// chosen over 3.0.x because its schema dialect IS JSON Schema 2020-12, which is
// what makes `"type": ["string","null"]` — the shape every nullable column in
// section 2 produces — expressible without the 3.0 `nullable:` extension that
// openapi-typescript handles inconsistently.
const Version = "3.1.0"

// Info is the document header.
type Info struct {
	Title       string
	Version     string
	Description string
}

// DefaultInfo is the header the committed api/openapi.json carries. The
// document's own `version` is deliberately NOT buildinfo.Version: the file is
// committed and drift-checked, so stamping a build version into it would make
// every build a diff and the check would fail on every developer machine.
func DefaultInfo() Info {
	return Info{
		Title:   "Llama Man",
		Version: "1",
		Description: "The Llama Man management API. Generated from the route registry in " +
			"internal/api; do not hand-edit (DESIGN section 3, D43).",
	}
}

// Generate builds the OpenAPI document for routes.
//
// The result is a map rather than a struct tree because encoding/json sorts map
// keys, which is exactly the determinism the drift check needs: two runs over
// the same registry produce byte-identical output regardless of the order
// routes were registered in.
func Generate(routes []api.Route, info Info) (map[string]any, error) {
	sorted := make([]api.Route, len(routes))
	copy(sorted, routes)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Pattern != sorted[j].Pattern {
			return api.OpenAPIPath(sorted[i].Pattern) < api.OpenAPIPath(sorted[j].Pattern)
		}
		return sorted[i].Method < sorted[j].Method
	})

	sch := newSchemas()

	// The error envelope is registered first and unconditionally: every route
	// can answer with it, and registering it up front gives it the unqualified
	// name `ErrorEnvelope` before anything else can claim it.
	envelopeRef, err := sch.ref(model.ErrorEnvelope{})
	if err != nil {
		return nil, err
	}

	paths := map[string]any{}
	tags := map[string]struct{}{}

	for _, rt := range sorted {
		op, err := operation(sch, envelopeRef, rt)
		if err != nil {
			return nil, fmt.Errorf("route %s: %w", rt.OperationID, err)
		}
		p := api.OpenAPIPath(rt.Pattern)
		item, _ := paths[p].(map[string]any)
		if item == nil {
			item = map[string]any{}
			paths[p] = item
		}
		method := lower(rt.Method)
		if _, dup := item[method]; dup {
			return nil, fmt.Errorf("openapi: %s %s is documented twice", rt.Method, p)
		}
		item[method] = op
		if rt.Tag != "" {
			tags[rt.Tag] = struct{}{}
		}
	}

	tagList := make([]any, 0, len(tags))
	names := make([]string, 0, len(tags))
	for t := range tags {
		names = append(names, t)
	}
	sort.Strings(names)
	for _, n := range names {
		tagList = append(tagList, map[string]any{"name": n})
	}

	components := map[string]any{
		"schemas":         schemaMap(sch),
		"securitySchemes": securitySchemes(),
	}

	return map[string]any{
		"openapi": Version,
		"info": map[string]any{
			"title":       info.Title,
			"version":     info.Version,
			"description": info.Description,
		},
		"tags":       tagList,
		"paths":      paths,
		"components": components,
	}, nil
}

func schemaMap(s *schemas) map[string]any {
	out := make(map[string]any, len(s.byName))
	for k, v := range s.byName {
		out[k] = v
	}
	return out
}

// securitySchemes describes the three credentials DESIGN section 3 defines.
// `token` — the gateway ports — is deliberately absent: section 3.15 says those
// listeners "are not this API", and the route registry refuses that Auth value.
func securitySchemes() map[string]any {
	return map[string]any{
		"sessionCookie": map[string]any{
			"type": "apiKey", "in": "cookie", "name": middleware.CookieSession,
			"description": "`<session_id>.<secret>`; HttpOnly, SameSite=Lax, Path=/, " +
				"Secure only over TLS. Only sha256(secret) is stored.",
		},
		"csrfToken": map[string]any{
			"type": "apiKey", "in": "header", "name": middleware.HeaderCSRF,
			"description": "Double-submit echo of the non-HttpOnly `" + middleware.CookieCSRF +
				"` cookie. Required on every non-GET of a session endpoint.",
		},
		"setupToken": map[string]any{
			"type": "apiKey", "in": "header", "name": middleware.HeaderSetupToken,
			"description": "The one-time claim token, required from every non-loopback " +
				"caller until the admin account exists (D38).",
		},
	}
}

func operation(sch *schemas, envelopeRef map[string]any, rt api.Route) (map[string]any, error) {
	op := map[string]any{
		"operationId": rt.OperationID,
		"summary":     rt.Summary,
		"responses":   map[string]any{},
	}
	if rt.Tag != "" {
		op["tags"] = []any{rt.Tag}
	}

	var params []any
	for _, p := range api.PathParams(rt.Pattern) {
		desc := "Path segment."
		if p.Multi {
			// OpenAPI has no multi-segment path parameter, so the fact that
			// this one carries slashes has to be said in prose — section 3.6's
			// whole point is that a repo id is `owner/name` and stays
			// unescaped in the URL.
			desc = "May contain `/` — this is a multi-segment path parameter " +
				"(a Hugging Face repo id such as `bartowski/Qwen3-8B-GGUF`), " +
				"sent unescaped."
		}
		params = append(params, map[string]any{
			"name": p.Name, "in": "path", "required": true,
			"description": desc,
			"schema":      map[string]any{"type": "string"},
		})
	}
	for _, q := range rt.Query {
		schema := map[string]any{"type": orString(q.Type)}
		if len(q.Enum) > 0 {
			enum := make([]any, len(q.Enum))
			for i, v := range q.Enum {
				enum[i] = v
			}
			schema["enum"] = enum
		}
		params = append(params, map[string]any{
			"name": q.Name, "in": "query", "required": q.Required,
			"description": q.Description,
			"schema":      schema,
		})
	}
	if rt.Idempotent {
		params = append(params, map[string]any{
			"name": middleware.HeaderIdempotencyKey, "in": "header", "required": false,
			"description": "Optional replay key. A repeat within 10 minutes returns the " +
				"original job with 200 instead of creating a second one; the same key with a " +
				"different body inside the window is 422 idempotency_key_reused (D39/D65).",
			"schema": map[string]any{
				"type": "string", "maxLength": middleware.MaxIdempotencyKeyLen,
			},
		})
	}
	if len(params) > 0 {
		op["parameters"] = params
	}

	switch rt.Auth {
	case api.AuthPublic:
		op["security"] = []any{}
	case api.AuthSession:
		sec := map[string]any{"sessionCookie": []any{}}
		if rt.Method != http.MethodGet && rt.Method != http.MethodHead {
			sec["csrfToken"] = []any{}
		}
		op["security"] = []any{sec}
	case api.AuthSetup:
		// Loopback needs no credential at all (D38), so the empty requirement
		// is listed alongside the token one rather than instead of it.
		op["security"] = []any{
			map[string]any{},
			map[string]any{"setupToken": []any{}},
		}
	}

	if rt.RequestBody != nil {
		ref, err := sch.ref(rt.RequestBody)
		if err != nil {
			return nil, err
		}
		op["requestBody"] = map[string]any{
			"required": true,
			"content":  map[string]any{"application/json": map[string]any{"schema": ref}},
		}
	}

	responses := op["responses"].(map[string]any)
	success, err := responseObject(sch, envelopeRef, rt.Success)
	if err != nil {
		return nil, err
	}
	responses[strconv.Itoa(rt.Success.Status)] = success

	for _, e := range rt.Errors {
		body, err := responseObject(sch, envelopeRef, e)
		if err != nil {
			return nil, err
		}
		key := strconv.Itoa(e.Status)
		if _, dup := responses[key]; dup {
			return nil, fmt.Errorf("status %d is documented twice", e.Status)
		}
		responses[key] = body
	}

	// The statuses every route can answer with, added only where the route did
	// not already document them. They are not decoration: the conformance
	// checker fails an UNDOCUMENTED status, and a session route really can
	// answer 401, 403 and 409 from the middleware chain without its handler
	// ever running.
	for _, common := range commonErrors(rt) {
		key := strconv.Itoa(common.Status)
		if _, present := responses[key]; present {
			continue
		}
		body, err := responseObject(sch, envelopeRef, common)
		if err != nil {
			return nil, err
		}
		responses[key] = body
	}

	return op, nil
}

// commonErrors are the statuses the middleware chain itself can produce for a
// route, derived from the route's own declaration rather than pasted onto every
// operation: a public route cannot answer 401, and a GET cannot fail CSRF.
func commonErrors(rt api.Route) []api.Response {
	out := []api.Response{{
		Status:      http.StatusInternalServerError,
		Description: "An unhandled error. The message is a constant; the detail is in the journal.",
		Codes:       []model.ErrorCode{api.CodeInternalError},
	}}

	switch rt.Auth {
	case api.AuthSession:
		out = append(out,
			api.Response{
				Status:      http.StatusUnauthorized,
				Description: "No valid admin session accompanied the request.",
				Codes:       []model.ErrorCode{middleware.CodeUnauthorized},
			},
			api.Response{
				Status: http.StatusConflict,
				Description: "This host has not been claimed yet. The SPA routes to the wizard " +
					"on this code alone.",
				Codes: []model.ErrorCode{middleware.CodeSetupRequired},
			},
		)
		if rt.Method != http.MethodGet && rt.Method != http.MethodHead {
			out = append(out, api.Response{
				Status:      http.StatusForbidden,
				Description: "The CSRF double-submit, Origin or Sec-Fetch-Site check failed.",
				Codes:       []model.ErrorCode{middleware.CodeCSRFFailed},
			})
		}
	case api.AuthSetup:
		forbidden := api.Response{
			Status:      http.StatusForbidden,
			Description: "A non-loopback caller presented no valid setup token (D38).",
			Codes:       []model.ErrorCode{middleware.CodeSetupTokenRequired},
		}
		if rt.Method != http.MethodGet && rt.Method != http.MethodHead {
			// The fetch-metadata half of the CSRF check runs here too: a
			// `setup` route has no session to double-submit against, and the
			// loopback rule cannot tell the operator's browser from a page
			// driving it.
			forbidden.Description = "A non-loopback caller presented no valid setup token (D38), " +
				"or the Origin/Sec-Fetch-Site check said cross-site."
			forbidden.Codes = append(forbidden.Codes, middleware.CodeCSRFFailed)
		}
		out = append(out, forbidden, api.Response{
			Status:      http.StatusConflict,
			Description: "This host has already been claimed; the setup window is closed.",
			Codes:       []model.ErrorCode{middleware.CodeSetupAlreadyClaimed},
		})
		if rt.Method != http.MethodGet && rt.Method != http.MethodHead {
			out = append(out, api.Response{
				Status: http.StatusUnsupportedMediaType,
				Description: "The request body carried no `Content-Type: application/json`, " +
					"which is the shape a cross-origin request built to avoid a preflight has.",
				Codes: []model.ErrorCode{middleware.CodeUnsupportedMediaType},
			})
		}
	}

	if rt.Idempotent {
		out = append(out, api.Response{
			Status:      http.StatusBadRequest,
			Description: "The Idempotency-Key header is not a short printable ASCII token.",
			Codes:       []model.ErrorCode{middleware.CodeIdempotencyKeyInvalid},
		})
	}
	if rt.RateLimit != nil {
		out = append(out, api.Response{
			Status:      http.StatusTooManyRequests,
			Description: "Rate limited; `details.retry_after_ms` says for how long.",
			Codes:       []model.ErrorCode{rt.RateLimit.Code},
		})
	}
	return out
}

func responseObject(sch *schemas, envelopeRef map[string]any, r api.Response) (map[string]any, error) {
	out := map[string]any{"description": r.Description}

	switch {
	case r.Status >= 400:
		// Every non-2xx is the one envelope of section 3. The codes this
		// status can carry are listed beside it as an extension rather than
		// folded into the schema: narrowing `error.code` with an allOf against
		// a schema that already says `additionalProperties: false` is the kind
		// of JSON Schema construction validators disagree about, and the value
		// here is that the closed set is machine-readable — which an extension
		// gives openapi-typescript and the conformance checker alike.
		out["content"] = map[string]any{
			"application/json": map[string]any{"schema": envelopeRef},
		}
		if len(r.Codes) > 0 {
			codes := make([]string, len(r.Codes))
			for i, c := range r.Codes {
				codes[i] = string(c)
			}
			sort.Strings(codes)
			asAny := make([]any, len(codes))
			for i, c := range codes {
				asAny[i] = c
			}
			out["x-error-codes"] = asAny
		}

	case r.Body != nil:
		ref, err := sch.ref(r.Body)
		if err != nil {
			return nil, err
		}
		out["content"] = map[string]any{
			orString2(r.MediaType, "application/json"): map[string]any{"schema": ref},
		}

	case r.MediaType != "":
		out["content"] = map[string]any{
			r.MediaType: map[string]any{"schema": map[string]any{"type": "string"}},
		}
	}

	return out, nil
}

// Marshal renders a document as the exact bytes api/openapi.json carries:
// two-space indent, HTML escaping off (an `&` in a description must not become
// `&`), and a trailing newline so the file ends the way every other text
// file in the repo does.
func Marshal(doc map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// WriteFile generates the document for routes and writes it to path, creating
// the parent directory if needed. It is what `make openapi` and the drift
// check's `-update` flag call.
func WriteFile(path string, routes []api.Route, info Info) error {
	doc, err := Generate(routes, info)
	if err != nil {
		return err
	}
	b, err := Marshal(doc)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func lower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

func orString(s string) string {
	if s == "" {
		return "string"
	}
	return s
}

func orString2(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
