package prebuilt

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// assetServer serves one payload, optionally honoring Range, and records what
// it was asked for. It is the smallest honest stand-in for a release-asset CDN.
type assetServer struct {
	payload     []byte
	ignoreRange bool
	// cutAfter, when > 0, drops the connection after that many bytes — the
	// truncated-transfer case a resume has to recover from.
	cutAfter int
	requests []string
	auths    []string
}

func (a *assetServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.requests = append(a.requests, r.Header.Get("Range"))
		a.auths = append(a.auths, r.Header.Get("Authorization"))

		body := a.payload
		status := http.StatusOK
		if rng := r.Header.Get("Range"); rng != "" && !a.ignoreRange {
			start, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(rng, "bytes="), "-"))
			if err == nil && start < len(body) {
				body = body[start:]
				w.Header().Set("Content-Range", "bytes "+strconv.Itoa(start)+"-"+
					strconv.Itoa(len(a.payload)-1)+"/"+strconv.Itoa(len(a.payload)))
				status = http.StatusPartialContent
			}
		}
		if a.cutAfter > 0 && a.cutAfter < len(body) {
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(status)
			_, _ = w.Write(body[:a.cutAfter])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			// Closing without writing the rest is what a truncated transfer
			// looks like to the client.
			panic(http.ErrAbortHandler)
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestFetchVerifiesThePublishedDigest(t *testing.T) {
	payload := []byte(strings.Repeat("llama.cpp tarball bytes\n", 500))
	a := &assetServer{payload: payload}
	srv := a.start(t)
	dst := filepath.Join(t.TempDir(), "llama-b10621-bin-ubuntu-x64.tar.gz")

	got, err := (&HTTPFetcher{}).Fetch(t.Context(), srv.URL+"/asset", dst, sha256Hex(payload), nil)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got != sha256Hex(payload) {
		t.Errorf("digest = %q", got)
	}
	b, err := os.ReadFile(dst)
	if err != nil || len(b) != len(payload) {
		t.Fatalf("downloaded file: %d bytes, %v", len(b), err)
	}
	if _, err := os.Stat(dst + PartSuffix); err == nil {
		t.Error("the .part file was left behind")
	}
	// Section 6.2: no Authorization header ever reaches an asset host.
	for _, auth := range a.auths {
		if auth != "" {
			t.Errorf("the download carried an Authorization header: %q", auth)
		}
	}
}

func TestFetchRejectsAChecksumMismatch(t *testing.T) {
	payload := []byte("not what github published")
	a := &assetServer{payload: payload}
	srv := a.start(t)
	dst := filepath.Join(t.TempDir(), "asset.tar.gz")

	_, err := (&HTTPFetcher{}).Fetch(t.Context(), srv.URL+"/asset", dst, sha256Hex([]byte("something else")), nil)
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("error = %v, want ErrChecksumMismatch", err)
	}
	// The wrong bytes must not be left where a resume would extend them.
	if _, err := os.Stat(dst + PartSuffix); err == nil {
		t.Error("the mismatched partial file was kept; the next attempt would resume from it")
	}
	if _, err := os.Stat(dst); err == nil {
		t.Error("the mismatched file was published to the destination")
	}
}

func TestFetchWithoutAPublishedDigest(t *testing.T) {
	// Plenty of older releases carry no `digest`. That is not an error — the
	// hash is still computed and recorded, so the manifest can state what
	// arrived.
	payload := []byte("older release, no digest published")
	srv := (&assetServer{payload: payload}).start(t)
	dst := filepath.Join(t.TempDir(), "asset.tar.gz")

	got, err := (&HTTPFetcher{}).Fetch(t.Context(), srv.URL+"/asset", dst, "", nil)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got != sha256Hex(payload) {
		t.Errorf("digest = %q, want the computed hash", got)
	}
}

func TestFetchResumesAndStillHashesTheWholeFile(t *testing.T) {
	// The property that matters: a resumed download's digest must cover the
	// bytes that were already on disk as well as the ones just fetched. A
	// hasher started at the resume point would produce a hash of the tail and
	// silently accept a corrupt prefix.
	payload := []byte(strings.Repeat("0123456789abcdef", 1000))
	dir := t.TempDir()
	dst := filepath.Join(dir, "asset.tar.gz")
	const already = 4096
	if err := os.WriteFile(dst+PartSuffix, payload[:already], 0o640); err != nil {
		t.Fatal(err)
	}

	a := &assetServer{payload: payload}
	srv := a.start(t)
	got, err := (&HTTPFetcher{}).Fetch(t.Context(), srv.URL+"/asset", dst, sha256Hex(payload), nil)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got != sha256Hex(payload) {
		t.Errorf("digest = %q, want the hash of the WHOLE file", got)
	}
	b, err := os.ReadFile(dst)
	if err != nil || string(b) != string(payload) {
		t.Fatalf("resumed file is %d bytes, want %d (%v)", len(b), len(payload), err)
	}
	if len(a.requests) != 1 || a.requests[0] != "bytes=4096-" {
		t.Errorf("range requests = %v, want one resuming at 4096", a.requests)
	}
}

func TestFetchStartsOverWhenTheServerIgnoresTheRange(t *testing.T) {
	// A server that answers 200 to a ranged request is sending the WHOLE file.
	// Appending it to the existing prefix would produce a corrupt archive whose
	// hash then fails — this is the case that must reset instead.
	payload := []byte(strings.Repeat("abcdefgh", 2000))
	dir := t.TempDir()
	dst := filepath.Join(dir, "asset.tar.gz")
	if err := os.WriteFile(dst+PartSuffix, payload[:1000], 0o640); err != nil {
		t.Fatal(err)
	}

	srv := (&assetServer{payload: payload, ignoreRange: true}).start(t)
	got, err := (&HTTPFetcher{}).Fetch(t.Context(), srv.URL+"/asset", dst, sha256Hex(payload), nil)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got != sha256Hex(payload) {
		t.Errorf("digest = %q", got)
	}
	b, _ := os.ReadFile(dst)
	if len(b) != len(payload) {
		t.Errorf("file is %d bytes, want %d — the ignored range was appended", len(b), len(payload))
	}
}

func TestFetchKeepsThePartialFileAfterATruncatedTransfer(t *testing.T) {
	// Keeping it is what makes the retry cheap: the next attempt resumes rather
	// than re-downloading 40 MB.
	payload := []byte(strings.Repeat("x", 20000))
	dir := t.TempDir()
	dst := filepath.Join(dir, "asset.tar.gz")

	srv := (&assetServer{payload: payload, cutAfter: 5000}).start(t)
	if _, err := (&HTTPFetcher{}).Fetch(t.Context(), srv.URL+"/asset", dst, sha256Hex(payload), nil); err == nil {
		t.Fatal("a truncated transfer was reported as success")
	}
	fi, err := os.Stat(dst + PartSuffix)
	if err != nil {
		t.Fatalf("the partial file was discarded: %v", err)
	}
	if fi.Size() == 0 || fi.Size() >= int64(len(payload)) {
		t.Errorf("partial file is %d bytes", fi.Size())
	}
}

func TestFetchReportsProgress(t *testing.T) {
	payload := []byte(strings.Repeat("y", 50000))
	srv := (&assetServer{payload: payload}).start(t)
	dst := filepath.Join(t.TempDir(), "asset.tar.gz")

	var last, total int64
	calls := 0
	_, err := (&HTTPFetcher{}).Fetch(t.Context(), srv.URL+"/asset", dst, "", func(done, tot int64) {
		calls++
		last, total = done, tot
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if calls == 0 {
		t.Fatal("progress was never reported")
	}
	if last != int64(len(payload)) {
		t.Errorf("final progress = %d, want %d", last, len(payload))
	}
	if total != int64(len(payload)) {
		t.Errorf("total = %d, want %d", total, len(payload))
	}
}

func TestFetchUnexpectedStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	dst := filepath.Join(t.TempDir(), "asset.tar.gz")
	_, err := (&HTTPFetcher{}).Fetch(t.Context(), srv.URL+"/asset", dst, "", nil)
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("error = %v, want one naming the status", err)
	}
}

func TestFetchNoResumeDiscardsThePartial(t *testing.T) {
	payload := []byte("complete payload")
	dir := t.TempDir()
	dst := filepath.Join(dir, "asset.tar.gz")
	if err := os.WriteFile(dst+PartSuffix, []byte("stale prefix"), 0o640); err != nil {
		t.Fatal(err)
	}

	a := &assetServer{payload: payload}
	srv := a.start(t)
	got, err := (&HTTPFetcher{NoResume: true}).Fetch(t.Context(), srv.URL+"/asset", dst, sha256Hex(payload), nil)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got != sha256Hex(payload) {
		t.Errorf("digest = %q", got)
	}
	if len(a.requests) != 1 || a.requests[0] != "" {
		t.Errorf("range requests = %v, want one unconditional request", a.requests)
	}
}
