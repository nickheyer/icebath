package plugins

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Depprobe plugin in the selected generation home loaded by a fresh manager under host isolation
func depprobeHome(t *testing.T, manifest, initPy, pyproject string) (string, *Manager) {
	t.Helper()
	python := liveInterpreters(t)["system"]
	installHostEnvHooks()
	selectGeneration(t, true, "1.0")
	t.Setenv("PYTHONPATH", "")
	h := os.Getenv("ICEBATH_HOME")
	dir := filepath.Join(h, "plugins", "depprobe")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "plugin.yaml"), manifest)
	writeFile(t, filepath.Join(dir, "__init__.py"), initPy)
	if pyproject != "" {
		writeFile(t, filepath.Join(dir, "pyproject.toml"), pyproject)
	}
	writeFile(t, filepath.Join(h, "config.yaml"), "plugins:\n  isolation: host\n  enabled: [depprobe]\n  disabled: []\n")
	previous := hostInterpreter
	hostInterpreter = func(context.Context) (string, error) { return python, nil }
	t.Cleanup(func() { hostInterpreter = previous })
	m := NewManager(h)
	t.Cleanup(func() {
		if m.pluginHostInstance != nil {
			m.pluginHostInstance.Shutdown()
		}
	})
	m.DiscoverAndLoad(homeCtx(h), true)
	return h, m
}

const importsFixture = "import icebath_depprobe_fixture\n\ndef register(ctx):\n    pass\n"

// Port of test_restart_follows_what_pm_installs_for_the_plugin
func TestRestartFollowsWhatPMInstallsForThePlugin(t *testing.T) {
	cases := []struct {
		shape, manifest, initPy, pyproject string
		restart                            bool
	}{
		{"pyproject", "name: depprobe\nversion: '1.0'\n", importsFixture,
			"[project]\nname = \"depprobe\"\nversion = \"1.0\"\ndependencies = [\"" + depReq + "\"]\n", true},
		{"pip_dependencies", "name: depprobe\nversion: '1.0'\npip_dependencies:\n  - '" + depReq + "'\n", importsFixture, "", true},
		{"external", "name: depprobe\nversion: '1.0'\npython_runtime: external\npython_dependencies:\n  - '" + depReq + "'\n",
			"def register(ctx):\n    pass\n", "", false},
	}
	for _, c := range cases {
		t.Run(c.shape, func(t *testing.T) {
			h, m := depprobeHome(t, c.manifest, c.initPy, c.pyproject)
			ctx := homeCtx(h)
			entry, err := findActivation(summaryItems(append(ActivationSummaries(ctx, m), DependencyBlockedSummaries(ctx, m)...)), "depprobe")
			if err != nil {
				t.Fatal(err)
			}
			if got := entry != nil && len(toList(entry["missing_dependencies"])) > 0; got != c.restart {
				t.Fatalf("restart = %v for %#v (load error %q)", got, entry, loadedError(m, "depprobe"))
			}
		})
	}
}

// Port of test_a_plugin_importing_its_new_dependency_at_load_still_asks_for_a_restart
func TestPluginImportingItsNewDependencyAtLoadStillAsksForARestart(t *testing.T) {
	h, m := depprobeHome(t, "name: depprobe\nversion: '1.0'\npython_dependencies:\n  - '"+depReq+"'\n", importsFixture, "")
	ctx := homeCtx(h)
	m.mu.RLock()
	names := anyList(sortedKeys(m.plugins))
	m.mu.RUnlock()
	answer := map[string]any{"reloaded": true, "home": h, "plugins": names,
		"activations": summaryItems(append(ActivationSummaries(ctx, m), DependencyBlockedSummaries(ctx, m)...))}
	calls := []gatewayCall{}
	installActivationServices(t, &Services{HostRendezvous: &fakeRendezvous{}, ReloadGatewayPlugins: fakeGateway(answer, nil, &calls)})
	result, hint := activateHint(t)
	act, _ := result["activation"].(map[string]any)
	if result["restart_required"] != true || !reflect.DeepEqual(act["missing_dependencies"], []any{depReq}) ||
		strings.Contains(hint, "nothing of this plugin needs") {
		t.Fatalf("result = %#v hint = %q", result, hint)
	}
	for field, kind := range map[string]string{"name": "str", "key": "str", "activated_now": "dict", "deferred": "dict", "missing_dependencies": "list", "missing_in": "list"} {
		var ok bool
		switch kind {
		case "str":
			_, ok = act[field].(string)
		case "dict":
			_, ok = act[field].(map[string]any)
		case "list":
			_, ok = act[field].([]any)
		}
		if !ok {
			t.Fatalf("activation field %s is not a %s: %#v", field, kind, act)
		}
	}
}

// Reports whether the manager holds the probe hook on on_session_start
func probeHooked(m *Manager) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, e := range m.hooks["on_session_start"] {
		if strings.Contains(e.name, "_probe_hook") {
			return true
		}
	}
	return false
}

// Port of test_the_gateways_forced_rediscovery_drops_a_disabled_plugins_hook
func TestForcedRediscoveryDropsADisabledPluginsHook(t *testing.T) {
	h, m := liveHome(t, liveInterpreters(t)["system"], map[string]string{"probe": "def _probe_hook(**kwargs):\n    return None\n\n" +
		"def register(ctx):\n    ctx.register_hook('on_session_start', _probe_hook)\n"})
	m.DiscoverAndLoad(homeCtx(h), true)
	if !probeHooked(m) {
		t.Fatalf("probe hook missing after discovery (load error %q)", loadedError(m, "probe"))
	}
	writeFile(t, filepath.Join(h, "config.yaml"), "plugins:\n  isolation: host\n  enabled: []\n  disabled: [probe]\n")
	m.DiscoverAndLoad(homeCtx(h), true)
	if probeHooked(m) {
		t.Fatal("a disabled plugin kept its hook across the forced rediscovery")
	}
}
