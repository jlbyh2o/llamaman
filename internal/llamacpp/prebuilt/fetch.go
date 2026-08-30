package prebuilt

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The download (DESIGN section 6.4 step 1): "the asset into `tmp/` with the same
// resumable downloader as models (section 7.4); SHA-256 computed inline and
// compared with GitHub's digest when present."
//
// The interface is what matters here. `Fetcher` is declared by this package —
// its consumer — so that when internal/hf/download's resumable downloader
// exists it drops in behind this seam with its rate limiting, its progress
// reporting and its retry policy, and nothing in the prebuilt pipeline changes.
// HTTPFetcher below is the straightforward implementation the pipeline uses in
// the meantime: resume from a partial file, hash inline, verify before the file
// is handed on.
//
// Hashing INLINE rather than re-reading the file afterwards is not an
// optimization, it is the correctness property: a 40 MB tarball read twice can
// change between the two reads, and the thing that gets extracted must be the
// thing that was hashed.

// Fetcher downloads a release asset to a local path.
type Fetcher interface {
	// Fetch downloads url into dst, returning the SHA-256 of the bytes that
	// ended up there. It may resume a partial `dst.part` from a previous
	// attempt. `expectSHA256`, when non-empty, is the digest the result must
	// match — a mismatch is an error and the file is not left in place.
	Fetch(ctx context.Context, url, dst, expectSHA256 string, progress ProgressFunc) (string, error)
}

// ProgressFunc is called with bytes-so-far and the total when known (zero when
// the server did not say). Nil is allowed.
type ProgressFunc func(done, total int64)

// ErrChecksumMismatch means the bytes that arrived are not the bytes GitHub
// published. It is deliberately its own error: a mismatch is not a transient
// network problem to retry silently — it is a corrupted mirror, a truncated
// resume, or something worse, and it must be visible.
var ErrChecksumMismatch = errors.New("prebuilt: downloaded file does not match the published checksum")

// HTTPFetcher is the default Fetcher.
type HTTPFetcher struct {
	// Client is the HTTP client. Nil builds one with no total timeout —
	// a 40 MB download on a slow link must not be killed by a clock — and a
	// generous response-header timeout instead.
	Client *http.Client
	// UserAgent is sent verbatim.
	UserAgent string
	// Resume enables ranged resumption of a `.part` file. Default true.
	NoResume bool
}

var _ Fetcher = (*HTTPFetcher)(nil)

// PartSuffix is appended to the destination while a download is in flight, so
// an interrupted transfer is never mistaken for a complete file.
const PartSuffix = ".part"

// Fetch implements Fetcher.
func (f *HTTPFetcher) Fetch(ctx context.Context, url, dst, expectSHA256 string, progress ProgressFunc) (string, error) {
	client := f.Client
	if client == nil {
		client = &http.Client{
			Transport: &http.Transport{
				ResponseHeaderTimeout: 60 * time.Second,
				IdleConnTimeout:       90 * time.Second,
			},
		}
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return "", err
	}
	part := dst + PartSuffix

	// A resumable download must hash from the FIRST byte, so an existing
	// partial file is re-read through the hasher before the transfer continues.
	// That costs one sequential read of what is already on disk and buys the
	// property that the digest always covers the whole file.
	h := sha256.New()
	var offset int64
	if !f.NoResume {
		var err error
		offset, err = rehashPartial(part, h)
		if err != nil {
			return "", err
		}
	} else {
		_ = os.Remove(part)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	if f.UserAgent != "" {
		req.Header.Set("User-Agent", f.UserAgent)
	}
	// No Authorization header, ever: a release asset is served from a CDN with
	// its own signed URL, and section 6.2 forbids sending the GitHub token
	// anywhere but api.github.com.
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("prebuilt: downloading %s: %w", url, err)
	}
	defer resp.Body.Close()

	flags := os.O_WRONLY | os.O_CREATE
	switch {
	case offset > 0 && resp.StatusCode == http.StatusPartialContent:
		// The body is about to be appended to the partial file, so the response
		// must begin at exactly the byte the `Range` asked for. An origin that
		// answers `bytes 0-…` to a `bytes=<offset>-` request would otherwise
		// splice its first bytes into the middle of the archive. The digest
		// check below catches it whenever GitHub published one — and says
		// nothing useful when it did not, which is why the offset is verified
		// rather than inferred.
		start, err := contentRangeStart(resp.Header.Get("Content-Range"))
		if err != nil {
			return "", fmt.Errorf("prebuilt: downloading %s: %w", url, err)
		}
		if start != offset {
			return "", fmt.Errorf(
				"prebuilt: downloading %s: asked for bytes from %d and the server answered from %d",
				url, offset, start)
		}
		flags |= os.O_APPEND
	case resp.StatusCode == http.StatusOK:
		// The server ignored the range, or there was nothing to resume. Start
		// over rather than appending to a prefix the response does not follow.
		h.Reset()
		offset = 0
		flags |= os.O_TRUNC
	default:
		return "", fmt.Errorf("prebuilt: downloading %s: unexpected status %d %s",
			url, resp.StatusCode, http.StatusText(resp.StatusCode))
	}

	total := resp.ContentLength
	if total > 0 {
		total += offset
	}

	out, err := os.OpenFile(part, flags, 0o640)
	if err != nil {
		return "", err
	}
	done := offset
	pw := &progressWriter{w: io.MultiWriter(out, h), done: &done, total: total, fn: progress}
	if _, err := io.Copy(pw, resp.Body); err != nil {
		out.Close()
		// The partial file is KEPT: the next attempt resumes from it.
		return "", fmt.Errorf("prebuilt: downloading %s: %w", url, err)
	}
	if err := out.Close(); err != nil {
		return "", err
	}

	sum := hex.EncodeToString(h.Sum(nil))
	if expectSHA256 != "" && !strings.EqualFold(sum, expectSHA256) {
		// A wrong file must not be left where a resume would extend it.
		_ = os.Remove(part)
		return sum, fmt.Errorf("%w: got %s, expected %s", ErrChecksumMismatch, sum, expectSHA256)
	}
	if err := os.Rename(part, dst); err != nil {
		return sum, fmt.Errorf("prebuilt: finalizing %s: %w", dst, err)
	}
	return sum, nil
}

// contentRangeStart reads the first byte position out of `bytes 100-199/40000`.
// A `206` with no readable `Content-Range` is refused rather than trusted: the
// caller is about to append the body to a partial file, and "where do these
// bytes go" is not a question to answer by assumption.
func contentRangeStart(v string) (int64, error) {
	spec := strings.TrimSpace(v)
	rest, ok := strings.CutPrefix(spec, "bytes ")
	if !ok {
		return 0, fmt.Errorf("unreadable Content-Range %q on a 206", v)
	}
	span, _, ok := strings.Cut(strings.TrimSpace(rest), "/")
	if !ok {
		return 0, fmt.Errorf("unreadable Content-Range %q on a 206", v)
	}
	first, _, ok := strings.Cut(span, "-")
	if !ok || first == "" {
		return 0, fmt.Errorf("unreadable Content-Range %q on a 206", v)
	}
	n, err := strconv.ParseInt(strings.TrimSpace(first), 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("unreadable Content-Range %q on a 206", v)
	}
	return n, nil
}

// rehashPartial reads an existing partial file through h and returns its size.
func rehashPartial(part string, h hash.Hash) (int64, error) {
	f, err := os.Open(part)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()
	n, err := io.Copy(h, f)
	if err != nil {
		return 0, fmt.Errorf("prebuilt: re-reading the partial download: %w", err)
	}
	return n, nil
}

type progressWriter struct {
	w     io.Writer
	done  *int64
	total int64
	fn    ProgressFunc
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	*p.done += int64(n)
	if p.fn != nil {
		p.fn(*p.done, p.total)
	}
	return n, err
}
