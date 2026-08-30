package github

import (
	"context"
	"sync"
	"time"
)

// The ETag cache (DESIGN section 6.2): "GitHub calls go through one client with
// If-None-Match conditional requests persisted in `release_cache.etag`, served
// stale-while-revalidating."
//
// Two properties make this worth a whole interface rather than a map:
//
//  1. It is PERSISTED. Unauthenticated api.github.com allows 60 requests per
//     hour per IP, and a daemon that restarted with an empty cache would spend
//     that budget re-fetching a release list that has not changed since
//     yesterday. A conditional request that comes back `304` costs a round trip
//     and nothing against the rate limit's body budget.
//  2. It is the STALE fallback. When the limit is exhausted, or GitHub is
//     unreachable, the cached body is served with `stale: true` rather than an
//     error — which is what lets `GET /api/v1/llamacpp/releases` answer "why is
//     the nightly list stale" on screen instead of showing an empty page.
//
// D49's first invariant keeps the SQL out of here: this package declares the
// interface it needs and the llamacpp service backs it with `release_cache`
// rows keyed by `(source='llamacpp', tag=<the Key below>)`.

// Entry is one cached response.
type Entry struct {
	// ETag is the validator to send back as If-None-Match. Empty means the
	// response carried none, and the next request is unconditional.
	ETag string
	// Body is the response body verbatim, which is what a stale read serves.
	Body []byte
	// FetchedAt is when the body was last known fresh — updated on a 304 as
	// well as on a 200, because a 304 IS a freshness confirmation.
	FetchedAt time.Time
}

// Cache persists conditional-request state across daemon restarts. A nil Cache
// is legal and means every request is unconditional and nothing is stale-served
// — correct, just wasteful of a small hourly budget.
type Cache interface {
	// Load returns the cached entry for a key. A miss is (Entry{}, false, nil);
	// an error is a real storage failure and is logged, never fatal.
	Load(ctx context.Context, key string) (Entry, bool, error)
	// Save stores an entry under a key.
	Save(ctx context.Context, key string, e Entry) error
}

// Cache keys. They are stable strings rather than URLs because a URL carries
// the configurable base and would change every one of them if a test pointed
// the client at an httptest server.
const (
	// KeyLatestRelease is `GET /repos/{repo}/releases/latest`.
	KeyLatestRelease = "releases/latest"
	// KeyReleaseList is `GET /repos/{repo}/releases?per_page=N`.
	KeyReleaseList = "releases/list"
)

// KeyNightlyTag is the key for one release's `nightly-tag.txt` asset. It is per
// tag because the asset's content is the pinned build of THAT release and never
// changes once published — a cached one is good forever.
func KeyNightlyTag(tag string) string { return "releases/" + tag + "/nightly-tag.txt" }

// MemoryCache is an in-process Cache. It is the default when no persistent one
// is supplied, and it is what the tests in this package use: within one daemon
// lifetime it already saves the rate-limit budget, and it makes the conditional
// path exercisable without a database.
type MemoryCache struct {
	mu      sync.RWMutex
	entries map[string]Entry
}

// NewMemoryCache returns an empty in-process cache.
func NewMemoryCache() *MemoryCache {
	return &MemoryCache{entries: make(map[string]Entry)}
}

// Load implements Cache.
func (c *MemoryCache) Load(_ context.Context, key string) (Entry, bool, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.entries[key]
	return e, ok, nil
}

// Save implements Cache.
func (c *MemoryCache) Save(_ context.Context, key string, e Entry) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]Entry)
	}
	c.entries[key] = e
	return nil
}

var _ Cache = (*MemoryCache)(nil)
