package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Real OpenAI wire SSE server recording every chat completion body, optionally stalling after the first chunk
type inlineWire struct {
	mu       sync.Mutex
	requests []map[string]any
	stall    bool
	hits     chan struct{}
	base     string
}

func newInlineWire(t *testing.T, stallAfterFirstChunk bool) *inlineWire {
	t.Helper()
	w := &inlineWire{stall: stallAfterFirstChunk, hits: make(chan struct{}, 16)}
	chunk := func(delta map[string]any, finish any, usage map[string]any) string {
		row := map[string]any{"id": "c1", "object": "chat.completion.chunk", "created": 1, "model": "m",
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
		if usage != nil {
			row["usage"] = usage
		}
		raw, _ := json.Marshal(row)
		return "data: " + string(raw) + "\n\n"
	}
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			http.NotFound(rw, r)
			return
		}
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		w.mu.Lock()
		w.requests = append(w.requests, body)
		w.mu.Unlock()
		w.hits <- struct{}{}
		rw.Header().Set("Content-Type", "text/event-stream")
		rw.WriteHeader(http.StatusOK)
		flusher, _ := rw.(http.Flusher)
		flush := func() {
			if flusher != nil {
				flusher.Flush()
			}
		}
		_, _ = io.WriteString(rw, chunk(map[string]any{"role": "assistant", "content": "hello"}, nil, nil))
		flush()
		if w.stall {
			for i := 0; i < 400; i++ {
				time.Sleep(50 * time.Millisecond)
				if _, err := io.WriteString(rw, ": keepalive\n\n"); err != nil {
					return
				}
				flush()
			}
			return
		}
		_, _ = io.WriteString(rw, chunk(map[string]any{"content": " world"}, nil, nil))
		_, _ = io.WriteString(rw, chunk(map[string]any{}, "stop", map[string]any{"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5}))
		_, _ = io.WriteString(rw, "data: [DONE]\n\n")
		flush()
	}))
	t.Cleanup(srv.Close)
	w.base = srv.URL + "/v1"
	return w
}

// Chat completion bodies the wire received so far
func (w *inlineWire) completions() []map[string]any {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []map[string]any
	for _, r := range w.requests {
		if _, ok := r["messages"]; ok {
			out = append(out, r)
		}
	}
	return out
}

// Custom endpoint agent on the given platform like tests/agent/test_direct_contexts_stream_inline.py _make_agent
func inlineWireAgent(t *testing.T, baseURL, platform string) (*AIAgent, context.Context) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	env := &tools.Env{Home: h, Cwd: h}
	ctx := tools.WithToolEnv(context.Background(), env)
	a, err := NewAIAgent(ctx, AIAgentOptions{APIKey: "test-key", BaseURL: baseURL, Model: "m", Provider: "custom", Platform: platform,
		QuietMode: true, SkipContextFiles: true, SkipMemory: true, EnabledToolsets: []string{}, MaxIterations: new(1), Cwd: h, Env: env})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return a, ctx
}

// Records the goroutine that constructs the request client, the Python spy on _create_request_openai_client
func spyClientGoroutine(t *testing.T) *uint64 {
	t.Helper()
	var issuedOn uint64
	prev := openAIClientConstructor
	openAIClientConstructor = func(kwargs map[string]any) (any, error) {
		issuedOn = pythread.GoroutineID()
		return prev(kwargs)
	}
	t.Cleanup(func() { openAIClientConstructor = prev })
	return &issuedOn
}

func inlineKwargs() map[string]any {
	return map[string]any{"model": "m", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
}

// Content and finish reason of the first choice of a streamed OpenAI shaped response
func inlineResponseText(t *testing.T, resp any) (string, string) {
	t.Helper()
	content, _ := parityAt(t, parityJSON(t, resp), "choices", 0, "message", "content").(string)
	finish, _ := parityAt(t, parityJSON(t, resp), "choices", 0, "finish_reason").(string)
	return content, finish
}

func TestDirectContextsStreamOnTheWireAndOnTheCallingThread(t *testing.T) {
	for _, platform := range []string{"subagent", "cron"} {
		t.Run(platform, func(t *testing.T) {
			wire := newInlineWire(t, false)
			a, ctx := inlineWireAgent(t, wire.base, platform)
			if !provider.ShouldUseDirectAPICall(ctx, a.provider, a.platform, provider.APIMode(a.apiMode)) {
				t.Fatalf("%s must route inline", platform)
			}
			issuedOn := spyClientGoroutine(t)
			resp, err := a.interruptibleStreamingAPICall(ctx, inlineKwargs(), nil)
			if err != nil {
				t.Fatal(err)
			}
			completions := wire.completions()
			if len(completions) == 0 {
				t.Fatal("no chat completion reached the wire")
			}
			if completions[len(completions)-1]["stream"] != true {
				t.Fatalf("%s turn went out non-streaming: stream=%v", platform, completions[len(completions)-1]["stream"])
			}
			if *issuedOn != pythread.GoroutineID() {
				t.Fatalf("request dispatched from goroutine %d, caller is %d", *issuedOn, pythread.GoroutineID())
			}
			content, finish := inlineResponseText(t, resp)
			if content != "hello world" || finish != "stop" {
				t.Fatalf("content %q finish %q", content, finish)
			}
		})
	}
}

func TestInteractivePlatformStillUsesTheWorkerThread(t *testing.T) {
	wire := newInlineWire(t, false)
	a, ctx := inlineWireAgent(t, wire.base, "cli")
	if provider.ShouldUseDirectAPICall(ctx, a.provider, a.platform, provider.APIMode(a.apiMode)) {
		t.Fatal("cli must keep the interrupt worker")
	}
	issuedOn := spyClientGoroutine(t)
	resp, err := a.interruptibleStreamingAPICall(ctx, inlineKwargs(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if *issuedOn == 0 || *issuedOn == pythread.GoroutineID() {
		t.Fatalf("request dispatched from goroutine %d, caller is %d", *issuedOn, pythread.GoroutineID())
	}
	if content, _ := inlineResponseText(t, resp); content != "hello world" {
		t.Fatalf("content %q", content)
	}
}

func TestInlineStreamStaleDetectorStillFiresFromMonitorThread(t *testing.T) {
	t.Setenv(provider.StreamStaleTimeoutEnv, "1.0")
	t.Setenv("ICEBATH_STREAM_RETRIES", "0")
	wire := newInlineWire(t, true)
	a, ctx := inlineWireAgent(t, wire.base, "subagent")
	started := time.Now()
	_, err := a.interruptibleStreamingAPICall(ctx, inlineKwargs(), nil)
	elapsed := time.Since(started)
	var te *provider.AuxTransportError
	if !errors.As(err, &te) || te.Class != "RemoteProtocolError" {
		t.Fatalf("err %T %v", err, err)
	}
	if elapsed >= 6*time.Second {
		t.Fatalf("inline stream was not bounded by the stale detector (%.1fs)", elapsed.Seconds())
	}
}

func TestInlineStreamCrossThreadInterruptAbortsPromptly(t *testing.T) {
	t.Setenv(provider.StreamStaleTimeoutEnv, "60")
	t.Setenv("ICEBATH_STREAM_RETRIES", "0")
	wire := newInlineWire(t, true)
	a, ctx := inlineWireAgent(t, wire.base, "cron")
	type outcome struct {
		kind    string
		elapsed time.Duration
	}
	done := make(chan outcome, 1)
	go func() {
		t0 := time.Now()
		_, err := a.interruptibleStreamingAPICall(ctx, inlineKwargs(), nil)
		kind := "returned"
		var ie *provider.InterruptedError
		switch {
		case err == nil:
		case errors.As(err, &ie):
			kind = "InterruptedError"
		default:
			kind = fmt.Sprintf("%T", err)
		}
		done <- outcome{kind, time.Since(t0)}
	}()
	select {
	case <-wire.hits:
	case <-time.After(5 * time.Second):
		t.Fatal("request never reached the wire")
	}
	time.Sleep(300 * time.Millisecond)
	a.Interrupt("test interrupt")
	select {
	case out := <-done:
		if out.kind != "InterruptedError" || out.elapsed >= 5*time.Second {
			t.Fatalf("outcome %+v", out)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("inline stream did not unwind after interrupt")
	}
}
