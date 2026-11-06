package provider

import (
	"context"
	"errors"
	"testing"
)

// Reasoning effort per sent request, after flattening extra_body the way the OpenAI SDK merges it
func auxSentHas(sent []map[string]any, field string) []bool {
	out := make([]bool, 0, len(sent))
	for _, kw := range sent {
		_, has := kw[field]
		if extra, ok := kw["extra_body"].(map[string]any); ok && !has {
			_, has = extra[field]
		}
		out = append(out, has)
	}
	return out
}

func TestAuxStructuredParamRejectionStripsReasoningEffortOnRetry(t *testing.T) {
	client := &auxCallFakeClient{base: "https://api.example/v1", create: func(_ context.Context, kwargs map[string]any) (any, error) {
		body := map[string]any{}
		for k, v := range kwargs {
			body[k] = v
		}
		if extra, ok := body["extra_body"].(map[string]any); ok {
			delete(body, "extra_body")
			for k, v := range extra {
				body[k] = v
			}
		}
		if _, has := body["reasoning_effort"]; has {
			return nil, errors.New("Error code: 400 - {'error': {'param': 'reasoning.effort', 'error_code': 'invalid_reasoning_effort', 'retryable': False}}")
		}
		return auxCallOK("ok"), nil
	}}
	temp := 0.3
	resp, err := auxCallFallbackCandidate(context.Background(), client, "custom-relay", "fallback_chain[0](custom)", auxFallbackRequest{
		Task: "title_generation", Messages: []map[string]any{{"role": "user", "content": "hi"}}, Temperature: &temp, MaxTokens: 16,
		EffectiveTimeout: 30, EffectiveExtraBody: map[string]any{}, ReasoningConfig: map[string]any{"enabled": true, "effort": "max"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := ExtractContentOrReasoning(resp, -1); got != "ok" {
		t.Fatalf("response %v", resp)
	}
	if got := auxSentHas(client.sent, "reasoning_effort"); len(got) != 2 || !got[0] || got[1] {
		t.Fatalf("reasoning_effort per call %v (%v)", got, client.sent)
	}
}

func TestAuxFallbackCandidateRecoversFromRejectedTemperature(t *testing.T) {
	client := auxRejectingClient("temperature")
	temp := 0.3
	resp, err := auxCallFallbackCandidate(context.Background(), client, "relay-model-x", "fallback_chain[0](openai)", auxFallbackRequest{
		Task: "title_generation", Messages: []map[string]any{{"role": "user", "content": "hi"}}, Temperature: &temp, MaxTokens: 16,
		EffectiveTimeout: 30, EffectiveExtraBody: map[string]any{}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := ExtractContentOrReasoning(resp, -1); got != "ok" {
		t.Fatalf("response %v", resp)
	}
	if got := auxSentHas(client.sent, "temperature"); len(got) != 2 || !got[0] || got[1] {
		t.Fatalf("temperature per call %v", got)
	}
}

func TestAuxAutoRouteForwardsMainRuntimeEntraCallableIntact(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	writeConfig(t, h, map[string]any{"model": map[string]any{"provider": "azure-foundry", "base_url": "https://r.openai.azure.com/openai/v1",
		"api_mode": "chat_completions", "auth_mode": "entra_id", "default": "gpt-4o"}})
	calls := 0
	mainTokenProvider := func() string { calls++; return "main-session-jwt" }
	client, resolved, effective, err := auxResolveAutoRoute(context.Background(), map[string]any{"provider": "azure-foundry", "model": "gpt-4o",
		"api_mode": "chat_completions", "base_url": "https://r.openai.azure.com/openai/v1", "api_key": mainTokenProvider}, "")
	if err != nil || client == nil {
		t.Fatalf("client %v err %v", client, err)
	}
	if resolved != "gpt-4o" || effective != "azure-foundry" {
		t.Fatalf("resolved %q effective %q", resolved, effective)
	}
	oc, ok := client.(*auxOpenAIClient)
	if !ok {
		t.Fatalf("client type %T", client)
	}
	if oc.apiKeyProvider == nil || oc.apiKey != "" {
		t.Fatalf("the Entra callable must reach the OpenAI client as a callable, static key %q", oc.apiKey)
	}
	token, err := oc.apiKeyProvider()
	if err != nil || token != "main-session-jwt" || calls != 1 {
		t.Fatalf("token %q err %v calls %d", token, err, calls)
	}
}
