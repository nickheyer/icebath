package provider

import (
	"context"
	"reflect"
	"testing"
)

const auxNous401BaseURL = "https://inference-api.nousresearch.com/v1"

// Auto route whose real acquisition resolves the stale client and whose Nous refresh yields the fresh one
func auxNous401Fixture(t *testing.T, stale, fresh AuxClient, syncRebuild AuxClient) context.Context {
	t.Helper()
	auxIsolateClientCache(t)
	ctx := auxMainTestHome(t, "")
	auxFixedTaskRoute(t, auxTaskRoute{Provider: "auto"})
	prevResolve, prevNous, prevCreate := auxResolveProviderClientFn, auxResolveNousRuntimeAPIFn, auxCreateOpenAIClientFn
	t.Cleanup(func() {
		auxResolveProviderClientFn, auxResolveNousRuntimeAPIFn, auxCreateOpenAIClientFn = prevResolve, prevNous, prevCreate
	})
	auxResolveProviderClientFn = func(context.Context, string, string, ProviderClientOptions) (AuxClient, string, error) {
		return stale, "nous-model", nil
	}
	auxResolveNousRuntimeAPIFn = func(context.Context, bool, string) (string, string, bool) {
		return "fresh-key", auxNous401BaseURL, true
	}
	auxCreateOpenAIClientFn = func(context.Context, any, string, auxOpenAIOptions) AuxClient { return syncRebuild }
	_ = fresh
	prevValidate := auxValidateLLMResponseFn
	t.Cleanup(func() { auxValidateLLMResponseFn = prevValidate })
	auxValidateLLMResponseFn = func(_ context.Context, response any, _, _, _ string) (any, error) { return response, nil }
	return ctx
}

// Whether any cache entry holds exactly this client
func auxCacheHoldsClient(client AuxClient) bool {
	auxClientCacheMu.Lock()
	defer auxClientCacheMu.Unlock()
	for _, entry := range auxClientCache {
		if entry.Client == client {
			return true
		}
	}
	return false
}

func TestCallLlmAutoProviderEvictsStaleClientEndToEnd(t *testing.T) {
	stale := auxScriptedCallClient(auxNous401BaseURL, "k", auxExc("stale creds", 401))
	fresh := auxScriptedCallClient(auxNous401BaseURL, "k", auxDummyResponse())
	ctx := auxNous401Fixture(t, stale, fresh, fresh)
	result, err := auxCallLLM(ctx, auxCallRequest{Task: "compression", Messages: []any{map[string]any{"role": "user", "content": "hi"}}})
	if err != nil || !reflect.DeepEqual(result, auxDummyResponse()) {
		t.Fatalf("got (%v, %v)", result, err)
	}
	if auxCallCount(stale) != 1 || auxCallCount(fresh) != 1 {
		t.Fatalf("stale calls %d fresh calls %d", auxCallCount(stale), auxCallCount(fresh))
	}
	if auxCacheHoldsClient(stale) {
		t.Fatal("stale auto-provider client survived the 401 refresh: the acquisition site dropped the task dimension so the refresh keyed the fresh client under a different cache entry (#58894)")
	}
	if !auxCacheHoldsClient(fresh) {
		t.Fatal("fresh client missing from the cache")
	}
}

func TestAsyncCallLlmAutoProviderEvictsStaleClientEndToEnd(t *testing.T) {
	stale := auxScriptedCallClient(auxNous401BaseURL, "k", auxExc("stale creds", 401))
	fresh := auxScriptedCallClient(auxNous401BaseURL, "k", auxDummyResponse())
	rebuilt := auxBareClient{base: auxNous401BaseURL, key: "fresh-key"}
	ctx := auxNous401Fixture(t, stale, fresh, rebuilt)
	prevAsync := auxToAsyncClientFn
	t.Cleanup(func() { auxToAsyncClientFn = prevAsync })
	auxToAsyncClientFn = func(_ context.Context, c AuxClient, m string, _ bool) (AuxClient, string) {
		if c == AuxClient(rebuilt) {
			return fresh, "nous-model"
		}
		return c, m
	}
	result, err := AsyncCallLLM(ctx, map[string]any{"task": "session_search", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"main_runtime": map[string]any{"provider": "nous", "model": "Hermes-4-405B"}})
	if err != nil || !reflect.DeepEqual(result, auxDummyResponse()) {
		t.Fatalf("got (%v, %v)", result, err)
	}
	if auxCallCount(stale) != 1 || auxCallCount(fresh) != 1 {
		t.Fatalf("stale calls %d fresh calls %d", auxCallCount(stale), auxCallCount(fresh))
	}
	if auxCacheHoldsClient(stale) {
		t.Fatal("stale auto-provider async client survived the 401 refresh: the async acquisition site dropped the task/main_runtime dimension so the refresh keyed the fresh client under a different cache entry (#58894)")
	}
	if !auxCacheHoldsClient(fresh) {
		t.Fatal("fresh client missing from the cache")
	}
}
