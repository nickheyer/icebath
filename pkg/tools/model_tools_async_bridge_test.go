package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/state"
)

// Registry dispatcher whose single tool runs handler under the given name
func bridgeDispatcher(t *testing.T, name string, handler Handler) (*Dispatcher, *Env) {
	t.Helper()
	r := NewRegistry()
	r.Register(Def{Name: name, Toolset: "web", Schema: map[string]any{"name": name, "description": name, "parameters": map[string]any{"type": "object"}},
		Check: func(*Env) bool { return true }, Handler: handler})
	return NewDispatcher(r), newTestToolEnv(t)
}

// Fails the test when fn has not returned before the deadline
func returnsWithin(t *testing.T, limit time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(limit):
		t.Fatalf("%s blocked for more than %s", what, limit)
	}
}

func TestCachedTransportSurvivesBetweenDispatches(t *testing.T) {
	var newConns atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			newConns.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()
	var mu sync.Mutex
	var client *http.Client
	var transports []http.RoundTripper
	d, env := bridgeDispatcher(t, "cached_transport_probe", func(ctx context.Context, call *Call) string {
		mu.Lock()
		if client == nil {
			client = &http.Client{Transport: &http.Transport{}, Timeout: 10 * time.Second}
		}
		c := client
		transports = append(transports, c.Transport)
		mu.Unlock()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
		if err != nil {
			return toolErrorJSON(err.Error(), nil)
		}
		resp, err := c.Do(req)
		if err != nil {
			return toolErrorJSON(err.Error(), nil)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return JSON(map[string]any{"body": string(body)})
	})
	ctx := WithDispatcher(context.Background(), d)
	for i := range 2 {
		out := decodeModelToolsResult(t, HandleFunctionCall(ctx, &Call{Name: "cached_transport_probe", Args: map[string]any{}, ToolCallID: fmt.Sprintf("c%d", i), Env: env}))
		if out["body"] != "ok" {
			t.Fatalf("call %d result %v", i, out)
		}
	}
	if len(transports) != 2 || transports[0] != transports[1] {
		t.Fatalf("cached transport changed between calls %v", transports)
	}
	if n := newConns.Load(); n != 1 {
		t.Fatalf("the cached transport's connection was torn down between calls, %d connections opened", n)
	}
	client.CloseIdleConnections()
}

func TestConcurrentWorkersKeepDistinctPerCallState(t *testing.T) {
	type seen struct {
		worker               WorkerID
		callID, boundCallID  string
		innerBound, afterOwn string
		dispatcher           *Dispatcher
	}
	var mu sync.Mutex
	got := map[string]seen{}
	barrier := make(chan struct{})
	var arrived atomic.Int32
	release := func() {
		if arrived.Add(1) == 3 {
			close(barrier)
		}
	}
	var innerSeen sync.Map
	reg := NewRegistry()
	reg.Register(handlerDef("inner_state_probe", func(ctx context.Context, call *Call) string {
		id, _ := loadSessionString(call.Env, "approval.tool_call_id")
		innerSeen.Store(call.ToolCallID, id)
		return `{"inner": true}`
	}))
	reg.Register(handlerDef("worker_state_probe", func(ctx context.Context, call *Call) string {
		release()
		select {
		case <-barrier:
		case <-time.After(10 * time.Second):
			return toolErrorJSON("workers never ran together", nil)
		}
		bound, _ := loadSessionString(call.Env, "approval.tool_call_id")
		innerID := call.ToolCallID + "-inner"
		HandleFunctionCall(ctx, &Call{Name: "inner_state_probe", Args: map[string]any{}, ToolCallID: innerID, Env: call.Env})
		inner, _ := innerSeen.Load(innerID)
		after, _ := loadSessionString(call.Env, "approval.tool_call_id")
		mu.Lock()
		got[call.ToolCallID] = seen{worker: WorkerOf(ctx), callID: call.ToolCallID, boundCallID: bound, innerBound: fmt.Sprint(inner), afterOwn: after, dispatcher: DispatcherOf(ctx)}
		mu.Unlock()
		return `{"ok": true}`
	}))
	d := NewDispatcher(reg)
	host := &fakeExecutorHost{env: newTestToolEnv(t)}
	e := NewExecutor(host, d)
	var messages []*state.Message
	returnsWithin(t, 30*time.Second, "concurrent batch", func() {
		if err := e.ExecuteToolCallsConcurrent(context.Background(), toolCallsFor("worker_state_probe", "w1", "w2", "w3"), &messages, "task", true); err != nil {
			t.Errorf("batch error %v", err)
		}
	})
	if len(got) != 3 {
		t.Fatalf("workers recorded %d of 3, messages %d", len(got), len(messages))
	}
	workers := map[WorkerID]bool{}
	for id, s := range got {
		workers[s.worker] = true
		if s.boundCallID != id || s.afterOwn != id {
			t.Fatalf("worker %s read another call's correlation id, bound %q after nested call %q", id, s.boundCallID, s.afterOwn)
		}
		if s.innerBound != id+"-inner" {
			t.Fatalf("nested dispatch from %s bound %q", id, s.innerBound)
		}
		if s.dispatcher != d {
			t.Fatalf("worker %s ran without its dispatcher bound", id)
		}
	}
	if len(workers) != 3 {
		t.Fatalf("concurrent calls shared a worker identity %v", workers)
	}
	if id, ok := loadSessionString(host.env, "approval.tool_call_id"); ok || id != "" {
		t.Fatalf("a per call correlation id leaked into the session values %q", id)
	}
}

func TestDispatchFromInsideAnotherHandlerDoesNotBlock(t *testing.T) {
	reg := NewRegistry()
	reg.Register(handlerDef("simple_probe", func(context.Context, *Call) string { return `{"value": 42}` }))
	reg.Register(handlerDef("outer_probe", func(ctx context.Context, call *Call) string {
		direct := HandleFunctionCall(ctx, &Call{Name: "simple_probe", Args: map[string]any{}, Env: call.Env})
		fromGoroutine := make(chan string, 1)
		go func() {
			fromGoroutine <- HandleFunctionCall(ctx, &Call{Name: "simple_probe", Args: map[string]any{}, Env: call.Env})
		}()
		return JSON(map[string]any{"direct": direct, "goroutine": <-fromGoroutine})
	}))
	d := NewDispatcher(reg)
	env := newTestToolEnv(t)
	var out map[string]any
	returnsWithin(t, 10*time.Second, "nested dispatch through HandleFunctionCall", func() {
		out = decodeModelToolsResult(t, HandleFunctionCall(WithDispatcher(context.Background(), d), &Call{Name: "outer_probe", Args: map[string]any{}, Env: env}))
	})
	if out["direct"] != `{"value": 42}` || out["goroutine"] != `{"value": 42}` {
		t.Fatalf("nested results %v", out)
	}
	host := &fakeExecutorHost{env: env}
	var messages []*state.Message
	returnsWithin(t, 10*time.Second, "nested dispatch inside an executor worker", func() {
		if err := NewExecutor(host, d).ExecuteToolCallsConcurrent(context.Background(), toolCallsFor("outer_probe", "o1", "o2"), &messages, "task", true); err != nil {
			t.Errorf("batch error %v", err)
		}
	})
	for _, m := range messages {
		if m.Role == "tool" && !strings.Contains(fmt.Sprint(m.Content), `{\"value\": 42}`) {
			t.Fatalf("worker result %v", m.Content)
		}
	}
}

func TestTimedOutHandlerDoesNotBlockCallerReturn(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	var started atomic.Int32
	e, _, _ := registryExecutor(t, handlerDef("stuck_probe", func(context.Context, *Call) string {
		started.Add(1)
		<-release
		return "late"
	}))
	t.Setenv("ICEBATH_CONCURRENT_TOOL_TIMEOUT_S", "0.5")
	for _, run := range []struct {
		name string
		fn   func(context.Context, []state.ToolCall, *[]*state.Message, string, bool) error
	}{{"sequential", e.ExecuteToolCallsSequential}, {"concurrent", e.ExecuteToolCallsConcurrent}} {
		var messages []*state.Message
		begin := time.Now()
		returnsWithin(t, 10*time.Second, run.name+" executor with a handler ignoring cancellation", func() {
			if err := run.fn(context.Background(), toolCallsFor("stuck_probe", run.name+"-1"), &messages, "task", true); err != nil {
				t.Errorf("%s error %v", run.name, err)
			}
		})
		if elapsed := time.Since(begin); elapsed > 5*time.Second {
			t.Fatalf("%s caller blocked %s past a 0.5s tool timeout", run.name, elapsed)
		}
		var content string
		for _, m := range messages {
			if m.Role == "tool" {
				content = fmt.Sprint(m.Content)
			}
		}
		if content == "" || strings.Contains(content, "late") || !strings.Contains(strings.ToLower(content), "timed out") {
			t.Fatalf("%s timeout result %q", run.name, content)
		}
	}
	if started.Load() != 2 {
		t.Fatalf("handler started %d times", started.Load())
	}
}

func TestTimedOutHandlerContextIsCancelled(t *testing.T) {
	cancelObserved := make(chan string, 2)
	e, _, _ := registryExecutor(t, handlerDef("cancellable_probe", func(ctx context.Context, call *Call) string {
		select {
		case <-ctx.Done():
			cancelObserved <- call.ToolCallID
			return "cancelled"
		case <-time.After(60 * time.Second):
			return "finished"
		}
	}))
	t.Setenv("ICEBATH_CONCURRENT_TOOL_TIMEOUT_S", "0.5")
	for _, run := range []struct {
		name string
		fn   func(context.Context, []state.ToolCall, *[]*state.Message, string, bool) error
	}{{"sequential", e.ExecuteToolCallsSequential}, {"concurrent", e.ExecuteToolCallsConcurrent}} {
		t.Run(run.name, func(t *testing.T) {
			var messages []*state.Message
			begin := time.Now()
			if err := run.fn(context.Background(), toolCallsFor("cancellable_probe", run.name), &messages, "task", true); err != nil {
				t.Fatalf("error %v", err)
			}
			if elapsed := time.Since(begin); elapsed > 3*time.Second {
				t.Fatalf("caller blocked %s", elapsed)
			}
			select {
			case id := <-cancelObserved:
				if id != run.name {
					t.Fatalf("cancellation reached %q instead of %q", id, run.name)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("timed out handler never saw its context cancelled, its goroutine leaked")
			}
		})
	}
}

// PNG data URL of an 8 by 8 blue square
func blueSquareDataURL(t *testing.T) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for i := 0; i < 64; i++ {
		img.Set(i%8, i/8, color.RGBA{0, 0, 255, 255})
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

func TestConsecutiveImageDispatchesKeepTheAuxClientAlive(t *testing.T) {
	aux := &fakeVisionAux{reply: func(int, []*state.Message, AuxOptions) (string, error) { return "A cat sitting on a chair.", nil }}
	env := visionEnv(t, aux)
	d := NewDispatcher(Default)
	args := map[string]any{"image_url": blueSquareDataURL(t), "question": "Describe"}
	first := decodeModelToolsResult(t, d.RegistryDispatch(context.Background(), &Call{Name: "vision_analyze", Args: args, Env: env}))
	client := env.Aux
	second := decodeModelToolsResult(t, d.RegistryDispatch(context.Background(), &Call{Name: "vision_analyze", Args: args, Env: env}))
	if first["success"] != true || second["success"] != true {
		t.Fatalf("first %v second %v", first, second)
	}
	if analysis, _ := first["analysis"].(string); !strings.Contains(strings.ToLower(analysis), "cat") {
		t.Fatalf("analysis %v", first["analysis"])
	}
	if env.Aux != client || aux.count() != 2 {
		t.Fatalf("aux client replaced or skipped between dispatches, completions %d", aux.count())
	}
}
