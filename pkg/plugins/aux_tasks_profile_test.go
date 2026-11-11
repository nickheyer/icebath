package plugins

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Writes a host plugin whose register makes the one given context call
func writeAuxPlugin(t *testing.T, h, name, call string) {
	t.Helper()
	dir := filepath.Join(h, "plugins", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "plugin.yaml"), `{"name": "`+name+`", "version": "0.1.0", "description": "`+name+` probe"}`)
	writeFile(t, filepath.Join(dir, "__init__.py"), "def register(ctx):\n    ctx."+call+"\n")
}

// Port of test_listing_is_per_profile_and_carries_inheritance, the per profile discovery half the dashboard lists
func TestPluginAuxTasksListingIsPerProfileAndCarriesInheritance(t *testing.T) {
	python := liveInterpreters(t)["system"]
	installHostEnvHooks()
	installLlmServices(t, []string{"vision", "compression"}, nil)
	ledgerSwapBundled(t)
	t.Setenv("PYTHONPATH", "")
	previous := hostInterpreter
	hostInterpreter = func(context.Context) (string, error) { return python, nil }
	t.Cleanup(func() { hostInterpreter = previous })
	side := "register_auxiliary_task('side_task', display_name='Side model', description='side model for side', inherit_from='compression')"
	alpha := "register_auxiliary_task('alpha_task', display_name='Alpha side model', description='side model for alpha_plugin', defaults={'timeout': 7})"
	homeA := t.TempDir()
	homeB := filepath.Join(homeA, "profiles", "b")
	writeAuxPlugin(t, homeA, "side", side)
	writeAuxPlugin(t, homeA, "alpha_plugin", alpha)
	writeAuxPlugin(t, homeB, "side", side)
	writeFile(t, filepath.Join(homeA, "config.yaml"), `{"plugins": {"isolation": "host", "enabled": ["side", "alpha_plugin"]}, "auxiliary": {"compression": {"provider": "openrouter", "model": "vendor/model-a"}}}`)
	writeFile(t, filepath.Join(homeB, "config.yaml"), `{"plugins": {"isolation": "host", "enabled": ["side"]}, "auxiliary": {"compression": {"provider": "openrouter", "model": "vendor/model-b"}}}`)
	t.Setenv("ICEBATH_HOME", homeA)
	rows := func(h string) map[string]map[string]any {
		out := map[string]map[string]any{}
		for _, row := range GetPluginAuxiliaryTasks(homeCtx(h)) {
			out[row["key"].(string)] = row
		}
		t.Cleanup(func() {
			if m := GetPluginManager(homeCtx(h)); m.pluginHostInstance != nil {
				m.pluginHostInstance.Shutdown()
			}
		})
		return out
	}
	a, b := rows(homeA), rows(homeB)
	if got := sortedKeys(a); !reflect.DeepEqual(got, []string{"alpha_task", "side_task"}) {
		t.Fatalf("profile a tasks %v", a)
	}
	if got := sortedKeys(b); !reflect.DeepEqual(got, []string{"side_task"}) {
		t.Fatalf("profile b tasks %v", b)
	}
	for _, row := range []map[string]any{a["side_task"], b["side_task"]} {
		if row["inherit_from"] != "compression" || row["plugin"] != "side" || row["display_name"] != "Side model" {
			t.Fatalf("side row %v", row)
		}
	}
	if a["alpha_task"]["inherit_from"] != nil || fmt.Sprint(a["alpha_task"]["defaults"].(map[string]any)["timeout"]) != "7" {
		t.Fatalf("alpha row %v", a["alpha_task"])
	}
}
