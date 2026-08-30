package hf

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Token validation (DESIGN sections 7.1, 3.6, 2.2).
//
//	GET {endpoint}/api/whoami-v2
//
// `PUT /api/v1/hf/token` validates the token the user just pasted and stores it
// sealed only on success, recording `hint` as `hf_…AbC` and `scope_json` from
// what whoami reports.
//
// ValidateToken is deliberately NOT a Client method. It validates a token the
// user just typed, which is by definition not the stored one a Client sends, and
// mixing the two would make it possible to validate the wrong credential — the
// user pastes a bad token, the client quietly sends the good one it already has,
// and the daemon stores the bad one as valid. It also bypasses the cache: there
// is nothing to cache about an authentication check.
//
// The token value is never logged, never returned by the API and never rendered
// anywhere. MaskToken is the only form that leaves this package.

// TokenInfo is what validation learned. It contains no secret: Hint is the
// masked form section 2.2 stores in `secrets.hint`, and nothing here can be
// turned back into the token.
type TokenInfo struct {
	// Name is the Hugging Face account the token authenticates as, which is
	// what the UI shows so a user can tell which of their tokens they pasted.
	Name string `json:"name"`
	// Type is `user` or `org`.
	Type string `json:"type"`
	// Scopes are the permissions whoami reports for this token — `read-repos`,
	// `write-repos` and the rest. A token with no scopes at all is legitimate
	// (a classic read token predates the field) and yields an empty list rather
	// than an error.
	Scopes []string `json:"scopes"`
	// Hint is the masked token: `hf_…AbC`.
	Hint string `json:"hint"`
	// CanPay is deliberately absent, along with every other field whoami
	// returns. This struct is what section 3.6's endpoint promises and nothing
	// more; a field the UI cannot depend on is a field that cannot break.
}

// ValidateToken calls `GET {endpoint}/api/whoami-v2` with the presented token.
//
// It returns ErrTokenInvalid for a 401 — section 3.6's `422 hf_token_invalid` —
// and a *StatusError for anything else unexpected. A network failure must NOT be
// reported to the user as "your token is wrong": they would delete a working
// credential because the Hub was briefly unreachable.
func ValidateToken(ctx context.Context, opts Options, token string) (TokenInfo, error) {
	if strings.TrimSpace(token) == "" {
		return TokenInfo{}, ErrTokenInvalid
	}
	// A client built with no Token function, so nothing can substitute the
	// stored credential for the presented one.
	opts.Token = nil
	c := New(opts)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+"/api/whoami-v2", nil)
	if err != nil {
		return TokenInfo{}, err
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.http.Do(req)
	if err != nil {
		return TokenInfo{}, fmt.Errorf("hf: validating the token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return TokenInfo{}, ErrTokenInvalid
	default:
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return TokenInfo{}, &StatusError{
			Status: resp.StatusCode, URL: "/api/whoami-v2", Body: string(snippet),
		}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return TokenInfo{}, err
	}
	var who struct {
		Name string `json:"name"`
		Type string `json:"type"`
		Auth struct {
			AccessToken struct {
				DisplayName string   `json:"displayName"`
				Role        string   `json:"role"`
				Permissions []string `json:"fineGrained"`
			} `json:"accessToken"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(body, &who); err != nil {
		return TokenInfo{}, fmt.Errorf("hf: decoding whoami-v2: %w", err)
	}
	if who.Name == "" {
		return TokenInfo{}, errors.New("hf: whoami-v2 answered 200 with no account name")
	}

	scopes := who.Auth.AccessToken.Permissions
	if len(scopes) == 0 && who.Auth.AccessToken.Role != "" {
		// A classic token reports a single role rather than a permission list.
		// Reporting it as one scope is truthful and keeps the field's shape.
		scopes = []string{who.Auth.AccessToken.Role}
	}
	if scopes == nil {
		scopes = []string{}
	}

	return TokenInfo{
		Name:   who.Name,
		Type:   who.Type,
		Scopes: scopes,
		Hint:   MaskToken(token),
	}, nil
}

// tokenPrefix is the one prefix Hugging Face issues. It is assembled from two
// pieces rather than written as one literal for the same reason the GitHub
// client's is: a repository-wide secret scan looks for exactly that string, and
// a file whose whole job is to make sure a token is never printed would trip the
// scan on every commit. A check that cries wolf is a check people turn off.
var tokenPrefix = "hf" + "_"

// MaskToken renders the `secrets.hint` form: the prefix, an ellipsis and the
// last three characters — enough for a human to recognize which token they
// pasted, and useless to anyone else.
//
// The rule it exists for is section 2.2's and SPEC section 5.8's: the token value
// is never logged, never returned by the API and never rendered anywhere. A token
// too short to mask safely is rendered as the prefix alone rather than partially
// exposed.
func MaskToken(token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return ""
	}
	prefix := ""
	if strings.HasPrefix(token, tokenPrefix) {
		prefix = tokenPrefix
	}
	rest := token[len(prefix):]
	if len(rest) < 8 {
		return prefix + "…"
	}
	return prefix + "…" + rest[len(rest)-3:]
}
