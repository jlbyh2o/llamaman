package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jlbyh2o/llamaman/internal/api/middleware"
	"github.com/jlbyh2o/llamaman/internal/auth"
	"github.com/jlbyh2o/llamaman/internal/model"
)

// The endpoint tests of sections 3.1 and 3.2. internal/auth's own suite proves
// the RULES — the argon2id verification, the lockout arithmetic, the session
// windows, the claim race; what is proven here is the HTTP contract those rules
// are reached through: the cookies section 3 fixes, the statuses each error code
// maps to, and D38's loopback rule as a request experiences it.

// stubSessions is a controllable SessionService.
type stubSessions struct {
	complete bool
	session  *model.Session
	loginErr error
	// loopbackOnly makes AuthorizeSetup behave like the real service: loopback
	// always passes, everyone else must present the token below.
	token   string
	csrfOK  bool
	changed struct{ current, next string }
	revoked []string
	list    []model.SessionInfo
}

func (s *stubSessions) SetupComplete(context.Context) (bool, error) { return s.complete, nil }

func (s *stubSessions) ResolveSession(context.Context, string, string, string) (model.Session, error) {
	if s.session == nil {
		return model.Session{}, auth.ErrNoSession
	}
	return *s.session, nil
}

func (s *stubSessions) VerifyCSRF(context.Context, string, string, string) bool { return s.csrfOK }

func (s *stubSessions) AuthorizeSetup(_ context.Context, loopback bool, presented, _ string) error {
	if loopback || (s.token != "" && presented == s.token) {
		return nil
	}
	return auth.ErrSetupTokenRequired
}

func (s *stubSessions) SessionState(context.Context, string) (model.SessionState, error) {
	out := model.SessionState{SetupComplete: s.complete}
	if s.session != nil {
		expires := s.session.ExpiresAt
		out.Authenticated, out.ExpiresAt = true, &expires
	}
	return out, nil
}

func (s *stubSessions) Login(context.Context, string, string, string) (model.SessionCredential, error) {
	if s.loginErr != nil {
		return model.SessionCredential{}, s.loginErr
	}
	return model.SessionCredential{
		SessionID:     "01SESSION",
		SessionCookie: "01SESSION.secret",
		CSRFToken:     "csrf-token",
		ExpiresAt:     time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC).UnixMilli(),
	}, nil
}

func (s *stubSessions) Logout(_ context.Context, id string) error {
	s.revoked = append(s.revoked, id)
	return nil
}

func (s *stubSessions) ChangePassword(_ context.Context, _, current, next string) error {
	s.changed.current, s.changed.next = current, next
	return nil
}

func (s *stubSessions) Sessions(context.Context, string) ([]model.SessionInfo, error) {
	return s.list, nil
}

func (s *stubSessions) RevokeSession(_ context.Context, id string) error {
	s.revoked = append(s.revoked, id)
	return nil
}

// stubSetup is a controllable SetupService.
type stubSetup struct {
	state     model.SetupState
	claimErr  error
	claimedBy string
	skipped   model.WizardStep
	skipErr   error
	finished  bool
	finishErr error
}

func (s *stubSetup) State(_ context.Context, loopback bool) (model.SetupState, error) {
	out := s.state
	out.TokenRequired = !loopback && !out.Claimed
	return out, nil
}

func (s *stubSetup) ClaimPassword(_ context.Context, _, ip, _ string) (model.SessionCredential, error) {
	if s.claimErr != nil {
		return model.SessionCredential{}, s.claimErr
	}
	s.claimedBy = ip
	return model.SessionCredential{
		SessionID: "01SESSION", SessionCookie: "01SESSION.secret", CSRFToken: "csrf-token",
		ExpiresAt: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC).UnixMilli(),
	}, nil
}

func (s *stubSetup) Skip(_ context.Context, step model.WizardStep) error {
	s.skipped = step
	return s.skipErr
}

func (s *stubSetup) Finish(context.Context) error {
	s.finished = s.finishErr == nil
	return s.finishErr
}

// liveSession is a session the gate will accept, for the `session` routes.
func liveSession() *middleware.Session {
	return &middleware.Session{
		ID:        "01SESSION",
		CreatedAt: time.Now().Add(-time.Hour),
		ExpiresAt: time.Now().Add(time.Hour),
	}
}

func cookieByName(rr *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range (&http.Response{Header: rr.Header()}).Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// TestLoginSetsTheTwoCookies pins section 3's cookie contract, which three
// layers depend on and none of them can renegotiate.
func TestLoginSetsTheTwoCookies(t *testing.T) {
	t.Parallel()

	svc := &stubSessions{complete: true}
	a := newTestAPI(t, Config{Auth: stubAuth{complete: true}, Sessions: svc})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login",
		strings.NewReader(`{"password":"a good password"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := serve(a, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (body %s)", rr.Code, rr.Body.String())
	}

	session := cookieByName(rr, middleware.CookieSession)
	if session == nil {
		t.Fatal("no lm_session cookie was set")
	}
	if session.Value != "01SESSION.secret" {
		t.Errorf("lm_session = %q, want the credential the service minted", session.Value)
	}
	if !session.HttpOnly {
		t.Error("lm_session is not HttpOnly; script would be able to read the session secret")
	}
	if session.SameSite != http.SameSiteLaxMode || session.Path != "/" {
		t.Errorf("lm_session SameSite/Path = %v/%q, want Lax and /", session.SameSite, session.Path)
	}
	if session.Secure {
		t.Error("lm_session is Secure on a plain-HTTP request; the LAN deployment would never send it back")
	}

	csrf := cookieByName(rr, middleware.CookieCSRF)
	if csrf == nil {
		t.Fatal("no lm_csrf cookie was set")
	}
	if csrf.HttpOnly {
		t.Error("lm_csrf is HttpOnly; the SPA could not echo it in X-CSRF-Token")
	}
	if csrf.Value != "csrf-token" {
		t.Errorf("lm_csrf = %q, want the token the service derived", csrf.Value)
	}
}

// TestLoginErrorsMapToStatuses is the section 3.1 table: 401 for a wrong
// password, 429 for a lockout with `retry_after_sec` in details.
func TestLoginErrorsMapToStatuses(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   model.ErrorCode
		wantDetail string
	}{
		{
			name:       "wrong password",
			err:        model.Error{Code: model.CodeBadCredentials, Message: "no"},
			wantStatus: http.StatusUnauthorized,
			wantCode:   model.CodeBadCredentials,
		},
		{
			name: "locked out",
			err: model.Error{Code: model.CodeLockedOut, Message: "later",
				Details: map[string]any{"retry_after_sec": 900}},
			wantStatus: http.StatusTooManyRequests,
			wantCode:   model.CodeLockedOut,
			wantDetail: "retry_after_sec",
		},
		{
			name:       "a password that fails the length rule",
			err:        model.Error{Code: model.CodePasswordInvalid, Message: "too short"},
			wantStatus: http.StatusBadRequest,
			wantCode:   model.CodePasswordInvalid,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := newTestAPI(t, Config{
				Auth:     stubAuth{complete: true},
				Sessions: &stubSessions{complete: true, loginErr: tc.err},
			})
			req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login",
				strings.NewReader(`{"password":"x"}`))
			req.Header.Set("Content-Type", "application/json")
			rr := serve(a, req)

			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rr.Code, tc.wantStatus, rr.Body.String())
			}
			var env model.ErrorEnvelope
			if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode the error envelope: %v", err)
			}
			if env.Error.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", env.Error.Code, tc.wantCode)
			}
			if tc.wantDetail != "" {
				if _, ok := env.Error.Details[tc.wantDetail]; !ok {
					t.Errorf("details = %v, want a %s entry", env.Error.Details, tc.wantDetail)
				}
			}
		})
	}
}

// TestLogoutClearsCookies: a browser must stop sending a revoked session id.
func TestLogoutClearsCookies(t *testing.T) {
	t.Parallel()

	svc := &stubSessions{complete: true, csrfOK: true}
	a := newTestAPI(t, Config{
		Auth:     stubAuth{complete: true, session: liveSession(), csrfOK: true},
		Sessions: svc,
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: middleware.CookieCSRF, Value: "csrf-token"})
	req.Header.Set(middleware.HeaderCSRF, "csrf-token")
	rr := serve(a, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (body %s)", rr.Code, rr.Body.String())
	}
	if len(svc.revoked) != 1 || svc.revoked[0] != "01SESSION" {
		t.Fatalf("revoked = %v, want the caller's own session", svc.revoked)
	}
	for _, name := range []string{middleware.CookieSession, middleware.CookieCSRF} {
		c := cookieByName(rr, name)
		if c == nil || c.MaxAge >= 0 {
			t.Errorf("%s was not expired by logout: %+v", name, c)
		}
	}
}

// TestSessionRoutesRequireTheCSRFPair is section 3's double-submit, as the
// routing table mounts it: a `session` non-GET without the pair is 403.
func TestSessionRoutesRequireTheCSRFPair(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		cookie     string
		header     string
		csrfOK     bool
		wantStatus int
	}{
		{"the matching pair", "csrf-token", "csrf-token", true, http.StatusNoContent},
		{"no cookie", "", "csrf-token", true, http.StatusForbidden},
		{"no header", "csrf-token", "", true, http.StatusForbidden},
		{"a pair the service rejects", "forged", "forged", false, http.StatusForbidden},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := newTestAPI(t, Config{
				Auth:     stubAuth{complete: true, session: liveSession(), csrfOK: tc.csrfOK},
				Sessions: &stubSessions{complete: true, csrfOK: tc.csrfOK},
			})
			req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
			if tc.cookie != "" {
				req.AddCookie(&http.Cookie{Name: middleware.CookieCSRF, Value: tc.cookie})
			}
			if tc.header != "" {
				req.Header.Set(middleware.HeaderCSRF, tc.header)
			}
			rr := serve(a, req)

			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rr.Code, tc.wantStatus, rr.Body.String())
			}
		})
	}
}

// TestSetupPasswordLoopbackRule is D38 end to end, through the real adapter: a
// loopback caller claims with no token, a remote caller must present one, and a
// wrong token is refused.
func TestSetupPasswordLoopbackRule(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		remoteAddr string
		token      string
		wantStatus int
		wantCode   model.ErrorCode
	}{
		{"loopback, no token", "127.0.0.1:54321", "", http.StatusNoContent, ""},
		{"loopback over IPv6, no token", "[::1]:54321", "", http.StatusNoContent, ""},
		{"remote with the token", "192.168.1.20:54321", "the-token", http.StatusNoContent, ""},
		{"remote with no token", "192.168.1.20:54321", "", http.StatusForbidden, middleware.CodeSetupTokenRequired},
		{"remote with a wrong token", "192.168.1.20:54321", "nope", http.StatusForbidden, middleware.CodeSetupTokenRequired},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			sessions := &stubSessions{token: "the-token"}
			setup := &stubSetup{}
			a := newTestAPI(t, Config{
				Auth:     NewAuthenticator(sessions),
				Sessions: sessions,
				Setup:    setup,
			})

			req := httptest.NewRequest(http.MethodPost, "/api/v1/setup/password",
				strings.NewReader(`{"password":"a good password"}`))
			req.Header.Set("Content-Type", "application/json")
			req.RemoteAddr = tc.remoteAddr
			if tc.token != "" {
				req.Header.Set(middleware.HeaderSetupToken, tc.token)
			}
			rr := serve(a, req)

			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rr.Code, tc.wantStatus, rr.Body.String())
			}
			if tc.wantCode != "" {
				var env model.ErrorEnvelope
				if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
					t.Fatalf("decode the error envelope: %v", err)
				}
				if env.Error.Code != tc.wantCode {
					t.Fatalf("code = %q, want %q", env.Error.Code, tc.wantCode)
				}
				return
			}
			if cookieByName(rr, middleware.CookieSession) == nil {
				t.Error("a successful claim did not log the caller in")
			}
		})
	}
}

// TestSetupPasswordOnAClaimedHost: the gate answers `setup_already_claimed`,
// which is the opposite of `setup_required` and must not be confused with it.
func TestSetupPasswordOnAClaimedHost(t *testing.T) {
	t.Parallel()

	sessions := &stubSessions{complete: true}
	a := newTestAPI(t, Config{
		Auth:     NewAuthenticator(sessions),
		Sessions: sessions,
		Setup:    &stubSetup{state: model.SetupState{Claimed: true}},
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/setup/password",
		strings.NewReader(`{"password":"a good password"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:54321"
	rr := serve(a, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", rr.Code, rr.Body.String())
	}
	var env model.ErrorEnvelope
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode the error envelope: %v", err)
	}
	if env.Error.Code != middleware.CodeSetupAlreadyClaimed {
		t.Fatalf("code = %q, want %q", env.Error.Code, middleware.CodeSetupAlreadyClaimed)
	}
}

// TestSetupStateIsPublicAndSaysWhatTheSPANeeds.
func TestSetupState(t *testing.T) {
	t.Parallel()

	setup := &stubSetup{state: model.SetupState{
		Steps: []model.WizardStepView{
			{Step: model.StepPassword, State: model.WizardComplete},
			{Step: model.StepToolchain, State: model.WizardActive, Skippable: true},
			{Step: model.StepLlamacpp, State: model.WizardPending, Blocked: true},
		},
	}}
	a := newTestAPI(t, Config{Auth: stubAuth{}, Setup: setup})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/setup/state", nil)
	req.RemoteAddr = "192.168.1.20:33333"
	rr := serve(a, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	var body SetupStateDTO
	decode(t, rr, &body)

	if !body.TokenRequired {
		t.Error("a remote caller was told no setup token is required on an unclaimed host")
	}
	if len(body.Steps) != 3 {
		t.Fatalf("%d steps, want 3", len(body.Steps))
	}
	if body.ActiveStep == nil || *body.ActiveStep != string(model.StepToolchain) {
		t.Errorf("active_step = %v, want toolchain", body.ActiveStep)
	}
	if !body.Steps[1].Skippable || !body.Steps[2].Blocked {
		t.Errorf("the skippable and blocked flags did not survive the DTO: %+v", body.Steps)
	}
}

// TestSetupSkipErrorsMapToStatuses: an unknown step is a 400, a locked one a 409.
func TestSetupSkipErrorsMapToStatuses(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		err        error
		wantStatus int
	}{
		{"a step that does not exist", model.Error{Code: model.CodeWizardStepUnknown, Message: "no"}, http.StatusBadRequest},
		{"a step that may not be skipped", model.Error{Code: model.CodeWizardStepLocked, Message: "no"}, http.StatusConflict},
		{"a step that may be skipped", nil, http.StatusNoContent},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			setup := &stubSetup{skipErr: tc.err}
			a := newTestAPI(t, Config{
				Auth:     stubAuth{complete: true, session: liveSession(), csrfOK: true},
				Sessions: &stubSessions{complete: true, csrfOK: true},
				Setup:    setup,
			})

			req := httptest.NewRequest(http.MethodPost, "/api/v1/setup/skip",
				strings.NewReader(`{"step":"models"}`))
			req.Header.Set("Content-Type", "application/json")
			req.AddCookie(&http.Cookie{Name: middleware.CookieCSRF, Value: "csrf-token"})
			req.Header.Set(middleware.HeaderCSRF, "csrf-token")
			rr := serve(a, req)

			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rr.Code, tc.wantStatus, rr.Body.String())
			}
			if tc.err == nil && setup.skipped != model.StepModels {
				t.Fatalf("the handler skipped %q, want models", setup.skipped)
			}
		})
	}
}

// TestAuthRoutesWithoutAService: a build with no authentication service answers
// 503 rather than pretending, and the routes stay in the document.
func TestAuthRoutesWithoutAService(t *testing.T) {
	t.Parallel()

	a := newTestAPI(t, Config{Auth: stubAuth{complete: true}})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login",
		strings.NewReader(`{"password":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := serve(a, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body %s)", rr.Code, rr.Body.String())
	}
}
