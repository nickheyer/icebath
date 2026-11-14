package provider

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

const auxRelayReasoning400 = "Error code: 400 - {'error': {'message': 'Unrecognized request argument supplied: reasoning_effort', 'type': 'invalid_request_error', 'param': '', 'code': None}}"

// Custom relay route, the scripted client cached, validator bypassed and the payment fallback empty
func auxReasoningRejectionFixture(t *testing.T, outcomes ...any) (context.Context, *auxCallFakeClient) {
	t.Helper()
	ctx := auxMainTestHome(t, "")
	auxFixedTaskRoute(t, auxTaskRoute{Provider: "custom", Model: "gpt-4.1-mini", BaseURL: "https://relay.example/v1", APIKey: "sk-x"})
	client := auxScriptedCallClient("https://relay.example/v1", "sk-x", outcomes...)
	auxSwapCachedClient(t, client, "gpt-4.1-mini")
	prev := auxValidateLLMResponseFn
	t.Cleanup(func() { auxValidateLLMResponseFn = prev })
	auxValidateLLMResponseFn = func(_ context.Context, response any, _, _, _ string) (any, error) { return response, nil }
	auxSwapPaymentFallbackNone(t)
	return ctx, client
}

func auxRuntimeError(message string) error {
	return &AuxSDKError{Class: "RuntimeError", Message: message}
}

// Title call with a JSON response format and reasoning disabled over the sync or async surface
func auxReasoningRejectionCall(t *testing.T, ctx context.Context, asyncMode bool) (any, error) {
	t.Helper()
	messages := []any{map[string]any{"role": "user", "content": "hi"}}
	extraBody := map[string]any{"response_format": map[string]any{"type": "json_object"}}
	reasoning := map[string]any{"enabled": false}
	if asyncMode {
		return AsyncCallLLM(ctx, map[string]any{"task": "title_generation", "messages": messages, "extra_body": extraBody, "reasoning_config": reasoning})
	}
	return auxCallLLM(ctx, auxCallRequest{Task: "title_generation", Messages: messages, ExtraBody: extraBody, ReasoningConfig: reasoning})
}

func TestReasoningEffortRejectionRetriesOnceWithoutReasoningFields(t *testing.T) {
	for _, asyncMode := range []bool{false, true} {
		ctx, client := auxReasoningRejectionFixture(t, auxRuntimeError(auxRelayReasoning400), auxDummyResponse())
		result, err := auxReasoningRejectionCall(t, ctx, asyncMode)
		if err != nil || !reflect.DeepEqual(result, auxDummyResponse()) {
			t.Fatalf("async=%v got (%v, %v)", asyncMode, result, err)
		}
		client.mu.Lock()
		sent := append([]map[string]any(nil), client.sent...)
		client.mu.Unlock()
		if len(sent) != 2 {
			t.Fatalf("async=%v calls %d", asyncMode, len(sent))
		}
		first, retry := sent[0], sent[1]
		if first["reasoning_effort"] != "none" {
			t.Fatalf("async=%v first reasoning_effort %v", asyncMode, first["reasoning_effort"])
		}
		if _, ok := retry["reasoning_effort"]; ok {
			t.Fatalf("async=%v retry kept reasoning_effort %v", asyncMode, retry)
		}
		retryExtra, _ := retry["extra_body"].(map[string]any)
		if _, ok := retryExtra["reasoning"]; ok {
			t.Fatalf("async=%v retry kept extra_body.reasoning %v", asyncMode, retryExtra)
		}
		if !reflect.DeepEqual(retryExtra["response_format"], map[string]any{"type": "json_object"}) || retry["model"] != first["model"] {
			t.Fatalf("async=%v retry %v first %v", asyncMode, retry, first)
		}
	}
}

func TestUnrelated400DoesNotStripReasoningFields(t *testing.T) {
	ctx, client := auxReasoningRejectionFixture(t, auxRuntimeError("HTTP 400: Invalid value: 'tool'. Supported values are: 'assistant'"))
	_, err := auxReasoningRejectionCall(t, ctx, false)
	if err == nil || !strings.Contains(err.Error(), "Invalid value") {
		t.Fatalf("got %v", err)
	}
	if auxCallCount(client) != 1 {
		t.Fatalf("calls %d", auxCallCount(client))
	}
}

func TestModelGating400NamingAThinkingModelStillReachesTheFallbackChain(t *testing.T) {
	ctx, client := auxReasoningRejectionFixture(t, auxRuntimeError("Error code: 400 - The model kimi-k2-thinking is not supported when using this account"))
	fbClient := auxScriptedCallClient("https://fallback.example/v1", "fb", map[string]any{"fb": true})
	auxSwapFallbackSeams(t)
	fallbackCalled := false
	auxTryConfiguredFallbackChainFn = func(context.Context, string, string, string, string, string, FailureScope) (AuxClient, string, string, error) {
		fallbackCalled = true
		return fbClient, "fallback-model", "fallback", nil
	}
	result, err := auxCallLLM(ctx, auxCallRequest{Task: "title_generation", Messages: []any{map[string]any{"role": "user", "content": "hi"}}, ReasoningConfig: map[string]any{"enabled": false}})
	if err != nil || !reflect.DeepEqual(result, map[string]any{"fb": true}) {
		t.Fatalf("got (%v, %v)", result, err)
	}
	if auxCallCount(client) != 1 || !fallbackCalled {
		t.Fatalf("primary calls %d fallback %v", auxCallCount(client), fallbackCalled)
	}
}
