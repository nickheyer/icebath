package provider

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func anthSignedTurn(question, answer, sig, thinking string) []map[string]any {
	if thinking == "" {
		thinking = "thought-" + sig
	}
	return []map[string]any{{"role": "user", "content": question}, {"role": "assistant", "content": answer, "reasoning": thinking,
		"reasoning_details": []any{map[string]any{"type": "thinking", "thinking": thinking, "signature": sig}}}}
}

func anthCarrier(t *testing.T) []map[string]any {
	return anthMsgs(t, `[{"role":"user","content":"Q"},{"role":"assistant","content":"answer","reasoning":"secret chain","reasoning_content":"secret chain",
		"timestamp":1234567890.0,"finish_reason":"tool_calls",
		"reasoning_details":[{"type":"thinking","thinking":"secret chain","signature":"sig_bad"},{"type":"redacted_thinking","data":"red_bad"}],
		"anthropic_content_blocks":[{"type":"thinking","thinking":"secret chain","signature":"sig_bad"},{"type":"text","text":"answer"},
		{"type":"tool_use","id":"tool_1","name":"search","input":{"q":"x"}},{"type":"redacted_thinking","data":"red_bad"}],
		"tool_calls":[{"id":"tool_1","type":"function","function":{"name":"search","arguments":"{\"q\":\"x\"}"}}]},
		{"role":"tool","tool_call_id":"tool_1","content":"result"}]`)
}

func anthWireThinking(wire []map[string]any) []map[string]any {
	var out []map[string]any
	for _, m := range wire {
		if m["role"] != "assistant" {
			continue
		}
		for _, b := range anthBlocks(m["content"]) {
			if b["type"] == "thinking" || b["type"] == "redacted_thinking" {
				out = append(out, b)
			}
		}
	}
	return out
}

func anthCloneMsgs(msgs []map[string]any) []map[string]any {
	data, _ := json.Marshal(msgs)
	var out []map[string]any
	_ = json.Unmarshal(data, &out)
	return out
}

func anthConcat(parts ...[]map[string]any) []map[string]any {
	var out []map[string]any
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

var anthPreservedThinking = []struct {
	model     string
	preserved bool
}{
	{"claude-opus-4-4", false}, {"claude-opus-4-5", true}, {"claude-opus-4-6", true}, {"claude-sonnet-4-5", false}, {"claude-sonnet-4-6", true},
	{"claude-haiku-4-5", false}, {"claude-opus-4-20250514", false}, {"claude-sonnet-4-20250514", false}, {"claude-opus-4-5-20251101", true},
	{"claude-opus-4-6-20250414", true}, {"claude-sonnet-4-5-20250929", false}, {"claude-opus-5", true}, {"claude-sonnet-5-5", true},
	{"claude-fable-5-1", true}, {"claude-mythos-5", true}, {"claude-mythos-preview", true}, {"claude-fable-5-2", true}, {"claude-mythos-6", true},
	{"claude-newfamily-7", true}, {"claude-haiku-5", false}, {"claude-3-7-sonnet-20250219", false}, {"claude-sonnet-4", false}, {"hermes-4-405b", false},
}

// A formerly latest turn stays byte stable where history keeps thinking and loses it elsewhere
func TestAnthropicHistoricalThinkingReplayByRoute(t *testing.T) {
	type routeCase struct {
		model, base string
		replays     bool
	}
	cases := []routeCase{{"claude-opus-4-6", "https://inference-api.nousresearch.com/v1/messages", true}, {"claude-opus-4-6", "https://openrouter.ai/api/v1", false}}
	for _, p := range anthPreservedThinking {
		cases = append(cases, routeCase{p.model, "https://api.anthropic.com", p.preserved})
	}
	for _, c := range cases {
		prefix := anthConcat(anthSignedTurn("Q1", "A1", "sig_1", "x"), anthSignedTurn("Q2", "A2", "sig_2", ""))
		_, short := convertMessagesToAnthropic(anthCloneMsgs(prefix), convertOptions{baseURL: c.base, model: c.model})
		_, longer := convertMessagesToAnthropic(anthConcat(anthCloneMsgs(prefix), anthSignedTurn("Q3", "A3", "sig_3", "")), convertOptions{baseURL: c.base, model: c.model})
		if c.replays {
			if !anthJSONEqual(longer[3], short[3]) {
				t.Fatalf("%s %s formerly latest turn changed: %v vs %v", c.model, c.base, longer[3], short[3])
			}
		} else if len(anthWireThinking([]map[string]any{longer[3]})) != 0 {
			t.Fatalf("%s %s historical thinking must drop: %v", c.model, c.base, longer[3])
		}
	}
}

type anthMemStore struct {
	mu   sync.Mutex
	data map[string]map[string]any
}

func (s *anthMemStore) ModelConfigValue(sessionID, key string) (any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[sessionID][key]
	return v, ok
}

func (s *anthMemStore) PatchModelConfig(sessionID string, patch map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data == nil {
		s.data = map[string]map[string]any{}
	}
	if s.data[sessionID] == nil {
		s.data[sessionID] = map[string]any{}
	}
	data, _ := json.Marshal(patch)
	var clean map[string]any
	_ = json.Unmarshal(data, &clean)
	for k, v := range clean {
		s.data[sessionID][k] = v
	}
	return nil
}

func anthTracker(store *anthMemStore, session, model, base string) (*RejectedThinkingTracker, *bool) {
	reset := false
	return &RejectedThinkingTracker{Mode: ModeAnthropicMessages, BaseURL: base, Model: model, SessionID: session, Store: store,
		ResetUsageAnchor: func() { reset = true }}, &reset
}

// Signature rejection recovery repairs the retry copy, leaves history untouched and strips every thinking block
func anthRejectSignatures(t *testing.T, tr *RejectedThinkingTracker, history []map[string]any) {
	t.Helper()
	before := anthCloneMsgs(history)
	request := anthCloneMsgs(history)
	tr.RememberRejectedThinking(request)
	if !anthJSONEqual(history, before) {
		t.Fatal("rejection recovery must not mutate history")
	}
	_, wire := convertMessagesToAnthropic(request, convertOptions{model: tr.Model})
	if blocks := anthWireThinking(wire); len(blocks) != 0 {
		t.Fatalf("retry copy still carries thinking: %v", blocks)
	}
}

func TestAnthropicRejectedThinkingNeverReturns(t *testing.T) {
	const anth = "https://api.anthropic.com"
	for _, boundary := range []string{"same_agent", "rebuild", "session_switch", "compression_child", "context_selection", "carrier", "unfingerprintable", "kimi_route", "future_family"} {
		t.Run(boundary, func(t *testing.T) {
			store := &anthMemStore{}
			model, base := "claude-opus-4-6", anth
			switch boundary {
			case "kimi_route":
				model, base = "kimi-k2.5", "https://api.kimi.com/coding"
			case "future_family":
				model = "claude-fable-5-2"
			}
			agent, reset := anthTracker(store, "s1", model, base)
			rejected := anthConcat(anthSignedTurn("Q1", "A1", "sig_rejected_1", ""), anthSignedTurn("Q2", "A2", "sig_rejected_2", ""))
			switch boundary {
			case "unfingerprintable":
				rejected = []map[string]any{{"role": "user", "content": "Q1"}, {"role": "assistant", "content": "A1"}}
			case "carrier":
				rejected = anthCarrier(t)
			}
			anthRejectSignatures(t, agent, rejected)
			if boundary != "kimi_route" && !*reset {
				t.Fatal("rejection must drop the stale usage anchor")
			}
			reader := agent
			switch boundary {
			case "rebuild", "context_selection", "carrier":
				reader, _ = anthTracker(store, "s1", "claude-opus-4-6", anth)
			case "session_switch":
				rejected = anthSignedTurn("Q1", "A1", "sig_rejected_s2", "")
				other, _ := anthTracker(store, "s2", "claude-opus-4-6", anth)
				anthRejectSignatures(t, other, rejected)
				agent.SessionID = "s2"
			}
			later := anthConcat(anthCloneMsgs(rejected), anthSignedTurn("Q3", "A3", "sig_kept", ""), anthSignedTurn("Q4", "A4", "sig_new", ""),
				[]map[string]any{{"role": "user", "content": "continue"}})
			if boundary == "compression_child" {
				agent.SessionID = "s1-child"
				agent.CarryRejectedThinkingToSession("s1")
				reader, _ = anthTracker(store, "s1-child", "claude-opus-4-6", anth)
				later = later[2:]
			}
			request := anthCloneMsgs(later)
			reader.ApplyRejectedThinkingSuppression(request)
			_, wire := convertMessagesToAnthropic(request, convertOptions{baseURL: reader.BaseURL, model: reader.Model})
			want := []string{"sig_kept", "sig_new"}
			if boundary == "kimi_route" {
				want = append([]string{"sig_rejected_1", "sig_rejected_2"}, want...)
			}
			var got []string
			for _, b := range anthWireThinking(wire) {
				got = append(got, asStr(b["signature"]))
			}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("replayed signatures %v, want %v", got, want)
			}
			text, _ := json.Marshal(wire)
			if strings.Contains(string(text), "tool_use") != (boundary == "carrier") || strings.Contains(string(text), "secret chain") {
				t.Fatalf("carrier replay wrong: %s", text)
			}
		})
	}
}

func anthSigned(sig string) map[string]any {
	return map[string]any{"type": "thinking", "thinking": "thought " + sig, "signature": sig}
}

func anthLoopStep(n int) []map[string]any {
	id := fmt.Sprintf("t%d", n)
	sig := fmt.Sprintf("sig_loop%d", n)
	return []map[string]any{{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": id, "type": "function", "function": map[string]any{"name": "read_file", "arguments": "{}"}}},
		"reasoning_details": []any{anthSigned(sig)}, "anthropic_content_blocks": []any{anthSigned(sig), map[string]any{"type": "tool_use", "id": id, "name": "read_file", "input": map[string]any{}}}},
		{"role": "tool", "tool_call_id": id, "content": "ok"}}
}

func anthPlatformHistory() []map[string]any {
	return anthConcat([]map[string]any{{"role": "user", "content": "Q1"}, {"role": "assistant", "content": "A1", "reasoning_details": []any{anthSigned("sig_prior")}},
		{"role": "user", "content": "Q2"}}, anthLoopStep(1), anthLoopStep(2))
}

func anthWireSignatures(wire []map[string]any) []string {
	sigs := []string{}
	for _, b := range anthWireThinking(wire) {
		if b["type"] == "thinking" {
			sigs = append(sigs, asStr(b["signature"]))
		}
	}
	return sigs
}

func TestAnthropicSignedThinkingReplaysWhereTheRouteCanVerifyIt(t *testing.T) {
	const bedrock = "https://bedrock-runtime.us-east-1.amazonaws.com"
	all, loop, none := []string{"sig_prior", "sig_loop1", "sig_loop2"}, []string{"sig_loop1", "sig_loop2"}, []string{}
	cases := []struct {
		name, base, model string
		expected          []string
		signed            bool
	}{
		{"bedrock_opus", bedrock, "us.anthropic.claude-opus-4-8-v1:0", all, true},
		{"bedrock_sonnet45", bedrock, "us.anthropic.claude-sonnet-4-5-20250929-v1:0", loop, true},
		{"bedrock_fips", "https://bedrock-runtime-fips.us-gov-west-1.amazonaws.com", "anthropic.claude-opus-4-8", all, true},
		{"vertex", "https://us-east5-aiplatform.googleapis.com/v1/projects/p/locations/us-east5/publishers/anthropic/models", "claude-opus-4-8", all, true},
		{"foundry", "https://my-res.services.ai.azure.com/anthropic", "claude-opus-4-8", all, true},
		{"minimax", "https://api.minimax.io/anthropic", "MiniMax-M2.7", all, true},
		{"minimax_cn", "https://api.minimaxi.com/anthropic", "MiniMax-M3", all, true},
		{"native_haiku55", "", "claude-haiku-5-5", all, true},
		{"native_haiku45", "", "claude-haiku-4-5", loop, true},
		{"native_sonnet45", "", "claude-sonnet-4-5", loop, true},
		{"native_opus48", "", "claude-opus-4-8", all, true},
		{"unknown_relay", "https://relay.example.com/anthropic", "claude-opus-4-8", none, false},
		{"bedrock_lookalike_host", "https://bedrock-runtime.us-east-1.amazonaws.com.evil.test", "claude-opus-4-8", none, false},
		{"bedrock_lookalike_path", "https://evil.test/bedrock-runtime.us-east-1.amazonaws.com", "claude-opus-4-8", none, false},
		{"deepseek", "https://api.deepseek.com/anthropic", "deepseek-v4-pro", none, false},
		{"deepseek_on_bedrock_host", bedrock, "deepseek-v4-pro", none, false},
		{"kimi", "https://api.kimi.com/coding", "kimi-k2.5", all, false},
	}
	for _, c := range cases {
		history := anthPlatformHistory()
		before := anthCloneMsgs(history)
		_, wire := convertMessagesToAnthropic(history, convertOptions{baseURL: c.base, model: c.model})
		if got := anthWireSignatures(wire); fmt.Sprint(got) != fmt.Sprint(c.expected) {
			t.Fatalf("%s replayed %v, want %v", c.name, got, c.expected)
		}
		for step := 1; step <= 2; step++ {
			sig, id := fmt.Sprintf("sig_loop%d", step), fmt.Sprintf("t%d", step)
			var types []string
			for _, m := range wire {
				if m["role"] != "assistant" {
					continue
				}
				for _, b := range anthBlocks(m["content"]) {
					if b["signature"] == sig || b["id"] == id {
						types = append(types, asStr(b["type"]))
					}
				}
			}
			want := "[tool_use]"
			if strings.Contains(fmt.Sprint(c.expected), sig) {
				want = "[thinking tool_use]"
			}
			if fmt.Sprint(types) != want {
				t.Fatalf("%s step %d order %v, want %s", c.name, step, types, want)
			}
		}
		if tracksRejectedThinking(ModeAnthropicMessages, c.base, c.model) != c.signed {
			t.Fatalf("%s tracks rejected thinking must be %v", c.name, c.signed)
		}
		prior := strings.Contains(fmt.Sprint(c.expected), "sig_prior")
		if nativeAnthropicPreservesPriorThinking(c.base, c.model) != (c.signed && prior) {
			t.Fatalf("%s preserves prior thinking must be %v", c.name, c.signed && prior)
		}
		if !anthJSONEqual(history, before) {
			t.Fatalf("%s mutated stored history", c.name)
		}
	}
}

func TestAnthropicSteerMergedIntoToolResultKeepsLoopThinking(t *testing.T) {
	history := append(anthPlatformHistory(), map[string]any{"role": "user", "content": "steer: also check b"})
	_, wire := convertMessagesToAnthropic(history, convertOptions{model: "claude-sonnet-4-5"})
	if got := anthWireSignatures(wire); fmt.Sprint(got) != "[sig_loop1 sig_loop2]" {
		t.Fatalf("steer lost loop thinking: %v", got)
	}
}

func anthKimiStored(t *testing.T, blocks ...map[string]any) map[string]any {
	t.Helper()
	c := &anthropicClient{route: Route{Provider: "kimi-coding", Model: "k3"}, transport: newAnthDirectTransport(Route{})}
	stop := "end_turn"
	for _, b := range blocks {
		if b["type"] == "tool_use" {
			stop = "tool_use"
		}
	}
	msg := anthMessage(stop, blocks...)
	msg["usage"] = nil
	resp, err := c.normalizeResponse(msg, 200, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	return messageToDict(resp.Message)
}

func anthKimiReplayThinking(t *testing.T, base, model string) []map[string]any {
	t.Helper()
	stored := anthKimiStored(t, map[string]any{"type": "thinking", "thinking": "five: 5 11 27 63 88", "signature": "sig-k3"}, anthText("5 27 88"))
	_, out := convertMessagesToAnthropic([]map[string]any{{"role": "user", "content": "q1"}, stored, {"role": "user", "content": "q2"}}, convertOptions{baseURL: base, model: model})
	var thinking []map[string]any
	for _, b := range anthBlocks(anthFirstRole(out, "assistant")["content"]) {
		if b["type"] == "thinking" {
			thinking = append(thinking, b)
		}
	}
	return thinking
}

func TestAnthropicKimiSignedThinkingReplay(t *testing.T) {
	if th := anthKimiReplayThinking(t, "https://api.moonshot.cn/anthropic", "k3"); len(th) == 0 || th[0]["signature"] != "sig-k3" {
		t.Fatalf("moonshot must keep signed thinking: %v", th)
	}
	for _, m := range []string{"kimi-k2.5", "k3"} {
		if len(anthKimiReplayThinking(t, "https://api.deepseek.com/anthropic", m)) == 0 {
			t.Fatalf("kimi model %s on a foreign gateway must keep thinking", m)
		}
	}
	stored := anthKimiStored(t, map[string]any{"type": "thinking", "thinking": "plan both reads", "signature": "sig-k3"},
		anthToolUse("toolu_1", "read_file", map[string]any{"path": "a.py"}), anthToolUse("toolu_2", "read_file", map[string]any{"path": "b.py"}))
	_, out := convertMessagesToAnthropic([]map[string]any{{"role": "user", "content": "inspect both"}, stored,
		{"role": "tool", "tool_call_id": "toolu_1", "content": "a.py: ok"}, {"role": "user", "content": "continue"}}, convertOptions{baseURL: "https://api.kimi.com/coding", model: "k3"})
	assistant := anthFirstRole(out, "assistant")
	if _, leaked := assistant["_thinking_signature_invalidated"]; leaked {
		t.Fatalf("internal marker leaked: %v", assistant)
	}
	if anthCountType(anthBlocks(assistant["content"]), "thinking") == 0 {
		t.Fatalf("kimi replays orphan invalidated thinking as is: %v", assistant)
	}
}
