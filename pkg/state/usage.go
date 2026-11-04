package state

import (
	"database/sql"
	"errors"
)

// Token and cost report of one API call or one auxiliary task
type Usage struct {
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ReasoningTokens  int64
	EstimatedCostUSD float64
	// Negative when the provider reported no invoice
	ActualCostUSD   float64
	HasActualCost   bool
	CostStatus      string
	CostSource      string
	PricingVersion  string
	Model           string
	BillingProvider string
	BillingBaseURL  string
	BillingMode     string
	APICalls        int
	// Auxiliary task name, empty for the main loop
	Task string
}

// Adds a usage report to the session totals and the per model attribution rows
func (d *DB) UpdateTokenCounts(sessionID string, u Usage) error {
	return d.retryWrite(func(tx *Tx) error {
		var actual any
		if u.HasActualCost {
			actual = u.ActualCostUSD
		}
		if u.Task == "" {
			if _, err := tx.Exec(`UPDATE sessions SET
				input_tokens = input_tokens + ?, output_tokens = output_tokens + ?,
				cache_read_tokens = cache_read_tokens + ?, cache_write_tokens = cache_write_tokens + ?,
				reasoning_tokens = reasoning_tokens + ?,
				estimated_cost_usd = COALESCE(estimated_cost_usd, 0) + ?,
				actual_cost_usd = CASE WHEN ? IS NULL THEN actual_cost_usd ELSE COALESCE(actual_cost_usd, 0) + ? END,
				cost_status = COALESCE(?, cost_status), cost_source = COALESCE(?, cost_source),
				pricing_version = COALESCE(?, pricing_version),
				billing_provider = COALESCE(billing_provider, ?), billing_base_url = COALESCE(billing_base_url, ?),
				billing_mode = COALESCE(billing_mode, ?), model = COALESCE(model, ?),
				api_call_count = COALESCE(api_call_count, 0) + ?
				WHERE id = ?`,
				u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens, u.ReasoningTokens,
				u.EstimatedCostUSD, actual, u.ActualCostUSD, nullStr(u.CostStatus), nullStr(u.CostSource),
				nullStr(u.PricingVersion), nullStr(u.BillingProvider), nullStr(u.BillingBaseURL), nullStr(u.BillingMode),
				nullStr(u.Model), u.APICalls, sessionID); err != nil {
				return err
			}
		}
		if u.Model == "" {
			return nil
		}
		_, err := tx.Exec(`INSERT INTO session_model_usage (session_id, model, billing_provider, billing_base_url, billing_mode, task,
			api_call_count, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens,
			estimated_cost_usd, actual_cost_usd, cost_status, cost_source, first_seen, last_seen)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(session_id, model, billing_provider, billing_base_url, billing_mode, task) DO UPDATE SET
				api_call_count = api_call_count + excluded.api_call_count,
				input_tokens = input_tokens + excluded.input_tokens,
				output_tokens = output_tokens + excluded.output_tokens,
				cache_read_tokens = cache_read_tokens + excluded.cache_read_tokens,
				cache_write_tokens = cache_write_tokens + excluded.cache_write_tokens,
				reasoning_tokens = reasoning_tokens + excluded.reasoning_tokens,
				estimated_cost_usd = estimated_cost_usd + excluded.estimated_cost_usd,
				actual_cost_usd = actual_cost_usd + excluded.actual_cost_usd,
				cost_status = COALESCE(excluded.cost_status, cost_status),
				cost_source = COALESCE(excluded.cost_source, cost_source),
				last_seen = excluded.last_seen`,
			sessionID, u.Model, u.BillingProvider, u.BillingBaseURL, u.BillingMode, u.Task, u.APICalls,
			u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens, u.ReasoningTokens,
			u.EstimatedCostUSD, u.ActualCostUSD, nullStr(u.CostStatus), nullStr(u.CostSource), now(), now())
		return err
	}, WritePatience)
}

// Sets the billing route of a session outright
func (d *DB) UpdateBillingRoute(sessionID, provider, baseURL, mode string) error {
	return d.write(func(tx *Tx) error {
		_, err := tx.Exec("UPDATE sessions SET billing_provider = ?, billing_base_url = ?, billing_mode = COALESCE(?, billing_mode) WHERE id = ?",
			provider, baseURL, nullStr(mode), sessionID)
		return err
	})
}

// Attribution row of one model and route within a session
type ModelUsage struct {
	Model            string
	BillingProvider  string
	BillingBaseURL   string
	BillingMode      string
	Task             string
	APICalls         int
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ReasoningTokens  int64
	EstimatedCostUSD float64
	ActualCostUSD    float64
	CostStatus       string
	CostSource       string
}

// Attribution rows of a session, main loop and auxiliary tasks alike
func (d *DB) ModelUsage(sessionID string) ([]ModelUsage, error) {
	var out []ModelUsage
	err := d.read(func(q querier) error {
		rows, err := q.Query(`SELECT model, billing_provider, billing_base_url, billing_mode, task, api_call_count, input_tokens,
			output_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens, estimated_cost_usd, actual_cost_usd,
			cost_status, cost_source FROM session_model_usage WHERE session_id = ? ORDER BY first_seen`, sessionID)
		if err != nil {
			return err
		}
		defer rows.Close()
		out = nil
		for rows.Next() {
			var u ModelUsage
			var status, source sql.NullString
			if err := rows.Scan(&u.Model, &u.BillingProvider, &u.BillingBaseURL, &u.BillingMode, &u.Task, &u.APICalls,
				&u.InputTokens, &u.OutputTokens, &u.CacheReadTokens, &u.CacheWriteTokens, &u.ReasoningTokens,
				&u.EstimatedCostUSD, &u.ActualCostUSD, &status, &source); err != nil {
				return err
			}
			u.CostStatus, u.CostSource = status.String, source.String
			out = append(out, u)
		}
		return rows.Err()
	})
	return out, err
}

// Totals across every session
type Totals struct {
	Sessions         int
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ReasoningTokens  int64
	EstimatedCostUSD float64
	ActualCostUSD    float64
	APICalls         int64
}

// Totals over sessions with at least one message
func (d *DB) UsageTotals(includeArchived bool) (Totals, error) {
	var t Totals
	where := "message_count >= 1"
	if !includeArchived {
		where += " AND archived = 0"
	}
	err := d.read(func(q querier) error {
		return q.QueryRow(`SELECT COUNT(*), COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0),
			COALESCE(SUM(cache_read_tokens), 0), COALESCE(SUM(cache_write_tokens), 0), COALESCE(SUM(reasoning_tokens), 0),
			COALESCE(SUM(estimated_cost_usd), 0), COALESCE(SUM(actual_cost_usd), 0), COALESCE(SUM(api_call_count), 0)
			FROM sessions WHERE `+where).Scan(&t.Sessions, &t.InputTokens, &t.OutputTokens, &t.CacheReadTokens,
			&t.CacheWriteTokens, &t.ReasoningTokens, &t.EstimatedCostUSD, &t.ActualCostUSD, &t.APICalls)
	})
	return t, err
}

// Totals per model across every session
func (d *DB) UsageByModel() ([]ModelUsage, error) {
	var out []ModelUsage
	err := d.read(func(q querier) error {
		rows, err := q.Query(`SELECT model, billing_provider, SUM(api_call_count), SUM(input_tokens), SUM(output_tokens),
			SUM(cache_read_tokens), SUM(cache_write_tokens), SUM(reasoning_tokens), SUM(estimated_cost_usd), SUM(actual_cost_usd)
			FROM session_model_usage GROUP BY model, billing_provider ORDER BY SUM(estimated_cost_usd) DESC`)
		if err != nil {
			return err
		}
		defer rows.Close()
		out = nil
		for rows.Next() {
			var u ModelUsage
			if err := rows.Scan(&u.Model, &u.BillingProvider, &u.APICalls, &u.InputTokens, &u.OutputTokens, &u.CacheReadTokens,
				&u.CacheWriteTokens, &u.ReasoningTokens, &u.EstimatedCostUSD, &u.ActualCostUSD); err != nil {
				return err
			}
			out = append(out, u)
		}
		return rows.Err()
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return out, err
}
