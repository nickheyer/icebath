package provider

import "github.com/nickheyer/icebath/internal/provider"

// Effort at the top as asked, none when thinking is turned off, nothing when unasked
func shape(call *provider.Call) (map[string]any, map[string]any) {
	switch ask := call.Ask; {
	case !ask.Set, !ask.Off && ask.Effort == "":
		return nil, nil
	case ask.Off:
		return nil, map[string]any{"reasoning_effort": "none"}
	default:
		return nil, map[string]any{"reasoning_effort": ask.Effort}
	}
}
