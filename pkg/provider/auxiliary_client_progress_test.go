package provider

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Chat stream over a fixed chunk list that records its close
type auxSliceStream struct {
	chunks []map[string]any
	closed bool
}

func (s *auxSliceStream) Next(context.Context) (map[string]any, error) {
	if len(s.chunks) == 0 {
		return nil, io.EOF
	}
	c := s.chunks[0]
	s.chunks = s.chunks[1:]
	return c, nil
}

func (s *auxSliceStream) Close() error { s.closed = true; return nil }

// Client whose create is a test callback recording every kwargs map
type auxScriptedClient struct {
	mu     sync.Mutex
	calls  []map[string]any
	base   string
	create func(kwargs map[string]any) (any, error)
}

func (c *auxScriptedClient) BaseURL() string { return c.base }
func (c *auxScriptedClient) APIKey() string  { return "k" }
func (c *auxScriptedClient) Close() error    { return nil }
func (c *auxScriptedClient) CreateChatCompletion(_ context.Context, kwargs map[string]any) (any, error) {
	c.mu.Lock()
	c.calls = append(c.calls, kwargs)
	c.mu.Unlock()
	return c.create(kwargs)
}

// Client answering stream requests with the chunks or the error and plain requests with the response
func auxFakeStreamingClient(response any, chunks []map[string]any, streamErr error) *auxScriptedClient {
	return &auxScriptedClient{create: func(kwargs map[string]any) (any, error) {
		if kwargs["stream"] == true {
			if streamErr != nil {
				return nil, streamErr
			}
			return &auxSliceStream{chunks: chunks}, nil
		}
		return response, nil
	}}
}

func auxChunk(delta map[string]any, finish any, usage any) map[string]any {
	return map[string]any{"id": "c1", "model": "m1", "choices": []any{map[string]any{"delta": delta, "finish_reason": finish}}, "usage": usage}
}

const auxOpenrouter402 = "Error code: 402 - {'error': {'message': 'This request requires more credits, or fewer max_tokens. You requested up to 65536 tokens, but can only afford 7117. To increase, visit https://openrouter.ai/settings/credits and add more credits', 'code': 402}}"

func TestAuxAffordableExtraction(t *testing.T) {
	if n, ok := auxAffordableMaxTokensFromError(auxExc(auxOpenrouter402, 402)); !ok || n != 7117-64 {
		t.Fatalf("budget minus margin %d %v", n, ok)
	}
	if _, ok := auxAffordableMaxTokensFromError(auxExc("Error code: 402 - insufficient funds", 402)); ok {
		t.Fatal("plain exhaustion")
	}
	if _, ok := auxAffordableMaxTokensFromError(auxExc("You requested up to 65536 tokens, but can only afford 100.", 402)); ok {
		t.Fatal("tiny budget is exhaustion")
	}
	if _, ok := auxAffordableMaxTokensFromError(&AuxTimeoutError{Message: "Codex auxiliary Responses stream stalled: no new output for 60.0s"}); ok {
		t.Fatal("non payment error")
	}
	if n, ok := auxAffordableMaxTokensFromError(auxExc("can only afford 12,345 tokens", 402)); !ok || n != 12345-64 {
		t.Fatalf("comma grouped %d", n)
	}
}

// Client that 402s while the cap exceeds the affordable budget
func auxFlakyClient(affordable int, always bool, message string) *auxScriptedClient {
	return &auxScriptedClient{create: func(kwargs map[string]any) (any, error) {
		capVal := kwargs["max_tokens"]
		if capVal == nil {
			capVal = kwargs["max_completion_tokens"]
		}
		n, isInt := capVal.(int)
		if always || !isInt || n > affordable {
			return nil, auxExc(message, 402)
		}
		return map[string]any{"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "summary"}, "finish_reason": "stop"}}, "model": kwargs["model"]}, nil
	}}
}

func TestAuxCreateWithProgressAffordableRetry(t *testing.T) {
	ctx := home.WithOverride(context.Background(), t.TempDir())
	t.Setenv("OPENROUTER_API_KEY", "sk-or-test")
	msgs := []any{map[string]any{"role": "user", "content": "summarize"}}
	client := auxFlakyClient(7117, false, auxOpenrouter402)
	resp, err := auxCreateWithProgress(ctx, client, map[string]any{"model": "google/gemini-3.6-flash", "messages": msgs}, "compression", false)
	if err != nil || len(client.calls) != 2 || client.calls[1]["max_tokens"] != 7117-64 {
		t.Fatalf("err %v calls %v", err, client.calls)
	}
	if auxField(auxField(resp.(map[string]any)["choices"].([]any)[0], "message", nil), "content", nil) != "summary" {
		t.Fatalf("resp %v", resp)
	}
	client = auxFlakyClient(100, false, auxOpenrouter402)
	if _, err := auxCreateWithProgress(ctx, client, map[string]any{"model": "m", "max_tokens": 5000, "messages": msgs}, "compression", false); err == nil || len(client.calls) != 1 {
		t.Fatal("an affordable cap does not spin")
	}
	client = auxFlakyClient(0, true, "Error code: 402 - insufficient funds")
	if _, err := auxCreateWithProgress(ctx, client, map[string]any{"model": "m", "messages": msgs}, "compression", false); err == nil || len(client.calls) != 1 {
		t.Fatal("plain exhaustion is not retried")
	}
	client = auxFlakyClient(0, true, auxOpenrouter402)
	if _, err := auxCreateWithProgress(ctx, client, map[string]any{"model": "m", "messages": msgs}, "compression", false); err == nil || len(client.calls) != 2 {
		t.Fatal("the retry 402 surfaces")
	}
}

func auxEnvelopeData(content string) map[string]any {
	return map[string]any{
		"id": "chatcmpl-env", "object": "chat.completion", "created": float64(1751000000), "model": "cline-pass/kimi-k2.7-code",
		"choices": []any{map[string]any{"index": float64(0), "message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": float64(10), "completion_tokens": float64(5), "total_tokens": float64(15)},
	}
}

func TestAuxDataEnvelopeUnwrap(t *testing.T) {
	got, err := auxUnwrapDataEnvelope(map[string]any{"data": auxEnvelopeData("unwrapped"), "success": true}, "vision")
	m, _ := got.(map[string]any)
	if err != nil || m["model"] != "cline-pass/kimi-k2.7-code" || m["created"] != float64(1751000000) || auxField(m["usage"], "total_tokens", nil) != float64(15) {
		t.Fatalf("validated unwrap %v %v", got, err)
	}
	loose := map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "hi"}}}}
	got, _ = auxUnwrapDataEnvelope(map[string]any{"data": loose}, "")
	m, _ = got.(map[string]any)
	if m["object"] != "chat.completion" || m["id"] != "" || auxField(m["choices"].([]any)[0], "finish_reason", nil) != "stop" {
		t.Fatalf("namespace unwrap %v", got)
	}
	_, err = auxUnwrapDataEnvelope(map[string]any{"choices": nil, "success": false, "data": map[string]any{"error": map[string]any{"message": "upstream provider rejected the request", "code": "bad_model"}}}, "vision")
	var re *AuxRuntimeError
	if !errors.As(err, &re) || re.Message != "Auxiliary vision: provider returned error envelope: upstream provider rejected the request" {
		t.Fatalf("error envelope %v", err)
	}
	normal := map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "hi"}}}}
	for _, resp := range []map[string]any{normal, {"choices": []any{}}, {"choices": nil, "success": true, "data": map[string]any{"no": "choices"}}} {
		got, err := auxUnwrapDataEnvelope(resp, "")
		if err != nil || !auxSameObject(got, resp) {
			t.Fatalf("unchanged %v", resp)
		}
	}
}

func TestAuxRecoverAndExtractResponseText(t *testing.T) {
	resp := map[string]any{"output": []any{map[string]any{"type": "reasoning", "content": []any{map[string]any{"text": "skip"}}}, map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": " a "}, map[string]any{"text": "b"}, map[string]any{"type": "refusal", "text": "no"}}}}}
	if got := auxExtractAuxResponseText(resp); got != "a\nb" {
		t.Fatalf("got %q", got)
	}
	if auxExtractAuxResponseText(map[string]any{"output_text": "  hi  "}) != "hi" || auxRecoverAuxResponseMessage(map[string]any{}) != nil {
		t.Fatal("output_text and empty")
	}
	rec := auxRecoverAuxResponseMessage(resp).(map[string]any)
	if auxField(rec["choices"].([]any)[0], "finish_reason", nil) != "stop" || !auxSameObject(rec, resp) {
		t.Fatalf("recovered %v", rec)
	}
}

func TestAuxCreateWithProgressStreamsUnderAHook(t *testing.T) {
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
	if msg["content"] != "Hello world" || msg["reasoning"] != "thinking..." || choice["finish_reason"] != "stop" || auxField(got.(map[string]any)["usage"], "total_tokens", nil) != 7 || ticks != 3 {
		t.Fatalf("msg %v ticks %d", msg, ticks)
	}
	ticks = 0
	client = auxFakeStreamingClient(nil, []map[string]any{auxChunk(map[string]any{"reasoning_content": "thinking..."}, "stop", nil)}, nil)
	got, _ = auxCreateWithProgress(ctx, client, map[string]any{"model": "m1", "messages": []any{}, "timeout": 30}, "", false)
	if auxField(auxField(got.(map[string]any)["choices"].([]any)[0], "message", nil), "reasoning", nil) != "thinking..." || ticks != 1 {
		t.Fatalf("reasoning content ticks %d", ticks)
	}
}

func TestAuxCreateWithProgressCompleteResponseAndPlainFallback(t *testing.T) {
	ticks := 0
	ctx := AuxProgressHook(context.Background(), func() error { ticks++; return nil })
	complete := map[string]any{"id": "r1", "choices": []any{map[string]any{"message": map[string]any{"content": "done"}}}}
	client := &auxScriptedClient{create: func(map[string]any) (any, error) { return complete, nil }}
	got, _ := auxCreateWithProgress(ctx, client, map[string]any{"model": "m1", "messages": []any{}}, "", false)
	if client.calls[0]["stream"] != true || !auxSameObject(got, complete) || ticks != 1 {
		t.Fatalf("complete response ticks %d", ticks)
	}
	client = auxFakeStreamingClient(complete, nil, &AuxSDKError{Class: "RuntimeError", Message: "stream is not supported by this model"})
	got, _ = auxCreateWithProgress(ctx, client, map[string]any{"model": "m1", "messages": []any{}}, "", false)
	if _, has := client.calls[1]["stream"]; !auxSameObject(got, complete) || len(client.calls) != 2 || has {
		t.Fatalf("plain fallback %v", client.calls)
	}
	client = auxFakeStreamingClient(complete, nil, &AuxSDKError{Class: "RuntimeError", Message: "stream is not supported by this model"})
	if _, err := auxCreateWithProgress(context.Background(), client, map[string]any{"model": "m1"}, "", true); err == nil || len(client.calls) != 1 {
		t.Fatal("force stream surfaces the rejection")
	}
}

func TestAuxDispatchOnlyAuthFailureDoesNotTickProgress(t *testing.T) {
	progress, dispatches := 0, 0
	ctx := AuxProgressHook(context.Background(), func() error { progress++; return nil })
	ctx = AuxThreadLocalHook(ctx, AuxDispatchLocal, func() error { dispatches++; return nil })
	client := auxFakeStreamingClient(nil, nil, auxExc("unauthorized", 401))
	if _, err := auxCreateWithProgressOnce(ctx, client, map[string]any{"model": "m1", "timeout": 30}, "", false); err == nil {
		t.Fatal("auth failure surfaces")
	}
	if dispatches != 1 || progress != 0 || len(client.calls) != 1 {
		t.Fatalf("dispatches %d progress %d", dispatches, progress)
	}
	client = auxFakeStreamingClient(nil, nil, auxExc("unauthorized", 401))
	if _, err := auxAcreateWithProgress(ctx, client, map[string]any{"model": "m1"}, "", false); err == nil || dispatches != 2 || progress != 0 {
		t.Fatal("async auth failure surfaces without progress")
	}
}

func TestAuxAggregateChatStreamReassemblesToolCallsAndCloses(t *testing.T) {
	stream := &auxSliceStream{chunks: []map[string]any{
		auxChunk(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call_1", "function": map[string]any{"name": "do_thing", "arguments": `{"a"`}}}}, nil, nil),
		auxChunk(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": nil, "function": map[string]any{"name": nil, "arguments": ": 1}"}}}}, "tool_calls", nil),
	}}
	got, err := auxAggregateChatStream(context.Background(), stream, "", nil, nil)
	if err != nil || !stream.closed {
		t.Fatalf("err %v closed %v", err, stream.closed)
	}
	choice := got.(map[string]any)["choices"].([]any)[0].(map[string]any)
	calls := choice["message"].(map[string]any)["tool_calls"].([]any)
	fn := calls[0].(map[string]any)["function"].(map[string]any)
	if len(calls) != 1 || fn["name"] != "do_thing" || fn["arguments"] != `{"a": 1}` || choice["finish_reason"] != "tool_calls" {
		t.Fatalf("calls %v", calls)
	}
	async := &auxSliceStream{chunks: []map[string]any{auxChunk(map[string]any{"content": "ok"}, "stop", nil)}}
	got, err = auxAcreateWithStream(context.Background(), &auxScriptedClient{create: func(map[string]any) (any, error) { return async, nil }}, map[string]any{"model": "m"})
	if err != nil || !async.closed || auxField(auxField(got.(map[string]any)["choices"].([]any)[0], "message", nil), "content", nil) != "ok" {
		t.Fatalf("async aggregation %v %v", got, err)
	}
}

func TestAuxStreamAccumulatorCeilingAndHostDeadline(t *testing.T) {
	now := time.Unix(1000, 0)
	prev := promptReadingNow
	promptReadingNow = func() time.Time { return now }
	t.Cleanup(func() { promptReadingNow = prev })
	ceiling := 600.0
	acc := newAuxChatStreamAccumulator(context.Background(), "m", &ceiling)
	now = now.Add(601 * time.Second)
	if _, err := acc.Feed(auxChunk(map[string]any{"content": "x"}, nil, nil)); err == nil || err.Error() != "Auxiliary streamed call timed out after 600s total ceiling (stream still open but over budget)" || !auxIsTimeoutError(err) {
		t.Fatalf("ceiling %v", err)
	}
	deadline := now.Add(5 * time.Second)
	ctx := AuxStreamDeadline(context.Background(), func() (time.Time, bool) { return deadline, true })
	acc = newAuxChatStreamAccumulator(ctx, "m", nil)
	if _, err := acc.Feed(auxChunk(map[string]any{"content": "x"}, nil, nil)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(7 * time.Second)
	if _, err := acc.Feed(auxChunk(map[string]any{"content": "y"}, nil, nil)); err == nil || !strings.Contains(err.Error(), "host compression deadline after 7s") {
		t.Fatalf("host deadline %v", err)
	}
}

func TestAuxProviderRequiresStream(t *testing.T) {
	prev := auxLoadConfigFn
	t.Cleanup(func() { auxLoadConfigFn = prev })
	auxLoadConfigFn = func(context.Context) (config.Tree, error) {
		return config.Tree{"auxiliary": map[string]any{"stream_only_base_urls": []any{" Gateway.Example ", "", 3}}}, nil
	}
	ctx := context.Background()
	if auxProviderRequiresStream(ctx, "") || !auxProviderRequiresStream(ctx, "https://copilot.tencent.com/v1") || !auxProviderRequiresStream(ctx, "https://api.gateway.example/v1") || auxProviderRequiresStream(ctx, "https://api.openai.com/v1") {
		t.Fatal("stream only markers")
	}
	auxLoadConfigFn = func(context.Context) (config.Tree, error) { return config.Tree{"auxiliary": nil}, nil }
	if auxProviderRequiresStream(ctx, "https://api.gateway.example/v1") {
		t.Fatal("a null auxiliary section reads as no markers")
	}
}

func TestAuxStreamRequestPlan(t *testing.T) {
	kw, model, ceiling := auxStreamRequestPlan(map[string]any{"model": "m", "timeout": 200}, &auxScriptedClient{base: "https://api.example.com/v1"})
	if kw["stream"] != true || model != "m" || ceiling != 800 || auxField(kw["stream_options"], "include_usage", nil) != true {
		t.Fatalf("%v %q %v", kw, model, ceiling)
	}
	if _, _, c := auxStreamRequestPlan(map[string]any{"timeout": "soon"}, nil); c != 600 {
		t.Fatalf("unparsable timeout gives the floor %v", c)
	}
}
