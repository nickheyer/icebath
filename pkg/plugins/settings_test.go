package plugins

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

const demoSettingsManifest = `name: demo-plugin
version: 1.0.0
config_schema:
  api_url: {type: str, default: "https://example.invalid", description: "Service endpoint"}
  retries: {type: int, default: 3}
  verbose: {type: bool, default: false}
  mode: {type: str, choices: [fast, careful], default: fast}
  api_key: {type: secret, description: "Token"}
`

const portableSettingsManifest = `{
  "$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json",
  "name": "portable-demo",
  "version": "1.0.0",
  "extensions": {"com.nousresearch.hermes": {"config_schema": {
    "show_chip": {"type": "bool", "default": true},
    "api_key": {"type": "secret", "env": "PORTABLE_DEMO_KEY"}
  }}}
}
`

func settingsHome(t *testing.T) (string, string) {
	h := filepath.Join(t.TempDir(), "icebath-home")
	dir := filepath.Join(h, "plugins", "demo-plugin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte(demoSettingsManifest), 0o644)
	_ = os.WriteFile(filepath.Join(h, "config.yaml"), []byte("plugins:\n  entries:\n    demo-plugin:\n      settings:\n        retries: 7\n"), 0o644)
	return h, dir
}

func fieldsByKey(fields []map[string]any) (map[string]map[string]any, []string) {
	out := map[string]map[string]any{}
	var order []string
	for _, f := range fields {
		out[f["key"].(string)] = f
		order = append(order, f["key"].(string))
	}
	return out, order
}

func TestFieldTypesMatchConfigSchemaTypes(t *testing.T) {
	for name := range settingsFieldTypes {
		if _, ok := ConfigSchemaTypes[name]; !ok {
			t.Fatalf("field type %s missing from ConfigSchemaTypes", name)
		}
	}
	for name := range ConfigSchemaTypes {
		if _, ok := settingsFieldTypes[name]; !ok {
			t.Fatalf("schema type %s missing from field types", name)
		}
	}
}

func TestListCarriesSchemaFieldsWithCurrentValuesAndNoSecretValues(t *testing.T) {
	h, dir := settingsHome(t)
	t.Setenv("DEMO_PLUGIN_API_KEY", "shh")
	fields, order := fieldsByKey(PluginSettingsFields(context.Background(), h, "demo-plugin", dir))
	if !reflect.DeepEqual(order, []string{"api_url", "retries", "verbose", "mode", "api_key"}) {
		t.Fatalf("order %v", order)
	}
	if fields["api_url"]["type"] != "string" || fields["api_url"]["value"] != "https://example.invalid" {
		t.Fatal(fields["api_url"])
	}
	if fields["retries"]["type"] != "number" || fields["retries"]["value"] != 7 {
		t.Fatal(fields["retries"])
	}
	if fields["verbose"]["type"] != "boolean" || fields["verbose"]["value"] != false {
		t.Fatal(fields["verbose"])
	}
	if fields["mode"]["type"] != "enum" || !reflect.DeepEqual(fields["mode"]["choices"], []any{"fast", "careful"}) {
		t.Fatal(fields["mode"])
	}
	want := map[string]any{"key": "api_key", "type": "secret", "label": "api_key", "description": "Token",
		"required": false, "env": "DEMO_PLUGIN_API_KEY", "has_value": true}
	if !reflect.DeepEqual(fields["api_key"], want) {
		t.Fatalf("%#v", fields["api_key"])
	}
}

func TestSettingsWritesThePluginNamespaceAndRefusesSecretsAndBadTypes(t *testing.T) {
	h, dir := settingsHome(t)
	written, err := SavePluginSettings(context.Background(), h, "demo-plugin", dir, []PluginSettingValue{
		{"api_url", "https://real.invalid"}, {"retries", 2}, {"mode", "careful"}})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(written)
	if !reflect.DeepEqual(written, []string{"api_url", "mode", "retries"}) {
		t.Fatal(written)
	}
	entry := pluginSettingsEntry(loadConfigOrEmpty(h), "demo-plugin")
	if !reflect.DeepEqual(entry["settings"], map[string]any{"api_url": "https://real.invalid", "retries": 2, "mode": "careful"}) {
		t.Fatalf("%#v", entry["settings"])
	}
	fields, _ := fieldsByKey(PluginSettingsFields(context.Background(), h, "demo-plugin", dir))
	if fields["retries"]["value"] != 2 || fields["mode"]["value"] != "careful" {
		t.Fatal(fields)
	}
	for _, bad := range []PluginSettingValue{{"api_key", "leak"}, {"retries", "two"}, {"mode", "reckless"}, {"unknown", 1}, {"retries", true}} {
		if _, err := SavePluginSettings(context.Background(), h, "demo-plugin", dir, []PluginSettingValue{bad}); err == nil {
			t.Fatalf("%v accepted", bad)
		}
	}
	raw, _ := os.ReadFile(filepath.Join(h, "config.yaml"))
	if strings.Contains(string(raw), "api_key") {
		t.Fatal("secret written to config.yaml")
	}
}

func TestPortablePluginJSONSchemaListsAndSavesLikeANativeOne(t *testing.T) {
	h, _ := settingsHome(t)
	dir := filepath.Join(h, "plugins", "portable-demo")
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(portableSettingsManifest), 0o644)
	fields, order := fieldsByKey(PluginSettingsFields(context.Background(), h, "portable-demo", dir))
	if !reflect.DeepEqual(order, []string{"show_chip", "api_key"}) {
		t.Fatal(order)
	}
	if fields["show_chip"]["type"] != "boolean" || fields["show_chip"]["value"] != true {
		t.Fatal(fields["show_chip"])
	}
	if fields["api_key"]["type"] != "secret" || fields["api_key"]["env"] != "PORTABLE_DEMO_KEY" {
		t.Fatal(fields["api_key"])
	}
	written, err := SavePluginSettings(context.Background(), h, "portable-demo", dir, []PluginSettingValue{{"show_chip", false}})
	if err != nil || !reflect.DeepEqual(written, []string{"show_chip"}) {
		t.Fatal(written, err)
	}
	if got := pluginSettingsEntry(loadConfigOrEmpty(h), "portable-demo")["settings"]; !reflect.DeepEqual(got, map[string]any{"show_chip": false}) {
		t.Fatalf("%#v", got)
	}
	if _, err := SavePluginSettings(context.Background(), h, "portable-demo", dir, []PluginSettingValue{{"api_key", "leak"}}); err == nil {
		t.Fatal("secret accepted")
	}
}

func TestSecretEnvNameUpperSnakesPluginAndKey(t *testing.T) {
	if got := SecretEnvName("image_gen/fal", "api_key", map[string]any{}); got != "IMAGE_GEN_FAL_API_KEY" {
		t.Fatal(got)
	}
	if got := SecretEnvName("x", "k", map[string]any{"env": " CUSTOM_ENV "}); got != "CUSTOM_ENV" {
		t.Fatal(got)
	}
}
