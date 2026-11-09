package provider

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMoaKeyStrippedFromNativeClient(t *testing.T) {
	var got map[string]any
	native := &fakeChatClient{create: func(_ context.Context, kw map[string]any) (any, error) { got = kw; return "native-response", nil }}
	a := &fakeChatAgent{provider: "moa", apiMode: "chat_completions", client: native}
	kwargs := map[string]any{"model": "gpt-4", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"_moa_prepared_request": map[string]any{"messages": []any{}, "model": "x"}}
	if _, err := DispatchNonstreamingAPIRequest(t.Context(), a, kwargs, func(string, string) (any, error) { return a.client, nil }); err != nil {
		t.Fatal(err)
	}
	if _, leaked := got["_moa_prepared_request"]; leaked {
		t.Fatalf("_moa_prepared_request leaked to native client: %v", got)
	}
}

func TestAnthropicNonStreamingStaleAbortsRequestClientNotShared(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	request := &fakeChatClient{create: func(context.Context, map[string]any) (any, error) {
		time.Sleep(2500 * time.Millisecond)
		return &struct{ ok bool }{true}, nil
	}}
	a := &fakeChatAgent{provider: "anthropic", apiMode: "anthropic_messages", baseURL: "https://api.anthropic.com", model: "x",
		resolve:         func(map[string]any) (any, error) { return 0.05, nil },
		anthropicCreate: func(string) (any, error) { return request, nil },
		hint:            func(any) (string, error) { return "", nil }}
	_, err := InterruptibleAPICall(t.Context(), a, map[string]any{"model": "x", "messages": []any{}})
	var te *TimeoutPhaseError
	if !errors.As(err, &te) {
		t.Fatalf("err %v", err)
	}
	a.mu.Lock()
	aborts := append([]clientCall(nil), a.aAborts...)
	a.mu.Unlock()
	if len(aborts) != 1 || aborts[0].client != request || aborts[0].reason != "stale_call_kill" {
		t.Fatalf("aborts %+v", aborts)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		closes := append([]clientCall(nil), a.aCloses...)
		a.mu.Unlock()
		if len(closes) > 0 {
			if closes[0].client != request {
				t.Fatalf("closes %+v", closes)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the worker never closed its own request client")
}
