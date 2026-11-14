package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

type executorCall struct {
	name, taskID string
	args         map[string]any
}

// Fixture agent whose executor dispatches the named handlers over a private registry with post hooks recorded
func executorTestAgent(t *testing.T, handlers map[string]tools.Handler) (*AIAgent, func() []executorCall, func() []tools.PostToolCallEvent) {
	t.Helper()
	a := runAgentFixture(t)
	reg := tools.NewRegistry()
	var mu sync.Mutex
	var calls []executorCall
	var posts []tools.PostToolCallEvent
	a.validToolNames = map[string]bool{}
	for name, h := range handlers {
		h := h
		reg.Register(tools.Def{Name: name, Toolset: "web", Schema: map[string]any{"name": name, "parameters": map[string]any{"type": "object"}},
			Handler: func(ctx context.Context, c *tools.Call) string {
				mu.Lock()
				calls = append(calls, executorCall{name: c.Name, taskID: c.TaskID, args: c.Args})
				mu.Unlock()
				return h(ctx, c)
			}})
		a.validToolNames[name] = true
	}
	d := tools.NewDispatcher(reg)
	d.PostToolCall = append(d.PostToolCall, func(ctx context.Context, env *tools.Env, ev tools.PostToolCallEvent) {
		mu.Lock()
		posts = append(posts, ev)
		mu.Unlock()
	})
	a.toolDispatcher = d
	return a, func() []executorCall { mu.Lock(); defer mu.Unlock(); return append([]executorCall{}, calls...) },
		func() []tools.PostToolCallEvent {
			mu.Lock()
			defer mu.Unlock()
			return append([]tools.PostToolCallEvent{}, posts...)
		}
}

func toolCall(id, name, arguments string) state.ToolCall {
	return state.ToolCall{ID: id, Type: "function", Function: state.ToolCallFunction{Name: name, Arguments: arguments}}
}

func fixed(result string) tools.Handler {
	return func(context.Context, *tools.Call) string { return result }
}

func TestSingleToolExecuted(t *testing.T) {
	a, calls, _ := executorTestAgent(t, map[string]tools.Handler{"web_search": fixed("search result")})
	var messages []*state.Message
	if err := a.toolExecutor().ExecuteToolCallsSequential(t.Context(), []state.ToolCall{toolCall("c1", "web_search", `{"q":"test"}`)}, &messages, "task-1", true); err != nil {
		t.Fatal(err)
	}
	got := calls()
	if len(got) != 1 || got[0].name != "web_search" || got[0].taskID != "task-1" || got[0].args["q"] != "test" {
		t.Fatalf("calls %+v", got)
	}
	if len(messages) != 1 || messages[0].Role != "tool" || !strings.Contains(state.FlattenMessageText(messages[0].Content, "\n"), "search result") {
		t.Fatalf("messages %+v", messages)
	}
}

func TestSequentialToolCallsRunInOrder(t *testing.T) {
	a, calls, _ := executorTestAgent(t, map[string]tools.Handler{"web_search": fixed("ok")})
	var messages []*state.Message
	start := time.Now()
	if err := a.toolExecutor().ExecuteToolCallsSequential(t.Context(), []state.ToolCall{toolCall("c1", "web_search", "{}"), toolCall("c2", "web_search", "{}")}, &messages, "task-1", true); err != nil {
		t.Fatal(err)
	}
	if len(calls()) != 2 || time.Since(start) > 5*time.Second {
		t.Fatalf("calls %d in %v", len(calls()), time.Since(start))
	}
	if len(messages) != 2 || messages[0].ToolCallID != "c1" || messages[1].ToolCallID != "c2" {
		t.Fatalf("messages %+v", messages)
	}
}

func TestInterruptSkipsRemainingWithCancelledHooks(t *testing.T) {
	a, calls, posts := executorTestAgent(t, map[string]tools.Handler{"web_search": fixed("ok")})
	a.Interrupt("")
	var messages []*state.Message
	if err := a.toolExecutor().ExecuteToolCallsSequential(t.Context(), []state.ToolCall{toolCall("c1", "web_search", "{}"), toolCall("c2", "web_search", "{}")}, &messages, "task-1", true); err != nil {
		t.Fatal(err)
	}
	if len(calls()) != 0 || len(messages) != 2 {
		t.Fatalf("calls %d messages %d", len(calls()), len(messages))
	}
	text := strings.ToLower(state.FlattenMessageText(messages[0].Content, "\n"))
	if !strings.Contains(text, "cancelled") && !strings.Contains(text, "interrupted") {
		t.Fatalf("first row %q", text)
	}
	got := posts()
	if len(got) != 2 || got[0].IDs.ToolCallID != "c1" || got[1].IDs.ToolCallID != "c2" || got[0].Status != "cancelled" || got[1].Status != "cancelled" {
		t.Fatalf("post hooks %+v", got)
	}
}

func TestInvalidJSONArgsRejectedWithoutDispatch(t *testing.T) {
	for _, args := range []string{"not valid json", ""} {
		a, calls, posts := executorTestAgent(t, map[string]tools.Handler{"web_search": fixed("ok")})
		var messages []*state.Message
		if err := a.toolExecutor().ExecuteToolCallsConcurrent(t.Context(), []state.ToolCall{toolCall("c1", "web_search", args)}, &messages, "task-1", true); err != nil {
			t.Fatal(err)
		}
		if len(calls()) != 0 || len(messages) != 1 || messages[0].ToolCallID != "c1" ||
			!strings.Contains(strings.ToLower(state.FlattenMessageText(messages[0].Content, "\n")), "tool was not executed") {
			t.Fatalf("%q: calls %d messages %+v", args, len(calls()), messages)
		}
		got := posts()
		if len(got) != 1 || got[0].IDs.ToolCallID != "c1" || got[0].Status != "error" || got[0].ErrorType != "invalid_tool_arguments" {
			t.Fatalf("%q: post hooks %+v", args, got)
		}
	}
}

const liveNote = "[System note: plugin blender was installed and is live in this chat now.]"

func TestPluginInstalledDuringToolIsToldInItsResultOnce(t *testing.T) {
	var a *AIAgent
	a, _, _ = executorTestAgent(t, map[string]tools.Handler{"web_search": func(context.Context, *tools.Call) string {
		a.queuePluginNotice("blender", "[System note: plugin blender (an older install) is live.]")
		a.queuePluginNotice("blender", liveNote)
		return `{"results": ["a web page about Blender scripting"]}`
	}})
	var messages []*state.Message
	if err := a.toolExecutor().ExecuteToolCallsSequential(t.Context(), []state.ToolCall{toolCall("c1", "web_search", "{}")}, &messages, "task-1", true); err != nil {
		t.Fatal(err)
	}
	text := state.FlattenMessageText(messages[0].Content, "\n")
	if !strings.HasSuffix(text, "\n\n"+liveNote) || strings.Contains(text, "older install") {
		t.Fatalf("tool row %q", text)
	}
	if rest := popPluginNotices(a); rest != "" {
		t.Fatalf("notice told twice %q", rest)
	}
}

func TestNoticeNoToolCarriedRidesNextUserTurnOnce(t *testing.T) {
	a := runAgentFixture(t)
	a.queuePluginNotice("blender", liveNote)
	if got := popPluginNotices(a); got != liveNote {
		t.Fatalf("first pop %q", got)
	}
	if got := popPluginNotices(a); got != "" {
		t.Fatalf("second pop %q", got)
	}
}

func TestToolMetricSinkConsumesMetricsAndForwardsTheRest(t *testing.T) {
	var forwarded []tools.Event
	env := withToolMetricSink(t.Context(), &tools.Env{Notify: func(ev tools.Event) { forwarded = append(forwarded, ev) }})
	env.Emit(tools.Event{Kind: "metric", Data: map[string]any{"name": ""}})
	env.Emit(tools.Event{Kind: "progress", Data: map[string]any{"text": "x"}})
	if len(forwarded) != 1 || forwarded[0].Kind != "progress" {
		t.Fatalf("forwarded %+v", forwarded)
	}
	if withToolMetricSink(t.Context(), nil) != nil {
		t.Fatal("nil env grew a sink")
	}
}

func TestToolCallbacksAdaptBetweenExecutorAndCodexShapes(t *testing.T) {
	a := runAgentFixture(t)
	var gotExtra map[string]any
	a.toolProgressCallback = func(event, name, preview string, args map[string]any, extra map[string]any) { gotExtra = extra }
	d, r := 1.5, "out"
	a.ToolProgressCallback()("tool.completed", "web_search", nil, nil, &provider.CodexToolProgressExtra{Duration: &d, IsError: true, Result: &r})
	if gotExtra["duration"] != 1.5 || gotExtra["is_error"] != true || gotExtra["result"] != "out" {
		t.Fatalf("codex to executor extra %v", gotExtra)
	}
	var gotCodex *provider.CodexToolProgressExtra
	a.toolProgressCallback = func(event, name string, preview any, args any, extra *provider.CodexToolProgressExtra) {
		gotCodex = extra
	}
	agentExecutorHost{a}.ToolProgressCallback()("tool.completed", "web_search", "", nil, map[string]any{"duration": 2.0, "is_error": false, "result": "r"})
	if gotCodex == nil || *gotCodex.Duration != 2.0 || gotCodex.IsError || *gotCodex.Result != "r" {
		t.Fatalf("executor to codex extra %+v", gotCodex)
	}
}
