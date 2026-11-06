package provider

import (
	"context"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// Agent side summary state the fake records, the calls hermes assertions read
type summaryFake struct {
	maxIter                  int
	suppress, sanitize       bool
	system, ephemeral        string
	prefill                  []map[string]any
	printed                  []string
	canonicalized            [][]map[string]any
	built                    [][]map[string]any
	sent                     []map[string]any
	replies                  []any
	errs                     []error
	transport                Transport
	oauth                    bool
	maxTokens                *int
	outboundSanitized        int
	evicted, stripped, drops int
}

func (a *fakeChatAgent) s() *summaryFake {
	if a.sum == nil {
		a.sum = &summaryFake{maxIter: 90, transport: GetTransport("chat_completions")}
	}
	return a.sum
}
func (a *fakeChatAgent) MaxIterations() int         { return a.s().maxIter }
func (a *fakeChatAgent) SuppressStatusOutput() bool { return a.s().suppress }
func (a *fakeChatAgent) SafePrint(text string, diagnostic bool) {
	a.s().printed = append(a.s().printed, text)
}
func (a *fakeChatAgent) ShouldSanitizeToolCalls() bool { return a.s().sanitize }
func (a *fakeChatAgent) CopyReasoningContentForAPI(src, dst map[string]any) {
	if v, ok := src["reasoning"]; ok {
		dst["reasoning_content"] = v
	}
}
func (a *fakeChatAgent) SanitizeToolCallsForStrictAPI(msg map[string]any, model string) {}
func (a *fakeChatAgent) CachedSystemPrompt() string                                     { return a.s().system }
func (a *fakeChatAgent) EphemeralSystemPrompt() string                                  { return a.s().ephemeral }
func (a *fakeChatAgent) PrefillMessages() []map[string]any                              { return a.s().prefill }
func (a *fakeChatAgent) SanitizeAPIMessages(msgs []map[string]any) []map[string]any     { return msgs }
func (a *fakeChatAgent) EvictStaleOutboundToolImages(msgs []map[string]any) int {
	a.s().evicted++
	return 0
}
func (a *fakeChatAgent) StripImagesForRejectingModel(msgs []map[string]any) bool {
	a.s().stripped++
	return false
}
func (a *fakeChatAgent) DropThinkingOnlyAndMergeUsers(msgs []map[string]any) []map[string]any {
	a.s().drops++
	return msgs
}
func (a *fakeChatAgent) SubstituteAPIContent(msg map[string]any) {
	if v, ok := msg["api_content"]; ok {
		msg["content"] = v
		delete(msg, "api_content")
	}
}
func (a *fakeChatAgent) CanonicalizeAPIToolCalls(msgs []map[string]any) {
	snap := make([]map[string]any, len(msgs))
	for i, m := range msgs {
		snap[i] = maps.Clone(m)
	}
	a.s().canonicalized = append(a.s().canonicalized, snap)
}
func (a *fakeChatAgent) CloneMessageForSend(msg map[string]any) map[string]any {
	out := maps.Clone(msg)
	if parts, ok := msg["content"].([]any); ok {
		cp := make([]any, len(parts))
		for i, p := range parts {
			if m, ok := p.(map[string]any); ok {
				cp[i] = maps.Clone(m)
			} else {
				cp[i] = p
			}
		}
		out["content"] = cp
	}
	return out
}
func (a *fakeChatAgent) BuildAPIKwargs(apiMessages []map[string]any) (map[string]any, error) {
	a.s().built = append(a.s().built, apiMessages)
	return map[string]any{"model": a.model, "messages": apiMessages, "tools": []any{}, "tool_choice": "auto", "parallel_tool_calls": true}, nil
}
func (a *fakeChatAgent) SanitizeOutboundKwargs(kwargs map[string]any) { a.s().outboundSanitized++ }
func (a *fakeChatAgent) InterruptibleAPICall(ctx context.Context, apiKwargs map[string]any) (any, error) {
	s := a.s()
	s.sent = append(s.sent, apiKwargs)
	i := len(s.sent) - 1
	if i < len(s.errs) && s.errs[i] != nil {
		return nil, s.errs[i]
	}
	if i < len(s.replies) {
		return s.replies[i], nil
	}
	return chatReply(""), nil
}
func (a *fakeChatAgent) Transport() Transport        { return a.s().transport }
func (a *fakeChatAgent) MaxTokens() *int             { return a.s().maxTokens }
func (a *fakeChatAgent) IsAnthropicOAuth() bool      { return a.s().oauth }
func (a *fakeChatAgent) AnthropicPreserveDots() bool { return false }
func (a *fakeChatAgent) AnthropicBaseURL() string    { return a.baseURL }

// Chat completion object carrying one assistant text answer
func chatReply(text string) map[string]any {
	return map[string]any{"id": "c1", "object": "chat.completion", "model": "m", "choices": []any{map[string]any{"index": 0,
		"message": map[string]any{"role": "assistant", "content": text}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1}}
}

func summaryAgent(t *testing.T, apiMode string) *fakeChatAgent {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := &fakeChatAgent{provider: "custom", apiMode: apiMode, baseURL: "https://api.groq.com/openai/v1", model: "m", plat: "cli"}
	a.s().system = "SYS"
	return a
}

func summaryHistory() []map[string]any {
	return []map[string]any{
		{"role": "user", "content": "q"},
		{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "t1", "type": "function", "function": map[string]any{"name": "f", "arguments": "{}"}}},
			"reasoning_details": []any{map[string]any{"type": "thinking", "thinking": "x", "signature": "SIG"}}},
		{"role": "tool", "tool_call_id": "t1", "content": "r", "name": "f"},
	}
}

func TestAnthropicSummaryMessagesKeepReasoningDetails(t *testing.T) {
	agent := summaryAgent(t, "anthropic_messages")
	history := summaryHistory()
	out := iterationSummaryAPIMessages(agent, history)
	if out[0]["role"] != "system" || out[0]["content"] != "SYS" {
		t.Fatalf("system row %v", out[0])
	}
	i := slices.IndexFunc(out, func(m map[string]any) bool { return m["role"] == "assistant" })
	if !reflect.DeepEqual(out[i]["reasoning_details"], history[1]["reasoning_details"]) {
		t.Fatalf("assistant %v", out[i])
	}
	if _, has := out[3]["name"]; has || history[2]["name"] != "f" {
		t.Fatalf("tool row name %v history %v", out[3], history[2])
	}
}

func TestStrictChatRouteSummaryWireDropsReasoningDetails(t *testing.T) {
	agent := summaryAgent(t, "chat_completions")
	agent.s().replies = []any{chatReply("done")}
	history := summaryHistory()
	final, err := HandleMaxIterations(t.Context(), agent, &history)
	if err != nil || final != "done" {
		t.Fatalf("final %q err %v", final, err)
	}
	s := agent.s()
	if len(s.built) != 1 || len(s.sent) != 1 || s.outboundSanitized != 1 || !reflect.DeepEqual(s.sent[0]["messages"], s.built[0]) {
		t.Fatalf("built %v sent %v", s.built, s.sent)
	}
	if _, has := s.sent[0]["tools"]; !has {
		t.Fatal("chat summary dropped the cached tools")
	}
}

func TestCanonicalizesToolCallArgumentsAndStripsContent(t *testing.T) {
	agent := summaryAgent(t, "chat_completions")
	history := []map[string]any{
		{"role": "user", "content": "  q  ", "_thinking_prefill": true, "timestamp": 1.0},
		{"role": "assistant", "content": "  thinking out loud  ", "tool_calls": []any{map[string]any{"id": "t1", "type": "function", "function": map[string]any{"name": "f", "arguments": `{"b": 1, "a": 2}`}}}},
		{"role": "tool", "tool_call_id": "t1", "content": "  r  "},
	}
	snapshot := []map[string]any{maps.Clone(history[0]), maps.Clone(history[1]), maps.Clone(history[2])}
	out := iterationSummaryAPIMessages(agent, history)
	canon := agent.s().canonicalized
	if len(canon) != 1 || canon[0][1]["content"] != "q" || canon[0][3]["content"] != "r" {
		t.Fatalf("canonicalized %v", canon)
	}
	if out[1]["content"] != "q" || out[2]["content"] != "thinking out loud" || out[3]["content"] != "r" {
		t.Fatalf("out %v", out)
	}
	if _, has := out[1]["_thinking_prefill"]; has {
		t.Fatalf("underscore key survived %v", out[1])
	}
	if _, has := out[1]["timestamp"]; has {
		t.Fatalf("persistence key survived %v", out[1])
	}
	if !reflect.DeepEqual(history, snapshot) {
		t.Fatalf("history mutated %v", history)
	}
	if s := agent.s(); s.evicted != 1 || s.stripped != 1 || s.drops != 1 {
		t.Fatalf("send path passes %+v", s)
	}
}

func TestStripsLoneSurrogatesLikeTheSendPath(t *testing.T) {
	agent := summaryAgent(t, "chat_completions")
	history := []map[string]any{
		{"role": "user", "content": "clip \xed\xa0\x80 paste"},
		{"role": "assistant", "content": "ok \xed\xa0\xbd"},
		{"role": "user", "content": []any{map[string]any{"type": "text", "text": "part \xed\xa0\x80"}}},
	}
	out := iterationSummaryAPIMessages(agent, history)
	if out[1]["content"] != "clip � paste" || out[2]["content"] != "ok �" {
		t.Fatalf("out %q %q", out[1]["content"], out[2]["content"])
	}
	if parts := out[3]["content"].([]any); parts[0].(map[string]any)["text"] != "part �" {
		t.Fatalf("parts %v", parts)
	}
	if history[0]["content"] != "clip \xed\xa0\x80 paste" || history[2]["content"].([]any)[0].(map[string]any)["text"] != "part \xed\xa0\x80" {
		t.Fatalf("history mutated %v", history)
	}
}

func TestHandleMaxIterationsRetriesOnceStripsThinkAndFallsBackToSiteCopy(t *testing.T) {
	agent := summaryAgent(t, "codex_responses")
	agent.s().transport = GetTransport("chat_completions")
	agent.s().replies = []any{chatReply(""), chatReply("<think>plan</think>\n  Summary text")}
	history := summaryHistory()
	final, err := HandleMaxIterations(t.Context(), agent, &history)
	if err != nil || final != "Summary text" || history[len(history)-1]["content"] != "Summary text" || history[len(history)-2]["role"] != "user" {
		t.Fatalf("final %q err %v history %v", final, err, history)
	}
	if s := agent.s(); len(s.sent) != 2 || s.sent[0]["tools"] != nil || !strings.Contains(s.printed[0], "Reached maximum iterations (90)") {
		t.Fatalf("sent %v printed %v", s.sent, s.printed)
	}
	interrupted := summaryAgent(t, "chat_completions")
	interrupted.s().errs = []error{&InterruptedError{Message: "Agent interrupted during API call"}}
	history = summaryHistory()
	if _, err := HandleMaxIterations(t.Context(), interrupted, &history); err == nil || len(history) != 3 {
		t.Fatalf("interrupt err %v history %d", err, len(history))
	}
	failing := summaryAgent(t, "chat_completions")
	failing.s().errs = []error{context.DeadlineExceeded}
	history = summaryHistory()
	final, err = HandleMaxIterations(t.Context(), failing, &history)
	if err != nil || !strings.Contains(final, "90") || len(history) != 4 {
		t.Fatalf("failure final %q err %v history %d", final, err, len(history))
	}
}
