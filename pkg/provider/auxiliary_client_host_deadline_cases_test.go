package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Chunk iterator recording how far the consumer drained it
type auxCountingStream struct {
	count   int
	yielded int
	closed  bool
}

func (s *auxCountingStream) Next(context.Context) (map[string]any, error) {
	if s.yielded >= s.count {
		return nil, io.EOF
	}
	s.yielded++
	return map[string]any{"id": "resp-1", "model": "test-model", "usage": nil,
		"choices": []any{map[string]any{"index": 0, "finish_reason": nil, "delta": map[string]any{"content": "x", "tool_calls": nil}}}}, nil
}

func (s *auxCountingStream) Close() error {
	s.closed = true
	return nil
}

// Host deadline that already elapsed one second ago
func auxElapsedHostDeadline(ctx context.Context) context.Context {
	return AuxStreamDeadline(ctx, AuxStreamDeadlineAt(time.Now().Add(-time.Second)))
}

// Aggregates the stream under the hermes test's 2400 second ceiling
func auxAggregateUnderCeiling(ctx context.Context, stream AuxChatStream) (any, error) {
	ceiling := 2400.0
	return auxAggregateChatStream(ctx, stream, "m", &ceiling, nil)
}

func TestStreamedSummaryStopsAtAnElapsedHostDeadline(t *testing.T) {
	stream := &auxCountingStream{count: 50}
	_, err := auxAggregateUnderCeiling(auxElapsedHostDeadline(context.Background()), stream)
	var timeout *AuxTimeoutError
	if !errors.As(err, &timeout) || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("want a timed out error, got %v", err)
	}
	if stream.yielded != 1 || !stream.closed {
		t.Fatalf("yielded %d closed %v", stream.yielded, stream.closed)
	}
}

func TestStreamedSummaryRunsToCompletionUnderALiveHostDeadline(t *testing.T) {
	stream := &auxCountingStream{count: 5}
	ctx := AuxStreamDeadline(context.Background(), AuxStreamDeadlineAt(time.Now().Add(600*time.Second)))
	response, err := auxAggregateUnderCeiling(ctx, stream)
	if err != nil || ExtractContentOrReasoning(response, -1) != "xxxxx" || stream.yielded != 5 {
		t.Fatalf("response %v err %v yielded %d", response, err, stream.yielded)
	}
}

func TestNoHostDeadlineKeepsTheHistoricalCeilingBehaviour(t *testing.T) {
	stream := &auxCountingStream{count: 5}
	response, err := auxAggregateUnderCeiling(context.Background(), stream)
	if err != nil || ExtractContentOrReasoning(response, -1) != "xxxxx" || stream.yielded != 5 {
		t.Fatalf("response %v err %v yielded %d", response, err, stream.yielded)
	}
	_ = auxElapsedHostDeadline(context.Background())
	stream2 := &auxCountingStream{count: 3}
	response, err = auxAggregateUnderCeiling(context.Background(), stream2)
	if err != nil || ExtractContentOrReasoning(response, -1) != "xxx" {
		t.Fatalf("an exited scope leaked into the next call: %v %v", response, err)
	}
}

func TestNoneDeadlineIsANoOpPassthrough(t *testing.T) {
	stream := &auxCountingStream{count: 3}
	response, err := auxAggregateUnderCeiling(AuxStreamDeadline(context.Background(), nil), stream)
	if err != nil || ExtractContentOrReasoning(response, -1) != "xxx" {
		t.Fatalf("response %v err %v", response, err)
	}
}

func TestNestedNoneInheritsRatherThanEscapingTheHostDeadline(t *testing.T) {
	outer := time.Now().Add(-time.Second)
	stream := &auxCountingStream{count: 50}
	nested := AuxStreamDeadline(AuxStreamDeadline(context.Background(), AuxStreamDeadlineAt(outer)), nil)
	if got, ok := auxCurrentAuxStreamDeadline(nested); !ok || !got.Equal(outer) {
		t.Fatalf("nested deadline %v %v", got, ok)
	}
	_, err := auxAggregateUnderCeiling(nested, stream)
	var timeout *AuxTimeoutError
	if !errors.As(err, &timeout) || stream.yielded != 1 {
		t.Fatalf("err %v yielded %d", err, stream.yielded)
	}
}

func TestAsyncStreamMirrorHonoursTheHostDeadline(t *testing.T) {
	stream := &auxCountingStream{count: 50}
	ceiling := 2400.0
	_, err := auxAggregateChatStreamAsync(auxElapsedHostDeadline(context.Background()), stream, "m", &ceiling)
	var timeout *AuxTimeoutError
	if !errors.As(err, &timeout) || stream.yielded != 1 {
		t.Fatalf("err %v yielded %d", err, stream.yielded)
	}
}

// Codex event stream yielding content deltas at the delay until closed or the limit is reached
type auxLiveCodexEvents struct {
	limit   int
	delay   time.Duration
	yielded atomic.Int64
	closed  atomic.Bool
}

func (s *auxLiveCodexEvents) Next(context.Context) (map[string]any, error) {
	if s.closed.Load() || (s.limit > 0 && int(s.yielded.Load()) >= s.limit) {
		return nil, io.EOF
	}
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	s.yielded.Add(1)
	return map[string]any{"type": "response.output_text.delta", "delta": "tok"}, nil
}

func (s *auxLiveCodexEvents) Close() error {
	s.closed.Store(true)
	return nil
}

// Codex SDK double handing out one event stream
type auxCodexWireClient struct {
	auxEffectiveStamp
	events AuxChatStream
}

func (c *auxCodexWireClient) BaseURL() string { return "https://chatgpt.com/backend-api/codex" }
func (c *auxCodexWireClient) APIKey() string  { return "test" }
func (c *auxCodexWireClient) CreateChatCompletion(context.Context, map[string]any) (any, error) {
	return nil, errors.New("chat completions unused")
}

func (c *auxCodexWireClient) CreateResponse(context.Context, map[string]any) (any, error) {
	return c.events, nil
}

func (c *auxCodexWireClient) Close() error { return nil }

// Event consumer double driving every event through the hook and answering a summary message
func auxConsumeCodexForTest(ctx context.Context, stream AuxChatStream, _ string, onEvent func(event any) error) (map[string]any, error) {
	for {
		event, err := stream.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if err := onEvent(event); err != nil {
			return nil, err
		}
	}
	return map[string]any{"output": []any{map[string]any{"type": "message",
		"content": []any{map[string]any{"type": "output_text", "text": "summary"}}}}, "usage": nil}, nil
}

// Codex adapter over the event stream with the consumer double installed for the test
func auxCodexWireAdapter(t *testing.T, events AuxChatStream) *auxCodexCompletionsAdapter {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	prev := auxConsumeCodexEventStreamFn
	auxConsumeCodexEventStreamFn = auxConsumeCodexForTest
	t.Cleanup(func() { auxConsumeCodexEventStreamFn = prev })
	return &auxCodexCompletionsAdapter{client: &auxCodexWireClient{events: events}, model: "gpt-5.6-sol"}
}

func auxSummarizeKwargs() map[string]any {
	return map[string]any{"messages": []any{map[string]any{"role": "user", "content": "summarize"}}, "timeout": 300}
}

func TestCodexStreamStopsAtTheHostDeadlineNotItsOwnCeiling(t *testing.T) {
	events := &auxLiveCodexEvents{delay: 20 * time.Millisecond}
	adapter := auxCodexWireAdapter(t, events)
	ctx := AuxStreamDeadline(context.Background(), AuxStreamDeadlineAt(time.Now().Add(400*time.Millisecond)))
	start := time.Now()
	_, err := adapter.create(ctx, auxSummarizeKwargs())
	elapsed := time.Since(start)
	var timeout *AuxTimeoutError
	if !errors.As(err, &timeout) || !strings.Contains(err.Error(), "hard ceiling") {
		t.Fatalf("want a hard ceiling timeout, got %v", err)
	}
	if elapsed >= 5*time.Second || events.yielded.Load() >= 100 {
		t.Fatalf("stream outlived the host deadline by %v with %d events", elapsed, events.yielded.Load())
	}
}

func TestCodexStreamWithoutHostDeadlineKeepsItsCeiling(t *testing.T) {
	adapter := auxCodexWireAdapter(t, &auxLiveCodexEvents{limit: 3})
	response, err := adapter.create(context.Background(), auxSummarizeKwargs())
	if err != nil || ExtractContentOrReasoning(response, -1) != "summary" {
		t.Fatalf("response %v err %v", response, err)
	}
}

// Anthropic Messages SSE server streaming the text deltas at the delay and reporting when its handler exits
type auxSlowAnthropicServer struct {
	srv      *httptest.Server
	written  atomic.Int64
	exited   chan struct{}
	exitOnce sync.Once
}

func newAuxSlowAnthropicServer(t *testing.T, deltas []string, delay time.Duration) *auxSlowAnthropicServer {
	t.Helper()
	s := &auxSlowAnthropicServer{exited: make(chan struct{})}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer s.exitOnce.Do(func() { close(s.exited) })
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		write := func(payload map[string]any) bool {
			data, _ := json.Marshal(payload)
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", payload["type"], data); err != nil {
				return false
			}
			if flusher != nil {
				flusher.Flush()
			}
			return true
		}
		if !write(anthSSEStart()) || !write(anthSSEBlockStart(0, anthText(""))) {
			return
		}
		for _, text := range deltas {
			if delay > 0 {
				time.Sleep(delay)
			}
			select {
			case <-r.Context().Done():
				return
			default:
			}
			s.written.Add(1)
			if !write(anthSSETextDelta(0, text)) {
				return
			}
		}
		write(anthSSEBlockStop(0))
		write(anthSSEMessageDelta("end_turn"))
		write(anthSSEStop())
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// Repeats the delta text count times
func auxRepeatedDeltas(text string, count int) []string {
	out := make([]string, count)
	for i := range out {
		out[i] = text
	}
	return out
}

// Anthropic SDK client against the server, closed after the test
func auxAnthropicWireClient(t *testing.T, s *auxSlowAnthropicServer) *anthropicSDKClient {
	t.Helper()
	client, err := buildAnthropicClient(context.Background(), "sk-ant-api-key", s.srv.URL, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

func auxAnthropicWireKwargs() map[string]any {
	return map[string]any{"model": "m", "messages": []any{}}
}

func TestAnthropicStreamStopsAtTheHostDeadline(t *testing.T) {
	server := newAuxSlowAnthropicServer(t, auxRepeatedDeltas("tok", 10000), 10*time.Millisecond)
	var ticks atomic.Int64
	ctx := AuxProgressHook(context.Background(), func() error { ticks.Add(1); return nil })
	ctx = AuxStreamDeadline(ctx, AuxStreamDeadlineAt(time.Now().Add(300*time.Millisecond)))
	hook := auxAnthropicAuxStreamEventHook(ctx)
	start := time.Now()
	_, err := createAnthropicMessage(ctx, auxAnthropicWireClient(t, server), auxAnthropicWireKwargs(), hook)
	var timeout *AuxTimeoutError
	if !errors.As(err, &timeout) || !strings.Contains(err.Error(), "timed out at the host compression deadline") {
		t.Fatalf("want the host deadline timeout, got %v", err)
	}
	if time.Since(start) >= 5*time.Second {
		t.Fatalf("stream outlived the host deadline, %v", time.Since(start))
	}
	auxAwaitSignal(t, server.exited, 5*time.Second, "stream context must be closed on the deadline")
	if ticks.Load() == 0 {
		t.Fatal("substantive deltas must still tick the progress hook")
	}
	if server.written.Load() >= 1000 {
		t.Fatalf("server streamed %d deltas past the deadline", server.written.Load())
	}
}

func TestAnthropicStreamHonoursAnExplicitHardCancel(t *testing.T) {
	server := newAuxSlowAnthropicServer(t, auxRepeatedDeltas("tok", 10000), 10*time.Millisecond)
	var cancelled atomic.Bool
	ctx := AuxProgressHook(context.Background(), func() error { return nil })
	ctx = AuxInterruptProtection(ctx, true, AuxCancelFunc(func() (bool, error) { return cancelled.Load(), nil }), nil)
	hook := auxAnthropicAuxStreamEventHook(ctx)
	consumed := 0
	flipAfterFirst := func(event any) error {
		cancelled.Store(true)
		consumed++
		return hook(event)
	}
	_, err := createAnthropicMessage(ctx, auxAnthropicWireClient(t, server), auxAnthropicWireKwargs(), flipAfterFirst)
	if !auxIsExplicitCancellation(err) {
		t.Fatalf("want the explicit cancellation, got %v", err)
	}
	if consumed != 1 {
		t.Fatalf("consumed %d events after the cancel", consumed)
	}
	auxAwaitSignal(t, server.exited, 5*time.Second, "stream context must be closed on the cancel")
}

func TestAnthropicStreamWithoutHostDeadlineRunsToCompletion(t *testing.T) {
	server := newAuxSlowAnthropicServer(t, []string{"su", "mm", "a", "r", "y"}, 0)
	ctx := AuxProgressHook(context.Background(), func() error { return nil })
	hook := auxAnthropicAuxStreamEventHook(ctx)
	message, err := createAnthropicMessage(ctx, auxAnthropicWireClient(t, server), auxAnthropicWireKwargs(), hook)
	if err != nil {
		t.Fatal(err)
	}
	content, _ := message["content"].([]any)
	block, _ := content[0].(map[string]any)
	if block["text"] != "summary" || server.written.Load() != 5 {
		t.Fatalf("message %v written %d", message, server.written.Load())
	}
}
