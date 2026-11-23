package provider

import (
	"bytes"
	"context"
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

func (s *setValueHome) set(key, value string) error {
	return config.SetConfigValue(context.Background(), s.h, key, value, moaCommandSources())
}

func (s *setValueHome) mustSet(key, value string) {
	s.t.Helper()
	if err := s.set(key, value); err != nil {
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

func TestSetValueEchoMasksSecret(t *testing.T) {
	s := newSetValueHome(t, "")
	secret := "cfut_ANOTHERSECRET0987654321zyxwvu"
	s.mustSet("model.api_key", secret)
	if strings.Contains(s.out.String(), secret) || !strings.Contains(s.out.String(), "Set model.api_key") {
		t.Fatalf("echo %q", s.out)
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
	s.mustSet("display.skin", "contrast")
	if !strings.Contains(s.text(), "skin: contrast") {
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

func TestSetValueLiteralDotKeyEscaping(t *testing.T) {
	seed := "providers:\n  qwen3.5-397b-wafer-non-zdr:\n    base_url: https://pass.wafer.ai/v1\n  openrouter:\n    api_key: or-keep\n"
	s := newSetValueHome(t, seed)
	s.mustSet(`providers.qwen3\.5-397b-wafer-non-zdr.extra_headers`, `{"Wafer-ZDR": "required"}`)
	providers, _ := s.doc()["providers"].(map[string]any)
	want := map[string]any{"base_url": "https://pass.wafer.ai/v1", "extra_headers": map[string]any{"Wafer-ZDR": "required"}}
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
	s = newSetValueHome(t, "providers:\n  qwen3.5-397b-wafer-non-zdr:\n    base_url: https://pass.wafer.ai/v1\n    extra_headers: '{\"K\": \"V\"}'\n")
	if err := s.command(config.ConfigArgs{Command: "unset", Key: `providers.qwen3\.5-397b-wafer-non-zdr.extra_headers`}); err != nil {
		t.Fatal(err)
	}
	if got := s.at("providers", "qwen3.5-397b-wafer-non-zdr"); !reflect.DeepEqual(got, map[string]any{"base_url": "https://pass.wafer.ai/v1"}) {
		t.Fatalf("nested unset %v", got)
	}
	s = newSetValueHome(t, "providers:\n  qwen3.5-397b:\n    base_url: https://pass.wafer.ai/v1\n")
	if got := s.get(`providers.qwen3\.5-397b.base_url`, false, false); got != "https://pass.wafer.ai/v1" {
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
	if got := s.doc(); !reflect.DeepEqual(got, want) {
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
	if !strings.Contains(s.env(), "TELEGRAM_GROUP_ALLOWED_USERS=222,333") || !strings.Contains(s.env(), "ICEBATH_TIMEZONE=Europe/Berlin") {
		t.Fatalf("env %q", s.env())
	}
	s.assertDoc(map[string]any{"model": map[string]any{"default": "test/model"}})
	if got := s.get("TELEGRAM_GROUP_ALLOWED_USERS", false, false); got != "222,333" {
		t.Fatalf("get %q", got)
	}
}

func TestEnvWriterDenylistGuardsUpperSnakeNamesAndUnknownNamesGetANote(t *testing.T) {
	s := envRoutingHome(t, "model:\n  default: test/model\n")
	if err := s.set("ICEBATH_YOLO_MODE", "true"); err == nil || !strings.Contains(s.errOut.String(), "denylist") {
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
