package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/jlbyh2o/llamaman/internal/instances"
)

// The health probe of §5.8.
//
// `GET http://127.0.0.1:<internal>/health` with a 2 s timeout is what drives
// `starting → loading → ready`, and it is the reason instance state in the UI is
// a fact rather than an inference from systemd: a unit is `active` the moment
// llama-server's main process exists, which is minutes before the model is
// loaded and the port answers. Only the probe knows the difference, and the
// three-state answer below is exactly that difference.

// HealthTimeout is the per-probe budget. Two seconds is long enough that a host
// under load still answers and short enough that a wedged instance does not
// stall the reconcile pass for every other instance behind it.
const HealthTimeout = 2 * time.Second

// Health is what one probe learned. Code is the HTTP status, or 0 when the
// connection itself failed — which is the ordinary answer while llama-server is
// still binding its socket and is NOT the same as a 503.
type Health struct {
	Code int
	Err  error
}

// OK reports a live, loaded server.
func (h Health) OK() bool { return h.Code == http.StatusOK }

// Loading reports llama-server's own "still loading the model" answer. It is
// distinguished from an unreachable port because the two mean different things
// to the state machine: `loading` is progress, an unreachable port during
// `starting` is not yet anything.
func (h Health) Loading() bool { return h.Code == http.StatusServiceUnavailable }

// Props is the subset of `GET /props` §5.8 records on the first ready: the
// context actually in force and the number of slots the server built.
//
// They are read from the SERVER rather than from the instance's own flags
// because llama.cpp is entitled to disagree with what was asked for — a context
// clamped to the model's trained maximum is the common case — and the number
// shown beside a running instance should be the one it is really serving with.
type Props struct {
	CtxSize    *int64
	SlotsTotal *int64
}

// Prober is the seam every test replaces. The production implementation is
// HTTPProber; a test supplies one that answers from a script, which is how the
// `starting → loading → ready` sequence is exercised without waiting on a real
// model load.
type Prober interface {
	Health(ctx context.Context, port int) Health
	Props(ctx context.Context, port int) (Props, error)
}

// HTTPProber probes over loopback, and over loopback only. The gateway is the
// front door (SPEC §1), an instance never listens on a routable address, and a
// prober that could be pointed elsewhere would be a way to make the daemon
// issue requests to an arbitrary host.
type HTTPProber struct {
	client *http.Client
}

// NewHTTPProber returns a prober with the §5.8 timeout.
//
// The transport disables keep-alives deliberately. A pooled connection to an
// instance that has just been restarted is a connection to a socket the new
// process does not own, and the first probe after a restart would fail for a
// reason that has nothing to do with the instance's health.
func NewHTTPProber() *HTTPProber {
	return &HTTPProber{
		client: &http.Client{
			Timeout: HealthTimeout,
			Transport: &http.Transport{
				DisableKeepAlives:   true,
				DialContext:         (&net.Dialer{Timeout: HealthTimeout}).DialContext,
				TLSHandshakeTimeout: HealthTimeout,
			},
		},
	}
}

// Health probes `/health`.
func (p *HTTPProber) Health(ctx context.Context, port int) Health {
	resp, err := p.get(ctx, port, "/health")
	if err != nil {
		return Health{Err: err}
	}
	defer resp.Body.Close()
	return Health{Code: resp.StatusCode}
}

// Props probes `/props` and keeps the two fields §5.8 names.
//
// Every other field is discarded rather than stored: `/props` is llama.cpp's
// own surface and changes with it, and a supervisor that decoded all of it
// would break on an upstream rename of something it never used.
func (p *HTTPProber) Props(ctx context.Context, port int) (Props, error) {
	resp, err := p.get(ctx, port, "/props")
	if err != nil {
		return Props{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Props{}, fmt.Errorf("supervisor: /props answered %d", resp.StatusCode)
	}

	var body struct {
		TotalSlots *int64 `json:"total_slots"`
		Default    struct {
			NCtx *int64 `json:"n_ctx"`
		} `json:"default_generation_settings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return Props{}, fmt.Errorf("supervisor: decode /props: %w", err)
	}
	return Props{CtxSize: body.Default.NCtx, SlotsTotal: body.TotalSlots}, nil
}

func (p *HTTPProber) get(ctx context.Context, port int, path string) (*http.Response, error) {
	url := fmt.Sprintf("http://%s:%d%s", instances.LoopbackHost, port, path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return p.client.Do(req)
}
