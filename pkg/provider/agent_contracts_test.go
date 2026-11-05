package provider

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"
)

func carrierKeys(set map[string]bool) []string {
	out := []string{}
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestGetUnregisteredTransportReturnsNil(t *testing.T) {
	if tr := GetTransport("nonexistent_mode"); tr != nil {
		t.Fatalf("got %T", tr)
	}
	want := []string{"anthropic_messages", "bedrock_converse", "chat_completions", "codex_responses"}
	if got := carrierKeys(RegisteredAPIModes()); !reflect.DeepEqual(got, want) {
		t.Fatalf("registered %v", got)
	}
	for _, mode := range want {
		if tr := GetTransport(mode); tr == nil || tr.APIMode() != mode {
			t.Fatalf("%s resolved to %v", mode, tr)
		}
	}
}

func TestRegisteredPluginTransportIsAnAcceptedAPIMode(t *testing.T) {
	if RegisteredAPIModes()["example_dialect"] {
		t.Fatal("dialect registered before the plugin")
	}
	RegisterTransport("example_dialect", func() Transport { return chatCompletionsTransport{} })
	t.Cleanup(func() {
		transportsMu.Lock()
		delete(transports, "example_dialect")
		transportsMu.Unlock()
	})
	if !RegisteredAPIModes()["example_dialect"] || RegisteredAPIModes()["bogus_mode"] {
		t.Fatalf("modes %v", RegisteredAPIModes())
	}
	if RegisteredAPIModes()["gemini_native"] {
		t.Fatal("gemini_native has an adapter but no transport")
	}
}

func TestAnthropicTransportThroughTheRegistry(t *testing.T) {
	tr := GetTransport("anthropic_messages")
	tools, _ := tr.ConvertTools([]map[string]any{{"type": "function", "function": map[string]any{
		"name": "test_tool", "description": "A test", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}}}).([]map[string]any)
	if len(tools) != 1 || tools[0]["name"] != "test_tool" || tools[0]["input_schema"] == nil {
		t.Fatalf("tools %v", tools)
	}
	for in, want := range map[string]string{"end_turn": "stop", "tool_use": "tool_calls", "max_tokens": "length", "stop_sequence": "stop",
		"refusal": "content_filter", "model_context_window_exceeded": "length", "unknown": "stop"} {
		if got := tr.MapFinishReason(in); got != want {
			t.Fatalf("%s mapped to %s", in, got)
		}
	}
	msg := map[string]any{"type": "message", "role": "assistant", "model": "claude-sonnet-4-6", "stop_reason": "end_turn",
		"content": []any{map[string]any{"type": "text", "text": "Hello world"}}, "usage": map[string]any{"input_tokens": 10.0, "output_tokens": 5.0}}
	resp, err := tr.NormalizeResponse(msg, nil)
	if err != nil || resp.Message.Content != "Hello world" || len(resp.Message.ToolCalls) != 0 || resp.FinishReason != "stop" {
		t.Fatalf("resp %+v err %v", resp, err)
	}
	if !tr.ValidateResponse(msg) || tr.ExtractCacheStats(msg) != nil {
		t.Fatal("validate or cache stats")
	}
	msg["usage"] = map[string]any{"cache_read_input_tokens": 7.0}
	if got := tr.ExtractCacheStats(msg); !reflect.DeepEqual(got, map[string]int{"cached_tokens": 7, "creation_tokens": 0}) {
		t.Fatalf("cache stats %v", got)
	}
}

func TestResponsesAndBedrockStopReasonTables(t *testing.T) {
	codex := GetTransport("codex_responses")
	for in, want := range map[string]string{"completed": "stop", "incomplete": "length", "failed": "stop", "cancelled": "stop", "other": "stop"} {
		if got := codex.MapFinishReason(in); got != want {
			t.Fatalf("%s mapped to %s", in, got)
		}
	}
	if GetTransport("chat_completions").MapFinishReason("whatever") != "whatever" {
		t.Fatal("chat completions passes reasons through")
	}
	if GetTransport("bedrock_converse").ValidateResponse(map[string]any{"x": 1}) {
		t.Fatal("converse dict without output validated")
	}
}

func TestChatTransportCacheStatsAndValidation(t *testing.T) {
	tr := GetTransport("chat_completions")
	resp := map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "hi"}}},
		"usage": map[string]any{"prompt_tokens_details": map[string]any{"cached_tokens": 3.0, "cache_write_tokens": 2.0}}}
	if !tr.ValidateResponse(resp) || !reflect.DeepEqual(tr.ExtractCacheStats(resp), map[string]int{"cached_tokens": 3, "creation_tokens": 2}) {
		t.Fatal("chat stats")
	}
	resp["usage"] = map[string]any{"prompt_cache_hit_tokens": 9.0}
	if !reflect.DeepEqual(tr.ExtractCacheStats(resp), map[string]int{"cached_tokens": 9, "creation_tokens": 0}) {
		t.Fatal("deepseek stats")
	}
	if tr.ValidateResponse(map[string]any{"choices": []any{}}) {
		t.Fatal("empty choices validated")
	}
}

func TestNormalizeFinishReasonFoldsToContract(t *testing.T) {
	for in, want := range map[string]string{"STOP": "stop", "MAX_TOKENS": "length", "Tool_Calls": "tool_calls", "end": "stop",
		"function_call": "tool_calls", "content_filter": "content_filter"} {
		if got := NormalizeFinishReason(in); got != want {
			t.Fatalf("%s folded to %v", in, got)
		}
	}
	for _, raw := range []any{nil, "", 24} {
		if got := NormalizeFinishReason(raw); got != raw {
			t.Fatalf("%v changed to %v", raw, got)
		}
	}
}

func TestReasoningEchoFamilies(t *testing.T) {
	cases := []struct{ provider, model, base, family string }{
		{"kimi-coding", "", "", "kimi"},
		{"custom", "m", "https://api.moonshot.ai/v1", "kimi"},
		{"DeepSeek", "", "", "deepseek"},
		{"openrouter", "deepseek/deepseek-v4", "https://openrouter.ai/api/v1", "deepseek"},
		{"xiaomi", "", "", "mimo"},
		{"nous", "stealth/missingno", "", "missingno"},
		{"openrouter", "anthropic/claude", "https://openrouter.ai/api/v1", ""},
	}
	for _, c := range cases {
		if got := ReasoningEchoFamily(c.provider, c.model, c.base); got != c.family {
			t.Fatalf("%+v got %q", c, got)
		}
		if NeedsReasoningEcho(c.provider, c.model, c.base) != (c.family != "") {
			t.Fatalf("%+v needs echo", c)
		}
	}
	if ok, err := MatchesReasoningEchoFamily("kimi", "KIMI-CODING", "", ""); ok || err != nil {
		t.Fatal("kimi raw providers match case sensitively")
	}
	if _, err := MatchesReasoningEchoFamily("nope", "", "", ""); err == nil || err.Error() != "KeyError: 'nope'" {
		t.Fatalf("unknown family %v", err)
	}
}

func TestEachRouteGetsExactlyTheCarriersItReads(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	cases := []struct {
		provider, base, model string
		want                  []string
	}{
		{"mistral", "https://api.mistral.ai/v1", "deepseek-r1", []string{}},
		{"custom", "https://api.groq.com/openai/v1", "m", []string{}},
		{"custom", "https://api.cerebras.ai/v1", "m", []string{}},
		{"fireworks", "https://api.fireworks.ai/inference/v1", "m", []string{"reasoning_content"}},
		{"nous", "https://inference-api.nousresearch.com/v1", "z-ai/glm-5.3", []string{"reasoning", "reasoning_content", "reasoning_details"}},
		{"kilocode", "https://api.kilo.ai/api/gateway", "m", []string{"reasoning", "reasoning_content", "reasoning_details"}},
		{"novita", "https://api.novita.ai/openai/v1", "m", []string{"reasoning_content", "reasoning_details"}},
		{"ollama-cloud", "https://ollama.com/v1", "m", []string{"reasoning", "reasoning_content"}},
		{"custom", "http://127.0.0.1:8080/v1", "m", []string{"reasoning", "reasoning_content"}},
		{"zai", "https://api.z.ai/api/paas/v4", "glm-5.3", []string{"reasoning_content"}},
		{"openai-api", "https://api.openai.com/v1", "gpt-5", []string{"reasoning_content"}},
	}
	for _, c := range cases {
		route := ReasoningReplayRouteFor("chat_completions", c.provider, c.model, c.base, false, nil)
		if got := carrierKeys(route.Carriers); !reflect.DeepEqual(got, c.want) {
			t.Fatalf("%s %s carriers %v want %v", c.provider, c.base, got, c.want)
		}
	}
	strict := ReasoningReplayRouteFor("chat_completions", "mistral", "deepseek-r1", "https://api.mistral.ai/v1", false, nil)
	if strict.Pad || strict.Carriers == nil || len(strict.Carriers) != 0 {
		t.Fatalf("strict route %+v", strict)
	}
	pad := ReasoningReplayRouteFor("", "deepseek", "deepseek-v4-flash", "https://api.deepseek.com/v1", false, nil)
	if !pad.Pad || !pad.Carriers[carrierRC] {
		t.Fatalf("must echo route %+v", pad)
	}
	rejected := ReasoningReplayRouteFor("chat_completions", "deepseek", "deepseek-v4-flash", "https://api.deepseek.com/v1", false, map[string]bool{carrierRC: true})
	if rejected.Pad || len(rejected.Carriers) != 0 {
		t.Fatalf("rejected route %+v", rejected)
	}
	native := ReasoningReplayRouteFor("anthropic_messages", "custom", "m", "", true, nil)
	if !native.Pad || native.Carriers != nil {
		t.Fatalf("native route %+v", native)
	}
	if again := ReasoningReplayRouteFor("chat_completions", "fireworks", "m", "", false, nil); again.Carriers[carrierR] {
		t.Fatal("route table mutated by a caller")
	}
}

func TestRejectedReasoningFieldsUseUnicodeWordBoundaries(t *testing.T) {
	sent := []any{map[string]any{"role": "user", "reasoning": "x"},
		map[string]any{"role": "assistant", "reasoning_content": "t", "reasoning": "t", "reasoning_details": []any{}}}
	cases := []struct {
		body any
		want []string
	}{
		{"Extra inputs are not permitted: messages.1.reasoning_details", []string{"reasoning", "reasoning_details"}},
		{"Unknown field reasoning_content", []string{"reasoning", "reasoning_content", "reasoning_details"}},
		{"unknown parameter: reasoning", []string{}},
		{"unknown field: messages[1].reasoning is unsupported", []string{"reasoning", "reasoning_details"}},
		{"unknown field éreasoning_content", []string{}},
		{"unknown field reasoning_content١", []string{}},
		{"unknown field ́reasoning_details", []string{"reasoning", "reasoning_details"}},
		{"value must not be empty: reasoning_content", []string{}},
		{map[string]any{"error": "Unknown field reasoning_content"}, []string{"reasoning", "reasoning_content", "reasoning_details"}},
		{nil, []string{}},
	}
	for _, c := range cases {
		if got := carrierKeys(RejectedReasoningFields(c.body, sent)); !reflect.DeepEqual(got, c.want) {
			t.Fatalf("%v got %v want %v", c.body, got, c.want)
		}
	}
	if got := RejectedReasoningFields("unknown field reasoning_content", "not a list"); len(got) != 0 {
		t.Fatalf("non list %v", got)
	}
}

func TestNativeAnthropicAccountingProjection(t *testing.T) {
	signed := map[string]any{"type": "thinking", "thinking": "deep", "signature": "sig"}
	messages := []any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "assistant", "content": "raw", "api_content": "wire", "reasoning_content": "storage only",
			"reasoning": "dropped", "timestamp": 1.0, "reasoning_details": []any{signed}},
		map[string]any{"role": "assistant", "content": "plain", "reasoning_content": "kept"},
		"not a dict",
	}
	projected, thinking := NativeAnthropicAccountingProjection(messages)
	list := projected.([]any)
	want := []any{messages[0], map[string]any{"role": "assistant", "content": "wire"},
		map[string]any{"role": "assistant", "content": "plain", "reasoning_content": "kept"}, "not a dict"}
	if !reflect.DeepEqual(list, want) || !reflect.DeepEqual(thinking, []string{"deep"}) {
		t.Fatalf("projected %v thinking %v", list, thinking)
	}
	if same, none := NativeAnthropicAccountingProjection("x"); same != "x" || none != nil {
		t.Fatal("non list passes through")
	}
}

func TestRepairPassZeroReserialisesCompactASCII(t *testing.T) {
	cases := map[string]string{
		`{"a": 1}`:                     `{"a":1}`,
		"{\"code\": \"line1\nline2\"}": `{"code":"line1\nline2"}`,
		` {"k": "é"} `:                 "{\"k\":\"\\u00e9\"}",
		"　{\"a\": [1, 2]}　":            `{"a":[1,2]}`,
	}
	for in, want := range cases {
		if got := RepairToolCallArguments(in, "t"); got != want {
			t.Fatalf("%q got %q want %q", in, got, want)
		}
	}
}

func TestStreamingRepairTruncatedObjectAndEmpty(t *testing.T) {
	var parsed map[string]any
	if err := json.Unmarshal([]byte(RepairToolCallArguments(`{"command": "ls -la", "timeout": 30`, "terminal")), &parsed); err != nil ||
		parsed["command"] != "ls -la" || parsed["timeout"] != 30.0 {
		t.Fatalf("truncated object %v %v", parsed, err)
	}
	if RepairToolCallArguments("", "test") != "{}" {
		t.Fatal("empty arguments")
	}
}
