package provider

import (
	"slices"

	"github.com/nickheyer/icebath/internal/provider"
)

// Efforts Meta's models take
var efforts = []string{"minimal", "low", "medium", "high", "xhigh"}

// Effort at the top always: minimal when thinking is off, medium unasked or for a level it doesn't take
func shape(call *provider.Call) (map[string]any, map[string]any) {
	if call.Ask.Off {
		return nil, map[string]any{"reasoning_effort": "minimal"}
	}
	effort := "medium"
	if clamped := provider.ClampEffort(call.Ask.Effort, efforts, nil); slices.Contains(efforts, clamped) {
		effort = clamped
	}
	return nil, map[string]any{"reasoning_effort": effort}
}
