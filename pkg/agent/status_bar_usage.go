package agent

import (
	"context"
	"math"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// StatusBarUsage is the agent state the CLI status bar snapshot reads on every repaint
type StatusBarUsage struct {
	Counters         map[string]int
	HasCompressor    bool
	ContextTokens    int
	ContextLength    int
	ContextPercent   *int
	ContextEstimated bool
	ContextPinned    bool
	Compressions     int
	LatencyHistory   []float64
	OutputHistory    []int
}

// Tells the built in preflight display seed from a provider reading of the context occupancy
func contextDisplaySource(engine ContextEngine) string {
	compressor, builtin := engine.(*ContextCompressor)
	if !builtin {
		return "provider_usage"
	}
	if shown := compressor.LastPromptTokens; shown > 0 && shown != compressor.lastRealPromptTokens {
		return "local_estimate"
	}
	return "provider_usage"
}

// ContextDisplaySource tells a local preflight estimate from a provider reading of the context occupancy
func ContextDisplaySource(engine ContextEngine) string { return contextDisplaySource(engine) }

// StatusBarUsage snapshots the session counters, context occupancy and latency history for the status bar
func (a *AIAgent) StatusBarUsage(ctx context.Context) StatusBarUsage {
	usage := StatusBarUsage{Counters: map[string]int{
		"session_input_tokens": a.sessionInputTokens, "session_output_tokens": a.sessionOutputTokens,
		"session_cache_read_tokens": a.sessionCacheReadTokens, "session_cache_write_tokens": a.sessionCacheWriteTokens,
		"session_prompt_tokens": a.sessionPromptTokens, "session_completion_tokens": a.sessionCompletionTokens,
		"session_total_tokens": a.sessionTotalTokens, "session_api_calls": a.sessionAPICalls,
	}, LatencyHistory: append([]float64{}, a.apiLatencyHistory...), OutputHistory: append([]int{}, a.apiOutputHistory...)}
	engine := a.contextCompressor
	if engine == nil {
		return usage
	}
	base := engine.Base()
	usage.HasCompressor = true
	contextTokens := max(0, base.LastPromptTokens)
	usage.ContextEstimated = contextDisplaySource(engine) != "provider_usage"
	func() {
		defer func() { _ = recover() }()
		messages, anchor := a.liveSessionMessages(), a.turnBaseUsageAnchor.Load()
		anchored, ok := provider.AnchoredContextTokens(ctx, messages, anchor, false)
		if !ok || anchored <= 0 {
			return
		}
		contextTokens = anchored
		delta := messages[anchor.BaseCount:]
		if len(delta) > 0 && delta[0] != nil && delta[0].Role == "assistant" {
			delta = delta[1:]
		}
		usage.ContextEstimated = len(delta) > 0
	}()
	contextLength := max(0, base.ContextLength)
	if contextLength != 0 {
		contextTokens = min(contextTokens, contextLength)
	}
	usage.ContextTokens, usage.ContextLength = contextTokens, contextLength
	var configContextLength any
	if compressor, builtin := engine.(*ContextCompressor); builtin && compressor.configContextLength != nil {
		configContextLength = *compressor.configContextLength
	}
	usage.ContextPinned = isContextPinned(contextLength, configContextLength)
	usage.Compressions = base.CompressionCount
	if contextLength != 0 {
		pct := max(0, min(100, int(math.RoundToEven(float64(contextTokens)/float64(contextLength)*100))))
		usage.ContextPercent = &pct
	}
	return usage
}

// TodoItems reads the agent's task list, false when the agent carries no todo store
func (a *AIAgent) TodoItems() ([]tools.TodoItem, bool) {
	if a.todoStore == nil {
		return nil, false
	}
	return a.todoStore.Read(), true
}
