package agent

import (
	"context"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

func (a *AIAgent) RequestedProvider() string                         { return a.requestedProvider }
func (a *AIAgent) APIKey() string                                    { return a.apiKey }
func (a *AIAgent) ReasoningConfig() map[string]any                   { return a.reasoningConfig }
func (a *AIAgent) EphemeralSystemPrompt() string                     { return a.ephemeralSystemPrompt }
func (a *AIAgent) CachedSystemPrompt() string                        { return a.cachedSystemPrompt }
func (a *AIAgent) SessionCwd() string                                { return a.sessionCwd }
func (a *AIAgent) ShowCommentary() bool                              { return a.showCommentary }
func (a *AIAgent) HoldStream() bool                                  { return a.holdStream }
func (a *AIAgent) InterruptRequested() bool                          { return a.interruptRequested.Load() }
func (a *AIAgent) ClearInterrupt()                                   { a.clearInterrupt(false) }
func (a *AIAgent) ItersSinceSkill() int                              { return a.itersSinceSkill }
func (a *AIAgent) SetItersSinceSkill(n int)                          { a.itersSinceSkill = n }
func (a *AIAgent) SkillNudgeInterval() int                           { return a.skillNudgeInterval }
func (a *AIAgent) CodexSession() *provider.CodexAppServerSession     { return a.codexSession }
func (a *AIAgent) SetCodexSession(s *provider.CodexAppServerSession) { a.codexSession = s }
func (a *AIAgent) CodexSessionPrompt() *string                       { return a.codexSessionPrompt }
func (a *AIAgent) SetCodexSessionPrompt(p *string)                   { a.codexSessionPrompt = p }
func (a *AIAgent) CodexSessionModelProvider() *string                { return a.codexSessionModelProvider }
func (a *AIAgent) SetCodexSessionModelProvider(p *string)            { a.codexSessionModelProvider = p }
func (a *AIAgent) SessionDBCreated() bool                            { return a.sessionDBCreated }
func (a *AIAgent) EnsureDBSession() error                            { return a.ensureDBSession() }
func (a *AIAgent) SessionSource() string                             { return provider.SessionSourceFor(a.env, a.platform) }
func (a *AIAgent) AddSessionAPICalls(n int)                          { a.sessionAPICalls += n }
func (a *AIAgent) AddSessionEstimatedCostUSD(usd float64)            { a.sessionEstimatedCostUSD += usd }
func (a *AIAgent) SetSessionCost(status, source string) {
	a.sessionCostStatus, a.sessionCostSource = status, source
}
func (a *AIAgent) SetLastCompactionInPlace(on bool)            { a.lastCompactionInPlace = on }
func (a *AIAgent) SetCurrentStreamedAssistantText(t string)    { a.setCurrentStreamedAssistantText(t) }
func (a *AIAgent) CompressionCheckpointRequired() bool         { return a.compressionCheckpointRequired }
func (a *AIAgent) SetUsageAnchor(anchor *provider.UsageAnchor) { a.setUsageAnchor(anchor, false) }
func (a *AIAgent) EmitCompactionStatus()                       { a.emitCompactionStatus() }
func (a *AIAgent) EmitDiagnosticStatus(text string)            { a.emitDiagnosticStatus(text) }
func (a *AIAgent) PublishProviderNativeCompaction() {
	PublishProviderNativeCompaction(a.homeContext(), a)
}
func (a *AIAgent) EventCallback() func(string, map[string]any) { return a.eventCallback }
func (a *AIAgent) TouchActivityHook() func(desc string)        { return a.TouchActivity }

// Interrupt message, nil standing for None when no message was given
func (a *AIAgent) InterruptMessage() any {
	if a.interruptMessage == "" {
		return nil
	}
	return a.interruptMessage
}

// Session store the codex runtime patches, nil without a session db
func (a *AIAgent) CodexSessionDB() provider.CodexSessionDB {
	if a.sessionDB == nil {
		return nil
	}
	return codexSessionDBFor(a.sessionDB)
}

// Flushes messages to the session db through the session persistence writer
func (a *AIAgent) FlushMessagesToSessionDB(messages []map[string]any) state.FlushOutcome {
	return a.flushMessagesToSessionDB(messages)
}

// Adds a usage count to the session_ prefixed counter the usage key names
func (a *AIAgent) AddSessionUsage(key string, value int) {
	switch "session_" + key {
	case "session_prompt_tokens":
		a.sessionPromptTokens += value
	case "session_completion_tokens":
		a.sessionCompletionTokens += value
	case "session_total_tokens":
		a.sessionTotalTokens += value
	case "session_input_tokens":
		a.sessionInputTokens += value
	case "session_output_tokens":
		a.sessionOutputTokens += value
	case "session_cache_read_tokens":
		a.sessionCacheReadTokens += value
	case "session_cache_write_tokens":
		a.sessionCacheWriteTokens += value
	case "session_reasoning_tokens":
		a.sessionReasoningTokens += value
	default:
		panic("AIAgent has no session usage counter session_" + key)
	}
}

// Context compressor as the codex runtime reads it, nil when the engine lacks that surface
func (a *AIAgent) CodexContextCompressor() provider.CodexContextCompressor {
	cc, _ := a.contextCompressor.(provider.CodexContextCompressor)
	return cc
}

// Progress hook in the codex shape, an executor shaped hook adapted to it
func (a *AIAgent) ToolProgressCallback() codexProgressFn {
	switch cb := progressFn(a.toolProgressCallback).(type) {
	case codexProgressFn:
		return cb
	case toolProgressFn:
		return func(event, name string, preview any, args any, extra *provider.CodexToolProgressExtra) {
			p, _ := preview.(string)
			m, _ := args.(map[string]any)
			cb(event, name, p, m, toolProgressExtra(extra))
		}
	}
	return nil
}

// Start hook in the codex shape, an executor shaped hook adapted to it
func (a *AIAgent) ToolStartCallback() codexStartFn {
	switch cb := a.toolStartCallback.(type) {
	case codexStartFn:
		return cb
	case toolStartFn:
		return func(callID, name string, args any) {
			m, _ := args.(map[string]any)
			cb(callID, name, m)
		}
	}
	return nil
}

// Complete hook in the codex shape, an executor shaped hook adapted to it
func (a *AIAgent) ToolCompleteCallback() codexCompleteFn {
	switch cb := a.toolCompleteCallback.(type) {
	case codexCompleteFn:
		return cb
	case toolCompleteFn:
		return func(callID, name string, args any, result string) {
			m, _ := args.(map[string]any)
			cb(callID, name, m, result)
		}
	}
	return nil
}

// Stream delivery method the attribute names, nil for a name the agent lacks
func (a *AIAgent) StreamDeltaHook(attr string) func(text string) {
	switch attr {
	case "_fire_stream_delta":
		return a.fireStreamDelta
	case "_fire_reasoning_delta":
		return a.fireReasoningDelta
	case "_fire_streamed_codex_commentary":
		return a.fireStreamedCodexCommentary
	}
	return nil
}

func (a *AIAgent) InterimAssistantMessageHook() func(message map[string]any) {
	return a.emitInterimAssistantMessage
}

func (a *AIAgent) SyncExternalMemoryForTurnHook() func(originalUserMessage, finalResponse any, interrupted bool, messages []map[string]any) {
	return a.syncExternalMemoryForTurn
}

func (a *AIAgent) SpawnBackgroundReviewHook() func(messagesSnapshot []map[string]any, reviewMemory, reviewSkills bool) {
	return func(messagesSnapshot []map[string]any, reviewMemory, reviewSkills bool) {
		a.spawnBackgroundReview(context.Background(), messagesSnapshot, reviewMemory, reviewSkills, nil, false)
	}
}

var _ provider.CodexRuntimeAgent = (*AIAgent)(nil)
