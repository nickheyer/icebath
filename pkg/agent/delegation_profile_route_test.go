package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Home, PROBE_SECRET and model.default as a task bound to ctx sees them, the hermes _snapshot
func profileSnapshot(ctx context.Context) map[string]any {
	var secret any
	if v, ok, _ := provider.GetSecret(ctx, "PROBE_SECRET"); ok {
		secret = v
	}
	model := ""
	if tree, err := config.LoadReadonly(ctx, home.Dir(ctx), nil); err == nil {
		if m, ok := tree["model"].(map[string]any); ok {
			model, _ = m["default"].(string)
		}
	}
	return map[string]any{"home": realDir(home.Dir(ctx)), "secret": secret, "model": model}
}

func realDir(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

func wantSnapshot(h string, secret any, model string) map[string]any {
	return map[string]any{"home": realDir(h), "secret": secret, "model": model}
}

// Merges sections into a profile's config.yaml the way the hermes _set_config does
func setProfileConfig(t *testing.T, h string, sections map[string]any) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(h, "config.yaml")
	cfg, err := config.ReadUserConfigRaw(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range sections {
		cfg[k] = v
	}
	if err := config.AtomicConfigReplace(ctx, h, path, cfg, ""); err != nil {
		t.Fatal(err)
	}
}

func routeProfileParent(launch string) *AIAgent {
	parent := routeFieldsParent(nil)
	parent.credentialPool = provider.NewPool(provider.HomePoolEnv(launch), "openrouter", nil)
	return parent
}

func coderRoute(t *testing.T, parent *AIAgent, route map[string]any, cache map[string]*tools.TaskRoute) *tools.TaskRoute {
	t.Helper()
	if cache == nil {
		cache = map[string]*tools.TaskRoute{}
	}
	r, err := routeToTaskRoute(context.Background(), route, parent, cache, map[string]any{}, tools.DefaultDelegationBackend)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Builds a child through the route with a fake constructor, returning the child and the construction options
func buildRoutedChild(t *testing.T, parent *AIAgent, route *tools.TaskRoute, construct func(ctx context.Context, o AIAgentOptions) *AIAgent) (*AIAgent, AIAgentOptions) {
	t.Helper()
	var seen AIAgentOptions
	saved := newChildAIAgentFn
	newChildAIAgentFn = func(ctx context.Context, o AIAgentOptions) (*AIAgent, error) {
		seen = o
		if construct != nil {
			return construct(ctx, o), nil
		}
		return &AIAgent{sessionID: "child-1"}, nil
	}
	t.Cleanup(func() { newChildAIAgentFn = saved })
	spec := routeOverrides(route)
	spec.TaskIndex, spec.Goal, spec.Model, spec.MaxIterations, spec.TaskCount, spec.Parent = 0, "work as coder", route.Creds["model"], 5, 1, parent
	child, err := buildChildAgent(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	return child, seen
}

func TestChildIsBuiltAndConversesInsideTheTargetProfileScope(t *testing.T) {
	launch, coder := twoRouteProfiles(t)
	if err := os.WriteFile(filepath.Join(coder, "SOUL.md"), []byte("I am the coder profile."), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	parent := routeProfileParent(launch)
	route := coderRoute(t, parent, map[string]any{"profile": "coder"}, nil)
	seen := map[string]map[string]any{}
	child, _ := buildRoutedChild(t, parent, route, func(ctx context.Context, o AIAgentOptions) *AIAgent {
		built := profileSnapshot(ctx)
		built["model"], built["base_url"], built["api_key"], built["soul"] = o.Model, o.BaseURL, o.APIKey, o.LoadSoulIdentity
		seen["built"] = built
		return &AIAgent{sessionID: "child-1"}
	})
	want := wantSnapshot(coder, "coder-secret", "coder-model")
	want["base_url"], want["api_key"], want["soul"] = "https://coder.invalid/v1", "coder-key", true
	if !reflect.DeepEqual(seen["built"], want) {
		t.Fatalf("built %v", seen["built"])
	}
	if child.credentialPool == parent.credentialPool {
		t.Fatal("child shares the launch profile's pool")
	}
	if profileSnapshot(ctx)["secret"] != "launch-secret" {
		t.Fatal("launch scope changed after the build")
	}
	saved := runChildConversationFn
	runChildConversationFn = func(ctx context.Context, child *AIAgent, _ any, _ RunConversationOptions) (map[string]any, error) {
		ran := profileSnapshot(ctx)
		soul, _ := LoadSoulMD(ctx, &tools.Env{Home: home.Dir(ctx)}, nil, "")
		ran["soul"] = soul
		seen["ran"] = ran
		return map[string]any{"final_response": "ok", "completed": true}, nil
	}
	t.Cleanup(func() { runChildConversationFn = saved })
	convChild := &AIAgent{provider: "custom", model: "coder-model", delegateProfileHome: coder}
	convChild.env = tools.ToolEnvFrom(childProfileScope(ctx, convChild))
	runParent := &AIAgent{env: &tools.Env{Home: launch}}
	run := &childRun{ctx: ctx, child: convChild, parent: runParent, taskIndex: 0, goal: "work as coder", heartbeat: newChildHeartbeat(convChild, runParent, 0)}
	run.seedWorkspace()
	result, failure, _ := run.awaitChild()
	if failure != nil || result["final_response"] != "ok" {
		t.Fatalf("result %v failure %v", result, failure)
	}
	ran := seen["ran"]
	soul, _ := ran["soul"].(string)
	if ran["home"] != realDir(coder) || ran["secret"] != "coder-secret" || ran["model"] != "coder-model" || !strings.Contains(soul, "coder profile") {
		t.Fatalf("ran %v", ran)
	}
	if !reflect.DeepEqual(profileSnapshot(ctx), wantSnapshot(launch, "launch-secret", "launch-model")) {
		t.Fatalf("launch snapshot %v", profileSnapshot(ctx))
	}
}

func TestSecretMissingFromTargetProfileNeverFallsBackToTheLaunchEnv(t *testing.T) {
	_, coder := twoRouteProfiles(t)
	if err := os.WriteFile(filepath.Join(coder, ".env"), []byte("PROFILE_KEY=coder-key\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	scoped := delegatedProfileScope(ctx, coder)
	if v, ok, err := provider.GetSecret(scoped, "PROBE_SECRET"); ok || err != nil {
		t.Fatalf("scoped secret %q %v %v", v, ok, err)
	}
	if v, _, _ := provider.GetSecret(ctx, "PROBE_SECRET"); v != "launch-secret" {
		t.Fatalf("launch secret %q", v)
	}
	raw, _ := json.Marshal(profileSnapshot(ctx))
	var back map[string]any
	if err := json.Unmarshal(raw, &back); err != nil || back["model"] != "launch-model" {
		t.Fatalf("snapshot %s %v", raw, err)
	}
}

func TestProfileWhoseDefaultIsAnAliasTakesTheAliasRoute(t *testing.T) {
	launch, coder := twoRouteProfiles(t)
	setProfileConfig(t, coder, map[string]any{
		"model":         map[string]any{"default": "localqwen"},
		"model_aliases": map[string]any{"localqwen": map[string]any{"model": "qwen3-27b", "base_url": "http://127.0.0.1:8099/v1"}},
	})
	route := coderRoute(t, routeProfileParent(launch), map[string]any{"profile": "coder"}, nil)
	if route.Creds["model"] != "qwen3-27b" || route.Creds["base_url"] != "http://127.0.0.1:8099/v1" {
		t.Fatalf("creds %v", route.Creds)
	}
}

func TestNamedCustomProfileChildLeasesItsOwnAccountOnASharedUrl(t *testing.T) {
	launch, coder := twoRouteProfiles(t)
	url := "https://gateway.invalid/v1"
	setProfileConfig(t, coder, map[string]any{
		"model":     map[string]any{"provider": "open-ai", "default": "coder-model"},
		"providers": map[string]any{"claude-ai": map[string]any{"api": url, "api_key": "key-a"}, "open-ai": map[string]any{"api": url, "api_key": "key-b"}},
	})
	entry := func(cid, key string) map[string]any {
		return map[string]any{"id": cid, "label": cid, "auth_type": "api_key", "priority": 0, "source": "manual", "access_token": key, "base_url": url}
	}
	auth, _ := json.Marshal(map[string]any{"version": 1, "credential_pool": map[string]any{
		"claude-ai": []any{entry("a", "pool-key-a")}, "open-ai": []any{entry("b", "pool-key-b")}}})
	if err := os.WriteFile(filepath.Join(coder, "auth.json"), auth, 0o600); err != nil {
		t.Fatal(err)
	}
	parent := routeProfileParent(launch)
	child, _ := buildRoutedChild(t, parent, coderRoute(t, parent, map[string]any{"profile": "coder"}, nil), nil)
	if child.credentialPool == nil {
		t.Fatal("child has no pool")
	}
	var tokens []string
	for _, e := range child.credentialPool.Entries() {
		tokens = append(tokens, e.AccessToken)
	}
	if !reflect.DeepEqual(tokens, []string{"pool-key-b"}) {
		t.Fatalf("tokens %v", tokens)
	}
}

func TestProfileChildReasoningIsTheTargetProfilesResolvedSetting(t *testing.T) {
	cases := []struct {
		name     string
		agentCfg map[string]any
		expected map[string]any
	}{
		{"override_off", map[string]any{"reasoning_effort": "high", "reasoning_overrides": map[string]any{"coder-model": false}}, map[string]any{"enabled": false}},
		{"unset", map[string]any{}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			launch, coder := twoRouteProfiles(t)
			setProfileConfig(t, coder, map[string]any{"agent": c.agentCfg})
			setProfileConfig(t, launch, map[string]any{"delegation": map[string]any{"reasoning_effort": "xhigh"}})
			parent := routeProfileParent(launch)
			parent.reasoningConfig = map[string]any{"enabled": true, "effort": "xhigh"}
			_, built := buildRoutedChild(t, parent, coderRoute(t, parent, map[string]any{"profile": "coder"}, nil), nil)
			if (c.expected == nil && built.ReasoningConfig != nil) || (c.expected != nil && !reflect.DeepEqual(built.ReasoningConfig, c.expected)) {
				t.Fatalf("reasoning %v want %v", built.ReasoningConfig, c.expected)
			}
		})
	}
}

func TestProfileChildLeaseAndSchemaRetryRunInsideTheTargetProfile(t *testing.T) {
	launch, coder := twoRouteProfiles(t)
	ctx := context.Background()
	seen := map[string]map[string]any{}
	pool := provider.NewPool(provider.HomePoolEnv(coder), "custom", []provider.PooledCredential{{ID: "e1", BaseURL: "https://coder.invalid/v1"}})
	child := &AIAgent{baseURL: "https://coder.invalid/v1", sessionID: "child-1", delegateProfileHome: coder, credentialPool: pool,
		delegateOutputSchema: map[string]any{"type": "object", "required": []any{"ok"}}}
	savedSwap, savedRun := swapChildCredentialFn, runChildConversationFn
	swapChildCredentialFn = func(ctx context.Context, _ *AIAgent, _ provider.PooledCredential) (bool, error) {
		seen["lease"] = profileSnapshot(ctx)
		return true, nil
	}
	runChildConversationFn = func(ctx context.Context, _ *AIAgent, _ any, _ RunConversationOptions) (map[string]any, error) {
		seen["retry"] = profileSnapshot(ctx)
		return map[string]any{"final_response": `{"ok": true}`, "api_calls": 1}, nil
	}
	t.Cleanup(func() { swapChildCredentialFn, runChildConversationFn = savedSwap, savedRun })
	leaseChildCredential(ctx, child)
	outcome := validateChildOutputSchema(ctx, child, map[string]any{"final_response": "not json"}, 0, "t-1", nil)
	if outcome.valid == nil || !*outcome.valid || outcome.retries != 1 {
		t.Fatalf("outcome %+v", outcome)
	}
	if seen["lease"] == nil || seen["lease"]["home"] != realDir(coder) || seen["lease"]["model"] != "coder-model" {
		t.Fatalf("lease %v", seen["lease"])
	}
	if !reflect.DeepEqual(seen["retry"], wantSnapshot(coder, "coder-secret", "coder-model")) {
		t.Fatalf("retry %v", seen["retry"])
	}
	if !reflect.DeepEqual(profileSnapshot(ctx), wantSnapshot(launch, "launch-secret", "launch-model")) {
		t.Fatalf("launch snapshot %v", profileSnapshot(ctx))
	}
}

func TestProfileScopeThatFailsToEnterLeavesTheParentInItsOwnProfile(t *testing.T) {
	for _, failing := range []string{"agent.secret_scope.set_secret_scope", "tools.terminal_scope.install_profile_terminal_scope"} {
		t.Run(failing, func(t *testing.T) {
			launch, coder := twoRouteProfiles(t)
			ctx := context.Background()
			before := []any{profileSnapshot(ctx), provider.CurrentSecretScopeHome(ctx), provider.IsMultiplexActive(ctx)}
			scoped := delegatedProfileScope(ctx, coder)
			if realDir(home.Dir(scoped)) != realDir(coder) {
				t.Fatalf("scope home %s", home.Dir(scoped))
			}
			after := []any{profileSnapshot(ctx), provider.CurrentSecretScopeHome(ctx), provider.IsMultiplexActive(ctx)}
			if !reflect.DeepEqual(before, after) || before[0].(map[string]any)["home"] != realDir(launch) {
				t.Fatalf("parent context changed: %v -> %v", before, after)
			}
		})
	}
}

// Backend routing every task to the coder profile
type coderProfileProvider struct {
	tools.DelegationProviderDefaults
}

func newCoderProfileProvider() *coderProfileProvider {
	p := &coderProfileProvider{}
	p.Self = p
	return p
}

func (p *coderProfileProvider) Name() string                   { return "profile_routes" }
func (p *coderProfileProvider) DisplayName() string            { return "profile_routes" }
func (p *coderProfileProvider) GetSetupSchema() map[string]any { return map[string]any{} }
func (p *coderProfileProvider) RouteTask(map[string]any, map[string]any) (map[string]any, error) {
	return map[string]any{"profile": "coder"}, nil
}

func resolveCoderTask(t *testing.T, parent *AIAgent) ([]*tools.TaskRoute, string) {
	t.Helper()
	backend := newCoderProfileProvider()
	prev := tools.DelegationRegistryHooks.GetProvider
	tools.DelegationRegistryHooks.GetProvider = func(ctx context.Context, name string) tools.DelegationProvider {
		if name == backend.Name() {
			return backend
		}
		return prev(ctx, name)
	}
	t.Cleanup(func() { tools.DelegationRegistryHooks.GetProvider = prev })
	routes, msg, err := resolveTaskRoutes(context.Background(), []map[string]any{{"goal": "work as coder"}}, map[string]any{"backend": backend.Name()}, parent)
	if err != nil {
		t.Fatal(err)
	}
	return routes, msg
}

func TestProfileRouteIsRefusedUnderAManagedWorkerConfigSnapshot(t *testing.T) {
	launch, _ := twoRouteProfiles(t)
	frozen, err := config.ReadUserConfigRaw(context.Background(), filepath.Join(launch, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	config.ResetWorkerPolicyForTests()
	t.Cleanup(config.ResetWorkerPolicyForTests)
	if _, err := config.BindWorkerConfig(frozen); err != nil {
		t.Fatal(err)
	}
	parent := routeProfileParent(launch)
	routes, msg := resolveCoderTask(t, parent)
	if msg != "" || len(routes) != 1 || !strings.Contains(routes[0].Error, "managed worker") {
		t.Fatalf("routes %v msg %q", routes, msg)
	}
	_, err = routeToTaskRoute(context.Background(), map[string]any{"profile": "coder"}, parent, map[string]*tools.TaskRoute{}, map[string]any{}, tools.DefaultDelegationBackend)
	var routeErr *tools.DelegationRouteError
	if !errors.As(err, &routeErr) {
		t.Fatalf("err %v", err)
	}
}

func TestOneProfileUnderTwoLabelsResolvesOnceAndKeepsEachLabel(t *testing.T) {
	launch, _ := twoRouteProfiles(t)
	calls := 0
	saved := resolveRuntimeProviderFn
	resolveRuntimeProviderFn = func(ctx context.Context, h, requested, targetModel, explicitBaseURL, explicitAPIKey string) (*provider.Runtime, error) {
		calls++
		return saved(ctx, h, requested, targetModel, explicitBaseURL, explicitAPIKey)
	}
	t.Cleanup(func() { resolveRuntimeProviderFn = saved })
	cache, parent := map[string]*tools.TaskRoute{}, routeProfileParent(launch)
	first := coderRoute(t, parent, map[string]any{"profile": "coder", "label": "reviewer"}, cache)
	second := coderRoute(t, parent, map[string]any{"profile": "coder", "label": "fixer"}, cache)
	if calls != 1 {
		t.Fatalf("resolved %d times", calls)
	}
	if first.Label != "reviewer" || second.Label != "fixer" || !reflect.DeepEqual(first.Creds, second.Creds) || first.Creds["model"] != "coder-model" {
		t.Fatalf("first %v second %v", first, second)
	}
}

func TestProfileChildReadsTheStartingDirectorySeededForIt(t *testing.T) {
	cases := []struct {
		name     string
		worktree map[string]string
	}{{"no_worktree", nil}, {"worktree", map[string]string{"path": "/work/wt-1", "branch": "subagent/wt-1"}}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			launch, coder := twoRouteProfiles(t)
			ctx := context.Background()
			saved := createIsolatedWorktreeFn
			createIsolatedWorktreeFn = func(context.Context, *AIAgent, string, string) map[string]string { return c.worktree }
			t.Cleanup(func() { createIsolatedWorktreeFn = saved })
			parentEnv := &tools.Env{Home: launch}
			child := &AIAgent{delegateProfileHome: coder}
			child.env = tools.ToolEnvFrom(childProfileScope(ctx, child))
			parent := &AIAgent{currentTaskID: "parent-task", env: parentEnv}
			tools.RecordSessionCwd(parentEnv, "parent-task", "/work/repo")
			t.Cleanup(func() { tools.ClearSessionCwd(parentEnv, "parent-task") })
			run := &childRun{ctx: ctx, child: child, parent: parent, taskIndex: 0, goal: "work as coder"}
			run.seedWorkspace()
			want := "/work/repo"
			if c.worktree != nil {
				want = c.worktree["path"]
			}
			got, ok := tools.GetSessionCwd(tools.ToolEnvFrom(childProfileScope(ctx, child)), run.childTaskID)
			if !ok || got != want {
				t.Fatalf("child cwd %q %v want %q", got, ok, want)
			}
		})
	}
}

func dockerPersistent() map[string]any {
	return map[string]any{"terminal": map[string]any{"backend": "docker", "container_persistent": true}}
}

func TestProfileChildUsesTheTargetProfilesPersistentContainer(t *testing.T) {
	launch, coder := twoRouteProfiles(t)
	setProfileConfig(t, coder, dockerPersistent())
	ctx := context.Background()
	env := &tools.Env{Home: launch, Values: &sync.Map{}}
	tools.SetSessionVars(env, tools.SessionVars{SessionKey: "agent:main:telegram:dm:1", Profile: "default"})
	t.Cleanup(func() { tools.ClearSessionVars(env) })
	scopedCtx := delegatedProfileScope(ctx, coder)
	scoped := tools.EnvForProfileHome(scopedCtx, env, coder)
	if key, err := tools.ResolveContainerTaskID(scoped, "child-task"); err != nil || key != "profile:coder" {
		t.Fatalf("container key %q %v", key, err)
	}
	if got := tools.GetSessionEnv(scoped, "ICEBATH_SESSION_PROFILE", ""); got != "default" {
		t.Fatalf("session profile %q", got)
	}
}

func TestChildRoutedBackToTheLaunchProfileUsesTheLaunchProfilesContainer(t *testing.T) {
	launch, coder := twoRouteProfiles(t)
	for _, h := range []string{launch, coder} {
		setProfileConfig(t, h, dockerPersistent())
	}
	ctx := context.Background()
	env := &tools.Env{Home: launch, Values: &sync.Map{}}
	tools.SetSessionVars(env, tools.SessionVars{SessionKey: "agent:coder:telegram:dm:1", Profile: "coder"})
	t.Cleanup(func() { tools.ClearSessionVars(env) })
	coderCtx := delegatedProfileScope(ctx, coder)
	coderEnv := tools.EnvForProfileHome(coderCtx, env, coder)
	if key, err := tools.ResolveContainerTaskID(coderEnv, "parent-task"); err != nil || key != "profile:coder" {
		t.Fatalf("coder key %q %v", key, err)
	}
	launchCtx := delegatedProfileScope(coderCtx, launch)
	if key, err := tools.ResolveContainerTaskID(tools.EnvForProfileHome(launchCtx, coderEnv, launch), "child-task"); err != nil || key != "default" {
		t.Fatalf("launch key %q %v", key, err)
	}
	if key, err := tools.ResolveContainerTaskID(coderEnv, "parent-task"); err != nil || key != "profile:coder" {
		t.Fatalf("coder key after %q %v", key, err)
	}
}

func TestProfilePinnedToAMissingAcpCommandIsRefusedAtRouting(t *testing.T) {
	launch, coder := twoRouteProfiles(t)
	provider.RegisterProvider(provider.Profile{Name: "acme-acp", DisplayName: "acme-acp", AuthType: "external_process", BaseURL: "acp://acme",
		ProcessCommand: "acme", ProcessArgs: []string{"--acp", "--stdio"}, ProcessCommandEnvVars: []string{"ICEBATH_ACME_ACP_COMMAND"},
		Overlay: &provider.IcebathOverlay{Transport: "codex_responses", AuthType: "external_process", BaseURLOverride: "acp://acme", BaseURLEnvVar: "ACME_ACP_BASE_URL"}})
	t.Cleanup(func() { provider.UnregisterProvider("acme-acp") })
	setProfileConfig(t, coder, map[string]any{"model": map[string]any{"provider": "acme-acp", "default": "coder-model"}})
	t.Setenv("ICEBATH_ACME_ACP_COMMAND", "icebath-test-missing-acp-binary")
	t.Setenv("ACME_ACP_BASE_URL", "acp+tcp://127.0.0.1:9")
	routes, msg := resolveCoderTask(t, routeProfileParent(launch))
	if msg != "" || len(routes) != 1 || len(routes[0].Creds) != 0 {
		t.Fatalf("routes %v msg %q", routes, msg)
	}
	if !strings.Contains(routes[0].Error, "icebath-test-missing-acp-binary") || !strings.Contains(routes[0].Error, "coder") {
		t.Fatalf("route error %q", routes[0].Error)
	}
}

var _ = pyjson.NewObject
