package agent

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// Tool definitions the hermes parity fixture patches into model_tools.get_tool_definitions
func parityToolDefs(names ...string) []any {
	out := make([]any, 0, len(names))
	for _, n := range names {
		out = append(out, map[string]any{"type": "function", "function": map[string]any{"name": n, "description": n + " tool",
			"parameters": map[string]any{"type": "object", "properties": map[string]any{}}}})
	}
	return out
}

// Agent built like tests/agent/test_provider_parity.py _make_agent with web_search and terminal as its tools
func parityAgent(t *testing.T, providerName, apiMode, baseURL, model string) *AIAgent {
	t.Helper()
	if model == "" && providerName == "nous" {
		model = "gpt-5"
	}
	a := newFixtureAgent(t, AIAgentOptions{Provider: providerName, APIMode: apiMode, BaseURL: baseURL, Model: model, MaxIterations: new(4)})
	a.tools = parityToolDefs("web_search", "terminal")
	return a
}

// JSON shaped copy of a value so assertions read it like the Python dict
func parityJSON(t *testing.T, v any) any {
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

// Value under a path of object keys and list indexes, nil for an absent key
func parityAt(t *testing.T, v any, path ...any) any {
	t.Helper()
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

// True when the object under path holds key
func parityHas(t *testing.T, v any, key string, path ...any) bool {
	t.Helper()
	m, ok := parityAt(t, v, path...).(map[string]any)
	if !ok {
		t.Fatalf("no object at %v", path)
	}
	_, has := m[key]
	return has
}

// API kwargs of the agent in JSON shape
func parityKwargs(t *testing.T, a *AIAgent, messages []map[string]any) map[string]any {
	t.Helper()
	kwargs, err := a.buildAPIKwargs(messages, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	return parityJSON(t, kwargs).(map[string]any)
}

// Fails unless both values agree in JSON shape
func paritySame(t *testing.T, got, want any, label string) {
	t.Helper()
	if !reflect.DeepEqual(parityJSON(t, got), parityJSON(t, want)) {
		t.Fatalf("%s: got %v want %v", label, got, want)
	}
}

// Chat history carrying the Codex only tool call fields the OpenRouter and Fireworks cases strip
func parityCodexFieldHistory(id, itemID string, reasoningItems bool, extra map[string]any) []map[string]any {
	tc := map[string]any{"id": id, "call_id": id, "response_item_id": itemID, "type": "function",
		"function": map[string]any{"name": "terminal", "arguments": "{\"command\":\"pwd\"}"}}
	if extra != nil {
		tc["extra_content"] = extra
	}
	assistant := map[string]any{"role": "assistant", "content": "Checking now.", "tool_calls": []any{tc}}
	if reasoningItems {
		assistant["codex_reasoning_items"] = []any{map[string]any{"type": "reasoning", "id": "rs_1", "encrypted_content": "blob"}}
	}
	return []map[string]any{{"role": "user", "content": "hi"}, assistant, {"role": "tool", "tool_call_id": id, "content": "/tmp"}}
}

// Gemini history whose tool call carries a google thought signature before an image request
func parityGeminiImageHistory(signature string) []map[string]any {
	return []map[string]any{
		{"role": "user", "content": "use a tool first"},
		{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "call_123", "type": "function",
			"function":      map[string]any{"name": "terminal", "arguments": "{}"},
			"extra_content": map[string]any{"google": map[string]any{"thought_signature": signature}}}}},
		{"role": "tool", "tool_call_id": "call_123", "content": "ok"},
		{"role": "user", "content": []any{map[string]any{"type": "text", "text": "describe this image"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AA=="}}}},
	}
}

func TestBuildApiKwargsOpenRouterIncludesReasoningInExtraBody(t *testing.T) {
	a := parityAgent(t, "openrouter", "chat_completions", "https://openrouter.ai/api/v1", "")
	a.model = "anthropic/claude-sonnet-4-20250514"
	kwargs := parityKwargs(t, a, []map[string]any{{"role": "user", "content": "hi"}})
	if !parityHas(t, kwargs, "reasoning", "extra_body") || parityAt(t, kwargs, "extra_body", "reasoning", "enabled") != true {
		t.Fatalf("extra_body %v", kwargs["extra_body"])
	}
}

func TestBuildApiKwargsOpenRouterNoResponsesApiFields(t *testing.T) {
	a := parityAgent(t, "openrouter", "chat_completions", "https://openrouter.ai/api/v1", "")
	kwargs := parityKwargs(t, a, []map[string]any{{"role": "user", "content": "hi"}})
	for _, key := range []string{"input", "instructions", "store"} {
		if parityHas(t, kwargs, key) {
			t.Fatalf("%s present in chat completions kwargs", key)
		}
	}
}

func TestBuildApiKwargsOpenRouterStripsCodexOnlyToolCallFieldsFromChatMessages(t *testing.T) {
	a := parityAgent(t, "openrouter", "chat_completions", "https://openrouter.ai/api/v1", "")
	messages := parityCodexFieldHistory("call_123", "fc_123", true, map[string]any{"thought_signature": "opaque"})
	before := parityJSON(t, messages)
	kwargs := parityKwargs(t, a, messages)
	if parityHas(t, kwargs, "codex_reasoning_items", "messages", 1) {
		t.Fatal("codex_reasoning_items reached the chat wire")
	}
	tc := parityAt(t, kwargs, "messages", 1, "tool_calls", 0)
	if parityAt(t, tc, "id") != "call_123" || parityAt(t, tc, "function", "name") != "terminal" {
		t.Fatalf("tool call %v", tc)
	}
	for _, key := range []string{"extra_content", "call_id", "response_item_id"} {
		if parityHas(t, tc, key) {
			t.Fatalf("%s kept on the wire copy", key)
		}
	}
	paritySame(t, messages, before, "stored history")
	if parityAt(t, parityJSON(t, messages), 1, "tool_calls", 0, "call_id") != "call_123" || !parityHas(t, parityJSON(t, messages), "codex_reasoning_items", 1) {
		t.Fatal("stored history lost its Codex fields")
	}
}

func TestBuildApiKwargsOpenRouterKeepsExtraContentForGeminiTarget(t *testing.T) {
	a := parityAgent(t, "openrouter", "chat_completions", "https://openrouter.ai/api/v1", "google/gemini-3-pro-preview")
	signature := map[string]any{"google": map[string]any{"thought_signature": "opaque"}}
	messages := parityCodexFieldHistory("call_123", "fc_123", false, signature)
	before := parityJSON(t, messages)
	tc := parityAt(t, parityKwargs(t, a, messages), "messages", 1, "tool_calls", 0)
	paritySame(t, parityAt(t, tc, "extra_content"), signature, "extra_content")
	if parityHas(t, tc, "call_id") || parityHas(t, tc, "response_item_id") {
		t.Fatalf("Responses ids kept: %v", tc)
	}
	paritySame(t, messages, before, "stored history")
}

func TestBuildApiKwargsOpenRouterGeminiImageRequestDropsCorruptedThoughtSignature(t *testing.T) {
	a := parityAgent(t, "openrouter", "chat_completions", "https://openrouter.ai/api/v1", "google/gemini-3.8-flash")
	messages := parityGeminiImageHistory("")
	kwargs := parityKwargs(t, a, messages)
	if parityHas(t, kwargs, "extra_content", "messages", 1, "tool_calls", 0) {
		t.Fatal("empty thought signature replayed with the image request")
	}
	paritySame(t, parityAt(t, parityJSON(t, messages), 1, "tool_calls", 0, "extra_content"),
		map[string]any{"google": map[string]any{"thought_signature": ""}}, "stored extra_content")
}

func TestBuildApiKwargsOpenRouterGeminiImageRequestKeepsNonemptyThoughtSignature(t *testing.T) {
	a := parityAgent(t, "openrouter", "chat_completions", "https://openrouter.ai/api/v1", "google/gemini-3.8-flash")
	kwargs := parityKwargs(t, a, parityGeminiImageHistory("signed-call"))
	paritySame(t, parityAt(t, kwargs, "messages", 1, "tool_calls", 0, "extra_content"),
		map[string]any{"google": map[string]any{"thought_signature": "signed-call"}}, "extra_content")
}

func TestBuildApiKwargsOpenRouterGeminiNativePassesBaseUrlForTopLevelThinkingConfig(t *testing.T) {
	a := parityAgent(t, "gemini", "chat_completions", "https://generativelanguage.googleapis.com/v1beta", "gemini-3-flash-preview")
	a.reasoningConfig = map[string]any{"enabled": true, "effort": "high"}
	kwargs := parityKwargs(t, a, []map[string]any{{"role": "user", "content": "hi"}})
	paritySame(t, parityAt(t, kwargs, "extra_body", "thinking_config"), map[string]any{"includeThoughts": true, "thinkingLevel": "high"}, "thinking_config")
	if parityHas(t, kwargs, "extra_body", "extra_body") {
		t.Fatal("extra_body nested inside extra_body")
	}
}

func TestDeveloperRoleSwapGpt5CodexGetDeveloperRole(t *testing.T) {
	for _, model := range []string{"openai/gpt-5", "openai/codex-mini"} {
		a := parityAgent(t, "openrouter", "chat_completions", "https://openrouter.ai/api/v1", "")
		a.model = model
		kwargs := parityKwargs(t, a, []map[string]any{{"role": "system", "content": "You are helpful."}, {"role": "user", "content": "hi"}})
		if parityAt(t, kwargs, "messages", 0, "role") != "developer" || parityAt(t, kwargs, "messages", 0, "content") != "You are helpful." ||
			parityAt(t, kwargs, "messages", 1, "role") != "user" {
			t.Fatalf("%s: messages %v", model, kwargs["messages"])
		}
	}
}

func TestBuildApiKwargsChatCompletionsServiceTierIncludesServiceTierViaRequestOverrides(t *testing.T) {
	a := parityAgent(t, "openrouter", "chat_completions", "https://openrouter.ai/api/v1", "")
	a.model = "gpt-4.1"
	a.requestOverrides = map[string]any{"service_tier": "priority"}
	if kwargs := parityKwargs(t, a, []map[string]any{{"role": "user", "content": "hi"}}); kwargs["service_tier"] != "priority" {
		t.Fatalf("service_tier %v", kwargs["service_tier"])
	}
}

func TestBuildApiKwargsKimiNoTemperatureOverrideKimiForCodingOmitsTemperature(t *testing.T) {
	a := parityAgent(t, "kimi-coding", "chat_completions", "https://api.kimi.com/coding/v1", "kimi-for-coding")
	if kwargs := parityKwargs(t, a, []map[string]any{{"role": "user", "content": "hi"}}); parityHas(t, kwargs, "temperature") {
		t.Fatalf("temperature sent: %v", kwargs["temperature"])
	}
}

func TestBuildApiKwargsAIGatewayIncludesReasoningInExtraBody(t *testing.T) {
	a := parityAgent(t, "ai-gateway", "chat_completions", "https://ai-gateway.vercel.sh/v1", "gpt-4o")
	kwargs := parityKwargs(t, a, []map[string]any{{"role": "user", "content": "hi"}})
	if !parityHas(t, kwargs, "reasoning", "extra_body") || parityAt(t, kwargs, "extra_body", "reasoning", "enabled") != true {
		t.Fatalf("extra_body %v", kwargs["extra_body"])
	}
}

func TestBuildApiKwargsNousPortalIncludesNousProductTags(t *testing.T) {
	a := parityAgent(t, "nous", "chat_completions", "https://inference-api.nousresearch.com/v1", "gpt-5")
	kwargs := parityKwargs(t, a, []map[string]any{{"role": "user", "content": "hi"}})
	paritySame(t, parityAt(t, kwargs, "extra_body", "tags"), provider.NousPortalTags(context.Background(), a.sessionID), "tags")
}

func TestBuildApiKwargsNousPortalConnectorDataUseAsksForANoTrainingRouteAndNeverReachesTheWire(t *testing.T) {
	cases := []struct {
		dataUse  []any
		provider map[string]any
	}{
		{nil, nil},
		{[]any{"no_training"}, map[string]any{"data_collection": "deny"}},
		{[]any{"zero_retention"}, map[string]any{"data_collection": "deny", "zdr": true}},
	}
	for _, tc := range cases {
		a := parityAgent(t, "nous", "chat_completions", "https://inference-api.nousresearch.com/v1", "gpt-5")
		tool := map[string]any{"role": "tool", "tool_call_id": "c1", "content": "inbox"}
		last := map[string]any{"role": "tool", "tool_call_id": "c1", "content": "inbox"}
		if tc.dataUse != nil {
			last["data_use"] = tc.dataUse
		}
		kwargs := parityKwargs(t, a, []map[string]any{
			{"role": "user", "content": "read my mail"},
			{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "c1", "type": "function",
				"function": map[string]any{"name": "tool_call", "arguments": "{}"}}}},
			last,
		})
		var want any
		if tc.provider != nil {
			want = tc.provider
		}
		paritySame(t, parityAt(t, kwargs, "extra_body", "provider"), want, "provider routing")
		messages := kwargs["messages"].([]any)
		paritySame(t, messages[len(messages)-1], tool, "wire tool message")
	}
}

func TestBuildApiKwargsNousPortalConnectorDataUseIsNotSentToOtherProviders(t *testing.T) {
	a := parityAgent(t, "openrouter", "chat_completions", "https://openrouter.ai/api/v1", "")
	kwargs := parityKwargs(t, a, []map[string]any{
		{"role": "user", "content": "hi"},
		{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "c1", "type": "function",
			"function": map[string]any{"name": "tool_call", "arguments": "{}"}}}},
		{"role": "tool", "tool_call_id": "c1", "content": "inbox", "data_use": []any{"no_training"}},
	})
	if extra, _ := kwargs["extra_body"].(map[string]any); extra != nil && extra["provider"] != nil {
		t.Fatalf("provider routing sent to openrouter: %v", extra["provider"])
	}
	messages := kwargs["messages"].([]any)
	if parityHas(t, messages[len(messages)-1], "data_use") {
		t.Fatal("data_use reached the wire")
	}
}

func TestBuildApiKwargsCustomEndpointFireworksToolCallPayloadStripsCodexOnlyFields(t *testing.T) {
	a := parityAgent(t, "custom", "chat_completions", "https://api.fireworks.ai/inference/v1", "")
	kwargs := parityKwargs(t, a, parityCodexFieldHistory("call_fw_123", "fc_fw_123", true, nil))
	if parityAt(t, kwargs, "tools", 0, "function", "name") != "web_search" || parityHas(t, kwargs, "input") {
		t.Fatalf("kwargs %v", kwargs)
	}
	if extra, _ := kwargs["extra_body"].(map[string]any); len(extra) != 0 {
		t.Fatalf("extra_body %v", extra)
	}
	assistant := parityAt(t, kwargs, "messages", 1)
	tc := parityAt(t, assistant, "tool_calls", 0)
	if parityHas(t, assistant, "codex_reasoning_items") || parityAt(t, tc, "id") != "call_fw_123" || parityAt(t, tc, "type") != "function" ||
		parityAt(t, tc, "function", "name") != "terminal" || parityHas(t, tc, "call_id") || parityHas(t, tc, "response_item_id") {
		t.Fatalf("assistant %v", assistant)
	}
}

func TestBuildApiKwargsCodexUsesResponsesApiFormat(t *testing.T) {
	a := parityAgent(t, "openai-codex", "codex_responses", "https://chatgpt.com/backend-api/codex", "")
	kwargs := parityKwargs(t, a, []map[string]any{{"role": "user", "content": "hi"}})
	if !parityHas(t, kwargs, "input") || !parityHas(t, kwargs, "instructions") || parityHas(t, kwargs, "messages") || kwargs["store"] != false {
		t.Fatalf("kwargs %v", kwargs)
	}
}

func TestBuildApiKwargsCodexIncludesServiceTierViaRequestOverrides(t *testing.T) {
	a := parityAgent(t, "openai-codex", "codex_responses", "https://chatgpt.com/backend-api/codex", "")
	a.model = "gpt-5.4"
	a.serviceTier = "priority"
	a.requestOverrides = map[string]any{"service_tier": "priority"}
	if kwargs := parityKwargs(t, a, []map[string]any{{"role": "user", "content": "hi"}}); kwargs["service_tier"] != "priority" {
		t.Fatalf("service_tier %v", kwargs["service_tier"])
	}
}

func TestBuildApiKwargsCodexToolsConvertedToResponsesFormat(t *testing.T) {
	a := parityAgent(t, "openai-codex", "codex_responses", "https://chatgpt.com/backend-api/codex", "")
	kwargs := parityKwargs(t, a, []map[string]any{{"role": "user", "content": "hi"}})
	tools, _ := kwargs["tools"].([]any)
	if len(tools) == 0 || !parityHas(t, tools[0], "name") || parityHas(t, tools[0], "function") {
		t.Fatalf("tools %v", tools)
	}
}

func TestBuildAssistantMessageOpenrouterReasoningFields(t *testing.T) {
	a := parityAgent(t, "openrouter", "chat_completions", "https://openrouter.ai/api/v1", "")
	result := a.buildAssistantMessage(&provider.Response{Message: &state.Message{Role: "assistant", Content: "answer", Reasoning: "I thought about it"}}, "stop")
	if result["content"] != "answer" || result["reasoning"] != "I thought about it" {
		t.Fatalf("result %v", result)
	}
	if _, has := result["codex_reasoning_items"]; has {
		t.Fatalf("codex_reasoning_items on a chat completions row: %v", result)
	}
}

func TestBuildAssistantMessageOpenrouterReasoningDetailsPreservedUnmodified(t *testing.T) {
	a := parityAgent(t, "openrouter", "chat_completions", "https://openrouter.ai/api/v1", "")
	detail := map[string]any{"type": "thinking", "thinking": "deep thoughts here", "signature": "sig123_opaque_blob",
		"encrypted_content": "some_provider_blob", "extra_field": "should_not_be_dropped"}
	raw, _ := json.Marshal([]any{detail})
	result := a.buildAssistantMessage(&provider.Response{Message: &state.Message{Role: "assistant", Content: "answer", ReasoningDetails: raw}}, "stop")
	paritySame(t, parityAt(t, parityJSON(t, result), "reasoning_details", 0), detail, "stored detail")
}

func TestBuildAssistantMessageCodexPreservesEncryptedReasoning(t *testing.T) {
	a := parityAgent(t, "openai-codex", "codex_responses", "https://chatgpt.com/backend-api/codex", "")
	items := []any{map[string]any{"type": "reasoning", "id": "rs_1", "encrypted_content": "gAAAA_blob"}}
	raw, _ := json.Marshal(items)
	result := a.buildAssistantMessage(&provider.Response{Message: &state.Message{Role: "assistant", Content: "result", Reasoning: "summary text", CodexReasoningItems: raw}}, "stop")
	paritySame(t, result["codex_reasoning_items"], items, "codex_reasoning_items")
}

func TestProviderRoutingSortThroughput(t *testing.T) {
	a := parityAgent(t, "openrouter", "chat_completions", "https://openrouter.ai/api/v1", "")
	a.providerSort = "throughput"
	kwargs := parityKwargs(t, a, []map[string]any{{"role": "user", "content": "hi"}})
	if parityAt(t, kwargs, "extra_body", "provider", "sort") != "throughput" {
		t.Fatalf("extra_body %v", kwargs["extra_body"])
	}
}
