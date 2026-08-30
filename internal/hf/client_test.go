package hf

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The Hub client's unit tests (DESIGN section 15: "an httptest server serving
// canned `/api/models`, `/tree` and a Range-capable `resolve`, plus adversarial
// modes — ignore the range, truncate mid-stream, wrong ETag, 429 with
// `Retry-After`, and a redirect to a second host asserting the `Authorization`
// header is dropped").
//
// Every response body is a checked-in fixture under `testdata/`, never a live
// call: the rule is that a unit test makes no network request, and a fixture is
// what makes a Hub schema change show up as a deliberate edit to a file rather
// than as a green suite over an API that moved.

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// newClient builds a client against srv with no sleeping, so a retry test costs
// microseconds rather than seconds.
func newClient(t *testing.T, srv *httptest.Server, opts ...func(*Options)) *Client {
	t.Helper()
	o := Options{
		Endpoint:  srv.URL,
		UserAgent: "llamaman/test",
		Sleep:     func(context.Context, time.Duration) error { return nil },
		CacheTTL:  -1, // off by default; the cache test turns it on
	}
	for _, f := range opts {
		f(&o)
	}
	return New(o)
}

func TestSearchParsesTheFixture(t *testing.T) {
	t.Parallel()

	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Link", `<`+r.URL.String()+`&cursor=NEXT>; rel="next"`)
		_, _ = w.Write(fixture(t, "search.json"))
	}))
	defer srv.Close()

	page, err := newClient(t, srv).Search(context.Background(), SearchParams{Query: "qwen"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	// `filter=gguf` is not optional: this product runs llama.cpp, and a search
	// that returned safetensors repositories would offer a model it cannot run.
	if !strings.Contains(gotQuery, "filter=gguf") {
		t.Errorf("query %q does not carry filter=gguf", gotQuery)
	}
	if len(page.Items) != 3 {
		t.Fatalf("got %d results, want 3", len(page.Items))
	}
	if page.NextCursor != "NEXT" {
		t.Errorf("next_cursor = %q, want NEXT", page.NextCursor)
	}

	// The tri-state `gated` field is the reason hubModel decodes it as raw JSON:
	// the Hub sends `false` for an open repository and the STRING "manual" or
	// "auto" for a gated one, so a bool field would fail to unmarshal on exactly
	// the repositories that matter most here.
	cases := []struct {
		id     string
		gated  bool
		author string
	}{
		{"bartowski/Qwen3-8B-GGUF", false, "bartowski"},
		{"meta-llama/Llama-3.1-8B-Instruct-GGUF", true, "meta-llama"},
		{"gpt2", false, ""},
	}
	for i, want := range cases {
		got := page.Items[i]
		if got.ID != want.id || got.Gated != want.gated || got.Author != want.author {
			t.Errorf("item %d = {%s gated=%v author=%s}, want {%s gated=%v author=%s}",
				i, got.ID, got.Gated, got.Author, want.id, want.gated, want.author)
		}
	}
	if page.Items[0].GGUF == nil || page.Items[0].GGUF.ContextLength != 40960 {
		t.Errorf("the Hub's gguf summary did not survive decoding: %+v", page.Items[0].GGUF)
	}
	if page.Items[2].GGUF != nil {
		t.Error("a repository the Hub computed no gguf summary for must report nil, not a zero")
	}
}

func TestModelParsesTheFixture(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/models/bartowski/Qwen3-8B-GGUF" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write(fixture(t, "model.json"))
	}))
	defer srv.Close()

	info, err := newClient(t, srv).Model(context.Background(), "bartowski/Qwen3-8B-GGUF")
	if err != nil {
		t.Fatalf("Model: %v", err)
	}
	if info.SHA != "4f0ac1c0a1f0ee0b1c2d3e4f5a6b7c8d9e0f1a2b" {
		t.Errorf("sha = %q", info.SHA)
	}
	if len(info.Siblings) != 6 {
		t.Errorf("siblings = %d, want 6", len(info.Siblings))
	}
	if info.CardData["license"] != "apache-2.0" {
		t.Errorf("card_data did not survive: %+v", info.CardData)
	}
}

// TestTreeUsesLFSSize is the single most consequential assertion in this
// package. For an LFS entry the top-level `size` is the ~130-byte POINTER;
// reading it makes a 4.9 GB quantization look free, breaks the fit calculator
// outright and waves a download past the disk guard that exists to stop it
// (SPEC section 3.2, DESIGN section 7.1).
func TestTreeUsesLFSSize(t *testing.T) {
	t.Parallel()

	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write(fixture(t, "tree.json"))
	}))
	defer srv.Close()

	entries, err := newClient(t, srv).Tree(context.Background(), "bartowski/Qwen3-8B-GGUF", "main")
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	// `expand=1` is what makes the `lfs` object appear at all. Without it every
	// number below is wrong by four orders of magnitude with no symptom.
	if !strings.Contains(gotQuery, "expand=1") || !strings.Contains(gotQuery, "recursive=1") {
		t.Errorf("query %q is missing recursive=1&expand=1", gotQuery)
	}

	want := map[string]struct {
		size int64
		lfs  bool
		oid  string
	}{
		".gitattributes":                    {1519, false, "1a2b3c4d5e6f"},
		"README.md":                         {8342, false, "aa11bb22cc33"},
		"Qwen3-8B-Q4_K_M.gguf":              {4920736256, true, "5f2b1c9d0e7a4b3c8d6e5f4a3b2c1d0e9f8a7b6c5d4e3f2a1b0c9d8e7f6a5b4c"},
		"Qwen3-8B-Q8_0-00001-of-00002.gguf": {4294967296, true, "1111111111111111111111111111111111111111111111111111111111111111"},
		"Qwen3-8B-Q8_0-00002-of-00002.gguf": {4294967296, true, "2222222222222222222222222222222222222222222222222222222222222222"},
		"mmproj-Qwen3-8B-f16.gguf":          {1048576, true, "3333333333333333333333333333333333333333333333333333333333333333"},
	}
	if len(entries) != len(want) {
		t.Fatalf("got %d entries, want %d (the directory row must be dropped)", len(entries), len(want))
	}
	for _, e := range entries {
		w, ok := want[e.Path]
		if !ok {
			t.Errorf("unexpected entry %s", e.Path)
			continue
		}
		if e.Size != w.size {
			t.Errorf("%s size = %d, want %d (the LFS size, never the pointer size)",
				e.Path, e.Size, w.size)
		}
		if e.LFS != w.lfs || e.OID != w.oid {
			t.Errorf("%s = {lfs=%v oid=%s}, want {lfs=%v oid=%s}", e.Path, e.LFS, e.OID, w.lfs, w.oid)
		}
	}
}

func TestGroupTreeShardsAndProjector(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "tree.json"))
	}))
	defer srv.Close()

	entries, err := newClient(t, srv).Tree(context.Background(), "bartowski/Qwen3-8B-GGUF", "")
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	groups := GroupTree(entries)

	// Three groups: one single-file quant, one two-shard set, one projector.
	// The non-GGUF files are not groups at all — this product downloads GGUFs.
	if len(groups) != 3 {
		t.Fatalf("got %d groups, want 3: %+v", len(groups), groups)
	}
	byKey := map[string]FileGroup{}
	for _, g := range groups {
		byKey[g.Key] = g
	}

	q4 := byKey["Qwen3-8B-Q4_K_M.gguf"]
	if q4.ShardTotal != 1 || len(q4.Files) != 1 || q4.TotalBytes != 4920736256 || q4.QuantLabel != "Q4_K_M" {
		t.Errorf("Q4_K_M group = %+v", q4)
	}

	q8 := byKey["Qwen3-8B-Q8_0"]
	if q8.ShardTotal != 2 || len(q8.Files) != 2 || !q8.Complete {
		t.Errorf("Q8_0 shard set = %+v", q8)
	}
	if q8.TotalBytes != 4294967296*2 {
		t.Errorf("Q8_0 total = %d, want the sum of both shards", q8.TotalBytes)
	}
	// Shard order is the set's order, not the map's: llama.cpp is handed shard 1
	// and finds the rest by name.
	if q8.Files[0].Path != "Qwen3-8B-Q8_0-00001-of-00002.gguf" {
		t.Errorf("shard 1 = %s", q8.Files[0].Path)
	}

	mm := byKey["mmproj-Qwen3-8B-f16.gguf"]
	if !mm.Mmproj {
		t.Errorf("the projector was not recognized: %+v", mm)
	}
}

// TestGatedMapping is section 3.6's whole gated-repo UX in one table. Four
// responses that are all "401 or 403" have four different remedies, and
// answering them identically leaves a user staring at a repository page that
// works in their browser with no idea what to do.
func TestGatedMapping(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		status    int
		errorCode string
		token     string
		check     func(t *testing.T, err error)
	}{
		{
			name: "gated with no token", status: http.StatusUnauthorized, errorCode: "GatedRepo",
			check: func(t *testing.T, err error) {
				g, ok := IsGated(err)
				if !ok {
					t.Fatalf("err = %v, want a GatedError", err)
				}
				if g.Repo != "meta-llama/Llama-3.1-8B" {
					t.Errorf("repo = %q", g.Repo)
				}
				if !strings.HasSuffix(g.RequestURL, "/meta-llama/Llama-3.1-8B") {
					t.Errorf("request_url = %q, want the repository page to link out to", g.RequestURL)
				}
			},
		},
		{
			name: "gated with a token is still gated", status: http.StatusForbidden,
			errorCode: "GatedRepo", token: "hf_XXXX",
			check: func(t *testing.T, err error) {
				if _, ok := IsGated(err); !ok {
					t.Fatalf("err = %v, want a GatedError", err)
				}
			},
		},
		{
			name: "grant pending review is still gated", status: http.StatusForbidden,
			errorCode: "GatedRepoAccessRequestPending", token: "hf_XXXX",
			check: func(t *testing.T, err error) {
				if _, ok := IsGated(err); !ok {
					t.Fatalf("err = %v, want a GatedError", err)
				}
			},
		},
		{
			name: "RepoNotFound with no token means sign in", status: http.StatusUnauthorized,
			errorCode: "RepoNotFound",
			check: func(t *testing.T, err error) {
				p, ok := IsPrivate(err)
				if !ok {
					t.Fatalf("err = %v, want a PrivateError", err)
				}
				if p.HaveToken {
					t.Error("have_token must be false when no credential was sent")
				}
				if !strings.Contains(p.Error(), "sign in") {
					t.Errorf("message = %q, want it to say to sign in", p.Error())
				}
			},
		},
		{
			name:   "RepoNotFound with a token means this token cannot see it",
			status: http.StatusForbidden, errorCode: "RepoNotFound", token: "hf_XXXX",
			check: func(t *testing.T, err error) {
				p, ok := IsPrivate(err)
				if !ok {
					t.Fatalf("err = %v, want a PrivateError", err)
				}
				if !p.HaveToken {
					t.Error("have_token must be true when a credential was sent")
				}
			},
		},
		{
			name:   "a bare 401 with a token is a statement about the token",
			status: http.StatusUnauthorized, token: "hf_XXXX",
			check: func(t *testing.T, err error) {
				if !errors.Is(err, ErrTokenInvalid) {
					t.Fatalf("err = %v, want ErrTokenInvalid", err)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.errorCode != "" {
					w.Header().Set("X-Error-Code", tc.errorCode)
				}
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			c := newClient(t, srv, func(o *Options) {
				if tc.token != "" {
					o.Token = func(context.Context) (string, error) { return tc.token, nil }
				}
			})
			_, err := c.Head(context.Background(), "meta-llama/Llama-3.1-8B", "main", "model.gguf")
			if err == nil {
				t.Fatal("Head succeeded on a refusal")
			}
			tc.check(t, err)
		})
	}
}

// TestTokenIsStrippedOnCrossHostRedirect is the CDN case. `resolve/` redirects
// to a host with its own signed URL, and carrying the user's token there would
// hand it to somebody who never needed it — and, worse, several CDNs reject a
// request that carries both their signature and an Authorization header, so
// this is a correctness fix as much as a security one.
func TestTokenIsStrippedOnCrossHostRedirect(t *testing.T) {
	t.Parallel()

	var cdnAuth atomic.Value
	cdnAuth.Store("")
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cdnAuth.Store(r.Header.Get("Authorization"))
		w.Header().Set("Etag", `"cdn-validator"`)
		w.Header().Set("Content-Length", "4")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte("data"))
		}
	}))
	defer cdn.Close()

	var originAuth atomic.Value
	originAuth.Store("")
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originAuth.Store(r.Header.Get("Authorization"))
		w.Header().Set("X-Linked-Etag", `"deadbeef"`)
		http.Redirect(w, r, cdn.URL+"/signed?sig=abc", http.StatusFound)
	}))
	defer origin.Close()

	c := newClient(t, origin, func(o *Options) {
		o.Token = func(context.Context) (string, error) { return "hf_XXXX", nil }
	})
	if _, err := c.Head(context.Background(), "org/repo", "main", "model.gguf"); err != nil {
		t.Fatalf("Head: %v", err)
	}

	if got := originAuth.Load().(string); got != "Bearer hf_XXXX" {
		t.Errorf("the origin saw Authorization = %q, want the bearer token", got)
	}
	if got := cdnAuth.Load().(string); got != "" {
		t.Errorf("the CDN saw Authorization = %q, want it stripped on the host change", got)
	}
}

// TestRetryHonorsRetryAfter asserts the 429 path: the request is retried, the
// header is respected, and the eventual 200 is the answer.
func TestRetryHonorsRetryAfter(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write(fixture(t, "model.json"))
	}))
	defer srv.Close()

	var slept []time.Duration
	var mu sync.Mutex
	c := newClient(t, srv, func(o *Options) {
		o.Sleep = func(_ context.Context, d time.Duration) error {
			mu.Lock()
			slept = append(slept, d)
			mu.Unlock()
			return nil
		}
	})
	if _, err := c.Model(context.Background(), "bartowski/Qwen3-8B-GGUF"); err != nil {
		t.Fatalf("Model: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want 2", calls.Load())
	}
	if len(slept) != 1 || slept[0] != 2*time.Second {
		t.Errorf("slept %v, want one wait of exactly the Retry-After value", slept)
	}
}

func TestRetryGivesUpAndReportsTheRateLimit(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := newClient(t, srv, func(o *Options) { o.MaxTries = 3 })
	_, err := c.Model(context.Background(), "org/repo")
	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("err = %v, want a RateLimitError", err)
	}
	if calls.Load() != 3 {
		t.Errorf("calls = %d, want exactly MaxTries", calls.Load())
	}
}

// TestServerErrorsRetryAndClientErrorsDoNot pins which statuses spend the
// budget. Retrying a 404 or a gate is a guaranteed waste of four round trips
// and four seconds of a user's attention.
func TestServerErrorsRetryAndClientErrorsDoNot(t *testing.T) {
	t.Parallel()

	cases := []struct {
		status    int
		wantCalls int32
	}{
		{http.StatusInternalServerError, 3},
		{http.StatusBadGateway, 3},
		{http.StatusNotFound, 1},
		{http.StatusForbidden, 1},
		{http.StatusBadRequest, 1},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			c := newClient(t, srv, func(o *Options) { o.MaxTries = 3 })
			if _, err := c.Model(context.Background(), "org/repo"); err == nil {
				t.Fatal("expected an error")
			}
			if calls.Load() != tc.wantCalls {
				t.Errorf("calls = %d, want %d", calls.Load(), tc.wantCalls)
			}
		})
	}
}

// TestCacheServesSearchAndTree covers the 30-minute TTL window, and — as
// importantly — that a resolve HEAD is NOT cached: it has to re-ask the origin,
// because that is where the validator for the next resume comes from.
func TestCacheServesSearchAndTree(t *testing.T) {
	t.Parallel()

	var treeCalls, headCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			headCalls.Add(1)
			w.Header().Set("X-Linked-Etag", `"abc"`)
			w.WriteHeader(http.StatusOK)
			return
		}
		treeCalls.Add(1)
		_, _ = w.Write(fixture(t, "tree.json"))
	}))
	defer srv.Close()

	now := time.Now()
	c := newClient(t, srv, func(o *Options) {
		o.CacheTTL = 30 * time.Minute
		o.Now = func() time.Time { return now }
	})
	ctx := context.Background()
	for range 3 {
		if _, err := c.Tree(ctx, "org/repo", "main"); err != nil {
			t.Fatalf("Tree: %v", err)
		}
		if _, err := c.Head(ctx, "org/repo", "main", "model.gguf"); err != nil {
			t.Fatalf("Head: %v", err)
		}
	}
	if treeCalls.Load() != 1 {
		t.Errorf("tree calls = %d, want 1 inside the TTL window", treeCalls.Load())
	}
	if headCalls.Load() != 3 {
		t.Errorf("HEAD calls = %d, want 3: a resolve HEAD is never cached", headCalls.Load())
	}

	// Past the window, and after an explicit invalidation, the Hub is asked
	// again. The invalidation is what `PUT /hf/token` calls: a user who has just
	// signed in must not be told they still cannot see a gated repository.
	now = now.Add(31 * time.Minute)
	if _, err := c.Tree(ctx, "org/repo", "main"); err != nil {
		t.Fatalf("Tree: %v", err)
	}
	if treeCalls.Load() != 2 {
		t.Errorf("tree calls = %d, want 2 once the TTL expired", treeCalls.Load())
	}
	c.InvalidateCache()
	if _, err := c.Tree(ctx, "org/repo", "main"); err != nil {
		t.Fatalf("Tree: %v", err)
	}
	if treeCalls.Load() != 3 {
		t.Errorf("tree calls = %d, want 3 after InvalidateCache", treeCalls.Load())
	}
}

// TestLimiterReservesASlotForInteractiveWork is section 7.1's "one client-side
// limiter so a bulk metadata refresh cannot starve a user-initiated search". A
// single-queue limiter cannot make that promise: a hundred queued background
// requests would sit in front of the search however fair the queue is.
func TestLimiterReservesASlotForInteractiveWork(t *testing.T) {
	t.Parallel()

	l := newLimiter(3)
	ctx := context.Background()

	// Fill every slot a background caller is allowed to take.
	var releases []func()
	for i := range 2 {
		rel, err := l.acquire(ctx, PriorityBackground)
		if err != nil {
			t.Fatalf("background acquire %d: %v", i, err)
		}
		releases = append(releases, rel)
	}

	// A third background request must block; an interactive one must not.
	blocked, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, err := l.acquire(blocked, PriorityBackground); err == nil {
		t.Fatal("a third background request was admitted; the reserved slot was spent")
	}

	rel, err := l.acquire(ctx, PriorityInteractive)
	if err != nil {
		t.Fatalf("the interactive caller was starved: %v", err)
	}
	rel()
	for _, r := range releases {
		r()
	}
}

func TestCardIsRawMarkdownAndAMissingOneIsNotAnError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "no-card") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/raw/main/README.md") {
			t.Errorf("path = %q, want the /raw/ form", r.URL.Path)
		}
		_, _ = w.Write(fixture(t, "card.md"))
	}))
	defer srv.Close()

	c := newClient(t, srv)
	card, err := c.Card(context.Background(), "org/repo", "")
	if err != nil {
		t.Fatalf("Card: %v", err)
	}
	// Returned RAW. Rendering is internal/mdrender's job behind
	// `GET /hf/card/{repo...}`, and nothing in this package interprets a byte of
	// what is, after all, markdown written by a stranger.
	if !strings.Contains(card, "# Qwen3-8B GGUF") {
		t.Errorf("card = %q, want the raw markdown", card)
	}

	// A repository with no README is ordinary; a 404 on the card is not a 404 on
	// the model.
	empty, err := c.Card(context.Background(), "org/no-card", "")
	if err != nil {
		t.Fatalf("Card on a repository with no README: %v", err)
	}
	if empty != "" {
		t.Errorf("card = %q, want empty", empty)
	}
}

func TestValidateTokenUsesThePresentedCredential(t *testing.T) {
	t.Parallel()

	var seen atomic.Value
	seen.Store("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Store(r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") != "Bearer hf_PRESENTED" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write(fixture(t, "whoami.json"))
	}))
	defer srv.Close()

	// The client is configured with a DIFFERENT stored token. Validation must
	// send the one the user just pasted: mixing the two would let the daemon
	// store a bad token as valid because the good one it already had answered
	// for it.
	opts := Options{
		Endpoint: srv.URL,
		Token:    func(context.Context) (string, error) { return "hf_STORED", nil },
	}
	info, err := ValidateToken(context.Background(), opts, "hf_PRESENTED")
	if err != nil {
		t.Fatalf("ValidateToken: %v", err)
	}
	if got := seen.Load().(string); got != "Bearer hf_PRESENTED" {
		t.Errorf("sent %q, want the presented token", got)
	}
	if info.Name != "example-user" {
		t.Errorf("name = %q", info.Name)
	}
	if len(info.Scopes) != 2 {
		t.Errorf("scopes = %v", info.Scopes)
	}
	// The hint is the only form of the token that leaves this package.
	if info.Hint != "hf_…TED" {
		t.Errorf("hint = %q, want the masked form", info.Hint)
	}
	if strings.Contains(info.Hint, "PRESENTED") {
		t.Error("the hint disclosed the token")
	}
}

func TestMaskToken(t *testing.T) {
	t.Parallel()

	cases := []struct{ in, want string }{
		{"", ""},
		{"hf_abcdefghijklAbC", "hf_…AbC"},
		{"hf_short", "hf_…"},
		{"nopeprefixvalue", "…lue"},
	}
	for _, tc := range cases {
		if got := MaskToken(tc.in); got != tc.want {
			t.Errorf("MaskToken(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestValidateRepoAndPathRefusePathEscapes(t *testing.T) {
	t.Parallel()

	// A repo id and a file name both become path segments in a Hub URL AND in a
	// cache directory. A `..` in either escapes both, so the check lives at the
	// one boundary where the value is still untrusted.
	for _, bad := range []string{"", "a/b/c", "../etc", "org/..", "org/na\x00me"} {
		if err := ValidateRepo(bad); err == nil {
			t.Errorf("ValidateRepo(%q) was accepted", bad)
		}
	}
	for _, good := range []string{"gpt2", "bartowski/Qwen3-8B-GGUF"} {
		if err := ValidateRepo(good); err != nil {
			t.Errorf("ValidateRepo(%q) = %v", good, err)
		}
	}
	for _, bad := range []string{"", "/abs", "../x", "a/../../b", "a/./b"} {
		if err := ValidateFilePath(bad); err == nil {
			t.Errorf("ValidateFilePath(%q) was accepted", bad)
		}
	}
	for _, good := range []string{"model.gguf", "original/model.gguf"} {
		if err := ValidateFilePath(good); err != nil {
			t.Errorf("ValidateFilePath(%q) = %v", good, err)
		}
	}
}
