package commands

import (
	"bytes"
	"context"
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

func (c *configHome) set(key, value string) error {
	return config.SetConfigValue(context.Background(), c.h, key, value, configCommandSources(c.h))
}

func (c *configHome) mustSet(key, value string) {
	c.t.Helper()
	if err := c.set(key, value); err != nil {
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
			c.mustSet(key, "test-value-123")
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
			c.mustSet(key, "secret-456")
			if !strings.Contains(c.readEnv(), key+"=secret-456") || strings.Contains(c.readConfig(), key) {
				t.Fatalf("%s env %q config %q", key, c.readEnv(), c.readConfig())
			}
		}
	})
}

func TestPlatformSettingReachesTheGatewayLoader(t *testing.T) {
	c := newConfigHome(t)
	c.isolateEnv("TELEGRAM_BOT_TOKEN")
	c.write("platforms:\n  telegram:\n    enabled: false\n")
	c.mustSet("platforms.telegram.enabled", "true")
	if c.at("platforms", "telegram", "enabled") != true {
		t.Fatalf("saved %v", c.parsed())
	}
	gc, err := gateway.LoadGatewayConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p := gc.Platforms[gateway.PlatformTelegram]; p == nil || !p.Enabled {
		t.Fatalf("gateway loader %#v", p)
	}
}

func TestConfigYamlRouting(t *testing.T) {
	t.Run("test_simple_key", func(t *testing.T) {
		c := newConfigHome(t)
		c.mustSet("model.default", "gpt-4o")
		if c.at("model", "default") != "gpt-4o" || c.readEnv() != "" {
			t.Fatalf("config %q env %q", c.readConfig(), c.readEnv())
		}
	})
	t.Run("test_tool_search_defer_takes_a_list_literal", func(t *testing.T) {
		c := newConfigHome(t)
		c.mustSet("tools.tool_search.defer", `["todo_list", "skill_manage"]`)
		if got := c.at("tools", "tool_search", "defer"); !reflect.DeepEqual(got, []any{"todo_list", "skill_manage"}) {
			t.Fatalf("saved %#v", got)
		}
	})
	t.Run("test_text_setting_keeps_a_word_yaml_reads_as_a_boolean", func(t *testing.T) {
		c := newConfigHome(t)
		c.mustSet("terminal.docker_shared_container_key", "off")
		if c.at("terminal", "docker_shared_container_key") != "off" || c.readEnv() != "" {
			t.Fatalf("config %q env %q", c.readConfig(), c.readEnv())
		}
	})
}

func TestFalsyValues(t *testing.T) {
	t.Run("test_config_command_rejects_missing_value", func(t *testing.T) {
		c := newConfigHome(t)
		if err := c.command(config.ConfigArgs{Command: "set", Key: "model.default"}); exitStatus(err) != 1 {
			t.Fatalf("missing value %v", err)
		}
	})
	t.Run("test_config_command_accepts_empty_string", func(t *testing.T) {
		c := newConfigHome(t)
		if err := c.command(config.ConfigArgs{Command: "set", Key: "model.default", Value: strPtr("")}); err != nil {
			t.Fatal(err)
		}
		if stored, held := config.CfgGet(c.parsed(), "model", "default"); !held || stored != "" {
			t.Fatalf("config %q", c.readConfig())
		}
	})
}

func TestConfigGetUnset(t *testing.T) {
	t.Run("test_config_get_prints_resolved_nested_value", func(t *testing.T) {
		c := newConfigHome(t)
		c.mustSet("terminal.timeout", "120")
		c.drain()
		if err := c.command(config.ConfigArgs{Command: "get", Key: "terminal.timeout"}); err != nil {
			t.Fatal(err)
		}
		if out, _ := c.drain(); strings.TrimSpace(out) != "120" {
			t.Fatalf("get %q", out)
		}
	})
	t.Run("test_config_unset_removes_yaml_key", func(t *testing.T) {
		c := newConfigHome(t)
		c.mustSet("terminal.backend", "docker")
		if c.at("terminal", "backend") != "docker" || c.readEnv() != "" {
			t.Fatalf("config %q env %q", c.readConfig(), c.readEnv())
		}
		c.drain()
		if err := c.command(config.ConfigArgs{Command: "unset", Key: "terminal.backend"}); err != nil {
			t.Fatal(err)
		}
		if _, held := config.CfgGet(c.parsed(), "terminal", "backend"); held || c.readEnv() != "" {
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

func TestConfigGetUnknownKey(t *testing.T) {
	t.Run("test_unknown_key_is_refused_on_stderr_and_stdout_stays_empty", func(t *testing.T) {
		for _, tc := range [][2]string{
			{"compression.compressor.enabled", "compression:\n  compressor:\n    enabled: true\n"},
			{"gateway.mirror_endpoint", "gateway:\n  mirror_endpoint: http://127.0.0.1:1\n"},
			{"my_custom_setting", "my_custom_setting: hello\n"},
		} {
			c := newConfigHome(t)
			c.write(tc[1])
			if err := c.command(config.ConfigArgs{Command: "get", Key: tc[0], JSON: true}); exitStatus(err) != 1 {
				t.Fatalf("%s not refused %v", tc[0], err)
			}
			out, errOut := c.drain()
			if out != "" || !strings.Contains(errOut, tc[0]+" is not a setting icebath knows") {
				t.Fatalf("%s stdout %q stderr %q", tc[0], out, errOut)
			}
		}
	})
	t.Run("test_settings_and_map_entries_print_their_value", func(t *testing.T) {
		for _, tc := range [][3]string{
			{"terminal.timeout", "terminal:\n  timeout: 120\n", "120"},
			{"mcp_servers.local.url", "mcp_servers:\n  local:\n    url: http://127.0.0.1:1\n", "http://127.0.0.1:1"},
		} {
			c := newConfigHome(t)
			c.write(tc[1])
			if err := c.command(config.ConfigArgs{Command: "get", Key: tc[0]}); err != nil {
				t.Fatal(err)
			}
			out, errOut := c.drain()
			if strings.TrimSpace(out) != tc[2] || errOut != "" {
				t.Fatalf("%s stdout %q stderr %q", tc[0], out, errOut)
			}
		}
	})
}

func TestListNavigation(t *testing.T) {
	t.Run("test_indexed_set_inside_a_map_setting_keeps_sibling_items", func(t *testing.T) {
		c := newConfigHome(t)
		c.write("mcp_servers:\n  files:\n    command: npx\n    args:\n      - -y\n      - old-package\n      - /srv\n")
		c.mustSet("mcp_servers.files.args.1", "new-package")
		if got := c.at("mcp_servers", "files", "args"); !reflect.DeepEqual(got, []any{"-y", "new-package", "/srv"}) {
			t.Fatalf("args %#v", got)
		}
		if c.at("mcp_servers", "files", "command") != "npx" {
			t.Fatalf("config %q", c.readConfig())
		}
	})
}

func TestStringTypedConfigValues(t *testing.T) {
	t.Run("test_string_typed_values_are_not_coerced", func(t *testing.T) {
		for _, value := range []string{"off", "smart", "manual"} {
			c := newConfigHome(t)
			c.mustSet("approvals.mode", value)
			if got, isStr := c.at("approvals", "mode").(string); !isStr || got != value {
				t.Fatalf("%q saved %#v", value, c.at("approvals", "mode"))
			}
		}
	})
	t.Run("test_non_string_settings_take_their_own_type", func(t *testing.T) {
		for _, tc := range []struct {
			key, value string
			want       any
		}{{"terminal.persistent_shell", "off", false}, {"approvals.timeout", "30", 30}} {
			c := newConfigHome(t)
			c.mustSet(tc.key, tc.value)
			got := c.at(strings.Split(tc.key, ".")...)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("%s saved %#v want %#v", tc.key, got, tc.want)
			}
		}
	})
}

func TestSecretRedactionInDisplay(t *testing.T) {
	t.Run("test_set_echo_masks_secret_value", func(t *testing.T) {
		c := newConfigHome(t)
		secret := "cfut_ANOTHERSECRET0987654321zyxwvu"
		c.mustSet("providers.lab.api_key", secret)
		if out, _ := c.drain(); strings.Contains(out, secret) || !strings.Contains(out, "Set providers.lab.api_key") {
			t.Fatalf("stdout %q", out)
		}
		if c.at("providers", "lab", "api_key") != secret {
			t.Fatalf("config %q", c.readConfig())
		}
	})
}

func TestUnknownKeysAreRefused(t *testing.T) {
	const start = "model:\n  default: gpt-4o\n"
	for _, tc := range []struct{ key, suggestion string }{
		{"agent.gateway.strict", "gateway.strict"},
		{"agent.max_turnz", "agent.max_turns"},
		{"brand_new_future_key", ""},
	} {
		c := newConfigHome(t)
		c.write(start)
		if err := c.set(tc.key, "true"); exitStatus(err) != 1 {
			t.Fatalf("%s not refused %v", tc.key, err)
		}
		_, errOut := c.drain()
		if c.readConfig() != start || !strings.Contains(errOut, tc.key+" is not a setting icebath knows; nothing was written") {
			t.Fatalf("%s config %q stderr %q", tc.key, c.readConfig(), errOut)
		}
		hint := ""
		if tc.suggestion != "" {
			hint = "Did you mean: " + tc.suggestion
		}
		if strings.Contains(errOut, "Did you mean") != (hint != "") || !strings.Contains(errOut, hint) {
			t.Fatalf("%s suggestion in %q", tc.key, errOut)
		}
	}
}

func TestDisplaySkinTouch(t *testing.T) {
	old := time.Unix(1_000_000_000, 0)
	t.Run("test_reaffirming_same_skin_moves_the_watcher_signature", func(t *testing.T) {
		c := newConfigHome(t)
		skin := filepath.Join(c.h, "skins", "synthwave.yaml")
		os.MkdirAll(filepath.Dir(skin), 0o755)
		os.WriteFile(skin, []byte("name: synthwave\ncolors:\n  background: '#1a1030'\n"), 0o644)
		os.Chtimes(skin, old, old)
		c.mustSet("display.skin", "synthwave")
		if st, _ := os.Stat(skin); !st.ModTime().After(old) {
			t.Fatal("first set left the mtime")
		}
		os.Chtimes(skin, old, old)
		c.mustSet("display.skin", "synthwave")
		if st, _ := os.Stat(skin); !st.ModTime().After(old) {
			t.Fatal("re-affirmed set left the mtime")
		}
	})
	t.Run("test_builtin_or_missing_skin_file_is_fine", func(t *testing.T) {
		c := newConfigHome(t)
		c.mustSet("display.skin", "contrast")
		if !strings.Contains(c.readConfig(), "skin: contrast") {
			t.Fatalf("config %q", c.readConfig())
		}
	})
	t.Run("test_touch_preserves_skin_file_contents", func(t *testing.T) {
		c := newConfigHome(t)
		skin := filepath.Join(c.h, "skins", "neon.yaml")
		os.MkdirAll(filepath.Dir(skin), 0o755)
		body := "name: neon\ncolors:\n  ui_accent: '#ff33aa'\n"
		os.WriteFile(skin, []byte(body), 0o644)
		c.mustSet("display.skin", "neon")
		if got, _ := os.ReadFile(skin); string(got) != body {
			t.Fatalf("skin %q", got)
		}
	})
}

func TestSectionNamesAreNotSettings(t *testing.T) {
	t.Run("test_section_name_is_refused_and_the_section_kept", func(t *testing.T) {
		for _, tc := range [][2]string{{"model", "claude-opus-4"}, {"model", `{"provider": "openrouter", "default": "z-ai/glm-5.3-flash"}`}, {"terminal", "zsh"}} {
			c := newConfigHome(t)
			start := map[string]any{
				"model":    map[string]any{"default": "gpt-4o", "provider": "openai-api"},
				"terminal": map[string]any{"backend": "docker", "docker_image": "python:3.12"},
			}
			c.writeDoc(start)
			if err := c.set(tc[0], tc[1]); exitStatus(err) != 1 {
				t.Fatalf("%s=%q not refused %v", tc[0], tc[1], err)
			}
			if got := c.parsed(); !reflect.DeepEqual(got, start) {
				t.Fatalf("%s=%q changed the config to %v", tc[0], tc[1], got)
			}
		}
	})
	t.Run("test_dotted_key_changes_one_field_of_its_section", func(t *testing.T) {
		c := newConfigHome(t)
		c.writeDoc(map[string]any{"model": map[string]any{"default": "gpt-4o", "provider": "openai-api"}})
		c.mustSet("model.default", "claude-opus-4")
		if m := c.parsed()["model"].(map[string]any); m["default"] != "claude-opus-4" || m["provider"] != "openai-api" {
			t.Fatalf("model %v", m)
		}
	})
}

func TestMalformedYAMLConfigPreservation(t *testing.T) {
	const broken = "model:\n  default: gpt-4o\nterminal:\n  backend: docker\n  broken: [this is invalid YAML"
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
		check(t, c, c.set("agent.max_turns", "50"))
	})
	t.Run("test_unset_config_value_refuses_broken_yaml", func(t *testing.T) {
		c := newConfigHome(t)
		c.write(broken)
		check(t, c, config.UnsetConfigValue(context.Background(), c.h, "model.default", configCommandSources(c.h)))
	})
}

func TestLiteralDotKeyEscaping(t *testing.T) {
	providers := func(c *configHome) {
		c.write("providers:\n  qwen3.5-397b-wafer-non-zdr:\n    base_url: https://pass.wafer.ai/v1\n  openrouter:\n    api_key: or-keep\n")
	}
	t.Run("test_set_preserves_literal_dot_in_provider_key", func(t *testing.T) {
		c := newConfigHome(t)
		providers(c)
		c.mustSet(`providers.qwen3\.5-397b-wafer-non-zdr.extra_headers`, `{"Wafer-ZDR": "required"}`)
		saved := c.parsed()["providers"].(map[string]any)
		target, _ := saved["qwen3.5-397b-wafer-non-zdr"].(map[string]any)
		if _, has := saved["qwen3"]; has || target["base_url"] != "https://pass.wafer.ai/v1" ||
			!reflect.DeepEqual(target["extra_headers"], map[string]any{"Wafer-ZDR": "required"}) ||
			!reflect.DeepEqual(saved["openrouter"], map[string]any{"api_key": "or-keep"}) {
			t.Fatalf("providers %v", saved)
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
		c.write("providers:\n  qwen3.5-397b-wafer-non-zdr:\n    base_url: https://pass.wafer.ai/v1\n    extra_headers: '{\"K\": \"V\"}'\n")
		if err := c.command(config.ConfigArgs{Command: "unset", Key: `providers.qwen3\.5-397b-wafer-non-zdr.extra_headers`}); err != nil {
			t.Fatal(err)
		}
		target := c.parsed()["providers"].(map[string]any)["qwen3.5-397b-wafer-non-zdr"].(map[string]any)
		if _, has := target["extra_headers"]; has || target["base_url"] != "https://pass.wafer.ai/v1" {
			t.Fatalf("target %v", target)
		}
	})
	t.Run("test_get_reads_literal_dot_provider_key", func(t *testing.T) {
		c := newConfigHome(t)
		c.write("providers:\n  qwen3.5-397b:\n    base_url: https://pass.wafer.ai/v1\n")
		if err := c.command(config.ConfigArgs{Command: "get", Key: `providers.qwen3\.5-397b.base_url`}); err != nil {
			t.Fatal(err)
		}
		if out, _ := c.drain(); strings.TrimSpace(out) != "https://pass.wafer.ai/v1" {
			t.Fatalf("stdout %q", out)
		}
	})
	t.Run("test_unescaped_dotted_path_unchanged", func(t *testing.T) {
		c := newConfigHome(t)
		c.mustSet("terminal.backend", "docker")
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
	t.Run("test_plain_word_where_the_setting_wants_a_list_is_refused", func(t *testing.T) {
		c := newConfigHome(t)
		c.writeDoc(map[string]any{"model": map[string]any{"default": "m"}})
		if err := c.set("fallback_providers", "plainstring"); exitStatus(err) != 1 {
			t.Fatalf("plain string %v", err)
		}
		if err := c.set("fallback_providers", "- provider: x\n  model: \"C:\\models\\x\""); exitStatus(err) != 1 {
			t.Fatalf("bad literal %v", err)
		}
		_, errOut := c.drain()
		if !strings.Contains(errOut, "fallback_providers: takes a list") || !strings.Contains(errOut, "not valid YAML/JSON") ||
			strings.Contains(errOut, "exit 1") || strings.Contains(c.readConfig(), "fallback_providers") {
			t.Fatalf("stderr %q config %q", errOut, c.readConfig())
		}
	})
	t.Run("test_valid_literal_and_scalar_keys_still_write", func(t *testing.T) {
		c := newConfigHome(t)
		aliases := map[string]any{"quick": map[string]any{"model": "some/model", "provider": "openrouter"}}
		c.writeDoc(map[string]any{"model": map[string]any{"default": "m", "aliases": aliases}})
		c.mustSet("fallback_providers", "[{provider: openrouter, model: some/model}]")
		c.mustSet("model.default", "bar")
		if err := c.set("model.aliases", "notamap"); exitStatus(err) != 1 {
			t.Fatalf("not a map %v", err)
		}
		saved := c.parsed()
		if !reflect.DeepEqual(saved["fallback_providers"], []any{map[string]any{"provider": "openrouter", "model": "some/model"}}) ||
			!reflect.DeepEqual(saved["model"], map[string]any{"default": "bar", "aliases": aliases}) {
			t.Fatalf("saved %v", saved)
		}
	})
	t.Run("test_container_setting_refuses_a_plain_word", func(t *testing.T) {
		for _, key := range []string{"model.aliases", "providers"} {
			c := newConfigHome(t)
			c.writeDoc(map[string]any{"model": map[string]any{"default": "m"}})
			if err := c.set(key, "notacontainer"); exitStatus(err) != 1 {
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
		c.mustSet("agent.disabled_toolsets", "web")
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
		c.writeDoc(map[string]any{"model": model, "providers": map[string]any{"mylab": map[string]any{"base_url": "http://10.0.0.5:8000/v1", "api_key": "k"}}})
	}
	t.Run("test_switching_provider_clears_foreign_route", func(t *testing.T) {
		for _, tc := range []struct {
			target string
			route  map[string]any
		}{{"anthropic", switchRoute}, {"mylab", switchRoute}, {"anthropic", map[string]any{"api_mode": "codex_responses"}}} {
			c := newConfigHome(t)
			seed(c, mergedModel(map[string]any{"provider": "opencode-go", "default": "gpt-5.3-codex"}, tc.route))
			c.mustSet("model.provider", tc.target)
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
			c.mustSet(tc.key, tc.target)
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
