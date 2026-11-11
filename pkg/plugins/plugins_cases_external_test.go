package plugins_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Registry manager of a fixture home after one discovery sweep, the registry reset and its host stopped with the test
func casesExternalDiscover(t *testing.T, h string) *plugins.Manager {
	t.Helper()
	plugins.ResetPluginManagersForTests()
	ctx := home.WithOverride(t.Context(), h)
	m := plugins.GetPluginManager(ctx)
	t.Cleanup(func() {
		if m.CasesHostCreated() {
			if host, err := m.PluginHost(); err == nil {
				host.Shutdown()
			}
		}
		plugins.ResetPluginManagersForTests()
	})
	m.DiscoverAndLoad(ctx, false)
	return m
}

// Provider discovery runs before the entry point is enabled, the registry state upstream reaches this case with
func TestPluginLoadingEntrypointModelProviderAutoCoercedToModelProvider(t *testing.T) {
	h := plugins.CasesHome(t, "")
	provider.GetProviderProfile(home.WithOverride(t.Context(), h), "fakeprovider")
	casesExternalWrite(t, filepath.Join(h, "config.yaml"), "plugins:\n  enabled: [fakeprovider]\n")
	plugins.CasesEntryPointSite(t, "fakeprovider = fakeprovider:register\n", map[string]string{
		"fakeprovider.py": "class ProviderProfile:\n    pass\ndef register_provider(profile):\n    pass\n"})
	m := casesExternalDiscover(t, h)
	entry := m.CasesLoaded("fakeprovider")
	if entry == nil || entry.Manifest.Kind != "model-provider" {
		t.Fatalf("expected auto-coerced kind model-provider, got %+v", entry)
	}
	if entry.Module != "" || m.CasesHostCreated() {
		t.Fatalf("the general manager imported fakeprovider: module %q", entry.Module)
	}
	if profile, ok := provider.GetProviderProfile(home.WithOverride(t.Context(), h), "fakeprovider"); ok {
		t.Fatalf("fakeprovider resolved to a provider profile: %+v", profile)
	}
	if m.CasesHostCreated() {
		t.Fatal("the provider lookup imported fakeprovider")
	}
}

func TestPluginLoadingEntrypointDuplicateDoesNotBlockDirectoryProviderActivation(t *testing.T) {
	h := plugins.CasesHome(t, "  enabled: [mempalace_dup]\n")
	plugins.CasesEntryPointSite(t, "mempalace_dup = mempalace_dup:register\n", map[string]string{
		"mempalace_dup.py": "class MemPalaceProvider:\n    pass\ndef register_memory_provider(name, cls):\n    pass\n"})
	providerDir := filepath.Join(h, "plugins", "mempalace_dup")
	casesExternalWrite(t, filepath.Join(providerDir, "__init__.py"), "from agent.memory_provider import MemoryProvider\nclass MyProvider(MemoryProvider):\n"+
		"    @property\n    def name(self): return 'mempalace_dup'\n    def is_available(self): return True\n    def initialize(self, **kw): pass\n"+
		"    def sync_turn(self, *a, **kw): pass\n    def get_tool_schemas(self): return []\n    def handle_tool_call(self, *a, **kw): return '{}'\n")
	casesExternalWrite(t, filepath.Join(providerDir, "plugin.yaml"), "name: mempalace_dup\ndescription: dup\n")
	m := casesExternalDiscover(t, h)
	entry := m.CasesLoaded("mempalace_dup")
	if entry == nil || entry.Manifest.Kind != "exclusive" {
		t.Fatalf("expected auto-coerced kind exclusive, got %+v", entry)
	}
	if entry.Module != "" || m.CasesHostCreated() {
		t.Fatalf("the general manager imported mempalace_dup: module %q", entry.Module)
	}
	ctx, env := home.WithOverride(t.Context(), h), &tools.Env{Home: h}
	count := 0
	for _, discovered := range memory.DiscoverMemoryProviders(ctx, env) {
		if discovered.Name == "mempalace_dup" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("mempalace_dup discovered %d times", count)
	}
	p := memory.LoadMemoryProvider(ctx, env, "mempalace_dup")
	if p == nil {
		t.Fatal("the directory provider did not load")
	}
	if p.Name() != "mempalace_dup" || !p.Available(ctx) {
		t.Fatalf("provider name %q available %v", p.Name(), p.Available(ctx))
	}
}

// File write that creates the parent directories first
func casesExternalWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
