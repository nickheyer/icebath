package tools

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// Executor over a fake host whose only tool execution middleware is run and whose post hook statuses are recorded
func sequentialInterruptExecutor(t *testing.T, run func() string) (*Executor, *fakeExecutorHost, func() []string) {
	t.Helper()
	old := sequentialInterruptPollInterval
	sequentialInterruptPollInterval = 50 * time.Millisecond
	t.Cleanup(func() { sequentialInterruptPollInterval = old })
	t.Setenv("ICEBATH_CONCURRENT_TOOL_TIMEOUT_S", "0")
	host := &fakeExecutorHost{env: newTestToolEnv(t)}
	d := NewDispatcher(NewRegistry())
	d.ToolExecutionMiddleware = []ToolExecutionMiddleware{func(context.Context, *Env, string, map[string]any, map[string]any, ToolHookIDs, func(map[string]any) (string, error)) (string, error) {
		return run(), nil
	}}
	var mu sync.Mutex
	var statuses []string
	d.PostToolCall = []PostToolCallHook{func(_ context.Context, _ *Env, ev PostToolCallEvent) {
		mu.Lock()
		statuses = append(statuses, ev.Status)
		mu.Unlock()
	}}
	return NewExecutor(host, d), host, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), statuses...)
	}
}

func unusedExecute(context.Context, map[string]any) (string, error) { return "unused", nil }

func TestInterruptAbandonsNoncooperativeTool(t *testing.T) {
	started := make(chan struct{})
	var once sync.Once
	e, host, statuses := sequentialInterruptExecutor(t, func() string {
		once.Do(func() { close(started) })
		time.Sleep(30 * time.Second)
		return "late result"
	})
	go func() {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
		}
		time.Sleep(100 * time.Millisecond)
		host.interrupted.Store(true)
	}()
	t0 := time.Now()
	managed, err := e.runSequentialToolExecutionMiddleware(context.Background(), managedInput{name: "image_generate",
		args: map[string]any{"prompt": "x"}, taskID: "t", toolCallID: "call_1", execute: unusedExecute}, nil)
	elapsed := time.Since(t0)
	if err != nil {
		t.Fatal(err)
	}
	if !managed.Cancelled || !strings.Contains(managed.Result, "cancelled") {
		t.Fatalf("result %+v", managed)
	}
	if elapsed >= 10*time.Second {
		t.Fatalf("elapsed %s", elapsed)
	}
	found := false
	for _, s := range statuses() {
		found = found || s == "cancelled"
	}
	if !found {
		t.Fatalf("post statuses %v", statuses())
	}
}

func TestInterruptPrefersRealResultFromCooperativeTool(t *testing.T) {
	e, host, _ := sequentialInterruptExecutor(t, func() string {
		time.Sleep(300 * time.Millisecond)
		return "real result"
	})
	host.interrupted.Store(true)
	managed, err := e.runSequentialToolExecutionMiddleware(context.Background(), managedInput{name: "web_search",
		args: map[string]any{}, taskID: "t", toolCallID: "call_2", execute: unusedExecute}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if managed.Result != "real result" || managed.Cancelled {
		t.Fatalf("result %+v", managed)
	}
}
