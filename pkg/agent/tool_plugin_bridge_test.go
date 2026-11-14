package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Fresh plugin manager of the test home with the lifecycle service installed, and a context registering on it
func bridgePluginContext(t *testing.T) *plugins.Context {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	plugins.ResetPluginManagersForTests()
	t.Cleanup(plugins.ResetPluginManagersForTests)
	previous := plugins.InstalledServices()
	plugins.InstallServices(&plugins.Services{InvokeLifecycleHook: LifecycleInvokeHook})
	t.Cleanup(func() { plugins.InstallServices(previous) })
	ctx := context.Background()
	m := plugins.GetPluginManager(ctx)
	m.DiscoverAndLoad(ctx, false)
	return plugins.NewContext(&plugins.Manifest{Name: "bridge-probe", Key: "bridge-probe"}, m)
}

func bridgedDispatcher(t *testing.T, handler tools.Handler) (*tools.Dispatcher, *tools.Env) {
	t.Helper()
	r := tools.NewRegistry()
	r.Register(tools.Def{Name: "web_search", Toolset: "test", Handler: handler,
		Schema: map[string]any{"name": "web_search", "description": "web_search", "parameters": map[string]any{"type": "object"}}})
	d := tools.NewDispatcher(r)
	installToolPluginBridge(d)
	return d, &tools.Env{Home: t.TempDir()}
}

func TestDefaultDispatcherCarriesThePluginBridge(t *testing.T) {
	d := tools.DefaultDispatcher
	pointer := func(f any) uintptr { return reflect.ValueOf(f).Pointer() }
	has := func(hooks any, want any) bool {
		v := reflect.ValueOf(hooks)
		for i := 0; i < v.Len(); i++ {
			if v.Index(i).Pointer() == pointer(want) {
				return true
			}
		}
		return false
	}
	if !has(d.PreToolCall, pluginPreToolCall) || !has(d.PostToolCall, pluginPostToolCall) || !has(d.TransformToolResult, pluginTransformToolResult) ||
		!has(d.ToolRequestMiddleware, pluginToolRequestMiddleware) || !has(d.ToolExecutionMiddleware, pluginToolExecutionMiddleware) {
		t.Fatal("the process wide dispatcher is missing a plugin bridge entry")
	}
}

func TestPluginHooksAndMiddlewareRunAroundARegistryDispatch(t *testing.T) {
	c := bridgePluginContext(t)
	var mu sync.Mutex
	var pre, post, transform []map[string]any
	keep := func(into *[]map[string]any, kw map[string]any) {
		copied := map[string]any{}
		for k, v := range kw {
			copied[k] = v
		}
		mu.Lock()
		*into = append(*into, copied)
		mu.Unlock()
	}
	c.RegisterHook("pre_tool_call", func(_ context.Context, kw map[string]any) (any, error) {
		keep(&pre, kw)
		return map[string]any{"action": "modify", "args": map[string]any{"limit": 3}}, nil
	}, false)
	c.RegisterHook("post_tool_call", func(_ context.Context, kw map[string]any) (any, error) { keep(&post, kw); return nil, nil }, false)
	c.RegisterHook("transform_tool_result", func(_ context.Context, kw map[string]any) (any, error) {
		keep(&transform, kw)
		return "transformed:" + kw["result"].(string), nil
	}, false)
	c.RegisterMiddleware(plugins.ToolRequestMiddleware, func(kw map[string]any) (any, error) {
		args := map[string]any{"rewritten": true}
		for k, v := range kw["args"].(map[string]any) {
			args[k] = v
		}
		return map[string]any{"args": args, "source": "bridge-probe", "reason": "rewrite"}, nil
	})
	c.RegisterMiddleware(plugins.ToolExecutionMiddleware, func(kw map[string]any) (any, error) {
		args := map[string]any{"wrapped": true}
		for k, v := range kw["args"].(map[string]any) {
			args[k] = v
		}
		out, err := kw["next_call"].(plugins.NextCall)(args)
		if err != nil {
			return nil, err
		}
		return "[" + out.(string) + "]", nil
	})
	var dispatched map[string]any
	d, env := bridgedDispatcher(t, func(_ context.Context, call *tools.Call) string {
		dispatched = call.Args
		return "ran"
	})
	out := d.Dispatch(context.Background(), &tools.Call{Name: "web_search", Args: map[string]any{"q": "test"}, TaskID: "task-1",
		ToolCallID: "tool-1", SessionID: "session-1", TurnID: "turn-1", APIRequestID: "req-1", Env: env})
	if out != "transformed:[ran]" {
		t.Fatalf("result %q", out)
	}
	wantArgs := map[string]any{"q": "test", "rewritten": true, "limit": 3, "wrapped": true}
	if !reflect.DeepEqual(dispatched, wantArgs) {
		t.Fatalf("dispatched %v", dispatched)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(pre) != 1 || len(post) != 1 || len(transform) != 1 {
		t.Fatalf("calls pre %d post %d transform %d", len(pre), len(post), len(transform))
	}
	trace := []any{map[string]any{"source": "bridge-probe", "reason": "rewrite"}}
	ids := map[string]any{"task_id": "task-1", "session_id": "session-1", "tool_call_id": "tool-1", "turn_id": "turn-1", "api_request_id": "req-1"}
	for key, want := range ids {
		if pre[0][key] != want || post[0][key] != want || transform[0][key] != want {
			t.Fatalf("%s pre %v post %v transform %v", key, pre[0][key], post[0][key], transform[0][key])
		}
	}
	if pre[0]["tool_name"] != "web_search" || !reflect.DeepEqual(pre[0]["args"], map[string]any{"q": "test", "rewritten": true}) || !reflect.DeepEqual(pre[0]["middleware_trace"], trace) {
		t.Fatalf("pre %v", pre[0])
	}
	afterModify := map[string]any{"q": "test", "rewritten": true, "limit": 3}
	if post[0]["tool_name"] != "web_search" || !reflect.DeepEqual(post[0]["args"], afterModify) || post[0]["result"] != "[ran]" || post[0]["status"] != "ok" ||
		post[0]["error_type"] != nil || post[0]["error_message"] != nil || !reflect.DeepEqual(post[0]["middleware_trace"], trace) {
		t.Fatalf("post %v", post[0])
	}
	if duration, ok := post[0]["duration_ms"].(int); !ok || duration < 0 {
		t.Fatalf("duration %v", post[0]["duration_ms"])
	}
	if _, has := transform[0]["middleware_trace"]; has || transform[0]["result"] != "[ran]" || transform[0]["status"] != "ok" || !reflect.DeepEqual(transform[0]["args"], afterModify) {
		t.Fatalf("transform %v", transform[0])
	}
}

func TestPluginBlockCarriesTheModifiedArgsAndSkipsTheHandler(t *testing.T) {
	c := bridgePluginContext(t)
	c.RegisterHook("pre_tool_call", func(context.Context, map[string]any) (any, error) {
		return map[string]any{"action": "modify", "args": map[string]any{"limit": 3}}, nil
	}, false)
	c.RegisterHook("pre_tool_call", func(context.Context, map[string]any) (any, error) {
		return map[string]any{"action": "approve", "message": "ask first", "rule_key": " rule "}, nil
	}, false)
	c.RegisterHook("pre_tool_call", func(context.Context, map[string]any) (any, error) {
		return map[string]any{"action": "block", "message": "not allowed"}, nil
	}, false)
	c.RegisterHook("pre_tool_call", func(context.Context, map[string]any) (any, error) {
		return map[string]any{"action": "modify", "args": map[string]any{"late": true}}, nil
	}, false)
	directive := pluginPreToolCall(context.Background(), nil, "web_search", map[string]any{"q": "test"}, tools.ToolHookIDs{}, nil)
	want := tools.PreToolDirective{Action: "block", Message: "not allowed", Args: map[string]any{"q": "test", "limit": 3}}
	if !reflect.DeepEqual(directive, want) {
		t.Fatalf("directive %+v", directive)
	}
	var post []map[string]any
	c.RegisterHook("post_tool_call", func(_ context.Context, kw map[string]any) (any, error) { post = append(post, kw); return nil, nil }, false)
	ran := false
	d, env := bridgedDispatcher(t, func(context.Context, *tools.Call) string { ran = true; return "ran" })
	out := d.Dispatch(context.Background(), &tools.Call{Name: "web_search", Args: map[string]any{"q": "test"}, Env: env})
	var blocked map[string]any
	if err := json.Unmarshal([]byte(out), &blocked); err != nil || ran || blocked["error"] != "not allowed" {
		t.Fatalf("ran %v result %q %v", ran, out, err)
	}
	if len(post) != 1 || post[0]["status"] != "blocked" || post[0]["error_type"] != "plugin_block" || post[0]["error_message"] != "not allowed" ||
		!reflect.DeepEqual(post[0]["args"], map[string]any{"q": "test", "limit": 3}) {
		t.Fatalf("post %v", post)
	}
}

func TestPluginApproveReachesTheDispatcherWithItsRuleKeyAndModifiedArgs(t *testing.T) {
	c := bridgePluginContext(t)
	c.RegisterHook("pre_tool_call", func(context.Context, map[string]any) (any, error) {
		return map[string]any{"action": "approve", "message": "ask first", "rule_key": " rule "}, nil
	}, false)
	c.RegisterHook("pre_tool_call", func(context.Context, map[string]any) (any, error) {
		return map[string]any{"action": "modify", "args": map[string]any{"limit": 3}}, nil
	}, false)
	directive := pluginPreToolCall(context.Background(), nil, "web_search", map[string]any{"q": "test"}, tools.ToolHookIDs{}, nil)
	want := tools.PreToolDirective{Action: "approve", Message: "ask first", RuleKey: "rule", Args: map[string]any{"q": "test", "limit": 3}}
	if !reflect.DeepEqual(directive, want) {
		t.Fatalf("directive %+v", directive)
	}
}

func TestBridgeFailsOpenWithoutPluginsAndPassesDownstreamErrorsThrough(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	plugins.ResetPluginManagersForTests()
	t.Cleanup(plugins.ResetPluginManagersForTests)
	previous := plugins.InstalledServices()
	plugins.InstallServices(nil)
	t.Cleanup(func() { plugins.InstallServices(previous) })
	ctx := context.Background()
	if d := pluginPreToolCall(ctx, nil, "web_search", map[string]any{"q": "x"}, tools.ToolHookIDs{}, nil); !reflect.DeepEqual(d, tools.PreToolDirective{}) {
		t.Fatalf("directive without services %+v", d)
	}
	if d := pluginToolRequestMiddleware(ctx, nil, "web_search", map[string]any{"q": "x"}, map[string]any{"q": "x"}, tools.ToolHookIDs{SessionID: "s"}); d.Args != nil || d.Trace != nil {
		t.Fatalf("request directive without middleware %+v", d)
	}
	if text, replaced := pluginTransformToolResult(ctx, nil, tools.PostToolCallEvent{ToolName: "web_search", Result: "r"}); replaced || text != "" {
		t.Fatalf("transform without hooks %q %v", text, replaced)
	}
	boom := errors.New("handler failed")
	out, err := pluginToolExecutionMiddleware(ctx, nil, "web_search", map[string]any{"q": "x"}, nil, tools.ToolHookIDs{},
		func(args map[string]any) (string, error) {
			if !reflect.DeepEqual(args, map[string]any{"q": "x"}) {
				t.Errorf("downstream args %v", args)
			}
			return "", boom
		})
	if out != "" || err != boom {
		t.Fatalf("got %q %v", out, err)
	}
	if got := toolMiddlewareResultText(map[string]any{"a": "<b>"}); got != `{"a":"<b>"}` {
		t.Fatalf("json text %q", got)
	}
	for errorType, want := range map[string]any{"": nil, "tool_error": nil, "plugin_block": nil, "edit_approval_denied": nil, "edit_approval_error": nil, "ValueError": ""} {
		if got := toolEventErrorMessage(tools.PostToolCallEvent{ErrorType: errorType}); got != want {
			t.Fatalf("%q message %v", errorType, got)
		}
	}
	if got := toolEventErrorMessage(tools.PostToolCallEvent{ErrorType: "ValueError", ErrorMessage: "bad"}); got != "bad" {
		t.Fatalf("message %v", got)
	}
}
