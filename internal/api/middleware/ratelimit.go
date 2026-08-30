package middleware

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// RateLimit is a per-route policy. There is no global limiter, and that is a
// deliberate reading of DESIGN section 3: "`429` is used for exactly one thing:
// `POST /system/restart` while this boot has not yet cleared its unit's
// start-limit counter". A blanket limiter would put a second, undocumented
// meaning on that status, and the response-conformance middleware (D43) would
// be right to fail it.
//
// So this layer exists in the chain, in section 1's position, but it does
// nothing on a route that declares no policy — and the one 429 the design does
// name carries its own code, supplied here rather than invented by this
// package. The login lockout's `429 locked_out` is NOT this layer: it is
// per-account, stored, and lives in internal/auth.
type RateLimit struct {
	// Burst is how many requests may arrive at once before the bucket is
	// empty. Must be >= 1.
	Burst int
	// Every is how long one token takes to refill.
	Every time.Duration
	// Code is the model.ErrorCode the 429 carries. Required: the design names
	// each 429 individually, so a route that wants one must say which.
	Code model.ErrorCode
	// Message is the human half of the envelope.
	Message string
	// KeyFunc buckets requests. Nil buckets by client IP.
	KeyFunc func(*http.Request) string
}

// RateLimiter builds the layer for p. A nil policy returns a nil Middleware,
// which Chain.Then skips.
//
// The buckets are in memory and per-process on purpose: this daemon is one
// process on one host, and a limiter that needed the database would put a write
// on the path of every request it is supposed to be cheapening.
func RateLimiter(p *RateLimit, now func() time.Time) Middleware {
	if p == nil {
		return nil
	}
	if now == nil {
		now = time.Now
	}
	burst := p.Burst
	if burst < 1 {
		burst = 1
	}
	every := p.Every
	if every <= 0 {
		every = time.Second
	}
	key := p.KeyFunc
	if key == nil {
		key = clientIP
	}
	code := p.Code
	if code == "" {
		code = CodeInternalError
	}

	b := &buckets{now: now, burst: float64(burst), every: every}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ok, retryAfter := b.take(key(r))
			if !ok {
				secs := int((retryAfter + time.Second - 1) / time.Second)
				if secs < 1 {
					secs = 1
				}
				w.Header().Set("Retry-After", strconv.Itoa(secs))
				WriteError(w, http.StatusTooManyRequests, code, p.Message, map[string]any{
					"retry_after_ms": retryAfter.Milliseconds(),
				})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// buckets is a token bucket per key, swept lazily. A bucket that has refilled
// to full is indistinguishable from one that never existed, so the sweep can
// simply delete it — there is no state to lose.
type buckets struct {
	now   func() time.Time
	burst float64
	every time.Duration

	mu       sync.Mutex
	m        map[string]*bucket
	lastGC   time.Time
	gcEvery  time.Duration
	gcMaxLen int
}

type bucket struct {
	tokens float64
	at     time.Time
}

func (b *buckets) take(key string) (ok bool, retryAfter time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.now()
	if b.m == nil {
		b.m = make(map[string]*bucket)
		b.lastGC = now
		b.gcEvery = 5 * time.Minute
		b.gcMaxLen = 1024
	}
	b.gc(now)

	e, seen := b.m[key]
	if !seen {
		e = &bucket{tokens: b.burst, at: now}
		b.m[key] = e
	} else {
		refill := now.Sub(e.at).Seconds() / b.every.Seconds()
		e.tokens += refill
		if e.tokens > b.burst {
			e.tokens = b.burst
		}
		e.at = now
	}

	if e.tokens < 1 {
		deficit := 1 - e.tokens
		return false, time.Duration(deficit * float64(b.every))
	}
	e.tokens--
	return true, 0
}

// gc drops buckets that have refilled to full, which are exactly the ones that
// carry no information. It runs at most every gcEvery, and immediately once the
// map has grown past gcMaxLen so a burst of distinct keys cannot grow it
// without bound between sweeps.
func (b *buckets) gc(now time.Time) {
	if now.Sub(b.lastGC) < b.gcEvery && len(b.m) <= b.gcMaxLen {
		return
	}
	b.lastGC = now
	full := b.burst * float64(b.every)
	for k, e := range b.m {
		if e.tokens+now.Sub(e.at).Seconds()/b.every.Seconds() >= b.burst &&
			now.Sub(e.at) > time.Duration(full) {
			delete(b.m, k)
		}
	}
}
