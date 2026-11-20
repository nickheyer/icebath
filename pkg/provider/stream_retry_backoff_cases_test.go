package provider

import (
	"context"
	"errors"
	"testing"
	"time"
)

func backoffTestCall(agent *fakeChatAgent) *streamingCall {
	c := newStreamingCall(context.Background(), agent, map[string]any{}, nil)
	c.lastChunk.Store(0)
	return c
}

func TestTransientDropsRetryWithCappedExponentialBackoff(t *testing.T) {
	agent := &fakeChatAgent{provider: "anthropic", apiMode: "chat_completions", baseURL: "https://api.anthropic.com/v1", model: "m"}
	c := backoffTestCall(agent)
	var waits []time.Duration
	prev := waitStreamRetryBackoffFn
	waitStreamRetryBackoffFn = func(_ ChatCompletionAgent, delay time.Duration) { waits = append(waits, delay) }
	defer func() { waitStreamRetryBackoffFn = prev }()
	drops := []error{
		&AnthropicConnectionError{Err: errors.New("connection reset")},
		&AuxTransportError{Class: "ConnectError", Message: "reset"},
		&AuxTransportError{Class: "ReadError", Message: "reset"},
		&AuxTransportError{Class: "RemoteProtocolError", Message: "closed"},
	}
	for attempt, err := range drops {
		if !streamingCallHandleError(c, err, attempt, 10) {
			t.Fatalf("attempt %d with %T was not retried", attempt, err)
		}
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second}
	if len(waits) != len(want) {
		t.Fatalf("waits %v want %v", waits, want)
	}
	for i := range want {
		if waits[i] != want[i] {
			t.Fatalf("waits %v want %v", waits, want)
		}
	}
	if c.lastChunk.Load() <= 0 {
		t.Fatal("stale clock was not restarted after the backoff")
	}
	c.mu.Lock()
	recorded := c.err
	c.mu.Unlock()
	if recorded != nil {
		t.Fatalf("result carries an error after retried drops: %v", recorded)
	}
}

func TestBackoffWaitReturnsImmediatelyOnInterrupt(t *testing.T) {
	agent := &fakeChatAgent{provider: "openrouter", apiMode: "chat_completions", baseURL: "https://openrouter.ai/api/v1", model: "m"}
	c := backoffTestCall(agent)
	timer := time.AfterFunc(200*time.Millisecond, func() {
		agent.mu.Lock()
		agent.interrupted = true
		agent.mu.Unlock()
	})
	defer timer.Stop()
	start := time.Now()
	c.retryAfterDrop(&AuxTransportError{Class: "ConnectError", Message: "x"}, 5, 10, false, "t")
	if elapsed := time.Since(start); elapsed >= time.Second {
		t.Fatalf("backoff held the interrupt for %v", elapsed)
	}
}
