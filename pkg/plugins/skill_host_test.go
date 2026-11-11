package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/skills"
)

// Temp home with an empty bundled dir and fresh managers, the process home pointing at it
func skillHostHome(t *testing.T, cfg string) (string, *Manager) {
	t.Helper()
	root := t.TempDir()
	h := filepath.Join(root, ".icebath")
	memWrite(t, filepath.Join(h, "config.yaml"), cfg)
	if err := os.MkdirAll(filepath.Join(root, "bundled"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("ICEBATH_BUNDLED_PLUGINS", filepath.Join(root, "bundled"))
	ResetPluginManagersForTests()
	t.Cleanup(ResetPluginManagersForTests)
	return h, GetPluginManager(homeCtx(h))
}

// Scope over home h with an empty skills dir so only plugin skills answer
func skillHostScope(t *testing.T, h string) *skills.Scope {
	t.Helper()
	s := skills.NewScope(h, nil)
	s.SkillsDirOverride = filepath.Join(t.TempDir(), "empty-skills")
	if err := os.MkdirAll(s.SkillsDirOverride, 0o755); err != nil {
		t.Fatal(err)
	}
	return s
}

// Writes a plugin SKILL.md under root and records it in the manager registry as the Python tests do
func registerHostSkill(t *testing.T, m *Manager, root, plugin, name, content string) string {
	t.Helper()
	if content == "" {
		content = "---\nname: " + name + "\ndescription: " + name + " desc\n---\n\n" + name + " body.\n"
	}
	md := filepath.Join(root, "plugins", plugin, "skills", name, "SKILL.md")
	memWrite(t, md, content)
	m.mu.Lock()
	m.pluginSkills[plugin+":"+name] = &PluginSkill{Path: md, Plugin: plugin, BareName: name}
	m.mu.Unlock()
	return md
}

// skill_view answer decoded from JSON
func skillViewJSON(t *testing.T, s *skills.Scope, name string) map[string]any {
	t.Helper()
	out := map[string]any{}
	if err := json.Unmarshal([]byte(s.SkillView(context.Background(), name, "", "", true)), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// test_plugin_skills.py TestPluginSkillRegistry test_list_plugin_skills and test_remove_plugin_skill
func TestPluginSkillRegistry(t *testing.T) {
	m := NewManager(t.TempDir())
	root := t.TempDir()
	for _, name := range []string{"bar", "foo", "baz"} {
		registerHostSkill(t, m, root, "myplugin", name, "---\nname: "+name+"\n---\n")
	}
	if got := m.ListPluginSkills("myplugin"); strings.Join(got, ",") != "bar,baz,foo" {
		t.Fatalf("list = %v", got)
	}
	if got := m.ListPluginSkills("other"); got == nil || len(got) != 0 {
		t.Fatalf("other = %#v", got)
	}
	registerHostSkill(t, m, root, "p", "x", "---\nname: x\n---\n")
	m.RemovePluginSkill("p:x")
	if _, ok := m.FindPluginSkill("p:x"); ok {
		t.Fatal("removed skill still found")
	}
	m.RemovePluginSkill("p:x")
}

// test_plugin_skills.py TestPluginContextRegisterSkill, all six cases
func TestPluginContextRegisterSkill(t *testing.T) {
	fresh := func() *Context {
		return newContext(&Manifest{Name: "testplugin", Key: "testplugin", Version: "1.0.0", Description: "test", Source: "user"}, NewManager(t.TempDir()))
	}
	dir := t.TempDir()
	md := filepath.Join(dir, "skills", "my-skill", "SKILL.md")
	memWrite(t, md, "---\nname: my-skill\n---\nContent.\n")
	c := fresh()
	if _, err := c.RegisterSkill("my-skill", md, "A test skill", nil); err != nil {
		t.Fatal(err)
	}
	if got, ok := c.manager.FindPluginSkill("testplugin:my-skill"); !ok || got != md {
		t.Fatalf("happy path = %q %v", got, ok)
	}
	if _, err := fresh().RegisterSkill("ns:foo", md, "", nil); err == nil || !strings.Contains(err.Error(), "must not contain ':'") {
		t.Fatalf("colon = %v", err)
	}
	var missing *FileNotFoundError
	if _, err := fresh().RegisterSkill("foo", filepath.Join(dir, "nonexistent.md"), "", nil); !errors.As(err, &missing) {
		t.Fatalf("missing = %v", err)
	}
	first, second := filepath.Join(dir, "first", "SKILL.md"), filepath.Join(dir, "second", "SKILL.md")
	memWrite(t, first, "first")
	memWrite(t, second, "second")
	portable := fresh()
	portable.manifest.Portable = true
	if _, err := portable.RegisterSkill("foo", first, "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := portable.RegisterSkill("foo", second, "", nil); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("portable duplicate = %v", err)
	}
	native := fresh()
	for _, p := range []string{first, second} {
		if _, err := native.RegisterSkill("foo", p, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := native.manager.FindPluginSkill("testplugin:foo"); got != second {
		t.Fatalf("native overwrite = %q", got)
	}
}

// test_plugin_skills.py skill_view halves served by the registered host over a real manager
func TestRegisteredHostServesQualifiedSkillView(t *testing.T) {
	h, m := skillHostHome(t, "{}\n")
	s := skillHostScope(t, h)
	root := t.TempDir()
	registerHostSkill(t, m, root, "superpowers", "writing-plans", "")
	r := skillViewJSON(t, s, "superpowers:writing-plans")
	if r["success"] != true || r["name"] != "superpowers:writing-plans" || !strings.Contains(r["content"].(string), "writing-plans body.") {
		t.Fatalf("qualified %v", r)
	}
	r = skillViewJSON(t, s, "plugin:superpowers:writing-plans")
	if r["success"] != true || r["name"] != "superpowers:writing-plans" {
		t.Fatalf("index heading prefix %v", r)
	}
	r = skillViewJSON(t, s, "superpowers:nonexistent")
	if r["success"] != false || !strings.Contains(r["error"].(string), "not found") || len(r["available_skills"].([]any)) != 1 {
		t.Fatalf("missing skill %v", r)
	}
	r = skillViewJSON(t, s, "bad.name:skill")
	if r["success"] != false || !strings.Contains(r["error"].(string), "Invalid namespace") {
		t.Fatalf("namespace %v", r)
	}
	md := registerHostSkill(t, m, root, "stale", "gone", "")
	os.Remove(md)
	r = skillViewJSON(t, s, "stale:gone")
	if r["success"] != false || !strings.Contains(r["error"].(string), "no longer exists") {
		t.Fatalf("stale %v", r)
	}
	if _, ok := m.FindPluginSkill("stale:gone"); ok {
		t.Fatal("stale entry kept")
	}
}

// test_plugin_skills.py test_disabled_plugin and skills_list listing through the host
func TestRegisteredHostListsAndGuardsPluginSkills(t *testing.T) {
	h, m := skillHostHome(t, "plugins:\n  disabled: [parked]\n")
	s := skillHostScope(t, h)
	root := t.TempDir()
	registerHostSkill(t, m, root, "parked", "foo", "")
	registerHostSkill(t, m, root, "superpowers", "writing-plans", "")
	m.mu.Lock()
	m.pluginSkills["superpowers:writing-plans"].Description = "Plan writing"
	m.pluginSkills["superpowers:alien"] = &PluginSkill{Path: filepath.Join(root, "x"), Plugin: "superpowers", BareName: "alien",
		Frontmatter: map[string]any{"platforms": []any{"plan9"}}}
	m.mu.Unlock()
	r := skillViewJSON(t, s, "parked:foo")
	if r["success"] != false || !strings.Contains(r["error"].(string), "disabled") {
		t.Fatalf("disabled plugin %v", r)
	}
	listed := map[string]any{}
	if err := json.Unmarshal([]byte(s.SkillsList(context.Background(), "")), &listed); err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, sk := range listed["skills"].([]any) {
		entry := sk.(map[string]any)
		if _, leaked := entry["frontmatter"]; leaked {
			t.Fatalf("frontmatter leaked %v", entry)
		}
		names = append(names, entry["name"].(string)+"="+entry["description"].(string))
	}
	if strings.Join(names, ",") != "parked:foo=,superpowers:writing-plans=Plan writing" {
		t.Fatalf("listed %v", listed)
	}
	host := newSkillHost(context.Background(), h)
	if got := host.SkillOwners("parked:foo"); strings.Join(got, ",") != "parked" {
		t.Fatalf("owners %v", got)
	}
	if !host.DisabledPlugins()["parked"] {
		t.Fatal("disabled plugins missing parked")
	}
	if _, err := host.ActiveMemoryProvider(); err == nil {
		t.Fatal("memory provider answered without the service")
	}
}

// test_setup_beat_plugin_skills.py test_a_skill_saved_under_the_hashed_portable_name_still_loads with real discovery
func TestASkillSavedUnderTheHashedPortableNameStillLoads(t *testing.T) {
	h, m := skillHostHome(t, "{\"plugins\": {\"enabled\": [\"blender\"]}}\n")
	plugin := filepath.Join(h, "plugins", "blender")
	memWrite(t, filepath.Join(plugin, "plugin.json"), "{\"$schema\": \""+PluginSchemaV1+"\", \"name\": \"blender\"}")
	memWrite(t, filepath.Join(plugin, "skills", "blender", "SKILL.md"), "---\nname: blender\ndescription: Drive Blender.\n---\nBody.\n")
	memWrite(t, filepath.Join(h, "plugins", ".install-metadata.json"), "{\"blender\": {\"source\": \"https://example.com/blender.git\", "+
		"\"catalog\": {\"name\": \"blender\", \"repo\": \"https://example.com/blender.git\", \"sha\": \""+strings.Repeat("0", 40)+"\"}}}")
	m.DiscoverAndLoad(homeCtx(h), true)
	meta := m.ListPluginSkillMetadata()
	if len(meta) != 1 || meta[0]["name"] != "blender:blender" {
		t.Fatalf("registered %v", meta)
	}
	r := skillViewJSON(t, skillHostScope(t, h), portableDataNamespace("blender")+":blender")
	if r["success"] != true || r["name"] != "blender:blender" {
		t.Fatalf("legacy %v", r)
	}
	if got := newSkillHost(context.Background(), h).LegacyPortableSkillName("agent-plugin-other-00000000", "blender"); got != "" {
		t.Fatalf("foreign namespace %q", got)
	}
}

// Memory provider package whose register adds a maintenance skill beside its own file
func skillProviderBody(name string) string {
	return "from pathlib import Path\nfrom agent.memory_provider import MemoryProvider\nclass Provider(MemoryProvider):\n" +
		"    @property\n    def name(self): return " + `"` + name + `"` + "\n    def is_available(self): return True\n" +
		"    def initialize(self, **kw): pass\n    def sync_turn(self, *a, **kw): pass\n    def get_tool_schemas(self): return []\n" +
		"    def handle_tool_call(self, *a, **kw): return '{}'\ndef register(ctx):\n    ctx.register_memory_provider(Provider())\n" +
		"    ctx.register_skill('maintenance', Path(__file__).parent / 'skills' / 'maintenance' / 'SKILL.md')\n"
}

// test_plugin_skills.py test_lazily_loads_memory_provider_registered_skill and test_discovery_does_not_pre_register_inactive_memory_provider_skills over hosted providers
func TestMemoryProviderSkillsResolveThroughHost(t *testing.T) {
	installLiveServices(t)
	h, _ := liveHome(t, liveInterpreters(t)["system"], nil)
	m := memoryManager(t, h)
	for name, body := range map[string]string{"memactive": "Active body.", "meminactive": "Inactive body.", "memtest": "Provider skill body."} {
		dir := memoryProviderDir(t, h, name, skillProviderBody(name))
		memWrite(t, filepath.Join(dir, "skills", "maintenance", "SKILL.md"), "---\nname: maintenance\ndescription: Memory maintenance\n---\n\n"+body+"\n")
	}
	active, loads, tracked := "memtest", []string{}, map[string]string{}
	load := func(ctx context.Context, name string, registerSkills bool) {
		track := func(q string) { tracked[q], _ = m.FindPluginSkill(q) }
		if _, err := CollectMemoryProviderInHost(ctx, name, filepath.Join(h, "plugins", name), memoryModule(name), registerSkills, track); err != nil {
			t.Fatalf("collect %s = %v", name, err)
		}
	}
	svc := *installedServices()
	svc.ActiveMemoryProvider = func(context.Context, string) string { return active }
	svc.LoadMemoryProvider = func(ctx context.Context, _, name string) {
		loads = append(loads, name)
		load(ctx, name, name == active)
	}
	svc.PruneInactiveMemoryProviderSkills = func(_ context.Context, _, keep string) {
		for q, p := range tracked {
			if ns, _, _ := strings.Cut(q, ":"); ns == keep {
				continue
			}
			if current, ok := m.FindPluginSkill(q); ok && current == p {
				m.RemovePluginSkill(q)
			}
			delete(tracked, q)
		}
	}
	InstallServices(&svc)
	s := skillHostScope(t, h)
	r := skillViewJSON(t, s, "inactive:maintenance")
	if r["success"] != false || !strings.Contains(strings.ToLower(r["error"].(string)), "not found") || len(loads) != 0 {
		t.Fatalf("inactive provider %v loads %v", r, loads)
	}
	r = skillViewJSON(t, s, "memtest:maintenance")
	if r["success"] != true || r["name"] != "memtest:maintenance" || !strings.Contains(r["content"].(string), "Provider skill body.") {
		t.Fatalf("lazy load %v", r)
	}
	active = "memactive"
	for _, name := range []string{"memactive", "meminactive"} {
		load(homeCtx(h), name, false)
	}
	r = skillViewJSON(t, s, "meminactive:maintenance")
	if r["success"] != false || !strings.Contains(strings.ToLower(r["error"].(string)), "not found") {
		t.Fatalf("inactive discovered %v", r)
	}
	r = skillViewJSON(t, s, "memactive:maintenance")
	if r["success"] != true || r["name"] != "memactive:maintenance" || !strings.Contains(r["content"].(string), "Active body.") {
		t.Fatalf("active %v", r)
	}
	if _, ok := m.FindPluginSkill("memtest:maintenance"); ok {
		t.Fatal("prune kept the formerly active provider skill")
	}
}
