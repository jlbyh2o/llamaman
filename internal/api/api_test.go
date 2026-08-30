package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jlbyh2o/llamaman/internal/api/middleware"
	"github.com/jlbyh2o/llamaman/internal/model"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// stubMeta answers GET /api/v1/meta without a database.
type stubMeta struct {
	meta Meta
	err  error
}

func (s stubMeta) Meta(context.Context) (Meta, error) { return s.meta, s.err }

// stubAuth is a fully controllable Authenticator, so a test can put the daemon
// in any of the four states the session gate distinguishes.
type stubAuth struct {
	complete    bool
	completeErr error
	session     *middleware.Session
	setupOK     bool
	csrfOK      bool
}

func (s stubAuth) SetupComplete(context.Context) (bool, error) { return s.complete, s.completeErr }

func (s stubAuth) Authenticate(context.Context, *http.Request) (*middleware.Session, error) {
	if s.session == nil {
		return nil, middleware.ErrNoSession
	}
	return s.session, nil
}

func (s stubAuth) AuthorizeSetup(context.Context, *http.Request) error {
	if s.setupOK {
		return nil
	}
	return middleware.ErrSetupTokenRequired
}

func (s stubAuth) VerifyCSRF(context.Context, *middleware.Session, string, string) bool {
	return s.csrfOK
}

func newTestAPI(t *testing.T, cfg Config) *API {
	t.Helper()
	if cfg.Logger == nil {
		cfg.Logger = quietLogger()
	}
	if cfg.Meta == nil {
		cfg.Meta = stubMeta{meta: Meta{Version: "test", Commit: "abc", UIPort: 5526}}
	}
	if cfg.Events == nil {
		cfg.Events = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
		})
	}
	a, err := New(cfg)
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	return a
}

// TestRoutingTable is the routing-table test DESIGN section 15 asks for: the
// registry mounts exactly the endpoints this binary claims to serve, at the
// method and pattern the section 3 tables name, with the Auth column each row
// carries.
func TestRoutingTable(t *testing.T) {
	t.Parallel()

	a := newTestAPI(t, Config{Auth: stubAuth{}})

	want := map[string]struct {
		auth Auth
		op   string
	}{
		"GET /healthz":       {AuthPublic, "getHealth"},
		"GET /api/v1/meta":   {AuthPublic, "getMeta"},
		"GET /api/v1/events": {AuthSession, "streamEvents"},

		// Section 3.1, in the order that table lists them.
		"GET /api/v1/auth/session":          {AuthPublic, "getAuthSession"},
		"POST /api/v1/auth/login":           {AuthPublic, "login"},
		"POST /api/v1/auth/logout":          {AuthSession, "logout"},
		"POST /api/v1/auth/password":        {AuthSession, "changePassword"},
		"GET /api/v1/auth/sessions":         {AuthSession, "listSessions"},
		"DELETE /api/v1/auth/sessions/{id}": {AuthSession, "revokeSession"},

		// Section 3.2, the rows whose subsystems exist today. The four that
		// delegate to internal/toolchain, internal/llamacpp, internal/hf and the
		// cache scanner are deliberately unmounted until those land — a
		// documented endpoint must be one the binary actually serves (D43).
		"GET /api/v1/setup/state":     {AuthPublic, "getSetupState"},
		"POST /api/v1/setup/password": {AuthSetup, "setupPassword"},
		"POST /api/v1/setup/skip":     {AuthSession, "skipSetupStep"},
		"POST /api/v1/setup/complete": {AuthSession, "completeSetup"},

		// Section 3.10, the five rows whose service exists today. The rest of
		// that table needs the supervisor, the gateway or the fit calculator
		// and is unmounted for the same reason the four setup rows above are.
		"GET /api/v1/instances":         {AuthSession, "listInstances"},
		"POST /api/v1/instances":        {AuthSession, "createInstance"},
		"GET /api/v1/instances/{id}":    {AuthSession, "getInstance"},
		"PATCH /api/v1/instances/{id}":  {AuthSession, "patchInstance"},
		"DELETE /api/v1/instances/{id}": {AuthSession, "deleteInstance"},

		// Section 3.5, complete: all twelve rows of the llama.cpp lifecycle
		// table. `…/plan` is section 6.3's whole subject and `…/retry` is the
		// operation section 2.5's reuse-and-reset row and D4 name, so neither is
		// a convenience that could be left out.
		"GET /api/v1/llamacpp/active":                  {AuthSession, "getActiveLlamacpp"},
		"GET /api/v1/llamacpp/versions":                {AuthSession, "listLlamacppVersions"},
		"POST /api/v1/llamacpp/versions":               {AuthSession, "installLlamacppVersion"},
		"GET /api/v1/llamacpp/versions/{id}":           {AuthSession, "getLlamacppVersion"},
		"DELETE /api/v1/llamacpp/versions/{id}":        {AuthSession, "deleteLlamacppVersion"},
		"POST /api/v1/llamacpp/versions/{id}/cancel":   {AuthSession, "cancelLlamacppVersion"},
		"POST /api/v1/llamacpp/versions/{id}/retry":    {AuthSession, "retryLlamacppVersion"},
		"GET /api/v1/llamacpp/versions/{id}/log":       {AuthSession, "getLlamacppVersionLog"},
		"POST /api/v1/llamacpp/versions/{id}/activate": {AuthSession, "activateLlamacppVersion"},
		"POST /api/v1/llamacpp/rollback":               {AuthSession, "rollbackLlamacpp"},
		"GET /api/v1/llamacpp/releases":                {AuthSession, "listLlamacppReleases"},
		"GET /api/v1/llamacpp/plan":                    {AuthSession, "planLlamacppInstall"},

		// Section 3.6, complete: the remote Hub surface and the two validating
		// credential triples. Every repo-scoped path puts the verb IN FRONT of
		// the `{repo...}` wildcard, because ServeMux panics at registration on a
		// multi-segment wildcard that is not final.
		"GET /api/v1/hf/search":          {AuthSession, "searchHF"},
		"GET /api/v1/hf/model/{repo...}": {AuthSession, "getHFModel"},
		"GET /api/v1/hf/tree/{repo...}":  {AuthSession, "getHFTree"},
		"GET /api/v1/hf/card/{repo...}":  {AuthSession, "getHFCard"},
		"GET /api/v1/hf/peek/{repo...}":  {AuthSession, "peekHFFile"},
		"GET /api/v1/hf/token":           {AuthSession, "getHFToken"},
		"PUT /api/v1/hf/token":           {AuthSession, "putHFToken"},
		"DELETE /api/v1/hf/token":        {AuthSession, "deleteHFToken"},
		"GET /api/v1/github/token":       {AuthSession, "getGitHubToken"},
		"PUT /api/v1/github/token":       {AuthSession, "putGitHubToken"},
		"DELETE /api/v1/github/token":    {AuthSession, "deleteGitHubToken"},

		// Section 3.7, complete: the local model catalog and the cache roots,
		// scans and strays beside it. Every row of that table is here — the
		// service behind them exists, and the two long actions (scan, verify)
		// plus the delete answer 202 with a job receipt.
		"GET /api/v1/models":                     {AuthSession, "listModels"},
		"GET /api/v1/models/{id}":                {AuthSession, "getModel"},
		"GET /api/v1/models/{id}/metadata":       {AuthSession, "getModelMetadata"},
		"GET /api/v1/models/{id}/delete-preview": {AuthSession, "previewModelDelete"},
		"DELETE /api/v1/models/{id}":             {AuthSession, "deleteModel"},
		"POST /api/v1/models/{id}/verify":        {AuthSession, "verifyModel"},
		"POST /api/v1/models/{id}/pair-mmproj":   {AuthSession, "pairModelMmproj"},
		"GET /api/v1/cache/roots":                {AuthSession, "listCacheRoots"},
		"POST /api/v1/cache/roots":               {AuthSession, "addCacheRoot"},
		"POST /api/v1/cache/roots/{id}/promote":  {AuthSession, "promoteCacheRoot"},
		"DELETE /api/v1/cache/roots/{id}":        {AuthSession, "detachCacheRoot"},
		"POST /api/v1/cache/scan":                {AuthSession, "scanCache"},
		"GET /api/v1/cache/scans/{id}":           {AuthSession, "getCacheScan"},
		"GET /api/v1/cache/strays":               {AuthSession, "listStrays"},
		"DELETE /api/v1/cache/strays/{id}":       {AuthSession, "deleteStray"},
		"POST /api/v1/cache/strays/{id}/dismiss": {AuthSession, "dismissStray"},

		// Section 3.8, complete: the download queue. `POST /downloads` is the
		// one long action here and it is idempotent (D65), so a
		// double-clicked Download replays into `200` rather than queuing a
		// second one.
		"GET /api/v1/downloads":              {AuthSession, "listDownloads"},
		"POST /api/v1/downloads":             {AuthSession, "createDownload"},
		"GET /api/v1/downloads/{id}":         {AuthSession, "getDownload"},
		"PATCH /api/v1/downloads/{id}":       {AuthSession, "reorderDownload"},
		"POST /api/v1/downloads/{id}/pause":  {AuthSession, "pauseDownload"},
		"POST /api/v1/downloads/{id}/resume": {AuthSession, "resumeDownload"},
		"POST /api/v1/downloads/{id}/retry":  {AuthSession, "retryDownload"},
		"POST /api/v1/downloads/{id}/cancel": {AuthSession, "cancelDownload"},

		// Section 3.12, complete: the API tokens the gateway accepts, and the
		// denial counters beside them. `POST /api/v1/tokens` is the only
		// response in this API that ever contains a secret, and `DELETE` is a
		// revoke — soft and terminal.
		"GET /api/v1/tokens":            {AuthSession, "listAPITokens"},
		"POST /api/v1/tokens":           {AuthSession, "createAPIToken"},
		"GET /api/v1/tokens/{id}":       {AuthSession, "getAPIToken"},
		"PATCH /api/v1/tokens/{id}":     {AuthSession, "patchAPIToken"},
		"DELETE /api/v1/tokens/{id}":    {AuthSession, "deleteAPIToken"},
		"GET /api/v1/tokens/{id}/usage": {AuthSession, "getAPITokenUsage"},
		"GET /api/v1/gateway/denials":   {AuthSession, "listGatewayDenials"},

		// Section 3.9, complete: the fit calculator. Both are POSTs because the
		// body is a FlagSet, and neither creates anything — a `GET` with a
		// forty-field query string was the alternative.
		"POST /api/v1/fit/estimate":       {AuthSession, "estimateFit"},
		"POST /api/v1/fit/estimate-batch": {AuthSession, "estimateFitBatch"},

		// Section 3.13, complete: the benchmark runs and the two analysis
		// endpoints beside them. `POST /bench/runs` is the one long action and
		// it is idempotent (D65) — a double-clicked Run replays into `200`
		// rather than expanding a second sweep — and it answers `201` for a
		// draft, which is a row created with nothing queued.
		"GET /api/v1/bench/runs":              {AuthSession, "listBenchRuns"},
		"POST /api/v1/bench/runs":             {AuthSession, "createBenchRun"},
		"GET /api/v1/bench/preflight":         {AuthSession, "benchPreflight"},
		"GET /api/v1/bench/runs/{id}":         {AuthSession, "getBenchRun"},
		"PATCH /api/v1/bench/runs/{id}":       {AuthSession, "patchBenchRun"},
		"DELETE /api/v1/bench/runs/{id}":      {AuthSession, "deleteBenchRun"},
		"POST /api/v1/bench/runs/{id}/start":  {AuthSession, "startBenchRun"},
		"POST /api/v1/bench/runs/{id}/cancel": {AuthSession, "cancelBenchRun"},
		"GET /api/v1/bench/runs/{id}/results": {AuthSession, "getBenchRunResults"},
		"GET /api/v1/bench/runs/{id}/export":  {AuthSession, "exportBenchRun"},
		"POST /api/v1/bench/compare":          {AuthSession, "compareBenchRuns"},
		"GET /api/v1/bench/series":            {AuthSession, "benchSeries"},
	}

	got := map[string]struct {
		auth Auth
		op   string
	}{}
	for _, rt := range a.Routes() {
		got[rt.Method+" "+rt.Pattern] = struct {
			auth Auth
			op   string
		}{rt.Auth, rt.OperationID}
	}

	for key, w := range want {
		g, ok := got[key]
		if !ok {
			t.Errorf("route %s is not mounted", key)
			continue
		}
		if g != w {
			t.Errorf("route %s = %+v, want %+v", key, g, w)
		}
	}
	for key := range got {
		if _, expected := want[key]; !expected {
			t.Errorf("route %s is mounted but not in the expected table", key)
		}
	}
}

// TestEventsRouteAbsentWithoutTransport: a document that promised an endpoint
// the binary does not serve would fail the D43 conformance suite, correctly.
func TestEventsRouteAbsentWithoutTransport(t *testing.T) {
	t.Parallel()

	a, err := New(Config{Logger: quietLogger(), Auth: stubAuth{}, Meta: stubMeta{}})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	for _, rt := range a.Routes() {
		if rt.OperationID == "streamEvents" {
			t.Fatal("the SSE route is registered with no transport behind it")
		}
	}
}

func TestHealthAndMeta(t *testing.T) {
	t.Parallel()

	a := newTestAPI(t, Config{
		Auth: stubAuth{},
		Meta: stubMeta{meta: Meta{
			Version: "v1.2.3", Commit: "deadbee", SetupComplete: true, Claimed: true, UIPort: 5530,
		}},
	})

	t.Run("healthz", func(t *testing.T) {
		rr := serve(a, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rr.Code)
		}
		var body Health
		decode(t, rr, &body)
		if body.Status != "ok" {
			t.Errorf("status field = %q, want %q", body.Status, "ok")
		}
	})

	t.Run("meta", func(t *testing.T) {
		rr := serve(a, httptest.NewRequest(http.MethodGet, "/api/v1/meta", nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rr.Code)
		}
		var body Meta
		decode(t, rr, &body)
		if body.Version != "v1.2.3" || body.UIPort != 5530 || !body.Claimed {
			t.Errorf("meta = %+v, want the values the provider returned", body)
		}
	})
}

// TestErrorEnvelopeShape asserts the one body shape DESIGN section 3 defines,
// for every status this layer produces without a handler running.
func TestErrorEnvelopeShape(t *testing.T) {
	t.Parallel()

	a := newTestAPI(t, Config{Auth: stubAuth{}})

	cases := []struct {
		name       string
		req        *http.Request
		wantStatus int
		wantCode   model.ErrorCode
		wantHeader [2]string
	}{
		{
			name:       "unknown api path is 404",
			req:        httptest.NewRequest(http.MethodGet, "/api/v1/nope", nil),
			wantStatus: http.StatusNotFound,
			wantCode:   CodeNotFound,
		},
		{
			name:       "wrong method on a real path is 405 with Allow",
			req:        httptest.NewRequest(http.MethodPost, "/api/v1/meta", nil),
			wantStatus: http.StatusMethodNotAllowed,
			wantCode:   CodeMethodNotAllowed,
			wantHeader: [2]string{"Allow", "GET"},
		},
		{
			name:       "session route on an unclaimed host is setup_required",
			req:        httptest.NewRequest(http.MethodGet, "/api/v1/events", nil),
			wantStatus: http.StatusConflict,
			wantCode:   middleware.CodeSetupRequired,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := serve(a, tc.req)
			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rr.Code, tc.wantStatus, rr.Body.String())
			}
			if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			if rr.Header().Get("Cache-Control") != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", rr.Header().Get("Cache-Control"))
			}
			if tc.wantHeader[0] != "" && rr.Header().Get(tc.wantHeader[0]) != tc.wantHeader[1] {
				t.Errorf("%s = %q, want %q", tc.wantHeader[0],
					rr.Header().Get(tc.wantHeader[0]), tc.wantHeader[1])
			}

			// The envelope has exactly one top-level key, "error", whose
			// object carries code and message and nothing beyond details.
			var raw map[string]json.RawMessage
			decode(t, rr, &raw)
			if len(raw) != 1 {
				t.Fatalf("body has %d top-level keys, want exactly `error`: %s", len(raw), rr.Body)
			}
			var env model.ErrorEnvelope
			if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
				t.Fatalf("body is not the error envelope: %v (%s)", err, rr.Body)
			}
			if env.Error.Code != tc.wantCode {
				t.Errorf("error.code = %q, want %q", env.Error.Code, tc.wantCode)
			}
			if env.Error.Message == "" {
				t.Error("error.message is empty")
			}
		})
	}
}

// TestSessionGateResults walks the four states the gate distinguishes on one
// session route.
func TestSessionGateResults(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		auth       stubAuth
		wantStatus int
		wantCode   model.ErrorCode
	}{
		{
			name:       "unclaimed host routes to the wizard",
			auth:       stubAuth{complete: false},
			wantStatus: http.StatusConflict,
			wantCode:   middleware.CodeSetupRequired,
		},
		{
			name:       "claimed host with no session",
			auth:       stubAuth{complete: true},
			wantStatus: http.StatusUnauthorized,
			wantCode:   middleware.CodeUnauthorized,
		},
		{
			name:       "claimed host with a session",
			auth:       stubAuth{complete: true, session: &middleware.Session{ID: "01J"}},
			wantStatus: http.StatusOK,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestAPI(t, Config{Auth: tc.auth})
			rr := serve(a, httptest.NewRequest(http.MethodGet, "/api/v1/events", nil))
			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", rr.Code, tc.wantStatus, rr.Body)
			}
			if tc.wantCode == "" {
				return
			}
			var env model.ErrorEnvelope
			if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
				t.Fatalf("body is not the error envelope: %v", err)
			}
			if env.Error.Code != tc.wantCode {
				t.Errorf("error.code = %q, want %q", env.Error.Code, tc.wantCode)
			}
		})
	}
}

// TestNoAuthenticatorIsA500NotAPanic: a daemon built without an authenticator
// is a construction bug, but taking the process down would take the gateway
// listeners with it (section 9.4).
func TestNoAuthenticatorIsA500NotAPanic(t *testing.T) {
	t.Parallel()

	a := newTestAPI(t, Config{Auth: nil})
	rr := serve(a, httptest.NewRequest(http.MethodGet, "/api/v1/events", nil))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
}

// TestFallbackServesTheSPAOutsideTheAPINamespace: a browser reloading a
// client-side route must get the app shell, while a miss inside /api/v1 stays
// a JSON 404.
func TestFallbackServesTheSPAOutsideTheAPINamespace(t *testing.T) {
	t.Parallel()

	a := newTestAPI(t, Config{
		Auth: stubAuth{},
		Fallback: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = io.WriteString(w, "<!doctype html>")
		}),
	})

	rr := serve(a, httptest.NewRequest(http.MethodGet, "/instances/01J", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "doctype") {
		t.Errorf("a client-side route got %d %q, want the SPA shell", rr.Code, rr.Body)
	}

	rr = serve(a, httptest.NewRequest(http.MethodGet, "/api/v1/nope", nil))
	if rr.Code != http.StatusNotFound ||
		!strings.HasPrefix(rr.Header().Get("Content-Type"), "application/json") {
		t.Errorf("a miss inside /api/v1 got %d %q, want a JSON 404",
			rr.Code, rr.Header().Get("Content-Type"))
	}
}

func serve(a *API, r *http.Request) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	a.ServeHTTP(rr, r)
	return rr
}

func decode(t *testing.T, rr *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rr.Body.Bytes(), v); err != nil {
		t.Fatalf("decoding %s: %v", rr.Body, err)
	}
}

// TestRequestLogSeesTheMatchedRoute pins the slot mechanism of
// middleware.WithRouteSlot. The request log and the panic recovery both sit
// OUTSIDE the mux, so without it they can only ever name the raw path — which
// for a wildcard route is unbounded, high-cardinality and attacker-chosen.
func TestRequestLogSeesTheMatchedRoute(t *testing.T) {
	t.Parallel()

	var lines []map[string]any
	handler := slog.NewJSONHandler(writerFunc(func(p []byte) (int, error) {
		var m map[string]any
		if err := json.Unmarshal(p, &m); err == nil {
			lines = append(lines, m)
		}
		return len(p), nil
	}), &slog.HandlerOptions{Level: slog.LevelDebug})

	a := newTestAPI(t, Config{Logger: slog.New(handler), Auth: stubAuth{}})
	serve(a, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if len(lines) == 0 {
		t.Fatal("the request log wrote nothing")
	}
	last := lines[len(lines)-1]
	if last["route"] != "getHealth" {
		t.Errorf("log line = %v, want route=getHealth (not a raw path)", last)
	}
	if _, hasPath := last["path"]; hasPath {
		t.Errorf("log line = %v, want no raw path once the route is known", last)
	}
}

// TestRequestLogNamesThePathWhenNothingMatched: a 404 has no operation id, and
// the path is then the only thing that identifies the request.
func TestRequestLogNamesThePathWhenNothingMatched(t *testing.T) {
	t.Parallel()

	var lines []map[string]any
	handler := slog.NewJSONHandler(writerFunc(func(p []byte) (int, error) {
		var m map[string]any
		if err := json.Unmarshal(p, &m); err == nil {
			lines = append(lines, m)
		}
		return len(p), nil
	}), &slog.HandlerOptions{Level: slog.LevelDebug})

	a := newTestAPI(t, Config{Logger: slog.New(handler), Auth: stubAuth{}})
	serve(a, httptest.NewRequest(http.MethodGet, "/api/v1/nope", nil))

	if len(lines) == 0 {
		t.Fatal("the request log wrote nothing")
	}
	last := lines[len(lines)-1]
	if last["path"] != "/api/v1/nope" {
		t.Errorf("log line = %v, want the raw path for an unmatched request", last)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// newRequest and serve are `do` split in two, for the tests that have to set a
// header of their own — `Accept: text/event-stream` above all, which is how
// section 3.5's build log and section 3.3's journal choose their form.
func newRequest(method, target, body string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	r.AddCookie(&http.Cookie{Name: middleware.CookieCSRF, Value: "c"})
	r.Header.Set(middleware.HeaderCSRF, "c")
	return r
}

// contains is strings.Contains, named here so a test assertion reads as a
// sentence about the response rather than about the standard library.
func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }
