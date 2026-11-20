package provider

import (
	"fmt"
	"strings"
	"testing"
)

func auxCacheKeyLiveRuntime(model string, apiKey any) map[string]any {
	return map[string]any{"provider": "custom:llama-swap", "model": model, "base_url": "http://llama-swap.test/v1",
		"api_key": apiKey, "api_mode": "chat_completions", "auth_mode": "api_key"}
}

func TestRuntimeContextTokenRestoresPreviousValueAfterTurn(t *testing.T) {
	auxIsolateClientCache(t)
	ctx := ClearRuntimeMain(auxMainTestHome(t, ""))
	turn := SetRuntimeMain(ctx, "custom:llama-swap", "turn-model", RuntimeMainOptions{BaseURL: "http://llama-swap.test/v1", APIKey: "local-key", APIMode: "chat_completions", AuthMode: "api_key"})
	if auxNormalizeMainRuntime(turn, nil)["model"] != "turn-model" {
		t.Fatalf("turn runtime %v", auxNormalizeMainRuntime(turn, nil))
	}
	if got := auxNormalizeMainRuntime(ctx, nil); len(got) != 0 {
		t.Fatalf("previous context %v", got)
	}
}

func TestUnhashableCallableRuntimeApiKeysAreSafeSecretFreeDiscriminators(t *testing.T) {
	auxIsolateClientCache(t)
	ctx := ClearRuntimeMain(auxMainTestHome(t, ""))
	firstProvider := func() string { return "first-super-secret-token" }
	secondProvider := func() string { return "second-super-secret-token" }
	first, _, err := auxClientCacheKeyFor(ctx, "auto", false, "", nil, "", auxCacheKeyLiveRuntime("same", firstProvider), false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := auxClientCacheKeyFor(ctx, "auto", false, "", nil, "", auxCacheKeyLiveRuntime("same", secondProvider), false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("callable providers collapsed to one key %+v", first)
	}
	rendered := fmt.Sprintf("%#v %#v", first, second)
	if strings.Contains(rendered, "first-super-secret-token") || strings.Contains(rendered, "second-super-secret-token") {
		t.Fatalf("token leaked into the key %s", rendered)
	}
}

func TestStringApiKeysAreNotRetainedInCacheKeyRepr(t *testing.T) {
	auxIsolateClientCache(t)
	ctx := ClearRuntimeMain(auxMainTestHome(t, ""))
	firstSecret, secondSecret := "first-literal-super-secret", "second-literal-super-secret"
	first, _, err := auxClientCacheKeyFor(ctx, "auto", false, "", firstSecret, "", auxCacheKeyLiveRuntime("same", firstSecret), false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := auxClientCacheKeyFor(ctx, "auto", false, "", secondSecret, "", auxCacheKeyLiveRuntime("same", secondSecret), false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("string credentials collapsed to one key %+v", first)
	}
	rendered := fmt.Sprintf("%#v %#v", first, second)
	if strings.Contains(rendered, firstSecret) || strings.Contains(rendered, secondSecret) {
		t.Fatalf("secret leaked into the key %s", rendered)
	}
}
