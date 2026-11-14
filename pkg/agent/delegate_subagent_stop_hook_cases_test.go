package agent

import (
	"context"
	"sort"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Parent of test_subagent_stop_hook.py _make_parent with the child builder replaced by a bare agent
func stopHookParent(t *testing.T, sessionID string) *AIAgent {
	t.Helper()
	parent := batchValidationParent(t)
	parent.sessionID = sessionID
	prev := buildChildPreservingParentToolsFn
	buildChildPreservingParentToolsFn = func(context.Context, childBuildSpec) (*AIAgent, error) { return &AIAgent{}, nil }
	t.Cleanup(func() { buildChildPreservingParentToolsFn = prev })
	return parent
}

// Captures subagent_stop payloads with the goroutine each callback body ran on under _goroutine
func captureSubagentStopWithGoroutine(t *testing.T) func() []map[string]any {
	t.Helper()
	plugins.ResetPluginManagersForTests()
	t.Cleanup(plugins.ResetPluginManagersForTests)
	ctx := context.Background()
	m := plugins.GetPluginManager(ctx)
	m.DiscoverAndLoad(ctx, false)
	var mu sync.Mutex
	var calls []map[string]any
	c := plugins.NewContext(&plugins.Manifest{Name: "stop-probe", Key: "stop-probe"}, m)
	c.RegisterHook("subagent_stop", func(_ context.Context, kw map[string]any) (any, error) {
		copied := map[string]any{"_goroutine": pythread.GoroutineID()}
		for k, v := range kw {
			copied[k] = v
		}
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, copied)
		return nil, nil
	}, false)
	return func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]any(nil), calls...)
	}
}

// Replaces the single child run with scripted entries keyed by task index
func scriptChildEntries(t *testing.T, entries ...*pyjson.Object) {
	t.Helper()
	prev := runSingleChildFn
	runSingleChildFn = func(_ context.Context, i int, _ string, _, _ *AIAgent, _ string, _, _ any) *pyjson.Object {
		return entries[i]
	}
	t.Cleanup(func() { runSingleChildFn = prev })
}

func stopHookEntry(i int, summary string, apiCalls int, duration float64, role any) *pyjson.Object {
	return pyjson.ObjectOf("task_index", i, "status", "completed", "summary", summary, "api_calls", apiCalls, "duration_seconds", duration, "_child_role", role)
}

func TestSingleTaskFiresOnce(t *testing.T) {
	parent := stopHookParent(t, "parent-1")
	captured := captureSubagentStopWithGoroutine(t)
	scriptChildEntries(t, stopHookEntry(0, "Done!", 3, 5.0, "analyst"))
	if _, err := delegateTask(context.Background(), parent, map[string]any{"goal": "do X"}); err != nil {
		t.Fatal(err)
	}
	calls := captured()
	if len(calls) != 1 {
		t.Fatalf("captured %v", calls)
	}
	payload := calls[0]
	if payload["child_role"] != "analyst" || payload["child_status"] != "completed" || payload["child_summary"] != "Done!" || payload["duration_ms"] != 5000 {
		t.Fatalf("payload %v", payload)
	}
}

func TestSingleTaskFiresOnParentThread(t *testing.T) {
	parent := stopHookParent(t, "parent-1")
	captured := captureSubagentStopWithGoroutine(t)
	scriptChildEntries(t, stopHookEntry(0, "x", 1, 0.1, nil))
	main := pythread.GoroutineID()
	if _, err := delegateTask(context.Background(), parent, map[string]any{"goal": "go"}); err != nil {
		t.Fatal(err)
	}
	calls := captured()
	if len(calls) == 0 || calls[0]["_goroutine"] != main {
		t.Fatalf("captured %v on goroutine %d", calls, main)
	}
}

func TestSingleTaskPayloadIncludesParentSessionId(t *testing.T) {
	parent := stopHookParent(t, "sess-xyz")
	captured := captureSubagentStopWithGoroutine(t)
	scriptChildEntries(t, stopHookEntry(0, "x", 1, 0.1, nil))
	if _, err := delegateTask(context.Background(), parent, map[string]any{"goal": "go"}); err != nil {
		t.Fatal(err)
	}
	if calls := captured(); len(calls) != 1 || calls[0]["parent_session_id"] != "sess-xyz" {
		t.Fatalf("captured %v", calls)
	}
}

func TestBatchModeFiresPerChild(t *testing.T) {
	parent := stopHookParent(t, "parent-1")
	captured := captureSubagentStopWithGoroutine(t)
	scriptChildEntries(t, stopHookEntry(0, "A", 1, 1.0, "role-a"), stopHookEntry(1, "B", 2, 2.0, "role-b"), stopHookEntry(2, "C", 3, 3.0, "role-c"))
	_, err := delegateTask(context.Background(), parent, map[string]any{"tasks": batchGoals("Investigate module A", "Investigate module B", "Investigate module C")})
	if err != nil {
		t.Fatal(err)
	}
	calls := captured()
	if len(calls) != 3 {
		t.Fatalf("captured %v", calls)
	}
	var roles []string
	for _, c := range calls {
		roles = append(roles, c["child_role"].(string))
	}
	sort.Strings(roles)
	if roles[0] != "role-a" || roles[1] != "role-b" || roles[2] != "role-c" {
		t.Fatalf("roles %v", roles)
	}
}

func TestBatchModeAllFiresOnParentThread(t *testing.T) {
	parent := stopHookParent(t, "parent-1")
	captured := captureSubagentStopWithGoroutine(t)
	scriptChildEntries(t, stopHookEntry(0, "A", 1, 1.0, nil), stopHookEntry(1, "B", 2, 2.0, nil))
	main := pythread.GoroutineID()
	_, err := delegateTask(context.Background(), parent, map[string]any{"tasks": batchGoals("Investigate module A", "Investigate module B")})
	if err != nil {
		t.Fatal(err)
	}
	calls := captured()
	if len(calls) != 2 {
		t.Fatalf("captured %v", calls)
	}
	for _, payload := range calls {
		if payload["_goroutine"] != main {
			t.Fatalf("payload %v fired off the parent goroutine %d", payload, main)
		}
	}
}

func TestPayloadShapeIncludesRedactedToolCallHistory(t *testing.T) {
	parent := stopHookParent(t, "parent-1")
	captured := captureSubagentStopWithGoroutine(t)
	trace := []any{pyjson.ObjectOf(
		"tool", "write_file", "args_bytes", 128, "result_bytes", 32, "status", "ok",
		"input_summary", pyjson.ObjectOf(
			"argument_keys", []any{"content", "path", "token"},
			"targets", pyjson.ObjectOf("path", "/private/report.json", "url", "https://user:password@example.test:8443/upload?token=secret", "token", "must-not-leak"),
		),
		"args", pyjson.ObjectOf("path", "/private/report.json"), "result", "secret output",
	)}
	scriptChildEntries(t, pyjson.ObjectOf("task_index", 0, "status", "completed", "summary", "wrote the report", "api_calls", 1, "duration_seconds", 0.1, "tool_trace", trace))
	if _, err := delegateTask(context.Background(), parent, map[string]any{"goal": "do X"}); err != nil {
		t.Fatal(err)
	}
	calls := captured()
	if len(calls) != 1 {
		t.Fatalf("captured %v", calls)
	}
	want := []any{map[string]any{
		"tool_name": "write_file",
		"tool_input": map[string]any{
			"argument_keys": []any{"content", "path", "token"},
			"targets":       map[string]any{"path": "/private/report.json", "url": "https://example.test:8443/upload"},
		},
		"input_bytes": 128, "output_bytes": 32, "status": "ok",
	}}
	if got := sortedJSON(t, calls[0]["tool_call_history"]); got != sortedJSON(t, want) {
		t.Fatalf("tool_call_history %s", got)
	}
}

func TestPayloadShapeResultDoesNotLeakChildRoleField(t *testing.T) {
	parent := stopHookParent(t, "parent-1")
	captureSubagentStopWithGoroutine(t)
	scriptChildEntries(t, stopHookEntry(0, "x", 1, 0.1, "leaf"))
	raw, err := delegateTask(context.Background(), parent, map[string]any{"goal": "do X"})
	parsed := dispatchPayload(t, raw, err)
	results := batchResults(t, parsed)
	if len(results) == 0 || entryHas(results[0].(*pyjson.Object), "_child_role") {
		t.Fatalf("parsed %v", parsed)
	}
}
