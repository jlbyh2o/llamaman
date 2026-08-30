package gateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/tokens"
)

// TestNoAuthTrafficIsStillCounted is D56, and it is the reason that decision
// exists: `auth_mode='none'` is an explicit SPEC §3.4 feature, and a
// token-keyed primary key silently drops all of its traffic — including bytes
// and errors, which `/metrics` cannot supply.
func TestNoAuthTrafficIsStillCounted(t *testing.T) {
	t.Parallel()

	body := strings.Repeat("x", 300)
	h := newHarness(t, model.AuthNone, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	}))

	req, _ := http.NewRequest(http.MethodPost, h.url("/v1/completions"),
		strings.NewReader(strings.Repeat("q", 120)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := rawClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	h.flush(1)

	got := h.store.usageFor(h.instanceID)
	if got.AuthMode != model.AuthNone {
		t.Errorf("auth_mode = %q, want none — the column exists so this traffic has a home",
			got.AuthMode)
	}
	if got.Requests != 1 {
		t.Errorf("requests = %d, want 1", got.Requests)
	}
	if got.Errors != 0 {
		t.Errorf("errors = %d, want 0", got.Errors)
	}
	if got.BytesIn != 120 {
		t.Errorf("bytes_in = %d, want 120", got.BytesIn)
	}
	if got.BytesOut != int64(len(body)) {
		t.Errorf("bytes_out = %d, want %d", got.BytesOut, len(body))
	}

	// D56's second half: `token_usage_daily` is written only when a credential
	// was presented, so a no-auth instance accrues NOTHING there.
	if rows := h.store.tokenRows(); len(rows) != 0 {
		t.Errorf("token_usage_daily got %d rows for an auth_mode='none' instance; it is the "+
			"per-credential BREAKDOWN and there was no credential", len(rows))
	}
}

// TestTokenTrafficAccruesBothTablesWithTheTokenTableASubset is the other row of
// section 15's accounting test: "a token instance accrues both tables with the
// token table a strict subset".
func TestTokenTrafficAccruesBothTablesWithTheTokenTableASubset(t *testing.T) {
	t.Parallel()

	h := newHarness(t, model.AuthToken, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"usage":{"prompt_tokens":11,"completion_tokens":7}}`)
	}))

	minted, err := h.tokens.Mint(context.Background(), tokens.MintParams{Name: "counting"})
	if err != nil {
		t.Fatal(err)
	}

	const authenticated = 3
	for range authenticated {
		req, _ := http.NewRequest(http.MethodPost, h.url("/v1/chat/completions"), strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+minted.Secret)
		resp, err := rawClient().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
	}

	// One refused request, which must land in the denial counters and in
	// NEITHER usage table: nothing was proxied.
	refused, _ := http.NewRequest(http.MethodPost, h.url("/v1/chat/completions"), nil)
	resp, err := rawClient().Do(refused)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	h.flush(authenticated)

	inst := h.store.usageFor(h.instanceID)
	if inst.Requests != authenticated {
		t.Errorf("instance requests = %d, want %d — the denial is not a proxied request",
			inst.Requests, authenticated)
	}
	if inst.AuthMode != model.AuthToken {
		t.Errorf("auth_mode = %q, want token", inst.AuthMode)
	}

	var tokenRequests, tokenBytesOut int64
	var prompt, completion int64
	for _, row := range h.store.tokenRows() {
		if row.TokenID != minted.Token.ID {
			t.Errorf("token_usage_daily row for %q, want %q", row.TokenID, minted.Token.ID)
		}
		if row.InstanceID != h.instanceID {
			t.Errorf("token_usage_daily instance = %q, want %q", row.InstanceID, h.instanceID)
		}
		tokenRequests += row.Requests
		tokenBytesOut += row.BytesOut
		if row.PromptTokens != nil {
			prompt += *row.PromptTokens
		}
		if row.CompletionTokens != nil {
			completion += *row.CompletionTokens
		}
	}
	if tokenRequests != authenticated {
		t.Errorf("token requests = %d, want %d", tokenRequests, authenticated)
	}
	if tokenRequests > inst.Requests || tokenBytesOut > inst.BytesOut {
		t.Errorf("the per-token table is not a subset of the instance table: "+
			"%d/%d requests, %d/%d bytes", tokenRequests, inst.Requests, tokenBytesOut, inst.BytesOut)
	}
	// The tail tap read `usage` out of the response (§9.3).
	if prompt != 11*authenticated || completion != 7*authenticated {
		t.Errorf("reported tokens = %d prompt / %d completion, want %d / %d",
			prompt, completion, 11*authenticated, 7*authenticated)
	}

	if len(h.store.denialRows()) != 1 {
		t.Errorf("gateway_denials_daily got %d rows, want 1", len(h.store.denialRows()))
	}
}

// TestUpstreamErrorsCountAsErrors: `instance_usage_daily.errors` is what the
// dashboard's error rate reads, and it exists for both auth modes (§2.9).
func TestUpstreamErrorsCountAsErrors(t *testing.T) {
	t.Parallel()

	h := newHarness(t, model.AuthNone, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))

	resp, err := rawClient().Get(h.url("/v1/models"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	h.flush(1)
	got := h.store.usageFor(h.instanceID)
	if got.Requests != 1 || got.Errors != 1 {
		t.Errorf("requests/errors = %d/%d, want 1/1", got.Requests, got.Errors)
	}
}

// TestRevocationTakesEffectWithinOneRequest is section 9.3's epoch counter, and
// the whole reason Llama Man owns the public port (SPEC §1): revoking a token
// takes effect within one request, with no reload of anything and no restart.
func TestRevocationTakesEffectWithinOneRequest(t *testing.T) {
	t.Parallel()

	h := newHarness(t, model.AuthToken, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	minted, err := h.tokens.Mint(context.Background(), tokens.MintParams{Name: "revoke me"})
	if err != nil {
		t.Fatal(err)
	}

	call := func() int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, h.url("/v1/models"), nil)
		req.Header.Set("Authorization", "Bearer "+minted.Secret)
		resp, err := rawClient().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if got := call(); got != http.StatusOK {
		t.Fatalf("first call = %d, want 200", got)
	}
	readsAfterFirst := h.tokStore.hashReads()

	// The second call must be served from the cache: a database read per
	// request would put the daemon back in the data path §9.1 keeps it out of.
	if got := call(); got != http.StatusOK {
		t.Fatalf("second call = %d, want 200", got)
	}
	if h.tokStore.hashReads() != readsAfterFirst {
		t.Errorf("the verified-token cache re-read the row on a hit: %d reads, want %d",
			h.tokStore.hashReads(), readsAfterFirst)
	}

	epochBefore := h.tokens.Epoch()
	if err := h.tokens.Revoke(context.Background(), minted.Token.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if h.tokens.Epoch() == epochBefore {
		t.Error("revoking a token did not bump the epoch; the cache would go on trusting it")
	}

	// No restart, no reconcile, no reload: the very next request is refused.
	if got := call(); got != http.StatusUnauthorized {
		t.Errorf("the call after the revoke = %d, want 401", got)
	}

	// Two of the three calls were proxied; the third was refused, and a refusal
	// is booked before its response is written.
	h.flush(2)
	var revoked int64
	for _, d := range h.store.denialRows() {
		if d.Reason == model.DeniedRevoked {
			revoked += d.Count
		}
	}
	if revoked != 1 {
		t.Errorf("revoked denials = %d, want 1", revoked)
	}
}

// TestDisablingATokenTakesEffectImmediately covers the other half of SPEC
// §3.4's "enable/disable/revoke instantly" — and that disable is reversible
// while revoke is not.
func TestDisablingATokenTakesEffectImmediately(t *testing.T) {
	t.Parallel()

	h := newHarness(t, model.AuthToken, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	minted, err := h.tokens.Mint(context.Background(), tokens.MintParams{Name: "toggle"})
	if err != nil {
		t.Fatal(err)
	}
	call := func() int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, h.url("/v1/models"), nil)
		req.Header.Set("Authorization", "Bearer "+minted.Secret)
		resp, err := rawClient().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if got := call(); got != http.StatusOK {
		t.Fatalf("before disabling = %d, want 200", got)
	}

	disabled := model.TokenDisabled
	if _, err := h.tokens.Patch(context.Background(), minted.Token.ID,
		tokens.PatchParams{State: &disabled}); err != nil {
		t.Fatalf("Patch: %v", err)
	}
	if got := call(); got != http.StatusUnauthorized {
		t.Errorf("while disabled = %d, want 401", got)
	}

	active := model.TokenActive
	if _, err := h.tokens.Patch(context.Background(), minted.Token.ID,
		tokens.PatchParams{State: &active}); err != nil {
		t.Fatalf("Patch: %v", err)
	}
	if got := call(); got != http.StatusOK {
		t.Errorf("after re-enabling = %d, want 200", got)
	}
}

// TestRateLimitedTokenGets429: a rate limit is not a credential failure, so an
// SDK must not be told to check its key (§9.3's `rate_limit_rpm` bucket).
func TestRateLimitedTokenGets429(t *testing.T) {
	t.Parallel()

	h := newHarness(t, model.AuthToken, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	limit := int64(2)
	secret := h.mint(tokens.MintParams{Name: "slow down", RateLimitRPM: &limit})

	statuses := make([]int, 0, 4)
	for range 4 {
		req, _ := http.NewRequest(http.MethodGet, h.url("/v1/models"), nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		resp, err := rawClient().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		statuses = append(statuses, resp.StatusCode)
	}

	want := []int{http.StatusOK, http.StatusOK, http.StatusTooManyRequests, http.StatusTooManyRequests}
	if fmt.Sprint(statuses) != fmt.Sprint(want) {
		t.Errorf("statuses = %v, want %v", statuses, want)
	}
}

// TestDenialBurstRaisesAWarnEvent is section 9.3's last line: five denials from
// one address within a minute emit a `warn` event, so the dashboard can show
// "unauthorized attempts on port 8081".
func TestDenialBurstRaisesAWarnEvent(t *testing.T) {
	h := newHarness(t, model.AuthToken, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for range DenialBurst {
		req, _ := http.NewRequest(http.MethodGet, h.url("/v1/models"), nil)
		req.Header.Set("Authorization", "Bearer lm_nope")
		resp, err := rawClient().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	// The event is recorded from a goroutine so a refusal is never slowed by a
	// database write.
	waitFor(t, func() bool {
		for _, a := range h.events.actions() {
			if a == "gateway_denial_burst" {
				return true
			}
		}
		return false
	}, "the denial burst never raised an event")
}

// TestDenialWatchIsBounded: the map is keyed by SOURCE ADDRESS, and the source
// address is chosen by whoever is connecting.
//
// `gateway.bind` is 0.0.0.0 by default (§9.1, SPEC §1's trusted-LAN exposure),
// so any host that can complete a handshake can add a key. Without a sweep an
// expired window was rewritten in place and never deleted, so ten million
// addresses — trivial from one /64 with IPv6 source rotation — was about two
// gigabytes of daemon heap and an OOM kill that took the management UI, the
// supervisor and every public listener with it. This asserts both halves of the
// bound: expiry actually deletes, and a flood inside one window is capped.
func TestDenialWatchIsBounded(t *testing.T) {
	t.Parallel()

	w := newDenialWatch()
	base := time.Unix(1_700_000_000, 0).UTC()

	// A million distinct addresses, spread over an hour: every one of them ages
	// out, and the sweep is what makes that mean "is deleted".
	for i := range 200_000 {
		at := base.Add(time.Duration(i) * time.Millisecond)
		w.note("01INST", "203.0.113."+strconv.Itoa(i%256)+":"+strconv.Itoa(i), at)
	}

	w.mu.Lock()
	n := len(w.at)
	w.mu.Unlock()
	if n > MaxDenialWindows {
		t.Fatalf("the watch holds %d windows, above the cap of %d", n, MaxDenialWindows)
	}

	// The same flood inside ONE window, where nothing has expired: the cap is
	// the only thing standing between this and the heap.
	burst := newDenialWatch()
	for i := range 100_000 {
		burst.note("01INST", "198.51.100.1:"+strconv.Itoa(i), base)
	}
	burst.mu.Lock()
	n = len(burst.at)
	burst.mu.Unlock()
	if n > MaxDenialWindows {
		t.Fatalf("a same-window flood left %d windows, above the cap of %d", n, MaxDenialWindows)
	}
}

// TestDenialWatchStillWarnsOnABurst is the behavior the bound must not cost:
// §9.3's "five denials from one IP within a minute emit a warn event", once.
func TestDenialWatchStillWarnsOnABurst(t *testing.T) {
	t.Parallel()

	w := newDenialWatch()
	at := time.Unix(1_700_000_000, 0).UTC()

	warned := 0
	for i := range DenialBurst + 4 {
		if w.note("01INST", "203.0.113.7:5000", at.Add(time.Duration(i)*time.Second)) {
			warned++
		}
	}
	if warned != 1 {
		t.Errorf("a burst of %d raised %d warnings, want exactly 1",
			DenialBurst+4, warned)
	}

	// A window that has aged out starts over rather than resuming, and the new
	// window is a NEW entry rather than the old one rewritten in place.
	if w.note("01INST", "203.0.113.7:5000", at.Add(2*DenialWindow)) {
		t.Error("the first denial of a fresh window raised a warning")
	}
}
