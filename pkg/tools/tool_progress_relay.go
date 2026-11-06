package tools

import (
	"context"
	"math"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Event kind a running tool's progress reaches the agent's progress callback as
const ToolProgressEvent = "tool.progress"

// Minimum spacing between relayed progress updates of one call
var toolProgressMinInterval = 500 * time.Millisecond

// Monotonic clock the progress throttle reads, swapped by tests
var toolProgressNow = func() time.Duration { return time.Since(toolProgressEpoch) }

var toolProgressEpoch = time.Now().Add(-time.Second)

// Progress sink of one running tool call, total nil when the server sent none
type ProgressReporter func(progress float64, total *float64, message string)

type toolProgressKey struct{}

// Relays one call's progress as throttled tool.progress callbacks, the first and the final update always passing
func toolProgressReporter(callback func(event, name, preview string, args map[string]any, extra map[string]any), toolName, toolCallID string) ProgressReporter {
	var mu sync.Mutex
	var last time.Duration
	return func(progress float64, total *float64, message string) {
		done := total != nil && *total > 0 && progress >= *total
		now := toolProgressNow()
		mu.Lock()
		if last != 0 && !done && now-last < toolProgressMinInterval {
			mu.Unlock()
			return
		}
		last = now
		mu.Unlock()
		var percent, totalVal any
		if total != nil {
			totalVal = *total
			if *total != 0 {
				percent = int(math.RoundToEven(math.Min(math.Max(progress / *total, 0), 1) * 100))
			}
		}
		defer func() {
			if r := recover(); r != nil {
				toolsLogger(nil).Debug("tool.progress callback failed for "+toolName, "panic", r)
			}
		}()
		callback(ToolProgressEvent, toolName, message, nil, map[string]any{
			"tool_call_id": toolCallID, "progress": progress, "total": totalVal, "percent": percent})
	}
}

// Binds the call's relay for the dispatch, no reporter when the agent has no progress callback
func BoundToolProgress(ctx context.Context, callback func(event, name, preview string, args map[string]any, extra map[string]any), toolName, toolCallID string) context.Context {
	var reporter ProgressReporter
	if callback != nil {
		reporter = toolProgressReporter(callback, toolName, toolCallID)
	}
	return context.WithValue(ctx, toolProgressKey{}, reporter)
}

// The running call's reporter, nil outside a dispatched tool call
func CurrentToolProgress(ctx context.Context) ProgressReporter {
	if ctx == nil {
		return nil
	}
	reporter, _ := ctx.Value(toolProgressKey{}).(ProgressReporter)
	return reporter
}

// Wire payload every viewer surface publishes for tool.progress, percent 0 to 100 only when a total was sent
func ToolProgressPayload(name, message string, kwargs map[string]any) map[string]any {
	progress := 0.0
	switch v := kwargs["progress"].(type) {
	case float64:
		progress = v
	case float32:
		progress = float64(v)
	case int:
		progress = float64(v)
	case int32:
		progress = float64(v)
	case int64:
		progress = float64(v)
	case uint:
		progress = float64(v)
	case uint64:
		progress = float64(v)
	case bool:
		if v {
			progress = 1
		}
	}
	var msg any
	if message != "" {
		msg = message
	}
	toolID := ""
	if v := kwargs["tool_call_id"]; pyTruthy(v) {
		toolID = pyrepr.Str(v)
	}
	return map[string]any{"tool_id": toolID, "name": name, "progress": progress, "total": kwargs["total"],
		"percent": kwargs["percent"], "message": msg}
}
