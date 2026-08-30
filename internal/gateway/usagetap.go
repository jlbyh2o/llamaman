package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"sync"
)

// The bounded tail tap (DESIGN section 9.3).
//
// This is the ONE place the gateway looks inside a response, so the mechanism,
// its memory bound and its failure mode are stated rather than implied.
//
//   - NOTHING IS HELD BACK. The tap is an io.Reader in front of the upstream
//     body: every byte is returned to the copier immediately and a COPY of the
//     last `gateway.usage_parse_kb` bytes is kept in a fixed ring. Under
//     `FlushInterval: -1` that means every byte still reaches the client at the
//     moment it arrived. "Extract usage" never becomes "buffer the response",
//     which is what the 200 MB test in §15 exists to prove.
//   - CLASSIFICATION USES THE RESPONSE, NEVER THE REQUEST. The gateway promised
//     not to buffer the request body, so it cannot know whether the client sent
//     `"stream": true` — and it does not need to. `Content-Type:
//     text/event-stream` is a stream; anything else is not. That distinction is
//     available in the response headers before the first byte of body and costs
//     nothing.
//   - THE FAILURE MODE IS NULL. A truncated tail, a `usage` object split across
//     the ring boundary, a compressed body, or any parse error leaves both
//     counts nil. It is never an error the client sees, never a retry, and never
//     a reason to hold a response. Instance-level token totals come from
//     llama-server's own `/metrics` regardless, which is why abstaining is
//     better than guessing.

// maxTapContentLength is §9.3's cap for the non-streamed case: a tap is
// installed only when there is no `Content-Length` or it is at most 8 MiB.
const maxTapContentLength = 8 << 20

// Usage is what the tap recovered. Both fields are nil unless the upstream
// actually reported them.
type Usage struct {
	PromptTokens     *int64
	CompletionTokens *int64
}

// ring keeps the last len(buf) bytes written to it, in order, without ever
// growing. It is the memory bound §9.3 promises: `gateway.usage_parse_kb` per
// in-flight proxied request, allocated from a pool, hard-capped and never grown.
type ring struct {
	buf  []byte
	pos  int
	full bool
}

func (r *ring) Write(p []byte) (int, error) {
	n := len(p)
	if len(r.buf) == 0 {
		return n, nil
	}
	// Only the last len(buf) bytes of p can survive, so a huge write costs one
	// copy of the buffer rather than one pass over the payload.
	if len(p) >= len(r.buf) {
		copy(r.buf, p[len(p)-len(r.buf):])
		r.pos = 0
		r.full = true
		return n, nil
	}
	for len(p) > 0 {
		c := copy(r.buf[r.pos:], p)
		p = p[c:]
		r.pos += c
		if r.pos == len(r.buf) {
			r.pos = 0
			r.full = true
		}
	}
	return n, nil
}

// bytes returns the retained tail in order. It allocates once, at EOF, on a
// buffer that is already bounded.
func (r *ring) bytes() []byte {
	if !r.full {
		return r.buf[:r.pos]
	}
	out := make([]byte, 0, len(r.buf))
	out = append(out, r.buf[r.pos:]...)
	out = append(out, r.buf[:r.pos]...)
	return out
}

// tailTap copies every byte straight through and keeps the last N in a ring.
type tailTap struct {
	rc     io.ReadCloser
	ring   *ring
	pool   *tapPool
	stream bool

	mu   sync.Mutex
	seen Usage
	done bool
}

func (t *tailTap) Read(p []byte) (int, error) {
	n, err := t.rc.Read(p)
	if n > 0 {
		_, _ = t.ring.Write(p[:n])
	}
	if err == io.EOF {
		t.finish()
	}
	return n, err
}

// Close finishes the scan even when the body was abandoned part way — a client
// that disconnected mid-stream still produced bytes worth accounting for, and
// the ring has to go back to the pool either way.
func (t *tailTap) Close() error {
	t.finish()
	return t.rc.Close()
}

func (t *tailTap) finish() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return
	}
	t.done = true

	tail := t.ring.bytes()
	if t.stream {
		t.seen = parseStreamUsage(tail)
	} else {
		t.seen = parseJSONUsage(tail)
	}
	t.pool.put(t.ring)
	t.ring = &ring{}
}

// usage reports what the scan found. It is read after the proxy returns, by
// which time the body has been copied and finish has run.
func (t *tailTap) usage() Usage {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.seen
}

// tapPool hands out ring buffers of one size. A size change — the operator
// edited `gateway.usage_parse_kb` — is handled by discarding the wrong-sized
// buffer rather than by keeping a pool per size: the setting changes about once
// in the life of an install.
type tapPool struct {
	pool sync.Pool
}

func newTapPool() *tapPool {
	return &tapPool{pool: sync.Pool{New: func() any { return &ring{} }}}
}

func (p *tapPool) get(size int) *ring {
	r := p.pool.Get().(*ring)
	if cap(r.buf) < size {
		r.buf = make([]byte, size)
	}
	r.buf = r.buf[:size]
	r.pos, r.full = 0, false
	return r
}

func (p *tapPool) put(r *ring) {
	if r == nil || cap(r.buf) == 0 {
		return
	}
	r.pos, r.full = 0, false
	p.pool.Put(r)
}

var usageKey = []byte(`"usage"`)

// parseJSONUsage scans a non-streamed tail BACKWARDS for `"usage"` and parses
// the object after it with a brace-matching scanner.
//
// Backwards because `usage` is at the end of an OpenAI completion body and the
// tail is the end of the response: the first match from the right is the one
// that belongs to this response rather than to a string inside a message the
// model produced.
func parseJSONUsage(tail []byte) Usage {
	for at := bytes.LastIndex(tail, usageKey); at >= 0; at = bytes.LastIndex(tail[:at], usageKey) {
		obj, ok := objectAfter(tail, at+len(usageKey))
		if !ok {
			continue
		}
		if u, ok := decodeUsage(obj); ok {
			return u
		}
	}
	return Usage{}
}

// parseStreamUsage scans the FINAL non-empty `data:` frame, which is where
// `usage` appears when — and only when — the client set
// `stream_options.include_usage`.
//
// `data: [DONE]` is the last frame of an OpenAI stream and carries no JSON, so
// frames are walked backwards until one parses.
func parseStreamUsage(tail []byte) Usage {
	lines := bytes.Split(tail, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimRight(lines[i], "\r")
		payload, ok := bytes.CutPrefix(line, []byte("data:"))
		if !ok {
			continue
		}
		payload = bytes.TrimSpace(payload)
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		// A frame that begins part way through — the ring boundary landed
		// inside it — will not parse, and that is the abstention §9.3 asks for.
		var frame struct {
			Usage json.RawMessage `json:"usage"`
		}
		if err := json.Unmarshal(payload, &frame); err != nil {
			continue
		}
		if len(frame.Usage) == 0 || bytes.Equal(frame.Usage, []byte("null")) {
			continue
		}
		if u, ok := decodeUsage(frame.Usage); ok {
			return u
		}
	}
	return Usage{}
}

// objectAfter finds the `{ … }` that follows a key at index i, matching braces
// while respecting strings and their escapes.
func objectAfter(b []byte, i int) ([]byte, bool) {
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r') {
		i++
	}
	if i >= len(b) || b[i] != ':' {
		return nil, false
	}
	i++
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r') {
		i++
	}
	if i >= len(b) || b[i] != '{' {
		return nil, false
	}

	depth := 0
	inString := false
	escaped := false
	for j := i; j < len(b); j++ {
		c := b[j]
		switch {
		case escaped:
			escaped = false
		case inString && c == '\\':
			escaped = true
		case c == '"':
			inString = !inString
		case inString:
			// nothing: braces inside a string are text
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return b[i : j+1], true
			}
		}
	}
	// The object ran past the end of the tail: truncated, so NULL.
	return nil, false
}

func decodeUsage(obj []byte) (Usage, bool) {
	var raw struct {
		PromptTokens     *int64 `json:"prompt_tokens"`
		CompletionTokens *int64 `json:"completion_tokens"`
	}
	if err := json.Unmarshal(obj, &raw); err != nil {
		return Usage{}, false
	}
	if raw.PromptTokens == nil && raw.CompletionTokens == nil {
		return Usage{}, false
	}
	return Usage{PromptTokens: raw.PromptTokens, CompletionTokens: raw.CompletionTokens}, true
}
