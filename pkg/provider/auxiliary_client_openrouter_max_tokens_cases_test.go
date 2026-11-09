package provider

import "testing"

func auxOpenRouterMaxTokensMessages() []any {
	return []any{map[string]any{"role": "user", "content": "test"}}
}

func TestOpenRouterMaxTokensOpenrouterProviderIncludesMaxTokens(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	kwargs, err := auxBuildCallKwargs(ctx, "openrouter", "openai/gpt-4o-mini", auxOpenRouterMaxTokensMessages(), auxCallKwargsOptions{MaxTokens: 2000})
	if err != nil {
		t.Fatal(err)
	}
	if !auxKwargsCarryCap(kwargs, 2000) {
		t.Fatalf("kwargs %v", kwargs)
	}
}

func TestOpenRouterMaxTokensOpenrouterBaseUrlIncludesMaxTokens(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	kwargs, err := auxBuildCallKwargs(ctx, "openai", "gpt-4o-mini", auxOpenRouterMaxTokensMessages(),
		auxCallKwargsOptions{MaxTokens: 2000, BaseURL: "https://openrouter.ai/api/v1"})
	if err != nil {
		t.Fatal(err)
	}
	if !auxKwargsCarryCap(kwargs, 2000) {
		t.Fatalf("kwargs %v", kwargs)
	}
}

func TestOpenRouterMaxTokensGenericProviderOmitsMaxTokens(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	kwargs, err := auxBuildCallKwargs(ctx, "openai", "gpt-4o-mini", auxOpenRouterMaxTokensMessages(), auxCallKwargsOptions{MaxTokens: 2000})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := kwargs["max_tokens"]; ok {
		t.Fatalf("kwargs %v", kwargs)
	}
}

func TestOpenRouterMaxTokensAnthropicCompatStillIncludesMaxTokens(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	kwargs, err := auxBuildCallKwargs(ctx, "minimax", "MiniMax-Text-01", auxOpenRouterMaxTokensMessages(), auxCallKwargsOptions{MaxTokens: 4000})
	if err != nil {
		t.Fatal(err)
	}
	if kwargs["max_tokens"] != 4000 {
		t.Fatalf("kwargs %v", kwargs)
	}
}

func TestOpenRouterMaxTokensNoneMaxTokensNeverIncluded(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	for _, tc := range []struct{ provider, base string }{{"openrouter", ""}, {"openai", "https://openrouter.ai/api/v1"}, {"minimax", ""}} {
		kwargs, err := auxBuildCallKwargs(ctx, tc.provider, "test-model", auxOpenRouterMaxTokensMessages(), auxCallKwargsOptions{MaxTokens: nil, BaseURL: tc.base})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := kwargs["max_tokens"]; ok {
			t.Fatalf("max_tokens should not be set when None for %s", tc.provider)
		}
	}
}
