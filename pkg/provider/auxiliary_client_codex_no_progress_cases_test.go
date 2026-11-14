package provider

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

type auxTimedEvent struct {
	delay time.Duration
	event map[string]any
}

// Event stream replaying timed steps, then repeating the tail event until closed or ending the stream
type auxTimedStream struct {
	steps []auxTimedEvent
	tail  *auxTimedEvent
	i     int
	done  chan struct{}
	once  sync.Once
}

func newAuxTimedStream(steps []auxTimedEvent, tail *auxTimedEvent) *auxTimedStream {
	return &auxTimedStream{steps: steps, tail: tail, done: make(chan struct{})}
}

func (s *auxTimedStream) Next(ctx context.Context) (map[string]any, error) {
	var ev auxTimedEvent
	switch {
	case s.i < len(s.steps):
		ev = s.steps[s.i]
		s.i++
	case s.tail != nil:
		ev = *s.tail
	default:
		return nil, io.EOF
	}
	select {
	case <-time.After(ev.delay):
		return ev.event, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.done:
		return nil, io.EOF
	}
}

func (s *auxTimedStream) Close() error {
	s.once.Do(func() { close(s.done) })
	return nil
}

func auxContentEvent() map[string]any {
	return map[string]any{"type": "response.output_text.delta", "delta": "tok"}
}
func auxKeepaliveEvent() map[string]any { return map[string]any{"type": "response.in_progress"} }

func auxRepeat(n int, delay time.Duration, event map[string]any) []auxTimedEvent {
	out := make([]auxTimedEvent, n)
	for i := range out {
		out[i] = auxTimedEvent{delay, event}
	}
	return out
}

// Installs a no progress window and a stream consumer that drains every event into a summary response
func auxNoProgressWindow(t *testing.T, seconds float64) {
	t.Helper()
	prevWindow, prevConsume := auxStreamNoProgressTimeoutSeconds, auxConsumeCodexEventStreamFn
	t.Cleanup(func() { auxStreamNoProgressTimeoutSeconds, auxConsumeCodexEventStreamFn = prevWindow, prevConsume })
	auxStreamNoProgressTimeoutSeconds = seconds
	auxConsumeCodexEventStreamFn = func(ctx context.Context, stream AuxChatStream, _ string, onEvent func(event any) error) (map[string]any, error) {
		for {
			ev, err := stream.Next(ctx)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, err
			}
			if err := onEvent(ev); err != nil {
				return nil, err
			}
		}
		return map[string]any{"output": []any{map[string]any{"type": "message",
			"content": []any{map[string]any{"type": "output_text", "text": "summary"}}}}, "usage": nil}, nil
	}
}

func auxCodexAdapterFor(answer func() any) *auxCodexCompletionsAdapter {
	return &auxCodexCompletionsAdapter{client: &auxScriptedResponsesClient{base: CodexBaseURL, answer: answer}, model: "gpt-5.6-sol"}
}

func auxExpectTimeout(t *testing.T, err error, fragment string) {
	t.Helper()
	var timeout *AuxTimeoutError
	if !errors.As(err, &timeout) || !strings.Contains(err.Error(), fragment) {
		t.Fatalf("want TimeoutError with %q, got %v", fragment, err)
	}
}

func TestNoProgressFailFastDeadStreamFailsAtNoProgressWindowNotTotalTimeout(t *testing.T) {
	auxNoProgressWindow(t, 0.3)
	adapter := auxCodexAdapterFor(func() any {
		return newAuxTimedStream(nil, &auxTimedEvent{20 * time.Millisecond, auxKeepaliveEvent()})
	})
	start := time.Now()
	_, err := adapter.create(context.Background(), map[string]any{"messages": auxConcurrencyMessages("summarize"), "timeout": 300.0})
	auxExpectTimeout(t, err, "no-progress timeout")
	if elapsed := time.Since(start); elapsed >= 5*time.Second {
		t.Fatalf("fail-fast took %.1fs", elapsed.Seconds())
	}
}

func TestNoProgressFailFastLiveStreamOutlivesTheOldAbsoluteTotalTimeout(t *testing.T) {
	auxNoProgressWindow(t, 0.4)
	adapter := auxCodexAdapterFor(func() any { return newAuxTimedStream(auxRepeat(8, 100*time.Millisecond, auxContentEvent()), nil) })
	resp, err := adapter.create(context.Background(), map[string]any{"messages": auxConcurrencyMessages("summarize"), "timeout": 0.4})
	if err != nil {
		t.Fatal(err)
	}
	if got := auxActualContent(t, resp); got != "summary" {
		t.Fatalf("content %q", got)
	}
}

func TestNoProgressFailFastMidStreamStallRaisesStalledTimeout(t *testing.T) {
	auxNoProgressWindow(t, 0.3)
	adapter := auxCodexAdapterFor(func() any {
		return newAuxTimedStream(auxRepeat(2, 0, auxContentEvent()), &auxTimedEvent{20 * time.Millisecond, auxKeepaliveEvent()})
	})
	start := time.Now()
	_, err := adapter.create(context.Background(), map[string]any{"messages": auxConcurrencyMessages("summarize"), "timeout": 300.0})
	auxExpectTimeout(t, err, "stalled: no new output")
	if elapsed := time.Since(start); elapsed >= 5*time.Second {
		t.Fatalf("stall detection took %.1fs", elapsed.Seconds())
	}
}

func TestNoProgressFailFastHardCeilingBoundsATokenDrip(t *testing.T) {
	auxNoProgressWindow(t, 5.0)
	prev := auxStreamTotalCeilingFn
	t.Cleanup(func() { auxStreamTotalCeilingFn = prev })
	auxStreamTotalCeilingFn = func(float64) float64 { return 0.3 }
	adapter := auxCodexAdapterFor(func() any { return newAuxTimedStream(nil, &auxTimedEvent{50 * time.Millisecond, auxContentEvent()}) })
	_, err := adapter.create(context.Background(), map[string]any{"messages": auxConcurrencyMessages("summarize"), "timeout": 300.0})
	auxExpectTimeout(t, err, "hard ceiling")
}

// Responses client whose create blocks until released, recording when each Close happened
type auxBlockingResponsesClient struct {
	release chan struct{}
	mu      sync.Mutex
	closes  []time.Time
}

func (c *auxBlockingResponsesClient) BaseURL() string { return CodexBaseURL }
func (c *auxBlockingResponsesClient) APIKey() string  { return "codex-key" }
func (c *auxBlockingResponsesClient) CreateChatCompletion(context.Context, map[string]any) (any, error) {
	return nil, errors.New("chat completions unused")
}

func (c *auxBlockingResponsesClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closes = append(c.closes, time.Now())
	return nil
}

func (c *auxBlockingResponsesClient) CreateResponse(context.Context, map[string]any) (any, error) {
	select {
	case <-c.release:
	case <-time.After(30 * time.Second):
	}
	return &auxFakeEventStream{}, nil
}

func (c *auxBlockingResponsesClient) closeTimes() []time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Time(nil), c.closes...)
}

func TestNoProgressFailFastWatchdogTimerFiresWhileBlockedBeforeFirstEvent(t *testing.T) {
	auxNoProgressWindow(t, 0.3)
	client := &auxBlockingResponsesClient{release: make(chan struct{})}
	adapter := &auxCodexCompletionsAdapter{client: client, model: "gpt-5.6-sol"}
	done := make(chan error, 1)
	go func() {
		_, err := adapter.create(context.Background(), map[string]any{"messages": auxConcurrencyMessages("x"), "timeout": 300.0})
		done <- err
	}()
	time.Sleep(time.Second)
	if closes := client.closeTimes(); len(closes) != 0 {
		t.Fatalf("stranger-thread close: %v", closes)
	}
	released := time.Now()
	close(client.release)
	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("owner never surfaced the timeout")
	}
	auxExpectTimeout(t, err, "no-progress timeout")
	closes := client.closeTimes()
	if len(closes) != 1 || closes[0].Before(released) {
		t.Fatalf("the owning call must close the client once on unwind, got %v after release %v", closes, released)
	}
}

func auxAssistantResponse(model, content string) map[string]any {
	return map[string]any{"model": model, "usage": nil, "choices": []any{map[string]any{"index": 0,
		"message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}}}
}

// Compression call_llm whose primary fails first, with every fallback but the payment rung disabled
func auxCompressionRetryGate(t *testing.T, primaryErr error, second map[string]any) (any, *auxScriptedClient, *int) {
	t.Helper()
	ctx := auxMainTestHome(t, "")
	auxSwapTaskRoute(t, "auto", "gpt-5.6-sol")
	calls := 0
	primary := &auxScriptedClient{base: CodexBaseURL, create: func(map[string]any) (any, error) {
		calls++
		if calls == 1 || second == nil {
			return nil, primaryErr
		}
		return second, nil
	}}
	auxSwapCachedClient(t, primary, "gpt-5.6-sol")
	fallback := &auxScriptedClient{base: "https://openrouter.ai/api/v1", create: func(map[string]any) (any, error) {
		return auxAssistantResponse("fb", "fallback"), nil
	}}
	prevConfigured, prevMain, prevPayment, prevBackoff := auxTryConfiguredFallbackChainFn, auxTryMainFallbackChainFn, auxTryPaymentFallbackFn, auxTransientRetryBackoffBase
	t.Cleanup(func() {
		auxTryConfiguredFallbackChainFn, auxTryMainFallbackChainFn, auxTryPaymentFallbackFn, auxTransientRetryBackoffBase = prevConfigured, prevMain, prevPayment, prevBackoff
	})
	auxTryConfiguredFallbackChainFn = func(context.Context, string, string, string, string, string, FailureScope) (AuxClient, string, string, error) {
		return nil, "", "", nil
	}
	auxTryMainFallbackChainFn = func(context.Context, string, string, string, string, string, FailureScope) (AuxClient, string, string, error) {
		return nil, "", "", nil
	}
	paymentCalls := 0
	auxTryPaymentFallbackFn = func(context.Context, string, string, string, string, FailureScope, map[string]any) (AuxClient, string, string, error) {
		paymentCalls++
		return fallback, "fb", "openrouter", nil
	}
	auxTransientRetryBackoffBase = 0
	result, err := CallLLM(ctx, map[string]any{"task": "compression", "messages": auxConcurrencyMessages("summarize")})
	if err != nil {
		t.Fatal(err)
	}
	return result, primary, &paymentCalls
}

func TestCompressionRetryGateNoProgressTimeoutRetriesSameProvider(t *testing.T) {
	err := &AuxTimeoutError{Message: "Codex auxiliary Responses stream produced no output within 60.0s (no-progress timeout, 60.2s elapsed)"}
	result, primary, payment := auxCompressionRetryGate(t, err, auxAssistantResponse("gpt-5.6-sol", "retried"))
	if got := auxActualContent(t, result); got != "retried" {
		t.Fatalf("content %q", got)
	}
	if n := primary.callCount(); n != 2 || *payment != 0 {
		t.Fatalf("primary calls %d payment fallback calls %d", n, *payment)
	}
}

func TestCompressionRetryGateStalledTimeoutSkipsSameProviderRetry(t *testing.T) {
	err := &AuxTimeoutError{Message: "Codex auxiliary Responses stream stalled: no new output for 60.0s (247.3s elapsed)"}
	result, primary, payment := auxCompressionRetryGate(t, err, nil)
	if got := auxActualContent(t, result); got != "fallback" {
		t.Fatalf("content %q", got)
	}
	if n := primary.callCount(); n != 1 || *payment == 0 {
		t.Fatalf("primary calls %d payment fallback calls %d", n, *payment)
	}
}

func TestCompressionRetryGateHardCeilingTimeoutSkipsSameProviderRetry(t *testing.T) {
	err := &AuxTimeoutError{Message: "Codex auxiliary Responses stream exceeded 600.0s hard ceiling"}
	result, primary, payment := auxCompressionRetryGate(t, err, nil)
	if got := auxActualContent(t, result); got != "fallback" {
		t.Fatalf("content %q", got)
	}
	if n := primary.callCount(); n != 1 || *payment == 0 {
		t.Fatalf("primary calls %d payment fallback calls %d", n, *payment)
	}
}
