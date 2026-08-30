package store

import (
	"context"
	"fmt"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// Gateway accounting: `instance_usage_daily`, `token_usage_daily` and
// `gateway_denials_daily` (DESIGN sections 2.9 and 9.3, D56).
//
// Every method here is an ADDITIVE upsert, because the gateway keeps its
// counters in memory and flushes deltas every 5 s and on shutdown (§9.3). Two
// consequences are deliberate:
//
//   - A flush is idempotent in the only sense that matters — it adds what it
//     carries — so a retried flush double-counts and a dropped one under-counts.
//     The gateway therefore clears its map only after the write commits.
//   - D56's order is instance-FIRST. `instance_usage_daily` is written for every
//     proxied request including `auth_mode='none'`, and `token_usage_daily` is
//     the per-credential breakdown written ADDITIONALLY. Writing only the second
//     would leave a no-auth instance — an explicit SPEC §3.4 feature — with no
//     requests, no bytes and no errors anywhere in the database.
//
// `prompt_tokens` and `completion_tokens` are the one pair that is not a plain
// sum. NULL means "the upstream did not report it" (§9.3's tail tap abstains
// rather than guesses), and a NULL delta must therefore leave the stored value
// alone instead of coercing it to zero — otherwise one unreported request would
// erase a day of reported ones.

// InstanceUsageDelta is one flush of the instance-first counters (§9.3, D56).
type InstanceUsageDelta struct {
	InstanceID string
	// Day is 'YYYY-MM-DD' UTC. The caller owns the clock, so a test can flush
	// two days without waiting for one.
	Day      string
	AuthMode model.AuthMode

	Requests   int64
	Errors     int64
	BytesIn    int64
	BytesOut   int64
	DurationMS int64
}

// AddInstanceUsage adds one delta to `instance_usage_daily`.
func (s *Store) AddInstanceUsage(ctx context.Context, tx Tx, d InstanceUsageDelta) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO instance_usage_daily
		   (instance_id, day, auth_mode, requests, errors, bytes_in, bytes_out, duration_ms)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(instance_id, day, auth_mode) DO UPDATE SET
		   requests    = requests + excluded.requests,
		   errors      = errors + excluded.errors,
		   bytes_in    = bytes_in + excluded.bytes_in,
		   bytes_out   = bytes_out + excluded.bytes_out,
		   duration_ms = duration_ms + excluded.duration_ms`,
		d.InstanceID, d.Day, string(d.AuthMode),
		d.Requests, d.Errors, d.BytesIn, d.BytesOut, d.DurationMS)
	if err != nil {
		return fmt.Errorf("add instance usage: %w", err)
	}
	return nil
}

// TokenUsageDelta is one flush of the per-credential breakdown (§9.3, D56).
type TokenUsageDelta struct {
	TokenID    string
	InstanceID string
	Day        string

	Requests int64
	Errors   int64
	BytesIn  int64
	BytesOut int64
	// PromptTokens and CompletionTokens are nil when the upstream reported
	// nothing this flush. Nil LEAVES THE COLUMN ALONE; it does not add zero and
	// it does not turn a NULL into a 0, because "not reported" and "zero tokens"
	// are different sentences on screen (§9.3, F14).
	PromptTokens     *int64
	CompletionTokens *int64
	DurationMS       int64
}

// AddTokenUsage adds one delta to `token_usage_daily`.
func (s *Store) AddTokenUsage(ctx context.Context, tx Tx, d TokenUsageDelta) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO token_usage_daily
		   (token_id, instance_id, day, requests, errors, bytes_in, bytes_out,
		    prompt_tokens, completion_tokens, duration_ms)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(token_id, instance_id, day) DO UPDATE SET
		   requests  = requests + excluded.requests,
		   errors    = errors + excluded.errors,
		   bytes_in  = bytes_in + excluded.bytes_in,
		   bytes_out = bytes_out + excluded.bytes_out,
		   prompt_tokens = CASE WHEN excluded.prompt_tokens IS NULL
		                        THEN prompt_tokens
		                        ELSE COALESCE(prompt_tokens, 0) + excluded.prompt_tokens END,
		   completion_tokens = CASE WHEN excluded.completion_tokens IS NULL
		                        THEN completion_tokens
		                        ELSE COALESCE(completion_tokens, 0) + excluded.completion_tokens END,
		   duration_ms = duration_ms + excluded.duration_ms`,
		d.TokenID, d.InstanceID, d.Day, d.Requests, d.Errors, d.BytesIn, d.BytesOut,
		d.PromptTokens, d.CompletionTokens, d.DurationMS)
	if err != nil {
		return fmt.Errorf("add token usage: %w", err)
	}
	return nil
}

// DenialDelta is one flush of `gateway_denials_daily` (§2.9): why the gateway
// turned a request away, per instance and day.
type DenialDelta struct {
	InstanceID string
	Day        string
	Reason     model.DenialReason
	Count      int64
}

// AddGatewayDenial adds one delta to `gateway_denials_daily`.
//
// A denied request is counted HERE and nowhere else: `instance_usage_daily`
// counts every PROXIED request (D56), and a request that never reached the
// upstream was not one of them. Conflating the two would make an instance under
// a credential-stuffing attempt look busy.
func (s *Store) AddGatewayDenial(ctx context.Context, tx Tx, d DenialDelta) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO gateway_denials_daily (instance_id, day, reason, count)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(instance_id, day, reason) DO UPDATE SET count = count + excluded.count`,
		d.InstanceID, d.Day, string(d.Reason), d.Count)
	if err != nil {
		return fmt.Errorf("add gateway denial: %w", err)
	}
	return nil
}

// UsageRange bounds a usage read. Both ends are inclusive 'YYYY-MM-DD' strings
// and an empty one is unbounded, which is what `?from=&to=` sends when the UI
// asks for everything.
type UsageRange struct {
	From string
	To   string
}

func (r UsageRange) where(column string, args *[]any) string {
	clause := ""
	if r.From != "" {
		clause += " AND " + column + " >= ?"
		*args = append(*args, r.From)
	}
	if r.To != "" {
		clause += " AND " + column + " <= ?"
		*args = append(*args, r.To)
	}
	return clause
}

// InstanceUsageRow is one row of `instance_usage_daily`.
type InstanceUsageRow struct {
	InstanceID string
	Day        string
	AuthMode   model.AuthMode

	Requests   int64
	Errors     int64
	BytesIn    int64
	BytesOut   int64
	DurationMS int64
}

// InstanceUsage reads the gateway's own counters for one instance, oldest day
// first. An empty instance id reads every instance, which is what the dashboard
// summary wants.
func (s *Store) InstanceUsage(ctx context.Context, tx Tx, instanceID string,
	rng UsageRange) ([]InstanceUsageRow, error) {

	args := []any{}
	query := `SELECT instance_id, day, auth_mode, requests, errors, bytes_in, bytes_out, duration_ms
	            FROM instance_usage_daily WHERE 1 = 1`
	if instanceID != "" {
		query += ` AND instance_id = ?`
		args = append(args, instanceID)
	}
	query += rng.where("day", &args) + ` ORDER BY day, instance_id, auth_mode`

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("select instance usage: %w", err)
	}
	defer rows.Close()

	var out []InstanceUsageRow
	for rows.Next() {
		var (
			v    InstanceUsageRow
			mode string
		)
		if err := rows.Scan(&v.InstanceID, &v.Day, &mode, &v.Requests, &v.Errors,
			&v.BytesIn, &v.BytesOut, &v.DurationMS); err != nil {
			return nil, fmt.Errorf("scan instance usage: %w", err)
		}
		v.AuthMode = model.AuthMode(mode)
		out = append(out, v)
	}
	return out, rows.Err()
}

// TokenUsageRow is one row of `token_usage_daily`. The two token counts stay
// nullable all the way to the wire: the UI says "not reported" rather than 0.
type TokenUsageRow struct {
	TokenID    string
	InstanceID string
	Day        string

	Requests         int64
	Errors           int64
	BytesIn          int64
	BytesOut         int64
	PromptTokens     *int64
	CompletionTokens *int64
	DurationMS       int64
}

// TokenUsage reads the per-credential breakdown for one token, oldest day first.
func (s *Store) TokenUsage(ctx context.Context, tx Tx, tokenID string,
	rng UsageRange) ([]TokenUsageRow, error) {

	args := []any{tokenID}
	query := `SELECT token_id, instance_id, day, requests, errors, bytes_in, bytes_out,
	                 prompt_tokens, completion_tokens, duration_ms
	            FROM token_usage_daily WHERE token_id = ?`
	query += rng.where("day", &args) + ` ORDER BY day, instance_id`

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("select token usage: %w", err)
	}
	defer rows.Close()

	var out []TokenUsageRow
	for rows.Next() {
		var v TokenUsageRow
		if err := rows.Scan(&v.TokenID, &v.InstanceID, &v.Day, &v.Requests, &v.Errors,
			&v.BytesIn, &v.BytesOut, &v.PromptTokens, &v.CompletionTokens,
			&v.DurationMS); err != nil {
			return nil, fmt.Errorf("scan token usage: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// DenialRow is one row of `gateway_denials_daily`.
type DenialRow struct {
	InstanceID string
	Day        string
	Reason     model.DenialReason
	Count      int64
}

// GatewayDenials reads the denial counters `GET /api/v1/gateway/denials`
// answers with, oldest day first.
func (s *Store) GatewayDenials(ctx context.Context, tx Tx, instanceID string,
	rng UsageRange) ([]DenialRow, error) {

	args := []any{}
	query := `SELECT instance_id, day, reason, count FROM gateway_denials_daily WHERE 1 = 1`
	if instanceID != "" {
		query += ` AND instance_id = ?`
		args = append(args, instanceID)
	}
	query += rng.where("day", &args) + ` ORDER BY day, instance_id, reason`

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("select gateway denials: %w", err)
	}
	defer rows.Close()

	var out []DenialRow
	for rows.Next() {
		var (
			v      DenialRow
			reason string
		)
		if err := rows.Scan(&v.InstanceID, &v.Day, &reason, &v.Count); err != nil {
			return nil, fmt.Errorf("scan gateway denial: %w", err)
		}
		v.Reason = model.DenialReason(reason)
		out = append(out, v)
	}
	return out, rows.Err()
}
