package tools

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// Session ids reach the registry handler on the standard path and on the execute_code path
// Dispatches one call with a session id and returns what the handler saw
func sessionForwardingCall(t *testing.T, tool, session string) *Call {
	t.Helper()
	var seen *Call
	d, _, env := modelToolsDispatcher(t, func(ctx context.Context, call *Call) string { seen = call; return `{"result": "ok"}` }, tool)
	d.DispatchWith(WithDispatcher(context.Background(), d), &Call{Name: tool, Args: map[string]any{"query": "test"}, TaskID: "t1", SessionID: session, Env: env},
		DispatchOptions{SkipPreToolCallHook: true})
	if seen == nil || seen.SessionID != session || seen.TaskID != "t1" {
		t.Fatalf("handler saw %+v", seen)
	}
	return seen
}

func TestStandardPathForwardsSessionId(t *testing.T) {
	sessionForwardingCall(t, "web_search", "sess-abc")
}

func TestExecuteCodePathForwardsSessionId(t *testing.T) {
	sessionForwardingCall(t, "execute_code", "sess-xyz")
}

// Dispatcher with one fixed result tool and the given transform and post hooks
func transformDispatcher(t *testing.T, tool, result string, post PostToolCallHook, transforms ...TransformToolResultHook) (*Dispatcher, *Env) {
	t.Helper()
	r := NewRegistry()
	r.Register(Def{Name: tool, Toolset: "test", Schema: map[string]any{"name": tool, "description": tool, "parameters": map[string]any{"type": "object"}}, Handler: constHandler(result)})
	d := NewDispatcher(r)
	if post != nil {
		d.PostToolCall = append(d.PostToolCall, post)
	}
	d.TransformToolResult = append(d.TransformToolResult, transforms...)
	return d, &Env{Home: t.TempDir()}
}

func runTransformCall(d *Dispatcher, env *Env, tool string, args map[string]any) string {
	return d.DispatchWith(WithDispatcher(context.Background(), d), &Call{Name: tool, Args: args, TaskID: "t1", SessionID: "s1", ToolCallID: "tc1", Env: env},
		DispatchOptions{SkipPreToolCallHook: true})
}

// The first string a transform hook returns replaces the result and every hook still runs
func TestFirstValidStringReturnReplacesResult(t *testing.T) {
	var ran []int
	var mu sync.Mutex
	hook := func(i int, out string, ok bool) TransformToolResultHook {
		return func(context.Context, *Env, PostToolCallEvent) (string, bool) {
			mu.Lock()
			ran = append(ran, i)
			mu.Unlock()
			return out, ok
		}
	}
	d, env := transformDispatcher(t, "dummy_tool", `{"output": "original"}`, nil, hook(0, "", false), hook(1, "", false), hook(2, "first", true), hook(3, "second", true))
	if out := runTransformCall(d, env, "dummy_tool", nil); out != "first" {
		t.Fatalf("out %q", out)
	}
	if !reflect.DeepEqual(ran, []int{0, 1, 2, 3}) {
		t.Fatalf("hooks run %v, every callback must run", ran)
	}
}

// The transform hook receives the tool name, args, raw result and every correlation id
func TestHookReceivesExpectedKwargs(t *testing.T) {
	var got PostToolCallEvent
	d, env := transformDispatcher(t, "my_tool", `{"ok": true}`, nil, func(_ context.Context, _ *Env, ev PostToolCallEvent) (string, bool) {
		got = ev
		return "", false
	})
	if out := runTransformCall(d, env, "my_tool", map[string]any{"a": 1.0, "b": "x"}); out != `{"ok": true}` {
		t.Fatalf("out %q", out)
	}
	if got.ToolName != "my_tool" || !reflect.DeepEqual(got.Args, map[string]any{"a": 1.0, "b": "x"}) || got.Result != `{"ok": true}` ||
		got.IDs.TaskID != "t1" || got.IDs.SessionID != "s1" || got.IDs.ToolCallID != "tc1" {
		t.Fatalf("hook saw %+v", got)
	}
}

// A post tool call observer never replaces the result
func TestPostToolCallRemainsObservational(t *testing.T) {
	d, env := transformDispatcher(t, "dummy_tool", `{"output": "original"}`, func(context.Context, *Env, PostToolCallEvent) {}, func(context.Context, *Env, PostToolCallEvent) (string, bool) {
		return "", false
	})
	if out := runTransformCall(d, env, "dummy_tool", nil); out != `{"output": "original"}` {
		t.Fatalf("out %q", out)
	}
}

// Post tool call sees the original result before the transform hook replaces it
func TestTransformToolResultRunsAfterPostToolCall(t *testing.T) {
	var observed [][2]string
	d, env := transformDispatcher(t, "dummy_tool", `{"raw": "value"}`,
		func(_ context.Context, _ *Env, ev PostToolCallEvent) {
			observed = append(observed, [2]string{"post_tool_call", ev.Result})
		},
		func(_ context.Context, _ *Env, ev PostToolCallEvent) (string, bool) {
			observed = append(observed, [2]string{"transform_tool_result", ev.Result})
			return "rewritten", true
		})
	if out := runTransformCall(d, env, "dummy_tool", nil); out != "rewritten" {
		t.Fatalf("out %q", out)
	}
	want := [][2]string{{"post_tool_call", `{"raw": "value"}`}, {"transform_tool_result", `{"raw": "value"}`}}
	if !reflect.DeepEqual(observed, want) {
		t.Fatalf("order %v", observed)
	}
}

var (
	previewTUIScope     = []string{"coding", "project"}
	previewDesktopScope = []string{"coding", "desktop_ui", "project"}
)

func previewBridgeError(t *testing.T, name string, scope []string) string {
	t.Helper()
	env := &Env{Home: t.TempDir()}
	out := DefaultDispatcher.DispatchWith(context.Background(), &Call{Name: ToolCallName, Args: map[string]any{"name": name, "arguments": map[string]any{"action": "elements"}}, Env: env},
		DispatchOptions{EnabledToolsets: append([]string(nil), scope...)})
	msg, _ := decodeModelToolsResult(t, out)["error"].(string)
	return msg
}

func previewUnwrap(t *testing.T, scope []string) (string, string) {
	t.Helper()
	e := NewExecutor(&fakeExecutorHost{env: &Env{Home: t.TempDir()}, enabled: scope}, DefaultDispatcher)
	name, _, block := e.unwrapToolSearchCall(context.Background(), ToolCallName, map[string]any{"name": "drive_preview", "arguments": map[string]any{"action": "elements"}}, false)
	return name, block
}

func TestBridgeTuiScopeNamesTheGuiSurface(t *testing.T) {
	err := strings.ToLower(previewBridgeError(t, "drive_preview", previewTUIScope))
	if !strings.Contains(err, "desktop") || strings.Contains(err, "use tool_search to find") {
		t.Fatalf("bridge in tui scope: %q", err)
	}
}

func TestBridgeTuiScopeDesktopPreviewNamesTheGuiSurface(t *testing.T) {
	err := strings.ToLower(previewBridgeError(t, "desktop_preview", previewTUIScope))
	if !strings.Contains(err, "desktop") || strings.Contains(err, "use tool_search to find") {
		t.Fatalf("bridge in tui scope: %q", err)
	}
}

func TestUnwrapTuiScopeNamesTheGuiSurface(t *testing.T) {
	if _, block := previewUnwrap(t, previewTUIScope); !strings.Contains(strings.ToLower(block), "desktop") || strings.Contains(strings.ToLower(block), "use tool_search to find") {
		t.Fatalf("unwrap in tui scope: %q", block)
	}
}

func TestBridgeDesktopScopeStillReachesTheTool(t *testing.T) {
	if err := previewBridgeError(t, "drive_preview", previewDesktopScope); !strings.Contains(strings.ToLower(err), "desktop") {
		t.Fatalf("bridge in desktop scope: %q", err)
	}
}

func TestUnwrapDesktopScopeNotBlockedThroughTheDispatcher(t *testing.T) {
	if name, block := previewUnwrap(t, previewDesktopScope); block != "" || name != "drive_preview" {
		t.Fatalf("unwrap in desktop scope blocked %q as %q", block, name)
	}
}

func TestAbsentNonSurfaceCoreToolReportsUnavailability(t *testing.T) {
	if err := previewBridgeError(t, "computer_use", previewTUIScope); !strings.Contains(err, "not available in this execution context") {
		t.Fatalf("absent core tool: %q", err)
	}
}

func installWorkerProcess(t *testing.T, worker bool) {
	prev := IsWorkerProcess
	t.Cleanup(func() { IsWorkerProcess = prev })
	IsWorkerProcess = func() bool { return worker }
}

func workerBridgeError(t *testing.T, env *Env, name string, toolsets []string) string {
	t.Helper()
	out := DefaultDispatcher.DispatchWith(context.Background(), &Call{Name: ToolCallName, Args: map[string]any{"calls": []any{map[string]any{"name": name, "arguments": map[string]any{}}}}, Env: env},
		DispatchOptions{EnabledToolsets: toolsets})
	msg, _ := decodeModelToolsResult(t, out)["error"].(string)
	return msg
}

// A delegate_task absent from the turn reads as unavailable, worker or not, never as call it directly
func TestDelegateTaskAbsentFromATurnIsReportedAsUnavailable(t *testing.T) {
	for _, worker := range []bool{true, false} {
		installWorkerProcess(t, worker)
		env := &Env{Home: t.TempDir()}
		turnTools := []map[string]any{{"type": "function", "function": map[string]any{"name": "kanban_create", "parameters": map[string]any{}}}}
		cfg := ToolSearchConfigFromRaw(map[string]any{})
		var described map[string]any
		if err := json.Unmarshal([]byte(DispatchToolDescribe(context.Background(), env, map[string]any{"names": []any{"delegate_task"}}, turnTools, &cfg, nil)), &described); err != nil {
			t.Fatal(err)
		}
		errs, _ := described["errors"].(map[string]any)
		describedMsg, _ := errs["delegate_task"].(string)
		for _, msg := range []string{workerBridgeError(t, env, "delegate_task", []string{"file"}), describedMsg} {
			if !strings.Contains(msg, "not available in this execution context") || strings.Contains(msg, "Call it directly") || strings.Contains(msg, "managed worker") {
				t.Fatalf("worker %v: %q", worker, msg)
			}
		}
		if strings.Contains(InvalidToolNameErrorContent("delegate_task", map[string]bool{"kanban_create": true}), "managed worker") {
			t.Fatalf("worker %v: invalid tool name text names the managed worker", worker)
		}
	}
}

// Core tool guidance says call it directly only when the session can reach the tool
func TestCoreToolGuidanceFollowsTheSession(t *testing.T) {
	for _, tc := range []struct {
		name     string
		toolsets []string
		want     string
	}{
		{"clarify", []string{"file"}, "not available in this execution context"},
		{"clarify", []string{"file", "clarify"}, "Call it directly"},
		{"delegate_task", []string{"file", "delegation"}, "Call it directly"},
	} {
		installWorkerProcess(t, tc.name == "delegate_task")
		if msg := workerBridgeError(t, &Env{Home: t.TempDir()}, tc.name, tc.toolsets); !strings.Contains(msg, tc.want) || strings.Contains(msg, "managed worker") {
			t.Fatalf("%s %v: %q", tc.name, tc.toolsets, msg)
		}
	}
}

// A deferred tool absent from the session is reported unavailable and never sent to tool_search
func TestDeferredToolAbsentFromTheSessionIsNotSentToToolSearch(t *testing.T) {
	installWorkerProcess(t, false)
	env := &Env{Home: t.TempDir(), Config: map[string]any{"tools": map[string]any{"tool_search": map[string]any{"defer": []any{"delegate_task"}}}}}
	described := decodeModelToolsResult(t, DefaultDispatcher.DispatchWith(context.Background(), &Call{Name: "tool_describe", Args: map[string]any{"names": []any{"delegate_task"}}, Env: env},
		DispatchOptions{EnabledToolsets: []string{"file"}}))
	errs, _ := described["errors"].(map[string]any)
	describedMsg, _ := errs["delegate_task"].(string)
	for _, msg := range []string{workerBridgeError(t, env, "delegate_task", []string{"file"}), describedMsg} {
		if !strings.Contains(msg, "not available in this execution context") || strings.Contains(msg, "tool_search") {
			t.Fatalf("%q", msg)
		}
	}
}

// Each schema build hands its checks its own explicit kanban selection, never a neighbour's
func TestKanbanSelectionReachesChecksPerBuild(t *testing.T) {
	ClearToolDefinitionsCache()
	t.Cleanup(ClearToolDefinitionsCache)
	var mu sync.Mutex
	calls := 0
	both := make(chan struct{})
	check := NoCacheCheck(func(env *Env) bool {
		requested, inAssembly := KanbanToolsetRequested(env)
		mu.Lock()
		if calls++; calls == 2 {
			close(both)
		}
		mu.Unlock()
		<-both
		return inAssembly && requested
	})
	r := NewRegistry()
	r.Register(Def{Name: "kanban_create", Toolset: "kanban", Schema: memoSchema("kanban_create", "Create a card"), Handler: constHandler("{}"), Check: check})
	r.Register(Def{Name: "read_file", Toolset: "file", Schema: memoSchema("read_file", "Read a file"), Handler: constHandler("{}")})
	env := &Env{Home: t.TempDir()}
	names := func(enabled []string) map[string]bool {
		out := map[string]bool{}
		for _, d := range r.ToolDefinitions(context.Background(), env, DefinitionOptions{Enabled: enabled, Quiet: true, SkipToolSearchAssembly: true}) {
			out[d["function"].(map[string]any)["name"].(string)] = true
		}
		return out
	}
	var named, broad map[string]bool
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); named = names([]string{"kanban"}) }()
	go func() { defer wg.Done(); broad = names(nil) }()
	wg.Wait()
	if !named["kanban_create"] || broad["kanban_create"] || !broad["read_file"] {
		t.Fatalf("named %v broad %v", named, broad)
	}
	if names([]string{"all"})["kanban_create"] || !names([]string{"kanban", "file"})["kanban_create"] {
		t.Fatal("an all or default selection counted as an explicit kanban opt in")
	}
	if requested, inAssembly := KanbanToolsetRequested(env); requested || inAssembly {
		t.Fatal("selection leaked out of the schema build")
	}
}
