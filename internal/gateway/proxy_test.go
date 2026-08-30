package gateway

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/tokens"
)

// TestTransportNeverAsksForCompression is D36, stated as the test it exists for.
//
// Without `DisableCompression: true`, Go's Transport adds `Accept-Encoding:
// gzip` to a request whose client sent none, and transparently decompresses the
// answer — so the bytes the client receives are NOT the bytes llama-server sent,
// and SPEC §3.4's pass-through is broken in a way no test that only compares
// decoded JSON would notice.
func TestTransportNeverAsksForCompression(t *testing.T) {
	t.Parallel()

	seen := make(chan string, 4)
	h := newHarness(t, model.AuthNone, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("Accept-Encoding")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}))

	req, err := http.NewRequest(http.MethodGet, h.url("/v1/models"), nil)
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately no Accept-Encoding: this is the case D36 exists for.
	resp, err := rawClient().Do(req)
	if err != nil {
		t.Fatalf("GET through the gateway: %v", err)
	}
	resp.Body.Close()

	select {
	case got := <-seen:
		if got != "" {
			t.Errorf("llama-server saw Accept-Encoding %q; D36 requires the client's own "+
				"negotiation to pass through untouched, which for an absent header means absent",
				got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the upstream never saw the request")
	}
}

// TestCompressedBodyPassesThroughByteForByte is the other half of D36: when the
// CLIENT negotiates gzip, the compressed bytes reach it unchanged and the
// `Content-Encoding` header survives. A transport that decompressed would hand
// the client plaintext under a gzip header, or strip the header and change the
// byte count the accounting recorded.
func TestCompressedBodyPassesThroughByteForByte(t *testing.T) {
	t.Parallel()

	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	if _, err := zw.Write([]byte(`{"choices":[{"text":"hello"}]}`)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	want := compressed.Bytes()

	h := newHarness(t, model.AuthNone, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			t.Errorf("the client's Accept-Encoding did not reach the upstream: %q",
				r.Header.Get("Accept-Encoding"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(want)
	}))

	req, _ := http.NewRequest(http.MethodGet, h.url("/v1/completions"), nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := rawClient().Do(req)
	if err != nil {
		t.Fatalf("GET through the gateway: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Content-Encoding"); got != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip", got)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("the body was rewritten in flight: got %d bytes, want the %d the upstream sent",
			len(got), len(want))
	}
}

// TestPathAndQueryPassThroughVerbatim: `Rewrite` sets only the upstream URL
// (§9.2), so the OpenAI-compatible API passes through unmodified.
func TestPathAndQueryPassThroughVerbatim(t *testing.T) {
	t.Parallel()

	type seenReq struct {
		method string
		path   string
		query  string
		body   string
		auth   string
		apiKey string
		xff    string
	}
	got := make(chan seenReq, 1)

	h := newHarness(t, model.AuthNone, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- seenReq{
			method: r.Method,
			path:   r.URL.EscapedPath(),
			query:  r.URL.RawQuery,
			body:   string(body),
			auth:   r.Header.Get("Authorization"),
			apiKey: r.Header.Get("X-API-Key"),
			xff:    r.Header.Get("X-Forwarded-For"),
		}
		w.WriteHeader(http.StatusOK)
	}))

	req, _ := http.NewRequest(http.MethodPost,
		h.url("/v1/chat/completions?stream=true&n=2"), strings.NewReader(`{"model":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer lm_should-not-be-forwarded")
	req.Header.Set("X-API-Key", "lm_should-not-be-forwarded")
	resp, err := rawClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	seen := <-got
	if seen.method != http.MethodPost {
		t.Errorf("method = %s, want POST", seen.method)
	}
	if seen.path != "/v1/chat/completions" {
		t.Errorf("path = %q, want /v1/chat/completions", seen.path)
	}
	if seen.query != "stream=true&n=2" {
		t.Errorf("query = %q, want it verbatim", seen.query)
	}
	if seen.body != `{"model":"x"}` {
		t.Errorf("body = %q, want it verbatim", seen.body)
	}
	// §9.2: the client's credential is replaced, never forwarded — llama-server
	// runs without `--api-key` and logs its requests.
	if seen.auth != "" || seen.apiKey != "" {
		t.Errorf("a client credential reached llama-server: Authorization=%q X-API-Key=%q",
			seen.auth, seen.apiKey)
	}
	if seen.xff == "" {
		t.Error("X-Forwarded-For was not set")
	}
}

// TestSSEStreamIsFlushedFrameByFrame proves `FlushInterval: -1`: a chunked SSE
// stream reaches the client as it is produced rather than at the end, and the
// bytes are identical.
//
// The assertion is about TIME, not just content: the upstream writes frame two
// only after the test has read frame one from the gateway, so a buffering proxy
// deadlocks here rather than passing with a slower response.
func TestSSEStreamIsFlushedFrameByFrame(t *testing.T) {
	t.Parallel()

	readFirst := make(chan struct{})
	frames := []string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"he\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"llo\"}}]}\n\n",
		"data: [DONE]\n\n",
	}

	h := newHarness(t, model.AuthNone, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)

		io.WriteString(w, frames[0])
		fl.Flush()
		select {
		case <-readFirst:
		case <-time.After(5 * time.Second):
			t.Error("the client never received the first frame; the stream was buffered")
			return
		}
		for _, f := range frames[1:] {
			io.WriteString(w, f)
			fl.Flush()
		}
	}))

	resp, err := rawClient().Get(h.url("/v1/chat/completions"))
	if err != nil {
		t.Fatalf("GET the stream: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	br := bufio.NewReader(resp.Body)
	first := readFrame(t, br)
	if first != frames[0] {
		t.Errorf("frame 1 = %q, want %q", first, frames[0])
	}
	close(readFirst)

	for i, want := range frames[1:] {
		if got := readFrame(t, br); got != want {
			t.Errorf("frame %d = %q, want %q", i+2, got, want)
		}
	}
}

// readFrame reads one `data: …\n\n` frame.
func readFrame(t *testing.T, br *bufio.Reader) string {
	t.Helper()
	var sb strings.Builder
	for {
		line, err := br.ReadString('\n')
		sb.WriteString(line)
		if err != nil {
			t.Fatalf("reading a frame: %v", err)
		}
		if line == "\n" {
			return sb.String()
		}
	}
}

// TestUnauthorizedEnvelope is section 3.15's refusal shape: `401
// {"error":{"code":"invalid_api_key","message":"…"}}` in the OpenAI-compatible
// shape so SDKs surface a sensible message.
func TestUnauthorizedEnvelope(t *testing.T) {
	t.Parallel()

	reached := make(chan struct{}, 1)
	h := newHarness(t, model.AuthToken, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))

	good := h.mint(tokens.MintParams{Name: "good"})

	cases := []struct {
		name   string
		header string
		value  string
		query  string
		reason model.DenialReason
	}{
		{name: "no credential at all", reason: model.DeniedMissing},
		{name: "an unknown secret", header: "Authorization", value: "Bearer lm_notatoken",
			reason: model.DeniedUnknown},
		{name: "an unknown secret in X-API-Key", header: "X-API-Key", value: "lm_notatoken",
			reason: model.DeniedUnknown},
		{name: "an unknown secret in the query", query: "?api_key=lm_notatoken",
			reason: model.DeniedUnknown},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, h.url("/v1/chat/completions"+tc.query), nil)
			if tc.header != "" {
				req.Header.Set(tc.header, tc.value)
			}
			resp, err := rawClient().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", resp.StatusCode)
			}
			if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Errorf("Content-Type = %q, want JSON so an SDK can parse it", ct)
			}

			var env struct {
				Error struct {
					Message string `json:"message"`
					Type    string `json:"type"`
					Code    string `json:"code"`
				} `json:"error"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
				t.Fatalf("decoding the envelope: %v", err)
			}
			if env.Error.Code != string(CodeInvalidAPIKey) {
				t.Errorf("error.code = %q, want %q", env.Error.Code, CodeInvalidAPIKey)
			}
			if env.Error.Message == "" {
				t.Error("error.message is empty; SDKs show it to the user")
			}
			if env.Error.Type != "invalid_request_error" {
				t.Errorf("error.type = %q, want invalid_request_error", env.Error.Type)
			}
		})
	}

	select {
	case <-reached:
		t.Fatal("a refused request reached llama-server")
	default:
	}

	// The denial reasons are counted per instance and reason (§2.9), and the
	// counters are what the dashboard reads.
	if err := h.gw.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	counted := map[model.DenialReason]int64{}
	for _, d := range h.store.denialRows() {
		counted[d.Reason] += d.Count
	}
	if counted[model.DeniedMissing] != 1 {
		t.Errorf("missing denials = %d, want 1", counted[model.DeniedMissing])
	}
	if counted[model.DeniedUnknown] != 3 {
		t.Errorf("unknown denials = %d, want 3", counted[model.DeniedUnknown])
	}

	// And a good token still gets through, so the refusals above are not simply
	// "this listener denies everything".
	req, _ := http.NewRequest(http.MethodPost, h.url("/v1/chat/completions"), nil)
	req.Header.Set("Authorization", "Bearer "+good)
	resp, err := rawClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("an authenticated request answered %d, want 200", resp.StatusCode)
	}
}

// TestScopedTokenReachesOnlyItsInstances covers SPEC §3.4's per-instance
// scoping: `global` passes everywhere, `instances` requires a `token_instances`
// row for the listener's instance (§9.3).
func TestScopedTokenReachesOnlyItsInstances(t *testing.T) {
	t.Parallel()

	h := newHarness(t, model.AuthToken, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	elsewhere := h.mint(tokens.MintParams{
		Name:        "other instance only",
		Scope:       model.ScopeInstances,
		InstanceIDs: []string{"01JINSTANCE0000000000000099"},
	})
	here := h.mint(tokens.MintParams{
		Name:        "this instance",
		Scope:       model.ScopeInstances,
		InstanceIDs: []string{h.instanceID},
	})
	global := h.mint(tokens.MintParams{Name: "global"})

	for _, tc := range []struct {
		name   string
		secret string
		want   int
	}{
		{"a token scoped to another instance", elsewhere, http.StatusUnauthorized},
		{"a token scoped to this instance", here, http.StatusOK},
		{"a global token", global, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, h.url("/v1/models"), nil)
			req.Header.Set("Authorization", "Bearer "+tc.secret)
			resp, err := rawClient().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

// TestHealthAndInfoAreUnauthenticated is section 3.15's first two rows.
// `/health` stays unauthenticated to match llama-server behavior (SPEC §3.4),
// and both are answered by the gateway itself so a STOPPED instance still has a
// health endpoint rather than connection-refused.
func TestHealthAndInfoAreUnauthenticated(t *testing.T) {
	t.Parallel()

	proxied := make(chan string, 4)
	h := newHarness(t, model.AuthToken, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied <- r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))

	for _, tc := range []struct {
		state  model.InstanceState
		status int
		body   string
	}{
		{model.InstanceReady, http.StatusOK, `{"status":"ok"}`},
		{model.InstanceDegraded, http.StatusOK, `{"status":"ok"}`},
		{model.InstanceLoading, http.StatusServiceUnavailable, `{"status":"loading model"}`},
		{model.InstanceStopped, http.StatusServiceUnavailable, `{"status":"stopped"}`},
		{model.InstanceFailed, http.StatusServiceUnavailable, `{"status":"stopped"}`},
	} {
		t.Run(string(tc.state), func(t *testing.T) {
			h.setState(tc.state)
			resp, err := rawClient().Get(h.url("/health"))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.status)
			}
			body, _ := io.ReadAll(resp.Body)
			if strings.TrimSpace(string(body)) != tc.body {
				t.Errorf("body = %q, want %q", strings.TrimSpace(string(body)), tc.body)
			}
		})
	}

	h.setState(model.InstanceReady)
	resp, err := rawClient().Get(h.url("/llamaman/info"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var info InfoResponse
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatal(err)
	}
	if info.Instance != "stub" {
		t.Errorf("instance = %q, want stub", info.Instance)
	}
	if !info.AuthRequired {
		t.Error("auth_required = false on a token instance")
	}

	select {
	case p := <-proxied:
		t.Errorf("%s was proxied; §9.2 answers it locally", p)
	default:
	}
}

// TestStoppedInstanceAnswers503 is section 9.1's promise: a listener is open
// whenever the instance row exists, so a client hitting a stopped instance gets
// a JSON 503 instead of connection-refused — which is far easier to debug, and
// keeps another process from stealing the port.
func TestStoppedInstanceAnswers503(t *testing.T) {
	t.Parallel()

	h := newHarness(t, model.AuthNone, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	h.setState(model.InstanceStopped)

	resp, err := rawClient().Get(h.url("/v1/models"))
	if err != nil {
		t.Fatalf("the port refused the connection; §9.1 keeps it open: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	var env errorEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	if env.Error.Code != string(CodeInstanceNotRunning) {
		t.Errorf("error.code = %q, want %q", env.Error.Code, CodeInstanceNotRunning)
	}
}

// TestLoadingInstanceCarriesRetryAfter: "when the instance is loading, a
// Retry-After derived from the previous launch's observed load time" (§9.2).
func TestLoadingInstanceCarriesRetryAfter(t *testing.T) {
	t.Parallel()

	h := newHarness(t, model.AuthNone, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	at := time.Now().Add(-time.Hour).UnixMilli()
	ready := at + 42_000
	h.store.mu.Lock()
	h.store.starts[h.instanceID] = []model.InstanceStart{{
		ID: "01JSTART0000000000000000001", InstanceID: h.instanceID, At: at, ReadyAt: &ready,
	}}
	h.store.mu.Unlock()

	h.setState(model.InstanceLoading)

	resp, err := rawClient().Get(h.url("/v1/models"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got != "42" {
		t.Errorf("Retry-After = %q, want 42 — the previous launch's observed load time", got)
	}
}
