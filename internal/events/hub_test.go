package events

import (
	"testing"
	"time"
)

// recvWithin drains one Frame from ch, failing the test if none arrives
// within d.
func recvWithin(t *testing.T, ch <-chan Frame, d time.Duration) Frame {
	t.Helper()
	select {
	case f, ok := <-ch:
		if !ok {
			t.Fatalf("channel closed before a frame arrived")
		}
		return f
	case <-time.After(d):
		t.Fatalf("timed out waiting for a frame")
		return Frame{}
	}
}

func TestHub_FanOutToNSubscribers(t *testing.T) {
	h := NewHub(8)
	t.Cleanup(h.Close)

	const n = 5
	subs := make([]*Subscription, n)
	for i := range subs {
		subs[i] = h.Subscribe(nil) // nil = every topic
		t.Cleanup(subs[i].Close)
	}

	want := []Frame{
		{Topic: TopicInstances, ID: "01A", Data: []byte(`{"n":1}`)},
		{Topic: TopicDownloads, ID: "01B", Data: []byte(`{"n":2}`)},
	}
	for _, f := range want {
		h.Publish(f)
	}

	for i, sub := range subs {
		for j, wf := range want {
			got := recvWithin(t, sub.Frames(), time.Second)
			if got.Topic != wf.Topic || got.ID != wf.ID || string(got.Data) != string(wf.Data) {
				t.Errorf("subscriber %d frame %d = %+v, want %+v", i, j, got, wf)
			}
		}
	}
}

func TestHub_TopicFilter(t *testing.T) {
	h := NewHub(8)
	t.Cleanup(h.Close)

	instancesOnly := h.Subscribe([]Topic{TopicInstances})
	t.Cleanup(instancesOnly.Close)

	h.Publish(Frame{Topic: TopicDownloads, ID: "01A"})
	h.Publish(Frame{Topic: TopicInstances, ID: "01B"})

	got := recvWithin(t, instancesOnly.Frames(), time.Second)
	if got.Topic != TopicInstances || got.ID != "01B" {
		t.Fatalf("got %+v, want the instances frame only", got)
	}

	select {
	case f := <-instancesOnly.Frames():
		t.Fatalf("received unwanted frame %+v", f)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestHub_SlowSubscriberDropsWithoutBlockingOthers(t *testing.T) {
	h := NewHub(2) // small buffer so overflow is easy to force
	t.Cleanup(h.Close)

	slow := h.Subscribe(nil) // never drained below
	t.Cleanup(slow.Close)
	fast := h.Subscribe(nil)
	t.Cleanup(fast.Close)

	const total = 10

	// fast is drained right after each Publish call, one frame at a time, so
	// its own buffer never needs more than one slot regardless of goroutine
	// scheduling — unlike draining it from a separate, freely-racing
	// goroutine, this cannot itself manufacture a drop for fast to confuse
	// with the property under test: that slow's full buffer never stalls
	// delivery to a sibling whose buffer has room. recvWithin's timeout is
	// what proves delivery is not stalled — Publish returning is not.
	for i := 0; i < total; i++ {
		id := string(rune('a' + i))
		h.Publish(Frame{Topic: TopicEvents, ID: id})
		got := recvWithin(t, fast.Frames(), time.Second)
		if got.ID != id {
			t.Fatalf("fast subscriber frame %d = %+v, want id %q", i, got, id)
		}
	}

	// The slow subscriber's buffer holds at most 2; Publish must have dropped
	// the rest rather than blocking on it.
	if d := slow.Dropped(); d == 0 {
		t.Fatalf("Dropped() = 0, want > 0 after publishing %d frames into a buffer of 2", total)
	}
	drained := 0
	for {
		select {
		case _, ok := <-slow.Frames():
			if !ok {
				t.Fatalf("channel closed unexpectedly")
			}
			drained++
		default:
			if drained > 2 {
				t.Fatalf("drained %d frames, buffer size was 2", drained)
			}
			return
		}
	}
}

func TestSubscription_CloseEndsFrames(t *testing.T) {
	h := NewHub(4)
	t.Cleanup(h.Close)

	sub := h.Subscribe(nil)
	sub.Close()
	sub.Close() // idempotent

	select {
	case _, ok := <-sub.Frames():
		if ok {
			t.Fatalf("received a frame on a closed subscription")
		}
	case <-time.After(time.Second):
		t.Fatalf("Frames() did not close")
	}

	// Publishing after Close must not panic or block.
	h.Publish(Frame{Topic: TopicEvents, ID: "01Z"})
}

func TestHub_CloseClosesSubscribers(t *testing.T) {
	h := NewHub(4)
	sub := h.Subscribe(nil)

	h.Close()
	h.Close() // idempotent

	select {
	case _, ok := <-sub.Frames():
		if ok {
			t.Fatalf("received a frame after Hub.Close")
		}
	case <-time.After(time.Second):
		t.Fatalf("Frames() did not close after Hub.Close")
	}

	// Subscribe and Publish after Close must return promptly rather than hang.
	done := make(chan struct{})
	go func() {
		late := h.Subscribe(nil)
		<-late.Frames() // closed already, returns immediately
		h.Publish(Frame{Topic: TopicEvents, ID: "01Y"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("Subscribe/Publish hung after Hub.Close")
	}
}
