package plugins

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// old_updater_surface.json plugin_compat rows: the three frozen exports stay inert
func TestPluginCompatFrozenExports(t *testing.T) {
	if r := CompatReport(nil, true); len(r) != 0 || r == nil {
		t.Fatalf("report %v", r)
	}
	if !RemovalInEffect(nil) || len(SummaryLines(map[string][]any{}, nil)) != 0 {
		t.Fatal("removal and summary")
	}
}

const legacyFixtureInit = `"""Frozen legacy plugin used by the behavior compatibility suite."""

received_sessions = []


def _on_session_start(*, session_id):
    received_sessions.append(session_id)
    return {"legacy_session_id": session_id, "received": list(received_sessions)}


def register(ctx):
    ctx.register_hook("on_session_start", _on_session_start)
`

// test_legacy_plugin_loads_and_ignores_additive_hook_and_manifest_fields through a hosted child
func TestLegacyPluginLoadsAndIgnoresAdditiveHookAndManifestFields(t *testing.T) {
	installLiveServices(t)
	installHostEnvHooks()
	python := liveInterpreters(t)["system"]
	root := t.TempDir()
	h := filepath.Join(root, "icebath-home")
	dir := filepath.Join(h, "plugins", "legacy-contract-fixture")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "plugin.yaml"), "name: legacy-contract-fixture\nversion: \"0.1.0\"\ndescription: Frozen plugin fixture for native behavior compatibility\nprovides_hooks:\n  - on_session_start\nfuture_manifest_field:\n  schema: 2\n  additive: true\n")
	writeFile(t, filepath.Join(dir, "__init__.py"), legacyFixtureInit)
	writeFile(t, filepath.Join(h, "config.yaml"), "plugins:\n  enabled:\n  - legacy-contract-fixture\n")
	bundled := filepath.Join(root, "bundled-plugins")
	if err := os.MkdirAll(bundled, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", filepath.Join(root, "os-home"))
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("ICEBATH_BUNDLED_PLUGINS", bundled)
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
	m.mu.RLock()
	loaded := m.plugins["legacy-contract-fixture"]
	m.mu.RUnlock()
	if loaded == nil || !loaded.Enabled || loaded.Error != "" || loaded.Module == "" || loaded.Manifest.Version != "0.1.0" {
		t.Fatalf("loaded %+v", loaded)
	}
	hooked := false
	for _, name := range loaded.HooksRegistered {
		hooked = hooked || name == "on_session_start"
	}
	results := m.InvokeHook(homeCtx(h), "on_session_start", map[string]any{"session_id": "legacy-session", "resumed": true, "future_additive_field": map[string]any{"nested": "value"}})
	want := []any{map[string]any{"legacy_session_id": "legacy-session", "received": []any{"legacy-session"}}}
	if !hooked || !reflect.DeepEqual(results, want) {
		t.Fatalf("hooks %v results %#v", loaded.HooksRegistered, results)
	}
}

// plugin_install_phase.py InstallPhase ids the desktop catalog card words in its own language
func TestInstallPhaseIDs(t *testing.T) {
	got := []string{string(InstallPhaseDownloading), string(InstallPhasePythonPackages), string(InstallPhaseLoadingTools)}
	if !reflect.DeepEqual(got, []string{"downloading", "python_packages", "loading_tools"}) {
		t.Fatalf("phases %v", got)
	}
}
