package gateway

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
	"github.com/jlbyh2o/llamaman/internal/tokens"
)

// The gateway's collaborators are faked rather than driven against a real
// database, which is the rule DESIGN section 15 sets for every consumer of
// internal/store except the two that open the file themselves. What is NOT
// faked here is the thing under test: the listeners are real sockets, the proxy
// is the real httputil.ReverseProxy with the real D36 transport, and the token
// verifier is the real internal/tokens service over a fake row store — because
// the epoch cache is one of the behaviors these tests exist to prove.

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// fakeStore is the gateway's Store. Its Tx is nil throughout: every method here
// ignores it, and passing one would only pretend there was a transaction.
type fakeStore struct {
	mu     sync.Mutex
	views  []model.InstanceView
	starts map[string][]model.InstanceStart

	instanceUsage []store.InstanceUsageDelta
	tokenUsage    []store.TokenUsageDelta
	denials       []store.DenialDelta
	events        []model.Event

	writeErr error
}

func newFakeStore() *fakeStore {
	return &fakeStore{starts: map[string][]model.InstanceStart{}}
}

func (f *fakeStore) Read(ctx context.Context, fn func(context.Context, store.Tx) error) error {
	return fn(ctx, nil)
}

func (f *fakeStore) Write(ctx context.Context, fn func(context.Context, store.Tx) error) error {
	f.mu.Lock()
	err := f.writeErr
	f.mu.Unlock()
	if err != nil {
		return err
	}
	return fn(ctx, nil)
}

func (f *fakeStore) InstanceViews(_ context.Context, _ store.Tx, _ store.InstanceFilter) ([]model.InstanceView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]model.InstanceView, len(f.views))
	copy(out, f.views)
	return out, nil
}

func (f *fakeStore) InstanceStarts(_ context.Context, _ store.Tx, id string, _ int) ([]model.InstanceStart, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts[id], nil
}

func (f *fakeStore) AddInstanceUsage(_ context.Context, _ store.Tx, d store.InstanceUsageDelta) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.instanceUsage = append(f.instanceUsage, d)
	return nil
}

func (f *fakeStore) AddTokenUsage(_ context.Context, _ store.Tx, d store.TokenUsageDelta) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokenUsage = append(f.tokenUsage, d)
	return nil
}

func (f *fakeStore) AddGatewayDenial(_ context.Context, _ store.Tx, d store.DenialDelta) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.denials = append(f.denials, d)
	return nil
}

func (f *fakeStore) GatewayDenials(_ context.Context, _ store.Tx, instanceID string,
	_ store.UsageRange) ([]store.DenialRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.DenialRow
	for _, d := range f.denials {
		if instanceID != "" && d.InstanceID != instanceID {
			continue
		}
		out = append(out, store.DenialRow{
			InstanceID: d.InstanceID, Day: d.Day, Reason: d.Reason, Count: d.Count,
		})
	}
	return out, nil
}

func (f *fakeStore) setViews(views ...model.InstanceView) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.views = views
}

func (f *fakeStore) usageFor(instanceID string) store.InstanceUsageDelta {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out store.InstanceUsageDelta
	for _, d := range f.instanceUsage {
		if d.InstanceID != instanceID {
			continue
		}
		out.InstanceID, out.Day, out.AuthMode = d.InstanceID, d.Day, d.AuthMode
		out.Requests += d.Requests
		out.Errors += d.Errors
		out.BytesIn += d.BytesIn
		out.BytesOut += d.BytesOut
		out.DurationMS += d.DurationMS
	}
	return out
}

func (f *fakeStore) tokenRows() []store.TokenUsageDelta {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.TokenUsageDelta, len(f.tokenUsage))
	copy(out, f.tokenUsage)
	return out
}

func (f *fakeStore) denialRows() []store.DenialDelta {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.DenialDelta, len(f.denials))
	copy(out, f.denials)
	return out
}

// fakeEvents records what the gateway logged, so the F6 banner and the denial
// burst can be asserted without a database.
type fakeEvents struct {
	mu   sync.Mutex
	rows []model.Event
}

func (e *fakeEvents) Append(_ context.Context, _ store.Tx, ev model.Event) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rows = append(e.rows, ev)
	return nil
}

func (e *fakeEvents) Publish(model.Event) {}

func (e *fakeEvents) actions() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, 0, len(e.rows))
	for _, ev := range e.rows {
		out = append(out, ev.Action)
	}
	return out
}

// fakeSettings answers the `gateway.*` keys with the registry defaults unless a
// test overrides one.
type fakeSettings struct {
	mu      sync.Mutex
	ints    map[string]int64
	strings map[string]string
}

func newFakeSettings() *fakeSettings {
	return &fakeSettings{
		ints: map[string]int64{
			"gateway.idle_timeout_sec":    300,
			"gateway.max_body_mb":         256,
			"gateway.usage_parse_kb":      64,
			"gateway.request_timeout_sec": 0,
			"gateway.drain_sec":           20,
		},
		strings: map[string]string{"gateway.bind": "127.0.0.1"},
	}
}

func (s *fakeSettings) GetInt(_ context.Context, key string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.ints[key]
	if !ok {
		return 0, fmt.Errorf("no such setting %q", key)
	}
	return v, nil
}

func (s *fakeSettings) GetString(_ context.Context, key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.strings[key]
	if !ok {
		return "", fmt.Errorf("no such setting %q", key)
	}
	return v, nil
}

func (s *fakeSettings) setInt(key string, v int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ints[key] = v
}

// fakeTokenStore is the row store behind a REAL tokens.Service.
type fakeTokenStore struct {
	mu     sync.Mutex
	rows   map[string]store.APIToken
	byHash map[string]string
	scopes map[string][]string
	reads  int
}

func newFakeTokenStore() *fakeTokenStore {
	return &fakeTokenStore{
		rows:   map[string]store.APIToken{},
		byHash: map[string]string{},
		scopes: map[string][]string{},
	}
}

func (f *fakeTokenStore) Read(ctx context.Context, fn func(context.Context, store.Tx) error) error {
	return fn(ctx, nil)
}

func (f *fakeTokenStore) Write(ctx context.Context, fn func(context.Context, store.Tx) error) error {
	return fn(ctx, nil)
}

func (f *fakeTokenStore) InsertAPIToken(_ context.Context, _ store.Tx, t store.APIToken) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, dup := f.byHash[t.TokenHash]; dup {
		return fmt.Errorf("UNIQUE constraint failed: api_tokens.token_hash")
	}
	f.rows[t.ID] = t
	f.byHash[t.TokenHash] = t.ID
	return nil
}

func (f *fakeTokenStore) APIToken(_ context.Context, _ store.Tx, id string) (store.APIToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.rows[id]
	if !ok {
		return store.APIToken{}, store.ErrNotFound
	}
	return t, nil
}

func (f *fakeTokenStore) APITokenByHash(_ context.Context, _ store.Tx, hash string) (store.APIToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	id, ok := f.byHash[hash]
	if !ok {
		return store.APIToken{}, store.ErrNotFound
	}
	return f.rows[id], nil
}

func (f *fakeTokenStore) APITokens(_ context.Context, _ store.Tx) ([]store.APIToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.APIToken, 0, len(f.rows))
	for _, t := range f.rows {
		out = append(out, t)
	}
	return out, nil
}

func (f *fakeTokenStore) UpdateAPIToken(_ context.Context, _ store.Tx, t store.APIToken) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cur, ok := f.rows[t.ID]
	if !ok || cur.State == model.TokenRevoked {
		return false, nil
	}
	f.rows[t.ID] = t
	return true, nil
}

func (f *fakeTokenStore) RevokeAPIToken(_ context.Context, _ store.Tx, id string, at int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.rows[id]
	if !ok || t.State == model.TokenRevoked {
		return false, nil
	}
	t.State = model.TokenRevoked
	t.RevokedAt = &at
	t.UpdatedAt = at
	f.rows[id] = t
	return true, nil
}

func (f *fakeTokenStore) TouchAPIToken(_ context.Context, _ store.Tx, id string,
	at int64, ip *string, delta int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.rows[id]
	if !ok {
		return false, nil
	}
	t.LastUsedAt = &at
	t.LastUsedIP = ip
	t.RequestCount += delta
	f.rows[id] = t
	return true, nil
}

func (f *fakeTokenStore) TokenInstances(_ context.Context, _ store.Tx, tokenID string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.scopes[tokenID], nil
}

func (f *fakeTokenStore) AllTokenInstances(_ context.Context, _ store.Tx) (map[string][]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string][]string{}
	for k, v := range f.scopes {
		out[k] = append([]string(nil), v...)
	}
	return out, nil
}

func (f *fakeTokenStore) SetTokenInstances(_ context.Context, _ store.Tx, tokenID string, ids []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(ids) == 0 {
		delete(f.scopes, tokenID)
		return nil
	}
	f.scopes[tokenID] = append([]string(nil), ids...)
	return nil
}

func (f *fakeTokenStore) TokenUsage(_ context.Context, _ store.Tx, _ string,
	_ store.UsageRange) ([]store.TokenUsageRow, error) {
	return nil, nil
}

func (f *fakeTokenStore) hashReads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

// harness is one gateway with one instance in front of one stub upstream.
type harness struct {
	t        *testing.T
	gw       *Gateway
	store    *fakeStore
	settings *fakeSettings
	events   *fakeEvents
	tokens   *tokens.Service
	tokStore *fakeTokenStore

	instanceID string
	upstream   *httptest.Server
	// upstreamPort is what the gateway proxies to.
	upstreamPort int
}

// newHarness stands up a stub llama-server on 127.0.0.1 and a gateway in front
// of it, and returns once the public listener is accepting. The gateway has NO
// fd store, which is the `listener_continuity='none'` mode most tests do not
// care about; newHarnessWithFDStore is the D58 variant.
func newHarness(t *testing.T, mode model.AuthMode, upstream http.Handler) *harness {
	t.Helper()
	return newHarnessWithFDStore(t, mode, upstream, nil)
}

// newHarnessWithFDStore is newHarness plus systemd's file-descriptor store.
func newHarnessWithFDStore(t *testing.T, mode model.AuthMode, upstream http.Handler,
	fds FDStore) *harness {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind the stub upstream: %v", err)
	}
	srv := &httptest.Server{Listener: ln, Config: &http.Server{Handler: upstream}}
	srv.Start()
	t.Cleanup(srv.Close)

	h := &harness{
		t:            t,
		store:        newFakeStore(),
		settings:     newFakeSettings(),
		events:       &fakeEvents{},
		tokStore:     newFakeTokenStore(),
		instanceID:   "01JINSTANCE0000000000000001",
		upstream:     srv,
		upstreamPort: ln.Addr().(*net.TCPAddr).Port,
	}

	tokSvc, err := tokens.New(tokens.Config{Store: h.tokStore})
	if err != nil {
		t.Fatalf("tokens.New: %v", err)
	}
	h.tokens = tokSvc

	port, install := presetPort(t)
	h.store.setViews(instanceView(h.instanceID, port, h.upstreamPort, mode, model.InstanceReady))

	gw, err := New(Config{
		Store:    h.store,
		Settings: h.settings,
		Tokens:   tokSvc,
		Events:   h.events,
		FDStore:  fds,
		Logger:   quiet(),
	})
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}
	install(gw)
	h.gw = gw
	t.Cleanup(func() { gw.Close() })

	if err := gw.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return h
}

// publicPort is the port the gateway bound for the instance.
func (h *harness) publicPort() int {
	port, ok := h.gw.Ports()[h.instanceID]
	if !ok {
		h.t.Fatalf("no listener for instance %s (bind errors: %v)", h.instanceID, h.gw.BindErrors())
	}
	return port
}

func (h *harness) url(path string) string {
	return fmt.Sprintf("http://127.0.0.1:%d%s", h.publicPort(), path)
}

// setState changes the observed state and reconciles, the way the supervisor
// writing `instance_status` and the refresh tick would.
func (h *harness) setState(state model.InstanceState) {
	h.t.Helper()
	views, _ := h.store.InstanceViews(context.Background(), nil, store.InstanceFilter{})
	views[0].Status.State = state
	h.store.setViews(views...)
	if err := h.gw.Reconcile(context.Background()); err != nil {
		h.t.Fatalf("Reconcile: %v", err)
	}
}

// booked reports how many proxied requests the accountant is holding for this
// harness's instance.
func (h *harness) booked() int64 {
	a := h.gw.acct
	a.mu.Lock()
	defer a.mu.Unlock()
	var n int64
	for k, c := range a.instances {
		if k.instanceID == h.instanceID {
			n += c.requests
		}
	}
	return n
}

// flush waits until `want` proxied requests have been booked and then writes the
// counters.
//
// The wait is the point. serveProxy books its record AFTER the reverse proxy has
// finished writing the response, so the client's Do() and body read can complete
// while the server goroutine has not reached the accountant yet — a test that
// flushed the instant its request came back was racing that goroutine and saw an
// empty row perhaps half the time under -race. Denials are deliberately not
// counted here: deny books before it writes its refusal, so a refusal the client
// has already read is already booked.
func (h *harness) flush(want int64) {
	h.t.Helper()
	waitFor(h.t, func() bool { return h.booked() >= want },
		fmt.Sprintf("the gateway booked fewer than %d requests; the accounting record is "+
			"written after the proxy returns, so the flush has to wait for it", want))
	if err := h.gw.Flush(context.Background()); err != nil {
		h.t.Fatalf("Flush: %v", err)
	}
}

// mint creates a token through the real service and returns its secret.
func (h *harness) mint(p tokens.MintParams) string {
	h.t.Helper()
	if p.Name == "" {
		p.Name = "test"
	}
	minted, err := h.tokens.Mint(context.Background(), p)
	if err != nil {
		h.t.Fatalf("Mint: %v", err)
	}
	return minted.Secret
}

func instanceView(id string, publicPort, internalPort int, mode model.AuthMode,
	state model.InstanceState) model.InstanceView {
	return model.InstanceView{
		Instance: model.Instance{
			ID:           id,
			Name:         "stub",
			PublicPort:   publicPort,
			InternalPort: internalPort,
			AuthMode:     mode,
		},
		Status: model.InstanceStatus{InstanceID: id, State: state},
	}
}

// presetPort binds a real listener now and returns its port, together with an
// install func that arranges for the gateway's OWN bind of that same port to
// hand back this exact socket instead of calling net.Listen fresh.
//
// The naive way to give a test a port — bind, read the number, close, hand the
// number to the gateway to bind later — leaves a window between the close and
// the gateway's Reconcile in which another goroutine can grab the same port.
// Under a whole-module `go test -race ./...` that window is real: any other
// test's ephemeral bind can land on it, Reconcile then records a bind failure
// instead of a listener, and the test dies later in publicPort() with "address
// already in use" (observed roughly 1 run in 16). Keeping the socket open from
// the moment it is chosen until the gateway itself claims it makes that race
// structurally impossible rather than merely unlikely.
func presetPort(t *testing.T) (port int, install func(g *Gateway)) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind a harness listener: %v", err)
	}
	t.Cleanup(func() { ln.Close() }) // no-op once claimed; a safety net if a test never reconciles.
	port = ln.Addr().(*net.TCPAddr).Port

	var claimed bool
	install = func(g *Gateway) {
		g.listenFn = func(bind string, p int) (net.Listener, error) {
			if p == port && !claimed {
				claimed = true
				return ln, nil
			}
			return listen(bind, p)
		}
	}
	return port, install
}

// rawClient never decompresses and never reuses a connection, so a test can
// assert on the exact bytes the gateway wrote.
func rawClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{DisableCompression: true, DisableKeepAlives: true},
		Timeout:   10 * time.Second,
	}
}
