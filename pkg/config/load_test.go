package config

import (
	"reflect"
	"testing"
)

func TestNormalizeRootModelKeysFoldsRootProviderIntoModel(t *testing.T) {
	doc := NormalizeRootModelKeys(map[string]any{"provider": "openrouter", "model": "gpt-x"})
	model, _ := doc["model"].(map[string]any)
	if model["default"] != "gpt-x" || model["provider"] != "openrouter" {
		t.Fatalf("model section %v", doc)
	}
	plain := map[string]any{"model": map[string]any{"default": "m", "provider": "auto"}}
	if got := NormalizeRootModelKeys(plain); got["model"].(map[string]any)["default"] != "m" {
		t.Fatalf("got %v", got)
	}
}

func TestRootLevelProviderOverride(t *testing.T) {
	model := func(doc map[string]any) map[string]any { return doc["model"].(map[string]any) }
	t.Run("test_normalize_root_model_keys_moves_to_model", func(t *testing.T) {
		result := normalizeModelKeys(map[string]any{"provider": "opencode-go", "base_url": "https://example.com/v1",
			"model": map[string]any{"default": "some-model"}})
		_, hasProvider := result["provider"]
		_, hasBase := result["base_url"]
		if hasProvider || hasBase || model(result)["provider"] != "opencode-go" || model(result)["base_url"] != "https://example.com/v1" {
			t.Fatalf("%v", result)
		}
	})
	t.Run("test_normalize_root_model_keys_does_not_override_existing", func(t *testing.T) {
		result := normalizeModelKeys(map[string]any{"provider": "stale-provider",
			"model": map[string]any{"default": "some-model", "provider": "correct-provider"}})
		if _, has := result["provider"]; has || model(result)["provider"] != "correct-provider" {
			t.Fatalf("%v", result)
		}
	})
	t.Run("test_normalize_model_alias_to_default", func(t *testing.T) {
		result := normalizeModelKeys(map[string]any{"model": map[string]any{"model": "via-model-key"}})
		if _, has := model(result)["model"]; has || model(result)["default"] != "via-model-key" {
			t.Fatalf("%v", result)
		}
	})
	t.Run("test_normalize_model_wins_over_name", func(t *testing.T) {
		result := normalizeModelKeys(map[string]any{"model": map[string]any{"model": "m-key", "name": "n-key"}})
		_, hasModel := model(result)["model"]
		_, hasName := model(result)["name"]
		if hasModel || hasName || model(result)["default"] != "m-key" {
			t.Fatalf("%v", result)
		}
	})
	t.Run("test_nested_dict_default_flattens_model_and_provider", func(t *testing.T) {
		result := normalizeModelKeys(map[string]any{"model": map[string]any{
			"default": map[string]any{"provider": "nous", "model": "nested-default-model"}}})
		if model(result)["default"] != "nested-default-model" || model(result)["provider"] != "nous" {
			t.Fatalf("%v", result)
		}
	})
	t.Run("test_nested_dict_default_provider_wins_over_auto", func(t *testing.T) {
		result := normalizeModelKeys(map[string]any{"model": map[string]any{
			"default": map[string]any{"provider": "nous", "model": "nested-default-model"}, "provider": "auto"}})
		if model(result)["default"] != "nested-default-model" || model(result)["provider"] != "nous" {
			t.Fatalf("%v", result)
		}
	})
	t.Run("test_nested_dict_default_never_overrides_explicit_provider", func(t *testing.T) {
		result := normalizeModelKeys(map[string]any{"model": map[string]any{
			"default": map[string]any{"provider": "nous", "model": "nested-default-model"}, "provider": "anthropic"}})
		if model(result)["default"] != "nested-default-model" || model(result)["provider"] != "anthropic" {
			t.Fatalf("%v", result)
		}
	})
	t.Run("test_nested_dict_model_alias_flattens_to_default", func(t *testing.T) {
		result := normalizeModelKeys(map[string]any{"model": map[string]any{
			"model": map[string]any{"provider": "openai", "model": "nested-alias-model"}}})
		if _, has := model(result)["model"]; has || model(result)["default"] != "nested-alias-model" || model(result)["provider"] != "openai" {
			t.Fatalf("%v", result)
		}
	})
	t.Run("test_flat_string_default_untouched", func(t *testing.T) {
		result := normalizeModelKeys(map[string]any{"model": map[string]any{"default": "flat-default-model", "provider": "auto"}})
		if model(result)["default"] != "flat-default-model" || model(result)["provider"] != "auto" {
			t.Fatalf("%v", result)
		}
	})
}

// Python truthiness decides every alias and root fold, a scalar model kept verbatim
func TestNormalizeRootModelKeysFollowsPythonTruthiness(t *testing.T) {
	untouched := map[string]any{"model": map[string]any{"default": "m", "provider": nil, "name": 0, "api_base": false}, "base_url": 0}
	if got := normalizeModelKeys(untouched); reflect.ValueOf(got).Pointer() != reflect.ValueOf(untouched).Pointer() {
		t.Fatalf("falsy aliases and a null provider need no work %v", got)
	}
	scalar := normalizeModelKeys(map[string]any{"model": "  spaced  ", "provider": 7})
	if m := scalar["model"].(map[string]any); m["default"] != "  spaced  " || m["provider"] != "7" {
		t.Fatalf("scalar model keeps its text and the provider becomes a string %v", scalar)
	}
	falsy := normalizeModelKeys(map[string]any{"model": 0, "provider": "p"})
	if m := falsy["model"].(map[string]any); !reflect.DeepEqual(m, map[string]any{"provider": "p"}) {
		t.Fatalf("falsy scalar model is dropped %v", falsy)
	}
	flag := normalizeModelKeys(map[string]any{"model": map[string]any{"default": map[string]any{"model": "", "default": "fallback", "provider": true}}})
	if m := flag["model"].(map[string]any); m["default"] != "fallback" || m["provider"] != "True" {
		t.Fatalf("empty nested model falls back to default and the provider is str() %v", flag)
	}
	ordered := normalizeModelKeys(map[string]any{"model": map[string]any{"name": "n"}, "provider": "p", "base_url": "u"})
	if got := MappingKeys(ordered["model"].(map[string]any)); !reflect.DeepEqual(got, []string{"provider", "base_url", "default"}) {
		t.Fatalf("new model keys follow insertion order %v", got)
	}
	if m, p := SplitModelDefault(map[string]any{"model": 0, "default": 1.5, "provider": nil}); m != "1.5" || p != "" {
		t.Fatalf("split %q %q", m, p)
	}
}

// Stripped documents keep the mapping order of the input like the dict comprehension upstream
func TestStripDefaultsKeepsMappingOrder(t *testing.T) {
	inner := map[string]any{"b": 2, "a": 1, "same": true}
	SetMappingKeys(inner, []string{"b", "a", "same"})
	doc := map[string]any{"zeta": inner, "alpha": 1, "_config_version": 9}
	SetMappingKeys(doc, []string{"zeta", "alpha", "_config_version"})
	out := StripDefaults(doc, map[string]any{"alpha": 1, "zeta": map[string]any{"same": true}, "_config_version": 9}, nil)
	if got := MappingKeys(out); !reflect.DeepEqual(got, []string{"zeta", "_config_version"}) {
		t.Fatalf("root order %v", got)
	}
	if got := MappingKeys(out["zeta"].(map[string]any)); !reflect.DeepEqual(got, []string{"b", "a"}) {
		t.Fatalf("nested order %v", got)
	}
}
