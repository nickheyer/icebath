package config

import (
	"context"
	"encoding/json"
	"math/big"
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
	t.Run("test_set_hits_existing_literal_key_no_phantom", func(t *testing.T) {
		s, _ := newCfgSetHome(t, dottedProviderConfig)
		s.mustSet("providers.myprov.models.grok-4.6.supports_vision", "true")
		models, _ := s.at("providers", "myprov", "models").(map[string]any)
		if AsMap(models["grok-4.6"])["supports_vision"] != true {
			t.Fatalf("literal key %v", models)
		}
		if _, has := models["grok-4"]; has {
			t.Fatalf("phantom sibling %v", models)
		}
	})
	t.Run("test_get_reads_real_dotted_key", func(t *testing.T) {
		s, _ := newCfgSetHome(t, dottedProviderConfig)
		s.command(ConfigArgs{Command: "get", Key: "providers.myprov.models.grok-4.6.context_length"})
		if got := strings.TrimSpace(FacadeStdout.(interface{ String() string }).String()); got != "500000" {
			t.Fatalf("get %q", got)
		}
	})
	t.Run("test_unset_removes_real_dotted_key_field", func(t *testing.T) {
		s, _ := newCfgSetHome(t, map[string]any{"providers": map[string]any{"myprov": map[string]any{"models": map[string]any{
			"grok-4.6": map[string]any{"context_length": 500000, "supports_vision": true}}}}})
		s.command(ConfigArgs{Command: "unset", Key: "providers.myprov.models.grok-4.6.supports_vision"})
		target := AsMap(s.at("providers", "myprov", "models", "grok-4.6"))
		if _, has := target["supports_vision"]; has || !pyEq(target["context_length"], 500000) {
			t.Fatalf("unset %v", target)
		}
	})
	t.Run("test_set_refuses_phantom_when_dotted_sibling_exists", func(t *testing.T) {
		s, _ := newCfgSetHome(t, dottedProviderConfig)
		err := s.set("providers.myprov.models.grok-4.7.context_length", "128000")
		errText := FacadeNoticeOut.(interface{ String() string }).String()
		if exitCode(err) < 0 || !strings.Contains(errText, "Refusing to create nested key") || !strings.Contains(errText, "grok-4") {
			t.Fatalf("phantom %v %q", err, errText)
		}
		if _, has := AsMap(s.at("providers", "myprov", "models"))["grok-4"]; has {
			t.Fatal("phantom written")
		}
	})
	t.Run("test_escaped_form_creates_new_dotted_key", func(t *testing.T) {
		s, _ := newCfgSetHome(t, dottedProviderConfig)
		s.mustSet(`providers.myprov.models.grok-4\.7.context_length`, "128000")
		if got := s.at("providers", "myprov", "models", "grok-4.7"); !reflect.DeepEqual(got, map[string]any{"context_length": 128000}) {
			t.Fatalf("escaped create %v", got)
		}
	})
}

func TestMatrixRoomIds(t *testing.T) {
	t.Run("test_set_existing_room_id_key", func(t *testing.T) {
		s, _ := newCfgSetHome(t, map[string]any{"matrix": map[string]any{"channel_prompts": map[string]any{"!TestRoom:example.org": "old"}}})
		s.mustSet("matrix.channel_prompts.!TestRoom:example.org", "be brief")
		if got := s.at("matrix", "channel_prompts"); !reflect.DeepEqual(got, map[string]any{"!TestRoom:example.org": "be brief"}) {
			t.Fatalf("room id %v", got)
		}
	})
	t.Run("test_escaped_creates_new_room_id_key", func(t *testing.T) {
		s, _ := newCfgSetHome(t, map[string]any{"matrix": map[string]any{"channel_prompts": map[string]any{}}})
		s.mustSet(`matrix.channel_prompts.!TestRoom:example\.org`, "be brief")
		if got := s.at("matrix", "channel_prompts", "!TestRoom:example.org"); got != "be brief" {
			t.Fatalf("escaped room id %v", s.raw())
		}
	})
}

func TestCustomProvidersListIndex(t *testing.T) {
	t.Run("test_set_updates_existing_dotted_model_under_list_index", func(t *testing.T) {
		s, _ := newCfgSetHome(t, map[string]any{"custom_providers": []any{map[string]any{"name": "Local Ollama", "models": map[string]any{"qwen3.5:4b": map[string]any{"context_length": 16384}}}}})
		s.mustSet("custom_providers.0.models.qwen3.5:4b.context_length", "65536")
		models := AsMap(AsMap(s.raw()["custom_providers"].([]any)[0])["models"])
		if !reflect.DeepEqual(models, map[string]any{"qwen3.5:4b": map[string]any{"context_length": 65536}}) {
			t.Fatalf("list indexed %v", models)
		}
	})
	t.Run("test_escaped_creates_absent_dotted_model_under_list_index", func(t *testing.T) {
		s, _ := newCfgSetHome(t, map[string]any{"custom_providers": []any{map[string]any{"name": "Local Ollama", "models": map[string]any{}}}})
		s.mustSet(`custom_providers.0.models.qwen3\.5:4b.context_length`, "65536")
		models := AsMap(AsMap(s.raw()["custom_providers"].([]any)[0])["models"])
		if !reflect.DeepEqual(models, map[string]any{"qwen3.5:4b": map[string]any{"context_length": 65536}}) {
			t.Fatalf("escaped list indexed %v", models)
		}
	})
}

func TestAtomicRoundtripYamlUpdate(t *testing.T) {
	t.Run("test_existing_dotted_model_id_not_split", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yaml")
		os.WriteFile(path, []byte("model_overrides:\n  zai:\n    glm-5.3:\n      supports_reasoning: false\n"), 0o600)
		if err := atomicRoundtripYAMLUpdate(context.Background(), path, "model_overrides.zai.glm-5.3.supports_reasoning", true); err != nil {
			t.Fatal(err)
		}
		doc, _ := ReadDocument(path)
		zai := AsMap(AsMap(doc["model_overrides"])["zai"])
		if _, has := zai["glm-5"]; AsMap(zai["glm-5.3"])["supports_reasoning"] != true || has {
			t.Fatalf("existing dotted id %v", doc)
		}
	})
	t.Run("test_escaped_dotted_key_created", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yaml")
		os.WriteFile(path, []byte("model_overrides: {}\n"), 0o600)
		if err := atomicRoundtripYAMLUpdate(context.Background(), path, `model_overrides.zai.glm-5\.3.supports_reasoning`, true); err != nil {
			t.Fatal(err)
		}
		doc, _ := ReadDocument(path)
		if got, _ := CfgGet(doc, "model_overrides", "zai", "glm-5.3", "supports_reasoning"); got != true {
			t.Fatalf("escaped create %v", doc)
		}
	})
	t.Run("test_plain_paths_unchanged", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yaml")
		os.WriteFile(path, []byte("# keep this comment\ndisplay:\n  personality: default\n"), 0o600)
		if err := atomicRoundtripYAMLUpdate(context.Background(), path, "display.personality", "hacker"); err != nil {
			t.Fatal(err)
		}
		text, _ := os.ReadFile(path)
		doc, _ := ReadDocument(path)
		if !strings.Contains(string(text), "# keep this comment") || Tree(doc).String("display.personality", "") != "hacker" {
			t.Fatalf("plain path %s", text)
		}
	})
}

var dottedLeafConfig = map[string]any{"providers": map[string]any{"Bai": map[string]any{"models": map[string]any{"glm-5.3-flash": map[string]any{"context_length": 128000}}}}}

func TestDottedLeafKeys(t *testing.T) {
	t.Run("test_set_leaf_parent_is_dotted", func(t *testing.T) {
		s, _ := newCfgSetHome(t, dottedLeafConfig)
		s.mustSet("providers.Bai.models.glm-5.3-flash.context_length", "1000000")
		models := AsMap(s.at("providers", "Bai", "models"))
		if _, has := models["glm-5"]; has || !pyEq(AsMap(models["glm-5.3-flash"])["context_length"], 1000000) {
			t.Fatalf("dotted parent %v", models)
		}
	})
	t.Run("test_get_dotted_leaf_mapping", func(t *testing.T) {
		s, _ := newCfgSetHome(t, dottedLeafConfig)
		s.command(ConfigArgs{Command: "get", Key: "providers.Bai.models.glm-5.3-flash", JSON: true})
		if out := FacadeStdout.(interface{ String() string }).String(); !strings.Contains(out, "128000") {
			t.Fatalf("dotted leaf get %q", out)
		}
	})
	t.Run("test_unset_dotted_leaf", func(t *testing.T) {
		s, _ := newCfgSetHome(t, dottedLeafConfig)
		s.command(ConfigArgs{Command: "unset", Key: "providers.Bai.models.glm-5.3-flash"})
		if _, has := AsMap(s.at("providers", "Bai", "models"))["glm-5.3-flash"]; has {
			t.Fatalf("dotted leaf unset %v", s.raw())
		}
	})
}

func TestBackwardCompatibility(t *testing.T) {
	t.Run("test_plain_nested_set", func(t *testing.T) {
		s, _ := newCfgSetHome(t, nil)
		s.mustSet("terminal.backend", "docker")
		if s.at("terminal", "backend") != "docker" {
			t.Fatalf("plain set %v", s.raw())
		}
	})
	t.Run("test_plain_nested_get_and_unset_helpers", func(t *testing.T) {
		tree := Tree{"a": map[string]any{"b": map[string]any{"c": 1}}}
		if v, _ := tree.Lookup("a.b.c"); v != 1 || !tree.Unset("a.b.c") {
			t.Fatal("plain get and unset")
		}
		if _, ok := tree.Lookup("a.b.c"); ok {
			t.Fatal("unset value still found")
		}
	})
	t.Run("test_list_index_navigation_unchanged", func(t *testing.T) {
		listed := Tree{"custom_providers": []any{map[string]any{"name": "p1"}}}
		if err := listed.Set("custom_providers.0.name", "p2"); err != nil || AsMap(listed["custom_providers"].([]any)[0])["name"] != "p2" {
			t.Fatalf("list index set %v %v", err, listed)
		}
	})
}

// Python list subscripts, typed write errors, list element pruning and insertion order of new keys
func TestNestedPathPythonSemantics(t *testing.T) {
	cfg := Tree{"custom_providers": []any{map[string]any{"name": "a"}, map[string]any{"name": "b"}}, "label": "x"}
	if v, ok := cfg.Lookup("custom_providers.-1.name"); !ok || v != "b" {
		t.Fatalf("negative index %v %v", v, ok)
	}
	if v, ok := cfg.Lookup("custom_providers. 1 .name"); !ok || v != "b" {
		t.Fatalf("int() spacing %v %v", v, ok)
	}
	if _, ok := cfg.Lookup(""); ok {
		t.Fatal("empty path resolves only a literal empty key")
	}
	cases := []struct{ path, kind, msg string }{
		{"custom_providers.x", "ValueError", "invalid literal for int() with base 10: 'x'"},
		{"custom_providers.5", "IndexError", "list assignment index out of range"},
		{"custom_providers.x.name", "TypeError", "Cannot navigate into list at key 'custom_providers.x.name': segment 'x' is not a numeric index"},
		{"custom_providers.9.name", "IndexError", "list index out of range"},
		{"label.inner.deep", "", ""},
	}
	for _, tc := range cases {
		err := cfg.Clone().Set(tc.path, 1)
		if tc.kind == "" {
			if err != nil {
				t.Fatalf("%s replaces the scalar with a mapping, got %v", tc.path, err)
			}
			continue
		}
		pe, ok := err.(*PathError)
		if !ok || pe.Kind != tc.kind || pe.Msg != tc.msg {
			t.Fatalf("%s: %#v", tc.path, err)
		}
	}
	scalarItems := Tree{"items": []any{"plain"}}
	if err, ok := scalarItems.Set("items.0.name", 1).(*PathError); !ok || err.Kind != "TypeError" || err.Msg != "Cannot navigate into str at key 'items.0.name'" {
		t.Fatalf("scalar item %#v", err)
	}
	pruned := Tree{"custom_providers": []any{map[string]any{"name": "a"}, map[string]any{"only": 1}}}
	if !pruned.Unset("custom_providers.1.only") || len(pruned["custom_providers"].([]any)) != 1 {
		t.Fatalf("emptied list element is deleted %v", pruned)
	}
	shadow := map[string]any{"grok-4.6": 1, "grok-4.5": 2}
	SetMappingKeys(shadow, []string{"grok-4.6", "grok-4.5"})
	if got := phantomSibling(shadow, "grok-4"); got != "grok-4.6" {
		t.Fatalf("first sibling in mapping order, got %s", got)
	}
	fresh := Tree{}
	for _, k := range []string{"zeta", "alpha", "mid"} {
		if err := fresh.Set(k+".x", 1); err != nil {
			t.Fatal(err)
		}
	}
	if got := MappingKeys(fresh); !reflect.DeepEqual(got, []string{"zeta", "alpha", "mid"}) {
		t.Fatalf("insertion order %v", got)
	}
}

func TestConfigSetListValues(t *testing.T) {
	errText := func() string { return FacadeNoticeOut.(interface{ String() string }).String() }
	t.Run("test_list_literal_is_parsed_to_list", func(t *testing.T) {
		s, _ := newCfgSetHome(t, nil)
		s.mustSet("platform_toolsets.line", `["clarify", "file", "web"]`)
		if !reflect.DeepEqual(s.at("platform_toolsets", "line"), []any{"clarify", "file", "web"}) {
			t.Fatalf("%v", s.raw())
		}
	})
	t.Run("test_mapping_literal_is_parsed_to_dict", func(t *testing.T) {
		s, _ := newCfgSetHome(t, nil)
		s.mustSet("mcp_servers.demo.env", `{"terminal": "off"}`)
		if !reflect.DeepEqual(s.at("mcp_servers", "demo", "env"), map[string]any{"terminal": "off"}) {
			t.Fatalf("%v", s.raw())
		}
	})
	t.Run("test_yaml_flow_list_is_parsed", func(t *testing.T) {
		s, _ := newCfgSetHome(t, nil)
		s.mustSet("plugins.enabled", "[model-providers/gemini]")
		if !reflect.DeepEqual(s.at("plugins", "enabled"), []any{"model-providers/gemini"}) {
			t.Fatalf("%v", s.raw())
		}
	})
	t.Run("test_invalid_list_literal_is_refused_and_nothing_written", func(t *testing.T) {
		s, _ := newCfgSetHome(t, nil)
		if err := s.set("platform_toolsets.line", `["unclosed`); exitCode(err) < 0 || !strings.Contains(strings.ToLower(errText()), "not valid yaml/json") {
			t.Fatalf("invalid literal %v %q", err, errText())
		}
		if _, has := s.raw()["platform_toolsets"]; has {
			t.Fatal("invalid literal written")
		}
	})
	t.Run("test_scalar_values_unaffected", func(t *testing.T) {
		s, _ := newCfgSetHome(t, nil)
		s.mustSet("agent.max_turns", "300")
		s.mustSet("display.compact", "true")
		s.mustSet("tts.provider", "edge")
		if !pyEq(s.at("agent", "max_turns"), 300) || s.at("display", "compact") != true || s.at("tts", "provider") != "edge" {
			t.Fatalf("scalars %v", s.raw())
		}
	})
	t.Run("test_multiline_yaml_list_is_parsed", func(t *testing.T) {
		s, _ := newCfgSetHome(t, nil)
		s.mustSet("custom_providers", "- name: foo\n  base_url: https://foo.example/v1\n- name: bar\n  base_url: https://bar.example/v1")
		if !reflect.DeepEqual(s.raw()["custom_providers"], []any{map[string]any{"name": "foo", "base_url": "https://foo.example/v1"}, map[string]any{"name": "bar", "base_url": "https://bar.example/v1"}}) {
			t.Fatalf("%v", s.raw())
		}
	})
	t.Run("test_multiline_yaml_mapping_is_parsed", func(t *testing.T) {
		s, _ := newCfgSetHome(t, nil)
		s.mustSet("mcp_servers.demo.env", "terminal: off\nbrowser: on")
		if !reflect.DeepEqual(s.at("mcp_servers", "demo", "env"), map[string]any{"terminal": false, "browser": true}) {
			t.Fatalf("%v", s.raw())
		}
	})
	stays := func(t *testing.T, key, value string) {
		t.Helper()
		s, _ := newCfgSetHome(t, nil)
		s.mustSet(key, value)
		if got, isStr := s.at(strings.Split(key, ".")...).(string); !isStr || got != value {
			t.Fatalf("%s %q saved %#v", key, value, s.at(strings.Split(key, ".")...))
		}
	}
	t.Run("test_string_typed_key_bracket_value_stays_string", func(t *testing.T) { stays(t, "approvals.mode", "[off]") })
	t.Run("test_string_typed_key_negative_number_stays_string", func(t *testing.T) { stays(t, "approvals.mode", "-5") })
	t.Run("test_dash_prefixed_scalar_not_treated_as_list", func(t *testing.T) { stays(t, "weird.flag", "--verbose") })
	t.Run("test_plain_scalar_that_parses_to_scalar_kept_as_string", func(t *testing.T) {
		stays(t, "some.note", "line one\nline two without yaml shape")
	})
	t.Run("test_round_trip_through_load_config", func(t *testing.T) {
		s, _ := newCfgSetHome(t, nil)
		s.mustSet("platform_toolsets.line", `["clarify", "file", "web"]`)
		cfg, err := Load(s.h, facadeLookup)
		if err != nil || !reflect.DeepEqual(cfg.Get("platform_toolsets.line"), []any{"clarify", "file", "web"}) {
			t.Fatalf("round trip %v %v", err, cfg.Get("platform_toolsets.line"))
		}
	})
	t.Run("test_bare_string_into_list_slot_absent_from_defaults_is_refused", func(t *testing.T) {
		s, _ := newCfgSetHome(t, nil)
		for _, key := range []string{"plugins.enabled", "model_catalog.excluded_providers"} {
			FacadeNoticeOut.(interface{ Reset() }).Reset()
			if err := s.set(key, "a,b"); exitCode(err) < 0 || !strings.Contains(errText(), "must be a list") {
				t.Fatalf("%s bare string %v", key, err)
			}
		}
		if len(s.raw()) != 0 {
			t.Fatalf("refused list slots wrote %v", s.raw())
		}
	})
}

// Int and float coercion follow int(), float() and Decimal on every spelling they accept
func TestConfigSetCoercionFollowsPythonNumberParsers(t *testing.T) {
	big, _ := new(big.Int).SetString("123456789012345678901234567890", 10)
	for in, want := range map[string]any{"1_000": 1000, "\u0661\u0662": 12, "5.": 5.0, "1_0.5": 10.5, "\u0661.\u0665": 1.5,
		"123456789012345678901234567890": big, "1e400": nil, "0.1000000000000000055511151231257827": nil} {
		got, ok := coerceInt(in)
		if !ok {
			got, ok = coerceFloat(in)
		}
		if want == nil {
			if ok {
				t.Fatalf("%q coerced to %#v", in, got)
			}
			continue
		}
		if !ok || !pyEq(got, want) || PyTypeName(got) != PyTypeName(want) {
			t.Fatalf("%q coerced to %#v want %#v", in, got, want)
		}
	}
	if PyTypeName(big) != "int" {
		t.Fatal("a big int is an int")
	}
}

func TestFastSafeLoadResolvesYAML11Scalars(t *testing.T) {
	doc, err := FastSafeLoad([]byte("a: on\nb: off\nc: y\nd: 017\ne: 1_000\nf: 1:30\ng: 0o17\nh: 'off'\ni: \"on\"\nj: 1_000.5\nk: .5e3\non: 1\nl: [No, '5', 0x1f]\nm: 1:30.5\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"a": true, "b": false, "c": true, "d": 15, "e": 1000, "f": 90, "g": "0o17", "h": "off", "i": "on",
		"j": 1000.5, "k": ".5e3", PyKeyString(true): 1, "l": []any{false, "5", 31}, "m": 90.5}
	if !reflect.DeepEqual(doc, want) {
		t.Fatalf("got %#v", doc)
	}
}
