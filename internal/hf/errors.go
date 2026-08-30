package hf

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// The failures this client names (DESIGN sections 7.1, 3.6).
//
// Four of them are the same HTTP status wearing different meanings, and telling
// them apart is the whole of section 3.6's gated-repo UX:
//
//	401/403 + `x-error-code: GatedRepo`   → hf_gated: the repo exists, the user
//	                                        must accept its terms IN A BROWSER
//	401/403 + `x-error-code: RepoNotFound`
//	  with no token stored                → private, sign in
//	  with a token stored                 → this token cannot see it
//	401 on any call with a token          → the token is invalid or revoked
//
// Answering all four as "403 forbidden" would leave a user staring at a repo
// page that works in their browser with no idea what to do, which is exactly the
// state SPEC section 3.2's "one click from search to running" cannot afford.

var (
	// ErrNotFound is a 404 from the Hub: no such repository, revision or file.
	ErrNotFound = errors.New("hf: not found")

	// ErrTokenInvalid is a 401 on a request that carried a token. It is section
	// 3.6's `422 hf_token_invalid` on `PUT /hf/token`, and on any other call it
	// means the stored token was revoked while the daemon was running.
	ErrTokenInvalid = errors.New("hf: the token is invalid or has been revoked")

	// ErrNoRange is returned when an origin answers a resumed transfer with
	// `200` instead of `206`. It is not a failure the caller reports: section
	// 7.4 discards the partial, clears the stale validator and restarts, and
	// this value is how the transfer says which of the two happened.
	ErrNoRange = errors.New("hf: the origin ignored the Range request")
)

// GatedError is a repository whose files are behind an access grant. It carries
// the two fields section 3.6 puts in the `403 hf_gated` body, and nothing else:
// the UI's whole job here is to link out, because grants are browser-only on
// Hugging Face's side and there is no API this daemon could call on the user's
// behalf.
type GatedError struct {
	// Repo is the repository id, `bartowski/Qwen3-8B-GGUF`.
	Repo string
	// RequestURL is the page a human accepts the terms on. It is the repository
	// page itself: HF renders the "Agree and access" form there, and deep
	// linking to a form path that HF is free to move would age badly.
	RequestURL string
	// Status is the status the Hub actually answered with — 401 without a token,
	// 403 with one — kept for the log rather than for the user.
	Status int
}

func (e *GatedError) Error() string {
	return fmt.Sprintf("hf: %s is a gated repository; access must be granted at %s",
		e.Repo, e.RequestURL)
}

// PrivateError is a repository the caller cannot see: HF answers `RepoNotFound`
// for a private repo rather than disclosing that it exists, so this is the same
// response as a genuine 404 and only the token's presence tells them apart.
//
// It is a distinct type because the two remedies are different sentences: "sign
// in" when no token is stored, and "this token cannot see that repository" when
// one is.
type PrivateError struct {
	Repo string
	// HaveToken records whether a token was sent. False means "sign in"; true
	// means "the stored token does not grant access".
	HaveToken bool
	Status    int
}

func (e *PrivateError) Error() string {
	if e.HaveToken {
		return fmt.Sprintf("hf: %s is private or does not exist, and the stored token cannot see it", e.Repo)
	}
	return fmt.Sprintf("hf: %s is private or does not exist; sign in to access it", e.Repo)
}

// RateLimitError is a 429 that survived every retry. RetryAfter is the
// `Retry-After` header when the Hub sent one, and zero when it did not.
type RateLimitError struct {
	RetryAfter time.Duration
	URL        string
}

func (e *RateLimitError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("hf: rate limited by the Hub; retry after %s", e.RetryAfter)
	}
	return "hf: rate limited by the Hub"
}

// StatusError is any other unexpected status. Body is a bounded snippet, for the
// log — it is never rendered to a user, because a Hub error body is not a string
// this product has vetted.
type StatusError struct {
	Status int
	URL    string
	Body   string
}

func (e *StatusError) Error() string {
	if e.Body != "" {
		return fmt.Sprintf("hf: %s answered %d: %s", e.URL, e.Status, e.Body)
	}
	return fmt.Sprintf("hf: %s answered %d", e.URL, e.Status)
}

// SizeMismatchError is section 7.4's `size_mismatch`: a `Content-Range` whose
// total does not equal the size recorded when the download was planned. It is
// fatal for the task rather than retryable — the file upstream is not the file
// this download was sized against, and re-requesting it would produce the same
// contradiction.
type SizeMismatchError struct {
	Expected int64
	Got      int64
	URL      string
}

func (e *SizeMismatchError) Error() string {
	return fmt.Sprintf("hf: %s reports %d bytes where %d were expected", e.URL, e.Got, e.Expected)
}

// The `x-error-code` values the Hub sends. They are the authoritative signal —
// far more reliable than parsing an error body — and section 7.1 names both.
const (
	errorCodeHeader   = "X-Error-Code"
	errorCodeGated    = "GatedRepo"
	errorCodeNotFound = "RepoNotFound"
	// errorCodeGatedAlt is what the Hub sends for a repo whose grant is pending
	// review rather than never requested. Both are "you cannot read this until
	// a human on the other side acts", which is one message to a user.
	errorCodeGatedAlt = "GatedRepoAccessRequestPending"
)

// classifyAccess turns a 401/403 into the specific error the UI can act on.
// haveToken is what separates "sign in" from "this token is not enough", and
// endpoint + repo build the browser URL the gated case links out to.
func classifyAccess(status int, header http.Header, endpoint, repo string, haveToken bool) error {
	code := strings.TrimSpace(header.Get(errorCodeHeader))
	switch {
	case strings.EqualFold(code, errorCodeGated), strings.EqualFold(code, errorCodeGatedAlt):
		return &GatedError{Repo: repo, RequestURL: endpoint + "/" + repo, Status: status}
	case strings.EqualFold(code, errorCodeNotFound):
		return &PrivateError{Repo: repo, HaveToken: haveToken, Status: status}
	case status == http.StatusUnauthorized && haveToken:
		// A 401 on a request that carried a credential is a statement about the
		// credential. Without one it is a statement about the resource, which
		// the RepoNotFound branch above already covers and this one must not
		// mislabel.
		return ErrTokenInvalid
	default:
		// No `x-error-code` at all. A 403 with a token is most likely a grant
		// this token does not carry; without one it is the sign-in case. Either
		// way `PrivateError` says something true and actionable, where a bare
		// "403 forbidden" would not.
		return &PrivateError{Repo: repo, HaveToken: haveToken, Status: status}
	}
}

// IsGated reports whether err is a gated-repository refusal, and returns it.
// It is the shape `POST /api/v1/downloads` and the three metadata endpoints all
// need in order to answer `403 hf_gated` with `{"repo","request_url"}`.
func IsGated(err error) (*GatedError, bool) {
	var g *GatedError
	if errors.As(err, &g) {
		return g, true
	}
	return nil, false
}

// IsPrivate reports whether err is the private-or-absent refusal.
func IsPrivate(err error) (*PrivateError, bool) {
	var p *PrivateError
	if errors.As(err, &p) {
		return p, true
	}
	return nil, false
}
