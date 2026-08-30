package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/jlbyh2o/llamaman/internal/api/middleware"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
	"github.com/jlbyh2o/llamaman/internal/tokens"
)

// Handler tests for the token endpoints of DESIGN section 3.12.
//
// The service is stubbed, because what this layer owns is the wire contract:
// which body shape becomes which service call, which status each outcome maps
// to, and — the rule that matters most here — that the secret appears in exactly
// one response and nowhere else.

type stubTokens struct {
	list  []tokens.Token
	one   tokens.Token
	mint  tokens.Minted
	usage []store.TokenUsageRow
	err   error

	gotID     string
	gotMint   tokens.MintParams
	gotPatch  tokens.PatchParams
	gotRange  store.UsageRange
	revoked   string
	listCalls int
}

func (s *stubTokens) List(context.Context) ([]tokens.Token, error) {
	s.listCalls++
	return s.list, s.err
}

func (s *stubTokens) Get(_ context.Context, id string) (tokens.Token, error) {
	s.gotID = id
	return s.one, s.err
}

func (s *stubTokens) Mint(_ context.Context, p tokens.MintParams) (tokens.Minted, error) {
	s.gotMint = p
	return s.mint, s.err
}

func (s *stubTokens) Patch(_ context.Context, id string, p tokens.PatchParams) (tokens.Token, error) {
	s.gotID, s.gotPatch = id, p
	return s.one, s.err
}

func (s *stubTokens) Revoke(_ context.Context, id string) error {
	s.revoked = id
	return s.err
}

func (s *stubTokens) Usage(_ context.Context, id string, rng store.UsageRange) ([]store.TokenUsageRow, error) {
	s.gotID, s.gotRange = id, rng
	return s.usage, s.err
}

type stubGateway struct {
	rows       []store.DenialRow
	err        error
	gotID      string
	gotRange   store.UsageRange
	denialCall int
}

func (s *stubGateway) Denials(_ context.Context, id string, rng store.UsageRange) ([]store.DenialRow, error) {
	s.denialCall++
	s.gotID, s.gotRange = id, rng
	return s.rows, s.err
}

func sampleToken() tokens.Token {
	rpm := int64(600)
	used := int64(1_700_000_500_000)
	ip := "192.0.2.7"
	return tokens.Token{
		ID:           "01JTOKEN00000000000000000A",
		Name:         "laptop",
		Prefix:       "lm_ab12cd",
		Scope:        model.ScopeInstances,
		State:        model.TokenActive,
		InstanceIDs:  []string{"01JQWEN"},
		RateLimitRPM: &rpm,
		CreatedAt:    1_700_000_000_000,
		UpdatedAt:    1_700_000_100_000,
		LastUsedAt:   &used,
		LastUsedIP:   &ip,
		RequestCount: 91,
	}
}

func tokenAPI(t *testing.T, svc APITokenService, gw GatewayService) *API {
	t.Helper()
	return newTestAPI(t, Config{
		Auth: stubAuth{
			complete: true,
			session:  &middleware.Session{ID: "s-1"},
			csrfOK:   true,
		},
		APITokens: svc,
		Gateway:   gw,
	})
}

func TestListTokensNeverCarriesASecret(t *testing.T) {
	svc := &stubTokens{list: []tokens.Token{sampleToken()}}
	w := do(t, tokenAPI(t, svc, nil), http.MethodGet, "/api/v1/tokens", "")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, `"secret"`) {
		t.Errorf("the listing carries a `secret` field:\n%s", body)
	}

	var got List[APITokenDTO]
	decode(t, w, &got)
	if len(got.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(got.Items))
	}
	item := got.Items[0]
	if item.Prefix != "lm_ab12cd" {
		t.Errorf("prefix = %q", item.Prefix)
	}
	if item.Hint != "lm_ab12cd…" {
		t.Errorf("hint = %q, want the prefix and an ellipsis — the rest of the secret does "+
			"not exist anywhere", item.Hint)
	}
	if item.Scope != string(model.ScopeInstances) || item.State != string(model.TokenActive) {
		t.Errorf("scope/state = %q/%q", item.Scope, item.State)
	}
	if item.RequestCount != 91 {
		t.Errorf("request_count = %d, want 91", item.RequestCount)
	}
	if item.LastUsedAt == nil || *item.LastUsedAt != Time(1_700_000_500_000) {
		t.Errorf("last_used_at = %v, want the RFC 3339 form", item.LastUsedAt)
	}
	if item.RevokedAt != nil {
		t.Errorf("revoked_at = %v on an active token, want null", *item.RevokedAt)
	}
}

// TestCreateTokenIsTheOnlyResponseWithASecret is section 3.12's one rule that
// cannot be recovered from if it is broken: the secret is shown once.
func TestCreateTokenIsTheOnlyResponseWithASecret(t *testing.T) {
	svc := &stubTokens{mint: tokens.Minted{
		Token:  sampleToken(),
		Secret: "lm_not-a-real-secret-value",
	}}
	a := tokenAPI(t, svc, nil)

	w := do(t, a, http.MethodPost, "/api/v1/tokens",
		`{"name":"laptop","scope":"instances","instance_ids":["01JQWEN"],`+
			`"rate_limit_rpm":600,"expires_at":"2026-06-01T00:00:00Z"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%s)", w.Code, w.Body.String())
	}

	var got CreateAPITokenResponse
	decode(t, w, &got)
	if got.Secret != "lm_not-a-real-secret-value" {
		t.Errorf("secret = %q", got.Secret)
	}
	if got.Hint == "" || strings.Contains(got.Hint, "not-a-real") {
		t.Errorf("hint = %q, want the masked form", got.Hint)
	}

	// The body reached the service intact, including the parsed expiry.
	if svc.gotMint.Name != "laptop" || svc.gotMint.Scope != model.ScopeInstances {
		t.Errorf("mint params = %+v", svc.gotMint)
	}
	if len(svc.gotMint.InstanceIDs) != 1 || svc.gotMint.InstanceIDs[0] != "01JQWEN" {
		t.Errorf("instance_ids = %v", svc.gotMint.InstanceIDs)
	}
	if svc.gotMint.RateLimitRPM == nil || *svc.gotMint.RateLimitRPM != 600 {
		t.Errorf("rate_limit_rpm = %v", svc.gotMint.RateLimitRPM)
	}
	if svc.gotMint.ExpiresAt == nil || *svc.gotMint.ExpiresAt != 1_780_272_000_000 {
		t.Errorf("expires_at = %v, want the millisecond form of 2026-06-01T00:00:00Z",
			svc.gotMint.ExpiresAt)
	}
}

// TestCreateTokenInfersTheScopeFromInstanceIDs: a body that named instances but
// no scope meant `instances`. Minting a global token from it would silently
// grant more than was asked for.
func TestCreateTokenInfersTheScopeFromInstanceIDs(t *testing.T) {
	svc := &stubTokens{mint: tokens.Minted{Token: sampleToken(), Secret: "lm_x"}}
	do(t, tokenAPI(t, svc, nil), http.MethodPost, "/api/v1/tokens",
		`{"name":"scoped","instance_ids":["01JQWEN"]}`)

	if svc.gotMint.Scope != model.ScopeInstances {
		t.Errorf("scope = %q, want instances", svc.gotMint.Scope)
	}

	svc2 := &stubTokens{mint: tokens.Minted{Token: sampleToken(), Secret: "lm_x"}}
	do(t, tokenAPI(t, svc2, nil), http.MethodPost, "/api/v1/tokens", `{"name":"plain"}`)
	if svc2.gotMint.Scope != model.ScopeGlobal {
		t.Errorf("scope = %q, want global", svc2.gotMint.Scope)
	}
}

func TestCreateTokenRefusalsKeepTheirCodes(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   model.ErrorCode
	}{
		{
			name:   "no name",
			err:    model.Error{Code: tokens.CodeTokenNameRequired, Message: "a token needs a name"},
			status: http.StatusUnprocessableEntity,
			code:   tokens.CodeTokenNameRequired,
		},
		{
			name:   "a scope that reaches nothing",
			err:    model.Error{Code: tokens.CodeTokenScopeInvalid, Message: "name at least one"},
			status: http.StatusUnprocessableEntity,
			code:   tokens.CodeTokenScopeInvalid,
		},
		{
			name:   "a negative rate limit",
			err:    model.Error{Code: tokens.CodeTokenRateLimitInvalid, Message: "no"},
			status: http.StatusUnprocessableEntity,
			code:   tokens.CodeTokenRateLimitInvalid,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := &stubTokens{err: tc.err}
			w := do(t, tokenAPI(t, svc, nil), http.MethodPost, "/api/v1/tokens", `{"name":"x"}`)
			if w.Code != tc.status {
				t.Errorf("status = %d, want %d", w.Code, tc.status)
			}
			if got := errorCode(t, w); got != string(tc.code) {
				t.Errorf("code = %q, want %q", got, tc.code)
			}
		})
	}
}

func TestCreateTokenRejectsABadExpiry(t *testing.T) {
	svc := &stubTokens{mint: tokens.Minted{Token: sampleToken(), Secret: "lm_x"}}
	w := do(t, tokenAPI(t, svc, nil), http.MethodPost, "/api/v1/tokens",
		`{"name":"x","expires_at":"next tuesday"}`)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if got := errorCode(t, w); got != string(CodeBadRequest) {
		t.Errorf("code = %q, want %q", got, CodeBadRequest)
	}
}

func TestPatchTokenMapsTheBody(t *testing.T) {
	svc := &stubTokens{one: sampleToken()}
	a := tokenAPI(t, svc, nil)

	w := do(t, a, http.MethodPatch, "/api/v1/tokens/01JTOKEN00000000000000000A",
		`{"name":"desktop","state":"disabled","instance_ids":[],"rate_limit_rpm":0,"expires_at":""}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if svc.gotID != "01JTOKEN00000000000000000A" {
		t.Errorf("id = %q", svc.gotID)
	}
	p := svc.gotPatch
	if p.Name == nil || *p.Name != "desktop" {
		t.Errorf("name = %v", p.Name)
	}
	if p.State == nil || *p.State != model.TokenDisabled {
		t.Errorf("state = %v", p.State)
	}
	// An explicitly empty list is "make it global", which is different from
	// omitting the field.
	if p.InstanceIDs == nil || len(*p.InstanceIDs) != 0 {
		t.Errorf("instance_ids = %v, want an explicit empty list", p.InstanceIDs)
	}
	if p.RateLimitRPM == nil || *p.RateLimitRPM == nil || **p.RateLimitRPM != 0 {
		t.Errorf("rate_limit_rpm = %v, want an explicit 0", p.RateLimitRPM)
	}
	// An empty string removes the expiry.
	if p.ExpiresAt == nil || *p.ExpiresAt != nil {
		t.Errorf("expires_at = %v, want an explicit clear", p.ExpiresAt)
	}
}

func TestPatchTokenLeavesOmittedFieldsAlone(t *testing.T) {
	svc := &stubTokens{one: sampleToken()}
	do(t, tokenAPI(t, svc, nil), http.MethodPatch,
		"/api/v1/tokens/01JTOKEN00000000000000000A", `{"name":"only the name"}`)

	p := svc.gotPatch
	if p.State != nil || p.InstanceIDs != nil || p.RateLimitRPM != nil || p.ExpiresAt != nil {
		t.Errorf("an omitted field became an instruction: %+v", p)
	}
}

// TestPatchARevokedTokenIsA409: revoked is terminal, and an admin who believes
// they just re-enabled a leaked credential must be told they did not.
func TestPatchARevokedTokenIsA409(t *testing.T) {
	svc := &stubTokens{err: model.Error{
		Code: tokens.CodeTokenRevoked, Message: "this token is revoked",
	}}
	w := do(t, tokenAPI(t, svc, nil), http.MethodPatch,
		"/api/v1/tokens/01JTOKEN00000000000000000A", `{"state":"active"}`)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
	if got := errorCode(t, w); got != string(tokens.CodeTokenRevoked) {
		t.Errorf("code = %q, want %q", got, tokens.CodeTokenRevoked)
	}
}

func TestDeleteTokenRevokes(t *testing.T) {
	svc := &stubTokens{}
	w := do(t, tokenAPI(t, svc, nil), http.MethodDelete,
		"/api/v1/tokens/01JTOKEN00000000000000000A", "")

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}
	if svc.revoked != "01JTOKEN00000000000000000A" {
		t.Errorf("revoked = %q", svc.revoked)
	}
	if w.Body.Len() != 0 {
		t.Errorf("a 204 carried a body: %q", w.Body.String())
	}
}

func TestUnknownTokenIsA404(t *testing.T) {
	for _, tc := range []struct{ method, target, body string }{
		{http.MethodGet, "/api/v1/tokens/01JNOSUCH", ""},
		{http.MethodPatch, "/api/v1/tokens/01JNOSUCH", `{"name":"x"}`},
		{http.MethodDelete, "/api/v1/tokens/01JNOSUCH", ""},
		{http.MethodGet, "/api/v1/tokens/01JNOSUCH/usage", ""},
	} {
		svc := &stubTokens{err: store.ErrNotFound}
		w := do(t, tokenAPI(t, svc, nil), tc.method, tc.target, tc.body)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404", tc.method, tc.target, w.Code)
		}
	}
}

// TestTokenUsageKeepsNullsNull: NULL means the upstream did not report the
// counts, and the UI says "not reported" rather than showing a zero that reads
// as an answer (§9.3, F14).
func TestTokenUsageKeepsNullsNull(t *testing.T) {
	prompt := int64(120)
	svc := &stubTokens{usage: []store.TokenUsageRow{
		{
			TokenID: "01JTOKEN00000000000000000A", InstanceID: "01JQWEN", Day: "2026-03-01",
			Requests: 9, Errors: 1, BytesIn: 400, BytesOut: 9000,
			PromptTokens: &prompt, DurationMS: 1234,
		},
	}}
	w := do(t, tokenAPI(t, svc, nil), http.MethodGet,
		"/api/v1/tokens/01JTOKEN00000000000000000A/usage?from=2026-03-01&to=2026-03-31", "")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if svc.gotRange != (store.UsageRange{From: "2026-03-01", To: "2026-03-31"}) {
		t.Errorf("range = %+v", svc.gotRange)
	}

	var got List[TokenUsageDTO]
	decode(t, w, &got)
	if len(got.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(got.Items))
	}
	row := got.Items[0]
	if row.PromptTokens == nil || *row.PromptTokens != 120 {
		t.Errorf("prompt_tokens = %v, want 120", row.PromptTokens)
	}
	if row.CompletionTokens != nil {
		t.Errorf("completion_tokens = %v, want null", *row.CompletionTokens)
	}
	if !strings.Contains(w.Body.String(), `"completion_tokens":null`) {
		t.Errorf("an unreported count did not serialize as null:\n%s", w.Body.String())
	}
}

// TestUsageRangeIgnoresUnparseableDays: a dashboard that showed nothing because
// a bookmark carried `from=last-week` would be worse than one that showed
// everything.
func TestUsageRangeIgnoresUnparseableDays(t *testing.T) {
	svc := &stubTokens{}
	do(t, tokenAPI(t, svc, nil), http.MethodGet,
		"/api/v1/tokens/01JTOKEN00000000000000000A/usage?from=last-week&to=", "")

	if svc.gotRange != (store.UsageRange{}) {
		t.Errorf("range = %+v, want unbounded", svc.gotRange)
	}
}

func TestGatewayDenials(t *testing.T) {
	gw := &stubGateway{rows: []store.DenialRow{
		{InstanceID: "01JQWEN", Day: "2026-03-01", Reason: model.DeniedUnknown, Count: 12},
	}}
	w := do(t, tokenAPI(t, &stubTokens{}, gw), http.MethodGet,
		"/api/v1/gateway/denials?instance_id=01JQWEN&from=2026-03-01", "")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if gw.gotID != "01JQWEN" || gw.gotRange.From != "2026-03-01" {
		t.Errorf("filters = %q %+v", gw.gotID, gw.gotRange)
	}

	var got List[GatewayDenialDTO]
	decode(t, w, &got)
	if len(got.Items) != 1 || got.Items[0].Reason != string(model.DeniedUnknown) {
		t.Errorf("items = %+v", got.Items)
	}
}

// TestTokenRoutesWithoutTheirServices: a documented endpoint whose subsystem is
// nil reports the gap rather than faking an answer.
func TestTokenRoutesWithoutTheirServices(t *testing.T) {
	a := newTestAPI(t, Config{Auth: stubAuth{
		complete: true, session: &middleware.Session{ID: "s-1"}, csrfOK: true,
	}})

	for _, tc := range []struct{ method, target, body string }{
		{http.MethodGet, "/api/v1/tokens", ""},
		{http.MethodPost, "/api/v1/tokens", `{"name":"x"}`},
		{http.MethodGet, "/api/v1/tokens/01JX", ""},
		{http.MethodPatch, "/api/v1/tokens/01JX", `{"name":"x"}`},
		{http.MethodDelete, "/api/v1/tokens/01JX", ""},
		{http.MethodGet, "/api/v1/tokens/01JX/usage", ""},
		{http.MethodGet, "/api/v1/gateway/denials", ""},
	} {
		w := do(t, a, tc.method, tc.target, tc.body)
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s = %d, want 503", tc.method, tc.target, w.Code)
		}
	}
}

// TestCreateTokenRejectsUnknownFields is the request-side mirror of D43: a
// client that misspells a field should be told, not silently ignored.
func TestCreateTokenRejectsUnknownFields(t *testing.T) {
	svc := &stubTokens{mint: tokens.Minted{Token: sampleToken(), Secret: "lm_x"}}
	w := do(t, tokenAPI(t, svc, nil), http.MethodPost, "/api/v1/tokens",
		`{"name":"x","instances_ids":["01JQWEN"]}`)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}
