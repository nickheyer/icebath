package agent

import (
	"context"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

func reasoningChildOptions(t *testing.T, delegationBlock string) AIAgentOptions {
	t.Helper()
	parent := mockDelegateParent(t, 0)
	writeDelegationBlock(t, parent.env.Home, delegationBlock)
	parent.reasoningConfig = map[string]any{"enabled": true, "effort": "xhigh"}
	built := captureChildBuilds(t, parent.env)
	if _, err := buildChildAgent(tools.WithToolEnv(context.Background(), parent.env), childSpec(parent, "test", nil, 50, "leaf")); err != nil {
		t.Fatal(err)
	}
	return (*built)[0]
}

func TestDelegationReasoningEffortInheritsParentReasoningWhenNoOverride(t *testing.T) {
	o := reasoningChildOptions(t, "  max_iterations: 50\n  reasoning_effort: \"\"\n")
	if !reflect.DeepEqual(o.ReasoningConfig, map[string]any{"enabled": true, "effort": "xhigh"}) {
		t.Fatalf("reasoning_config %v", o.ReasoningConfig)
	}
}

func TestDelegationReasoningEffortOverrideReasoningEffortFromConfig(t *testing.T) {
	o := reasoningChildOptions(t, "  max_iterations: 50\n  reasoning_effort: low\n")
	if !reflect.DeepEqual(o.ReasoningConfig, map[string]any{"enabled": true, "effort": "low"}) {
		t.Fatalf("reasoning_config %v", o.ReasoningConfig)
	}
}

func TestDispatchDelegateTaskModelAcpArgsNotForwarded(t *testing.T) {
	parent := mockDelegateParent(t, 0)
	var captured map[string]any
	prev := delegateTaskFn
	delegateTaskFn = func(_ context.Context, _ *AIAgent, kwargs map[string]any) (string, error) {
		captured = kwargs
		return "{}", nil
	}
	t.Cleanup(func() { delegateTaskFn = prev })
	if _, err := parent.DispatchDelegateTask(context.Background(), map[string]any{
		"goal": "test", "acp_command": "claude", "acp_args": []any{"--acp", "--stdio"},
		"tasks": []any{map[string]any{"goal": "nested", "acp_command": "codex", "acp_args": []any{"--acp"}}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, has := captured["acp_command"]; has {
		t.Fatal("acp_command forwarded")
	}
	if _, has := captured["acp_args"]; has {
		t.Fatal("acp_args forwarded")
	}
	tasks, _ := captured["tasks"].([]any)
	if captured["goal"] != "test" || len(tasks) != 1 {
		t.Fatalf("captured %v", captured)
	}
	first, _ := tasks[0].(map[string]any)
	if _, has := first["acp_command"]; has {
		t.Fatalf("nested acp_command forwarded %v", first)
	}
	if _, has := first["acp_args"]; has {
		t.Fatalf("nested acp_args forwarded %v", first)
	}
}

func TestConcurrencyDefaultsLoadConfigPrefersActivePersistentConfigOverCliDefaults(t *testing.T) {
	env := delegationTestEnv(t, 0, false)
	writeDelegationBlock(t, env.Home, "  max_iterations: 50\n  max_concurrent_children: 50\n  max_spawn_depth: 10\n")
	ctx := tools.WithToolEnv(context.Background(), env)
	if got := tools.LoadDelegationConfig(ctx)["max_concurrent_children"]; got != 50 || tools.MaxConcurrentChildren(ctx) != 50 {
		t.Fatalf("config %v knob %d", got, tools.MaxConcurrentChildren(ctx))
	}
}

func TestConcurrencyDefaultsZeroClampedToOne(t *testing.T) {
	env := delegationTestEnv(t, 0, false)
	writeDelegationBlock(t, env.Home, "  max_concurrent_children: 0\n")
	if got := tools.MaxConcurrentChildren(tools.WithToolEnv(context.Background(), env)); got != 1 {
		t.Fatalf("max_concurrent_children %d", got)
	}
}

func TestAsyncCapUnifiedStaleMaxAsyncChildrenIgnored(t *testing.T) {
	env := delegationTestEnv(t, 0, false)
	writeDelegationBlock(t, env.Home, "  max_concurrent_children: 15\n  max_async_children: 3\n")
	if got := tools.MaxAsyncChildren(tools.WithToolEnv(context.Background(), env)); got != 15 {
		t.Fatalf("max_async_children %d", got)
	}
}

func TestMaxSpawnDepthMaxSpawnDepthClampedBelowOne(t *testing.T) {
	env := delegationTestEnv(t, 0, false)
	writeDelegationBlock(t, env.Home, "  max_spawn_depth: 0\n")
	if got := tools.MaxSpawnDepth(tools.WithToolEnv(context.Background(), env)); got != 1 {
		t.Fatalf("max_spawn_depth %d", got)
	}
}
