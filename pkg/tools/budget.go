package tools

import (
	"context"
	"math"
)

// Characters per token of every rough model estimate, the one ratio provider, media and agent derive theirs from
const ModelCharsPerToken = 4

const (
	DefaultResultSizeChars    = 100000
	DefaultTurnBudgetChars    = 200000
	DefaultPreviewSizeChars   = 1500
	DefaultMCPResultSizeChars = 50000
	MCPToolPrefix             = "mcp_"
	// BudgetUnlimited is the infinite threshold that never persists a result
	BudgetUnlimited = math.MaxInt

	perResultWindowFraction = 0.15
	perTurnWindowFraction   = 0.30
	minResultSizeChars      = 8000
	minTurnBudgetChars      = 16000
)

// Never overridden thresholds, read_file unlimited prevents persist read persist loops
var PinnedThresholds = map[string]int{"read_file": BudgetUnlimited}

// BudgetConfig holds the per result, per turn, and preview size budgets for tool result persistence
type BudgetConfig struct {
	DefaultResultSize int
	TurnBudget        int
	PreviewSize       int
	MCPResultSize     int
	ToolOverrides     map[string]int
}

// DefaultBudget matches the historical hardcoded behavior exactly
var DefaultBudget = BudgetConfig{
	DefaultResultSize: DefaultResultSizeChars,
	TurnBudget:        DefaultTurnBudgetChars,
	PreviewSize:       DefaultPreviewSizeChars,
	MCPResultSize:     DefaultMCPResultSizeChars,
}

// Configured tool_budget.mcp_result_size_chars, the built in default on any issue
func configuredMCPResultSize(env *Env) int {
	block := cfgTree(env).Map("tool_budget")
	if block == nil {
		return DefaultMCPResultSizeChars
	}
	if raw, ok := block["mcp_result_size_chars"]; ok && raw != nil {
		if n, isInt := asIntValue(raw); isInt && n > 0 {
			return n
		}
	}
	return DefaultMCPResultSizeChars
}

// Threshold for one tool, pinned then overrides then mcp prefix then registry then default
func (c BudgetConfig) ResolveThreshold(env *Env, toolName string) int {
	if pinned, ok := PinnedThresholds[toolName]; ok {
		return pinned
	}
	if override, ok := c.ToolOverrides[toolName]; ok {
		return override
	}
	if len(toolName) >= len(MCPToolPrefix) && toolName[:len(MCPToolPrefix)] == MCPToolPrefix {
		return min(c.MCPResultSize, c.DefaultResultSize)
	}
	registryValue := Default.MaxResultSize(context.Background(), ScopeKey(envHome(env)), toolName, c.DefaultResultSize)
	if registryValue < 0 || registryValue == BudgetUnlimited {
		return BudgetUnlimited
	}
	return min(registryValue, c.DefaultResultSize)
}

// BudgetForContextWindow scales the budget to the model's context window, capped at the defaults and floored
func BudgetForContextWindow(env *Env, contextLength int) BudgetConfig {
	mcpResultSize := configuredMCPResultSize(env)
	if contextLength <= 0 {
		if mcpResultSize == DefaultMCPResultSizeChars {
			return DefaultBudget
		}
		cfg := DefaultBudget
		cfg.MCPResultSize = mcpResultSize
		return cfg
	}
	windowChars := contextLength * ModelCharsPerToken
	return BudgetConfig{
		DefaultResultSize: max(minResultSizeChars, min(int(float64(windowChars)*perResultWindowFraction), DefaultResultSizeChars)),
		TurnBudget:        max(minTurnBudgetChars, min(int(float64(windowChars)*perTurnWindowFraction), DefaultTurnBudgetChars)),
		PreviewSize:       DefaultPreviewSizeChars,
		MCPResultSize:     mcpResultSize,
	}
}
