package github

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// One client, one policy (DESIGN section 6.2). Everything this package sends to
// api.github.com goes through do(), which owns four things that must not be
// re-decided per call site:
//
//   - the conditional request (`If-None-Match` from the persisted ETag) and
//     the stale-while-revalidating fallback;
//   - the optional token, and the rule that it is NEVER sent anywhere but
//     api.github.com — not to raw.githubusercontent.com, not to a release-asset
//     CDN redirect (the same cross-host header strip section 7.1 applies to the
//     HF token);
//   - the rate-limit accounting the UI shows;
//   - a bounded retry that does not turn an exhausted hourly budget into an
//     hour of retries.

// DefaultBaseURL is api.github.com.
const DefaultBaseURL = "https://api.github.com"

// DefaultRepo is upstream llama.cpp.
const DefaultRepo = "ggml-org/llama.cpp"

// maxAPIBody bounds a release-listing response. GitHub's own cap on a
// `per_page=100` listing is far below this; the limit exists so a
// misconfigured base URL cannot stream gigabytes into memory.
const maxAPIBody = 8 << 20

// maxAssetBody bounds the little text assets this client reads inline —
// `nightly-tag.txt` is a dozen bytes.
const maxAssetBody = 64 << 10

// Options configures a Client. Every field has a working default except the
// user agent, which callers should set from internal/buildinfo.
type Options struct {
	// HTTP is the transport. Nil builds one with sane timeouts. A caller that
	// supplies its own gets its CheckRedirect REPLACED — the cross-host header
	// strip is not optional.
	HTTP *http.Client
	// BaseURL is the API root. Empty uses DefaultBaseURL; a test points it at
	// an httptest server.
	BaseURL string
	// Repo is `owner/name`. Empty uses DefaultRepo.
	Repo string
	// Token returns the stored GitHub token, or "" when none is stored. It is a
	// function rather than a string because the token is sealed in `secrets`
	// and may be added, replaced or deleted while the daemon runs — a client
	// constructed at boot with a snapshot would keep using a token the user
	// revoked. Nil means anonymous.
	Token func(ctx context.Context) (string, error)
	// Cache persists ETags across restarts. Nil uses an in-process cache.
	Cache Cache
	// UserAgent is sent verbatim. Empty uses a generic one; the daemon passes
	// `llamaman/<version> (+https://github.com/jlbyh2o/llamaman)`.
	UserAgent string
	// Now supplies the clock. Nil uses time.Now.
	Now func() time.Time
	// Logger receives one line per stale-serve and per retry. Nil uses
	// slog.Default.
	Logger *slog.Logger
	// MaxRetries bounds retries of a 5xx or a transport error. Zero uses 3.
	MaxRetries int
	// Sleep is how a retry waits. Nil uses a context-aware time.After, and a
	// test substitutes a no-op so the suite does not spend seconds sleeping.
	Sleep func(ctx context.Context, d time.Duration) error
}

// Client is the llama.cpp Releases client.
type Client struct {
	http      *http.Client
	baseURL   string
	repo      string
	token     func(ctx context.Context) (string, error)
	cache     Cache
	userAgent string
	now       func() time.Time
	log       *slog.Logger
	retries   int
	sleep     func(ctx context.Context, d time.Duration) error

	mu   sync.RWMutex
	rate RateLimit
}

// New builds a client.
func New(opts Options) *Client {
	c := &Client{
		http:      opts.HTTP,
		baseURL:   strings.TrimSuffix(cmpOr(opts.BaseURL, DefaultBaseURL), "/"),
		repo:      cmpOr(opts.Repo, DefaultRepo),
		token:     opts.Token,
		cache:     opts.Cache,
		userAgent: cmpOr(opts.UserAgent, "llamaman (+https://github.com/jlbyh2o/llamaman)"),
		now:       opts.Now,
		log:       opts.Logger,
		retries:   opts.MaxRetries,
		sleep:     opts.Sleep,
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: 30 * time.Second}
	}
	// Not optional, and applied even to a caller-supplied client: an
	// api.github.com response that redirects to a CDN must not carry the token
	// with it.
	c.http.CheckRedirect = stripAuthOnHostChange
	if c.cache == nil {
		c.cache = NewMemoryCache()
	}
	if c.now == nil {
		c.now = func() time.Time { return time.Now().UTC() }
	}
	if c.log == nil {
		c.log = slog.Default()
	}
	if c.retries <= 0 {
		c.retries = 3
	}
	if c.sleep == nil {
		c.sleep = sleepCtx
	}
	return c
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// stripAuthOnHostChange is the cross-host header strip. A release asset URL
// redirects from api.github.com to objects.githubusercontent.com with its own
// signed query string; sending our Authorization header along would hand a
// user's GitHub token to a host that never needed it — and, worse, some CDNs
// reject a request that carries both their signature and an Authorization
// header, so this is a correctness fix as much as a security one.
func stripAuthOnHostChange(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("github: stopped after 10 redirects")
	}
	prev := via[len(via)-1]
	if prev.URL.Host != req.URL.Host {
		req.Header.Del("Authorization")
	}
	return nil
}

// RateLimit returns the last rate-limit numbers api.github.com reported. Known
// is false until a request has been made.
func (c *Client) RateLimit() RateLimit {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.rate
}

// Meta is what a call reports about HOW it got its answer, alongside the answer
// itself. It is not decoration: `GET /api/v1/llamacpp/releases` renders "as of
// 14:02, GitHub unreachable" from exactly these fields.
type Meta struct {
	// Stale is true when the body came from the cache because the network or
	// the rate limit would not produce a fresh one.
	Stale bool `json:"stale"`
	// NotModified is true when GitHub answered 304 — the cached body is
	// confirmed current, which is the opposite of stale.
	NotModified bool `json:"not_modified"`
	// FetchedAt is when the body served was last confirmed fresh.
	FetchedAt time.Time `json:"fetched_at"`
	// RateLimit is the budget as of this call.
	RateLimit RateLimit `json:"rate_limit"`
}

// apiGet performs one conditional GET against the API and returns the body.
func (c *Client) apiGet(ctx context.Context, key, path string) ([]byte, Meta, error) {
	return c.get(ctx, key, c.baseURL+path, true, maxAPIBody)
}

// get is the whole request policy. `authorized` is false for anything that is
// not api.github.com — a release asset, above all.
func (c *Client) get(ctx context.Context, key, rawURL string, authorized bool, maxBody int64) ([]byte, Meta, error) {
	var cached Entry
	var haveCached bool
	if key != "" {
		e, ok, err := c.cache.Load(ctx, key)
		if err != nil {
			c.log.Warn("github: release cache unreadable", "key", key, "error", err)
		} else if ok {
			cached, haveCached = e, true
		}
	}

	var lastErr error
	for attempt := 0; attempt <= c.retries; attempt++ {
		if attempt > 0 {
			// 500 ms, 1 s, 2 s. Bounded on purpose: the failure this retries is
			// a transient 5xx, and anything longer would hold a job's lease
			// while achieving nothing.
			backoff := time.Duration(500<<(attempt-1)) * time.Millisecond
			if err := c.sleep(ctx, backoff); err != nil {
				return nil, Meta{RateLimit: c.RateLimit()}, err
			}
		}

		body, meta, retry, err := c.attempt(ctx, key, rawURL, authorized, maxBody, cached, haveCached)
		switch {
		case err == nil:
			return body, meta, nil
		case !retry:
			return nil, meta, err
		}
		lastErr = err
	}

	// Out of retries. A cached body is a better answer than an error.
	if haveCached {
		c.log.Warn("github: serving a stale release cache", "key", key, "error", lastErr)
		return cached.Body, Meta{Stale: true, FetchedAt: cached.FetchedAt, RateLimit: c.RateLimit()}, nil
	}
	return nil, Meta{RateLimit: c.RateLimit()}, lastErr
}

// attempt performs one HTTP request. `retry` reports whether the error it
// returns is worth another attempt.
func (c *Client) attempt(ctx context.Context, key, rawURL string, authorized bool, maxBody int64,
	cached Entry, haveCached bool) (body []byte, meta Meta, retry bool, err error) {

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, Meta{}, false, err
	}
	req.Header.Set("User-Agent", c.userAgent)
	if authorized {
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		if err := c.authorize(ctx, req); err != nil {
			return nil, Meta{}, false, err
		}
	}
	if haveCached && cached.ETag != "" {
		req.Header.Set("If-None-Match", cached.ETag)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// A canceled context is the caller's decision, never a retry.
		if ctx.Err() != nil {
			return nil, Meta{RateLimit: c.RateLimit()}, false, err
		}
		return nil, Meta{RateLimit: c.RateLimit()}, true, err
	}
	defer resp.Body.Close()

	rl := c.recordRateLimit(resp, req.Header.Get("Authorization") != "")
	now := c.now()

	switch {
	case resp.StatusCode == http.StatusNotModified && haveCached:
		// A 304 is a freshness confirmation, so the timestamp moves even though
		// the body did not.
		cached.FetchedAt = now
		c.save(ctx, key, cached)
		return cached.Body, Meta{NotModified: true, FetchedAt: now, RateLimit: rl}, false, nil

	case resp.StatusCode == http.StatusOK:
		b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		if err != nil {
			return nil, Meta{RateLimit: rl}, true, err
		}
		if key != "" {
			c.save(ctx, key, Entry{ETag: resp.Header.Get("ETag"), Body: b, FetchedAt: now})
		}
		return b, Meta{FetchedAt: now, RateLimit: rl}, false, nil

	case resp.StatusCode == http.StatusNotFound:
		return nil, Meta{RateLimit: rl}, false, fmt.Errorf("%w: %s", ErrNotFound, rawURL)

	case resp.StatusCode == http.StatusUnauthorized:
		return nil, Meta{RateLimit: rl}, false, ErrTokenInvalid

	case isRateLimited(resp, rl):
		// Retrying an exhausted hourly budget would burn an hour. Serve what we
		// have, or say plainly that the budget is gone and when it returns.
		if haveCached {
			c.log.Warn("github: rate limited, serving the cached release data",
				"key", key, "reset_at", rl.ResetAt, "authenticated", rl.Authenticated)
			return cached.Body, Meta{Stale: true, FetchedAt: cached.FetchedAt, RateLimit: rl}, false, nil
		}
		return nil, Meta{RateLimit: rl}, false, &ErrRateLimit{RateLimit: rl}

	case resp.StatusCode >= 500:
		return nil, Meta{RateLimit: rl}, true, &StatusError{Status: resp.StatusCode, URL: rawURL}

	default:
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, Meta{RateLimit: rl}, false,
			&StatusError{Status: resp.StatusCode, URL: rawURL, Body: string(snippet)}
	}
}

// authorize attaches the token, and only ever to api.github.com.
func (c *Client) authorize(ctx context.Context, req *http.Request) error {
	if c.token == nil {
		return nil
	}
	tok, err := c.token(ctx)
	if err != nil {
		// A secrets box that cannot be opened must not take the whole release
		// list down with it: anonymous is a supported mode.
		c.log.Warn("github: stored token unavailable, continuing anonymously", "error", err)
		return nil
	}
	if tok == "" {
		return nil
	}
	if !sameHost(req.URL, c.baseURL) {
		// Defense in depth behind the redirect strip: a caller that hands this
		// method a non-API URL gets no token rather than a leaked one.
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	return nil
}

func sameHost(u *url.URL, base string) bool {
	b, err := url.Parse(base)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, b.Host)
}

func (c *Client) save(ctx context.Context, key string, e Entry) {
	if key == "" {
		return
	}
	if err := c.cache.Save(ctx, key, e); err != nil {
		c.log.Warn("github: release cache unwritable", "key", key, "error", err)
	}
}

// isRateLimited distinguishes "your budget is spent" from an ordinary 403.
// GitHub answers both with 403; the discriminators are `x-ratelimit-remaining:
// 0`, the secondary-limit `retry-after` header, and a 429.
func isRateLimited(resp *http.Response, rl RateLimit) bool {
	if resp.StatusCode == http.StatusTooManyRequests {
		return true
	}
	if resp.StatusCode != http.StatusForbidden {
		return false
	}
	if rl.Known && rl.Remaining <= 0 {
		return true
	}
	return resp.Header.Get("Retry-After") != ""
}

func (c *Client) recordRateLimit(resp *http.Response, authenticated bool) RateLimit {
	rl := RateLimit{Authenticated: authenticated}
	if v := resp.Header.Get("X-RateLimit-Limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			rl.Limit = n
			rl.Known = true
		}
	}
	if v := resp.Header.Get("X-RateLimit-Remaining"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			rl.Remaining = n
			rl.Known = true
		}
	}
	if v := resp.Header.Get("X-RateLimit-Reset"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			rl.ResetAt = time.Unix(n, 0).UTC()
			rl.Known = true
		}
	}
	if !rl.Known {
		// Nothing to record; keep whatever the last call learned rather than
		// overwriting real numbers with zeros.
		c.mu.RLock()
		prev := c.rate
		c.mu.RUnlock()
		return prev
	}
	c.mu.Lock()
	c.rate = rl
	c.mu.Unlock()
	return rl
}
