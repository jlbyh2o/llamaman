package sse

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jlbyh2o/llamaman/internal/events"
	"github.com/jlbyh2o/llamaman/internal/model"
)

// Handler is the HTTP transport behind `GET /api/v1/events` (DESIGN §3.14). It
// owns the SSE handshake, the heartbeat comment, the wire framing of an
// events.Frame, and Last-Event-ID resume; it does not own routing, session
// auth, or the error envelope's place in the route registry — that is
// internal/api's job (DESIGN §1) — but it does write the same
// model.ErrorEnvelope shape for the one request it can reject outright: an
// unrecognized `?topics=` entry.
//
// A Handler has no notion of who is asking: the caller (internal/api, behind
// its session middleware) decides whether this request is authorized before
// ServeHTTP is ever called.
type Handler struct {
	cfg Config
}

// Config wires a Handler.
type Config struct {
	// Hub is the fan-out every live frame is read from. Required — NewHandler
	// panics without one, the same way jobs.New refuses a nil store.
	Hub *events.Hub

	// Replay resolves a `Last-Event-ID` reconnect by reading the durable
	// `events` table (§3.14). *store.Store satisfies this interface
	// structurally (DESIGN §1, invariant 1). Nil is accepted — a composition
	// root that has not wired persistence yet gets "every reconnect starts
	// from now" instead of a panic — but a real deployment always sets it,
	// since resuming without it silently drops the very gap a client
	// reconnected to close.
	Replay Replayer

	// Heartbeat is the `:keepalive` comment interval (§3.14). Zero uses
	// DefaultHeartbeat.
	Heartbeat time.Duration
	// RetryMillis is the `retry:` directive sent once at the start of the
	// stream (§3.14). Zero uses DefaultRetryMillis.
	RetryMillis int
	// ReplayPageSize bounds one EventsAfter call while a reconnect is
	// catching up; the handler pages through as many as it takes. Zero uses
	// DefaultReplayPageSize.
	ReplayPageSize int

	// Logger receives a debug line for an ordinary client disconnect and a
	// warn line for a replay read that failed. Nil uses slog.Default.
	Logger *slog.Logger
}

// Defaults for the Config fields DESIGN leaves to policy.
const (
	// DefaultHeartbeat is the `:keepalive` interval §3.14 names.
	DefaultHeartbeat = 20 * time.Second
	// DefaultRetryMillis is the `retry:` directive value §3.14 names.
	DefaultRetryMillis = 3000
	// DefaultReplayPageSize bounds one EventsAfter call during a reconnect's
	// catch-up; it matches store.DefaultEventLimit's role for the polled log
	// view, without this package needing to import that constant.
	DefaultReplayPageSize = 200
)

// ErrCodeInvalidTopic is the code an unrecognized `?topics=` entry answers
// with, in the model.Error shape every other endpoint uses (§3). It lives here
// rather than in internal/model because DESIGN's error-code catalog (§3)
// enumerates codes as they arrive with their endpoints, and this one arrives
// with GET /api/v1/events.
const ErrCodeInvalidTopic model.ErrorCode = "invalid_topic"

// NewHandler builds a Handler. It panics if cfg.Hub is nil — a Handler with no
// fan-out to read from is a construction bug, not a request-time condition.
func NewHandler(cfg Config) *Handler {
	if cfg.Hub == nil {
		panic("sse: Config.Hub is required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Handler{cfg: cfg}
}

func (h *Handler) heartbeat() time.Duration {
	if h.cfg.Heartbeat > 0 {
		return h.cfg.Heartbeat
	}
	return DefaultHeartbeat
}

func (h *Handler) retryMillis() int {
	if h.cfg.RetryMillis > 0 {
		return h.cfg.RetryMillis
	}
	return DefaultRetryMillis
}

func (h *Handler) replayPageSize() int {
	if h.cfg.ReplayPageSize > 0 {
		return h.cfg.ReplayPageSize
	}
	return DefaultReplayPageSize
}

// ServeHTTP performs the subscription handshake described by §3.14: parse
// `?topics=`, subscribe to the Hub, optionally replay from `Last-Event-ID`,
// then stream live frames with a periodic heartbeat until the client goes away
// or the Hub is closed.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	topics, err := parseTopics(r.URL.Query().Get("topics"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, ErrCodeInvalidTopic, err.Error())
		return
	}

	// Subscribe BEFORE any replay read. A frame published while the replay
	// query is running must land in this subscriber's buffer, not vanish in
	// the gap between "finished reading history" and "started reading live" —
	// that gap is exactly the kind of miss §3.14's Last-Event-ID exists to
	// close, and reordering these two steps would reopen it.
	sub := h.cfg.Hub.Subscribe(topics)
	defer sub.Close()

	rc := http.NewResponseController(w)

	header := w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
	header.Set("X-Accel-Buffering", "no") // nginx: do not buffer this response
	w.WriteHeader(http.StatusOK)

	if !writeAndFlush(w, rc, fmt.Sprintf("retry: %d\n\n", h.retryMillis())) {
		return
	}

	ctx := r.Context()
	lastReplayedID := ""
	if wantsReplay(topics) {
		if id := r.Header.Get("Last-Event-ID"); id != "" {
			var ok bool
			lastReplayedID, ok = h.replay(ctx, w, rc, id)
			if !ok {
				return
			}
		}
	}

	ticker := time.NewTicker(h.heartbeat())
	defer ticker.Stop()

	for {
		// A drop means this connection has a hole in it, and the client cannot
		// see one: the Hub drops a frame for a subscriber whose buffer is full
		// (events.Hub.run) and only counts it. Leaving the stream open would
		// leave §4's patch-into-cache projection permanently wrong for whatever
		// those frames carried, with nothing to correct it short of a manual
		// reload. So the connection ends here, which is what makes the
		// `retry:` directive and Last-Event-ID close the gap they exist for:
		// the client reconnects and, for the "events" topic, replays from the
		// last id it actually received. A subscription's drop count only ever
		// grows, so any non-zero value is this connection's own gap.
		if dropped := sub.Dropped(); dropped > 0 {
			h.cfg.Logger.Warn("ending an SSE stream that fell behind",
				"dropped", dropped, "topics", topics)
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !writeAndFlush(w, rc, ":keepalive\n\n") {
				return
			}
		case f, ok := <-sub.Frames():
			if !ok {
				return // the Hub was closed (daemon shutdown, §1)
			}
			// A replayed row and its live-published twin carry the same ULID
			// (EncodeEventFrame is deterministic, recorder.go's doc comment),
			// so a plain string comparison against the replay's high-water
			// mark is what keeps a client from seeing one row twice.
			if f.Topic == events.TopicEvents && f.ID <= lastReplayedID {
				continue
			}
			if !writeAndFlush(w, rc, encodeFrame(f)) {
				return
			}
		}
	}
}

// wantsReplay reports whether topics (as parsed from `?topics=`) includes the
// "events" topic that Last-Event-ID resume applies to — nil/empty means every
// topic, per Hub.Subscribe's own convention.
func wantsReplay(topics []events.Topic) bool {
	if len(topics) == 0 {
		return true
	}
	for _, t := range topics {
		if t == events.TopicEvents {
			return true
		}
	}
	return false
}

// parseTopics parses the comma-separated `?topics=` value into the Hub's
// filter shape. An empty value returns (nil, nil) — "every topic" — matching
// Hub.Subscribe. An unrecognized topic is the one request-time error this
// package rejects outright, rather than silently dropping it and leaving a
// client to wonder why a topic it asked for never arrives.
func parseTopics(raw string) ([]events.Topic, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	topics := make([]events.Topic, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		t := events.Topic(p)
		if !t.Valid() {
			return nil, fmt.Errorf("unknown topic %q", p)
		}
		topics = append(topics, t)
	}
	return topics, nil
}

// encodeFrame renders one events.Frame as the wire form §3.14 shows:
// `event: <topic>` / `id: <ulid>` / one or more `data:` lines / a blank line.
// Splitting Data on "\n" is defensive — every producer in this codebase emits
// single-line JSON — but the SSE spec requires a multi-line payload to repeat
// the `data:` field, so a future producer that does not stays correct.
func encodeFrame(f events.Frame) string {
	var b strings.Builder
	fmt.Fprintf(&b, "event: %s\nid: %s\n", f.Topic, f.ID)
	data := f.Data
	if len(data) == 0 {
		data = []byte("{}")
	}
	for _, line := range strings.Split(string(data), "\n") {
		b.WriteString("data: ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	return b.String()
}

// writeAndFlush writes s and flushes it to the underlying connection,
// reporting false (and logging, unless the client simply went away) if either
// step failed — the caller's signal to stop serving this connection.
func writeAndFlush(w io.Writer, rc *http.ResponseController, s string) bool {
	if _, err := io.WriteString(w, s); err != nil {
		return false
	}
	if err := rc.Flush(); err != nil && err != http.ErrNotSupported {
		return false
	}
	return true
}

// writeJSONError writes the model.ErrorEnvelope shape §3 defines for every
// non-2xx response. It is called only before the SSE handshake has written a
// status line, so an ordinary JSON error response is still possible here.
func writeJSONError(w http.ResponseWriter, status int, code model.ErrorCode, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(model.ErrorEnvelope{Error: model.Error{Code: code, Message: message}})
}
