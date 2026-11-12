package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Command sources with the production plugin manifests of the home as runtime key owners
func runtimeKeySources(h string) config.ConfigCommandSources {
	src := moaCommandSources()
	src.RuntimeKeyOwners.ActiveManifests = func() []config.PluginConfigKeys { return plugins.ActiveConfigKeyManifests(h) }
	return src
}

// Sets a key through the plugin aware sources and reports whether the unknown key notice printed
func runtimeKeyNotice(t *testing.T, h, key, value string, out, errOut *bytes.Buffer) bool {
	t.Helper()
	out.Reset()
	errOut.Reset()
	config.ResetCache()
	if err := config.SetConfigValue(context.Background(), h, key, value, false, runtimeKeySources(h)); err != nil {
		t.Fatalf("set %s: %v %s", key, err, errOut)
	}
	if !strings.Contains(out.String(), "Set "+key) {
		t.Fatalf("set %s printed %q", key, out)
	}
	return strings.Contains(out.String()+errOut.String(), unknownKeyNotice)
}

func writeRuntimeKeysConfig(t *testing.T, h string, data map[string]any) {
	t.Helper()
	text, _ := json.Marshal(data)
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), text, 0o600); err != nil {
		t.Fatal(err)
	}
	config.ResetCache()
}

func writeKeysPlugin(t *testing.T, root, name string, configKeys []string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	text, _ := json.Marshal(map[string]any{"name": name, "config_keys": configKeys})
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), text, 0o644); err != nil {
		t.Fatal(err)
	}
}

func runtimeKeysHome(t *testing.T) (string, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	t.Setenv(plugins.EnvEnableProjectPlugins, "")
	os.Unsetenv(plugins.EnvEnableProjectPlugins)
	h, out, errOut := moaKeysHome(t, map[string]any{})
	os.Remove(filepath.Join(h, "config.yaml"))
	config.ResetCache()
	return h, out, errOut
}

func TestConfigGetBrowserSelectionKeysPrintValueWithoutNotice(t *testing.T) {
	h, out, errOut := runtimeKeysHome(t)
	writeRuntimeKeysConfig(t, h, map[string]any{"browser": map[string]any{"cloud_provider": "browser-use", "use_gateway": true}})
	for _, tc := range []struct {
		key  string
		want any
	}{{"browser.cloud_provider", "browser-use"}, {"browser.use_gateway", true}} {
		out.Reset()
		errOut.Reset()
		err := config.ConfigCommand(context.Background(), h, config.ConfigArgs{Command: "get", Key: tc.key, JSON: true}, runtimeKeySources(h))
		if err != nil {
			t.Fatal(err)
		}
		var got any
		if err := json.Unmarshal(out.Bytes(), &got); err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%s printed %q", tc.key, out)
		}
		if strings.Contains(errOut.String(), unknownKeyNotice) {
			t.Fatalf("%s noticed", tc.key)
		}
	}
}

func TestConfigSetRuntimeReadKeyIsSilent(t *testing.T) {
	for _, tc := range [][2]string{
		{"browser.cloud_provider", "browserbase"},
		{"web.use_gateway", "true"},
		{"dashboard.kanban.default_tenant", "t1"},
	} {
		h, out, errOut := runtimeKeysHome(t)
		if runtimeKeyNotice(t, h, tc[0], tc[1], out, errOut) {
			t.Fatalf("%s noticed", tc[0])
		}
	}
}

func TestEverySelectionCategoryKeySetIsWhatReadSelectionReturns(t *testing.T) {
	h, out, errOut := runtimeKeysHome(t)
	nameKeys := map[string]map[string]bool{}
	for dotted := range tools.SelectionConfigKeys() {
		section, key, _ := strings.Cut(dotted, ".")
		if nameKeys[section] == nil {
			nameKeys[section] = map[string]bool{}
		}
		nameKeys[section][key] = true
	}
	for _, want := range []string{"web", "image_gen", "video_gen", "tts", "stt", "browser"} {
		if nameKeys[want] == nil {
			t.Fatalf("selection category %s missing", want)
		}
	}
	for section, keys := range nameKeys {
		key := ""
		for _, k := range []string{"cloud_provider", "backend", "provider"} {
			if keys[k] {
				key = k
				break
			}
		}
		writeRuntimeKeysConfig(t, h, map[string]any{})
		if runtimeKeyNotice(t, h, section+"."+key, "vendor-"+section, out, errOut) {
			t.Fatalf("%s.%s noticed", section, key)
		}
		config.ResetCache()
		if got := tools.ReadSelection(&tools.Env{Home: h}, section); got != "vendor-"+section {
			t.Fatalf("%s read %q", section, got)
		}
	}
}

func TestPluginConfigKeysCountOnlyWhileThePluginIsEnabled(t *testing.T) {
	h, out, errOut := runtimeKeysHome(t)
	writeKeysPlugin(t, filepath.Join(h, "plugins"), "widget-plugin", []string{"display.widget"})
	if !runtimeKeyNotice(t, h, "display.widget.size", "3", out, errOut) {
		t.Fatal("installed but not enabled must notice")
	}
	writeRuntimeKeysConfig(t, h, map[string]any{"plugins": map[string]any{"enabled": []any{"widget-plugin"}}})
	if runtimeKeyNotice(t, h, "display.widget.size", "4", out, errOut) {
		t.Fatal("enabled plugin key noticed")
	}
	if !runtimeKeyNotice(t, h, "display.widgetx", "3", out, errOut) {
		t.Fatal("undeclared sibling must notice")
	}
	if !runtimeKeyNotice(t, h, "browser.cloud_providr", "browserbase", out, errOut) {
		t.Fatal("selection key typo must notice")
	}
	writeRuntimeKeysConfig(t, h, map[string]any{"plugins": map[string]any{"enabled": []any{"widget-plugin"}, "disabled": []any{"widget-plugin"}}})
	if !runtimeKeyNotice(t, h, "display.widget.size", "5", out, errOut) {
		t.Fatal("disabled wins")
	}
}

func TestPortablePluginDeclaresConfigKeysInItsHermesExtension(t *testing.T) {
	h, out, errOut := runtimeKeysHome(t)
	dir := filepath.Join(h, "plugins", "portwidget")
	os.MkdirAll(dir, 0o755)
	text, _ := json.Marshal(map[string]any{
		"$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json", "name": "portwidget", "version": "1.0.0",
		"extensions": map[string]any{"com.nousresearch.hermes": map[string]any{"config_keys": []any{"display.portwidget"}}},
	})
	os.WriteFile(filepath.Join(dir, "plugin.json"), text, 0o644)
	if !runtimeKeyNotice(t, h, "display.portwidget.size", "3", out, errOut) {
		t.Fatal("installed but not enabled must notice")
	}
	writeRuntimeKeysConfig(t, h, map[string]any{"plugins": map[string]any{"enabled": []any{"portwidget"}}})
	if runtimeKeyNotice(t, h, "display.portwidget.size", "4", out, errOut) {
		t.Fatal("enabled portable plugin key noticed")
	}
}

func TestPluginCannotSilenceTyposInAWholeCoreSectionThroughConfigSet(t *testing.T) {
	h, out, errOut := runtimeKeysHome(t)
	writeKeysPlugin(t, filepath.Join(h, "plugins"), "hostile", []string{"agent", "agent.turn_liveness"})
	writeRuntimeKeysConfig(t, h, map[string]any{"plugins": map[string]any{"enabled": []any{"hostile"}}})
	for _, tc := range [][2]string{{"agent.max_turnz", "agent.max_turns"}, {"agent.turn_liveness.timeout", "agent.turn_liveness.timeout_s"}} {
		out.Reset()
		errOut.Reset()
		config.ResetCache()
		if err := config.SetConfigValue(context.Background(), h, tc[0], "1", false, runtimeKeySources(h)); err != nil {
			t.Fatal(err)
		}
		printed := out.String() + errOut.String()
		if !strings.Contains(printed, unknownKeyNotice) || !strings.Contains(printed, tc[1]) {
			t.Fatalf("%s printed %q", tc[0], printed)
		}
	}
}

func TestEnabledProjectPluginDeclaresItsConfigKeys(t *testing.T) {
	h, out, errOut := runtimeKeysHome(t)
	proj := filepath.Join(t.TempDir(), "proj")
	writeKeysPlugin(t, filepath.Join(proj, ".icebath", "plugins"), "widget-plugin", []string{"display.widget"})
	writeRuntimeKeysConfig(t, h, map[string]any{"plugins": map[string]any{"enabled": []any{"widget-plugin"}}})
	t.Chdir(proj)
	if !runtimeKeyNotice(t, h, "display.widget.size", "3", out, errOut) {
		t.Fatal("project plugins are opt in")
	}
	t.Setenv(plugins.EnvEnableProjectPlugins, "1")
	if runtimeKeyNotice(t, h, "display.widget.size", "4", out, errOut) {
		t.Fatal("enabled project plugin key noticed")
	}
}

func TestSetupHiddenEnvAppliesToPluginPlatformsNobodyEnumerated(t *testing.T) {
	for _, key := range []string{"IRC_ALLOW_ALL_USERS", "SIMPLEX_HOME_CHANNEL", "NTFY_HOME_CHANNEL_NAME", "LINE_ALLOW_ALL_USERS"} {
		if !IsSetupHiddenEnv(key) {
			t.Fatal(key)
		}
	}
}
