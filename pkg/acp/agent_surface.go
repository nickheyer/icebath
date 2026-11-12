package acp

import (
	"context"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/state"
)

// Agent whose cached system prompt can be dropped so the next turn rebuilds it
type systemPromptInvalidator interface {
	InvalidateSystemPrompt(ctx context.Context) error
}

var _ systemPromptInvalidator = (*agent.AIAgent)(nil)

// Model of a session agent, empty when the agent exposes none
func agentModel(a any) string {
	if m, ok := a.(interface{ Model() string }); ok {
		return m.Model()
	}
	return ""
}

// Provider of a session agent, empty when the agent exposes none
func agentProvider(a any) string {
	if m, ok := a.(interface{ Provider() string }); ok {
		return m.Provider()
	}
	return ""
}

// Base URL of a session agent, empty when the agent exposes none
func agentBaseURL(a any) string {
	if m, ok := a.(interface{ BaseURL() string }); ok {
		return m.BaseURL()
	}
	return ""
}

// API mode of a session agent, empty when the agent exposes none
func agentAPIMode(a any) string {
	if m, ok := a.(interface{ APIMode() string }); ok {
		return m.APIMode()
	}
	return ""
}

// API key of a session agent, empty when the agent exposes none
func agentAPIKey(a any) string {
	if m, ok := a.(interface{ APIKey() string }); ok {
		return m.APIKey()
	}
	return ""
}

// Internal session head of an agent, def when the agent exposes none
func agentSessionID(a any, def string) string {
	if m, ok := a.(interface{ SessionID() string }); ok {
		return m.SessionID()
	}
	return def
}

// Enabled toolsets of an agent, nil when the agent exposes none
func agentEnabledToolsets(a any) []string {
	if m, ok := a.(interface{ EnabledToolsets() []string }); ok {
		return m.EnabledToolsets()
	}
	return nil
}

// Disabled toolsets of an agent, nil when the agent exposes none
func agentDisabledToolsets(a any) []string {
	if m, ok := a.(interface{ DisabledToolsets() []string }); ok {
		return m.DisabledToolsets()
	}
	return nil
}

// Whether the agent itself flushes its transcript to db
func agentOwnsPersistence(a any, db *state.SessionDB) bool {
	holder, ok := a.(interface{ SessionDB() *state.DB })
	if !ok || db == nil || holder.SessionDB() != db {
		return false
	}
	created, ok := a.(interface{ SessionDBCreated() bool })
	return ok && created.SessionDBCreated()
}

// Cached system prompt of an agent, empty when the agent exposes none
func agentCachedSystemPrompt(a any) string {
	if m, ok := a.(interface{ CachedSystemPrompt() string }); ok {
		return m.CachedSystemPrompt()
	}
	return ""
}

// Tool definitions of an agent, nil when the agent exposes none
func agentToolDefs(a any) []map[string]any {
	if m, ok := a.(interface{ ToolDefs() []map[string]any }); ok {
		return m.ToolDefs()
	}
	return nil
}

// Context window, compression threshold and last prompt tokens of the agent's context engine, zeros without one
func agentCompressorFigures(a any) (contextLength, thresholdTokens, lastPromptTokens int) {
	if m, ok := a.(interface{ ContextCompressor() agent.ContextEngine }); ok {
		if engine := m.ContextCompressor(); engine != nil {
			if base := engine.Base(); base != nil {
				return base.ContextLength, base.ThresholdTokens, base.LastPromptTokens
			}
		}
	}
	return 0, 0, 0
}

// False only when the agent reports automatic compaction disabled
func agentCompressionEnabled(a any) bool {
	if m, ok := a.(interface{ CompressionEnabled() bool }); ok {
		return m.CompressionEnabled()
	}
	return true
}
