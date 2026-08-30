package openapi

import (
	"bytes"
	"context"
	"flag"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/jlbyh2o/llamaman/internal/api"
	"github.com/jlbyh2o/llamaman/internal/api/middleware"
)

// update regenerates the committed document instead of comparing against it:
//
//	go test ./internal/api/openapi -update
//
// This is the generator's entry point. It is a test flag rather than a
// `go:generate` program because the routes are declared by api.New, which needs
// a Config — building one from a main package would mean a second, parallel
// description of the daemon's wiring, which is exactly what D43 exists to
// prevent.
var update = flag.Bool("update", false, "rewrite api/openapi.json from the route registry")

// specPath is the committed document, relative to this package.
const specPath = "../../../api/openapi.json"

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// documentedAPI builds the API exactly as the composition root does, minus the
// collaborators that only affect behavior: the document is a projection of the
// ROUTE TABLE, and every route must be present for it to be complete.
func documentedAPI(t *testing.T) *api.API {
	t.Helper()
	a, err := api.New(api.Config{
		Logger: quiet(),
		Auth:   middleware.Unconfigured{},
		Meta:   nilMeta{},
		Events: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
	})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	return a
}

type nilMeta struct{}

func (nilMeta) Meta(context.Context) (api.Meta, error) { return api.Meta{}, nil }

// TestGenerationRuns is the generation test: the document builds from the live
// route registry, and every route in the registry appears in it.
func TestGenerationRuns(t *testing.T) {
	t.Parallel()

	a := documentedAPI(t)
	doc, err := Generate(a.Routes(), DefaultInfo())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if doc["openapi"] != Version {
		t.Errorf("openapi = %v, want %s", doc["openapi"], Version)
	}

	paths, _ := doc["paths"].(map[string]any)
	if len(paths) != len(distinctPaths(a.Routes())) {
		t.Errorf("document has %d paths, the registry has %d distinct ones",
			len(paths), len(distinctPaths(a.Routes())))
	}

	for _, rt := range a.Routes() {
		p := api.OpenAPIPath(rt.Pattern)
		item, ok := paths[p].(map[string]any)
		if !ok {
			t.Errorf("path %q is missing from the document", p)
			continue
		}
		op, ok := item[lower(rt.Method)].(map[string]any)
		if !ok {
			t.Errorf("%s %s is missing from the document", rt.Method, p)
			continue
		}
		if op["operationId"] != rt.OperationID {
			t.Errorf("%s %s operationId = %v, want %q", rt.Method, p, op["operationId"], rt.OperationID)
		}
		responses, _ := op["responses"].(map[string]any)
		if _, ok := responses["500"]; !ok {
			t.Errorf("%s %s does not document a 500, which the recover layer can always produce",
				rt.Method, p)
		}
	}

	components, _ := doc["components"].(map[string]any)
	schemas, _ := components["schemas"].(map[string]any)
	if _, ok := schemas["ErrorEnvelope"]; !ok {
		t.Error("the document has no ErrorEnvelope schema")
	}
}

// TestGeneratedIsDeterministic: the drift check is only meaningful if two runs
// over the same registry produce byte-identical output.
func TestGeneratedIsDeterministic(t *testing.T) {
	t.Parallel()

	a := documentedAPI(t)
	first := marshalRoutes(t, a)
	for i := 0; i < 5; i++ {
		if got := marshalRoutes(t, a); !bytes.Equal(first, got) {
			t.Fatal("two generations of the same registry differ")
		}
	}
}

// TestCommittedSpecIsUpToDate is the D43 drift check: "openapi.json is
// generated from the route registry and drift-checked in CI". Run with -update
// to regenerate.
func TestCommittedSpecIsUpToDate(t *testing.T) {
	a := documentedAPI(t)
	want := marshalRoutes(t, a)

	if *update {
		if err := os.MkdirAll(filepath.Dir(specPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(specPath, want, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s (%d bytes)", specPath, len(want))
		return
	}

	got, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("reading the committed document: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is out of date with the route registry.\n"+
			"Regenerate it: go test ./internal/api/openapi -update", specPath)
	}
}

// TestMultiSegmentWildcardIsDocumentedAsSuch: OpenAPI has no multi-segment path
// parameter, so section 3.6's repo id has to be described in prose or a client
// generator will escape the `/` and break every HF call.
func TestMultiSegmentWildcardIsDocumentedAsSuch(t *testing.T) {
	t.Parallel()

	reg := api.NewRegistry()
	err := reg.Add(api.Route{
		Method: http.MethodGet, Pattern: "/api/v1/hf/tree/{repo...}", Auth: api.AuthSession,
		OperationID: "getHFTree", Tag: "hf",
		Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		Success: api.Response{Status: http.StatusOK, Description: "tree"},
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	doc, err := Generate(reg.Routes(), DefaultInfo())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	paths, _ := doc["paths"].(map[string]any)
	item, ok := paths["/api/v1/hf/tree/{repo}"].(map[string]any)
	if !ok {
		t.Fatalf("the multi-segment wildcard was not rendered as {repo}: %v", keys(paths))
	}
	op, _ := item["get"].(map[string]any)
	params, _ := op["parameters"].([]any)
	if len(params) != 1 {
		t.Fatalf("parameters = %v, want exactly the repo path parameter", params)
	}
	p, _ := params[0].(map[string]any)
	desc, _ := p["description"].(string)
	if !bytes.Contains([]byte(desc), []byte("May contain `/`")) {
		t.Errorf("repo parameter description = %q, want it to say the value may contain a slash", desc)
	}
}

// TestConformanceChecker exercises the three failures D43 names, plus the pass.
func TestConformanceChecker(t *testing.T) {
	t.Parallel()

	reg := api.NewRegistry()
	mustAdd(t, reg, api.Route{
		Method: http.MethodGet, Pattern: "/api/v1/thing", Auth: api.AuthPublic,
		OperationID: "getThing", Tag: "thing",
		Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		Success: api.Response{
			Status: http.StatusOK, Description: "a thing", Body: thingDTO{},
		},
	})

	doc, err := Generate(reg.Routes(), DefaultInfo())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	cases := []struct {
		name       string
		body       string
		status     int
		known      bool
		wantIssues int
	}{
		{"a conforming body", `{"name":"a","size":1}`, 200, true, 0},
		{"a missing documented field", `{"name":"a"}`, 200, true, 1},
		{"an extra field", `{"name":"a","size":1,"extra":true}`, 200, true, 1},
		{"an undocumented status", `{"name":"a","size":1}`, 201, true, 1},
		{"an undocumented endpoint", `{}`, 200, false, 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := NewChecker(doc)
			h := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))

			r := httptest.NewRequest(http.MethodGet, "/api/v1/thing", nil)
			if tc.known {
				r = r.WithContext(middleware.WithRoute(r.Context(), "getThing"))
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, r)

			if got := len(c.Violations()); got != tc.wantIssues {
				t.Fatalf("violations = %d (%v), want %d", got, c.Violations(), tc.wantIssues)
			}
			// The response must reach the client unchanged either way: the
			// checker inspects, it does not rewrite.
			if rr.Code != tc.status || rr.Body.String() != tc.body {
				t.Errorf("the checker altered the response: %d %q", rr.Code, rr.Body)
			}
		})
	}
}

func TestConformanceCheckerErrorEnvelope(t *testing.T) {
	t.Parallel()

	reg := api.NewRegistry()
	mustAdd(t, reg, api.Route{
		Method: http.MethodGet, Pattern: "/api/v1/thing", Auth: api.AuthSession,
		OperationID: "getThing", Tag: "thing",
		Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		Success: api.Response{Status: http.StatusOK, Description: "a thing"},
	})
	doc, err := Generate(reg.Routes(), DefaultInfo())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	cases := []struct {
		name       string
		body       string
		wantIssues int
	}{
		{"a documented code", `{"error":{"code":"unauthorized","message":"no"}}`, 0},
		{"a code this status does not carry", `{"error":{"code":"model_in_use","message":"no"}}`, 1},
		{"not the envelope", `{"message":"no"}`, 1},
		{"an extra field inside error", `{"error":{"code":"unauthorized","message":"no","hint":"x"}}`, 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := NewChecker(doc)
			h := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, tc.body)
			}))
			r := httptest.NewRequest(http.MethodGet, "/api/v1/thing", nil).
				WithContext(middleware.WithRoute(context.Background(), "getThing"))
			h.ServeHTTP(httptest.NewRecorder(), r)
			if got := len(c.Violations()); got != tc.wantIssues {
				t.Fatalf("violations = %d (%v), want %d", got, c.Violations(), tc.wantIssues)
			}
		})
	}
}

func marshalRoutes(t *testing.T, a *api.API) []byte {
	t.Helper()
	doc, err := Generate(a.Routes(), DefaultInfo())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	b, err := Marshal(doc)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return b
}

func mustAdd(t *testing.T, reg *api.Registry, rt api.Route) {
	t.Helper()
	if err := reg.Add(rt); err != nil {
		t.Fatalf("Add %s: %v", rt.OperationID, err)
	}
}

func distinctPaths(routes []api.Route) map[string]struct{} {
	out := map[string]struct{}{}
	for _, rt := range routes {
		out[api.OpenAPIPath(rt.Pattern)] = struct{}{}
	}
	return out
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// thingDTO is the shape the conformance test's stub handler answers with. It
// is a named type because a DTO must be: an anonymous struct has no name to put
// in components/schemas, and the generator says so rather than inventing one.
type thingDTO struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}
