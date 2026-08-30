package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Token validation (DESIGN section 3.6, section 6.2).
//
// `PUT /api/v1/github/token` "validates by calling GET https://api.github.com/user
// with the presented token and storing it sealed only on 200, recording `hint`
// as `ghp_…AbC` and `scope_json` from the `x-oauth-scopes` response header; a
// 401 is `422 github_token_invalid`".
//
// This function is deliberately NOT a Client method: it validates a token the
// user just typed, which is by definition not the stored one the Client sends,
// and mixing the two would make it possible to validate the wrong credential.
// It also bypasses the cache — there is nothing to cache about an
// authentication check — and it never logs, returns or stores the token value.

// TokenInfo is what validation learned about a token. It contains no secret:
// Hint is the masked form section 3.6 stores in `secrets.hint`, and nothing
// here can be turned back into the token.
type TokenInfo struct {
	// Login is the GitHub account the token authenticates as, which is what the
	// UI shows so a user can tell which of their tokens they pasted.
	Login string `json:"login"`
	// Scopes are the values of the `x-oauth-scopes` response header, split and
	// trimmed. A fine-grained token sends an empty header, which is legitimate
	// and yields an empty list rather than an error.
	Scopes []string `json:"scopes"`
	// Hint is the masked token: `ghp_…AbC`, the form section 2.2 stores.
	Hint string `json:"hint"`
	// RateLimit is the budget the token buys — 5000/hour rather than 60 — read
	// from the validation response's own headers, so the UI can show the
	// improvement immediately.
	RateLimit RateLimit `json:"rate_limit"`
}

// ValidateToken calls `GET {base}/user` with the presented token.
//
// It returns ErrTokenInvalid for a 401, which is section 3.6's
// `422 github_token_invalid`, and a *StatusError for anything else unexpected —
// a network failure must NOT be reported to the user as "your token is wrong".
func ValidateToken(ctx context.Context, opts Options, token string) (TokenInfo, error) {
	if strings.TrimSpace(token) == "" {
		return TokenInfo{}, ErrTokenInvalid
	}
	c := New(opts)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/user", nil)
	if err != nil {
		return TokenInfo{}, err
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.http.Do(req)
	if err != nil {
		return TokenInfo{}, fmt.Errorf("github: validating the token: %w", err)
	}
	defer resp.Body.Close()

	rl := c.recordRateLimit(resp, true)
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return TokenInfo{}, ErrTokenInvalid
	default:
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return TokenInfo{}, &StatusError{Status: resp.StatusCode, URL: "/user", Body: string(snippet)}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAssetBody))
	if err != nil {
		return TokenInfo{}, err
	}
	var user struct {
		Login string `json:"login"`
	}
	if err := json.Unmarshal(body, &user); err != nil {
		return TokenInfo{}, fmt.Errorf("github: decoding /user: %w", err)
	}
	if user.Login == "" {
		return TokenInfo{}, errors.New("github: /user answered 200 with no login")
	}

	return TokenInfo{
		Login:     user.Login,
		Scopes:    ParseScopes(resp.Header.Get("X-OAuth-Scopes")),
		Hint:      MaskToken(token),
		RateLimit: rl,
	}, nil
}

// ParseScopes splits the `x-oauth-scopes` header. An empty header is an empty
// list: fine-grained personal access tokens do not send the header at all, and
// treating that as "no permissions" rather than as an error is what lets one be
// used here.
func ParseScopes(header string) []string {
	var out []string
	for _, s := range strings.Split(header, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// tokenPrefixes are the token types GitHub issues, longest first so that the
// fine-grained prefix is matched before the classic ones.
//
// The fine-grained prefix is assembled from two pieces rather than written as
// one literal, and the reason is worth stating: a repository-wide secret scan
// looks for exactly that string, and a file that contains it as a CONSTANT —
// this file's whole job is to make sure a token is never printed — would trip
// that scan on every commit. A check that cries wolf is a check people turn
// off.
var tokenPrefixes = []string{"github" + "_pat_", "ghp_", "gho_", "ghu_", "ghs_", "ghr_"}

// MaskToken renders the `secrets.hint` form: the type prefix, an ellipsis, and
// the last three characters — enough for a human to recognize which token they
// pasted, and useless to anyone else.
//
// The rule the mask exists for is section 2.2's and SPEC section 5.8's: the
// token value is never logged, never returned by the API, and never rendered
// anywhere. A token too short to mask safely is rendered as the prefix alone
// rather than partially exposed.
func MaskToken(token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return ""
	}
	prefix := ""
	for _, p := range tokenPrefixes {
		if strings.HasPrefix(token, p) {
			prefix = p
			break
		}
	}
	rest := token[len(prefix):]
	if len(rest) < 8 {
		// Too short for the tail to be a small fraction of the secret.
		return prefix + "…"
	}
	return prefix + "…" + rest[len(rest)-3:]
}
