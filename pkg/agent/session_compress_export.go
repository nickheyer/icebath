package agent

import (
	"context"
	"fmt"
	"sync"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/settings"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Agent side of the gateway's canonical session compress
type SessionCompressAgent struct{}

// Plugin engines whose last compress was refused because it would grow the conversation
var refusedWouldGrow sync.Map

// String field of a runtime mapping, empty when absent or falsy
func runtimeText(runtime map[string]any, key string) string {
	if v := runtime[key]; anyutil.IsTrue(v) {
		return anyutil.String(v)
	}
	return ""
}

// Configured window agent init hands a plugin engine for this runtime under the settings of ctx, else the matching custom provider's
func sessionConfiguredContextLength(ctx context.Context, model, providerName, baseURL string, providers []map[string]any) *int {
	var configured *int
	if n, ok := settings.ModelContextLength.Lookup(ctx); ok {
		route := &AIAgent{model: model, provider: providerName, baseURL: baseURL}
		configured = scopeContextLengthToDefaultRuntime(ctx, route, providers, &n, baseURL)
	}
	if configured == nil && len(providers) > 0 {
		if n := provider.CustomProviderContextLength(model, baseURL, providers); n != 0 {
			configured = &n
		}
	}
	if configured == nil || *configured == 0 {
		return nil
	}
	return configured
}

// Session's plugin context engine, the resident agent's own or one bound like agent init, nil for the built in one
func sessionPluginEngine(ctx context.Context, job gateway.SessionCompressJob, resident *AIAgent) (ContextEngine, error) {
	if resident != nil {
		engine := resident.contextCompressor
		if engine == nil || isNilEngine(engine) {
			return nil, nil
		}
		if _, builtin := engine.(*ContextCompressor); builtin {
			return nil, nil
		}
		return engine, nil
	}
	if job.SafeMode {
		return nil, nil
	}
	ctx = settings.WithFrozen(ctx, job.Config)
	engine := selectContextEngine(ctx)
	if engine == nil || isNilEngine(engine) {
		return nil, nil
	}
	if thresholds := compressionModelThresholds(ctx); len(thresholds) > 0 {
		engine.Base().ModelThresholds = thresholds
	}
	baseURL, apiKey, providerName := runtimeText(job.Runtime, "base_url"), runtimeText(job.Runtime, "api_key"), runtimeText(job.Runtime, "provider")
	providers := provider.CustomProviderEntries(ctx)
	request := provider.ContextLengthRequest{Model: job.Model, BaseURL: baseURL, APIKey: apiKey, Provider: providerName, CustomProviders: providers}
	if configured := sessionConfiguredContextLength(ctx, job.Model, providerName, baseURL, providers); configured != nil {
		request.ConfigContextLength = *configured
	}
	engine.UpdateModel(job.Model, provider.GetModelContextLength(ctx, request), baseURL, apiKey, providerName, runtimeText(job.Runtime, "api_mode"))
	if binder, ok := engine.(interface {
		BindSessionState(db SessionStore, sessionID string) error
	}); ok {
		if err := binder.BindSessionState(job.DB, job.Target); err != nil {
			return nil, err
		}
	}
	return engine, nil
}

// Deep copy of message rows through JSON
func cloneRows(rows []map[string]any) []map[string]any {
	out := make([]map[string]any, len(rows))
	for i, row := range rows {
		out[i], _ = jsonutil.Clone(row).(map[string]any)
	}
	return out
}

// One forced compress on a plugin engine, nil when it changed nothing or would grow the conversation
func sessionEngineCompress(ctx context.Context, engine ContextEngine, job gateway.SessionCompressJob, memoryContext string) ([]map[string]any, error) {
	refusedWouldGrow.Delete(engine)
	tokens := estimateRequestRowsRough(ctx, job.Head, "", nil)
	call := compressCall{engine: engine, kwargs: supportedCompressionKwargs(engine, &tokens, job.FocusTopic, true, memoryContext, false)}
	restore := aggressiveCompression(engine, job.Aggressive)
	compressed, err := call.run(ctx, cloneRows(job.Head))
	restore()
	if err != nil {
		return nil, err
	}
	if anyutil.Equal(jsonutil.Plain(rowsAsList(compressed)), jsonutil.Plain(rowsAsList(job.Head))) {
		return nil, nil
	}
	compressed = provider.RejoinCompressedHeadAndTail(compressed, job.Tail)
	if estimateRowsRough(ctx, compressed) > estimateRowsRough(ctx, job.Messages) {
		refusedWouldGrow.Store(engine, true)
		return nil, nil
	}
	return compressed, nil
}

// Rows as a JSON list value
func rowsAsList(rows []map[string]any) []any {
	out := make([]any, len(rows))
	for i, row := range rows {
		out[i] = row
	}
	return out
}

// Summarizes the head with the session's plugin engine or a built in compressor for its runtime
func (SessionCompressAgent) Summarize(ctx context.Context, job gateway.SessionCompressJob) ([]map[string]any, any, error) {
	resident, _ := job.Resident.(*AIAgent)
	memoryContext := ""
	if resident != nil {
		mc, err := preCompressMemoryContext(ctx, resident, job.Head, false)
		if err != nil {
			return nil, nil, err
		}
		memoryContext = mc
	}
	engine, err := sessionPluginEngine(ctx, job, resident)
	if err != nil {
		return nil, nil, err
	}
	if engine != nil {
		compressed, err := sessionEngineCompress(ctx, engine, job, memoryContext)
		if err != nil {
			if resident == nil {
				endSessionEngine(engine, job.Target, job.Messages)
			}
			return nil, nil, err
		}
		return compressed, engine, nil
	}
	options := DefaultContextCompressorOptions(job.Model)
	options.BaseURL, options.APIKey = runtimeText(job.Runtime, "base_url"), runtimeText(job.Runtime, "api_key")
	options.Provider, options.APIMode = runtimeText(job.Runtime, "provider"), runtimeText(job.Runtime, "api_mode")
	options.QuietMode, options.AbortOnSummaryFailure = true, true
	frozen := settings.WithFrozen(ctx, job.Config)
	options.ProtectFirstN = max(0, settings.CompressionProtectFirstN.Get(frozen))
	options.ProtectLastN = max(0, settings.CompressionProtectLastN.Get(frozen))
	options.MinTailUserMessages = max(1, settings.CompressionMinTailUserMessages.Get(frozen))
	entries := provider.CustomProviderEntries(frozen)
	options.CustomProviders = make([]any, len(entries))
	for i, entry := range entries {
		options.CustomProviders[i] = entry
	}
	compressor := NewContextCompressor(ctx, options)
	compressed, err := compressor.CompressContext(ctx, cloneRows(job.Head), nil, job.FocusTopic, true, memoryContext)
	if err != nil {
		return nil, compressor, err
	}
	for _, m := range compressed {
		if anyutil.IsTrue(m["_compressed_summary"]) {
			return provider.RejoinCompressedHeadAndTail(compressed, job.Tail), compressor, nil
		}
	}
	return nil, compressor, nil
}

// Ends an engine's session, a failure only logged
func endSessionEngine(engine ContextEngine, sessionID string, messages []map[string]any) {
	defer func() {
		if r := recover(); r != nil {
			pkgLog().Warn(fmt.Sprintf("context engine on_session_end failed: %v", r))
		}
	}()
	engine.OnSessionEnd(sessionID, messages)
}

// Whether the engine is the built in compressor
func (SessionCompressAgent) BuiltinEngine(engine any) bool {
	_, builtin := engine.(*ContextCompressor)
	return builtin
}

// Ends a one off engine's session
func (SessionCompressAgent) EndEngine(engine any, sessionID string, messages []map[string]any) {
	if e, ok := engine.(ContextEngine); ok && e != nil {
		endSessionEngine(e, sessionID, messages)
	}
}

// Manual compression report dict of before and after rows
func (SessionCompressAgent) ManualSummary(before, after []map[string]any, beforeTokens, afterTokens int, engine any) map[string]any {
	var st *ManualCompressionState
	if e, ok := engine.(ContextEngine); ok && e != nil {
		st = manualCompressionState(e)
		if refused, _ := refusedWouldGrow.Load(e); refused == true {
			if st == nil {
				st = &ManualCompressionState{}
			}
			st.LastCompressRefusedWouldGrow = true
		}
	}
	fb := SummarizeManualCompression(before, after, beforeTokens, afterTokens, st)
	var note any
	if fb.Note != nil {
		note = *fb.Note
	}
	return map[string]any{"noop": fb.Noop, "aborted": fb.Aborted, "refused_would_grow": fb.RefusedWouldGrow, "fallback_used": fb.FallbackUsed,
		"headline": fb.Headline, "token_line": fb.TokenLine, "note": note}
}

// Announces the committed compression boundary to the engine that compressed
func (SessionCompressAgent) NotifyCompressionComplete(ctx context.Context, engine any, platform, sessionKey, newSessionID, oldSessionID string) {
	owner := &AIAgent{platform: platform, gatewaySessionKey: sessionKey}
	if e, ok := engine.(ContextEngine); ok {
		owner.contextCompressor = e
	}
	notifyContextEngineCompressionComplete(ctx, owner, newSessionID, oldSessionID)
}

// Fires post_compaction for a committed manual summarize
func (SessionCompressAgent) FirePostCompaction(ctx context.Context, sessionID, oldSessionID string, beforeTokens, afterTokens, messagesRemoved any) {
	FirePostCompaction(ctx, sessionID, oldSessionID, "summarize", "manual", beforeTokens, afterTokens, messagesRemoved)
}

// Rough request estimate of rows with a system prompt and tool schemas
func (SessionCompressAgent) RequestTokens(ctx context.Context, messages []map[string]any, systemPrompt string, tools []map[string]any) int {
	return estimateRequestRowsRough(ctx, messages, systemPrompt, tools)
}

// Rough message only estimate of rows
func (SessionCompressAgent) MessagesTokens(ctx context.Context, messages []map[string]any) int {
	return estimateRowsRough(ctx, messages)
}

// Cached system prompt and tool schemas the agent sends
func (SessionCompressAgent) RequestBasis(agent any) (string, []map[string]any) {
	a, ok := agent.(*AIAgent)
	if !ok || a == nil {
		return "", nil
	}
	return a.cachedSystemPrompt, agentToolMaps(a)
}
