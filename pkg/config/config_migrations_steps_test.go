package config

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

func TestStaleListGainsConnectionsForEveryPlatform(t *testing.T) {
	h := writeMigrationHome(t, t.TempDir(), map[string]any{"_config_version": 44,
		"platform_toolsets":      map[string]any{"cli": []any{"file", "terminal", "web"}, "telegram": []any{"file", "web"}},
		"known_builtin_toolsets": map[string]any{"cli": []any{"browser", "file", "memory", "skills", "terminal", "todo", "web"}}}, "")
	res := runFrom(t, h, 44)
	raw := rawMigrated(t, h)
	for _, path := range [][]string{{"platform_toolsets", "cli"}, {"platform_toolsets", "telegram"}, {"known_builtin_toolsets", "cli"}} {
		v, _ := CfgGet(raw, path...)
		if !pyContains(v.([]any), "connections") {
			t.Fatalf("%v lacks connections: %v", path, v)
		}
	}
	n := 0
	for _, e := range res.ConfigAdded {
		if strings.Contains(strings.ToLower(e), "connections") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("config added %v", res.ConfigAdded)
	}
}

func TestDeclinesCompositesAndEmptyListsAreUntouched(t *testing.T) {
	for name, tc := range map[string][2]map[string]any{
		"declined":  {{"cli": []any{"file", "terminal", "web"}}, {"cli": []any{"browser", "connections", "file", "web"}}},
		"composite": {{"cli": []any{"icebath-cli"}}, {}},
		"empty":     {{"cli": []any{}}, {}},
	} {
		cfg := map[string]any{"_config_version": 44, "platform_toolsets": tc[0]}
		if len(tc[1]) > 0 {
			cfg["known_builtin_toolsets"] = tc[1]
		}
		h := writeMigrationHome(t, t.TempDir(), cfg, "")
		res := runFrom(t, h, 44)
		if !reflect.DeepEqual(rawMigrated(t, h)["platform_toolsets"], tc[0]) {
			t.Fatalf("%s: %v", name, rawMigrated(t, h)["platform_toolsets"])
		}
		for _, e := range res.ConfigAdded {
			if strings.Contains(strings.ToLower(e), "connections") {
				t.Fatalf("%s claimed %v", name, res.ConfigAdded)
			}
		}
	}
}

func TestGlobalDisableIsNotOverriddenOrClaimed(t *testing.T) {
	h := writeMigrationHome(t, t.TempDir(), map[string]any{"_config_version": 44,
		"platform_toolsets": map[string]any{"cli": []any{"file", "terminal", "web"}},
		"agent":             map[string]any{"disabled_toolsets": []any{"connections"}}}, "")
	res := runFrom(t, h, 44)
	if pyContains(AsMap(rawMigrated(t, h)["platform_toolsets"])["cli"].([]any), "connections") {
		t.Fatal("a global disable was overridden")
	}
	if strings.Contains(strings.Join(res.ConfigAdded, "\n"), "connections") {
		t.Fatalf("claimed %v", res.ConfigAdded)
	}
}

func TestRerunAddsNothingAndKeepsOneConnections(t *testing.T) {
	h := writeMigrationHome(t, t.TempDir(), map[string]any{"_config_version": 44,
		"platform_toolsets": map[string]any{"cli": []any{"file", "web"}}}, "")
	runFrom(t, h, 44)
	res := runFrom(t, h, 44)
	list := AsMap(rawMigrated(t, h)["platform_toolsets"])["cli"].([]any)
	count := 0
	for _, v := range list {
		if v == "connections" {
			count++
		}
	}
	if count != 1 || strings.Contains(strings.Join(res.ConfigAdded, "\n"), "connections") {
		t.Fatalf("list %v added %v", list, res.ConfigAdded)
	}
}

func TestV34ResetsStaleNameScrubsRenderedTextAndKeepsManualPrompts(t *testing.T) {
	h := writeMigrationHome(t, t.TempDir(), map[string]any{"_config_version": 33, "display": map[string]any{"personality": "kawaii"},
		"agent": map[string]any{"system_prompt": "You are kawaii."}}, "")
	res := runFrom(t, h, 33)
	raw := rawMigrated(t, h)
	if v, _ := CfgGet(raw, "display", "personality"); v != "" {
		t.Fatalf("personality %v", v)
	}
	if v, _ := CfgGet(raw, "agent", "system_prompt"); v != "" {
		t.Fatalf("system prompt %v", v)
	}
	if !strings.Contains(strings.Join(res.ConfigAdded, "\n"), "personality") {
		t.Fatalf("config added %v", res.ConfigAdded)
	}
	manual := writeMigrationHome(t, t.TempDir(), map[string]any{"_config_version": 33, "display": map[string]any{"personality": "pirate"},
		"agent": map[string]any{"system_prompt": "my manual prompt"}}, "")
	runFrom(t, manual, 33)
	if v, _ := CfgGet(rawMigrated(t, manual), "agent", "system_prompt"); v != "my manual prompt" {
		t.Fatalf("manual prompt %v", v)
	}
	quiet := writeMigrationHome(t, t.TempDir(), map[string]any{"_config_version": 33}, "")
	if res := runFrom(t, quiet, 33); strings.Contains(strings.Join(res.ConfigAdded, "\n"), "personality") {
		t.Fatalf("noop claimed %v", res.ConfigAdded)
	}
	later := writeMigrationHome(t, t.TempDir(), map[string]any{"_config_version": 34, "display": map[string]any{"personality": "kawaii"}}, "")
	runFrom(t, later, 34)
	if v, _ := CfgGet(rawMigrated(t, later), "display", "personality"); v != "kawaii" {
		t.Fatalf("post v34 choice reset to %v", v)
	}
}

func TestV38RemovesOnlyLegacyRelayPluginKeys(t *testing.T) {
	h := writeMigrationHome(t, t.TempDir(), map[string]any{"_config_version": 37,
		"plugins": map[string]any{"enabled": []any{"keep-me", "observability/nemo_relay"}}}, "")
	res := runFrom(t, h, 37)
	if !reflect.DeepEqual(AsMap(rawMigrated(t, h)["plugins"])["enabled"], []any{"keep-me"}) {
		t.Fatalf("plugins %v", rawMigrated(t, h)["plugins"])
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "ICEBATH_NEMO_RELAY_PLUGINS_TOML") {
		t.Fatalf("warnings %v", res.Warnings)
	}
}

func TestMigration41StripsTheSoulOnceAndSkipsConfigOnlyRuns(t *testing.T) {
	h := writeMigrationHome(t, t.TempDir(), map[string]any{"_config_version": 40}, "")
	soul := "# Me\n\n## Messaging other agents\nroster\n"
	os.WriteFile(filepath.Join(h, "SOUL.md"), []byte("\ufeff"+soul), 0o644)
	res := &MigrationResults{}
	RunMigrations(h, 40, res, true, MigrationOptions{ConfigOnly: true}, migrationTestSources(h, nil))
	if data, _ := os.ReadFile(filepath.Join(h, "SOUL.md")); !strings.Contains(string(data), "Messaging other agents") {
		t.Fatal("a config only run rewrote SOUL.md")
	}
	res = runFrom(t, h, 40)
	if data, _ := os.ReadFile(filepath.Join(h, "SOUL.md")); string(data) != "# Me\n\n" {
		t.Fatalf("soul %q", data)
	}
	if !strings.Contains(strings.Join(res.ConfigAdded, "\n"), "removed legacy Bot Mode section from SOUL.md (default)") {
		t.Fatalf("config added %v", res.ConfigAdded)
	}
	if res = runFrom(t, h, 40); strings.Contains(strings.Join(res.ConfigAdded, "\n"), "SOUL.md") {
		t.Fatal("second pass stripped again")
	}
}

func TestV16CreatesPlatformEntriesInDocumentOrder(t *testing.T) {
	h := t.TempDir()
	os.WriteFile(home.ConfigPath(h), []byte("_config_version: 15\ndisplay:\n  tool_progress_overrides:\n    telegram: all\n    discord: off\n  platforms:\n    discord:\n      tool_progress: new\n"), 0o644)
	var out bytes.Buffer
	RunMigrations(h, 15, &MigrationResults{}, false, MigrationOptions{}, migrationTestSources(h, &out))
	platforms := AsMap(AsMap(rawMigrated(t, h)["display"])["platforms"])
	if AsMap(platforms["telegram"])["tool_progress"] != "all" || AsMap(platforms["discord"])["tool_progress"] != "new" {
		t.Fatalf("platforms %v", platforms)
	}
	if !strings.Contains(out.String(), "display.platforms: telegram=all, discord=off") {
		t.Fatalf("output %q", out.String())
	}
}

func TestEarlyStepsRewriteLegacyKeys(t *testing.T) {
	dir := t.TempDir()
	plugin := filepath.Join(dir, "plugins", "notes-helper")
	os.MkdirAll(plugin, 0o755)
	os.WriteFile(filepath.Join(plugin, "plugin.yaml"), []byte("name: notes-helper\n"), 0o644)
	os.MkdirAll(filepath.Join(dir, "plugins", "off-one"), 0o755)
	os.WriteFile(filepath.Join(dir, "plugins", "off-one", "plugin.yml"), []byte("version: 1\n"), 0o644)
	h := writeMigrationHome(t, dir, map[string]any{"_config_version": 11,
		"custom_providers": []any{
			map[string]any{"name": "My (Local) Box", "base_url": "http://localhost:8080/v1", "api_key": "no-key"},
			map[string]any{"name": "", "url": "https://API.Example.com/v1"},
		},
		"providers":     map[string]any{"my-local-box": map[string]any{"api": "http://other"}},
		"stt":           map[string]any{"model": "whisper-1", "provider": "local"},
		"compression":   map[string]any{"summary_provider": "auto", "summary_model": " fast "},
		"plugins":       map[string]any{"disabled": []any{"off-one"}},
		"memory":        map[string]any{"write_mode": " Approve "},
		"skills":        map[string]any{"write_mode": "off"},
		"delegation":    map[string]any{"max_async_children": "7", "max_concurrent_children": 5},
		"agent":         map[string]any{"verify_on_stop": "auto"},
		"model_catalog": map[string]any{"ttl_hours": 24},
	}, "LLM_MODEL=gpt\n")
	t.Setenv("LLM_MODEL", "")
	os.Unsetenv("LLM_MODEL")
	var out bytes.Buffer
	res := &MigrationResults{}
	RunMigrations(h, 11, res, false, MigrationOptions{}, migrationTestSources(h, &out))
	raw := rawMigrated(t, h)
	providers := AsMap(raw["providers"])
	if _, kept := raw["custom_providers"]; kept || AsMap(providers["my-local-box-0"])["api"] != "http://localhost:8080/v1" || AsMap(providers["api-example-com"]) == nil {
		t.Fatalf("providers %v", providers)
	}
	if _, ok := AsMap(providers["my-local-box-0"])["api_key"]; ok {
		t.Fatal("no-key api key kept")
	}
	if !reflect.DeepEqual(raw["stt"], nil) && AsMap(raw["stt"])["local"] != nil {
		t.Fatalf("a cloud model name reached local whisper: %v", raw["stt"])
	}
	if v, _ := CfgGet(raw, "auxiliary", "compression", "model"); v != "fast" {
		t.Fatalf("aux compression %v", raw["auxiliary"])
	}
	if v, _ := CfgGet(raw, "plugins", "enabled"); !reflect.DeepEqual(v, []any{"notes-helper"}) {
		t.Fatalf("plugins enabled %v", v)
	}
	if v, _ := CfgGet(raw, "memory", "write_approval"); v != true {
		t.Fatalf("memory %v", raw["memory"])
	}
	if v, _ := CfgGet(raw, "delegation", "max_concurrent_children"); v != 7 {
		t.Fatalf("delegation %v", raw["delegation"])
	}
	if v, _ := CfgGet(raw, "agent", "verify_on_stop"); v != false {
		t.Fatalf("agent %v", raw["agent"])
	}
	if v, ok := CfgGet(raw, "model_catalog", "ttl_hours"); ok {
		t.Fatalf("ttl_hours %v survived the 24 to 1 to unset ladder", v)
	}
	if env := testDotenv(h); env["LLM_MODEL"] != "" {
		t.Fatalf("LLM_MODEL kept %q", env["LLM_MODEL"])
	}
	if info, err := os.Stat(filepath.Join(h, "logs", "curator")); err != nil || !info.IsDir() {
		t.Fatal("curator log directory missing")
	}
	for _, want := range []string{"Migrated 2 custom provider(s) to providers: section", "    \u2192 my-local-box-0: http://localhost:8080/v1",
		"Migrated compression.summary_* \u2192 auxiliary.compression: model= fast ", "grandfathered 1 existing plugin(s)", "Cleared LLM_MODEL from .env",
		"\u2713 Curator settings now available (enabled, interval_hours, min_idle_hours"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("warnings %v", res.Warnings)
	}
}

func TestPythonParityHelpers(t *testing.T) {
	for raw, want := range map[string]string{"http://Host.Example:8080/v1": "host.example", "localhost:8080": "", "https://[::1]:9/x": "::1", "//a.b@C.d": "c.d"} {
		if got, err := pyURLHostname(raw); err != nil || got != want {
			t.Fatalf("%s -> %q %v", raw, got, err)
		}
	}
	for _, bad := range []string{"http://[::1", "http://[1.2.3.4]/", "http://x[::1]/"} {
		if _, err := pyURLHostname(bad); err == nil {
			t.Fatalf("%s parsed", bad)
		}
	}
	if !pyEq(true, 1) || !pyEq(24.0, 24) || pyEq("1", 1) || !pyEq([]any{1, true}, []any{1.0, 1}) {
		t.Fatal("python equality")
	}
	if n, ok, _ := pyIntOf(" 1_0 "); !ok || n != 10 {
		t.Fatal("python int of a string")
	}
	if _, ok, _ := pyIntOf("3.5"); ok {
		t.Fatal("python int accepted a decimal string")
	}
	defaults := Defaults()
	for _, tc := range []struct {
		order []string
		m     map[string]any
	}{{curatorDefaultOrder, AsMap(defaults["curator"])}, {auxCuratorDefaultOrder, AsMap(AsMap(defaults["auxiliary"])["curator"])}} {
		if len(tc.order) != len(tc.m) {
			t.Fatalf("order %v does not cover %v", tc.order, SortedKeys(tc.m))
		}
		for _, k := range tc.order {
			if _, ok := tc.m[k]; !ok {
				t.Fatalf("order names %s the defaults lack", k)
			}
		}
	}
	for raw, want := range map[string]string{"ok\xff": "byte 0xff in position 2: invalid start byte", "a\xe2\x28\xa1": "byte 0xe2 in position 1: invalid continuation byte",
		"ab\xe2\x82": "bytes in position 2-3: unexpected end of data"} {
		if err := pyUTF8DecodeError([]byte(raw)); err == nil || !strings.HasSuffix(err.Error(), want) {
			t.Fatalf("%q -> %v", raw, err)
		}
	}
	if !strings.Contains(SupportFloorMessage("/x/home"), "set _config_version: 12") {
		t.Fatal(SupportFloorMessage("/x/home"))
	}
}

func TestOSErrorTextFollowsPython(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	os.WriteFile(file, nil, 0o644)
	got := pyOSErrorString(os.MkdirAll(filepath.Join(file, "x"), 0o777))
	if got != "[Errno 20] Not a directory: '"+filepath.Join(file, "x")+"'" && got != "[Errno 20] Not a directory: '"+file+"'" {
		t.Fatalf("got %q", got)
	}
}
