package provider

import (
	"context"
	"math"
	"reflect"
	"strings"
	"testing"
)

func unmask5xxAgent() *fakeChatAgent {
	return &fakeChatAgent{provider: "custom", apiMode: "chat_completions", model: "gpt-5.6-sol"}
}

// Streaming call past its first delta gate with the default stale timeout, no deltas delivered yet
func unmask5xxCall(agent *fakeChatAgent, apiKwargs map[string]any) *streamingCall {
	c := newStreamingCall(context.Background(), agent, apiKwargs, nil)
	c.firstDeltaFired.Store(true)
	c.mu.Lock()
	c.staleTimeout = 180.0
	c.mu.Unlock()
	return c
}

func unmask5xxOutcome(c *streamingCall) (any, error, float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.response, c.err, c.staleTimeout
}

func TestStream5xxProbeSuccessDeliversResponseWithoutLatch(t *testing.T) {
	agent := unmask5xxAgent()
	c := unmask5xxCall(agent, map[string]any{"model": "m", "stream": true, "stream_options": map[string]any{"x": 1}, "messages": []any{}})
	var seenKwargs []map[string]any
	var staleDuringProbe []float64
	agent.s().dispatch = func(_ context.Context, kwargs map[string]any) (any, error) {
		seenKwargs = append(seenKwargs, kwargs)
		c.mu.Lock()
		staleDuringProbe = append(staleDuringProbe, c.staleTimeout)
		c.mu.Unlock()
		return chatReply("ok"), nil
	}
	handled := !streamingCallHandleError(c, &APIError{StatusCode: 500, Message: "something went wrong"}, 2, 2)
	if !handled {
		t.Fatal("a pre-delta 5xx with a successful probe must end the retry loop as handled")
	}
	response, err, stale := unmask5xxOutcome(c)
	if response == nil || err != nil {
		t.Fatalf("response %v err %v", response, err)
	}
	if len(seenKwargs) != 1 {
		t.Fatalf("probe calls %d", len(seenKwargs))
	}
	if _, has := seenKwargs[0]["stream"]; has {
		t.Fatalf("probe kwargs still carry stream: %v", seenKwargs[0])
	}
	if _, has := seenKwargs[0]["stream_options"]; has {
		t.Fatalf("probe kwargs still carry stream_options: %v", seenKwargs[0])
	}
	st := agent.st()
	st.mu.Lock()
	deltas := append([]string(nil), st.deltas...)
	disabled := st.streamingDisabled
	st.mu.Unlock()
	if !reflect.DeepEqual(deltas, []string{"ok"}) {
		t.Fatalf("deltas %q", deltas)
	}
	if disabled || agent.RetrySession().DisableStreaming {
		t.Fatal("a one-turn recovery must not latch streaming off for the session")
	}
	if !math.IsInf(staleDuringProbe[0], 1) || stale != 180.0 {
		t.Fatalf("stale during probe %v after %v", staleDuringProbe[0], stale)
	}
	agent.mu.Lock()
	statuses := append([]string(nil), agent.bufferedStatuses...)
	agent.mu.Unlock()
	if len(statuses) == 0 || !strings.Contains(statuses[0], "non-streaming retry succeeded") {
		t.Fatalf("buffered statuses %q", statuses)
	}
}

func TestStream5xxUnmaskedByProbe4xx(t *testing.T) {
	agent := unmask5xxAgent()
	c := unmask5xxCall(agent, map[string]any{"model": "m", "messages": []any{}})
	real := &APIError{StatusCode: 400, Message: "real validation message"}
	agent.s().dispatch = func(context.Context, map[string]any) (any, error) { return nil, real }
	handled := streamingCallHandleError(c, &APIError{StatusCode: 500, Message: "something went wrong"}, 2, 2)
	if handled {
		t.Fatal("the loop must stop so the unmasked error propagates")
	}
	response, err, _ := unmask5xxOutcome(c)
	if err != error(real) {
		t.Fatalf("result error %v, want the probe's 400 to replace the opaque 500", err)
	}
	if response != nil {
		t.Fatalf("response %v", response)
	}
}
