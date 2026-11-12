package provider

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

// JSON shaped copy of a value so assertions read it like the Python dict
func ppJSON(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// Value under a path of object keys and list indexes of a JSON shaped value
func ppAt(t *testing.T, v any, path ...any) any {
	t.Helper()
	v = ppJSON(t, v)
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				t.Fatalf("no object for key %q in %T", k, v)
			}
			v = m[k]
		case int:
			l, ok := v.([]any)
			if !ok || k >= len(l) {
				t.Fatalf("no index %d in %T", k, v)
			}
			v = l[k]
		}
	}
	return v
}

// Nous auth pool entry served only for the nous provider, the way the hermes test patches _read_nous_auth
func ppNousPoolEntry(t *testing.T, entry *PooledCredential) {
	t.Helper()
	old := auxSelectPoolEntryFn
	t.Cleanup(func() { auxSelectPoolEntryFn = old })
	auxSelectPoolEntryFn = func(_ context.Context, provider string) (bool, *PooledCredential) {
		if provider == "nous" {
			return true, entry
		}
		return false, nil
	}
}

// Registry rows and the Nous runtime refresh emptied so only the explicit seams decide the route
func ppNoRegistryNoNousRuntime(t *testing.T) {
	t.Helper()
	prevRows, prevNous := auxRegistryRowsFn, auxResolveNousRuntimeAPIFn
	auxRegistryRowsFn = func(context.Context) []*Profile { return nil }
	auxResolveNousRuntimeAPIFn = func(context.Context, bool, string) (string, string, bool) { return "", "", false }
	t.Cleanup(func() { auxRegistryRowsFn, auxResolveNousRuntimeAPIFn = prevRows, prevNous })
}

func TestBuildApiKwargsNousPortalAuxiliaryNousCallCarryingAConnectorResultAsksForANoTrainingRoute(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	messages := []any{
		map[string]any{"role": "user", "content": "title this"},
		map[string]any{"role": "tool", "tool_call_id": "c1", "content": "inbox", "data_use": []any{"no_training"}},
	}
	kwargs, err := auxBuildCallKwargs(ctx, "nous", "hermes-4", messages, auxCallKwargsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sameJSON(t, ppAt(t, kwargs, "extra_body", "provider"), map[string]any{"data_collection": "deny"}, "provider routing")
}

func TestChatMessagesToResponsesInputUserMessagePassesThrough(t *testing.T) {
	items := convertHistory(t, []*state.Message{{Role: "user", Content: "hello"}}, replayOpts())
	sameJSON(t, items, []map[string]any{{"type": "message", "role": "user", "content": "hello"}}, "items")
}

func TestChatMessagesToResponsesInputSystemMessagesFiltered(t *testing.T) {
	items := convertHistory(t, []*state.Message{{Role: "system", Content: "be helpful"}, {Role: "user", Content: "hello"}}, replayOpts())
	if len(items) != 1 || items[0]["role"] != "user" {
		t.Fatalf("items %v", items)
	}
}

func TestChatMessagesToResponsesInputAssistantToolCallsBecomeFunctionCallItems(t *testing.T) {
	msg := &state.Message{Role: "assistant", Content: "", ToolCalls: []state.ToolCall{{ID: "call_abc",
		Function: state.ToolCallFunction{Name: "web_search", Arguments: `{"query": "test"}`}, Extra: map[string]any{"call_id": "call_abc"}}}}
	calls := itemsOfType(convertHistory(t, []*state.Message{msg}, replayOpts()), "function_call")
	if len(calls) != 1 || calls[0]["name"] != "web_search" || calls[0]["call_id"] != "call_abc" {
		t.Fatalf("function_call items %v", calls)
	}
}

func TestChatMessagesToResponsesInputPreflightPreservesAssistantOutputText(t *testing.T) {
	normalized, err := preflightCodexInputItems(jsonList([]map[string]any{
		{"role": "user", "content": []map[string]any{{"type": "input_text", "text": "hi"}}},
		{"role": "assistant", "content": []map[string]any{{"type": "output_text", "text": "hello"}}},
	}), false, false)
	if err != nil {
		t.Fatal(err)
	}
	if ppAt(t, normalized, 0, "content", 0, "type") != "input_text" || ppAt(t, normalized, 1, "content", 0, "type") != "output_text" {
		t.Fatalf("normalized %v", normalized)
	}
}

func TestChatMessagesToResponsesInputFullRoundTripWithListContent(t *testing.T) {
	part := func(text string) any { return []any{map[string]any{"type": "text", "text": text}} }
	items := convertHistory(t, []*state.Message{{Role: "user", Content: part("hello")}, {Role: "assistant", Content: part("hi there")},
		{Role: "user", Content: part("continue")}}, replayOpts())
	normalized, err := preflightCodexInputItems(jsonList(items), false, false)
	if err != nil {
		t.Fatal(err)
	}
	if ppAt(t, normalized, 0, "content", 0, "type") != "input_text" || ppAt(t, normalized, 2, "content", 0, "type") != "input_text" ||
		ppAt(t, normalized, 1, "content", 0, "type") != "output_text" {
		t.Fatalf("normalized %v", normalized)
	}
}

func TestChatContentToResponsesPartsAssistantRoleWithMixedInputOutputTextTypes(t *testing.T) {
	parts, err := chatContentToResponsesParts(jsonList([]map[string]any{
		{"type": "input_text", "text": "a"}, {"type": "output_text", "text": "b"}, {"type": "text", "text": "c"}}), "assistant")
	if err != nil {
		t.Fatal(err)
	}
	texts := []string{}
	for _, p := range parts {
		if p["type"] != "output_text" {
			t.Fatalf("part %v kept type %v", p, p["type"])
		}
		texts = append(texts, p["text"].(string))
	}
	if strings.Join(texts, ",") != "a,b,c" {
		t.Fatalf("texts %v", texts)
	}
}

// Responses API object with the given output items and a completed status
func ppCodexResponse(output ...map[string]any) map[string]any {
	return jsonMap(map[string]any{"status": "completed", "output": output})
}

func TestNormalizeCodexResponseTextResponse(t *testing.T) {
	msg, reason, err := normalizeCodexResponse(ppCodexResponse(map[string]any{"type": "message", "status": "completed",
		"content": []map[string]any{{"type": "output_text", "text": "Hello!"}}, "phase": "final_answer"}), "", "", true)
	if err != nil || msg.Content != "Hello!" || reason != "stop" {
		t.Fatalf("content %q reason %q err %v", msg.Content, reason, err)
	}
}

func TestNormalizeCodexResponseEncryptedContentCaptured(t *testing.T) {
	msg, _, err := normalizeCodexResponse(ppCodexResponse(
		map[string]any{"type": "reasoning", "encrypted_content": "gAAAA_secret_blob_123", "id": "rs_456", "status": nil,
			"summary": []map[string]any{{"type": "summary_text", "text": "Thinking"}}},
		map[string]any{"type": "message", "status": "completed", "content": []map[string]any{{"type": "output_text", "text": "done"}}, "phase": "final_answer"},
	), "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(msg.ReasoningItems) != 1 || msg.ReasoningItems[0]["encrypted_content"] != "gAAAA_secret_blob_123" || msg.ReasoningItems[0]["id"] != "rs_456" {
		t.Fatalf("reasoning items %v", msg.ReasoningItems)
	}
}

func TestNormalizeCodexResponseMessageItemsCapturedWithIdAndPhase(t *testing.T) {
	item := func(id, phase, text string) map[string]any {
		return map[string]any{"type": "message", "status": "completed", "id": id, "phase": phase,
			"content": []map[string]any{{"type": "output_text", "text": text}}}
	}
	msg, _, err := normalizeCodexResponse(ppCodexResponse(item("msg_abc", "commentary", "Thinking..."), item("msg_def", "final_answer", "Done!")), "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(msg.MessageItems) != 2 || ppAt(t, msg.MessageItems, 0, "id") != "msg_abc" || ppAt(t, msg.MessageItems, 0, "phase") != "commentary" ||
		ppAt(t, msg.MessageItems, 0, "content", 0, "text") != "Thinking..." || ppAt(t, msg.MessageItems, 1, "id") != "msg_def" ||
		ppAt(t, msg.MessageItems, 1, "phase") != "final_answer" || ppAt(t, msg.MessageItems, 1, "content", 0, "text") != "Done!" {
		t.Fatalf("message items %v", msg.MessageItems)
	}
}

func TestChatMessagesToResponsesInputMessageItemsReplaysExactMessageItems(t *testing.T) {
	raw, _ := json.Marshal([]map[string]any{{"type": "message", "role": "assistant", "status": "completed", "id": "msg_123",
		"phase": "final_answer", "content": []map[string]any{{"type": "output_text", "text": "Hello world"}}}})
	items := convertHistory(t, []*state.Message{{Role: "assistant", Content: "Hello world", CodexMessageItems: raw}, {Role: "user", Content: "follow up"}}, replayOpts())
	var assistant []map[string]any
	for _, it := range items {
		if it["type"] == "message" && it["role"] == "assistant" {
			assistant = append(assistant, it)
		}
	}
	if len(assistant) != 1 || assistant[0]["id"] != "msg_123" || assistant[0]["phase"] != "final_answer" || ppAt(t, assistant[0], "content", 0, "text") != "Hello world" {
		t.Fatalf("assistant items %v", assistant)
	}
}

func TestChatMessagesToResponsesInputMessageItemsFallbackToPlainWhenNoMessageItems(t *testing.T) {
	items := convertHistory(t, []*state.Message{{Role: "assistant", Content: "Hello world"}}, replayOpts())
	sameJSON(t, items, []map[string]any{{"type": "message", "role": "assistant", "content": "Hello world"}}, "items")
}

func TestAuxiliaryClientProviderPriorityOpenrouterAlwaysWins(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxClearUnhealthy(t)
	t.Setenv("OPENROUTER_API_KEY", "or-key")
	auxFixedPoolEntry(t, false, nil)
	client, model, err := GetTextAuxiliaryClient(ctx, "", nil)
	if err != nil || client == nil || model != auxOpenRouterModel {
		t.Fatalf("got %v %q %v", client, model, err)
	}
	if !strings.Contains(strings.ToLower(client.BaseURL()), "openrouter") {
		t.Fatalf("base url %q", client.BaseURL())
	}
}

func TestAuxiliaryClientProviderPriorityNousWhenNoOpenrouter(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxClearUnhealthy(t)
	t.Setenv("OPENROUTER_API_KEY", "")
	ppNousPoolEntry(t, &PooledCredential{Provider: "nous", AccessToken: nousJWT(3600, "inference:invoke", "test-user", nil), Extra: map[string]any{"scope": "inference:invoke"}})
	ppNoRegistryNoNousRuntime(t)
	prevRec := auxGetNousRecommendedAuxModelFn
	auxGetNousRecommendedAuxModelFn = func(context.Context, bool, bool) string { return "" }
	t.Cleanup(func() { auxGetNousRecommendedAuxModelFn = prevRec })
	client, model, err := GetTextAuxiliaryClient(ctx, "", nil)
	if err != nil || client == nil || model != auxNousModel {
		t.Fatalf("got %v %q %v", client, model, err)
	}
}

func TestAuxiliaryClientProviderPriorityCustomEndpointWhenNoNous(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxClearUnhealthy(t)
	t.Setenv("OPENROUTER_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "local-key")
	auxFixedPoolEntry(t, false, nil)
	ppNoRegistryNoNousRuntime(t)
	prevCustom := auxResolveCustomRuntimeFn
	auxResolveCustomRuntimeFn = func(context.Context) (string, any, string, error) {
		return "http://localhost:1234/v1", "local-key", "openai-compatible", nil
	}
	t.Cleanup(func() { auxResolveCustomRuntimeFn = prevCustom })
	client, _, err := GetTextAuxiliaryClient(ctx, "", nil)
	if err != nil || client == nil {
		t.Fatalf("got %v %v", client, err)
	}
	if strings.TrimRight(client.BaseURL(), "/") != "http://localhost:1234/v1" {
		t.Fatalf("base url %q", client.BaseURL())
	}
}

func TestAuxiliaryClientProviderPriorityCodexNotInAutoFallback(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxClearUnhealthy(t)
	for _, name := range []string{"OPENROUTER_API_KEY", "OPENAI_BASE_URL", "OPENAI_API_KEY"} {
		t.Setenv(name, "")
	}
	auxFixedPoolEntry(t, false, nil)
	ppNoRegistryNoNousRuntime(t)
	prevTok := auxReadCodexSingletonTokenFn
	auxReadCodexSingletonTokenFn = func(context.Context) string { return "codex-tok" }
	t.Cleanup(func() { auxReadCodexSingletonTokenFn = prevTok })
	client, model, err := GetTextAuxiliaryClient(ctx, "", nil)
	if err != nil || client != nil || model != "" {
		t.Fatalf("got %v %q %v", client, model, err)
	}
}

func TestCodexReasoningPreflightReasoningItemPassesThrough(t *testing.T) {
	normalized, err := preflightCodexInputItems(jsonList([]map[string]any{
		{"role": "user", "content": "hello"},
		{"type": "reasoning", "encrypted_content": "abc123encrypted", "id": "r_001", "summary": []map[string]any{{"type": "summary_text", "text": "Thinking about it"}}},
		{"role": "assistant", "content": "hi there"},
	}), false, false)
	if err != nil {
		t.Fatal(err)
	}
	reasoning := itemsOfType(normalized, "reasoning")
	if len(reasoning) != 1 || reasoning[0]["encrypted_content"] != "abc123encrypted" {
		t.Fatalf("reasoning items %v", reasoning)
	}
	if _, has := reasoning[0]["id"]; has {
		t.Fatalf("id kept on the normalized reasoning item %v", reasoning[0])
	}
	sameJSON(t, reasoning[0]["summary"], []map[string]any{{"type": "summary_text", "text": "Thinking about it"}}, "summary")
}

func TestCodexReasoningPreflightReasoningItemWithoutId(t *testing.T) {
	normalized, err := preflightCodexInputItems(jsonList([]map[string]any{{"type": "reasoning", "encrypted_content": "abc123"}}), false, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(normalized) != 1 {
		t.Fatalf("normalized %v", normalized)
	}
	if _, has := normalized[0]["id"]; has {
		t.Fatalf("id present %v", normalized[0])
	}
	sameJSON(t, normalized[0]["summary"], []any{}, "summary")
}

func TestCodexReasoningPreflightReasoningItemsReplayedFromHistory(t *testing.T) {
	raw, _ := json.Marshal([]map[string]any{{"type": "reasoning", "encrypted_content": "enc123", "id": "r_1"}})
	items := convertHistory(t, []*state.Message{{Role: "user", Content: "hello"}, {Role: "assistant", Content: "hi", CodexReasoningItems: raw},
		{Role: "user", Content: "follow up"}}, replayOpts())
	reasoning := itemsOfType(items, "reasoning")
	if len(reasoning) != 1 || reasoning[0]["encrypted_content"] != "enc123" {
		t.Fatalf("reasoning items %v", reasoning)
	}
}
