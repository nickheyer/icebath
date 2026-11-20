package provider

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

const (
	bedrockFRClaude  = "global.anthropic.claude-opus-5-5"
	bedrockFRGPT     = "us.openai.gpt-6.1-sol"
	bedrockFRURL     = "https://bedrock-runtime.us-east-1.amazonaws.com"
	bedrockFRBlob    = "R1BULXNlYWxlZC1yZWFzb25pbmctYmxvYg=="
	bedrockFRProfile = "arn:aws:bedrock:us-east-1:123456789012:application-inference-profile/abc123"
)

func bedrockGPTTurn(answer string, stamped bool, toolCall map[string]any) map[string]any {
	details := []any{map[string]any{"type": "thinking", "thinking": "gpt readable summary"}, map[string]any{"type": "redacted_thinking", "data": bedrockFRBlob}}
	if stamped {
		details = append(details, map[string]any{"type": BedrockConverseIssuerType, "model": bedrockFRGPT})
	}
	turn := map[string]any{"role": "assistant", "content": answer, "reasoning_details": details}
	if toolCall != nil {
		turn["tool_calls"] = []any{toolCall}
	}
	return turn
}

func bedrockClaudeTurn(answer, sig, issuer string) map[string]any {
	details := []any{map[string]any{"type": "thinking", "thinking": "claude chain", "signature": sig}}
	if issuer != "" {
		details = append(details, map[string]any{"type": BedrockConverseIssuerType, "model": issuer})
	}
	return map[string]any{"role": "assistant", "content": answer, "reasoning_details": details}
}

// Converts history for Claude with the Bedrock transport's profile resolver bound
func bedrockFRConvert(t *testing.T, history []map[string]any, baseURL string) []map[string]any {
	tr, _ := bedrockTransport(Route{Region: "us-east-1"})
	_, wire := convertMessagesToAnthropic(history, convertOptions{baseURL: baseURL, model: bedrockFRClaude, resolveProfileARN: anthProfileResolver(context.Background(), tr)})
	return wire
}

func bedrockWireThinking(wire []map[string]any) []map[string]any {
	var out []map[string]any
	for _, m := range wire {
		if m["role"] != "assistant" {
			continue
		}
		for _, b := range bedrockBlocks(m["content"]) {
			if b["type"] == "thinking" || b["type"] == "redacted_thinking" {
				out = append(out, b)
			}
		}
	}
	return out
}

// Content blocks of a wire message as plain maps, none for string content
func bedrockBlocks(v any) []map[string]any {
	var out []map[string]any
	_ = json.Unmarshal([]byte(bedrockJSON(v)), &out)
	return out
}

func bedrockJSON(v any) string {
	data, _ := json.Marshal(v)
	return string(data)
}

func bedrockContainsBlock(blocks []map[string]any, want map[string]any) bool {
	for _, b := range blocks {
		if reflect.DeepEqual(b, want) {
			return true
		}
	}
	return false
}

// Seeds the process inference profile cache for one ARN
func bedrockSeedProfile(t *testing.T, arn, model string) {
	bedrockProfileMu.Lock()
	bedrockProfileCache[arn] = model
	bedrockProfileMu.Unlock()
	t.Cleanup(func() {
		bedrockProfileMu.Lock()
		delete(bedrockProfileCache, arn)
		bedrockProfileMu.Unlock()
	})
}

func TestBedrockGPTStampedReasoningNeverReachesClaude(t *testing.T) {
	for _, base := range []string{bedrockFRURL, ""} {
		wire := bedrockFRConvert(t, []map[string]any{{"role": "user", "content": "q1"}, bedrockGPTTurn("a1", true, nil),
			{"role": "user", "content": "q2"}, bedrockClaudeTurn("a2", "claude-sig-2", ""), {"role": "user", "content": "q3"}}, base)
		js := bedrockJSON(wire)
		if strings.Contains(js, bedrockFRBlob) || strings.Contains(js, "gpt readable summary") {
			t.Fatalf("%q: gpt reasoning reached claude: %s", base, js)
		}
		if !bedrockContainsBlock(bedrockWireThinking(wire), map[string]any{"type": "thinking", "thinking": "claude chain", "signature": "claude-sig-2"}) {
			t.Fatalf("%q: claude lost its own thinking: %s", base, js)
		}
		if bedrockJSON(wire[1]["content"]) != `[{"text":"a1","type":"text"}]` {
			t.Fatalf("%q: gpt turn content %s", base, bedrockJSON(wire[1]["content"]))
		}
	}
}

func TestBedrockFallbackMidToolLoopDropsGPTReasoning(t *testing.T) {
	call := map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "terminal", "arguments": "{}"}}
	wire := bedrockFRConvert(t, []map[string]any{{"role": "user", "content": "run it"}, bedrockGPTTurn("", true, call),
		{"role": "tool", "tool_call_id": "call_1", "content": "ok"}}, bedrockFRURL)
	if strings.Contains(bedrockJSON(wire), bedrockFRBlob) || len(bedrockWireThinking(wire)) != 0 {
		t.Fatalf("in flight loop kept gpt reasoning: %s", bedrockJSON(wire))
	}
	blocks := bedrockBlocks(wire[1]["content"])
	if len(blocks) != 1 || blocks[0]["type"] != "tool_use" {
		t.Fatalf("assistant blocks %s", bedrockJSON(blocks))
	}
}

func TestBedrockClaudeStampedConverseTurnKeepsSignedThinking(t *testing.T) {
	turn := bedrockClaudeTurn("a1", "claude-sig-1", "us.anthropic.claude-sonnet-4-6")
	details := turn["reasoning_details"].([]any)
	turn["reasoning_details"] = []any{details[0], map[string]any{"type": "redacted_thinking", "data": "Q2xhdWRlLXJlZGFjdGVk"}, details[1]}
	got := bedrockWireThinking(bedrockFRConvert(t, []map[string]any{{"role": "user", "content": "q1"}, turn, {"role": "user", "content": "q2"}}, bedrockFRURL))
	want := []map[string]any{{"type": "thinking", "thinking": "claude chain", "signature": "claude-sig-1"}, {"type": "redacted_thinking", "data": "Q2xhdWRlLXJlZGFjdGVk"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("claude converse turn thinking %s", bedrockJSON(got))
	}
}

func TestBedrockUnstampedRedactedThinkingStillReplays(t *testing.T) {
	wire := bedrockFRConvert(t, []map[string]any{{"role": "user", "content": "q1"}, bedrockGPTTurn("a1", false, nil), {"role": "user", "content": "q2"}}, bedrockFRURL)
	if !bedrockContainsBlock(bedrockWireThinking(wire), map[string]any{"type": "redacted_thinking", "data": bedrockFRBlob}) {
		t.Fatalf("unstamped redacted thinking dropped: %s", bedrockJSON(wire))
	}
}

func TestBedrockUnresolvedProfileARNStampKeepsClaudesThinking(t *testing.T) {
	bedrockSeedProfile(t, bedrockFRProfile, bedrockFRProfile)
	wire := bedrockFRConvert(t, []map[string]any{{"role": "user", "content": "q1"}, bedrockClaudeTurn("a1", "claude-sig-1", bedrockFRProfile), {"role": "user", "content": "q2"}}, bedrockFRURL)
	if !bedrockContainsBlock(bedrockWireThinking(wire), map[string]any{"type": "thinking", "thinking": "claude chain", "signature": "claude-sig-1"}) {
		t.Fatalf("unresolved profile stamp dropped claude thinking: %s", bedrockJSON(wire))
	}
}

func TestBedrockProfileARNResolvingToGPTDropsItsReasoning(t *testing.T) {
	bedrockSeedProfile(t, bedrockFRProfile, "arn:aws:bedrock:us-east-1::foundation-model/openai.gpt-6.1-sol")
	turn := bedrockGPTTurn("a1", false, nil)
	turn["reasoning_details"] = append(turn["reasoning_details"].([]any), map[string]any{"type": BedrockConverseIssuerType, "model": bedrockFRProfile})
	wire := bedrockFRConvert(t, []map[string]any{{"role": "user", "content": "q1"}, turn, {"role": "user", "content": "q2"}}, bedrockFRURL)
	if strings.Contains(bedrockJSON(wire), bedrockFRBlob) {
		t.Fatalf("gpt reasoning behind a profile arn reached claude: %s", bedrockJSON(wire))
	}
}
