package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
)

// Temp home wired to the production config command collaborators with every facade stream captured
type configHome struct {
	t           *testing.T
	h           string
	out, errOut *bytes.Buffer
}

func newConfigHome(t *testing.T) *configHome {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv(config.EnvManagedDir, filepath.Join(t.TempDir(), "no-managed"))
	config.InvalidateManagedCache()
	config.ResetCache()
	config.ResetConfigParseFailures()
	t.Cleanup(func() { config.ResetCache(); config.ResetConfigParseFailures() })
	if err := os.WriteFile(filepath.Join(h, ".env"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	c := &configHome{t: t, h: h, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
	prevOut, prevErr, prevRefusal, prevParse := config.FacadeStdout, config.FacadeNoticeOut, config.EnvRefusalOut, config.ParseFailureOut
	config.FacadeStdout, config.FacadeNoticeOut, config.EnvRefusalOut, config.ParseFailureOut = c.out, c.errOut, c.errOut, c.errOut
	t.Cleanup(func() {
		config.FacadeStdout, config.FacadeNoticeOut, config.EnvRefusalOut, config.ParseFailureOut = prevOut, prevErr, prevRefusal, prevParse
	})
	return c
}

// Restores each named process env var after the test, the writers under test publishing into it
func (c *configHome) isolateEnv(names ...string) {
	for _, n := range names {
		prev, had := os.LookupEnv(n)
		c.t.Cleanup(func() {
			if had {
				os.Setenv(n, prev)
			} else {
				os.Unsetenv(n)
			}
		})
	}
}

func (c *configHome) set(key, value string, force bool) error {
	return config.SetConfigValue(context.Background(), c.h, key, value, force, configCommandSources(c.h))
}

func (c *configHome) mustSet(key, value string, force bool) {
	c.t.Helper()
	if err := c.set(key, value, force); err != nil {
		c.t.Fatalf("set %s: %v (stderr %q)", key, err, c.errOut.String())
	}
}

func (c *configHome) command(args config.ConfigArgs) error {
	return config.ConfigCommand(context.Background(), c.h, args, configCommandSources(c.h))
}

func (c *configHome) write(text string) {
	c.t.Helper()
	if err := os.WriteFile(filepath.Join(c.h, "config.yaml"), []byte(text), 0o600); err != nil {
		c.t.Fatal(err)
	}
}

func (c *configHome) writeDoc(doc map[string]any) {
	c.t.Helper()
	raw, err := config.SafeDump(doc)
	if err != nil {
		c.t.Fatal(err)
	}
	c.write(string(raw))
}

func (c *configHome) readConfig() string {
	raw, _ := os.ReadFile(filepath.Join(c.h, "config.yaml"))
	return string(raw)
}

func (c *configHome) readEnv() string {
	raw, _ := os.ReadFile(filepath.Join(c.h, ".env"))
	return string(raw)
}

func (c *configHome) parsed() map[string]any {
	c.t.Helper()
	doc, err := config.ReadDocument(filepath.Join(c.h, "config.yaml"))
	if err != nil {
		c.t.Fatal(err)
	}
	return doc
}

func (c *configHome) at(keys ...string) any {
	v, _ := config.CfgGet(c.parsed(), keys...)
	return v
}

// Takes and clears the captured stdout and stderr
func (c *configHome) drain() (string, string) {
	out, errOut := c.out.String(), c.errOut.String()
	c.out.Reset()
	c.errOut.Reset()
	return out, errOut
}

func exitStatus(err error) int {
	var exit *config.ConfigExit
	if errors.As(err, &exit) {
		return exit.Code
	}
	return -1
}

func strPtr(s string) *string { return &s }

func TestExplicitAllowlist(t *testing.T) {
	t.Run("test_explicit_key_routes_to_env", func(t *testing.T) {
		for _, key := range []string{"FAL_KEY", "SUDO_PASSWORD", "API_SERVER_KEY"} {
			c := newConfigHome(t)
			c.isolateEnv(key)
			c.mustSet(key, "test-value-123", false)
			if !strings.Contains(c.readEnv(), key+"=test-value-123") || strings.Contains(c.readConfig(), key) {
				t.Fatalf("%s env %q config %q", key, c.readEnv(), c.readConfig())
			}
		}
	})
}

func TestCatchAllPatterns(t *testing.T) {
	t.Run("test_api_key_suffix_routes_to_env", func(t *testing.T) {
		for _, key := range []string{"SOME_FUTURE_SERVICE_API_KEY", "MY_CUSTOM_TOKEN", "CLIENT_SECRET"} {
			c := newConfigHome(t)
			c.isolateEnv(key)
			c.mustSet(key, "secret-456", false)
			if !strings.Contains(c.readEnv(), key+"=secret-456") || strings.Contains(c.readConfig(), key) {
				t.Fatalf("%s env %q config %q", key, c.readEnv(), c.readConfig())
			}
		}
	})
}

func TestGatewayPlatformsPrefixRedirect(t *testing.T) {
	t.Run("test_set_lands_on_top_level_platforms_block_the_loader_reads", func(t *testing.T) {
		c := newConfigHome(t)
		c.isolateEnv("TELEGRAM_BOT_TOKEN")
		c.write("platforms:\n  telegram:\n    enabled: false\n")
		c.mustSet("gateway.platforms.telegram.enabled", "true", false)
		if out, _ := c.drain(); !strings.Contains(out, "saved as platforms.telegram.enabled") {
			t.Fatalf("stdout %q", out)
		}
		loaded := c.parsed()
		if _, has := loaded["gateway"]; has || c.at("platforms", "telegram", "enabled") != true {
			t.Fatalf("saved %v", loaded)
		}
		gc, err := gateway.LoadGatewayConfig(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if p := gc.Platforms[gateway.PlatformTelegram]; p == nil || !p.Enabled {
			t.Fatalf("gateway loader %#v", p)
		}
	})
	t.Run("test_get_and_unset_still_reach_a_legacy_nested_only_value", func(t *testing.T) {
		c := newConfigHome(t)
		legacy := "gateway:\n  platforms:\n    telegram:\n      enabled: true\n"
		c.write(legacy)
		if err := config.GetConfigValue(context.Background(), c.h, "gateway.platforms.telegram.enabled", false, false, configCommandSources(c.h)); err != nil {
			t.Fatal(err)
		}
		if out, _ := c.drain(); strings.ToLower(strings.TrimSpace(out)) != "true" {
			t.Fatalf("get %q", out)
		}
		if err := config.UnsetConfigValue(context.Background(), c.h, "gateway.platforms.telegram.enabled", configCommandSources(c.h)); err != nil {
			t.Fatal(err)
		}
		if _, has := c.parsed()["gateway"]; has {
			t.Fatalf("unset left %v", c.parsed())
		}
		c.write(legacy)
		c.mustSet("gateway.platforms.telegram.enabled", "false", false)
		if got := c.parsed(); !reflect.DeepEqual(got, map[string]any{"platforms": map[string]any{"telegram": map[string]any{"enabled": false}}}) {
			t.Fatalf("set over nested only %v", got)
		}
	})
}

func TestConfigYamlRouting(t *testing.T) {
	t.Run("test_simple_key", func(t *testing.T) {
		c := newConfigHome(t)
		c.mustSet("model", "gpt-4o", false)
		if !strings.Contains(c.readConfig(), "gpt-4o") || strings.Contains(c.readEnv(), "model") {
			t.Fatalf("config %q env %q", c.readConfig(), c.readEnv())
		}
	})
	t.Run("test_tool_search_defer_is_recognized", func(t *testing.T) {
		c := newConfigHome(t)
		c.mustSet("tools.tool_search.defer", `["todo_list", "skill_manage"]`, false)
		out, errOut := c.drain()
		if strings.Contains(out, "not a recognized config key") || strings.Contains(errOut, "not a recognized config key") {
			t.Fatalf("flagged %q %q", out, errOut)
		}
		if got := c.at("tools", "tool_search", "defer"); !reflect.DeepEqual(got, []any{"todo_list", "skill_manage"}) {
			t.Fatalf("saved %#v", got)
		}
	})
	t.Run("test_terminal_docker_shared_key_preserves_string_values", func(t *testing.T) {
		c := newConfigHome(t)
		c.isolateEnv("TERMINAL_DOCKER_SHARED_CONTAINER_KEY")
		c.mustSet("terminal.docker_shared_container_key", "off", false)
		if c.at("terminal", "docker_shared_container_key") != "off" || !strings.Contains(c.readEnv(), "TERMINAL_DOCKER_SHARED_CONTAINER_KEY=off") {
			t.Fatalf("config %q env %q", c.readConfig(), c.readEnv())
		}
		if out, _ := c.drain(); strings.Contains(out, "not a recognized config key") {
			t.Fatalf("flagged %q", out)
		}
	})
}

func TestFalsyValues(t *testing.T) {
	t.Run("test_config_command_rejects_missing_value", func(t *testing.T) {
		c := newConfigHome(t)
		if err := c.command(config.ConfigArgs{Command: "set", Key: "model"}); exitStatus(err) != 1 {
			t.Fatalf("missing value %v", err)
		}
	})
	t.Run("test_config_command_accepts_empty_string", func(t *testing.T) {
		c := newConfigHome(t)
		if err := c.command(config.ConfigArgs{Command: "set", Key: "model", Value: strPtr("")}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(c.readConfig(), "model") {
			t.Fatalf("config %q", c.readConfig())
		}
	})
}

func TestConfigGetUnset(t *testing.T) {
	t.Run("test_config_get_prints_resolved_nested_value", func(t *testing.T) {
		c := newConfigHome(t)
		c.isolateEnv("TERMINAL_TIMEOUT")
		c.mustSet("terminal.timeout", "120", false)
		c.drain()
		if err := c.command(config.ConfigArgs{Command: "get", Key: "terminal.timeout"}); err != nil {
			t.Fatal(err)
		}
		if out, _ := c.drain(); strings.TrimSpace(out) != "120" {
			t.Fatalf("get %q", out)
		}
	})
	t.Run("test_config_unset_removes_yaml_key_and_synced_env", func(t *testing.T) {
		c := newConfigHome(t)
		c.isolateEnv("TERMINAL_ENV")
		c.mustSet("terminal.backend", "docker", false)
		if !strings.Contains(c.readEnv(), "TERMINAL_ENV=docker") {
			t.Fatalf("env %q", c.readEnv())
		}
		c.drain()
		if err := c.command(config.ConfigArgs{Command: "unset", Key: "terminal.backend"}); err != nil {
			t.Fatal(err)
		}
		if len(c.parsed()) != 0 || strings.Contains(c.readEnv(), "TERMINAL_ENV=") {
			t.Fatalf("config %q env %q", c.readConfig(), c.readEnv())
		}
		if out, _ := c.drain(); !strings.Contains(out, "Unset terminal.backend") {
			t.Fatalf("stdout %q", out)
		}
	})
	t.Run("test_config_unset_removes_dotted_token_yaml_key", func(t *testing.T) {
		c := newConfigHome(t)
		c.write("platforms:\n  teams:\n    extra:\n      access_token: yaml-token\n      tenant_id: tenant\n")
		if err := c.command(config.ConfigArgs{Command: "unset", Key: "platforms.teams.extra.access_token"}); err != nil {
			t.Fatal(err)
		}
		extra, _ := c.at("platforms", "teams", "extra").(map[string]any)
		if _, has := extra["access_token"]; has || extra["tenant_id"] != "tenant" {
			t.Fatalf("extra %v", extra)
		}
		if out, _ := c.drain(); !strings.Contains(out, "Unset platforms.teams.extra.access_token") {
			t.Fatalf("stdout %q", out)
		}
	})
}

func TestConfigGetPhantomKeyNotice(t *testing.T) {
	t.Run("test_unknown_nested_key_flags_on_stderr_and_keeps_stdout_parseable", func(t *testing.T) {
		c := newConfigHome(t)
		c.write("compression:\n  compressor:\n    enabled: true\n")
		if err := c.command(config.ConfigArgs{Command: "get", Key: "compression.compressor.enabled", JSON: true}); err != nil {
			t.Fatal(err)
		}
		out, errOut := c.drain()
		var parsed any
		if err := json.Unmarshal([]byte(out), &parsed); err != nil || parsed != true {
			t.Fatalf("stdout %q %v", out, err)
		}
		if !strings.Contains(errOut, "not a recognized config key") {
			t.Fatalf("stderr %q", errOut)
		}
	})
	t.Run("test_unseeded_live_key_notice_hedges_instead_of_asserting_unread", func(t *testing.T) {
		c := newConfigHome(t)
		c.write("gateway:\n  proxy_url: http://127.0.0.1:1\n")
		if err := c.command(config.ConfigArgs{Command: "get", Key: "gateway.proxy_url"}); err != nil {
			t.Fatal(err)
		}
		out, errOut := c.drain()
		if strings.TrimSpace(out) != "http://127.0.0.1:1" || !strings.Contains(errOut, "may not read it") || strings.Contains(errOut, "does not read it") {
			t.Fatalf("stdout %q stderr %q", out, errOut)
		}
	})
	t.Run("test_recognized_and_custom_keys_are_not_flagged", func(t *testing.T) {
		for _, tc := range [][2]string{
			{"terminal.timeout", "terminal:\n  timeout: 120\n"},
			{"my_custom_setting", "my_custom_setting: hello\n"},
			{"mcp_servers.local.url", "mcp_servers:\n  local:\n    url: http://127.0.0.1:1\n"},
		} {
			c := newConfigHome(t)
			c.write(tc[1])
			if err := c.command(config.ConfigArgs{Command: "get", Key: tc[0]}); err != nil {
				t.Fatal(err)
			}
			out, errOut := c.drain()
			if strings.TrimSpace(out) == "" || strings.Contains(errOut, "not a recognized config key") {
				t.Fatalf("%s stdout %q stderr %q", tc[0], out, errOut)
			}
		}
	})
}

var _ = time.Second

func TestListNavigation(t *testing.T) {
	t.Run("test_indexed_set_preserves_sibling_list_entries", func(t *testing.T) {
		c := newConfigHome(t)
		c.write("custom_providers:\n- name: provider-a\n  api_key: old-a\n  base_url: https://a.example.com\n- name: provider-b\n  api_key: old-b\n  base_url: https://b.example.com\n")
		c.mustSet("custom_providers.0.api_key", "new-a", false)
		list, isList := c.parsed()["custom_providers"].([]any)
		if !isList || len(list) != 2 {
			t.Fatalf("list %v", c.parsed())
		}
		want := []any{
			map[string]any{"name": "provider-a", "api_key": "new-a", "base_url": "https://a.example.com"},
			map[string]any{"name": "provider-b", "api_key": "old-b", "base_url": "https://b.example.com"},
		}
		if !reflect.DeepEqual(list, want) {
			t.Fatalf("entries %v", list)
		}
	})
	t.Run("test_indexed_set_preserves_non_targeted_fields", func(t *testing.T) {
		c := newConfigHome(t)
		c.write("custom_providers:\n- name: provider-a\n  api_key: old\n  base_url: https://a.example.com\n  models:\n    foo: {}\n    bar: {}\n")
		c.mustSet("custom_providers.0.api_key", "rotated", false)
		entry := c.parsed()["custom_providers"].([]any)[0].(map[string]any)
		models, _ := entry["models"].(map[string]any)
		_, foo := models["foo"]
		_, bar := models["bar"]
		if entry["api_key"] != "rotated" || entry["name"] != "provider-a" || entry["base_url"] != "https://a.example.com" || len(models) != 2 || !foo || !bar {
			t.Fatalf("entry %v", entry)
		}
	})
	t.Run("test_deeper_nesting_through_list", func(t *testing.T) {
		c := newConfigHome(t)
		c.write("telegram:\n  allowlist:\n    - name: alice\n      role: admin\n    - name: bob\n      role: user\n")
		c.mustSet("telegram.allowlist.1.role", "admin", false)
		list, isList := c.at("telegram", "allowlist").([]any)
		if !isList || !reflect.DeepEqual(list, []any{map[string]any{"name": "alice", "role": "admin"}, map[string]any{"name": "bob", "role": "admin"}}) {
			t.Fatalf("allowlist %v", c.at("telegram", "allowlist"))
		}
	})
}

func TestStringTypedConfigValues(t *testing.T) {
	t.Run("test_string_typed_values_are_not_coerced", func(t *testing.T) {
		for _, value := range []string{"off", "true", "01"} {
			c := newConfigHome(t)
			c.mustSet("approvals.mode", value, false)
			if got, isStr := c.at("approvals", "mode").(string); !isStr || got != value {
				t.Fatalf("%q saved %#v", value, c.at("approvals", "mode"))
			}
		}
	})
	t.Run("test_non_string_defaults_keep_existing_coercion", func(t *testing.T) {
		for _, tc := range []struct {
			key, value string
			want       any
		}{{"terminal.persistent_shell", "off", false}, {"approvals.timeout", "30", 30}} {
			c := newConfigHome(t)
			c.isolateEnv("TERMINAL_PERSISTENT_SHELL")
			c.mustSet(tc.key, tc.value, false)
			got := c.at(strings.Split(tc.key, ".")...)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("%s saved %#v want %#v", tc.key, got, tc.want)
			}
		}
	})
	t.Run("test_unknown_keys_keep_existing_coercion", func(t *testing.T) {
		c := newConfigHome(t)
		c.mustSet("custom.enabled", "off", true)
		if c.at("custom", "enabled") != false {
			t.Fatalf("saved %#v", c.at("custom", "enabled"))
		}
	})
}

func TestSecretRedactionInDisplay(t *testing.T) {
	t.Run("test_set_echo_masks_secret_value", func(t *testing.T) {
		c := newConfigHome(t)
		secret := "cfut_ANOTHERSECRET0987654321zyxwvu"
		c.mustSet("model.api_key", secret, false)
		if out, _ := c.drain(); strings.Contains(out, secret) || !strings.Contains(out, "Set model.api_key") {
			t.Fatalf("stdout %q", out)
		}
	})
}

func TestSchemaValidation(t *testing.T) {
	t.Run("test_unknown_subkey_under_known_section_refused_before_write", func(t *testing.T) {
		for _, tc := range [][2]string{
			{"gateway.discord.gateway_restart_notification", "discord.gateway_restart_notification"},
			{"agent.gateway.strict", "gateway.strict"},
		} {
			c := newConfigHome(t)
			c.write("model: gpt-4o\n")
			if err := c.set(tc[0], "true", false); exitStatus(err) != 1 {
				t.Fatalf("%s not refused %v", tc[0], err)
			}
			_, errOut := c.drain()
			if c.readConfig() != "model: gpt-4o\n" || !strings.Contains(errOut, "nothing was written") || !strings.Contains(errOut, "Did you mean: "+tc[1]) {
				t.Fatalf("%s config %q stderr %q", tc[0], c.readConfig(), errOut)
			}
		}
	})
	t.Run("test_unknown_leaf_under_known_section_is_written_with_notice", func(t *testing.T) {
		for _, tc := range []struct {
			key, value string
			want       any
			suggestion string
		}{
			{"gateway.proxy_url", "http://127.0.0.1:1", "http://127.0.0.1:1", ""},
			{"agent.max_turnz", "50", 50, "agent.max_turns"},
			{"gateway.filter_silence_narration", "false", false, ""},
		} {
			c := newConfigHome(t)
			c.mustSet(tc.key, tc.value, false)
			if got := c.at(strings.Split(tc.key, ".")...); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("%s saved %#v", tc.key, got)
			}
			out, _ := c.drain()
			if !strings.Contains(out, "not a recognized config key") || strings.Contains(out, "bridged to the environment") || !strings.Contains(out, "Use --force") {
				t.Fatalf("%s stdout %q", tc.key, out)
			}
			if (tc.suggestion == "") == strings.Contains(out, "Did you mean") || (tc.suggestion != "" && !strings.Contains(out, "Did you mean: "+tc.suggestion)) {
				t.Fatalf("%s suggestion in %q", tc.key, out)
			}
		}
	})
	t.Run("test_unknown_top_level_key_still_written_with_notice", func(t *testing.T) {
		c := newConfigHome(t)
		c.isolateEnv("BRAND_NEW_FUTURE_KEY")
		c.mustSet("brand_new_future_key", "value", false)
		if out, _ := c.drain(); !strings.Contains(c.readConfig(), "brand_new_future_key") || !strings.Contains(out, "not a recognized config key") {
			t.Fatalf("config %q stdout %q", c.readConfig(), out)
		}
	})
	t.Run("test_force_suppresses_notice", func(t *testing.T) {
		c := newConfigHome(t)
		c.isolateEnv("BRAND_NEW_FUTURE_KEY")
		c.mustSet("brand_new_future_key", "value", true)
		if out, _ := c.drain(); strings.Contains(out, "not a recognized config key") || !strings.Contains(c.readConfig(), "brand_new_future_key") {
			t.Fatalf("config %q stdout %q", c.readConfig(), out)
		}
	})
}

func TestDisplaySkinTouch(t *testing.T) {
	old := time.Unix(1_000_000_000, 0)
	t.Run("test_reaffirming_same_skin_moves_the_watcher_signature", func(t *testing.T) {
		c := newConfigHome(t)
		skin := filepath.Join(c.h, "skins", "synthwave.yaml")
		os.MkdirAll(filepath.Dir(skin), 0o755)
		os.WriteFile(skin, []byte("name: synthwave\ncolors:\n  background: '#1a1030'\n"), 0o644)
		os.Chtimes(skin, old, old)
		c.mustSet("display.skin", "synthwave", false)
		if st, _ := os.Stat(skin); !st.ModTime().After(old) {
			t.Fatal("first set left the mtime")
		}
		os.Chtimes(skin, old, old)
		c.mustSet("display.skin", "synthwave", false)
		if st, _ := os.Stat(skin); !st.ModTime().After(old) {
			t.Fatal("re-affirmed set left the mtime")
		}
	})
	t.Run("test_builtin_or_missing_skin_file_is_fine", func(t *testing.T) {
		c := newConfigHome(t)
		c.mustSet("display.skin", "mono", false)
		if !strings.Contains(c.readConfig(), "skin: mono") {
			t.Fatalf("config %q", c.readConfig())
		}
	})
	t.Run("test_touch_preserves_skin_file_contents", func(t *testing.T) {
		c := newConfigHome(t)
		skin := filepath.Join(c.h, "skins", "neon.yaml")
		os.MkdirAll(filepath.Dir(skin), 0o755)
		body := "name: neon\ncolors:\n  ui_accent: '#ff33aa'\n"
		os.WriteFile(skin, []byte(body), 0o644)
		c.mustSet("display.skin", "neon", false)
		if got, _ := os.ReadFile(skin); string(got) != body {
			t.Fatalf("skin %q", got)
		}
	})
}

func TestMappingGuard(t *testing.T) {
	t.Run("test_bare_model_shorthand_preserves_siblings", func(t *testing.T) {
		c := newConfigHome(t)
		c.writeDoc(map[string]any{"model": map[string]any{"default": "gpt-4o", "provider": "openai-api", "context_length": 128000, "base_url": "https://api.example.com/v1"}})
		c.mustSet("model", "claude-sonnet-4-20250514", false)
		m, _ := c.parsed()["model"].(map[string]any)
		if m["default"] != "claude-sonnet-4-20250514" || m["provider"] != "openai-api" || !reflect.DeepEqual(m["context_length"], 128000) || m["base_url"] != "https://api.example.com/v1" {
			t.Fatalf("model %v", m)
		}
	})
	t.Run("test_bare_model_shorthand_creates_default_when_none", func(t *testing.T) {
		c := newConfigHome(t)
		c.mustSet("model", "gpt-5.6-sol", false)
		if !strings.Contains(c.readConfig(), "gpt-5.6-sol") {
			t.Fatalf("config %q", c.readConfig())
		}
	})
	t.Run("test_bare_model_container_literal_is_refused", func(t *testing.T) {
		for _, tc := range []struct {
			start   map[string]any
			literal string
		}{
			{map[string]any{"model": map[string]any{"default": "glm-5.3-flash", "provider": "zai"}}, `{"provider": "openai-codex", "default": "gpt-6.1-sol"}`},
			{map[string]any{}, "[a, b]"},
			{map[string]any{"model": "gpt-4o"}, "[a, b]"},
		} {
			c := newConfigHome(t)
			c.writeDoc(tc.start)
			if err := c.set("model", tc.literal, false); exitStatus(err) != 1 {
				t.Fatalf("%q not refused %v", tc.literal, err)
			}
			if got := c.parsed(); !reflect.DeepEqual(got, tc.start) {
				t.Fatalf("%q changed the config to %v", tc.literal, got)
			}
		}
	})
	t.Run("test_bare_model_mapping_force_replaces_section", func(t *testing.T) {
		c := newConfigHome(t)
		c.writeDoc(map[string]any{"model": map[string]any{"default": "gpt-4o", "provider": "openai-api", "context_length": 128000}})
		c.mustSet("model", `{"provider": "openrouter", "default": "z-ai/glm-5.3-flash"}`, true)
		if got := c.parsed()["model"]; !reflect.DeepEqual(got, map[string]any{"provider": "openrouter", "default": "z-ai/glm-5.3-flash"}) {
			t.Fatalf("model %v", got)
		}
	})
	t.Run("test_non_model_mapping_is_refused", func(t *testing.T) {
		c := newConfigHome(t)
		c.writeDoc(map[string]any{"terminal": map[string]any{"backend": "docker", "docker_image": "python:3.12", "shell": "bash"}})
		if err := c.set("terminal", "zsh", false); exitStatus(err) != 1 {
			t.Fatalf("not refused %v", err)
		}
	})
	t.Run("test_non_model_mapping_force_overwrites", func(t *testing.T) {
		c := newConfigHome(t)
		c.writeDoc(map[string]any{"terminal": map[string]any{"backend": "docker", "shell": "bash"}})
		c.mustSet("terminal", "zsh", true)
		if got := c.parsed()["terminal"]; got != "zsh" {
			t.Fatalf("terminal %v", got)
		}
	})
	t.Run("test_model_default_dotted_path_is_not_guarded", func(t *testing.T) {
		c := newConfigHome(t)
		c.writeDoc(map[string]any{"model": map[string]any{"default": "gpt-4o", "provider": "openai-api"}})
		c.mustSet("model.default", "claude-opus-4", false)
		if m := c.parsed()["model"].(map[string]any); m["default"] != "claude-opus-4" || m["provider"] != "openai-api" {
			t.Fatalf("model %v", m)
		}
	})
	t.Run("test_model_force_overwrites_entire_section", func(t *testing.T) {
		c := newConfigHome(t)
		c.writeDoc(map[string]any{"model": map[string]any{"default": "gpt-4o", "provider": "openai-api", "context_length": 128000}})
		c.mustSet("model", "claude-opus-4", true)
		if got := c.parsed()["model"]; got != "claude-opus-4" {
			t.Fatalf("model %v", got)
		}
	})
}

func TestScalarModelSubKeyPreservation(t *testing.T) {
	t.Run("test_scalar_model_id_preserved_after_provider_write", func(t *testing.T) {
		c := newConfigHome(t)
		c.mustSet("model", "gpt-4o", false)
		c.mustSet("model.provider", "openai", false)
		if m := c.parsed()["model"].(map[string]any); m["default"] != "gpt-4o" || m["provider"] != "openai" {
			t.Fatalf("model.default lost: %v", m)
		}
	})
	t.Run("test_scalar_model_id_preserved_after_api_key_write", func(t *testing.T) {
		c := newConfigHome(t)
		c.mustSet("model", "claude-sonnet", false)
		c.mustSet("model.api_key", "sk-test", false)
		if m := c.parsed()["model"].(map[string]any); m["default"] != "claude-sonnet" || m["api_key"] != "sk-test" {
			t.Fatalf("model %v", m)
		}
	})
}

func TestMalformedYAMLConfigPreservation(t *testing.T) {
	const broken = "model: gpt-4o\nterminal:\n  backend: docker\n  broken: [this is invalid YAML"
	check := func(t *testing.T, c *configHome, err error) {
		t.Helper()
		var rt *config.RuntimeError
		if !errors.As(err, &rt) || !strings.Contains(rt.Msg, "formatting error") {
			t.Fatalf("want a RuntimeError naming the formatting error, got %#v", err)
		}
		out, errOut := c.drain()
		if combined := out + errOut; !strings.Contains(combined, "formatting error") || !strings.Contains(combined, "`icebath config edit`") {
			t.Fatalf("output %q", combined)
		}
		if c.readConfig() != broken {
			t.Fatalf("Config was overwritten:\n%s", c.readConfig())
		}
	}
	t.Run("test_set_config_value_refuses_broken_yaml", func(t *testing.T) {
		c := newConfigHome(t)
		c.write(broken)
		check(t, c, c.set("agent.max_turns", "50", false))
	})
	t.Run("test_unset_config_value_refuses_broken_yaml", func(t *testing.T) {
		c := newConfigHome(t)
		c.write(broken)
		check(t, c, config.UnsetConfigValue(context.Background(), c.h, "model", configCommandSources(c.h)))
	})
}

func TestLiteralDotKeyEscaping(t *testing.T) {
	providers := func(c *configHome) {
		c.write("providers:\n  qwen3.5-397b-wafer-non-zdr:\n    api: https://pass.wafer.ai/v1\n  openrouter:\n    api_key: or-keep\n")
	}
	t.Run("test_set_preserves_literal_dot_in_provider_key", func(t *testing.T) {
		c := newConfigHome(t)
		providers(c)
		c.mustSet(`providers.qwen3\.5-397b-wafer-non-zdr.extra_headers`, `{"Wafer-ZDR": "required"}`, false)
		saved := c.parsed()["providers"].(map[string]any)
		target, _ := saved["qwen3.5-397b-wafer-non-zdr"].(map[string]any)
		if _, has := saved["qwen3"]; has || target["api"] != "https://pass.wafer.ai/v1" ||
			!reflect.DeepEqual(target["extra_headers"], map[string]any{"Wafer-ZDR": "required"}) ||
			!reflect.DeepEqual(saved["openrouter"], map[string]any{"api_key": "or-keep"}) {
			t.Fatalf("providers %v", saved)
		}
		if out, _ := c.drain(); strings.Contains(out, "not a recognized config key") {
			t.Fatalf("stdout %q", out)
		}
	})
	t.Run("test_unset_removes_literal_dot_provider_key", func(t *testing.T) {
		c := newConfigHome(t)
		providers(c)
		if err := c.command(config.ConfigArgs{Command: "unset", Key: `providers.qwen3\.5-397b-wafer-non-zdr`}); err != nil {
			t.Fatal(err)
		}
		saved := c.parsed()["providers"].(map[string]any)
		if _, has := saved["qwen3.5-397b-wafer-non-zdr"]; has || !reflect.DeepEqual(saved["openrouter"], map[string]any{"api_key": "or-keep"}) {
			t.Fatalf("providers %v", saved)
		}
		if out, _ := c.drain(); !strings.Contains(out, `Unset providers.qwen3\.5-397b-wafer-non-zdr`) {
			t.Fatalf("stdout %q", out)
		}
	})
	t.Run("test_unset_nested_field_under_literal_dot_key", func(t *testing.T) {
		c := newConfigHome(t)
		c.write("providers:\n  qwen3.5-397b-wafer-non-zdr:\n    api: https://pass.wafer.ai/v1\n    extra_headers: '{\"K\": \"V\"}'\n")
		if err := c.command(config.ConfigArgs{Command: "unset", Key: `providers.qwen3\.5-397b-wafer-non-zdr.extra_headers`}); err != nil {
			t.Fatal(err)
		}
		target := c.parsed()["providers"].(map[string]any)["qwen3.5-397b-wafer-non-zdr"].(map[string]any)
		if _, has := target["extra_headers"]; has || target["api"] != "https://pass.wafer.ai/v1" {
			t.Fatalf("target %v", target)
		}
	})
	t.Run("test_get_reads_literal_dot_provider_key", func(t *testing.T) {
		c := newConfigHome(t)
		c.write("providers:\n  qwen3.5-397b:\n    api: https://pass.wafer.ai/v1\n")
		if err := c.command(config.ConfigArgs{Command: "get", Key: `providers.qwen3\.5-397b.api`}); err != nil {
			t.Fatal(err)
		}
		if out, _ := c.drain(); strings.TrimSpace(out) != "https://pass.wafer.ai/v1" {
			t.Fatalf("stdout %q", out)
		}
	})
	t.Run("test_unescaped_dotted_path_unchanged", func(t *testing.T) {
		c := newConfigHome(t)
		c.isolateEnv("TERMINAL_ENV")
		c.mustSet("terminal.backend", "docker", false)
		if c.at("terminal", "backend") != "docker" {
			t.Fatalf("config %q", c.readConfig())
		}
	})
}

const getSecret = "OPAQUEKEYVALUE12345678"

func TestConfigGetRedaction(t *testing.T) {
	seed := func(c *configHome) {
		c.write("providers:\n  gemini:\n    api_key: " + getSecret + "\nmcp_servers:\n  s:\n    env:\n      MY_API_KEY: ${MY_API_KEY}\n    url: https://x.example\n")
		os.WriteFile(filepath.Join(c.h, ".env"), []byte("GEMINI_API_KEY="+getSecret+"\n"), 0o600)
		c.t.Setenv("MY_API_KEY", getSecret)
	}
	get := func(c *configHome, key string, raw bool) string {
		c.t.Helper()
		if err := config.GetConfigValue(context.Background(), c.h, key, false, raw, configCommandSources(c.h)); err != nil {
			c.t.Fatal(err)
		}
		out, _ := c.drain()
		return out
	}
	t.Run("test_config_get_masks_every_credential_path", func(t *testing.T) {
		for _, key := range []string{"providers", "providers.gemini.api_key", "GEMINI_API_KEY", "mcp_servers.s.env.MY_API_KEY"} {
			c := newConfigHome(t)
			c.isolateEnv("GEMINI_API_KEY")
			seed(c)
			out := get(c, key, false)
			if strings.Contains(out, getSecret) || !strings.Contains(out, getSecret[:4]) || (key == "providers" && !strings.Contains(out, "gemini")) {
				t.Fatalf("%s printed %q", key, out)
			}
		}
	})
	t.Run("test_config_get_raw_prints_the_real_value", func(t *testing.T) {
		c := newConfigHome(t)
		seed(c)
		if out := get(c, "providers.gemini.api_key", true); strings.TrimSpace(out) != getSecret {
			t.Fatalf("raw %q", out)
		}
	})
	t.Run("test_config_get_classifies_env_header_and_enum_keys", func(t *testing.T) {
		for _, tc := range []struct {
			key, envLine, yamlLine string
			masked                 bool
			plain                  string
		}{
			{"FAL_KEY", "FAL_KEY=" + getSecret, "", true, ""},
			{"VOICE_TOOLS_OPENAI_KEY", "VOICE_TOOLS_OPENAI_KEY=" + getSecret, "", true, ""},
			{"TERMINAL_SSH_HOST", "TERMINAL_SSH_HOST=" + getSecret, "", false, getSecret},
			{"mcp_servers.s.env.AWS_SECRET_ACCESS_KEY", "", "    env: {AWS_SECRET_ACCESS_KEY: " + getSecret + "}\n", true, ""},
			{"mcp_servers.s.headers.X-API-Key", "", "    headers: {X-API-Key: " + getSecret + "}\n", true, ""},
			{"mcp_servers.s.auth", "", "    auth: oauth\n", false, "oauth"},
			{"mcp_servers.s.env.UNSET_THING_API_KEY", "", "    env: {UNSET_THING_API_KEY: '${UNSET_THING_API_KEY}'}\n", false, "${UNSET_THING_API_KEY}"},
		} {
			c := newConfigHome(t)
			c.isolateEnv("FAL_KEY", "VOICE_TOOLS_OPENAI_KEY", "TERMINAL_SSH_HOST", "UNSET_THING_API_KEY")
			os.Unsetenv("UNSET_THING_API_KEY")
			c.write("mcp_servers:\n  s:\n    url: https://x.example\n" + tc.yamlLine)
			os.WriteFile(filepath.Join(c.h, ".env"), []byte(tc.envLine+"\n"), 0o600)
			out := strings.TrimSpace(get(c, tc.key, false))
			if tc.masked && (strings.Contains(out, getSecret) || !strings.Contains(out, getSecret[:4])) {
				t.Fatalf("%s not masked %q", tc.key, out)
			}
			if !tc.masked && out != tc.plain {
				t.Fatalf("%s printed %q want %q", tc.key, out, tc.plain)
			}
		}
	})
}

func TestContainerTypeRefusal(t *testing.T) {
	t.Run("test_string_where_schema_wants_list_is_refused", func(t *testing.T) {
		c := newConfigHome(t)
		c.writeDoc(map[string]any{"model": map[string]any{"default": "m"}})
		if err := c.set("custom_providers", "plainstring", false); exitStatus(err) != 1 {
			t.Fatalf("plain string %v", err)
		}
		if err := c.set("custom_providers", "- name: x\n  model: \"C:\\models\\x\"", false); exitStatus(err) != 1 {
			t.Fatalf("bad literal %v", err)
		}
		_, errOut := c.drain()
		if !strings.Contains(errOut, "must be a list, got a string") || !strings.Contains(errOut, "not valid YAML/JSON") || strings.Contains(c.readConfig(), "custom_providers") {
			t.Fatalf("stderr %q config %q", errOut, c.readConfig())
		}
	})
	t.Run("test_valid_literal_and_scalar_keys_still_write", func(t *testing.T) {
		c := newConfigHome(t)
		c.writeDoc(map[string]any{"model": map[string]any{"default": "m", "aliases": map[string]any{"a": "p/m"}}})
		c.mustSet("custom_providers", "[{name: ok, base_url: http://h/v1}]", false)
		c.mustSet("model.default", "bar", false)
		if err := c.set("model.aliases", "notamap", false); exitStatus(err) != 1 {
			t.Fatalf("not a map %v", err)
		}
		c.mustSet("model.aliases", "replaced", true)
		saved := c.parsed()
		if !reflect.DeepEqual(saved["custom_providers"], []any{map[string]any{"name": "ok", "base_url": "http://h/v1"}}) ||
			!reflect.DeepEqual(saved["model"], map[string]any{"default": "bar", "aliases": "replaced"}) {
			t.Fatalf("saved %v", saved)
		}
	})
	t.Run("test_unseeded_or_top_level_container_key_is_refused_without_on_disk_value", func(t *testing.T) {
		for _, key := range []string{"model.aliases", "providers", "toolsets"} {
			c := newConfigHome(t)
			c.writeDoc(map[string]any{"model": map[string]any{"default": "m"}})
			if err := c.set(key, "notacontainer", false); exitStatus(err) != 1 {
				t.Fatalf("%s not refused %v", key, err)
			}
			if got := c.parsed(); !reflect.DeepEqual(got, map[string]any{"model": map[string]any{"default": "m"}}) {
				t.Fatalf("%s saved %v", key, got)
			}
		}
	})
	t.Run("test_bare_name_for_string_list_slot_is_stored_as_one_item_list", func(t *testing.T) {
		c := newConfigHome(t)
		c.writeDoc(map[string]any{"model": map[string]any{"default": "m"}})
		c.mustSet("agent.disabled_toolsets", "web", false)
		if got := c.at("agent", "disabled_toolsets"); !reflect.DeepEqual(got, []any{"web"}) {
			t.Fatalf("saved %#v", got)
		}
	})
}

var switchRoute = map[string]any{"base_url": "https://chatgpt.com/backend-api/codex", "api_mode": "codex_responses"}

// Model section merged from the given maps in order
func mergedModel(parts ...map[string]any) map[string]any {
	out := map[string]any{}
	for _, p := range parts {
		for k, v := range p {
			out[k] = v
		}
	}
	return out
}

func TestProviderSwitchClearsBaseUrl(t *testing.T) {
	seed := func(c *configHome, model map[string]any) {
		c.writeDoc(map[string]any{"model": model, "custom_providers": []any{map[string]any{"name": "mylab", "base_url": "http://10.0.0.5:8000/v1", "api_key": "k"}}})
	}
	t.Run("test_switching_provider_clears_foreign_route", func(t *testing.T) {
		for _, tc := range []struct {
			target string
			route  map[string]any
		}{{"anthropic", switchRoute}, {"mylab", switchRoute}, {"anthropic", map[string]any{"api_mode": "codex_responses"}}} {
			c := newConfigHome(t)
			seed(c, mergedModel(map[string]any{"provider": "opencode-go", "default": "gpt-5.3-codex"}, tc.route))
			c.mustSet("model.provider", tc.target, false)
			if got := c.parsed()["model"]; !reflect.DeepEqual(got, map[string]any{"provider": tc.target, "default": "gpt-5.3-codex"}) {
				t.Fatalf("%s model %v", tc.target, got)
			}
			out, _ := c.drain()
			if !strings.Contains(out, "Cleared") || !strings.Contains(out, "opencode-go") {
				t.Fatalf("%s stdout %q", tc.target, out)
			}
			for k, v := range tc.route {
				if !strings.Contains(out, "model."+k+" ("+v.(string)+")") {
					t.Fatalf("%s stdout %q lacks model.%s", tc.target, out, k)
				}
			}
		}
	})
	t.Run("test_route_that_belongs_to_target_is_kept", func(t *testing.T) {
		for _, tc := range []struct {
			key, target string
			seed        map[string]any
		}{
			{"model.default", "gpt-5", mergedModel(map[string]any{"provider": "opencode-go"}, switchRoute)},
			{"model.provider", "opencode-go", mergedModel(map[string]any{"provider": "opencode-go"}, switchRoute)},
			{"model.provider", "openai-codex", mergedModel(map[string]any{"provider": "opencode-go"}, switchRoute)},
			{"model.provider", "mylab", map[string]any{"provider": "openai", "base_url": "http://10.0.0.5:8000/v1"}},
			{"model.provider", "custom", map[string]any{"provider": "openai", "base_url": "https://api.openai.com/v1"}},
			{"model.provider", "anthropic", map[string]any{"provider": "opencode-go", "base_url": "http://proxy.internal:8080/v1"}},
		} {
			c := newConfigHome(t)
			seed(c, mergedModel(tc.seed, map[string]any{"default": "m"}))
			c.mustSet(tc.key, tc.target, false)
			want := mergedModel(tc.seed, map[string]any{"default": "m", strings.SplitN(tc.key, ".", 2)[1]: tc.target})
			if got := c.parsed()["model"]; !reflect.DeepEqual(got, want) {
				t.Fatalf("%s=%s model %v want %v", tc.key, tc.target, got, want)
			}
			out, _ := c.drain()
			if strings.Contains(out, "Cleared") || strings.Contains(out, "still applies") != strings.Contains(tc.seed["base_url"].(string), "proxy.internal") {
				t.Fatalf("%s=%s stdout %q", tc.key, tc.target, out)
			}
		}
	})
}
