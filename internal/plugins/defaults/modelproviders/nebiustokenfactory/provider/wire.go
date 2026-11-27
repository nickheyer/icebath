package provider

import (
	"strings"

	"github.com/nickheyer/icebath/internal/provider"
)

// Id parts of Nebius's reasoning families, and the efforts they take
var (
	reasoners = []string{"deepseek-r1", "deepseek-v4", "deepseek-reasoner", "gpt-oss", "glm-5", "kimi-k2", "minimax-m2", "qwen3"}
	efforts   = []string{"low", "medium", "high"}
)

// Effort at the top for the reasoning families, medium unasked, clamped onto low, medium and high; nothing when thinking is off
func shape(call *provider.Call) (map[string]any, map[string]any) {
	model := strings.TrimSpace(call.Model)
	name := strings.ToLower(model[strings.LastIndex(model, "/")+1:])
	if !call.Thinking.Takes && !provider.Contains(name, reasoners...) {
		return nil, nil
	}
	effort := call.Ask.Effort
	if effort == "" {
		effort = "medium"
	}
	if call.Ask.Off || effort == "none" || effort == "off" || effort == "disabled" {
		return nil, nil
	}
	clamped := provider.ClampEffort(effort, efforts, nil)
	if clamped == "" {
		clamped = "medium"
	}
	return nil, map[string]any{"reasoning_effort": clamped}
}
