package hf

import (
	"context"
	"errors"
	"net/http"
	"net/url"
)

// The model card (DESIGN sections 7.1, 3.6).
//
//	GET {endpoint}/{repo}/raw/{rev}/README.md
//
// `/raw/` and not `/resolve/`: the card is a small git file, and `raw` serves it
// from the Hub itself rather than redirecting to the CDN. This is deliberately
// the one metadata read that is NOT cached — a card is read once when a user
// opens a model, and a 30-minute cache of a document nobody re-reads inside 30
// minutes buys nothing while holding a megabyte per model.
//
// What comes back is UNTRUSTED MARKDOWN written by a stranger. This function
// returns it raw and does not render it; `GET /api/v1/hf/card/{repo...}` answers
// with sanitized HTML (D35) produced by internal/mdrender, plus this raw text for
// the "view source" toggle. Nothing in this package interprets a byte of it.

// CardNotFound is not an error worth distinguishing at the call site: a
// repository with no README is ordinary, and section 3.6's endpoint answers with
// an empty card rather than a 404 for the model itself.

// Card fetches a repository's README at one revision.
//
// A missing README returns ("", nil): plenty of good repositories have none, and
// a 404 on the card is not a 404 on the model. Every other failure — a gate, a
// private repository, a transport error — is returned, because those are things
// the user can act on.
func (c *Client) Card(ctx context.Context, repo, revision string) (string, error) {
	if err := validateRepo(repo); err != nil {
		return "", err
	}
	rev := revision
	if rev == "" {
		rev = "main"
	}
	if err := validateRevision(rev); err != nil {
		return "", err
	}

	raw := c.endpoint + "/" + repo + "/raw/" + url.PathEscape(rev) + "/README.md"

	resp, err := c.do(ctx, request{
		method: http.MethodGet, url: raw, repo: repo, maxBody: maxCardBody,
		header: http.Header{"Accept": []string{"text/plain, text/markdown"}},
	})
	if err != nil {
		if isNotFound(err) {
			return "", nil
		}
		return "", err
	}
	return string(resp.body), nil
}

func isNotFound(err error) bool {
	return errors.Is(err, ErrNotFound)
}
