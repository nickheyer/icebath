package state

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"weak"

	"github.com/nickheyer/icebath/pkg/internal/pyctx"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

var tokenCounters = []string{"input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens", "reasoning_tokens"}

// Sessions UPDATE of one usage report, adding to the stored counters in delta mode and setting them otherwise
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
	for _, c := range tokenCounters {
		counters.WriteString("                   " + c + " = " + add(c) + ",\n")
	}
	estimated := "COALESCE(?, 0)"
	if delta {
		estimated = "COALESCE(estimated_cost_usd, 0) + COALESCE(?, 0)"
	}
	return "UPDATE sessions SET\n" + counters.String() + "                   estimated_cost_usd = " + estimated + `,
                   actual_cost_usd = CASE
                       WHEN ? IS NULL THEN actual_cost_usd
                       ELSE ` + add0("actual_cost_usd") + `
                   END,
                   cost_status = COALESCE(?, cost_status),
                   cost_source = COALESCE(?, cost_source),
                   pricing_version = COALESCE(?, pricing_version),
                   billing_provider = COALESCE(billing_provider, ?),
                   billing_base_url = COALESCE(billing_base_url, ?),
                   billing_mode = COALESCE(billing_mode, ?),
                   model = COALESCE(model, ?),
                   api_call_count = ` + add0("api_call_count") + `
                   WHERE id = ?`
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

// One update_token_counts call, nil pointers are the Python None keywords
type TokenCountDelta struct {
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ReasoningTokens  int64
	Model            *string
	EstimatedCostUSD *float64
	ActualCostUSD    *float64
	CostStatus       *string
	CostSource       *string
	PricingVersion   *string
	BillingProvider  *string
	BillingBaseURL   *string
	BillingMode      *string
	APICallCount     int
	Absolute         bool
	Source           *string
	Task             string
}

// Usage of one auxiliary call, a nil call count records one call
type AuxiliaryUsage struct {
	Model            *string
	BillingProvider  *string
	BillingBaseURL   *string
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ReasoningTokens  int64
	EstimatedCostUSD *float64
	APICallCount     *int
}

// Per route attribution keywords forwarded into the session_model_usage upsert
type modelUsage struct {
	model, billingProvider, billingBaseURL, billingMode *string
	inputTokens, outputTokens, cacheReadTokens          int64
	cacheWriteTokens, reasoningTokens                   int64
	estimatedCostUSD, actualCostUSD                     *float64
	costStatus, costSource                              *string
	apiCallCount                                        int
}

type queuedTokenDelta struct {
	sessionID string
	delta     TokenCountDelta
}

type tokenWriter struct{ done chan struct{} }

// Whether the writer goroutine has not returned yet
func (w *tokenWriter) alive() bool {
	if w == nil {
		return false
	}
	select {
	case <-w.done:
		return false
	default:
		return true
	}
}

// Queue, writer handle and busy protocol of the coalesced token writer, kept in sessionCore
type tokenWriterState struct {
	tokenMu         sync.Mutex
	tokenNotify     chan struct{}
	tokenQueue      []queuedTokenDelta
	tokenWriter     *tokenWriter
	tokenStop       bool
	tokenBusy       bool
	tokenAtexitHook *tokenExitHook
	updateHook      func(sessionID string, delta TokenCountDelta) error
}

// Wakes every waiter, called with the token lock held
func (s *tokenWriterState) tokenNotifyAll() {
	if s.tokenNotify != nil {
		close(s.tokenNotify)
	}
	s.tokenNotify = make(chan struct{})
}

// Releases the token lock until notified or the timeout passes, then retakes it
func (s *tokenWriterState) tokenWait(remaining float64) {
	if s.tokenNotify == nil {
		s.tokenNotify = make(chan struct{})
	}
	ch := s.tokenNotify
	s.tokenMu.Unlock()
	timer := time.NewTimer(secondsToDuration(remaining))
	select {
	case <-ch:
	case <-timer.C:
	}
	timer.Stop()
	s.tokenMu.Lock()
}

// Exit hook draining one store's token queue, holding the store weakly like the Python weakref
type tokenExitHook struct{ db weak.Pointer[SessionDB] }

var tokenExitHooks struct {
	mu    sync.Mutex
	hooks []*tokenExitHook
}

// Adds a hook to the process exit drain list
func registerTokenExitHook(h *tokenExitHook) {
	tokenExitHooks.mu.Lock()
	tokenExitHooks.hooks = append(tokenExitHooks.hooks, h)
	tokenExitHooks.mu.Unlock()
}

// Removes every registration of a hook like atexit.unregister
func unregisterTokenExitHook(h *tokenExitHook) {
	tokenExitHooks.mu.Lock()
	defer tokenExitHooks.mu.Unlock()
	kept := tokenExitHooks.hooks[:0]
	for _, other := range tokenExitHooks.hooks {
		if other != h {
			kept = append(kept, other)
		}
	}
	tokenExitHooks.hooks = kept
}

// Drains the token queue of every store still alive, newest registration first, for the process exit path
func RunTokenExitHooks() {
	tokenExitHooks.mu.Lock()
	hooks := append([]*tokenExitHook(nil), tokenExitHooks.hooks...)
	tokenExitHooks.hooks = nil
	tokenExitHooks.mu.Unlock()
	for i := len(hooks) - 1; i >= 0; i-- {
		if db := hooks[i].db.Value(); db != nil {
			db.drainTokenQueueAtExit()
		}
	}
}

// Sets the billing route unconditionally after the queued token deltas land
func (d *SessionDB) UpdateSessionBillingRoute(ctx context.Context, sessionID, provider, baseURL string, billingMode *string) error {
	d.FlushTokenCounts(5.0)
	return d.executeWrite(ctx, func(conn SQLiteConn) error {
		_, err := conn.ExecContext(ctx, `UPDATE sessions SET
                   billing_provider = ?,
                   billing_base_url = ?,
                   billing_mode = COALESCE(?, billing_mode)
                   WHERE id = ?`, provider, baseURL, optString(billingMode), sessionID)
		return err
	}, nil)
}

// Enqueues a token delta for the background writer, applying it inline once close stopped the writer
func (d *SessionDB) QueueTokenCounts(sessionID string, delta TokenCountDelta) error {
	s := &d.tokens
	s.tokenMu.Lock()
	writer := s.tokenWriter
	writerAlive := writer.alive()
	writerStopped := s.tokenStop && !writerAlive
	if !writerStopped {
		s.tokenQueue = append(s.tokenQueue, queuedTokenDelta{sessionID, delta})
		if !writerAlive {
			writer = &tokenWriter{done: make(chan struct{})}
			s.tokenWriter = writer
			go d.tokenWriterLoop(writer)
			if s.tokenAtexitHook == nil {
				s.tokenAtexitHook = &tokenExitHook{db: weak.Make(d)}
				registerTokenExitHook(s.tokenAtexitHook)
			}
		}
		s.tokenNotifyAll()
	}
	s.tokenMu.Unlock()
	if writerStopped {
		return d.UpdateTokenCounts(sessionID, delta)
	}
	return nil
}

// Applies a batch whose busy flag the caller claimed, then releases the flag
func (d *SessionDB) applyClaimedBatch(batch []queuedTokenDelta) {
	defer func() {
		d.tokens.tokenMu.Lock()
		d.tokens.tokenBusy = false
		d.tokens.tokenNotifyAll()
		d.tokens.tokenMu.Unlock()
	}()
	d.applyTokenBatch(batch)
}

// Read side token barrier that never waits on a goroutine running an event loop
func (d *SessionDB) FlushForRead(ctx context.Context) bool {
	if !pyctx.RunningLoop(ctx) {
		return d.FlushTokenCounts(5.0)
	}
	return d.FlushTokenCounts(0.0)
}

// Blocks until every queued token delta is applied, false on timeout
func (d *SessionDB) FlushTokenCounts(timeoutS float64) bool {
	s := &d.tokens
	s.tokenMu.Lock()
	if len(s.tokenQueue) == 0 && !s.tokenBusy {
		s.tokenMu.Unlock()
		return true
	}
	var batch []queuedTokenDelta
	deadline := monotonicSeconds() + timeoutS
	for len(s.tokenQueue) > 0 || s.tokenBusy {
		if !s.tokenWriter.alive() && !s.tokenBusy {
			s.tokenBusy = true
			batch = append([]queuedTokenDelta(nil), s.tokenQueue...)
			s.tokenQueue = nil
			break
		}
		remaining := deadline - monotonicSeconds()
		if remaining <= 0 {
			s.tokenMu.Unlock()
			return false
		}
		s.tokenWait(remaining)
	}
	s.tokenMu.Unlock()
	if len(batch) > 0 {
		d.applyClaimedBatch(batch)
	}
	return true
}

// Background writer that applies queued batches and retires after the idle window or a drained stop
func (d *SessionDB) tokenWriterLoop(w *tokenWriter) {
	defer close(w.done)
	s := &d.tokens
	for {
		s.tokenMu.Lock()
		idleDeadline := monotonicSeconds() + tokenWriterIdleSeconds
		for len(s.tokenQueue) == 0 && !s.tokenStop {
			remaining := idleDeadline - monotonicSeconds()
			if remaining <= 0 {
				if s.tokenWriter == w {
					s.tokenWriter = nil
				}
				s.tokenMu.Unlock()
				return
			}
			s.tokenWait(remaining)
		}
		if len(s.tokenQueue) == 0 {
			if s.tokenWriter == w {
				s.tokenWriter = nil
			}
			s.tokenMu.Unlock()
			return
		}
		s.tokenBusy = true
		batch := append([]queuedTokenDelta(nil), s.tokenQueue...)
		s.tokenQueue = nil
		s.tokenMu.Unlock()
		d.applyClaimedBatch(batch)
	}
}

// Coalescer the writer runs, a var so a test can make the merge fail like the Python attribute patch
var tokenCoalescer = func(batch []queuedTokenDelta) []queuedTokenDelta { return coalesceTokenDeltas(batch) }

// Coalesces a batch, falling back to the raw batch when the merge panics
func coalesceOrRaw(batch []queuedTokenDelta) (out []queuedTokenDelta) {
	defer func() {
		if r := recover(); r != nil {
			slog.Warn(fmt.Sprintf("async token accounting: coalesce failed, applying raw batch: %v", r))
			out = batch
		}
	}()
	return tokenCoalescer(batch)
}

// Applies one delta through the instance override a test installed, else UpdateTokenCounts
func (d *SessionDB) applyTokenUpdate(sessionID string, delta TokenCountDelta) error {
	d.tokens.tokenMu.Lock()
	hook := d.tokens.updateHook
	d.tokens.tokenMu.Unlock()
	if hook != nil {
		return hook(sessionID, delta)
	}
	return d.UpdateTokenCounts(sessionID, delta)
}

// Applies queued deltas in order after coalescing, logging every failure instead of raising
func (d *SessionDB) applyTokenBatch(batch []queuedTokenDelta) {
	for _, item := range coalesceOrRaw(batch) {
		if err := d.applyTokenUpdate(item.sessionID, item.delta); err != nil {
			slog.Warn(fmt.Sprintf("async token accounting: apply failed (session=%s): %s", item.sessionID, err))
		}
	}
}

// Value of a delta keyword by its Python name, nil for a None keyword
func (t *TokenCountDelta) field(name string) any {
	ptr := func(p *string) any {
		if p == nil {
			return nil
		}
		return *p
	}
	switch name {
	case "model":
		return ptr(t.Model)
	case "cost_status":
		return ptr(t.CostStatus)
	case "cost_source":
		return ptr(t.CostSource)
	case "pricing_version":
		return ptr(t.PricingVersion)
	case "billing_provider":
		return ptr(t.BillingProvider)
	case "billing_base_url":
		return ptr(t.BillingBaseURL)
	case "billing_mode":
		return ptr(t.BillingMode)
	case "source":
		return ptr(t.Source)
	case "task":
		return t.Task
	}
	panic("unknown token delta route field " + name)
}

// Adds another delta's summed counter of one Python keyword name
func (t *TokenCountDelta) addSum(name string, o *TokenCountDelta) {
	switch name {
	case "input_tokens":
		t.InputTokens += o.InputTokens
	case "output_tokens":
		t.OutputTokens += o.OutputTokens
	case "cache_read_tokens":
		t.CacheReadTokens += o.CacheReadTokens
	case "cache_write_tokens":
		t.CacheWriteTokens += o.CacheWriteTokens
	case "reasoning_tokens":
		t.ReasoningTokens += o.ReasoningTokens
	case "api_call_count":
		t.APICallCount += o.APICallCount
	default:
		panic("unknown token delta sum field " + name)
	}
}

// Cost slot of one Python keyword name
func (t *TokenCountDelta) costSlot(name string) **float64 {
	switch name {
	case "estimated_cost_usd":
		return &t.EstimatedCostUSD
	case "actual_cost_usd":
		return &t.ActualCostUSD
	}
	panic("unknown token delta cost field " + name)
}

// Merges adjacent incremental deltas with an identical route, never merging an absolute delta
func coalesceTokenDeltas(batch []queuedTokenDelta) []queuedTokenDelta {
	type group struct {
		key  []any
		item queuedTokenDelta
	}
	var groups []*group
	for _, entry := range batch {
		var key []any
		if !entry.delta.Absolute {
			key = []any{entry.sessionID}
			for _, f := range tokenDeltaRouteFields {
				key = append(key, entry.delta.field(f))
			}
		}
		if len(groups) > 0 && key != nil && sameRouteKey(groups[len(groups)-1].key, key) {
			merged := &groups[len(groups)-1].item.delta
			for _, f := range tokenDeltaSumFields {
				merged.addSum(f, &entry.delta)
			}
			for _, f := range tokenDeltaCostFields {
				value := *entry.delta.costSlot(f)
				if value == nil {
					continue
				}
				slot := merged.costSlot(f)
				sum := *value
				if *slot != nil {
					sum += **slot
				}
				*slot = &sum
			}
			continue
		}
		groups = append(groups, &group{key: key, item: queuedTokenDelta{entry.sessionID, entry.delta}})
	}
	out := make([]queuedTokenDelta, len(groups))
	for i, g := range groups {
		out[i] = g.item
	}
	return out
}

// Tuple equality of two route keys, a nil key never matching
func sameRouteKey(a, b []any) bool {
	if a == nil || b == nil || len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Stops the writer goroutine and applies leftovers synchronously under the busy protocol
func (d *SessionDB) stopTokenWriter(joinTimeoutS float64) {
	s := &d.tokens
	s.tokenMu.Lock()
	s.tokenStop = true
	s.tokenNotifyAll()
	writer := s.tokenWriter
	s.tokenMu.Unlock()
	if writer.alive() {
		timer := time.NewTimer(secondsToDuration(joinTimeoutS))
		select {
		case <-writer.done:
		case <-timer.C:
		}
		timer.Stop()
		if writer.alive() {
			s.tokenMu.Lock()
			queued := len(s.tokenQueue)
			s.tokenMu.Unlock()
			slog.Warn(fmt.Sprintf("async token accounting: writer did not stop within %.0fs; %d queued delta(s) not persisted", joinTimeoutS, queued))
			return
		}
	}
	s.tokenMu.Lock()
	deadline := monotonicSeconds() + joinTimeoutS
	for s.tokenBusy {
		remaining := deadline - monotonicSeconds()
		if remaining <= 0 {
			slog.Warn(fmt.Sprintf("async token accounting: concurrent drain did not finish within %.0fs; %d queued delta(s) not persisted", joinTimeoutS, len(s.tokenQueue)))
			s.tokenMu.Unlock()
			return
		}
		s.tokenWait(remaining)
	}
	batch := append([]queuedTokenDelta(nil), s.tokenQueue...)
	if len(batch) > 0 {
		s.tokenBusy = true
		s.tokenQueue = nil
	}
	s.tokenMu.Unlock()
	if len(batch) > 0 {
		d.applyClaimedBatch(batch)
	}
}

// Exit drain that never panics out of the exit path
func (d *SessionDB) drainTokenQueueAtExit() {
	defer func() { _ = recover() }()
	d.stopTokenWriter(10.0)
}

// Stops the token writer and drops its exit hook, the first close steps
func (d *SessionDB) closeTokenWriter() {
	d.stopTokenWriter(10.0)
	d.tokens.tokenMu.Lock()
	hook := d.tokens.tokenAtexitHook
	d.tokens.tokenAtexitHook = nil
	d.tokens.tokenMu.Unlock()
	if hook != nil {
		unregisterTokenExitHook(hook)
	}
}

// Updates totals and route attribution after ensuring the session row exists with the caller's real source
func (d *SessionDB) UpdateTokenCounts(sessionID string, delta TokenCountDelta) error {
	ctx := context.Background()
	source := "unknown"
	if delta.Source != nil && *delta.Source != "" {
		source = *delta.Source
	}
	if err := d.insertSessionRow(ctx, NewSession{ID: sessionID, Source: source, Model: derefString(delta.Model)}); err != nil {
		return err
	}
	return d.executeWrite(ctx, func(conn SQLiteConn) error {
		return d.updateTokenCountsInTransaction(ctx, conn, sessionID, delta)
	}, nil)
}

// Token UPDATE plus the first accounted route rewrite and the per model row, inside the caller's write
func (d *SessionDB) updateTokenCountsInTransaction(ctx context.Context, conn SQLiteConn, sessionID string, t TokenCountDelta) error {
	query := tokenUpdateDeltaSQL
	if t.Absolute {
		query = tokenUpdateAbsoluteSQL
	}
	hasUsage := t.InputTokens != 0 || t.OutputTokens != 0 || t.CacheReadTokens != 0 || t.CacheWriteTokens != 0 ||
		t.ReasoningTokens != 0 || t.APICallCount != 0 || (t.EstimatedCostUSD != nil && *t.EstimatedCostUSD != 0)
	hasAccountedUsage := hasUsage || (t.ActualCostUSD != nil && *t.ActualCostUSD != 0)
	accounted := func(p *string) any {
		if hasAccountedUsage {
			return optString(p)
		}
		return nil
	}
	params := []any{t.InputTokens, t.OutputTokens, t.CacheReadTokens, t.CacheWriteTokens, t.ReasoningTokens,
		optFloat(t.EstimatedCostUSD), optFloat(t.ActualCostUSD), optFloat(t.ActualCostUSD), optString(t.CostStatus),
		optString(t.CostSource), optString(t.PricingVersion), accounted(t.BillingProvider), accounted(t.BillingBaseURL),
		accounted(t.BillingMode), accounted(t.Model), t.APICallCount, sessionID}
	recordModelUsage := !t.Absolute && hasUsage
	rows, err := queryObjects(ctx, conn, "SELECT model, billing_provider, api_call_count FROM sessions WHERE id = ?", sessionID)
	if err != nil {
		return err
	}
	existing := pyjson.NewObject()
	if len(rows) > 0 {
		existing = rows[0]
	}
	calls, err := pyIntOf(orZero(rowValue(existing, "api_call_count")))
	if err != nil {
		return err
	}
	model, provider := derefString(t.Model), derefString(t.BillingProvider)
	firstAccountedRoute := t.Task == "" && calls.Sign() == 0 && hasAccountedUsage && model != "" && provider != "" &&
		(!pyEquals(rowValue(existing, "model"), model) || !pyEquals(rowValue(existing, "billing_provider"), provider))
	if firstAccountedRoute {
		if _, err := conn.ExecContext(ctx, `UPDATE sessions
                   SET model = ?, billing_provider = ?,
                   billing_base_url = ?, billing_mode = ?
                   WHERE id = ?`, model, provider, optString(t.BillingBaseURL), optString(t.BillingMode), sessionID); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, query, params...); err != nil {
		return err
	}
	if recordModelUsage {
		return d.recordModelUsage(ctx, conn, sessionID, t.Task, modelUsage{model: t.Model, billingProvider: t.BillingProvider,
			billingBaseURL: t.BillingBaseURL, billingMode: t.BillingMode, inputTokens: t.InputTokens, outputTokens: t.OutputTokens,
			cacheReadTokens: t.CacheReadTokens, cacheWriteTokens: t.CacheWriteTokens, reasoningTokens: t.ReasoningTokens,
			estimatedCostUSD: t.EstimatedCostUSD, actualCostUSD: t.ActualCostUSD, costStatus: t.CostStatus,
			costSource: t.CostSource, apiCallCount: t.APICallCount})
	}
	return nil
}

// Accumulates one usage delta into session_model_usage, main loop rows falling back to the session route
func (d *SessionDB) recordModelUsage(ctx context.Context, conn SQLiteConn, sessionID, task string, u modelUsage) error {
	rows, err := queryObjects(ctx, conn, "SELECT model, billing_provider, billing_base_url, billing_mode FROM sessions WHERE id = ?", sessionID)
	if err != nil {
		return err
	}
	sess := pyjson.NewObject()
	if len(rows) > 0 && task == "" {
		sess = rows[0]
	}
	pick := func(own *string, key, fallback string) any {
		if own != nil && *own != "" {
			return *own
		}
		if v := rowValue(sess, key); pyTruthy(v) {
			return v
		}
		return fallback
	}
	now := float64(time.Now().UnixNano()) / 1e9
	_, err = conn.ExecContext(ctx, modelUsageUpsertSQL, sessionID, pick(u.model, "model", "unknown"),
		pick(u.billingProvider, "billing_provider", ""), pick(u.billingBaseURL, "billing_base_url", ""),
		pick(u.billingMode, "billing_mode", ""), task, u.apiCallCount, u.inputTokens, u.outputTokens,
		u.cacheReadTokens, u.cacheWriteTokens, u.reasoningTokens, floatOrZero(u.estimatedCostUSD),
		floatOrZero(u.actualCostUSD), optString(u.costStatus), optString(u.costSource), now, now)
	return err
}

// Records an auxiliary call's usage per model, provider and task without touching the session totals
func (d *SessionDB) RecordAuxiliaryUsage(ctx context.Context, sessionID, task string, usage AuxiliaryUsage) error {
	if sessionID == "" || task == "" {
		return nil
	}
	calls := 1
	if usage.APICallCount != nil {
		calls = *usage.APICallCount
	}
	row := modelUsage{model: usage.Model, billingProvider: usage.BillingProvider, billingBaseURL: usage.BillingBaseURL,
		inputTokens: usage.InputTokens, outputTokens: usage.OutputTokens, cacheReadTokens: usage.CacheReadTokens,
		cacheWriteTokens: usage.CacheWriteTokens, reasoningTokens: usage.ReasoningTokens,
		estimatedCostUSD: usage.EstimatedCostUSD, apiCallCount: calls}
	patience := sessionTranscriptWritePatienceS
	return d.executeWrite(ctx, func(conn SQLiteConn) error {
		rows, err := queryObjects(ctx, conn, "SELECT 1 FROM sessions WHERE id=?", sessionID)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			if err := d.insertSessionRowInTransaction(ctx, conn, NewSession{ID: sessionID, Source: "unknown"}); err != nil {
				return err
			}
		} else if own := d.ownProfileName(); own != "" {
			if _, err := conn.ExecContext(ctx, "UPDATE sessions SET profile_name=? WHERE id=? AND profile_name IS NULL", own, sessionID); err != nil {
				return err
			}
		}
		return d.recordModelUsage(ctx, conn, sessionID, task, row)
	}, &patience)
}

// Per task auxiliary usage summed over the compression lineage, cut at the nearest explicit branch
func (d *SessionDB) AuxiliaryUsageByTask(ctx context.Context, sessionID string) (*pyjson.Object, error) {
	out := pyjson.NewObject()
	if sessionID == "" {
		return out, nil
	}
	chain, err := d.sessionLineageRootToTip(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	for i := len(chain) - 1; i >= 0; i-- {
		branch, err := d.isExplicitBranchSession(ctx, chain[i])
		if err != nil {
			return nil, err
		}
		if branch {
			chain = chain[i:]
			break
		}
	}
	args := make([]any, len(chain))
	for i, id := range chain {
		args[i] = id
	}
	rows, err := d.readAll(ctx, `SELECT task,
                       COALESCE(SUM(api_call_count), 0) AS api_calls,
                       COALESCE(SUM(input_tokens), 0) AS input_tokens,
                       COALESCE(SUM(output_tokens), 0) AS output_tokens,
                       COALESCE(SUM(cache_read_tokens), 0) AS cache_read_tokens,
                       COALESCE(SUM(cache_write_tokens), 0) AS cache_write_tokens,
                       COALESCE(SUM(reasoning_tokens), 0) AS reasoning_tokens,
                       COALESCE(SUM(estimated_cost_usd), 0) AS estimated_cost_usd
                  FROM session_model_usage
                 WHERE session_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(chain)), ",")+`) AND task != ''
                 GROUP BY task`, args...)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		values := pyjson.NewObject()
		for _, k := range row.Keys {
			if k != "task" {
				values.Set(k, row.Vals[k])
			}
		}
		out.Set(pyStr(rowValue(row, "task")), values)
	}
	return out, nil
}

// Tokens and spend of the store's root sessions in one scan, the billed figure preferred over the estimate
func (d *SessionDB) UsageTotals(ctx context.Context, minMessageCount int, includeArchived bool) (tokens int64, costUSD float64, err error) {
	where := []string{"parent_session_id IS NULL", "message_count >= ?"}
	if !includeArchived {
		where = append(where, "COALESCE(archived, 0) = 0")
	}
	row, err := d.readOne(ctx, `
            SELECT COALESCE(SUM(COALESCE(input_tokens, 0) + COALESCE(output_tokens, 0)), 0) AS tokens,
                   COALESCE(SUM(COALESCE(actual_cost_usd, estimated_cost_usd, 0)), 0) AS cost_usd
              FROM sessions
             WHERE `+strings.Join(where, " AND ")+`
            `, minMessageCount)
	if err != nil || row == nil {
		return 0, 0, err
	}
	n, err := pyIntOf(orZero(rowValue(row, "tokens")))
	if err != nil {
		return 0, 0, err
	}
	cost, err := pyFloatOf(orZero(rowValue(row, "cost_usd")))
	return n.Int64(), cost, err
}

// SQL parameter of an optional string, NULL for None
func optString(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// SQL parameter of an optional float, NULL for None
func optFloat(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

// Python float(x or 0.0) of an optional float
func floatOrZero(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

// Empty string for a None keyword
func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// Python x or 0 of a row value
func orZero(v any) any {
	if !pyTruthy(v) {
		return int64(0)
	}
	return v
}

// Python equality of a row value and a string
func pyEquals(v any, s string) bool {
	text, ok := v.(string)
	return ok && text == s
}

// Idle window after which the token writer retires, a var a test may tune like the class attribute
var tokenWriterIdleSeconds = 30.0
