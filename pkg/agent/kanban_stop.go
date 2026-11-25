package agent

import (
	"context"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

const defaultKanbanStopMaxAttempts = 2

// Every tool that ends this worker's responsibility for the card
var terminalKanbanTools = map[string]bool{
	"kanban_complete": true, "kanban_block": true, "kanban_schedule": true,
	"kanban_request_review": true, "kanban_request_changes": true,
}

// On for the dispatcher owned worker unless the kanban.stop_nudge setting turns it off
func kanbanStopNudgeEnabled(ctx context.Context) bool {
	return settings.KanbanStopNudge.Get(ctx) && tools.OwnedKanbanTask(ctx) != ""
}

// Tool name from a dict, a decoded object or a typed tool call, function.name first and then name
func kanbanToolCallName(tc any) string {
	switch call := tc.(type) {
	case state.ToolCall:
		return call.Function.Name
	case *state.ToolCall:
		if call == nil {
			return ""
		}
		return call.Function.Name
	case map[string]any, *jsonutil.Object:
		name := state.ToolCallField(call, "name")
		if fn, ok := jsonutil.AsMap(state.ToolCallField(call, "function")); ok {
			name = fn["name"]
		}
		if !anyutil.IsTrue(name) {
			return ""
		}
		return anyutil.String(name)
	}
	return ""
}

// Items of a dict, typed tool call or any list value
func anyItems(v any) []any {
	switch xs := v.(type) {
	case []any:
		return xs
	case []map[string]any:
		out := make([]any, len(xs))
		for i, x := range xs {
			out[i] = x
		}
		return out
	case []state.ToolCall:
		out := make([]any, len(xs))
		for i, x := range xs {
			out[i] = x
		}
		return out
	}
	return nil
}

// True when this conversation already invoked a terminal kanban tool
func sessionCalledKanbanTerminal(messages []map[string]any) bool {
	for _, msg := range messages {
		if msg == nil {
			continue
		}
		switch msg["role"] {
		case "assistant":
			for _, tc := range anyItems(msg["tool_calls"]) {
				if terminalKanbanTools[kanbanToolCallName(tc)] {
					return true
				}
			}
		case "tool":
			if name := msg["name"]; anyutil.IsTrue(name) && terminalKanbanTools[anyutil.String(name)] {
				return true
			}
		}
	}
	return false
}

// Synthetic follow up when a kanban worker stops without a terminal tool, empty when the guard stays quiet
func buildKanbanStopNudge(ctx context.Context, messages []map[string]any, attempts, maxAttempts int, taskID string) string {
	if !kanbanStopNudgeEnabled(ctx) || attempts >= maxAttempts || sessionCalledKanbanTerminal(messages) {
		return ""
	}
	tid := taskID
	if tid == "" {
		tid = os.Getenv("ICEBATH_KANBAN_TASK")
	}
	if tid = strings.TrimSpace(tid); tid == "" {
		tid = "this task"
	}
	return "[System: You are an Icebath kanban worker. A plain-text reply is NOT a " +
		"terminal state for the board.\n\n" +
		"Task `" + tid + "` has not been handed off: this session made no terminal board " +
		"call (`kanban_complete` / `kanban_request_review` / `kanban_block`). Ending now " +
		"causes a protocol violation (clean exit with the card still `running`).\n\n" +
		"Do this immediately in your next response — do not narrate intent:\n" +
		"1. Finish any remaining deliverable (write the required file(s) now).\n" +
		"2. Call `kanban_complete(summary=..., artifacts=[...])` if the work is done " +
		"and needs no review, `kanban_request_review(summary=...)` if it is a code " +
		"change that needs same-card review, OR `kanban_block(reason=...)` if you are " +
		"blocked. Reviewers approve with `kanban_complete` or send the card back with " +
		"`kanban_request_changes(reason=...)`.\n\n" +
		"Never end a turn with only a promise of future action. Repeated " +
		"protocol violations will block this task and require manual intervention.]"
}
