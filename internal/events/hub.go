package events

import "sync/atomic"

// Hub is the in-process fan-out this package owns (DESIGN §1): every published
// Frame is copied onto the channel of every Subscription whose requested topics
// include it. It carries frames; it does not decide what they mean — encoding a
// domain event into one is Recorder's job, and encoding richer per-entity
// patches (`instance.status` and the like) is each owning subsystem's, using
// the same Hub and the same Publish.
//
// A Hub has no notion of the database: it is pure fan-out, safe to construct in
// a test with no Store at all.
type Hub struct {
	bufferSize int

	// register/unregister/publish is a single-goroutine loop: every state
	// change funnels through one channel each, which is what lets Publish and
	// Subscribe never share a lock with a subscriber's own channel send (a
	// slow subscriber can only ever block itself, never another subscriber or
	// the publisher) while still making Subscribe/Close/Publish safe to call
	// from any number of goroutines concurrently.
	register   chan *Subscription
	unregister chan *Subscription
	publish    chan Frame
	closeReq   chan struct{}
	closed     chan struct{}
}

// Frame is one SSE message (§3.14): `event: <topic>` / `id: <ulid>` / `data:
// <json>`. ID is a ULID — ascending, so a subscriber can tell "already seen"
// from a plain string comparison, which is exactly what internal/sse's
// Last-Event-ID replay needs to avoid delivering a row twice.
type Frame struct {
	Topic Topic
	ID    string
	Data  []byte
}

// DefaultBufferSize is the per-subscriber channel size NewHub uses when a
// caller does not have a reason to pick another one. It is generous enough
// that an admin UI with several open tabs never drops a frame under ordinary
// load, while still bounding memory when one does stop reading.
const DefaultBufferSize = 256

// NewHub starts a Hub whose subscriber channels each hold bufferSize frames
// before Publish starts dropping for that subscriber. bufferSize <= 0 uses
// DefaultBufferSize. The returned Hub runs its dispatch loop in a goroutine
// until Close is called.
func NewHub(bufferSize int) *Hub {
	if bufferSize <= 0 {
		bufferSize = DefaultBufferSize
	}
	h := &Hub{
		bufferSize: bufferSize,
		register:   make(chan *Subscription),
		unregister: make(chan *Subscription),
		publish:    make(chan Frame),
		closeReq:   make(chan struct{}),
		closed:     make(chan struct{}),
	}
	go h.run()
	return h
}

func (h *Hub) run() {
	subs := make(map[*Subscription]struct{})
	defer func() {
		for s := range subs {
			close(s.ch)
		}
		close(h.closed)
	}()
	for {
		select {
		case s := <-h.register:
			subs[s] = struct{}{}
		case s := <-h.unregister:
			if _, ok := subs[s]; ok {
				delete(subs, s)
				close(s.ch)
			}
		case f := <-h.publish:
			for s := range subs {
				if !s.wants(f.Topic) {
					continue
				}
				select {
				case s.ch <- f:
				default:
					// The buffer is full: this subscriber is slower than the
					// stream, and blocking here would stall delivery to every
					// other subscriber (and, transitively, whatever service
					// method is calling Publish). Drop for this one subscriber
					// only and count it (§sse "backpressure drop policy").
					// Dropped is how the subscriber learns of the gap, and
					// acting on it is the subscriber's job: internal/sse ends
					// the stream on the first drop, so the client reconnects
					// and replays the "events" topic from its Last-Event-ID
					// (§3.14). A subscriber that never reads Dropped silently
					// keeps a hole.
					s.dropped.Add(1)
				}
			}
		case <-h.closeReq:
			return
		}
	}
}

// Publish fans f out to every current subscriber whose topics include f.Topic.
// It never blocks on a subscriber's channel and returns promptly even if every
// subscriber's buffer is full. Publish is a no-op after Close.
func (h *Hub) Publish(f Frame) {
	select {
	case h.publish <- f:
	case <-h.closed:
	}
}

// Subscribe registers a new Subscription for the given topics (nil or empty
// means every topic) and returns it. The caller must eventually call Close on
// it, or its goroutine (via Frames) leaks nothing but its own channel — but the
// Hub keeps sending to it, and its buffer keeps counting drops, until Close
// removes it.
func (h *Hub) Subscribe(topics []Topic) *Subscription {
	set := make(map[Topic]bool, len(topics))
	for _, t := range topics {
		set[t] = true
	}
	s := &Subscription{
		hub:    h,
		topics: set,
		ch:     make(chan Frame, h.bufferSize),
	}
	select {
	case h.register <- s:
	case <-h.closed:
		// The hub is already shut down: hand back a Subscription whose channel
		// is closed, so a caller ranging over Frames sees it end immediately
		// rather than blocking forever on a register that will never happen.
		close(s.ch)
	}
	return s
}

// Close stops the dispatch loop and closes every current subscriber's channel.
// It is for daemon shutdown; Publish and Subscribe become no-ops afterward
// (Publish returns immediately, Subscribe returns an already-closed
// Subscription).
func (h *Hub) Close() {
	select {
	case h.closeReq <- struct{}{}:
		<-h.closed
	case <-h.closed:
	}
}

// Subscription is one listener's view of a Hub: a topic filter and the buffered
// channel Frames matching it arrive on.
type Subscription struct {
	hub     *Hub
	topics  map[Topic]bool // empty = every topic
	ch      chan Frame
	dropped atomic.Int64
}

// Frames is the channel to range over. It is closed when Close is called (by
// the caller or by Hub.Close), after which a receive returns the zero Frame
// and ok == false — the caller's read loop should end at that point.
func (s *Subscription) Frames() <-chan Frame { return s.ch }

// Dropped returns how many frames this subscription has missed because its
// buffer was full when Publish tried to deliver to it.
func (s *Subscription) Dropped() int64 { return s.dropped.Load() }

// Close unsubscribes and closes the Frames channel. It is safe to call more
// than once and safe to call after the Hub itself has been closed.
func (s *Subscription) Close() {
	select {
	case s.hub.unregister <- s:
	case <-s.hub.closed:
	}
}

// wants reports whether t matches this subscription's topic filter. An empty
// filter (Subscribe called with nil or no topics) matches everything.
func (s *Subscription) wants(t Topic) bool {
	if len(s.topics) == 0 {
		return true
	}
	return s.topics[t]
}
