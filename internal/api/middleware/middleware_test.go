package middleware

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
)

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// TestChainOrderIsOutermostFirst pins the contract every other test in this
// package relies on: Chain[0] sees a request before Chain[1].
func TestChainOrderIsOutermostFirst(t *testing.T) {
	t.Parallel()

	var order []string
	mark := func(name string) Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
			})
		}
	}

	h := Chain{mark("first"), nil, mark("second"), mark("third")}.Then(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { order = append(order, "handler") }))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	want := []string{"first", "second", "third", "handler"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v", order, want)
	}
}

// authStub is a controllable Authenticator.
type authStub struct {
	complete bool
	session  *Session
	setupOK  bool
	csrfOK   bool
}

func (a authStub) SetupComplete(context.Context) (bool, error) { return a.complete, nil }

func (a authStub) Authenticate(context.Context, *http.Request) (*Session, error) {
	if a.session == nil {
		return nil, ErrNoSession
	}
	return a.session, nil
}

func (a authStub) AuthorizeSetup(context.Context, *http.Request) error {
	if a.setupOK {
		return nil
	}
	return ErrSetupTokenRequired
}

func (a authStub) VerifyCSRF(context.Context, *Session, string, string) bool { return a.csrfOK }

// TestPerRouteChainOrder asserts the order DESIGN section 1 lists — session,
// CSRF, rate limit, idempotency — by the only means that cannot be faked: for
// each adjacent pair, a request that would fail BOTH layers must be rejected by
// the outer one.
func TestPerRouteChainOrder(t *testing.T) {
	t.Parallel()

	// Assembled exactly as internal/api assembles it.
	build := func(auth Auth, a Authenticator, rl *RateLimit, idem bool) http.Handler {
		chain := Chain{}
		chain = chain.Append(SessionGate(auth, a))
		if auth == AuthSession {
			chain = chain.Append(CSRF(a))
		}
		chain = chain.Append(RateLimiter(rl, nil))
		if idem {
			chain = chain.Append(IdempotencyKeyExtractor())
		}
		return chain.Then(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
	}

	exhausted := &RateLimit{Burst: 1, Every: time.Hour, Code: "restart_rate_limited", Message: "wait"}

	cases := []struct {
		name       string
		handler    http.Handler
		req        func() *http.Request
		wantStatus int
		wantCode   model.ErrorCode
	}{
		{
			// Session before CSRF: no session AND no CSRF token — the gate
			// wins, so the client is told to log in rather than told its CSRF
			// token is bad.
			name:    "session precedes csrf",
			handler: build(AuthSession, authStub{complete: true}, nil, false),
			req: func() *http.Request {
				return httptest.NewRequest(http.MethodPost, "/x", nil)
			},
			wantStatus: http.StatusUnauthorized,
			wantCode:   CodeUnauthorized,
		},
		{
			// The setup gate precedes the session check: an unclaimed host
			// answers setup_required, which is the code the SPA routes to the
			// wizard on.
			name:    "setup gate precedes the session check",
			handler: build(AuthSession, authStub{complete: false}, nil, false),
			req: func() *http.Request {
				return httptest.NewRequest(http.MethodGet, "/x", nil)
			},
			wantStatus: http.StatusConflict,
			wantCode:   CodeSetupRequired,
		},
		{
			// CSRF before the rate limiter: a bad CSRF token on an exhausted
			// bucket is a 403, not a 429.
			name: "csrf precedes the rate limiter",
			handler: build(AuthSession,
				authStub{complete: true, session: &Session{ID: "01J"}, csrfOK: false},
				exhausted, false),
			req: func() *http.Request {
				return httptest.NewRequest(http.MethodPost, "/x", nil)
			},
			wantStatus: http.StatusForbidden,
			wantCode:   CodeCSRFFailed,
		},
		{
			// The rate limiter before the idempotency extractor: an invalid
			// key on an exhausted bucket is a 429, not a 400.
			name: "rate limiter precedes idempotency",
			handler: build(AuthPublic, authStub{complete: true},
				&RateLimit{Burst: 0, Every: time.Hour, Code: "restart_rate_limited"}, true),
			req: func() *http.Request {
				r := httptest.NewRequest(http.MethodPost, "/x", nil)
				r.Header.Set(HeaderIdempotencyKey, "not a valid key")
				return r
			},
			wantStatus: http.StatusTooManyRequests,
			wantCode:   "restart_rate_limited",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := tc.handler
			// Exhaust the single-token buckets so the limiter would reject.
			for i := 0; i < 3; i++ {
				h.ServeHTTP(httptest.NewRecorder(), tc.req())
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, tc.req())
			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", rr.Code, tc.wantStatus, rr.Body)
			}
			var env model.ErrorEnvelope
			if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
				t.Fatalf("body is not the error envelope: %v (%s)", err, rr.Body)
			}
			if env.Error.Code != tc.wantCode {
				t.Errorf("error.code = %q, want %q", env.Error.Code, tc.wantCode)
			}
		})
	}
}

func TestSessionGatePublicRouteHasNoLayer(t *testing.T) {
	t.Parallel()
	if SessionGate(AuthPublic, nil) != nil {
		t.Fatal("a public route should mount no gate at all")
	}
}

func TestSetupGate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		auth       authStub
		remote     string
		wantStatus int
		wantCode   model.ErrorCode
	}{
		{
			name: "claimed host closes the window",
			auth: authStub{complete: true, setupOK: true}, remote: "127.0.0.1:1",
			wantStatus: http.StatusConflict, wantCode: CodeSetupAlreadyClaimed,
		},
		{
			name: "unclaimed host, authorized",
			auth: authStub{complete: false, setupOK: true}, remote: "127.0.0.1:1",
			wantStatus: http.StatusOK,
		},
		{
			name: "unclaimed host, unauthorized",
			auth: authStub{complete: false, setupOK: false}, remote: "10.0.0.5:1",
			wantStatus: http.StatusForbidden, wantCode: CodeSetupTokenRequired,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := SessionGate(AuthSetup, tc.auth)(http.HandlerFunc(
				func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
			r := httptest.NewRequest(http.MethodPost, "/api/v1/setup/password", nil)
			r.RemoteAddr = tc.remote
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, r)
			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", rr.Code, tc.wantStatus, rr.Body)
			}
			if tc.wantCode == "" {
				return
			}
			var env model.ErrorEnvelope
			_ = json.Unmarshal(rr.Body.Bytes(), &env)
			if env.Error.Code != tc.wantCode {
				t.Errorf("error.code = %q, want %q", env.Error.Code, tc.wantCode)
			}
		})
	}
}

func TestCSRF(t *testing.T) {
	t.Parallel()

	sess := &Session{ID: "01J"}
	ok := authStub{complete: true, session: sess, csrfOK: true}

	cases := []struct {
		name       string
		auth       authStub
		method     string
		headers    map[string]string
		cookie     string
		wantStatus int
	}{
		{"GET is exempt", ok, http.MethodGet, nil, "", http.StatusOK},
		{"HEAD is exempt", ok, http.MethodHead, nil, "", http.StatusOK},
		{
			"POST with a matching pair", ok, http.MethodPost,
			map[string]string{HeaderCSRF: "tok"}, "tok", http.StatusOK,
		},
		{
			"POST with no header", ok, http.MethodPost,
			nil, "tok", http.StatusForbidden,
		},
		{
			"POST with no cookie", ok, http.MethodPost,
			map[string]string{HeaderCSRF: "tok"}, "", http.StatusForbidden,
		},
		{
			"POST the authenticator rejects",
			authStub{complete: true, session: sess, csrfOK: false}, http.MethodPost,
			map[string]string{HeaderCSRF: "tok"}, "tok", http.StatusForbidden,
		},
		{
			"cross-site fetch metadata", ok, http.MethodPost,
			map[string]string{HeaderCSRF: "tok", "Sec-Fetch-Site": "cross-site"}, "tok",
			http.StatusForbidden,
		},
		{
			"same-site fetch metadata", ok, http.MethodPost,
			map[string]string{HeaderCSRF: "tok", "Sec-Fetch-Site": "same-origin"}, "tok",
			http.StatusOK,
		},
		{
			"foreign Origin", ok, http.MethodPost,
			map[string]string{HeaderCSRF: "tok", "Origin": "https://evil.example"}, "tok",
			http.StatusForbidden,
		},
		{
			"matching Origin", ok, http.MethodPost,
			map[string]string{HeaderCSRF: "tok", "Origin": "http://example.com"}, "tok",
			http.StatusOK,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := Chain{SessionGate(AuthSession, tc.auth), CSRF(tc.auth)}.Then(
				http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusOK)
				}))
			r := httptest.NewRequest(tc.method, "http://example.com/x", nil)
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			if tc.cookie != "" {
				r.AddCookie(&http.Cookie{Name: CookieCSRF, Value: tc.cookie})
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, r)
			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", rr.Code, tc.wantStatus, rr.Body)
			}
		})
	}
}

// TestFetchMetadata is the cross-origin guard on the `setup` routes: the half
// of the CSRF check that needs no session, plus the Content-Type requirement
// that a `no-cors` POST built to avoid a preflight cannot satisfy.
//
// The attack it refuses is a drive-by claim: a page on any origin, loaded in a
// browser on the host, POSTs to 127.0.0.1 and claims an unclaimed daemon with
// the attacker's password. D38's loopback rule cannot see the difference — the
// request really does come from 127.0.0.1 — and these headers can.
func TestFetchMetadata(t *testing.T) {
	t.Parallel()

	const json = "application/json"

	cases := []struct {
		name       string
		method     string
		headers    map[string]string
		wantStatus int
	}{
		{"GET is exempt", http.MethodGet, nil, http.StatusOK},
		{
			"the wizard's own POST", http.MethodPost,
			map[string]string{"Content-Type": json, "Sec-Fetch-Site": "same-origin",
				"Origin": "http://example.com"},
			http.StatusOK,
		},
		{
			"a plain client with no browser headers", http.MethodPost,
			map[string]string{"Content-Type": json}, http.StatusOK,
		},
		{
			"a cross-site fetch", http.MethodPost,
			map[string]string{"Content-Type": json, "Sec-Fetch-Site": "cross-site"},
			http.StatusForbidden,
		},
		{
			"a foreign Origin", http.MethodPost,
			map[string]string{"Content-Type": json, "Origin": "https://evil.example"},
			http.StatusForbidden,
		},
		{
			// The exact shape of `fetch(url, {mode:"no-cors", body:new Blob([json])})`:
			// no Content-Type at all, which is how it avoids a preflight.
			"a no-cors POST with no Content-Type", http.MethodPost, nil,
			http.StatusUnsupportedMediaType,
		},
		{
			"a form-encoded body", http.MethodPost,
			map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
			http.StatusUnsupportedMediaType,
		},
		{
			"a charset parameter is still JSON", http.MethodPost,
			map[string]string{"Content-Type": "application/json; charset=utf-8"},
			http.StatusOK,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := FetchMetadata()(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))
			r := httptest.NewRequest(tc.method, "http://example.com/api/v1/setup/password", nil)
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, r)
			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", rr.Code, tc.wantStatus, rr.Body)
			}
		})
	}
}

func TestIdempotencyKeyExtractor(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		key        string
		set        bool
		wantStatus int
		wantInCtx  string
	}{
		{"absent is fine", "", false, http.StatusOK, ""},
		{"a plain token", "01JABCDEF", true, http.StatusOK, "01JABCDEF"},
		{"a uuid", "3f1a2b4c-0000-4000-8000-000000000000", true, http.StatusOK,
			"3f1a2b4c-0000-4000-8000-000000000000"},
		{"a space is rejected", "two words", true, http.StatusBadRequest, ""},
		{"a control character is rejected", "a\tb", true, http.StatusBadRequest, ""},
		{"too long", strings.Repeat("k", MaxIdempotencyKeyLen+1), true, http.StatusBadRequest, ""},
		{"at the limit", strings.Repeat("k", MaxIdempotencyKeyLen), true, http.StatusOK,
			strings.Repeat("k", MaxIdempotencyKeyLen)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var seen string
			h := IdempotencyKeyExtractor()(http.HandlerFunc(
				func(w http.ResponseWriter, r *http.Request) {
					seen, _ = IdempotencyKeyFrom(r.Context())
					w.WriteHeader(http.StatusOK)
				}))
			r := httptest.NewRequest(http.MethodPost, "/x", nil)
			if tc.set {
				r.Header.Set(HeaderIdempotencyKey, tc.key)
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, r)
			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", rr.Code, tc.wantStatus, rr.Body)
			}
			if seen != tc.wantInCtx {
				t.Errorf("key in context = %q, want %q", seen, tc.wantInCtx)
			}
		})
	}
}

func TestRateLimiterRefillsAndReportsRetryAfter(t *testing.T) {
	t.Parallel()

	now := time.Unix(0, 0)
	clock := func() time.Time { return now }
	h := RateLimiter(&RateLimit{
		Burst: 2, Every: time.Second, Code: "restart_rate_limited", Message: "wait",
	}, clock)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	do := func() *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/x", nil)
		r.RemoteAddr = "10.0.0.1:1234"
		h.ServeHTTP(rr, r)
		return rr
	}

	for i := 0; i < 2; i++ {
		if rr := do(); rr.Code != http.StatusOK {
			t.Fatalf("request %d = %d, want 200", i, rr.Code)
		}
	}

	rr := do()
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("the third request = %d, want 429", rr.Code)
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Error("no Retry-After header on the 429")
	}
	var env model.ErrorEnvelope
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("429 body is not the envelope: %v", err)
	}
	if _, ok := env.Error.Details["retry_after_ms"]; !ok {
		t.Errorf("429 details = %v, want retry_after_ms", env.Error.Details)
	}

	now = now.Add(time.Second)
	if rr := do(); rr.Code != http.StatusOK {
		t.Fatalf("after a refill interval = %d, want 200", rr.Code)
	}

	// A different client has its own bucket.
	rr = httptest.NewRecorder()
	other := httptest.NewRequest(http.MethodPost, "/x", nil)
	other.RemoteAddr = "10.0.0.2:1234"
	h.ServeHTTP(rr, other)
	if rr.Code != http.StatusOK {
		t.Fatalf("a second client = %d, want 200", rr.Code)
	}
}

func TestRecoverTurnsAPanicIntoAnEnvelope(t *testing.T) {
	t.Parallel()

	h := Chain{RequestLog(quiet(), nil), Recover(quiet())}.Then(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/x", nil))

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
	var env model.ErrorEnvelope
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("body is not the envelope: %v (%s)", err, rr.Body)
	}
	if env.Error.Code != CodeInternalError {
		t.Errorf("error.code = %q, want %q", env.Error.Code, CodeInternalError)
	}
}

// TestRecoverAfterFirstByteDoesNotCorruptTheBody: once a status line is on the
// wire the only honest signal left is closing the connection.
func TestRecoverAfterFirstByteDoesNotCorruptTheBody(t *testing.T) {
	t.Parallel()

	h := Chain{RequestLog(quiet(), nil), Recover(quiet())}.Then(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"items":[`)
			panic("boom")
		}))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/x", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want the 200 that was already written", rr.Code)
	}
	if rr.Body.String() != `{"items":[` {
		t.Errorf("body = %q, want the truncated body with no envelope appended", rr.Body)
	}
}

// TestRecoverRepanicsAbortHandler: net/http defines ErrAbortHandler as "abort
// this connection silently", and swallowing it would turn an ordinary client
// disconnect into a logged 500.
func TestRecoverRepanicsAbortHandler(t *testing.T) {
	t.Parallel()

	defer func() {
		if v := recover(); v != http.ErrAbortHandler {
			t.Fatalf("recovered %v, want http.ErrAbortHandler to propagate", v)
		}
	}()

	h := Recover(quiet())(http.HandlerFunc(
		func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
}

// TestRequestLogRecorderKeepsFlushWorking: the SSE handler flushes every frame
// through http.NewResponseController, which reaches the real writer by
// following Unwrap. A recorder without it would buffer a stream forever.
func TestRequestLogRecorderKeepsFlushWorking(t *testing.T) {
	t.Parallel()

	flushed := false
	h := RequestLog(quiet(), nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("Flush through the log recorder: %v", err)
			return
		}
		flushed = true
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	if !flushed {
		t.Fatal("the handler could not flush through the request-log recorder")
	}
}

func TestIsLoopback(t *testing.T) {
	t.Parallel()

	cases := []struct {
		remote string
		want   bool
	}{
		{"127.0.0.1:5526", true},
		{"127.0.0.53:1", true},
		{"[::1]:5526", true},
		{"10.0.0.5:5526", false},
		{"[2001:db8::1]:5526", false},
		{"garbage", false},
	}
	for _, tc := range cases {
		t.Run(tc.remote, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = tc.remote
			if got := IsLoopback(r); got != tc.want {
				t.Errorf("IsLoopback(%q) = %v, want %v", tc.remote, got, tc.want)
			}
		})
	}
}
