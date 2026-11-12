package agent

import (
	"context"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"os"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

const defaultKanbanStopMaxAttempts = 2

// Every tool that ends this worker's responsibility for the card
var terminalKanbanTools = map[string]bool{
	"kanban_complete": true, "kanban_block": true, "kanban_schedule": true,
	"kanban_request_review": true, "kanban_request_changes": true,
}

// On for the dispatcher owned worker unless ICEBATH_KANBAN_STOP_NUDGE disables it
func kanbanStopNudgeEnabled(ctx context.Context) bool {
	switch pystr.Lower(pystr.Strip(os.Getenv("ICEBATH_KANBAN_STOP_NUDGE"))) {
	case "0", "false", "no", "off":
		return false
	}
	return tools.OwnedKanbanTask(ctx) != ""
}

// Tool name from a dict or typed tool call, function.name first and then name
func kanbanToolCallName(tc any) string {
	switch call := tc.(type) {
	case state.ToolCall:
		return call.Function.Name
	case *state.ToolCall:
		if call == nil {
			return ""
		}
		return call.Function.Name
	case map[string]any:
		name := call["name"]
		if fn, ok := pyjson.Dict(call["function"]); ok {
			name = fn["name"]
		}
		if !pyTruthy(name) {
			return ""
		}
		return pyrepr.Str(name)
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
			if name := msg["name"]; pyTruthy(name) && terminalKanbanTools[pyrepr.Str(name)] {
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
	if tid = pystr.Strip(tid); tid == "" {
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
