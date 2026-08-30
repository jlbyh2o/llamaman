package hf

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/jlbyh2o/llamaman/internal/gguf"
)

// Resolve URLs, file metadata, Range reads and the pre-download peek
// (DESIGN sections 7.1, 7.2, 7.4, 8.5).
//
// # Two strings that must never be conflated
//
// Section 7.2's write path records two values off the same response, and section
// 7.4 spends a whole table on why mixing them silently breaks resume forever:
//
//   - the BLOB NAME — `x-linked-etag`, falling back to `etag`, with quotes and
//     any `W/` prefix stripped. It names `blobs/<etag>`, equals the sha256 hex
//     for an LFS object, and is NEVER sent in a header.
//   - the HTTP VALIDATOR — the `ETag` response header of the FINAL response
//     after redirects, byte for byte, quotes and `W/` included, together with the
//     host that issued it. It is used for nothing but `If-Range`.
//
// Sending the blob name as `If-Range` cannot match any validator an origin will
// ever compare it against. The server answers `200`, the design's own rule
// discards the partial, and resume silently never works on any file, forever,
// while every test that stubs the origin passes. FileMeta keeps them in two
// differently named fields for exactly that reason.

// FileMeta is everything one HEAD (or one streamed GET's headers) tells this
// daemon about a file.
type FileMeta struct {
	// URL is the huggingface.co resolve URL that was requested — never the
	// signed CDN URL it redirected to. It is what `download_tasks.url` stores,
	// because a signed URL expires and a download that resumes tomorrow must
	// re-resolve rather than fail on a stale signature.
	URL string

	// Etag is the BLOB NAME. See the file comment.
	Etag string
	// Validator is the HTTP VALIDATOR, byte-exact. Empty when the response
	// carried no `ETag` at all.
	Validator string
	// ValidatorHost is the host that issued Validator. A redirect that lands
	// somewhere else next time invalidates it, which is the third row of
	// section 7.4's table.
	ValidatorHost string
	// LastModified is the fallback `If-Range` validator, verbatim.
	LastModified string

	// Size is `x-linked-size`, falling back to `Content-Length`.
	Size int64
	// Commit is `x-repo-commit`: the RESOLVED commit sha of the revision that
	// was requested. It is how a download that asked for `main` learns the
	// commit that `models.revision` must record — section 2.6 forbids storing a
	// branch name there, because `main` names a different tree next week.
	Commit string

	// AcceptsRanges reports an `Accept-Ranges: bytes` on the response. It is
	// advisory: the real test is whether a ranged request comes back `206`, and
	// section 7.4's restart-on-200 rule handles the answer either way.
	AcceptsRanges bool
}

// ResolveURL builds `{endpoint}/{repo}/resolve/{rev}/{path}`.
//
// revision empty means `main`. The path is escaped segment by segment: a file
// inside a directory keeps its separators, and a name with a space or a `+` is
// escaped rather than reinterpreted.
func (c *Client) ResolveURL(repo, revision, filePath string) (string, error) {
	if err := validateRepo(repo); err != nil {
		return "", err
	}
	rev := revision
	if rev == "" {
		rev = "main"
	}
	if err := validateRevision(rev); err != nil {
		return "", err
	}
	if err := validateFilePath(filePath); err != nil {
		return "", err
	}
	segments := strings.Split(filePath, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	return c.endpoint + "/" + repo + "/resolve/" + url.PathEscape(rev) + "/" +
		strings.Join(segments, "/"), nil
}

// Head performs the HEAD of section 7.2's write path step 1.
//
// It follows redirects, because the headers that matter — the CDN's `ETag`, the
// final `Content-Length` — belong to the response that would actually serve the
// bytes. The token is stripped on the way (client.go), so the CDN never sees it.
func (c *Client) Head(ctx context.Context, repo, revision, filePath string) (FileMeta, error) {
	raw, err := c.ResolveURL(repo, revision, filePath)
	if err != nil {
		return FileMeta{}, err
	}
	resp, err := c.do(ctx, request{method: http.MethodHead, url: raw, repo: repo})
	if err != nil {
		return FileMeta{}, err
	}
	return metaFrom(raw, resp.header, resp.finalURL), nil
}

// metaFrom reads a FileMeta off a response's headers. It is shared by Head and
// by the streamed GET of a transfer, so the two can never disagree about what a
// blob is called or which validator was issued.
func metaFrom(requestURL string, h http.Header, final *url.URL) FileMeta {
	m := FileMeta{URL: requestURL}

	// The blob name. `x-linked-etag` is the LFS object's own tag and is what
	// huggingface_hub names the blob after; `etag` is the fallback for a plain
	// git file. Both are de-quoted and `W/`-stripped, because a file name with
	// quotes in it is not a file name.
	m.Etag = normalizeBlobName(firstNonEmpty(h.Get("X-Linked-Etag"), h.Get("Etag")))

	// The HTTP validator, byte for byte. No trimming, no de-quoting: the whole
	// point is that it goes back out exactly as it came in.
	m.Validator = strings.TrimSpace(h.Get("Etag"))
	if final != nil {
		m.ValidatorHost = final.Host
	}
	m.LastModified = strings.TrimSpace(h.Get("Last-Modified"))

	if v := h.Get("X-Linked-Size"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			m.Size = n
		}
	}
	if m.Size == 0 {
		if v := h.Get("Content-Length"); v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				m.Size = n
			}
		}
	}
	m.Commit = strings.TrimSpace(h.Get("X-Repo-Commit"))
	m.AcceptsRanges = strings.EqualFold(strings.TrimSpace(h.Get("Accept-Ranges")), "bytes")
	return m
}

// normalizeBlobName strips the `W/` prefix and the surrounding quotes. What is
// left is a file name; for an LFS object it is the sha256 hex, which is why a
// blob already on disk at the right size can be linked without downloading
// anything (section 7.2 write path step 2).
func normalizeBlobName(etag string) string {
	e := strings.TrimSpace(etag)
	e = strings.TrimPrefix(e, "W/")
	e = strings.TrimPrefix(e, "w/")
	return strings.Trim(e, `"`)
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// -----------------------------------------------------------------------------
// The transfer request
// -----------------------------------------------------------------------------

// OpenParams describes one file transfer, resumed or fresh.
type OpenParams struct {
	// URL is the resolve URL. It is passed rather than rebuilt so a resumed task
	// uses the exact URL its row recorded.
	URL string
	// Repo is the repository, for the 401/403 classification.
	Repo string

	// Offset is where to resume from — the size of the `.incomplete` file. Zero
	// starts at the beginning and sends no `Range` at all.
	Offset int64

	// Validator, ValidatorHost and LastModified are what a previous transfer of
	// this URL recorded. They select the conditional header by section 7.4's
	// three-row rule, in ConditionalHeader.
	Validator     string
	ValidatorHost string
	LastModified  string
}

// Transfer is an open body plus what its headers said.
type Transfer struct {
	// Body is the response body. The caller owns closing it.
	Body io.ReadCloser
	// Meta is the metadata read off THIS response, which replaces whatever the
	// task row held: section 7.4's "on success the validator recorded from this
	// response replaces the stored one".
	Meta FileMeta
	// Resumed reports a `206 Partial Content` — the transfer continues from
	// Offset. False means the origin sent the whole file and the caller must
	// discard its partial and restart from zero (section 7.4).
	Resumed bool
	// TotalSize is the object's full length: `Content-Range`'s total for a 206,
	// `Content-Length` for a 200.
	TotalSize int64
}

// ConditionalHeader implements section 7.4's rule — the whole of it, in one
// function, because it is the rule the design says is silently broken
// everywhere it is re-derived:
//
//	condition                                             header sent
//	----------------------------------------------------  ---------------------------
//	strong ETag recorded from the host we are about to hit  If-Range: <validator>
//	no strong ETag but a Last-Modified was recorded         If-Range: <last_modified>
//	neither, or the host differs                            no If-Range at all
//
// The last row is the COMMON one on Hugging Face and it is deliberate: `resolve/`
// redirects to a CDN whose `ETag` for the same object need not equal
// `x-linked-etag` and may differ between two requests for the same bytes.
// Omitting `If-Range` there is safe rather than sloppy, because the whole-file
// SHA-256 is the real integrity gate — a resumed transfer that spliced the wrong
// bytes fails the digest, is deleted and retried, and can never produce a corrupt
// blob. `If-Range` is an optimization that avoids one wasted re-download, not the
// correctness mechanism.
//
// host is the host the request will be sent to. It is compared against
// ValidatorHost because a validator issued by a different CDN node is a validator
// this origin has never heard of.
func (p OpenParams) ConditionalHeader(host string) (name, value string) {
	if p.Offset <= 0 {
		return "", ""
	}
	v := strings.TrimSpace(p.Validator)
	weak := strings.HasPrefix(v, "W/") || strings.HasPrefix(v, "w/")
	sameHost := p.ValidatorHost != "" && strings.EqualFold(p.ValidatorHost, host)

	if v != "" && !weak && sameHost && isEntityTag(v) {
		return "If-Range", v
	}
	if lm := strings.TrimSpace(p.LastModified); lm != "" && sameHost {
		return "If-Range", lm
	}
	return "", ""
}

// isEntityTag reports whether v is a well-formed strong entity-tag: a
// quoted-string, per RFC 9110 section 8.8.3.
//
// This check is the structural half of the rule above, and it exists to make ONE
// specific mistake impossible rather than merely discouraged. The blob name is a
// bare token — for an LFS object, 64 hex characters with no quotes — and if it
// were ever passed into Validator by a caller that conflated the two columns, it
// would otherwise sail through as "a strong validator with no W/ prefix" and be
// sent as `If-Range`. Every origin would then answer `200`, every resume would
// silently restart from zero, and every test that stubs an origin would still
// pass. An unquoted value is not an entity-tag any origin issued, so refusing to
// send it costs nothing real: section 7.4 is explicit that omitting `If-Range`
// is safe, because the whole-file SHA-256 is the actual integrity gate.
func isEntityTag(v string) bool {
	return len(v) >= 2 && strings.HasPrefix(v, `"`) && strings.HasSuffix(v, `"`)
}

// Open starts one file transfer.
//
// A `206` continues from Offset. A `200` means the server ignored the range or
// the file changed upstream, and Transfer.Resumed is false so the caller
// discards its partial and restarts — the design's rule, applied by the one
// caller that can act on it. A `416` is reported as ErrNoRange for the same
// reason: the partial is longer than the object, which is a restart and not a
// failure.
func (c *Client) Open(ctx context.Context, p OpenParams) (*Transfer, error) {
	header := http.Header{}
	if p.Offset > 0 {
		header.Set("Range", "bytes="+strconv.FormatInt(p.Offset, 10)+"-")
		u, err := url.Parse(p.URL)
		if err != nil {
			return nil, fmt.Errorf("hf: parsing the resolve URL: %w", err)
		}
		if name, value := p.ConditionalHeader(u.Host); name != "" {
			header.Set(name, value)
		}
	}
	// The identity encoding is requested explicitly. A transparently gzipped
	// body would make `Content-Length` describe the compressed stream while the
	// bytes written to disk are the inflated ones, and every byte counter,
	// resume offset and size check downstream would be wrong.
	header.Set("Accept-Encoding", "identity")

	resp, err := c.do(ctx, request{
		method: http.MethodGet, url: p.URL, repo: p.Repo, header: header, stream: true,
	})
	if err != nil {
		if isRangeNotSatisfiable(err) {
			return nil, ErrNoRange
		}
		return nil, err
	}

	t := &Transfer{Body: resp.raw, Meta: metaFrom(p.URL, resp.header, resp.finalURL)}
	switch resp.status {
	case http.StatusPartialContent:
		t.Resumed = true
		start, total, err := parseContentRange(resp.header.Get("Content-Range"))
		if err != nil {
			_ = resp.raw.Close()
			return nil, err
		}
		// The START is checked, not merely the total. The caller opens the
		// `.incomplete` file O_APPEND and copies this body onto the end of it,
		// so a `206` that begins anywhere but at Offset splices the wrong bytes
		// into the middle of a blob. Section 7.4 leans on the whole-file SHA-256
		// to catch that — and it does, for an LFS object, whose blob name IS the
		// digest. A plain git blob has no digest to check, so this is the only
		// place the mistake can be caught at all, and it is cheap enough to make
		// unconditional.
		if start != p.Offset {
			_ = resp.raw.Close()
			return nil, fmt.Errorf(
				"hf: the origin answered a range request from byte %d with bytes starting at %d",
				p.Offset, start)
		}
		t.TotalSize = total
		// `Content-Length` on a 206 is the length of the RANGE, not of the
		// object, so Meta.Size — read from `x-linked-size` or that header — is
		// only trustworthy when the linked size was present. The authority here
		// is the `Content-Range` total.
		if t.TotalSize > 0 {
			t.Meta.Size = t.TotalSize
		}
	default:
		t.Resumed = false
		t.TotalSize = t.Meta.Size
	}
	return t, nil
}

// isRangeNotSatisfiable recognizes the 416 a partial longer than the object
// produces.
func isRangeNotSatisfiable(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Status == http.StatusRequestedRangeNotSatisfiable
}

// parseContentRange reads both halves of `bytes 100-199/40000`: the FIRST BYTE
// the response carries and the object's total length.
//
// Both are required, and both are refused rather than guessed at when the origin
// gives `*`. The total is what catches a file that changed upstream; the start is
// what catches a `206` whose bytes do not begin where the `Range` asked, which
// the caller would otherwise append onto a partial file at the wrong offset.
func parseContentRange(v string) (start, total int64, err error) {
	unreadable := func() (int64, int64, error) {
		return 0, 0, fmt.Errorf("hf: unreadable Content-Range %q", v)
	}
	spec := strings.TrimSpace(v)
	// The unit is `bytes` and nothing else; a range in any other unit is not
	// something this caller can splice.
	rest, ok := strings.CutPrefix(spec, "bytes ")
	if !ok {
		return unreadable()
	}
	span, after, ok := strings.Cut(strings.TrimSpace(rest), "/")
	if !ok || after == "" || after == "*" {
		return unreadable()
	}
	total, perr := strconv.ParseInt(after, 10, 64)
	if perr != nil {
		return unreadable()
	}
	first, _, ok := strings.Cut(span, "-")
	if !ok || first == "" {
		return unreadable()
	}
	start, perr = strconv.ParseInt(strings.TrimSpace(first), 10, 64)
	if perr != nil || start < 0 {
		return unreadable()
	}
	return start, total, nil
}

// -----------------------------------------------------------------------------
// The remote GGUF peek (section 8.5)
// -----------------------------------------------------------------------------

// rangeReader is internal/gguf's RangeReader over a resolve URL. It is the
// wiring section 8.5 deferred: "the remote half is an interface there and
// nothing more — no HTTP client, no URL and no token appears in that package;
// the wiring lands with the downloader".
type rangeReader struct {
	c    *Client
	url  string
	repo string
}

// ReadRangeAt fills p with the object's bytes starting at off.
//
// It follows io.ReaderAt's contract, which the parser depends on: fill p
// entirely or return an error, and return io.EOF only for a read that starts at
// or past the end of the object. A 416 is exactly that read, which is why it
// becomes io.EOF here rather than an error the parser cannot interpret.
func (r rangeReader) ReadRangeAt(ctx context.Context, p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	last := off + int64(len(p)) - 1
	header := http.Header{}
	header.Set("Range", "bytes="+strconv.FormatInt(off, 10)+"-"+strconv.FormatInt(last, 10))
	header.Set("Accept-Encoding", "identity")

	resp, err := r.c.do(ctx, request{
		method: http.MethodGet, url: r.url, repo: r.repo, header: header, stream: true,
	})
	if err != nil {
		if isRangeNotSatisfiable(err) {
			return 0, io.EOF
		}
		return 0, err
	}
	defer func() { _ = resp.raw.Close() }()

	if resp.status != http.StatusPartialContent && off > 0 {
		// The origin ignored the range and is streaming the whole object from
		// zero. Reading forward to `off` would download the gigabytes the peek
		// exists to avoid, so it is refused instead.
		return 0, fmt.Errorf("hf: %s does not support Range requests", r.url)
	}
	n, err := io.ReadFull(resp.raw, p)
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		if n == 0 {
			return 0, io.EOF
		}
		// A short read at the end of the object is what a header peek does on
		// its last window; the parser bounds its own reads, so a partial fill
		// with no error is wrong and io.ErrUnexpectedEOF is right.
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}

// RangeReaderFor returns a gguf.RangeReader over one file of one repository.
// Callers that already know the object's size wrap it with gguf.ParseRemote.
func (c *Client) RangeReaderFor(repo, revision, filePath string) (gguf.RangeReader, string, error) {
	raw, err := c.ResolveURL(repo, revision, filePath)
	if err != nil {
		return nil, "", err
	}
	return rangeReader{c: c, url: raw, repo: repo}, raw, nil
}

// Peek is `GET /api/v1/hf/peek/{repo...}?file=` (sections 3.6, 8.5): the GGUF
// header read over HTTP Range BEFORE downloading twenty gigabytes.
//
// It costs one HEAD and a handful of 1 MiB Range requests — a typical header is
// well under 2 MiB, and even a large tokenizer array is a few windows — so the
// fit panel can be exact about a model the user has not committed to yet. That
// is the difference between "download it and find out" and SPEC section 3.2's
// promise, and it is why the parser was written against an io.ReaderAt.
//
// Only shard 1 of a sharded set has the geometry, which is why a caller peeks
// the first shard and nothing else (section 7.3).
func (c *Client) Peek(ctx context.Context, repo, revision, filePath string, opts ...gguf.Option) (*gguf.File, error) {
	meta, err := c.Head(ctx, repo, revision, filePath)
	if err != nil {
		return nil, err
	}
	if meta.Size <= 0 {
		return nil, fmt.Errorf("hf: %s reported no size, so its header cannot be bounded", filePath)
	}
	rr, _, err := c.RangeReaderFor(repo, revision, filePath)
	if err != nil {
		return nil, err
	}
	return gguf.ParseRemote(ctx, rr, meta.Size, opts...)
}

// -----------------------------------------------------------------------------
// Validation
// -----------------------------------------------------------------------------

// validateRepo refuses anything that is not `name` or `org/name`.
//
// It exists because a repo id arrives from a URL path (`/hf/tree/{repo...}`) and
// is then interpolated into a Hub URL and into a cache directory name. A `..`
// segment would escape both, so the check is one rule applied at the only
// boundary where the value is still untrusted.
func validateRepo(repo string) error {
	if repo == "" {
		return fmt.Errorf("hf: a repository id is required")
	}
	if len(repo) > 256 {
		return fmt.Errorf("hf: repository id is too long")
	}
	parts := strings.Split(repo, "/")
	if len(parts) > 2 {
		return fmt.Errorf("hf: %q is not a repository id (expected `name` or `org/name`)", repo)
	}
	for _, p := range parts {
		if p == "" || p == "." || p == ".." {
			return fmt.Errorf("hf: %q is not a repository id", repo)
		}
		if strings.ContainsAny(p, "\\?#%") || strings.ContainsRune(p, 0) {
			return fmt.Errorf("hf: %q is not a repository id", repo)
		}
	}
	return nil
}

// validateRevision refuses a revision that is not a plausible branch, tag or
// sha. It is the same boundary rule as validateRepo: the value becomes a URL
// segment and a snapshot DIRECTORY NAME, and `..` in either is a path escape.
func validateRevision(rev string) error {
	if rev == "" || len(rev) > 256 {
		return fmt.Errorf("hf: %q is not a revision", rev)
	}
	if rev == "." || rev == ".." || strings.Contains(rev, "..") {
		return fmt.Errorf("hf: %q is not a revision", rev)
	}
	if strings.ContainsRune(rev, 0) || strings.HasPrefix(rev, "/") {
		return fmt.Errorf("hf: %q is not a revision", rev)
	}
	return nil
}

// validateFilePath refuses a path that would escape the snapshot directory it is
// about to become a file in.
func validateFilePath(p string) error {
	if p == "" || len(p) > 1024 {
		return fmt.Errorf("hf: %q is not a file path", p)
	}
	if strings.HasPrefix(p, "/") || strings.ContainsRune(p, 0) {
		return fmt.Errorf("hf: %q is not a file path", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("hf: %q is not a file path", p)
		}
	}
	return nil
}

// ValidateFilePath is the exported boundary check, for the download service:
// every file name a client sends in `POST /downloads {"files":[…]}` becomes a
// path inside a snapshot directory, and one of them containing `..` would write
// outside the cache root.
func ValidateFilePath(p string) error { return validateFilePath(p) }

// ValidateRepo is the exported repository-id check, for the same reason.
func ValidateRepo(repo string) error { return validateRepo(repo) }

// ValidateRevision is the exported revision check, for the same reason.
func ValidateRevision(rev string) error { return validateRevision(rev) }
