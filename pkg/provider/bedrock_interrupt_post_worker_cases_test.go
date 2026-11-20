package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const bedrockPostWorkerModel = "anthropic.claude-3-sonnet-20240229-v1:0"

func bedrockPostWorkerAgent() *fakeChatAgent {
	return &fakeChatAgent{provider: "bedrock", apiMode: "bedrock_converse", model: bedrockPostWorkerModel}
}

func bedrockPostWorkerKwargs() map[string]any {
	return map[string]any{"__bedrock_region__": "us-east-1", "__bedrock_converse__": true, "modelId": bedrockPostWorkerModel,
		"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"text": "hi"}}}}}
}

func writeBedrockEndTurnStream(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
	w.WriteHeader(200)
	_, _ = w.Write(bc2Frame(t, "messageStart", map[string]any{"role": "assistant"}))
	_, _ = w.Write(bc2Frame(t, "messageStop", map[string]any{"stopReason": "end_turn"}))
	w.(http.Flusher).Flush()
}

type closeRecordingBody struct {
	io.ReadCloser
	closed *atomic.Bool
}

func (b closeRecordingBody) Close() error {
	b.closed.Store(true)
	return b.ReadCloser.Close()
}

type closeRecordingTransport struct {
	next   http.RoundTripper
	closed *atomic.Bool
}

func (rt closeRecordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := rt.next.RoundTrip(req)
	if err == nil && strings.HasSuffix(req.URL.Path, "/converse-stream") {
		resp.Body = closeRecordingBody{ReadCloser: resp.Body, closed: rt.closed}
	}
	return resp, err
}

func recordBedrockInterruptedWaits(t *testing.T) func() []bool {
	t.Helper()
	var mu sync.Mutex
	var waits []bool
	prev := recordInterruptedProviderWaitFn
	recordInterruptedProviderWaitFn = func(ctx context.Context, elapsed float64, responseStarted bool) bool {
		mu.Lock()
		waits = append(waits, responseStarted)
		mu.Unlock()
		return prev(ctx, elapsed, responseStarted)
	}
	t.Cleanup(func() { recordInterruptedProviderWaitFn = prev })
	return func() []bool {
		mu.Lock()
		defer mu.Unlock()
		return append([]bool(nil), waits...)
	}
}

func TestBedrockStreamInterruptNotSwallowedPostWorker(t *testing.T) {
	agent := bedrockPostWorkerAgent()
	bedrockStreamServer(t, func(w http.ResponseWriter) bool {
		agent.mu.Lock()
		agent.interrupted = true
		agent.mu.Unlock()
		writeBedrockEndTurnStream(t, w)
		return false
	})
	prevPoll := bedrockStreamPoll
	bedrockStreamPoll = 10 * time.Second
	t.Cleanup(func() { bedrockStreamPoll = prevPoll })
	waits := recordBedrockInterruptedWaits(t)
	out, err := BedrockStreamCall(t.Context(), agent, bedrockPostWorkerKwargs(), nil)
	var interrupted *InterruptedError
	if !errors.As(err, &interrupted) || out != nil {
		t.Fatalf("partial response swallowed the interrupt: out %#v err %v", out, err)
	}
	if !strings.Contains(err.Error(), "post-worker") {
		t.Fatalf("interrupt was not raised by the post-worker re-check: %v", err)
	}
	if got := waits(); len(got) != 1 || got[0] {
		t.Fatalf("interrupted wait records %v want one with response_started false", got)
	}
}

func TestBedrockStreamReturnsNormallyWhenNotInterrupted(t *testing.T) {
	agent := bedrockPostWorkerAgent()
	bedrockStreamServer(t, func(w http.ResponseWriter) bool {
		writeBedrockEndTurnStream(t, w)
		return false
	})
	client, err := bedrockRuntimeClient(t.Context(), "us-east-1", bedrockPostWorkerModel)
	if err != nil {
		t.Fatal(err)
	}
	var closed atomic.Bool
	next := client.http.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	client.http.Transport = closeRecordingTransport{next: next, closed: &closed}
	t.Cleanup(func() { client.http.Transport = next })
	waits := recordBedrockInterruptedWaits(t)
	out, err := BedrockStreamCall(t.Context(), agent, bedrockPostWorkerKwargs(), nil)
	if err != nil {
		t.Fatalf("guard fired without an interrupt: %v", err)
	}
	result, ok := out.(*bedrockConverseResult)
	if !ok || result == nil || result.FinishReason != "stop" {
		t.Fatalf("response %#v", out)
	}
	if len(waits()) != 0 {
		t.Fatalf("interrupted wait recorded without an interrupt: %v", waits())
	}
	if !closed.Load() {
		t.Fatal("provider stream was left open after the call returned")
	}
}
