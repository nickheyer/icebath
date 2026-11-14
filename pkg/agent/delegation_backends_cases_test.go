package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Hermes probe plugin picking a route per option, refusing the reserved one
const dbProbePlugin = `
from agent.delegation_provider import DelegationProvider, DelegationRouteError

class Options(DelegationProvider):
    @property
    def name(self):
        return "probe-options"

    def task_parameters(self, config):
        opts = self.settings(config).get("options") or []
        return {"option": {"type": "string", "enum": [o["name"] for o in opts],
                           "description": "; ".join(f"{o['name']}: {o['description']}" for o in opts)}}

    def description(self, config):
        return "Pick an ` + "`option`" + ` per task."

    def route_task(self, task, config):
        if task.get("option") == "refused":
            raise DelegationRouteError("that option is reserved for humans")
        for o in self.settings(config).get("options") or []:
            if o["name"] == task.get("option"):
                return {"base_url": o["base_url"], "api_key": "k-" + o["name"], "model": o["model"],
                        "reasoning_effort": o.get("reasoning_effort"), "label": o["name"],
                        "fallback_providers": o.get("fallback_providers")}
        return dict(config)

def register(ctx):
    ctx.register_delegation_provider(Options())
`

// Options of the probe backend
var dbOptions = []any{
	map[string]any{"name": "fast", "description": "cheap and quick", "base_url": "http://127.0.0.1:9/v1", "model": "fast-model"},
	map[string]any{"name": "deep", "description": "slow and careful", "base_url": "http://127.0.0.1:9/v1", "model": "deep-model", "reasoning_effort": "high"},
	map[string]any{"name": "refused", "description": "never allowed", "base_url": "http://127.0.0.1:9/v1", "model": "x"},
}

// Plugin services carrying the delegation provider registry the plugin manager registers into
func dbServices(t *testing.T) {
	t.Helper()
	plugins.InstallServices(&plugins.Services{ProviderRegistries: map[string]plugins.ScopedRegistry{"delegation_provider": DelegationScopedRegistry()}})
	t.Cleanup(func() { plugins.InstallServices(nil) })
}

// Installs the real delegation registry hooks for one test and restores the suite doubles afterwards
func dbRealRegistry(t *testing.T) {
	t.Helper()
	prev, prevBackend := tools.DelegationRegistryHooks, tools.DelegationBackendHooks
	InstallDelegationRegistryHooks()
	tools.ResetDelegationFirstUse()
	dbServices(t)
	t.Cleanup(func() {
		tools.DelegationRegistryHooks, tools.DelegationBackendHooks = prev, prevBackend
		tools.ResetDelegationFirstUse()
	})
}

// Children built by delegate_task, each completing at once
type dbRecorded struct {
	mu    sync.Mutex
	built []childBuildSpec
}

// Snapshot of the recorded child build specs
func (r *dbRecorded) specs() []childBuildSpec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.built)
}

// Models of the recorded children in build order
func (r *dbRecorded) models() []any {
	var out []any
	for _, spec := range r.specs() {
		out = append(out, spec.Model)
	}
	return out
}

// Records what each child is built with, children complete immediately
func dbRecord(t *testing.T) *dbRecorded {
	t.Helper()
	rec := &dbRecorded{}
	prevBuild, prevRun := buildChildAgentFn, runSingleChildFn
	buildChildAgentFn = func(_ context.Context, spec childBuildSpec) (*AIAgent, error) {
		rec.mu.Lock()
		rec.built = append(rec.built, spec)
		rec.mu.Unlock()
		model, _ := spec.Model.(string)
		return &AIAgent{model: model, delegateRole: "leaf"}, nil
	}
	runSingleChildFn = func(_ context.Context, taskIndex int, _ string, _, _ *AIAgent, _ string, _, _ any) *pyjson.Object {
		return pyjson.ObjectOf("task_index", taskIndex, "status", "completed", "summary", "ok", "api_calls", 1, "duration_seconds", 0)
	}
	t.Cleanup(func() { buildChildAgentFn, runSingleChildFn = prevBuild, prevRun })
	return rec
}

// The hermes _parent fixture
func dbParent(h, sessionID string) *AIAgent {
	return &AIAgent{sessionID: sessionID, model: "parent-model", provider: "openrouter", requestOverrides: map[string]any{},
		activeChildrenLock: &sync.Mutex{}, quietMode: true, env: &tools.Env{Home: h, Values: &sync.Map{}}}
}

// delegate_task result parsed back from its JSON text
func dbDelegate(t *testing.T, ctx context.Context, parent *AIAgent, tasks ...map[string]any) map[string]any {
	t.Helper()
	list := make([]any, len(tasks))
	for i, task := range tasks {
		list[i] = task
	}
	raw, err := delegateTask(ctx, parent, map[string]any{"tasks": list})
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("result %q: %v", raw, err)
	}
	return out
}

// Result rows of a delegate_task answer
func dbResults(t *testing.T, out map[string]any) []map[string]any {
	t.Helper()
	rows, _ := out["results"].([]any)
	var results []map[string]any
	for _, row := range rows {
		m, ok := row.(map[string]any)
		if !ok {
			t.Fatalf("result row %v", row)
		}
		results = append(results, m)
	}
	return results
}

// The delegate_task function definition of a home
func dbDefinition(h string) map[string]any {
	return tools.BuildDelegateDynamicSchemaOverrides(&tools.Env{Home: h, Values: &sync.Map{}})
}

// Per task properties of a definition
func dbTaskProps(definition map[string]any) map[string]any {
	return definition["parameters"].(map[string]any)["properties"].(map[string]any)["tasks"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
}

// JSON text of a definition
func dbJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// Go provider standing in for the hermes test classes with one route rule
type dbProvider struct {
	name   string
	params map[string]map[string]any
	route  func(task, config map[string]any) (map[string]any, error)
}

func (p *dbProvider) Name() string                                            { return p.name }
func (p *dbProvider) DisplayName() string                                     { return p.name }
func (p *dbProvider) GetSetupSchema() map[string]any                          { return map[string]any{} }
func (p *dbProvider) Summary() string                                         { return "" }
func (p *dbProvider) IsAvailable() bool                                       { return true }
func (p *dbProvider) Description(map[string]any) string                       { return "" }
func (p *dbProvider) TaskParameters(map[string]any) map[string]map[string]any { return p.params }
func (p *dbProvider) RouteTask(task, config map[string]any) (map[string]any, error) {
	return p.route(task, config)
}
func (p *dbProvider) Settings(config map[string]any) map[string]any {
	backends, _ := config["backends"].(map[string]any)
	own, _ := backends[p.name].(map[string]any)
	if own == nil {
		return map[string]any{}
	}
	return own
}

// Registers a provider in the global layer the way the hermes tests call _registry.register
func dbRegisterGlobal(t *testing.T, p tools.DelegationProvider) {
	t.Helper()
	if err := delegationRegistry.Register(p, ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { delegationRegistry.RestoreRegistration(p.Name(), p, nil, "") })
}

func TestRouteTaskAlwaysSeesTheCoreTaskPosition(t *testing.T) {
	dbRealRegistry(t)
	var mu sync.Mutex
	var seen []any
	positional := &dbProvider{name: "positional", params: map[string]map[string]any{"index": {"type": "integer"}},
		route: func(task, _ map[string]any) (map[string]any, error) {
			mu.Lock()
			seen = append(seen, task["index"])
			mu.Unlock()
			return map[string]any{}, nil
		}}
	dbRegisterGlobal(t, positional)
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	ctx := home.WithOverride(context.Background(), h)
	params, _ := tools.SchemaExtension(ctx, map[string]any{"backend": "positional"})
	if len(params) != 0 {
		t.Fatalf("schema extension %v", params)
	}
	_, refusal, err := resolveTaskRoutes(ctx, []map[string]any{{"goal": "first", "index": 7}, {"goal": "second", "index": 0}},
		map[string]any{"backend": "positional"}, dbParent(h, "root"))
	if err != nil || refusal != "" || dbJSON(t, seen) != `[0,1]` {
		t.Fatalf("refusal %q err %v seen %v", refusal, err, seen)
	}
}

func TestRuntimeWithZeroPluginsLoadedStillDelegatesOnTheBundledDefault(t *testing.T) {
	dbRealRegistry(t)
	rec := dbRecord(t)
	root := t.TempDir()
	h := filepath.Join(root, ".icebath")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("delegation:\n  model: pinned-model\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	plugins.ResetPluginManagersForTests()
	t.Cleanup(plugins.ResetPluginManagersForTests)
	ctx := home.WithOverride(context.Background(), h)
	plugins.GetPluginManager(ctx).MarkDiscoveredForTests()
	previous := delegationRegistry.SnapshotRegistration("default", "")
	if previous != nil {
		delegationRegistry.RestoreRegistration("default", previous, nil, "")
	}
	t.Cleanup(func() {
		if current := delegationRegistry.SnapshotRegistration("default", ""); current != nil {
			delegationRegistry.RestoreRegistration("default", current, previous, "")
		}
	})
	dbDelegate(t, ctx, dbParent(h, "root"), map[string]any{"goal": "one plain delegated task"})
	if models := rec.models(); len(models) == 0 || models[len(models)-1] != "pinned-model" {
		t.Fatalf("models %v", models)
	}
	active, err := tools.GetActiveDelegationProvider(ctx, map[string]any{})
	if err != nil || active.Name() != "default" {
		t.Fatalf("active %v %v", active, err)
	}
	if tools.LoadBundledDelegation(ctx, "../../tools") != nil {
		t.Fatal("config text became a path")
	}
}

func TestAReplacementDefaultStrategyGetsTheFullRouteContract(t *testing.T) {
	replacement := &dbProvider{name: "default", params: map[string]map[string]any{},
		route: func(map[string]any, map[string]any) (map[string]any, error) {
			return map[string]any{"base_url": "https://chatgpt.com/backend-api/codex", "api_key": "k", "model": "gpt-6-astra",
				"service_tier": "fast", "label": "codex-fast"}, nil
		}}
	prev := activeDelegationProviderFn
	activeDelegationProviderFn = func(context.Context, map[string]any) (tools.DelegationProvider, error) { return replacement, nil }
	t.Cleanup(func() { activeDelegationProviderFn = prev })
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	ctx := home.WithOverride(context.Background(), h)
	routes, refusal, err := resolveTaskRoutes(ctx, []map[string]any{{"goal": "one plain delegated task"}}, map[string]any{}, routeFieldsParent(nil))
	if err != nil || refusal != "" || len(routes) != 1 {
		t.Fatalf("routes %v %q %v", routes, refusal, err)
	}
	route := routes[0]
	overrides, _ := route.Creds["request_overrides"].(map[string]any)
	if route.Label != "codex-fast" || route.Creds["service_tier"] != "priority" || overrides["service_tier"] != "priority" {
		t.Fatalf("route %+v", route)
	}
}
