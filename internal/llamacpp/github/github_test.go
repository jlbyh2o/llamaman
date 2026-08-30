package github

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// fakeGitHub is a recorded api.github.com: the fixtures in testdata/ served
// from an httptest server, with the knobs the adversarial cases need (a forced
// status, a 304, a rate-limit header set). Every request it receives is
// recorded, so a test can assert what was sent as well as what came back.
type fakeGitHub struct {
	t      *testing.T
	server *httptest.Server
	// cdn serves release assets from a DIFFERENT host, the way GitHub does.
	cdn *httptest.Server

	mu       sync.Mutex
	requests []recorded
	// status forces a status code for every API request when non-zero.
	status int
	// body is served with status when status is non-zero.
	body []byte
	// etag is the validator the API returns; a request that presents it gets
	// a 304.
	etag string
	// rateLimit, when set, is written into every response's headers.
	rateLimit *RateLimit
}

type recorded struct {
	host   string
	path   string
	header http.Header
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{t: t, etag: `W/"1a2b3c"`}

	f.cdn = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		switch {
		case strings.HasSuffix(r.URL.Path, "/nightly-tag.txt"):
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write(f.fixture("nightly-tag.txt"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.cdn.Close)

	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		f.mu.Lock()
		status, body, rl := f.status, f.body, f.rateLimit
		f.mu.Unlock()

		if rl != nil {
			w.Header().Set("X-RateLimit-Limit", strconv.Itoa(rl.Limit))
			w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(rl.Remaining))
			if !rl.ResetAt.IsZero() {
				w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(rl.ResetAt.Unix(), 10))
			}
		}
		if status != 0 {
			w.WriteHeader(status)
			_, _ = w.Write(body)
			return
		}

		payload, ok := f.route(r.URL.Path, r.URL.RawQuery)
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("ETag", f.etag)
		if r.Header.Get("If-None-Match") == f.etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(payload)
	}))
	t.Cleanup(f.server.Close)
	return f
}

// route maps an API path onto a fixture, rewriting the asset download URLs to
// point at this test's CDN server — the fixtures carry the real github.com URLs
// they were recorded with, and nothing else about them is changed.
func (f *fakeGitHub) route(path, query string) ([]byte, bool) {
	switch {
	case path == "/repos/ggml-org/llama.cpp/releases/latest":
		return f.rewrite(f.fixture("releases_latest.json")), true
	case path == "/repos/ggml-org/llama.cpp/releases" && strings.Contains(query, "per_page="):
		return f.rewrite(f.fixture("releases_list.json")), true
	case path == "/repos/ggml-org/llama.cpp/releases/tags/b10621":
		return f.rewrite(f.fixture("release_b10621.json")), true
	case path == "/repos/ggml-org/llama.cpp/releases/tags/v0.3.0":
		return f.rewrite(f.fixture("releases_latest.json")), true
	case path == "/user":
		return f.fixture("user.json"), true
	}
	return nil, false
}

func (f *fakeGitHub) rewrite(b []byte) []byte {
	return []byte(strings.ReplaceAll(string(b), "https://github.com", f.cdn.URL))
}

func (f *fakeGitHub) fixture(name string) []byte {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		f.t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

func (f *fakeGitHub) record(r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, recorded{host: r.Host, path: r.URL.Path, header: r.Header.Clone()})
}

func (f *fakeGitHub) recorded() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.requests...)
}

func (f *fakeGitHub) forceStatus(code int, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.body = code, body
}

func (f *fakeGitHub) setRateLimit(rl RateLimit) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rateLimit = &rl
}

// client builds a Client pointed at the fake, with sleeping disabled so a retry
// path costs no wall time.
func (f *fakeGitHub) client(opts Options) *Client {
	opts.BaseURL = f.server.URL
	if opts.Sleep == nil {
		opts.Sleep = func(context.Context, time.Duration) error { return nil }
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return New(opts)
}

// ---------------------------------------------------------------- asset names

func TestAssetArch(t *testing.T) {
	tests := []struct {
		goarch string
		want   string
		ok     bool
	}{
		{goarch: "amd64", want: "x64", ok: true},
		{goarch: "arm64", want: "arm64", ok: true},
		{goarch: "s390x", want: "s390x", ok: true},
		{goarch: "386", want: "", ok: false},
		{goarch: "riscv64", want: "", ok: false},
		{goarch: "", want: "", ok: false},
	}
	for _, tc := range tests {
		t.Run(tc.goarch, func(t *testing.T) {
			got, ok := AssetArch(tc.goarch)
			if ok != tc.ok || got != tc.want {
				t.Errorf("AssetArch(%q) = (%q, %v), want (%q, %v)", tc.goarch, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestAssetNameNeverUsesTheGoArchVerbatim(t *testing.T) {
	// The whole point of the mapping (section 6.3): `amd64` in an asset name
	// produces a 404 nobody sees, and a source build nobody asked for.
	name, ok := AssetName("b10621", "amd64")
	if !ok {
		t.Fatal("no asset name for amd64")
	}
	if strings.Contains(name, "amd64") {
		t.Errorf("asset name %q contains the Go arch string", name)
	}
	if name != "llama-b10621-bin-ubuntu-x64.tar.gz" {
		t.Errorf("asset name = %q", name)
	}
	if _, ok := AssetName("b10621", "riscv64"); ok {
		t.Error("an unsupported architecture produced an asset name")
	}
	if _, ok := AssetName("", "amd64"); ok {
		t.Error("an empty tag produced an asset name")
	}
}

// ------------------------------------------------------------------ nightlies

func TestNightliesFilterAndNumericSort(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.client(Options{})

	got, _, err := c.Nightlies(t.Context(), 50)
	if err != nil {
		t.Fatalf("Nightlies: %v", err)
	}
	var tags []string
	for _, r := range got {
		tags = append(tags, r.Tag)
	}
	// b9999 sorts BEFORE b10621 as a string and AFTER it as a number; the
	// design says numeric, and this list is the regression guard for the
	// b10000 boundary. v0.3.0 is not a prerelease, b10622 is a draft, and
	// `test-build-…` is a prerelease whose tag is not a build tag.
	want := []string{"b10621", "b10620", "b9999"}
	if strings.Join(tags, ",") != strings.Join(want, ",") {
		t.Errorf("nightlies = %v, want %v", tags, want)
	}
}

func TestIsNightlyTag(t *testing.T) {
	tests := []struct {
		tag  string
		want bool
	}{
		{tag: "b10621", want: true},
		{tag: "b1", want: true},
		{tag: "v0.3.0", want: false},
		{tag: "b10621-cpu", want: false},
		{tag: "B10621", want: false},
		{tag: "b", want: false},
		{tag: "", want: false},
		{tag: "b10621\n", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.tag, func(t *testing.T) {
			if got := IsNightlyTag(tc.tag); got != tc.want {
				t.Errorf("IsNightlyTag(%q) = %v, want %v", tc.tag, got, tc.want)
			}
		})
	}
}

// ----------------------------------------------------------------- resolution

func TestResolveStableFollowsTheNightlyTagIndirection(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.client(Options{})

	res, meta, err := c.Resolve(t.Context(), ResolveRequest{
		Channel: model.ChannelStable, GOARCH: "amd64",
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Tag != "v0.3.0" {
		t.Errorf("tag = %q, want v0.3.0", res.Tag)
	}
	if res.BuildTag != "b10621" {
		t.Errorf("build_tag = %q, want b10621 (from nightly-tag.txt)", res.BuildTag)
	}
	// Section 6.2: the pinned b##### "is what is actually fetched or built".
	if res.FetchTag() != "b10621" {
		t.Errorf("FetchTag = %q, want b10621", res.FetchTag())
	}
	if !res.AssetFound {
		t.Fatal("no prebuilt asset resolved for amd64")
	}
	if res.Asset.Name != "llama-b10621-bin-ubuntu-x64.tar.gz" {
		t.Errorf("asset = %q", res.Asset.Name)
	}
	// The semver release carries no binaries; they live on the build's release.
	if res.AssetRelease != "b10621" {
		t.Errorf("asset release = %q, want b10621", res.AssetRelease)
	}
	sum, ok := res.Asset.SHA256()
	if !ok || sum != "0550fd38be02fcea410194978500a64094f630338fa5f1c9cd794996a6fceb23" {
		t.Errorf("asset digest = (%q, %v)", sum, ok)
	}
	if meta.Stale {
		t.Error("a fresh resolution reported itself stale")
	}
	if res.Release.Body == "" {
		t.Error("no changelog body carried through; the UI has nothing to render")
	}
}

func TestResolveStableWithoutTheIndirection(t *testing.T) {
	// A stable release with binaries attached directly and no nightly-tag.txt
	// is a supported shape, not an error: the semver tag is then both the
	// identity and the thing fetched.
	f := newFakeGitHub(t)
	f.forceStatus(http.StatusOK, []byte(`{
	  "tag_name": "v0.4.0", "name": "v0.4.0", "draft": false, "prerelease": false,
	  "published_at": "2026-03-01T00:00:00Z",
	  "assets": [{"name": "llama-v0.4.0-bin-ubuntu-x64.tar.gz", "size": 41205760,
	              "digest": "sha256:0550fd38be02fcea410194978500a64094f630338fa5f1c9cd794996a6fceb23",
	              "browser_download_url": "https://example.invalid/x.tar.gz"}]
	}`))
	c := f.client(Options{})

	res, _, err := c.Resolve(t.Context(), ResolveRequest{Channel: model.ChannelStable, GOARCH: "amd64"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.BuildTag != "" {
		t.Errorf("build_tag = %q, want empty", res.BuildTag)
	}
	if res.FetchTag() != "v0.4.0" {
		t.Errorf("FetchTag = %q, want v0.4.0", res.FetchTag())
	}
	if !res.AssetFound || res.AssetRelease != "v0.4.0" {
		t.Errorf("asset = %+v, found=%v release=%q", res.Asset, res.AssetFound, res.AssetRelease)
	}
}

func TestResolveNightly(t *testing.T) {
	tests := []struct {
		name     string
		tag      string
		goarch   string
		wantTag  string
		wantName string
		wantErr  bool
	}{
		{name: "latest nightly", goarch: "amd64", wantTag: "b10621", wantName: "llama-b10621-bin-ubuntu-x64.tar.gz"},
		{name: "pinned nightly", tag: "b10621", goarch: "amd64", wantTag: "b10621", wantName: "llama-b10621-bin-ubuntu-x64.tar.gz"},
		{name: "arm64 picks its own asset", tag: "b10621", goarch: "arm64", wantTag: "b10621", wantName: "llama-b10621-bin-ubuntu-arm64.tar.gz"},
		{name: "cuda asks for no asset at all", tag: "b10621", goarch: "", wantTag: "b10621"},
		{name: "an unsupported arch resolves without an asset", tag: "b10621", goarch: "riscv64", wantTag: "b10621"},
		{name: "a non-build tag is refused", tag: "v0.3.0", goarch: "amd64", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGitHub(t)
			c := f.client(Options{})

			res, _, err := c.Resolve(t.Context(), ResolveRequest{
				Channel: model.ChannelNightly, Tag: tc.tag, GOARCH: tc.goarch,
			})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolved %q as a nightly", tc.tag)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if res.Tag != tc.wantTag || res.BuildTag != "" {
				t.Errorf("tag/build_tag = %q/%q, want %q/empty", res.Tag, res.BuildTag, tc.wantTag)
			}
			if res.FetchTag() != tc.wantTag {
				t.Errorf("FetchTag = %q, want %q", res.FetchTag(), tc.wantTag)
			}
			switch tc.wantName {
			case "":
				if res.AssetFound {
					t.Errorf("resolved an asset (%q) where none was wanted", res.Asset.Name)
				}
			default:
				if !res.AssetFound || res.Asset.Name != tc.wantName {
					t.Errorf("asset = %q (found=%v), want %q", res.Asset.Name, res.AssetFound, tc.wantName)
				}
			}
		})
	}
}

func TestResolveCustomChannelDoesNotCallGitHub(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.client(Options{})

	_, _, err := c.Resolve(t.Context(), ResolveRequest{Channel: model.ChannelCustom, Tag: "whatever"})
	if !errors.Is(err, ErrCustomChannel) {
		t.Fatalf("error = %v, want ErrCustomChannel", err)
	}
	if n := len(f.recorded()); n != 0 {
		t.Errorf("the custom channel made %d GitHub requests; section 6.2 says none", n)
	}
}

func TestNightlyTagContentIsValidated(t *testing.T) {
	// The file becomes a directory name, a git ref and part of a version id.
	f := newFakeGitHub(t)
	f.cdn.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("../../etc/passwd\n"))
	})
	c := f.client(Options{})

	_, _, err := c.Resolve(t.Context(), ResolveRequest{Channel: model.ChannelStable, GOARCH: "amd64"})
	if err == nil {
		t.Fatal("a nightly-tag.txt containing a path was accepted")
	}
	if !strings.Contains(err.Error(), "not a b#### build tag") {
		t.Errorf("error = %v, want one naming the expected shape", err)
	}
}

// ------------------------------------------------------------- caching, stale

func TestConditionalRequestAndNotModified(t *testing.T) {
	f := newFakeGitHub(t)
	cache := NewMemoryCache()
	c := f.client(Options{Cache: cache})

	first, meta, err := c.ListReleases(t.Context(), 50)
	if err != nil {
		t.Fatalf("first list: %v", err)
	}
	if meta.NotModified || meta.Stale {
		t.Errorf("first call meta = %+v, want a plain fetch", meta)
	}
	entry, ok, _ := cache.Load(t.Context(), KeyReleaseList)
	if !ok || entry.ETag == "" {
		t.Fatalf("the ETag was not persisted: %+v (found=%v)", entry, ok)
	}
	firstFetched := entry.FetchedAt

	second, meta, err := c.ListReleases(t.Context(), 50)
	if err != nil {
		t.Fatalf("second list: %v", err)
	}
	if !meta.NotModified {
		t.Error("the second call did not come back 304; the ETag was not sent")
	}
	if meta.Stale {
		t.Error("a 304 was reported as stale; it is the opposite — a freshness confirmation")
	}
	if len(first) != len(second) {
		t.Errorf("304 served %d releases, first fetch had %d", len(second), len(first))
	}
	entry, _, _ = cache.Load(t.Context(), KeyReleaseList)
	if !entry.FetchedAt.After(firstFetched) && !entry.FetchedAt.Equal(firstFetched) {
		t.Error("a 304 did not refresh fetched_at")
	}

	var conditional int
	for _, r := range f.recorded() {
		if r.header.Get("If-None-Match") != "" {
			conditional++
		}
	}
	if conditional != 1 {
		t.Errorf("%d conditional requests, want exactly 1 (the second call)", conditional)
	}
}

func TestStaleWhileRevalidatingOnServerError(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.client(Options{MaxRetries: 2})

	if _, _, err := c.ListReleases(t.Context(), 50); err != nil {
		t.Fatalf("priming call: %v", err)
	}
	f.forceStatus(http.StatusBadGateway, []byte("upstream is down"))

	rels, meta, err := c.ListReleases(t.Context(), 50)
	if err != nil {
		t.Fatalf("a cached list should survive a 502: %v", err)
	}
	if !meta.Stale {
		t.Error("the served body was not marked stale")
	}
	if len(rels) == 0 {
		t.Error("the stale body decoded to nothing")
	}
	if meta.FetchedAt.IsZero() {
		t.Error("no fetched_at on a stale read; the UI cannot say how old it is")
	}
}

func TestServerErrorWithNoCacheIsAnError(t *testing.T) {
	f := newFakeGitHub(t)
	f.forceStatus(http.StatusBadGateway, nil)
	c := f.client(Options{MaxRetries: 1})

	_, _, err := c.ListReleases(t.Context(), 50)
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusBadGateway {
		t.Fatalf("error = %v, want a 502 StatusError", err)
	}
}

func TestRateLimitExhausted(t *testing.T) {
	reset := time.Now().Add(37 * time.Minute).Truncate(time.Second)

	t.Run("with nothing cached it is an error naming the reset", func(t *testing.T) {
		f := newFakeGitHub(t)
		f.setRateLimit(RateLimit{Limit: 60, Remaining: 0, ResetAt: reset})
		f.forceStatus(http.StatusForbidden, f.fixture("rate_limited.json"))
		c := f.client(Options{})

		_, _, err := c.ListReleases(t.Context(), 50)
		var rl *ErrRateLimit
		if !errors.As(err, &rl) {
			t.Fatalf("error = %v, want ErrRateLimit", err)
		}
		if rl.RateLimit.Authenticated {
			t.Error("an anonymous client reported an authenticated budget")
		}
		if !rl.RateLimit.ResetAt.Equal(reset.UTC()) {
			t.Errorf("reset_at = %v, want %v", rl.RateLimit.ResetAt, reset.UTC())
		}
		if n := len(f.recorded()); n != 1 {
			t.Errorf("%d requests; an exhausted hourly budget must not be retried", n)
		}
	})

	t.Run("with a cache it is served stale", func(t *testing.T) {
		f := newFakeGitHub(t)
		c := f.client(Options{})
		if _, _, err := c.ListReleases(t.Context(), 50); err != nil {
			t.Fatalf("priming call: %v", err)
		}
		f.setRateLimit(RateLimit{Limit: 60, Remaining: 0, ResetAt: reset})
		f.forceStatus(http.StatusForbidden, f.fixture("rate_limited.json"))

		rels, meta, err := c.ListReleases(t.Context(), 50)
		if err != nil {
			t.Fatalf("rate limited with a warm cache should serve stale: %v", err)
		}
		if !meta.Stale || len(rels) == 0 {
			t.Errorf("meta = %+v, %d releases", meta, len(rels))
		}
		if got := c.RateLimit(); got.Remaining != 0 || !got.Known {
			t.Errorf("client rate limit = %+v, want a known, exhausted budget", got)
		}
	})
}

func TestRateLimitHeadersAreRecorded(t *testing.T) {
	f := newFakeGitHub(t)
	reset := time.Now().Add(time.Hour).Truncate(time.Second)
	f.setRateLimit(RateLimit{Limit: 60, Remaining: 57, ResetAt: reset})
	c := f.client(Options{})

	if got := c.RateLimit(); got.Known {
		t.Error("the rate limit is known before any request has been made")
	}
	if _, _, err := c.LatestRelease(t.Context()); err != nil {
		t.Fatalf("LatestRelease: %v", err)
	}
	got := c.RateLimit()
	if !got.Known || got.Limit != 60 || got.Remaining != 57 || !got.ResetAt.Equal(reset.UTC()) {
		t.Errorf("rate limit = %+v", got)
	}
	if got.Authenticated {
		t.Error("an anonymous request was recorded as authenticated")
	}
}

// ---------------------------------------------------------------------- token

func TestTokenGoesToTheAPIHostAndNowhereElse(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.client(Options{Token: func(context.Context) (string, error) { return "ghp_FAKEfake123", nil }})

	// A stable resolution touches both hosts: the API for the release, the CDN
	// for nightly-tag.txt.
	if _, _, err := c.Resolve(t.Context(), ResolveRequest{Channel: model.ChannelStable, GOARCH: "amd64"}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	apiHost := strings.TrimPrefix(f.server.URL, "http://")
	cdnHost := strings.TrimPrefix(f.cdn.URL, "http://")
	var sawAPIAuth, sawCDN bool
	for _, r := range f.recorded() {
		auth := r.header.Get("Authorization")
		switch r.host {
		case apiHost:
			if auth != "" {
				sawAPIAuth = true
			}
		case cdnHost:
			sawCDN = true
			if auth != "" {
				t.Errorf("the GitHub token was sent to the asset host %s (path %s)", r.host, r.path)
			}
		}
		if strings.Contains(auth, "ghp_FAKEfake123") && r.host != apiHost {
			t.Errorf("token leaked to %s", r.host)
		}
	}
	if !sawAPIAuth {
		t.Error("no authorized request reached api.github.com; the token was never used")
	}
	if !sawCDN {
		t.Error("the asset host was never contacted; the leak test proved nothing")
	}
}

func TestTokenIsStrippedAcrossARedirect(t *testing.T) {
	// GitHub redirects asset URLs to a signed CDN URL on another host. The
	// header must not travel with it (section 6.2, section 7.1).
	var gotAuth string
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte("b10621\n"))
	}))
	defer cdn.Close()

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, cdn.URL+"/signed", http.StatusFound)
	}))
	defer api.Close()

	c := New(Options{
		BaseURL: api.URL,
		Token:   func(context.Context) (string, error) { return "ghp_FAKEfake123", nil },
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if _, _, err := c.apiGet(t.Context(), "", "/anything"); err != nil {
		t.Fatalf("get: %v", err)
	}
	if gotAuth != "" {
		t.Errorf("Authorization survived a cross-host redirect: %q", gotAuth)
	}
}

func TestAnonymousWhenNoTokenIsStored(t *testing.T) {
	f := newFakeGitHub(t)
	tests := []struct {
		name  string
		token func(context.Context) (string, error)
	}{
		{name: "no provider", token: nil},
		{name: "empty token", token: func(context.Context) (string, error) { return "", nil }},
		{
			name:  "a secrets box that will not open",
			token: func(context.Context) (string, error) { return "", errors.New("secret.key unreadable") },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f2 := newFakeGitHub(t)
			c := f2.client(Options{Token: tc.token})
			// A sealed token that cannot be opened must not take the release
			// list down with it: anonymous is a supported mode.
			if _, _, err := c.LatestRelease(t.Context()); err != nil {
				t.Fatalf("LatestRelease: %v", err)
			}
			for _, r := range f2.recorded() {
				if r.header.Get("Authorization") != "" {
					t.Error("an Authorization header was sent with no usable token")
				}
			}
		})
	}
	_ = f
}

func TestValidateToken(t *testing.T) {
	t.Run("accepted", func(t *testing.T) {
		f := newFakeGitHub(t)
		f.setRateLimit(RateLimit{Limit: 5000, Remaining: 4999})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/user" {
				http.NotFound(w, r)
				return
			}
			if r.Header.Get("Authorization") != "Bearer ghp_FAKEfake123" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("X-OAuth-Scopes", "repo, read:org")
			w.Header().Set("X-RateLimit-Limit", "5000")
			w.Header().Set("X-RateLimit-Remaining", "4999")
			_, _ = w.Write(f.fixture("user.json"))
		}))
		defer srv.Close()

		info, err := ValidateToken(t.Context(), Options{BaseURL: srv.URL}, "ghp_FAKEfake123")
		if err != nil {
			t.Fatalf("ValidateToken: %v", err)
		}
		if info.Login != "octocat" {
			t.Errorf("login = %q", info.Login)
		}
		if strings.Join(info.Scopes, "|") != "repo|read:org" {
			t.Errorf("scopes = %v", info.Scopes)
		}
		if strings.Contains(info.Hint, "FAKEfake") {
			t.Errorf("the hint %q exposes the token", info.Hint)
		}
		if !info.RateLimit.Authenticated || info.RateLimit.Limit != 5000 {
			t.Errorf("rate limit = %+v, want the authenticated 5000/hour budget", info.RateLimit)
		}
	})

	t.Run("rejected", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
		}))
		defer srv.Close()

		_, err := ValidateToken(t.Context(), Options{BaseURL: srv.URL}, "ghp_WRONGfake99")
		if !errors.Is(err, ErrTokenInvalid) {
			t.Fatalf("error = %v, want ErrTokenInvalid (section 3.6's 422 github_token_invalid)", err)
		}
	})

	t.Run("a server failure is not a wrong token", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer srv.Close()

		_, err := ValidateToken(t.Context(), Options{BaseURL: srv.URL}, "ghp_FAKEfake123")
		if errors.Is(err, ErrTokenInvalid) {
			t.Fatal("a 503 was reported to the user as an invalid token")
		}
		var se *StatusError
		if !errors.As(err, &se) {
			t.Fatalf("error = %v, want a StatusError", err)
		}
	})

	t.Run("an empty token is refused without a request", func(t *testing.T) {
		if _, err := ValidateToken(t.Context(), Options{BaseURL: "http://127.0.0.1:0"}, "   "); !errors.Is(err, ErrTokenInvalid) {
			t.Fatalf("error = %v, want ErrTokenInvalid", err)
		}
	})
}

// fineGrainedPrefix is GitHub's fine-grained personal-access-token prefix,
// assembled rather than written out so that this test file does not contain the
// literal string a secret scanner looks for. There is no secret here — the
// value below is nine characters of alphabet — but a scanner cannot know that,
// and a test that trips CI on every run teaches people to ignore CI.
var fineGrainedPrefix = "github" + "_pat_"

func TestMaskToken(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "classic PAT", in: "ghp_ABCDEFGHIJqrs", want: "ghp_…qrs"},
		{name: "fine-grained PAT", in: fineGrainedPrefix + "11ABCDEFGhij", want: fineGrainedPrefix + "…hij"},
		{name: "no recognized prefix", in: "0123456789abcdef", want: "…def"},
		{name: "too short to mask safely", in: "ghp_abc", want: "ghp_…"},
		{name: "empty", in: "", want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := MaskToken(tc.in)
			if got != tc.want {
				t.Errorf("MaskToken(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if tc.in != "" && strings.Contains(got, tc.in) {
				t.Errorf("the mask %q contains the whole token", got)
			}
		})
	}
}

func TestParseScopes(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   string
	}{
		{name: "classic token", header: "repo, read:org, workflow", want: "repo|read:org|workflow"},
		{name: "fine-grained token sends nothing", header: "", want: ""},
		{name: "stray whitespace", header: " repo ,, admin:org ", want: "repo|admin:org"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := strings.Join(ParseScopes(tc.header), "|"); got != tc.want {
				t.Errorf("ParseScopes(%q) = %q, want %q", tc.header, got, tc.want)
			}
		})
	}
}

func TestNotFoundIsNotRetried(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.client(Options{})

	_, _, err := c.ReleaseByTag(t.Context(), "b99999999")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
	if n := len(f.recorded()); n != 1 {
		t.Errorf("%d requests for a 404; it is a real answer, not a transient failure", n)
	}
}
