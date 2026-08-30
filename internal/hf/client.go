package hf

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// One client, one policy (DESIGN section 7.1).
//
// Everything this package sends to the Hub goes through do(), which owns five
// things that must not be re-decided per call site:
//
//   - the bearer token, and the rule that it is STRIPPED on a cross-host
//     redirect. `resolve/` redirects file downloads to a CDN with its own signed
//     URL; sending the user's token along would hand it to a host that never
//     needed it, and several CDNs reject a request carrying both their signature
//     and an Authorization header, so this is a correctness fix as much as a
//     security one.
//   - the retry: 429 and 5xx only, honoring `Retry-After`, jittered exponential
//     backoff, at most MaxTries attempts.
//   - the client-side limiter, so a bulk metadata refresh cannot starve a
//     user-initiated search.
//   - the 30-minute TTL cache for search, tree and metadata.
//   - the mapping from a 401/403 to the four different things it can mean
//     (errors.go).
//
// The token is never logged, never returned by the API and never rendered
// anywhere; MaskToken is the only form that leaves this package.

// DefaultEndpoint is the Hub. It is `settings['hf.endpoint']`'s default and a
// test points it at an httptest server.
const DefaultEndpoint = "https://huggingface.co"

// DefaultCacheTTL is section 7.1's cache window for search, tree and metadata.
// Thirty minutes is long enough that paging back and forth through a search
// costs nothing and short enough that a repository updated today is visible
// today.
const DefaultCacheTTL = 30 * time.Minute

// DefaultMaxTries is section 7.1's "max 5 tries" — the first attempt plus four
// retries.
const DefaultMaxTries = 5

// DefaultConcurrency is how many requests this client has in flight at once. It
// is deliberately small: the Hub's API is not the bottleneck in any workflow
// this product has, and a metadata refresh that opened thirty connections would
// be indistinguishable from abuse.
const DefaultConcurrency = 8

// maxJSONBody bounds an API response. A `tree?recursive=1` of a large repository
// is tens of kilobytes; the bound exists so a misconfigured endpoint cannot
// stream gigabytes into memory.
const maxJSONBody = 16 << 20

// maxCardBody bounds a README. Model cards are prose with tables; a megabyte is
// far past any real one and short enough to render.
const maxCardBody = 1 << 20

// Options configures a Client. Every field has a working default except the user
// agent, which the daemon sets from internal/buildinfo.
type Options struct {
	// HTTP is the transport. Nil builds one with the settings section 7.1 names:
	// `Timeout: 0` because this client streams multi-gigabyte bodies and a
	// whole-request deadline would kill a healthy 40 GB download, and
	// MaxIdleConnsPerHost: 8. Per-request contexts carry the deadlines instead.
	//
	// A caller that supplies its own gets its CheckRedirect REPLACED: the
	// cross-host header strip is not optional.
	HTTP *http.Client

	// Endpoint is `settings['hf.endpoint']`. Empty uses DefaultEndpoint.
	Endpoint string

	// Token returns the stored Hugging Face token, or "" when none is stored.
	//
	// It is a function rather than a string because the token is sealed in
	// `secrets` and may be added, replaced or deleted while the daemon runs — a
	// client constructed at boot with a snapshot would keep sending a token the
	// user revoked. Nil means anonymous, which is a fully supported mode: every
	// public GGUF repository is reachable without one.
	Token func(ctx context.Context) (string, error)

	// UserAgent is sent verbatim. Empty uses a generic one; the daemon passes
	// `llamaman/<version> (+https://github.com/jlbyh2o/llamaman)`.
	UserAgent string

	// CacheTTL is the search/tree/metadata window. Zero uses DefaultCacheTTL; a
	// negative value disables caching, which is what a test that asserts request
	// counts wants.
	CacheTTL time.Duration

	// MaxTries bounds attempts of a 429 or 5xx. Zero uses DefaultMaxTries.
	MaxTries int

	// Concurrency is the limiter's width. Zero uses DefaultConcurrency.
	Concurrency int

	// Now supplies the clock. Nil uses time.Now.
	Now func() time.Time

	// Sleep is how a retry waits. Nil uses a context-aware timer; a test
	// substitutes a no-op so the suite does not spend seconds sleeping.
	Sleep func(ctx context.Context, d time.Duration) error

	// Logger receives one line per retry. Nil uses slog.Default.
	Logger *slog.Logger
}

// Client is the Hugging Face Hub client.
type Client struct {
	http      *http.Client
	endpoint  string
	token     func(ctx context.Context) (string, error)
	userAgent string
	ttl       time.Duration
	maxTries  int
	now       func() time.Time
	sleep     func(ctx context.Context, d time.Duration) error
	log       *slog.Logger

	limiter *limiter

	mu    sync.Mutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	body      []byte
	fetchedAt time.Time
}

// New builds a Client.
func New(opts Options) *Client {
	c := &Client{
		http:      opts.HTTP,
		endpoint:  strings.TrimSuffix(cmpOr(opts.Endpoint, DefaultEndpoint), "/"),
		token:     opts.Token,
		userAgent: cmpOr(opts.UserAgent, "llamaman (+https://github.com/jlbyh2o/llamaman)"),
		ttl:       opts.CacheTTL,
		maxTries:  opts.MaxTries,
		now:       opts.Now,
		sleep:     opts.Sleep,
		log:       opts.Logger,
		cache:     map[string]cacheEntry{},
	}
	if c.http == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.MaxIdleConnsPerHost = 8
		// A whole-request timeout is deliberately absent (Timeout: 0). The
		// dial, TLS handshake and response-header deadlines below bound
		// everything that CAN hang without bounding the body, which for this
		// client is a 40 GB stream.
		transport.ResponseHeaderTimeout = 60 * time.Second
		transport.TLSHandshakeTimeout = 20 * time.Second
		transport.DialContext = (&net.Dialer{
			Timeout:   15 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext
		c.http = &http.Client{Transport: transport, Timeout: 0}
	}
	// Applied even to a caller-supplied client: not optional.
	c.http.CheckRedirect = stripAuthOnHostChange
	if c.ttl == 0 {
		c.ttl = DefaultCacheTTL
	}
	if c.maxTries <= 0 {
		c.maxTries = DefaultMaxTries
	}
	if c.now == nil {
		c.now = func() time.Time { return time.Now().UTC() }
	}
	if c.sleep == nil {
		c.sleep = sleepCtx
	}
	if c.log == nil {
		c.log = slog.Default()
	}
	width := opts.Concurrency
	if width <= 0 {
		width = DefaultConcurrency
	}
	c.limiter = newLimiter(width)
	return c
}

// Endpoint is the Hub base URL this client talks to, without a trailing slash.
// The API needs it to build the browser link a gated repository sends a user to.
func (c *Client) Endpoint() string { return c.endpoint }

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// stripAuthOnHostChange is section 7.1's cross-host header strip.
func stripAuthOnHostChange(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("hf: stopped after 10 redirects")
	}
	prev := via[len(via)-1]
	if prev.URL.Host != req.URL.Host {
		req.Header.Del("Authorization")
	}
	return nil
}

// -----------------------------------------------------------------------------
// The limiter
// -----------------------------------------------------------------------------

// Priority is the limiter's one axis. Section 7.1 asks for "one client-side
// limiter so a bulk metadata refresh cannot starve a user-initiated search", and
// a limiter with a single queue cannot make that promise: a hundred queued
// background requests would sit in front of the search no matter how fair the
// queue is.
type Priority int

const (
	// PriorityInteractive is a request a human is waiting on: a search, opening
	// a model, a card, a peek.
	PriorityInteractive Priority = iota
	// PriorityBackground is bulk work nobody is watching: a metadata refresh
	// across the catalog. It may use at most width-1 of the limiter's slots, so
	// there is ALWAYS one free for an interactive caller.
	PriorityBackground
)

type priorityKey struct{}

// WithPriority marks every request made under ctx as background or interactive.
// The default is interactive, which is the right default for a client whose
// callers are mostly HTTP handlers: bulk work is the exception and should have
// to say so.
func WithPriority(ctx context.Context, p Priority) context.Context {
	return context.WithValue(ctx, priorityKey{}, p)
}

func priorityFrom(ctx context.Context) Priority {
	if p, ok := ctx.Value(priorityKey{}).(Priority); ok {
		return p
	}
	return PriorityInteractive
}

// limiter is a two-class semaphore. `all` is the total width; `background` is
// one narrower, and a background caller must hold BOTH — which is what reserves
// the last slot for an interactive request no matter how much bulk work is
// queued behind it.
type limiter struct {
	all        chan struct{}
	background chan struct{}
}

func newLimiter(width int) *limiter {
	if width < 1 {
		width = 1
	}
	bg := width - 1
	if bg < 1 {
		// A width of one cannot reserve anything; the two classes then share
		// the single slot, which is degenerate but not wrong.
		bg = 1
	}
	return &limiter{
		all:        make(chan struct{}, width),
		background: make(chan struct{}, bg),
	}
}

func (l *limiter) acquire(ctx context.Context, p Priority) (func(), error) {
	if p == PriorityBackground {
		select {
		case l.background <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	select {
	case l.all <- struct{}{}:
	case <-ctx.Done():
		if p == PriorityBackground {
			<-l.background
		}
		return nil, ctx.Err()
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			<-l.all
			if p == PriorityBackground {
				<-l.background
			}
		})
	}, nil
}

// -----------------------------------------------------------------------------
// The request policy
// -----------------------------------------------------------------------------

// request describes one call through do().
type request struct {
	method string
	url    string
	// repo is the repository the call is about, for the 401/403 classification.
	// Empty for a call that is not repository-scoped (whoami).
	repo string
	// cacheKey enables the TTL cache. Empty disables it, which is right for
	// everything that is not search, tree or metadata: a resolve HEAD must
	// re-ask the origin, and a card is read once per model view.
	cacheKey string
	// maxBody bounds the response. Zero uses maxJSONBody.
	maxBody int64
	// header carries per-call additions — `Range`, `If-Range`, `Accept`.
	header http.Header
	// stream leaves the body open for the caller to read, which is what a file
	// transfer needs. The caller then owns closing it.
	stream bool
	// anonymous suppresses the token. Nothing uses it today; it exists so that a
	// future public-only call cannot accidentally spend a credential.
	anonymous bool
}

// response is what do() returns for a non-streaming call.
type response struct {
	body   []byte
	status int
	header http.Header
	// finalURL is the URL after redirects, which is where a `validator` was
	// issued and therefore what `validator_host` records.
	finalURL *url.URL
	// cached reports that the body came from the TTL cache and no request was
	// made.
	cached bool
	// raw is the still-open body of a streaming request. The caller owns
	// closing it, and body is nil in that case.
	raw io.ReadCloser
}

// getJSON performs a cached GET and returns the body.
func (c *Client) getJSON(ctx context.Context, cacheKey, rawURL, repo string) ([]byte, error) {
	resp, err := c.do(ctx, request{
		method: http.MethodGet, url: rawURL, repo: repo, cacheKey: cacheKey,
		header: http.Header{"Accept": []string{"application/json"}},
	})
	if err != nil {
		return nil, err
	}
	return resp.body, nil
}

// do is the whole request policy. A streaming request returns with the body
// still open in resp.raw; every other one reads and closes it.
func (c *Client) do(ctx context.Context, rq request) (*response, error) {
	if rq.cacheKey != "" && c.ttl > 0 {
		if body, ok := c.cacheGet(rq.cacheKey); ok {
			return &response{body: body, status: http.StatusOK, cached: true}, nil
		}
	}

	release, err := c.limiter.acquire(ctx, priorityFrom(ctx))
	if err != nil {
		return nil, err
	}
	defer release()

	var lastErr error
	for attempt := 1; attempt <= c.maxTries; attempt++ {
		resp, retryAfter, retry, err := c.attempt(ctx, rq)
		switch {
		case err == nil:
			if rq.cacheKey != "" && c.ttl > 0 {
				c.cachePut(rq.cacheKey, resp.body)
			}
			return resp, nil
		case !retry:
			return nil, err
		}
		lastErr = err

		if attempt == c.maxTries {
			break
		}
		if werr := c.sleep(ctx, c.backoff(attempt, retryAfter)); werr != nil {
			return nil, werr
		}
		c.log.Debug("hf: retrying", "url", rq.url, "attempt", attempt+1, "error", err)
	}
	return nil, lastErr
}

// backoff is section 7.1's "jittered exponential backoff", with `Retry-After`
// taking precedence when the Hub sent one.
//
// The jitter is not decoration: several tasks of one sharded download hit the
// same rate limit within milliseconds of each other, and an unjittered backoff
// would have all five of them retry in the same instant, forever.
func (c *Client) backoff(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		if retryAfter > 2*time.Minute {
			// A Retry-After measured in hours is not something to sleep a
			// worker through; the caller sees the rate-limit error instead.
			retryAfter = 2 * time.Minute
		}
		return retryAfter
	}
	base := time.Duration(500<<(attempt-1)) * time.Millisecond
	if base > 16*time.Second {
		base = 16 * time.Second
	}
	// Full jitter over [base/2, base).
	return base/2 + time.Duration(rand.Int64N(int64(base/2)+1))
}

// attempt performs one HTTP request. `retry` reports whether the error it
// returns is worth another attempt.
func (c *Client) attempt(ctx context.Context, rq request) (
	resp *response, retryAfter time.Duration, retry bool, err error) {

	req, err := http.NewRequestWithContext(ctx, rq.method, rq.url, nil)
	if err != nil {
		return nil, 0, false, err
	}
	req.Header.Set("User-Agent", c.userAgent)
	for k, vs := range rq.header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	haveToken := false
	if !rq.anonymous && c.token != nil {
		tok, terr := c.token(ctx)
		if terr != nil {
			// A secrets box that cannot be opened must not take the whole Hub
			// offline: every public GGUF repository is reachable anonymously.
			c.log.Warn("hf: the stored token could not be read; continuing anonymously", "error", terr)
		} else if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
			haveToken = true
		}
	}

	httpResp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			// A canceled context is the caller's decision, never a retry.
			return nil, 0, false, err
		}
		return nil, 0, true, fmt.Errorf("hf: %s %s: %w", rq.method, rq.url, err)
	}

	closeBody := func() {
		// Draining a bounded prefix before closing lets the connection go back
		// to the idle pool instead of being torn down, which matters for the
		// per-shard HEADs of a sharded download.
		_, _ = io.Copy(io.Discard, io.LimitReader(httpResp.Body, 4<<10))
		_ = httpResp.Body.Close()
	}

	status := httpResp.StatusCode
	switch {
	case status >= 200 && status < 300:
		out := &response{status: status, header: httpResp.Header, finalURL: httpResp.Request.URL}
		if rq.stream {
			out.raw = httpResp.Body
			return out, 0, false, nil
		}
		defer func() { _ = httpResp.Body.Close() }()
		limit := rq.maxBody
		if limit <= 0 {
			limit = maxJSONBody
		}
		b, rerr := io.ReadAll(io.LimitReader(httpResp.Body, limit))
		if rerr != nil {
			return nil, 0, true, fmt.Errorf("hf: reading %s: %w", rq.url, rerr)
		}
		out.body = b
		return out, 0, false, nil

	case status == http.StatusNotModified:
		defer closeBody()
		return &response{status: status, header: httpResp.Header, finalURL: httpResp.Request.URL},
			0, false, nil

	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		defer closeBody()
		return nil, 0, false, classifyAccess(status, httpResp.Header, c.endpoint, rq.repo, haveToken)

	case status == http.StatusNotFound:
		defer closeBody()
		return nil, 0, false, fmt.Errorf("%w: %s", ErrNotFound, rq.url)

	case status == http.StatusTooManyRequests:
		defer closeBody()
		after := parseRetryAfter(httpResp.Header.Get("Retry-After"), c.now())
		return nil, after, true, &RateLimitError{RetryAfter: after, URL: rq.url}

	case status >= 500:
		defer closeBody()
		return nil, 0, true, &StatusError{Status: status, URL: rq.url}

	default:
		defer func() { _ = httpResp.Body.Close() }()
		snippet, _ := io.ReadAll(io.LimitReader(httpResp.Body, 512))
		return nil, 0, false, &StatusError{Status: status, URL: rq.url, Body: string(snippet)}
	}
}

// parseRetryAfter reads the header in both of its forms — delta-seconds and an
// HTTP date. A value that parses as neither is zero, which sends the caller to
// the ordinary jittered backoff rather than to a guess.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

// -----------------------------------------------------------------------------
// The TTL cache
// -----------------------------------------------------------------------------

func (c *Client) cacheGet(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.cache[key]
	if !ok {
		return nil, false
	}
	if c.now().Sub(e.fetchedAt) > c.ttl {
		delete(c.cache, key)
		return nil, false
	}
	return e.body, true
}

func (c *Client) cachePut(key string, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// A cache that grows without bound over a long-lived daemon is a leak. The
	// cap is generous — a user browsing the Hub for an hour will not reach it —
	// and the eviction is deliberately crude, because every entry is
	// reconstructible with one request.
	const maxEntries = 512
	if len(c.cache) >= maxEntries {
		for k := range c.cache {
			delete(c.cache, k)
			if len(c.cache) < maxEntries/2 {
				break
			}
		}
	}
	c.cache[key] = cacheEntry{body: body, fetchedAt: c.now()}
}

// InvalidateCache drops every cached body. `PUT /api/v1/hf/token` calls it: a
// user who has just signed in must see the gated repository they were refused a
// minute ago, and a 30-minute-old anonymous answer would tell them they still
// cannot.
func (c *Client) InvalidateCache() {
	c.mu.Lock()
	defer c.mu.Unlock()
	clear(c.cache)
}
