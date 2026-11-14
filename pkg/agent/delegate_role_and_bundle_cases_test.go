package agent

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Child constructor double recording options and the built children, each answering the canned text
func captureChildAgents(t *testing.T, env *tools.Env, answer string) (*[]AIAgentOptions, *[]*AIAgent) {
	t.Helper()
	var built []AIAgentOptions
	var children []*AIAgent
	prev := newChildAIAgentFn
	newChildAIAgentFn = func(_ context.Context, o AIAgentOptions) (*AIAgent, error) {
		built = append(built, o)
		child, _ := cannedDelegateChild(t, env, answer)
		children = append(children, child)
		return child, nil
	}
	t.Cleanup(func() { newChildAIAgentFn = prev })
	return &built, &children
}

// Credentials resolving to None on every field, the mock_creds of the role tests
func noneCredentials(t *testing.T) {
	t.Helper()
	prev := resolveDelegationCredentialsFn
	resolveDelegationCredentialsFn = func(context.Context, map[string]any, *AIAgent) (map[string]any, error) {
		return map[string]any{"provider": nil, "base_url": nil, "api_key": nil, "api_mode": nil, "model": nil}, nil
	}
	t.Cleanup(func() { resolveDelegationCredentialsFn = prev })
}

func roleRun(t *testing.T, parent *AIAgent, kwargs map[string]any) (*AIAgent, AIAgentOptions) {
	t.Helper()
	built, children := captureChildAgents(t, parent.env, "done")
	if _, err := delegateTask(tools.WithToolEnv(context.Background(), parent.env), parent, kwargs); err != nil {
		t.Fatal(err)
	}
	if len(*children) != 1 {
		t.Fatalf("built %d children", len(*children))
	}
	return (*children)[0], (*built)[0]
}

func TestOrchestratorRoleSchemaRoleIsDepthDerivedNotCallerDeclared(t *testing.T) {
	parent := mockDelegateParent(t, 0)
	writeDelegationBlock(t, parent.env.Home, "  max_spawn_depth: 2\n")
	noneCredentials(t)
	if child, _ := roleRun(t, parent, map[string]any{"goal": "test"}); child.delegateRole != "orchestrator" {
		t.Fatalf("role %q", child.delegateRole)
	}
	if child, _ := roleRun(t, parent, map[string]any{"goal": "test", "role": "leaf"}); child.delegateRole != "orchestrator" {
		t.Fatalf("legacy leaf role overrode the depth derivation %q", child.delegateRole)
	}
}

func TestOrchestratorRoleSchemaSchemaOmitsAcpTransportFields(t *testing.T) {
	props := tools.DelegateTaskSchema["parameters"].(map[string]any)["properties"].(map[string]any)
	taskProps := props["tasks"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
	for _, key := range []string{"acp_command", "acp_args"} {
		if _, top := props[key]; top {
			t.Fatalf("%s advertised at the top level", key)
		}
		if _, task := taskProps[key]; task {
			t.Fatalf("%s advertised per task", key)
		}
	}
}

func hasToolset(list []string, name string) bool {
	for _, ts := range list {
		if ts == name {
			return true
		}
	}
	return false
}

func TestOrchestratorRoleBehaviorOrchestratorRoleKeepsDelegationAtDepth1(t *testing.T) {
	parent := mockDelegateParent(t, 0)
	writeDelegationBlock(t, parent.env.Home, "  max_spawn_depth: 2\n")
	noneCredentials(t)
	parent.enabledToolsets = []string{"terminal", "file"}
	child, o := roleRun(t, parent, map[string]any{"goal": "test", "role": "orchestrator"})
	if !hasToolset(o.EnabledToolsets, "delegation") || child.delegateRole != "orchestrator" {
		t.Fatalf("enabled %v role %q", o.EnabledToolsets, child.delegateRole)
	}
}

func TestOrchestratorRoleBehaviorOrchestratorBlockedAtMaxSpawnDepth(t *testing.T) {
	parent := mockDelegateParent(t, 1)
	writeDelegationBlock(t, parent.env.Home, "  max_spawn_depth: 2\n")
	noneCredentials(t)
	parent.enabledToolsets = []string{"terminal", "delegation"}
	child, o := roleRun(t, parent, map[string]any{"goal": "test", "role": "orchestrator"})
	if hasToolset(o.EnabledToolsets, "delegation") || child.delegateRole != "leaf" {
		t.Fatalf("enabled %v role %q", o.EnabledToolsets, child.delegateRole)
	}
}

type builtRoleAgent struct {
	enabledToolsets      []string
	isOrchestratorPrompt bool
}

func TestOrchestratorEndToEndEndToEndNestedOrchestration(t *testing.T) {
	parent := mockDelegateParent(t, 0)
	writeDelegationBlock(t, parent.env.Home, "  max_spawn_depth: 2\n")
	noneCredentials(t)
	parent.enabledToolsets = []string{"terminal", "file", "delegation"}
	env := parent.env
	ctx := tools.WithToolEnv(context.Background(), env)
	var builtAgents []builtRoleAgent
	prevNew := newChildAIAgentFn
	newChildAIAgentFn = func(_ context.Context, o AIAgentOptions) (*AIAgent, error) {
		isOrchestrator := strings.Contains(o.EphemeralSystemPrompt, "Orchestrator Role")
		builtAgents = append(builtAgents, builtRoleAgent{append([]string{}, o.EnabledToolsets...), isOrchestrator})
		if !isOrchestrator {
			child, _ := cannedDelegateChild(t, env, "done")
			return child, nil
		}
		child := &AIAgent{model: "test", env: env, platform: "cli", apiKey: "***", quietMode: true, suppressStatusOutput: true,
			enabledToolsets: []string{"terminal", "file", "delegation"}, activeChildrenLock: &sync.Mutex{}}
		prevRun := conversationLoopRun
		conversationLoopRun = func(c context.Context, a *AIAgent, userMessage any, o RunConversationOptions, apiMode string) (map[string]any, error) {
			if a != child {
				return prevRun(c, a, userMessage, o, apiMode)
			}
			if _, err := delegateTask(ctx, child, map[string]any{"tasks": []any{
				map[string]any{"goal": "Do leaf work stream A"}, map[string]any{"goal": "Do leaf work stream B"},
			}}); err != nil {
				return nil, err
			}
			return map[string]any{"final_response": "orchestrated 2 workers", "completed": true, "api_calls": 1, "messages": []any{}}, nil
		}
		t.Cleanup(func() { conversationLoopRun = prevRun })
		return child, nil
	}
	t.Cleanup(func() { newChildAIAgentFn = prevNew })
	if _, err := delegateTask(ctx, parent, map[string]any{"goal": "top-level orchestration", "role": "orchestrator"}); err != nil {
		t.Fatal(err)
	}
	if len(builtAgents) != 3 {
		t.Fatalf("built %d agents %v", len(builtAgents), builtAgents)
	}
	if !hasToolset(builtAgents[0].enabledToolsets, "delegation") || !builtAgents[0].isOrchestratorPrompt {
		t.Fatalf("orchestrator %v", builtAgents[0])
	}
	for _, leaf := range builtAgents[1:] {
		if hasToolset(leaf.enabledToolsets, "delegation") || leaf.isOrchestratorPrompt {
			t.Fatalf("leaf %v", leaf)
		}
	}
}

// test_delegate.py TestSubagentApprovalCallback test_executor_initializer_installs_callback_in_worker
func TestSubagentApprovalCallbackExecutorInitializerInstallsCallbackInWorker(t *testing.T) {
	env := delegationTestEnv(t, 0, false)
	child := livenessChild(env, "approval-child", 0, 0)
	childEnv := *env
	child.env = &childEnv
	var seen tools.Asker
	scriptChildConversation(t, child, func(context.Context) (map[string]any, error) {
		seen = child.env.Ask
		return map[string]any{"final_response": "done", "completed": true, "api_calls": 1}, nil
	})
	parent := &AIAgent{env: env}
	run := &childRun{ctx: tools.WithToolEnv(context.Background(), env), child: child, parent: parent, taskIndex: 0, goal: "approval scope", heartbeat: newChildHeartbeat(child, parent, 0)}
	run.seedWorkspace()
	if _, failure, _ := run.awaitChild(); failure != nil {
		t.Fatalf("failure %v", failure)
	}
	asker, ok := seen.(subagentApprovalAsker)
	if !ok || asker.cb("rm -rf /tmp/x", "dangerous", nil) != "deny" {
		t.Fatalf("worker asker %#v", seen)
	}
	if child.env.Ask != nil || env.Ask != nil {
		t.Fatal("the worker callback leaked past the worker")
	}
}

func fallbackChildOptions(t *testing.T, parent *AIAgent, spec childBuildSpec) (AIAgentOptions, error) {
	t.Helper()
	built := captureChildBuilds(t, parent.env)
	if _, err := buildChildAgent(tools.WithToolEnv(context.Background(), parent.env), spec); err != nil {
		return AIAgentOptions{}, err
	}
	return (*built)[0], nil
}

func TestFallbackModelInheritanceChildInheritsFallbackChain(t *testing.T) {
	parent := mockDelegateParent(t, 0)
	entry := map[string]any{"provider": "openrouter", "model": "gpt-4o-mini", "api_key": "sk-or-x"}
	parent.fallbackChain = []map[string]any{entry}
	o, err := fallbackChildOptions(t, parent, childSpec(parent, "test fallback inheritance", nil, 10, "leaf"))
	if err != nil || !reflect.DeepEqual(o.FallbackModel, []map[string]any{entry}) {
		t.Fatalf("fallback %v %v", o.FallbackModel, err)
	}
}

func TestFallbackModelInheritanceChildGetsNoFallbackWhenParentChainEmpty(t *testing.T) {
	parent := mockDelegateParent(t, 0)
	parent.fallbackChain = []map[string]any{}
	o, err := fallbackChildOptions(t, parent, childSpec(parent, "test no fallback", nil, 10, "leaf"))
	if err != nil || len(o.FallbackModel) != 0 {
		t.Fatalf("fallback %v %v", o.FallbackModel, err)
	}
}

func TestFallbackModelInheritancePinnedProviderDisablesParentFallbackChain(t *testing.T) {
	parent := mockDelegateParent(t, 0)
	parent.fallbackChain = []map[string]any{{"provider": "openrouter", "model": "gpt-4o-mini", "api_key": "sk-or-x"}}
	spec := childSpec(parent, "test pinned provider", "minimax/m2", 10, "leaf")
	spec.OverrideProvider, spec.OverrideBaseURL, spec.OverrideAPIKey = "minimax", "https://api.minimax.example/v1", "sk-mm-x"
	o, err := fallbackChildOptions(t, parent, spec)
	if err != nil || len(o.FallbackModel) != 0 {
		t.Fatalf("fallback %v %v", o.FallbackModel, err)
	}
}

func TestFallbackModelInheritancePinnedAcpCommandMissingRaises(t *testing.T) {
	parent := mockDelegateParent(t, 0)
	spec := childSpec(parent, "test pinned acp command", nil, 10, "leaf")
	spec.OverrideACPCommand = "definitely-not-a-real-binary"
	_, err := fallbackChildOptions(t, parent, spec)
	if err == nil || !strings.Contains(err.Error(), "definitely-not-a-real-binary") || !strings.Contains(strings.ToLower(err.Error()), "not") {
		t.Fatalf("err %v", err)
	}
}

func TestFallbackModelInheritanceResolveCredentialsRejectsMissingPinnedCommand(t *testing.T) {
	scriptRuntimeProviderCall(t, &provider.Runtime{APIKey: "sk-x", BaseURL: "https://api.example/v1", APIMode: "chat_completions", Provider: "acp-provider", Command: "missing-acp-binary"}, nil)
	_, err := resolveCreds(t, mockDelegateParent(t, 0), map[string]any{"provider": "acp-provider", "model": "some-model"})
	var refusal *tools.DelegationValueError
	if !errors.As(err, &refusal) || !strings.Contains(err.Error(), "missing-acp-binary") {
		t.Fatalf("err %v", err)
	}
}

// Live client double exposing the fallback endpoint the parent actually calls
type liveClientStub struct{ baseURL, apiKey string }

func (c liveClientStub) BaseURL() string { return c.baseURL }
func (c liveClientStub) APIKey() string  { return c.apiKey }

func TestAtomicChildCredentialBundleProviderOverrideNeverBorrowsParentBaseUrl(t *testing.T) {
	parent := mockDelegateParent(t, 0)
	spec := childSpec(parent, "bundle", nil, 10, "leaf")
	spec.OverrideProvider, spec.OverrideAPIKey = "copilot", "gh-x"
	o, err := fallbackChildOptions(t, parent, spec)
	if err != nil || o.Provider != "copilot" || o.BaseURL != "" || o.BaseURL == parent.baseURL {
		t.Fatalf("options %+v %v", o, err)
	}
}

func TestAtomicChildCredentialBundleNoOverrideInheritsLiveEndpointAndKeyTogether(t *testing.T) {
	parent := mockDelegateParent(t, 0)
	parent.baseURL, parent.apiKey = "https://fallback.example/v1", "FAKE-KEY-STALE-PRIMARY"
	parent.clientKwargs = map[string]any{"api_key": "FAKE-KEY-FALLBACK", "base_url": "https://fallback.example/v1/"}
	parent.client = liveClientStub{baseURL: "https://fallback.example/v1/", apiKey: "FAKE-KEY-FALLBACK"}
	o, err := fallbackChildOptions(t, parent, childSpec(parent, "bundle", nil, 10, "leaf"))
	if err != nil || o.Provider != parent.provider || o.BaseURL != "https://fallback.example/v1" || o.APIKey != "FAKE-KEY-FALLBACK" {
		t.Fatalf("options %+v %v", o, err)
	}
}

func TestAtomicChildCredentialBundleProviderWithoutBaseUrlIsRefused(t *testing.T) {
	scriptRuntimeProviderCall(t, &provider.Runtime{Provider: "copilot", BaseURL: "", APIKey: "gh-x"}, nil)
	_, err := resolveCreds(t, mockDelegateParent(t, 0), map[string]any{"provider": "copilot", "model": "gpt-5"})
	var refusal *tools.DelegationValueError
	if !errors.As(err, &refusal) {
		t.Fatalf("err %v", err)
	}
}
