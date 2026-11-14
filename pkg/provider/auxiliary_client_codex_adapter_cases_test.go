package provider

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Responses client answering create with the scripted events or completed response and recording the kwargs
type auxScriptedResponsesClient struct {
	base    string
	kwargs  map[string]any
	calls   int
	answer  func() any
	closeMu chan struct{}
}

func (c *auxScriptedResponsesClient) BaseURL() string { return c.base }
func (c *auxScriptedResponsesClient) APIKey() string  { return "codex-key" }
func (c *auxScriptedResponsesClient) Close() error    { return nil }
func (c *auxScriptedResponsesClient) CreateChatCompletion(context.Context, map[string]any) (any, error) {
	return nil, errors.New("chat completions unused")
}

// Records the effective wire body with extra_body folded in, as the hermes fixtures read it
func (c *auxScriptedResponsesClient) CreateResponse(_ context.Context, kwargs map[string]any) (any, error) {
	c.calls++
	c.kwargs = map[string]any{}
	for k, v := range kwargs {
		c.kwargs[k] = v
	}
	if extra, ok := kwargs["extra_body"].(map[string]any); ok {
		for k, v := range extra {
			c.kwargs[k] = v
		}
	}
	return c.answer(), nil
}

// Event stream over fixed maps, every Next after the last returning EOF
func auxEvents(events ...map[string]any) func() any {
	return func() any { return &auxFakeEventStream{events: events} }
}

func auxMessageItemDone(text string) map[string]any {
	return map[string]any{"type": "response.output_item.done", "item": map[string]any{"type": "message",
		"content": []any{map[string]any{"type": "output_text", "text": text}}}}
}

func auxCompletedEvent(usage any) map[string]any {
	return map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "id": "r1", "usage": usage}}
}

// Stream emitting a keepalive frame every interval until closed or the context ends
type auxSlowAliveStream struct {
	interval time.Duration
	left     int
	done     chan struct{}
}

func (s *auxSlowAliveStream) Next(ctx context.Context) (map[string]any, error) {
	if s.left <= 0 {
		return nil, io.EOF
	}
	s.left--
	select {
	case <-time.After(s.interval):
		return map[string]any{"type": "response.in_progress"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.done:
		return nil, io.EOF
	}
}

func (s *auxSlowAliveStream) Close() error {
	select {
	case <-s.done:
	default:
		close(s.done)
	}
	return nil
}

func auxChoiceMessage(t *testing.T, out any) (map[string]any, map[string]any) {
	t.Helper()
	resp, _ := out.(map[string]any)
	choices, _ := resp["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("response %v", out)
	}
	choice := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	return choice, message
}

func TestCodexAuxiliaryAdapterTimeoutForwardsTimeoutToResponsesCreate(t *testing.T) {
	client := &auxScriptedResponsesClient{answer: auxEvents(auxMessageItemDone("summary"), auxCompletedEvent(nil))}
	adapter := &auxCodexCompletionsAdapter{client: client, model: "gpt-5.5"}
	out, err := adapter.create(context.Background(), map[string]any{"messages": auxUserMessages("summarize this"), "timeout": 12.5})
	if err != nil {
		t.Fatal(err)
	}
	if client.kwargs["timeout"] != 12.5 || client.kwargs["stream"] != true {
		t.Fatalf("kwargs %v", client.kwargs)
	}
	if _, message := auxChoiceMessage(t, out); message["content"] != "summary" {
		t.Fatalf("response %v", out)
	}
}

func TestCodexAuxiliaryAdapterTimeoutEnforcesTotalTimeoutWhileStreamKeepsEmittingEvents(t *testing.T) {
	client := &auxScriptedResponsesClient{answer: func() any {
		return &auxSlowAliveStream{interval: 30 * time.Millisecond, left: 50, done: make(chan struct{})}
	}}
	adapter := &auxCodexCompletionsAdapter{client: client, model: "gpt-5.5"}
	started := time.Now()
	_, err := adapter.create(context.Background(), map[string]any{"messages": auxUserMessages("summarize this"), "timeout": 0.05})
	var timeout *AuxTimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("err %v", err)
	}
	if time.Since(started) >= time.Second {
		t.Fatalf("total timeout took %v", time.Since(started))
	}
}

func TestCodexAuxiliaryAdapterTimeoutNoProgressTimeoutKwargOverridesDefaultWindow(t *testing.T) {
	client := &auxScriptedResponsesClient{answer: func() any {
		return &auxSlowAliveStream{interval: 20 * time.Millisecond, left: 50, done: make(chan struct{})}
	}}
	adapter := &auxCodexCompletionsAdapter{client: client, model: "gpt-5.5"}
	started := time.Now()
	_, err := adapter.create(context.Background(), map[string]any{"messages": auxUserMessages("summarize this"), "timeout": 5.0, "no_progress_timeout": 0.05})
	var timeout *AuxTimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("err %v", err)
	}
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("no_progress_timeout=0.05 override should cut the stall off in well under 1s, took %.2fs (falling back to the 5s overall timeout instead of honoring the override)", elapsed.Seconds())
	}
}

// Prompt cache key the adapter derives for a memory summarizer call under a session scoped runtime main
func auxCacheKeyForSession(t *testing.T, sessionID string) string {
	t.Helper()
	client := &auxScriptedResponsesClient{answer: auxEvents(auxMessageItemDone("ok"), auxCompletedEvent(nil))}
	adapter := &auxCodexCompletionsAdapter{client: client, model: "gpt-5.5"}
	ctx := SetRuntimeMain(context.Background(), "openai", "gpt-5.5", RuntimeMainOptions{SessionID: sessionID})
	if _, err := adapter.create(ctx, map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": "You are a memory summarizer."},
		map[string]any{"role": "user", "content": "Summarize the last turn."}}}); err != nil {
		t.Fatal(err)
	}
	key, _ := client.kwargs["prompt_cache_key"].(string)
	if key == "" {
		t.Fatalf("no prompt_cache_key in %v", client.kwargs)
	}
	return key
}

func TestCodexAuxiliaryAdapterCacheScopeDifferentSessionsGetDifferentCacheKeys(t *testing.T) {
	if auxCacheKeyForSession(t, "session-A") == auxCacheKeyForSession(t, "session-B") {
		t.Fatal("unrelated sessions share a prompt cache key")
	}
}

func TestCodexAuxiliaryAdapterCacheScopeCronRefiresOfTheSameJobShareACacheKey(t *testing.T) {
	first := auxCacheKeyForSession(t, "cron_job42_20260801_090000")
	second := auxCacheKeyForSession(t, "cron_job42_20260802_090000")
	other := auxCacheKeyForSession(t, "cron_job99_20260801_090000")
	if first != second || first == other {
		t.Fatalf("keys %q %q %q", first, second, other)
	}
}

// Effective Responses wire body the adapter sends for a chat history
func auxCaptureCodexInput(t *testing.T, messages []any) map[string]any {
	t.Helper()
	client := &auxScriptedResponsesClient{answer: auxEvents(map[string]any{"type": "response.created"}, auxMessageItemDone("ok"), auxCompletedEvent(nil))}
	adapter := &auxCodexCompletionsAdapter{client: client, model: "gpt-5.5"}
	if _, err := adapter.create(context.Background(), map[string]any{"messages": messages, "model": "gpt-5.5"}); err != nil {
		t.Fatal(err)
	}
	return client.kwargs
}

func auxInputItems(t *testing.T, kwargs map[string]any) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, raw := range auxKwargMaps(kwargs["input"]) {
		out = append(out, raw)
	}
	if out == nil {
		t.Fatalf("input %v", kwargs["input"])
	}
	return out
}

func TestCodexAuxiliaryToolMessageConversionToolHistoryNeverLeaksRoleTool(t *testing.T) {
	kwargs := auxCaptureCodexInput(t, []any{
		map[string]any{"role": "system", "content": "You are a memory summarizer."},
		map[string]any{"role": "user", "content": "What files did I touch?"},
		map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "call_abc123", "type": "function",
			"function": map[string]any{"name": "search_files", "arguments": `{"pattern":"foo"}`}}}},
		map[string]any{"role": "tool", "tool_call_id": "call_abc123", "content": "Found 3 matches"},
		map[string]any{"role": "assistant", "content": "You touched bar.py."},
	})
	var functionCalls, outputs []map[string]any
	for _, item := range auxInputItems(t, kwargs) {
		if item["role"] == "tool" || item["role"] == "system" {
			t.Fatalf("raw role %v reached the Responses API %v", item["role"], item)
		}
		switch item["type"] {
		case "function_call":
			functionCalls = append(functionCalls, item)
		case "function_call_output":
			outputs = append(outputs, item)
		}
	}
	if len(functionCalls) == 0 || functionCalls[0]["call_id"] != "call_abc123" || functionCalls[0]["name"] != "search_files" {
		t.Fatalf("assistant tool_call must become a function_call item, got %v", functionCalls)
	}
	if len(outputs) == 0 || outputs[0]["call_id"] != "call_abc123" {
		t.Fatalf("tool result must become a function_call_output item, got %v", outputs)
	}
	if kwargs["instructions"] != "You are a memory summarizer." {
		t.Fatalf("instructions %v", kwargs["instructions"])
	}
}

func TestCodexAuxiliaryToolMessageConversionPlainTextHistoryStillWorks(t *testing.T) {
	kwargs := auxCaptureCodexInput(t, []any{
		map[string]any{"role": "system", "content": "sys"},
		map[string]any{"role": "user", "content": "hello"},
		map[string]any{"role": "assistant", "content": "hi there"},
	})
	roles := map[any]bool{}
	for _, item := range auxInputItems(t, kwargs) {
		roles[item["role"]] = true
	}
	if !roles["user"] || !roles["assistant"] || roles["tool"] || kwargs["instructions"] != "sys" {
		t.Fatalf("roles %v instructions %v", roles, kwargs["instructions"])
	}
}

func TestCodexAuxiliaryToolMessageConversionVideoInputFailsBeforeResponsesRequest(t *testing.T) {
	client := &auxScriptedResponsesClient{answer: auxEvents(auxCompletedEvent(nil))}
	adapter := &auxCodexCompletionsAdapter{client: client, model: "gpt-5.5"}
	_, err := adapter.create(context.Background(), map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "video_url", "video_url": map[string]any{"url": "data:video/mp4;base64,AAAA"}},
		map[string]any{"type": "text", "text": "Describe the video"}}}}})
	if err == nil || !strings.Contains(err.Error(), "does not support video_url input") {
		t.Fatalf("err %v", err)
	}
	if client.calls != 0 {
		t.Fatal("responses.create was called")
	}
}

func TestCodexAuxiliaryAdapterNullOutputRecoveryRecoversOutputItemWhenTerminalEventHasNullOutput(t *testing.T) {
	client := &auxScriptedResponsesClient{answer: auxEvents(map[string]any{"type": "response.created"}, auxMessageItemDone("aux survived"),
		map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "id": "resp_null_output", "output": nil, "usage": nil}})}
	adapter := &auxCodexCompletionsAdapter{client: client, model: "gpt-5.5"}
	out, err := adapter.create(context.Background(), map[string]any{"messages": auxUserMessages("summarize")})
	if err != nil {
		t.Fatal(err)
	}
	if _, message := auxChoiceMessage(t, out); message["content"] != "aux survived" {
		t.Fatalf("response %v", out)
	}
}

func TestCodexAuxiliaryAdapterCompletedResponseAcceptsCompletedResponseWhenStreamWasRequested(t *testing.T) {
	var streamed any
	client := &auxScriptedResponsesClient{}
	client.answer = func() any {
		streamed = client.kwargs["stream"]
		return map[string]any{"status": "completed", "id": "resp_completed",
			"output": []any{map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": "completed response"}}}},
			"usage":  map[string]any{"input_tokens": 11, "output_tokens": 3, "total_tokens": 14}}
	}
	adapter := &auxCodexCompletionsAdapter{client: client, model: "gpt-5.6-terra"}
	out, err := adapter.create(context.Background(), map[string]any{"messages": auxUserMessages("review this")})
	if err != nil || streamed != true {
		t.Fatalf("err %v stream %v", err, streamed)
	}
	_, message := auxChoiceMessage(t, out)
	usage, _ := out.(map[string]any)["usage"].(map[string]any)
	if message["content"] != "completed response" || usage["prompt_tokens"] != 11 || usage["completion_tokens"] != 3 || usage["total_tokens"] != 14 {
		t.Fatalf("response %v", out)
	}
}

func TestCodexAuxiliaryAdapterCompletedResponseCompletedResponseWithNullOutputDoesNotCrash(t *testing.T) {
	client := &auxScriptedResponsesClient{answer: func() any {
		return map[string]any{"status": "completed", "id": "r", "output": nil, "output_text": "", "usage": nil, "incomplete_details": nil, "error": nil}
	}}
	adapter := &auxCodexCompletionsAdapter{client: client, model: "gpt-5.5"}
	out, err := adapter.create(context.Background(), map[string]any{"messages": auxUserMessages("x")})
	if err != nil {
		t.Fatal(err)
	}
	choice, message := auxChoiceMessage(t, out)
	if message["content"] != nil || choice["finish_reason"] != "stop" {
		t.Fatalf("response %v", out)
	}
}

var auxReservedAliasToolNames = []string{"web_search", "search_files", "people_search", "read_file", "tool_search"}

func auxReservedAliasTools() []any {
	var tools []any
	for _, name := range auxReservedAliasToolNames {
		tools = append(tools, map[string]any{"type": "function", "function": map[string]any{"name": name, "description": name,
			"parameters": map[string]any{"type": "object", "properties": map[string]any{}}}})
	}
	return tools
}

func auxReservedAliasHistory() []any {
	return []any{
		map[string]any{"role": "system", "content": "sys"},
		map[string]any{"role": "user", "content": "find it"},
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "c1", "type": "function",
			"function": map[string]any{"name": "search_files", "arguments": "{}"}}}},
		map[string]any{"role": "tool", "tool_call_id": "c1", "content": "ok"},
	}
}

func TestCodexAuxiliaryAdapterReservedToolAliasesWireToolsMatchMainTransportAliasesAndStrict(t *testing.T) {
	cases := []struct {
		baseURL string
		aliased []string
	}{
		{"https://api.perplexity.ai/v1", []string{"web_search", "search_files", "people_search"}},
		{"https://opencode.ai/zen/v1", []string{"web_search", "search_files"}},
		{"https://api.x.ai/v1", []string{"web_search", "tool_search"}},
		{"https://api.perplexity.ai.evil.com/v1", nil},
		{"https://example.com/v1", nil},
	}
	ctx := auxMainTestHome(t, "")
	swapWebSearchResolution(t, nil, "brave")
	for _, c := range cases {
		aliased := map[string]bool{}
		for _, name := range c.aliased {
			aliased[name] = true
		}
		adapter := &auxCodexCompletionsAdapter{client: &auxScriptedResponsesClient{base: c.baseURL}, model: "m"}
		respKwargs, _, _, err := adapter.buildResponsesKwargs(ctx, map[string]any{"model": "m", "messages": auxReservedAliasHistory(), "tools": auxReservedAliasTools()})
		if err != nil {
			t.Fatal(err)
		}
		flags := classifyResponsesRoute("custom", c.baseURL)
		var mainTools []map[string]any
		for _, tool := range auxReservedAliasTools() {
			mainTools = append(mainTools, tool.(map[string]any))
		}
		mainKwargs := build(t, newCodexTransport(), "m", userMsgs("find it"), mainTools,
			codexParams{provider: "custom", baseURL: c.baseURL, isCodexBackend: flags.isCodexBackend, isXAI: flags.isXAI,
				webSearchBackend: func() (string, error) { return "brave", nil }})
		if !reflect.DeepEqual(respKwargs["tools"], mainKwargs["tools"]) {
			t.Fatalf("%s aux tools %v main tools %v", c.baseURL, respKwargs["tools"], mainKwargs["tools"])
		}
		wantNames := map[string]bool{}
		for _, name := range auxReservedAliasToolNames {
			if aliased[name] {
				wantNames[reservedToolAliasPrefix+name] = true
			} else {
				wantNames[name] = true
			}
		}
		gotNames := map[string]bool{}
		for _, tool := range auxKwargMaps(respKwargs["tools"]) {
			if tool["strict"] != false {
				t.Fatalf("%s strict %v", c.baseURL, tool["strict"])
			}
			gotNames[pyStr(tool["name"])] = true
		}
		if !reflect.DeepEqual(gotNames, wantNames) {
			t.Fatalf("%s names %v want %v", c.baseURL, gotNames, wantNames)
		}
		var historyNames []string
		for _, item := range auxKwargMaps(respKwargs["input"]) {
			if item["type"] == "function_call" {
				historyNames = append(historyNames, pyStr(item["name"]))
			}
		}
		wantHistory := "search_files"
		if aliased["search_files"] {
			wantHistory = reservedToolAliasPrefix + "search_files"
		}
		if !reflect.DeepEqual(historyNames, []string{wantHistory}) {
			t.Fatalf("%s history names %v", c.baseURL, historyNames)
		}
		wantAliases := map[string]string{}
		for name := range aliased {
			wantAliases[reservedToolAliasPrefix+name] = name
		}
		gotAliases, _ := respKwargs["_wire_aliases"].(map[string]string)
		if gotAliases == nil {
			gotAliases = map[string]string{}
		}
		if !reflect.DeepEqual(gotAliases, wantAliases) {
			t.Fatalf("%s aliases %v want %v", c.baseURL, gotAliases, wantAliases)
		}
	}
}

func TestCodexAuxiliaryAdapterReservedToolAliasesCreateMapsAliasesBackAndNeverSendsAliasMap(t *testing.T) {
	client := &auxScriptedResponsesClient{base: "https://api.perplexity.ai/v1", answer: func() any {
		return map[string]any{"status": "completed", "id": "resp_1", "usage": nil, "output": []any{map[string]any{
			"type": "function_call", "call_id": "c9", "id": "fc_9", "name": reservedToolAliasPrefix + "search_files", "arguments": `{"pattern": "x"}`}}}
	}}
	adapter := &auxCodexCompletionsAdapter{client: client, model: "m"}
	out, err := adapter.create(context.Background(), map[string]any{"messages": auxUserMessages("find it"), "tools": auxReservedAliasTools()})
	if err != nil {
		t.Fatal(err)
	}
	if _, leaked := client.kwargs["_wire_aliases"]; leaked {
		t.Fatal("alias map reached the wire")
	}
	names := map[string]bool{}
	for _, tool := range auxKwargMaps(client.kwargs["tools"]) {
		names[pyStr(tool["name"])] = true
	}
	if !names[reservedToolAliasPrefix+"search_files"] {
		t.Fatalf("wire tools %v", client.kwargs["tools"])
	}
	_, message := auxChoiceMessage(t, out)
	var called []string
	for _, tc := range auxKwargMaps(message["tool_calls"]) {
		fn, _ := tc["function"].(map[string]any)
		called = append(called, pyStr(fn["name"]))
	}
	if !reflect.DeepEqual(called, []string{"search_files"}) {
		t.Fatalf("tool calls %v", message["tool_calls"])
	}
}
