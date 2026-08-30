package sse

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jlbyh2o/llamaman/internal/events"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// --- test plumbing: a line-oriented SSE reader ------------------------------

// startLineReader streams r line by line onto a channel until r is closed or
// errors, at which point the channel is closed. It runs for the life of the
// test; the caller closing the response body (directly or via Cleanup) is
// what ends it.
func startLineReader(t *testing.T, r io.Reader) <-chan string {
	t.Helper()
	lines := make(chan string, 64)
	go func() {
		defer close(lines)
		br := bufio.NewReader(r)
		for {
			line, err := br.ReadString('\n')
			if line != "" {
				lines <- strings.TrimRight(line, "\n")
			}
			if err != nil {
				return
			}
		}
	}()
	return lines
}

// sseMsg is one parsed SSE message: either a comment/directive line
// (`:keepalive`, `retry: 3000`) or an `event`/`id`/`data` triple.
type sseMsg struct {
	event, id, data, comment string
}

// nextMsg accumulates lines from ch up to the next blank-line terminator,
// failing the test if none arrives within timeout.
func nextMsg(t *testing.T, ch <-chan string, timeout time.Duration) sseMsg {
	t.Helper()
	var m sseMsg
	deadline := time.After(timeout)
	for {
		select {
		case line, ok := <-ch:
			if !ok {
				t.Fatalf("stream closed before a complete message arrived")
			}
			if line == "" {
				return m
			}
			switch {
			case strings.HasPrefix(line, ":"):
				m.comment = line
			case strings.HasPrefix(line, "retry: "):
				m.comment = line
			case strings.HasPrefix(line, "event: "):
				m.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "id: "):
				m.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "data: "):
				if m.data != "" {
					m.data += "\n"
				}
				m.data += strings.TrimPrefix(line, "data: ")
			}
		case <-deadline:
			t.Fatalf("timed out after %s waiting for the next SSE message", timeout)
			return m
		}
	}
}

// streamRequest starts srv's client on path with the given header value set as
// Last-Event-ID (skipped if empty), returns the response and a line channel,
// and registers cleanup that cancels the request and closes the body.
func streamRequest(t *testing.T, srv *httptest.Server, path, lastEventID string) (*http.Response, <-chan string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp, startLineReader(t, resp.Body)
}

// --- parseTopics -------------------------------------------------------------

func TestParseTopics(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    []events.Topic
		wantErr bool
	}{
		{name: "empty means every topic", raw: "", want: nil},
		{name: "whitespace only means every topic", raw: "   ", want: nil},
		{name: "single topic", raw: "instances", want: []events.Topic{events.TopicInstances}},
		{
			name: "several topics, spaces tolerated",
			raw:  "instances, downloads ,jobs",
			want: []events.Topic{events.TopicInstances, events.TopicDownloads, events.TopicJobs},
		},
		{name: "unknown topic is rejected", raw: "instances,bogus", wantErr: true},
		{name: "empty entries are skipped", raw: "instances,,downloads", want: []events.Topic{events.TopicInstances, events.TopicDownloads}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseTopics(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseTopics(%q) = %v, nil; want an error", tt.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseTopics(%q) unexpected error: %v", tt.raw, err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("parseTopics(%q) = %v, want %v", tt.raw, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("parseTopics(%q) = %v, want %v", tt.raw, got, tt.want)
				}
			}
		})
	}
}

// --- handshake ---------------------------------------------------------------

func TestHandler_HandshakeHeadersAndRetryDirective(t *testing.T) {
	hub := events.NewHub(8)
	t.Cleanup(hub.Close)
	srv := httptest.NewServer(NewHandler(Config{Hub: hub, Heartbeat: time.Hour}))
	t.Cleanup(srv.Close)

	resp, lines := streamRequest(t, srv, "/", "")

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", cc)
	}

	msg := nextMsg(t, lines, 2*time.Second)
	want := fmt.Sprintf("retry: %d", DefaultRetryMillis)
	if msg.comment != want {
		t.Fatalf("first message = %+v, want the %q directive", msg, want)
	}
}

func TestHandler_InvalidTopicReturns400(t *testing.T) {
	hub := events.NewHub(8)
	t.Cleanup(hub.Close)
	srv := httptest.NewServer(NewHandler(Config{Hub: hub}))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "?topics=bogus")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var env model.ErrorEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	if env.Error.Code != ErrCodeInvalidTopic {
		t.Fatalf("error code = %q, want %q", env.Error.Code, ErrCodeInvalidTopic)
	}
}

// --- live fan-out through the transport --------------------------------------

func TestHandler_LiveDeliveryRespectsTopicFilter(t *testing.T) {
	hub := events.NewHub(8)
	t.Cleanup(hub.Close)
	srv := httptest.NewServer(NewHandler(Config{Hub: hub, Heartbeat: time.Hour}))
	t.Cleanup(srv.Close)

	_, lines := streamRequest(t, srv, "/?topics=instances", "")
	nextMsg(t, lines, 2*time.Second) // the retry directive

	hub.Publish(events.Frame{Topic: events.TopicDownloads, ID: "01DOWNLOAD", Data: []byte(`{"n":1}`)})
	hub.Publish(events.Frame{Topic: events.TopicInstances, ID: "01INSTANCE", Data: []byte(`{"n":2}`)})

	msg := nextMsg(t, lines, 2*time.Second)
	if msg.event != string(events.TopicInstances) || msg.id != "01INSTANCE" {
		t.Fatalf("got %+v, want only the instances frame (downloads was not subscribed)", msg)
	}
}

func TestHandler_HeartbeatSentWhenIdle(t *testing.T) {
	hub := events.NewHub(8)
	t.Cleanup(hub.Close)
	srv := httptest.NewServer(NewHandler(Config{Hub: hub, Heartbeat: 30 * time.Millisecond}))
	t.Cleanup(srv.Close)

	_, lines := streamRequest(t, srv, "/", "")
	nextMsg(t, lines, time.Second) // the retry directive

	msg := nextMsg(t, lines, time.Second)
	if msg.comment != ":keepalive" {
		t.Fatalf("got %+v, want a :keepalive comment", msg)
	}
}

// --- Last-Event-ID cursor resume ---------------------------------------------

func TestHandler_LastEventIDReplaysMissedEvents(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "llamaman.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.Migrate(ctx, store.MigrateOptions{}); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	mkEvent := func(id string) model.Event {
		return model.Event{
			ID: id, At: time.Now().UnixMilli(), Level: model.LevelInfo,
			Category: model.CategorySystem, Action: "test",
			Actor: model.ActorSystem, Message: "message for " + id,
		}
	}
	appendEvent := func(ev model.Event) {
		t.Helper()
		if err := s.Write(ctx, func(ctx context.Context, tx store.Tx) error {
			return s.AppendEvent(ctx, tx, ev)
		}); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}

	ids := make([]string, 3)
	for i := range ids {
		ids[i] = store.NewID(time.Now())
		appendEvent(mkEvent(ids[i]))
	}

	hub := events.NewHub(8)
	t.Cleanup(hub.Close)
	// ReplayPageSize: 1 forces the handler to page through the two missed
	// rows one at a time, exercising the "more pages follow" loop rather than
	// only its single-page fast path.
	srv := httptest.NewServer(NewHandler(Config{
		Hub: hub, Replay: s, Heartbeat: time.Hour, ReplayPageSize: 1,
	}))
	t.Cleanup(srv.Close)

	_, lines := streamRequest(t, srv, "/", ids[0])
	nextMsg(t, lines, time.Second) // the retry directive

	replayed := []sseMsg{
		nextMsg(t, lines, 2*time.Second),
		nextMsg(t, lines, 2*time.Second),
	}
	if replayed[0].id != ids[1] || replayed[1].id != ids[2] {
		t.Fatalf("replayed ids = [%s %s], want [%s %s]",
			replayed[0].id, replayed[1].id, ids[1], ids[2])
	}
	for _, m := range replayed {
		if m.event != string(events.TopicEvents) {
			t.Errorf("replayed message event = %q, want %q", m.event, events.TopicEvents)
		}
	}

	// A frame published live, after the replay caught up, must still be
	// delivered — and undeduplicated, since its id sorts above the replay's
	// high-water mark.
	liveID := store.NewID(time.Now())
	hub.Publish(events.EncodeEventFrame(mkEvent(liveID)))
	live := nextMsg(t, lines, 2*time.Second)
	if live.id != liveID {
		t.Fatalf("live message id = %q, want %q", live.id, liveID)
	}
}

func TestHandler_NoReplayerStartsLiveWithoutErroring(t *testing.T) {
	hub := events.NewHub(8)
	t.Cleanup(hub.Close)
	srv := httptest.NewServer(NewHandler(Config{Hub: hub, Heartbeat: time.Hour})) // Replay left nil
	t.Cleanup(srv.Close)

	_, lines := streamRequest(t, srv, "/", "01SOMEOLDID")
	nextMsg(t, lines, time.Second) // the retry directive; no replay content follows

	hub.Publish(events.Frame{Topic: events.TopicInstances, ID: "01LIVE", Data: []byte(`{}`)})
	msg := nextMsg(t, lines, 2*time.Second)
	if msg.id != "01LIVE" {
		t.Fatalf("got %+v, want the live frame (no replayer configured)", msg)
	}
}

// spyReplayer counts EventsAfter calls without touching a database, so a test
// can assert replay was skipped entirely.
type spyReplayer struct{ calls int }

func (r *spyReplayer) Read(ctx context.Context, fn func(context.Context, store.Tx) error) error {
	return fn(ctx, nil)
}

func (r *spyReplayer) EventsAfter(ctx context.Context, tx store.Tx, afterID string, limit int) ([]model.Event, error) {
	r.calls++
	return nil, nil
}

func TestHandler_ReplaySkippedWhenTopicsExcludeEvents(t *testing.T) {
	hub := events.NewHub(8)
	t.Cleanup(hub.Close)
	spy := &spyReplayer{}
	srv := httptest.NewServer(NewHandler(Config{Hub: hub, Replay: spy, Heartbeat: time.Hour}))
	t.Cleanup(srv.Close)

	_, lines := streamRequest(t, srv, "/?topics=instances", "01SOMEOLDID")
	nextMsg(t, lines, time.Second) // the retry directive

	hub.Publish(events.Frame{Topic: events.TopicInstances, ID: "01LIVE", Data: []byte(`{}`)})
	nextMsg(t, lines, 2*time.Second) // proves the live loop is running

	if spy.calls != 0 {
		t.Fatalf("EventsAfter was called %d times; replay should be skipped when topics excludes \"events\"", spy.calls)
	}
}

// --- backpressure ------------------------------------------------------------

// blockingWriter is a ResponseWriter that announces every write on `writes` and
// then waits for a token on `gate`, so a test can hold the handler INSIDE a
// network write while the Hub fills — and then overflows — this subscriber's
// buffer. Holding it there is what makes the drop deterministic: a handler free
// to drain would keep emptying the one-slot buffer between publishes.
type blockingWriter struct {
	header http.Header

	writes chan int      // every write announces its ordinal here
	gate   chan struct{} // and then waits here, until a token or a close

	mu sync.Mutex
	n  int
}

func newBlockingWriter() *blockingWriter {
	return &blockingWriter{
		header: make(http.Header),
		writes: make(chan int, 16),
		gate:   make(chan struct{}),
	}
}

func (w *blockingWriter) Header() http.Header  { return w.header }
func (w *blockingWriter) WriteHeader(code int) {}
func (w *blockingWriter) Flush()               {}

func (w *blockingWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	w.n++
	n := w.n
	w.mu.Unlock()
	w.writes <- n
	<-w.gate
	return len(b), nil
}

// TestHandler_EndsTheStreamAfterABackpressureDrop is the half of §3.14's
// `retry:`/Last-Event-ID machinery that has to exist on this side of the wire.
// The Hub drops a frame for a subscriber whose buffer is full and only counts
// it, so a slow client during a burst — a cache_scan appending thousands of
// rows, a bench sweep streaming progress — loses frames it will never be told
// about, and §4's patch-into-cache projection stays wrong until a manual
// reload. Ending the connection is what makes the client reconnect and replay.
func TestHandler_EndsTheStreamAfterABackpressureDrop(t *testing.T) {
	hub := events.NewHub(1) // one slot, so the overflow needs no burst to arrange
	t.Cleanup(hub.Close)

	h := NewHandler(Config{
		Hub: hub, Heartbeat: time.Hour,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req := httptest.NewRequest(http.MethodGet, "/?topics=instances", nil).WithContext(ctx)
	w := newBlockingWriter()

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(w, req)
	}()

	publish := func(n int) {
		t.Helper()
		hub.Publish(events.Frame{
			Topic: events.TopicInstances,
			ID:    fmt.Sprintf("01FRAME%d", n),
			Data:  []byte(`{}`),
		})
	}

	// Write 1 is the retry directive, and it happens only after Subscribe has
	// registered this connection — so every frame below is offered to it.
	if got := <-w.writes; got != 1 {
		t.Fatalf("first write ordinal = %d, want 1 (the retry directive)", got)
	}
	w.gate <- struct{}{}

	// Park the handler inside the write of the first frame. From here the
	// one-slot buffer takes exactly one more frame and every frame after that
	// is dropped — and Publish returning means the Hub's dispatch loop has
	// finished delivering the frame before it, so the drop is recorded by the
	// time the last Publish returns.
	publish(0)
	if got := <-w.writes; got != 2 {
		t.Fatalf("second write ordinal = %d, want 2 (the first frame)", got)
	}
	publish(1) // fills the buffer
	publish(2) // dropped
	publish(3) // returns only once the drop above has been counted

	close(w.gate) // the client catches up, having missed a frame

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ServeHTTP is still streaming after a dropped frame; the client would never learn of the gap")
	}
}
