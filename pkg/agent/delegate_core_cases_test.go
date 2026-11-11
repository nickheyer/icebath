package agent

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Parent of test_delegate.py _make_mock_parent at the given depth over a temp home
func mockDelegateParent(t *testing.T, depth int) *AIAgent {
	t.Helper()
	parent := batchValidationParent(t)
	parent.apiKey, parent.delegateDepth, parent.activeChildrenLock = "***", depth, &sync.Mutex{}
	return parent
}

// Replaces the child constructor with a canned child and records every construction option
func captureChildBuilds(t *testing.T, env *tools.Env, answers ...string) *[]AIAgentOptions {
	t.Helper()
	var built []AIAgentOptions
	prev := newChildAIAgentFn
	newChildAIAgentFn = func(_ context.Context, o AIAgentOptions) (*AIAgent, error) {
		built = append(built, o)
		child, _ := cannedDelegateChild(t, env, answers...)
		return child, nil
	}
	t.Cleanup(func() { newChildAIAgentFn = prev })
	return &built
}

func childSpec(parent *AIAgent, goal string, model any, maxIterations int, role string) childBuildSpec {
	return childBuildSpec{TaskIndex: 0, Goal: goal, Model: model, MaxIterations: maxIterations, TaskCount: 1, Parent: parent, Role: role}
}

func toolNamesOfDefinitions(defs []map[string]any) map[string]bool {
	names := map[string]bool{}
	for _, d := range defs {
		fn, _ := d["function"].(map[string]any)
		if n, _ := fn["name"].(string); n != "" {
			names[n] = true
		}
	}
	return names
}

func TestStripBlockedToolsRemovesBlockedToolsets(t *testing.T) {
	result := stripBlockedToolsets(toolsetTestContext(t), []string{"terminal", "file", "delegation", "clarify", "memory", "code_execution"})
	sort.Strings(result)
	if !reflect.DeepEqual(result, []string{"code_execution", "file", "terminal"}) {
		t.Fatalf("result %v", result)
	}
}

func TestStripBlockedToolsStripsCronjobToolset(t *testing.T) {
	result := stripBlockedToolsets(toolsetTestContext(t), []string{"terminal", "file", "cronjob", "web"})
	kept := map[string]bool{}
	for _, ts := range result {
		kept[ts] = true
	}
	if kept["cronjob"] || !kept["terminal"] || !kept["file"] || !kept["web"] {
		t.Fatalf("result %v", result)
	}
}

func TestStripBlockedToolsMixedCompositeIsSubtractedAtChildAssembly(t *testing.T) {
	parent := mockDelegateParent(t, 0)
	parent.enabledToolsets, parent.disabledToolsets = []string{"icebath-cli"}, []string{"browser"}
	built := captureChildBuilds(t, parent.env)
	ctx := tools.WithToolEnv(context.Background(), parent.env)
	if _, err := buildChildAgent(ctx, childSpec(parent, "Inspect safely", nil, 10, "leaf")); err != nil {
		t.Fatal(err)
	}
	disabled := map[string]bool{}
	for _, ts := range (*built)[0].DisabledToolsets {
		disabled[ts] = true
	}
	for _, want := range []string{"browser", "clarify", "cronjob", "delegation", "memory"} {
		if !disabled[want] {
			t.Fatalf("%q not denied in %v", want, (*built)[0].DisabledToolsets)
		}
	}
	if disabled["code_execution"] {
		t.Fatal("code_execution denied to the child")
	}
	names := toolNamesOfDefinitions(tools.Default.ToolDefinitions(ctx, parent.env, tools.DefinitionOptions{
		Enabled: (*built)[0].EnabledToolsets, Disabled: (*built)[0].DisabledToolsets, Quiet: true, SkipToolSearchAssembly: true}))
	if !names["terminal"] && !names["read_file"] && !names["web_search"] {
		t.Fatalf("no allowed tool survived %v", names)
	}
	for blocked := range delegateBlockedTools {
		if names[blocked] {
			t.Fatalf("blocked tool %q exposed", blocked)
		}
	}
}

func TestStripBlockedToolsOrchestratorCompositeRegainsOnlyDelegateTask(t *testing.T) {
	parent := mockDelegateParent(t, 0)
	writeDelegationBlock(t, parent.env.Home, "  orchestrator_enabled: true\n  max_spawn_depth: 2\n")
	parent.enabledToolsets, parent.disabledToolsets = []string{"icebath-cli"}, []string{"delegation", "browser"}
	built := captureChildBuilds(t, parent.env)
	ctx := tools.WithToolEnv(context.Background(), parent.env)
	if _, err := buildChildAgent(ctx, childSpec(parent, "Coordinate safely", nil, 10, "orchestrator")); err != nil {
		t.Fatal(err)
	}
	for _, ts := range (*built)[0].DisabledToolsets {
		if ts == "delegation" {
			t.Fatalf("delegation denied to an orchestrator %v", (*built)[0].DisabledToolsets)
		}
	}
	names := toolNamesOfDefinitions(tools.Default.ToolDefinitions(ctx, parent.env, tools.DefinitionOptions{
		Enabled: (*built)[0].EnabledToolsets, Disabled: (*built)[0].DisabledToolsets, Quiet: true, SkipToolSearchAssembly: true}))
	if !names["delegate_task"] {
		t.Fatalf("delegate_task missing from %v", names)
	}
	for blocked := range delegateBlockedTools {
		if blocked != "delegate_task" && names[blocked] {
			t.Fatalf("blocked tool %q exposed", blocked)
		}
	}
}

func TestDelegateTaskNoParentAgent(t *testing.T) {
	delegationTestEnv(t, 0, false)
	out, err := delegateTask(context.Background(), nil, map[string]any{"goal": "test"})
	if msg := entryText(dispatchPayload(t, out, err), "error"); !strings.Contains(msg, "parent agent") {
		t.Fatalf("error %q", msg)
	}
}

func TestDelegateTaskDepthLimit(t *testing.T) {
	parent := mockDelegateParent(t, 2)
	out, err := delegateTask(context.Background(), parent, map[string]any{"goal": "test"})
	if msg := entryText(dispatchPayload(t, out, err), "error"); !strings.Contains(strings.ToLower(msg), "depth limit") {
		t.Fatalf("error %q", msg)
	}
}

func TestDelegateTaskChildInheritsRuntimeCredentials(t *testing.T) {
	parent := mockDelegateParent(t, 0)
	parent.baseURL, parent.apiKey, parent.provider, parent.apiMode = "https://chatgpt.com/backend-api/codex", "***", "openai-codex", "codex_responses"
	built := captureChildBuilds(t, parent.env, "ok")
	if _, err := delegateTask(tools.WithToolEnv(context.Background(), parent.env), parent, map[string]any{"goal": "Test runtime inheritance"}); err != nil {
		t.Fatal(err)
	}
	if len(*built) != 1 {
		t.Fatalf("built %d children", len(*built))
	}
	o := (*built)[0]
	if o.BaseURL != parent.baseURL || o.APIKey != parent.apiKey || o.Provider != parent.provider || o.APIMode != parent.apiMode {
		t.Fatalf("options %+v", o)
	}
}

func TestDelegateTaskChildGetsDedicatedSessionDbNotParentsHandle(t *testing.T) {
	parent := mockDelegateParent(t, 0)
	ctx := tools.WithToolEnv(context.Background(), parent.env)
	parentDB, err := state.Acquire(ctx, filepath.Join(parent.env.Home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	parent.sessionDB = parentDB
	built := captureChildBuilds(t, parent.env)
	child, err := buildChildAgent(ctx, childSpec(parent, "test", "test-model", 5, "leaf"))
	if err != nil {
		t.Fatal(err)
	}
	childDB := (*built)[0].SessionDB
	if childDB == nil || !child.ownsSessionDB {
		t.Fatalf("child db %v owns %v", childDB, child.ownsSessionDB)
	}
	if !state.Release(parentDB) {
		t.Fatal("the parent handle is not registry managed")
	}
	if err := childDB.CreateSession(state.NewSession{ID: "child-session-81267", Source: "subagent", Model: "test-model"}); err != nil {
		t.Fatalf("child store unusable after the parent teardown %v", err)
	}
	closeChild(child, "")
}

func TestDelegateTaskChildWithoutParentDbStillDegradesToNone(t *testing.T) {
	parent := mockDelegateParent(t, 0)
	built := captureChildBuilds(t, parent.env)
	if _, err := buildChildAgent(tools.WithToolEnv(context.Background(), parent.env), childSpec(parent, "test", "test-model", 5, "leaf")); err != nil {
		t.Fatal(err)
	}
	if (*built)[0].SessionDB != nil {
		t.Fatalf("child db %v", (*built)[0].SessionDB)
	}
}

func TestDelegateTaskChildDedicatedDbFollowsParentsDbPath(t *testing.T) {
	parent := mockDelegateParent(t, 0)
	ctx := tools.WithToolEnv(context.Background(), parent.env)
	profilePath := filepath.Join(t.TempDir(), "profile-work", "state.db")
	parentDB, err := state.Acquire(ctx, profilePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.ReleaseOrClose(parentDB) })
	parent.sessionDB = parentDB
	built := captureChildBuilds(t, parent.env)
	child, err := buildChildAgent(ctx, childSpec(parent, "test", "test-model", 5, "leaf"))
	if err != nil {
		t.Fatal(err)
	}
	childDB := sessionStorePath((*built)[0].SessionDB)
	if childDB == "" || childDB != parentDB.Path() || filepath.Base(filepath.Dir(childDB)) != "profile-work" {
		t.Fatalf("child db %v", childDB)
	}
	closeChild(child, "")
}

func TestDelegateTaskNousChildRederivesApiModeFromModel(t *testing.T) {
	parent := mockDelegateParent(t, 0)
	if err := writeFileInHome(parent.env.Home, "config.yaml", "nous:\n  anthropic_wire: native\n"); err != nil {
		t.Fatal(err)
	}
	parent.baseURL, parent.apiKey, parent.provider = "https://inference-api.nousresearch.com/v1", "portal-jwt", "nous"
	parent.apiMode, parent.model = "anthropic_messages", "anthropic/claude-opus-4.8"
	built := captureChildBuilds(t, parent.env)
	ctx := tools.WithToolEnv(context.Background(), parent.env)
	if _, err := buildChildAgent(ctx, childSpec(parent, "Stay on chat completions", "hermes-4-405b", 10, "leaf")); err != nil {
		t.Fatal(err)
	}
	first := (*built)[0]
	if first.Provider != "nous" || first.Model != "hermes-4-405b" || first.APIMode != "chat_completions" {
		t.Fatalf("first child %+v", first)
	}
	parent.apiMode, parent.model = "chat_completions", "hermes-4-405b"
	if _, err := buildChildAgent(ctx, childSpec(parent, "Move onto Messages", "anthropic/claude-opus-4.8", 10, "leaf")); err != nil {
		t.Fatal(err)
	}
	if second := (*built)[1]; second.APIMode != "anthropic_messages" {
		t.Fatalf("second child %+v", second)
	}
}

func TestToolNamePreservationGlobalToolNamesRestoredAfterDelegation(t *testing.T) {
	parent := mockDelegateParent(t, 0)
	original := []string{"terminal", "read_file", "web_search", "execute_code", "delegate_task"}
	tools.RestoreLastResolvedToolNames(append([]string{}, original...))
	captureChildBuilds(t, parent.env, "done")
	if _, err := delegateTask(tools.WithToolEnv(context.Background(), parent.env), parent, map[string]any{"goal": "Test tool preservation"}); err != nil {
		t.Fatal(err)
	}
	if got := tools.LastResolvedToolNames(); !reflect.DeepEqual(got, original) {
		t.Fatalf("resolved tool names %v", got)
	}
}

func writeFileInHome(h, name, body string) error {
	return os.WriteFile(filepath.Join(h, name), []byte(body), 0o600)
}
