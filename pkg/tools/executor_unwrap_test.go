package tools

import (
	"context"
	"testing"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

func unwrapExecutor(t *testing.T, enabled ...string) *Executor {
	host := &fakeExecutorHost{env: newTestToolEnv(t), enabled: enabled}
	return NewExecutor(host, NewDispatcher(Default))
}

func TestSingleLocalUnwrapReachesTheUnderlyingTool(t *testing.T) {
	entries := []map[string]any{
		{"name": "session_search", "arguments": map[string]any{"session_id": "past-session"}},
		{"name": "todo_list", "arguments": map[string]any{"todos": []any{map[string]any{"id": "a", "content": "live-store-proof", "status": "pending"}}}},
	}
	env := newTestToolEnv(t)
	for name, toolset := range map[string]string{"session_search": "session_search", "todo_list": "todo"} {
		if Default.Entry(context.Background(), env, name) != nil {
			continue
		}
		Default.Register(Def{Name: name, Toolset: toolset, Schema: jsonutil.ObjectOf("name", name, "parameters", jsonutil.ObjectOf("type", "object")),
			Check: func(*Env) bool { return true }, Handler: func(context.Context, *Call) string { return "{}" }})
		t.Cleanup(func() { _ = Default.Deregister(name, "", "") })
	}
	for _, flatten := range []bool{false, true} {
		e := unwrapExecutor(t, "todo", "session_search", "connections")
		for _, entry := range entries {
			name, args, block := e.unwrapToolSearchCall(context.Background(), ToolCallName, map[string]any{"calls": []any{entry}}, "", flatten)
			if name != entry["name"] || block != "" || args == nil {
				t.Fatalf("flatten=%v %v -> name %q block %q", flatten, entry["name"], name, block)
			}
		}
	}
}
