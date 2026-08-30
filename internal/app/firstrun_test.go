package app

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jlbyh2o/llamaman/internal/api/middleware"
	"github.com/jlbyh2o/llamaman/internal/auth"
)

// The first-run acceptance test: SPEC section 3.9's "download → start → open
// browser → done", against the real boot sequence, the real database and the
// real listener.
//
// It is here rather than in internal/auth because the properties it asserts are
// properties of the WIRING — the boot sequence mints the token file (section 11.1
// step 8), the loopback rule lets a browser on this machine claim with no token
// at all (D38), the claim burns the file, and the session cookies the claim
// issues authenticate the very next request.

// startDaemon boots a daemon on a free loopback port and returns its address.
func startDaemon(t *testing.T, dir string) string {
	t.Helper()

	ready := make(chan string, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			Logger:           quiet(),
			Notifier:         &recordingNotifier{},
			StateDirOverride: dir,
			Getenv:           func(string) string { return "" },
			ReadyHook:        func(addr string) { ready <- addr },
		})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("the daemon did not shut down")
		}
	})

	select {
	case addr := <-ready:
		return addr
	case err := <-done:
		t.Fatalf("Run returned before it was listening: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the daemon never became ready")
	}
	return ""
}

// waitForTokenFile waits for section 11.1 step 8 to run.
//
// ReadyHook fires at step 7 — the port walk — because that is where the listener
// is bound and where a test learns the address; the setup claim is step 8. The
// gap is milliseconds, and waiting for the file rather than sleeping is what
// keeps this test deterministic on a loaded machine.
func waitForTokenFile(t *testing.T, path string) os.FileInfo {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		fi, err := os.Stat(path)
		if err == nil {
			return fi
		}
		if time.Now().After(deadline) {
			t.Fatalf("the boot never wrote %s: %v", path, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type testClient struct {
	t       *testing.T
	base    string
	cookies map[string]string
}

func newTestClient(t *testing.T, addr string) *testClient {
	return &testClient{t: t, base: "http://" + addr, cookies: map[string]string{}}
}

// do sends a request carrying whatever cookies the previous responses set, and
// echoes the CSRF cookie in the header the way the SPA does.
func (c *testClient) do(method, path, body string, headers map[string]string) *http.Response {
	c.t.Helper()

	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	} else {
		reader = strings.NewReader("")
	}
	req, err := http.NewRequest(method, c.base+path, reader)
	if err != nil {
		c.t.Fatalf("build the request: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, value := range c.cookies {
		req.AddCookie(&http.Cookie{Name: name, Value: value})
	}
	if v, ok := c.cookies[middleware.CookieCSRF]; ok && method != http.MethodGet {
		req.Header.Set(middleware.HeaderCSRF, v)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	for _, cookie := range resp.Cookies() {
		if cookie.MaxAge < 0 || cookie.Value == "" {
			delete(c.cookies, cookie.Name)
			continue
		}
		c.cookies[cookie.Name] = cookie.Value
	}
	return resp
}

func decodeInto(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode the response: %v", err)
	}
}

// TestFirstRunClaimFromLoopback is the whole of D38's common case.
func TestFirstRunClaimFromLoopback(t *testing.T) {
	dir := t.TempDir()
	seedLoopback(t, dir)
	addr := startDaemon(t, dir)
	c := newTestClient(t, addr)

	// Section 11.1 step 8: the boot minted the one-time token into a 0600 file.
	tokenPath := auth.SetupTokenPath(dir)
	fi := waitForTokenFile(t, tokenPath)
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("the setup token is mode %04o, want 0600", perm)
	}

	// The public state endpoint tells a loopback browser it needs no token.
	var state struct {
		Claimed       bool   `json:"claimed"`
		Complete      bool   `json:"complete"`
		TokenRequired bool   `json:"token_required"`
		ActiveStep    string `json:"active_step"`
	}
	decodeInto(t, c.do(http.MethodGet, "/api/v1/setup/state", "", nil), &state)
	if state.Claimed || state.Complete {
		t.Fatalf("a fresh host reports claimed=%v complete=%v", state.Claimed, state.Complete)
	}
	if state.TokenRequired {
		t.Fatal("a loopback caller was told a setup token is required (D38)")
	}
	if state.ActiveStep != "password" {
		t.Fatalf("active_step = %q, want password", state.ActiveStep)
	}

	// A session route on an unclaimed host is the setup gate, not a 401.
	resp := c.do(http.MethodGet, "/api/v1/auth/sessions", "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("GET /auth/sessions on an unclaimed host = %d, want 409 setup_required", resp.StatusCode)
	}

	// The claim itself, from loopback, with no token at all.
	resp = c.do(http.MethodPost, "/api/v1/setup/password", `{"password":"a good first password"}`, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /setup/password = %d, want 204", resp.StatusCode)
	}
	if c.cookies[middleware.CookieSession] == "" || c.cookies[middleware.CookieCSRF] == "" {
		t.Fatalf("the claim did not set both cookies: %v", c.cookies)
	}

	// Section 2.2a step 5: the file is unlinked immediately after the commit.
	if _, err := os.Stat(tokenPath); err == nil {
		t.Error("the setup-token file survived the claim")
	}

	// A second claim loses the race — the account exists, so the gate answers
	// `setup_already_claimed` rather than letting a second password through.
	resp = c.do(http.MethodPost, "/api/v1/setup/password", `{"password":"a second password"}`, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("a second claim = %d, want 409", resp.StatusCode)
	}

	// The session the claim issued authenticates the next request, and the CSRF
	// double-submit is satisfied by echoing the cookie the claim set.
	var sessions struct {
		Items []struct {
			ID      string `json:"id"`
			Current bool   `json:"current"`
		} `json:"items"`
		Total int `json:"total"`
	}
	decodeInto(t, c.do(http.MethodGet, "/api/v1/auth/sessions", "", nil), &sessions)
	if sessions.Total != 1 || !sessions.Items[0].Current {
		t.Fatalf("sessions = %+v, want exactly one, marked current", sessions)
	}

	// `GET /api/v1/meta` distinguishes claimed from complete: this is a wizard
	// interrupted after the password step, which section 11.2 requires be
	// resumable.
	var meta struct {
		SetupComplete bool `json:"setup_complete"`
		Claimed       bool `json:"claimed"`
	}
	decodeInto(t, c.do(http.MethodGet, "/api/v1/meta", "", nil), &meta)
	if !meta.Claimed || meta.SetupComplete {
		t.Fatalf("meta = %+v, want claimed with an unfinished wizard", meta)
	}

	// A non-GET without the CSRF header is refused even with a valid session.
	req := c.cookies[middleware.CookieCSRF]
	delete(c.cookies, middleware.CookieCSRF)
	resp = c.do(http.MethodPost, "/api/v1/auth/logout", "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST /auth/logout without the CSRF pair = %d, want 403", resp.StatusCode)
	}
	c.cookies[middleware.CookieCSRF] = req

	// Logout revokes the session and clears the cookies; the next call is a 401.
	resp = c.do(http.MethodPost, "/api/v1/auth/logout", "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /auth/logout = %d, want 204", resp.StatusCode)
	}
	resp = c.do(http.MethodGet, "/api/v1/auth/sessions", "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /auth/sessions after logout = %d, want 401", resp.StatusCode)
	}

	// And the password the claim set logs back in.
	resp = c.do(http.MethodPost, "/api/v1/auth/login", `{"password":"a good first password"}`, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /auth/login = %d, want 204", resp.StatusCode)
	}
	var session struct {
		Authenticated bool `json:"authenticated"`
		Claimed       bool `json:"claimed"`
		SetupComplete bool `json:"setup_complete"`
	}
	decodeInto(t, c.do(http.MethodGet, "/api/v1/auth/session", "", nil), &session)
	if !session.Authenticated || !session.Claimed {
		t.Fatalf("auth/session = %+v, want an authenticated caller on a claimed host", session)
	}
	// The two facts are DIFFERENT questions and this host is the case that
	// separates them: the password step has run, so `admin_account` exists and
	// the host is claimed — but the wizard has not reached `done`, so it is not
	// complete. Answering `setup_complete: true` here is what used to send a
	// returning browser to a dashboard it was not ready for, silently abandoning
	// a wizard section 11.2 requires be resumable.
	if session.SetupComplete {
		t.Errorf("auth/session reported setup_complete on a host whose wizard has not finished")
	}
}

// TestFirstRunFromAnotherAddressNeedsTheToken is the other half of D38: any
// origin that is not loopback must present the one-time token, and the token it
// must present is the one in the file the boot wrote.
func TestFirstRunFromAnotherAddressNeedsTheToken(t *testing.T) {
	dir := t.TempDir()
	seedLoopback(t, dir)
	addr := startDaemon(t, dir)

	waitForTokenFile(t, auth.SetupTokenPath(dir))
	token, err := auth.ReadSetupTokenFile(auth.SetupTokenPath(dir))
	if err != nil {
		t.Fatalf("read the setup token: %v", err)
	}
	if token == "" {
		t.Fatal("the boot wrote an empty setup token")
	}

	// The listener is on loopback, so a genuinely remote client cannot be
	// simulated here; what CAN be asserted end to end is that the token the file
	// carries is the one the daemon accepts — the sha256 in `setup_claim` and
	// the plaintext on disk agree. internal/api's own suite covers the address
	// branch with a synthesized RemoteAddr.
	c := newTestClient(t, addr)
	resp := c.do(http.MethodPost, "/api/v1/setup/password", `{"password":"a good first password"}`,
		map[string]string{middleware.HeaderSetupToken: token})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /setup/password with the token from the file = %d, want 204", resp.StatusCode)
	}
}
