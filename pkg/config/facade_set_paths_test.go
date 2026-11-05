package config

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type cfgSetHome struct {
	t       *testing.T
	h, path string
}

// Fresh facade home seeded with JSON data when given, output captured
func newCfgSetHome(t *testing.T, data map[string]any) (*cfgSetHome, *strings.Builder) {
	t.Helper()
	text := ""
	if data != nil {
		raw, _ := json.Marshal(data)
		text = string(raw)
	}
	h, path := facadeHome(t, text)
	os.WriteFile(filepath.Join(h, ".env"), nil, 0o600)
	out, errOut := captureFacade(t)
	_ = out
	b := &strings.Builder{}
	t.Cleanup(func() { b.WriteString(errOut.String()) })
	return &cfgSetHome{t: t, h: h, path: path}, b
}

func pathCommandSources() ConfigCommandSources {
	src := testCommandSources()
	src.RuntimeKeyOwners = RuntimeKeyOwners{
		IsPresetFieldPath:   func([]string) bool { return false },
		SelectionConfigKeys: func() map[string]bool { return nil },
		ActiveManifests:     func() []PluginConfigKeys { return nil },
	}
	return src
}

func (s *cfgSetHome) set(key, value string) error {
	return SetConfigValue(context.Background(), s.h, key, value, false, pathCommandSources())
}

func (s *cfgSetHome) mustSet(key, value string) {
	s.t.Helper()
	if err := s.set(key, value); err != nil {
		s.t.Fatalf("set %s: %v", key, err)
	}
}

func (s *cfgSetHome) command(args ConfigArgs) {
	s.t.Helper()
	if err := ConfigCommand(context.Background(), s.h, args, pathCommandSources()); err != nil {
		s.t.Fatalf("%s %s: %v", args.Command, args.Key, err)
	}
}

func (s *cfgSetHome) raw() map[string]any {
	s.t.Helper()
	doc, err := ReadDocument(s.path)
	if err != nil {
		s.t.Fatal(err)
	}
	return doc
}

func (s *cfgSetHome) at(keys ...string) any {
	v, _ := CfgGet(s.raw(), keys...)
	return v
}

var dottedProviderConfig = map[string]any{"providers": map[string]any{"myprov": map[string]any{
	"name": "My Provider", "base_url": "https://example.invalid/v1", "default_model": "grok-4.6",
	"models": map[string]any{"grok-4.6": map[string]any{"context_length": 500000}, "grok-4.5": map[string]any{"context_length": 500000}},
}}}

func TestUnescapedDottedModelKeys(t *testing.T) {
	s, _ := newCfgSetHome(t, dottedProviderConfig)
	s.mustSet("providers.myprov.models.grok-4.6.supports_vision", "true")
	models, _ := s.at("providers", "myprov", "models").(map[string]any)
	if AsMap(models["grok-4.6"])["supports_vision"] != true || models["grok-4"] != nil {
		t.Fatalf("literal key %v", models)
	}
	s, _ = newCfgSetHome(t, dottedProviderConfig)
	FacadeStdout.(interface{ Reset() }).Reset()
	s.command(ConfigArgs{Command: "get", Key: "providers.myprov.models.grok-4.6.context_length"})
	if got := strings.TrimSpace(FacadeStdout.(interface{ String() string }).String()); got != "500000" {
		t.Fatalf("get %q", got)
	}
	s, _ = newCfgSetHome(t, map[string]any{"providers": map[string]any{"myprov": map[string]any{"models": map[string]any{
		"grok-4.6": map[string]any{"context_length": 500000, "supports_vision": true}}}}})
	s.command(ConfigArgs{Command: "unset", Key: "providers.myprov.models.grok-4.6.supports_vision"})
	if got := s.at("providers", "myprov", "models", "grok-4.6"); !reflect.DeepEqual(got, map[string]any{"context_length": 500000}) {
		t.Fatalf("unset %v", got)
	}
	s, _ = newCfgSetHome(t, dottedProviderConfig)
	err := s.set("providers.myprov.models.grok-4.7.context_length", "128000")
	errText := FacadeNoticeOut.(interface{ String() string }).String()
	if exitCode(err) < 0 || !strings.Contains(errText, "Refusing to create nested key") || !strings.Contains(errText, "grok-4") {
		t.Fatalf("phantom %v %q", err, errText)
	}
	if AsMap(s.at("providers", "myprov", "models"))["grok-4"] != nil {
		t.Fatal("phantom written")
	}
	s.mustSet(`providers.myprov.models.grok-4\.7.context_length`, "128000")
	if got := s.at("providers", "myprov", "models", "grok-4.7"); !reflect.DeepEqual(got, map[string]any{"context_length": 128000}) {
		t.Fatalf("escaped create %v", got)
	}
}

func TestDottedRoomIdsAndListIndexedModels(t *testing.T) {
	s, _ := newCfgSetHome(t, map[string]any{"matrix": map[string]any{"channel_prompts": map[string]any{"!TestRoom:example.org": "old"}}})
	s.mustSet("matrix.channel_prompts.!TestRoom:example.org", "be brief")
	if got := s.at("matrix", "channel_prompts"); !reflect.DeepEqual(got, map[string]any{"!TestRoom:example.org": "be brief"}) {
		t.Fatalf("room id %v", got)
	}
	s, _ = newCfgSetHome(t, map[string]any{"matrix": map[string]any{"channel_prompts": map[string]any{}}})
	s.mustSet(`matrix.channel_prompts.!TestRoom:example\.org`, "be brief")
	if got := s.at("matrix", "channel_prompts", "!TestRoom:example.org"); got != "be brief" {
		t.Fatalf("escaped room id %v", s.raw())
	}
	s, _ = newCfgSetHome(t, map[string]any{"custom_providers": []any{map[string]any{"name": "Local Ollama", "models": map[string]any{"qwen3.5:4b": map[string]any{"context_length": 16384}}}}})
	s.mustSet("custom_providers.0.models.qwen3.5:4b.context_length", "65536")
	models := AsMap(AsMap(s.raw()["custom_providers"].([]any)[0])["models"])
	if !reflect.DeepEqual(models, map[string]any{"qwen3.5:4b": map[string]any{"context_length": 65536}}) {
		t.Fatalf("list indexed %v", models)
	}
	s, _ = newCfgSetHome(t, map[string]any{"custom_providers": []any{map[string]any{"name": "Local Ollama", "models": map[string]any{}}}})
	s.mustSet(`custom_providers.0.models.qwen3\.5:4b.context_length`, "65536")
	models = AsMap(AsMap(s.raw()["custom_providers"].([]any)[0])["models"])
	if !reflect.DeepEqual(models, map[string]any{"qwen3.5:4b": map[string]any{"context_length": 65536}}) {
		t.Fatalf("escaped list indexed %v", models)
	}
}

func TestAtomicRoundtripYAMLUpdateDottedKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	os.WriteFile(path, []byte("model_overrides:\n  zai:\n    glm-5.3:\n      supports_reasoning: false\n"), 0o600)
	if err := atomicRoundtripYAMLUpdate(path, "model_overrides.zai.glm-5.3.supports_reasoning", true); err != nil {
		t.Fatal(err)
	}
	doc, _ := ReadDocument(path)
	if zai := AsMap(AsMap(doc["model_overrides"])["zai"]); AsMap(zai["glm-5.3"])["supports_reasoning"] != true || zai["glm-5"] != nil {
		t.Fatalf("existing dotted id %v", doc)
	}
	os.WriteFile(path, []byte("model_overrides: {}\n"), 0o600)
	if err := atomicRoundtripYAMLUpdate(path, `model_overrides.zai.glm-5\.3.supports_reasoning`, true); err != nil {
		t.Fatal(err)
	}
	if doc, _ = ReadDocument(path); Tree(doc).Get(`model_overrides.zai.glm-5\.3.supports_reasoning`) != true {
		t.Fatalf("escaped create %v", doc)
	}
	os.WriteFile(path, []byte("# keep this comment\ndisplay:\n  personality: default\n"), 0o600)
	if err := atomicRoundtripYAMLUpdate(path, "display.personality", "hacker"); err != nil {
		t.Fatal(err)
	}
	text, _ := os.ReadFile(path)
	if doc, _ = ReadDocument(path); !strings.Contains(string(text), "# keep this comment") || Tree(doc).String("display.personality", "") != "hacker" {
		t.Fatalf("plain path %s", text)
	}
}

func TestDottedLeafKeysAndPlainPaths(t *testing.T) {
	cfg := map[string]any{"providers": map[string]any{"Bai": map[string]any{"models": map[string]any{"glm-5.3-flash": map[string]any{"context_length": 128000}}}}}
	s, _ := newCfgSetHome(t, cfg)
	s.mustSet("providers.Bai.models.glm-5.3-flash.context_length", "1000000")
	if got := s.at("providers", "Bai", "models"); !reflect.DeepEqual(got, map[string]any{"glm-5.3-flash": map[string]any{"context_length": 1000000}}) {
		t.Fatalf("dotted parent %v", got)
	}
	s, _ = newCfgSetHome(t, cfg)
	FacadeStdout.(interface{ Reset() }).Reset()
	s.command(ConfigArgs{Command: "get", Key: "providers.Bai.models.glm-5.3-flash", JSON: true})
	if out := FacadeStdout.(interface{ String() string }).String(); !strings.Contains(out, "128000") {
		t.Fatalf("dotted leaf get %q", out)
	}
	s.command(ConfigArgs{Command: "unset", Key: "providers.Bai.models.glm-5.3-flash"})
	if _, has := AsMap(s.at("providers", "Bai", "models"))["glm-5.3-flash"]; has {
		t.Fatalf("dotted leaf unset %v", s.raw())
	}
	s, _ = newCfgSetHome(t, nil)
	s.mustSet("terminal.backend", "docker")
	if s.at("terminal", "backend") != "docker" {
		t.Fatalf("plain set %v", s.raw())
	}
	tree := Tree{"a": map[string]any{"b": map[string]any{"c": 1}}}
	if v, _ := tree.Lookup("a.b.c"); v != 1 || !tree.Unset("a.b.c") {
		t.Fatal("plain get and unset")
	}
	if _, ok := tree.Lookup("a.b.c"); ok {
		t.Fatal("unset value still found")
	}
	listed := Tree{"custom_providers": []any{map[string]any{"name": "p1"}}}
	if err := listed.Set("custom_providers.0.name", "p2"); err != nil || AsMap(listed["custom_providers"].([]any)[0])["name"] != "p2" {
		t.Fatalf("list index set %v %v", err, listed)
	}
}

func TestConfigSetListValues(t *testing.T) {
	s, _ := newCfgSetHome(t, nil)
	s.mustSet("platform_toolsets.line", `["clarify", "file", "web"]`)
	s.mustSet("mcp_servers.demo.env", `{"terminal": "off"}`)
	s.mustSet("plugins.enabled", "[model-providers/gemini]")
	if !reflect.DeepEqual(s.at("platform_toolsets", "line"), []any{"clarify", "file", "web"}) ||
		!reflect.DeepEqual(s.at("mcp_servers", "demo", "env"), map[string]any{"terminal": "off"}) ||
		!reflect.DeepEqual(s.at("plugins", "enabled"), []any{"model-providers/gemini"}) {
		t.Fatalf("literals %v", s.raw())
	}
	cfg, err := Load(s.h, facadeLookup)
	if err != nil || !reflect.DeepEqual(cfg.Get("platform_toolsets.line"), []any{"clarify", "file", "web"}) {
		t.Fatalf("round trip %v %v", err, cfg.Get("platform_toolsets.line"))
	}
	s, _ = newCfgSetHome(t, nil)
	if err := s.set("platform_toolsets.line", `["unclosed`); exitCode(err) < 0 || !strings.Contains(strings.ToLower(FacadeNoticeOut.(interface{ String() string }).String()), "not valid yaml/json") {
		t.Fatalf("invalid literal %v", err)
	}
	if _, has := s.raw()["platform_toolsets"]; has {
		t.Fatal("invalid literal written")
	}
	s.mustSet("agent.max_turns", "300")
	s.mustSet("display.compact", "true")
	s.mustSet("tts.provider", "edge")
	if !pyEq(s.at("agent", "max_turns"), 300) || s.at("display", "compact") != true || s.at("tts", "provider") != "edge" {
		t.Fatalf("scalars %v", s.raw())
	}
	s, _ = newCfgSetHome(t, nil)
	s.mustSet("custom_providers", "- name: foo\n  base_url: https://foo.example/v1\n- name: bar\n  base_url: https://bar.example/v1")
	s.mustSet("mcp_servers.demo.env", "terminal: off\nbrowser: on")
	if !reflect.DeepEqual(s.raw()["custom_providers"], []any{map[string]any{"name": "foo", "base_url": "https://foo.example/v1"}, map[string]any{"name": "bar", "base_url": "https://bar.example/v1"}}) ||
		!reflect.DeepEqual(s.at("mcp_servers", "demo", "env"), map[string]any{"terminal": false, "browser": true}) {
		t.Fatalf("multiline %v", s.raw())
	}
	for _, c := range [][3]string{{"approvals.mode", "[off]", "[off]"}, {"approvals.mode", "-5", "-5"}, {"weird.flag", "--verbose", "--verbose"}, {"some.note", "line one\nline two without yaml shape", "line one\nline two without yaml shape"}} {
		s, _ = newCfgSetHome(t, nil)
		s.mustSet(c[0], c[1])
		if got := s.at(strings.Split(c[0], ".")...); got != c[2] {
			t.Fatalf("%s %q saved %#v", c[0], c[1], got)
		}
	}
	s, _ = newCfgSetHome(t, nil)
	for _, key := range []string{"plugins.enabled", "model_catalog.excluded_providers"} {
		buf := FacadeNoticeOut.(interface{ Reset() })
		buf.Reset()
		if err := s.set(key, "a,b"); exitCode(err) < 0 || !strings.Contains(FacadeNoticeOut.(interface{ String() string }).String(), "must be a list") {
			t.Fatalf("%s bare string %v", key, err)
		}
	}
	if len(s.raw()) != 0 {
		t.Fatalf("refused list slots wrote %v", s.raw())
	}
}

func TestFastSafeLoadResolvesYAML11Scalars(t *testing.T) {
	doc, err := FastSafeLoad([]byte("a: on\nb: off\nc: y\nd: 017\ne: 1_000\nf: 1:30\ng: 0o17\nh: 'off'\ni: \"on\"\nj: 1_000.5\nk: .5e3\non: 1\nl: [No, '5', 0x1f]\nm: 1:30.5\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"a": true, "b": false, "c": true, "d": 15, "e": 1000, "f": 90, "g": "0o17", "h": "off", "i": "on",
		"j": 1000.5, "k": ".5e3", "on": 1, "l": []any{false, "5", 31}, "m": 90.5}
	if !reflect.DeepEqual(doc, want) {
		t.Fatalf("got %#v", doc)
	}
}
