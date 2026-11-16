package plugins

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

func TestPluginInBothListsWarnsInsteadOfSilentDebugSkip(t *testing.T) {
	h := t.TempDir()
	dir := filepath.Join(h, "plugins", "obs", "intake")
	writeTree(t, dir, map[string]string{"plugin.yaml": "name: intake-probe\nversion: '1.0'\n", "__init__.py": "def register(ctx):\n    pass\n"})
	var manifest *Manifest
	for _, m := range CollectDirectoryManifests(h) {
		if m.Name == "intake-probe" {
			manifest = m
		}
	}
	if manifest == nil {
		t.Fatal("intake-probe not collected")
	}
	conflicting := GateManifest(h, manifest, map[string]bool{"obs/intake": true}, map[string]bool{"obs/intake": true}, true)
	if conflicting.Action != "placeholder" || conflicting.Error != "listed in both plugins.enabled and plugins.disabled — disabled wins" || conflicting.Level != "warn" {
		t.Fatalf("conflicting %+v", conflicting)
	}
	if want := "Plugin 'obs/intake' is listed in both plugins.enabled and plugins.disabled; the disable wins and it will not load — remove it from one of the two lists"; conflicting.Log != want {
		t.Fatalf("log %q", conflicting.Log)
	}
	if cross := GateManifest(h, manifest, map[string]bool{"obs/intake": true}, map[string]bool{"intake-probe": true}, true); cross.Level != "warn" {
		t.Fatalf("cross %+v", cross)
	}
	if plain := GateManifest(h, manifest, map[string]bool{"obs/intake": true}, map[string]bool{"other/plugin": true}, true); plain.Error != "disabled via config" || plain.Level != "debug" {
		t.Fatalf("plain %+v", plain)
	}
}

// Each refusing gate outcome logs the hermes gate_manifest message at its level
func TestGateManifestLogsTheHermesMessages(t *testing.T) {
	h := t.TempDir()
	target := filepath.Join(h, "plugins", "later-killed")
	for path, v := range map[string]any{
		liveCachePath(h): map[string]any{"entries": []any{}, "removed": []any{map[string]any{"name": "killed", "reason": "backdoor"}}},
		filepath.Join(h, "plugins", ".install-metadata.json"): map[string]any{"later-killed": map[string]any{"source": "file:///x/later-killed#main", "catalog": map[string]any{"name": "killed"}}},
	} {
		raw, _ := json.Marshal(v)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	relayErr := "removed — Relay lifecycle is owned by Icebath core; configure a standard user or system Relay plugins.toml, or use " +
		tools.RelayPluginsConfigEnv + " for an explicit user-file override"
	none := map[string]bool{}
	for _, tc := range []struct {
		m                  *Manifest
		disabled, enabled  map[string]bool
		level, log, errMsg string
	}{
		{&Manifest{Name: "nemo_relay", Key: "observability/nemo_relay", Source: "user"}, none, none, "warn",
			"Refusing to load removed Icebath Relay plugin 'observability/nemo_relay'; " + relayErr, relayErr},
		{&Manifest{Name: "quiet", Key: "quiet", Source: "user"}, map[string]bool{"quiet": true}, none, "debug", "Skipping disabled plugin 'quiet'", "disabled via config"},
		{&Manifest{Name: "mem", Key: "memory/mem", Source: "user", Kind: "exclusive"}, none, none, "debug",
			"Skipping 'memory/mem' (exclusive, handled by category discovery)", "exclusive plugin — activate via <category>.provider config"},
		{&Manifest{Name: "prov", Key: "model-providers/prov", Source: "user", Kind: "model-provider"}, none, none, "debug",
			"Skipping 'model-providers/prov' (model-provider, handled by providers/ discovery)", ""},
		{&Manifest{Name: "idle", Key: "idle", Source: "user"}, none, none, "debug", "Skipping 'idle' (not in plugins.enabled)",
			"not enabled in config (run `icebath plugins enable idle` to activate)"},
		{&Manifest{Name: "killed", Key: "killed", Source: "user", Path: target}, none, map[string]bool{"killed": true}, "warn",
			"Refusing to load plugin 'killed' — removed from the Icebath plugin catalog: backdoor; run `icebath plugins remove`",
			"removed from the Icebath plugin catalog: backdoor"},
	} {
		g := GateManifest(h, tc.m, tc.disabled, tc.enabled, true)
		if g.Action != "placeholder" || g.Level != tc.level || g.Log != tc.log || g.Error != tc.errMsg {
			t.Fatalf("%s gate %+v", tc.m.Key, g)
		}
	}
}
