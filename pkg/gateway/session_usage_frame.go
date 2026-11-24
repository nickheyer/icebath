package gateway

import (
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Session usage event payload carrying the TUI usage view and the cumulative counters in one frame
func UsageFrame(stats *jsonutil.Object) *jsonutil.Object {
	frame := jsonutil.ObjectOf("usage", tuiUsage(stats))
	for _, key := range stats.Keys() {
		frame.Set(key, stats.Value(key))
	}
	return frame
}
