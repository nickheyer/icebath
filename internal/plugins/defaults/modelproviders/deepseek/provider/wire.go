package provider

import (
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/provider"
)

// Models that think without a version in their id
var thinking = []string{"deepseek-flash"}

// Ids DeepSeek retired, and the one each now goes to
var retired = map[string]string{"deepseek-chat": "deepseek-flash", "deepseek-reasoner": "deepseek-flash"}

// Thinking toggle with the effort for V4 and later and the models that think without a version, thinking off when it is turned off
func shape(call *provider.Call) (map[string]any, map[string]any) {
	model := strings.ToLower(strings.TrimSpace(call.Model))
	versioned := strings.HasPrefix(model, "deepseek-v") && !strings.HasPrefix(model, "deepseek-v3")
	if !versioned && !slices.Contains(thinking, model) {
		return nil, nil
	}
	return provider.ThinkingToggle(call.Ask, provider.DeepSeekEfforts, provider.DeepSeekOverrides, true)
}

// Model id as DeepSeek names it: its own prefix dropped, in lower case, a retired id moved to the one replacing it
func native(spec provider.Spec, model string) string {
	bare := provider.Unprefixed(spec, model)
	if strings.Contains(bare, "/") {
		return bare
	}
	bare = strings.ToLower(bare)
	if replacement, ok := retired[bare]; ok {
		return replacement
	}
	return bare
}
