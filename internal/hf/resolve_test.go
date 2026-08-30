package hf

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jlbyh2o/llamaman/internal/gguf"
	"github.com/jlbyh2o/llamaman/internal/gguf/ggufbuild"
)

// The resume contract's tests (DESIGN section 7.4).
//
// Section 15 asks for exactly these: "an origin whose `ETag` differs from
// `x-linked-etag`, an origin that returns a DIFFERENT `ETag` on the second
// request, and an origin that returns a weak `W/` validator — each asserting
// that the request either carries a byte-exact strong validator or no `If-Range`
// at all… A regression guard asserts the de-quoted blob name is never sent in
// any header."

// TestMetaSeparatesTheBlobNameFromTheValidator is the distinction the whole
// resume path turns on. They are read off the same response and they are not the
// same string.
func TestMetaSeparatesTheBlobNameFromTheValidator(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		header        http.Header
		wantEtag      string // the BLOB NAME: de-quoted, W/-stripped
		wantValidator string // the HTTP VALIDATOR: byte-exact
		wantSize      int64
	}{
		{
			name: "x-linked-etag wins over etag",
			header: http.Header{
				"X-Linked-Etag":  {`"5f2b1c9d"`},
				"Etag":           {`"cdn-cache-key-2f8a"`},
				"X-Linked-Size":  {"4920736256"},
				"Content-Length": {"135"},
			},
			wantEtag: "5f2b1c9d", wantValidator: `"cdn-cache-key-2f8a"`, wantSize: 4920736256,
		},
		{
			name: "a weak validator is kept verbatim and the blob name is stripped",
			header: http.Header{
				"Etag":           {`W/"abc123"`},
				"Content-Length": {"64"},
			},
			wantEtag: "abc123", wantValidator: `W/"abc123"`, wantSize: 64,
		},
		{
			name: "content-length is the fallback size",
			header: http.Header{
				"X-Linked-Etag":  {`"deadbeef"`},
				"Content-Length": {"1024"},
			},
			wantEtag: "deadbeef", wantValidator: "", wantSize: 1024,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := metaFrom("https://hub/repo/resolve/main/f.gguf", tc.header, nil)
			if m.Etag != tc.wantEtag {
				t.Errorf("blob name = %q, want %q", m.Etag, tc.wantEtag)
			}
			if m.Validator != tc.wantValidator {
				t.Errorf("validator = %q, want %q (byte-exact, quotes and W/ included)",
					m.Validator, tc.wantValidator)
			}
			if m.Size != tc.wantSize {
				t.Errorf("size = %d, want %d", m.Size, tc.wantSize)
			}
		})
	}
}

// TestConditionalHeaderRule is section 7.4's three-row table, in code.
//
// The last row is the COMMON one on Hugging Face, and it is deliberate: the
// resolve URL redirects to a CDN whose ETag for the same object need not equal
// `x-linked-etag` and may differ between two requests for the same bytes.
// Omitting `If-Range` there is safe rather than sloppy, because the whole-file
// SHA-256 is the real integrity gate.
func TestConditionalHeaderRule(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		params    OpenParams
		host      string
		wantName  string
		wantValue string
	}{
		{
			name:   "a fresh transfer sends no conditional header at all",
			params: OpenParams{Offset: 0, Validator: `"abc"`, ValidatorHost: "cdn.example"},
			host:   "cdn.example",
		},
		{
			name:   "a strong validator from the same host is sent byte-exact",
			params: OpenParams{Offset: 100, Validator: `"abc123"`, ValidatorHost: "cdn.example"},
			host:   "cdn.example", wantName: "If-Range", wantValue: `"abc123"`,
		},
		{
			name:   "the host comparison is case-insensitive",
			params: OpenParams{Offset: 100, Validator: `"abc123"`, ValidatorHost: "CDN.Example"},
			host:   "cdn.example", wantName: "If-Range", wantValue: `"abc123"`,
		},
		{
			name: "a weak validator falls through to Last-Modified",
			params: OpenParams{
				Offset: 100, Validator: `W/"abc123"`, ValidatorHost: "cdn.example",
				LastModified: "Wed, 21 Oct 2026 07:28:00 GMT",
			},
			host: "cdn.example", wantName: "If-Range", wantValue: "Wed, 21 Oct 2026 07:28:00 GMT",
		},
		{
			name:   "a weak validator with no Last-Modified sends nothing",
			params: OpenParams{Offset: 100, Validator: `W/"abc"`, ValidatorHost: "cdn.example"},
			host:   "cdn.example",
		},
		{
			name: "a different host invalidates the validator entirely",
			params: OpenParams{
				Offset: 100, Validator: `"abc123"`, ValidatorHost: "cdn-a.example",
				LastModified: "Wed, 21 Oct 2026 07:28:00 GMT",
			},
			host: "cdn-b.example",
		},
		{
			name:   "nothing recorded sends a bare Range",
			params: OpenParams{Offset: 100},
			host:   "cdn.example",
		},
		{
			// The structural guard: a bare token is not an entity-tag any origin
			// issued, and the blob name is exactly such a token. A caller that
			// conflated the two columns would otherwise have it sent as
			// `If-Range`, every origin would answer 200, and resume would
			// silently restart from zero forever.
			name: "an unquoted value is not an entity-tag and is never sent",
			params: OpenParams{
				Offset:    100,
				Validator: strings.Repeat("d", 64), ValidatorHost: "cdn.example",
			},
			host: "cdn.example",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			name, value := tc.params.ConditionalHeader(tc.host)
			if name != tc.wantName || value != tc.wantValue {
				t.Errorf("ConditionalHeader = (%q, %q), want (%q, %q)",
					name, value, tc.wantName, tc.wantValue)
			}
		})
	}
}

// rangeOrigin is a Range-capable origin with the adversarial modes section 15
// asks for.
type rangeOrigin struct {
	t       *testing.T
	content []byte
	// blobName is `x-linked-etag`: the sha256 an LFS object is named by.
	blobName string
	// validators are returned as `ETag` in order, one per request, so a test can
	// make the second request disagree with the first.
	validators []string
	// ignoreRange answers 200 with the whole body even when a Range was sent.
	ignoreRange bool
	// truncateAt cuts the FIRST response short after this many bytes.
	truncateAt int

	mu       sync.Mutex
	requests []http.Header
}

func (o *rangeOrigin) validatorFor(n int) string {
	if len(o.validators) == 0 {
		return ""
	}
	if n < len(o.validators) {
		return o.validators[n]
	}
	return o.validators[len(o.validators)-1]
}

func (o *rangeOrigin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	n := len(o.requests)
	o.requests = append(o.requests, r.Header.Clone())
	o.mu.Unlock()

	// The regression guard section 15 names: the de-quoted blob name is never
	// sent in ANY header. Sending it as `If-Range` would match no validator the
	// origin will ever compare it against, the server would answer 200, the
	// partial would be discarded, and resume would silently never work on any
	// file, forever, while every stubbed test passed.
	for name, values := range r.Header {
		for _, v := range values {
			if strings.Contains(v, o.blobName) {
				o.t.Errorf("the blob name %q was sent in header %s: %q", o.blobName, name, v)
			}
		}
	}

	w.Header().Set("X-Linked-Etag", `"`+o.blobName+`"`)
	w.Header().Set("X-Linked-Size", strconv.Itoa(len(o.content)))
	w.Header().Set("X-Repo-Commit", "4f0ac1c0a1f0ee0b1c2d3e4f5a6b7c8d9e0f1a2b")
	w.Header().Set("Accept-Ranges", "bytes")
	if v := o.validatorFor(n); v != "" {
		w.Header().Set("Etag", v)
	}

	if r.Method == http.MethodHead {
		w.Header().Set("Content-Length", strconv.Itoa(len(o.content)))
		w.WriteHeader(http.StatusOK)
		return
	}

	body := o.content
	status := http.StatusOK
	rng := r.Header.Get("Range")
	if rng != "" && !o.ignoreRange {
		var start int64
		if _, err := fmtSscanRange(rng, &start); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if start >= int64(len(o.content)) {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		body = o.content[start:]
		w.Header().Set("Content-Range",
			"bytes "+strconv.FormatInt(start, 10)+"-"+
				strconv.Itoa(len(o.content)-1)+"/"+strconv.Itoa(len(o.content)))
		status = http.StatusPartialContent
	}

	if o.truncateAt > 0 && n == 0 && o.truncateAt < len(body) {
		// A body that ends early: the `Content-Length` promises more than the
		// bytes that arrive, which is exactly what a dropped connection looks
		// like to the reader.
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(status)
		_, _ = w.Write(body[:o.truncateAt])
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return
	}

	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (o *rangeOrigin) request(i int) http.Header {
	o.mu.Lock()
	defer o.mu.Unlock()
	if i >= len(o.requests) {
		return nil
	}
	return o.requests[i]
}

// fmtSscanRange reads `bytes=<start>-`.
func fmtSscanRange(v string, start *int64) (int, error) {
	_, after, ok := strings.Cut(v, "=")
	if !ok {
		return 0, errors.New("no =")
	}
	first, _, _ := strings.Cut(after, "-")
	n, err := strconv.ParseInt(first, 10, 64)
	if err != nil {
		return 0, err
	}
	*start = n
	return 1, nil
}

func TestOpenResumesWithAStrongValidator(t *testing.T) {
	t.Parallel()

	content := []byte(strings.Repeat("abcdefgh", 128))
	o := &rangeOrigin{
		t: t, content: content, blobName: strings.Repeat("a", 64),
		validators: []string{`"v1"`, `"v1"`},
	}
	srv := httptest.NewServer(o)
	defer srv.Close()

	c := newClient(t, srv)
	// First transfer: learn the validator.
	head, err := c.Head(context.Background(), "org/repo", "main", "model.gguf")
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if head.Etag != strings.Repeat("a", 64) {
		t.Fatalf("blob name = %q", head.Etag)
	}
	if head.Commit != "4f0ac1c0a1f0ee0b1c2d3e4f5a6b7c8d9e0f1a2b" {
		t.Errorf("x-repo-commit did not survive: %q", head.Commit)
	}

	// Resume from halfway with the validator this host issued.
	tr, err := c.Open(context.Background(), OpenParams{
		URL: head.URL, Repo: "org/repo", Offset: 512,
		Validator: head.Validator, ValidatorHost: head.ValidatorHost,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = tr.Body.Close() }()

	if !tr.Resumed {
		t.Fatal("Resumed = false on a 206")
	}
	if tr.TotalSize != int64(len(content)) {
		t.Errorf("total = %d, want %d", tr.TotalSize, len(content))
	}
	if got := o.request(1).Get("If-Range"); got != `"v1"` {
		t.Errorf("If-Range = %q, want the byte-exact strong validator", got)
	}
	rest, _ := io.ReadAll(tr.Body)
	if string(rest) != string(content[512:]) {
		t.Error("the resumed body is not the tail of the object")
	}
}

// TestOpenRestartsWhenTheOriginIgnoresTheRange is section 7.4's "a `200` means
// the server ignored the range or the file changed upstream". Resumed=false is
// how the transfer learns to discard its partial.
func TestOpenRestartsWhenTheOriginIgnoresTheRange(t *testing.T) {
	t.Parallel()

	content := []byte(strings.Repeat("z", 256))
	o := &rangeOrigin{t: t, content: content, blobName: strings.Repeat("b", 64), ignoreRange: true}
	srv := httptest.NewServer(o)
	defer srv.Close()

	c := newClient(t, srv)
	url, err := c.ResolveURL("org/repo", "main", "model.gguf")
	if err != nil {
		t.Fatalf("ResolveURL: %v", err)
	}
	tr, err := c.Open(context.Background(), OpenParams{URL: url, Repo: "org/repo", Offset: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = tr.Body.Close() }()

	if tr.Resumed {
		t.Fatal("Resumed = true on a 200; the partial would be spliced onto a whole-file body")
	}
	if tr.TotalSize != int64(len(content)) {
		t.Errorf("total = %d, want the whole object", tr.TotalSize)
	}
}

// TestOpenRefusesA206ThatStartsAtTheWrongOffset is the resume-corruption window.
//
// The caller derives its offset from the `.incomplete` file, sends
// `Range: bytes=<offset>-`, and on a `206` opens that file O_APPEND and copies
// the body onto the end of it. An origin that answers `bytes 0-…` — a
// misconfigured cache, a proxy that rewrote the range, a hostile mirror — has
// its first bytes spliced into the middle of the blob. Checking the total alone
// does not catch it, because the total in that header is right.
//
// For an LFS object the whole-file SHA-256 catches the result, since the blob
// name IS the digest. A plain git blob has a 40-hex etag that is not a digest, so
// it is verified on size alone and would be published to the shared cache with
// the wrong bytes. This is the check that makes both cases safe.
func TestOpenRefusesA206ThatStartsAtTheWrongOffset(t *testing.T) {
	t.Parallel()

	content := []byte(strings.Repeat("q", 4096))
	cases := []struct {
		name         string
		contentRange string
		wantIn       string
	}{
		{
			name:         "the origin restarted the range at zero",
			contentRange: "bytes 0-4095/4096",
			wantIn:       "starting at 0",
		},
		{
			name:         "the origin answered a later offset than was asked for",
			contentRange: "bytes 2048-4095/4096",
			wantIn:       "starting at 2048",
		},
		{
			name:         "the origin gave an unreadable range",
			contentRange: "bytes */4096",
			wantIn:       "unreadable Content-Range",
		},
		{
			name:         "the origin gave no range at all",
			contentRange: "",
			wantIn:       "unreadable Content-Range",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Linked-Etag", `"`+strings.Repeat("e", 64)+`"`)
				w.Header().Set("X-Linked-Size", strconv.Itoa(len(content)))
				w.Header().Set("Accept-Ranges", "bytes")
				if r.Method == http.MethodHead {
					w.Header().Set("Content-Length", strconv.Itoa(len(content)))
					w.WriteHeader(http.StatusOK)
					return
				}
				if tc.contentRange != "" {
					w.Header().Set("Content-Range", tc.contentRange)
				}
				w.Header().Set("Content-Length", strconv.Itoa(len(content)))
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write(content)
			}))
			defer srv.Close()

			c := newClient(t, srv)
			url, err := c.ResolveURL("org/repo", "main", "model.gguf")
			if err != nil {
				t.Fatalf("ResolveURL: %v", err)
			}
			tr, err := c.Open(context.Background(), OpenParams{
				URL: url, Repo: "org/repo", Offset: 1024,
			})
			if err == nil {
				_ = tr.Body.Close()
				t.Fatalf("Open accepted a 206 with Content-Range %q for a request from byte 1024",
					tc.contentRange)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("err = %v, want one mentioning %q", err, tc.wantIn)
			}
		})
	}
}

func TestParseContentRange(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		in        string
		wantStart int64
		wantTotal int64
		wantErr   bool
	}{
		{name: "a whole-object range", in: "bytes 0-4095/4096", wantStart: 0, wantTotal: 4096},
		{name: "a resumed range", in: "bytes 1024-4095/4096", wantStart: 1024, wantTotal: 4096},
		{name: "leading and trailing space", in: "  bytes 8-9/10  ", wantStart: 8, wantTotal: 10},
		{name: "an unknown total", in: "bytes 0-9/*", wantErr: true},
		{name: "an unsatisfied range", in: "bytes */4096", wantErr: true},
		{name: "another unit entirely", in: "items 0-9/10", wantErr: true},
		{name: "no unit", in: "0-9/10", wantErr: true},
		{name: "empty", in: "", wantErr: true},
		{name: "a negative start", in: "bytes -5-9/10", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start, total, err := parseContentRange(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseContentRange(%q) = (%d, %d, nil), want an error", tc.in, start, total)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseContentRange(%q) = %v", tc.in, err)
			}
			if start != tc.wantStart || total != tc.wantTotal {
				t.Errorf("parseContentRange(%q) = (%d, %d), want (%d, %d)",
					tc.in, start, total, tc.wantStart, tc.wantTotal)
			}
		})
	}
}

func TestOpenReportsErrNoRangeOn416(t *testing.T) {
	t.Parallel()

	o := &rangeOrigin{t: t, content: []byte("short"), blobName: strings.Repeat("c", 64)}
	srv := httptest.NewServer(o)
	defer srv.Close()

	c := newClient(t, srv)
	url, _ := c.ResolveURL("org/repo", "main", "model.gguf")
	_, err := c.Open(context.Background(), OpenParams{URL: url, Repo: "org/repo", Offset: 9999})
	if !errors.Is(err, ErrNoRange) {
		t.Fatalf("err = %v, want ErrNoRange", err)
	}
}

// TestBlobNameIsNeverSentAsIfRange is the regression guard, stated as its own
// test so a future refactor that "simplifies" OpenParams into one etag field
// fails here with a message that says why.
func TestBlobNameIsNeverSentAsIfRange(t *testing.T) {
	t.Parallel()

	blob := strings.Repeat("d", 64)
	o := &rangeOrigin{t: t, content: []byte(strings.Repeat("x", 100)), blobName: blob}
	srv := httptest.NewServer(o)
	defer srv.Close()

	c := newClient(t, srv)
	url, _ := c.ResolveURL("org/repo", "main", "model.gguf")

	// The caller passes the BLOB NAME where a validator would go — the exact
	// mistake section 7.4 exists to prevent. The rule refuses it, because a blob
	// name carries no quotes and is therefore not a validator any origin issued;
	// the origin's own handler asserts it never appears in a header.
	tr, err := c.Open(context.Background(), OpenParams{
		URL: url, Repo: "org/repo", Offset: 10,
		Validator: blob, ValidatorHost: strings.TrimPrefix(srv.URL, "http://"),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_ = tr.Body.Close()
}

// TestPeekReadsAHeaderOverRange is section 8.5's whole point: a 20 GB quant is
// measured from its first megabyte, before a byte of it is downloaded.
func TestPeekReadsAHeaderOverRange(t *testing.T) {
	t.Parallel()

	b := ggufbuild.New("qwen3").
		Set("qwen3.block_count", ggufbuild.U32(36)).
		Set("qwen3.context_length", ggufbuild.U32(40960)).
		Set("qwen3.embedding_length", ggufbuild.U32(4096)).
		Set("qwen3.attention.head_count", ggufbuild.U32(32)).
		Set("qwen3.attention.head_count_kv", ggufbuild.U32(8)).
		Layers(2, 4096, 11008, gguf.TypeQ4_K)
	full := b.Full()

	o := &rangeOrigin{t: t, content: full, blobName: strings.Repeat("e", 64)}
	srv := httptest.NewServer(o)
	defer srv.Close()

	f, err := newClient(t, srv).Peek(context.Background(), "org/repo", "main", "model.gguf")
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	shape := f.Shape()
	if shape.Architecture != "qwen3" || shape.BlockCount != 36 || shape.ContextLength != 40960 {
		t.Errorf("shape = %+v", shape)
	}

	// The peek must not have downloaded the object. One HEAD plus a small number
	// of windowed Range reads is the contract; anything that streams the whole
	// file has defeated the purpose.
	o.mu.Lock()
	reqs := len(o.requests)
	o.mu.Unlock()
	if reqs > 4 {
		t.Errorf("the peek made %d requests; a header read should take a handful", reqs)
	}
	for i := 1; i < reqs; i++ {
		if o.request(i).Get("Range") == "" {
			t.Errorf("request %d carried no Range header", i)
		}
	}
}
