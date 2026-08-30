package gguf

import (
	"context"
	"errors"
	"io"
	"sync"
)

// DefaultReadAhead is the window NewRemoteReaderAt fetches per miss: DESIGN
// section 8.5's 1 MiB, chosen because a typical header fits inside it and even a
// large token array is a handful of these — a sub-second peek instead of a 20 GB
// download.
const DefaultReadAhead = 1 << 20

// RangeReader is the remote half of DESIGN section 8.5. One method, one
// meaning: give me these bytes of that object.
//
// internal/hf implements it with an HTTP Range request against the resolve URL,
// carrying the token and the redirect handling that belong there. Nothing in
// this package speaks HTTP, knows a URL or holds a credential — the interface
// is the whole of the remote side here, and the wiring lands with the
// downloader.
//
// Implementations follow io.ReaderAt's contract: fill p entirely or return an
// error, and return io.EOF only for a read that starts at or past the end of the
// object.
type RangeReader interface {
	ReadRangeAt(ctx context.Context, p []byte, off int64) (int, error)
}

// RangeReaderFunc adapts a function to RangeReader.
type RangeReaderFunc func(ctx context.Context, p []byte, off int64) (int, error)

// ReadRangeAt calls f.
func (f RangeReaderFunc) ReadRangeAt(ctx context.Context, p []byte, off int64) (int, error) {
	return f(ctx, p, off)
}

// RemoteReaderAt turns a RangeReader into the io.ReaderAt the parser wants, by
// caching one read-ahead window.
//
// It exists because the two sides want opposite things. The parser reads a GGUF
// header as thousands of small sequential fields — an 8-byte length, a 40-byte
// key, a 4-byte type — and a remote fetch charges a round trip per read. One
// window in front of the cursor collapses that to a handful of requests, and
// because the parse is strictly forward, a window is all it ever needs.
//
// It is safe for concurrent use, but concurrent readers in different parts of
// the file will thrash the single window; the parser is sequential and does not.
//
// The context is bound at construction because io.ReaderAt has no room for one.
// Cancelling it fails every subsequent read, which is how a peek is abandoned.
type RemoteReaderAt struct {
	ctx       context.Context
	rr        RangeReader
	size      int64
	readAhead int

	mu       sync.Mutex
	window   []byte
	winStart int64
	requests int
	bytes    int64
}

// A RemoteOption adjusts the remote reader.
type RemoteOption func(*RemoteReaderAt)

// WithReadAhead sets the window size. A value below 1 restores DefaultReadAhead.
func WithReadAhead(n int) RemoteOption {
	return func(r *RemoteReaderAt) {
		if n < 1 {
			n = DefaultReadAhead
		}
		r.readAhead = n
	}
}

// NewRemoteReaderAt wraps rr, whose object is size bytes long.
func NewRemoteReaderAt(ctx context.Context, rr RangeReader, size int64, opts ...RemoteOption) *RemoteReaderAt {
	r := &RemoteReaderAt{ctx: ctx, rr: rr, size: size, readAhead: DefaultReadAhead, winStart: -1}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Stats reports how many range requests were issued and how many bytes they
// carried — what a peek costs, for the log line that says so.
func (r *RemoteReaderAt) Stats() (requests int, bytes int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.requests, r.bytes
}

// ReadAt implements io.ReaderAt.
func (r *RemoteReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if off < 0 {
		return 0, errors.New("gguf: negative offset")
	}
	if off >= r.size {
		return 0, io.EOF
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	n := 0
	for n < len(p) {
		at := off + int64(n)
		if at >= r.size {
			return n, io.EOF
		}
		if !r.hit(at) {
			// A fill that made no progress is the end of the road; one that
			// made some is not, even if the transport also reported an error —
			// a Range response shorter than asked for is ordinary, and dropping
			// the bytes it did carry would turn a working peek into a failure.
			if err := r.fill(at, len(p)-n); err != nil {
				return n, err
			}
		}
		copied := copy(p[n:], r.window[at-r.winStart:])
		if copied == 0 {
			return n, io.ErrUnexpectedEOF
		}
		n += copied
	}
	return n, nil
}

// hit reports whether the window already holds the byte at off.
func (r *RemoteReaderAt) hit(off int64) bool {
	return r.winStart >= 0 && off >= r.winStart && off < r.winStart+int64(len(r.window))
}

// fill fetches a window starting at off, at least want bytes long.
func (r *RemoteReaderAt) fill(off int64, want int) error {
	length := r.readAhead
	if want > length {
		length = want
	}
	if remaining := r.size - off; int64(length) > remaining {
		length = int(remaining)
	}
	if length <= 0 {
		return io.EOF
	}
	buf := make([]byte, length)
	got, err := r.rr.ReadRangeAt(r.ctx, buf, off)
	if got > 0 {
		r.window = buf[:got]
		r.winStart = off
		r.requests++
		r.bytes += int64(got)
		return nil
	}
	r.window, r.winStart = nil, -1
	if err == nil {
		err = io.ErrUnexpectedEOF
	}
	return err
}

// ParseRemote peeks at a GGUF header over a RangeReader: DESIGN section 8.5's
// "measure the quant before downloading 20 GB". size is the object's full
// length, which the caller already has from the file listing.
func ParseRemote(ctx context.Context, rr RangeReader, size int64, opts ...Option) (*File, error) {
	return Parse(NewRemoteReaderAt(ctx, rr, size), size, opts...)
}
