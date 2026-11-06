package tools

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/state"
)

type postRecord struct{ name, status, errorType, errorMessage, result string }

// Executor whose relay answers every call through run, recording every terminal post hook
func keyboardExecutor(t *testing.T, run func(ctx context.Context, name string) (string, error)) (*Executor, *fakeExecutorHost, func() []postRecord) {
	t.Helper()
	old := sequentialInterruptPollInterval
	sequentialInterruptPollInterval = 20 * time.Millisecond
	t.Cleanup(func() { sequentialInterruptPollInterval = old })
	t.Setenv("ICEBATH_CONCURRENT_TOOL_TIMEOUT_S", "0")
	host := &fakeExecutorHost{env: newTestToolEnv(t)}
	d := NewDispatcher(NewRegistry())
	d.Relay = func(ctx context.Context, _ *Env, name string, args map[string]any, _ ToolHookIDs, _ func(map[string]any) (string, error)) (string, map[string]any, error) {
		result, err := run(ctx, name)
		return result, args, err
	}
	var mu sync.Mutex
	var posts []postRecord
	d.PostToolCall = []PostToolCallHook{func(_ context.Context, _ *Env, ev PostToolCallEvent) {
		mu.Lock()
		posts = append(posts, postRecord{ev.ToolName, ev.Status, ev.ErrorType, ev.ErrorMessage, ev.Result})
		mu.Unlock()
	}}
	return NewExecutor(host, d), host, func() []postRecord {
		mu.Lock()
		defer mu.Unlock()
		return append([]postRecord(nil), posts...)
	}
}

func keyboardCalls(names ...string) []state.ToolCall {
	out := make([]state.ToolCall, len(names))
	for i, n := range names {
		out[i] = state.ToolCall{ID: "call_" + n, Type: "function", Function: state.ToolCallFunction{Name: n, Arguments: "{}"}}
	}
	return out
}

func messageText(m *state.Message) string {
	s, _ := m.Content.(string)
	return s
}

func TestKeyboardInterruptInConcurrentWorkerEmitsCancelledResult(t *testing.T) {
	e, host, posts := keyboardExecutor(t, func(_ context.Context, name string) (string, error) {
		if name == "tool_a" {
			return "", ErrKeyboardInterrupt
		}
		return `{"ok": true}`, nil
	})
	var messages []*state.Message
	if err := e.ExecuteToolCallsConcurrent(context.Background(), keyboardCalls("tool_a", "tool_b"), &messages, "task", false); err != nil {
		t.Fatalf("concurrent worker interrupt must not propagate: %v", err)
	}
	if !host.Interrupted() || host.ToolInterruptReason() != ReasonUserInterrupt {
		t.Fatalf("agent not interrupted with the user reason: %v %q", host.Interrupted(), host.ToolInterruptReason())
	}
	want := `{"error": "Tool execution cancelled. ` + InterruptSkipWording(ReasonUserInterrupt) + `", "status": "cancelled"}`
	if len(messages) != 2 || !strings.Contains(messageText(messages[0]), want) {
		t.Fatalf("cancelled result missing: %d %q", len(messages), messageText(messages[0]))
	}
	var cancelled []postRecord
	for _, p := range posts() {
		if p.errorType == "keyboard_interrupt" {
			cancelled = append(cancelled, p)
		}
	}
	if len(cancelled) != 1 || cancelled[0].status != "cancelled" || cancelled[0].name != "tool_a" || cancelled[0].result != want {
		t.Fatalf("terminal hook %+v", cancelled)
	}
}

func TestKeyboardInterruptInSequentialCallSkipsEveryRemainingCallThenPropagates(t *testing.T) {
	e, host, posts := keyboardExecutor(t, func(_ context.Context, name string) (string, error) {
		if name == "tool_a" {
			return "", ErrKeyboardInterrupt
		}
		t.Errorf("%s ran after the keyboard interrupt", name)
		return "", nil
	})
	var messages []*state.Message
	err := e.ExecuteToolCallsSequential(context.Background(), keyboardCalls("tool_a", "tool_b"), &messages, "task", true)
	if !errors.Is(err, ErrKeyboardInterrupt) {
		t.Fatalf("keyboard interrupt not re-raised: %v", err)
	}
	if !host.Interrupted() {
		t.Fatal("agent not interrupted")
	}
	wording := InterruptSkipWording(ReasonUserInterrupt)
	if len(messages) != 2 {
		t.Fatalf("want a result for this and the remaining call, got %d", len(messages))
	}
	for i, n := range []string{"tool_a", "tool_b"} {
		if got := messageText(messages[i]); got != "[Tool execution cancelled — "+n+" was skipped. "+wording+"]" || messages[i].ToolCallID != "call_"+n {
			t.Fatalf("skipped result %d %q", i, got)
		}
	}
	p := posts()
	if len(p) != 1 || p[0].status != "cancelled" || p[0].errorType != "keyboard_interrupt" || p[0].errorMessage != "Tool execution cancelled. "+wording {
		t.Fatalf("hooks %+v", p)
	}
}

func TestKeyboardInterruptCauseStopsTheSequentialPoll(t *testing.T) {
	e, _, posts := keyboardExecutor(t, func(context.Context, string) (string, error) {
		time.Sleep(300 * time.Millisecond)
		return "late", nil
	})
	ctx, cancel := context.WithCancelCause(context.Background())
	time.AfterFunc(60*time.Millisecond, func() { cancel(ErrKeyboardInterrupt) })
	var messages []*state.Message
	err := e.ExecuteToolCallsSequential(ctx, keyboardCalls("tool_a"), &messages, "task", true)
	if !errors.Is(err, ErrKeyboardInterrupt) || len(messages) != 1 || !strings.Contains(messageText(messages[0]), "was skipped") {
		t.Fatalf("err %v messages %d", err, len(messages))
	}
	if p := posts(); len(p) != 1 || p[0].errorType != "keyboard_interrupt" {
		t.Fatalf("hooks %+v", p)
	}
}

func TestKeyboardInterruptInDelegateDispatchPropagatesWithoutResults(t *testing.T) {
	e, host, _ := keyboardExecutor(t, func(context.Context, string) (string, error) { return "", ErrKeyboardInterrupt })
	host.delegate = func(context.Context, map[string]any) (string, error) { return "", ErrKeyboardInterrupt }
	var messages []*state.Message
	err := e.ExecuteToolCallsSequential(context.Background(), keyboardCalls("delegate_task", "tool_b"), &messages, "task", true)
	if !errors.Is(err, ErrKeyboardInterrupt) || len(messages) != 0 || host.Interrupted() {
		t.Fatalf("err %v messages %d interrupted %v", err, len(messages), host.Interrupted())
	}
	if len(host.delegateSp) != 2 || host.delegateSp[1] != nil {
		t.Fatalf("delegate spinner not cleared %v", host.delegateSp)
	}
}

func TestKeyboardInterruptCauseDuringConcurrentBatchWaitsForWorkersAndPropagates(t *testing.T) {
	var mu sync.Mutex
	finished := 0
	e, _, _ := keyboardExecutor(t, func(context.Context, string) (string, error) {
		time.Sleep(200 * time.Millisecond)
		mu.Lock()
		finished++
		mu.Unlock()
		return "done", nil
	})
	ctx, cancel := context.WithCancelCause(context.Background())
	time.AfterFunc(50*time.Millisecond, func() { cancel(ErrKeyboardInterrupt) })
	var messages []*state.Message
	err := e.ExecuteToolCallsConcurrent(ctx, keyboardCalls("tool_a", "tool_b"), &messages, "task", true)
	mu.Lock()
	defer mu.Unlock()
	if !errors.Is(err, ErrKeyboardInterrupt) || len(messages) != 0 || finished != 2 {
		t.Fatalf("err %v messages %d finished %d", err, len(messages), finished)
	}
}

func TestToolProgressRelayThrottlesAndAlwaysPassesTheFinalUpdate(t *testing.T) {
	var clock time.Duration = time.Second
	old := toolProgressNow
	toolProgressNow = func() time.Duration { return clock }
	t.Cleanup(func() { toolProgressNow = old })
	var got []map[string]any
	ctx := BoundToolProgress(context.Background(), func(event, name, preview string, args map[string]any, extra map[string]any) {
		if event != ToolProgressEvent || name != "mcp_tool" || args != nil {
			t.Errorf("event %q name %q", event, name)
		}
		extra["message"] = preview
		got = append(got, extra)
	}, "mcp_tool", "tc1")
	report := CurrentToolProgress(ctx)
	total := 4.0
	report(1, &total, "one")
	clock += 100 * time.Millisecond
	report(2, &total, "two")
	clock += 100 * time.Millisecond
	report(4, &total, "done")
	if len(got) != 2 || got[0]["percent"] != 25 || got[1]["percent"] != 100 || got[1]["tool_call_id"] != "tc1" {
		t.Fatalf("relayed %v", got)
	}
	if CurrentToolProgress(BoundToolProgress(context.Background(), nil, "x", "y")) != nil || CurrentToolProgress(context.Background()) != nil {
		t.Fatal("reporter without a callback")
	}
	payload := ToolProgressPayload("mcp_tool", "", map[string]any{"tool_call_id": "tc1", "progress": 2, "total": 4.0, "percent": 50})
	if payload["tool_id"] != "tc1" || payload["progress"] != 2.0 || payload["message"] != nil || payload["percent"] != 50 {
		t.Fatalf("payload %v", payload)
	}
}
