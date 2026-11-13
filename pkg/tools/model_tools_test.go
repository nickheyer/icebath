package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type hookLog struct {
	mu        sync.Mutex
	pre       [][]map[string]any
	preArgs   []map[string]any
	post      []PostToolCallEvent
	transform []PostToolCallEvent
}

// Dispatcher over a fresh registry with recording observers and the named fake tools
func modelToolsDispatcher(t *testing.T, handler Handler, names ...string) (*Dispatcher, *hookLog, *Env) {
	t.Helper()
	r := NewRegistry()
	for _, n := range names {
		r.Register(Def{Name: n, Toolset: "test", Schema: map[string]any{"name": n, "description": n, "parameters": map[string]any{"type": "object"}}, Handler: handler})
	}
	d := NewDispatcher(r)
	log := &hookLog{}
	d.PreToolCall = append(d.PreToolCall, func(ctx context.Context, env *Env, name string, args map[string]any, ids ToolHookIDs, trace []map[string]any) PreToolDirective {
		log.mu.Lock()
		log.pre = append(log.pre, trace)
		log.preArgs = append(log.preArgs, args)
		log.mu.Unlock()
		return PreToolDirective{}
	})
	d.PostToolCall = append(d.PostToolCall, func(ctx context.Context, env *Env, ev PostToolCallEvent) {
		log.mu.Lock()
		log.post = append(log.post, ev)
		log.mu.Unlock()
	})
	d.TransformToolResult = append(d.TransformToolResult, func(ctx context.Context, env *Env, ev PostToolCallEvent) (string, bool) {
		log.mu.Lock()
		log.transform = append(log.transform, ev)
		log.mu.Unlock()
		return "", false
	})
	return d, log, &Env{Home: t.TempDir()}
}

func decodeModelToolsResult(t *testing.T, s string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		t.Fatalf("result %q: %v", s, err)
	}
	return out
}

func TestAgentLoopToolReturnsError(t *testing.T) {
	d, _, env := modelToolsDispatcher(t, nil)
	for name := range AgentLoopTools {
		if _, ok := decodeModelToolsResult(t, d.Dispatch(context.Background(), &Call{Name: name, Env: env}))["error"]; !ok {
			t.Fatalf("%s did not error", name)
		}
	}
}

func TestHandleFunctionCallUnknownToolReturnsError(t *testing.T) {
	d, _, env := modelToolsDispatcher(t, nil)
	out := decodeModelToolsResult(t, d.Dispatch(context.Background(), &Call{Name: "totally_fake_tool_xyz", Env: env}))
	if e, _ := out["error"].(string); !strings.Contains(e, "totally_fake_tool_xyz") {
		t.Fatalf("result %v", out)
	}
}

func TestPostToolCallReceivesNonNegativeIntegerDurationMS(t *testing.T) {
	d, log, env := modelToolsDispatcher(t, func(ctx context.Context, call *Call) string { return `{"ok":true}` }, "web_search")
	d.Dispatch(context.Background(), &Call{Name: "web_search", Args: map[string]any{"q": "test"}, TaskID: "t1", Env: env})
	if len(log.pre) != 1 || len(log.post) != 1 || len(log.transform) != 1 {
		t.Fatalf("hooks pre %d post %d transform %d", len(log.pre), len(log.post), len(log.transform))
	}
	if log.post[0].DurationMS < 0 || log.post[0].DurationMS != log.transform[0].DurationMS {
		t.Fatalf("durations %d %d", log.post[0].DurationMS, log.transform[0].DurationMS)
	}
}

func TestTerminalNonzeroExitIsReportedAsError(t *testing.T) {
	result := `{"output": "", "exit_code": 1, "error": null}`
	d, log, env := modelToolsDispatcher(t, func(ctx context.Context, call *Call) string { return result }, "terminal")
	if got := d.Dispatch(context.Background(), &Call{Name: "terminal", Args: map[string]any{"command": "false"}, Env: env}); got != result {
		t.Fatalf("result %q", got)
	}
	for _, ev := range []PostToolCallEvent{log.post[0], log.transform[0]} {
		if ev.Status != "error" || ev.ErrorType != "tool_error" || ev.ErrorMessage != "exit 1" {
			t.Fatalf("event %+v", ev)
		}
	}
}

func TestToolRequestAndExecutionMiddlewareWrapRegistryDispatch(t *testing.T) {
	var dispatched map[string]any
	d, log, env := modelToolsDispatcher(t, func(ctx context.Context, call *Call) string {
		dispatched = call.Args
		return JSON(map[string]any{"ok": true, "args": call.Args})
	}, "web_search")
	var executionArgs map[string]any
	d.ToolRequestMiddleware = append(d.ToolRequestMiddleware, func(ctx context.Context, env *Env, name string, args, originalArgs map[string]any, ids ToolHookIDs) RequestMiddlewareDirective {
		next := map[string]any{"rewritten": true}
		for k, v := range args {
			next[k] = v
		}
		return RequestMiddlewareDirective{Args: next, Source: "test-middleware", Reason: "rewrite"}
	})
	d.ToolExecutionMiddleware = append(d.ToolExecutionMiddleware, func(ctx context.Context, env *Env, name string, args, originalArgs map[string]any, ids ToolHookIDs, next func(map[string]any) (string, error)) (string, error) {
		executionArgs = args
		wrapped := map[string]any{"wrapped": true}
		for k, v := range args {
			wrapped[k] = v
		}
		return next(wrapped)
	})
	out := decodeModelToolsResult(t, d.Dispatch(context.Background(), &Call{Name: "web_search", Args: map[string]any{"q": "test"}, TaskID: "task-1", ToolCallID: "tool-1", SessionID: "session-1", Env: env}))
	if !reflect.DeepEqual(executionArgs, map[string]any{"q": "test", "rewritten": true}) {
		t.Fatalf("execution args %v", executionArgs)
	}
	want := map[string]any{"q": "test", "rewritten": true, "wrapped": true}
	if !reflect.DeepEqual(dispatched, want) || !reflect.DeepEqual(out["args"], want) {
		t.Fatalf("dispatched %v result %v", dispatched, out)
	}
	trace := []map[string]any{{"source": "test-middleware", "reason": "rewrite"}}
	if !reflect.DeepEqual(log.pre[0], trace) || !reflect.DeepEqual(log.post[0].MiddlewareTrace, trace) {
		t.Fatalf("traces %v %v", log.pre[0], log.post[0].MiddlewareTrace)
	}
}

type interruptRaised struct{}

func (interruptRaised) Error() string { return "interrupted" }
func (interruptRaised) Unwrap() error { return ErrKeyboardInterrupt }

func TestRegistryExceptionEmitsTerminalToolHook(t *testing.T) {
	ran := false
	d, log, env := modelToolsDispatcher(t, func(ctx context.Context, call *Call) string { ran = true; return `{"ok":true}` }, "web_search")
	d.ToolExecutionMiddleware = append(d.ToolExecutionMiddleware, func(ctx context.Context, env *Env, name string, args, originalArgs map[string]any, ids ToolHookIDs, next func(map[string]any) (string, error)) (string, error) {
		return "", interruptRaised{}
	})
	out := decodeModelToolsResult(t, d.Dispatch(context.Background(), &Call{Name: "web_search", Args: map[string]any{"q": "test"}, TaskID: "task-1", SessionID: "session-1", ToolCallID: "tool-1", Env: env}))
	if _, ok := out["error"]; !ok || ran {
		t.Fatalf("result %v ran %v", out, ran)
	}
	if len(log.post) != 1 || log.post[0].Status != "error" || log.post[0].ErrorType != "interruptRaised" || log.post[0].DurationMS < 0 {
		t.Fatalf("post %+v", log.post)
	}
}

func TestKeyboardInterruptEscapesMiddleware(t *testing.T) {
	d, _, env := modelToolsDispatcher(t, func(ctx context.Context, call *Call) string { return `{"ok":true}` }, "web_search")
	d.ToolExecutionMiddleware = append(d.ToolExecutionMiddleware, func(ctx context.Context, env *Env, name string, args, originalArgs map[string]any, ids ToolHookIDs, next func(map[string]any) (string, error)) (string, error) {
		return "", interruptRaised{}
	})
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(ErrKeyboardInterrupt)
	_, err := d.RunToolExecutionMiddleware(ctx, env, "web_search", map[string]any{}, nil, ToolHookIDs{}, func(map[string]any) (string, error) {
		t.Fatal("frame failure under a keyboard interrupt cause fell through to dispatch")
		return "", nil
	})
	if !IsKeyboardInterrupt(ctx, err) {
		t.Fatalf("err %v", err)
	}
}

func TestACPEditDenialEmitsBlockedTerminalToolHook(t *testing.T) {
	d, log, env := modelToolsDispatcher(t, func(ctx context.Context, call *Call) string {
		t.Fatal("denied edit must not dispatch")
		return ""
	}, "write_file")
	d.EditApproval = func(ctx context.Context, env *Env, name string, args map[string]any) (string, error) {
		return `{"error": "Edit approval denied"}`, nil
	}
	out := decodeModelToolsResult(t, d.Dispatch(context.Background(), &Call{Name: "write_file", Args: map[string]any{"path": "private.txt", "content": "private"}, TaskID: "task-1", Env: env}))
	if !reflect.DeepEqual(out, map[string]any{"error": "Edit approval denied"}) || log.post[0].Status != "blocked" || log.post[0].ErrorType != "edit_approval_denied" {
		t.Fatalf("result %v post %+v", out, log.post)
	}
}

// Dispatches tool under a blocking pre tool call hook with a primed read loop tracker
func blockedToolDispatch(t *testing.T, tool string) (map[string]any, *hookLog, *taskTracker) {
	t.Helper()
	d, log, env := modelToolsDispatcher(t, func(ctx context.Context, call *Call) string {
		t.Fatal("dispatch should not run when blocked")
		return ""
	}, tool)
	d.PreToolCall = append(d.PreToolCall, func(ctx context.Context, env *Env, name string, args map[string]any, ids ToolHookIDs, trace []map[string]any) PreToolDirective {
		return PreToolDirective{Action: "block", Message: "Blocked by policy"}
	})
	td := taskData("t1-blocked")
	readTrackerLock.Lock()
	td.consecutive = 3
	readTrackerLock.Unlock()
	t.Cleanup(func() { ClearFileOpsCache("t1-blocked") })
	out := decodeModelToolsResult(t, d.Dispatch(context.Background(), &Call{Name: tool, Args: map[string]any{"path": "test.txt"}, TaskID: "t1-blocked", Env: env}))
	return out, log, td
}

func TestBlockedToolReturnsErrorAndSkipsDispatch(t *testing.T) {
	out, log, _ := blockedToolDispatch(t, "read_file")
	if !reflect.DeepEqual(out, map[string]any{"error": "Blocked by policy"}) {
		t.Fatalf("result %v", out)
	}
	ev := log.post[0]
	if ev.Status != "blocked" || ev.ErrorType != "plugin_block" || ev.ErrorMessage != "Blocked by policy" || ev.DurationMS != 0 {
		t.Fatalf("post %+v", ev)
	}
}

func TestBlockedToolSkipsReadLoopNotification(t *testing.T) {
	out, _, td := blockedToolDispatch(t, "web_search")
	if !reflect.DeepEqual(out, map[string]any{"error": "Blocked by policy"}) {
		t.Fatalf("result %v", out)
	}
	readTrackerLock.Lock()
	defer readTrackerLock.Unlock()
	if td.consecutive != 3 {
		t.Fatal("blocked call notified the read loop tracker")
	}
}

func TestInvalidHookReturnsDoNotBlock(t *testing.T) {
	d, _, env := modelToolsDispatcher(t, func(ctx context.Context, call *Call) string { return `{"ok": true}` }, "read_file")
	d.PreToolCall = append(d.PreToolCall, func(ctx context.Context, env *Env, name string, args map[string]any, ids ToolHookIDs, trace []map[string]any) PreToolDirective {
		return PreToolDirective{Action: "block"}
	}, func(ctx context.Context, env *Env, name string, args map[string]any, ids ToolHookIDs, trace []map[string]any) PreToolDirective {
		return PreToolDirective{Action: "deny", Message: "nope"}
	})
	if out := decodeModelToolsResult(t, d.Dispatch(context.Background(), &Call{Name: "read_file", Args: map[string]any{"path": "test.txt"}, TaskID: "t1", Env: env})); !reflect.DeepEqual(out, map[string]any{"ok": true}) {
		t.Fatalf("result %v", out)
	}
}

func TestRelayRewriteIsVisibleToPreToolAuthorization(t *testing.T) {
	var dispatched map[string]any
	d, log, env := modelToolsDispatcher(t, func(ctx context.Context, call *Call) string { dispatched = call.Args; return `{"ok": true}` }, "read_file")
	saved := relayToolRequestIntercepts
	t.Cleanup(func() { relayToolRequestIntercepts = saved })
	relayToolRequestIntercepts = func(ctx context.Context, sessionID, toolName string, args map[string]any) (map[string]any, error) {
		if toolName != "read_file" || sessionID != "s1" {
			t.Fatalf("relay saw %s %s", toolName, sessionID)
		}
		out := map[string]any{}
		for k, v := range args {
			out[k] = v
		}
		out["path"] = "approved.txt"
		return out, nil
	}
	d.Dispatch(context.Background(), &Call{Name: "read_file", Args: map[string]any{"path": "original.txt"}, TaskID: "t1", SessionID: "s1", Env: env})
	if log.preArgs[0]["path"] != "approved.txt" || dispatched["path"] != "approved.txt" {
		t.Fatalf("pre %v dispatch %v", log.preArgs[0], dispatched)
	}
	if !reflect.DeepEqual(log.post[0].MiddlewareTrace, []map[string]any{{"source": "nemo_relay"}}) {
		t.Fatalf("trace %v", log.post[0].MiddlewareTrace)
	}
}

func TestHandleFunctionCallUsesBoundDispatcherAndRawArgs(t *testing.T) {
	var seen *Call
	d, log, env := modelToolsDispatcher(t, func(ctx context.Context, call *Call) string { seen = call; return `{"ok": true}` }, "read_file")
	ctx := WithDispatcher(context.Background(), d)
	out := HandleFunctionCall(ctx, &Call{Name: "read_file", Args: map[string]any{"path": "a", "limit": 2.0}, RawArgs: `{"path": "a", "limit": 2}`, TaskID: "t1", Env: env})
	if out != `{"ok": true}` || seen.RawArgs != `{"path": "a", "limit": 2}` || seen.Args["path"] != "a" || len(log.post) != 1 {
		t.Fatalf("out %q seen %+v posts %d", out, seen, len(log.post))
	}
	HandleFunctionCall(ctx, &Call{Name: "read_file", RawArgs: `["not", "an", "object"]`, TaskID: "t1", Env: env})
	if seen.Args == nil || len(seen.Args) != 0 {
		t.Fatalf("non object arguments reached the handler as %v", seen.Args)
	}
	HandleFunctionCall(SuppressPostToolCallHook(ctx, true), &Call{Name: "read_file", Args: map[string]any{}, Env: env})
	if len(log.post) != 2 {
		t.Fatalf("suppressed context still emitted post_tool_call, %d events", len(log.post))
	}
	if DispatcherOf(context.Background()) != DefaultDispatcher {
		t.Fatal("unbound context did not use the process dispatcher")
	}
}

func TestNestedHandlerCallsInheritDispatcherAndSuppression(t *testing.T) {
	var inner *Dispatcher
	var innerSuppressed bool
	d, _, env := modelToolsDispatcher(t, func(ctx context.Context, call *Call) string {
		inner, innerSuppressed = DispatcherOf(ctx), PostToolCallHookSuppressed(ctx)
		return `{}`
	}, "execute_code")
	d.DispatchWith(context.Background(), &Call{Name: "execute_code", Args: map[string]any{}, Env: env}, DispatchOptions{SuppressPostToolCallHook: true})
	if inner != d || !innerSuppressed {
		t.Fatalf("inner dispatcher %p suppressed %v", inner, innerSuppressed)
	}
}

func TestSanitizeToolErrorCountsCodePoints(t *testing.T) {
	if got := SanitizeToolError(strings.Repeat("é", toolErrorMaxLen)); got != "[TOOL_ERROR] "+strings.Repeat("é", toolErrorMaxLen) {
		t.Fatalf("message at the limit truncated to len %d", len(got))
	}
	got := SanitizeToolError(strings.Repeat("é", toolErrorMaxLen+1))
	if want := "[TOOL_ERROR] " + strings.Repeat("é", toolErrorMaxLen-3) + "..."; got != want {
		t.Fatalf("len %d", len(got))
	}
	if got := SanitizeToolError(strings.Repeat("é", 10)); got != "[TOOL_ERROR] "+strings.Repeat("é", 10) {
		t.Fatalf("short message truncated %q", got)
	}
}

func TestObserverFieldsRenderErrorValuesLikePythonStr(t *testing.T) {
	if _, _, msg := toolResultObserverFields("web_search", `{"error": {"ok": true, "code": 1, "f": 2.50, "n": NaN}}`); msg != "{'ok': True, 'code': 1, 'f': 2.5, 'n': nan}" {
		t.Fatalf("message %q", msg)
	}
}

func TestToolCallBadArgsError(t *testing.T) {
	d, _, env := modelToolsDispatcher(t, nil)
	if out := decodeModelToolsResult(t, d.Dispatch(context.Background(), &Call{Name: "tool_call", Args: map[string]any{}, Env: env})); out["error"] == nil || out["error"] == "" {
		t.Fatalf("result %v", out)
	}
}

// Quiet tool definition names of a toolset grant
func bundleToolNames(t *testing.T, enabled, disabled []string) map[string]bool {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	out := map[string]bool{}
	for _, td := range Default.ToolDefinitions(context.Background(), &Env{Home: t.TempDir()}, DefinitionOptions{Enabled: enabled, Disabled: disabled, Quiet: true, ContextLength: 200000}) {
		out[toolDefName(td)] = true
	}
	return out
}

func TestDisablingPlatformBundlePreservesCoreTools(t *testing.T) {
	if a, b := bundleToolNames(t, []string{"icebath-telegram"}, nil), bundleToolNames(t, []string{"icebath-telegram"}, []string{"icebath-yuanbao"}); !reflect.DeepEqual(a, b) {
		t.Fatalf("tools lost after disabling icebath-yuanbao %v %v", a, b)
	}
}

func TestDisablingPlatformBundleRemovesOwnTools(t *testing.T) {
	if bundleToolNames(t, []string{"icebath-discord"}, []string{"icebath-discord"})["discord"] {
		t.Fatal("discord survived disabling its bundle")
	}
}

func TestDisablingBrowserKeepsWebSearchWhenWebEnabled(t *testing.T) {
	ctx := context.Background()
	browser := map[string]bool{}
	for _, n := range Default.ResolveToolset(ctx, "", "browser") {
		browser[n] = true
	}
	if browser["web_search"] || !browser["browser_exec"] {
		t.Fatalf("browser toolset %v", browser)
	}
	for _, ts := range []string{"web", "search"} {
		if !strings.Contains(","+strings.Join(Default.ResolveToolset(ctx, "", ts), ",")+",", ",web_search,") {
			t.Fatalf("%s lacks web_search", ts)
		}
	}
}

func TestBundleNonCoreToolsUnknownFallsBack(t *testing.T) {
	if got := Default.BundleNonCoreTools(context.Background(), "", "icebath-does-not-exist"); len(got) != 0 {
		t.Fatalf("unknown bundle %v", got)
	}
}

func TestDisablingCodingPreservesCoreButAtomicDisablesStillRemove(t *testing.T) {
	baseline := bundleToolNames(t, nil, nil)
	noCoding := bundleToolNames(t, nil, []string{"coding"})
	present := 0
	for _, n := range []string{"terminal", "read_file", "write_file", "web_search", "execute_code"} {
		if baseline[n] {
			present++
			if !noCoding[n] {
				t.Fatalf("core tool %s stripped by disabling coding", n)
			}
		}
	}
	if present == 0 {
		t.Fatal("no probed core tools present in baseline")
	}
	if bundleToolNames(t, nil, []string{"terminal"})["terminal"] || bundleToolNames(t, nil, []string{"file"})["write_file"] {
		t.Fatal("atomic toolset disable did not remove its tools")
	}
}

func TestToolDefsCacheKeySeesConfigReplacementWithPinnedMtime(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfg, []byte("mcp_servers:\n  aa: {command: a}\n"), 0o600)
	env := &Env{Home: dir}
	ctx := context.Background()
	before := toolDefsCacheKey(ctx, Default, env, DefinitionOptions{})
	st, _ := os.Stat(cfg)
	time.Sleep(20 * time.Millisecond)
	os.WriteFile(cfg, []byte("mcp_servers:\n  bb: {command: b}\n"), 0o600)
	os.Chtimes(cfg, st.ModTime(), st.ModTime())
	if toolDefsCacheKey(ctx, Default, env, DefinitionOptions{}) == before {
		t.Fatal("replaced config with a pinned mtime kept the memo key")
	}
}

func TestToolCallRejectsOutOfScopeAndUnwrapsInScope(t *testing.T) {
	env := searchEnv(t)
	var mu sync.Mutex
	var calls []*Call
	Default.Register(Def{Name: "mcp_x", Toolset: "mcp-x", Schema: map[string]any{"name": "mcp_x", "description": "x",
		"parameters": map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "integer"}}}},
		Handler: func(ctx context.Context, call *Call) string {
			mu.Lock()
			calls = append(calls, call)
			mu.Unlock()
			return `{"ok": true}`
		}})
	t.Cleanup(func() { Default.Remove("mcp_x") })
	ctx := WithDispatcher(context.Background(), NewDispatcher(Default))
	wrapped := map[string]any{"name": "mcp_x", "arguments": map[string]any{"a": 1}}
	env.Enabled = []string{"mcp-other"}
	out := decodeModelToolsResult(t, HandleFunctionCall(ctx, &Call{Name: ToolCallName, Args: wrapped, Env: env}))
	if out["error"] != "'mcp_x' is not available in this session. Use tool_search to find tools you can call." || len(calls) != 0 {
		t.Fatalf("out of scope result %v calls %d", out, len(calls))
	}
	env.Enabled = []string{"mcp-x"}
	raw := HandleFunctionCall(ctx, &Call{Name: ToolCallName, Args: wrapped, TaskID: "t", Env: env})
	if out := decodeModelToolsResult(t, raw); len(out) != 1 || out["ok"] != true {
		t.Fatalf("in scope result %s", raw)
	}
	if len(calls) != 1 || calls[0].Name != "mcp_x" || fmt.Sprint(calls[0].Args) != "map[a:1]" || calls[0].TaskID != "t" {
		t.Fatalf("underlying dispatch %+v", calls)
	}
}

// Tool definitions led by head followed by bare named tools
func rewriterDefs(head map[string]any, names ...string) []map[string]any {
	out := []map[string]any{head}
	for _, n := range names {
		out = append(out, toolDef(n, n+".", nil))
	}
	return out
}

// Description of the named tool after the dynamic schema pass, false when the pass dropped it
func rewrittenDescription(rendered []map[string]any, name string) (string, bool) {
	for _, td := range rendered {
		if toolDefName(td) == name {
			return toolDefDescription(td), true
		}
	}
	return "", false
}

func TestSilentWithoutWebTools(t *testing.T) {
	cdp := toolDef("browser_cdp", "Send a raw Chrome DevTools Protocol command.", nil)
	execDef := toolDef("browser_exec", "Drive the page."+ScreenshotsDetached, nil)
	for _, td := range applyDynamicSchemas(nil, []map[string]any{execDef, cdp, toolDef("terminal", "terminal.", nil)}) {
		if desc := toolDefDescription(td); strings.Contains(desc, "web_search") || strings.Contains(desc, "web_extract") {
			t.Fatalf("%s names an absent web tool %q", toolDefName(td), desc)
		}
	}
}

func TestBrowserCdpNamesWebExtractOnlyWhenPresent(t *testing.T) {
	cdp := toolDef("browser_cdp", "Send a raw Chrome DevTools Protocol command.", nil)
	if desc, _ := rewrittenDescription(applyDynamicSchemas(nil, rewriterDefs(cdp, "terminal", "web_extract")), "browser_cdp"); !strings.HasSuffix(desc, " The web_extract tool is available for fetching CDP documentation URLs.") {
		t.Fatalf("cdp description without the web_extract pointer %q", desc)
	}
	if _, kept := rewrittenDescription(applyDynamicSchemas(nil, rewriterDefs(toolDef("browser_exec", "Drive the page."+ScreenshotsDetached, nil), "web_search")), "browser_exec"); kept {
		t.Fatal("browser_exec kept in a session without terminal")
	}
}

func TestBrowserExecPointsAtVisionAnalyzeOnlyWhenTheSessionHasIt(t *testing.T) {
	for _, native := range []bool{false, true} {
		header := "Drive the page." + ScreenshotsDetached
		if native {
			header = "Drive the page. Screenshots arrive in the result."
		}
		for _, names := range [][]string{{"terminal"}, {"terminal", "vision_analyze"}} {
			desc, _ := rewrittenDescription(applyDynamicSchemas(nil, rewriterDefs(toolDef("browser_exec", header, nil), names...)), "browser_exec")
			want := !native && len(names) == 2
			if strings.Contains(desc, "pass its capture_screenshot() path to vision_analyze") != want {
				t.Fatalf("native %v names %v description %q", native, names, desc)
			}
		}
	}
}

func TestBlockAndApproveDirectivesCarryTheirModifiedArgs(t *testing.T) {
	d, log, env := modelToolsDispatcher(t, func(ctx context.Context, call *Call) string {
		t.Fatal("dispatch should not run when blocked")
		return ""
	}, "web_search")
	d.PreToolCall = append(d.PreToolCall, func(ctx context.Context, env *Env, name string, args map[string]any, ids ToolHookIDs, trace []map[string]any) PreToolDirective {
		return PreToolDirective{Action: "block", Message: "Blocked by policy", Args: map[string]any{"q": "test", "limit": 3}}
	})
	out := decodeModelToolsResult(t, d.Dispatch(context.Background(), &Call{Name: "web_search", Args: map[string]any{"q": "test"}, Env: env}))
	if !reflect.DeepEqual(out, map[string]any{"error": "Blocked by policy"}) {
		t.Fatalf("result %v", out)
	}
	if ev := log.post[0]; ev.ErrorType != "plugin_block" || !reflect.DeepEqual(ev.Args, map[string]any{"q": "test", "limit": 3}) {
		t.Fatalf("post %+v", ev)
	}
	block, modified := d.dispatchPreToolCallHooks(context.Background(), env, "web_search", map[string]any{"q": "test"}, ToolHookIDs{}, nil, DispatchOptions{})
	if block != "Blocked by policy" || !reflect.DeepEqual(modified, map[string]any{"q": "test", "limit": 3}) {
		t.Fatalf("block %q modified %v", block, modified)
	}
	silent := NewDispatcher(NewRegistry())
	silent.PreToolCall = append(silent.PreToolCall, func(ctx context.Context, env *Env, name string, args map[string]any, ids ToolHookIDs, trace []map[string]any) PreToolDirective {
		return PreToolDirective{Action: "block", Args: map[string]any{"ignored": true}}
	})
	if block, modified := silent.dispatchPreToolCallHooks(context.Background(), env, "web_search", map[string]any{"q": "test"}, ToolHookIDs{}, nil, DispatchOptions{}); block != "" || modified != nil {
		t.Fatalf("a block without a message applied %q %v", block, modified)
	}
}

func TestRequestMiddlewareTraceEntriesAreRecordedAsGiven(t *testing.T) {
	d, log, env := modelToolsDispatcher(t, func(ctx context.Context, call *Call) string { return `{"ok":true}` }, "web_search")
	chain := []map[string]any{{"source": "nemo_relay"}, {"source": "plugin", "name": "second"}}
	d.ToolRequestMiddleware = append(d.ToolRequestMiddleware, func(ctx context.Context, env *Env, name string, args, originalArgs map[string]any, ids ToolHookIDs) RequestMiddlewareDirective {
		return RequestMiddlewareDirective{Args: map[string]any{"q": "rewritten"}, Source: "outer", Trace: chain}
	})
	d.ToolRequestMiddleware = append(d.ToolRequestMiddleware, func(ctx context.Context, env *Env, name string, args, originalArgs map[string]any, ids ToolHookIDs) RequestMiddlewareDirective {
		return RequestMiddlewareDirective{Args: args, Trace: []map[string]any{}}
	})
	d.Dispatch(context.Background(), &Call{Name: "web_search", Args: map[string]any{"q": "test"}, Env: env})
	if !reflect.DeepEqual(log.pre[0], chain) || !reflect.DeepEqual(log.preArgs[0], map[string]any{"q": "rewritten"}) {
		t.Fatalf("trace %v args %v", log.pre[0], log.preArgs[0])
	}
}
