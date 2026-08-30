package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// DTO conventions (DESIGN section 3).
//
// Storage forms and wire forms are deliberately different, and the conversion
// lives here and only here:
//
//   - Timestamps are INTEGER Unix milliseconds in SQLite (section 2) and RFC
//     3339 UTC STRINGS on the wire. Time and TimePtr are the conversion.
//   - Durations are integers with a `_ms` suffix on the field name. There is
//     no helper because there is no conversion: milliseconds are the storage
//     form too, and the suffix is the documentation.
//   - Byte counts are plain JSON numbers of bytes — never a formatted string,
//     never kibibytes. Formatting is the UI's job.
//   - Lists are {"items":[…],"total":N,"next_cursor":"01J…"|null} with ULID
//     keyset pagination.
//
// A DTO struct never embeds a model struct. The two change for different
// reasons — a column is added because the daemon needs to remember something, a
// field is added because a client needs to see it — and embedding would make
// every schema change a wire change, which is exactly what the generated
// openapi.json and the D43 drift check exist to make visible.

// MaxRequestBody bounds a decoded JSON request body. Nothing in section 3 posts
// anything large: the biggest bodies are an instance's FlagSet and a bench
// sweep. Model bytes never travel through this API — they are downloaded by the
// daemon from Hugging Face — so a megabyte is generous.
const MaxRequestBody = 1 << 20

// Time renders a Unix-millisecond storage timestamp as the RFC 3339 UTC string
// section 3 puts on the wire.
func Time(ms int64) string {
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}

// TimePtr renders a nullable timestamp. A NULL column becomes JSON null rather
// than the epoch, which is the whole reason those columns are nullable (F14: a
// fact that has not been learned is NULL, not a zero that reads as an answer).
func TimePtr(ms *int64) *string {
	if ms == nil {
		return nil
	}
	s := Time(*ms)
	return &s
}

// List is the list envelope of section 3. It is generic so that `total` and
// `next_cursor` cannot be forgotten on a new collection endpoint.
//
// NextCursor is a *string so an exhausted page serializes as `"next_cursor":
// null` rather than `""`. The cursor is the last item's ULID, which is why
// section 2 insists ids sort by creation.
type List[T any] struct {
	Items      []T     `json:"items"`
	Total      int     `json:"total"`
	NextCursor *string `json:"next_cursor"`
}

// NewList builds a List, normalizing a nil slice to an empty one so the wire
// form is `[]` and never `null` — a client that has to test for both is a
// client that will eventually forget to.
func NewList[T any](items []T, total int, nextCursor *string) List[T] {
	if items == nil {
		items = []T{}
	}
	return List[T]{Items: items, Total: total, NextCursor: nextCursor}
}

// WriteJSON writes v as the body of a status response.
//
// It marshals to a buffer BEFORE writing the status line. A DTO that fails to
// marshal halfway through — an unsupported type, a NaN — would otherwise leave
// a 200 with a truncated body on the wire, which is unrecoverable; buffering
// makes it a 500 with an intact envelope instead.
func WriteJSON(w http.ResponseWriter, status int, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, err = w.Write(append(b, '\n'))
	return err
}

// WriteNoContent answers the 204 that section 3 uses for every mutation with
// nothing to report.
func WriteNoContent(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// DecodeJSON reads a JSON request body into v.
//
// Unknown fields are REJECTED. That is the request-side mirror of the D43
// response-conformance rule ("an extra field fails the suite"): a client that
// sends `{"pubic_port":8080}` should be told it misspelled the field, not have
// it silently ignored and then wonder why the port did not change.
func DecodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		mt := strings.TrimSpace(strings.SplitN(ct, ";", 2)[0])
		if !strings.EqualFold(mt, "application/json") {
			return Errorf(http.StatusUnsupportedMediaType, CodeUnsupportedMediaType,
				"request body must be application/json, got %q", mt)
		}
	}

	body := http.MaxBytesReader(w, r.Body, MaxRequestBody)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(v); err != nil {
		var maxErr *http.MaxBytesError
		switch {
		case errors.As(err, &maxErr):
			return Errorf(http.StatusRequestEntityTooLarge, CodeBadRequest,
				"request body is larger than %d bytes", MaxRequestBody)
		case errors.Is(err, io.EOF):
			return BadRequest("a JSON request body is required")
		default:
			return BadRequest("%s", jsonErrMessage(err))
		}
	}
	// Exactly one JSON value, and nothing after it. Two concatenated objects
	// are a client bug worth naming rather than silently reading the first.
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return BadRequest("the request body must contain exactly one JSON object")
	}
	return nil
}

// jsonErrMessage turns encoding/json's errors into something a client can act
// on, without echoing the offending bytes back — the body may contain a
// password, and an error message is the one part of a response that tends to
// end up in a log.
func jsonErrMessage(err error) string {
	var syn *json.SyntaxError
	if errors.As(err, &syn) {
		return "malformed JSON in the request body"
	}
	var typ *json.UnmarshalTypeError
	if errors.As(err, &typ) {
		if typ.Field != "" {
			return "field " + typ.Field + " has the wrong type"
		}
		return "a field in the request body has the wrong type"
	}
	// DisallowUnknownFields reports through a plain error whose text names the
	// field, and that text is ours to pass on: the field name came from the
	// request, but it is bounded and already echoed by every JSON API.
	if msg := err.Error(); strings.HasPrefix(msg, "json: unknown field ") {
		return "unknown " + strings.TrimPrefix(msg, "json: ")
	}
	return "the request body could not be decoded"
}
