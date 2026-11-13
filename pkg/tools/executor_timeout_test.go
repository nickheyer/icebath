package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/state"
)

type callPost struct{ callID, name, status, errorType, errorMessage string }

// Executor over a fresh registry holding defs with every terminal post hook recorded
func registryExecutor(t *testing.T, defs ...Def) (*Executor, *fakeExecutorHost, func() []callPost) {
	t.Helper()
	reg := NewRegistry()
	for _, d := range defs {
		reg.Register(d)
	}
	host := &fakeExecutorHost{env: newTestToolEnv(t)}
	d := NewDispatcher(reg)
	var mu sync.Mutex
	var posts []callPost
	d.PostToolCall = []PostToolCallHook{func(_ context.Context, _ *Env, ev PostToolCallEvent) {
		mu.Lock()
		posts = append(posts, callPost{ev.IDs.ToolCallID, ev.ToolName, ev.Status, ev.ErrorType, ev.ErrorMessage})
		mu.Unlock()
	}}
	return NewExecutor(host, d), host, func() []callPost {
		mu.Lock()
		defer mu.Unlock()
		return append([]callPost(nil), posts...)
	}
}

func handlerDef(name string, handler func(ctx context.Context, call *Call) string) Def {
	return Def{Name: name, Toolset: "web", Schema: map[string]any{"name": name, "parameters": map[string]any{"type": "object"}},
		Check: func(*Env) bool { return true }, Handler: handler}
}

func toolCallsFor(name string, ids ...string) []state.ToolCall {
	out := make([]state.ToolCall, len(ids))
	for i, id := range ids {
		out[i] = state.ToolCall{ID: id, Type: "function", Function: state.ToolCallFunction{Name: name, Arguments: "{}"}}
	}
	return out
}

func swapDuration(t *testing.T, target *time.Duration, v time.Duration) {
	old := *target
	*target = v
	t.Cleanup(func() { *target = old })
}

func TestConcurrentToolCallHeartbeat(t *testing.T) {
	swapDuration(t, &toolActivityHeartbeatInterval, 50*time.Millisecond)
	e, host, _ := registryExecutor(t, handlerDef("tool_a", func(_ context.Context, call *Call) string {
		time.Sleep(250 * time.Millisecond)
		return `{"ok": "tool_a"}`
	}))
	var messages []*state.Message
	if err := e.ExecuteToolCallsConcurrent(context.Background(), toolCallsFor("tool_a", "tc_a"), &messages, "task", true); err != nil {
		t.Fatal(err)
	}
	if n := host.touches(""); n < 3 {
		t.Fatalf("expected mid-call heartbeats, got %d", n)
	}
}

func TestHeartbeatExitsOnceWorkerIsInterrupted(t *testing.T) {
	host := &fakeExecutorHost{env: newTestToolEnv(t)}
	worker := NewWorkerID()
	stop := make(chan struct{})
	defer close(stop)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		runToolActivityHeartbeat(host, stop, "tool running: terminal", 50*time.Millisecond, worker)
	}()
	defer SetInterrupt(worker, false, "")
	time.Sleep(120 * time.Millisecond)
	if host.touches("tool running: terminal") == 0 {
		t.Fatal("heartbeat never stamped while the worker was live")
	}
	SetInterrupt(worker, true, "")
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("heartbeat kept running after its worker was abandoned")
	}
	n := host.touches("")
	time.Sleep(120 * time.Millisecond)
	if host.touches("") != n {
		t.Fatal("heartbeat stamped after exit")
	}
}

func TestSequentialTimeoutStopsAbandonedWorkersHeartbeat(t *testing.T) {
	swapDuration(t, &toolActivityHeartbeatInterval, 50*time.Millisecond)
	swapDuration(t, &sequentialInterruptPollInterval, 50*time.Millisecond)
	t.Setenv("ICEBATH_CONCURRENT_TOOL_TIMEOUT_S", "0.3")
	host := &fakeExecutorHost{env: newTestToolEnv(t)}
	e := NewExecutor(host, NewDispatcher(NewRegistry()))
	release := make(chan struct{})
	defer close(release)
	managed, err := e.runSequentialToolExecutionMiddleware(context.Background(), managedInput{name: "terminal", args: map[string]any{},
		taskID: "t", toolCallID: "tc1", execute: func(context.Context, map[string]any) (string, error) {
			<-release
			return "late", nil
		}}, nil)
	abandonedAt := time.Now()
	if err != nil || managed == nil || !managed.TimedOut {
		t.Fatalf("managed %+v err %v", managed, err)
	}
	time.Sleep(time.Until(abandonedAt.Add(60 * time.Millisecond)))
	n := host.touches("tool running:")
	time.Sleep(300 * time.Millisecond)
	if late := host.touches("tool running:") - n; late != 0 {
		t.Fatalf("abandoned tool kept stamping activity %d times", late)
	}
}

func messageEffect(m *state.Message) string {
	v, _ := m.DisplayMetadata[EffectDispositionKey].(string)
	return v
}

func TestSequentialToolTimeoutEmitsResultAndContinues(t *testing.T) {
	firstStarted := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	var dispatched []string
	e, host, posts := registryExecutor(t, handlerDef("web_extract", func(_ context.Context, call *Call) string {
		mu.Lock()
		dispatched = append(dispatched, call.ToolCallID)
		mu.Unlock()
		if call.ToolCallID == "hung" {
			close(firstStarted)
			<-release
			return "late result"
		}
		return "second result"
	}))
	var flushes int
	host.flush = func([]*state.Message) state.FlushOutcome {
		mu.Lock()
		flushes++
		mu.Unlock()
		return state.FlushPersisted
	}
	t.Setenv("ICEBATH_CONCURRENT_TOOL_TIMEOUT_S", "1.0")
	var messages []*state.Message
	started := time.Now()
	err := e.ExecuteToolCallsSequential(context.Background(), toolCallsFor("web_extract", "hung", "next"), &messages, "task", true)
	close(release)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-firstStarted:
	default:
		t.Fatal("hung call never dispatched")
	}
	if time.Since(started) >= 10*time.Second {
		t.Fatal("sequential timeout did not bound the batch")
	}
	mu.Lock()
	order := strings.Join(dispatched, ",")
	gotFlushes := flushes
	mu.Unlock()
	if order != "hung,next" || len(messages) != 2 || messages[0].ToolCallID != "hung" || messages[1].ToolCallID != "next" {
		t.Fatalf("dispatched %s messages %d", order, len(messages))
	}
	if !strings.Contains(messageText(messages[0]), "timed out after 1.0s") || messageEffect(messages[0]) != "unknown" || messageText(messages[1]) != "second result" {
		t.Fatalf("contents %q (%s) %q", messageText(messages[0]), messageEffect(messages[0]), messageText(messages[1]))
	}
	var timeouts []callPost
	for _, p := range posts() {
		if p.errorType == "tool_timeout" {
			timeouts = append(timeouts, p)
		}
	}
	if len(timeouts) != 1 || timeouts[0].status != "timeout" || gotFlushes == 0 {
		t.Fatalf("timeout events %+v flushes %d", timeouts, gotFlushes)
	}
}

func TestSequentialToolTimeoutSuppressesLateTerminalEvent(t *testing.T) {
	release := make(chan struct{})
	firstReturned := make(chan struct{})
	var mu sync.Mutex
	count := 0
	e, _, posts := registryExecutor(t, handlerDef("web_extract", func(context.Context, *Call) string {
		mu.Lock()
		count++
		n := count
		mu.Unlock()
		if n == 1 {
			<-release
			close(firstReturned)
			return "late result"
		}
		return "second result"
	}))
	t.Setenv("ICEBATH_CONCURRENT_TOOL_TIMEOUT_S", "1.0")
	var messages []*state.Message
	err := e.ExecuteToolCallsSequential(context.Background(), toolCallsFor("web_extract", "hung", "next"), &messages, "task", true)
	close(release)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-firstReturned:
	case <-time.After(time.Second):
		t.Fatal("hung call never returned")
	}
	time.Sleep(200 * time.Millisecond)
	got := posts()
	if len(got) != 2 || got[0].callID != "hung" || got[0].errorType != "tool_timeout" || got[1].callID != "next" || got[1].errorType != "" {
		t.Fatalf("terminal events %+v", got)
	}
}

func TestSequentialToolInterruptHidesLifecycleCancelDetail(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	e, host, posts := registryExecutor(t, handlerDef("web_extract", func(context.Context, *Call) string {
		close(started)
		<-release
		return "late result"
	}))
	swapDuration(t, &sequentialInterruptPollInterval, 50*time.Millisecond)
	t.Setenv("ICEBATH_CONCURRENT_TOOL_TIMEOUT_S", "30")
	go func() {
		<-started
		host.Interrupt("Lifecycle cancellation requested: PRIVATE_LIFECYCLE_REASON_DO_NOT_COPY", "subagent cancellation requested")
	}()
	var messages []*state.Message
	if err := e.ExecuteToolCallsSequential(context.Background(), toolCallsFor("web_extract", "hung"), &messages, "task", true); err != nil {
		t.Fatal(err)
	}
	content := messageText(messages[0])
	if !strings.Contains(content, "subagent cancellation requested") || strings.Contains(content, "PRIVATE_LIFECYCLE_REASON_DO_NOT_COPY") || strings.Contains(content, "user interrupt") {
		t.Fatalf("content %q", content)
	}
	got := posts()
	if len(got) == 0 || strings.Contains(got[0].errorMessage, "PRIVATE_LIFECYCLE_REASON_DO_NOT_COPY") || got[0].errorType != "tool_interrupted" {
		t.Fatalf("terminal events %+v", got)
	}
}

// Asker that outlasts the sequential deadline before answering every question with A
type slowClarifyAsker struct{ delay time.Duration }

func (a slowClarifyAsker) Ask(_ context.Context, qs []Question) (FormResult, error) {
	time.Sleep(a.delay)
	answers := make([]Answer, len(qs))
	for i := range qs {
		answers[i] = Answer{Status: "answered", Text: "A"}
	}
	return FormResult{Outcome: "submitted", Answers: answers}, nil
}

func TestSequentialTimeoutDoesNotCutClarifyHumanWait(t *testing.T) {
	for _, clarifyTimeout := range []int{ResolveClarifyTimeout(config.Tree{}), 0} {
		clarify := Def{Name: "clarify", Toolset: "clarify", Schema: clarifySchema, Check: func(*Env) bool { return true },
			Handler: func(ctx context.Context, call *Call) string {
				return clarifyTool(ctx, call.Env, call.Args["questions"])
			}}
		e, host, posts := registryExecutor(t, clarify, handlerDef("web_extract", func(context.Context, *Call) string { return "second result" }))
		host.env.Config = config.Tree{"agent": map[string]any{"clarify_timeout": clarifyTimeout}}
		host.env.Ask = slowClarifyAsker{delay: 1300 * time.Millisecond}
		t.Setenv("ICEBATH_CONCURRENT_TOOL_TIMEOUT_S", "1.0")
		calls := []state.ToolCall{{ID: "clarify-1", Type: "function", Function: state.ToolCallFunction{Name: "clarify",
			Arguments: `{"questions": [{"question": "Pick one?", "choices": ["A", "B"]}]}`}}}
		calls = append(calls, toolCallsFor("web_extract", "next")...)
		var messages []*state.Message
		started := time.Now()
		if err := e.ExecuteToolCallsSequential(context.Background(), calls, &messages, "task", true); err != nil {
			t.Fatal(err)
		}
		if time.Since(started) >= 10*time.Second || len(messages) != 2 || messages[0].ToolCallID != "clarify-1" || messages[1].ToolCallID != "next" {
			t.Fatalf("clarify_timeout=%d messages %d", clarifyTimeout, len(messages))
		}
		var payload struct {
			Responses []struct {
				UserResponse any `json:"user_response"`
			} `json:"responses"`
		}
		if err := json.Unmarshal([]byte(messageText(messages[0])), &payload); err != nil || len(payload.Responses) == 0 || payload.Responses[0].UserResponse != "A" {
			t.Fatalf("clarify_timeout=%d payload %q err %v", clarifyTimeout, messageText(messages[0]), err)
		}
		if strings.Contains(messageText(messages[0]), "timed out") || messageText(messages[1]) != "second result" {
			t.Fatalf("clarify_timeout=%d contents %q %q", clarifyTimeout, messageText(messages[0]), messageText(messages[1]))
		}
		for _, p := range posts() {
			if p.errorType == "tool_timeout" {
				t.Fatalf("clarify_timeout=%d generic deadline fired %+v", clarifyTimeout, p)
			}
		}
	}
}

func TestSequentialToolPollSurfacesWorkerErrorInsteadOfLooping(t *testing.T) {
	host := &fakeExecutorHost{env: newTestToolEnv(t)}
	e := NewExecutor(host, NewDispatcher(NewRegistry()))
	done := make(chan struct{})
	close(done)
	started := time.Now()
	if st := e.pollSequentialFuture(context.Background(), done, "some_tool", started.Add(2*time.Second), true, started, NewAuthorizationGate(host.env, 0)); st != "done" {
		t.Fatalf("poll status %q", st)
	}
	t.Setenv("ICEBATH_CONCURRENT_TOOL_TIMEOUT_S", "2")
	stalled := errors.New("tool stream stalled")
	started = time.Now()
	_, err := e.runSequentialToolExecutionMiddleware(context.Background(), managedInput{name: "some_tool", args: map[string]any{}, taskID: "t", toolCallID: "tc1",
		execute: func(context.Context, map[string]any) (string, error) { return "", stalled }}, nil)
	if !errors.Is(err, stalled) {
		t.Fatalf("worker error not surfaced: %v", err)
	}
	if elapsed := time.Since(started); elapsed >= 500*time.Millisecond {
		t.Fatalf("sequential poll hung %v on a dead worker", elapsed)
	}
}
