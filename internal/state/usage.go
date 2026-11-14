package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

const (
	tokenWriterIdle       = 30 * time.Second
	tokenFlushTimeout     = 5 * time.Second
	tokenWriterJoinBudget = 10 * time.Second
)

type TokenUsage struct {
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ReasoningTokens  int64
	APICallCount     int64
	Model            *string
	EstimatedCostUSD *float64
	ActualCostUSD    *float64
	CostStatus       *string
	CostSource       *string
	PricingVersion   *string
	BillingProvider  *string
	BillingBaseURL   *string
	BillingMode      *string
	Absolute         bool
	Source           *string
	Task             string
}

type queuedUsage struct {
	sessionID string
	usage     TokenUsage
}

type tokenWriter struct {
	mu      sync.Mutex
	queue   []queuedUsage
	alive   bool
	stop    bool
	busy    bool
	changed chan struct{}
	exited  chan struct{}
}

func tokenUpdateSQL(delta bool) string {
	add := func(col string) string {
		if delta {
			return col + " + ?"
		}
		return "?"
	}
	add0 := func(col string) string {
		if delta {
			return "COALESCE(" + col + ", 0) + ?"
		}
		return "?"
	}
	var counters strings.Builder
	for _, c := range []string{"input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens", "reasoning_tokens"} {
		counters.WriteString("                   " + c + " = " + add(c) + ",\n")
	}
	estimated := "COALESCE(?, 0)"
	if delta {
		estimated = "COALESCE(estimated_cost_usd, 0) + COALESCE(?, 0)"
	}
	return "UPDATE sessions SET\n" + counters.String() +
		"                   estimated_cost_usd = " + estimated + ",\n" +
		"                   actual_cost_usd = CASE\n" +
		"                       WHEN ? IS NULL THEN actual_cost_usd\n" +
		"                       ELSE " + add0("actual_cost_usd") + "\n" +
		"                   END,\n" +
		"                   cost_status = COALESCE(?, cost_status),\n" +
		"                   cost_source = COALESCE(?, cost_source),\n" +
		"                   pricing_version = COALESCE(?, pricing_version),\n" +
		"                   billing_provider = COALESCE(billing_provider, ?),\n" +
		"                   billing_base_url = COALESCE(billing_base_url, ?),\n" +
		"                   billing_mode = COALESCE(billing_mode, ?),\n" +
		"                   model = COALESCE(model, ?),\n" +
		"                   api_call_count = " + add0("api_call_count") + "\n" +
		"                   WHERE id = ?"
}

var (
	tokenUpdateAbsoluteSQL = tokenUpdateSQL(false)
	tokenUpdateDeltaSQL    = tokenUpdateSQL(true)
)

const modelUsageUpsertSQL = `INSERT INTO session_model_usage (
                   session_id, model, billing_provider, billing_base_url, billing_mode,
                   task, api_call_count, input_tokens, output_tokens,
                   cache_read_tokens, cache_write_tokens, reasoning_tokens,
                   estimated_cost_usd, actual_cost_usd, cost_status, cost_source,
                   first_seen, last_seen
               ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
               ON CONFLICT(session_id, model, billing_provider, billing_base_url, billing_mode, task)
               DO UPDATE SET
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
                   last_seen = excluded.last_seen`

func (s *Store) tokens() *tokenWriter { return &s.tables.state.tokens }

func (w *tokenWriter) broadcastLocked() {
	if w.changed != nil {
		close(w.changed)
	}
	w.changed = make(chan struct{})
}

func (w *tokenWriter) waitLocked(deadline time.Time) bool {
	if w.changed == nil {
		w.changed = make(chan struct{})
	}
	ch := w.changed
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return false
	}
	w.mu.Unlock()
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-ch:
		w.mu.Lock()
		return true
	case <-timer.C:
		w.mu.Lock()
		return false
	}
}

func textArg(v *string) any {
	if v == nil {
		return nil
	}
	return *v
}

func floatArg(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}

func (s *Store) UpdateSessionBillingRoute(ctx context.Context, sessionID, provider, baseURL string, billingMode *string) error {
	s.flushTokenCounts(ctx, tokenFlushTimeout)
	return s.WriteSQL(ctx, WritePatience, `UPDATE sessions SET
                   billing_provider = ?,
                   billing_base_url = ?,
                   billing_mode = COALESCE(?, billing_mode)
                   WHERE id = ?`, provider, baseURL, textArg(billingMode), sessionID)
}

func (s *Store) QueueTokenCounts(ctx context.Context, sessionID string, u TokenUsage) error {
	w := s.tokens()
	w.mu.Lock()
	stopped := w.stop && !w.alive
	if !stopped {
		w.queue = append(w.queue, queuedUsage{sessionID, u})
		if !w.alive {
			w.alive = true
			w.exited = make(chan struct{})
			go s.tokenWriterLoop(w.exited)
		}
		w.broadcastLocked()
	}
	w.mu.Unlock()
	if stopped {
		return s.UpdateTokenCounts(ctx, sessionID, u)
	}
	return nil
}

func (s *Store) applyClaimedBatch(batch []queuedUsage) {
	defer func() {
		w := s.tokens()
		w.mu.Lock()
		w.busy = false
		w.broadcastLocked()
		w.mu.Unlock()
	}()
	s.applyTokenBatch(batch)
}

func (s *Store) flushQueuedTokenDeltas(ctx context.Context, forRead bool) bool {
	if forRead {
		return s.flushTokenCounts(ctx, 0)
	}
	return s.flushTokenCounts(ctx, tokenFlushTimeout)
}

func (s *Store) FlushTokenCounts(ctx context.Context) bool {
	return s.flushTokenCounts(ctx, tokenFlushTimeout)
}

func (s *Store) flushTokenCounts(ctx context.Context, timeout time.Duration) bool {
	w := s.tokens()
	w.mu.Lock()
	if len(w.queue) == 0 && !w.busy {
		w.mu.Unlock()
		return true
	}
	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	var batch []queuedUsage
	for len(w.queue) > 0 || w.busy {
		if !w.alive && !w.busy {
			w.busy = true
			batch = w.queue
			w.queue = nil
			break
		}
		if !w.waitLocked(deadline) {
			w.mu.Unlock()
			return false
		}
	}
	w.mu.Unlock()
	if len(batch) > 0 {
		s.applyClaimedBatch(batch)
	}
	return true
}

func (s *Store) tokenWriterLoop(exited chan struct{}) {
	w := s.tokens()
	for {
		w.mu.Lock()
		idle := time.Now().Add(tokenWriterIdle)
		for len(w.queue) == 0 && !w.stop {
			if !w.waitLocked(idle) {
				break
			}
		}
		if len(w.queue) == 0 {
			w.alive = false
			close(exited)
			w.broadcastLocked()
			w.mu.Unlock()
			return
		}
		w.busy = true
		batch := w.queue
		w.queue = nil
		w.mu.Unlock()
		s.applyClaimedBatch(batch)
	}
}

func (s *Store) applyTokenBatch(batch []queuedUsage) {
	ctx := context.Background()
	for _, q := range coalesceTokenDeltas(batch) {
		if err := s.UpdateTokenCounts(ctx, q.sessionID, q.usage); err != nil {
			slog.Warn(fmt.Sprintf("async token accounting: apply failed (session=%s): %s", q.sessionID, errText(err)))
		}
	}
}

func sameText(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func sameRoute(a, b queuedUsage) bool {
	x, y := a.usage, b.usage
	return a.sessionID == b.sessionID && sameText(x.Model, y.Model) && sameText(x.CostStatus, y.CostStatus) &&
		sameText(x.CostSource, y.CostSource) && sameText(x.PricingVersion, y.PricingVersion) &&
		sameText(x.BillingProvider, y.BillingProvider) && sameText(x.BillingBaseURL, y.BillingBaseURL) &&
		sameText(x.BillingMode, y.BillingMode) && sameText(x.Source, y.Source) && x.Task == y.Task
}

func addCost(merged, value *float64) *float64 {
	if value == nil {
		return merged
	}
	total := *value
	if merged != nil {
		total += *merged
	}
	return &total
}

func coalesceTokenDeltas(batch []queuedUsage) []queuedUsage {
	var groups []queuedUsage
	mergeable := []bool{}
	for _, q := range batch {
		n := len(groups)
		if n > 0 && !q.usage.Absolute && mergeable[n-1] && sameRoute(groups[n-1], q) {
			m := &groups[n-1].usage
			m.InputTokens += q.usage.InputTokens
			m.OutputTokens += q.usage.OutputTokens
			m.CacheReadTokens += q.usage.CacheReadTokens
			m.CacheWriteTokens += q.usage.CacheWriteTokens
			m.ReasoningTokens += q.usage.ReasoningTokens
			m.APICallCount += q.usage.APICallCount
			m.EstimatedCostUSD = addCost(m.EstimatedCostUSD, q.usage.EstimatedCostUSD)
			m.ActualCostUSD = addCost(m.ActualCostUSD, q.usage.ActualCostUSD)
			continue
		}
		groups = append(groups, q)
		mergeable = append(mergeable, !q.usage.Absolute)
	}
	return groups
}

func stopTokenWriter(ctx context.Context, s *Store) {
	w := s.tokens()
	w.mu.Lock()
	w.stop = true
	w.broadcastLocked()
	alive, exited := w.alive, w.exited
	w.mu.Unlock()
	if alive && exited != nil {
		timer := time.NewTimer(tokenWriterJoinBudget)
		select {
		case <-exited:
			timer.Stop()
		case <-timer.C:
			w.mu.Lock()
			pending := len(w.queue)
			w.mu.Unlock()
			slog.Warn(fmt.Sprintf("async token accounting: writer did not stop within %.0fs; %d queued delta(s) not persisted",
				tokenWriterJoinBudget.Seconds(), pending))
			return
		}
	}
	w.mu.Lock()
	deadline := time.Now().Add(tokenWriterJoinBudget)
	for w.busy {
		if !w.waitLocked(deadline) {
			pending := len(w.queue)
			w.mu.Unlock()
			slog.Warn(fmt.Sprintf("async token accounting: concurrent drain did not finish within %.0fs; %d queued delta(s) not persisted",
				tokenWriterJoinBudget.Seconds(), pending))
			return
		}
	}
	batch := w.queue
	if len(batch) > 0 {
		w.busy = true
		w.queue = nil
	}
	w.mu.Unlock()
	if len(batch) > 0 {
		s.applyClaimedBatch(batch)
	}
}

func (s *Store) UpdateTokenCounts(ctx context.Context, sessionID string, u TokenUsage) error {
	source := "unknown"
	if u.Source != nil && *u.Source != "" {
		source = *u.Source
	}
	if err := s.Write(ctx, TranscriptWritePatience, func(c Conn) error {
		return s.InsertSessionRowIn(ctx, c, NewSession{ID: sessionID, Source: source, Model: u.Model})
	}); err != nil {
		return err
	}
	return s.Write(ctx, WritePatience, func(c Conn) error { return updateTokenCountsIn(ctx, c, sessionID, u) })
}

func nonEmpty(v *string) bool { return v != nil && *v != "" }

func updateTokenCountsIn(ctx context.Context, c Conn, sessionID string, u TokenUsage) error {
	query := tokenUpdateDeltaSQL
	if u.Absolute {
		query = tokenUpdateAbsoluteSQL
	}
	hasUsage := u.InputTokens != 0 || u.OutputTokens != 0 || u.CacheReadTokens != 0 || u.CacheWriteTokens != 0 ||
		u.ReasoningTokens != 0 || u.APICallCount != 0 || (u.EstimatedCostUSD != nil && *u.EstimatedCostUSD != 0)
	accounted := hasUsage || (u.ActualCostUSD != nil && *u.ActualCostUSD != 0)
	routed := func(v *string) any {
		if !accounted {
			return nil
		}
		return textArg(v)
	}
	var model, provider sql.NullString
	var calls sql.NullInt64
	err := c.QueryRowContext(ctx, "SELECT model, billing_provider, api_call_count FROM sessions WHERE id = ?", sessionID).Scan(&model, &provider, &calls)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	firstRoute := u.Task == "" && calls.Int64 == 0 && accounted && nonEmpty(u.Model) && nonEmpty(u.BillingProvider) &&
		(!model.Valid || model.String != *u.Model || !provider.Valid || provider.String != *u.BillingProvider)
	if firstRoute {
		if _, err := c.ExecContext(ctx, `UPDATE sessions
                   SET model = ?, billing_provider = ?,
                   billing_base_url = ?, billing_mode = ?
                   WHERE id = ?`, *u.Model, *u.BillingProvider, textArg(u.BillingBaseURL), textArg(u.BillingMode), sessionID); err != nil {
			return err
		}
	}
	if _, err := c.ExecContext(ctx, query, u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens, u.ReasoningTokens,
		floatArg(u.EstimatedCostUSD), floatArg(u.ActualCostUSD), floatArg(u.ActualCostUSD), textArg(u.CostStatus), textArg(u.CostSource),
		textArg(u.PricingVersion), routed(u.BillingProvider), routed(u.BillingBaseURL), routed(u.BillingMode), routed(u.Model),
		u.APICallCount, sessionID); err != nil {
		return err
	}
	if !u.Absolute && hasUsage {
		return recordModelUsageIn(ctx, c, sessionID, u)
	}
	return nil
}

func orText(values ...any) any {
	for _, v := range values {
		switch t := v.(type) {
		case *string:
			if t != nil && *t != "" {
				return *t
			}
		case sql.NullString:
			if t.Valid && t.String != "" {
				return t.String
			}
		case string:
			return t
		}
	}
	return nil
}

func recordModelUsageIn(ctx context.Context, c Conn, sessionID string, u TokenUsage) error {
	var model, provider, baseURL, mode sql.NullString
	if u.Task == "" {
		err := c.QueryRowContext(ctx, "SELECT model, billing_provider, billing_base_url, billing_mode FROM sessions WHERE id = ?", sessionID).
			Scan(&model, &provider, &baseURL, &mode)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	estimated, actual := 0.0, 0.0
	if u.EstimatedCostUSD != nil {
		estimated = *u.EstimatedCostUSD
	}
	if u.ActualCostUSD != nil {
		actual = *u.ActualCostUSD
	}
	now := nowEpoch()
	_, err := c.ExecContext(ctx, modelUsageUpsertSQL, sessionID, orText(u.Model, model, "unknown"), orText(u.BillingProvider, provider, ""),
		orText(u.BillingBaseURL, baseURL, ""), orText(u.BillingMode, mode, ""), u.Task, u.APICallCount,
		u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens, u.ReasoningTokens,
		estimated, actual, textArg(u.CostStatus), textArg(u.CostSource), now, now)
	return err
}

func (s *Store) RecordAuxiliaryUsage(ctx context.Context, sessionID, task string, u TokenUsage) error {
	if sessionID == "" || task == "" {
		return nil
	}
	aux := TokenUsage{Model: u.Model, BillingProvider: u.BillingProvider, BillingBaseURL: u.BillingBaseURL,
		InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CacheReadTokens: u.CacheReadTokens,
		CacheWriteTokens: u.CacheWriteTokens, ReasoningTokens: u.ReasoningTokens, EstimatedCostUSD: u.EstimatedCostUSD,
		APICallCount: u.APICallCount, Task: task}
	return s.Write(ctx, TranscriptWritePatience, func(c Conn) error {
		var one int
		err := c.QueryRowContext(ctx, "SELECT 1 FROM sessions WHERE id=?", sessionID).Scan(&one)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if err := s.InsertSessionRowIn(ctx, c, NewSession{ID: sessionID, Source: "unknown"}); err != nil {
				return err
			}
		case err != nil:
			return err
		default:
			if own := s.OwnProfileName(); own != "" {
				if _, err := c.ExecContext(ctx, "UPDATE sessions SET profile_name=? WHERE id=? AND profile_name IS NULL", own, sessionID); err != nil {
					return err
				}
			}
		}
		return recordModelUsageIn(ctx, c, sessionID, aux)
	})
}

type AuxiliaryUsage struct {
	APICalls         int64
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ReasoningTokens  int64
	EstimatedCostUSD float64
}

func (s *Store) AuxiliaryUsageByTask(ctx context.Context, sessionID string) (map[string]AuxiliaryUsage, error) {
	out := map[string]AuxiliaryUsage{}
	if sessionID == "" {
		return out, nil
	}
	chain, err := s.sessionLineageRootToTip(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	for i := len(chain) - 1; i >= 0; i-- {
		branch, err := s.isExplicitBranchSession(ctx, chain[i])
		if err != nil {
			return nil, err
		}
		if branch {
			chain = chain[i:]
			break
		}
	}
	err = s.Read(ctx, func(c Conn) error {
		clear(out)
		rows, err := c.QueryContext(ctx, `SELECT task,
                       COALESCE(SUM(api_call_count), 0) AS api_calls,
                       COALESCE(SUM(input_tokens), 0) AS input_tokens,
                       COALESCE(SUM(output_tokens), 0) AS output_tokens,
                       COALESCE(SUM(cache_read_tokens), 0) AS cache_read_tokens,
                       COALESCE(SUM(cache_write_tokens), 0) AS cache_write_tokens,
                       COALESCE(SUM(reasoning_tokens), 0) AS reasoning_tokens,
                       COALESCE(SUM(estimated_cost_usd), 0) AS estimated_cost_usd
                  FROM session_model_usage
                 WHERE session_id IN (`+bindMarks(len(chain))+`) AND task != ''
                 GROUP BY task`, stringArgs(chain)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var task string
			var a AuxiliaryUsage
			if err := rows.Scan(&task, &a.APICalls, &a.InputTokens, &a.OutputTokens, &a.CacheReadTokens, &a.CacheWriteTokens,
				&a.ReasoningTokens, &a.EstimatedCostUSD); err != nil {
				return err
			}
			out[task] = a
		}
		return rows.Err()
	})
	return out, err
}

func (s *Store) UsageTotals(ctx context.Context, minMessageCount int64, includeArchived bool) (int64, float64, error) {
	where := "parent_session_id IS NULL AND message_count >= ?"
	if !includeArchived {
		where += " AND COALESCE(archived, 0) = 0"
	}
	var tokens sql.NullInt64
	var cost sql.NullFloat64
	err := s.Read(ctx, func(c Conn) error {
		return c.QueryRowContext(ctx, `
            SELECT COALESCE(SUM(COALESCE(input_tokens, 0) + COALESCE(output_tokens, 0)), 0),
                   COALESCE(SUM(COALESCE(actual_cost_usd, estimated_cost_usd, 0)), 0)
              FROM sessions
             WHERE `+where+`
            `, minMessageCount).Scan(&tokens, &cost)
	})
	return tokens.Int64, cost.Float64, err
}
