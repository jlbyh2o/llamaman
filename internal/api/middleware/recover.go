package middleware

import (
	"log/slog"
	"net/http"
	"runtime/debug"
)

// Recover turns a panicking handler into a `500 internal_error` and one log
// line with the stack, so a bug in one endpoint cannot take the daemon down.
// That matters more here than in an ordinary web service: this process owns the
// public inference listeners (section 9.4), and a panic that killed it would
// drop every gateway port with it.
//
// It sits INSIDE RequestLog so the log records the 500 the client received, and
// OUTSIDE every per-route layer so a panic in the session gate is caught too.
//
// http.ErrAbortHandler is re-panicked rather than caught: net/http defines it
// as "abort this connection silently", it is what httputil.ReverseProxy raises
// when a client disconnects mid-stream, and swallowing it here would turn an
// ordinary disconnect into a logged 500.
func Recover(log *slog.Logger) Middleware {
	if log == nil {
		log = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				v := recover()
				if v == nil {
					return
				}
				if v == http.ErrAbortHandler {
					panic(v)
				}

				attrs := []any{"panic", v, "method", r.Method, "stack", string(debug.Stack())}
				if op, ok := RouteFrom(r.Context()); ok {
					attrs = append(attrs, "route", op)
				} else {
					attrs = append(attrs, "path", r.URL.Path)
				}
				log.Error("panic serving http request", attrs...)

				// Once a status line is on the wire there is nothing to
				// correct: the client already has a 200 and some bytes, and
				// appending an error envelope would corrupt the body it is
				// parsing. Closing the connection is the only honest signal
				// left, and returning from the handler after a panic does
				// exactly that for a response with no Content-Length.
				if hw, ok := w.(headerWriteReporter); ok && hw.WroteHeader() {
					return
				}
				WriteError(w, http.StatusInternalServerError, CodeInternalError,
					"internal error", nil)
			}()

			next.ServeHTTP(w, r)
		})
	}
}
