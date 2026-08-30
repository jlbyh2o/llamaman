package openapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/jlbyh2o/llamaman/internal/api"
	"github.com/jlbyh2o/llamaman/internal/api/middleware"
)

// Checker is the D43 response-conformance middleware: "an undocumented
// endpoint, a missing documented field, or an extra field fails the suite".
//
// It is wired ONLY in the integration suite (api.Config.Conformance), never in
// production. Two reasons, and the second is the important one: it buffers
// every response body in order to inspect it, which would defeat the streaming
// the gateway and the SSE endpoint are built around; and a contract check that
// ran in production would turn a documentation mistake into a user-visible
// outage, which is the opposite of what a contract check is for.
//
// # What is implemented, and what is stubbed
//
// Implemented here: the endpoint must be documented, the status it answered
// with must be documented for that operation, a non-2xx body must be section
// 3's error envelope carrying a code the document lists for that status, and a
// JSON response body must match the documented schema's required/extra field
// sets at the TOP LEVEL.
//
// Deliberately stubbed, pending the endpoints that need it: recursive
// validation into nested objects and arrays, `format` checks (RFC 3339,
// int64), and enum membership on non-error fields. Those become worth writing
// when there is a DTO with nesting to validate — today the live endpoints are
// three flat objects — and writing them now would be a validator nothing
// exercises. Violations() reports what the checker actually looked at, so a
// test can assert coverage rather than assume it.
type Checker struct {
	doc map[string]any

	// Violation is called for each failure. A test wires it to t.Errorf. Nil
	// collects into Violations() instead.
	Violation func(error)

	violations []error
}

// NewChecker builds a checker over a generated document.
func NewChecker(doc map[string]any) *Checker {
	return &Checker{doc: doc}
}

// NewCheckerForRoutes generates the document for routes and returns a checker
// over it. This is the constructor an integration test wants: it checks the
// handlers against the document the registry PRODUCES, which — together with
// the drift check that the committed file equals that same document — is what
// closes the loop D43 describes.
func NewCheckerForRoutes(routes []api.Route, info Info) (*Checker, error) {
	doc, err := Generate(routes, info)
	if err != nil {
		return nil, err
	}
	return NewChecker(doc), nil
}

// Violations returns the failures collected so far, when no Violation callback
// was set.
func (c *Checker) Violations() []error { return c.violations }

func (c *Checker) fail(format string, args ...any) {
	err := fmt.Errorf(format, args...)
	if c.Violation != nil {
		c.Violation(err)
		return
	}
	c.violations = append(c.violations, err)
}

// Middleware wraps the API's mux. It is api.Config.Conformance's shape.
func (c *Checker) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := &captureWriter{ResponseWriter: w, status: http.StatusOK}
		// The matched route is set inside the mux, below this layer; the slot
		// is how it travels back out (middleware.WithRouteSlot). Installing it
		// here as well as in the request log means a checker mounted on its own
		// — which is how a focused test wires it — still learns the operation.
		r = r.WithContext(middleware.WithRouteSlot(r.Context()))
		next.ServeHTTP(buf, r)
		buf.flush()
		c.check(r, buf)
	})
}

func (c *Checker) check(r *http.Request, buf *captureWriter) {
	op, ok := middleware.RouteFrom(r.Context())
	if !ok {
		// No matched route. A 404 or 405 from the fallback is expected and is
		// not an undocumented endpoint; anything else means a handler ran
		// outside the registry, which is exactly the "undocumented endpoint"
		// D43 names.
		if buf.status != http.StatusNotFound && buf.status != http.StatusMethodNotAllowed {
			c.fail("undocumented endpoint: %s %s answered %d outside the route registry",
				r.Method, r.URL.Path, buf.status)
		}
		return
	}

	responses := c.responsesFor(op)
	if responses == nil {
		c.fail("undocumented endpoint: operation %q is not in the document", op)
		return
	}

	documented, present := responses[strconv.Itoa(buf.status)].(map[string]any)
	if !present {
		c.fail("operation %q answered %d, which the document does not list (documented: %s)",
			op, buf.status, strings.Join(sortedKeys(responses), ", "))
		return
	}

	if buf.status == http.StatusNoContent {
		if buf.body.Len() > 0 {
			c.fail("operation %q answered 204 with a %d-byte body", op, buf.body.Len())
		}
		return
	}

	ct := buf.Header().Get("Content-Type")
	mediaType := strings.TrimSpace(strings.SplitN(ct, ";", 2)[0])

	content, _ := documented["content"].(map[string]any)
	if content == nil {
		return // a documented response with no body to check
	}
	if _, served := content[mediaType]; !served {
		c.fail("operation %q answered %d as %q; the document lists %s",
			op, buf.status, mediaType, strings.Join(sortedKeys(content), ", "))
		return
	}
	if mediaType != "application/json" {
		return // streams and rendered HTML are not schema-checked
	}

	var body map[string]any
	if err := json.Unmarshal(buf.body.Bytes(), &body); err != nil {
		c.fail("operation %q answered %d with a body that is not a JSON object: %v",
			op, buf.status, err)
		return
	}

	if buf.status >= 400 {
		c.checkErrorEnvelope(op, buf.status, documented, body)
		return
	}

	schema := resolve(c.doc, content["application/json"])
	c.checkObject(op, buf.status, schema, body)
}

// checkErrorEnvelope asserts section 3's one error shape, and that the code is
// one the document lists for this status.
func (c *Checker) checkErrorEnvelope(op string, status int, documented, body map[string]any) {
	inner, ok := body["error"].(map[string]any)
	if !ok {
		c.fail("operation %q answered %d without the {\"error\":{…}} envelope", op, status)
		return
	}
	code, _ := inner["code"].(string)
	if code == "" {
		c.fail("operation %q answered %d with no error.code", op, status)
		return
	}
	if _, ok := inner["message"].(string); !ok {
		c.fail("operation %q answered %d with no error.message", op, status)
	}
	for k := range inner {
		switch k {
		case "code", "message", "details":
		default:
			c.fail("operation %q answered %d with an extra field error.%s", op, status, k)
		}
	}

	allowed := stringsOf(documented["x-error-codes"])
	if len(allowed) == 0 {
		return // the document did not close the set for this status
	}
	for _, a := range allowed {
		if a == code {
			return
		}
	}
	c.fail("operation %q answered %d with code %q, which the document does not list for that status",
		op, status, code)
}

// checkObject compares the top level of a response body against its schema:
// every required property present, no property the schema does not declare.
func (c *Checker) checkObject(op string, status int, schema, body map[string]any) {
	if schema == nil {
		return
	}
	props, _ := schema["properties"].(map[string]any)
	if props == nil {
		return
	}

	for _, name := range stringsOf(schema["required"]) {
		if _, present := body[name]; !present {
			c.fail("operation %q answered %d without the documented field %q",
				op, status, name)
		}
	}

	if extra, ok := schema["additionalProperties"].(bool); ok && !extra {
		for name := range body {
			if _, declared := props[name]; !declared {
				c.fail("operation %q answered %d with an undocumented field %q",
					op, status, name)
			}
		}
	}
}

// responsesFor finds an operation's documented responses by operationId. The
// document is walked rather than indexed because the id is the stable name and
// the path is not: a route's pattern can be rewritten (section 3.6 moved a verb
// in front of a wildcard for exactly that reason) without its operation id
// changing.
func (c *Checker) responsesFor(operationID string) map[string]any {
	paths, _ := c.doc["paths"].(map[string]any)
	for _, item := range paths {
		methods, _ := item.(map[string]any)
		for _, opAny := range methods {
			op, _ := opAny.(map[string]any)
			if id, _ := op["operationId"].(string); id == operationID {
				r, _ := op["responses"].(map[string]any)
				return r
			}
		}
	}
	return nil
}

// resolve follows a media-type object's `schema`, dereferencing a
// `#/components/schemas/Name` pointer.
func resolve(doc map[string]any, mediaAny any) map[string]any {
	media, _ := mediaAny.(map[string]any)
	if media == nil {
		return nil
	}
	schema, _ := media["schema"].(map[string]any)
	if schema == nil {
		return nil
	}
	ref, _ := schema["$ref"].(string)
	if ref == "" {
		return schema
	}
	const prefix = "#/components/schemas/"
	if !strings.HasPrefix(ref, prefix) {
		return nil
	}
	components, _ := doc["components"].(map[string]any)
	all, _ := components["schemas"].(map[string]any)
	out, _ := all[strings.TrimPrefix(ref, prefix)].(map[string]any)
	return out
}

// stringsOf reads a string list out of a document, tolerating both forms it can
// arrive in: []string when the document is the in-memory map Generate produced
// (which is what an integration test checks against, so no marshal/unmarshal
// round trip has happened), and []any when it was parsed from the committed
// JSON file. A checker that handled only one of them would silently pass every
// required-field check in one of the two configurations.
func stringsOf(v any) []string {
	switch vs := v.(type) {
	case []string:
		return vs
	case []any:
		out := make([]string, 0, len(vs))
		for _, e := range vs {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// captureWriter buffers a response so the checker can read the body after the
// handler has finished, then writes it through.
//
// It does NOT implement Unwrap, deliberately: an SSE stream flushed through
// this wrapper would be buffered until the handler returned, which for a
// long-lived stream is forever. A test that wants to exercise the SSE endpoint
// runs without the checker, which is the same reason the checker is not wired
// in production.
type captureWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	body        bytes.Buffer
	flushed     bool
}

func (c *captureWriter) WriteHeader(status int) {
	if c.wroteHeader {
		return
	}
	c.wroteHeader = true
	c.status = status
}

func (c *captureWriter) Write(b []byte) (int, error) {
	if !c.wroteHeader {
		c.WriteHeader(http.StatusOK)
	}
	return c.body.Write(b)
}

func (c *captureWriter) flush() {
	if c.flushed {
		return
	}
	c.flushed = true
	c.ResponseWriter.WriteHeader(c.status)
	_, _ = c.ResponseWriter.Write(c.body.Bytes())
}
