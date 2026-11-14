package agent

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Clears the kanban task and stop nudge env signals
func clearKanbanEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{"ICEBATH_KANBAN_TASK", "ICEBATH_KANBAN_STOP_NUDGE", "ICEBATH_DELEGATED_CHILD_CONTEXT", "ICEBATH_NON_DISPATCHER_OWNED_CONTEXT"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
}

// Kanban stop nudge with the default attempts and no explicit task id
func kanbanNudge(ctx context.Context, messages []map[string]any) string {
	return buildKanbanStopNudge(ctx, messages, 0, defaultKanbanStopMaxAttempts, "")
}

// Assistant call of one tool followed by its tool result
func kanbanToolExchange(content, toolName, result string) []map[string]any {
	return []map[string]any{
		{"role": "assistant", "content": content, "tool_calls": []any{
			map[string]any{"id": "1", "type": "function", "function": map[string]any{"name": toolName, "arguments": "{}"}},
		}},
		{"role": "tool", "name": toolName, "tool_call_id": "1", "content": result},
	}
}

func TestEnvCanDisable(t *testing.T) {
	clearKanbanEnv(t)
	t.Setenv("ICEBATH_KANBAN_TASK", "t_abc")
	t.Setenv("ICEBATH_KANBAN_STOP_NUDGE", "0")
	ctx := context.Background()
	if kanbanStopNudgeEnabled(ctx) {
		t.Fatal("env 0 must disable the nudge")
	}
	if nudge := kanbanNudge(ctx, nil); nudge != "" {
		t.Fatalf("nudge = %q", nudge)
	}
}

func TestNudgeDisabledInsideDelegatedChild(t *testing.T) {
	clearKanbanEnv(t)
	t.Setenv("ICEBATH_KANBAN_TASK", "t_parent")
	ctx := context.Background()
	if !kanbanStopNudgeEnabled(ctx) {
		t.Fatal("owner must see the nudge enabled")
	}
	child := tools.WithDelegatedChildContext(ctx, true)
	if kanbanStopNudgeEnabled(child) {
		t.Fatal("delegated child must see the nudge disabled")
	}
	if nudge := kanbanNudge(child, nil); nudge != "" {
		t.Fatalf("nudge = %q", nudge)
	}
	if !kanbanStopNudgeEnabled(ctx) {
		t.Fatal("leaving the child context must re-enable the nudge")
	}
}

func TestNudgeDisabledInsideNonDispatcherContext(t *testing.T) {
	clearKanbanEnv(t)
	t.Setenv("ICEBATH_KANBAN_TASK", "t_parent")
	env := &tools.Env{Values: &sync.Map{}}
	ctx := tools.WithToolEnv(context.Background(), env)
	if !kanbanStopNudgeEnabled(ctx) {
		t.Fatal("owner must see the nudge enabled")
	}
	restore := tools.EnterNonDispatcherOwnedContext(env)
	if kanbanStopNudgeEnabled(ctx) {
		t.Fatal("non dispatcher context must see the nudge disabled")
	}
	if nudge := kanbanNudge(ctx, nil); nudge != "" {
		t.Fatalf("nudge = %q", nudge)
	}
	restore()
	if !kanbanStopNudgeEnabled(ctx) {
		t.Fatal("leaving the non dispatcher context must re-enable the nudge")
	}
}

func TestNudgeWhenNoTerminalTool(t *testing.T) {
	clearKanbanEnv(t)
	t.Setenv("ICEBATH_KANBAN_TASK", "t_46be8aa5")
	messages := append([]map[string]any{{"role": "user", "content": "work kanban task"}},
		kanbanToolExchange("Let me write the comprehensive recipe.", "kanban_heartbeat", "ok")...)
	nudge := kanbanNudge(context.Background(), messages)
	for _, want := range []string{"kanban_complete", "kanban_block", "t_46be8aa5"} {
		if !strings.Contains(nudge, want) {
			t.Fatalf("nudge lacks %q: %q", want, nudge)
		}
	}
}

func TestNoNudgeAfterKanbanComplete(t *testing.T) {
	clearKanbanEnv(t)
	t.Setenv("ICEBATH_KANBAN_TASK", "t_abc")
	messages := kanbanToolExchange("", "kanban_complete", "done")
	if !sessionCalledKanbanTerminal(messages) {
		t.Fatal("kanban_complete must count as terminal")
	}
	if nudge := kanbanNudge(context.Background(), messages); nudge != "" {
		t.Fatalf("nudge = %q", nudge)
	}
}

func TestNoNudgeAfterHandoffTool(t *testing.T) {
	for _, tc := range []struct{ toolName, who string }{
		{"kanban_request_review", "build worker handing off for same-card review"},
		{"kanban_request_changes", "review agent sending the card back"},
		{"kanban_schedule", "worker parking the card on a timed wait"},
	} {
		t.Run(tc.toolName, func(t *testing.T) {
			clearKanbanEnv(t)
			t.Setenv("ICEBATH_KANBAN_TASK", "t_handoff")
			messages := kanbanToolExchange("", tc.toolName, "ok")
			if !sessionCalledKanbanTerminal(messages) {
				t.Fatal(tc.who)
			}
			if nudge := kanbanNudge(context.Background(), messages); nudge != "" {
				t.Fatalf("nudge = %q", nudge)
			}
		})
	}
}

func TestNudgeStillFiresForNonTerminalKanbanTool(t *testing.T) {
	clearKanbanEnv(t)
	t.Setenv("ICEBATH_KANBAN_TASK", "t_abc")
	messages := kanbanToolExchange("Let me open the review next.", "kanban_comment", "ok")
	if sessionCalledKanbanTerminal(messages) {
		t.Fatal("kanban_comment is not terminal")
	}
	nudge := kanbanNudge(context.Background(), messages)
	if !strings.Contains(nudge, "kanban_request_review") || !strings.Contains(nudge, "kanban_block") {
		t.Fatalf("nudge = %q", nudge)
	}
}

func TestSessionCalledKanbanTerminalReadsTypedToolCalls(t *testing.T) {
	call := state.ToolCall{ID: "1", Type: "function", Function: state.ToolCallFunction{Name: "kanban_block", Arguments: "{}"}}
	for _, calls := range []any{[]state.ToolCall{call}, []any{call}, []any{&call}} {
		if !sessionCalledKanbanTerminal([]map[string]any{{"role": "assistant", "tool_calls": calls}}) {
			t.Fatalf("typed tool calls %T must count as terminal", calls)
		}
	}
	other := state.ToolCall{Function: state.ToolCallFunction{Name: "kanban_comment"}}
	if sessionCalledKanbanTerminal([]map[string]any{{"role": "assistant", "tool_calls": []any{other, (*state.ToolCall)(nil)}}}) {
		t.Fatal("kanban_comment is not terminal")
	}
}
