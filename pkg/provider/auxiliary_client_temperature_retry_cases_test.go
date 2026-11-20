package provider

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

const auxUnsupportedValueTemperature = "Error code: 400 - {'error': {'message': \"Unsupported value: 'temperature' does not support 0.1 with this model. Only the default (1) value is supported.\", 'param': 'temperature', 'code': 'unsupported_value'}}"

type auxScriptedResult struct {
	resp any
	err  error
}

// Fixed task route, a scripted client answering in sequence and the response validator bypassed
func auxTemperatureFixture(t *testing.T, route auxTaskRoute, clientBase string, results []auxScriptedResult) (context.Context, *auxScriptedClient) {
	t.Helper()
	auxResetTemperatureRejections()
	auxResetStructuredOutputRejections()
	t.Cleanup(auxResetTemperatureRejections)
	t.Cleanup(auxResetStructuredOutputRejections)
	ctx := auxMainTestHome(t, "")
	prevRoute := auxResolveTaskProviderModelFn
	t.Cleanup(func() { auxResolveTaskProviderModelFn = prevRoute })
	auxResolveTaskProviderModelFn = func(context.Context, any, string, string, string, any) auxTaskRoute { return route }
	next := 0
	client := &auxScriptedClient{base: clientBase, create: func(map[string]any) (any, error) {
		r := results[next]
		if next < len(results)-1 {
			next++
		}
		return r.resp, r.err
	}}
	auxSwapCachedClient(t, client, route.Model)
	prev := auxValidateLLMResponseFn
	t.Cleanup(func() { auxValidateLLMResponseFn = prev })
	auxValidateLLMResponseFn = func(_ context.Context, response any, _, _, _ string) (any, error) { return response, nil }
	return ctx, client
}

func auxDummyResponse() map[string]any { return map[string]any{"ok": true} }

// Swaps the payment fallback for one that yields nothing
func auxSwapPaymentFallbackNone(t *testing.T) {
	t.Helper()
	prev := auxTryPaymentFallbackFn
	t.Cleanup(func() { auxTryPaymentFallbackFn = prev })
	auxTryPaymentFallbackFn = func(context.Context, string, string, string, string, FailureScope, map[string]any) (AuxClient, string, string, error) {
		return nil, "", "", nil
	}
}

func auxTemperatures(client *auxScriptedClient) []any {
	client.mu.Lock()
	defer client.mu.Unlock()
	out := make([]any, 0, len(client.calls))
	for _, c := range client.calls {
		out = append(out, c["temperature"])
	}
	return out
}

func TestOpenaiDefaultOnlyFamiliesOmitTemperatureUpFront(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	v := 0.1
	for _, model := range []string{"gpt-5.5", "openai/gpt-5.5-pro", "gpt-5.1-2026-01-01", "gpt-5-codex", "o3-mini", "o4-mini"} {
		omit, fixed, err := auxFixedTemperatureForModel(model, "", "")
		if err != nil || !omit || fixed != nil {
			t.Fatalf("%s: %v %v %v", model, omit, fixed, err)
		}
		kwargs, err := auxBuildCallKwargs(ctx, "openai-api", model, []any{map[string]any{"role": "user", "content": "hi"}}, auxCallKwargsOptions{Temperature: &v})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := kwargs["temperature"]; ok {
			t.Fatalf("%s sent temperature %v", model, kwargs["temperature"])
		}
	}
	for _, accepts := range []string{"gpt-5-chat-latest", "gpt-4.1"} {
		kwargs, err := auxBuildCallKwargs(ctx, "openai-api", accepts, []any{}, auxCallKwargsOptions{Temperature: &v})
		if err != nil || kwargs["temperature"] != 0.1 {
			t.Fatalf("%s temperature %v %v", accepts, kwargs["temperature"], err)
		}
	}
}

func TestGemini36PlusOmitsTemperatureUpFront(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	v := 0.3
	for _, model := range []string{"gemini-3.6-flash", "google/gemini-3.7-flash", "gemini-3.8-flash", "gemini-4.0-flash", "gemini-4-argon", "google/gemini-4-argon"} {
		omit, fixed, err := auxFixedTemperatureForModel(model, "", "")
		if err != nil || !omit || fixed != nil {
			t.Fatalf("%s: %v %v %v", model, omit, fixed, err)
		}
		kwargs, err := auxBuildCallKwargs(ctx, "gemini", model, []any{map[string]any{"role": "user", "content": "hi"}}, auxCallKwargsOptions{Temperature: &v})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := kwargs["temperature"]; ok {
			t.Fatalf("%s sent temperature %v", model, kwargs["temperature"])
		}
	}
	for _, keeps := range []string{"gemini-2.5-flash", "gemini-3.5-flash-lite", "gemini-3-pro", "gemini-3.1-pro"} {
		omit, fixed, err := auxFixedTemperatureForModel(keeps, "", "")
		if err != nil || omit || fixed != nil {
			t.Fatalf("%s: %v %v %v", keeps, omit, fixed, err)
		}
		kwargs, err := auxBuildCallKwargs(ctx, "gemini", keeps, []any{}, auxCallKwargsOptions{Temperature: &v})
		if err != nil || kwargs["temperature"] != 0.3 {
			t.Fatalf("%s temperature %v %v", keeps, kwargs["temperature"], err)
		}
	}
}

func TestRouteThatRejectedTemperatureOmitsItNextCall(t *testing.T) {
	ctx, client := auxTemperatureFixture(t, auxTaskRoute{Provider: "custom", Model: "relay-model-x"}, "https://relay.example/v1",
		[]auxScriptedResult{{nil, errors.New(auxUnsupportedValueTemperature)}, {auxDummyResponse(), nil}, {auxDummyResponse(), nil}, {auxDummyResponse(), nil}})
	for _, content := range []string{"a", "b"} {
		if _, err := CallLLM(ctx, map[string]any{"task": "vision", "messages": auxConcurrencyMessages(content), "temperature": 0.1}); err != nil {
			t.Fatal(err)
		}
	}
	if got := auxTemperatures(client); !reflect.DeepEqual(got, []any{0.1, nil, nil}) {
		t.Fatalf("temperatures sent %v", got)
	}
	omit, fixed, err := auxFixedTemperatureForModel("relay-model-y", "https://relay.example/v1", "custom")
	if err != nil || omit || fixed != nil {
		t.Fatalf("another model on the route: %v %v %v", omit, fixed, err)
	}
}

func TestRouteMemoryKeyedOnEffectiveBaseUrlWhenClientHasNone(t *testing.T) {
	ctx, client := auxTemperatureFixture(t, auxTaskRoute{Provider: "custom", Model: "relay-model-x", BaseURL: "https://relay.example/v1", APIKey: "k"}, "",
		[]auxScriptedResult{{nil, errors.New(auxUnsupportedValueTemperature)}, {auxDummyResponse(), nil}, {auxDummyResponse(), nil}, {auxDummyResponse(), nil}})
	for _, content := range []string{"a", "b"} {
		if _, err := CallLLM(ctx, map[string]any{"task": "compression", "messages": auxConcurrencyMessages(content), "temperature": 0.1}); err != nil {
			t.Fatal(err)
		}
	}
	if got := auxTemperatures(client); !reflect.DeepEqual(got, []any{0.1, nil, nil}) {
		t.Fatalf("temperatures sent %v", got)
	}
}

// One temperature rejection then a dummy answer, asserting the single retry without temperature
func auxRetriesOnceWithoutTemperatureCase(t *testing.T, asyncMode bool, task, message string) {
	t.Helper()
	ctx, client := auxTemperatureFixture(t, auxTaskRoute{Provider: "openai-codex", Model: "relay-model-x"}, "https://api.openai.com/v1",
		[]auxScriptedResult{{nil, errors.New(message)}, {auxDummyResponse(), nil}})
	call := CallLLM
	content := "remember this"
	if asyncMode {
		call, content = AsyncCallLLM, "query"
	}
	result, err := call(ctx, map[string]any{"task": task, "messages": auxConcurrencyMessages(content), "temperature": 0.3, "max_tokens": 500})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result, auxDummyResponse()) {
		t.Fatalf("result %v", result)
	}
	if n := client.callCount(); n != 2 {
		t.Fatalf("create called %d times", n)
	}
	first, retry := client.calls[0], client.calls[1]
	if first["temperature"] != 0.3 {
		t.Fatalf("first temperature %v", first["temperature"])
	}
	if _, ok := retry["temperature"]; ok {
		t.Fatalf("retry kept temperature %v", retry["temperature"])
	}
	if _, ok := first["max_tokens"]; ok {
		t.Fatalf("first call sent max_tokens %v", first["max_tokens"])
	}
	if _, ok := retry["max_tokens"]; ok {
		t.Fatalf("retry sent max_tokens %v", retry["max_tokens"])
	}
	if retry["model"] != first["model"] {
		t.Fatalf("retry model %v first %v", retry["model"], first["model"])
	}
}

// A non temperature 400 raised on every attempt must surface after exactly one create
func auxNonTemperature400Case(t *testing.T, asyncMode bool, task, message string, temperature any) {
	t.Helper()
	ctx, client := auxTemperatureFixture(t, auxTaskRoute{Provider: "openai-codex", Model: "relay-model-x"}, "https://api.openai.com/v1",
		[]auxScriptedResult{{nil, errors.New(message)}})
	auxSwapPaymentFallbackNone(t)
	call := CallLLM
	if asyncMode {
		call = AsyncCallLLM
	}
	_, err := call(ctx, map[string]any{"task": task, "messages": auxConcurrencyMessages("x"), "temperature": temperature, "max_tokens": 500})
	if err == nil || !strings.Contains(err.Error(), message) {
		t.Fatalf("error %v", err)
	}
	if n := client.callCount(); n != 1 {
		t.Fatalf("create called %d times", n)
	}
}

func TestCallLlmUnsupportedTemperatureRetryRetriesOnceWithoutTemperature(t *testing.T) {
	for _, message := range []string{
		"HTTP 400: Unsupported parameter: temperature",
		"Error code: 400 - {'error': {'code': 'unsupported_parameter', 'param': 'temperature'}}",
		"Provider error: this model does not support temperature",
	} {
		auxRetriesOnceWithoutTemperatureCase(t, false, "compression", message)
	}
}

func TestCallLlmUnsupportedTemperatureRetryNonTemperature400DoesNotRetryAsTemperature(t *testing.T) {
	auxNonTemperature400Case(t, false, "compression", "HTTP 400: Invalid value: 'tool'. Supported values are: 'assistant'...", 0.3)
}

func TestCallLlmUnsupportedTemperatureRetryNoRetryWhenTemperatureNotInKwargs(t *testing.T) {
	auxNonTemperature400Case(t, false, "compression", "HTTP 400: Unsupported parameter: temperature", nil)
}

func TestAsyncCallLlmUnsupportedTemperatureRetryAsyncRetriesOnceWithoutTemperature(t *testing.T) {
	auxRetriesOnceWithoutTemperatureCase(t, true, "session_search", "HTTP 400: Unsupported parameter: temperature")
}

func TestAsyncCallLlmUnsupportedTemperatureRetryAsyncNonTemperature400DoesNotRetry(t *testing.T) {
	auxNonTemperature400Case(t, true, "session_search", "HTTP 400: Invalid value: 'tool'", 0.3)
}
