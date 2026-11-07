package provider

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
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
	kwargs                   map[string]any
	dispatch                 func(ctx context.Context, apiKwargs map[string]any) (any, error)
	sdkSeen                  []map[string]any
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
	if a.s().kwargs != nil {
		return maps.Clone(a.s().kwargs), nil
	}
	return map[string]any{"model": a.model, "messages": apiMessages, "tools": []any{}, "tool_choice": "auto", "parallel_tool_calls": true}, nil
}
func (a *fakeChatAgent) SanitizeOutboundKwargs(kwargs map[string]any) { a.s().outboundSanitized++ }
func (a *fakeChatAgent) InterruptibleAPICall(ctx context.Context, apiKwargs map[string]any) (any, error) {
	s := a.s()
	s.sent = append(s.sent, apiKwargs)
	if s.dispatch != nil {
		return s.dispatch(ctx, apiKwargs)
	}
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

func TestIterationLimitSummaryRetriesPastRouterTimeoutShim(t *testing.T) {
	completion := func(content string, completionTokens int) map[string]any {
		reply := chatReply(content)
		reply["usage"] = map[string]any{"completion_tokens": completionTokens}
		return reply
	}
	agent := summaryAgent(t, "chat_completions")
	agent.baseURL, agent.model = "https://openrouter.ai/api/v1", "test/model"
	agent.s().system = "You are helpful."
	agent.s().replies = []any{completion(routerTimeoutShim, 0), completion("Real summary.", 3)}
	history := []map[string]any{{"role": "user", "content": "do stuff"}}
	final, err := HandleMaxIterations(t.Context(), agent, &history)
	if err != nil || final != "Real summary." || len(agent.s().sent) != 2 {
		t.Fatalf("final %q err %v calls %d", final, err, len(agent.s().sent))
	}
}

func TestSummaryRequestSubstitutesSidecarBytes(t *testing.T) {
	agent := summaryAgent(t, "chat_completions")
	agent.baseURL = "https://openrouter.ai/api/v1"
	agent.s().replies = []any{chatReply("SUMMARY")}
	messages := []map[string]any{{"role": "user", "content": "q1", "api_content": "q1\n\nPLUGIN-CTX"}, {"role": "assistant", "content": "a1"}}
	out, err := HandleMaxIterations(t.Context(), agent, &messages)
	if err != nil || out != "SUMMARY" {
		t.Fatalf("summary %q err %v", out, err)
	}
	sent, _ := agent.s().sent[0]["messages"].([]map[string]any)
	var users []map[string]any
	for _, m := range sent {
		if _, has := m["api_content"]; has {
			t.Fatalf("sidecar key reached the wire: %v", m)
		}
		if m["role"] == "user" {
			users = append(users, m)
		}
	}
	if len(users) == 0 || users[0]["content"] != "q1\n\nPLUGIN-CTX" {
		t.Fatalf("summary request users %v", users)
	}
}

// MoA facade double recording the messages each prepared request was built from
type summaryMoaFacade struct{ prepared [][]map[string]any }

func (f *summaryMoaFacade) PrepareRequest(messages []map[string]any) (map[string]any, error) {
	f.prepared = append(f.prepared, messages)
	return map[string]any{"messages": messages}, nil
}

func TestMaxIterationsSummaryKeepsTheDataUseRequirementOnTheMoaPreparedRequest(t *testing.T) {
	agent := summaryAgent(t, "chat_completions")
	agent.provider, agent.baseURL, agent.model = "moa", "moa://local", "review"
	facade := &summaryMoaFacade{}
	agent.client = facade
	agent.s().replies = []any{chatReply("summary")}
	messages := []map[string]any{
		{"role": "user", "content": "read my mail"},
		{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "read_file", "arguments": "{}"}}}},
		{"role": "tool", "tool_call_id": "c1", "content": "inbox", "data_use": []any{"no_training"}},
	}
	out, err := HandleMaxIterations(t.Context(), agent, &messages)
	if err != nil || out != "summary" || len(facade.prepared) != 1 {
		t.Fatalf("summary %q err %v prepared %d", out, err, len(facade.prepared))
	}
	found := false
	for _, m := range facade.prepared[0] {
		if m["role"] == "tool" {
			found = reflect.DeepEqual(m["data_use"], []any{"no_training"})
		}
	}
	if !found || agent.s().sent[0]["_moa_prepared_request"] == nil {
		t.Fatalf("data_use lost on the MoA prepared request %v", facade.prepared[0])
	}
}

func TestIterationSummaryPathHandsTheSDKOnlyEmptyBulkSlots(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		raw, _ := json.Marshal(chatReply("ok"))
		_, _ = w.Write(raw)
	}))
	defer srv.Close()
	client := newAuxOpenAIClient("k", srv.URL+"/v1", auxOpenAIOptions{HTTPClient: srv.Client()})
	agent := summaryAgent(t, "chat_completions")
	agent.provider = "p"
	agent.s().transport = GetTransport("chat_completions")
	messages := []any{map[string]any{"role": "user", "content": strings.Repeat("x", 4096)}}
	tools := chatBypassWireBody()["tools"]
	agent.s().kwargs = map[string]any{"model": "m", "messages": messages, "tools": tools}
	agent.s().dispatch = func(ctx context.Context, request map[string]any) (any, error) {
		return DispatchNonstreamingAPIRequest(ctx, agent, request, func(reason, kind string) (any, error) { return client, nil })
	}
	attempt, err := chatSummaryAttempt(t.Context(), agent, []map[string]any{{"role": "user", "content": strings.Repeat("x", 4096)}}, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := attempt(t.Context(), 0); err != nil || out != "ok" {
		t.Fatalf("summary %q %v", out, err)
	}
	seen := agent.s().sdkSeen
	if len(seen) != 1 {
		t.Fatalf("sdk calls %d", len(seen))
	}
	if m, _ := seen[0]["messages"].([]any); m == nil || len(m) != 0 {
		t.Fatalf("sdk messages slot %v", seen[0]["messages"])
	}
	if tl, _ := seen[0]["tools"].([]any); tl == nil || len(tl) != 0 {
		t.Fatalf("sdk tools slot %v", seen[0]["tools"])
	}
	if extra, _ := seen[0]["extra_body"].(map[string]any); !reflect.DeepEqual(extra["messages"], messages) {
		t.Fatalf("extra body messages %v", seen[0]["extra_body"])
	}
}
