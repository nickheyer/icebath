package provider

import "github.com/nickheyer/icebath/internal/provider"

// Efforts TokenHub takes
var efforts = []string{"low", "medium", "high"}

// Reasoning a request to TokenHub asks for when it asks none
func defaultReasoning(*provider.Call) provider.Ask {
	return *provider.Effort("high")
}

// Effort at the top clamped onto low, medium and high, high when unasked; nothing when thinking is off
func shape(call *provider.Call) (map[string]any, map[string]any) {
	if !call.Ask.Set || call.Ask.Off {
		return nil, nil
	}
	effort := "high"
	if requested := call.Ask.Requested(); requested != "" {
		effort = provider.ClampEffort(requested, efforts, nil)
	}
	return nil, map[string]any{"reasoning_effort": effort}
}
