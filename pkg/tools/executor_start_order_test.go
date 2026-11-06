package tools

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/state"
)

type dispatchStamp struct {
	name string
	at   time.Time
}

// Executor whose guardrail check wedges one tool after the authorization gate until stop closes and whose tools record each dispatch
func wedgedExecutor(t *testing.T, wedged string, gateS float64, batchTimeout string) (*Executor, *fakeExecutorHost, func() []dispatchStamp, chan struct{}) {
	t.Helper()
	old := startOrderGateTimeoutS
	if gateS > 0 {
		startOrderGateTimeoutS = gateS
	}
	t.Cleanup(func() { startOrderGateTimeoutS = old })
	t.Setenv("ICEBATH_CONCURRENT_TOOL_TIMEOUT_S", batchTimeout)
	var mu sync.Mutex
	var stamps []dispatchStamp
	r := NewRegistry()
	for _, n := range []string{"tool_a", "tool_b", "tool_c"} {
		r.Register(Def{Name: n, Toolset: "probe", Schema: map[string]any{"name": n, "parameters": map[string]any{"type": "object"}},
			Handler: func(_ context.Context, c *Call) string {
				mu.Lock()
				stamps = append(stamps, dispatchStamp{c.Name, time.Now()})
				mu.Unlock()
				return `{"ok": "` + c.Name + `"}`
			}})
	}
	stop := make(chan struct{})
	var armed atomic.Bool
	wedgeEntered := make(chan struct{})
	d := NewDispatcher(r)
	d.PreToolCall = []PreToolCallHook{func(_ context.Context, _ *Env, name string, _ map[string]any, _ ToolHookIDs, _ []map[string]any) PreToolDirective {
		if name == wedged {
			armed.Store(true)
		} else {
			// Later tools reach the guardrail only after the wedged tool took the wedge, whatever the scheduler load
			select {
			case <-wedgeEntered:
			case <-stop:
			case <-time.After(10 * time.Second):
			}
			time.Sleep(50 * time.Millisecond)
		}
		return PreToolDirective{}
	}}
	host := &fakeExecutorHost{env: newTestToolEnv(t)}
	host.guardHook = func() {
		if armed.CompareAndSwap(true, false) {
			close(wedgeEntered)
			select {
			case <-stop:
			case <-time.After(30 * time.Second):
			}
		}
	}
	return NewExecutor(host, d), host, func() []dispatchStamp {
		mu.Lock()
		defer mu.Unlock()
		return append([]dispatchStamp(nil), stamps...)
	}, stop
}

func probeCalls(names ...string) []state.ToolCall {
	out := make([]state.ToolCall, len(names))
	for i, n := range names {
		out[i] = state.ToolCall{ID: "tc_" + strings.TrimPrefix(n, "tool_"), Type: "function", Function: state.ToolCallFunction{Name: n, Arguments: "{}"}}
	}
	return out
}

func stampNames(s []dispatchStamp) map[string]bool {
	out := map[string]bool{}
	for _, d := range s {
		out[d.name] = true
	}
	return out
}

func TestWedgedDispatchDoesNotStarveLaterTools(t *testing.T) {
	e, _, stamps, stop := wedgedExecutor(t, "tool_a", 0.3, "2.5")
	var messages []*state.Message
	e.ExecuteToolCallsConcurrent(context.Background(), probeCalls("tool_a", "tool_b", "tool_c"), &messages, "task", true)
	close(stop)
	names := stampNames(stamps())
	if !names["tool_b"] || !names["tool_c"] {
		t.Fatalf("later-ordered tools were starved by the wedged dispatch: %v", names)
	}
	for _, m := range messages {
		if (m.ToolCallID == "tc_b" || m.ToolCallID == "tc_c") && strings.Contains(m.Text(), "timed out") {
			t.Fatalf("%s ran but was still reported as timed out: %q", m.ToolCallID, m.Text())
		}
	}
}

func TestGateTimeoutStaysUnderTheBatchDeadline(t *testing.T) {
	e, _, stamps, stop := wedgedExecutor(t, "tool_a", 0, "2.0")
	var messages []*state.Message
	e.ExecuteToolCallsConcurrent(context.Background(), probeCalls("tool_a", "tool_b"), &messages, "task", true)
	close(stop)
	if !stampNames(stamps())["tool_b"] {
		t.Fatal("gate outlived the batch deadline, so tool_b was blamed without running")
	}
	if got := startOrderGateTimeout(2 * time.Second); got != time.Second {
		t.Fatalf("clamp %v", got)
	}
	if got := startOrderGateTimeout(0); got != 120*time.Second {
		t.Fatalf("disabled deadline keeps the stock bound, got %v", got)
	}
}

func TestAbandonedBatchDoesNotDispatchLate(t *testing.T) {
	e, host, stamps, stop := wedgedExecutor(t, "tool_a", 30, "60")
	go func() {
		time.Sleep(500 * time.Millisecond)
		host.Interrupt("user pressed stop", ReasonUserInterrupt)
	}()
	var messages []*state.Message
	started := time.Now()
	e.ExecuteToolCallsConcurrent(context.Background(), probeCalls("tool_a", "tool_b"), &messages, "task", true)
	returnedAt := time.Now()
	close(stop)
	if returnedAt.Sub(started) >= 25*time.Second {
		t.Fatal("batch waited out the full gate timeout instead of releasing parked workers on abandonment")
	}
	time.Sleep(500 * time.Millisecond)
	for _, s := range stamps() {
		if s.at.After(returnedAt) {
			t.Fatalf("tool dispatched after the batch was abandoned: %s", s.name)
		}
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if n := len(host.currentTool); n > 0 && host.currentTool[n-1] != "" {
		t.Fatalf("current tool left pointing at a dead tool: %q", host.currentTool[n-1])
	}
}

func TestDictErrorResultReachesTheModelFromAConcurrentWorker(t *testing.T) {
	r := NewRegistry()
	r.Register(Def{Name: "terminal", Toolset: "terminal", Schema: map[string]any{"name": "terminal", "parameters": map[string]any{"type": "object"}},
		Handler: func(context.Context, *Call) string { return `{"exit_code": 2, "output": "", "error": "boom"}` }})
	e := NewExecutor(&fakeExecutorHost{env: newTestToolEnv(t)}, NewDispatcher(r))
	var messages []*state.Message
	e.ExecuteToolCallsConcurrent(context.Background(), []state.ToolCall{{ID: "tc_t", Type: "function", Function: state.ToolCallFunction{Name: "terminal", Arguments: "{}"}}}, &messages, "task", true)
	if len(messages) != 1 || strings.Contains(messages[0].Text(), "thread did not return a result") || !strings.Contains(messages[0].Text(), "boom") {
		t.Fatalf("messages %v", messages)
	}
}
