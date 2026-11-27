package provider

import (
	"context"
	"slices"

	"github.com/nickheyer/icebath/internal/provider"
)

// Efforts Actual's relay takes
var relayEfforts = []string{"none", "low", "medium", "high", "max"}

// Thinking toggle of the relay, where effort none is a level of its own that also turns thinking off
func shape(call *provider.Call) (map[string]any, map[string]any) {
	switch ask := call.Ask; {
	case !ask.Set:
		return nil, nil
	case !ask.Off && ask.Effort == "none":
		return map[string]any{"thinking": map[string]any{"type": "disabled"}}, map[string]any{"reasoning_effort": "none"}
	default:
		return provider.ThinkingToggle(ask, relayEfforts, nil, true)
	}
}

func efforts(context.Context, string) ([]string, bool) {
	return slices.Clone(relayEfforts), true
}
