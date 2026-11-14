package provider

import (
	"context"
	"errors"
	"io"
	"reflect"
	"testing"
)

func auxCodexStatusMessage(text, phase, status string) map[string]any {
	item := map[string]any{"type": "message", "role": "assistant", "status": status,
		"content": []any{map[string]any{"type": "output_text", "text": text}}}
	if phase != "" {
		item["phase"] = phase
	} else {
		item["phase"] = nil
	}
	return item
}

// Stream replaying the final response as item done, text delta and terminal status events
type auxCodexStatusStream struct {
	events []map[string]any
}

func (s *auxCodexStatusStream) Next(context.Context) (map[string]any, error) {
	if len(s.events) == 0 {
		return nil, io.EOF
	}
	ev := s.events[0]
	s.events = s.events[1:]
	return ev, nil
}

func (s *auxCodexStatusStream) Close() error { return nil }

// Responses client answering with the final response object or its streamed replay
type auxCodexStatusClient struct {
	base     string
	final    map[string]any
	streamed bool
}

func (c *auxCodexStatusClient) BaseURL() string { return c.base }
func (c *auxCodexStatusClient) APIKey() string  { return "k" }
func (c *auxCodexStatusClient) Close() error    { return nil }
func (c *auxCodexStatusClient) CreateChatCompletion(context.Context, map[string]any) (any, error) {
	return nil, errors.New("chat completions unused")
}
func (c *auxCodexStatusClient) CreateResponse(_ context.Context, kwargs map[string]any) (any, error) {
	if kwargs["stream"] != true {
		return nil, errors.New("stream must be requested")
	}
	if !c.streamed {
		return c.final, nil
	}
	var events []map[string]any
	for _, item := range auxKwargMaps(c.final["output"]) {
		events = append(events, map[string]any{"type": "response.output_item.done", "item": item})
	}
	if text, _ := c.final["output_text"].(string); text != "" {
		events = append(events, map[string]any{"type": "response.output_text.delta", "delta": text})
	}
	events = append(events, map[string]any{"type": "response." + pyStr(c.final["status"]), "response": c.final})
	return &auxCodexStatusStream{events: events}, nil
}

// Chat completion the adapter normalizes from a final Responses object delivered whole or streamed
func auxCodexAdapterResponse(t *testing.T, final map[string]any, streamed bool, baseURL string) map[string]any {
	t.Helper()
	adapter := &auxCodexCompletionsAdapter{client: &auxCodexStatusClient{base: baseURL, final: final, streamed: streamed}, model: "aux-model"}
	out, err := adapter.create(auxMainTestHome(t, ""), map[string]any{"messages": []any{map[string]any{"role": "user", "content": "Summarize the task."}}})
	if err != nil {
		t.Fatalf("streamed=%v: %v", streamed, err)
	}
	resp, _ := out.(map[string]any)
	return resp
}

func auxCodexFirstChoice(t *testing.T, resp map[string]any) (map[string]any, map[string]any) {
	t.Helper()
	choices, _ := resp["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("response %v", resp)
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	return choice, message
}

func TestActualAdapterPreservesCompletionContract(t *testing.T) {
	toolCall := map[string]any{"type": "function_call", "status": "completed", "call_id": "call_1", "name": "inspect", "arguments": `{"path":"x"}`}
	leakLikeAnswer := "Summary of work.\nRunning the tests next.\n{\"cmd\": \"pytest -q\"}"
	partialToolCall := map[string]any{"type": "function_call", "status": "in_progress", "call_id": "call_2", "name": "inspect", "arguments": `{"pa`}
	codexReasoningOnly := map[string]any{"type": "reasoning", "id": "rs_test", "encrypted_content": nil, "summary": []any{map[string]any{"text": "still thinking"}}}
	cases := []struct {
		id              string
		baseURL, status string
		items           []map[string]any
		outputText      string
		reason          string
		expectContent   any
		expectFinish    string
	}{
		{"final-answer", "", "completed", []map[string]any{auxCodexStatusMessage("FINAL", "final_answer", "completed")}, "", "", "FINAL", "stop"},
		{"commentary-only", "", "completed", []map[string]any{auxCodexStatusMessage("COMMENTARY", "commentary", "completed")}, "", "", nil, "length"},
		{"tool-call", "", "completed", []map[string]any{toolCall}, "", "", nil, "tool_calls"},
		{"dict-items", "", "completed", []map[string]any{{"type": "message", "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": "DICT"}}}}, "", "", "DICT", "stop"},
		{"tool-call-like-answer", "", "completed", []map[string]any{auxCodexStatusMessage(leakLikeAnswer, "", "completed")}, "", "", leakLikeAnswer, "stop"},
		{"tool-call-like-partial-item", "", "completed", []map[string]any{auxCodexStatusMessage(leakLikeAnswer, "", "completed"), partialToolCall}, "", "", leakLikeAnswer, "length"},
		{"tool-call-like-output-text", "", "completed", nil, leakLikeAnswer, "", leakLikeAnswer, "stop"},
		{"tool-call-like-output-text-beside-commentary", "", "completed", []map[string]any{auxCodexStatusMessage("", "commentary", "completed")}, leakLikeAnswer, "", nil, "length"},
		{"token-cap", "", "incomplete", []map[string]any{auxCodexStatusMessage("PARTIAL", "final_answer", "completed")}, "", "max_output_tokens", "PARTIAL", "length"},
		{"token-cap-after-tool-call", "", "incomplete", []map[string]any{toolCall}, "", "max_output_tokens", nil, "tool_calls"},
		{"content-filter", "", "incomplete", []map[string]any{auxCodexStatusMessage("CF", "final_answer", "completed")}, "", "content_filter", "CF", "length"},
		{"tool-call-like-beside-commentary", "", "completed", []map[string]any{auxCodexStatusMessage("NOTE", "commentary", "completed"), auxCodexStatusMessage(leakLikeAnswer, "", "completed")}, "", "", leakLikeAnswer, "length"},
		{"codex-reasoning-only", "https://chatgpt.com/backend-api/codex", "completed", []map[string]any{codexReasoningOnly}, "", "", nil, "length"},
	}
	for _, streamed := range []bool{false, true} {
		for _, tc := range cases {
			items := []any{}
			for _, item := range tc.items {
				items = append(items, item)
			}
			final := map[string]any{"status": tc.status, "output": items, "output_text": tc.outputText, "error": nil,
				"usage": map[string]any{"input_tokens": 11, "output_tokens": 3, "total_tokens": 14}}
			if tc.reason != "" {
				final["incomplete_details"] = map[string]any{"reason": tc.reason}
			} else {
				final["incomplete_details"] = nil
			}
			choice, message := auxCodexFirstChoice(t, auxCodexAdapterResponse(t, final, streamed, tc.baseURL))
			if !reflect.DeepEqual(message["content"], tc.expectContent) || choice["finish_reason"] != tc.expectFinish {
				t.Fatalf("%s streamed=%v: content %v finish %v, want %v %v", tc.id, streamed, message["content"], choice["finish_reason"], tc.expectContent, tc.expectFinish)
			}
			if tc.expectFinish == "tool_calls" {
				calls := auxKwargMaps(message["tool_calls"])
				if len(calls) != 1 {
					t.Fatalf("%s streamed=%v: tool calls %v", tc.id, streamed, message["tool_calls"])
				}
				fn, _ := calls[0]["function"].(map[string]any)
				if calls[0]["id"] != "call_1" || fn["name"] != "inspect" || fn["arguments"] != `{"path":"x"}` {
					t.Fatalf("%s streamed=%v: tool call %v", tc.id, streamed, calls[0])
				}
			}
		}
	}
}
