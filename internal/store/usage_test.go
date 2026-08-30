package store

import (
	"context"
	"testing"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// TestInstanceUsageIsAdditivePerAuthMode is D56's shape in the schema: the
// primary key is (instance_id, day, auth_mode), so one instance that was
// switched from `token` to `none` mid-day keeps both halves of its history and
// every flush ADDS rather than replaces.
func TestInstanceUsageIsAdditivePerAuthMode(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	ctx := context.Background()
	const inst = "01JINST0000000000000000001"
	seedInstance(t, s, newInstance(inst, "inst1", 8081, 21001))

	flushes := []InstanceUsageDelta{
		{InstanceID: inst, Day: "2026-03-01", AuthMode: model.AuthToken,
			Requests: 3, Errors: 1, BytesIn: 100, BytesOut: 900, DurationMS: 1500},
		{InstanceID: inst, Day: "2026-03-01", AuthMode: model.AuthToken,
			Requests: 2, BytesIn: 50, BytesOut: 400, DurationMS: 500},
		{InstanceID: inst, Day: "2026-03-01", AuthMode: model.AuthNone,
			Requests: 7, BytesOut: 70},
		{InstanceID: inst, Day: "2026-03-02", AuthMode: model.AuthToken, Requests: 1},
	}
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		for _, d := range flushes {
			if err := s.AddInstanceUsage(ctx, tx, d); err != nil {
				return err
			}
		}
		return nil
	})

	rows, err := s.InstanceUsage(ctx, s.RO, inst, UsageRange{})
	if err != nil {
		t.Fatalf("InstanceUsage: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3 (two auth modes on day one, one on day two)", len(rows))
	}

	byKey := map[string]InstanceUsageRow{}
	for _, r := range rows {
		byKey[r.Day+"/"+string(r.AuthMode)] = r
	}
	tokenDay1 := byKey["2026-03-01/token"]
	if tokenDay1.Requests != 5 || tokenDay1.Errors != 1 {
		t.Errorf("token day 1 requests/errors = %d/%d, want 5/1",
			tokenDay1.Requests, tokenDay1.Errors)
	}
	if tokenDay1.BytesIn != 150 || tokenDay1.BytesOut != 1300 || tokenDay1.DurationMS != 2000 {
		t.Errorf("token day 1 bytes/duration = %d/%d/%d, want 150/1300/2000",
			tokenDay1.BytesIn, tokenDay1.BytesOut, tokenDay1.DurationMS)
	}
	noneDay1 := byKey["2026-03-01/none"]
	if noneDay1.Requests != 7 || noneDay1.BytesOut != 70 {
		t.Errorf("none day 1 = %d requests / %d bytes out, want 7/70",
			noneDay1.Requests, noneDay1.BytesOut)
	}

	// The range filter is inclusive at both ends.
	day2, err := s.InstanceUsage(ctx, s.RO, inst, UsageRange{From: "2026-03-02", To: "2026-03-02"})
	if err != nil {
		t.Fatal(err)
	}
	if len(day2) != 1 || day2[0].Day != "2026-03-02" {
		t.Errorf("the range filter returned %v", day2)
	}
}

// TestTokenUsageLeavesUnreportedCountsNull is the one column pair that is not a
// plain sum (§9.3): NULL means "the upstream did not report it", and a NULL
// delta must leave the stored value alone rather than coercing it to zero —
// otherwise one unreported request would erase a day of reported ones.
func TestTokenUsageLeavesUnreportedCountsNull(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	ctx := context.Background()
	const inst = "01JINST0000000000000000001"
	seedInstance(t, s, newInstance(inst, "inst1", 8081, 21001))
	tok := seedToken(t, s, "01JTOKEN00000000000000000A", "hash-a", model.ScopeGlobal)

	add := func(d TokenUsageDelta) {
		t.Helper()
		mustWrite(t, s, func(ctx context.Context, tx Tx) error {
			return s.AddTokenUsage(ctx, tx, d)
		})
	}
	read := func() TokenUsageRow {
		t.Helper()
		rows, err := s.TokenUsage(ctx, s.RO, tok.ID, UsageRange{})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 {
			t.Fatalf("rows = %d, want 1", len(rows))
		}
		return rows[0]
	}

	base := TokenUsageDelta{TokenID: tok.ID, InstanceID: inst, Day: "2026-03-01"}

	// A first flush with nothing reported: both counts stay NULL, which is what
	// lets the UI say "not reported" rather than showing a zero.
	first := base
	first.Requests, first.BytesOut = 1, 10
	add(first)
	if got := read(); got.PromptTokens != nil || got.CompletionTokens != nil {
		t.Fatalf("unreported counts became %v/%v, want NULL/NULL",
			got.PromptTokens, got.CompletionTokens)
	}

	// A flush that DID report starts the sum from zero.
	second := base
	second.Requests, second.BytesOut = 1, 20
	second.PromptTokens, second.CompletionTokens = ptr(int64(30)), ptr(int64(4))
	add(second)
	got := read()
	if got.PromptTokens == nil || *got.PromptTokens != 30 {
		t.Errorf("prompt_tokens = %v, want 30", got.PromptTokens)
	}
	if got.CompletionTokens == nil || *got.CompletionTokens != 4 {
		t.Errorf("completion_tokens = %v, want 4", got.CompletionTokens)
	}

	// A later unreported flush must not erase them.
	third := base
	third.Requests, third.BytesOut = 1, 5
	add(third)
	got = read()
	if got.PromptTokens == nil || *got.PromptTokens != 30 {
		t.Errorf("prompt_tokens = %v after an unreported flush, want it untouched at 30",
			got.PromptTokens)
	}
	if got.Requests != 3 || got.BytesOut != 35 {
		t.Errorf("requests/bytes_out = %d/%d, want 3/35", got.Requests, got.BytesOut)
	}

	// And a second reported flush adds to the running total.
	fourth := base
	fourth.PromptTokens = ptr(int64(2))
	add(fourth)
	if got := read(); got.PromptTokens == nil || *got.PromptTokens != 32 {
		t.Errorf("prompt_tokens = %v, want 32", got.PromptTokens)
	}
}

// TestGatewayDenialsAccumulatePerReason is §2.9's third counter, and the source
// of "unauthorized attempts on port 8081".
func TestGatewayDenialsAccumulatePerReason(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	ctx := context.Background()
	const a, b = "01JINST0000000000000000001", "01JINST0000000000000000002"
	seedInstance(t, s, newInstance(a, "inst1", 8081, 21001))
	seedInstance(t, s, newInstance(b, "inst2", 8082, 21002))

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		for _, d := range []DenialDelta{
			{InstanceID: a, Day: "2026-03-01", Reason: model.DeniedUnknown, Count: 4},
			{InstanceID: a, Day: "2026-03-01", Reason: model.DeniedUnknown, Count: 6},
			{InstanceID: a, Day: "2026-03-01", Reason: model.DeniedExpired, Count: 1},
			{InstanceID: b, Day: "2026-03-01", Reason: model.DeniedScope, Count: 2},
		} {
			if err := s.AddGatewayDenial(ctx, tx, d); err != nil {
				return err
			}
		}
		return nil
	})

	all, err := s.GatewayDenials(ctx, s.RO, "", UsageRange{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("rows = %d, want 3", len(all))
	}

	byKey := map[string]int64{}
	for _, r := range all {
		byKey[r.InstanceID+"/"+string(r.Reason)] = r.Count
	}
	if byKey[a+"/unknown"] != 10 {
		t.Errorf("unknown denials on %s = %d, want 10", a, byKey[a+"/unknown"])
	}
	if byKey[a+"/expired"] != 1 || byKey[b+"/scope"] != 2 {
		t.Errorf("counters = %v", byKey)
	}

	one, err := s.GatewayDenials(ctx, s.RO, b, UsageRange{})
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 1 || one[0].InstanceID != b {
		t.Errorf("the instance filter returned %v", one)
	}
}

// TestUsageTablesCascadeWithAPurgedInstance: `?purge=true` is the explicit hard
// delete, and section 3.10c says it cascades the accounting away. That is the
// one thing in this system that cannot be recomputed, which is why the UI puts
// it behind a second confirmation — and why the cascade is worth asserting.
func TestUsageTablesCascadeWithAPurgedInstance(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	ctx := context.Background()
	const inst = "01JINST0000000000000000001"
	seedInstance(t, s, newInstance(inst, "inst1", 8081, 21001))
	tok := seedToken(t, s, "01JTOKEN00000000000000000A", "hash-a", model.ScopeInstances)

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		if err := s.SetTokenInstances(ctx, tx, tok.ID, []string{inst}); err != nil {
			return err
		}
		if err := s.AddInstanceUsage(ctx, tx, InstanceUsageDelta{
			InstanceID: inst, Day: "2026-03-01", AuthMode: model.AuthToken, Requests: 5,
		}); err != nil {
			return err
		}
		if err := s.AddTokenUsage(ctx, tx, TokenUsageDelta{
			TokenID: tok.ID, InstanceID: inst, Day: "2026-03-01", Requests: 5,
		}); err != nil {
			return err
		}
		return s.AddGatewayDenial(ctx, tx, DenialDelta{
			InstanceID: inst, Day: "2026-03-01", Reason: model.DeniedUnknown, Count: 2,
		})
	})

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		_, err := s.PurgeInstance(ctx, tx, inst)
		return err
	})

	if rows, err := s.InstanceUsage(ctx, s.RO, inst, UsageRange{}); err != nil || len(rows) != 0 {
		t.Errorf("instance_usage_daily = %v (err %v), want empty", rows, err)
	}
	if rows, err := s.TokenUsage(ctx, s.RO, tok.ID, UsageRange{}); err != nil || len(rows) != 0 {
		t.Errorf("token_usage_daily = %v (err %v), want empty", rows, err)
	}
	if rows, err := s.GatewayDenials(ctx, s.RO, "", UsageRange{}); err != nil || len(rows) != 0 {
		t.Errorf("gateway_denials_daily = %v (err %v), want empty", rows, err)
	}
	// The token itself survives: purging one instance must not revoke a global
	// credential that also served others.
	if _, err := s.APIToken(ctx, s.RO, tok.ID); err != nil {
		t.Errorf("the token was deleted with the instance: %v", err)
	}
}
