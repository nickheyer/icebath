package config

import "testing"

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
