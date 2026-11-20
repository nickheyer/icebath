package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Set once event with a bounded wait
type auxSSEGate struct {
	ch   chan struct{}
	once sync.Once
}

func newAuxSSEGate() *auxSSEGate { return &auxSSEGate{ch: make(chan struct{})} }

func (g *auxSSEGate) set() { g.once.Do(func() { close(g.ch) }) }

// True when the gate was set before the wait ran out
func (g *auxSSEGate) wait(d time.Duration) bool {
	select {
	case <-g.ch:
		return true
	case <-time.After(d):
		return false
	}
}

// Loopback keepalive SSE endpoint whose request model picks the script
func auxSSEServer(t *testing.T, gates map[string]*auxSSEGate) *httptest.Server {
	t.Helper()
	chunk := func(content string, finish any, usage map[string]any) string {
		delta := map[string]any{}
		if content != "" {
			delta["content"] = content
		}
		payload := map[string]any{"id": "c1", "object": "chat.completion.chunk", "created": 0, "model": "m",
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
		if usage != nil {
			payload["usage"] = usage
		}
		raw, _ := json.Marshal(payload)
		return "data: " + string(raw) + "\n\n"
	}
	frame := func(w http.ResponseWriter, payload string) {
		_, _ = io.WriteString(w, payload)
		w.(http.Flusher).Flush()
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		script, _ := req["model"].(string)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		if script == "openai/o3" || script == "slow" {
			time.Sleep(2 * time.Second)
		}
		if script == "b" {
			gates["b_started"].set()
		}
		frame(w, chunk("partial", nil, nil))
		if script == "usage" {
			frame(w, chunk("", "stop", map[string]any{"prompt_tokens": 100, "completion_tokens": 9, "total_tokens": 109}))
		}
		gate, ok := gates[script]
		if !ok {
			gate = gates["release"]
		}
		gate.wait(30 * time.Second)
		if script != "a" {
			frame(w, "data: [DONE]\n\n")
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Gates and a caller against the real SDK client over the loopback transport, the compression window 0.4s
func auxSSEFixture(t *testing.T, configYAML string) (map[string]*auxSSEGate, func(model, task string, viaRelaySeam bool, provider string) (any, error)) {
	t.Helper()
	ctx := auxMainTestHome(t, configYAML)
	gates := map[string]*auxSSEGate{}
	for _, name := range []string{"release", "a", "b", "b_started", "usage"} {
		gates[name] = newAuxSSEGate()
	}
	srv := auxSSEServer(t, gates)
	prevWindow, prevTask, prevLocal := auxStreamNoProgressTimeoutSeconds, auxGetTaskNoProgressTimeoutFn, auxIsLocalEndpointFn
	auxStreamNoProgressTimeoutSeconds = 3.0
	auxGetTaskNoProgressTimeoutFn = func(_ context.Context, task string) float64 {
		if task == "compression" {
			return 0.4
		}
		return 0
	}
	auxIsLocalEndpointFn = func(string) bool { return false }
	t.Cleanup(func() {
		for _, gate := range gates {
			gate.set()
		}
		auxStreamNoProgressTimeoutSeconds, auxGetTaskNoProgressTimeoutFn, auxIsLocalEndpointFn = prevWindow, prevTask, prevLocal
	})
	client := newAuxOpenAIClient("k", srv.URL+"/v1", auxOpenAIOptions{MaxRetries: auxSDKRetries(0)})
	t.Cleanup(func() { _ = client.Close() })
	call := func(model, task string, viaRelaySeam bool, provider string) (any, error) {
		request := map[string]any{"model": model, "messages": []any{}, "timeout": 20.0}
		hooked := AuxProgressHook(ctx, func() error { return nil })
		if viaRelaySeam {
			return auxRelaySyncCompletion(auxRelayAuxCallScope(hooked, task), client, request, provider, "", nil)
		}
		return auxCreateWithProgress(hooked, client, request, task, false)
	}
	return gates, call
}

func TestAggregateChatStreamSilentStreamFailsFastThroughTheRealTransport(t *testing.T) {
	for _, tc := range []struct {
		name         string
		viaRelaySeam bool
	}{{"primary", false}, {"recovery_rung", true}} {
		t.Run(tc.name, func(t *testing.T) {
			_, call := auxSSEFixture(t, "")
			started := time.Now()
			_, err := call("m", "compression", tc.viaRelaySeam, "")
			if err == nil || !auxIsTimeoutError(err) {
				t.Fatalf("err %v", err)
			}
			if time.Since(started) >= 5*time.Second {
				t.Fatalf("took %v", time.Since(started))
			}
			if !strings.Contains(err.Error(), "stalled: no new output for 0.4s") {
				t.Fatalf("err %v", err)
			}
			if !auxShouldSkipSameProviderRetry("compression", err) {
				t.Fatal("a stalled stream skips the same provider retry")
			}
		})
	}
}

func TestAggregateChatStreamWatchdogOnlyCutsItsOwnUnfinishedStream(t *testing.T) {
	t.Run("reasoning_before_first_token", func(t *testing.T) {
		gates, call := auxSSEFixture(t, "")
		gates["release"].set()
		result, err := call("openai/o3", "compression", false, "")
		if err != nil {
			t.Fatal(err)
		}
		if auxFirstMessage(t, result)["content"] != "partial" {
			t.Fatalf("result %v", result)
		}
	})
	t.Run("provider_stale_timeout_before_first_token", func(t *testing.T) {
		gates, call := auxSSEFixture(t, "providers:\n  openrouter:\n    stale_timeout_seconds: 900\n")
		gates["release"].set()
		t.Setenv("ICEBATH_STREAM_STALE_TIMEOUT", "0.5")
		result, err := call("slow", "compression", true, "openrouter")
		if err != nil {
			t.Fatal(err)
		}
		if auxFirstMessage(t, result)["content"] != "partial" {
			t.Fatalf("result %v", result)
		}
	})
	t.Run("stall_after_terminal_usage", func(t *testing.T) {
		_, call := auxSSEFixture(t, "")
		result, err := call("usage", "compression", false, "")
		if err != nil {
			t.Fatal(err)
		}
		if auxFirstMessage(t, result)["content"] != "partial" {
			t.Fatalf("result %v", result)
		}
		usage, _ := auxField(result, "usage", nil).(map[string]any)
		if fmt.Sprint(usage["prompt_tokens"]) != "100" || fmt.Sprint(usage["completion_tokens"]) != "9" {
			t.Fatalf("usage %v", usage)
		}
	})
	t.Run("late_timer_after_pool_reuse", func(t *testing.T) {
		gates, call := auxSSEFixture(t, "")
		paused, resume := newAuxSSEGate(), newAuxSSEGate()
		prevShutdown := auxChatShutdownSocketFn
		auxChatShutdownSocketFn = func(conn net.Conn) {
			if paused.wait(0) {
				auxShutdownConn(conn)
				return
			}
			paused.set()
			resume.wait(3 * time.Second)
			auxShutdownConn(conn)
		}
		t.Cleanup(func() {
			resume.set()
			auxChatShutdownSocketFn = prevShutdown
		})
		type outcome struct {
			result any
			err    error
		}
		outcomes := map[string]chan outcome{"a": make(chan outcome, 1), "b": make(chan outcome, 1)}
		run := func(name, model, task string) {
			result, err := call(model, task, false, "")
			outcomes[name] <- outcome{result, err}
		}
		go run("a", "a", "compression")
		if !paused.wait(3 * time.Second) {
			t.Fatal("the watchdog never reached the socket shutdown")
		}
		gates["a"].set()
		var a outcome
		aDone := false
		select {
		case a = <-outcomes["a"]:
			aDone = true
		case <-time.After(300 * time.Millisecond):
		}
		go run("b", "b", "")
		if !gates["b_started"].wait(3 * time.Second) {
			t.Fatal("attempt b never reached the server")
		}
		resume.set()
		time.Sleep(200 * time.Millisecond)
		gates["b"].set()
		var b outcome
		select {
		case b = <-outcomes["b"]:
		case <-time.After(5 * time.Second):
			t.Fatal("attempt b never finished")
		}
		if !aDone {
			select {
			case a = <-outcomes["a"]:
			case <-time.After(5 * time.Second):
				t.Fatal("attempt a never finished")
			}
		}
		if a.err == nil || !auxIsTimeoutError(a.err) {
			t.Fatalf("attempt a %v %v", a.result, a.err)
		}
		if b.err != nil {
			t.Fatalf("attempt b %v", b.err)
		}
		if auxFirstMessage(t, b.result)["content"] != "partial" {
			t.Fatalf("attempt b result %v", b.result)
		}
	})
}
