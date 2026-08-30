package tokens

import (
	"sync"
	"time"
)

// The optional per-token bucket of `api_tokens.rate_limit_rpm` (DESIGN section
// 9.3: "Expiry, `state` and the optional `rate_limit_rpm` token bucket are
// checked per request").
//
// It is a token bucket rather than a fixed window because a fixed window lets a
// client spend a whole minute's budget in the last millisecond of one window and
// again in the first millisecond of the next — twice the configured rate, at the
// moment the operator was most confident it could not happen. The burst is one
// minute's worth, so a client that has been idle may still send its whole
// allowance at once, which is what "requests per minute" means to the person who
// typed the number.

// buckets holds one bucket per token id. It is bounded by the number of tokens,
// which is bounded by what an admin created — an attacker presenting unknown
// secrets never reaches here, because an unknown hash is denied before any
// bucket is consulted.
type buckets struct {
	mu sync.Mutex
	m  map[string]*bucket
}

type bucket struct {
	// rpm is the limit this bucket was built for. A token whose limit was
	// edited gets a fresh bucket rather than a rescaled one: the admin just
	// changed the policy, and the honest reading of that is "start counting
	// under the new one".
	rpm        int64
	tokens     float64
	lastRefill time.Time
}

func newBuckets() *buckets { return &buckets{m: map[string]*bucket{}} }

// allow consumes one request's worth of budget for id, and reports whether there
// was any. An rpm of zero or less is "no limit" and always allows.
func (b *buckets) allow(id string, rpm int64, now time.Time) bool {
	if rpm <= 0 {
		return true
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	bk, ok := b.m[id]
	if !ok || bk.rpm != rpm {
		bk = &bucket{rpm: rpm, tokens: float64(rpm), lastRefill: now}
		b.m[id] = bk
	}

	if elapsed := now.Sub(bk.lastRefill); elapsed > 0 {
		bk.tokens += elapsed.Seconds() * float64(rpm) / 60
		if bk.tokens > float64(rpm) {
			bk.tokens = float64(rpm)
		}
		bk.lastRefill = now
	}
	if bk.tokens < 1 {
		return false
	}
	bk.tokens--
	return true
}

// forget drops one token's bucket. Revoking a token and minting a new one must
// not hand the new one a spent bucket, and the ids differ, so this exists for
// the other direction: an id that is gone should not hold memory forever.
func (b *buckets) forget(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.m, id)
}
