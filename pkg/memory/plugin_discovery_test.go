package memory

import (
	"context"
	"encoding/json"
	"fmt"
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
