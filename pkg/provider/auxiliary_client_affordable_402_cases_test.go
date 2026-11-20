package provider

import (
	"fmt"
	"testing"
)

// True when the kwargs carry the cap under either OpenAI output cap key
func auxKwargsCarryCap(kwargs map[string]any, want int) bool {
	return fmt.Sprint(kwargs["max_tokens"]) == fmt.Sprint(want) || fmt.Sprint(kwargs["max_completion_tokens"]) == fmt.Sprint(want)
}

// True when neither OpenAI output cap key is present
func auxKwargsOmitCap(kwargs map[string]any) bool {
	_, a := kwargs["max_tokens"]
	_, b := kwargs["max_completion_tokens"]
	return !a && !b
}

func TestOpenRouterMaxTokensPreservedOpenrouterProviderIncludesMaxTokens(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	kwargs, err := auxBuildCallKwargs(ctx, "openrouter", "openai/gpt-4o-mini", []any{map[string]any{"role": "user", "content": "test"}}, auxCallKwargsOptions{MaxTokens: 2000})
	if err != nil {
		t.Fatal(err)
	}
	if !auxKwargsCarryCap(kwargs, 2000) {
		t.Fatalf("kwargs %v", kwargs)
	}
}

func TestOpenRouterMaxTokensPreservedOpenrouterBaseUrlIncludesMaxTokens(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	kwargs, err := auxBuildCallKwargs(ctx, "openai", "google/gemini-3.6-flash", []any{map[string]any{"role": "user", "content": "test"}},
		auxCallKwargsOptions{MaxTokens: 2000, BaseURL: "https://openrouter.ai/api/v1"})
	if err != nil {
		t.Fatal(err)
	}
	if !auxKwargsCarryCap(kwargs, 2000) {
		t.Fatalf("kwargs %v", kwargs)
	}
}

func TestOpenRouterMaxTokensPreservedGenericProviderStillOmitsMaxTokens(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	kwargs, err := auxBuildCallKwargs(ctx, "openai", "gpt-4o-mini", []any{map[string]any{"role": "user", "content": "test"}}, auxCallKwargsOptions{MaxTokens: 2000})
	if err != nil {
		t.Fatal(err)
	}
	if !auxKwargsOmitCap(kwargs) {
		t.Fatalf("kwargs %v", kwargs)
	}
}

func TestOpenRouterMaxTokensPreservedNoneMaxTokensNeverIncludedForOpenrouter(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	kwargs, err := auxBuildCallKwargs(ctx, "openrouter", "test-model", []any{map[string]any{"role": "user", "content": "test"}}, auxCallKwargsOptions{MaxTokens: nil})
	if err != nil {
		t.Fatal(err)
	}
	if !auxKwargsOmitCap(kwargs) {
		t.Fatalf("kwargs %v", kwargs)
	}
}
