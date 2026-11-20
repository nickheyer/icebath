package agent

import (
	"testing"
)

// Scripted tool round calling web_search once with empty arguments
func scriptToolRound(callID string) map[string]any {
	call := map[string]any{"id": callID, "type": "function", "function": map[string]any{"name": "web_search", "arguments": "{}"}}
	return map[string]any{"model": "test/model", "usage": nil,
		"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "", "tool_calls": []any{call}}, "finish_reason": "tool_calls"}}}
}

// Runs one turn of the workbook request over the scripted stages with tool work answering ok
func degenerateRun(t *testing.T, stages ...map[string]any) (*thinkingScript, map[string]any) {
	t.Helper()
	script := &thinkingScript{responses: stages}
	a, ctx := toolLoopAgent(t, script, 90, "ok")
	a.compressionEnabled = false
	a.intentAckContinuation = true
	result, err := a.RunConversation(ctx, "build the workbook", RunConversationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return script, result
}

func scriptUserRowsSent(body map[string]any) []any {
	var out []any
	rows, _ := body["messages"].([]any)
	for _, r := range rows {
		if m, _ := r.(map[string]any); m["role"] == "user" {
			out = append(out, m["content"])
		}
	}
	return out
}

func TestFragmentAfterToolWork(t *testing.T) {
	t.Run("test_fragment_after_tool_work_reprompts_instead_of_exiting", func(t *testing.T) {
		script, result := degenerateRun(t, scriptToolRound("call_1"), thinkingMockResponse("пар", "stop"), thinkingMockResponse("Workbook built: 3 sheets.", "stop"))
		calls := script.calls()
		if result["final_response"] != "Workbook built: 3 sheets." || len(calls) != 3 {
			t.Fatalf("final %v calls %d", result["final_response"], len(calls))
		}
		if users := scriptUserRowsSent(calls[2]); len(users) == 0 || users[len(users)-1] != degenerateFinalNudge {
			t.Fatalf("user rows of the re-prompt %v", users)
		}
		fragment, nudge := -1, -1
		for i, m := range resultRows(t, result) {
			switch m["content"] {
			case "пар":
				if fragment < 0 {
					fragment = i
				}
			case degenerateFinalNudge:
				if nudge < 0 {
					nudge = i
				}
			}
		}
		if fragment < 0 || fragment+1 != nudge {
			t.Fatalf("fragment row %d nudge row %d", fragment, nudge)
		}
	})
	t.Run("test_persistent_collapse_is_bounded_and_keeps_the_fragment", func(t *testing.T) {
		script, result := degenerateRun(t, scriptToolRound("call_1"), thinkingMockResponse("пар", "stop"), thinkingMockResponse("?warming up", "stop"))
		if len(script.calls()) != 3 || result["final_response"] != "?warming up" {
			t.Fatalf("final %v calls %d", result["final_response"], len(script.calls()))
		}
	})
	t.Run("test_fragment_without_tool_work_is_a_plain_answer", func(t *testing.T) {
		script, result := degenerateRun(t, thinkingMockResponse("пар", "stop"))
		if len(script.calls()) != 1 || result["final_response"] != "пар" {
			t.Fatalf("final %v calls %d", result["final_response"], len(script.calls()))
		}
	})
}
