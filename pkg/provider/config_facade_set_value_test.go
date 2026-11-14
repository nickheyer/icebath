package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

type setValueHome struct {
	t           *testing.T
	h           string
	out, errOut *bytes.Buffer
}

// Home with an empty .env, the given config text when not empty, and facade output captured
func newSetValueHome(t *testing.T, text string) *setValueHome {
	t.Helper()
	h, out, errOut := moaKeysHome(t, map[string]any{})
	path := filepath.Join(h, "config.yaml")
	if text == "" {
		os.Remove(path)
	} else if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return &setValueHome{t: t, h: h, out: out, errOut: errOut}
}

func (s *setValueHome) set(key, value string, force bool) error {
	return config.SetConfigValue(context.Background(), s.h, key, value, force, moaCommandSources())
}

func (s *setValueHome) mustSet(key, value string) {
	s.t.Helper()
	if err := s.set(key, value, false); err != nil {
		s.t.Fatalf("set %s: %v %s", key, err, s.errOut)
	}
}

func (s *setValueHome) command(args config.ConfigArgs) error {
	return config.ConfigCommand(context.Background(), s.h, args, moaCommandSources())
}

func (s *setValueHome) get(key string, asJSON, raw bool) string {
	s.t.Helper()
	s.out.Reset()
	s.errOut.Reset()
	if err := config.GetConfigValue(context.Background(), s.h, key, asJSON, raw, moaCommandSources()); err != nil {
		s.t.Fatalf("get %s: %v", key, err)
	}
	return strings.TrimSpace(s.out.String())
}

func (s *setValueHome) text() string {
	data, _ := os.ReadFile(filepath.Join(s.h, "config.yaml"))
	return string(data)
}

func (s *setValueHome) env() string {
	data, _ := os.ReadFile(filepath.Join(s.h, ".env"))
	return string(data)
}

func (s *setValueHome) doc() map[string]any {
	s.t.Helper()
	doc, err := config.ReadDocument(filepath.Join(s.h, "config.yaml"))
	if err != nil {
		s.t.Fatal(err)
	}
	return doc
}

func (s *setValueHome) at(keys ...string) any {
	v, _ := config.CfgGet(s.doc(), keys...)
	return v
}

func isConfigExit(err error, code int) bool {
	exit, ok := err.(*config.ConfigExit)
	return ok && exit.Code == code
}

func TestSetValueEnvRoutedKeys(t *testing.T) {
	for _, key := range []string{"FAL_KEY", "SUDO_PASSWORD", "API_SERVER_KEY", "SOME_FUTURE_SERVICE_API_KEY", "MY_CUSTOM_TOKEN", "CLIENT_SECRET"} {
		s := newSetValueHome(t, "")
		s.mustSet(key, "test-value-123")
		if !strings.Contains(s.env(), key+"=test-value-123") || strings.Contains(s.text(), key) {
			t.Fatalf("%s env %q config %q", key, s.env(), s.text())
		}
	}
}

func TestSetValueGatewayPlatformsPrefixRedirect(t *testing.T) {
	s := newSetValueHome(t, "platforms:\n  telegram:\n    enabled: false\n")
	s.mustSet("gateway.platforms.telegram.enabled", "true")
	if !strings.Contains(s.out.String(), "saved as platforms.telegram.enabled") || s.at("platforms", "telegram", "enabled") != true || s.doc()["gateway"] != nil {
		t.Fatalf("%q %v", s.out, s.doc())
	}
	legacy := "gateway:\n  platforms:\n    telegram:\n      enabled: true\n"
	s = newSetValueHome(t, legacy)
	if got := s.get("gateway.platforms.telegram.enabled", false, false); strings.ToLower(got) != "true" {
		t.Fatalf("legacy get %q", got)
	}
	if err := config.UnsetConfigValue(context.Background(), s.h, "gateway.platforms.telegram.enabled", moaCommandSources()); err != nil {
		t.Fatal(err)
	}
	if _, has := s.doc()["gateway"]; has {
		t.Fatalf("legacy unset left %v", s.doc())
	}
	os.WriteFile(filepath.Join(s.h, "config.yaml"), []byte(legacy), 0o600)
	s.mustSet("gateway.platforms.telegram.enabled", "false")
	want := map[string]any{"platforms": map[string]any{"telegram": map[string]any{"enabled": false}}}
	if !reflect.DeepEqual(s.doc(), want) {
		t.Fatalf("shadowed duplicate %v", s.doc())
	}
}

func TestSetValueConfigYamlRouting(t *testing.T) {
	s := newSetValueHome(t, "")
	s.mustSet("model", "gpt-4o")
	if !strings.Contains(s.text(), "gpt-4o") || strings.Contains(s.env(), "model") {
		t.Fatalf("model %q env %q", s.text(), s.env())
	}
	s = newSetValueHome(t, "")
	s.mustSet("tools.tool_search.defer", `["todo_list", "skill_manage"]`)
	if strings.Contains(s.out.String()+s.errOut.String(), unknownKeyNotice) || !reflect.DeepEqual(s.at("tools", "tool_search", "defer"), []any{"todo_list", "skill_manage"}) {
		t.Fatalf("defer %q %v", s.out, s.doc())
	}
	s = newSetValueHome(t, "")
	s.mustSet("terminal.docker_shared_container_key", "off")
	if s.at("terminal", "docker_shared_container_key") != "off" || !strings.Contains(s.env(), "TERMINAL_DOCKER_SHARED_CONTAINER_KEY=off") || strings.Contains(s.out.String(), unknownKeyNotice) {
		t.Fatalf("shared key %v %q %q", s.doc(), s.env(), s.out)
	}
}

func TestSetValueFalsyValues(t *testing.T) {
	s := newSetValueHome(t, "")
	if err := s.command(config.ConfigArgs{Command: "set", Key: "model"}); err == nil {
		t.Fatal("a missing value must exit")
	}
	empty := ""
	if err := s.command(config.ConfigArgs{Command: "set", Key: "model", Value: &empty}); err != nil || !strings.Contains(s.text(), "model") {
		t.Fatalf("empty value %v %q", err, s.text())
	}
}

func TestSetValueGetAndUnset(t *testing.T) {
	s := newSetValueHome(t, "")
	s.mustSet("terminal.timeout", "120")
	s.out.Reset()
	if err := s.command(config.ConfigArgs{Command: "get", Key: "terminal.timeout"}); err != nil || strings.TrimSpace(s.out.String()) != "120" {
		t.Fatalf("get %v %q", err, s.out)
	}
	s = newSetValueHome(t, "")
	s.mustSet("terminal.backend", "docker")
	if !strings.Contains(s.env(), "TERMINAL_ENV=docker") {
		t.Fatalf("env %q", s.env())
	}
	s.out.Reset()
	if err := s.command(config.ConfigArgs{Command: "unset", Key: "terminal.backend"}); err != nil {
		t.Fatal(err)
	}
	if len(s.doc()) != 0 || strings.Contains(s.env(), "TERMINAL_ENV=") || !strings.Contains(s.out.String(), "Unset terminal.backend") {
		t.Fatalf("unset %v %q %q", s.doc(), s.env(), s.out)
	}
	s = newSetValueHome(t, "platforms:\n  teams:\n    extra:\n      access_token: yaml-token\n      tenant_id: tenant\n")
	if err := s.command(config.ConfigArgs{Command: "unset", Key: "platforms.teams.extra.access_token"}); err != nil {
		t.Fatal(err)
	}
	extra, _ := s.at("platforms", "teams", "extra").(map[string]any)
	if _, has := extra["access_token"]; has || extra["tenant_id"] != "tenant" || !strings.Contains(s.out.String(), "Unset platforms.teams.extra.access_token") {
		t.Fatalf("dotted token unset %v %q", extra, s.out)
	}
}

func TestSetValueGetPhantomKeyNotice(t *testing.T) {
	s := newSetValueHome(t, "compression:\n  compressor:\n    enabled: true\n")
	var got any
	if out := s.get("compression.compressor.enabled", true, false); json.Unmarshal([]byte(out), &got) != nil || got != true || !strings.Contains(s.errOut.String(), unknownKeyNotice) {
		t.Fatalf("phantom %q %q", out, s.errOut)
	}
	s = newSetValueHome(t, "gateway:\n  proxy_url: http://127.0.0.1:1\n")
	if out := s.get("gateway.proxy_url", false, false); out != "http://127.0.0.1:1" || !strings.Contains(s.errOut.String(), "may not read it") || strings.Contains(s.errOut.String(), "does not read it") {
		t.Fatalf("unseeded live key %q %q", out, s.errOut)
	}
	for key, body := range map[string]string{
		"terminal.timeout":      "terminal:\n  timeout: 120\n",
		"my_custom_setting":     "my_custom_setting: hello\n",
		"mcp_servers.local.url": "mcp_servers:\n  local:\n    url: http://127.0.0.1:1\n",
	} {
		s = newSetValueHome(t, body)
		if out := s.get(key, false, false); out == "" || strings.Contains(s.errOut.String(), unknownKeyNotice) {
			t.Fatalf("%s %q %q", key, out, s.errOut)
		}
	}
}

func TestSetValueListNavigation(t *testing.T) {
	s := newSetValueHome(t, "custom_providers:\n- name: provider-a\n  api_key: old-a\n  base_url: https://a.example.com\n  models:\n    foo: {}\n    bar: {}\n- name: provider-b\n  api_key: old-b\n  base_url: https://b.example.com\n")
	s.mustSet("custom_providers.0.api_key", "new-a")
	entries, _ := s.doc()["custom_providers"].([]any)
	want := []any{
		map[string]any{"name": "provider-a", "api_key": "new-a", "base_url": "https://a.example.com", "models": map[string]any{"foo": map[string]any{}, "bar": map[string]any{}}},
		map[string]any{"name": "provider-b", "api_key": "old-b", "base_url": "https://b.example.com"},
	}
	if !reflect.DeepEqual(entries, want) {
		t.Fatalf("indexed set %v", entries)
	}
	s = newSetValueHome(t, "telegram:\n  allowlist:\n    - name: alice\n      role: admin\n    - name: bob\n      role: user\n")
	s.mustSet("telegram.allowlist.1.role", "admin")
	want = []any{map[string]any{"name": "alice", "role": "admin"}, map[string]any{"name": "bob", "role": "admin"}}
	if got := s.at("telegram", "allowlist"); !reflect.DeepEqual(got, want) {
		t.Fatalf("deeper nesting %v", got)
	}
}

func TestSetValueStringTypedValues(t *testing.T) {
	for _, value := range []string{"off", "true", "01"} {
		s := newSetValueHome(t, "")
		s.mustSet("approvals.mode", value)
		if got := s.at("approvals", "mode"); got != value {
			t.Fatalf("approvals.mode %q saved %#v", value, got)
		}
	}
	for _, c := range []struct {
		key, value string
		want       any
	}{{"terminal.persistent_shell", "off", false}, {"approvals.timeout", "30", 30}} {
		s := newSetValueHome(t, "")
		s.mustSet(c.key, c.value)
		parts := strings.Split(c.key, ".")
		if got := s.at(parts...); fmt.Sprintf("%T %v", got, got) != fmt.Sprintf("%T %v", c.want, c.want) {
			t.Fatalf("%s saved %#v", c.key, got)
		}
	}
	s := newSetValueHome(t, "")
	if err := s.set("custom.enabled", "off", true); err != nil || s.at("custom", "enabled") != false {
		t.Fatalf("unknown key coercion %v %v", err, s.doc())
	}
}

func TestSetValueEchoMasksSecret(t *testing.T) {
	s := newSetValueHome(t, "")
	secret := "cfut_ANOTHERSECRET0987654321zyxwvu"
	s.mustSet("model.api_key", secret)
	if strings.Contains(s.out.String(), secret) || !strings.Contains(s.out.String(), "Set model.api_key") {
		t.Fatalf("echo %q", s.out)
	}
}

func TestSetValueSchemaValidation(t *testing.T) {
	for key, suggestion := range map[string]string{
		"gateway.discord.gateway_restart_notification": "discord.gateway_restart_notification",
		"agent.gateway.strict":                         "gateway.strict",
	} {
		s := newSetValueHome(t, "model: gpt-4o\n")
		if err := s.set(key, "true", false); err == nil {
			t.Fatalf("%s written", key)
		}
		if s.text() != "model: gpt-4o\n" || !strings.Contains(s.errOut.String(), "nothing was written") || !strings.Contains(s.errOut.String(), "Did you mean: "+suggestion) {
			t.Fatalf("%s %q %q", key, s.text(), s.errOut)
		}
	}
	for _, c := range []struct {
		key, value string
		want       any
		suggestion string
	}{
		{"gateway.proxy_url", "http://127.0.0.1:1", "http://127.0.0.1:1", ""},
		{"agent.max_turnz", "50", 50, "agent.max_turns"},
		{"gateway.filter_silence_narration", "false", false, ""},
	} {
		s := newSetValueHome(t, "")
		s.mustSet(c.key, c.value)
		parts := strings.Split(c.key, ".")
		out := s.out.String()
		if got := s.at(parts...); !reflect.DeepEqual(got, c.want) || !strings.Contains(out, unknownKeyNotice) || strings.Contains(out, "bridged to the environment") || !strings.Contains(out, "Use --force") {
			t.Fatalf("%s saved %#v out %q", c.key, got, out)
		}
		if (c.suggestion == "") == strings.Contains(out, "Did you mean") || (c.suggestion != "" && !strings.Contains(out, "Did you mean: "+c.suggestion)) {
			t.Fatalf("%s suggestion %q", c.key, out)
		}
	}
	s := newSetValueHome(t, "")
	s.mustSet("brand_new_future_key", "value")
	if !strings.Contains(s.text(), "brand_new_future_key") || !strings.Contains(s.out.String(), unknownKeyNotice) {
		t.Fatalf("top level %q %q", s.text(), s.out)
	}
	s = newSetValueHome(t, "")
	if err := s.set("brand_new_future_key", "value", true); err != nil || strings.Contains(s.out.String(), unknownKeyNotice) || !strings.Contains(s.text(), "brand_new_future_key") {
		t.Fatalf("force %v %q", err, s.out)
	}
}

func TestSetValueDisplaySkinTouch(t *testing.T) {
	s := newSetValueHome(t, "")
	skins := filepath.Join(s.h, "skins")
	os.MkdirAll(skins, 0o755)
	skin := filepath.Join(skins, "synthwave.yaml")
	os.WriteFile(skin, []byte("name: synthwave\ncolors:\n  background: '#1a1030'\n"), 0o644)
	old := time.Unix(1_000_000_000, 0)
	for range 2 {
		os.Chtimes(skin, old, old)
		s.mustSet("display.skin", "synthwave")
		if st, _ := os.Stat(skin); !st.ModTime().After(old) {
			t.Fatal("reaffirming the skin left the watcher signature")
		}
	}
	s = newSetValueHome(t, "")
	s.mustSet("display.skin", "mono")
	if !strings.Contains(s.text(), "skin: mono") {
		t.Fatalf("builtin skin %q", s.text())
	}
	body := "name: neon\ncolors:\n  ui_accent: '#ff33aa'\n"
	os.MkdirAll(filepath.Join(s.h, "skins"), 0o755)
	os.WriteFile(filepath.Join(s.h, "skins", "neon.yaml"), []byte(body), 0o644)
	s.mustSet("display.skin", "neon")
	if data, _ := os.ReadFile(filepath.Join(s.h, "skins", "neon.yaml")); string(data) != body {
		t.Fatalf("touch rewrote the skin %q", data)
	}
}

func TestSetValueMappingGuard(t *testing.T) {
	s := newSetValueHome(t, "model:\n  default: gpt-4o\n  provider: openai-api\n  context_length: 128000\n  base_url: https://api.example.com/v1\n")
	s.mustSet("model", "claude-sonnet-4-20250514")
	want := map[string]any{"default": "claude-sonnet-4-20250514", "provider": "openai-api", "context_length": 128000, "base_url": "https://api.example.com/v1"}
	if !reflect.DeepEqual(s.doc()["model"], want) {
		t.Fatalf("shorthand %v", s.doc()["model"])
	}
	s = newSetValueHome(t, "")
	s.mustSet("model", "gpt-5.6-sol")
	if !strings.Contains(s.text(), "gpt-5.6-sol") {
		t.Fatalf("shorthand create %q", s.text())
	}
	for _, c := range []struct{ start, literal string }{
		{"model:\n  default: glm-5.3-flash\n  provider: zai\n", `{"provider": "openai-codex", "default": "gpt-6.1-sol"}`},
		{"", "[a, b]"},
		{"model: gpt-4o\n", "[a, b]"},
	} {
		s = newSetValueHome(t, c.start)
		if err := s.set("model", c.literal, false); !isConfigExit(err, 1) || s.text() != c.start {
			t.Fatalf("literal %q %v %q", c.literal, err, s.text())
		}
	}
	s = newSetValueHome(t, "model:\n  default: gpt-4o\n  provider: openai-api\n  context_length: 128000\n")
	if err := s.set("model", `{"provider": "openrouter", "default": "z-ai/glm-5.3-flash"}`, true); err != nil || !reflect.DeepEqual(s.doc()["model"], map[string]any{"provider": "openrouter", "default": "z-ai/glm-5.3-flash"}) {
		t.Fatalf("force mapping %v %v", err, s.doc())
	}
	s = newSetValueHome(t, "terminal:\n  backend: docker\n  docker_image: python:3.12\n  shell: bash\n")
	if err := s.set("terminal", "zsh", false); !isConfigExit(err, 1) {
		t.Fatalf("terminal scalar %v", err)
	}
	s = newSetValueHome(t, "terminal:\n  backend: docker\n  shell: bash\n")
	if err := s.set("terminal", "zsh", true); err != nil || s.doc()["terminal"] != "zsh" {
		t.Fatalf("terminal force %v %v", err, s.doc())
	}
	s = newSetValueHome(t, "model:\n  default: gpt-4o\n  provider: openai-api\n")
	s.mustSet("model.default", "claude-opus-4")
	if !reflect.DeepEqual(s.doc()["model"], map[string]any{"default": "claude-opus-4", "provider": "openai-api"}) {
		t.Fatalf("dotted default %v", s.doc())
	}
	s = newSetValueHome(t, "model:\n  default: gpt-4o\n  provider: openai-api\n  context_length: 128000\n")
	if err := s.set("model", "claude-opus-4", true); err != nil || s.doc()["model"] != "claude-opus-4" {
		t.Fatalf("force model %v %v", err, s.doc())
	}
}

func TestSetValueScalarModelSubKeyPreservation(t *testing.T) {
	for _, c := range [][3]string{{"gpt-4o", "provider", "openai"}, {"claude-sonnet", "api_key", "sk-test"}} {
		s := newSetValueHome(t, "")
		s.mustSet("model", c[0])
		s.mustSet("model."+c[1], c[2])
		if s.at("model", "default") != c[0] || s.at("model", c[1]) != c[2] {
			t.Fatalf("scalar model lost %v", s.doc())
		}
	}
}

func TestSetValueMalformedYAMLConfigPreservation(t *testing.T) {
	broken := "model: gpt-4o\nterminal:\n  backend: docker\n  broken: [this is invalid YAML"
	for _, op := range []string{"set", "unset"} {
		s := newSetValueHome(t, broken)
		var err error
		if op == "set" {
			err = s.set("agent.max_turns", "50", false)
		} else {
			err = config.UnsetConfigValue(context.Background(), s.h, "model", moaCommandSources())
		}
		combined := s.out.String() + s.errOut.String()
		if err == nil || !strings.Contains(err.Error(), "formatting error") || !strings.Contains(combined, "formatting error") || !strings.Contains(combined, "`icebath config edit`") || s.text() != broken {
			t.Fatalf("%s %v %q %q", op, err, combined, s.text())
		}
	}
}

func TestSetValueLiteralDotKeyEscaping(t *testing.T) {
	seed := "providers:\n  qwen3.5-397b-wafer-non-zdr:\n    api: https://pass.wafer.ai/v1\n  openrouter:\n    api_key: or-keep\n"
	s := newSetValueHome(t, seed)
	s.mustSet(`providers.qwen3\.5-397b-wafer-non-zdr.extra_headers`, `{"Wafer-ZDR": "required"}`)
	providers, _ := s.doc()["providers"].(map[string]any)
	want := map[string]any{"api": "https://pass.wafer.ai/v1", "extra_headers": map[string]any{"Wafer-ZDR": "required"}}
	if _, has := providers["qwen3"]; has || !reflect.DeepEqual(providers["qwen3.5-397b-wafer-non-zdr"], want) || !reflect.DeepEqual(providers["openrouter"], map[string]any{"api_key": "or-keep"}) || strings.Contains(s.out.String(), unknownKeyNotice) {
		t.Fatalf("escaped set %v %q", providers, s.out)
	}
	s = newSetValueHome(t, seed)
	if err := s.command(config.ConfigArgs{Command: "unset", Key: `providers.qwen3\.5-397b-wafer-non-zdr`}); err != nil {
		t.Fatal(err)
	}
	providers, _ = s.doc()["providers"].(map[string]any)
	if _, has := providers["qwen3.5-397b-wafer-non-zdr"]; has || !strings.Contains(s.out.String(), `Unset providers.qwen3\.5-397b-wafer-non-zdr`) {
		t.Fatalf("escaped unset %v %q", providers, s.out)
	}
	s = newSetValueHome(t, "providers:\n  qwen3.5-397b-wafer-non-zdr:\n    api: https://pass.wafer.ai/v1\n    extra_headers: '{\"K\": \"V\"}'\n")
	if err := s.command(config.ConfigArgs{Command: "unset", Key: `providers.qwen3\.5-397b-wafer-non-zdr.extra_headers`}); err != nil {
		t.Fatal(err)
	}
	if got := s.at("providers", "qwen3.5-397b-wafer-non-zdr"); !reflect.DeepEqual(got, map[string]any{"api": "https://pass.wafer.ai/v1"}) {
		t.Fatalf("nested unset %v", got)
	}
	s = newSetValueHome(t, "providers:\n  qwen3.5-397b:\n    api: https://pass.wafer.ai/v1\n")
	if got := s.get(`providers.qwen3\.5-397b.api`, false, false); got != "https://pass.wafer.ai/v1" {
		t.Fatalf("escaped get %q", got)
	}
	s = newSetValueHome(t, "")
	s.mustSet("terminal.backend", "docker")
	if s.at("terminal", "backend") != "docker" {
		t.Fatalf("plain dotted %v", s.doc())
	}
}

const getSecret = "OPAQUEKEYVALUE12345678"

func TestConfigGetRedaction(t *testing.T) {
	seed := func() *setValueHome {
		s := newSetValueHome(t, "providers:\n  gemini:\n    api_key: "+getSecret+"\nmcp_servers:\n  s:\n    env:\n      MY_API_KEY: ${MY_API_KEY}\n    url: https://x.example\n")
		os.WriteFile(filepath.Join(s.h, ".env"), []byte("GEMINI_API_KEY="+getSecret+"\n"), 0o600)
		t.Setenv("MY_API_KEY", getSecret)
		return s
	}
	for _, key := range []string{"providers", "providers.gemini.api_key", "GEMINI_API_KEY", "mcp_servers.s.env.MY_API_KEY"} {
		s := seed()
		out := s.get(key, false, false)
		if strings.Contains(out, getSecret) || !strings.Contains(out, getSecret[:4]) || (key == "providers" && !strings.Contains(out, "gemini")) {
			t.Fatalf("%s printed %q", key, out)
		}
	}
	if out := seed().get("providers.gemini.api_key", false, true); out != getSecret {
		t.Fatalf("raw %q", out)
	}
	os.Unsetenv("UNSET_THING_API_KEY")
	for _, c := range []struct {
		key, envLine, yamlLine string
		masked                 bool
	}{
		{"FAL_KEY", "FAL_KEY=" + getSecret, "", true},
		{"VOICE_TOOLS_OPENAI_KEY", "VOICE_TOOLS_OPENAI_KEY=" + getSecret, "", true},
		{"TERMINAL_SSH_HOST", "TERMINAL_SSH_HOST=" + getSecret, "", false},
		{"mcp_servers.s.env.AWS_SECRET_ACCESS_KEY", "", "    env: {AWS_SECRET_ACCESS_KEY: " + getSecret + "}\n", true},
		{"mcp_servers.s.headers.X-API-Key", "", "    headers: {X-API-Key: " + getSecret + "}\n", true},
		{"mcp_servers.s.auth", "", "    auth: oauth\n", false},
		{"mcp_servers.s.env.UNSET_THING_API_KEY", "", "    env: {UNSET_THING_API_KEY: '${UNSET_THING_API_KEY}'}\n", false},
	} {
		s := newSetValueHome(t, "mcp_servers:\n  s:\n    url: https://x.example\n"+c.yamlLine)
		os.WriteFile(filepath.Join(s.h, ".env"), []byte(c.envLine+"\n"), 0o600)
		out := s.get(c.key, false, false)
		want := map[string]string{"TERMINAL_SSH_HOST": getSecret, "mcp_servers.s.auth": "oauth"}[c.key]
		if want == "" {
			want = "${UNSET_THING_API_KEY}"
		}
		if (c.masked && (strings.Contains(out, getSecret) || !strings.Contains(out, getSecret[:4]))) || (!c.masked && out != want) {
			t.Fatalf("%s printed %q", c.key, out)
		}
	}
}

func TestSetValueContainerTypeRefusal(t *testing.T) {
	s := newSetValueHome(t, "model:\n  default: m\n")
	if err := s.set("custom_providers", "plainstring", false); err == nil {
		t.Fatal("string for a list written")
	}
	if err := s.set("custom_providers", "- name: x\n  model: \"C:\\models\\x\"", false); err == nil {
		t.Fatal("invalid literal written")
	}
	if !strings.Contains(s.errOut.String(), "must be a list, got a string") || !strings.Contains(s.errOut.String(), "not valid YAML/JSON") || strings.Contains(s.text(), "custom_providers") {
		t.Fatalf("%q %q", s.errOut, s.text())
	}
	s = newSetValueHome(t, "model:\n  default: m\n  aliases:\n    a: p/m\n")
	s.mustSet("custom_providers", "[{name: ok, base_url: http://h/v1}]")
	s.mustSet("model.default", "bar")
	if err := s.set("model.aliases", "notamap", false); err == nil {
		t.Fatal("scalar for a mapping written")
	}
	if err := s.set("model.aliases", "replaced", true); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s.doc()["custom_providers"], []any{map[string]any{"name": "ok", "base_url": "http://h/v1"}}) || !reflect.DeepEqual(s.doc()["model"], map[string]any{"default": "bar", "aliases": "replaced"}) {
		t.Fatalf("valid writes %v", s.doc())
	}
	for _, key := range []string{"model.aliases", "providers", "toolsets"} {
		s = newSetValueHome(t, "model:\n  default: m\n")
		if err := s.set(key, "notacontainer", false); err == nil || !reflect.DeepEqual(s.doc(), map[string]any{"model": map[string]any{"default": "m"}}) {
			t.Fatalf("%s %v %v", key, err, s.doc())
		}
	}
	s = newSetValueHome(t, "model:\n  default: m\n")
	s.mustSet("agent.disabled_toolsets", "web")
	if !reflect.DeepEqual(s.at("agent", "disabled_toolsets"), []any{"web"}) {
		t.Fatalf("bare name list %v", s.doc())
	}
}

func TestSetValueProviderSwitchClearsBaseURL(t *testing.T) {
	route := map[string]any{"base_url": "https://chatgpt.com/backend-api/codex", "api_mode": "codex_responses"}
	seedHome := func(model map[string]any) *setValueHome {
		data, _ := json.Marshal(map[string]any{"model": model, "custom_providers": []any{map[string]any{"name": "mylab", "base_url": "http://10.0.0.5:8000/v1", "api_key": "k"}}})
		return newSetValueHome(t, string(data))
	}
	with := func(base map[string]any, extra map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range base {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	for _, c := range []struct {
		target string
		seed   map[string]any
	}{{"anthropic", route}, {"mylab", route}, {"anthropic", map[string]any{"api_mode": "codex_responses"}}} {
		s := seedHome(with(map[string]any{"provider": "opencode-go", "default": "gpt-5.3-codex"}, c.seed))
		s.mustSet("model.provider", c.target)
		out := s.out.String()
		if !reflect.DeepEqual(s.doc()["model"], map[string]any{"provider": c.target, "default": "gpt-5.3-codex"}) || !strings.Contains(out, "Cleared") || !strings.Contains(out, "opencode-go") {
			t.Fatalf("%s %v %q", c.target, s.doc()["model"], out)
		}
		for k, v := range c.seed {
			if !strings.Contains(out, fmt.Sprintf("model.%s (%v)", k, v)) {
				t.Fatalf("%s missing %s in %q", c.target, k, out)
			}
		}
	}
	for _, c := range []struct {
		key, target string
		seed        map[string]any
	}{
		{"model.default", "gpt-5", with(map[string]any{"provider": "opencode-go"}, route)},
		{"model.provider", "opencode-go", with(map[string]any{"provider": "opencode-go"}, route)},
		{"model.provider", "openai-codex", with(map[string]any{"provider": "opencode-go"}, route)},
		{"model.provider", "mylab", map[string]any{"provider": "openai", "base_url": "http://10.0.0.5:8000/v1"}},
		{"model.provider", "custom", map[string]any{"provider": "openai", "base_url": "https://api.openai.com/v1"}},
		{"model.provider", "anthropic", map[string]any{"provider": "opencode-go", "base_url": "http://proxy.internal:8080/v1"}},
	} {
		s := seedHome(with(c.seed, map[string]any{"default": "m"}))
		s.mustSet(c.key, c.target)
		want := with(c.seed, map[string]any{"default": "m", strings.SplitN(c.key, ".", 2)[1]: c.target})
		out := s.out.String()
		if !reflect.DeepEqual(s.doc()["model"], want) || strings.Contains(out, "Cleared") || strings.Contains(out, "still applies") != strings.Contains(fmt.Sprint(c.seed["base_url"]), "proxy.internal") {
			t.Fatalf("%s=%s %v %q", c.key, c.target, s.doc()["model"], out)
		}
	}
}

func TestConfigUnsetRemovesTheEnvCopyCoreUsedToWrite(t *testing.T) {
	s := newSetValueHome(t, "terminal:\n  backend: singularity\n  singularity_image: docker://old:1\n")
	os.WriteFile(filepath.Join(s.h, ".env"), []byte("TERMINAL_SINGULARITY_IMAGE=docker://old:1\n"), 0o600)
	if err := config.UnsetConfigValue(context.Background(), s.h, "terminal.singularity_image", moaCommandSources()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(s.text(), "singularity_image") || strings.Contains(s.env(), "TERMINAL_SINGULARITY_IMAGE") {
		t.Fatalf("config %q env %q", s.text(), s.env())
	}
}

func TestConfigSetAndGetStillRecognizeTheImageKey(t *testing.T) {
	s := newSetValueHome(t, "terminal:\n  backend: singularity\n")
	s.mustSet("terminal.singularity_image", "docker://python:3.12")
	got := s.get("terminal.singularity_image", false, false)
	if !strings.Contains(got, "docker://python:3.12") || strings.Contains(s.out.String()+s.errOut.String(), "not a recognized config key") {
		t.Fatalf("get %q stderr %q", got, s.errOut.String())
	}
}

// Home with no .env, an absent managed dir and the routing fixture config
func envRoutingHome(t *testing.T, text string) *setValueHome {
	t.Helper()
	s := newSetValueHome(t, text)
	os.Remove(filepath.Join(s.h, ".env"))
	t.Setenv(config.EnvManagedDir, filepath.Join(s.h, "managed"))
	config.InvalidateManagedCache()
	return s
}

func (s *setValueHome) unset(key string) error {
	return config.UnsetConfigValue(context.Background(), s.h, key, moaCommandSources())
}

func (s *setValueHome) assertDoc(want map[string]any) {
	s.t.Helper()
	if got := s.doc(); !config.Equal(got, want) {
		s.t.Fatalf("config %v want %v", got, want)
	}
}

func TestPlatformEnvKeyRoundTripsWithoutAConfigYAMLCopy(t *testing.T) {
	s := envRoutingHome(t, "model:\n  default: test/model\n")
	s.mustSet("FEISHU_HOME_CHANNEL", "oc_ROUTING_TEST")
	s.assertDoc(map[string]any{"model": map[string]any{"default": "test/model"}})
	if s.env() != "FEISHU_HOME_CHANNEL=oc_ROUTING_TEST\n" {
		t.Fatalf("env %q", s.env())
	}
	if got := s.get("FEISHU_HOME_CHANNEL", false, false); !strings.HasSuffix(got, "oc_ROUTING_TEST") {
		t.Fatalf("get %q", got)
	}
	if err := s.unset("FEISHU_HOME_CHANNEL"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(s.env(), "FEISHU_HOME_CHANNEL") {
		t.Fatalf("env %q", s.env())
	}
	s.assertDoc(map[string]any{"model": map[string]any{"default": "test/model"}})
}

func TestRegisteredEnvSettingConvergesStaleConfigYAMLCopy(t *testing.T) {
	s := envRoutingHome(t, "model:\n  default: test/model\nDISCORD_ALLOWED_USERS: '111'\nWHATSAPP_MODE: web\n")
	s.mustSet("DISCORD_ALLOWED_USERS", "222")
	s.mustSet("TAVILY_API_KEY", "tvly-control")
	if !strings.Contains(s.env(), "DISCORD_ALLOWED_USERS=222") || !strings.Contains(s.env(), "TAVILY_API_KEY=tvly-control") {
		t.Fatalf("env %q", s.env())
	}
	s.assertDoc(map[string]any{"model": map[string]any{"default": "test/model"}, "WHATSAPP_MODE": "web"})
	if err := s.unset("WHATSAPP_MODE"); err != nil {
		t.Fatal(err)
	}
	s.assertDoc(map[string]any{"model": map[string]any{"default": "test/model"}})
	if err := config.GetConfigValue(context.Background(), s.h, "WHATSAPP_MODE", false, false, moaCommandSources()); err == nil {
		t.Fatal("get of a removed env setting succeeded")
	}
}

func TestUnregisteredUpperSnakeNameRoutesToEnvByShape(t *testing.T) {
	s := envRoutingHome(t, "model:\n  default: test/model\nTELEGRAM_GROUP_ALLOWED_USERS: '111'\n")
	s.mustSet("TELEGRAM_GROUP_ALLOWED_USERS", "222,333")
	s.mustSet("ICEBATH_TIMEZONE", "Europe/Berlin")
	s.mustSet("my_custom_flag", "hello")
	if !strings.Contains(s.env(), "TELEGRAM_GROUP_ALLOWED_USERS=222,333") || !strings.Contains(s.env(), "ICEBATH_TIMEZONE=Europe/Berlin") {
		t.Fatalf("env %q", s.env())
	}
	s.assertDoc(map[string]any{"model": map[string]any{"default": "test/model"}, "my_custom_flag": "hello"})
	if got := s.get("TELEGRAM_GROUP_ALLOWED_USERS", false, false); got != "222,333" {
		t.Fatalf("get %q", got)
	}
}

func TestEnvWriterDenylistGuardsUpperSnakeNamesAndUnknownNamesGetANote(t *testing.T) {
	s := envRoutingHome(t, "model:\n  default: test/model\n")
	if err := s.set("ICEBATH_YOLO_MODE", "true", true); err == nil || !strings.Contains(s.errOut.String(), "denylist") {
		t.Fatalf("denylisted set returned %v stderr %q", err, s.errOut.String())
	}
	if _, err := os.Stat(filepath.Join(s.h, ".env")); err == nil {
		t.Fatalf("denylisted name created .env %q", s.env())
	}
	s.assertDoc(map[string]any{"model": map[string]any{"default": "test/model"}})
	s.mustSet("SOME_PLUGIN_ONLY_KNOB", "xyz")
	if !strings.Contains(s.env(), "SOME_PLUGIN_ONLY_KNOB=xyz") {
		t.Fatalf("env %q", s.env())
	}
	s.assertDoc(map[string]any{"model": map[string]any{"default": "test/model"}})
}
