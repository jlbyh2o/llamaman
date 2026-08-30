package gateway

import (
	"net/http"
	"strconv"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// The gateway's error envelope (DESIGN section 3.15).
//
// It is NOT section 3's `{"error":{"code","message","details"}}`. These ports
// are not the management API — they are the OpenAI-compatible front door — and
// section 3.15 is explicit about why the shape differs: "`401
// {"error":{"code":"invalid_api_key","message":"…"}}` in the
// OpenAI-compatible shape so SDKs surface a sensible message". A client here is
// an OpenAI SDK, not this project's UI, and the whole value of owning the public
// port is that such a client needs no special handling.
//
// `type` is carried alongside because every OpenAI SDK reads it and because the
// two fields section 3.15 names are a floor rather than a ceiling. Nothing here
// ever carries a `details` object: an unauthenticated caller must not be told
// whether a key was disabled, revoked or merely expired.

// The codes these ports answer with.
const (
	// CodeInvalidAPIKey is section 3.15's 401, for every credential refusal:
	// missing, unknown, disabled, revoked, expired and out of scope. One code,
	// because distinguishing them for an unauthenticated caller would be an
	// oracle; the distinction is kept in `gateway_denials_daily`, where the
	// operator can see it and an attacker cannot.
	CodeInvalidAPIKey model.ErrorCode = "invalid_api_key"
	// CodeRateLimitExceeded is the 429 for `api_tokens.rate_limit_rpm`. A rate
	// limit is not a credential failure, and an SDK that saw a 401 here would
	// tell the user to check their key.
	CodeRateLimitExceeded model.ErrorCode = "rate_limit_exceeded"
	// CodeInstanceNotRunning is section 9.1's 503 for an instance whose listener
	// is open but whose model is not loaded, and section 9.2's 502 for an
	// upstream that did not answer. A client hitting a stopped instance gets
	// this instead of connection-refused, which is far easier to debug.
	CodeInstanceNotRunning model.ErrorCode = "instance_not_running"
	// CodeUpstreamUnavailable is the 503/504 for a failure that is the
	// gateway's own — it could not verify credentials, or
	// `gateway.request_timeout_sec` expired.
	CodeUpstreamUnavailable model.ErrorCode = "upstream_unavailable"
	// CodeRequestTooLarge is the 413 for a body over `gateway.max_body_mb`.
	CodeRequestTooLarge model.ErrorCode = "request_too_large"
)

// errorEnvelope is the OpenAI-compatible body.
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

// errorTypeFor maps a code onto the OpenAI `type` an SDK branches on.
func errorTypeFor(code model.ErrorCode) string {
	switch code {
	case CodeInvalidAPIKey:
		return "invalid_request_error"
	case CodeRateLimitExceeded:
		return "rate_limit_error"
	case CodeRequestTooLarge:
		return "invalid_request_error"
	default:
		return "server_error"
	}
}

// writeGatewayError writes one refusal and returns how many bytes it wrote, so
// the caller can account for them like any other response.
//
// retryAfter, when positive, becomes the `Retry-After` header section 9.2 asks
// for on a loading instance and the one a rate limit earns.
func writeGatewayError(w http.ResponseWriter, status int, code model.ErrorCode,
	message string, retryAfter int) int64 {

	if retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	}
	return writeJSON(w, status, errorEnvelope{Error: errorBody{
		Message: message,
		Type:    errorTypeFor(code),
		Code:    string(code),
	}})
}
