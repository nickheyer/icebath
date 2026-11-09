package provider

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Routes every task to openrouter test-model on a scripted client with the response validator bypassed
func auxConcurrencyFixture(t *testing.T, configYAML string, create func(kwargs map[string]any) (any, error)) (context.Context, *auxScriptedClient) {
	t.Helper()
	auxResetAuxSemaphores()
	t.Cleanup(auxResetAuxSemaphores)
	ctx := auxMainTestHome(t, configYAML)
	auxSwapTaskRoute(t, "openrouter", "test-model")
	client := &auxScriptedClient{base: "https://example.test/v1", create: create}
	auxSwapCachedClient(t, client, "test-model")
	prev := auxValidateLLMResponseFn
	t.Cleanup(func() { auxValidateLLMResponseFn = prev })
	auxValidateLLMResponseFn = func(_ context.Context, response any, _, _, _ string) (any, error) { return response, nil }
	return ctx, client
}

func auxConcurrencyResponse() map[string]any {
	return map[string]any{"id": "c1", "model": "test-model", "choices": []any{map[string]any{"index": 0,
		"message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop"}}}
}

func auxConcurrencyMessages(content string) []any {
	return []any{map[string]any{"role": "user", "content": content}}
}

// Create callback counting in flight calls and the highest overlap seen across a 50ms sleep
func auxInflightCounter(maxActive *int) func(map[string]any) (any, error) {
	var mu sync.Mutex
	active := 0
	return func(map[string]any) (any, error) {
		mu.Lock()
		active++
		if active > *maxActive {
			*maxActive = active
		}
		mu.Unlock()
		time.Sleep(50 * time.Millisecond)
		mu.Lock()
		active--
		mu.Unlock()
		return auxConcurrencyResponse(), nil
	}
}

func (c *auxScriptedClient) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

func TestSyncCallEnforcesLimitCallLlmCapsConcurrentInflight(t *testing.T) {
	maxActive := 0
	ctx, client := auxConcurrencyFixture(t, "auxiliary:\n  title_generation:\n    max_concurrency: 2\n", auxInflightCounter(&maxActive))
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := CallLLM(ctx, map[string]any{"task": "title_generation", "messages": auxConcurrencyMessages("hi")}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if maxActive > 2 {
		t.Fatalf("observed %d > limit 2", maxActive)
	}
	if n := client.callCount(); n != 6 {
		t.Fatalf("create called %d times", n)
	}
}

func TestSyncCallEnforcesLimitSemaphoreReleasedOnException(t *testing.T) {
	ctx, _ := auxConcurrencyFixture(t, "auxiliary:\n  title_generation:\n    max_concurrency: 1\n", func(map[string]any) (any, error) {
		return nil, errors.New("boom")
	})
	for i := 0; i < 3; i++ {
		_, err := CallLLM(ctx, map[string]any{"task": "title_generation", "messages": auxConcurrencyMessages("hi")})
		if err == nil || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("call %d: %v", i, err)
		}
	}
}

func TestSyncCallEnforcesLimitStreamHoldsPermitUntilConsumedAndPreservesOptions(t *testing.T) {
	chunk := map[string]any{"id": "c1", "choices": []any{map[string]any{"delta": map[string]any{"content": "chunk"}}}}
	var calls atomic.Int32
	ctx, client := auxConcurrencyFixture(t, "auxiliary:\n  compression:\n    max_concurrency: 1\n", func(map[string]any) (any, error) {
		if calls.Add(1) == 1 {
			return &auxSliceStream{chunks: []map[string]any{chunk}}, nil
		}
		return auxConcurrencyResponse(), nil
	})
	resp, err := CallLLM(ctx, map[string]any{"task": "compression", "messages": auxConcurrencyMessages("first"),
		"stream": true, "stream_options": map[string]any{"include_usage": true}})
	if err != nil {
		t.Fatal(err)
	}
	stream, ok := resp.(AuxChatStream)
	if !ok {
		t.Fatalf("stream call returned %T", resp)
	}
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		_, err := CallLLM(ctx, map[string]any{"task": "compression", "messages": auxConcurrencyMessages("second")})
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("second call never started")
	}
	time.Sleep(50 * time.Millisecond)
	if n := client.callCount(); n != 1 {
		t.Fatalf("second call reached create while the stream held the permit, %d calls", n)
	}
	var got []map[string]any
	for {
		c, err := stream.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, c)
	}
	if !reflect.DeepEqual(got, []map[string]any{chunk}) {
		t.Fatalf("stream yielded %v", got)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("second call still blocked after the stream was consumed")
	}
	if n := client.callCount(); n != 2 {
		t.Fatalf("create called %d times", n)
	}
	first := client.calls[0]
	if first["stream"] != true {
		t.Fatalf("first call stream %v", first["stream"])
	}
	if !reflect.DeepEqual(first["stream_options"], map[string]any{"include_usage": true}) {
		t.Fatalf("first call stream_options %v", first["stream_options"])
	}
}

func TestAsyncCallEnforcesLimitAsyncCallLlmCapsConcurrentInflight(t *testing.T) {
	maxActive := 0
	ctx, client := auxConcurrencyFixture(t, "auxiliary:\n  compression:\n    max_concurrency: 2\n", auxInflightCounter(&maxActive))
	ctx = WithAuxiliaryEventLoop(ctx, &auxTestLoop{})
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := AsyncCallLLM(ctx, map[string]any{"task": "compression", "messages": auxConcurrencyMessages("hi")}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if maxActive > 2 {
		t.Fatalf("observed %d > limit 2", maxActive)
	}
	if n := client.callCount(); n != 6 {
		t.Fatalf("create awaited %d times", n)
	}
}

func TestAsyncCallEnforcesLimitAsyncSemaphoreReleasedOnException(t *testing.T) {
	ctx, _ := auxConcurrencyFixture(t, "auxiliary:\n  compression:\n    max_concurrency: 1\n", func(map[string]any) (any, error) {
		return nil, errors.New("boom")
	})
	ctx = WithAuxiliaryEventLoop(ctx, &auxTestLoop{})
	for i := 0; i < 3; i++ {
		_, err := AsyncCallLLM(ctx, map[string]any{"task": "compression", "messages": auxConcurrencyMessages("hi")})
		if err == nil || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("call %d: %v", i, err)
		}
	}
}
