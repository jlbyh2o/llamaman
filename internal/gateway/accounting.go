package gateway

import (
	"context"
	"sync"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// Gateway accounting (DESIGN section 9.3, D56).
//
// D56's name for this is "instance-first", and it is a correctness rule rather
// than a preference: `instance_usage_daily` is written for EVERY proxied
// request, including `auth_mode='none'`, and `token_usage_daily` is the
// per-credential breakdown written ADDITIONALLY when a credential was presented.
// Writing only the second would mean a no-auth instance — an explicit SPEC §3.4
// feature — accumulated no requests, no bytes and no errors anywhere in the
// database, with only `gateway_denials_daily` to show for it.
//
// Counters live in memory and are upserted every FlushInterval and on shutdown.
// The maps are cleared only after the write COMMITS, so a failed flush
// under-reports nothing: the deltas are still there for the next one.

// FlushInterval is section 9.3's "upserted every 5 s and on shutdown".
const FlushInterval = 5 * time.Second

// day renders the 'YYYY-MM-DD' UTC key both usage tables are partitioned by.
func day(t time.Time) string { return t.UTC().Format("2006-01-02") }

type instanceUsageKey struct {
	instanceID string
	day        string
	authMode   model.AuthMode
}

type tokenUsageKey struct {
	tokenID    string
	instanceID string
	day        string
}

type denialKey struct {
	instanceID string
	day        string
	reason     model.DenialReason
}

// counters is one row's worth of deltas. The two token counts are pointers all
// the way through: nil means "the upstream reported nothing", which must stay
// distinguishable from zero right up to the SQL that writes it (§9.3, F14).
type counters struct {
	requests   int64
	errors     int64
	bytesIn    int64
	bytesOut   int64
	durationMS int64

	promptTokens     *int64
	completionTokens *int64
}

func (c *counters) addTokens(u Usage) {
	if u.PromptTokens != nil {
		v := *u.PromptTokens
		if c.promptTokens != nil {
			v += *c.promptTokens
		}
		c.promptTokens = &v
	}
	if u.CompletionTokens != nil {
		v := *u.CompletionTokens
		if c.completionTokens != nil {
			v += *c.completionTokens
		}
		c.completionTokens = &v
	}
}

// record is one finished request, as the handler hands it to the accountant.
type record struct {
	instanceID string
	authMode   model.AuthMode
	// tokenID is empty when no credential was presented — which is the whole of
	// `auth_mode='none'` traffic, and is exactly the case D56 exists for.
	tokenID string

	at       time.Time
	duration time.Duration
	bytesIn  int64
	bytesOut int64
	// failed is "this request did not succeed": a 4xx or 5xx from the upstream,
	// or an error the gateway itself answered with once the request had been
	// admitted.
	failed bool
	usage  Usage
}

// accountant holds the in-memory counters between flushes.
type accountant struct {
	mu        sync.Mutex
	instances map[instanceUsageKey]*counters
	tokens    map[tokenUsageKey]*counters
	denials   map[denialKey]int64
}

func newAccountant() *accountant {
	return &accountant{
		instances: map[instanceUsageKey]*counters{},
		tokens:    map[tokenUsageKey]*counters{},
		denials:   map[denialKey]int64{},
	}
}

// add books one proxied request.
func (a *accountant) add(r record) {
	d := day(r.at)
	ms := r.duration.Milliseconds()
	var errs int64
	if r.failed {
		errs = 1
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	ik := instanceUsageKey{instanceID: r.instanceID, day: d, authMode: r.authMode}
	ic, ok := a.instances[ik]
	if !ok {
		ic = &counters{}
		a.instances[ik] = ic
	}
	ic.requests++
	ic.errors += errs
	ic.bytesIn += r.bytesIn
	ic.bytesOut += r.bytesOut
	ic.durationMS += ms

	if r.tokenID == "" {
		return
	}
	tk := tokenUsageKey{tokenID: r.tokenID, instanceID: r.instanceID, day: d}
	tc, ok := a.tokens[tk]
	if !ok {
		tc = &counters{}
		a.tokens[tk] = tc
	}
	tc.requests++
	tc.errors += errs
	tc.bytesIn += r.bytesIn
	tc.bytesOut += r.bytesOut
	tc.durationMS += ms
	tc.addTokens(r.usage)
}

// deny books one refusal. A denied request never reached the upstream, so it is
// counted HERE and deliberately not in `instance_usage_daily`: conflating the
// two would make an instance under a credential-stuffing attempt look busy.
func (a *accountant) deny(instanceID string, reason model.DenialReason, at time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.denials[denialKey{instanceID: instanceID, day: day(at), reason: reason}]++
}

// pending reports whether anything is waiting to be written. It exists so a
// flush on an idle daemon costs no transaction at all.
func (a *accountant) pending() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.instances) > 0 || len(a.tokens) > 0 || len(a.denials) > 0
}

// drain takes the current deltas and leaves the accountant empty. The caller
// puts them back with restore if the write fails.
func (a *accountant) drain() (map[instanceUsageKey]*counters, map[tokenUsageKey]*counters, map[denialKey]int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	i, t, d := a.instances, a.tokens, a.denials
	a.instances = map[instanceUsageKey]*counters{}
	a.tokens = map[tokenUsageKey]*counters{}
	a.denials = map[denialKey]int64{}
	return i, t, d
}

// restore folds a failed flush's deltas back in. A flush that failed has not
// been recorded anywhere, and dropping it would quietly under-report the
// dashboard's request and byte counts forever.
func (a *accountant) restore(i map[instanceUsageKey]*counters, t map[tokenUsageKey]*counters, d map[denialKey]int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for k, v := range i {
		c, ok := a.instances[k]
		if !ok {
			a.instances[k] = v
			continue
		}
		c.requests += v.requests
		c.errors += v.errors
		c.bytesIn += v.bytesIn
		c.bytesOut += v.bytesOut
		c.durationMS += v.durationMS
	}
	for k, v := range t {
		c, ok := a.tokens[k]
		if !ok {
			a.tokens[k] = v
			continue
		}
		c.requests += v.requests
		c.errors += v.errors
		c.bytesIn += v.bytesIn
		c.bytesOut += v.bytesOut
		c.durationMS += v.durationMS
		c.addTokens(Usage{PromptTokens: v.promptTokens, CompletionTokens: v.completionTokens})
	}
	for k, v := range d {
		a.denials[k] += v
	}
}

// Flush writes the accumulated counters. It is called every FlushInterval and
// once more on shutdown (§9.4 step 5).
//
// All three tables go in ONE transaction: the per-token rows are a strict subset
// of the instance rows by construction, and a partial commit would break that
// invariant in a way no later flush repairs.
func (g *Gateway) Flush(ctx context.Context) error {
	if !g.acct.pending() {
		return g.tokens.Flush(ctx, false)
	}

	instances, tokenRows, denials := g.acct.drain()
	err := g.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		for k, v := range instances {
			if err := g.store.AddInstanceUsage(ctx, tx, store.InstanceUsageDelta{
				InstanceID: k.instanceID,
				Day:        k.day,
				AuthMode:   k.authMode,
				Requests:   v.requests,
				Errors:     v.errors,
				BytesIn:    v.bytesIn,
				BytesOut:   v.bytesOut,
				DurationMS: v.durationMS,
			}); err != nil {
				return err
			}
		}
		for k, v := range tokenRows {
			if err := g.store.AddTokenUsage(ctx, tx, store.TokenUsageDelta{
				TokenID:          k.tokenID,
				InstanceID:       k.instanceID,
				Day:              k.day,
				Requests:         v.requests,
				Errors:           v.errors,
				BytesIn:          v.bytesIn,
				BytesOut:         v.bytesOut,
				PromptTokens:     v.promptTokens,
				CompletionTokens: v.completionTokens,
				DurationMS:       v.durationMS,
			}); err != nil {
				return err
			}
		}
		for k, n := range denials {
			if err := g.store.AddGatewayDenial(ctx, tx, store.DenialDelta{
				InstanceID: k.instanceID,
				Day:        k.day,
				Reason:     k.reason,
				Count:      n,
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		g.acct.restore(instances, tokenRows, denials)
		return err
	}
	return g.tokens.Flush(ctx, false)
}

// denialWatch is section 9.3's last line: "five denials from one IP within a
// minute emit a `warn` event so the dashboard can show 'unauthorized attempts on
// port 8081'".
//
// It counts per instance AND per address, and emits at most one event per window
// per pair — an unauthenticated flood must not bury the history it is part of,
// which is the same rule internal/store's HasLoginAttemptSince applies to the
// login audit trail.
// # Why it is swept, and why it is capped
//
// The key is (instance, SOURCE ADDRESS), and the source address is chosen by
// whoever is connecting. `gateway.bind` defaults to 0.0.0.0 — SPEC section 1's
// trusted-LAN exposure — so any host that can complete a TCP handshake can add
// a key, and an expired window used to be REWRITTEN in place rather than
// deleted, with no sweep anywhere. A single /64 with IPv6 source rotation, or
// any botnet, could therefore grow this map without bound at roughly two hundred
// bytes per address until the daemon was OOM-killed, taking the management UI,
// the supervisor and every public listener with it.
//
// That input is the very input this type exists to watch, so the abuse path and
// the accounting path are the same path, and the bound has to be structural:
//
//   - every write sweeps windows older than DenialWindow, so the map holds at
//     most the distinct addresses seen in the last minute rather than every
//     address seen since boot;
//   - and because a fast enough flood can exceed that inside one window, the map
//     is hard-capped at MaxDenialWindows and evicts down to it.
//
// Eviction is preferred to refusing new keys: refusing would make the watch
// permanently blind to whoever arrived after the cap, which is precisely the
// attacker it is meant to notice. The durable count is unaffected either way —
// `gateway_denials_daily` is written from the accountant, not from here.
type denialWatch struct {
	mu sync.Mutex
	at map[denialWatchKey]*denialWindow
	// sweptAt is when the last expiry sweep ran, so an ordinary denial costs a
	// map lookup rather than a scan.
	sweptAt time.Time
}

type denialWatchKey struct {
	instanceID string
	ip         string
}

type denialWindow struct {
	start    time.Time
	count    int
	reported bool
}

// DenialBurst is how many denials from one address inside DenialWindow raise the
// warning.
const (
	DenialBurst  = 5
	DenialWindow = time.Minute
	// MaxDenialWindows caps how many (instance, address) pairs are tracked at
	// once. At roughly two hundred bytes each this is under two megabytes, and
	// it is far above any legitimate number of DENYING addresses in one minute —
	// a LAN with eight thousand distinct clients all presenting bad credentials
	// inside sixty seconds is an attack, not a workload.
	MaxDenialWindows = 8192
	// denialSweepEvery is how often the expiry sweep runs. Half the window means
	// nothing survives more than ninety seconds past its expiry, while an
	// ordinary denial still costs one map lookup.
	denialSweepEvery = DenialWindow / 2
	// denialLowWater is what an over-cap eviction reduces the map TO, rather
	// than merely to the cap. Evicting one entry per insert at the cap would
	// make every subsequent denial pay for a full scan; leaving a quarter of the
	// cap free amortizes that scan over thousands of inserts.
	denialLowWater = MaxDenialWindows * 3 / 4
)

func newDenialWatch() *denialWatch { return &denialWatch{at: map[denialWatchKey]*denialWindow{}} }

// note records one denial and reports whether this is the moment the burst
// threshold was crossed.
func (d *denialWatch) note(instanceID, ip string, now time.Time) bool {
	if ip == "" {
		return false
	}
	k := denialWatchKey{instanceID: instanceID, ip: ip}

	d.mu.Lock()
	defer d.mu.Unlock()

	d.sweepLocked(now)

	w, ok := d.at[k]
	if !ok || now.Sub(w.start) > DenialWindow {
		d.at[k] = &denialWindow{start: now, count: 1}
		return false
	}
	w.count++
	if w.count < DenialBurst || w.reported {
		return false
	}
	w.reported = true
	return true
}

// sweepLocked deletes expired windows, and evicts arbitrary ones if the map is
// still over the cap. The caller holds d.mu.
//
// Deleting is the point: the previous code rewrote an expired window in place,
// which kept the KEY alive forever — so an address that sent one denial a week
// ago still cost memory. An entry that has aged out carries no information, and
// keeping it is indistinguishable from a leak.
func (d *denialWatch) sweepLocked(now time.Time) {
	full := len(d.at) >= MaxDenialWindows
	if !full && now.Sub(d.sweptAt) < denialSweepEvery {
		return
	}
	d.sweptAt = now

	for k, w := range d.at {
		if now.Sub(w.start) > DenialWindow {
			delete(d.at, k)
		}
	}
	// A flood fast enough to fill the cap inside one window, where nothing has
	// expired yet: evict down to the low-water mark. Map iteration order is
	// randomized, so this is an arbitrary eviction rather than an oldest-first
	// one — the honest trade, since finding the oldest would cost another full
	// scan for a distinction that does not matter under an attack.
	for k := range d.at {
		if len(d.at) <= denialLowWater {
			break
		}
		delete(d.at, k)
	}
}

// forget drops the windows for an instance whose listener is closing, so a
// deleted instance does not hold addresses in memory for the life of the daemon.
func (d *denialWatch) forget(instanceID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for k := range d.at {
		if k.instanceID == instanceID {
			delete(d.at, k)
		}
	}
}
