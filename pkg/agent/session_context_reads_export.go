package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
)

// What a context breakdown reads off an agent besides the transcript
type contextMarks struct {
	model, provider                                   string
	tools                                             int
	hasCompressor                                     bool
	contextLength, lastPromptTokens, compressionCount int
	turnBase, usage                                   provider.UsageAnchor
	hasTurnBase, hasUsage                             bool
}

// Session usage stats of an agent value with its window plan gauge, the zero shape for no agent
func SessionUsageStats(ctx context.Context, value any) *pyjson.Object {
	a, _ := value.(*AIAgent)
	stats := UsageStats(ctx, a)
	if a == nil {
		return stats
	}
	if capture := a.CreditsCapture(); capture != nil {
		if gauge := gateway.PlanGauge(capture.Credits()); gauge != nil {
			stats.Set("plan_gauge", gauge)
		}
	}
	return stats
}

// Ordered dict of a JSON tagged value, the Python dict the breakdown helpers return
func objectOfTagged(value any) (any, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return pyjson.Loads(string(raw))
}

// Category breakdown of a live agent over its model history with its context file manifest
func SessionContextBreakdown(ctx context.Context, value any, conversation []map[string]any) (*pyjson.Object, error) {
	a, ok := value.(*AIAgent)
	if !ok || a == nil {
		return nil, fmt.Errorf("AttributeError: %T has no context breakdown", value)
	}
	payload, err := ComputeSessionContextBreakdown(ctx, a, conversation)
	if err != nil {
		return nil, err
	}
	loaded, err := objectOfTagged(payload)
	if err != nil {
		return nil, err
	}
	out, _ := loaded.(*pyjson.Object)
	sources, err := ContextFileSourcesForAgent(ctx, a)
	if err != nil {
		return nil, err
	}
	files, err := objectOfTagged(sources)
	if err != nil {
		return nil, err
	}
	out.Set("context_files", files)
	return out, nil
}

// Categories and estimated total of a frozen system prompt and tool schemas
func SessionStoredContextBreakdown(ctx context.Context, systemPrompt string, tools []map[string]any, conversation []map[string]any) (*pyjson.Object, error) {
	payload := StoredContextBreakdown(ctx, systemPrompt, tools, conversation)
	categories, err := objectOfTagged(payload.Categories)
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("categories", categories, "estimated_total", payload.EstimatedTotal), nil
}

// Comparable marks of model, toolset size, window, measured tokens, compressions and usage anchors
func SessionContextMarks(value any) any {
	a, ok := value.(*AIAgent)
	if !ok || a == nil {
		return contextMarks{}
	}
	marks := contextMarks{model: a.model, provider: a.provider, tools: len(a.tools)}
	if a.contextCompressor != nil {
		base := a.contextCompressor.Base()
		marks.hasCompressor = true
		marks.contextLength, marks.lastPromptTokens, marks.compressionCount = base.ContextLength, base.LastPromptTokens, base.CompressionCount
	}
	if turnBase := a.turnBaseUsageAnchor.Load(); turnBase != nil {
		marks.turnBase, marks.hasTurnBase = *turnBase, true
	}
	if a.usageAnchor != nil {
		marks.usage, marks.hasUsage = *a.usageAnchor, true
	}
	return marks
}
