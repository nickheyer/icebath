package provider

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// Responses client fake that streams a scripted event list from its direct create
type auxEventsResponsesClient struct {
	base   string
	events []map[string]any
}

func (c *auxEventsResponsesClient) BaseURL() string { return c.base }
func (c *auxEventsResponsesClient) APIKey() string  { return "codex-key" }
func (c *auxEventsResponsesClient) Close() error    { return nil }
func (c *auxEventsResponsesClient) CreateChatCompletion(context.Context, map[string]any) (any, error) {
	return nil, errors.New("chat completions unused")
}
func (c *auxEventsResponsesClient) CreateResponse(context.Context, map[string]any) (any, error) {
	return &auxFakeEventStream{events: c.events}, nil
}

// Chat chunk with one tool call delta
func auxToolCallChunk(call map[string]any) map[string]any {
	return auxChunk(map[string]any{"tool_calls": []any{call}}, nil, nil)
}

func TestContentBearingProgressCodexEmptyAndStructuralEventsAreNotProgress(t *testing.T) {
	events := []map[string]any{
		{"type": "response.created"},
		{"type": "response.output_item.added", "item": map[string]any{"type": "message"}},
		{"type": "response.content_part.added", "part": map[string]any{"type": "output_text"}},
		{"type": "response.output_text.delta", "delta": ""},
		{"type": "response.reasoning_summary_text.delta", "delta": ""},
		{"type": "response.function_call_arguments.delta", "delta": ""},
		{"type": "response.output_item.added", "item": map[string]any{"type": "function_call"}},
	}
	for _, event := range events {
		if codexEventHasContent(event) {
			t.Errorf("event %v reads as content", event)
		}
	}
}

func TestContentBearingProgressCodexNonemptyDeltasAreProgress(t *testing.T) {
	events := []map[string]any{
		{"type": "response.output_text.delta", "delta": "token"},
		{"type": "response.reasoning_summary_text.delta", "delta": "thought"},
		{"type": "response.function_call_arguments.delta", "delta": `{"x"`},
		{"type": "response.output_item.added", "item": map[string]any{"type": "function_call", "id": "item_1", "call_id": "call_1", "name": "lookup"}},
	}
	for _, event := range events {
		if !codexEventHasContent(event) {
			t.Errorf("event %v reads as structural", event)
		}
	}
}

func TestContentBearingProgressChatStreamEmptyToolScaffoldingIsNotProgress(t *testing.T) {
	ticks := 0
	ctx := AuxProgressHook(context.Background(), func() error { ticks++; return nil })
	acc := newAuxChatStreamAccumulator(ctx, "", nil)
	if _, err := acc.Feed(auxToolCallChunk(map[string]any{"index": 0, "id": nil, "function": map[string]any{"name": nil, "arguments": ""}})); err != nil {
		t.Fatal(err)
	}
	if ticks != 0 {
		t.Fatalf("ticks %d", ticks)
	}
}

func TestContentBearingProgressChatStreamSubstantiveToolFragmentsAreProgress(t *testing.T) {
	calls := []map[string]any{
		{"index": 0, "id": "call_1", "function": nil},
		{"index": 0, "id": nil, "function": map[string]any{"name": "lookup", "arguments": ""}},
		{"index": 0, "id": nil, "function": map[string]any{"name": nil, "arguments": `{"q"`}},
	}
	for _, call := range calls {
		ticks := 0
		ctx := AuxProgressHook(context.Background(), func() error { ticks++; return nil })
		acc := newAuxChatStreamAccumulator(ctx, "", nil)
		if _, err := acc.Feed(auxToolCallChunk(call)); err != nil {
			t.Fatal(err)
		}
		if ticks != 1 {
			t.Errorf("tool call %v ticks %d", call, ticks)
		}
	}
}

func TestContentBearingProgressOpenrouterReasoningDetailsKeepCompressionAlive(t *testing.T) {
	ticks := 0
	ctx := AuxProgressHook(context.Background(), func() error { ticks++; return nil })
	acc := newAuxChatStreamAccumulator(ctx, "", nil)
	detail := map[string]any{"type": "reasoning.summary", "summary": "working..."}
	if _, err := acc.Feed(auxChunk(map[string]any{"reasoning_details": []any{detail}}, nil, nil)); err != nil {
		t.Fatal(err)
	}
	result, err := acc.Finish()
	if err != nil {
		t.Fatal(err)
	}
	if ticks != 1 {
		t.Fatalf("ticks %d", ticks)
	}
	if got := auxFirstMessage(t, result)["reasoning_details"]; !reflect.DeepEqual(got, []any{detail}) {
		t.Fatalf("reasoning_details %v", got)
	}
}

func TestContentBearingProgressStructuralReasoningDetailsAreNotProgress(t *testing.T) {
	ticks := 0
	ctx := AuxProgressHook(context.Background(), func() error { ticks++; return nil })
	acc := newAuxChatStreamAccumulator(ctx, "", nil)
	if _, err := acc.Feed(auxChunk(map[string]any{"reasoning_details": []any{map[string]any{"type": "reasoning.encrypted", "signature": "sig"}}}, nil, nil)); err != nil {
		t.Fatal(err)
	}
	if ticks != 0 {
		t.Fatalf("ticks %d", ticks)
	}
}

func TestContentBearingProgressCodexAdapterUpdatesFenceOnlyForSubstantiveEvents(t *testing.T) {
	events := []map[string]any{
		{"type": "response.created"},
		{"type": "response.output_text.delta", "delta": ""},
		{"type": "response.output_text.delta", "delta": "token"},
		{"type": "response.output_item.added", "item": map[string]any{"type": "function_call", "id": "item_1", "call_id": "call_1", "name": "lookup"}},
	}
	adapter := &auxCodexCompletionsAdapter{client: &auxEventsResponsesClient{base: "https://chatgpt.com/backend-api/codex", events: events}, model: "gpt-5.6-sol"}
	prev := auxConsumeCodexEventStreamFn
	auxConsumeCodexEventStreamFn = func(ctx context.Context, stream AuxChatStream, _ string, onEvent func(any) error) (map[string]any, error) {
		for {
			event, err := stream.Next(ctx)
			if err != nil {
				break
			}
			if err := onEvent(event); err != nil {
				return nil, err
			}
		}
		return map[string]any{"output": []any{}, "usage": nil}, nil
	}
	t.Cleanup(func() { auxConsumeCodexEventStreamFn = prev })
	touches := 0
	ctx := AuxProgressHook(context.Background(), func() error { touches++; return nil })
	if _, err := adapter.create(ctx, map[string]any{"messages": []any{map[string]any{"role": "user", "content": "summarize"}}}); err != nil {
		t.Fatal(err)
	}
	if touches != 2 {
		t.Fatalf("touches %d", touches)
	}
}

func TestContentBearingProgressAnthropicAdapterUpdatesFenceOnlyForSubstantiveEvents(t *testing.T) {
	events := []map[string]any{
		{"type": "ping"},
		{"type": "content_block_delta", "delta": map[string]any{"type": "text_delta", "text": ""}},
		{"type": "content_block_delta", "delta": map[string]any{"type": "input_json_delta", "partial_json": `{"q"`}},
		{"type": "content_block_start", "content_block": map[string]any{"type": "tool_use", "id": "toolu_1", "name": "lookup"}},
	}
	adapter := newAuxAnthropicCompletionsAdapter(&anthropicSDKClient{}, "claude-sonnet-4-6", false, "")
	prevBuild, prevCreate := auxAnthBuildKwargsFn, auxCreateAnthropicMessageFn
	auxAnthBuildKwargsFn = func(anthBuildInput) (map[string]any, error) {
		return map[string]any{"model": "claude-sonnet-4-6", "messages": []any{}}, nil
	}
	auxCreateAnthropicMessageFn = func(_ context.Context, _ *anthropicSDKClient, _ map[string]any, onStreamEvent func(any) error) (map[string]any, error) {
		for _, event := range events {
			if err := onStreamEvent(event); err != nil {
				return nil, err
			}
		}
		return nil, errors.New("stop after callback verification")
	}
	t.Cleanup(func() { auxAnthBuildKwargsFn, auxCreateAnthropicMessageFn = prevBuild, prevCreate })
	touches := 0
	ctx := AuxProgressHook(context.Background(), func() error { touches++; return nil })
	_, err := adapter.create(ctx, map[string]any{"messages": []any{map[string]any{"role": "user", "content": "summarize"}}})
	if err == nil || !strings.Contains(err.Error(), "stop after callback verification") {
		t.Fatalf("err %v", err)
	}
	if touches != 2 {
		t.Fatalf("touches %d", touches)
	}
}

func TestContentBearingProgressContentFreeFramesStillRecordTtfpTiming(t *testing.T) {
	responses := 0
	ctx := AuxThreadLocalHook(context.Background(), AuxProviderResponseLocal, func() error { responses++; return nil })
	ctx = AuxProgressHook(ctx, func() error { return nil })
	acc := newAuxChatStreamAccumulator(ctx, "", nil)
	if _, err := acc.Feed(map[string]any{"id": nil, "model": nil, "choices": []any{}, "usage": nil}); err != nil {
		t.Fatal(err)
	}
	if responses == 0 {
		t.Fatal("content-free first frame must still record TTFP")
	}
}
