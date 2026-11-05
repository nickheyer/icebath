package provider

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestAnthropicIsOAuthToken(t *testing.T) {
	if !isOAuthToken("sk-ant-oat01-abcdef1234567890") {
		t.Fatal("setup token must be OAuth")
	}
	if isOAuthToken("sk-ant-api03-abcdef1234567890") {
		t.Fatal("console API key must not be OAuth")
	}
	if isOAuthToken("sk-ant-api-user-abcdef1234567890") {
		t.Fatal("user scoped API key must not be OAuth")
	}
}

// Sends one request through the client to the fake endpoint and returns what the wire carried
func anthSendOne(t *testing.T, baseURL, key string) anthCaptured {
	t.Helper()
	srv := newAnthServer(t)
	c := srv.client(srv.route(baseURL, key))
	if _, err := c.Complete(context.Background(), anthUserRequest("claude-sonnet-4-20250514", "hi")); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	return srv.last()
}

func TestAnthropicBuildClientAPIKeyUsesAPIKey(t *testing.T) {
	got := anthSendOne(t, "", "sk-ant-api03-something")
	if got.Header.Get("X-Api-Key") != "sk-ant-api03-something" || got.Header.Get("Authorization") != "" {
		t.Fatalf("api key auth headers wrong: %v", got.Header)
	}
	betas := got.Header.Get("anthropic-beta")
	if !strings.Contains(betas, "interleaved-thinking-2025-05-14") {
		t.Fatalf("common betas missing: %q", betas)
	}
	for _, b := range []string{"context-1m-2025-08-07", "oauth-2025-04-20", "claude-code-20250219"} {
		if strings.Contains(betas, b) {
			t.Fatalf("beta %s must not ride an api key request: %q", b, betas)
		}
	}
	if !strings.HasPrefix(got.URL, "https://api.anthropic.com/v1/messages") {
		t.Fatalf("default endpoint wrong: %s", got.URL)
	}
}

func TestAnthropicBuildClientOpenCodeGetsAttributionHeaders(t *testing.T) {
	got := anthSendOne(t, "https://opencode.ai/zen/go/v1", "sk-opencode-secret")
	if want := attributionHeaders("IcebathAgent")["HTTP-Referer"]; want == "" || got.Header.Get("HTTP-Referer") != want {
		t.Fatalf("HTTP-Referer wrong: %v", got.Header)
	}
	want := attributionHeaders("IcebathAgent")
	if got.Header.Get("X-Title") != want["X-Title"] || got.Header.Get("User-Agent") != want["User-Agent"] {
		t.Fatalf("attribution headers wrong: %v want %v", got.Header, want)
	}
	if got.Header.Get("User-Agent") != UserAgent("IcebathAgent") {
		t.Fatalf("User-Agent wrong: %q", got.Header.Get("User-Agent"))
	}
	if got.Header.Get("X-Api-Key") != "sk-opencode-secret" || got.Header.Get("anthropic-beta") == "" {
		t.Fatalf("opencode auth or betas wrong: %v", got.Header)
	}
}

func TestAnthropicBuildClientMinimaxUsesBearer(t *testing.T) {
	got := anthSendOne(t, "https://api.minimax.io/anthropic", "minimax-secret-123")
	if got.Header.Get("Authorization") != "Bearer minimax-secret-123" {
		t.Fatalf("minimax must use bearer: %v", got.Header)
	}
	if _, has := got.Header["X-Api-Key"]; has {
		t.Fatalf("bearer construction must omit x-api-key: %v", got.Header)
	}
	if got.Header.Get("anthropic-beta") != "interleaved-thinking-2025-05-14" {
		t.Fatalf("minimax betas wrong: %q", got.Header.Get("anthropic-beta"))
	}
}

func TestAnthropicBuildClientAzureFoundryUsesBearer(t *testing.T) {
	got := anthSendOne(t, "https://my-resource.openai.azure.com/anthropic", "azure-foundry-secret-123")
	if got.Header.Get("Authorization") != "Bearer azure-foundry-secret-123" || got.Header.Get("X-Api-Key") != "" {
		t.Fatalf("azure must use bearer: %v", got.Header)
	}
	if !strings.Contains(got.URL, "api-version=2025-04-15") {
		t.Fatalf("azure api-version query missing: %s", got.URL)
	}
	if !strings.Contains(got.Header.Get("anthropic-beta"), "context-1m-2025-08-07") {
		t.Fatalf("azure keeps the 1M beta: %q", got.Header.Get("anthropic-beta"))
	}
}

func TestAnthropicBuildClientPalantirFoundryUsesBearer(t *testing.T) {
	got := anthSendOne(t, "https://acme.palantirfoundry.com/api/v2/llm/proxy/anthropic", "foundry-secret-123")
	if got.Header.Get("Authorization") != "Bearer foundry-secret-123" || got.Header.Get("X-Api-Key") != "" {
		t.Fatalf("palantir must use bearer: %v", got.Header)
	}
}

func TestAnthropicClientDoesNotRetryInternally(t *testing.T) {
	srv := newAnthServer(t, anthReplyJSON(529, map[string]any{"type": "error", "error": map[string]any{"type": "overloaded_error", "message": "busy"}}))
	c := srv.client(srv.route("", "sk-ant-api03-something"))
	if _, err := c.Complete(context.Background(), anthUserRequest("claude-sonnet-4-20250514", "hi")); err == nil {
		t.Fatal("overloaded reply must surface as an error")
	}
	if n := len(srv.requests()); n != 1 {
		t.Fatalf("client must leave retries to the outer loop, sent %d requests", n)
	}
}

func TestAnthropicNormalizeModelName(t *testing.T) {
	if got := normalizeModelName("anthropic/claude-sonnet-4-20250514", false); got != "claude-sonnet-4-20250514" {
		t.Fatalf("prefix strip: %q", got)
	}
	for in, want := range map[string]string{"qwen3.5-plus": "qwen3.5-plus", "anthropic/qwen3.5-plus": "qwen3.5-plus", "qwen3.5-flash": "qwen3.5-flash"} {
		if got := normalizeModelName(in, true); got != want {
			t.Fatalf("preserve dots %q: %q", in, got)
		}
	}
}

func TestAnthropicConvertTools(t *testing.T) {
	tools := anthMsgs(t, `[{"type":"function","function":{"name":"search","description":"Search the web","parameters":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}}}]`)
	got := convertToolsToAnthropic(tools)
	if len(got) != 1 || got[0]["name"] != "search" || got[0]["description"] != "Search the web" {
		t.Fatalf("converted tool wrong: %v", got)
	}
	props := got[0]["input_schema"].(map[string]any)["properties"].(map[string]any)
	if props["query"].(map[string]any)["type"] != "string" {
		t.Fatalf("schema lost: %v", got[0])
	}
	if len(convertToolsToAnthropic([]map[string]any{})) != 0 || len(convertToolsToAnthropic(nil)) != 0 {
		t.Fatal("empty tools must convert to an empty list")
	}
}

func TestAnthropicConvertToolsStripsNullableUnion(t *testing.T) {
	tools := anthMsgs(t, `[{"type":"function","function":{"name":"run","description":"Run command","parameters":{"type":"object","properties":{"command":{"type":"string"},"timeout":{"anyOf":[{"type":"integer"},{"type":"null"}],"default":null}},"required":["command"]}}}]`)
	schema := convertToolsToAnthropic(tools)[0]["input_schema"].(map[string]any)
	anthMustEqualJSON(t, schema["properties"].(map[string]any)["timeout"], map[string]any{"type": "integer", "default": nil})
	anthMustEqualJSON(t, schema["required"], []any{"command"})
}

func TestAnthropicConvertToolsDeduplicatesNames(t *testing.T) {
	var tools []map[string]any
	for _, n := range []string{"lcm_grep", "lcm_describe", "lcm_grep", "lcm_expand", "lcm_describe"} {
		tools = append(tools, map[string]any{"type": "function", "function": map[string]any{"name": n, "description": "Tool " + n, "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}})
	}
	got := convertToolsToAnthropic(tools)
	var names []string
	for _, tool := range got {
		names = append(names, asStr(tool["name"]))
	}
	if strings.Join(names, ",") != "lcm_grep,lcm_describe,lcm_expand" {
		t.Fatalf("duplicates kept: %v", names)
	}
}

func TestAnthropicConvertStripsNonAdjacentToolUse(t *testing.T) {
	msgs := anthMsgs(t, `[{"role":"assistant","content":"","tool_calls":[{"id":"tc_late","function":{"name":"search","arguments":"{}"}}]},
		{"role":"user","content":"actually, something else"},{"role":"assistant","content":"sure"},
		{"role":"tool","tool_call_id":"tc_late","content":"late result"}]`)
	_, result := convertMessagesToAnthropic(msgs, convertOptions{})
	for _, m := range result {
		for _, b := range anthBlocks(m["content"]) {
			if (m["role"] == "assistant" && b["type"] == "tool_use") || (m["role"] == "user" && b["type"] == "tool_result") {
				t.Fatalf("non adjacent tool pair must be stripped: %v", result)
			}
		}
	}
}

func TestAnthropicConvertSystemWithCacheControl(t *testing.T) {
	msgs := anthMsgs(t, `[{"role":"system","content":[{"type":"text","text":"System prompt","cache_control":{"type":"ephemeral"}}]},{"role":"user","content":"Hi"}]`)
	system, _ := convertMessagesToAnthropic(msgs, convertOptions{})
	blocks := anthBlocks(system)
	if len(blocks) == 0 {
		t.Fatalf("system must be a block list: %#v", system)
	}
	anthMustEqualJSON(t, blocks[0]["cache_control"], map[string]any{"type": "ephemeral"})
}

func TestAnthropicConvertAssistantCacheControlPreserved(t *testing.T) {
	msgs := applyAnthropicCacheControl(anthMsgs(t, `[{"role":"system","content":"System prompt"},{"role":"assistant","content":"Hello from assistant"}]`), "5m", false, "", true)
	_, result := convertMessagesToAnthropic(msgs, convertOptions{})
	blocks := anthBlocks(anthFirstRole(result, "assistant")["content"])
	if blocks[0]["type"] != "text" || blocks[0]["text"] != "Hello from assistant" {
		t.Fatalf("assistant block wrong: %v", blocks)
	}
	anthMustEqualJSON(t, blocks[0]["cache_control"], map[string]any{"type": "ephemeral"})
}

func TestAnthropicConvertAssistantToolUseCacheControlPreserved(t *testing.T) {
	msgs := applyAnthropicCacheControl(anthMsgs(t, `[{"role":"system","content":"System prompt"},{"role":"user","content":"Run the tool"},
		{"role":"assistant","content":"","tool_calls":[{"id":"tc_1","function":{"name":"test_tool","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"tc_1","content":"result"}]`), "5m", true, "", true)
	_, result := convertMessagesToAnthropic(msgs, convertOptions{})
	blocks := anthBlocks(anthFirstRole(result, "assistant")["content"])
	last := blocks[len(blocks)-1]
	if last["type"] != "tool_use" || last["id"] != "tc_1" {
		t.Fatalf("tool_use must be last: %v", blocks)
	}
	anthMustEqualJSON(t, last["cache_control"], map[string]any{"type": "ephemeral"})
}

func TestAnthropicOrderedReplayKeepsCacheControlFromNonemptyContent(t *testing.T) {
	msgs := applyAnthropicCacheControl(anthMsgs(t, `[{"role":"system","content":"System prompt"},{"role":"user","content":"Read a.py"},
		{"role":"assistant","content":"I will read a.py now.","anthropic_content_blocks":[{"type":"thinking","thinking":"Need a tool.","signature":"sig_1"},
		{"type":"text","text":"I will read a.py now."},{"type":"tool_use","id":"tc_1","name":"test_tool","input":{}}],
		"tool_calls":[{"id":"tc_1","type":"function","function":{"name":"test_tool","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"tc_1","content":"contents"}]`), "5m", false, "", true)
	_, result := convertMessagesToAnthropic(msgs, convertOptions{})
	blocks := anthBlocks(anthFirstRole(result, "assistant")["content"])
	marked := 0
	for _, b := range blocks {
		if _, ok := b["cache_control"].(map[string]any); ok {
			marked++
		}
	}
	if marked == 0 {
		t.Fatalf("assistant cache breakpoint dropped by ordered replay: %v", blocks)
	}
	if blocks[0]["type"] != "thinking" {
		t.Fatalf("signed thinking must lead: %v", blocks)
	}
}

func TestAnthropicOrderedReplayToolUseCacheControlPreserved(t *testing.T) {
	msgs := applyAnthropicCacheControl(anthMsgs(t, `[{"role":"system","content":"System prompt"},{"role":"user","content":"Run the tool"},
		{"role":"assistant","content":"","anthropic_content_blocks":[{"type":"thinking","thinking":"Need a tool.","signature":"sig_1"},
		{"type":"tool_use","id":"tc_1","name":"test_tool","input":{"query":"raw"}}],
		"tool_calls":[{"id":"tc_1","function":{"name":"test_tool","arguments":"{\"query\":\"redacted\"}"}}]},
		{"role":"tool","tool_call_id":"tc_1","content":"result"}]`), "5m", true, "", true)
	_, result := convertMessagesToAnthropic(msgs, convertOptions{})
	blocks := anthBlocks(anthFirstRole(result, "assistant")["content"])
	if len(blocks) != 2 || blocks[0]["type"] != "thinking" || blocks[1]["type"] != "tool_use" {
		t.Fatalf("replay shape wrong: %v", blocks)
	}
	if _, has := blocks[0]["cache_control"]; has {
		t.Fatalf("thinking must not carry cache_control: %v", blocks[0])
	}
	anthMustEqualJSON(t, blocks[1]["input"], map[string]any{"query": "redacted"})
	anthMustEqualJSON(t, blocks[1]["cache_control"], map[string]any{"type": "ephemeral"})
	if blocks[1]["id"] != "tc_1" {
		t.Fatalf("tool id wrong: %v", blocks[1])
	}
}

func TestAnthropicToolCacheControlPreservedOnToolResult(t *testing.T) {
	msgs := applyAnthropicCacheControl(anthMsgs(t, `[{"role":"system","content":"System prompt"},
		{"role":"assistant","content":"","tool_calls":[{"id":"tc_1","function":{"name":"test_tool","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"tc_1","content":"result"}]`), "5m", true, "", true)
	_, result := convertMessagesToAnthropic(msgs, convertOptions{})
	var block map[string]any
	for _, m := range result {
		if bs := anthBlocks(m["content"]); m["role"] == "user" && len(bs) > 0 && bs[0]["type"] == "tool_result" {
			block = bs[0]
		}
	}
	if block == nil || block["tool_use_id"] != "tc_1" || block["content"] != "result" {
		t.Fatalf("tool_result wrong: %v", result)
	}
	anthMustEqualJSON(t, block["cache_control"], map[string]any{"type": "ephemeral"})
}

func TestAnthropicEmptyUserMessageGetsFillerText(t *testing.T) {
	_, result := convertMessagesToAnthropic(anthMsgs(t, `[{"role":"user","content":""}]`), convertOptions{})
	if result[0]["role"] != "user" || result[0]["content"] != "(empty message)" {
		t.Fatalf("filler text missing: %v", result)
	}
}

func TestAnthropicLeadingAssistantGetsUserTurnPrepended(t *testing.T) {
	system, result := convertMessagesToAnthropic(anthMsgs(t, `[{"role":"system","content":"You are helpful."},
		{"role":"assistant","content":"[Context compaction summary] earlier work..."},{"role":"user","content":"continue"}]`), convertOptions{})
	if system != "You are helpful." {
		t.Fatalf("system wrong: %#v", system)
	}
	anthMustEqualJSON(t, result[0], map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "(empty)"}}})
	if result[1]["role"] != "assistant" {
		t.Fatalf("assistant must follow: %v", result)
	}
	data, _ := json.Marshal(result)
	if !strings.Contains(string(data), "Context compaction summary") {
		t.Fatalf("summary lost: %s", data)
	}
}

func anthUserOnly(text string) []map[string]any {
	return []map[string]any{{"role": "user", "content": text}}
}

func TestAnthropicManualThinkingForPre46Models(t *testing.T) {
	enabled := true
	kw, err := anthBuildKwargs(anthBuildInput{model: "claude-sonnet-4-20250514", messages: anthUserOnly("think hard"), maxTokens: 4096,
		reasoning: &anthReasoningConfig{enabled: &enabled, effort: "high"}})
	if err != nil {
		t.Fatal(err)
	}
	thinking := kw["thinking"].(map[string]any)
	if thinking["type"] != "enabled" || thinking["budget_tokens"] != 16000 {
		t.Fatalf("manual thinking wrong: %v", thinking)
	}
	if kw["temperature"] != 1 && kw["temperature"] != 1.0 {
		t.Fatalf("manual thinking forces temperature 1: %v", kw["temperature"])
	}
	if kw["max_tokens"].(int) < 16000+4096 {
		t.Fatalf("max_tokens must cover budget: %v", kw["max_tokens"])
	}
	if _, has := kw["output_config"]; has {
		t.Fatal("manual path must not send output_config")
	}
}

func TestAnthropicAdaptiveThinkingFor46Models(t *testing.T) {
	enabled := true
	kw, _ := anthBuildKwargs(anthBuildInput{model: "claude-opus-4-6", messages: anthUserOnly("think hard"), maxTokens: 4096,
		reasoning: &anthReasoningConfig{enabled: &enabled, effort: "high"}})
	anthMustEqualJSON(t, kw["thinking"], map[string]any{"type": "adaptive", "display": "summarized"})
	anthMustEqualJSON(t, kw["output_config"], map[string]any{"effort": "high"})
	if _, has := kw["temperature"]; has || kw["max_tokens"] != 4096 {
		t.Fatalf("adaptive kwargs wrong: %v", kw)
	}
}

func TestAnthropicUnknownClaudeModelsClassifyModern(t *testing.T) {
	for _, m := range []string{"claude-fable-5", "anthropic/claude-fable-5", "claude-saga-2", "anthropic/claude-opus-9"} {
		if !anthSupportsAdaptiveThinking(m) || !anthSupportsXHighEffort(m) || !anthForbidsSamplingParams(m) {
			t.Fatalf("%s must classify modern", m)
		}
	}
	for _, m := range []string{"minimax-m2", "qwen3-max", "glm-4.6"} {
		if anthSupportsAdaptiveThinking(m) || anthSupportsXHighEffort(m) || anthForbidsSamplingParams(m) {
			t.Fatalf("%s must stay on the manual path", m)
		}
	}
}

func TestAnthropicBareK3SlugIsKimiFamily(t *testing.T) {
	for _, m := range []string{"k3", "K3", "moonshotai/k3", "k3.1-preview", "k3-turbo"} {
		if !modelNameIsKimiFamily(m) {
			t.Fatalf("%s must be kimi family", m)
		}
	}
	if !anthSupportsAdaptiveThinking("k3") {
		t.Fatal("k3 must use adaptive thinking")
	}
	for _, m := range []string{"k30", "k3000-chat", "keras-3"} {
		if modelNameIsKimiFamily(m) {
			t.Fatalf("%s must not be kimi family", m)
		}
	}
}

func TestAnthropicFastModeOmittedForUnsupportedModel(t *testing.T) {
	kw, _ := anthBuildKwargs(anthBuildInput{model: "claude-opus-4-7", messages: anthUserOnly("hi"), maxTokens: 1024, fastMode: true})
	if extra, ok := kw["extra_body"].(map[string]any); ok && extra["speed"] != nil {
		t.Fatalf("speed injected for unsupported model: %v", extra)
	}
	if h, ok := kw["extra_headers"].(map[string]string); ok && strings.Contains(h["anthropic-beta"], "fast-mode-2026-02-01") {
		t.Fatalf("fast mode beta injected: %v", h)
	}
}

func TestAnthropicNormalizeResponse(t *testing.T) {
	c := &anthropicClient{route: Route{Provider: "anthropic", Model: "claude-sonnet-4-20250514"}, transport: newAnthDirectTransport(Route{})}
	resp, err := c.normalizeResponse(anthMessage("tool_use", anthText("Searching..."), anthToolUse("tc_1", "search", map[string]any{"query": "test"})), 200, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message.Content != "Searching..." || resp.FinishReason != "tool_calls" || len(resp.Message.ToolCalls) != 1 || resp.Message.ToolCalls[0].Function.Name != "search" {
		t.Fatalf("tool use response wrong: %+v", resp.Message)
	}
	anthMustEqualJSON(t, anthJSON(t, resp.Message.ToolCalls[0].Function.Arguments), map[string]any{"query": "test"})

	resp, _ = c.normalizeResponse(anthMessage("end_turn", map[string]any{"type": "thinking", "thinking": "Let me reason about this..."}, anthText("The answer is 42.")), 200, nil, 0)
	if resp.Message.Content != "The answer is 42." || resp.Message.Reasoning != "Let me reason about this..." {
		t.Fatalf("thinking response wrong: %+v", resp.Message)
	}
	anthMustEqualJSON(t, anthJSON(t, string(resp.Message.ReasoningDetails)), []any{map[string]any{"type": "thinking", "thinking": "Let me reason about this..."}})

	for stop, want := range map[string]string{"end_turn": "stop", "tool_use": "tool_calls", "max_tokens": "length"} {
		resp, _ = c.normalizeResponse(anthMessage(stop, anthText("x")), 200, nil, 0)
		if resp.FinishReason != want {
			t.Fatalf("%s maps to %q, want %q", stop, resp.FinishReason, want)
		}
	}
}

func TestAnthropicRoleAlternation(t *testing.T) {
	_, result := convertMessagesToAnthropic(anthMsgs(t, `[{"role":"user","content":"Hello"},{"role":"user","content":"World"}]`), convertOptions{})
	anthMustEqualJSON(t, result, []any{map[string]any{"role": "user", "content": []any{anthText("Hello"), anthText("World")}}})
	_, result = convertMessagesToAnthropic(anthMsgs(t, `[{"role":"user","content":"Hi"},{"role":"assistant","content":"Hello!"},{"role":"user","content":"How are you?"}]`), convertOptions{})
	if len(result) != 3 || result[0]["role"] != "user" || result[1]["role"] != "assistant" || result[2]["role"] != "user" {
		t.Fatalf("alternation broken: %v", result)
	}
}

func TestAnthropicThinkingSignatureManagement(t *testing.T) {
	_, result := convertMessagesToAnthropic(anthMsgs(t, `[{"role":"assistant","content":"Response.","reasoning_details":[{"type":"redacted_thinking","data":"opaque_signature_data"}]}]`), convertOptions{})
	n := 0
	for _, b := range anthBlocks(anthFirstRole(result, "assistant")["content"]) {
		if b["type"] == "redacted_thinking" {
			n++
			if b["data"] != "opaque_signature_data" {
				t.Fatalf("redacted data lost: %v", b)
			}
		}
	}
	if n != 1 {
		t.Fatalf("redacted thinking with data must be kept: %v", result)
	}

	_, result = convertMessagesToAnthropic(anthMsgs(t, `[{"role":"assistant","content":"Response.","reasoning_details":[{"type":"redacted_thinking"}]}]`), convertOptions{})
	for _, m := range result {
		for _, b := range anthBlocks(m["content"]) {
			if b["type"] == "redacted_thinking" {
				t.Fatalf("redacted thinking without data must drop: %v", result)
			}
		}
	}

	_, result = convertMessagesToAnthropic(anthMsgs(t, `[{"role":"assistant","content":"","tool_calls":[{"id":"tc_1","function":{"name":"t","arguments":"{}"}}],
		"reasoning_details":[{"type":"thinking","thinking":"Reasoning.","signature":"sig_1","cache_control":{"type":"ephemeral"}}]},
		{"role":"tool","tool_call_id":"tc_1","content":"result"}]`), convertOptions{})
	for _, b := range anthBlocks(anthFirstRole(result, "assistant")["content"]) {
		if _, has := b["cache_control"]; has && (b["type"] == "thinking" || b["type"] == "redacted_thinking") {
			t.Fatalf("cache_control must be stripped from thinking: %v", b)
		}
	}

	_, result = convertMessagesToAnthropic(anthMsgs(t, `[{"role":"user","content":"Question 1"},
		{"role":"assistant","content":"Answer 1","reasoning_details":[{"type":"thinking","thinking":"Thought 1","signature":"sig_1"}]},
		{"role":"user","content":"Question 2"},
		{"role":"assistant","content":"Answer 2","reasoning_details":[{"type":"thinking","thinking":"Thought 2","signature":"sig_2"}]},
		{"role":"user","content":"Question 3"},
		{"role":"assistant","content":"Answer 3","reasoning_details":[{"type":"thinking","thinking":"Thought 3","signature":"sig_3"}]}]`), convertOptions{})
	var assistants []map[string]any
	for _, m := range result {
		if m["role"] == "assistant" {
			assistants = append(assistants, m)
		}
	}
	if len(assistants) != 3 {
		t.Fatalf("assistants: %v", result)
	}
	for _, a := range assistants[:2] {
		for _, b := range anthBlocks(a["content"]) {
			if b["type"] == "thinking" || b["type"] == "redacted_thinking" {
				t.Fatalf("older turns must drop thinking: %v", a)
			}
		}
	}
	var last []map[string]any
	for _, b := range anthBlocks(assistants[2]["content"]) {
		if b["type"] == "thinking" {
			last = append(last, b)
		}
	}
	if len(last) != 1 || last[0]["signature"] != "sig_3" {
		t.Fatalf("latest turn must keep its signed thinking: %v", assistants[2])
	}
}

var anthDummyTool = []map[string]any{{"type": "function", "function": map[string]any{"name": "test", "description": "x", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}}}

func TestAnthropicToolChoice(t *testing.T) {
	kw, _ := anthBuildKwargs(anthBuildInput{model: "claude-sonnet-4-20250514", messages: anthUserOnly("Hi"), tools: anthDummyTool, maxTokens: 4096, toolChoice: "auto"})
	anthMustEqualJSON(t, kw["tool_choice"], map[string]any{"type": "auto"})
	kw, _ = anthBuildKwargs(anthBuildInput{model: "claude-sonnet-4-20250514", messages: anthUserOnly("Hi"), tools: anthDummyTool, maxTokens: 4096, toolChoice: "search"})
	anthMustEqualJSON(t, kw["tool_choice"], map[string]any{"type": "tool", "name": "search"})
}

func TestAnthropicResolveMaxTokens(t *testing.T) {
	for _, v := range []any{0, math.NaN(), true, false} {
		if _, ok := anthResolvePositiveMaxTokens(v); ok {
			t.Fatalf("%v must not resolve", v)
		}
	}
	if got, err := anthResolveMessagesMaxTokens(8192, "claude-opus-4-6"); err != nil || got != 8192 {
		t.Fatalf("positive requested wins: %d %v", got, err)
	}
	if got, err := anthResolveMessagesMaxTokens(0.5, "claude-opus-4-6"); err != nil || got <= 0 {
		t.Fatalf("sub one float falls back: %d %v", got, err)
	}
}

func anthConvertAssistant(t *testing.T, text string) []map[string]any {
	t.Helper()
	return anthBlocks(convertAssistantMessage(anthJSON(t, text).(map[string]any), convertOptions{})["content"])
}

func anthCountType(blocks []map[string]any, typ string) int {
	n := 0
	for _, b := range blocks {
		if b["type"] == typ {
			n++
		}
	}
	return n
}

func anthMarked(blocks []map[string]any) []map[string]any {
	var out []map[string]any
	for _, b := range blocks {
		if _, ok := b["cache_control"].(map[string]any); ok {
			out = append(out, b)
		}
	}
	return out
}

func TestAnthropicBlankTextBlockFiltering(t *testing.T) {
	blocks := anthConvertAssistant(t, `{"role":"assistant","content":[{"type":"text","text":null},{"type":"tool_use","id":"call_none","name":"web_search","input":{"query":"test"}}]}`)
	if anthCountType(blocks, "text") != 0 || anthCountType(blocks, "tool_use") != 1 {
		t.Fatalf("null text must be filtered: %v", blocks)
	}

	blocks = anthConvertAssistant(t, `{"role":"assistant","content":[{"type":"text","text":"I'll look that up."},
		{"type":"tool_use","id":"call_cache","name":"web_search","input":{"query":"test"}},{"type":"text","text":"","cache_control":{"type":"ephemeral"}}]}`)
	for _, b := range blocks {
		if s, _ := b["text"].(string); b["type"] == "text" && strings.TrimSpace(s) == "" {
			t.Fatalf("blank text must drop: %v", blocks)
		}
	}
	if m := anthMarked(blocks); len(m) != 1 || m[0]["type"] != "tool_use" {
		t.Fatalf("marker must relocate to tool_use: %v", blocks)
	}

	blocks = anthConvertAssistant(t, `{"role":"assistant","content":"","anthropic_content_blocks":[{"type":"tool_use","id":"call_5","name":"web_search","input":{"query":"test"}},
		{"type":"text","text":"  ","cache_control":{"type":"ephemeral"}}],"tool_calls":[{"id":"call_5","function":{"name":"web_search","arguments":"{\"query\": \"test\"}"}}]}`)
	if anthCountType(blocks, "text") != 0 {
		t.Fatalf("blank replay text must drop: %v", blocks)
	}
	if m := anthMarked(blocks); len(m) != 1 || m[0]["type"] != "tool_use" {
		t.Fatalf("replay marker must relocate to tool_use: %v", blocks)
	}
}

func TestAnthropicAllBlankFallbackAndNonStringText(t *testing.T) {
	blocks := anthConvertAssistant(t, `{"role":"assistant","content":[{"type":"text","text":"","cache_control":{"type":"ephemeral"}}]}`)
	anthMustEqualJSON(t, blocks, []any{map[string]any{"type": "text", "text": "(empty)", "cache_control": map[string]any{"type": "ephemeral"}}})

	blocks = anthConvertAssistant(t, `{"role":"assistant","content":[{"type":"text","text":7},{"type":"tool_use","id":"call_int","name":"web_search","input":{"query":"test"}}]}`)
	if anthCountType(blocks, "text") != 0 || anthCountType(blocks, "tool_use") != 1 {
		t.Fatalf("non string text must drop: %v", blocks)
	}

	blocks = anthConvertAssistant(t, `{"role":"assistant","content":[{"type":"text","text":{"nested":"garbage"}}],"tool_calls":[{"id":"call_d","function":{"name":"web_search","arguments":"{\"query\": \"test\"}"}}]}`)
	if anthCountType(blocks, "text") != 0 {
		t.Fatalf("dict text must drop: %v", blocks)
	}
}

func TestAnthropicReplayAllBlankFallback(t *testing.T) {
	blocks := anthConvertAssistant(t, `{"role":"assistant","content":"","anthropic_content_blocks":[{"type":"text","text":" ","cache_control":{"type":"ephemeral"}}]}`)
	anthMustEqualJSON(t, blocks, []any{map[string]any{"type": "text", "text": "(empty)", "cache_control": map[string]any{"type": "ephemeral"}}})

	blocks = anthConvertAssistant(t, `{"role":"assistant","content":"","anthropic_content_blocks":[{"type":"thinking","thinking":"reasoning","signature":"sig-A"},
		{"type":"text","text":"  ","cache_control":{"type":"ephemeral"}}]}`)
	anthMustEqualJSON(t, blocks[0], map[string]any{"type": "thinking", "thinking": "reasoning", "signature": "sig-A"})
	if m := anthMarked(blocks); len(m) != 1 || m[0]["type"] != "text" || strings.TrimSpace(asStr(m[0]["text"])) == "" {
		t.Fatalf("marker must land on a non blank filler block: %v", blocks)
	}

	blocks = anthConvertAssistant(t, `{"role":"assistant","content":"","anthropic_content_blocks":[{"type":"thinking","thinking":"reasoning","signature":"sig-B"},{"type":"text","text":"\n"}]}`)
	var texts []map[string]any
	for _, b := range blocks {
		if b["type"] == "text" {
			texts = append(texts, b)
		}
	}
	anthMustEqualJSON(t, texts, []any{anthText("(empty)")})
}

func TestAnthropicFinalPayloadHasNoBlankTextBlocks(t *testing.T) {
	conv := func(text string) []map[string]any {
		_, result := convertMessagesToAnthropic(anthMsgs(t, text), convertOptions{})
		if v := anthBlankTextBlocks(result); len(v) > 0 {
			t.Fatalf("blank text blocks %v in %v", v, result)
		}
		return result
	}
	for _, text := range []string{`""`, `"   "`} {
		if r := conv(`[{"role":"user","content":` + text + `}]`); r[0]["content"] != "(empty message)" {
			t.Fatalf("blank user string: %v", r)
		}
	}
	anthMustEqualJSON(t, conv(`[{"role":"user","content":[{"type":"text","text":""}]}]`)[0]["content"], []any{anthText("(empty message)")})
	anthMustEqualJSON(t, conv(`[{"role":"user","content":[{"type":"text","text":"real question"},{"type":"text","text":"   "}]}]`)[0]["content"], []any{anthText("real question")})

	r := conv(`[{"role":"user","content":"call a tool"},{"role":"assistant","content":"","tool_calls":[{"id":"call_1","function":{"name":"web_search","arguments":"{\"query\": \"x\"}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"result text"}]`)
	if anthCountType(anthBlocks(anthFirstRole(r, "assistant")["content"]), "tool_use") != 1 {
		t.Fatalf("tool_use lost: %v", r)
	}
	found := false
	for _, m := range r {
		if m["role"] == "user" && anthCountType(anthBlocks(m["content"]), "tool_result") > 0 {
			found = true
		}
	}
	if !found {
		t.Fatalf("tool_result lost: %v", r)
	}

	r = conv(`[{"role":"user","content":"do it"},{"role":"assistant","content":"   ","tool_calls":[{"id":"call_2","function":{"name":"web_search","arguments":"{\"query\": \"y\"}"}}]},
		{"role":"tool","tool_call_id":"call_2","content":"ok"}]`)
	anthMustEqualJSON(t, anthFirstRole(r, "assistant")["content"], []any{anthToolUse("call_2", "web_search", map[string]any{"query": "y"})})

	r = conv(`[{"role":"system","content":"sys"},{"role":"assistant","content":"[Context compaction summary] earlier work"},{"role":"user","content":"continue"}]`)
	anthMustEqualJSON(t, r[0]["content"], []any{anthText("(empty)")})

	r = conv(`[{"role":"user","content":"screenshot please"},{"role":"assistant","content":"","tool_calls":[{"id":"call_3","function":{"name":"screenshot","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call_3","content":[{"type":"text","text":"   "},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]`)
	images := 0
	for _, m := range r {
		for _, b := range anthBlocks(m["content"]) {
			if b["type"] == "tool_result" {
				images += anthCountType(anthBlocks(b["content"]), "image")
			}
		}
	}
	if images != 1 {
		t.Fatalf("tool_result image lost: %v", r)
	}
}

func TestAnthropicOAuthSystemPromptSanitizerPreservesDocsURL(t *testing.T) {
	sys := "Hermes Agent by Nous Research uses hermes-agent skills. Docs: https://hermes-agent.nousresearch.com/docs ; " +
		"interpreter ~/.hermes/hermes-agent/venv/bin/python ; source github.com/NousResearch/hermes-agent ; mail hermes-agent@example.com ; " +
		"skill_view(name='hermes-agent') ; hermes-agent's docs ; built by hermes-agent."
	kw, err := anthBuildKwargs(anthBuildInput{model: "claude-sonnet-4-20250514", messages: []map[string]any{{"role": "system", "content": sys}, {"role": "user", "content": "Hi"}},
		maxTokens: 4096, isOAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	blocks := anthBlocks(kw["system"])
	var texts []string
	for _, b := range blocks {
		texts = append(texts, asStr(b["text"]))
	}
	all := strings.Join(texts, "\n")
	for _, want := range []string{"Claude Code by Anthropic uses claude-code skills.", "https://hermes-agent.nousresearch.com/docs", "~/.hermes/hermes-agent/venv/bin/python",
		"github.com/NousResearch/hermes-agent", "hermes-agent@example.com", "skill_view(name='hermes-agent')", "built by claude-code.", "claude-code's docs"} {
		if !strings.Contains(all, want) {
			t.Fatalf("system missing %q: %s", want, all)
		}
	}
	if n := strings.Count(texts[len(texts)-1], "claude-code"); n != 3 {
		t.Fatalf("caller block carries %d claude-code slugs, want 3: %s", n, texts[len(texts)-1])
	}
}

func TestAnthropicUnsupportedInlineImageSubtypeDowngradesToText(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, result := convertMessagesToAnthropic(anthMsgs(t, `[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}},
		{"type":"image_url","image_url":{"url":"data:image/svg+xml;base64,PHN2Zy8+"}},{"type":"image_url","image_url":{"url":"data:image/jpg;base64,/9j/4AAQ"}}]}]`), convertOptions{})
	blocks := anthBlocks(result[0]["content"])
	if len(blocks) != 3 || blocks[0]["type"] != "image" || blocks[1]["type"] != "text" || blocks[2]["type"] != "image" {
		t.Fatalf("block types wrong: %v", blocks)
	}
	anthMustEqualJSON(t, blocks[0]["source"], map[string]any{"type": "base64", "media_type": "image/png", "data": "iVBORw0KGgo="})
	if !strings.Contains(asStr(blocks[1]["text"]), "image/svg+xml") || blocks[2]["source"].(map[string]any)["media_type"] != "image/jpeg" {
		t.Fatalf("svg downgrade or jpg normalize wrong: %v", blocks)
	}
}
