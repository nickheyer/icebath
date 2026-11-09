package agent

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

// test_review_engine.py test_delegate_task_credentials_cfg_overrides_delegation_config
func TestDelegateTaskCredentialsCfgOverridesDelegationConfig(t *testing.T) {
	env := asyncDispatchFixture(t)
	var seen map[string]any
	prevCreds, prevBuild, prevRun := resolveDelegationCredentialsFn, buildChildPreservingParentToolsFn, runSingleChildFn
	resolveDelegationCredentialsFn = func(_ context.Context, cfg map[string]any, _ *AIAgent) (map[string]any, error) {
		seen = cfg
		return map[string]any{"model": cfg["model"], "provider": nil, "base_url": nil, "api_key": nil, "api_mode": nil, "command": nil, "args": nil}, nil
	}
	buildChildPreservingParentToolsFn = func(context.Context, childBuildSpec) (*AIAgent, error) { return &AIAgent{delegateRole: "leaf"}, nil }
	runSingleChildFn = func(_ context.Context, i int, _ string, _, _ *AIAgent, _ string, _, _ any) *pyjson.Object {
		return pyjson.ObjectOf("task_index", i, "status", "completed", "summary", "ok", "api_calls", 1, "duration_seconds", 0.1, "model", "m", "exit_reason", "completed")
	}
	t.Cleanup(func() {
		resolveDelegationCredentialsFn, buildChildPreservingParentToolsFn, runSingleChildFn = prevCreds, prevBuild, prevRun
	})
	override := map[string]any{"provider": "openrouter", "model": "review-model-x"}
	parsed := asyncDelegate(t, env, asyncTestParent(env, "review-parent-sess"), map[string]any{"goal": "review this", "background": true, "credentials_cfg": override})
	if entryText(parsed, "status") != "dispatched" || !reflect.DeepEqual(seen, override) {
		t.Fatalf("handle %v seen %v", parsed, seen)
	}
	if evt := drainOneCompletion(5 * time.Second); evt == nil {
		t.Fatal("the background unit never completed")
	}
}

// test_review_engine.py test_child_system_prompt_embeds_workspace_context
func TestChildSystemPromptEmbedsWorkspaceContext(t *testing.T) {
	env := delegationTestEnv(t, 0, false)
	workspace := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "AGENTS.md"), []byte("# Project Rules\nAll fixes must cover sibling call sites.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := tools.WithToolEnv(context.Background(), env)
	prompt := buildChildSystemPrompt(ctx, env, "do the thing", workspace, "leaf", tools.MaxSpawnDepth(ctx), 1)
	if !strings.Contains(prompt, "sibling call sites") {
		t.Fatalf("prompt %q", prompt)
	}
}

// test_delegate_child_cache_ttl.py test_real_spawn_path_applies_it
func TestRealSpawnPathAppliesIt(t *testing.T) {
	parent := newFixtureAgentWithConfig(t, "prompt_caching:\n  cache_ttl: 1h\nmodel:\n  default: anthropic/claude-sonnet-4.6\n", AIAgentOptions{
		SessionID: "p", APIKey: "k", BaseURL: "https://openrouter.ai/api/v1", Provider: "openrouter", APIMode: "chat_completions",
		Model: "anthropic/claude-sonnet-4.6", Platform: "cli", EnabledToolsets: []string{"file"},
	})
	if parent.cacheTTL != "1h" {
		t.Fatalf("fixture: the operator's 1h must actually be in effect, got %q", parent.cacheTTL)
	}
	child, err := buildChildAgent(tools.WithToolEnv(context.Background(), parent.env), childBuildSpec{
		TaskIndex: 0, Goal: "goal", Context: nil, Model: nil, MaxIterations: 4, TaskCount: 1, Parent: parent, Role: "leaf",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		closeChild(child, "")
		_ = parent.Close()
	}()
	if child.cacheTTL != "5m" || parent.cacheTTL != "1h" {
		t.Fatalf("child %q parent %q", child.cacheTTL, parent.cacheTTL)
	}
}
