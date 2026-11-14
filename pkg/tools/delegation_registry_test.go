package tools

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
)

// Registry double standing in for the provider lane ProviderRegistry and the bundled delegation plugins
type delegationRegistryDouble struct {
	mu          sync.Mutex
	registered  map[string]DelegationProvider
	bundled     map[string]func() DelegationProvider
	discoveries int
}

var testDelegationRegistry = &delegationRegistryDouble{
	registered: map[string]DelegationProvider{},
	bundled:    map[string]func() DelegationProvider{"default": func() DelegationProvider { return newTestDefaultDelegationProvider() }},
}

// Bundled default strategy double with the delegation section minus service_tier as its route
type testDefaultDelegationProvider struct{ DelegationProviderDefaults }

func newTestDefaultDelegationProvider() *testDefaultDelegationProvider {
	p := &testDefaultDelegationProvider{}
	p.Self = p
	return p
}

func (*testDefaultDelegationProvider) Name() string                   { return "default" }
func (*testDefaultDelegationProvider) DisplayName() string            { return "Default" }
func (*testDefaultDelegationProvider) GetSetupSchema() map[string]any { return map[string]any{} }
func (*testDefaultDelegationProvider) RouteTask(_, config map[string]any) (map[string]any, error) {
	route := map[string]any{}
	for k, v := range config {
		if k != "service_tier" {
			route[k] = v
		}
	}
	return route, nil
}

// Installs the registry double the pkg/tools test binary resolves delegation providers through
func installDelegationRegistryDoubles() {
	r := testDelegationRegistry
	DelegationRegistryHooks.GetProvider = func(_ context.Context, name string) DelegationProvider {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.registered[name]
	}
	DelegationRegistryHooks.DisabledPlugins = func(h string) map[string]bool {
		out := map[string]bool{}
		tree, err := config.LoadReadonly(home.WithOverride(context.Background(), h), h, nil)
		if err != nil {
			return out
		}
		plugins, _ := tree["plugins"].(map[string]any)
		disabled, _ := plugins["disabled"].([]any)
		for _, name := range disabled {
			if s, ok := name.(string); ok {
				out[s] = true
			}
		}
		return out
	}
	DelegationRegistryHooks.LoadBundled = func(_ context.Context, name string, register func(DelegationProvider)) bool {
		build, ok := r.bundled[name]
		if ok {
			register(build())
		}
		return ok
	}
	DelegationRegistryHooks.DiscoverPlugins = func(context.Context, bool) {
		r.mu.Lock()
		r.discoveries++
		r.mu.Unlock()
	}
}

// Registers a provider in the double for one test and resets the first use cache around it
func registerTestDelegationProvider(t *testing.T, p DelegationProvider) {
	t.Helper()
	r := testDelegationRegistry
	r.mu.Lock()
	previous, had := r.registered[p.Name()]
	r.registered[p.Name()] = p
	r.mu.Unlock()
	resetDelegationFirstUse(t)
	t.Cleanup(func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if had {
			r.registered[p.Name()] = previous
		} else {
			delete(r.registered, p.Name())
		}
	})
}

func resetDelegationFirstUse(t *testing.T) {
	t.Helper()
	delegationFirstUseMu.Lock()
	saved := delegationFirstUse
	delegationFirstUse = map[string]DelegationProvider{}
	delegationFirstUseMu.Unlock()
	t.Cleanup(func() {
		delegationFirstUseMu.Lock()
		delegationFirstUse = saved
		delegationFirstUseMu.Unlock()
	})
}

func delegationHome(t *testing.T, cfg string) context.Context {
	t.Helper()
	h := t.TempDir()
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return home.WithOverride(context.Background(), h)
}

// Options provider of test_delegation_backends.py offering its configured options as an enum parameter
type optionsDelegationProvider struct{ DelegationProviderDefaults }

func newOptionsDelegationProvider() *optionsDelegationProvider {
	p := &optionsDelegationProvider{}
	p.Self = p
	return p
}

func (*optionsDelegationProvider) Name() string                   { return "probe-options" }
func (*optionsDelegationProvider) DisplayName() string            { return "probe-options" }
func (*optionsDelegationProvider) GetSetupSchema() map[string]any { return map[string]any{} }
func (p *optionsDelegationProvider) options(config map[string]any) []any {
	opts, _ := p.Settings(config)["options"].([]any)
	return opts
}
func (p *optionsDelegationProvider) TaskParameters(config map[string]any) map[string]map[string]any {
	names, descs := []any{}, []string{}
	for _, o := range p.options(config) {
		m := o.(map[string]any)
		names = append(names, m["name"])
		descs = append(descs, m["name"].(string)+": "+m["description"].(string))
	}
	return map[string]map[string]any{"option": {"type": "string", "enum": names, "description": strings.Join(descs, "; ")}}
}
func (*optionsDelegationProvider) Description(map[string]any) string {
	return "Pick an `option` per task."
}
func (p *optionsDelegationProvider) RouteTask(task, config map[string]any) (map[string]any, error) {
	if task["option"] == "refused" {
		return nil, &DelegationRouteError{Message: "that option is reserved for humans"}
	}
	return config, nil
}

var probeOptions = []any{
	map[string]any{"name": "fast", "description": "cheap and quick", "base_url": "http://127.0.0.1:9/v1", "model": "fast-model"},
	map[string]any{"name": "deep", "description": "slow and careful", "base_url": "http://127.0.0.1:9/v1", "model": "deep-model", "reasoning_effort": "high"},
}

// Provider parameters never redefine core or route fields
func TestPluginParametersNeverRedefineCoreOrRouteFields(t *testing.T) {
	greedy := &greedyDelegationProvider{}
	greedy.Self = greedy
	registerTestDelegationProvider(t, greedy)
	params, _ := SchemaExtension(context.Background(), map[string]any{"backend": "greedy"})
	if len(params) != 1 || params["tier"] == nil {
		t.Fatalf("params %v", params)
	}
}

type greedyDelegationProvider struct{ DelegationProviderDefaults }

func (*greedyDelegationProvider) Name() string                   { return "greedy" }
func (*greedyDelegationProvider) DisplayName() string            { return "greedy" }
func (*greedyDelegationProvider) GetSetupSchema() map[string]any { return map[string]any{} }
func (*greedyDelegationProvider) TaskParameters(map[string]any) map[string]map[string]any {
	return map[string]map[string]any{"goal": {"type": "string"}, "model": {"type": "string"}, "api_key": {"type": "string"},
		"tier": {"type": "string", "enum": []any{"a", "b"}}}
}
func (*greedyDelegationProvider) RouteTask(_, _ map[string]any) (map[string]any, error) {
	return map[string]any{}, nil
}

// The selected backend extends the delegate_task schema with its enum and guidance
func TestSelectedPluginBackendExtendsTheDelegateTaskSchema(t *testing.T) {
	registerTestDelegationProvider(t, newOptionsDelegationProvider())
	ctx := delegationHome(t, "delegation:\n  backend: probe-options\n  backends:\n    probe-options:\n      options:\n"+
		"        - {name: fast, description: cheap and quick}\n        - {name: deep, description: slow and careful}\n")
	def := buildDelegateDynamicSchemaOverrides(&Env{Home: home.Dir(ctx)})
	props := def["parameters"].(map[string]any)["properties"].(map[string]any)["tasks"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
	option := props["option"].(map[string]any)
	if !reflect.DeepEqual(option["enum"], []any{"fast", "deep"}) || option["description"] != "fast: cheap and quick; deep: slow and careful" {
		t.Fatalf("option %v", option)
	}
	if !strings.HasSuffix(def["description"].(string), "\nPick an `option` per task.") {
		t.Fatalf("description %q", def["description"])
	}
	if _, ok := props["group"]; ok {
		t.Fatal("group is advertised without independent_completions")
	}
}

// A backend's settings are its own backends block, never the same keys beside the core delegation keys
func TestBackendSettingsComeFromItsOwnBackendsBlock(t *testing.T) {
	p := newOptionsDelegationProvider()
	registerTestDelegationProvider(t, p)
	cfg := map[string]any{"backend": "probe-options", "options": probeOptions, "backends": map[string]any{"model_map": map[string]any{"options": probeOptions}}}
	params, addendum := SchemaExtension(context.Background(), cfg)
	if !reflect.DeepEqual(params["option"]["enum"], []any{}) || addendum != "Pick an `option` per task." {
		t.Fatalf("params %v addendum %q", params, addendum)
	}
}

// A provider registered as default that reports unavailable falls back to the bundled strategy
func TestAnUnavailableReplacementDefaultFallsBackToTheBundledStrategy(t *testing.T) {
	u := &unavailableDelegationProvider{}
	u.Self = u
	registerTestDelegationProvider(t, u)
	p, err := GetActiveDelegationProvider(context.Background(), map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.(*testDefaultDelegationProvider); !ok {
		t.Fatalf("provider %T", p)
	}
}

type unavailableDelegationProvider struct{ DelegationProviderDefaults }

func (*unavailableDelegationProvider) Name() string                   { return "default" }
func (*unavailableDelegationProvider) DisplayName() string            { return "default" }
func (*unavailableDelegationProvider) GetSetupSchema() map[string]any { return map[string]any{} }
func (*unavailableDelegationProvider) IsAvailable() bool              { return false }
func (*unavailableDelegationProvider) RouteTask(_, _ map[string]any) (map[string]any, error) {
	panic("an unavailable provider is never asked for a route")
}

// The first use load of the bundled default never outlives a later disable
func TestFirstUseDefaultNeverOutlivesALaterDisable(t *testing.T) {
	resetDelegationFirstUse(t)
	h := t.TempDir()
	ctx := home.WithOverride(context.Background(), h)
	write := func(body string) {
		if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("delegation:\n  model: pinned-model\n")
	if p, err := GetActiveDelegationProvider(ctx, map[string]any{"model": "pinned-model"}); err != nil || p.Name() != "default" {
		t.Fatalf("%v %v", p, err)
	}
	write("plugins:\n  disabled: [delegation/default]\ndelegation:\n  model: pinned-model\n")
	_, err := GetActiveDelegationProvider(ctx, map[string]any{"model": "pinned-model"})
	var routeErr *DelegationRouteError
	if !errors.As(err, &routeErr) || !strings.Contains(routeErr.Message, "delegation/default is disabled") ||
		!strings.HasSuffix(routeErr.Message, " No subagent was started.") {
		t.Fatalf("%v", err)
	}
}

// A backend no plugin provides fails closed with the install fix after one discovery sweep
func TestAMissingBackendFailsClosedNamingTheFix(t *testing.T) {
	r := testDelegationRegistry
	r.mu.Lock()
	before := r.discoveries
	r.mu.Unlock()
	p, reason := DelegationBackendStatus(context.Background(), ConfiguredBackend(map[string]any{"backend": "  Model-Map "}))
	if p != nil || !strings.HasPrefix(reason, "Delegation backend 'model-map' is not installed") || !strings.Contains(reason, "`icebath plugins list`") {
		t.Fatalf("%v %q", p, reason)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.discoveries != before+1 {
		t.Fatalf("discoveries %d -> %d", before, r.discoveries)
	}
	if ConfiguredBackend(map[string]any{"backend": 3}) != "default" || ConfiguredBackend(nil) != "default" {
		t.Fatal("non string backend is not default")
	}
}

// The delegate_task registration carries the hermes toolset, emoji, check and static schema
func TestDelegateTaskRegistersInTheDelegationToolset(t *testing.T) {
	def := Default.Get("delegate_task")
	if def == nil || def.Toolset != "delegation" || def.Emoji != "\U0001F500" || !def.Check(nil) {
		t.Fatalf("%+v", def)
	}
	raw, _ := json.Marshal(def.Schema)
	if !strings.Contains(string(raw), `"enum":["spawn","list","steer","stop"]`) {
		t.Fatal(string(raw))
	}
	got := stripModelHiddenTaskFields([]any{map[string]any{"goal": "g", "acp_command": "x", "acp_args": []any{}}, "raw"})
	if !reflect.DeepEqual(got, []any{map[string]any{"goal": "g"}, "raw"}) {
		t.Fatalf("%v", got)
	}
	plain := []any{map[string]any{"goal": "g"}}
	if back := stripModelHiddenTaskFields(plain); &back.([]any)[0] != &plain[0] {
		t.Fatal("unchanged list was copied")
	}
	if !modelBackgroundValue(nil) {
		t.Fatal("a top level registry call is not background")
	}
}
