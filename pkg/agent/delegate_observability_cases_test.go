package agent

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Child whose conversation returns the scripted result, the MockAgent of the observability tests
func scriptedResultChild(t *testing.T, env *tools.Env, model string, promptTokens, completionTokens int, result map[string]any) {
	t.Helper()
	scriptedCostChild(t, env, model, promptTokens, completionTokens, 0, result)
}

// Scripted child that also carries an estimated session cost
func scriptedCostChild(t *testing.T, env *tools.Env, model string, promptTokens, completionTokens int, cost float64, result map[string]any) {
	t.Helper()
	prevNew := newChildAIAgentFn
	newChildAIAgentFn = func(context.Context, AIAgentOptions) (*AIAgent, error) {
		child := &AIAgent{model: model, env: env, delegateDepth: 1, platform: "subagent", quietMode: true, suppressStatusOutput: true,
			sessionPromptTokens: promptTokens, sessionCompletionTokens: completionTokens, sessionEstimatedCostUSD: cost}
		prevRun := conversationLoopRun
		conversationLoopRun = func(ctx context.Context, a *AIAgent, userMessage any, o RunConversationOptions, apiMode string) (map[string]any, error) {
			if a != child {
				return prevRun(ctx, a, userMessage, o, apiMode)
			}
			out := map[string]any{}
			for k, v := range result {
				out[k] = v
			}
			return out, nil
		}
		t.Cleanup(func() { conversationLoopRun = prevRun })
		return child, nil
	}
	t.Cleanup(func() { newChildAIAgentFn = prevNew })
}

func delegateFirstResult(t *testing.T, parent *AIAgent, goal string) *pyjson.Object {
	t.Helper()
	out, err := delegateTask(tools.WithToolEnv(context.Background(), parent.env), parent, map[string]any{"goal": goal})
	return firstResult(t, dispatchPayload(t, out, err))
}

func traceEntries(t *testing.T, entry *pyjson.Object) []*pyjson.Object {
	t.Helper()
	raw, _ := entry.Get("tool_trace")
	list, _ := raw.([]any)
	out := make([]*pyjson.Object, 0, len(list))
	for _, item := range list {
		out = append(out, item.(*pyjson.Object))
	}
	return out
}

func traceToolCall(id, name, args string) map[string]any {
	return map[string]any{"id": id, "function": map[string]any{"name": name, "arguments": args}}
}

func TestDelegateObservabilityObservabilityFieldsPresent(t *testing.T) {
	parent := mockDelegateParent(t, 0)
	scriptedResultChild(t, parent.env, "claude-sonnet-4-6", 5000, 1200, map[string]any{
		"final_response": "done", "completed": true, "interrupted": false, "api_calls": 3,
		"messages": []any{
			map[string]any{"role": "user", "content": "do something"},
			map[string]any{"role": "assistant", "tool_calls": []any{traceToolCall("tc_1", "web_search", `{"query": "test"}`)}},
			map[string]any{"role": "tool", "tool_call_id": "tc_1", "content": `{"results": [1,2,3]}`},
			map[string]any{"role": "assistant", "content": "done"},
		},
	})
	entry := delegateFirstResult(t, parent, "Test observability")
	tokens, _ := entry.Get("tokens")
	in, _ := tokens.(*pyjson.Object).Get("input")
	out, _ := tokens.(*pyjson.Object).Get("output")
	if entryText(entry, "model") != "claude-sonnet-4-6" || entryText(entry, "exit_reason") != "completed" || !pyrepr.Equal(in, 5000) || !pyrepr.Equal(out, 1200) {
		t.Fatalf("entry %v", entry)
	}
	trace := traceEntries(t, entry)
	if len(trace) != 1 || entryText(trace[0], "tool") != "web_search" || !entryHas(trace[0], "args_bytes") || !entryHas(trace[0], "result_bytes") || entryText(trace[0], "status") != "ok" {
		t.Fatalf("trace %v", trace)
	}
	if summary, _ := trace[0].Get("input_summary"); sortedJSON(t, summary) != sortedJSON(t, map[string]any{"argument_keys": []any{"query"}, "targets": map[string]any{}}) {
		t.Fatalf("input_summary %v", summary)
	}
}

func TestDelegateObservabilityToolTraceHandlesListContentBlocks(t *testing.T) {
	parent := mockDelegateParent(t, 0)
	scriptedResultChild(t, parent.env, "claude-sonnet-4-6", 0, 0, map[string]any{
		"final_response": "done", "completed": true, "interrupted": false, "api_calls": 1,
		"messages": []any{
			map[string]any{"role": "assistant", "tool_calls": []any{traceToolCall("tc_1", "image_generate", `{"prompt": "x"}`)}},
			map[string]any{"role": "tool", "tool_call_id": "tc_1", "content": []any{map[string]any{"type": "text", "text": `{"success": true}`}}},
		},
	})
	trace := traceEntries(t, delegateFirstResult(t, parent, "Test list content"))
	resultBytes, _ := trace[0].Get("result_bytes")
	if n, err := pyint.Float(resultBytes); entryText(trace[0], "tool") != "image_generate" || entryText(trace[0], "status") != "ok" || err != nil || n <= 0 {
		t.Fatalf("trace %v", trace)
	}
}

func TestDelegateObservabilityParallelToolCallsPairedCorrectly(t *testing.T) {
	parent := mockDelegateParent(t, 0)
	scriptedResultChild(t, parent.env, "claude-sonnet-4-6", 3000, 800, map[string]any{
		"final_response": "done", "completed": true, "interrupted": false, "api_calls": 1,
		"messages": []any{
			map[string]any{"role": "assistant", "tool_calls": []any{
				traceToolCall("tc_a", "web_search", `{"q": "a"}`), traceToolCall("tc_b", "web_search", `{"q": "b"}`), traceToolCall("tc_c", "terminal", `{"cmd": "ls"}`),
			}},
			map[string]any{"role": "tool", "tool_call_id": "tc_a", "content": `{"ok": true}`},
			map[string]any{"role": "tool", "tool_call_id": "tc_b", "content": "Error: rate limited"},
			map[string]any{"role": "tool", "tool_call_id": "tc_c", "content": "file1.txt\nfile2.txt"},
			map[string]any{"role": "assistant", "content": "done"},
		},
	})
	trace := traceEntries(t, delegateFirstResult(t, parent, "Test parallel"))
	if len(trace) != 3 {
		t.Fatalf("trace %v", trace)
	}
	for i, want := range []struct{ tool, status string }{{"web_search", "ok"}, {"web_search", "error"}, {"terminal", "ok"}} {
		if entryText(trace[i], "tool") != want.tool || entryText(trace[i], "status") != want.status || !entryHas(trace[i], "result_bytes") {
			t.Fatalf("trace %d %v", i, trace[i])
		}
	}
}

func TestDelegateObservabilityEmptySentinelMarksStatusFailed(t *testing.T) {
	parent := mockDelegateParent(t, 0)
	scriptedResultChild(t, parent.env, "claude-sonnet-4-6", 0, 0, map[string]any{
		"final_response": "(empty)", "completed": true, "interrupted": false, "api_calls": 4, "messages": []any{},
	})
	if entry := delegateFirstResult(t, parent, "Test empty sentinel"); entryText(entry, "status") != "failed" {
		t.Fatalf("entry %v", entry)
	}
}

func TestDelegateObservabilityFailedChildWithErrorSummaryMarksStatusFailed(t *testing.T) {
	parent := mockDelegateParent(t, 0)
	scriptedResultChild(t, parent.env, "claude-sonnet-4-6", 0, 0, map[string]any{
		"final_response": "API call failed after 3 retries: HTTP 524 — origin timeout", "completed": false, "failed": true,
		"error": "HTTP 524 — origin timeout", "failure_reason": "server_error", "interrupted": false, "api_calls": 3, "messages": []any{},
	})
	entry := delegateFirstResult(t, parent, "Test failed child")
	truncated, _ := entry.Get("truncated")
	if entryText(entry, "status") != "failed" || entryText(entry, "failure_reason") != "server_error" || entryText(entry, "error") != "HTTP 524 — origin timeout" ||
		entryText(entry, "exit_reason") != "error" || truncated != false {
		t.Fatalf("entry %v", entry)
	}
}

func TestDelegateObservabilitySuccessfulChildStillCompleted(t *testing.T) {
	parent := mockDelegateParent(t, 0)
	scriptedResultChild(t, parent.env, "claude-sonnet-4-6", 0, 0, map[string]any{
		"final_response": "All done.", "completed": true, "interrupted": false, "api_calls": 2, "messages": []any{},
	})
	entry := delegateFirstResult(t, parent, "Test success control")
	if entryText(entry, "status") != "completed" || entryText(entry, "exit_reason") != "completed" || entryHas(entry, "failure_reason") {
		t.Fatalf("entry %v", entry)
	}
}
