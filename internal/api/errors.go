package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/jlbyh2o/llamaman/internal/api/middleware"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// The error codes this layer owns, beyond the ones internal/model closes a
// column with and the ones internal/api/middleware writes. They follow the
// precedent internal/sse set with `invalid_topic`: a code is declared beside
// the code path that returns it, and DESIGN section 3's catalog grows as the
// endpoints that answer with it arrive.
const (
	// CodeNotFound is the 404 for a path no route matches, and the one a
	// handler returns for an id that names no row.
	CodeNotFound model.ErrorCode = "not_found"
	// CodeMethodNotAllowed is the 405 for a path that exists under other
	// methods. The response carries an `Allow` header.
	CodeMethodNotAllowed model.ErrorCode = "method_not_allowed"
	// CodeBadRequest is the 400 for a body that is not valid JSON, carries an
	// unknown field, or is too large. A body that parses but fails a domain
	// rule gets that rule's own code instead.
	CodeBadRequest model.ErrorCode = "bad_request"
	// CodeUnsupportedMediaType is the 415 for a request body sent as anything
	// other than JSON — or, on a `setup` route, with no Content-Type at all.
	// It is the middleware's constant because both layers answer with it.
	CodeUnsupportedMediaType = middleware.CodeUnsupportedMediaType
	// CodeInternalError is the 500 of last resort. It is deliberately the same
	// string internal/jobs writes into `jobs.error_code` for an unhandled
	// worker error: one name for "we do not know what went wrong".
	CodeInternalError = middleware.CodeInternalError

	// CodeHFTokenInvalid and CodeGitHubTokenInvalid are the 422s of section
	// 3.6's two credential PUTs: the PROVIDER refused the presented token.
	//
	// They live in this package rather than in a client package because the
	// refusal is this layer's reading of a 401 from somewhere else. A network
	// failure is deliberately NOT one of them — telling a user their working
	// token is wrong because the Hub was briefly unreachable makes them delete
	// a good credential.
	CodeHFTokenInvalid     model.ErrorCode = "hf_token_invalid"
	CodeGitHubTokenInvalid model.ErrorCode = "github_token_invalid"
)

// Error is one API error with the status it is answered with. It is the type a
// handler returns; WriteError renders it into section 3's envelope.
//
// It exists alongside model.Error rather than replacing it because the two
// answer different questions. model.Error is the wire shape and the domain's
// own error value — a service returns it without knowing anything about HTTP.
// This type is that value plus the status the API chose for it, which is a
// decision only this layer can make.
type Error struct {
	Status  int
	Code    model.ErrorCode
	Message string
	Details map[string]any
	// Err is the cause, for the log. It is never sent to the client: an
	// internal error message is exactly the kind of detail that leaks paths
	// and query text to whoever provoked it.
	Err error
}

// Error implements the error interface.
func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Err)
	}
	return string(e.Code) + ": " + e.Message
}

// Unwrap exposes the cause to errors.Is and errors.As.
func (e *Error) Unwrap() error { return e.Err }

// Errorf builds an *Error with a formatted message.
func Errorf(status int, code model.ErrorCode, format string, args ...any) *Error {
	return &Error{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}

// WithDetails returns e with the details map section 3's envelope carries. It
// mutates and returns the receiver so it composes on one line at a return site.
func (e *Error) WithDetails(d map[string]any) *Error {
	e.Details = d
	return e
}

// WithCause returns e carrying the underlying error, for the log only.
func (e *Error) WithCause(err error) *Error {
	e.Err = err
	return e
}

// NotFound, BadRequest and Conflict are the three shapes handlers reach for
// most often.
func NotFound(format string, args ...any) *Error {
	return Errorf(http.StatusNotFound, CodeNotFound, format, args...)
}

// BadRequest is the 400 for a malformed request this layer can name.
func BadRequest(format string, args ...any) *Error {
	return Errorf(http.StatusBadRequest, CodeBadRequest, format, args...)
}

// Conflict is the 409 shape section 3 uses for every guard that refuses a
// state change — `model_in_use`, `job_in_flight`, `setup_required` and the
// rest — so the code is the caller's to supply.
func Conflict(code model.ErrorCode, format string, args ...any) *Error {
	return Errorf(http.StatusConflict, code, format, args...)
}

// WriteError renders err as section 3's envelope with a matching status. It is
// the ONE place a handler's error becomes a response, and it recognizes exactly
// four shapes, in order:
//
//  1. *api.Error — a status this layer already chose.
//  2. model.Error — a domain error carrying a closed-enum code but no status.
//     The status comes from statusForCode below, so a service can return
//     `model_in_use` without importing net/http.
//  3. store.ErrNotFound — "no such row" is a domain answer (its own doc
//     comment says so), and 404 is what it means on the wire.
//  4. anything else — a 500 whose message is a constant. The real error is
//     logged; it is never sent, because an unclassified error is by definition
//     one nobody has vetted for what it discloses.
func WriteError(w http.ResponseWriter, r *http.Request, log *slog.Logger, err error) {
	if log == nil {
		log = slog.Default()
	}

	var apiErr *Error
	switch {
	case errors.As(err, &apiErr):
		// keep it

	case errors.Is(err, store.ErrNotFound):
		apiErr = &Error{Status: http.StatusNotFound, Code: CodeNotFound,
			Message: "not found", Err: err}

	default:
		var me model.Error
		if errors.As(err, &me) {
			apiErr = &Error{Status: statusForCode(me.Code), Code: me.Code,
				Message: me.Message, Details: me.Details, Err: err}
		} else {
			apiErr = &Error{Status: http.StatusInternalServerError, Code: CodeInternalError,
				Message: "internal error", Err: err}
		}
	}

	if apiErr.Status >= 500 {
		attrs := []any{"code", apiErr.Code, "error", apiErr.Err, "method", r.Method}
		if op, ok := middleware.RouteFrom(r.Context()); ok {
			attrs = append(attrs, "route", op)
		} else {
			attrs = append(attrs, "path", r.URL.Path)
		}
		log.Error("api error", attrs...)
	}

	middleware.WriteError(w, apiErr.Status, apiErr.Code, apiErr.Message, apiErr.Details)
}

// statusForCode maps the codes a domain package can return without knowing
// about HTTP onto the statuses DESIGN section 3 pairs them with. A code with no
// entry gets 500, which is the honest answer: an unmapped code means this layer
// has not been taught what the domain meant by it.
func statusForCode(c model.ErrorCode) int {
	switch c {
	case model.CodeIdempotencyKeyReused:
		return http.StatusUnprocessableEntity
	case model.CodePortUnavailable:
		return http.StatusUnprocessableEntity
	case model.CodeInstanceNameInvalid,
		model.CodeDraftVocabMismatch,
		model.CodeNGLAutoConflict,
		model.CodeExtraFlagForbidden,
		model.CodeBadFlags,
		model.CodeModelMissing:
		// Section 3.10's save-time refusals. All six describe a body that
		// parsed but names a configuration this host will not accept, which is
		// what 422 means throughout this API — a 400 would say the request was
		// malformed, and it was not.
		//
		// `model_missing` and `bad_flags` are also launcher error codes
		// (§5.6's exit table). One name per condition is deliberate: the API
		// refuses at save time exactly what the launcher would refuse at start
		// time, and a user who sees the code twice should see the same word.
		return http.StatusUnprocessableEntity
	case model.CodeConflictGeneration, model.CodeInstanceNameTaken:
		return http.StatusConflict
	case model.CodeSettingInvalid, model.CodePasswordInvalid, model.CodeWizardStepUnknown:
		return http.StatusBadRequest
	case model.CodeBadCredentials:
		return http.StatusUnauthorized
	case model.CodeLockedOut:
		// Section 3.1's own table names this 429, and it is the only one that is
		// not section 3.3's restart rate limit. It carries `retry_after_sec` in
		// details rather than the `retry_after_ms` of the restart guard, because
		// a lockout is measured in minutes and told to a human.
		return http.StatusTooManyRequests
	case model.CodeWizardStepLocked:
		return http.StatusConflict
	case model.CodeJobInFlight,
		model.CodeDownloadExists,
		model.CodeSelfUpdateNotCancelable:
		return http.StatusConflict
	case model.CodeModelInUse, model.CodeRootIsPrimary:
		// Section 3.7's two refusals to change state. Both describe a request
		// that is well formed and currently impossible, which is what 409 means
		// here — the caller can make it possible (stop using the model, promote
		// another root) and try again.
		return http.StatusConflict
	case model.CodeRootNotWritable, model.CodeRootPathProtected:
		// A path this host will not accept as a writable cache root. 422 rather
		// than 400 for the same reason section 3.10's save-time refusals are:
		// the body parsed, and what it named is the problem.
		return http.StatusUnprocessableEntity
	case CodeNotFound:
		return http.StatusNotFound
	case CodeBadRequest:
		return http.StatusBadRequest
	case CodeUnsupportedMediaType:
		return http.StatusUnsupportedMediaType
	case CodeMethodNotAllowed:
		return http.StatusMethodNotAllowed
	case middleware.CodeUnauthorized:
		return http.StatusUnauthorized
	case middleware.CodeCSRFFailed, middleware.CodeSetupTokenRequired:
		return http.StatusForbidden
	case middleware.CodeSetupRequired, middleware.CodeSetupAlreadyClaimed:
		return http.StatusConflict
	case middleware.CodeIdempotencyKeyInvalid:
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}
