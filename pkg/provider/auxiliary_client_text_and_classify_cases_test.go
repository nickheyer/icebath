package provider

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestGetTextAuxiliaryClientCodexPoolEntryTakesPriorityOverAuthStore(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxFixedPoolEntry(t, true, &PooledCredential{Provider: "openai-codex", AccessToken: "pooled-codex-token", BaseURL: "https://chatgpt.com/backend-api/codex"})
	prev := auxReadCodexSingletonTokenFn
	auxReadCodexSingletonTokenFn = func(context.Context) string {
		t.Fatal("legacy codex store should not run")
		return ""
	}
	t.Cleanup(func() { auxReadCodexSingletonTokenFn = prev })
	client, model, err := auxBuildCodexClient(ctx, "gpt-5.4")
	if err != nil || model != "gpt-5.4" {
		t.Fatalf("got %q %v", model, err)
	}
	if _, ok := client.(*CodexAuxiliaryClient); !ok {
		t.Fatalf("got %T", client)
	}
}

func TestGetTextAuxiliaryClientReturnsNoneWhenNothingAvailable(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxClearUnhealthy(t)
	for _, name := range []string{"OPENAI_BASE_URL", "OPENAI_API_KEY", "OPENROUTER_API_KEY"} {
		t.Setenv(name, "")
	}
	auxFixedPoolEntry(t, false, nil)
	prevRows, prevNous := auxRegistryRowsFn, auxResolveNousRuntimeAPIFn
	auxRegistryRowsFn = func(context.Context) []*Profile { return nil }
	auxResolveNousRuntimeAPIFn = func(context.Context, bool, string) (string, string, bool) { return "", "", false }
	t.Cleanup(func() { auxRegistryRowsFn, auxResolveNousRuntimeAPIFn = prevRows, prevNous })
	client, model, err := GetTextAuxiliaryClient(ctx, "", nil)
	if err != nil || client != nil || model != "" {
		t.Fatalf("got %v %q %v", client, model, err)
	}
}

func TestGetTextAuxiliaryClientCustomEndpointUsesCodexWrapperWhenRuntimeRequestsResponsesApi(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxClearUnhealthy(t)
	t.Setenv("OPENROUTER_API_KEY", "")
	auxFixedPoolEntry(t, false, nil)
	prevCustom, prevNous, prevMain := auxResolveCustomRuntimeFn, auxResolveNousRuntimeAPIFn, auxReadMainModel
	auxResolveCustomRuntimeFn = func(context.Context) (string, string, string, error) {
		return "https://api.openai.com/v1", "sk-test", "codex_responses", nil
	}
	auxResolveNousRuntimeAPIFn = func(context.Context, bool, string) (string, string, bool) { return "", "", false }
	auxReadMainModel = func(context.Context) string { return "gpt-5.3-codex" }
	t.Cleanup(func() {
		auxResolveCustomRuntimeFn, auxResolveNousRuntimeAPIFn, auxReadMainModel = prevCustom, prevNous, prevMain
	})
	client, model, err := GetTextAuxiliaryClient(ctx, "", nil)
	if err != nil || model != "gpt-5.3-codex" {
		t.Fatalf("got %v %q %v", client, model, err)
	}
	if _, ok := client.(*CodexAuxiliaryClient); !ok {
		t.Fatalf("got %T", client)
	}
	if strings.TrimRight(client.BaseURL(), "/") != "https://api.openai.com/v1" || client.APIKey() != "sk-test" {
		t.Fatalf("base %q key %q", client.BaseURL(), client.APIKey())
	}
}

func TestVisionClientFallbackVisionAutoIncludesActiveProviderWhenConfigured(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	t.Setenv("ANTHROPIC_API_KEY", "***")
	prevProvider, prevModel := auxReadMainProvider, auxReadMainModel
	auxReadMainProvider = func(context.Context) string { return "anthropic" }
	auxReadMainModel = func(context.Context) string { return "claude-sonnet-4" }
	t.Cleanup(func() { auxReadMainProvider, auxReadMainModel = prevProvider, prevModel })
	auxRecordAnthropicBuild(t)
	prevToken := auxResolveAnthropicTokenFn
	auxResolveAnthropicTokenFn = func(context.Context) string { return "***" }
	t.Cleanup(func() { auxResolveAnthropicTokenFn = prevToken })
	backends, err := GetAvailableVisionBackends(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(backends, "anthropic") {
		t.Fatalf("backends %v", backends)
	}
}

func TestVisionClientFallbackAnthropicAuxiliaryClientAggregatesStreamResponse(t *testing.T) {
	ev := func(name, data string) string { return fmt.Sprintf("event: %s\ndata: %s\n\n", name, data) }
	sse := ev("message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-20250514","content":[],"usage":{"input_tokens":3,"output_tokens":0}}}`) +
		ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`) +
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"streamed aux response"}}`) +
		ev("content_block_stop", `{"type":"content_block_stop","index":0}`) +
		ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`) +
		ev("message_stop", `{"type":"message_stop"}`)
	srv, bodies := auxAnthropicServer(t, sse, 0, "")
	real, err := buildAnthropicClient(context.Background(), "sk-test", srv.URL+"/v1/", 30, false)
	if err != nil {
		t.Fatal(err)
	}
	defer real.Close()
	client := newAnthropicAuxiliaryClientFrom(real, "claude-sonnet-4-20250514", "sk-test", "https://sse-only.example/v1", false)
	out, err := client.CreateChatCompletion(context.Background(), map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "summarize"}}, "max_tokens": 16})
	if err != nil {
		t.Fatal(err)
	}
	if len(*bodies) != 1 || (*bodies)[0]["stream"] != true {
		t.Fatalf("requests %v", *bodies)
	}
	resp := out.(map[string]any)
	message := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	usage := resp["usage"].(map[string]any)
	if message["content"] != "streamed aux response" || usage["prompt_tokens"] != 3 || usage["completion_tokens"] != 4 {
		t.Fatalf("response %v", resp)
	}
}

func TestAuxiliaryPoolAwarenessCachedGmiClientKeepsExplicitSlashModelOverride(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxIsolateClientCache(t)
	fake := &auxCallFakeClient{}
	calls := auxFakeResolveProviderClient(t, func(string, string, ProviderClientOptions) (AuxClient, string) {
		return fake, "google/gemini-3.1-flash-lite-preview"
	})
	ShutdownCachedClients(ctx)
	t.Cleanup(func() { ShutdownCachedClients(ctx) })
	req := auxCachedClientRequest{Provider: "gmi", Model: "google/gemini-3.1-flash-lite-preview", BaseURL: "https://api.gmi-serving.com/v1", APIKey: "gmi-key"}
	client, model, err := auxGetCachedClient(ctx, req)
	if err != nil || client != AuxClient(fake) || model != "google/gemini-3.1-flash-lite-preview" {
		t.Fatalf("first %v %q %v", client, model, err)
	}
	req.Model = "openai/gpt-5.4-mini"
	client, model, err = auxGetCachedClient(ctx, req)
	if err != nil || client != AuxClient(fake) || model != "openai/gpt-5.4-mini" {
		t.Fatalf("second %v %q %v", client, model, err)
	}
	if *calls != 2 {
		t.Fatalf("resolve calls %d", *calls)
	}
}

func TestIsPaymentError402StatusCode(t *testing.T) {
	if !auxIsPaymentError(auxExc("Payment Required", 402)) {
		t.Fatal("402 not payment")
	}
}

func TestIsPaymentErrorResourceExhaustedSeparatorVariantsArePayment(t *testing.T) {
	for _, spelling := range []string{"RESOURCE_EXHAUSTED", "ResourceExhausted", "resource-exhausted"} {
		for _, status := range []int{0, 429} {
			if !auxIsPaymentError(auxExc(spelling+": Worker local total request limit reached (32/32)", status)) {
				t.Fatalf("%s status %d not payment", spelling, status)
			}
		}
	}
}

func TestIsPaymentError403SubscriptionRequiredIsPayment(t *testing.T) {
	if !auxIsPaymentError(auxExc("this model requires a subscription, upgrade for access: https://ollama.com/upgrade", 403)) {
		t.Fatal("403 subscription not payment")
	}
}

func TestIsPaymentError404GenericNotFoundIsNotPayment(t *testing.T) {
	if auxIsPaymentError(auxExc("Not Found", 404)) {
		t.Fatal("generic 404 is payment")
	}
}

func TestIsModelNotFoundErrorNousOpenrouterCatalog404(t *testing.T) {
	if !auxIsModelNotFoundError(auxExc("Model 'gpt-5.4-mini' not found. The requested model does not exist in our configuration or OpenRouter catalog.", 404)) {
		t.Fatal("catalog 404 not model not found")
	}
}

func TestIsModelNotFoundErrorBilling404IsNotModelNotFound(t *testing.T) {
	exc := auxExc("Model 'gpt-5' is not available on the free tier. Upgrade.", 404)
	if auxIsModelNotFoundError(exc) || !auxIsPaymentError(exc) {
		t.Fatalf("model not found %v payment %v", auxIsModelNotFoundError(exc), auxIsPaymentError(exc))
	}
}

func TestIsModelNotFoundErrorOutOfFunds404IsNotModelNotFound(t *testing.T) {
	if auxIsModelNotFoundError(auxExc("Your API key is blocked or out of funds. model_not_found", 404)) {
		t.Fatal("out of funds 404 is model not found")
	}
}

func TestIsModelIncompatibleErrorCodexChatgptAccountModelGating(t *testing.T) {
	if !auxIsModelIncompatibleError(auxExc(`Error code: 400 - {'detail': "The 'glm-5.2' model is not supported when using Codex with a ChatGPT account."}`, 400)) {
		t.Fatal("codex gating 400 not incompatible")
	}
}

func TestIsModelIncompatibleErrorNotFoundIsNotIncompatible(t *testing.T) {
	exc := auxExc("openrouter/foo/bar is not a valid model ID", 400)
	if auxIsModelIncompatibleError(exc) || !auxIsModelNotFoundError(exc) {
		t.Fatalf("incompatible %v not found %v", auxIsModelIncompatibleError(exc), auxIsModelNotFoundError(exc))
	}
}

func TestIsModelIncompatibleErrorPayment400IsNotIncompatible(t *testing.T) {
	if auxIsModelIncompatibleError(auxExc("insufficient credits: model is not supported on free tier", 400)) {
		t.Fatal("billing 400 is incompatible")
	}
}

func TestRefreshNousRecommendedModelFallsBackToDefaultWhenPortalUnavailable(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	prev := auxGetNousRecommendedAuxModelFn
	auxGetNousRecommendedAuxModelFn = func(context.Context, bool, bool) string { panic(errors.New("portal down")) }
	t.Cleanup(func() { auxGetNousRecommendedAuxModelFn = prev })
	if out := auxRefreshNousRecommendedModel(ctx, false, "some/dead-model"); out != auxNousModel {
		t.Fatalf("got %q", out)
	}
}

func TestRefreshNousRecommendedModelReturnsNoneWhenNoDistinctAlternative(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	prev := auxGetNousRecommendedAuxModelFn
	auxGetNousRecommendedAuxModelFn = func(context.Context, bool, bool) string { return auxNousModel }
	t.Cleanup(func() { auxGetNousRecommendedAuxModelFn = prev })
	if out := auxRefreshNousRecommendedModel(ctx, false, auxNousModel); out != "" {
		t.Fatalf("got %q", out)
	}
}

func TestIsRateLimitError429WithRateLimitMessage(t *testing.T) {
	if !auxIsRateLimitError(auxExc("Rate limit exceeded, try again in 2 seconds", 429)) {
		t.Fatal("429 not rate limit")
	}
}

func TestIsRateLimitErrorOpenaiRatelimiterrorClassname(t *testing.T) {
	if !auxIsRateLimitError(&AuxSDKError{Class: "RateLimitError", Message: "rate limit exceeded"}) {
		t.Fatal("RateLimitError class not rate limit")
	}
}

func TestIsTimeoutErrorTimedOutString(t *testing.T) {
	if !auxIsTimeoutError(auxExc("Request timed out.", 0)) {
		t.Fatal("timed out string not timeout")
	}
}

func TestIsTimeoutErrorTimeoutTypename(t *testing.T) {
	if !auxIsTimeoutError(&ReadTimeout{msg: "slow"}) {
		t.Fatal("ReadTimeout type not timeout")
	}
}

func TestIsConnectionErrorConnectionRefused(t *testing.T) {
	if !auxIsConnectionError(auxExc("Connection refused", 0)) {
		t.Fatal("connection refused not connection error")
	}
}

func TestIsConnectionErrorNormalApiErrorNotConnection(t *testing.T) {
	if auxIsConnectionError(auxExc("Bad Request: invalid model", 400)) {
		t.Fatal("400 api error is connection error")
	}
}
