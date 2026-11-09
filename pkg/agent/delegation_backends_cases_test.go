package agent

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/builtin/delegationdefault"
	"github.com/nickheyer/icebath/pkg/config"
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

// Temp home with the probe plugin on disk, a config selecting plugins and delegation, discovered by the real manager
func dbHome(t *testing.T, root string, delegation map[string]any, pluginNames, disabled []string) (context.Context, string, *plugins.Manager) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatalf("python3 is required to run the plugin host child: %v", err)
	}
	t.Cleanup(plugins.SwapHostInterpreter(func(context.Context) (string, error) { return python, nil }))
	config.RegisterConfigFacade(config.FacadeHooks{CoreDeclaredEnvNames: config.StaticCoreEnvNames})
	h := filepath.Join(root, ".icebath")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(pluginNames, "probe-options") {
		dir := filepath.Join(h, "plugins", "probe-options")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte("name: probe-options\nversion: '1.0'\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "__init__.py"), []byte(dbProbePlugin), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	enabled := []any{}
	for _, name := range pluginNames {
		enabled = append(enabled, name)
	}
	off := []any{}
	for _, name := range disabled {
		off = append(off, name)
	}
	cfg := map[string]any{"plugins": map[string]any{"isolation": "host", "enabled": enabled, "disabled": off}, "delegation": delegation}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", filepath.Join(root, "os-home"))
	t.Setenv("ICEBATH_HOME", h)
	dbServices(t)
	plugins.ResetPluginManagersForTests()
	t.Cleanup(plugins.ResetPluginManagersForTests)
	ctx := home.WithOverride(context.Background(), h)
	m := plugins.GetPluginManager(ctx)
	m.DiscoverAndLoad(ctx, false)
	return ctx, h, m
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

func TestSelectedPluginBackendExtendsSchemaAndRoutesEachTask(t *testing.T) {
	ctx, h, m := dbHome(t, t.TempDir(), map[string]any{"backend": "probe-options", "backends": map[string]any{"probe-options": map[string]any{"options": dbOptions}}}, []string{"probe-options"}, nil)
	defer m.Unload(nil)
	dbRealRegistry(t)
	rec := dbRecord(t)
	active, err := tools.GetActiveDelegationProvider(ctx, tools.LoadDelegationConfig(ctx))
	if err != nil || active.Name() != "probe-options" {
		t.Fatalf("active %v %v", active, err)
	}
	definition := dbDefinition(h)
	option := dbTaskProps(definition)["option"].(map[string]any)
	if dbJSON(t, option["enum"]) != `["fast","deep","refused"]` || !strings.Contains(option["description"].(string), "deep: slow and careful") {
		t.Fatalf("option %v", option)
	}
	if !strings.HasSuffix(definition["description"].(string), "Pick an `option` per task.") {
		t.Fatalf("description %q", definition["description"])
	}
	if dbJSON(t, dbDefinition(h)) != dbJSON(t, definition) {
		t.Fatal("the definition is not byte stable for the session")
	}
	out := dbDelegate(t, ctx, dbParent(h, "root"),
		map[string]any{"goal": "summarize the quick thing", "option": "fast"},
		map[string]any{"goal": "think hard about the slow thing", "option": "deep"},
		map[string]any{"goal": "no option: plain delegation config"})
	for _, row := range dbResults(t, out) {
		if row["status"] != "completed" {
			t.Fatalf("results %v", out)
		}
	}
	built := rec.specs()
	if len(built) != 3 {
		t.Fatalf("built %d children", len(built))
	}
	fast, deep, plain := built[0], built[1], built[2]
	if fast.Model != "fast-model" || fast.OverrideBaseURL != "http://127.0.0.1:9/v1" || fast.OverrideAPIKey != "k-fast" {
		t.Fatalf("fast %+v", fast)
	}
	if deep.Model != "deep-model" || deep.OverrideReasoningEffort != "high" {
		t.Fatalf("deep %+v", deep)
	}
	if plain.Model != nil || plain.OverrideProvider != nil || plain.ProfileHome != "" {
		t.Fatalf("plain %+v", plain)
	}
}

func TestAnUnroutableTaskIsRefusedAloneAndItsSiblingsRun(t *testing.T) {
	ctx, h, m := dbHome(t, t.TempDir(), map[string]any{"backend": "probe-options", "backends": map[string]any{"probe-options": map[string]any{"options": dbOptions}}}, []string{"probe-options"}, nil)
	defer m.Unload(nil)
	dbRealRegistry(t)
	rec := dbRecord(t)
	out := dbDelegate(t, ctx, dbParent(h, "root"),
		map[string]any{"goal": "a task with a typo in it", "option": "fsat"},
		map[string]any{"goal": "summarize the quick thing", "option": "fast"},
		map[string]any{"goal": "a task the backend refuses", "option": "refused"})
	results := dbResults(t, out)
	if len(results) != 3 {
		t.Fatalf("results %v", out)
	}
	typo, fast, refused := results[0], results[1], results[2]
	for i, row := range results {
		if row["task_index"] != float64(i) {
			t.Fatalf("task_index order %v", results)
		}
	}
	if typo["status"] != "error" || !strings.Contains(typo["error"].(string), "must be one of fast, deep, refused") {
		t.Fatalf("typo %v", typo)
	}
	if refused["status"] != "error" || !strings.Contains(refused["error"].(string), "reserved for humans") {
		t.Fatalf("refused %v", refused)
	}
	if dbJSON(t, typo["route"]) != `{"backend":"probe-options","refused":true}` {
		t.Fatalf("route %v", typo["route"])
	}
	if fast["status"] != "completed" || dbJSON(t, rec.models()) != `["fast-model"]` {
		t.Fatalf("fast %v models %v", fast, rec.models())
	}
	bad := dbDelegate(t, ctx, dbParent(h, "root"), map[string]any{"goal": "only a refused task", "option": "refused"})
	if errText, _ := bad["error"].(string); !strings.Contains(errText, "reserved for humans") || len(rec.specs()) != 1 {
		t.Fatalf("bad %v built %d", bad, len(rec.specs()))
	}
}

func TestAMissingBackendFailsClosedAndDefaultKeepsTodaysRoute(t *testing.T) {
	pinned := map[string]any{"model": "pinned-model", "base_url": "http://127.0.0.1:9/v1", "api_key": "k"}
	dbRealRegistry(t)
	rec := dbRecord(t)
	var definitions []string
	var outcomes []map[string]any
	for i, delegation := range []map[string]any{{}, {"backend": "default"}, {"backend": "not-installed"}} {
		cfg := map[string]any{}
		for k, v := range pinned {
			cfg[k] = v
		}
		for k, v := range delegation {
			cfg[k] = v
		}
		ctx, h, m := dbHome(t, filepath.Join(t.TempDir(), string(rune('0'+i))), cfg, nil, nil)
		definitions = append(definitions, dbJSON(t, dbDefinition(h)))
		outcomes = append(outcomes, dbDelegate(t, ctx, dbParent(h, "root"), map[string]any{"goal": "one plain delegated task"}))
		m.Unload(nil)
	}
	if definitions[0] != definitions[1] || definitions[1] != definitions[2] || strings.Contains(definitions[0], `"option"`) {
		t.Fatalf("definitions %v", definitions)
	}
	built := rec.specs()
	if len(built) != 2 {
		t.Fatalf("built %d", len(built))
	}
	for _, spec := range built {
		if spec.Model != "pinned-model" || spec.OverrideBaseURL != "http://127.0.0.1:9/v1" {
			t.Fatalf("spec %+v", spec)
		}
	}
	errText, _ := outcomes[2]["error"].(string)
	if !strings.Contains(errText, "'not-installed' is not installed") || !strings.Contains(errText, "icebath config set delegation.backend default") {
		t.Fatalf("error %q", errText)
	}
}

func TestABackendRouteWithoutItsOwnChainInheritsDelegationFallbackProviders(t *testing.T) {
	chain := []any{map[string]any{"provider": "openrouter", "model": "backup-model"}}
	deep := map[string]any{}
	for k, v := range dbOptions[1].(map[string]any) {
		deep[k] = v
	}
	deep["fallback_providers"] = []any{}
	options := []any{dbOptions[0], deep}
	ctx, h, m := dbHome(t, t.TempDir(), map[string]any{"backend": "probe-options", "fallback_providers": chain,
		"backends": map[string]any{"probe-options": map[string]any{"options": options}}}, []string{"probe-options"}, nil)
	defer m.Unload(nil)
	dbRealRegistry(t)
	routes, refusal, err := resolveTaskRoutes(ctx, []map[string]any{{"goal": "inherits", "option": "fast"}, {"goal": "own empty chain", "option": "deep"}},
		tools.LoadDelegationConfig(ctx), dbParent(h, "root"))
	if err != nil || refusal != "" || len(routes) != 2 {
		t.Fatalf("routes %v %q %v", routes, refusal, err)
	}
	inherited := resolveChildFallbackChain(dbParent(h, "root"), routes[0].RoutingCfg, true)
	if len(inherited) != 1 || inherited[0]["provider"] != "openrouter" || inherited[0]["model"] != "backup-model" {
		t.Fatalf("inherited %v", inherited)
	}
	if disabled := resolveChildFallbackChain(dbParent(h, "root"), routes[1].RoutingCfg, true); disabled != nil {
		t.Fatalf("disabled %v", disabled)
	}
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

func TestASavedSwitchRefusesThePreviousBackendsParameterAndNamesTheFix(t *testing.T) {
	ctx, h, m := dbHome(t, t.TempDir(), map[string]any{"backend": "probe-options", "backends": map[string]any{"probe-options": map[string]any{"options": dbOptions}}}, []string{"probe-options"}, nil)
	defer m.Unload(nil)
	dbRealRegistry(t)
	rec := dbRecord(t)
	profiles := &dbProvider{name: "probe-profiles", params: map[string]map[string]any{"target": {"type": "string", "enum": []any{"work"}}},
		route: func(task, _ map[string]any) (map[string]any, error) {
			target, _ := task["target"].(string)
			if target == "" {
				target = "profile-default"
			}
			return map[string]any{"model": target + "-model"}, nil
		}}
	dbRegisterGlobal(t, profiles)
	var names []string
	for _, b := range tools.ListDelegationBackends(ctx) {
		names = append(names, b.Name)
	}
	if dbJSON(t, names) != `["default","probe-options","probe-profiles"]` {
		t.Fatalf("backends %v", names)
	}
	before, err := os.ReadFile(home.ConfigPath(h))
	if err != nil {
		t.Fatal(err)
	}
	if err := tools.SetGlobalBackend(ctx, "nope", "", false); err == nil || !strings.Contains(err.Error(), "'nope' is not installed") {
		t.Fatalf("nope %v", err)
	}
	if err := tools.SetGlobalBackend(ctx, "probe-profiles", "", true); err == nil || !strings.Contains(err.Error(), "apply_now needs") {
		t.Fatalf("apply now %v", err)
	}
	if after, _ := os.ReadFile(home.ConfigPath(h)); string(after) != string(before) {
		t.Fatal("a refused switch wrote config.yaml")
	}
	if err := tools.SetGlobalBackend(ctx, "probe-profiles", "chat-a", false); err != nil {
		t.Fatal(err)
	}
	saved, err := config.ReadUserConfigRaw(ctx, home.ConfigPath(h))
	if err != nil || saved["delegation"].(map[string]any)["backend"] != "probe-profiles" {
		t.Fatalf("saved %v %v", saved, err)
	}
	out := dbDelegate(t, ctx, dbParent(h, "chat-a"),
		map[string]any{"goal": "the quick thing", "option": "fast"},
		map[string]any{"goal": "a plain task"},
		map[string]any{"goal": "a work task", "target": "work"},
		map[string]any{"goal": "a tier task", "tier": "a"})
	results := dbResults(t, out)
	if len(results) != 4 {
		t.Fatalf("results %v", out)
	}
	refused, plain, work, unknown := results[0], results[1], results[2], results[3]
	if refused["status"] != "error" || dbJSON(t, refused["route"]) != `{"backend":"probe-profiles","refused":true}` {
		t.Fatalf("refused %v", refused)
	}
	wantRefused := "Task 0: the delegation backend is 'probe-profiles', which does not take 'option' (declared by " +
		"'probe-options'), so this task was not started. This session's delegate_task was built for another " +
		"backend: run `/delegation probe-profiles --now` to rebuild this session's delegate_task for " +
		"'probe-profiles', or start a new session with /new."
	if refused["error"] != wantRefused {
		t.Fatalf("refused error %q", refused["error"])
	}
	wantUnknown := "Task 3: the delegation backend is 'probe-profiles', which does not take 'tier', and no installed " +
		"delegation backend does, so this task was not started. Resend it without 'tier';"
	if errText, _ := unknown["error"].(string); !strings.HasPrefix(errText, wantUnknown) {
		t.Fatalf("unknown error %q", errText)
	}
	if plain["status"] != "completed" || work["status"] != "completed" {
		t.Fatalf("plain %v work %v", plain, work)
	}
	if dbJSON(t, rec.models()) != `["profile-default-model","work-model"]` {
		t.Fatalf("models %v", rec.models())
	}
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

func TestBackendKeyReadsThroughTheRealLoader(t *testing.T) {
	dbRealRegistry(t)
	ctx, h, m := dbHome(t, filepath.Join(t.TempDir(), "unset"), map[string]any{}, nil, nil)
	cfg, err := config.LoadReadonly(ctx, h, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg["delegation"].(map[string]any)["backend"] != "default" || tools.ConfiguredBackend(tools.LoadDelegationConfig(ctx)) != "default" {
		t.Fatalf("unset %v", cfg["delegation"])
	}
	m.Unload(nil)
	ctx, _, m = dbHome(t, filepath.Join(t.TempDir(), "set"), map[string]any{"backend": " Probe-Options "}, []string{"probe-options"}, nil)
	defer m.Unload(nil)
	if got := tools.ConfiguredBackend(tools.LoadDelegationConfig(ctx)); got != "probe-options" {
		t.Fatalf("set %q", got)
	}
}

func TestDefaultStrategyIsTheBundledPluginThroughTheSameSeam(t *testing.T) {
	dbRealRegistry(t)
	rec := dbRecord(t)
	ctx, h, m := dbHome(t, filepath.Join(t.TempDir(), "on"), map[string]any{"model": "pinned-model"}, nil, nil)
	enabled := false
	for _, row := range m.ListPlugins() {
		if row["key"] == "delegation/default" {
			enabled, _ = row["enabled"].(bool)
		}
	}
	if !enabled {
		t.Fatalf("delegation/default not enabled in %v", m.ListPlugins())
	}
	active, err := tools.GetActiveDelegationProvider(ctx, map[string]any{})
	if err != nil || active.Name() != "default" {
		t.Fatalf("active %v %v", active, err)
	}
	if _, ok := active.(*delegationdefault.DefaultDelegationProvider); !ok {
		t.Fatalf("active provider is %T", active)
	}
	dbDelegate(t, ctx, dbParent(h, "root"), map[string]any{"goal": "one plain delegated task"})
	if models := rec.models(); len(models) == 0 || models[len(models)-1] != "pinned-model" {
		t.Fatalf("models %v", models)
	}
	m.Unload(nil)
	ctx, h, m = dbHome(t, filepath.Join(t.TempDir(), "off"), map[string]any{}, nil, []string{"delegation/default"})
	defer m.Unload(nil)
	out := dbDelegate(t, ctx, dbParent(h, "root"), map[string]any{"goal": "one plain delegated task"})
	if errText, _ := out["error"].(string); !strings.Contains(errText, "delegation/default is disabled") {
		t.Fatalf("off %v", out)
	}
	if _, has := dbTaskProps(dbDefinition(h))["option"]; has {
		t.Fatal("option advertised without a strategy")
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
