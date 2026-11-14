package dashboard

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Temp home for a config case with every cached config read dropped
func configCaseHome(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	return h
}

// Saves a config document into the temp home the way save_config does
func configCaseSave(t *testing.T, h string, cfg map[string]any) {
	t.Helper()
	if err := config.SaveConfig(context.Background(), h, cfg, config.SaveOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestTimezoneFieldIsSearchableSelect(t *testing.T) {
	configCaseHome(t)
	raw, _ := configSchema().Get("timezone")
	entry := raw.(*pyjson.Object)
	if entry.GetOr("type", nil) != "select" || entry.GetOr("searchable", nil) != true || entry.GetOr("clearable", nil) != true {
		t.Fatalf("entry %v", entry)
	}
	var options []string
	for _, o := range entry.GetOr("options", nil).([]any) {
		options = append(options, o.(string))
	}
	if len(options) < 1 || !sort.StringsAreSorted(options) || slices.Contains(options, "") || !slices.Contains(options, "UTC") {
		t.Fatalf("options %d sorted %v", len(options), sort.StringsAreSorted(options))
	}
	if len(timezoneOptions()) < 1 {
		t.Fatal("timezone options empty")
	}
}

func TestDynamicMergeRecomputesMemoryProviderOptions(t *testing.T) {
	h := configCaseHome(t)
	configCaseSave(t, h, map[string]any{"memory": map[string]any{"provider": "honcho"}})
	frozen := configSchema()
	prev := memoryProviderOptions
	memoryProviderOptions = func(context.Context) []string { return []string{"", "honcho", "mem0", "freshly_installed"} }
	t.Cleanup(func() { memoryProviderOptions = prev })
	fields := schemaWithDynamicProviderOptions(context.Background())
	raw, _ := fields.Get("memory.provider")
	entry := raw.(*pyjson.Object)
	if !slices.Contains(entry.GetOr("options", nil).([]any), any("freshly_installed")) {
		t.Fatalf("options %v", entry.GetOr("options", nil))
	}
	if entry.GetOr("type", nil) != "select" {
		t.Fatalf("type %v", entry.GetOr("type", nil))
	}
	if original, _ := frozen.Get("memory.provider"); original == raw {
		t.Fatal("the schema entry was mutated in place")
	}
}

func TestNormalizeExtractsContextLengthFromDict(t *testing.T) {
	configCaseHome(t)
	result := normalizeConfigForWeb(map[string]any{"model": map[string]any{
		"default": "anthropic/claude-opus-4.6", "provider": "openrouter", "context_length": 200000}})
	if result["model"] != "anthropic/claude-opus-4.6" || result["model_context_length"] != 200000 {
		t.Fatalf("result %v", result)
	}
}

func TestNormalizeBareStringModelYieldsZero(t *testing.T) {
	configCaseHome(t)
	result := normalizeConfigForWeb(map[string]any{"model": "anthropic/claude-sonnet-4"})
	if result["model"] != "anthropic/claude-sonnet-4" || result["model_context_length"] != int64(0) {
		t.Fatalf("result %v", result)
	}
}

// Context length of a denormalized model block as a Python int
func modelContextLength(t *testing.T, result map[string]any) any {
	t.Helper()
	model, ok := result["model"].(map[string]any)
	if !ok {
		t.Fatalf("model is not a dict: %v", result["model"])
	}
	return pyjson.Plain(configJSON(model["context_length"]))
}

func TestDenormalizeWritesContextLengthIntoModelDict(t *testing.T) {
	h := configCaseHome(t)
	configCaseSave(t, h, map[string]any{"model": map[string]any{"default": "anthropic/claude-opus-4.6", "provider": "openrouter"}})
	result, err := denormalizeConfigFromWeb(context.Background(), map[string]any{
		"model": "anthropic/claude-opus-4.6", "model_context_length": int64(100000)})
	if err != nil {
		t.Fatal(err)
	}
	if !pyjson.Equal(modelContextLength(t, result), int64(100000)) {
		t.Fatalf("context_length %v", modelContextLength(t, result))
	}
	if _, held := result["model_context_length"]; held {
		t.Fatal("virtual field kept")
	}
}

func TestDenormalizeContextLengthAloneIsApplied(t *testing.T) {
	h := configCaseHome(t)
	configCaseSave(t, h, map[string]any{"model": map[string]any{"default": "anthropic/claude-sonnet-4", "provider": "anthropic",
		"context_length": 100000}})
	result, err := denormalizeConfigFromWeb(context.Background(), map[string]any{"model_context_length": int64(200000)})
	if err != nil {
		t.Fatal(err)
	}
	if !pyjson.Equal(modelContextLength(t, result), int64(200000)) || result["model"].(map[string]any)["default"] != "anthropic/claude-sonnet-4" {
		t.Fatalf("result %v", result["model"])
	}
}

func TestDenormalizeModelAlonePreservesContextLength(t *testing.T) {
	h := configCaseHome(t)
	configCaseSave(t, h, map[string]any{"model": map[string]any{"default": "anthropic/claude-sonnet-4", "context_length": 150000}})
	result, err := denormalizeConfigFromWeb(context.Background(), map[string]any{"model": "anthropic/claude-opus-4.6"})
	if err != nil {
		t.Fatal(err)
	}
	if !pyjson.Equal(modelContextLength(t, result), int64(150000)) || result["model"].(map[string]any)["default"] != "anthropic/claude-opus-4.6" {
		t.Fatalf("result %v", result["model"])
	}
}

// Single profile home whose only credential is an OpenRouter key, so provider_has_credentials answers for openrouter alone
func openrouterOnlyHome(t *testing.T) string {
	t.Helper()
	h := configCaseHome(t)
	if err := os.WriteFile(filepath.Join(h, ".env"), []byte("OPENROUTER_API_KEY=sk-or-test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENROUTER_API_KEY", "sk-or-test")
	if provider.MultiplexProcess() {
		provider.SetMultiplexActive(false)
		t.Cleanup(func() { provider.SetMultiplexActive(true) })
	}
	return h
}

func TestVendorSlugSwitchesOffNonAggregatorProvider(t *testing.T) {
	h := openrouterOnlyHome(t)
	configCaseSave(t, h, map[string]any{"model": map[string]any{"default": "llama3.2", "provider": "ollama-local",
		"base_url": "http://localhost:11434/v1", "api_mode": "chat_completions"}})
	result, err := denormalizeConfigFromWeb(context.Background(), map[string]any{"model": "google/gemini-2.5-flash"})
	if err != nil {
		t.Fatal(err)
	}
	model := result["model"].(map[string]any)
	if model["provider"] != "openrouter" || model["default"] != "google/gemini-2.5-flash" {
		t.Fatalf("model %v", model)
	}
	if model["base_url"] == "http://localhost:11434/v1" {
		t.Fatal("stale local base_url carried over")
	}
}

func TestContextLengthOverrideSurvivesProviderSwitch(t *testing.T) {
	h := openrouterOnlyHome(t)
	configCaseSave(t, h, map[string]any{"model": map[string]any{"default": "llama3.2", "provider": "ollama-local"}})
	result, err := denormalizeConfigFromWeb(context.Background(), map[string]any{
		"model": "google/gemini-2.5-flash", "model_context_length": int64(128000)})
	if err != nil {
		t.Fatal(err)
	}
	model := result["model"].(map[string]any)
	if model["provider"] != "openrouter" || !pyjson.Equal(modelContextLength(t, result), int64(128000)) {
		t.Fatalf("model %v", model)
	}
}
