package agent

import (
	"math"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// MemoryContextMaxChars is the code point cap of memory context crossing a context engine boundary
const MemoryContextMaxChars = 6000

const (
	memoryContextHeadChars = 4000
	memoryContextTailChars = 1500
)

// Provider memory context redacted with URL credentials and elided in the middle past the cap
func SanitizeMemoryContext(env *tools.Env, memoryContext string) string {
	sanitized := tools.RedactSensitiveTextWith(env, pystr.Strip(memoryContext), tools.RedactOptions{Force: true, URLCredentials: true})
	if len([]rune(sanitized)) <= MemoryContextMaxChars {
		return sanitized
	}
	return tools.ElideMiddle(sanitized, memoryContextHeadChars, memoryContextTailChars)
}

// ContextEngine decides when and how the conversation is compacted, tracks usage and may expose tools
type ContextEngine interface {
	Name() string
	Base() *ContextEngineBase
	UpdateFromResponse(usage map[string]any)
	ShouldCompress(promptTokens *int) bool
	Compress(messages []map[string]any, currentTokens *int, focusTopic *string, force bool, memoryContext string) ([]map[string]any, error)
	PruneToolResultsOnly(messages []map[string]any, currentTokens *int) ([]map[string]any, int)
	SelectContext(requestMessages, conversationMessages []map[string]any, incomingMessage map[string]any, budgetTokens int) []map[string]any
	OnTurnComplete(messages []map[string]any, usage map[string]any, kwargs map[string]any)
	ShouldCompressPreflight(messages []map[string]any) bool
	ShouldDeferPreflightToRealUsage(roughTokens int) bool
	GetAutomaticCompactionStatusMessage(phase, defaultMessage string, context map[string]any) *string
	HasContentToCompress(messages []map[string]any) bool
	OnSessionStart(sessionID string, kwargs map[string]any)
	OnSessionEnd(sessionID string, messages []map[string]any)
	OnSessionReset()
	GetToolSchemas() []map[string]any
	HandleToolCall(name string, args map[string]any, kwargs map[string]any) string
	GetStatus() map[string]any
	CloneForAgent() ContextEngine
	UpdateModel(model string, contextLength int, baseURL, apiKey, provider, apiMode string)
}

// ContextEngineInfo is the optional reason reporting form of should_compress an engine may override
type ContextEngineInfo interface {
	ShouldCompressInfo(promptTokens *int) (bool, *string)
}

// ContextEngineBase holds the shared engine state and the default hooks every engine embeds
type ContextEngineBase struct {
	LastPromptTokens              int
	LastCompletionTokens          int
	LastTotalTokens               int
	ThresholdTokens               int
	ContextLength                 int
	CompressionCount              int
	ThresholdPercent              float64
	ProtectFirstN                 int
	ProtectLastN                  int
	EmitAutomaticCompactionStatus bool
	ModelThresholds               map[string]float64
	configThresholdPercent        *float64
	BaseThresholdPercent          float64
}

// Base state with the class defaults of the Python engine
func NewContextEngineBase() ContextEngineBase {
	return ContextEngineBase{ThresholdPercent: 0.75, ProtectFirstN: 3, ProtectLastN: 6, EmitAutomaticCompactionStatus: true}
}

func (b *ContextEngineBase) Base() *ContextEngineBase { return b }

func (b *ContextEngineBase) PruneToolResultsOnly(messages []map[string]any, currentTokens *int) ([]map[string]any, int) {
	return messages, 0
}

// Nil keeps the request messages unchanged
func (b *ContextEngineBase) SelectContext(requestMessages, conversationMessages []map[string]any, incomingMessage map[string]any, budgetTokens int) []map[string]any {
	return nil
}

func (b *ContextEngineBase) OnTurnComplete(messages []map[string]any, usage map[string]any, kwargs map[string]any) {
}

func (b *ContextEngineBase) ShouldCompressPreflight(messages []map[string]any) bool { return false }

func (b *ContextEngineBase) ShouldDeferPreflightToRealUsage(roughTokens int) bool { return false }

// Default status text, nil when the engine suppresses automatic compaction status
func (b *ContextEngineBase) GetAutomaticCompactionStatusMessage(phase, defaultMessage string, context map[string]any) *string {
	if !b.EmitAutomaticCompactionStatus {
		return nil
	}
	return &defaultMessage
}

func (b *ContextEngineBase) HasContentToCompress(messages []map[string]any) bool { return true }

func (b *ContextEngineBase) OnSessionStart(sessionID string, kwargs map[string]any) {}

func (b *ContextEngineBase) OnSessionEnd(sessionID string, messages []map[string]any) {}

// Zeroes the per session token counters and the compression count
func (b *ContextEngineBase) OnSessionReset() {
	b.LastPromptTokens, b.LastCompletionTokens, b.LastTotalTokens, b.CompressionCount = 0, 0, 0, 0
}

func (b *ContextEngineBase) GetToolSchemas() []map[string]any { return []map[string]any{} }

// JSON error for a tool name the engine does not own
func (b *ContextEngineBase) HandleToolCall(name string, args map[string]any, kwargs map[string]any) string {
	obj := pyjson.NewObject()
	obj.Set("error", "Unknown context engine tool: "+name)
	out, _ := pyjson.Dumps(obj)
	return out
}

// Usage summary for display, the percent capped at the integer 100
func (b *ContextEngineBase) GetStatus() map[string]any {
	lastPrompt := max(b.LastPromptTokens, 0)
	var usage any = 0
	if b.ContextLength != 0 {
		pct := float64(lastPrompt) / float64(b.ContextLength) * 100
		if pct < 100 {
			usage = pct
		} else {
			usage = 100
		}
	}
	return map[string]any{
		"last_prompt_tokens": lastPrompt,
		"threshold_tokens":   b.ThresholdTokens,
		"context_length":     b.ContextLength,
		"usage_percent":      usage,
		"compression_count":  b.CompressionCount,
	}
}

// Rebinds the window and recomputes the threshold from the per model table and the first configured percent
func (b *ContextEngineBase) UpdateModel(model string, contextLength int, baseURL, apiKey, provider, apiMode string) {
	b.ContextLength = contextLength
	if b.configThresholdPercent == nil {
		pct := b.ThresholdPercent
		b.configThresholdPercent = &pct
	}
	b.BaseThresholdPercent = resolveModelThreshold(model, b.ModelThresholds, *b.configThresholdPercent, provider)
	b.ThresholdPercent = b.BaseThresholdPercent
	b.ThresholdTokens = int(math.Trunc(float64(contextLength) * b.ThresholdPercent))
}

// Copy of the base state with its own threshold table for a cloned engine
func (b *ContextEngineBase) CloneBase() ContextEngineBase {
	c := *b
	if b.ModelThresholds != nil {
		c.ModelThresholds = make(map[string]float64, len(b.ModelThresholds))
		for k, v := range b.ModelThresholds {
			c.ModelThresholds[k] = v
		}
	}
	if b.configThresholdPercent != nil {
		pct := *b.configThresholdPercent
		c.configThresholdPercent = &pct
	}
	return c
}

// Engine verdict with its reason when it reports one, else the bare should_compress
func ShouldCompressInfo(e ContextEngine, promptTokens *int) (bool, *string) {
	if info, ok := e.(ContextEngineInfo); ok {
		return info.ShouldCompressInfo(promptTokens)
	}
	return e.ShouldCompress(promptTokens), nil
}

// Host visible automatic compaction status, nil to emit nothing
func AutomaticCompactionStatusMessage(e ContextEngine, phase, defaultMessage string, context map[string]any) *string {
	if !e.Base().EmitAutomaticCompactionStatus {
		return nil
	}
	message := e.GetAutomaticCompactionStatusMessage(phase, defaultMessage, context)
	if message == nil {
		return nil
	}
	text := pystr.Strip(*message)
	if text == "" {
		return nil
	}
	return &text
}
