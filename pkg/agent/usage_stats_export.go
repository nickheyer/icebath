package agent

import (
	"context"
	"math"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Records the output token count at the start of a viewer turn for its last turn tokens figure
func (a *AIAgent) MarkViewerTurnOutputBase() {
	base := a.sessionOutputTokens
	a.viewerTurnOutputBase = &base
}

// Rounded to two places inside the plausible range, nil for nan or an outlier
func usageFinite(value float64) any {
	if math.IsNaN(value) || value <= 0 || value >= 1e6 {
		return nil
	}
	return pyint.Round(value, 2)
}

// Status bar context figures of the compressor, zeros without one
func usageContext(ctx context.Context, a *AIAgent) *pyjson.Object {
	var engine ContextEngine
	if a != nil {
		engine = a.contextCompressor
	}
	if engine == nil {
		return pyjson.ObjectOf("last_prompt_tokens", 0, "context_tokens", 0, "context_length", nil,
			"config_context_length", nil, "context_source", nil, "compression_count", 0)
	}
	base := engine.Base()
	last := max(0, base.LastPromptTokens)
	tokens := last
	func() {
		defer func() { _ = recover() }()
		anchored, ok := provider.AnchoredContextTokens(ctx, a.liveSessionMessages(), a.turnBaseUsageAnchor.Load(), false)
		if ok && anchored > 0 {
			tokens = anchored
		}
	}()
	length := max(0, base.ContextLength)
	contextTokens := tokens
	var contextLength any
	if length != 0 {
		contextTokens = min(tokens, length)
		contextLength = length
	}
	var configContextLength any
	if compressor, builtin := engine.(*ContextCompressor); builtin && compressor.configContextLength != nil {
		configContextLength = *compressor.configContextLength
	}
	return pyjson.ObjectOf("last_prompt_tokens", last, "context_tokens", contextTokens, "context_length", contextLength,
		"config_context_length", configContextLength, "context_source", contextDisplaySource(engine),
		"compression_count", base.CompressionCount)
}

// Field dict of a rate limit bucket, the dataclass asdict shape
func rateLimitBucketDict(b provider.RateLimitBucket) *pyjson.Object {
	var captured float64
	if !b.CapturedAt.IsZero() {
		captured = float64(b.CapturedAt.UnixNano()) / 1e9
	}
	return pyjson.ObjectOf("limit", b.Limit, "remaining", b.Remaining, "reset_seconds", b.ResetSeconds, "captured_at", captured)
}

// Last captured rate limit state as its dataclass dict, nil before any headers were captured
func usageRateLimit(a *AIAgent) any {
	capture := a.CreditsCapture()
	if capture == nil {
		return nil
	}
	state := capture.RateLimit()
	if state == nil {
		return nil
	}
	var captured float64
	if !state.CapturedAt.IsZero() {
		captured = float64(state.CapturedAt.UnixNano()) / 1e9
	}
	return pyjson.ObjectOf("requests_min", rateLimitBucketDict(state.RequestsMin), "requests_hour", rateLimitBucketDict(state.RequestsHour),
		"tokens_min", rateLimitBucketDict(state.TokensMin), "tokens_hour", rateLimitBucketDict(state.TokensHour),
		"captured_at", captured, "provider", state.Provider)
}

// Frozen session usage and stats dict of an agent, zeros before the first turn built one
func UsageStats(ctx context.Context, a *AIAgent) *pyjson.Object {
	counter := func(get func(*AIAgent) int) int {
		if a == nil {
			return 0
		}
		return get(a)
	}
	stats := pyjson.ObjectOf(
		"input_tokens", counter(func(a *AIAgent) int { return a.sessionInputTokens }),
		"output_tokens", counter(func(a *AIAgent) int { return a.sessionOutputTokens }),
		"cache_read_tokens", counter(func(a *AIAgent) int { return a.sessionCacheReadTokens }),
		"cache_write_tokens", counter(func(a *AIAgent) int { return a.sessionCacheWriteTokens }),
		"prompt_tokens", counter(func(a *AIAgent) int { return a.sessionPromptTokens }),
		"completion_tokens", counter(func(a *AIAgent) int { return a.sessionCompletionTokens }),
		"total_tokens", counter(func(a *AIAgent) int { return a.sessionTotalTokens }),
		"reasoning_tokens", counter(func(a *AIAgent) int { return a.sessionReasoningTokens }),
		"api_calls", counter(func(a *AIAgent) int { return a.sessionAPICalls }))
	contextFields := usageContext(ctx, a)
	for _, key := range contextFields.Keys {
		stats.Set(key, contextFields.Vals[key])
	}
	var avgLatency, tps any
	if a != nil {
		latencies, outputs := a.apiLatencyHistory, a.apiOutputHistory
		if n := min(len(latencies), len(outputs)); n > 0 {
			total := 0.0
			for _, v := range latencies[len(latencies)-n:] {
				total += v
			}
			avgLatency = usageFinite(total / float64(n))
			if total > 0 {
				out := 0
				for _, v := range outputs[len(outputs)-n:] {
					out += v
				}
				tps = usageFinite(float64(out) / total)
			}
		}
	}
	lastTurn := 0
	if a != nil && a.viewerTurnOutputBase != nil {
		lastTurn = max(0, a.sessionOutputTokens-*a.viewerTurnOutputBase)
	}
	var rateLimit, fallbackModel any
	model, providerName := "", ""
	if a != nil {
		rateLimit = usageRateLimit(a)
		model, providerName = a.model, a.provider
		if a.fallbackActivated && model != "" {
			fallbackModel = model
		}
	}
	stats.Set("avg_latency_s", avgLatency)
	stats.Set("tokens_per_s", tps)
	stats.Set("last_turn_tokens", lastTurn)
	stats.Set("rate_limit", rateLimit)
	stats.Set("active_subagents", tools.AsyncDelegationActiveCount())
	stats.Set("model", model)
	stats.Set("provider", providerName)
	stats.Set("fallback_model", fallbackModel)
	return stats
}
