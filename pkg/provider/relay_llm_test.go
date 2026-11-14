package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyctx"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

type relayLLMTestDelta struct {
	Content   any `json:"content"`
	ToolCalls any `json:"tool_calls"`
}

type relayLLMTestChoice struct {
	Delta        relayLLMTestDelta `json:"delta"`
	FinishReason any               `json:"finish_reason"`
}

type relayLLMTestChunk struct {
	Model   string               `json:"model"`
	Choices []relayLLMTestChoice `json:"choices"`
	Usage   any                  `json:"usage"`
}

type relayLLMTestMessage struct {
	Role      string `json:"role"`
	Content   string `json:"content"`
	ToolCalls any    `json:"tool_calls"`
}

type relayLLMTestCompletedChoice struct {
	Message      relayLLMTestMessage `json:"message"`
	FinishReason string              `json:"finish_reason"`
}

type relayLLMTestCompleted struct {
	Model   string                        `json:"model"`
	Choices []relayLLMTestCompletedChoice `json:"choices"`
	Usage   any                           `json:"usage"`
}

type relayLLMTestRaw struct {
	Model   string `json:"model"`
	Content string `json:"content"`
}

type relayLLMTestBase struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	Role       string         `json:"role"`
	Model      string         `json:"model"`
	Content    []any          `json:"content"`
	StopReason any            `json:"stop_reason"`
	Usage      map[string]any `json:"usage"`
}

type relayLLMTestOpaque struct{ hidden int }

func (relayLLMTestOpaque) String() string { return "opaque-provider-object" }

type relayLLMTestProviderError struct{ msg string }

func (e *relayLLMTestProviderError) Error() string { return e.msg }

func relayLLMCompletedResponse(content string) *relayLLMTestCompleted {
	return &relayLLMTestCompleted{Model: "test-model", Choices: []relayLLMTestCompletedChoice{{
		Message: relayLLMTestMessage{Role: "assistant", Content: content}, FinishReason: "stop",
	}}}
}

// Attribute check standing in for hasattr choices on a provider object
func relayLLMChoicesPredicate(value any) bool {
	_, ok := value.(*relayLLMTestCompleted)
	return ok
}

func relayLLMSID(s string) *string { return &s }

// Relay double without codecs so every protocol resolves to no codec
func relayLLMNoCodec(f *relayLLMFake) {
	f.relay.Codecs = nil
}

func relayLLMFinal(v any) func(context.Context) (any, error) {
	return func(context.Context) (any, error) { return v, nil }
}

func relayLLMDict(context.Context) (any, error) { return map[string]any{}, nil }

func relayLLMFactory(v any) func(context.Context, map[string]any) (any, error) {
	return func(context.Context, map[string]any) (any, error) { return v, nil }
}

func relayLLMEqual(t *testing.T, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func relayLLMRuntime(turn *tools.RelayTurnContext) *tools.RelayRuntime {
	return turn.Lease.Host.(*tools.RelayRuntime)
}

// Replaces the binding stream_execute with a generator body driving the provider stream
func relayLLMStreamBody(f *relayLLMFake, body func(ctx context.Context, upstream func(context.Context) (*tools.RelayAsyncIterator, error), observe func(context.Context, any) error, finalizer func(context.Context) (any, error), yield func(any) bool) error) {
	f.relay.LLM.StreamExecute = func(_ context.Context, _ string, request any, provider tools.RelayProviderStreamCallback, observe func(context.Context, any) error, finalizer func(context.Context) (any, error), _ map[string]any) (any, error) {
		gen := relayLLMTestGen(func(ctx context.Context, yield func(any) bool) error {
			return body(ctx, func(c context.Context) (*tools.RelayAsyncIterator, error) { return provider(c, request) }, observe, finalizer, yield)
		})
		return tools.RelayAwaitable(func(context.Context) (any, error) { return gen, nil }), nil
	}
}

// Wraps the binding stream so its aclose never finishes until the test ends
func relayLLMWedgeAclose(t *testing.T, f *relayLLMFake) {
	inner := f.relay.LLM.StreamExecute
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	f.relay.LLM.StreamExecute = func(ctx context.Context, op string, request any, provider tools.RelayProviderStreamCallback, observe func(context.Context, any) error, finalizer func(context.Context) (any, error), kwargs map[string]any) (any, error) {
		v, err := inner(ctx, op, request, provider, observe, finalizer, kwargs)
		if err != nil {
			return nil, err
		}
		opened, err := v.(tools.RelayAwaitable)(ctx)
		if err != nil {
			return nil, err
		}
		it := opened.(*tools.RelayAsyncIterator)
		wedged := &tools.RelayAsyncIterator{Next: it.Next, Aclose: func(c context.Context) error {
			<-stop
			return it.Aclose(c)
		}}
		return tools.RelayAwaitable(func(context.Context) (any, error) { return wedged, nil }), nil
	}
	prev := relayAcloseTimeout
	relayAcloseTimeout = 0.2
	t.Cleanup(func() { relayAcloseTimeout = prev })
}

// Provider accepting only the payload keyword like a strict native transport
func relayLLMStrict(observed *[]any, result func(payload any) any) func(context.Context, map[string]any) (any, error) {
	return func(_ context.Context, request map[string]any) (any, error) {
		for k := range request {
			if k != "payload" {
				return nil, fmt.Errorf("strict_transport() got an unexpected keyword argument '%s'", k)
			}
		}
		*observed = append(*observed, request["payload"])
		return result(request["payload"]), nil
	}
}

func TestRelayRequestBodyOmitsClientTimeout(t *testing.T) {
	for _, mode := range []string{"chat_completions", "codex_responses", "anthropic_messages"} {
		request := map[string]any{"model": "test-model", "timeout": 1800.0}
		body := relayRequestBody(request, map[string]any{"api_mode": mode})
		if _, ok := body["timeout"]; ok {
			t.Fatalf("%s body kept timeout", mode)
		}
		relayLLMEqual(t, request["timeout"], 1800.0)
	}
}

func TestRelayUninterceptedProviderCallbackPreservesClientTimeout(t *testing.T) {
	f, ctx, _ := relayLLMTurn(t)
	var relayRequests []any
	var providerRequests []map[string]any
	original := f.relay.LLM.Execute
	f.relay.LLM.Execute = func(c context.Context, name string, request any, cb tools.RelayProviderCallback, kw map[string]any) (any, error) {
		relayRequests = append(relayRequests, request.(*tools.RelayLLMRequest).Content)
		return original(c, name, request, cb, kw)
	}
	relayLLMNoCodec(f)
	result, err := RelayExecute(ctx, map[string]any{"model": "test-model", "messages": []any{}, "timeout": 1800.0},
		func(_ context.Context, request map[string]any) (any, error) {
			providerRequests = append(providerRequests, request)
			return map[string]any{"content": "done"}, nil
		}, RelayExecuteOptions{Name: "custom", ModelName: "test-model", SessionID: relayLLMSID("session-1"), Metadata: map[string]any{"api_mode": "chat_completions"}})
	if err != nil {
		t.Fatal(err)
	}
	relayLLMEqual(t, result, map[string]any{"content": "done"})
	relayLLMEqual(t, relayRequests, []any{map[string]any{"model": "test-model", "messages": []any{}}})
	relayLLMEqual(t, providerRequests[0]["timeout"], 1800.0)
}

func relayLLMCaptureExecuteNames(f *relayLLMFake) *[]string {
	names := &[]string{}
	original := f.relay.LLM.Execute
	f.relay.LLM.Execute = func(c context.Context, name string, request any, cb tools.RelayProviderCallback, kw map[string]any) (any, error) {
		*names = append(*names, name)
		return original(c, name, request, cb, kw)
	}
	return names
}

func TestRelaySyncExecutionUsesCanonicalRelayOperationName(t *testing.T) {
	f, ctx, _ := relayLLMTurn(t)
	names := relayLLMCaptureExecuteNames(f)
	relayLLMNoCodec(f)
	result, err := RelayExecute(ctx, map[string]any{"model": "test-model", "messages": []any{}}, relayLLMFactory(map[string]any{"content": "done"}),
		RelayExecuteOptions{Name: "custom", ModelName: "test-model", SessionID: relayLLMSID("session-1"), Metadata: map[string]any{"api_mode": "chat_completions"}})
	if err != nil {
		t.Fatal(err)
	}
	relayLLMEqual(t, result, map[string]any{"content": "done"})
	relayLLMEqual(t, *names, []string{"openai.chat_completions"})
}

func TestRelayAsyncExecutionUsesCanonicalRelayOperationName(t *testing.T) {
	f, ctx, _ := relayLLMTurn(t)
	names := relayLLMCaptureExecuteNames(f)
	relayLLMNoCodec(f)
	provider := func(context.Context, map[string]any) (any, error) {
		return tools.RelayAwaitable(func(context.Context) (any, error) { return map[string]any{"content": "done"}, nil }), nil
	}
	result, err := RelayExecuteAsync(ctx, map[string]any{"model": "test-model", "input": "hello"}, provider,
		RelayExecuteOptions{Name: "custom", ModelName: "test-model", SessionID: relayLLMSID("session-1"), Metadata: map[string]any{"api_mode": "codex_responses"}})
	if err != nil {
		t.Fatal(err)
	}
	relayLLMEqual(t, result, map[string]any{"content": "done"})
	relayLLMEqual(t, *names, []string{"openai.responses"})
}

func TestRelayStreamExecutionUsesCanonicalRelayOperationName(t *testing.T) {
	f, ctx, _ := relayLLMTurn(t)
	var names []string
	original := f.relay.LLM.StreamExecute
	f.relay.LLM.StreamExecute = func(c context.Context, name string, request any, provider tools.RelayProviderStreamCallback, observe func(context.Context, any) error, finalizer func(context.Context) (any, error), kw map[string]any) (any, error) {
		names = append(names, name)
		return original(c, name, request, provider, observe, finalizer, kw)
	}
	relayLLMNoCodec(f)
	s, err := NewManagedLlmStream(ctx, map[string]any{"model": "test-model", "messages": []any{}}, relayLLMFactory([]any{map[string]any{"delta": "done"}}),
		ManagedLlmStreamOptions{SessionID: "session-1", Name: "custom", ModelName: "test-model", Finalizer: relayLLMFinal(map[string]any{"content": "done"}),
			Metadata: map[string]any{"api_mode": "anthropic_messages"}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	relayLLMEqual(t, relayLLMDrain(t, ctx, s), []any{map[string]any{"delta": "done"}})
	relayLLMEqual(t, names, []string{"anthropic.messages"})
}

func TestRelayProtocolDrivesOperationAndCodec(t *testing.T) {
	for _, tc := range []struct{ mode, operation, class string }{
		{"chat_completions", "openai.chat_completions", "OpenAIChatCodec"},
		{"codex_responses", "openai.responses", "OpenAIResponsesCodec"},
		{"anthropic_messages", "anthropic.messages", "AnthropicMessagesCodec"},
	} {
		want := &tools.NemoRelayCodec{}
		ctor := func() (*tools.NemoRelayCodec, error) { return want, nil }
		codecs := &tools.NemoRelayCodecs{}
		reflect.ValueOf(codecs).Elem().FieldByName(tc.class).Set(reflect.ValueOf(ctor))
		relay := &tools.NemoRelay{Codecs: codecs}
		metadata := map[string]any{"api_mode": tc.mode}
		relayLLMEqual(t, relayOperationName("custom-provider", metadata), tc.operation)
		got, err := relayCodec(relay, metadata)
		if err != nil || got != want {
			t.Fatalf("%s codec = %p, %v, want %p", tc.mode, got, err, want)
		}
	}
}

func relayLLMWithContent(body map[string]any, extra map[string]any) map[string]any {
	out := relayCopyMap(body)
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func TestRelayProviderRequestOverlaysInterceptorAddedCodexField(t *testing.T) {
	original := map[string]any{"model": "gpt-5.6-sol", "input": "hello"}
	metadata := map[string]any{"api_mode": "codex_responses"}
	body := relayRequestBody(original, metadata)
	intercepted := &tools.RelayLLMRequest{Content: relayLLMWithContent(body, map[string]any{"prompt_cache_retention": "24h"}), Headers: map[string]any{}}
	request := relayProviderRequest(original, intercepted, body, relayCopyMap(body), metadata)
	if _, ok := original["prompt_cache_retention"]; ok {
		t.Fatal("original mutated")
	}
	relayLLMEqual(t, request["prompt_cache_retention"], "24h")
}

func TestRelayProviderRequestOverlaysInterceptorAddedExtraBody(t *testing.T) {
	original := map[string]any{"model": "gpt-5.6-sol", "input": "hello"}
	metadata := map[string]any{"api_mode": "codex_responses"}
	body := relayRequestBody(original, metadata)
	intercepted := &tools.RelayLLMRequest{Content: relayLLMWithContent(body, map[string]any{"extra_body": map[string]any{"prompt_cache_retention": "24h"}}), Headers: map[string]any{}}
	request := relayProviderRequest(original, intercepted, body, relayCopyMap(body), metadata)
	if _, ok := original["extra_body"]; ok {
		t.Fatal("original mutated")
	}
	relayLLMEqual(t, request["extra_body"], map[string]any{"prompt_cache_retention": "24h"})
}

const relayLLMTraceparent = "00-11111111111111111111111111111111-2222222222222222-01"

func TestRelayProviderRequestMapsHeadersForSupportedSDKModes(t *testing.T) {
	for _, mode := range []string{"chat_completions", "codex_responses", "anthropic_messages"} {
		original := map[string]any{"model": "test-model"}
		metadata := map[string]any{"api_mode": mode}
		body := relayRequestBody(original, metadata)
		request := relayProviderRequest(original, &tools.RelayLLMRequest{Content: body, Headers: map[string]any{"traceparent": relayLLMTraceparent}}, body, relayCopyMap(body), metadata)
		relayLLMEqual(t, request["extra_headers"], map[string]any{"traceparent": relayLLMTraceparent})
	}
}

func TestRelayProviderRequestPreservesCustomHeadersForNativeTransport(t *testing.T) {
	original := map[string]any{"payload": "provider-native"}
	request := relayProviderRequest(original, &tools.RelayLLMRequest{Content: original, Headers: map[string]any{
		"traceparent": relayLLMTraceparent, "x-custom-route": "private",
	}}, original, relayCopyMap(original), map[string]any{"api_mode": "strict_native"})
	relayLLMEqual(t, request["extra_headers"], map[string]any{"x-custom-route": "private"})
}

func TestRelayProviderRequestTracesCustomTransportWithHeaderCapability(t *testing.T) {
	original := map[string]any{"payload": "provider-native", "extra_headers": map[string]any{"authorization": "Bearer provider-token"}}
	request := relayProviderRequest(original, &tools.RelayLLMRequest{Content: original, Headers: map[string]any{"traceparent": relayLLMTraceparent}},
		original, relayCopyMap(original), map[string]any{"api_mode": "custom"})
	relayLLMEqual(t, request["extra_headers"], map[string]any{"authorization": "Bearer provider-token", "traceparent": relayLLMTraceparent})
}

func TestRelayManagedRequestDoesNotAddSDKHeadersToStrictCallback(t *testing.T) {
	_, ctx, _ := relayLLMTurn(t)
	var observed []any
	result, err := RelayExecute(ctx, map[string]any{"payload": "provider-native"}, relayLLMStrict(&observed, func(p any) any { return map[string]any{"content": p} }),
		RelayExecuteOptions{Name: "strict-native", ModelName: "strict-model", SessionID: relayLLMSID("session-1"),
			Metadata: map[string]any{"api_mode": "bedrock_converse", "api_request_id": "strict-native-request"}})
	if err != nil {
		t.Fatal(err)
	}
	relayLLMEqual(t, observed, []any{"provider-native"})
	relayLLMEqual(t, result, map[string]any{"content": "provider-native"})
}

func TestRelayManagedStreamDoesNotAddSDKHeadersToStrictCallback(t *testing.T) {
	_, ctx, _ := relayLLMTurn(t)
	var observed []any
	chunks := []any{map[string]any{"delta": "provider-native"}}
	s, err := NewManagedLlmStream(ctx, map[string]any{"payload": "provider-native"}, relayLLMStrict(&observed, func(any) any { return chunks }),
		ManagedLlmStreamOptions{SessionID: "session-1", Name: "strict-native", ModelName: "strict-model", Finalizer: relayLLMFinal(map[string]any{"content": "provider-native"}),
			Metadata: map[string]any{"api_mode": "bedrock_converse", "api_request_id": "strict-native-stream"}})
	if err != nil {
		t.Fatal(err)
	}
	relayLLMEqual(t, relayLLMDrain(t, ctx, s), chunks)
	relayLLMEqual(t, observed, []any{"provider-native"})
}

func TestRelayManagedStreamDeliversEachChunkBeforeTheProviderSendsTheNext(t *testing.T) {
	_, ctx, _ := relayLLMTurn(t)
	release := make(chan struct{})
	raw := &relayLLMRawStream{chunks: []any{map[string]any{"delta": "first"}, map[string]any{"delta": "second"}}, onNext: func(i int) error {
		if i != 1 {
			return nil
		}
		select {
		case <-release:
			return nil
		case <-time.After(10 * time.Second):
			return errors.New("first chunk never reached the consumer during the provider pause")
		}
	}}
	s, err := NewManagedLlmStream(ctx, map[string]any{"payload": "paused"}, relayLLMFactory(raw),
		ManagedLlmStreamOptions{SessionID: "session-1", Name: "paused-native", ModelName: "paused-model", Finalizer: relayLLMFinal(map[string]any{"content": "first second"}),
			Metadata: map[string]any{"api_mode": "bedrock_converse", "api_request_id": "paused-stream"}})
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	relayLLMEqual(t, first, map[string]any{"delta": "first"})
	close(release)
	relayLLMEqual(t, relayLLMDrain(t, ctx, s), []any{map[string]any{"delta": "second"}})
}

func TestRelayStreamUsesRewrittenRequestAndPostInterceptChunks(t *testing.T) {
	f, ctx, turn := relayLLMTurn(t)
	var captured []map[string]any
	f.registerLLMRequest("icebath-test-request", 1, func(_ string, request *tools.RelayLLMRequest, annotated any) (*tools.RelayLLMRequest, any) {
		content := relayLLMWithContent(request.Content.(map[string]any), map[string]any{"temperature": 0.25})
		return &tools.RelayLLMRequest{Headers: request.Headers, Content: content}, annotated
	})
	f.registerLLMStreamExecution("icebath-test-stream", 1, func(_ context.Context, name string, request *tools.RelayLLMRequest, sctx relayLLMFakeStreamContext, next relayLLMFakeNext) (*tools.RelayAsyncIterator, error) {
		if name != "test-provider" || sctx.responseCodec != nil {
			t.Errorf("stream intercept saw %q with codec %v", name, sctx.responseCodec)
		}
		return relayLLMTestGen(func(ctx context.Context, yield func(any) bool) error {
			upstream, err := next(ctx, request)
			if err != nil {
				return err
			}
			for {
				chunk, err := upstream.Next(ctx)
				if err == io.EOF {
					return nil
				}
				if err != nil {
					return err
				}
				updated := relayCopyMap(chunk.(map[string]any))
				var choices []any
				for _, c := range updated["choices"].([]any) {
					choices = append(choices, relayCopyMap(c.(map[string]any)))
				}
				if len(choices) > 0 {
					first := choices[0].(map[string]any)
					delta, _ := relayDictCopy(first["delta"])
					if delta == nil {
						delta = map[string]any{}
					}
					if text, ok := delta["content"].(string); ok && text != "" {
						delta["content"] = strings.ToUpper(text)
					}
					first["delta"] = delta
					updated["choices"] = choices
				}
				if !yield(updated) {
					return nil
				}
			}
		}), nil
	})
	defer f.deregister("icebath-test-stream")
	defer f.deregister("icebath-test-request")
	rawChunks := []any{
		relayLLMTestChunk{Model: "test-model", Choices: []relayLLMTestChoice{{Delta: relayLLMTestDelta{Content: "hello"}}}},
		relayLLMTestChunk{Model: "test-model", Choices: []relayLLMTestChoice{{FinishReason: "stop"}}},
	}
	s, err := NewManagedLlmStream(ctx, map[string]any{"model": "test-model", "messages": []any{}, "extra_headers": map[string]any{"authorization": "Bearer provider-token"}},
		func(_ context.Context, request map[string]any) (any, error) {
			captured = append(captured, request)
			return rawChunks, nil
		}, ManagedLlmStreamOptions{SessionID: "session-1", Name: "test-provider", ModelName: "test-model",
			Finalizer: relayLLMFinal(map[string]any{"model": "test-model", "choices": []any{map[string]any{
				"message": map[string]any{"role": "assistant", "content": "HELLO"}, "finish_reason": "stop"}}}),
			Metadata: map[string]any{"api_mode": "custom", "api_request_id": "request-1", "call_role": "primary"}})
	if err != nil {
		t.Fatal(err)
	}
	chunks := relayLLMDrain(t, ctx, s)
	relayLLMEqual(t, captured[0]["temperature"], 0.25)
	headers := captured[0]["extra_headers"].(map[string]any)
	relayLLMEqual(t, headers["authorization"], "Bearer provider-token")
	parts := strings.Split(headers["traceparent"].(string), "-")
	if len(parts) != 4 || parts[0] != "00" || len(parts[1]) != 32 || len(parts[2]) != 16 || parts[3] != "01" {
		t.Fatalf("traceparent %v", parts)
	}
	for _, hex := range parts[1:3] {
		if _, err := strconv.ParseUint(hex[:16], 16, 64); err != nil {
			t.Fatal(err)
		}
	}
	first := chunks[0].(map[string]any)["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	relayLLMEqual(t, first["content"], "HELLO")
	relayLLMEqual(t, chunks[1], rawChunks[1])
	if !s.OutputModified {
		t.Fatal("output_modified unset")
	}
	relayLLMEqual(t, relayLLMLogicalCalls(turn), map[string]any{})
}

func TestRelayLiveStreamDefersRuntimeShutdownUntilExhaustion(t *testing.T) {
	relayLLMInstallFake(t, "stream-shutdown-profile")
	ctx := pyctx.With(context.Background(), pyctx.New())
	host := tools.GetRelayRuntime(ctx, true, tools.CurrentRelayProfileKey(ctx))
	if host == nil {
		t.Fatal("no runtime")
	}
	if err := host.RetainManagedExecution("test.live-stream"); err != nil {
		t.Fatal(err)
	}
	defer host.ReleaseManagedExecution("test.live-stream")
	if session, err := host.EnsureSession(ctx, map[string]any{"session_id": "stream-shutdown"}, tools.RelaySessionOptions{}); err != nil || session == nil {
		t.Fatalf("ensure_session = %v, %v", session, err)
	}
	chunks := []any{map[string]any{"delta": "first"}, map[string]any{"delta": "second"}}
	s, err := NewManagedLlmStream(ctx, map[string]any{"model": "test-model", "messages": []any{}}, relayLLMFactory(chunks),
		ManagedLlmStreamOptions{SessionID: "stream-shutdown", Name: "test-provider", ModelName: "test-model", Finalizer: relayLLMFinal(map[string]any{"content": "complete"}),
			Metadata: map[string]any{"api_mode": "custom"}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	host.Shutdown()
	if host.ShutdownComplete() {
		t.Fatal("shutdown finished under a live stream")
	}
	relayLLMEqual(t, relayLLMDrain(t, ctx, s), chunks)
	deadline := time.Now().Add(5 * time.Second)
	for !host.ShutdownComplete() {
		if time.Now().After(deadline) {
			t.Fatal("shutdown never completed after exhaustion")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRelayAnthropicStreamAccumulatorMergesPlainProviderObject(t *testing.T) {
	a := NewAnthropicStreamAccumulator()
	for _, event := range []map[string]any{
		{"type": "message_start", "message": map[string]any{"id": "message-1", "type": "message", "role": "assistant", "model": "claude-test", "usage": map[string]any{"input_tokens": 10}}},
		{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": "hello"}},
	} {
		if err := a.Observe(event); err != nil {
			t.Fatal(err)
		}
	}
	response := a.Response(&relayLLMTestBase{ID: "message-1", Type: "message", Role: "assistant", Model: "claude-test", Content: []any{}, Usage: map[string]any{"input_tokens": 10}}).(map[string]any)
	relayLLMEqual(t, response["id"], "message-1")
	relayLLMEqual(t, response["content"].([]any)[0].(map[string]any)["text"], "hello")
	relayLLMEqual(t, response["usage"].(map[string]any)["input_tokens"], 10)
}

func TestRelayAnthropicStreamAccumulatorNullDeltaUsageKeepsMessageStartCounts(t *testing.T) {
	a := NewAnthropicStreamAccumulator()
	_ = a.Observe(map[string]any{"type": "message_start", "message": map[string]any{"id": "message-1", "type": "message", "role": "assistant", "model": "claude-test",
		"usage": map[string]any{"input_tokens": 12, "cache_read_input_tokens": 3000, "cache_creation_input_tokens": 200, "output_tokens": 1}}})
	_ = a.Observe(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"},
		"usage": map[string]any{"output_tokens": 42, "input_tokens": nil, "cache_read_input_tokens": nil, "cache_creation_input_tokens": nil}})
	relayLLMEqual(t, a.Finalize()["usage"], map[string]any{"input_tokens": 12, "cache_read_input_tokens": 3000, "cache_creation_input_tokens": 200, "output_tokens": 42})
}

func TestRelayJSONableDoesNotProbeDynamicAttributes(t *testing.T) {
	relayLLMEqual(t, tools.RelayJSONable(&relayLLMTestOpaque{}), "opaque-provider-object")
}

func TestRelayAsyncProviderCallbackPreservesCallerContext(t *testing.T) {
	_, ctx, _ := relayLLMTurn(t)
	callerValue := pyctx.NewVar("async_llm_caller_value", "default")
	callerValue.Set(ctx, "caller")
	provider := func(context.Context, map[string]any) (any, error) {
		return tools.RelayAwaitable(func(c context.Context) (any, error) {
			time.Sleep(0)
			return map[string]any{"caller_value": callerValue.Get(c)}, nil
		}), nil
	}
	result, err := RelayExecuteAsync(ctx, map[string]any{"model": "test-model", "messages": []any{}}, provider,
		RelayExecuteOptions{Name: "test-provider", ModelName: "test-model", SessionID: relayLLMSID("session-1"),
			Metadata: map[string]any{"api_mode": "custom", "api_request_id": "request-async-context"}})
	if err != nil {
		t.Fatal(err)
	}
	relayLLMEqual(t, result, map[string]any{"caller_value": "caller"})
}

func TestRelayAnthropicStreamCallbacksDoNotReenterCapturedContext(t *testing.T) {
	_, ctx, _ := relayLLMTurn(t)
	callerValue := pyctx.NewVar("anthropic_stream_caller_value", "default")
	callerValue.Set(ctx, "caller")
	callerContext := pyctx.Current(ctx)
	var observed []any
	var contexts []*pyctx.Context
	a := NewAnthropicStreamAccumulator()
	chunks := []any{
		map[string]any{"type": "message_start", "message": map[string]any{"id": "message-1", "type": "message", "role": "assistant", "model": "claude-test",
			"usage": map[string]any{"input_tokens": 1, "output_tokens": 0}}},
		map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": map[string]any{"output_tokens": 1}},
	}
	s, err := NewManagedLlmStream(ctx, map[string]any{"model": "claude-test", "max_tokens": 16, "messages": []any{map[string]any{"role": "user", "content": "hi"}}},
		relayLLMFactory(chunks), ManagedLlmStreamOptions{SessionID: "session-1", Name: "anthropic", ModelName: "claude-test",
			Finalizer: func(context.Context) (any, error) { return a.Finalize(), nil },
			OnChunk: func(c context.Context, chunk any) error {
				observed = append(observed, callerValue.Get(c))
				contexts = append(contexts, pyctx.Current(c))
				return a.Observe(chunk)
			},
			Metadata: map[string]any{"api_mode": "anthropic_messages", "api_request_id": "request-anthropic-context-reentry"}})
	if err != nil {
		t.Fatal(err)
	}
	entered, release, held := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(held)
		holder := pyctx.With(context.Background(), callerContext)
		callerValue.Set(holder, "caller")
		close(entered)
		<-release
	}()
	<-entered
	got := relayLLMDrain(t, ctx, s)
	close(release)
	<-held
	relayLLMEqual(t, got, chunks)
	relayLLMEqual(t, observed, []any{"caller", "caller"})
	if len(contexts) != 2 || contexts[0] == contexts[1] || contexts[0] == callerContext || contexts[1] == callerContext {
		t.Fatal("callbacks did not each run in a fresh copy of the captured context")
	}
}

func TestRelayExplicitStreamCloseSurfacesProviderCloseFailure(t *testing.T) {
	_, ctx, _ := relayLLMTurn(t)
	raw := &relayLLMRawStream{chunks: []any{map[string]any{"delta": "partial"}}, closeErr: errors.New("provider close failed")}
	s, err := NewManagedLlmStream(ctx, map[string]any{"model": "test-model", "messages": []any{}}, relayLLMFactory(raw),
		ManagedLlmStreamOptions{SessionID: "session-1", Name: "test-provider", ModelName: "test-model", Finalizer: relayLLMFinal(map[string]any{"content": "partial"}),
			Metadata: map[string]any{"api_mode": "custom", "api_request_id": "request-close-failure"}})
	if err != nil {
		t.Fatal(err)
	}
	chunk, err := s.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	relayLLMEqual(t, chunk, map[string]any{"delta": "partial"})
	if err := s.Close(); err == nil || err.Error() != "provider close failed" {
		t.Fatalf("close = %v", err)
	}
	relayLLMEqual(t, raw.closeCalls, 1)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRelayNonStreamDefersLogicalSuccessAndReusesScopeForRetry(t *testing.T) {
	_, ctx, turn := relayLLMTurn(t)
	o := RelayExecuteOptions{Name: "test-provider", ModelName: "test-model", SessionID: relayLLMSID("session-1"),
		Metadata: map[string]any{"api_mode": "custom", "api_request_id": "request-retry"}, DeferLogicalCompletion: true}
	first, err := RelayExecute(ctx, map[string]any{"model": "test-model", "messages": []any{}}, relayLLMFactory(map[string]any{"content": "invalid"}), o)
	if err != nil {
		t.Fatal(err)
	}
	firstHandle := relayLLMLogicalCalls(turn)["request-retry"]
	second, err := RelayExecute(ctx, map[string]any{"model": "test-model", "messages": []any{}}, relayLLMFactory(map[string]any{"content": "valid"}), o)
	if err != nil {
		t.Fatal(err)
	}
	relayLLMEqual(t, first, map[string]any{"content": "invalid"})
	relayLLMEqual(t, second, map[string]any{"content": "valid"})
	relayLLMEqual(t, relayLLMLogicalCalls(turn), map[string]any{"request-retry": firstHandle})
	if err := RelayCompleteLogicalCall(ctx, "request-retry", RelayLogicalCompletion{Outcome: "success"}); err != nil {
		t.Fatal(err)
	}
	relayLLMEqual(t, relayLLMLogicalCalls(turn), map[string]any{})
}

func TestRelayLogicalCloseSkipsPopUnderConcurrentTurnScope(t *testing.T) {
	f, ctx, turn := relayLLMTurn(t)
	if _, err := RelayExecute(ctx, map[string]any{"model": "test-model", "messages": []any{}}, relayLLMFactory(map[string]any{"content": "valid"}),
		RelayExecuteOptions{Name: "test-provider", ModelName: "test-model", SessionID: relayLLMSID("session-1"),
			Metadata: map[string]any{"api_mode": "custom", "api_request_id": "request-overlap"}, DeferLogicalCompletion: true}); err != nil {
		t.Fatal(err)
	}
	own := relayLLMLogicalCalls(turn)["request-overlap"]
	host, session := relayLLMRuntime(turn), turn.Lease.Session
	sibling, err := host.RunInSession(ctx, session, func(c context.Context) (any, error) {
		return f.relay.Scope.Push(c, tools.RelayLogicalLLMScope, f.relay.ScopeType.Function, map[string]any{"handle": nil, "input": map[string]any{}})
	}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !tools.RelayIdentical(f.top(), sibling) {
		t.Fatal("sibling must be on top")
	}
	if err := RelayCompleteLogicalCall(ctx, "request-overlap", RelayLogicalCompletion{Outcome: "success"}); err != nil {
		t.Fatal(err)
	}
	relayLLMEqual(t, relayLLMLogicalCalls(turn), map[string]any{})
	if !tools.RelayIdentical(f.top(), sibling) {
		t.Fatal("the close popped through the sibling scope")
	}
	if _, err := host.RunInSession(ctx, session, func(c context.Context) (any, error) { return f.relay.Scope.Pop(c, sibling, map[string]any{}) }, false, nil); err != nil {
		t.Fatal(err)
	}
	if !tools.RelayIdentical(f.top(), own) {
		t.Fatal("the skipped scope stays on the stack and is reclaimed by the session-close drain")
	}
}

func TestRelayNonStreamResultSurvivesLogicalScopeCloseFailure(t *testing.T) {
	f, ctx, turn := relayLLMTurn(t)
	original := f.relay.Scope.Pop
	popCalls := 0
	f.relay.Scope.Pop = func(c context.Context, handle any, kwargs map[string]any) (any, error) {
		popCalls++
		if popCalls == 1 {
			return nil, &tools.RelayError{Class: "RuntimeError", Message: "simulated logical scope close failure"}
		}
		return original(c, handle, kwargs)
	}
	raw := &relayLLMTestRaw{Model: "test-model", Content: "raw"}
	result, err := RelayExecute(ctx, map[string]any{"model": "test-model", "messages": []any{}}, relayLLMFactory(raw),
		RelayExecuteOptions{Name: "test-provider", ModelName: "test-model", SessionID: relayLLMSID("session-1"),
			Metadata: map[string]any{"api_mode": "custom", "api_request_id": "request-close"}})
	if err != nil {
		t.Fatal(err)
	}
	if result != any(raw) {
		t.Fatalf("result %#v is not the provider response", result)
	}
	if _, ok := relayLLMLogicalCalls(turn)["request-close"]; !ok {
		t.Fatal("handle dropped after a failed close")
	}
	tools.RelayCoordinator.EndTurn(ctx, turn, "success")
	relayLLMEqual(t, relayLLMLogicalCalls(turn), map[string]any{})
}

func TestRelayStreamFlushesBufferedProviderChunksAfterRelayFailure(t *testing.T) {
	f, ctx, turn := relayLLMTurn(t)
	rawChunks := []any{map[string]any{"delta": "first"}, map[string]any{"delta": "second"}}
	relayLLMStreamBody(f, func(c context.Context, upstream func(context.Context) (*tools.RelayAsyncIterator, error), observe func(context.Context, any) error, finalizer func(context.Context) (any, error), yield func(any) bool) error {
		up, _ := upstream(c)
		first, _ := up.Next(c)
		_ = observe(c, first)
		if !yield(first) {
			return nil
		}
		second, _ := up.Next(c)
		_ = observe(c, second)
		if _, err := up.Next(c); err != io.EOF {
			t.Errorf("provider stream not exhausted: %v", err)
		}
		_, _ = finalizer(c)
		return errors.New("simulated buffered Relay failure")
	})
	s, err := NewManagedLlmStream(ctx, map[string]any{"model": "test-model", "messages": []any{}}, relayLLMFactory(rawChunks),
		ManagedLlmStreamOptions{SessionID: "session-1", Name: "test-provider", ModelName: "test-model", Finalizer: relayLLMFinal(map[string]any{"content": "complete"}),
			Metadata: map[string]any{"api_mode": "custom", "api_request_id": "request-buffered-failure"}})
	if err != nil {
		t.Fatal(err)
	}
	relayLLMEqual(t, relayLLMDrain(t, ctx, s), rawChunks)
	relayLLMEqual(t, relayLLMLogicalCalls(turn), map[string]any{})
}

func TestRelayWedgedRelayAcloseDoesNotBlockStreamClose(t *testing.T) {
	f, ctx, turn := relayLLMTurn(t)
	rawChunks := []any{map[string]any{"delta": "first"}, map[string]any{"delta": "second"}}
	relayLLMStreamBody(f, func(c context.Context, upstream func(context.Context) (*tools.RelayAsyncIterator, error), _ func(context.Context, any) error, _ func(context.Context) (any, error), yield func(any) bool) error {
		up, _ := upstream(c)
		for i := 0; i < 2; i++ {
			chunk, err := up.Next(c)
			if err != nil {
				return err
			}
			if !yield(chunk) {
				return nil
			}
		}
		return nil
	})
	relayLLMWedgeAclose(t, f)
	s, err := NewManagedLlmStream(ctx, map[string]any{"model": "test-model", "messages": []any{}}, relayLLMFactory(rawChunks),
		ManagedLlmStreamOptions{SessionID: "session-1", Name: "test-provider", ModelName: "test-model", Finalizer: relayLLMFinal(map[string]any{"content": "complete"}),
			Metadata: map[string]any{"api_mode": "custom", "api_request_id": "request-wedged-aclose-close"}})
	if err != nil {
		t.Fatal(err)
	}
	chunk, err := s.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	relayLLMEqual(t, chunk, rawChunks[0])
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if s.runtimeLease != nil {
		t.Fatal("runtime lease kept after an abandoned close")
	}
	if !relayLLMRuntime(turn).WaitOperationsIdle(1.0) {
		t.Fatal("operations never went idle")
	}
}

func TestRelayWedgedRelayAcloseDoesNotBlockProviderFallback(t *testing.T) {
	f, ctx, turn := relayLLMTurn(t)
	rawChunks := []any{map[string]any{"delta": "first"}, map[string]any{"delta": "second"}}
	relayLLMStreamBody(f, func(c context.Context, upstream func(context.Context) (*tools.RelayAsyncIterator, error), _ func(context.Context, any) error, _ func(context.Context) (any, error), _ func(any) bool) error {
		up, _ := upstream(c)
		_, _ = up.Next(c)
		_, _ = up.Next(c)
		if _, err := up.Next(c); err != io.EOF {
			t.Errorf("provider stream not exhausted: %v", err)
		}
		return errors.New("simulated Relay failure after buffering")
	})
	relayLLMWedgeAclose(t, f)
	s, err := NewManagedLlmStream(ctx, map[string]any{"model": "test-model", "messages": []any{}}, relayLLMFactory(rawChunks),
		ManagedLlmStreamOptions{SessionID: "session-1", Name: "test-provider", ModelName: "test-model", Finalizer: relayLLMFinal(map[string]any{"content": "complete"}),
			Metadata: map[string]any{"api_mode": "custom", "api_request_id": "request-wedged-aclose"}})
	if err != nil {
		t.Fatal(err)
	}
	relayLLMEqual(t, relayLLMDrain(t, ctx, s), rawChunks)
	relayLLMEqual(t, relayLLMLogicalCalls(turn), map[string]any{})
	if s.runtimeLease != nil {
		t.Fatal("runtime lease kept after the provider fallback")
	}
}

func TestRelayStreamRefusesReplayAfterTransformedRelayOutput(t *testing.T) {
	f, ctx, _ := relayLLMTurn(t)
	rawChunks := []any{map[string]any{"delta": "first"}, map[string]any{"delta": "second"}}
	relayLLMStreamBody(f, func(c context.Context, upstream func(context.Context) (*tools.RelayAsyncIterator, error), observe func(context.Context, any) error, finalizer func(context.Context) (any, error), yield func(any) bool) error {
		up, _ := upstream(c)
		_, _ = up.Next(c)
		transformed := map[string]any{"delta": "first (rewritten)"}
		_ = observe(c, transformed)
		if !yield(transformed) {
			return nil
		}
		_, _ = up.Next(c)
		if _, err := up.Next(c); err != io.EOF {
			t.Errorf("provider stream not exhausted: %v", err)
		}
		_, _ = finalizer(c)
		return errors.New("simulated buffered Relay failure")
	})
	s, err := NewManagedLlmStream(ctx, map[string]any{"model": "test-model", "messages": []any{}}, relayLLMFactory(rawChunks),
		ManagedLlmStreamOptions{SessionID: "session-1", Name: "test-provider", ModelName: "test-model", Finalizer: relayLLMFinal(map[string]any{"content": "complete"}),
			Metadata: map[string]any{"api_mode": "custom", "api_request_id": "request-transformed-failure"}})
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	relayLLMEqual(t, first.(map[string]any)["delta"], "first (rewritten)")
	if _, err := s.Next(ctx); err == nil || err.Error() != "simulated buffered Relay failure" {
		t.Fatalf("second next = %v", err)
	}
}

func TestRelayStreamDoesNotReplayChunksRelayPassedOver(t *testing.T) {
	f, ctx, _ := relayLLMTurn(t)
	rawChunks := []any{map[string]any{"delta": "first"}, map[string]any{"delta": "second"}}
	relayLLMStreamBody(f, func(c context.Context, upstream func(context.Context) (*tools.RelayAsyncIterator, error), observe func(context.Context, any) error, finalizer func(context.Context) (any, error), yield func(any) bool) error {
		up, _ := upstream(c)
		_, _ = up.Next(c)
		second, _ := up.Next(c)
		_ = observe(c, second)
		if !yield(second) {
			return nil
		}
		if _, err := up.Next(c); err != io.EOF {
			t.Errorf("provider stream not exhausted: %v", err)
		}
		_, _ = finalizer(c)
		return errors.New("simulated buffered Relay failure")
	})
	s, err := NewManagedLlmStream(ctx, map[string]any{"model": "test-model", "messages": []any{}}, relayLLMFactory(rawChunks),
		ManagedLlmStreamOptions{SessionID: "session-1", Name: "test-provider", ModelName: "test-model", Finalizer: relayLLMFinal(map[string]any{"content": "complete"}),
			Metadata: map[string]any{"api_mode": "custom", "api_request_id": "request-reordered-failure"}})
	if err != nil {
		t.Fatal(err)
	}
	relayLLMEqual(t, relayLLMDrain(t, ctx, s), []any{map[string]any{"delta": "second"}})
}

func TestRelayBypassedStreamStillHonorsChunkAcceptance(t *testing.T) {
	_, ctx, turn := relayLLMTurn(t)
	turn.Lease.Host.ReleaseManagedExecution("test.relay_llm")
	raw := &relayLLMRawStream{chunks: []any{map[string]any{"delta": "accepted"}, map[string]any{"delta": "rejected"}, map[string]any{"delta": "unreachable"}}}
	s, err := NewManagedLlmStream(ctx, map[string]any{"model": "test-model", "messages": []any{}}, relayLLMFactory(raw),
		ManagedLlmStreamOptions{SessionID: "session-1", Name: "test-provider", ModelName: "test-model", Finalizer: relayLLMDict,
			AcceptChunk: func(chunk any) bool { return chunk.(map[string]any)["delta"] != "rejected" }})
	if err != nil {
		t.Fatal(err)
	}
	relayLLMEqual(t, relayLLMDrain(t, ctx, s), []any{map[string]any{"delta": "accepted"}})
	relayLLMEqual(t, raw.closeCalls, 1)
}

func TestRelayAnthropicCodecPreservesToolHistoryAndCachedSystemBlocks(t *testing.T) {
	_, ctx, _ := relayLLMTurn(t)
	request := map[string]any{
		"model": "claude-sonnet-4-5", "max_tokens": 512,
		"system": []any{map[string]any{"type": "text", "text": "You are Icebath.", "cache_control": map[string]any{"type": "ephemeral"}}},
		"messages": []any{
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "Run pwd"}}},
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "toolu_01", "name": "terminal", "input": map[string]any{"command": "pwd"}}}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_01", "content": []any{map[string]any{"type": "text", "text": "/tmp/worktree"}}}}},
		},
	}
	wire := func(v any) string {
		s, err := pyjson.Dumps(v, pyjson.SortKeys(true), pyjson.EnsureASCII(false), pyjson.Separators(",", ":"))
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	originalWire := wire(request)
	observed := ""
	_, err := RelayExecute(ctx, request, func(_ context.Context, final map[string]any) (any, error) {
		body := map[string]any{}
		for k, v := range final {
			if k != "extra_headers" {
				body[k] = v
			}
		}
		observed = wire(body)
		return map[string]any{"id": "msg_01", "type": "message", "role": "assistant", "model": "claude-sonnet-4-5",
			"content": []any{map[string]any{"type": "text", "text": "Done"}}, "stop_reason": "end_turn", "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 10, "output_tokens": 1}}, nil
	}, RelayExecuteOptions{Name: "anthropic", ModelName: "claude-sonnet-4-5", SessionID: relayLLMSID("session-1"),
		Metadata: map[string]any{"api_mode": "anthropic_messages", "api_request_id": "request-anthropic"}})
	if err != nil {
		t.Fatal(err)
	}
	relayLLMEqual(t, observed, originalWire)
}

func TestRelayAsyncNonStreamReturnsNamespacedInterceptorResult(t *testing.T) {
	f, ctx, _ := relayLLMTurn(t)
	f.relay.LLM.Execute = func(_ context.Context, _ string, request any, cb tools.RelayProviderCallback, _ map[string]any) (any, error) {
		return tools.RelayAwaitable(func(c context.Context) (any, error) {
			response, err := cb(c, request)
			if err != nil {
				return nil, err
			}
			out := relayCopyMap(response.(map[string]any))
			out["post_interceptor"], out["usage"] = true, map[string]any{"input_tokens": 10}
			return out, nil
		}), nil
	}
	provider := func(context.Context, map[string]any) (any, error) {
		return tools.RelayAwaitable(func(context.Context) (any, error) { return map[string]any{"content": "raw"}, nil }), nil
	}
	result, err := RelayExecuteAsync(ctx, map[string]any{"model": "test-model", "messages": []any{}}, provider,
		RelayExecuteOptions{Name: "test-provider", ModelName: "test-model", SessionID: relayLLMSID("session-1"),
			Metadata: map[string]any{"api_mode": "custom", "api_request_id": "request-async-post"}})
	if err != nil {
		t.Fatal(err)
	}
	m := result.(map[string]any)
	relayLLMEqual(t, m["content"], "raw")
	relayLLMEqual(t, m["post_interceptor"], true)
	relayLLMEqual(t, m["usage"].(map[string]any)["input_tokens"], 10)
}

func TestRelayNonStreamPreservesProviderErrorFromRelayWrapperSuffix(t *testing.T) {
	f, ctx, turn := relayLLMTurn(t)
	providerErr := &relayLLMTestProviderError{msg: "provider failed"}
	f.relay.LLM.Execute = func(_ context.Context, _ string, request any, cb tools.RelayProviderCallback, _ map[string]any) (any, error) {
		return tools.RelayAwaitable(func(c context.Context) (any, error) {
			v, err := cb(c, request)
			if err != nil {
				return nil, &tools.RelayError{Class: "RuntimeError", Message: fmt.Sprintf("internal error: %s: %s (retried 3x)", "relayLLMTestProviderError", err.Error())}
			}
			return v, nil
		}), nil
	}
	_, err := RelayExecute(ctx, map[string]any{"model": "test-model", "messages": []any{}}, func(context.Context, map[string]any) (any, error) { return nil, providerErr },
		RelayExecuteOptions{Name: "test-provider", ModelName: "test-model", SessionID: relayLLMSID("session-1"),
			Metadata: map[string]any{"api_mode": "custom", "api_request_id": "request-error"}})
	if err != error(providerErr) {
		t.Fatalf("error %v is not the provider error", err)
	}
	if _, ok := relayLLMLogicalCalls(turn)["request-error"]; !ok {
		t.Fatal("failed call dropped its logical scope")
	}
}

func TestRelayCodecBaselineFailureIsExplicit(t *testing.T) {
	f, ctx, _ := relayLLMTurn(t)
	body := map[string]any{"model": "test-model", "messages": []any{}}
	request, _ := f.relay.LLMRequest(map[string]any{}, body)
	failing := func() (*tools.NemoRelayCodec, error) {
		return &tools.NemoRelayCodec{Decode: func(context.Context, any) (any, error) { return nil, errors.New("simulated codec failure") }}, nil
	}
	f.relay.Codecs = &tools.NemoRelayCodecs{OpenAIChatCodec: failing, OpenAIResponsesCodec: failing, AnthropicMessagesCodec: failing}
	baseline, err := relayCodecRoundTripRequestBody(ctx, f.relay, request, body, map[string]any{"api_mode": "chat_completions"})
	if err != nil || baseline != nil {
		t.Fatalf("baseline = %v, %v", baseline, err)
	}
}

func TestRelayStreamCurrentUnwrapsCompletedResponse(t *testing.T) {
	f := relayLLMInstallFake(t, "profile")
	ctx, _ := relayLLMOpenTurn(t, f, "session-moa", "turn-moa", "task-moa", false)
	completed := &relayLLMTestCompleted{Model: "kimi-k3", Choices: []relayLLMTestCompletedChoice{{Message: relayLLMTestMessage{Content: "done"}, FinishReason: "stop"}}}
	result, err := RelayStreamCurrent(ctx, map[string]any{"model": "kimi-k3", "stream": true}, relayLLMFactory(completed),
		RelayStreamOptions{Name: "kimi-coding", ModelName: "kimi-k3", Finalizer: relayLLMDict, CompletedResponsePredicate: relayLLMChoicesPredicate})
	if err != nil {
		t.Fatal(err)
	}
	if result != any(completed) {
		t.Fatalf("result %#v is not the completed response", result)
	}
}

func TestRelayStreamCurrentStreamsIteratorsWithPredicate(t *testing.T) {
	f := relayLLMInstallFake(t, "profile")
	ctx, _ := relayLLMOpenTurn(t, f, "session-moa", "turn-moa", "task-moa", false)
	chunks := []any{map[string]any{"delta": "a"}, map[string]any{"delta": "b"}}
	result, err := RelayStreamCurrent(ctx, map[string]any{"model": "m", "stream": true}, relayLLMFactory(chunks),
		RelayStreamOptions{Name: "provider", ModelName: "m", Finalizer: relayLLMDict, CompletedResponsePredicate: relayLLMChoicesPredicate})
	if err != nil {
		t.Fatal(err)
	}
	relayLLMEqual(t, relayLLMDrain(t, ctx, result.(*ManagedLlmStream)), chunks)
}

func TestRelayStreamCurrentCompletedResponseReleasesManagedStream(t *testing.T) {
	f, ctx, turn := relayLLMTurn(t)
	completed := relayLLMCompletedResponse("done")
	relayLLMStreamBody(f, func(c context.Context, upstream func(context.Context) (*tools.RelayAsyncIterator, error), _ func(context.Context, any) error, finalizer func(context.Context) (any, error), yield func(any) bool) error {
		up, _ := upstream(c)
		chunk, err := up.Next(c)
		if err == io.EOF {
			chunk = map[string]any{"injected": "relay-chunk"}
		} else if err != nil {
			return err
		}
		if !yield(chunk) {
			return nil
		}
		_, err = finalizer(c)
		return err
	})
	var captured []*ManagedLlmStream
	prev := relayStreamFn
	relayStreamFn = func(ctx context.Context, request map[string]any, factory func(context.Context, map[string]any) (any, error), o ManagedLlmStreamOptions) (relayStreamHandle, error) {
		s, err := NewManagedLlmStream(ctx, request, factory, o)
		captured = append(captured, s)
		return s, err
	}
	defer func() { relayStreamFn = prev }()
	result, err := RelayStreamCurrent(ctx, map[string]any{"model": "test-model", "messages": []any{}, "stream": true}, relayLLMFactory(completed),
		RelayStreamOptions{Name: "test-provider", ModelName: "test-model", Finalizer: relayLLMDict,
			Metadata: map[string]any{"api_request_id": "request-completed-drop"}, CompletedResponsePredicate: relayLLMChoicesPredicate})
	if err != nil {
		t.Fatal(err)
	}
	if result != any(completed) {
		t.Fatalf("result %#v is not the completed response", result)
	}
	managed := captured[0]
	if !managed.closed || managed.runtimeLease != nil {
		t.Fatalf("managed stream left open: closed=%v lease=%v", managed.closed, managed.runtimeLease)
	}
	if !relayLLMRuntime(turn).WaitOperationsIdle(0) {
		t.Fatal("operations not idle")
	}
	relayLLMEqual(t, relayLLMLogicalCalls(turn), map[string]any{})
	found := false
	for _, output := range f.popOutputs() {
		found = found || reflect.DeepEqual(output, map[string]any{"outcome": "success"})
	}
	if !found {
		t.Fatalf("no success pop in %v", f.popOutputs())
	}
}

type relayLLMLazyStream struct{ completed, final any }

func (l *relayLLMLazyStream) primeCompletedResponse(context.Context) error {
	l.final = l.completed
	return nil
}
func (l *relayLLMLazyStream) finishLogical(context.Context, string, error) error { return nil }
func (l *relayLLMLazyStream) closeStream(context.Context, string, error) error   { return nil }
func (l *relayLLMLazyStream) finalResponse() any                                 { return l.final }

func TestRelayStreamCurrentPrimesLazyCompletedResponse(t *testing.T) {
	_, ctx, _ := relayLLMTurn(t)
	completed := relayLLMCompletedResponse("done")
	lazy := &relayLLMLazyStream{completed: completed}
	prev := relayStreamFn
	relayStreamFn = func(context.Context, map[string]any, func(context.Context, map[string]any) (any, error), ManagedLlmStreamOptions) (relayStreamHandle, error) {
		return lazy, nil
	}
	defer func() { relayStreamFn = prev }()
	result, err := RelayStreamCurrent(ctx, map[string]any{"model": "test-model", "messages": []any{}, "stream": true}, relayLLMFactory(completed),
		RelayStreamOptions{Name: "test-provider", ModelName: "test-model", Finalizer: relayLLMDict, CompletedResponsePredicate: relayLLMChoicesPredicate})
	if err != nil {
		t.Fatal(err)
	}
	if result != any(completed) {
		t.Fatalf("result %#v is not the completed response", result)
	}
}

func TestRelayStreamCurrentUnwrapsCompletedResponseWithRealInterceptor(t *testing.T) {
	f, ctx, _ := relayLLMTurn(t)
	completed := relayLLMCompletedResponse("done")
	f.registerLLMStreamExecution("icebath-test-prime-completed", 1, func(c context.Context, _ string, request *tools.RelayLLMRequest, _ relayLLMFakeStreamContext, next relayLLMFakeNext) (*tools.RelayAsyncIterator, error) {
		return next(c, request)
	})
	defer f.deregister("icebath-test-prime-completed")
	result, err := RelayStreamCurrent(ctx, map[string]any{"model": "test-model", "messages": []any{}, "stream": true}, relayLLMFactory(completed),
		RelayStreamOptions{Name: "test-provider", ModelName: "test-model", Finalizer: relayLLMFinal(completed), CompletedResponsePredicate: relayLLMChoicesPredicate})
	if err != nil {
		t.Fatal(err)
	}
	if result != any(completed) {
		t.Fatalf("result %#v is not the completed response", result)
	}
}

func TestRelayStreamCurrentPreservesRealRelayInterceptorChunks(t *testing.T) {
	f, ctx, _ := relayLLMTurn(t)
	f.registerLLMStreamExecution("icebath-test-prime-stream", 1, func(_ context.Context, _ string, request *tools.RelayLLMRequest, _ relayLLMFakeStreamContext, next relayLLMFakeNext) (*tools.RelayAsyncIterator, error) {
		return relayLLMTestGen(func(c context.Context, yield func(any) bool) error {
			upstream, err := next(c, request)
			if err != nil {
				return err
			}
			for {
				chunk, err := upstream.Next(c)
				if err == io.EOF {
					return nil
				}
				if err != nil {
					return err
				}
				out := relayCopyMap(chunk.(map[string]any))
				out["delta"] = strings.ToUpper(out["delta"].(string))
				if !yield(out) {
					return nil
				}
			}
		}), nil
	})
	defer f.deregister("icebath-test-prime-stream")
	result, err := RelayStreamCurrent(ctx, map[string]any{"model": "test-model", "messages": []any{}, "stream": true},
		relayLLMFactory([]any{map[string]any{"delta": "a"}, map[string]any{"delta": "b"}}),
		RelayStreamOptions{Name: "test-provider", ModelName: "test-model", Finalizer: relayLLMFinal(map[string]any{"content": "AB"}), CompletedResponsePredicate: relayLLMChoicesPredicate})
	if err != nil {
		t.Fatal(err)
	}
	s := result.(*ManagedLlmStream)
	relayLLMEqual(t, relayLLMDrain(t, ctx, s), []any{map[string]any{"delta": "A"}, map[string]any{"delta": "B"}})
	if !s.OutputModified {
		t.Fatal("output_modified unset")
	}
}

func TestRelayStreamCurrentSurfacesManagedFactoryErrorBeforeReturn(t *testing.T) {
	_, ctx, _ := relayLLMTurn(t)
	_, err := RelayStreamCurrent(ctx, map[string]any{"model": "test-model", "messages": []any{}, "stream": true},
		func(context.Context, map[string]any) (any, error) {
			return nil, errors.New("provider failed before streaming")
		},
		RelayStreamOptions{Name: "test-provider", ModelName: "test-model", Finalizer: relayLLMDict, CompletedResponsePredicate: relayLLMChoicesPredicate})
	if err == nil || !strings.Contains(err.Error(), "provider failed before streaming") {
		t.Fatalf("err = %v", err)
	}
}

func TestRelayStreamManagedTrapsDirectCompletedResponse(t *testing.T) {
	_, ctx, _ := relayLLMTurn(t)
	s, err := NewManagedLlmStream(ctx, map[string]any{"model": "test-model", "messages": []any{}}, relayLLMFactory(relayLLMCompletedResponse("done")),
		ManagedLlmStreamOptions{SessionID: "session-1", Name: "test-provider", ModelName: "test-model", Finalizer: relayLLMDict, CompletedResponsePredicate: relayLLMChoicesPredicate})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.primeCompletedResponse(ctx); err != nil {
		t.Fatal(err)
	}
	if !s.closed {
		t.Fatal("stream not closed after the completed response")
	}
	relayLLMEqual(t, relayLLMDrain(t, ctx, s), []any(nil))
	relayLLMEqual(t, s.FinalResponse.(*relayLLMTestCompleted).Choices[0].Message.Content, "done")
}

func TestRelayStreamCurrentInsideManagedCallbackReturnsRaw(t *testing.T) {
	_, ctx, _ := relayLLMTurn(t)
	outer := func(c context.Context, _ map[string]any) (any, error) {
		return RelayStreamCurrent(c, map[string]any{"model": "test-model", "messages": []any{}}, relayLLMFactory(relayLLMCompletedResponse("done")),
			RelayStreamOptions{Name: "moa-aggregator", ModelName: "test-model", Finalizer: relayLLMDict, CompletedResponsePredicate: relayLLMChoicesPredicate})
	}
	s, err := NewManagedLlmStream(ctx, map[string]any{"model": "test-model", "messages": []any{}}, outer,
		ManagedLlmStreamOptions{SessionID: "session-1", Name: "moa", ModelName: "test-model", Finalizer: relayLLMDict, CompletedResponsePredicate: relayLLMChoicesPredicate})
	if err != nil {
		t.Fatal(err)
	}
	relayLLMEqual(t, relayLLMDrain(t, ctx, s), []any(nil))
	relayLLMEqual(t, s.FinalResponse.(*relayLLMTestCompleted).Choices[0].Message.Content, "done")
}

func TestAnthropicStreamAccumulatorRejectsUnhashableEventType(t *testing.T) {
	acc := NewAnthropicStreamAccumulator()
	if err := acc.Observe(map[string]any{"type": map[string]any{}}); err == nil || err.Error() != "unhashable type: 'dict'" {
		t.Fatalf("dict event type error %v", err)
	}
	if err := acc.Observe(map[string]any{"type": []any{"message_start"}}); err == nil || err.Error() != "unhashable type: 'list'" {
		t.Fatalf("list event type error %v", err)
	}
	if err := acc.Observe(map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_1", "extra": true}}); err != nil {
		t.Fatal(err)
	}
	if got := acc.Finalize(); got["id"] != "msg_1" || got["extra"] != nil {
		t.Fatalf("message start kept %v", got)
	}
}
