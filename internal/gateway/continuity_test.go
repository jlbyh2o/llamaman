package gateway

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/tokens"
)

// waitFor polls cond until it holds or the test gives up.
func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(msg)
}

// fakeFDStore is the socketpair-free half of a real round trip: it takes the
// descriptor the gateway offers and DUPS it, exactly as systemd does when it
// pulls one out of an SCM_RIGHTS datagram. The dup keeps the listening socket —
// and its accept queue — alive after the first daemon closes its own copy,
// which is the property D58 depends on and the only one worth testing.
type fakeFDStore struct {
	mu       sync.Mutex
	held     map[string]int
	refuse   error
	refuseAt int
	calls    int
}

func newFakeFDStore() *fakeFDStore { return &fakeFDStore{held: map[string]int{}} }

func (f *fakeFDStore) StoreFD(name string, fd int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.refuse != nil && f.calls > f.refuseAt {
		return f.refuse
	}
	dup, err := syscall.Dup(fd)
	if err != nil {
		return err
	}
	f.held[name] = dup
	return nil
}

// inherited renders what the next boot would read out of
// LISTEN_FDS/LISTEN_FDNAMES.
func (f *fakeFDStore) inherited() []InheritedFD {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]InheritedFD, 0, len(f.held))
	for name, fd := range f.held {
		out = append(out, InheritedFD{FD: fd, Name: name})
	}
	return out
}

func (f *fakeFDStore) close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for name, fd := range f.held {
		syscall.Close(fd)
		delete(f.held, name)
	}
}

// TestFDStoreRoundTripKeepsThePortOpen is D58 and §9.4, end to end:
//
//	bind → hand off → the first daemon goes away → a client connects into the
//	kernel accept queue → the second daemon adopts the SAME socket → the queued
//	request is served.
//
// The "no connection-refused window" claim is not asserted by timing here; it is
// asserted structurally, which is stronger: the connection is made while NO
// process is accepting, and it still gets an answer.
func TestFDStoreRoundTripKeepsThePortOpen(t *testing.T) {
	fds := newFakeFDStore()
	defer fds.close()

	h := newHarnessWithFDStore(t, model.AuthNone,
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			io.WriteString(w, "served\n")
		}), fds)

	port := h.publicPort()
	if h.gw.Continuity() != model.ContinuityFDStore {
		t.Fatalf("continuity = %q before the hand-off, want fdstore", h.gw.Continuity())
	}

	// §9.4 steps 3-4, then 6.
	if got := h.gw.Drain(context.Background(), 2*time.Second); got.Dropped != 0 {
		t.Errorf("the drain dropped %d listeners with nothing in flight", got.Dropped)
	}
	stored, err := h.gw.HandOff()
	if err != nil {
		t.Fatalf("HandOff: %v", err)
	}
	if stored != 1 {
		t.Fatalf("stored %d descriptors, want 1", stored)
	}

	// The first daemon ends. Its own descriptor closes; the store's dup is what
	// keeps the socket — and the port — alive.
	if err := h.gw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// A client arrives during the gap. Nothing is accepting.
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("the port refused a connection during the restart gap: %v", err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "GET /v1/models HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatalf("writing into the queued connection: %v", err)
	}

	// The next boot: §9.4's startup half.
	next, err := New(Config{
		Store: h.store, Settings: h.settings, Tokens: h.tokens,
		Events: h.events, FDStore: fds, Logger: quiet(),
	})
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}
	defer next.Close()

	if unclaimed := next.Adopt(fds.inherited()); len(unclaimed) != 0 {
		t.Errorf("Adopt left %d descriptors unclaimed, want 0", len(unclaimed))
	}
	if err := next.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := next.Ports()[h.instanceID]; got != port {
		t.Fatalf("the adopted listener is on port %d, want the original %d", got, port)
	}
	if next.Continuity() != model.ContinuityFDStore {
		t.Errorf("continuity = %q after adopting, want fdstore", next.Continuity())
	}

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	status, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("the connection that waited through the restart got no answer: %v", err)
	}
	if !strings.Contains(status, "200") {
		t.Errorf("the queued request answered %q, want a 200", strings.TrimSpace(status))
	}
}

// TestAdoptedSocketIsClosedWhenThePortChanged is the third branch of §9.4's
// startup: "a stored fd whose `public_port` changed while the daemon was down is
// closed and rebound".
func TestAdoptedSocketIsClosedWhenThePortChanged(t *testing.T) {
	fds := newFakeFDStore()
	defer fds.close()

	h := newHarnessWithFDStore(t, model.AuthNone,
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}), fds)

	oldPort := h.publicPort()
	if _, err := h.gw.HandOff(); err != nil {
		t.Fatalf("HandOff: %v", err)
	}
	h.gw.Close()

	// The admin changed the port while the daemon was down.
	newPort, install := presetPort(t)
	h.store.setViews(instanceView(h.instanceID, newPort, h.upstreamPort,
		model.AuthNone, model.InstanceReady))

	next, err := New(Config{
		Store: h.store, Settings: h.settings, Tokens: h.tokens, Logger: quiet(), FDStore: fds,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	install(next)
	next.Adopt(fds.inherited())
	if err := next.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if got := next.Ports()[h.instanceID]; got != newPort {
		t.Fatalf("bound port = %d, want the new %d", got, newPort)
	}
	// The old socket was closed rather than left listening on a port no
	// instance claims. The store still holds a dup, so this asserts the
	// gateway's own copy is gone rather than that the port is free.
	fds.close()
	waitFor(t, func() bool {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", oldPort), 200*time.Millisecond)
		if err != nil {
			return true
		}
		c.Close()
		return false
	}, "the old port is still listening after the rebind")
}

// TestAdoptLeavesForeignNamesAlone: the management listener is `ui` and belongs
// to the composition root. A gateway that adopted and then closed it would take
// the admin UI down on every restart.
func TestAdoptLeavesForeignNamesAlone(t *testing.T) {
	t.Parallel()

	h := newHarness(t, model.AuthNone, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	unclaimed := h.gw.Adopt([]InheritedFD{{FD: 3, Name: "ui"}, {FD: 4, Name: "something-else"}})
	if len(unclaimed) != 2 {
		t.Fatalf("Adopt claimed %d of 2 foreign descriptors", 2-len(unclaimed))
	}
	for _, fd := range unclaimed {
		if _, ours := InstanceIDFromFDName(fd.Name); ours {
			t.Errorf("%q was returned as unclaimed but parses as one of ours", fd.Name)
		}
	}
}

// TestNoFDStoreReportsNoneAndStillWorks is the fallback §9.4 names: systemd
// older than v229, `systemd_control='exec'`, or a user manager with the store
// disabled. The restart still happens; it simply has a short connection-refused
// window, and NOTHING SILENTLY DEGRADES — the flag says so.
func TestNoFDStoreReportsNoneAndStillWorks(t *testing.T) {
	t.Parallel()

	h := newHarness(t, model.AuthNone, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "ok")
	}))

	if got := h.gw.Continuity(); got != model.ContinuityNone {
		t.Errorf("continuity = %q with no fd store, want none", got)
	}
	stored, err := h.gw.HandOff()
	if err != nil {
		t.Errorf("HandOff without a store returned %v; a missing store is not a failure", err)
	}
	if stored != 0 {
		t.Errorf("stored = %d without a store, want 0", stored)
	}

	// The listener still serves — this mode is degraded, not broken.
	resp, err := rawClient().Get(h.url("/v1/models"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// TestRefusedFDStoreDegradesToNone: `FDSTORE=1` rejected is the other half of
// "the daemon detects it at boot" — and the answer must move to `none` so both
// the self-update dialog and the restart confirmation say "clients will see
// ~2 s of connection refused" instead of "no interruption".
func TestRefusedFDStoreDegradesToNone(t *testing.T) {
	t.Parallel()

	fds := newFakeFDStore()
	defer fds.close()
	fds.refuse = fmt.Errorf("FileDescriptorStoreMax=0")
	h := newHarnessWithFDStore(t, model.AuthNone,
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}), fds)

	if _, err := h.gw.HandOff(); err == nil {
		t.Fatal("HandOff reported success against a store that refuses")
	}
	if got := h.gw.Continuity(); got != model.ContinuityNone {
		t.Errorf("continuity = %q after a refusal, want none", got)
	}
}

// TestDrainKeepsTheSocketOpenWhileFinishingInFlight is §9.4 steps 3 and 4
// together, and the distinction they turn on: the listener stops ACCEPTING but
// the socket stays OPEN, so a connection that arrives during the drain is queued
// by the kernel rather than refused, while the request already running is
// allowed to finish.
func TestDrainKeepsTheSocketOpenWhileFinishingInFlight(t *testing.T) {
	inFlight := make(chan struct{})
	release := make(chan struct{})

	h := newHarness(t, model.AuthNone, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		close(inFlight)
		<-release
		io.WriteString(w, "data: last\n\n")
	}))
	port := h.publicPort()

	type result struct {
		body string
		err  error
	}
	streamed := make(chan result, 1)
	go func() {
		resp, err := rawClient().Get(h.url("/v1/chat/completions"))
		if err != nil {
			streamed <- result{err: err}
			return
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		streamed <- result{body: string(body), err: err}
	}()

	<-inFlight

	drained := make(chan DrainResult, 1)
	go func() { drained <- h.gw.Drain(context.Background(), 10*time.Second) }()

	// A client that arrives during the drain still CONNECTS — the socket is
	// open — and is simply not served yet.
	waitForDrainStart(t, h.gw)
	late, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	if err != nil {
		t.Fatalf("the socket was closed by the drain; §9.4 step 3 keeps it open: %v", err)
	}
	defer late.Close()
	if _, err := io.WriteString(late, "GET /v1/models HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatalf("writing into the queued connection: %v", err)
	}
	late.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, err := late.Read(make([]byte, 1)); err == nil {
		t.Error("a connection made during the drain was served; the listener is still accepting")
	}

	// The in-flight stream finishes inside the window, and the drain reports it
	// dropped nothing — which is what makes "zero dropped requests" a measured
	// claim rather than a hope.
	close(release)
	got := <-drained
	if got.Dropped != 0 {
		t.Errorf("dropped = %d, want 0 for a request that completed inside the window", got.Dropped)
	}
	if got.Listeners != 1 {
		t.Errorf("listeners = %d, want 1", got.Listeners)
	}

	res := <-streamed
	if res.err != nil {
		t.Fatalf("the in-flight stream failed: %v", res.err)
	}
	if !strings.Contains(res.body, "data: first") || !strings.Contains(res.body, "data: last") {
		t.Errorf("the in-flight stream was cut short: %q", res.body)
	}
}

// waitForDrainStart waits until every listener has been paused.
func waitForDrainStart(t *testing.T, g *Gateway) {
	t.Helper()
	waitFor(t, func() bool {
		g.mu.Lock()
		defer g.mu.Unlock()
		for _, l := range g.listeners {
			if !l.ln.paused.Load() {
				return false
			}
		}
		return len(g.listeners) > 0
	}, "the drain never paused the listeners")
}

// TestDrainClosesWhatOutlivesTheWindow: "streams that outlive it are closed; the
// count is logged and recorded in the `events` row". A generation longer than
// the drain window IS interrupted, and the UI's restart confirmation says so.
func TestDrainClosesWhatOutlivesTheWindow(t *testing.T) {
	started := make(chan struct{})
	stuck := make(chan struct{})
	defer close(stuck)

	h := newHarness(t, model.AuthNone, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(started)
		<-stuck
	}))

	go func() {
		resp, err := rawClient().Get(h.url("/v1/chat/completions"))
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	<-started

	got := h.gw.Drain(context.Background(), 150*time.Millisecond)
	if got.Dropped != 1 {
		t.Errorf("dropped = %d, want 1 — a stream that outlives the window is closed", got.Dropped)
	}
}

// TestPortChangeMovesTheListener is §9.1: "changing `public_port` closes the old
// listener and opens the new one".
func TestPortChangeMovesTheListener(t *testing.T) {
	t.Parallel()

	h := newHarness(t, model.AuthNone, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "ok")
	}))
	oldPort := h.publicPort()

	newPort, install := presetPort(t)
	h.store.setViews(instanceView(h.instanceID, newPort, h.upstreamPort,
		model.AuthNone, model.InstanceReady))
	install(h.gw)
	if err := h.gw.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if got := h.gw.Ports()[h.instanceID]; got != newPort {
		t.Fatalf("port = %d, want %d", got, newPort)
	}
	resp, err := rawClient().Get(fmt.Sprintf("http://127.0.0.1:%d/v1/models", newPort))
	if err != nil {
		t.Fatalf("the new port does not serve: %v", err)
	}
	resp.Body.Close()

	waitFor(t, func() bool {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", oldPort), 200*time.Millisecond)
		if err != nil {
			return true
		}
		c.Close()
		return false
	}, "the old port is still listening")
}

// TestDeletedInstanceClosesItsListener: a listener is open "whenever the
// instance row exists and is not deleted" (§9.1).
func TestDeletedInstanceClosesItsListener(t *testing.T) {
	t.Parallel()

	h := newHarness(t, model.AuthNone, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	port := h.publicPort()

	h.store.setViews()
	if err := h.gw.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(h.gw.Ports()) != 0 {
		t.Errorf("the gateway still holds %d listeners after the instance went away", len(h.gw.Ports()))
	}
	waitFor(t, func() bool {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if err != nil {
			return true
		}
		c.Close()
		return false
	}, "the port is still listening for a deleted instance")
}

// TestBindFailureIsABannerNotAStartFailure is F6: "a bind failure is a
// per-instance banner and a notification, never a daemon start failure".
func TestBindFailureIsABannerNotAStartFailure(t *testing.T) {
	t.Parallel()

	// Hold the port with an unrelated process's socket, which is exactly the
	// genuinely unpredictable case §9.1 leaves to F6.
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	taken := blocker.Addr().(*net.TCPAddr).Port

	st := newFakeStore()
	st.setViews(instanceView("01JINSTANCE0000000000000001", taken, 9999,
		model.AuthNone, model.InstanceReady))
	events := &fakeEvents{}
	tokStore := newFakeTokenStore()
	tokSvc, err := tokensServiceFor(tokStore)
	if err != nil {
		t.Fatal(err)
	}

	g, err := New(Config{Store: st, Settings: newFakeSettings(), Tokens: tokSvc,
		Events: events, Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	if err := g.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile refused to finish because one port was taken: %v", err)
	}
	if len(g.BindErrors()) != 1 {
		t.Errorf("BindErrors has %d entries, want 1", len(g.BindErrors()))
	}
	waitFor(t, func() bool {
		for _, a := range events.actions() {
			if a == "gateway_bind_failed" {
				return true
			}
		}
		return false
	}, "no gateway_bind_failed event was raised")

	// The banner is raised ONCE, not once per refresh interval.
	before := len(events.actions())
	for range 3 {
		if err := g.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, func() bool { return true }, "")
	if got := len(events.actions()); got != before {
		t.Errorf("repeated reconciles raised %d more events; the banner must not repeat",
			got-before)
	}
}

// TestUsageTapDisabledByZero: `gateway.usage_parse_kb = 0` disables the tap
// entirely, which is the setting for anyone who wants byte-for-byte pass-through
// with provably zero inspection (§9.3).
func TestUsageTapDisabledByZero(t *testing.T) {
	t.Parallel()

	h := newHarness(t, model.AuthToken, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"usage":{"prompt_tokens":5,"completion_tokens":6}}`)
	}))
	h.settings.setInt("gateway.usage_parse_kb", 0)
	if err := h.gw.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}

	secret := h.mint(tokens.MintParams{Name: "no tap"})
	req, _ := http.NewRequest(http.MethodPost, h.url("/v1/chat/completions"), nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := rawClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// The response is unchanged — the tap never touched bytes anyway — and the
	// counts are simply not recorded.
	if !strings.Contains(string(body), `"prompt_tokens":5`) {
		t.Errorf("the body was altered: %q", body)
	}
	h.flush(1)
	for _, row := range h.store.tokenRows() {
		if row.PromptTokens != nil || row.CompletionTokens != nil {
			t.Errorf("token counts were recorded with the tap disabled: %v/%v",
				row.PromptTokens, row.CompletionTokens)
		}
	}
}

// tokensServiceFor builds the real token service over a fake row store.
func tokensServiceFor(st *fakeTokenStore) (*tokens.Service, error) {
	return tokens.New(tokens.Config{Store: st})
}
