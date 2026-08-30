package middleware

import (
	"log/slog"
	"net/http"
	"time"
)

// RequestLog is the outermost layer. It records one line per request at the
// level the status earns — 5xx is an error, 4xx a warn, everything else info —
// so a journald reader tailing at `warn` sees exactly the requests that went
// wrong (internal/logx is the handler; this layer only decides what to say).
//
// It is outside Recover, not inside, so that a panic recovered below is logged
// as the 500 the client actually received. The reverse order would log a
// status of 0 for the one request most worth reading about.
//
// The line names the matched route's operation id rather than the raw path,
// because a path with a wildcard in it (`/api/v1/hf/tree/{repo...}`) is
// unbounded, high-cardinality, and attacker-chosen — the operation id is the
// stable name the OpenAPI document and the metrics both use. The raw path is
// logged too, but only for a request that FAILED to match a route, where it is
// the only thing that identifies it.
func RequestLog(log *slog.Logger, now func() time.Time) Middleware {
	if log == nil {
		log = slog.Default()
	}
	if now == nil {
		now = time.Now
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			started := now()
			rec := &recorder{ResponseWriter: w, status: http.StatusOK}

			// The route is matched by the mux, which is inside this layer, so
			// the operation id has to travel back out through a slot installed
			// here rather than through a context value added down there.
			r = r.WithContext(WithRouteSlot(r.Context()))

			next.ServeHTTP(rec, r)

			elapsed := now().Sub(started)
			attrs := []any{
				"method", r.Method,
				"status", rec.status,
				"duration_ms", elapsed.Milliseconds(),
				"bytes", rec.written,
				"ip", clientIP(r),
			}
			if op, ok := RouteFrom(r.Context()); ok {
				attrs = append(attrs, "route", op)
			} else {
				attrs = append(attrs, "path", r.URL.Path)
			}

			switch {
			case rec.status >= 500:
				log.Error("http request", attrs...)
			case rec.status >= 400:
				log.Warn("http request", attrs...)
			default:
				log.Info("http request", attrs...)
			}
		})
	}
}

// recorder captures the status and byte count without getting in the way of
// anything else the handler needs from the ResponseWriter.
//
// Unwrap is what keeps it out of the way: http.NewResponseController follows it
// to reach the real writer, so Flush, SetWriteDeadline and Hijack all still
// work through this wrapper — which matters because the SSE handler
// (internal/sse) flushes on every frame and would otherwise buffer forever
// behind a naive wrapper.
type recorder struct {
	http.ResponseWriter
	status      int
	written     int64
	wroteHeader bool
}

func (rec *recorder) WriteHeader(status int) {
	if rec.wroteHeader {
		return
	}
	rec.wroteHeader = true
	rec.status = status
	rec.ResponseWriter.WriteHeader(status)
}

func (rec *recorder) Write(b []byte) (int, error) {
	if !rec.wroteHeader {
		rec.WriteHeader(http.StatusOK)
	}
	n, err := rec.ResponseWriter.Write(b)
	rec.written += int64(n)
	return n, err
}

// Unwrap exposes the underlying writer to http.NewResponseController.
func (rec *recorder) Unwrap() http.ResponseWriter { return rec.ResponseWriter }

// WroteHeader reports whether a status has been written yet. Recover consults
// it: a panic after the first byte cannot be turned into a 500, and pretending
// otherwise would append a JSON envelope to a half-written response.
func (rec *recorder) WroteHeader() bool { return rec.wroteHeader }

// headerWriteReporter is what Recover looks for when deciding whether it can
// still write an error envelope. The recorder above satisfies it; so does any
// future wrapper that wants Recover to make the same distinction.
type headerWriteReporter interface{ WroteHeader() bool }
