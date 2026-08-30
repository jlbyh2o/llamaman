package systemd

import (
	"context"
	"path"
	"sync"
)

// subHub fans one manager's sub-state stream out to every SubscribeSubState
// caller.
//
// It exists because go-systemd allows exactly ONE sub-state subscriber per
// connection (SetSubStateSubscriber replaces whatever was there), while this
// design has more than one interested reader — the supervisor watching
// `llamaman-instance@*.service`, and anything else that wants the daemon unit —
// and because the fan-out has to survive a reconnect: the underlying connection
// is replaced, the subscribers' channels are not.
type subHub struct {
	mu   sync.Mutex
	next int
	subs map[int]*subscriber
	// dropped counts events discarded because a subscriber's buffer was full.
	// A drop is survivable — every consumer of this stream also polls, and the
	// reconnect path re-reads properties wholesale — but it must be countable
	// rather than silent.
	dropped uint64
	closed  bool
}

type subscriber struct {
	pattern string
	ch      chan SubStateEvent
}

func newSubHub() *subHub {
	return &subHub{subs: make(map[int]*subscriber)}
}

// subscribe registers a pattern and returns its channel. The registration is
// removed and the channel closed when ctx is done.
func (h *subHub) subscribe(ctx context.Context, pattern string) <-chan SubStateEvent {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		ch := make(chan SubStateEvent)
		close(ch)
		return ch
	}
	id := h.next
	h.next++
	s := &subscriber{pattern: pattern, ch: make(chan SubStateEvent, 256)}
	h.subs[id] = s
	h.mu.Unlock()

	go func() {
		<-ctx.Done()
		h.mu.Lock()
		if cur, ok := h.subs[id]; ok && cur == s {
			delete(h.subs, id)
			close(s.ch)
		}
		h.mu.Unlock()
	}()

	return s.ch
}

// interested reports whether any subscriber's pattern matches this unit.
//
// It is asked BEFORE a properties round trip is spent on a unit, which is what
// keeps a system manager's constant churn — every unit on the host emits
// PropertiesChanged — from costing one D-Bus call each.
func (h *subHub) interested(unit string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.subs {
		if ok, err := path.Match(s.pattern, unit); err == nil && ok {
			return true
		}
	}
	return false
}

// publish delivers one event to every subscriber whose pattern matches.
func (h *subHub) publish(ev SubStateEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.subs {
		ok, err := path.Match(s.pattern, ev.Unit)
		if err != nil || !ok {
			continue
		}
		select {
		case s.ch <- ev:
		default:
			h.dropped++
		}
	}
}

// closeAll closes every subscriber channel. Called once, from Close.
func (h *subHub) closeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	for id, s := range h.subs {
		delete(h.subs, id)
		close(s.ch)
	}
}

// dropCount reports how many events were discarded for full buffers.
func (h *subHub) dropCount() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.dropped
}
