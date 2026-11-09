package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

// Assistant message dict of the first choice of a chat completion shaped result
func auxFirstMessage(t *testing.T, result any) map[string]any {
	t.Helper()
	choice, ok := auxField(result, "choices", nil).([]any)
	if !ok || len(choice) == 0 {
		t.Fatalf("result %v", result)
	}
	msg, ok := auxField(choice[0], "message", nil).(map[string]any)
	if !ok {
		t.Fatalf("choice %v", choice[0])
	}
	return msg
}

func TestAuxProgressHookHookIsThreadLocal(t *testing.T) {
	ticks := 0
	_ = AuxProgressHook(context.Background(), func() error { ticks++; return nil })
	seen := make(chan int, 1)
	go func() {
		auxNotifyAuxProgress(context.Background())
		seen <- ticks
	}()
	if got := <-seen; got != 0 {
		t.Fatalf("seen in thread %d", got)
	}
}

func TestCreateWithProgressHookUpgradesToStreamingAndTicksOnlyForPayload(t *testing.T) {
	ticks := 0
	ctx := AuxProgressHook(context.Background(), func() error { ticks++; return nil })
	chunks := []map[string]any{
		{"id": nil, "model": nil, "choices": []any{}, "usage": nil},
		auxChunk(map[string]any{"content": "", "reasoning": "", "tool_calls": []any{map[string]any{"index": 0, "id": nil, "function": map[string]any{"name": nil, "arguments": ""}}}}, nil, nil),
		auxChunk(map[string]any{"reasoning": "thinking..."}, nil, nil),
		auxChunk(map[string]any{"content": "Hello "}, nil, nil),
		auxChunk(map[string]any{"content": "world"}, "stop", map[string]any{"prompt_tokens": 5, "completion_tokens": 2, "total_tokens": 7}),
	}
	client := auxFakeStreamingClient(nil, chunks, nil)
	got, err := auxCreateWithProgress(ctx, client, map[string]any{"model": "m1", "messages": []any{}, "timeout": 30}, "", false)
	if err != nil || client.calls[0]["stream"] != true {
		t.Fatalf("err %v", err)
	}
	choice := got.(map[string]any)["choices"].([]any)[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	if msg["content"] != "Hello world" || msg["reasoning"] != "thinking..." || choice["finish_reason"] != "stop" || auxField(got.(map[string]any)["usage"], "total_tokens", nil) != 7 {
		t.Fatalf("msg %v", msg)
	}
	if ticks != 3 {
		t.Fatalf("ticks %d", ticks)
	}
}

func TestCreateWithProgressReasoningOnlyInModelExtraIsCapturedAndCountsAsProgress(t *testing.T) {
	ticks := 0
	ctx := AuxProgressHook(context.Background(), func() error { ticks++; return nil })
	client := auxFakeStreamingClient(nil, []map[string]any{auxChunk(map[string]any{"reasoning_content": "thinking..."}, "stop", nil)}, nil)
	got, err := auxCreateWithProgress(ctx, client, map[string]any{"model": "m1", "messages": []any{}, "timeout": 30}, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if auxFirstMessage(t, got)["reasoning"] != "thinking..." {
		t.Fatalf("result %v", got)
	}
	if ticks != 1 {
		t.Fatalf("ticks %d", ticks)
	}
}

func TestCreateWithProgressCompletedResponseTicksOnlyTerminalSignals(t *testing.T) {
	ticks := 0
	ctx := AuxProgressHook(context.Background(), func() error { ticks++; return nil })
	complete := map[string]any{"id": "r1", "model": "m1", "object": "chat.completion", "choices": []any{map[string]any{"index": 0,
		"message": map[string]any{"role": "assistant", "content": "non-streamed"}, "finish_reason": "stop"}}, "usage": nil}
	client := &auxScriptedClient{create: func(map[string]any) (any, error) { return complete, nil }}
	got, err := auxCreateWithProgress(ctx, client, map[string]any{"model": "m1", "messages": []any{}}, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if client.calls[0]["stream"] != true || !auxSameObject(got, complete) {
		t.Fatalf("calls %v result %v", client.calls, got)
	}
	if ticks != 1 {
		t.Fatalf("ticks %d", ticks)
	}
}

func TestCreateWithProgressStreamingRejectedFallsBackToPlainCall(t *testing.T) {
	ctx := AuxProgressHook(context.Background(), func() error { return nil })
	complete := map[string]any{"id": "r1", "choices": []any{map[string]any{"message": map[string]any{"content": "non-streamed"}}}}
	client := auxFakeStreamingClient(complete, nil, &AuxSDKError{Class: "RuntimeError", Message: "stream is not supported by this model"})
	got, err := auxCreateWithProgress(ctx, client, map[string]any{"model": "m1", "messages": []any{}}, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if !auxSameObject(got, complete) || len(client.calls) != 2 || client.calls[0]["stream"] != true {
		t.Fatalf("plain fallback %v", client.calls)
	}
	if _, has := client.calls[1]["stream"]; has {
		t.Fatalf("second call keeps stream %v", client.calls[1])
	}
}

func TestAggregateChatStreamToolCallDeltasAreReassembled(t *testing.T) {
	stream := &auxSliceStream{chunks: []map[string]any{
		auxChunk(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call_1", "function": map[string]any{"name": "do_thing", "arguments": `{"a"`}}}}, nil, nil),
		auxChunk(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": nil, "function": map[string]any{"name": nil, "arguments": ": 1}"}}}}, "tool_calls", nil),
	}}
	got, err := auxAggregateChatStream(context.Background(), stream, "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	choice := got.(map[string]any)["choices"].([]any)[0].(map[string]any)
	calls := choice["message"].(map[string]any)["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("calls %v", calls)
	}
	fn := calls[0].(map[string]any)["function"].(map[string]any)
	if calls[0].(map[string]any)["id"] != "call_1" || fn["name"] != "do_thing" || fn["arguments"] != `{"a": 1}` || choice["finish_reason"] != "tool_calls" {
		t.Fatalf("calls %v finish %v", calls, choice["finish_reason"])
	}
}

func TestAggregateChatStreamStreamCloseIsCalled(t *testing.T) {
	stream := &auxSliceStream{chunks: []map[string]any{auxChunk(map[string]any{"content": "ok"}, "stop", nil)}}
	got, err := auxAggregateChatStream(context.Background(), stream, "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if auxFirstMessage(t, got)["content"] != "ok" || !stream.closed {
		t.Fatalf("result %v closed %v", got, stream.closed)
	}
}

func TestProviderRequiresStreamNormalEndpointsAreNot(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxFakeConfigTree(t, config.Tree{})
	if auxProviderRequiresStream(ctx, "https://openrouter.ai/api/v1") || auxProviderRequiresStream(ctx, "") {
		t.Fatal("normal endpoints never require streaming")
	}
}

func TestProviderRequiresStreamConfigMarkerMatchesCustomEndpoint(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxFakeConfigTree(t, config.Tree{"auxiliary": map[string]any{"stream_only_base_urls": []any{"my-proxy.example.com"}}})
	if !auxProviderRequiresStream(ctx, "https://my-proxy.example.com/v1") || auxProviderRequiresStream(ctx, "https://other.example.com/v1") {
		t.Fatal("config markers match their own host only")
	}
	auxFakeConfigTree(t, config.Tree{"auxiliary": map[string]any{"stream_only_base_urls": []any{" Gateway.Example ", "", 3}}})
	if auxProviderRequiresStream(ctx, "") || !auxProviderRequiresStream(ctx, "https://copilot.tencent.com/v1") || !auxProviderRequiresStream(ctx, "https://api.gateway.example/v1") || auxProviderRequiresStream(ctx, "https://api.openai.com/v1") {
		t.Fatal("stream only markers")
	}
	auxFakeConfigTree(t, config.Tree{"auxiliary": nil})
	if auxProviderRequiresStream(ctx, "https://api.gateway.example/v1") {
		t.Fatal("a null auxiliary section reads as no markers")
	}
}

func TestForceStreamForceStreamStreamsWithoutAHook(t *testing.T) {
	client := auxFakeStreamingClient(nil, []map[string]any{auxChunk(map[string]any{"content": "hi"}, "stop", nil)}, nil)
	got, err := auxCreateWithProgress(context.Background(), client, map[string]any{"model": "m1", "messages": []any{}}, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if client.calls[0]["stream"] != true || auxFirstMessage(t, got)["content"] != "hi" {
		t.Fatalf("calls %v result %v", client.calls, got)
	}
}

func TestForceStreamForceStreamDoesNotRetryNonstreamingOnFailure(t *testing.T) {
	complete := map[string]any{"id": "r1", "choices": []any{map[string]any{"message": map[string]any{"content": "non-streamed"}}}}
	client := auxFakeStreamingClient(complete, nil, &AuxSDKError{Class: "RuntimeError", Message: "HTTP 400 bad request"})
	_, err := auxCreateWithProgress(context.Background(), client, map[string]any{"model": "m1", "messages": []any{}}, "", true)
	if err == nil || !strings.Contains(err.Error(), "bad request") {
		t.Fatalf("err %v", err)
	}
	if len(client.calls) != 1 {
		t.Fatalf("calls %v", client.calls)
	}
}

func TestAsyncStreamAggregationAsyncStreamIsConsumedWithAsyncFor(t *testing.T) {
	stream := &auxSliceStream{chunks: []map[string]any{
		auxChunk(map[string]any{"content": "part1 "}, nil, nil),
		auxChunk(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call_9", "function": map[string]any{"name": "lookup", "arguments": `{"q":`}}}}, nil, nil),
		auxChunk(map[string]any{"content": "part2", "tool_calls": []any{map[string]any{"index": 0, "id": nil, "function": map[string]any{"name": nil, "arguments": `"x"}`}}}}, "tool_calls", nil),
	}}
	got, err := auxAggregateChatStreamAsync(context.Background(), stream, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	msg := auxFirstMessage(t, got)
	calls := msg["tool_calls"].([]any)
	fn := calls[0].(map[string]any)["function"].(map[string]any)
	finish := auxField(got.(map[string]any)["choices"].([]any)[0], "finish_reason", nil)
	if msg["content"] != "part1 part2" || fn["name"] != "lookup" || fn["arguments"] != `{"q":"x"}` || finish != "tool_calls" || !stream.closed {
		t.Fatalf("msg %v finish %v closed %v", msg, finish, stream.closed)
	}
}

func TestAsyncStreamAggregationAcreateWithStreamPassesStreamKwargs(t *testing.T) {
	stream := &auxSliceStream{chunks: []map[string]any{auxChunk(map[string]any{"content": "ok"}, "stop", nil)}}
	client := &auxScriptedClient{create: func(map[string]any) (any, error) { return stream, nil }}
	got, err := auxAcreateWithStream(context.Background(), client, map[string]any{"model": "m1", "messages": []any{}, "timeout": 30})
	if err != nil {
		t.Fatal(err)
	}
	if client.calls[0]["stream"] != true || auxFirstMessage(t, got)["content"] != "ok" {
		t.Fatalf("calls %v result %v", client.calls, got)
	}
}

func TestAsyncStreamAggregationAsyncDispatchOnlyAuthFailureDoesNotTickProgress(t *testing.T) {
	progress, dispatches := 0, 0
	ctx := AuxProgressHook(context.Background(), func() error { progress++; return nil })
	ctx = AuxThreadLocalHook(ctx, AuxDispatchLocal, func() error { dispatches++; return nil })
	client := auxFakeStreamingClient(nil, nil, auxExc("unauthorized", 401))
	if _, err := auxAcreateWithProgress(ctx, client, map[string]any{"model": "m1", "messages": []any{}, "timeout": 30}, "", false); err == nil {
		t.Fatal("auth failure surfaces")
	}
	if dispatches != 1 || progress != 0 {
		t.Fatalf("dispatches %d progress %d", dispatches, progress)
	}
}
