package plugins

import (
	"reflect"
	"strings"
	"testing"
)

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
