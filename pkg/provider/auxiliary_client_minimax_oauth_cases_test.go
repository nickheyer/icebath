package provider

import (
	"context"
	"reflect"
	"sort"
	"testing"
)

const auxMinimaxInferenceBase = "https://api.minimax.io/anthropic"

// Swaps the MiniMax OAuth resolver for one yielding a minting token provider at the inference base
func auxFakeMinimaxRuntimeCreds(t *testing.T) map[string]any {
	t.Helper()
	creds := map[string]any{"provider": "minimax-oauth", "api_key": func() (string, error) { return "fresh-bearer", nil },
		"base_url": auxMinimaxInferenceBase, "source": "oauth"}
	prev := auxMinimaxRuntimeCredentialsFn
	t.Cleanup(func() { auxMinimaxRuntimeCredentialsFn = prev })
	auxMinimaxRuntimeCredentialsFn = func(PoolEnv, bool) (map[string]any, error) { return creds, nil }
	return creds
}

func TestResolveMinimaxOauthBuildsAnthropicWrapperWithThirdPartyWire(t *testing.T) {
	auxIsolateClientCache(t)
	ctx := auxMainTestHome(t, "")
	creds := auxFakeMinimaxRuntimeCreds(t)
	captured := auxFakeAnthropicBuild(t, auxNoAnthropicPool, "")
	client, model, err := ResolveProviderClient(ctx, "minimax-oauth", "MiniMax-M3", ProviderClientOptions{})
	if err != nil || client == nil {
		t.Fatalf("minimax-oauth must produce a configured client when credentials are present, got (%v, %v)", client, err)
	}
	ac, ok := client.(*AnthropicAuxiliaryClient)
	if !ok {
		t.Fatalf("minimax-oauth must build an AnthropicAuxiliaryClient (the inference endpoint is /anthropic). Got %T.", client)
	}
	if ac.adapter.isOAuth {
		t.Fatal("MiniMax is a third-party Anthropic-protocol endpoint: is_oauth must be False")
	}
	if captured[0] == nil || reflect.ValueOf(captured[0]).Pointer() != reflect.ValueOf(creds["api_key"]).Pointer() {
		t.Fatalf("build_anthropic_client must receive the callable token provider, got %T", captured[0])
	}
	if captured[1] != auxMinimaxInferenceBase || model != "MiniMax-M3" {
		t.Fatalf("base %v model %q", captured[1], model)
	}
}

func TestResolveMinimaxOauthToolNamesUnprefixedOnWire(t *testing.T) {
	auxIsolateClientCache(t)
	ctx := auxMainTestHome(t, "")
	auxFakeMinimaxRuntimeCreds(t)
	auxFakeAnthropicBuild(t, auxNoAnthropicPool, "")
	var wireNames []string
	prev := auxCreateAnthropicMessageFn
	t.Cleanup(func() { auxCreateAnthropicMessageFn = prev })
	auxCreateAnthropicMessageFn = func(_ context.Context, _ *anthropicSDKClient, apiKwargs map[string]any, _ func(event any) error) (map[string]any, error) {
		for _, tool := range auxKwargMaps(apiKwargs["tools"]) {
			wireNames = append(wireNames, pyStr(tool["name"]))
		}
		return map[string]any{"content": []any{}, "stop_reason": "end_turn",
			"usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}, nil
	}
	client, _, err := ResolveProviderClient(ctx, "minimax-oauth", "MiniMax-M3", ProviderClientOptions{})
	if err != nil || client == nil {
		t.Fatalf("got (%v, %v)", client, err)
	}
	if _, err := client.CreateChatCompletion(ctx, map[string]any{"model": "MiniMax-M3",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"tools": []any{
			map[string]any{"type": "function", "function": map[string]any{"name": "read_file", "description": "x", "parameters": map[string]any{}}},
			map[string]any{"type": "function", "function": map[string]any{"name": "session_search", "description": "y", "parameters": map[string]any{}}},
		}}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(wireNames)
	if !reflect.DeepEqual(wireNames, []string{"read_file", "session_search"}) {
		t.Fatalf("Third-party Anthropic-protocol endpoints must see unprefixed tool names, got %v", wireNames)
	}
}
