package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/skills"
)

const dualPluginSource = `from agent.memory_provider import MemoryProvider
from .values import LABEL

class Provider(MemoryProvider):
    name = "dual"
    def is_available(self): return True
    def initialize(self, session_id, **kwargs): pass
    def get_tool_schemas(self): return []

def make_hook(label):
    def callback(**kwargs):
        return {"context": label}
    return callback

def register(ctx):
    ctx.register_memory_provider(Provider())
    ctx.register_hook("pre_llm_call", make_hook(LABEL))
    ctx.register_hook("pre_llm_call", make_hook("second"))
`

// Home with a dual kind plugin that is both a general plugin and the configured memory provider
func dualHome(t *testing.T, h string, enabled bool) (context.Context, *plugins.Manager) {
	ctx, _ := discHome(t)
	plugins.ResetPluginManagersForTests()
	plugins.InstallServices(&plugins.Services{HostObject: hostedMemoryProviderObject})
	t.Cleanup(func() {
		plugins.ResetPluginManagersForTests()
		plugins.InstallServices(nil)
	})
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("ICEBATH_BUNDLED_PLUGINS", filepath.Join(h, "empty"))
	t.Setenv("ICEBATH_ENABLE_PROJECT_PLUGINS", "")
	list := "[]"
	if enabled {
		list = "[dual]"
	}
	discWrite(t, filepath.Join(h, "config.yaml"), "plugins:\n  enabled: "+list+"\nmemory:\n  provider: dual\n")
	plugin := filepath.Join(h, "plugins", "dual")
	discWrite(t, filepath.Join(plugin, "plugin.yaml"), "name: dual\nversion: 1.0.0\nkind: standalone\n")
	discWrite(t, filepath.Join(plugin, "values.py"), "LABEL = 'first'\n")
	discWrite(t, filepath.Join(plugin, "__init__.py"), dualPluginSource)
	t.Chdir(h)
	return ctx, plugins.GetPluginManager(ctx)
}

// Host object adapter of the top level wiring for the memory provider kind general plugin loads check
func hostedMemoryProviderObject(kind string, obj any) (string, bool) {
	o, ok := obj.(*plugins.HostedObject)
	if !ok || kind != "memory_provider" {
		return "", false
	}
	name, _ := o.Get(context.Background(), "name")
	return pyrepr.Str(name), true
}

func dualContexts(ctx context.Context, m *plugins.Manager) []any {
	return m.InvokeHook(ctx, "pre_llm_call", map[string]any{"session_id": ""})
}

func hookContexts(labels ...string) []any {
	out := make([]any, len(labels))
	for i, l := range labels {
		out[i] = map[string]any{"context": l}
	}
	return out
}

func sameContexts(got, want []any) bool {
	return (len(got) == 0 && len(want) == 0) || reflect.DeepEqual(got, want)
}

// Entries of the manager fallback hook ledger memory loaders fill
func fallbackLedgerLen(m *plugins.Manager) int {
	return reflect.ValueOf(m).Elem().FieldByName("memoryHookRegistrations").Len()
}

func TestDualKindPluginHooksRunOnce(t *testing.T) {
	for _, order := range []string{"plugin-first", "memory-first", "memory-only"} {
		t.Run(order, func(t *testing.T) {
			ctx, m := dualHome(t, t.TempDir(), order != "memory-only")
			defer m.Unload(nil)
			if order == "plugin-first" {
				m.DiscoverAndLoad(ctx, false)
			}
			provider := LoadMemoryProvider(ctx, nil, "dual")
			if provider == nil || provider.Name() != "dual" {
				t.Fatalf("provider %v", provider)
			}
			if order == "memory-first" {
				m.DiscoverAndLoad(ctx, false)
			}
			expected := hookContexts("first", "second")
			if got := dualContexts(ctx, m); !sameContexts(got, expected) {
				t.Fatalf("contexts %#v", got)
			}
			if again := LoadMemoryProvider(ctx, nil, "dual"); again == nil || again == provider {
				t.Fatalf("reload returned %v", again)
			}
			if got := dualContexts(ctx, m); !sameContexts(got, expected) {
				t.Fatalf("contexts after reload %#v", got)
			}
			m.Unload(nil)
			if got := dualContexts(ctx, m); len(got) != 0 {
				t.Fatalf("contexts after unload %#v", got)
			}
			if n := fallbackLedgerLen(m); n != 0 {
				t.Fatalf("fallback ledger kept %d sources", n)
			}
			if order != "memory-only" {
				m.DiscoverAndLoad(ctx, true)
			}
			if LoadMemoryProvider(ctx, nil, "dual") == nil {
				t.Fatal("provider did not load after unload")
			}
			if got := dualContexts(ctx, m); !sameContexts(got, expected) {
				t.Fatalf("contexts after rediscovery %#v", got)
			}
		})
	}
}

func TestSameNameDifferentSourcesAreNotSuppressed(t *testing.T) {
	root := t.TempDir()
	h := filepath.Join(root, "home")
	ctx, m := dualHome(t, h, true)
	defer m.Unload(nil)
	project := filepath.Join(root, "project")
	source := filepath.Join(project, ".icebath", "plugins", "dual")
	for _, name := range []string{"plugin.yaml", "__init__.py"} {
		data, err := os.ReadFile(filepath.Join(h, "plugins", "dual", name))
		if err != nil {
			t.Fatal(err)
		}
		discWrite(t, filepath.Join(source, name), string(data))
	}
	discWrite(t, filepath.Join(source, "values.py"), "LABEL = \"project\"\n")
	t.Chdir(project)
	t.Setenv("ICEBATH_ENABLE_PROJECT_PLUGINS", "1")
	if LoadMemoryProvider(ctx, nil, "dual") == nil {
		t.Fatal("provider did not load")
	}
	m.DiscoverAndLoad(ctx, false)
	if got := dualContexts(ctx, m); !sameContexts(got, hookContexts("first", "second", "project", "second")) {
		t.Fatalf("contexts %#v", got)
	}
}

func TestReexportedRegisterUsesPluginSource(t *testing.T) {
	for _, order := range []string{"plugin-first", "memory-first"} {
		t.Run(order, func(t *testing.T) {
			h := t.TempDir()
			ctx, m := dualHome(t, h, true)
			defer m.Unload(nil)
			plugin := filepath.Join(h, "plugins", "dual")
			discWrite(t, filepath.Join(plugin, "implementation.py"), dualPluginSource)
			discWrite(t, filepath.Join(plugin, "__init__.py"), "from .implementation import register, Provider  # MemoryProvider\n")
			if order == "plugin-first" {
				m.DiscoverAndLoad(ctx, false)
			}
			if LoadMemoryProvider(ctx, nil, "dual") == nil {
				t.Fatal("provider did not load")
			}
			if order == "memory-first" {
				m.DiscoverAndLoad(ctx, false)
			}
			if got := dualContexts(ctx, m); !sameContexts(got, hookContexts("first", "second")) {
				t.Fatalf("contexts %#v", got)
			}
		})
	}
}

func TestDiscoverFindsProviders(t *testing.T) {
	ctx, _ := discHome(t)
	for _, d := range DiscoverMemoryProviders(ctx, nil) {
		if d.Name == "byterover" {
			return
		}
	}
	t.Fatal("bundled byterover not discovered")
}

func TestLoadProviderByName(t *testing.T) {
	ctx, _ := discHome(t)
	if p := LoadMemoryProvider(ctx, nil, "byterover"); p == nil || p.Name() != "byterover" {
		t.Fatalf("provider %v", p)
	}
}

func TestLoadNonexistentReturnsNone(t *testing.T) {
	ctx, _ := discHome(t)
	if p := LoadMemoryProvider(ctx, nil, "nonexistent_provider"); p != nil {
		t.Fatalf("provider %v", p)
	}
}

// User memory provider package whose provider reports display as its name
func userMemoryPlugin(t *testing.T, h, name, display string, manifest bool) {
	dir := filepath.Join(h, "plugins", name)
	discWrite(t, filepath.Join(dir, "__init__.py"), "from agent.memory_provider import MemoryProvider\n"+
		"class MyProvider(MemoryProvider):\n"+
		"    @property\n"+
		"    def name(self): return "+fmt.Sprintf("%q", display)+"\n"+
		"    def is_available(self): return True\n"+
		"    def initialize(self, **kw): pass\n"+
		"    def sync_turn(self, *a, **kw): pass\n"+
		"    def get_tool_schemas(self): return []\n"+
		"    def handle_tool_call(self, *a, **kw): return '{}'\n")
	if manifest {
		discWrite(t, filepath.Join(dir, "plugin.yaml"), "name: "+name+"\ndescription: Test user provider\n")
	}
}

func TestLoadUserPlugin(t *testing.T) {
	ctx, h := discHome(t)
	userMemoryPlugin(t, h, "myexternal", "myexternal", true)
	p := LoadMemoryProvider(ctx, nil, "myexternal")
	if p == nil || p.Name() != "myexternal" || !p.Available(ctx) {
		t.Fatalf("provider %v", p)
	}
}

func TestBundledTakesPrecedence(t *testing.T) {
	ctx, h := discHome(t)
	userMemoryPlugin(t, h, "byterover", "byterover-FAKE", false)
	if p := LoadMemoryProvider(ctx, nil, "byterover"); p == nil || p.Name() != "byterover" {
		t.Fatalf("provider %v", p)
	}
	count := 0
	for _, d := range DiscoverMemoryProviders(ctx, nil) {
		if d.Name == "byterover" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("byterover listed %d times", count)
	}
}

// Installs an entrymem entry point naming exposure in a module of its own, with a packaged skill on request
func entryPointModule(t *testing.T, module, exposure string, includeSkill bool) {
	root := discEntryPoint(t, "entrymem", module+":"+exposure)
	registerSkill := ""
	if includeSkill {
		discWrite(t, filepath.Join(root, "skills", "maintenance", "SKILL.md"),
			"---\nname: maintenance\ndescription: Memory maintenance\n---\n\nPackaged provider maintenance body.\n")
		registerSkill = "    ctx.register_skill(\n        'maintenance',\n        Path(__file__).parent / 'skills' / 'maintenance' / 'SKILL.md',\n    )\n"
	}
	discWrite(t, filepath.Join(root, module+".py"), "from pathlib import Path\n"+
		"from agent.memory_provider import MemoryProvider\n"+
		"class Provider(MemoryProvider):\n"+
		"    @property\n"+
		"    def name(self): return 'entrymem'\n"+
		"    def is_available(self): return True\n"+
		"    def initialize(self, **kw): pass\n"+
		"    def sync_turn(self, *a, **kw): pass\n"+
		"    def get_tool_schemas(self): return []\n"+
		"    def handle_tool_call(self, *a, **kw): return '{}'\n"+
		"def register(ctx):\n"+
		"    ctx.register_memory_provider(Provider())\n"+
		registerSkill+
		"def make_provider():\n"+
		"    return Provider()\n")
}

// Installs the memory services the plugin skill host reaches, load defaulting to the real provider load
func memSkillServices(t *testing.T, load func(ctx context.Context, h, name string)) {
	if load == nil {
		load = func(ctx context.Context, h, name string) { LoadMemoryProvider(home.WithOverride(ctx, h), nil, name) }
	}
	plugins.ResetPluginManagersForTests()
	plugins.InstallServices(&plugins.Services{
		ActiveMemoryProvider: func(ctx context.Context, h string) string {
			return ActiveMemoryProvider(home.WithOverride(ctx, h), nil)
		},
		PruneInactiveMemoryProviderSkills: func(ctx context.Context, h, active string) {
			PruneInactiveMemoryProviderSkills(home.WithOverride(ctx, h), nil, active)
		},
		LoadMemoryProvider: load,
	})
	t.Cleanup(func() {
		plugins.InstallServices(nil)
		plugins.ResetPluginManagersForTests()
	})
}

func memSkillView(t *testing.T, ctx context.Context, h, name string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(skills.NewScope(h, nil).SkillView(ctx, name, "", "", true)), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func wantSkillNotFound(t *testing.T, result map[string]any) {
	t.Helper()
	msg, _ := result["error"].(string)
	if result["success"] != false || !strings.Contains(strings.ToLower(msg), "not found") {
		t.Fatalf("result %v", result)
	}
}

func wantSkillServed(t *testing.T, result map[string]any, name, body string) {
	t.Helper()
	content, _ := result["content"].(string)
	if result["success"] != true || result["name"] != name || !strings.Contains(content, body) {
		t.Fatalf("result %v", result)
	}
}

func TestDiscoverFindsEntryPointProvider(t *testing.T) {
	ctx, _ := discHome(t)
	entryPointModule(t, "ep_memory_provider", "register", false)
	for _, d := range DiscoverMemoryProviders(ctx, nil) {
		if d == (DiscoveredProvider{Name: "entrymem", Description: "", Available: true}) {
			return
		}
	}
	t.Fatal("entry point provider not discovered as available")
}

func TestLoadEntryPointProvider(t *testing.T) {
	for _, exposure := range []string{"register", "Provider", "make_provider"} {
		t.Run(exposure, func(t *testing.T) {
			ctx, _ := discHome(t)
			entryPointModule(t, "ep_memory_provider", exposure, false)
			p := LoadMemoryProvider(ctx, nil, "entrymem")
			if p == nil || p.Name() != "entrymem" || !p.Available(ctx) {
				t.Fatalf("provider %v", p)
			}
		})
	}
}

func TestActiveEntryPointProviderRegistersSkill(t *testing.T) {
	ctx, h := discHome(t)
	memSkillServices(t, nil)
	entryPointModule(t, "ep_memory_provider_with_skill", "register", true)
	discWrite(t, filepath.Join(h, "config.yaml"), "memory:\n  provider: entrymem\n")
	wantSkillServed(t, memSkillView(t, ctx, h, "entrymem:maintenance"), "entrymem:maintenance", "Packaged provider maintenance body.")
}

func TestInactiveEntryPointLoadDoesNotRegisterSkill(t *testing.T) {
	ctx, h := discHome(t)
	memSkillServices(t, nil)
	entryPointModule(t, "ep_inactive_memory_provider_with_skill", "register", true)
	discWrite(t, filepath.Join(h, "config.yaml"), "memory:\n  provider: other-provider\n")
	if LoadMemoryProvider(ctx, nil, "entrymem") == nil {
		t.Fatal("provider did not load")
	}
	if path, ok := plugins.GetPluginManager(ctx).FindPluginSkill("entrymem:maintenance"); ok {
		t.Fatalf("inactive provider registered %s", path)
	}
}

func TestSwitchingProviderPrunesRegisteredEntryPointSkill(t *testing.T) {
	ctx, h := discHome(t)
	memSkillServices(t, nil)
	entryPointModule(t, "ep_switched_memory_provider_with_skill", "register", true)
	discWrite(t, filepath.Join(h, "config.yaml"), "memory:\n  provider: entrymem\n")
	wantSkillServed(t, memSkillView(t, ctx, h, "entrymem:maintenance"), "entrymem:maintenance", "Packaged provider maintenance body.")
	discWrite(t, filepath.Join(h, "config.yaml"), "memory:\n  provider: other-provider\n")
	wantSkillNotFound(t, memSkillView(t, ctx, h, "entrymem:maintenance"))
	if path, ok := plugins.GetPluginManager(ctx).FindPluginSkill("entrymem:maintenance"); ok {
		t.Fatalf("switched provider skill still registered at %s", path)
	}
}

func TestDoesNotLazyLoadInactiveMemoryProviderSkill(t *testing.T) {
	ctx, h := discHome(t)
	memSkillServices(t, func(_ context.Context, _, name string) { t.Errorf("unexpected provider load: %s", name) })
	discWrite(t, filepath.Join(h, "config.yaml"), "memory:\n  provider: active\n")
	wantSkillNotFound(t, memSkillView(t, ctx, h, "inactive:maintenance"))
}

// User memory provider named name that registers a maintenance skill with body
func memSkillProvider(t *testing.T, h, name, body string) {
	dir := filepath.Join(h, "plugins", name)
	discWrite(t, filepath.Join(dir, "skills", "maintenance", "SKILL.md"), "---\nname: maintenance\ndescription: Memory maintenance\n---\n\n"+body+"\n")
	discWrite(t, filepath.Join(dir, "__init__.py"), "from pathlib import Path\n"+
		"from agent.memory_provider import MemoryProvider\n"+
		"class Provider(MemoryProvider):\n"+
		"    @property\n"+
		"    def name(self): return "+fmt.Sprintf("%q", name)+"\n"+
		"    def is_available(self): return True\n"+
		"    def initialize(self, **kw): pass\n"+
		"    def sync_turn(self, *a, **kw): pass\n"+
		"    def get_tool_schemas(self): return []\n"+
		"    def handle_tool_call(self, *a, **kw): return '{}'\n"+
		"def register(ctx):\n"+
		"    ctx.register_memory_provider(Provider())\n"+
		"    ctx.register_skill('maintenance', Path(__file__).parent / 'skills' / 'maintenance' / 'SKILL.md')\n")
}

func TestLazilyLoadsMemoryProviderRegisteredSkill(t *testing.T) {
	ctx, _ := discHome(t)
	h := filepath.Join(t.TempDir(), ".icebath")
	t.Setenv("ICEBATH_HOME", h)
	memSkillServices(t, nil)
	memSkillProvider(t, h, "memtest", "Provider skill body.")
	discWrite(t, filepath.Join(h, "config.yaml"), "memory:\n  provider: memtest\n")
	wantSkillServed(t, memSkillView(t, ctx, h, "memtest:maintenance"), "memtest:maintenance", "Provider skill body.")
}

func TestDiscoveryDoesNotPreRegisterInactiveMemoryProviderSkills(t *testing.T) {
	ctx, _ := discHome(t)
	h := filepath.Join(t.TempDir(), ".icebath")
	t.Setenv("ICEBATH_HOME", h)
	memSkillServices(t, nil)
	memSkillProvider(t, h, "memactive", "Active body.")
	memSkillProvider(t, h, "meminactive", "Inactive body.")
	discWrite(t, filepath.Join(h, "config.yaml"), "memory:\n  provider: memactive\n")
	DiscoverMemoryProviders(ctx, nil)
	wantSkillNotFound(t, memSkillView(t, ctx, h, "meminactive:maintenance"))
	wantSkillServed(t, memSkillView(t, ctx, h, "memactive:maintenance"), "memactive:maintenance", "Active body.")
}
