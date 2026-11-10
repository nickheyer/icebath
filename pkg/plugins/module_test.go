package plugins

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Active profile with discovery marked done and lifecycle hooks routed to its manager
func directiveProfile(t *testing.T, cfg string) (context.Context, *Manager) {
	t.Helper()
	ctx, m := streamProfile(t, cfg)
	m.discovered = true
	saved := installedServices()
	InstallServices(&Services{
		InvokeLifecycleHook: func(ctx context.Context, hook string, kw map[string]any) []any {
			return GetPluginManager(ctx).InvokeHook(ctx, hook, kw)
		},
		RequestToolApproval: func(_ context.Context, tool, msg, rule, turn, call, session string) (map[string]any, error) {
			if rule == "explode" {
				return nil, &runtimeError{"gate down"}
			}
			return map[string]any{"approved": rule == "ok", "message": ""}, nil
		},
		FailoverReason: func(v any) (any, bool) {
			s, ok := v.(string)
			return s, ok && (s == "rate_limit" || s == "context_overflow")
		},
	})
	t.Cleanup(func() { InstallServices(saved) })
	return ctx, m
}

// Hook callback returning a fixed value
func returns(v any) HookFunc {
	return func(context.Context, map[string]any) (any, error) { return v, nil }
}

func TestPreToolCallDirectivePrecedence(t *testing.T) {
	ctx, m := directiveProfile(t, "")
	addHook(m, "pre_tool_call", returns(map[string]any{"action": "approve", "message": "first", "rule_key": " grain "}))
	addHook(m, "pre_tool_call", returns(map[string]any{"action": "approve", "message": "second"}))
	addHook(m, "pre_tool_call", returns(map[string]any{"action": "modify", "args": map[string]any{"b": 2}}))
	addHook(m, "pre_tool_call", returns(map[string]any{"action": "block"}))
	addHook(m, "pre_tool_call", returns("noise"))
	d, err := preToolCallDirectiveDetails(ctx, "t", map[string]any{"a": 1}, PreToolCallIDs{})
	if err != nil || d.Action != "approve" || d.Message != "first" || d.RuleKey != "grain" || !reflect.DeepEqual(d.ModifiedArgs, map[string]any{"a": 1, "b": 2}) {
		t.Fatalf("%+v %v", d, err)
	}
	addHook(m, "pre_tool_call", returns(map[string]any{"action": "block", "message": "veto"}))
	addHook(m, "pre_tool_call", returns(map[string]any{"action": "modify", "args": map[string]any{"c": 3}}))
	d, _ = preToolCallDirectiveDetails(ctx, "t", map[string]any{"a": 1}, PreToolCallIDs{})
	if d.Action != "block" || d.Message != "veto" || d.ModifiedArgs["c"] != nil {
		t.Fatalf("%+v", d)
	}
	if msg, _ := GetPreToolCallBlockMessage(ctx, "t", nil, PreToolCallIDs{}); msg != "veto" {
		t.Fatalf("block message %q", msg)
	}
}

func TestPreToolCallModifyLastWinsAndNoneWithoutHooks(t *testing.T) {
	ctx, m := directiveProfile(t, "")
	if _, args, _ := DispatchPreToolCallHooks(ctx, "t", map[string]any{"a": 1}, PreToolCallIDs{}); args != nil {
		t.Fatal("modified args without hooks")
	}
	addHook(m, "pre_tool_call", returns(map[string]any{"action": "modify", "args": map[string]any{"a": 2}}))
	addHook(m, "pre_tool_call", returns(map[string]any{"action": "modify", "args": map[string]any{"a": 3}}))
	addHook(m, "pre_tool_call", returns(map[string]any{"action": "modify", "args": "bad"}))
	if msg, args, _ := DispatchPreToolCallHooks(ctx, "t", nil, PreToolCallIDs{}); msg != "" || !reflect.DeepEqual(args, map[string]any{"a": 3}) {
		t.Fatalf("%q %v", msg, args)
	}
}

func TestResolvePreToolBlockRunsApprovalGateFailClosed(t *testing.T) {
	for rule, want := range map[string]string{"ok": "", "deny": "BLOCKED: plugin approval required for t", "explode": "BLOCKED: plugin approval gate failed for t"} {
		ctx, m := directiveProfile(t, "")
		addHook(m, "pre_tool_call", returns(map[string]any{"action": "approve", "rule_key": rule}))
		if got, err := ResolvePreToolBlock(ctx, "t", nil, PreToolCallIDs{}); got != want || err != nil {
			t.Fatalf("%s: %q %v", rule, got, err)
		}
	}
}

func TestPreToolCallTimeoutFailsClosedThroughResolve(t *testing.T) {
	ctx, m := directiveProfile(t, "plugins:\n  hook_callback_timeout: 0.1\n")
	hold := make(chan struct{})
	defer close(hold)
	addHook(m, "pre_tool_call", func(context.Context, map[string]any) (any, error) { <-hold; return nil, nil })
	t0 := time.Now()
	for _, q := range []string{"x", "y"} {
		if msg, _ := ResolvePreToolBlock(ctx, "web_search", map[string]any{"query": q}, PreToolCallIDs{}); msg != preToolCallTimeoutBlockMessage {
			t.Fatalf("msg %q", msg)
		}
	}
	if time.Since(t0) > 5*time.Second {
		t.Fatal("caller blocked")
	}
}

func TestToolWhitelistIsContextScoped(t *testing.T) {
	ctx, m := directiveProfile(t, "")
	seen := 0
	addHook(m, "pre_tool_call", func(context.Context, map[string]any) (any, error) { seen++; return nil, nil })
	limited := WithToolWhitelist(ctx, map[string]bool{"read_file": true}, "")
	if msg, _ := ResolvePreToolBlock(limited, "read_file", nil, PreToolCallIDs{}); msg != "" || seen != 1 {
		t.Fatalf("allowed tool %q seen %d", msg, seen)
	}
	if msg, _ := ResolvePreToolBlock(limited, "terminal", nil, PreToolCallIDs{}); msg != "Tool 'terminal' denied: not in this thread's tool whitelist" || seen != 1 {
		t.Fatalf("denied %q", msg)
	}
	if msg, _ := ResolvePreToolBlock(ClearToolWhitelist(limited), "terminal", nil, PreToolCallIDs{}); msg != "" || seen != 2 {
		t.Fatalf("cleared %q", msg)
	}
	if msg, _ := ResolvePreToolBlock(ctx, "terminal", nil, PreToolCallIDs{}); msg != "" {
		t.Fatalf("parent context restricted %q", msg)
	}
}

func TestPreVerifyContinueMessage(t *testing.T) {
	ctx, m := directiveProfile(t, "")
	if GetPreVerifyContinueMessage(ctx, PreVerifyInput{}) != "" {
		t.Fatal("message without hooks")
	}
	var got map[string]any
	addHook(m, "pre_verify", func(_ context.Context, kw map[string]any) (any, error) {
		got = kw
		return map[string]any{"action": "stop", "message": "x"}, nil
	})
	addHook(m, "pre_verify", returns(map[string]any{"decision": "Block", "reason": "  keep going  "}))
	msg := GetPreVerifyContinueMessage(ctx, PreVerifyInput{Coding: true, Attempt: 2, ChangedPaths: []string{"a.go"}})
	if msg != "keep going" || got["coding"] != true || got["attempt"] != 2 || !reflect.DeepEqual(got["changed_paths"], []any{"a.go"}) {
		t.Fatalf("%q %v", msg, got)
	}
}

func TestPluginErrorClassificationRunAllPickFirst(t *testing.T) {
	ctx, m := directiveProfile(t, "")
	logs := captureLogs(t)
	addHook(m, "transform_api_error_classification", returns(map[string]any{"reason": "nonsense"}))
	addHook(m, "transform_api_error_classification", returns(map[string]any{"reason": " Rate_Limit ", "retryable": 1, "message": strings.Repeat("m", 600), "error_context": map[string]any{"k": "v"}}))
	addHook(m, "transform_api_error_classification", returns(map[string]any{"reason": "context_overflow"}))
	got, err := GetPluginErrorClassification(ctx, ErrorClassificationInput{Provider: "p"})
	if err != nil || got["reason"] != "rate_limit" || got["retryable"] != true || len(got["message"].(string)) != 500 || got["error_context"] == nil {
		t.Fatalf("%v %v", got, err)
	}
	if !strings.Contains(logs.String(), "skipped 1 valid classification(s)") {
		t.Fatal("no conflict warning")
	}
}

func TestPluginManagerScopedByHomeAndPublishedHosts(t *testing.T) {
	ResetPluginManagersForTests()
	t.Cleanup(ResetPluginManagersForTests)
	a, b := homeCtx(t.TempDir()), homeCtx(t.TempDir())
	ma, mb := GetPluginManager(a), GetPluginManager(b)
	if ma == mb || GetPluginManager(a) != ma {
		t.Fatal("managers not keyed per home")
	}
	owner := &struct{ n int }{}
	var calls int
	PublishGatewayMessageHost(owner, func(map[string]any) bool { calls++; return true })
	PublishTUIMessageHost(owner, func(map[string]any) bool { return true })
	mc := GetPluginManager(homeCtx(t.TempDir()))
	if !ma.HasGatewayMessageInjector() || !mc.HasGatewayMessageInjector() || !mc.HasTUIMessageInjector() || !mc.InjectGatewayMessage(nil) || calls != 1 {
		t.Fatal("published host not stamped")
	}
	ClearPublishedGatewayMessageHost(&struct{ n int }{})
	if !mb.HasGatewayMessageInjector() {
		t.Fatal("foreign owner cleared the host")
	}
	ClearPublishedGatewayMessageHost(owner)
	ClearPublishedTUIMessageHost(owner)
	if mb.HasGatewayMessageInjector() || mc.HasTUIMessageInjector() || mb.InjectTUIMessage(nil) {
		t.Fatal("owner clear left a host")
	}
}

func TestActiveConfigKeyManifestsAndDisabledPlugins(t *testing.T) {
	h := t.TempDir()
	for name, keys := range map[string]string{"lit": "alpha.key", "dark": "beta.key", "idle": "gamma.key"} {
		dir := filepath.Join(h, "plugins", name)
		writePluginDir(t, dir, "name: "+name+"\nversion: 1.0.0\nconfig_keys: ["+keys+"]\n")
	}
	cfg := "plugins:\n  enabled: [lit, dark]\n  disabled: [dark]\n"
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := DisabledPlugins(h); !reflect.DeepEqual(got, map[string]bool{"dark": true}) {
		t.Fatalf("disabled %v", got)
	}
	var names []string
	for _, m := range ActiveConfigKeyManifests(h) {
		if m.Name == "lit" || m.Name == "dark" || m.Name == "idle" {
			names = append(names, m.Name)
		}
	}
	if !reflect.DeepEqual(names, []string{"lit"}) {
		t.Fatalf("active %v", names)
	}
}

func TestPyTitle(t *testing.T) {
	if got := pyTitle("web search_x2y"); got != "Web Search_X2Y" {
		t.Fatalf("%q", got)
	}
}

func TestPreToolCallDirectiveDetailsIsTheExportedFormOfTheResolver(t *testing.T) {
	ctx, m := directiveProfile(t, "")
	addHook(m, "pre_tool_call", returns(map[string]any{"action": "modify", "args": map[string]any{"b": 2}}))
	addHook(m, "pre_tool_call", returns(map[string]any{"action": "approve", "message": "ask", "rule_key": " grain "}))
	got, err := PreToolCallDirectiveDetails(ctx, "t", map[string]any{"a": 1}, PreToolCallIDs{TaskID: "task"})
	want, wantErr := preToolCallDirectiveDetails(ctx, "t", map[string]any{"a": 1}, PreToolCallIDs{TaskID: "task"})
	if err != nil || wantErr != nil || !reflect.DeepEqual(got, want) || got.Action != "approve" || got.RuleKey != "grain" || !reflect.DeepEqual(got.ModifiedArgs, map[string]any{"a": 1, "b": 2}) {
		t.Fatalf("got %+v %v want %+v %v", got, err, want, wantErr)
	}
}
