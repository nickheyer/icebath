package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

func conversationLoopLog() *slog.Logger {
	return slog.Default().With("logger", "agent.conversation_loop")
}

// ResponseUsageOutcome carries the possibly rearmed compression budget and whether the loop clears its preflight latch
type ResponseUsageOutcome struct {
	CompressionAttempts int
	Rearmed             bool
}

// Optional engine capabilities the built in compressor exposes and plugin engines may lack
type realUsageAwaiter interface {
	AwaitingRealUsageAfterCompression() bool
}

type usageLessResponseNoter interface{ NoteUsageLessResponse() }

type compactionClearLatch interface {
	VerifyCompactionClearedThreshold() bool
}

type contextProbeTracker interface {
	ContextProbed() bool
	ContextProbePersistable() bool
	ResetContextProbe()
}

// Optional MoA client capabilities for advisor usage, trace flushing and the aggregator slot
type moaReferenceUsageClient interface {
	ConsumeReferenceUsage() (usage *provider.CanonicalUsage, cost any, err error)
}

type moaTraceClient interface {
	ConsumeAndSaveTrace(sessionID string, aggregatorOutputFallback *string) error
}

type moaAggregatorSlotClient interface {
	LastAggregatorSlot() map[string]any
}

// Surface the agent's own row create stamps so an accounting guard never mints an anonymous session
func (a *AIAgent) agentSessionSource() string {
	return provider.SessionSourceFor(a.env, a.platform)
}

// Folds MoA advisor usage into the reported counts and flushes the turn trace, returning the advisor cost
func (a *AIAgent) foldMoAUsage(canonical provider.CanonicalUsage) (any, provider.CanonicalUsage, any) {
	client := a.client
	var refCost any
	if c, ok := client.(moaReferenceUsageClient); ok {
		refUsage, cost, err := c.ConsumeReferenceUsage()
		if err != nil {
			conversationLoopLog().Debug(fmt.Sprintf("MoA reference usage accounting failed: %s", err))
		} else {
			refCost = cost
			if refUsage != nil {
				canonical = canonical.Add(*refUsage)
			}
		}
	}
	if c, ok := client.(moaTraceClient); ok {
		var fallback *string
		if a.currentStreamedAssistantText() != "" {
			text := a.currentStreamedAssistantText()
			fallback = &text
		}
		if err := c.ConsumeAndSaveTrace(a.sessionID, fallback); err != nil {
			conversationLoopLog().Debug(fmt.Sprintf("MoA trace flush failed: %s", err))
		}
	}
	return client, canonical, refCost
}

// Python isinstance int check over a decoded usage attribute, bool included
func pyIntAttr(v any) (int64, bool) {
	switch n := v.(type) {
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	case json.Number:
		parsed, err := n.Int64()
		return parsed, err == nil
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int(), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return int64(rv.Uint()), true
	}
	return 0, false
}

func groupedInt(n int) string {
	text, _ := pyrepr.Format(n, ",")
	return text
}

// Folds response usage into the compressor, anchors, session counters, the token queue and the API call log line
func (a *AIAgent) recordResponseUsage(ctx context.Context, response any, messages []*state.Message, apiCallCount int,
	apiDuration float64, compressionAttempts, maxCompressionAttempts int) ResponseUsageOutcome {
	rearmed := false
	compressor := a.contextCompressor
	log := conversationLoopLog()
	a.sessionAPICalls++
	rawUsage := state.ToolCallField(response, "usage")
	providerName := a.provider
	if providerName == "" {
		providerName = "unknown"
	}
	if !pyrepr.Truthy(rawUsage) {
		if w, ok := compressor.(realUsageAwaiter); ok && w.AwaitingRealUsageAfterCompression() {
			compressor.UpdateFromResponse(map[string]any{})
		}
		if n, ok := compressor.(usageLessResponseNoter); ok {
			n.NoteUsageLessResponse()
		}
		log.Info(fmt.Sprintf("API call #%d: model=%s provider=%s in=? out=? total=? latency=%.1fs usage=unavailable",
			a.sessionAPICalls, a.model, providerName, apiDuration))
		return ResponseUsageOutcome{CompressionAttempts: compressionAttempts, Rearmed: rearmed}
	}

	tier, _ := state.ToolCallField(response, "service_tier").(string)
	canonical := provider.WithServedServiceTier(provider.NormalizeUsage(rawUsage, a.provider, a.apiMode), tier)
	aggregator := canonical
	moaClient, canonical, moaRefCost := a.foldMoAUsage(canonical)
	promptTokens := canonical.PromptTokens()
	completionTokens := canonical.OutputTokens
	totalTokens := canonical.TotalTokens()

	contextPrompt := 0
	if n, ok := pyIntAttr(state.ToolCallField(rawUsage, "initial_prompt_tokens")); ok && n > 0 {
		contextPrompt = int(n)
	}
	contextCacheRead, haveContextCacheRead := 0, false
	if contextPrompt != 0 {
		initialCached := attrOr(rawUsage, "initial_cached_tokens", 0)
		if n, ok := pyIntAttr(initialCached); ok {
			contextCacheRead, haveContextCacheRead = int(n), true
		}
	}
	usagePrompt := promptTokens
	usageTotal := totalTokens
	if contextPrompt != 0 {
		usagePrompt = contextPrompt
		usageTotal = contextPrompt + completionTokens
	}
	usageInput := canonical.InputTokens
	usageCacheRead := canonical.CacheReadTokens
	if haveContextCacheRead {
		usageInput = contextPrompt - contextCacheRead
		usageCacheRead = contextCacheRead
	}
	usageDict := map[string]any{
		"prompt_tokens":      usagePrompt,
		"completion_tokens":  completionTokens,
		"total_tokens":       usageTotal,
		"input_tokens":       usageInput,
		"output_tokens":      canonical.OutputTokens,
		"cache_read_tokens":  usageCacheRead,
		"cache_write_tokens": canonical.CacheWriteTokens,
		"reasoning_tokens":   canonical.ReasoningTokens,
	}
	completedCompactionPending := false
	if l, ok := compressor.(compactionClearLatch); ok {
		completedCompactionPending = l.VerifyCompactionClearedThreshold()
	}
	compressor.UpdateFromResponse(usageDict)

	anchorPrompt := contextPrompt
	if anchorPrompt == 0 {
		anchorPrompt = aggregator.PromptTokens()
	}
	a.calibrateFromUsage(ctx, messages, anchorPrompt)
	if anchor := provider.CaptureUsageAnchor(anchorPrompt, aggregator.OutputTokens, messages); anchor != nil {
		a.setUsageAnchor(anchor, apiCallCount == 1)
	}
	threshold := compressor.Base().ThresholdTokens
	if shouldRearmCompressionBudget(compressionAttempts, completedCompactionPending, usagePrompt, threshold) {
		log.Info(fmt.Sprintf("Compression budget rearmed after provider-confirmed recovery: prompt=%s < threshold=%s (attempts were %d/%d)",
			groupedInt(usagePrompt), groupedInt(threshold), compressionAttempts, maxCompressionAttempts))
		compressionAttempts = 0
		rearmed = true
	}

	a.lastTurnUsage = make(map[string]any, len(usageDict))
	for k, v := range usageDict {
		a.lastTurnUsage[k] = v
	}
	a.lastPromptSizeTokens = anchorPrompt

	if p, ok := compressor.(contextProbeTracker); ok && p.ContextProbed() {
		length := engineContextLength(compressor)
		if p.ContextProbePersistable() {
			provider.SaveProviderContextLength(ctx, a.model, a.baseURL, length, a.provider)
			a.safePrint(fmt.Sprintf("%s💾 Cached context length: %s tokens for %s", a.logPrefix, groupedInt(length), a.model), false)
		}
		p.ResetContextProbe()
	}

	a.sessionPromptTokens += promptTokens
	a.sessionCompletionTokens += completionTokens
	a.sessionTotalTokens += totalTokens
	a.sessionInputTokens += canonical.InputTokens
	a.sessionOutputTokens += canonical.OutputTokens
	a.sessionCacheReadTokens += canonical.CacheReadTokens
	a.sessionCacheWriteTokens += canonical.CacheWriteTokens
	a.sessionReasoningTokens += canonical.ReasoningTokens
	a.apiLatencyHistory = appendBounded(a.apiLatencyHistory, apiDuration, apiHistoryLen)
	a.apiOutputHistory = appendBounded(a.apiOutputHistory, canonical.OutputTokens, apiHistoryLen)

	cachePct := ""
	if canonical.CacheReadTokens != 0 && promptTokens != 0 {
		cachePct = fmt.Sprintf(" cache=%d/%d (%.0f%%)", canonical.CacheReadTokens, promptTokens,
			100*float64(canonical.CacheReadTokens)/float64(promptTokens))
	}
	if canonical.CacheWriteTokens != 0 {
		cachePct += fmt.Sprintf(" write=%d", canonical.CacheWriteTokens)
	}
	ident := ""
	if rid, ok := state.ToolCallField(response, "id").(string); ok && rid != "" {
		ident = " id=" + rid
	}
	if upstream, ok := state.ToolCallField(response, "provider").(string); ok && upstream != "" {
		ident += " upstream=" + upstream
	}
	log.Info(fmt.Sprintf("API call #%d: model=%s provider=%s in=%d out=%d total=%d latency=%.1fs%s%s",
		a.sessionAPICalls, a.model, providerName, promptTokens, completionTokens, totalTokens, apiDuration, cachePct, ident))
	if a.sessionAPICalls == 1 && a.provider == "nous" {
		maybeSwitchWireAfterFirstResponse(a, ctx, response, a.sessionAPICalls)
	}

	costModel, costProvider, costBaseURL := a.model, a.provider, a.baseURL
	if s, ok := moaClient.(moaAggregatorSlotClient); ok {
		if slot := s.LastAggregatorSlot(); len(slot) > 0 && pyrepr.Truthy(slot["model"]) {
			costModel = pyrepr.Str(slot["model"])
			costProvider = pyStrOr(slot["provider"], a.provider)
			costBaseURL = pyStrOr(slot["base_url"], a.baseURL)
		}
	}
	costResult := provider.EstimateUsageCost(ctx, costModel, aggregator, costProvider, costBaseURL, a.apiKey)
	var costDelta *float64
	if costResult.AmountUSD != nil {
		amount, _ := costResult.AmountUSD.Float64()
		costDelta = &amount
		a.sessionEstimatedCostUSD += amount
	}
	if moaRefCost != nil {
		if moaCost, err := pyint.Float(moaRefCost); err == nil {
			a.sessionEstimatedCostUSD += moaCost
			sum := moaCost
			if costDelta != nil {
				sum += *costDelta
			}
			costDelta = &sum
		}
	}
	a.sessionCostStatus = costResult.Status
	a.sessionCostSource = costResult.Source

	if a.sessionDB != nil && a.sessionID != "" {
		if err := a.queueResponseTokenCounts(canonical, costResult, costDelta); err != nil {
			log.Debug(fmt.Sprintf("Token persistence failed (session=%s, tokens=%d): %s", a.sessionID, totalTokens, err))
		}
	}

	if a.verboseLogging {
		slog.Debug(fmt.Sprintf("Token usage: prompt=%s, completion=%s, total=%s",
			groupedInt(usagePrompt), groupedInt(completionTokens), groupedInt(usageTotal)))
	}
	cached := canonical.CacheReadTokens
	written := canonical.CacheWriteTokens
	if (cached != 0 || written != 0) && !a.quietMode {
		hitPct := 0.0
		if usagePrompt > 0 {
			hitPct = float64(cached) / float64(usagePrompt) * 100
		}
		a.vprint(fmt.Sprintf("%s   💾 Cache: %s/%s tokens (%.0f%% hit, %s written)",
			a.logPrefix, groupedInt(cached), groupedInt(usagePrompt), hitPct, groupedInt(written)), false, false)
	}
	return ResponseUsageOutcome{CompressionAttempts: compressionAttempts, Rearmed: rearmed}
}

// Ensures the session row then queues this call's token and cost delta for the background writer
func (a *AIAgent) queueResponseTokenCounts(canonical provider.CanonicalUsage, cost provider.CostResult, costDelta *float64) error {
	if !a.sessionDBCreated {
		if err := a.ensureDBSession(); err != nil {
			return err
		}
	}
	source := a.agentSessionSource()
	status, costSource := cost.Status, cost.Source
	delta := state.TokenCountDelta{
		Source:           &source,
		InputTokens:      int64(canonical.InputTokens),
		OutputTokens:     int64(canonical.OutputTokens),
		CacheReadTokens:  int64(canonical.CacheReadTokens),
		CacheWriteTokens: int64(canonical.CacheWriteTokens),
		ReasoningTokens:  int64(canonical.ReasoningTokens),
		EstimatedCostUSD: costDelta,
		CostStatus:       &status,
		CostSource:       &costSource,
		BillingProvider:  &a.provider,
		BillingBaseURL:   &a.baseURL,
		Model:            &a.model,
		APICallCount:     1,
		Task:             a.turnRouteTask,
	}
	if cost.Status == "included" {
		mode := "subscription_included"
		delta.BillingMode = &mode
	}
	return a.sessionDB.QueueTokenCounts(a.sessionID, delta)
}

const apiHistoryLen = 10

// Appends to a rolling history keeping the newest entries like a bounded deque
func appendBounded[T any](history []T, value T, maxLen int) []T {
	history = append(history, value)
	if len(history) > maxLen {
		history = append(history[:0:0], history[len(history)-maxLen:]...)
	}
	return history
}

// Attribute of an object or key of an attribute bag map, def when absent and nil when present as None
func attrOr(v any, key string, def any) any {
	switch m := v.(type) {
	case map[string]any:
		if value, ok := m[key]; ok {
			return value
		}
		return def
	case state.PyObject:
		for _, kv := range m {
			if kv.Key == key {
				return kv.Value
			}
		}
		return def
	}
	if value := state.ToolCallField(v, key); value != nil {
		return value
	}
	return def
}
