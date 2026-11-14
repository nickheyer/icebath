package provider

import (
	"strings"
	"testing"
)

func anthKw(t *testing.T, model string, cfg *anthReasoningConfig, baseURL string) map[string]any {
	t.Helper()
	kw, err := anthBuildKwargs(anthBuildInput{model: model, messages: anthUserOnly("hello"), maxTokens: 4096, reasoning: cfg, baseURL: baseURL})
	if err != nil {
		t.Fatal(err)
	}
	return kw
}

func anthOn(effort string) *anthReasoningConfig {
	on := true
	return &anthReasoningConfig{enabled: &on, effort: effort}
}

func anthOff(effort string) *anthReasoningConfig {
	off := false
	return &anthReasoningConfig{enabled: &off, effort: effort}
}

func TestAnthropicThinkingOffIsSentExplicitly(t *testing.T) {
	for _, m := range []string{"anthropic/claude-opus-5", "claude-opus-4-6"} {
		anthMustEqualJSON(t, anthKw(t, m, anthOff(""), "")["thinking"], map[string]any{"type": "disabled"})
		kw := anthKw(t, m, anthOff("high"), "")
		anthMustEqualJSON(t, kw["thinking"], map[string]any{"type": "disabled"})
		if _, has := kw["output_config"]; has {
			t.Fatalf("%s disable must carry no effort dial: %v", m, kw)
		}
	}
	for _, m := range []string{"anthropic/claude-fable-5", "claude-opus-5-5", "anthropic/claude-opus-5.5", "claude-mythos-5-1", "anthropic/claude-mythos-5.1"} {
		if _, has := anthKw(t, m, anthOff(""), "")["thinking"]; has {
			t.Fatalf("%s is reasoning mandatory and must keep the omission", m)
		}
	}
	for _, m := range []string{"claude-sonnet-5-5", "anthropic/claude-sonnet-5.5"} {
		anthMustEqualJSON(t, anthKw(t, m, anthOff(""), "")["thinking"], map[string]any{"type": "between_tools"})
	}
	anthMustEqualJSON(t, anthKw(t, "claude-sonnet-5", anthOff(""), "")["thinking"], map[string]any{"type": "disabled"})
	for _, c := range []struct{ model, base string }{{"claude-sonnet-4-5", ""}, {"anthropic/claude-haiku-4.5", ""}, {"kimi-k2.5", "https://api.kimi.com/coding"}} {
		if _, has := anthKw(t, c.model, anthOff(""), c.base)["thinking"]; has {
			t.Fatalf("%s must keep the omission", c.model)
		}
	}
}

func TestAnthropicThinkingEnablePathIsUnchanged(t *testing.T) {
	kw := anthKw(t, "anthropic/claude-opus-5", anthOn("high"), "")
	anthMustEqualJSON(t, kw["thinking"], map[string]any{"type": "adaptive", "display": "summarized"})
	anthMustEqualJSON(t, kw["output_config"], map[string]any{"effort": "high"})
	kw = anthKw(t, "anthropic/claude-fable-5", anthOn("max"), "")
	anthMustEqualJSON(t, kw["thinking"], map[string]any{"type": "adaptive", "display": "summarized"})
	anthMustEqualJSON(t, kw["output_config"], map[string]any{"effort": "max"})
	thinking, _ := anthKw(t, "claude-sonnet-4-5", anthOn("high"), "")["thinking"].(map[string]any)
	if thinking["type"] != "enabled" || thinking["budget_tokens"].(int) <= 0 {
		t.Fatalf("legacy enable must send budget tokens: %v", thinking)
	}
	if _, has := anthKw(t, "anthropic/claude-haiku-4.5", anthOn("high"), "")["thinking"]; has {
		t.Fatal("pre thinking haiku never gets thinking")
	}
	anthMustEqualJSON(t, anthKw(t, "anthropic/claude-haiku-5.5", anthOn("high"), "")["thinking"], map[string]any{"type": "adaptive", "display": "summarized"})
	if _, has := anthKw(t, "anthropic/claude-opus-5", nil, "")["thinking"]; has {
		t.Fatal("no reasoning config sends no thinking field")
	}
}

func TestAnthropicThinkingDisableVerdict(t *testing.T) {
	for _, m := range []string{"minimax-m2.7", "qwen3-max", "kimi-k2.5"} {
		if anthAcceptsThinkingDisable(m) {
			t.Fatalf("%s must be left alone", m)
		}
	}
	if !anthAcceptsThinkingDisable("anthropic/claude-opus-6") {
		t.Fatal("unknown claude releases default to disableable")
	}
}

func anthInterleavedResponse(t *testing.T) *Response {
	t.Helper()
	c := &anthropicClient{route: Route{Provider: "anthropic", Model: "claude-opus-4-8"}, transport: newAnthDirectTransport(Route{})}
	msg := anthMessage("tool_use",
		map[string]any{"type": "thinking", "thinking": "Plan: inspect file A first.", "signature": "sig-AAA"},
		anthToolUse("toolu_1", "read_file", map[string]any{"path": "a.py"}),
		map[string]any{"type": "thinking", "thinking": "A looked fine; now inspect B.", "signature": "sig-BBB"},
		anthToolUse("toolu_2", "read_file", map[string]any{"path": "b.py"}))
	msg["usage"] = nil
	resp, err := c.normalizeResponse(msg, 200, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func anthReplayedOrder(content any) []string {
	var order []string
	for _, b := range anthBlocks(content) {
		switch b["type"] {
		case "thinking", "redacted_thinking":
			order = append(order, "thinking:"+asStr(b["signature"]))
		case "tool_use":
			order = append(order, "tool_use:"+asStr(b["id"]))
		}
	}
	return order
}

func TestAnthropicInterleavedOrderPreservedOnReplay(t *testing.T) {
	stored := messageToDict(anthInterleavedResponse(t).Message)
	msgs := []map[string]any{{"role": "user", "content": "Inspect a.py and b.py."}, stored,
		{"role": "tool", "tool_call_id": "toolu_1", "content": "a.py: ok"}, {"role": "tool", "tool_call_id": "toolu_2", "content": "b.py: ok"}}
	_, out := convertMessagesToAnthropic(msgs, convertOptions{model: "claude-opus-4-8"})
	var last map[string]any
	for _, m := range out {
		if m["role"] == "assistant" {
			last = m
		}
	}
	anthMustEqualJSON(t, anthReplayedOrder(last["content"]), []string{"thinking:sig-AAA", "tool_use:toolu_1", "thinking:sig-BBB", "tool_use:toolu_2"})
}

func TestAnthropicReplayFallsBackWithoutOrderedBlocks(t *testing.T) {
	stored := messageToDict(anthInterleavedResponse(t).Message)
	delete(stored, "anthropic_content_blocks")
	msgs := []map[string]any{stored, {"role": "tool", "tool_call_id": "toolu_1", "content": "a ok"}, {"role": "tool", "tool_call_id": "toolu_2", "content": "b ok"}}
	_, out := convertMessagesToAnthropic(msgs, convertOptions{model: "claude-opus-4-8"})
	var last map[string]any
	for _, m := range out {
		if m["role"] == "assistant" {
			last = m
		}
	}
	ids := map[string]bool{}
	for _, b := range anthBlocks(last["content"]) {
		if b["type"] == "tool_use" {
			ids[asStr(b["id"])] = true
		}
	}
	if len(anthBlocks(last["content"])) == 0 || len(ids) != 2 || !ids["toolu_1"] || !ids["toolu_2"] {
		t.Fatalf("fallback replay wrong: %v", last)
	}
}

func TestAnthropicInterleavedReplayResourcesRedactedToolInput(t *testing.T) {
	msgs := anthMsgs(t, `[{"role":"user","content":"Hit the API twice."},
		{"role":"assistant","content":"","reasoning_details":[{"type":"thinking","thinking":"Call the API.","signature":"sig-AAA"},{"type":"thinking","thinking":"Now the second call.","signature":"sig-BBB"}],
		"tool_calls":[{"id":"toolu_1","type":"function","function":{"name":"terminal","arguments":"{\"command\": \"curl -H 'Authorization: Bearer [REDACTED_SECRET]'\"}"}},
		{"id":"toolu_2","type":"function","function":{"name":"terminal","arguments":"{\"command\": \"echo done\"}"}}],
		"anthropic_content_blocks":[{"type":"thinking","thinking":"Call the API.","signature":"sig-AAA"},
		{"type":"tool_use","id":"toolu_1","name":"terminal","input":{"command":"curl -H 'Authorization: Bearer sk-LIVE-SECRET-123'"}},
		{"type":"thinking","thinking":"Now the second call.","signature":"sig-BBB"},{"type":"tool_use","id":"toolu_2","name":"terminal","input":{"command":"echo done"}}]},
		{"role":"tool","tool_call_id":"toolu_1","content":"200 OK"},{"role":"tool","tool_call_id":"toolu_2","content":"done"}]`)
	_, out := convertMessagesToAnthropic(msgs, convertOptions{model: "claude-opus-4-8"})
	var last map[string]any
	for _, m := range out {
		if m["role"] == "assistant" {
			last = m
		}
	}
	uses := map[string]map[string]any{}
	for _, b := range anthBlocks(last["content"]) {
		if b["type"] == "tool_use" {
			uses[asStr(b["id"])] = b
		}
	}
	if len(uses) != 2 || uses["toolu_1"] == nil || uses["toolu_2"] == nil {
		t.Fatalf("tool_use blocks missing: %v", last)
	}
	cmd := asStr(uses["toolu_1"]["input"].(map[string]any)["command"])
	if strings.Contains(cmd, "sk-LIVE-SECRET-123") || !strings.Contains(cmd, "[REDACTED_SECRET]") {
		t.Fatalf("tool input must come from the redacted tool_calls: %q", cmd)
	}
	anthMustEqualJSON(t, anthReplayedOrder(last["content"]), []string{"thinking:sig-AAA", "tool_use:toolu_1", "thinking:sig-BBB", "tool_use:toolu_2"})
}

func anthAssistantThinking(out []map[string]any, types ...string) []map[string]any {
	want := map[string]bool{"thinking": true}
	if len(types) > 0 {
		want = map[string]bool{}
		for _, t := range types {
			want[t] = true
		}
	}
	var blocks []map[string]any
	for _, b := range anthBlocks(anthFirstRole(out, "assistant")["content"]) {
		if want[asStr(b["type"])] {
			blocks = append(blocks, b)
		}
	}
	return blocks
}

func TestAnthropicDeepSeekPreservesThinking(t *testing.T) {
	_, out := convertMessagesToAnthropic(anthMsgs(t, `[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"thinking","thinking":"anthropic-signed payload","signature":"anthropic-sig-xyz"},
		{"type":"text","text":"hello"}]},{"role":"user","content":"again"}]`), convertOptions{baseURL: "https://api.deepseek.com/anthropic"})
	if th := anthAssistantThinking(out); len(th) != 0 {
		t.Fatalf("signed anthropic thinking must strip on deepseek: %v", th)
	}
	_, out = convertMessagesToAnthropic(anthMsgs(t, `[{"role":"user","content":"hi"},{"role":"assistant","reasoning_content":"r1",
		"tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_1","content":"ok"}]`),
		convertOptions{baseURL: "https://api.deepseek.com/anthropic"})
	for _, m := range out {
		for _, b := range anthBlocks(m["content"]) {
			if _, has := b["cache_control"]; has && (b["type"] == "thinking" || b["type"] == "redacted_thinking") {
				t.Fatalf("cache_control on thinking: %v", b)
			}
		}
	}
}

func TestAnthropicDeepSeekModelNameDoesNotOverrideNativeSignatureContract(t *testing.T) {
	block := map[string]any{"type": "thinking", "thinking": "signed native reasoning", "signature": "sig"}
	for _, u := range []string{"", "https://api.anthropic.com", "https://inference-api.nousresearch.com/anthropic"} {
		msgs := []map[string]any{{"role": "assistant", "content": []any{map[string]any{"type": "thinking", "thinking": "signed native reasoning", "signature": "sig"}, anthText("answer")}}}
		manageThinkingSignatures(msgs, u, "deepseek-v4")
		anthMustEqualJSON(t, anthBlocks(msgs[0]["content"])[0], block)
	}
}

func TestAnthropicDeepSeekProxyKeepsUnsignedThinkingInOlderToolTurnsOnly(t *testing.T) {
	for _, c := range []struct {
		model string
		kept  []any
	}{
		{"vendor/deepseek-v4", []any{map[string]any{"type": "thinking", "thinking": "unsigned"}}},
		{" DeepSeek-Pro ", []any{map[string]any{"type": "thinking", "thinking": "unsigned"}}},
		{"deepseek-chat", []any{}},
		{"vendor/other-model", []any{}},
	} {
		history := anthMsgs(t, `[{"role":"user","content":"inspect"},{"role":"assistant","content":"checking","reasoning_details":[
			{"type":"thinking","thinking":"unsigned","cache_control":{"type":"ephemeral"}},{"type":"thinking","thinking":"foreign signed","signature":"sig"},
			{"type":"redacted_thinking","data":"redacted-signature"}],"tool_calls":[{"id":"call_1","type":"function","function":{"name":"inspect","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"ok"},{"role":"assistant","content":"done"}]`)
		snapshot := anthCloneMsgs(history)
		_, out := convertMessagesToAnthropic(history, convertOptions{baseURL: "https://proxy.example/anthropic", model: c.model})
		got := anthAssistantThinking(out, "thinking", "redacted_thinking")
		if got == nil {
			got = []map[string]any{}
		}
		anthMustEqualJSON(t, got, c.kept)
		if !anthJSONEqual(history, snapshot) {
			t.Fatalf("%s mutated history", c.model)
		}
	}
}

func TestAnthropicKimiCodingThinking(t *testing.T) {
	if _, has := anthKw(t, "kimi-k2.5", anthOff(""), "https://api.kimi.com/coding")["thinking"]; has {
		t.Fatal("kimi coding explicit disable omits thinking")
	}
	if th, _ := anthKw(t, "MiniMax-M2.7", anthOn("medium"), "https://api.minimax.io/anthropic")["thinking"].(map[string]any); th["type"] != "enabled" {
		t.Fatalf("non kimi third party gets manual thinking: %v", th)
	}
	if _, has := anthKw(t, "claude-sonnet-4-20250514", anthOn("medium"), "")["thinking"]; !has {
		t.Fatal("native anthropic gets thinking")
	}
	for _, c := range [][2]string{{"https://api.kimi.com/coding", "kimi-k2.5"}, {"https://api.kimi.com/coding/v1", "kimi-k2.5"}, {"https://api.kimi.com/coding/anthropic", "kimi-k2.5"},
		{"https://api.kimi.com/v1", "kimi-k2.5"}, {"https://api.moonshot.ai/anthropic", "moonshot-v1-32k"}, {"https://api.moonshot.cn/anthropic", "moonshot-v1-32k"},
		{"https://api.moonshot.cn/anthropic/v1", "kimi-0714-preview"}, {"http://my-kimi-proxy.internal", "kimi-2.6"}, {"https://llm.example.com/anthropic", "moonshotai/kimi-k2.5"}} {
		kw := anthKw(t, c[1], anthOn("high"), c[0])
		th, _ := kw["thinking"].(map[string]any)
		if th["type"] != "adaptive" || th["budget_tokens"] != nil {
			t.Fatalf("%s %s must get adaptive thinking: %v", c[0], c[1], th)
		}
		anthMustEqualJSON(t, kw["output_config"], map[string]any{"effort": "high"})
		if _, has := kw["temperature"]; has || kw["max_tokens"] != 4096 {
			t.Fatalf("%s %s kwargs wrong: %v", c[0], c[1], kw)
		}
	}
	_, out := convertMessagesToAnthropic(anthMsgs(t, `[{"role":"user","content":"hi"},{"role":"assistant","reasoning_content":"planning the tool call",
		"tool_calls":[{"id":"call_1","type":"function","function":{"name":"skill_view","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_1","content":"ok"}]`),
		convertOptions{baseURL: "http://my-kimi-proxy.internal", model: "kimi-2.6"})
	if th := anthAssistantThinking(out); len(th) != 1 || th[0]["thinking"] != "planning the tool call" {
		t.Fatalf("kimi replay must keep unsigned thinking: %v", th)
	}
}
