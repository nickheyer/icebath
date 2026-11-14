package provider

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// Anthropic message stream replaying scripted events whose SDK snapshot is the given final message
type scriptedAnthMessageStream struct {
	events []map[string]any
	pos    int
	final  map[string]any
}

func (s *scriptedAnthMessageStream) Next(context.Context) (any, error) {
	if s.pos >= len(s.events) {
		return nil, io.EOF
	}
	s.pos++
	return s.events[s.pos-1], nil
}

func (s *scriptedAnthMessageStream) Close() error { return nil }
func (s *scriptedAnthMessageStream) HTTPResponse() *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{}}
}
func (s *scriptedAnthMessageStream) AttemptConn() net.Conn    { return nil }
func (s *scriptedAnthMessageStream) Snapshot() map[string]any { return s.final }

// Client whose messages.stream hands out a fresh scripted stream per attempt like the hermes MagicMock
type scriptedAnthClient struct {
	events []map[string]any
	final  map[string]any
}

func (c *scriptedAnthClient) MessagesStream(context.Context, map[string]any) (anthMessageStream, error) {
	return &scriptedAnthMessageStream{events: append([]map[string]any(nil), c.events...), final: c.final}, nil
}

// Agent of tests/agent/test_anthropic_mid_tool_call_drop.py _make_anthropic_agent over the scripted client
func midToolCallAgent(t *testing.T, client *scriptedAnthClient) *fakeChatAgent {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	return &fakeChatAgent{provider: "custom", apiMode: "anthropic_messages", baseURL: "https://example.com/v1", model: "claude-opus-4-7", plat: "cli",
		resolve:         func(map[string]any) (any, error) { return 60.0, nil },
		anthropicCreate: func(string) (any, error) { return client, nil }}
}

func midToolUseBlock(input map[string]any) map[string]any {
	if input == nil {
		input = map[string]any{}
	}
	return map[string]any{"type": "tool_use", "id": "toolu_x", "name": "write_file", "input": input}
}

func midToolUseStartEvent() map[string]any {
	return map[string]any{"type": "content_block_start", "content_block": map[string]any{"type": "tool_use", "name": "write_file"}}
}

func TestAnthropicMidToolCallStreamDropToolUseWithoutStopReasonRaisesEmptyStream(t *testing.T) {
	dropped := map[string]any{"content": []any{midToolUseBlock(nil)}, "stop_reason": nil, "usage": map[string]any{"input_tokens": 10, "output_tokens": 2}}
	agent := midToolCallAgent(t, &scriptedAnthClient{events: []map[string]any{midToolUseStartEvent(), {"type": "message_stop"}}, final: dropped})
	_, err := InterruptibleStreamingAPICall(t.Context(), agent, map[string]any{"model": "claude-opus-4-7"}, nil)
	var empty *EmptyStreamError
	if !errors.As(err, &empty) || !strings.Contains(err.Error(), "tool_use") {
		t.Fatalf("err %T %v", err, err)
	}
}

func TestAnthropicMidToolCallStreamDropCompletedToolUseWithStopReasonPasses(t *testing.T) {
	done := map[string]any{"content": []any{midToolUseBlock(map[string]any{"path": "a.txt"})}, "stop_reason": "tool_use",
		"usage": map[string]any{"input_tokens": 10, "output_tokens": 5}}
	agent := midToolCallAgent(t, &scriptedAnthClient{events: []map[string]any{midToolUseStartEvent(), {"type": "message_stop"}}, final: done})
	response, err := InterruptibleStreamingAPICall(t.Context(), agent, map[string]any{"model": "claude-opus-4-7"}, nil)
	if err != nil || !reflect.DeepEqual(response, done) {
		t.Fatalf("response %v err %v", response, err)
	}
}

func TestAnthropicMidToolCallStreamDropTextOnlyMessageWithoutMessageStopRaisesEmptyStream(t *testing.T) {
	textOnly := map[string]any{"content": []any{map[string]any{"type": "text", "text": "partial answer"}}, "stop_reason": nil,
		"usage": map[string]any{"input_tokens": 10, "output_tokens": 5}}
	textDelta := map[string]any{"type": "content_block_delta", "delta": map[string]any{"type": "text_delta", "text": "partial answer"}}
	agent := midToolCallAgent(t, &scriptedAnthClient{events: []map[string]any{textDelta}, final: textOnly})
	_, err := InterruptibleStreamingAPICall(t.Context(), agent, map[string]any{"model": "claude-opus-4-7"}, nil)
	var empty *EmptyStreamError
	if !errors.As(err, &empty) || !strings.Contains(err.Error(), "message_stop") {
		t.Fatalf("err %T %v", err, err)
	}
}
