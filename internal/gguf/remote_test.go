package gguf_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/jlbyh2o/llamaman/internal/gguf"
)

// fakeRange is an in-memory RangeReader that records what was asked of it. It
// stands in for internal/hf's HTTP Range client, which does not exist yet and
// which this package deliberately does not know about.
type fakeRange struct {
	mu      sync.Mutex
	data    []byte
	ranges  [][2]int64 // offset, length
	maxRead int        // when > 0, serve at most this many bytes per call
	fail    error
	after   int // fail from this call onward
	calls   int
}

func (f *fakeRange) ReadRangeAt(ctx context.Context, p []byte, off int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	f.calls++
	if f.fail != nil && f.calls > f.after {
		return 0, f.fail
	}
	f.ranges = append(f.ranges, [2]int64{off, int64(len(p))})
	if off >= int64(len(f.data)) {
		return 0, io.EOF
	}
	n := copy(p, f.data[off:])
	if f.maxRead > 0 && n > f.maxRead {
		n = f.maxRead
	}
	if n < len(p) {
		return n, io.ErrUnexpectedEOF
	}
	return n, nil
}

// TestRemotePeek is DESIGN section 8.5's point: the same parser, over an
// interface that fetches ranges, reading a header without the file. The
// assertion that matters is the request count — a peek that cost one round trip
// per 8-byte field would be useless, and the read-ahead window is what stops it.
func TestRemotePeek(t *testing.T) {
	raw := fixtureBytes(t, "llama.gguf")
	// The object is much larger than the header: this is a 20 GB quant of which
	// only the first kilobytes will ever be fetched.
	const objectSize = 20 << 30
	fr := &fakeRange{data: raw}

	rr := gguf.NewRemoteReaderAt(context.Background(), fr, objectSize)
	f, err := gguf.Parse(rr, objectSize)
	if err != nil {
		t.Fatalf("Parse over a range reader: %v", err)
	}

	local := loadFixture(t, "llama.gguf")
	if diff := cmp.Diff(local.KV.All(), f.KV.All()); diff != "" {
		t.Errorf("remote metadata differs from local (-local +remote):\n%s", diff)
	}
	if diff := cmp.Diff(local.Tensors, f.Tensors); diff != "" {
		t.Errorf("remote tensor index differs from local (-local +remote):\n%s", diff)
	}
	// FileSize is what the caller vouched for, so Complete is answered against
	// the whole object rather than the bytes fetched.
	if f.FileSize != objectSize || !f.Complete() {
		t.Errorf("FileSize = %d, Complete = %v; want %d and true", f.FileSize, f.Complete(), int64(objectSize))
	}

	requests, bytes := rr.Stats()
	if requests != 1 {
		t.Errorf("a %d-byte header took %d range requests; one 1 MiB window should cover it", len(raw), requests)
	}
	if bytes < int64(len(raw)) {
		t.Errorf("fetched %d bytes for a %d-byte header", bytes, len(raw))
	}
	if got := fr.ranges[0][1]; got != gguf.DefaultReadAhead {
		t.Errorf("first request asked for %d bytes, want the %d default window", got, gguf.DefaultReadAhead)
	}
}

// TestRemoteWindowing covers the window itself, against the read pattern it
// exists for: eight bytes at a time, which is what decoding a header looks like
// once the parser's own buffering is taken away. A 512-byte window must turn 64
// of those reads into one request, and must return the same bytes as the object.
func TestRemoteWindowing(t *testing.T) {
	raw := fixtureBytes(t, "llama.gguf")
	fr := &fakeRange{data: raw}
	rr := gguf.NewRemoteReaderAt(context.Background(), fr, int64(len(raw)), gguf.WithReadAhead(512))

	got := make([]byte, 0, len(raw))
	buf := make([]byte, 8)
	for off := 0; off < len(raw); off += len(buf) {
		n, err := rr.ReadAt(buf, int64(off))
		if err != nil && !(errors.Is(err, io.EOF) && off+n == len(raw)) {
			t.Fatalf("ReadAt(%d) = %d, %v", off, n, err)
		}
		got = append(got, buf[:n]...)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("read back %d bytes, want the %d-byte object", len(got), len(raw))
	}

	requests, _ := rr.Stats()
	if lo, hi := len(raw)/512, len(raw)/512+2; requests < lo || requests > hi {
		t.Errorf("%d requests for a %d-byte object through a 512-byte window; want between %d and %d",
			requests, len(raw), lo, hi)
	}
	for i, r := range fr.ranges {
		if r[0] < 0 || r[0] >= int64(len(raw)) {
			t.Errorf("request %d started at %d, outside the object", i, r[0])
		}
		if r[0]+r[1] > int64(len(raw)) {
			t.Errorf("request %d asked for [%d,%d) past the %d-byte object", i, r[0], r[0]+r[1], len(raw))
		}
	}
}

// TestRemoteReaderAtContract covers the io.ReaderAt edges the parser does not
// exercise but a future caller might.
func TestRemoteReaderAtContract(t *testing.T) {
	data := []byte("0123456789")
	newReader := func() *gguf.RemoteReaderAt {
		return gguf.NewRemoteReaderAt(context.Background(), &fakeRange{data: data}, int64(len(data)), gguf.WithReadAhead(4))
	}

	t.Run("reads across windows", func(t *testing.T) {
		r := newReader()
		p := make([]byte, 10)
		n, err := r.ReadAt(p, 0)
		if err != nil || n != 10 {
			t.Fatalf("ReadAt = %d, %v; want 10, nil", n, err)
		}
		if string(p) != string(data) {
			t.Errorf("read %q, want %q", p, data)
		}
	})

	t.Run("reads from an offset", func(t *testing.T) {
		r := newReader()
		p := make([]byte, 3)
		if _, err := r.ReadAt(p, 7); err != nil {
			t.Fatalf("ReadAt: %v", err)
		}
		if string(p) != "789" {
			t.Errorf("read %q, want %q", p, "789")
		}
	})

	t.Run("past the end is EOF", func(t *testing.T) {
		r := newReader()
		if _, err := r.ReadAt(make([]byte, 1), 10); !errors.Is(err, io.EOF) {
			t.Errorf("ReadAt past the end = %v, want io.EOF", err)
		}
	})

	t.Run("empty read", func(t *testing.T) {
		r := newReader()
		if n, err := r.ReadAt(nil, 0); n != 0 || err != nil {
			t.Errorf("ReadAt(nil) = %d, %v; want 0, nil", n, err)
		}
	})

	t.Run("negative offset", func(t *testing.T) {
		r := newReader()
		if _, err := r.ReadAt(make([]byte, 1), -1); err == nil {
			t.Error("a negative offset was accepted")
		}
	})
}

// TestRemoteFailurePropagates covers the two ways a peek stops: the transport
// fails, or the caller cancels. Neither may be silently turned into a short but
// plausible header.
func TestRemoteFailurePropagates(t *testing.T) {
	raw := fixtureBytes(t, "llama.gguf")

	t.Run("transport error", func(t *testing.T) {
		want := errors.New("503 from the hub")
		fr := &fakeRange{data: raw, fail: want}
		_, err := gguf.ParseRemote(context.Background(), fr, int64(len(raw)))
		if !errors.Is(err, want) {
			t.Fatalf("error = %v, want %v", err, want)
		}
	})

	t.Run("canceled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		fr := &fakeRange{data: raw}
		_, err := gguf.ParseRemote(ctx, fr, int64(len(raw)))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	})

	t.Run("a truncated body is not a truncated model", func(t *testing.T) {
		// The transport hands back fewer bytes than asked for, forever. The
		// parse must fail rather than loop.
		fr := &fakeRange{data: raw[:64], maxRead: 8}
		_, err := gguf.ParseRemote(context.Background(), fr, int64(len(raw)))
		if err == nil {
			t.Fatal("a short-reading transport produced a header")
		}
	})
}

// TestRangeReaderFunc covers the adapter, which is how a test or a small caller
// supplies a range reader without a type.
func TestRangeReaderFunc(t *testing.T) {
	raw := fixtureBytes(t, "llama.gguf")
	var calls int
	fn := gguf.RangeReaderFunc(func(ctx context.Context, p []byte, off int64) (int, error) {
		calls++
		if off >= int64(len(raw)) {
			return 0, io.EOF
		}
		return copy(p, raw[off:]), nil
	})
	f, err := gguf.ParseRemote(context.Background(), fn, int64(len(raw)))
	if err != nil {
		t.Fatalf("ParseRemote: %v", err)
	}
	if f.KV.Len() == 0 || calls == 0 {
		t.Errorf("KV.Len = %d after %d calls", f.KV.Len(), calls)
	}
}
