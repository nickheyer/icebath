package gateway

import "github.com/nickheyer/icebath/pkg/internal/pyjson"

// Session usage event payload carrying the TUI usage view and the cumulative counters in one frame
func UsageFrame(stats *pyjson.Object) *pyjson.Object {
	frame := pyjson.ObjectOf("usage", tuiUsage(stats))
	for _, key := range stats.Keys {
		frame.Set(key, stats.Vals[key])
	}
	return frame
}
