package middleware

import (
	"net/http"
	"strconv"
)

// MaxIdempotencyKeyLen bounds the header this layer accepts. The key is stored
// verbatim as the primary key of `idempotency_keys` (section 2.3, D65), so an
// unbounded one would be an unbounded row; 255 is longer than any client
// generator needs and short enough to index.
const MaxIdempotencyKeyLen = 255

// IdempotencyKeyExtractor is the fourth layer of the per-route chain and the
// smallest of the four. It validates the optional `Idempotency-Key` header and
// puts it in the request context; it decides NOTHING about replay.
//
// That division is the point. D39/D65 put the 10-minute window in the
// `idempotency_keys` table and the "same key, different body" check behind
// `idx_jobs_one_live_per_subject` and the fingerprint column — both of which
// have to be evaluated inside the ONE transaction that inserts the job (D97),
// which is internal/jobs' Enqueue, not a middleware that has already let the
// handler run. A middleware that tried to answer a replay here would have to
// open a second transaction and would race the first.
//
// So: this layer extracts, validates the shape, and hands the key to the
// handler through the context. `200 with the original job` and
// `422 idempotency_key_reused` are both written by the handler from what
// jobs.Enqueue returns.
//
// It is mounted only on the routes DESIGN section 3 marks as job-creating.
// On any other route the header is meaningless and is left alone.
func IdempotencyKeyExtractor() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get(HeaderIdempotencyKey)
			if key == "" {
				// The header is optional (section 3: "job-creating POSTs
				// accept an optional Idempotency-Key header"), so its absence
				// is not an error and leaves nothing in the context.
				next.ServeHTTP(w, r)
				return
			}
			if !validIdempotencyKey(key) {
				WriteError(w, http.StatusBadRequest, CodeIdempotencyKeyInvalid,
					HeaderIdempotencyKey+" must be 1-"+strconv.Itoa(MaxIdempotencyKeyLen)+
						" printable ASCII characters", nil)
				return
			}
			next.ServeHTTP(w, r.WithContext(WithIdempotencyKey(r.Context(), key)))
		})
	}
}

// validIdempotencyKey accepts a short run of printable ASCII. It is
// deliberately not "any UTF-8": the key is a client-chosen opaque token that
// ends up in a primary key, a log line and an error message, and restricting it
// to printable ASCII removes every question about normalization, control
// characters and log injection at once.
func validIdempotencyKey(k string) bool {
	if k == "" || len(k) > MaxIdempotencyKeyLen {
		return false
	}
	for i := 0; i < len(k); i++ {
		if k[i] < 0x21 || k[i] > 0x7e {
			return false
		}
	}
	return true
}
