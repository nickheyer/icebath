package provider

import (
	"slices"

	"github.com/nickheyer/icebath/internal/provider"
)

// Efforts Ollama's cloud takes, with the level a missing one maps to
var (
	efforts   = []string{"none", "low", "medium", "high", "max"}
	overrides = map[string]string{"xhigh": "max"}
)

// Effort at the top for models Ollama says think: none when thinking is off, nothing unasked, else clamped with xhigh taken as max
func shape(call *provider.Call) (map[string]any, map[string]any) {
	ask := call.Ask
	if !ask.Set || call.OllamaThinks == nil || !call.OllamaThinks() {
		return nil, nil
	}
	if ask.Off || ask.Effort == "none" {
		return nil, map[string]any{"reasoning_effort": "none"}
	}
	if ask.Effort == "" {
		return nil, nil
	}
	if clamped := provider.ClampEffort(ask.Effort, efforts, overrides); slices.Contains(efforts, clamped) {
		return nil, map[string]any{"reasoning_effort": clamped}
	}
	return nil, nil
}
