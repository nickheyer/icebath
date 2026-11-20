package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"testing"
)

// Chat completions stream replaying scripted chunks, then failing with cause or ending cleanly without one
type scriptedChatStream struct {
	chunks []map[string]any
	cause  error
	pos    int
}

func (s *scriptedChatStream) Next(context.Context) (map[string]any, error) {
	if s.pos < len(s.chunks) {
		s.pos++
		return s.chunks[s.pos-1], nil
	}
	if s.cause != nil {
		return nil, s.cause
	}
	return nil, io.EOF
}

func (s *scriptedChatStream) Close() error { return nil }

func chatContentChunk(text string) map[string]any {
	return map[string]any{"model": "test/model", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": text}, "finish_reason": nil}}}
}

func chatToolStartChunk(index int, name string) map[string]any {
	call := map[string]any{"index": index, "id": "c_" + name, "type": "function", "function": map[string]any{"name": name, "arguments": ""}}
	return map[string]any{"model": "test/model", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{call}}, "finish_reason": nil}}}
}

func chatFinishChunk(reason string) map[string]any {
	return map[string]any{"model": "test/model", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": reason}}}
}

// Chat completions agent whose stream consumer already holds the visible text
func partialDeliveryAgent(t *testing.T, streamed string) *fakeChatAgent {
	t.Helper()
	a := scriptedStreamAgent(t)
	a.st().streamedText = streamed
	return a
}

// Streams chunks then cause through the agent streaming call and returns the length cut with its assistant message
func partialDeliveryCut(t *testing.T, agent *fakeChatAgent, chunks []map[string]any, cause error) (map[string]any, map[string]any, *fakeChatClient) {
	t.Helper()
	client := &fakeChatClient{create: func(context.Context, map[string]any) (any, error) {
		return &scriptedChatStream{chunks: chunks, cause: cause}, nil
	}}
	agent.create = func(string, map[string]any) (any, error) { return client, nil }
	resp, err := InterruptibleStreamingAPICall(t.Context(), agent, scriptedStreamKwargs(), nil)
	cut, _ := resp.(map[string]any)
	if err != nil || cut == nil || cut["id"] != PartialStreamStubID {
		t.Fatalf("resp %+v err %v", resp, err)
	}
	choices, _ := cut["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices %+v", cut["choices"])
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	if choice["finish_reason"] != "length" || message == nil {
		t.Fatalf("cut choice %+v", choice)
	}
	return cut, message, client
}

func partialStreamStubRaw(t *testing.T, resp *Response) map[string]any {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal(resp.Raw, &raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestPartialStreamOverflowErrorReturnsTerminalStub(t *testing.T) {
	text := "Here's my long partial answer ..."
	t.Setenv("ICEBATH_STREAM_RETRIES", "0")
	cut, message, _ := partialDeliveryCut(t, partialDeliveryAgent(t, text), []map[string]any{chatContentChunk(text)},
		errors.New("This model's maximum context length is 128000 tokens. However, you requested 140000 tokens."))
	if cut[ccRawOverflowTerminal] != true {
		t.Fatalf("overflow cut %+v", cut)
	}
	if message["content"] != nil {
		t.Fatalf("recovered text seeded %v", message)
	}
}

func TestPayloadTooLargePartialIsNotMadeTerminal(t *testing.T) {
	text := "some media-heavy partial output ..."
	t.Setenv("ICEBATH_STREAM_RETRIES", "0")
	cut, message, _ := partialDeliveryCut(t, partialDeliveryAgent(t, text), []map[string]any{chatContentChunk(text)},
		errors.New("Request payload too large (413): 5MB exceeds the 4MB limit"))
	if message["content"] != text || cut[ccRawOverflowTerminal] != false {
		t.Fatalf("payload too large cut %+v", cut)
	}
}

func TestPartialStreamStalledMidToolCallWarnsAndNamesTheDroppedCalls(t *testing.T) {
	t.Setenv("ICEBATH_STREAM_RETRIES", "0")
	agent := partialDeliveryAgent(t, "Let me look.")
	agent.st().warnings = true
	chunks := []map[string]any{chatContentChunk("Let me look.")}
	for i, n := range []string{"read_file", "search_files", "terminal", "patch"} {
		chunks = append(chunks, chatToolStartChunk(i, n))
	}
	logs := captureLogs(t)
	cut, message, _ := partialDeliveryCut(t, agent, chunks, &APIError{Message: "bad request body", StatusCode: 400})
	warn := "\n\n⚠ Stream stalled mid tool-call (read_file, search_files, terminal, +1 more); the action was not executed. Ask me to retry if you want to continue."
	st := agent.st()
	st.mu.Lock()
	deltas := append([]string(nil), st.deltas...)
	st.mu.Unlock()
	warned := 0
	for _, d := range deltas {
		if d == warn {
			warned++
		}
	}
	if message["content"] != "Let me look."+warn || warned != 1 {
		t.Fatalf("content %q deltas %q", message["content"], deltas)
	}
	if got := cut[ccRawDroppedToolNames]; fmt.Sprint(got) != "[read_file search_files terminal patch]" {
		t.Fatalf("dropped %v", got)
	}
	if logCount(logs, "Partial stream dropped tool call(s) ['read_file', 'search_files', 'terminal', 'patch'] after") != 1 ||
		logCount(logs, "returning length-truncated cut") != 0 {
		t.Fatal("tool call drop log")
	}
}

func TestContentPolicyPartialStreamStubIsTaggedForFallback(t *testing.T) {
	t.Setenv("ICEBATH_STREAM_RETRIES", "0")
	cut, _, _ := partialDeliveryCut(t, partialDeliveryAgent(t, "Sure, here"), []map[string]any{chatContentChunk("Sure, here")},
		&APIError{StatusCode: 400, Message: "Your request was flagged by our safety system: content_filter"})
	if cut[ccRawContentFilter] != true || cut[ccRawOverflowTerminal] == true {
		t.Fatalf("content filter cut %+v", cut)
	}
}

func TestStreamFailureAfterDeliveryIsPartial(t *testing.T) {
	t.Setenv("ICEBATH_STREAM_RETRIES", "2")
	cut, message, client := partialDeliveryCut(t, partialDeliveryAgent(t, "partial"), []map[string]any{chatContentChunk("partial")},
		&AuxTransportError{Class: "ReadError", Message: "connection lost"})
	if message["content"] != "partial" || cut[ccRawOverflowTerminal] == true {
		t.Fatalf("cut %+v", cut)
	}
	if n := client.calls.Load(); n != 1 {
		t.Fatalf("calls %d", n)
	}
}

func TestAnthropicPartialStubPassesTransportValidation(t *testing.T) {
	for _, c := range []struct {
		content  string
		overflow bool
	}{{"partial answer", false}, {"", true}} {
		stub := buildPartialStreamStub(Route{Provider: "anthropic", Mode: ModeAnthropicMessages}, c.content, "", "m", nil, nil,
			partialStreamStubOptions{overflowTerminal: c.overflow, apiMode: ModeAnthropicMessages})
		raw := partialStreamStubRaw(t, stub)
		tr := GetTransport(string(ModeAnthropicMessages))
		if !tr.ValidateResponse(raw) || raw["stop_reason"] != "max_tokens" || tr.MapFinishReason("max_tokens") != "length" || raw[ccRawOverflowTerminal] != c.overflow {
			t.Fatalf("anthropic stub %s", stub.Raw)
		}
		blocks, _ := raw["content"].([]any)
		if len(blocks) != 1 || blocks[0].(map[string]any)["text"] != c.content {
			t.Fatalf("one text block %v", blocks)
		}
	}
}
