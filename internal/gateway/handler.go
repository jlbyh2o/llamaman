package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
	"github.com/jlbyh2o/llamaman/internal/tokens"
)

// The request path (DESIGN section 9.2):
//
//	accept → route (/health and /llamaman/info handled locally; everything else proxied)
//	       → extract credential (Authorization: Bearer | X-API-Key | ?api_key=)
//	       → auth_mode=='none' ? allow : verify(token)
//	       → httputil.ReverseProxy → 127.0.0.1:<internal_port>
//	       → account (bytes, duration, reported usage) → batched flusher

// The two gateway-owned paths of section 3.15. Both are PUBLIC: `/health` stays
// unauthenticated to match llama-server behavior (SPEC §3.4), and
// `/llamaman/info` carries only what a client needs to self-configure.
const (
	healthPath = "/health"
	infoPath   = "/llamaman/info"
)

// instanceHandler serves one instance's public port.
type instanceHandler struct {
	g     *Gateway
	snap  atomic.Pointer[instanceSnapshot]
	proxy *httputil.ReverseProxy

	// retryAfter caches the previous launch's observed load time, so the
	// `Retry-After` on a loading instance is derived from what this model
	// actually took rather than from a constant. It is read at most once a
	// minute and only while an instance is loading, which is rare.
	retryMu   atomic.Pointer[retryHint]
	retryBusy atomic.Bool
}

type retryHint struct {
	seconds int
	at      time.Time
}

// retryHintTTL bounds how stale a Retry-After hint may be.
const retryHintTTL = time.Minute

// defaultRetryAfter is what a host with no completed launch to learn from
// answers. A model that has never finished loading has no observed time, and a
// number is more useful to a client than an absent header.
const defaultRetryAfter = 15

func (h *instanceHandler) buildProxy() {
	h.proxy = &httputil.ReverseProxy{
		// Rewrite sets ONLY the upstream URL. Path and query pass through
		// verbatim, which is what makes the OpenAI-compatible API pass through
		// unmodified (SPEC §3.4, §9.2).
		Rewrite: func(pr *httputil.ProxyRequest) {
			snap := h.snap.Load()
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = net.JoinHostPort("127.0.0.1", strconv.Itoa(snap.InternalPort))
			pr.Out.Host = pr.In.Host

			// X-Forwarded-For/Proto are APPENDED, not replaced: a gateway behind
			// a reverse proxy of the operator's own must not erase the chain.
			pr.SetXForwarded()

			// The client's credential is REPLACED, never forwarded (§9.2).
			// llama-server runs without `--api-key`, so there is nothing to
			// replace it with, and passing an `lm_…` secret to a subprocess that
			// logs its requests would put it in the journal.
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("X-Api-Key")
		},
		Transport: h.g.proxyTr,
		// Immediate flush, so SSE and chunked token streams are not buffered
		// (§9.2). This is what makes a token appear on screen as it is
		// generated rather than at the end of the response.
		FlushInterval:  -1,
		ModifyResponse: h.modifyResponse,
		ErrorHandler:   h.upstreamError,
		ErrorLog:       nil,
	}
}

func (h *instanceHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	snap := h.snap.Load()
	switch r.URL.Path {
	case healthPath:
		h.serveHealth(w, snap)
		return
	case infoPath:
		h.serveInfo(w, snap)
		return
	}
	h.serveProxy(w, r, snap)
}

// serveHealth answers section 3.15's gateway-owned `/health`, matching
// llama-server semantics. It is answered LOCALLY rather than proxied so that a
// stopped instance still has a health endpoint — which is the difference between
// "this model is not loaded" and connection-refused.
func (h *instanceHandler) serveHealth(w http.ResponseWriter, snap *instanceSnapshot) {
	switch {
	case snap.serving():
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	case snap.loading():
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "loading model"})
	default:
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "stopped"})
	}
}

// InfoResponse is `GET /llamaman/info` (§3.15): enough for a client to
// self-configure, nothing sensitive.
type InfoResponse struct {
	Instance string  `json:"instance"`
	Model    *string `json:"model"`
	// AuthRequired is false for `auth_mode='none'`, which is the toggle SPEC
	// §3.4 calls the per-instance "no auth required" switch.
	AuthRequired bool `json:"auth_required"`
}

func (h *instanceHandler) serveInfo(w http.ResponseWriter, snap *instanceSnapshot) {
	writeJSON(w, http.StatusOK, InfoResponse{
		Instance:     snap.Name,
		Model:        snap.ModelID,
		AuthRequired: snap.AuthMode == model.AuthToken,
	})
}

// serveProxy is everything else: authenticate, proxy, account.
func (h *instanceHandler) serveProxy(w http.ResponseWriter, r *http.Request, snap *instanceSnapshot) {
	g := h.g
	start := g.now()
	ip := clientIP(r)

	var verified tokens.Verified
	if snap.AuthMode == model.AuthToken {
		secret := credential(r)
		v, err := g.tokens.Verify(r.Context(), secret, snap.ID)
		if err != nil {
			h.deny(w, snap, err, ip, start)
			return
		}
		verified = v
		g.tokens.RecordUse(v.TokenID, start, ip)
	}

	// The instance is not up. This is answered here rather than by dialing a
	// port nothing is listening on, and it is COUNTED: the request was admitted
	// and authenticated, it produced an error response, and `instance_usage_daily`
	// is where this instance's error rate lives (§2.9). It is deliberately not a
	// `gateway_denials_daily` row — nothing was denied, the model is simply not
	// loaded.
	if !snap.serving() {
		retry := 0
		if snap.loading() {
			retry = h.retryAfterSeconds(r.Context(), snap.ID)
		}
		n := h.writeNotRunning(w, snap, retry)
		g.acct.add(record{
			instanceID: snap.ID,
			authMode:   snap.AuthMode,
			tokenID:    verified.TokenID,
			at:         start,
			duration:   g.now().Sub(start),
			bytesOut:   n,
			failed:     true,
		})
		return
	}

	tune := g.tune.Load()

	// The request body is size-capped and STREAMED, never buffered (§9.2). The
	// response has deliberately no cap: a 40-minute completion stream has no
	// size known in advance.
	in := &countingReader{}
	if r.Body != nil {
		in.rc = http.MaxBytesReader(w, r.Body, tune.maxBodyBytes)
		r.Body = in
	}
	out := &countingWriter{ResponseWriter: w, status: http.StatusOK}

	// The client's context is propagated, so a disconnect cancels upstream and
	// frees the llama-server slot immediately. `gateway.request_timeout_sec` is
	// 0 by default, which is "never cap a generation".
	ctx := r.Context()
	if tune.requestTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, tune.requestTimeout)
		defer cancel()
	}
	st := &reqState{tapBytes: tune.usageTapBytes}
	r = r.WithContext(withReqState(ctx, st))

	h.proxy.ServeHTTP(out, r)

	usage := Usage{}
	if st.tap != nil {
		usage = st.tap.usage()
	}
	g.acct.add(record{
		instanceID: snap.ID,
		authMode:   snap.AuthMode,
		tokenID:    verified.TokenID,
		at:         start,
		duration:   g.now().Sub(start),
		bytesIn:    in.n.Load(),
		bytesOut:   out.n,
		failed:     out.status >= 400,
		usage:      usage,
	})
}

// deny writes section 3.15's refusal and books it.
//
// The reason is what `gateway_denials_daily` counts (§2.9) and what turns "five
// denials from one IP within a minute" into the dashboard's "unauthorized
// attempts on port 8081". The client is told only that the key was refused: a
// message that distinguished `disabled` from `expired` for an unauthenticated
// caller would be an oracle.
func (h *instanceHandler) deny(w http.ResponseWriter, snap *instanceSnapshot,
	err error, ip string, at time.Time) {

	reason, ok := tokens.Denied(err)
	if !ok {
		// A verification that failed for a reason that is not a denial — the
		// database was unreachable — is a 503, not a 401. Telling a client with
		// a good token that their token is bad would send them to rotate it.
		h.g.log.Error("could not verify a gateway credential",
			"instance_id", snap.ID, "error", err)
		writeGatewayError(w, http.StatusServiceUnavailable, CodeUpstreamUnavailable,
			"the gateway could not verify credentials just now", 0)
		return
	}

	h.g.acct.deny(snap.ID, reason, at)
	if h.g.watch.note(snap.ID, ip, at) {
		go h.g.recordEvent(context.Background(), snap.ID, "gateway_denial_burst", model.LevelWarn,
			"unauthorized attempts on port "+strconv.Itoa(snap.PublicPort)+" from "+ip)
	}

	if reason == model.DeniedRateLimited {
		// A rate limit is not a credential failure, and an SDK that read a 401
		// here would tell the user to check their key. 429 with a Retry-After is
		// what OpenAI-compatible clients already handle.
		writeGatewayError(w, http.StatusTooManyRequests, CodeRateLimitExceeded,
			"this API key is over its configured requests-per-minute limit", 60)
		return
	}
	writeGatewayError(w, http.StatusUnauthorized, CodeInvalidAPIKey,
		"an API key is required; pass it as `Authorization: Bearer lm_…`", 0)
}

// writeNotRunning answers a request for an instance that is not serving, and
// returns how many bytes it wrote.
func (h *instanceHandler) writeNotRunning(w http.ResponseWriter, snap *instanceSnapshot, retry int) int64 {
	msg := "instance " + snap.Name + " is not running"
	if retry > 0 {
		msg = "instance " + snap.Name + " is loading its model"
	}
	return writeGatewayError(w, http.StatusServiceUnavailable, CodeInstanceNotRunning, msg, retry)
}

// retryAfterSeconds derives a Retry-After from the previous launch's observed
// load time (§9.2).
//
// It reads the start ledger at most once a minute, and only while an instance is
// loading — a state that lasts seconds and that a client polls through. A host
// with no completed launch to learn from gets the default rather than no header.
func (h *instanceHandler) retryAfterSeconds(ctx context.Context, instanceID string) int {
	now := h.g.now()
	if hint := h.retryMu.Load(); hint != nil && now.Sub(hint.at) < retryHintTTL {
		return hint.seconds
	}
	// One reader at a time; everyone else uses the previous answer, or the
	// default on the very first loading request.
	if !h.retryBusy.CompareAndSwap(false, true) {
		if hint := h.retryMu.Load(); hint != nil {
			return hint.seconds
		}
		return defaultRetryAfter
	}
	defer h.retryBusy.Store(false)

	var rows []model.InstanceStart
	if err := h.g.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		rows, err = h.g.store.InstanceStarts(ctx, tx, instanceID, 5)
		return err
	}); err != nil {
		return defaultRetryAfter
	}

	seconds := defaultRetryAfter
	for _, row := range rows {
		if row.ReadyAt == nil || *row.ReadyAt <= row.At {
			continue
		}
		observed := int((*row.ReadyAt - row.At + 999) / 1000)
		if observed > 0 {
			seconds = observed
		}
		break
	}
	h.retryMu.Store(&retryHint{seconds: seconds, at: now})
	return seconds
}

// modifyResponse installs the bounded tail tap (§9.3). It never changes a header
// and never touches a byte of the body on its way to the client.
func (h *instanceHandler) modifyResponse(resp *http.Response) error {
	st := reqStateFrom(resp.Request.Context())
	if st == nil || st.tapBytes <= 0 {
		// `gateway.usage_parse_kb = 0` disables the tap entirely, which is the
		// setting for anyone who wants byte-for-byte pass-through with provably
		// zero inspection.
		return nil
	}

	mediaType := strings.TrimSpace(strings.SplitN(resp.Header.Get("Content-Type"), ";", 2)[0])
	stream := strings.EqualFold(mediaType, "text/event-stream")
	if !stream {
		if !strings.EqualFold(mediaType, "application/json") {
			return nil
		}
		// A declared length above the cap means a body that is not a completion
		// response, and scanning it would buy nothing.
		if resp.ContentLength > maxTapContentLength {
			return nil
		}
	}
	// A compressed body cannot be scanned, and D36 guarantees we did not ask for
	// one — but a client that negotiated its own encoding gets it passed
	// through, and that response is opaque to us by design.
	if resp.Header.Get("Content-Encoding") != "" {
		return nil
	}

	tap := &tailTap{
		rc:     resp.Body,
		ring:   h.g.taps.get(st.tapBytes),
		pool:   h.g.taps,
		stream: stream,
	}
	st.tap = tap
	resp.Body = tap
	return nil
}

// upstreamError turns a failed proxy attempt into section 9.2's answer: a 502 or
// 503 in the OpenAI error shape.
func (h *instanceHandler) upstreamError(w http.ResponseWriter, r *http.Request, err error) {
	// A client that hung up is not an error worth a response — there is nobody
	// left to read it — and it is the ordinary end of an abandoned stream.
	if errors.Is(err, context.Canceled) {
		return
	}

	var maxBytes *http.MaxBytesError
	if errors.As(err, &maxBytes) {
		writeGatewayError(w, http.StatusRequestEntityTooLarge, CodeRequestTooLarge,
			"the request body is larger than gateway.max_body_mb allows", 0)
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		writeGatewayError(w, http.StatusGatewayTimeout, CodeUpstreamUnavailable,
			"the model did not answer within gateway.request_timeout_sec", 0)
		return
	}

	snap := h.snap.Load()
	h.g.log.Warn("a proxied request could not reach llama-server",
		"instance", snap.Name, "instance_id", snap.ID, "port", snap.InternalPort, "error", err)
	writeGatewayError(w, http.StatusBadGateway, CodeInstanceNotRunning,
		"instance "+snap.Name+" did not answer on its internal port", 0)
}

// credential extracts the presented secret. Section 3.15 accepts three forms,
// in this order of precedence: `Authorization: Bearer lm_…`, `X-API-Key: lm_…`,
// and `?api_key=`.
func credential(r *http.Request) string {
	if v := r.Header.Get("Authorization"); v != "" {
		if rest, ok := cutPrefixFold(v, "bearer "); ok {
			return strings.TrimSpace(rest)
		}
		return strings.TrimSpace(v)
	}
	if v := strings.TrimSpace(r.Header.Get("X-API-Key")); v != "" {
		return v
	}
	return strings.TrimSpace(r.URL.Query().Get("api_key"))
}

func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || !strings.EqualFold(s[:len(prefix)], prefix) {
		return s, false
	}
	return s[len(prefix):], true
}

// clientIP is the address a denial is attributed to. A request with no
// parseable remote address — a unix socket in a test — gives the empty string,
// which is a fact rather than an address and is never counted as a burst.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return strings.TrimSpace(r.RemoteAddr)
	}
	return host
}

// reqState is what the handler and the proxy's two callbacks share about one
// request. It travels in the context because ReverseProxy gives ModifyResponse
// and ErrorHandler nothing else that is per-request.
type reqState struct {
	tapBytes int
	tap      *tailTap
}

type reqStateKey struct{}

func withReqState(ctx context.Context, st *reqState) context.Context {
	return context.WithValue(ctx, reqStateKey{}, st)
}

func reqStateFrom(ctx context.Context) *reqState {
	st, _ := ctx.Value(reqStateKey{}).(*reqState)
	return st
}

// countingReader counts the request bytes that reached the upstream. It is an
// io.ReadCloser wrapper rather than a full copy, so the body is still streamed.
type countingReader struct {
	rc io.ReadCloser
	n  atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.rc.Read(p)
	if n > 0 {
		c.n.Add(int64(n))
	}
	return n, err
}

func (c *countingReader) Close() error { return c.rc.Close() }

// countingWriter counts the response bytes and remembers the status.
//
// It implements Flush and Unwrap so that `FlushInterval: -1` still flushes:
// httputil.ReverseProxy reaches the flusher through http.ResponseController,
// which follows Unwrap. It deliberately does NOT implement io.ReaderFrom, which
// would let the copier bypass Write and lose the byte count.
type countingWriter struct {
	http.ResponseWriter
	n           int64
	status      int
	wroteHeader bool
}

func (c *countingWriter) WriteHeader(status int) {
	if c.wroteHeader {
		return
	}
	c.wroteHeader = true
	c.status = status
	c.ResponseWriter.WriteHeader(status)
}

func (c *countingWriter) Write(p []byte) (int, error) {
	if !c.wroteHeader {
		c.WriteHeader(http.StatusOK)
	}
	n, err := c.ResponseWriter.Write(p)
	c.n += int64(n)
	return n, err
}

func (c *countingWriter) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the underlying writer to http.ResponseController, which is how
// the proxy reaches Flush and how an upgrade reaches Hijack.
func (c *countingWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

func writeJSON(w http.ResponseWriter, status int, v any) int64 {
	b, err := json.Marshal(v)
	if err != nil {
		b = []byte(`{"error":{"code":"internal_error","message":"internal error"}}`)
		status = http.StatusInternalServerError
	}
	b = append(b, '\n')
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	n, _ := w.Write(b)
	return int64(n)
}
