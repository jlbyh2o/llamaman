package github

import (
	"errors"
	"fmt"
	"net/http"
	"time"
)

// ErrNotFound is a 404 from api.github.com: the repository, the release or the
// asset does not exist. It is a real answer and never served stale.
var ErrNotFound = errors.New("github: not found")

// ErrRateLimited is a 403/429 whose headers say the hourly budget is spent and
// for which no cached body exists to serve instead.
//
// Section 6.2 is explicit about what this means to a user: unauthenticated
// api.github.com allows 60 requests per hour per IP, and an optional
// user-supplied token raises that to 5000. So the error carries the reset time
// and whether the request was authenticated, because "wait 34 minutes" and
// "add a GitHub token in Settings → Builds" are different remedies and the UI
// must be able to say which one applies.
type ErrRateLimit struct {
	RateLimit RateLimit
}

func (e *ErrRateLimit) Error() string {
	who := "unauthenticated"
	if e.RateLimit.Authenticated {
		who = "authenticated"
	}
	if e.RateLimit.ResetAt.IsZero() {
		return fmt.Sprintf("github: %s rate limit exhausted", who)
	}
	return fmt.Sprintf("github: %s rate limit exhausted, resets at %s",
		who, e.RateLimit.ResetAt.UTC().Format(time.RFC3339))
}

// ErrTokenInvalid is the 401 from `GET https://api.github.com/user` with a
// presented token, which section 3.6 maps to `422 github_token_invalid`. It is
// a distinct type so the API layer never has to match on an error string.
var ErrTokenInvalid = errors.New("github: token rejected")

// StatusError is any other unexpected HTTP status.
type StatusError struct {
	Status int
	URL    string
	Body   string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("github: %s: unexpected status %d %s", e.URL, e.Status, http.StatusText(e.Status))
}

// RateLimit is what api.github.com's own headers report about the caller's
// budget. It is section 3.6's `rate_limit` object on `GET /api/v1/github/token`
// and section 6.2's on `GET /api/v1/llamacpp/releases`.
type RateLimit struct {
	Limit     int       `json:"limit"`
	Remaining int       `json:"remaining"`
	ResetAt   time.Time `json:"reset_at"`
	// Authenticated is whether the request that produced these numbers carried
	// a token. It is the difference between a 60/hour budget and a 5000/hour
	// one, which is the entire reason the token setting exists.
	Authenticated bool `json:"authenticated"`
	// Known is false before any request has been made, so a UI can say
	// "unknown" rather than "0 remaining" on a daemon that has not called out
	// yet — the same never-report-zero-for-unknown rule internal/hw follows.
	Known bool `json:"known"`
}
