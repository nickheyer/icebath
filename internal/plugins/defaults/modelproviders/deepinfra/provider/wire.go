package provider

import (
	"slices"

	"github.com/nickheyer/icebath/internal/provider"
)

// Effort at the top on the OpenAI compatible vocabulary, none the only way to turn thinking off and an unasked effort left out
func shape(call *provider.Call) (map[string]any, map[string]any) {
	if call.Ask.Set && call.Ask.Off {
		return nil, map[string]any{"reasoning_effort": "none"}
	}
	if clamped := provider.ClampEffort(call.Ask.Requested(), provider.Efforts, nil); slices.Contains(provider.Efforts, clamped) {
		return nil, map[string]any{"reasoning_effort": clamped}
	}
	return nil, nil
}
