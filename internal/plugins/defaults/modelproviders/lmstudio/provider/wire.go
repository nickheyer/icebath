package provider

import (
	"slices"

	"github.com/nickheyer/icebath/internal/provider"
)

// Efforts LM Studio takes, the words it also takes for them, and the levels above its most it maps down
var (
	efforts = []string{"none", "minimal", "low", "medium", "high", "xhigh"}
	spelled = map[string]string{"off": "none", "on": "medium"}
	capped  = map[string]string{"max": "xhigh", "ultra": "xhigh"}
)

// Effort at the top clamped to the options the model publishes, the generic nested reasoning when nothing read them
func shape(call *provider.Call) (map[string]any, map[string]any) {
	if !call.Thinking.Known {
		return provider.NestedReasoning(call.Ask), nil
	}
	if !call.Thinking.Takes {
		return nil, nil
	}
	if effort, ok := resolve(call.Ask, call.Thinking.Efforts); ok {
		return nil, map[string]any{"reasoning_effort": effort}
	}
	return nil, nil
}

// Effort LM Studio is sent for the ask: medium unasked, none when off, else the level asked when it takes it; false when the model publishes
// options that don't hold it
func resolve(ask provider.Ask, allowed []string) (string, bool) {
	effort := "medium"
	if ask.Set {
		raw := ask.Effort
		if ask.Off {
			raw = "none"
		}
		if word, ok := spelled[raw]; ok {
			raw = word
		}
		if level, ok := capped[raw]; ok {
			raw = level
		}
		if slices.Contains(efforts, raw) {
			effort = raw
		}
	}
	if len(allowed) == 0 {
		return effort, true
	}
	for _, option := range allowed {
		if word, ok := spelled[option]; ok {
			option = word
		}
		if option == effort {
			return effort, true
		}
	}
	return "", false
}
