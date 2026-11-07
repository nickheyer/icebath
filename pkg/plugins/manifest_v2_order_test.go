package plugins

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Loads user directory plugins whose register appends their name to a shared order file
func mv2Load(t *testing.T, manifests map[string]string) (*Manager, []string, string) {
	t.Helper()
	logs := captureLogs(t)
	python := liveInterpreters(t)["system"]
	order := filepath.Join(t.TempDir(), "order.txt")
	bodies := map[string]string{}
	for name := range manifests {
		bodies[name] = "def register(ctx):\n    with open(" + pyQuote(order) + ", 'a') as f:\n        f.write(" + pyQuote(name+"\n") + ")\n"
	}
	t.Setenv("ICEBATH_ENABLE_PROJECT_PLUGINS", "0")
	empty := filepath.Join(t.TempDir(), "empty-bundled")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_BUNDLED_PLUGINS", empty)
	_, m := liveHomeWithManifests(t, python, bodies, manifests)
	raw, _ := os.ReadFile(order)
	return m, strings.Fields(string(raw)), logs.String()
}

// Temp home of host isolated directory plugins whose plugin.yaml carries the given extra lines, discovered once
func liveHomeWithManifests(t *testing.T, python string, bodies, extra map[string]string) (string, *Manager) {
	t.Helper()
	installHostEnvHooks()
	root := t.TempDir()
	h := filepath.Join(root, ".icebath")
	names := []string{}
	for name, body := range bodies {
		dir := filepath.Join(h, "plugins", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, "plugin.yaml"), "name: "+name+"\nversion: 0.1.0\ndescription: test "+name+"\n"+extra[name])
		writeFile(t, filepath.Join(dir, "__init__.py"), body)
		names = append(names, name)
	}
	writeFile(t, filepath.Join(h, "config.yaml"), "plugins:\n  isolation: "+IsolationHost+"\n  enabled: ["+strings.Join(names, ", ")+"]\n")
	t.Setenv("HOME", filepath.Join(root, "os-home"))
	t.Setenv("ICEBATH_HOME", h)
	previous := hostInterpreter
	hostInterpreter = func(context.Context) (string, error) { return python, nil }
	t.Cleanup(func() { hostInterpreter = previous })
	m := NewManager(h)
	t.Cleanup(func() {
		if m.pluginHostInstance != nil {
			m.pluginHostInstance.Shutdown()
		}
	})
	m.DiscoverAndLoad(homeCtx(h), false)
	return h, m
}

func TestManifestV2DependencyOrder(t *testing.T) {
	t.Run("dep_registers_before_dependent", func(t *testing.T) {
		_, order, _ := mv2Load(t, map[string]string{"aaa-consumer": "manifest_version: 2\nrequires_plugins:\n- id: zzz-base\n", "zzz-base": ""})
		if !reflect.DeepEqual(order, []string{"zzz-base", "aaa-consumer"}) {
			t.Fatalf("order = %v", order)
		}
	})
	t.Run("missing_dep_warns_but_loads", func(t *testing.T) {
		m, _, logs := mv2Load(t, map[string]string{"needy": "manifest_version: 2\nrequires_plugins:\n- id: ghost-plugin\n"})
		if p := m.plugins["needy"]; p == nil || !p.Enabled || !strings.Contains(logs, "ghost-plugin") || !strings.Contains(logs, "loading anyway") {
			t.Fatalf("needy = %+v logs %s", p, logs)
		}
	})
	t.Run("cycle_warns_and_falls_back_alpha", func(t *testing.T) {
		_, order, logs := mv2Load(t, map[string]string{"cyc-a": "manifest_version: 2\nrequires_plugins:\n- id: cyc-b\n", "cyc-b": "manifest_version: 2\nrequires_plugins:\n- id: cyc-a\n"})
		if !reflect.DeepEqual(order, []string{"cyc-a", "cyc-b"}) || !strings.Contains(strings.ToLower(logs), "cycle") {
			t.Fatalf("order = %v logs %s", order, logs)
		}
	})
}

func TestResolveOrderPureFunction(t *testing.T) {
	manifests := map[string]*Manifest{
		"b": {Name: "b", Key: "b", RequiresPlugins: []PluginDependency{{ID: "c"}}},
		"a": {Name: "a", Key: "a", RequiresPlugins: []PluginDependency{{ID: "b"}}},
		"c": {Name: "c", Key: "c"},
	}
	if got := ResolveLoadOrder(manifests); !reflect.DeepEqual(got, []string{"c", "b", "a"}) {
		t.Fatalf("order = %v", got)
	}
}

func TestResolveOrderMatchesByManifestName(t *testing.T) {
	manifests := map[string]*Manifest{
		"cat/impl": {Name: "impl-name", Key: "cat/impl"},
		"user":     {Name: "user", Key: "user", RequiresPlugins: []PluginDependency{{ID: "impl-name"}}},
	}
	if got := ResolveLoadOrder(manifests); !reflect.DeepEqual(got, []string{"cat/impl", "user"}) {
		t.Fatalf("order = %v", got)
	}
}

// The cycle warning names the graphlib cycle path and a self dependency is ignored with its own warning
func TestResolveOrderCycleAndSelfDependencyWarnings(t *testing.T) {
	logs := captureLogs(t)
	manifests := map[string]*Manifest{
		"a": {Name: "a", Key: "a", RequiresPlugins: []PluginDependency{{ID: "b"}}},
		"b": {Name: "b", Key: "b", RequiresPlugins: []PluginDependency{{ID: "c"}}},
		"c": {Name: "c", Key: "c", RequiresPlugins: []PluginDependency{{ID: "a"}}},
		"d": {Name: "d", Key: "d", RequiresPlugins: []PluginDependency{{ID: "d"}}},
	}
	if got := ResolveLoadOrder(manifests); !reflect.DeepEqual(got, []string{"a", "b", "c", "d"}) {
		t.Fatalf("order = %v", got)
	}
	for _, want := range []string{"Plugin d declares a dependency on itself; ignoring", "Plugin dependency cycle detected (a -> c -> b -> a); falling back to alphabetical load order for all plugins"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("missing %q in %s", want, logs.String())
		}
	}
}
