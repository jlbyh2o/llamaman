package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jlbyh2o/llamaman/internal/api/middleware"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
}

func TestValidatePattern(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		pattern string
		wantErr string // substring; empty means it must be accepted
	}{
		{"plain", "/api/v1/meta", ""},
		{"health", "/healthz", ""},
		{"single wildcard", "/api/v1/models/{id}", ""},
		{"wildcard mid path", "/api/v1/downloads/{id}/cancel", ""},
		{"two wildcards", "/api/v1/instances/{id}/starts/{start_id}", ""},
		{"multi segment last", "/api/v1/hf/tree/{repo...}", ""},
		{"end of path marker", "/api/v1/{$}", ""},

		// The rule DESIGN section 3.6 exists for. Registering this against a
		// real ServeMux panics AT REGISTRATION, taking the daemon down at boot.
		{
			"multi segment not last",
			"/api/v1/hf/models/{repo...}/tree",
			"must be the final segment",
		},
		{"empty", "", "empty pattern"},
		{"no leading slash", "api/v1/meta", "must begin with '/'"},
		{"empty segment", "/api//v1", "empty path segment"},
		{"partial wildcard prefix", "/api/v1/models/pre{id}", "whole path segment"},
		{"partial wildcard suffix", "/api/v1/models/{id}post", "whole path segment"},
		{"unbalanced close", "/api/v1/models/id}", "unbalanced"},
		{"anonymous wildcard", "/api/v1/models/{}", "needs a name"},
		{"duplicate wildcard", "/api/v1/{id}/x/{id}", "duplicate wildcard name"},
		{"end marker not last", "/api/{$}/v1", "may only be the final segment"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validatePattern(tc.pattern)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("validatePattern(%q) = %v, want nil", tc.pattern, err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("validatePattern(%q) = nil, want an error containing %q", tc.pattern, tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("validatePattern(%q) = %v, want an error containing %q", tc.pattern, err, tc.wantErr)
			}
		})
	}
}

// TestPatternsNeverPanicOnServeMux is the CI test DESIGN section 3.6 asks for:
// "a CI test additionally constructs the real ServeMux from the route registry
// inside a recover() and fails on any panic, so this class of mistake cannot
// return".
//
// It runs against every pattern validatePattern accepts, so the two checks
// cannot both be wrong in the same direction.
func TestPatternsNeverPanicOnServeMux(t *testing.T) {
	t.Parallel()

	patterns := []string{
		"/healthz",
		"/api/v1/meta",
		"/api/v1/events",
		"/api/v1/models/{id}",
		"/api/v1/downloads/{id}/cancel",
		"/api/v1/hf/tree/{repo...}",
		"/api/v1/hf/card/{repo...}",
	}

	for _, p := range patterns {
		if err := validatePattern(p); err != nil {
			t.Fatalf("validatePattern(%q) rejected a pattern this test expects to be legal: %v", p, err)
		}
		mux := http.NewServeMux()
		if err := handleSafely(mux, "GET "+p, okHandler()); err != nil {
			t.Errorf("registering %q on a real ServeMux failed: %v", p, err)
		}
	}
}

// TestHandleSafelyConvertsThePanic proves the second half of that guarantee:
// when a pattern does reach the mux and panics, the composition root gets an
// error rather than a dead daemon.
func TestHandleSafelyConvertsThePanic(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	if err := handleSafely(mux, "GET /api/v1/hf/models/{repo...}/tree", okHandler()); err == nil {
		t.Fatal("handleSafely returned nil for a pattern ServeMux panics on")
	} else if !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("handleSafely error = %v, want it to name the panic", err)
	}
}

func TestRegistryAdd(t *testing.T) {
	t.Parallel()

	base := Route{
		Method: http.MethodGet, Pattern: "/api/v1/thing", Auth: AuthPublic,
		OperationID: "getThing", Handler: okHandler(),
		Success: Response{Status: http.StatusOK},
	}

	cases := []struct {
		name    string
		mutate  func(*Route)
		wantErr string
	}{
		{"valid", func(*Route) {}, ""},
		{"no method", func(r *Route) { r.Method = "" }, "no method"},
		{"unknown method", func(r *Route) { r.Method = "FROB" }, "unknown method"},
		{"no handler", func(r *Route) { r.Handler = nil }, "no handler"},
		{"no operation id", func(r *Route) { r.OperationID = "" }, "no operation id"},
		{"bad auth", func(r *Route) { r.Auth = "sudo" }, "invalid auth"},
		{"token auth", func(r *Route) { r.Auth = AuthToken }, "belongs to the gateway listeners"},
		{"bad pattern", func(r *Route) { r.Pattern = "/a/{x...}/b" }, "must be the final segment"},
		{
			"idempotent GET",
			func(r *Route) { r.Idempotent = true },
			"only a job-creating POST",
		},
		{
			"rate limit without a code",
			func(r *Route) { r.RateLimit = &rateLimitWithoutCode },
			"must name the error code",
		},
		{"non-2xx success", func(r *Route) { r.Success.Status = 404 }, "is not a 2xx"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rt := base
			tc.mutate(&rt)
			err := NewRegistry().Add(rt)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("Add = %v, want nil", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("Add = nil, want an error containing %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("Add = %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestRegistryRejectsDuplicates(t *testing.T) {
	t.Parallel()

	rt := Route{
		Method: http.MethodGet, Pattern: "/api/v1/thing", Auth: AuthPublic,
		OperationID: "getThing", Handler: okHandler(),
		Success: Response{Status: http.StatusOK},
	}

	reg := NewRegistry()
	if err := reg.Add(rt); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	if err := reg.Add(rt); err == nil {
		t.Fatal("adding the same method+pattern twice was accepted")
	}

	other := rt
	other.Pattern = "/api/v1/other"
	if err := reg.Add(other); err == nil {
		t.Fatal("reusing an operation id on a second pattern was accepted")
	}
}

func TestOpenAPIPathAndParams(t *testing.T) {
	t.Parallel()

	cases := []struct {
		pattern   string
		wantPath  string
		wantNames []string
		wantMulti []bool
	}{
		{"/healthz", "/healthz", nil, nil},
		{"/api/v1/models/{id}", "/api/v1/models/{id}", []string{"id"}, []bool{false}},
		{
			"/api/v1/hf/tree/{repo...}", "/api/v1/hf/tree/{repo}",
			[]string{"repo"}, []bool{true},
		},
		{
			"/api/v1/downloads/{id}/cancel", "/api/v1/downloads/{id}/cancel",
			[]string{"id"}, []bool{false},
		},
	}

	for _, tc := range cases {
		t.Run(tc.pattern, func(t *testing.T) {
			t.Parallel()
			if got := OpenAPIPath(tc.pattern); got != tc.wantPath {
				t.Errorf("OpenAPIPath(%q) = %q, want %q", tc.pattern, got, tc.wantPath)
			}
			params := PathParams(tc.pattern)
			if len(params) != len(tc.wantNames) {
				t.Fatalf("PathParams(%q) = %v, want %d params", tc.pattern, params, len(tc.wantNames))
			}
			for i, p := range params {
				if p.Name != tc.wantNames[i] || p.Multi != tc.wantMulti[i] {
					t.Errorf("PathParams(%q)[%d] = %+v, want {%s %v}",
						tc.pattern, i, p, tc.wantNames[i], tc.wantMulti[i])
				}
			}
		})
	}
}

// rateLimitWithoutCode is the policy a route must not be able to declare: a 429
// that does not say which of section 3's two documented meanings it carries.
var rateLimitWithoutCode = middleware.RateLimit{Burst: 1, Every: time.Second}
