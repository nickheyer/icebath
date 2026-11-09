package agent

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/tools"
)

var inlineLiveControl = []string{"action", "subagent_id", "message"}

// Agent built inside the gateway delegation inline scope the way test_delegate_gateway_inline.py _built does
func builtInlineAgent(t *testing.T, env *tools.Env, inline bool) *AIAgent {
	t.Helper()
	a := &AIAgent{platform: "whatsapp", quietMode: true, env: env}
	loadTools(tools.GatewayDelegationInlineScope(tools.WithToolEnv(context.Background(), env), inline), a, []string{"icebath-cli"}, nil)
	return a
}

// The delegate_task function of a tools list
func delegateFunctionOf(t *testing.T, list []any) map[string]any {
	t.Helper()
	for _, item := range list {
		m, _ := item.(map[string]any)
		fn, _ := m["function"].(map[string]any)
		if fn["name"] == "delegate_task" {
			return fn
		}
	}
	t.Fatalf("delegate_task missing from %v", list)
	return nil
}

func toolNamesOf(list []any) map[string]bool {
	names := map[string]bool{}
	for _, item := range list {
		m, _ := item.(map[string]any)
		fn, _ := m["function"].(map[string]any)
		if n, _ := fn["name"].(string); n != "" {
			names[n] = true
		}
	}
	return names
}

func inlineTestEnv(t *testing.T) *tools.Env {
	t.Helper()
	env := delegationTestEnv(t, 0, false)
	os.Unsetenv("ICEBATH_SINGLE_QUERY_SESSION")
	t.Cleanup(func() {
		for !tools.DefaultProcessRegistry().CompletionQueue.Empty() {
			tools.DefaultProcessRegistry().CompletionQueue.GetNowait()
		}
	})
	return env
}

func TestTheBuildScopePicksTheSchemaAndTheAgentKeepsIt(t *testing.T) {
	env := inlineTestEnv(t)
	ctx := tools.WithToolEnv(context.Background(), env)
	background := builtInlineAgent(t, env, false)
	var registry map[string]any
	for _, d := range tools.Default.ToolDefinitions(ctx, env, tools.DefinitionOptions{Enabled: []string{"icebath-cli"}, Disabled: background.disabledToolsets, Quiet: true}) {
		if fn, _ := d["function"].(map[string]any); fn["name"] == "delegate_task" {
			registry = fn
		}
	}
	if registry == nil || sortedJSON(t, delegateFunctionOf(t, background.tools)) != sortedJSON(t, registry) {
		t.Fatalf("background schema differs from the registry definition")
	}
	inline := builtInlineAgent(t, env, true)
	joined := tools.OneshotDelegateFunction(registry)
	inlineFn := delegateFunctionOf(t, inline.tools)
	if sortedJSON(t, inlineFn) != sortedJSON(t, joined) || strings.Contains(inlineFn["description"].(string), "END YOUR TURN") {
		t.Fatalf("inline schema %v", inlineFn)
	}
	props := inlineFn["parameters"].(map[string]any)["properties"].(map[string]any)
	for _, p := range inlineLiveControl {
		if _, has := props[p]; has {
			t.Fatalf("inline schema carries live control %q", p)
		}
	}
	if !toolNamesOf(inline.tools)["clarify"] {
		t.Fatal("clarify dropped from the inline build")
	}
	mcp.RefreshAgentMCPTools(ctx, inline, mcp.RefreshOptions{Quiet: true, ContentAware: true})
	if sortedJSON(t, delegateFunctionOf(t, inline.tools)) != sortedJSON(t, joined) {
		t.Fatal("a rebuild outside the gateway scope dropped the session's variant")
	}
}

func restoreInlinePin(t *testing.T, ctx context.Context, fresh, first *AIAgent) {
	t.Helper()
	mcp.RestoreAgentToolPrefix(ctx, fresh, map[string]any{"version": mcp.ToolPinVersion(), "tools": first.tools})
}

func TestASessionKeepsItsModeUntilANewSession(t *testing.T) {
	env := inlineTestEnv(t)
	ctx := tools.WithToolEnv(context.Background(), env)
	first := builtInlineAgent(t, env, true)
	fresh := builtInlineAgent(t, env, false)
	restoreInlinePin(t, ctx, fresh, first)
	if sortedJSON(t, delegateFunctionOf(t, fresh.tools)) != sortedJSON(t, delegateFunctionOf(t, first.tools)) {
		t.Fatal("pin did not restore the joined schema")
	}
	if !sessionJoinsDelegation(fresh) || sessionJoinsDelegation(builtInlineAgent(t, env, false)) {
		t.Fatal("joined mode does not follow the pin")
	}
	background := builtInlineAgent(t, env, false)
	rebuilt := builtInlineAgent(t, env, true)
	restoreInlinePin(t, ctx, rebuilt, background)
	if sessionJoinsDelegation(rebuilt) {
		t.Fatal("a background session flipped to joined after interim was turned off")
	}
}

func TestARestoredModeSurvivesALaterToolsRefresh(t *testing.T) {
	for _, refresh := range []struct {
		name string
		opts mcp.RefreshOptions
	}{{"compaction-refresh", mcp.RefreshOptions{Quiet: true, ContentAware: true}}, {"reload-mcp", mcp.RefreshOptions{Quiet: true}}} {
		for _, pinnedInline := range []bool{true, false} {
			name := refresh.name + "-pinned-background"
			if pinnedInline {
				name = refresh.name + "-pinned-inline"
			}
			t.Run(name, func(t *testing.T) {
				env := inlineTestEnv(t)
				ctx := tools.WithToolEnv(context.Background(), env)
				first := builtInlineAgent(t, env, pinnedInline)
				fresh := builtInlineAgent(t, env, !pinnedInline)
				restoreInlinePin(t, ctx, fresh, first)
				if sessionJoinsDelegation(fresh) != pinnedInline {
					t.Fatalf("joined %v after the pin", sessionJoinsDelegation(fresh))
				}
				mcp.RefreshAgentMCPTools(ctx, fresh, refresh.opts)
				if sessionJoinsDelegation(fresh) != pinnedInline || sortedJSON(t, delegateFunctionOf(t, fresh.tools)) != sortedJSON(t, delegateFunctionOf(t, first.tools)) {
					t.Fatalf("refresh flipped the session, joined %v", sessionJoinsDelegation(fresh))
				}
			})
		}
	}
}

// Parent of test_delegate_gateway_inline.py _parent carrying a built tools list
func inlineParent(env *tools.Env, list []any, depth int) *AIAgent {
	return &AIAgent{delegateDepth: depth, sessionID: "sess", tools: list, env: env}
}

// The fast_children fixture, leaf children answering at once over credentials of model m
func fastInlineChildren(t *testing.T, env *tools.Env) {
	t.Helper()
	prevBuild, prevRun, prevCreds := buildChildPreservingParentToolsFn, runSingleChildFn, resolveDelegationCredentialsFn
	buildChildPreservingParentToolsFn = func(context.Context, childBuildSpec) (*AIAgent, error) {
		return &AIAgent{delegateRole: "leaf", subagentID: "s1"}, nil
	}
	runSingleChildFn = func(_ context.Context, i int, goal string, _, _ *AIAgent, _ string, _, _ any) *pyjson.Object {
		return pyjson.ObjectOf("task_index", i, "status", "completed", "summary", "done: "+goal, "api_calls", 1,
			"duration_seconds", 0.1, "model", "m", "exit_reason", "completed")
	}
	resolveDelegationCredentialsFn = func(context.Context, map[string]any, *AIAgent) (map[string]any, error) {
		return map[string]any{"provider": nil, "base_url": nil, "api_key": nil, "api_mode": nil, "command": nil, "args": nil, "model": "m"}, nil
	}
	t.Cleanup(func() {
		buildChildPreservingParentToolsFn, runSingleChildFn, resolveDelegationCredentialsFn = prevBuild, prevRun, prevCreds
	})
	tools.SetSessionVars(env, tools.SessionVars{Platform: "whatsapp", ChatID: "c1", SessionKey: "agent:main:whatsapp:dm:c1", SessionID: "sess"})
}

func inlineDelegate(t *testing.T, env *tools.Env, parent *AIAgent, kwargs map[string]any) *pyjson.Object {
	t.Helper()
	out, err := delegateTask(tools.WithToolEnv(context.Background(), env), parent, kwargs)
	return dispatchPayload(t, out, err)
}

func TestDispatchFollowsTheSchemaTheSessionCarries(t *testing.T) {
	env := inlineTestEnv(t)
	fastInlineChildren(t, env)
	var dispatched []tools.AsyncDispatch
	prev := dispatchAsyncDelegationBatchFn
	dispatchAsyncDelegationBatchFn = func(_ context.Context, _ *tools.Env, d tools.AsyncDispatch) (map[string]any, error) {
		dispatched = append(dispatched, d)
		return map[string]any{"status": "dispatched", "delegation_id": "d1"}, nil
	}
	t.Cleanup(func() { dispatchAsyncDelegationBatchFn = prev })
	joined := inlineDelegate(t, env, inlineParent(env, builtInlineAgent(t, env, true).tools, 0), map[string]any{"goal": "audit", "background": true})
	if entryText(firstResult(t, joined), "summary") != "done: audit" || entryHas(joined, "note") || len(dispatched) != 0 {
		t.Fatalf("joined %v dispatched %d", joined, len(dispatched))
	}
	detached := inlineDelegate(t, env, inlineParent(env, builtInlineAgent(t, env, false).tools, 0), map[string]any{"goal": "audit", "background": true})
	if entryText(detached, "status") != "dispatched" || !strings.Contains(entryText(detached, "note"), "END YOUR TURN") || len(dispatched) != 1 {
		t.Fatalf("detached %v dispatched %d", detached, len(dispatched))
	}
}

func TestNestedAndOneShotDelegationKeepTheirOwnPaths(t *testing.T) {
	env := inlineTestEnv(t)
	fastInlineChildren(t, env)
	writeDelegationBlock(t, env.Home, "  max_spawn_depth: 2\n")
	nested := inlineDelegate(t, env, inlineParent(env, builtInlineAgent(t, env, false).tools, 1), map[string]any{"goal": "audit", "background": false})
	if entryText(firstResult(t, nested), "status") != "completed" || entryHas(nested, "note") {
		t.Fatalf("nested %v", nested)
	}
	oneshot := inlineParent(env, builtInlineAgent(t, env, true).tools, 0)
	t.Setenv("ICEBATH_SINGLE_QUERY_SESSION", "1")
	if !tools.AgentIsOneshot(&oneshot.oneshotSession, env) {
		t.Fatal("one shot session not pinned")
	}
	os.Unsetenv("ICEBATH_SINGLE_QUERY_SESSION")
	result := inlineDelegate(t, env, oneshot, map[string]any{"goal": "audit", "background": true})
	if !strings.Contains(entryText(result, "note"), "SYNCHRONOUSLY") {
		t.Fatalf("one shot result %v", result)
	}
}

func TestLiveControlDegradesToPlainAnswersInAnInlineSession(t *testing.T) {
	env := inlineTestEnv(t)
	fastInlineChildren(t, env)
	parent := inlineParent(env, builtInlineAgent(t, env, true).tools, 0)
	listed := inlineDelegate(t, env, parent, map[string]any{"action": "list"})
	if listed == nil {
		t.Fatal("list gave no object")
	}
	stopped := inlineDelegate(t, env, parent, map[string]any{"action": "stop", "subagent_id": "nope"})
	if !entryHas(stopped, "error") {
		t.Fatalf("stop %v", stopped)
	}
}
