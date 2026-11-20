package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
)

// Responses stream replaying one incomplete final response as item done and status events
type codexPartialStream struct{ events []map[string]any }

func (s *codexPartialStream) Next(context.Context) (map[string]any, error) {
	if len(s.events) == 0 {
		return nil, io.EOF
	}
	ev := s.events[0]
	s.events = s.events[1:]
	return ev, nil
}

func (s *codexPartialStream) Close() error { return nil }

// Responses client streaming a partial checkpoint cut by the output token cap
type codexPartialResponsesClient struct{ final map[string]any }

func (c *codexPartialResponsesClient) BaseURL() string { return "" }
func (c *codexPartialResponsesClient) APIKey() string  { return "k" }
func (c *codexPartialResponsesClient) Close() error    { return nil }
func (c *codexPartialResponsesClient) CreateChatCompletion(context.Context, map[string]any) (any, error) {
	return nil, errors.New("chat completions unused")
}
func (c *codexPartialResponsesClient) CreateResponse(_ context.Context, kwargs map[string]any) (any, error) {
	if kwargs["stream"] != true {
		return nil, errors.New("stream must be requested")
	}
	var events []map[string]any
	for _, item := range c.final["output"].([]any) {
		events = append(events, map[string]any{"type": "response.output_item.done", "item": item})
	}
	events = append(events, map[string]any{"type": "response." + c.final["status"].(string), "response": c.final})
	return &codexPartialStream{events: events}, nil
}

func TestCompressorRejectsActualAdapterPartialSummary(t *testing.T) {
	partial := map[string]any{"status": "incomplete", "output": []any{map[string]any{"type": "message", "role": "assistant", "phase": "final_answer",
		"status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "PARTIAL_CHECKPOINT"}}}},
		"incomplete_details": map[string]any{"reason": "max_output_tokens"}, "error": nil,
		"usage": map[string]any{"input_tokens": 11, "output_tokens": 3, "total_tokens": 14}}
	var calls []any
	prev := compressorCallLLM
	t.Cleanup(func() { compressorCallLLM = prev })
	compressorCallLLM = func(ctx context.Context, kwargs map[string]any) (any, error) {
		calls = append(calls, kwargs["model"])
		adapter := provider.NewCodexAuxiliaryClient(&codexPartialResponsesClient{final: partial}, "aux-model")
		return adapter.CreateChatCompletion(ctx, map[string]any{"messages": []any{map[string]any{"role": "user", "content": "Summarize the task."}}})
	}
	t.Setenv("ICEBATH_HOME", t.TempDir())
	contextLength := 100000
	opts := DefaultContextCompressorOptions("main-model")
	opts.QuietMode, opts.TailMode, opts.ProtectFirstN, opts.ProtectLastN, opts.AbortOnSummaryFailure, opts.ConfigContextLength = true, "legacy", 2, 2, false, &contextLength
	compressor := NewContextCompressor(context.Background(), opts)
	var messages, original []map[string]any
	for i := 0; i < 12; i++ {
		role := "assistant"
		if i%2 == 0 {
			role = "user"
		}
		content := fmt.Sprintf("task %d ", i) + strings.Repeat("x", 50)
		messages = append(messages, map[string]any{"role": role, "content": content})
		original = append(original, map[string]any{"role": role, "content": content})
	}
	currentTokens := 999999
	result, err := compressor.Compress(messages, &currentTokens, nil, true, "")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, []any{nil}) {
		t.Fatalf("summary calls %v", calls)
	}
	if !reflect.DeepEqual(result, original) || !reflect.DeepEqual(messages, original) {
		t.Fatalf("partial checkpoint was committed: result %v", result)
	}
	if compressor.previousSummary != nil || !compressor.lastSummaryTruncatedFailure || !compressor.lastCompressAborted {
		t.Fatalf("previous summary %v truncated %v aborted %v", compressor.previousSummary, compressor.lastSummaryTruncatedFailure, compressor.lastCompressAborted)
	}
}
