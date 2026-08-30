package source

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jlbyh2o/llamaman/internal/procx"
)

// DefaultRingLines is the in-memory tail DESIGN section 6.5 specifies: "an
// in-memory ring of the last 5000 lines with a broadcast channel".
const DefaultRingLines = 5000

// Entry is one line of build output as the log keeps it: the procx line plus
// the phase that produced it, which is what section 6.5 means by "prefixed in
// build.log".
type Entry struct {
	At     time.Time
	Phase  Phase
	Stream procx.Stream
	Text   string
}

// String renders the on-disk form: "[compile] the line".
func (e Entry) String() string {
	if e.Phase == "" {
		return e.Text
	}
	return "[" + string(e.Phase) + "] " + e.Text
}

// LogSink is one build's log: the durable file (F15), the in-memory ring, and
// the fan-out that `GET /api/v1/llamacpp/versions/{id}/log`'s live tail reads
// (section 3.5).
//
// Writes are serialized and subscriber sends are non-blocking: a browser that
// stops reading its SSE stream must not be able to slow a compile down, so a
// full subscriber channel DROPS rather than blocks. The file and the ring are
// the complete records; the channel is a convenience for whoever is watching.
type LogSink struct {
	mu     sync.Mutex
	file   *os.File
	buf    *bufio.Writer
	phase  Phase
	ring   []Entry
	first  int // index of the oldest entry when the ring has wrapped
	filled bool
	subs   map[int]chan Entry
	nextID int
	closed bool
}

// OpenLog opens (or creates) the build log at path and returns a sink over it.
// The file is opened for APPEND: D4's Retry re-runs a build against warm
// objects, and the second attempt's output belongs after the first attempt's in
// the same file. Rotating the log is D71's reuse-and-reset, which is the
// llama.cpp service's move, not this package's.
func OpenLog(path string, ringLines int) (*LogSink, error) {
	if ringLines <= 0 {
		ringLines = DefaultRingLines
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("source: create log directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return nil, fmt.Errorf("source: open build log: %w", err)
	}
	return &LogSink{
		file: f,
		buf:  bufio.NewWriterSize(f, 32*1024),
		ring: make([]Entry, 0, ringLines),
		subs: make(map[int]chan Entry),
	}, nil
}

// NewMemoryLog returns a sink with no file behind it, for a caller that wants
// the ring and the fan-out without a path (and for tests).
func NewMemoryLog(ringLines int) *LogSink {
	if ringLines <= 0 {
		ringLines = DefaultRingLines
	}
	return &LogSink{
		ring: make([]Entry, 0, ringLines),
		subs: make(map[int]chan Entry),
	}
}

// SetPhase stamps every following line with the named phase.
func (s *LogSink) SetPhase(p Phase) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.phase = p
}

// Line records one line of a child's output.
func (s *LogSink) Line(l procx.Line) {
	if s == nil {
		return
	}
	at := l.At
	if at.IsZero() {
		at = time.Now()
	}
	text := l.Text
	if l.Truncated {
		text += " …[line truncated]"
	}
	s.write(Entry{At: at, Stream: l.Stream, Text: text})
}

// Printf records one line of the pipeline's own commentary — the phase banners
// and the reason for an automatic retry — on the stdout stream, so a person
// reading the file sees why a command ran and not only its output.
func (s *LogSink) Printf(format string, args ...any) {
	if s == nil {
		return
	}
	s.write(Entry{At: time.Now(), Stream: procx.StreamStdout, Text: fmt.Sprintf(format, args...)})
}

func (s *LogSink) write(e Entry) {
	s.mu.Lock()
	e.Phase = s.phase
	if s.buf != nil && !s.closed {
		_, _ = s.buf.WriteString(e.String())
		_ = s.buf.WriteByte('\n')
	}
	s.pushLocked(e)
	subs := make([]chan Entry, 0, len(s.subs))
	for _, ch := range s.subs {
		subs = append(subs, ch)
	}
	s.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- e:
		default:
		}
	}
}

func (s *LogSink) pushLocked(e Entry) {
	if len(s.ring) < cap(s.ring) {
		s.ring = append(s.ring, e)
		return
	}
	s.ring[s.first] = e
	s.first = (s.first + 1) % len(s.ring)
	s.filled = true
}

// Tail returns the last n entries in order, or the whole ring when n <= 0 or
// exceeds it.
func (s *LogSink) Tail(n int) []Entry {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	size := len(s.ring)
	out := make([]Entry, 0, size)
	if !s.filled {
		out = append(out, s.ring...)
	} else {
		out = append(out, s.ring[s.first:]...)
		out = append(out, s.ring[:s.first]...)
	}
	if n > 0 && n < len(out) {
		out = out[len(out)-n:]
	}
	return out
}

// Subscribe returns a channel of entries written from now on, and the function
// that stops it. The channel is buffered and lossy by design (see the type
// comment); a subscriber that needs the whole log reads the file.
func (s *LogSink) Subscribe(buffer int) (<-chan Entry, func()) {
	if buffer <= 0 {
		buffer = 256
	}
	ch := make(chan Entry, buffer)

	s.mu.Lock()
	id := s.nextID
	s.nextID++
	s.subs[id] = ch
	s.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			s.mu.Lock()
			delete(s.subs, id)
			s.mu.Unlock()
			close(ch)
		})
	}
}

// Flush pushes buffered bytes to the file, which is what makes the log
// readable WHILE the build runs rather than only after it.
func (s *LogSink) Flush() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.buf == nil || s.closed {
		return nil
	}
	return s.buf.Flush()
}

// Close flushes and closes the file. Subscribers are left alone: their channels
// belong to whoever took them, and Unsubscribe is theirs to call.
func (s *LogSink) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.buf == nil {
		return nil
	}
	if err := s.buf.Flush(); err != nil {
		_ = s.file.Close()
		return err
	}
	return s.file.Close()
}

// CopyTo writes the durable log to a second path — section 6.5's destination
// (b), `versions/<id>/build.log`. It is called at publish, against the STAGING
// tree, so the published directory carries its own build log and nothing is
// ever written into a directory `versions/active` can resolve into (D78).
func (s *LogSink) CopyTo(path string) error {
	if s == nil || s.file == nil {
		return nil
	}
	if err := s.Flush(); err != nil {
		return err
	}
	src, err := os.Open(s.file.Name())
	if err != nil {
		return fmt.Errorf("source: read build log: %w", err)
	}
	defer src.Close()

	dst, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return fmt.Errorf("source: write %s: %w", path, err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		return fmt.Errorf("source: copy build log: %w", err)
	}
	return dst.Close()
}

// LogRegistry holds the sink of every build that is currently running, so the
// log endpoint can find the live one. A build whose id is absent is not
// running, and its log is read from the file.
type LogRegistry struct {
	mu    sync.Mutex
	sinks map[string]*LogSink
}

// NewLogRegistry returns an empty registry.
func NewLogRegistry() *LogRegistry {
	return &LogRegistry{sinks: make(map[string]*LogSink)}
}

// Sink returns the live sink for a version id, if there is one.
func (r *LogRegistry) Sink(id string) (*LogSink, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sinks[id]
	return s, ok
}

func (r *LogRegistry) put(id string, s *LogSink) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sinks[id] = s
}

func (r *LogRegistry) drop(id string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sinks, id)
}
