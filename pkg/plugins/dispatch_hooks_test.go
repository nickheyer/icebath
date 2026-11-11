package plugins

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type runtimeError struct{ msg string }

func (e *runtimeError) Error() string { return e.msg }

// Manager over a temp home whose config sets plugins.hook_callback_timeout
func timeoutManager(t *testing.T, timeout string) *Manager {
	t.Helper()
	h := t.TempDir()
	if timeout != "" {
		body := "plugins:\n  hook_callback_timeout: " + timeout + "\n"
		if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return NewManager(h)
}

// Appends a raw callback to a hook the way tests poke manager registries directly
func addHook(m *Manager, hookName string, fn HookFunc) *hookEntry {
	_, name := callbackNames(fn)
	e := &hookEntry{fn: fn, name: name}
	m.mu.Lock()
	m.hooks[hookName] = append(m.hooks[hookName], e)
	m.mu.Unlock()
	return e
}

func boomPolicy(context.Context, map[string]any) (any, error) {
	return nil, &runtimeError{"policy plugin blew up"}
}

func onPreTool(_ context.Context, kwargs map[string]any) (any, error) {
	if _, ok := kwargs["tool_data"]; !ok {
		return nil, &runtimeError{"on_pre_tool() missing 1 required positional argument: 'tool_data'"}
	}
	return nil, nil
}

func TestHookTimeoutDoesNotBlockCaller(t *testing.T) {
	m := timeoutManager(t, "0.15")
	hold, started := make(chan struct{}), make(chan struct{}, 1)
	addHook(m, "post_tool_call", func(context.Context, map[string]any) (any, error) {
		started <- struct{}{}
		<-hold
		return "late", nil
	})
	addHook(m, "post_tool_call", func(context.Context, map[string]any) (any, error) { return map[string]any{"ok": true}, nil })
	t0 := time.Now()
	results := m.InvokeHook(context.Background(), "post_tool_call", map[string]any{"tool_name": "terminal", "args": map[string]any{}, "result": "{}"})
	elapsed := time.Since(t0)
	<-started
	close(hold)
	if !reflect.DeepEqual(results, []any{map[string]any{"ok": true}}) || elapsed > 5*time.Second {
		t.Fatalf("results %v elapsed %v", results, elapsed)
	}
}

func TestHookExceptionStillIsolatedUnderTimeoutPath(t *testing.T) {
	m := timeoutManager(t, "1.0")
	addHook(m, "post_tool_call", func(context.Context, map[string]any) (any, error) { return nil, &runtimeError{"plugin blew up"} })
	addHook(m, "post_tool_call", func(context.Context, map[string]any) (any, error) { return "survived", nil })
	if got := m.InvokeHook(context.Background(), "post_tool_call", nil); !reflect.DeepEqual(got, []any{"survived"}) {
		t.Fatalf("got %v", got)
	}
}

func TestSystemExitIsReportedUnderTimeoutPath(t *testing.T) {
	m := timeoutManager(t, "1.0")
	logs := captureLogs(t)
	addHook(m, "post_tool_call", func(context.Context, map[string]any) (any, error) {
		panic("bounded plugin requested process exit")
	})
	addHook(m, "post_tool_call", func(context.Context, map[string]any) (any, error) { return "survived", nil })
	if got := m.InvokeHook(context.Background(), "post_tool_call", nil); !reflect.DeepEqual(got, []any{"survived"}) {
		t.Fatalf("got %v", got)
	}
	if !strings.Contains(logs.String(), "bounded plugin requested process exit") {
		t.Fatalf("panic not reported: %s", logs.String())
	}
}

func TestPreToolCallCallbackExceptionFailsClosed(t *testing.T) {
	for _, timeout := range []string{"0.0", "1.0"} {
		m := timeoutManager(t, timeout)
		addHook(m, "pre_tool_call", boomPolicy)
		addHook(m, "pre_tool_call", func(context.Context, map[string]any) (any, error) { return map[string]any{"action": "approve"}, nil })
		results := m.InvokeHook(context.Background(), "pre_tool_call", map[string]any{"tool_name": "terminal", "args": map[string]any{}})
		if len(results) != 2 || results[0].(map[string]any)["action"] != "block" || results[1].(map[string]any)["action"] != "approve" {
			t.Fatalf("timeout %s results %v", timeout, results)
		}
		msg := results[0].(map[string]any)["message"].(string)
		if !strings.Contains(msg, "boomPolicy") || !strings.Contains(msg, "runtimeError: policy plugin blew up") {
			t.Fatalf("message %q", msg)
		}
	}
}

func TestHookCallbackTimeoutReadsConfig(t *testing.T) {
	cases := map[string]float64{"": 30, "0.12": 0.12, "-1": 30, "9999": 600, "'abc'": 30, "'2.5'": 2.5, "true": 1, "[1]": 30, "0": 0}
	for raw, want := range cases {
		if got := timeoutManager(t, raw).resolveHookCallbackTimeout(); got != want {
			t.Fatalf("%q -> %v want %v", raw, got, want)
		}
	}
}

func TestSubagentStopStaysOnCallerThread(t *testing.T) {
	m := timeoutManager(t, "1.0")
	var stack string
	addHook(m, "subagent_stop", func(context.Context, map[string]any) (any, error) {
		stack = string(debug.Stack())
		return "ok", nil
	})
	if got := m.InvokeHook(context.Background(), "subagent_stop", map[string]any{"parent_session_id": "p1"}); !reflect.DeepEqual(got, []any{"ok"}) {
		t.Fatalf("got %v", got)
	}
	if !strings.Contains(stack, "TestSubagentStopStaysOnCallerThread") {
		t.Fatalf("callback ran off the caller goroutine:\n%s", stack)
	}
}

func TestSystemExitFromCallerThreadHookIsIsolated(t *testing.T) {
	m := timeoutManager(t, "1.0")
	logs := captureLogs(t)
	addHook(m, "subagent_stop", func(context.Context, map[string]any) (any, error) { panic("plugin requested process exit") })
	addHook(m, "subagent_stop", func(context.Context, map[string]any) (any, error) { return "survived", nil })
	if got := m.InvokeHook(context.Background(), "subagent_stop", map[string]any{"parent_session_id": "p1"}); !reflect.DeepEqual(got, []any{"survived"}) {
		t.Fatalf("got %v", got)
	}
	if !strings.Contains(logs.String(), "plugin requested process exit") {
		t.Fatal("panic not reported")
	}
}

func TestHungCallbackSuppressesRepeatFires(t *testing.T) {
	m := timeoutManager(t, "0.1")
	hold := make(chan struct{})
	defer close(hold)
	var starts atomic.Int32
	addHook(m, "post_tool_call", func(context.Context, map[string]any) (any, error) {
		starts.Add(1)
		<-hold
		return "late", nil
	})
	t0 := time.Now()
	for range 2 {
		if got := m.InvokeHook(context.Background(), "post_tool_call", nil); len(got) != 0 {
			t.Fatalf("got %v", got)
		}
	}
	if starts.Load() != 1 || time.Since(t0) > 5*time.Second {
		t.Fatalf("starts %d", starts.Load())
	}
}

func TestConcurrentSameToolCallsWithDistinctIDsBothRun(t *testing.T) {
	m := timeoutManager(t, "5.0")
	hold := make(chan struct{})
	var starts atomic.Int32
	addHook(m, "pre_tool_call", func(context.Context, map[string]any) (any, error) {
		starts.Add(1)
		<-hold
		return "ok", nil
	})
	var wg sync.WaitGroup
	fire := func(id string) {
		defer wg.Done()
		m.InvokeHook(context.Background(), "pre_tool_call", map[string]any{"tool_name": "read_file", "tool_input": map[string]any{}, "session_id": "s1", "tool_call_id": id})
	}
	wg.Add(2)
	go fire("call-a")
	time.Sleep(100 * time.Millisecond)
	go fire("call-b")
	time.Sleep(400 * time.Millisecond)
	close(hold)
	wg.Wait()
	if starts.Load() != 2 {
		t.Fatalf("starts %d", starts.Load())
	}
}

func TestRepeatedSameCallIdentityStillDeduplicated(t *testing.T) {
	m := timeoutManager(t, "5.0")
	hold, started := make(chan struct{}), make(chan struct{}, 1)
	var starts atomic.Int32
	addHook(m, "post_tool_call", func(context.Context, map[string]any) (any, error) {
		starts.Add(1)
		started <- struct{}{}
		<-hold
		return "late", nil
	})
	fire := func() {
		m.InvokeHook(context.Background(), "post_tool_call", map[string]any{"tool_name": "read_file", "tool_call_id": "same-call"})
	}
	firstDone := make(chan struct{})
	go func() { fire(); close(firstDone) }()
	<-started
	fire()
	if starts.Load() != 1 {
		t.Fatalf("starts %d", starts.Load())
	}
	close(hold)
	<-firstDone
}

func TestHungWorkerCapsNewCallIdentitiesAfterSuppression(t *testing.T) {
	m := timeoutManager(t, "0.1")
	m.hookTimeoutSuppression = 0
	logs := captureLogs(t)
	hold := make(chan struct{})
	defer close(hold)
	var starts atomic.Int32
	addHook(m, "post_tool_call", func(context.Context, map[string]any) (any, error) {
		starts.Add(1)
		<-hold
		return "late", nil
	})
	for i := range hookMaxAbandonedWorkers + 3 {
		if got := m.InvokeHook(context.Background(), "post_tool_call", map[string]any{"tool_name": "read_file", "tool_call_id": fmt.Sprintf("call-%d", i)}); len(got) != 0 {
			t.Fatalf("got %v", got)
		}
	}
	if starts.Load() != hookMaxAbandonedWorkers || !strings.Contains(logs.String(), "abandoned worker(s) still running") {
		t.Fatalf("starts %d logs %s", starts.Load(), logs.String())
	}
}

func TestHungWorkerDoesNotFailClosedForever(t *testing.T) {
	m := timeoutManager(t, "0.1")
	m.hookTimeoutSuppression = 200 * time.Millisecond
	hold := make(chan struct{})
	defer close(hold)
	var starts atomic.Int32
	addHook(m, "pre_tool_call", func(context.Context, map[string]any) (any, error) {
		if starts.Add(1) == 1 {
			<-hold
		}
		return nil, nil
	})
	blocked := []any{map[string]any{"action": "block", "message": preToolCallTimeoutBlockMessage}}
	call := func(id string) []any {
		return m.InvokeHook(context.Background(), "pre_tool_call", map[string]any{"tool_name": "read_file", "tool_call_id": id})
	}
	if got := call("call-a"); !reflect.DeepEqual(got, blocked) {
		t.Fatalf("call-a %v", got)
	}
	if got := call("call-b"); !reflect.DeepEqual(got, blocked) {
		t.Fatalf("call-b %v", got)
	}
	time.Sleep(300 * time.Millisecond)
	if got := call("call-c"); len(got) != 0 || starts.Load() != 2 {
		t.Fatalf("call-c %v starts %d", got, starts.Load())
	}
}

func TestWorkerFinishingAtTimeoutDoesNotLeavePhantomAbandonedEntry(t *testing.T) {
	saved := waitHookWorker
	waitHookWorker = func(_ context.Context, finished <-chan struct{}, _ time.Duration) bool {
		<-finished
		return false
	}
	t.Cleanup(func() { waitHookWorker = saved })
	m := timeoutManager(t, "0.1")
	m.hookTimeoutSuppression = 0
	var starts atomic.Int32
	addHook(m, "post_tool_call", func(context.Context, map[string]any) (any, error) { starts.Add(1); return "done", nil })
	if got := m.InvokeHook(context.Background(), "post_tool_call", map[string]any{"tool_name": "read_file", "tool_call_id": "call-a"}); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
	if len(m.hookAbandoned) != 0 {
		t.Fatalf("phantom abandoned %v", m.hookAbandoned)
	}
	m.InvokeHook(context.Background(), "post_tool_call", map[string]any{"tool_name": "read_file", "tool_call_id": "call-b"})
	if starts.Load() != 2 {
		t.Fatalf("callback started %d times", starts.Load())
	}
}

func TestPreToolCallWorkerStartFailureFailsClosedWithoutSticking(t *testing.T) {
	saved := startHookWorker
	attempts := 0
	startHookWorker = func(run func()) error {
		attempts++
		if attempts == 1 {
			return &runtimeError{"can't start new thread"}
		}
		return saved(run)
	}
	t.Cleanup(func() { startHookWorker = saved })
	m := timeoutManager(t, "1.0")
	var calls atomic.Int32
	addHook(m, "pre_tool_call", func(context.Context, map[string]any) (any, error) { calls.Add(1); return nil, nil })
	first := m.InvokeHook(context.Background(), "pre_tool_call", nil)
	if !reflect.DeepEqual(first, []any{map[string]any{"action": "block", "message": preToolCallTimeoutBlockMessage}}) {
		t.Fatalf("first %v", first)
	}
	if second := m.InvokeHook(context.Background(), "pre_tool_call", nil); len(second) != 0 {
		t.Fatalf("second %v", second)
	}
	if calls.Load() != 1 {
		t.Fatalf("policy ran %d times", calls.Load())
	}
}

func TestSystemExitFromMiddlewareIsIsolated(t *testing.T) {
	m := timeoutManager(t, "")
	logs := captureLogs(t)
	m.middleware["tool_call"] = []*middlewareEntry{
		{fn: func(map[string]any) (any, error) { panic("middleware requested process exit") }, name: "exits"},
		{fn: func(map[string]any) (any, error) { return "survived", nil }, name: "survivor"},
	}
	if got := m.InvokeMiddleware("tool_call", "", map[string]any{}); !reflect.DeepEqual(got, []any{"survived"}) {
		t.Fatalf("got %v", got)
	}
	if !strings.Contains(logs.String(), "middleware requested process exit") {
		t.Fatalf("panic not reported: %s", logs.String())
	}
}

func TestAsyncHookResultIsAwaitedAlongsideSync(t *testing.T) {
	m := timeoutManager(t, "")
	addHook(m, "pre_gateway_dispatch", func(context.Context, map[string]any) (any, error) { return "sync", nil })
	addHook(m, "pre_gateway_dispatch", func(context.Context, map[string]any) (any, error) {
		return AwaitFunc(func(context.Context) (any, error) { return "async", nil }), nil
	})
	if got := m.InvokeHook(context.Background(), "pre_gateway_dispatch", nil); !reflect.DeepEqual(got, []any{"sync", "async"}) {
		t.Fatalf("got %v", got)
	}
	if got := m.InvokeHook(WithRunningLoop(context.Background()), "pre_gateway_dispatch", nil); !reflect.DeepEqual(got, []any{"sync", "async"}) {
		t.Fatalf("running loop got %v", got)
	}
}

func TestAInvokeHookAwaitsOnCallerAndBoundsPolicyHooks(t *testing.T) {
	m := timeoutManager(t, "0.1")
	addHook(m, "pre_tool_call", func(context.Context, map[string]any) (any, error) {
		return AwaitFunc(func(ctx context.Context) (any, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}), nil
	})
	addHook(m, "pre_tool_call", func(context.Context, map[string]any) (any, error) { return "inline", nil })
	got := m.AInvokeHook(context.Background(), "pre_tool_call", nil)
	want := []any{map[string]any{"action": "block", "message": preToolCallTimeoutBlockMessage}, "inline"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

func TestResolvePluginCommandResult(t *testing.T) {
	ctx := WithRunningLoop(context.Background())
	if v, err := ResolvePluginCommandResult(ctx, AwaitFunc(func(context.Context) (any, error) { return "done", nil })); v != "done" || err != nil {
		t.Fatalf("%v %v", v, err)
	}
	if v, _ := ResolvePluginCommandResult(ctx, 7); v != 7 {
		t.Fatalf("plain %v", v)
	}
	saved := pluginCommandAwaitTimeout
	pluginCommandAwaitTimeout = 50 * time.Millisecond
	t.Cleanup(func() { pluginCommandAwaitTimeout = saved })
	hold := make(chan struct{})
	defer close(hold)
	t0 := time.Now()
	_, err := ResolvePluginCommandResult(ctx, AwaitFunc(func(context.Context) (any, error) { <-hold; return nil, nil }))
	var te *PluginCommandTimeoutError
	if err == nil || !strings.Contains(err.Error(), "did not complete") || time.Since(t0) > 5*time.Second || !asError(err, &te) {
		t.Fatalf("err %v", err)
	}
}

func asError(err error, target **PluginCommandTimeoutError) bool {
	te, ok := err.(*PluginCommandTimeoutError)
	*target = te
	return ok
}

func TestIdenticalHookFailureWarnsOnceThenDebug(t *testing.T) {
	m := timeoutManager(t, "")
	logs := captureLogs(t)
	addHook(m, "pre_tool_call", onPreTool)
	for i := range 5 {
		m.InvokeHook(context.Background(), "pre_tool_call", map[string]any{"tool_name": "read_file", "args": map[string]any{"path": fmt.Sprintf("/p%d", i)}})
	}
	var warns, debugs []string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "onPreTool") && strings.Contains(line, "level=WARN") {
			warns = append(warns, line)
		}
		if strings.Contains(line, "onPreTool") && strings.Contains(line, "level=DEBUG") {
			debugs = append(debugs, line)
		}
	}
	if len(warns) != 1 || len(debugs) != 4 || !strings.Contains(warns[0], "tool_data") || !strings.Contains(warns[0], "tool_name") {
		t.Fatalf("warns %v debugs %d", warns, len(debugs))
	}
}

func TestDistinctHookFailuresEachWarn(t *testing.T) {
	m := timeoutManager(t, "")
	logs := captureLogs(t)
	addHook(m, "post_tool_call", func(_ context.Context, kw map[string]any) (any, error) {
		return nil, &runtimeError{fmt.Sprint("bad ", kw["n"])}
	})
	m.InvokeHook(context.Background(), "post_tool_call", map[string]any{"n": 1})
	m.InvokeHook(context.Background(), "post_tool_call", map[string]any{"n": 2})
	if strings.Count(logs.String(), "level=WARN") != 2 {
		t.Fatalf("logs %s", logs.String())
	}
}

func TestHeldTransformsSnapshotAndRun(t *testing.T) {
	m := timeoutManager(t, "")
	c := newContext(&Manifest{Name: "holder", Key: "holder"}, m)
	if len(m.HeldTransforms()) != 0 {
		t.Fatal("nothing holds yet")
	}
	c.RegisterHook(StreamHoldingHook, func(context.Context, map[string]any) (any, error) { return nil, &runtimeError{"x"} }, true)
	c.RegisterHook(StreamHoldingHook, func(_ context.Context, kw map[string]any) (any, error) {
		return "rewritten " + kw["text"].(string), nil
	}, false)
	held := m.HeldTransforms()
	if len(held) != 2 {
		t.Fatalf("held %d", len(held))
	}
	if out, failed := m.RunHeldTransforms(context.Background(), held, map[string]any{"text": "hi"}); out != "rewritten hi" || failed {
		t.Fatalf("%q %v", out, failed)
	}
	if out, failed := m.RunHeldTransforms(context.Background(), held[:1], map[string]any{"text": "hi"}); out != "" || !failed {
		t.Fatalf("%q %v", out, failed)
	}
}

func TestErrorTypeNameNamesTheRaisedClass(t *testing.T) {
	for want, err := range map[string]error{"ValueError": &HostBuiltinError{Type: "ValueError", Message: "x"},
		"KeyError": &PluginHostError{TypeName: "KeyError"}, "PluginHostUnavailable": &PluginHostUnavailable{Reason: "gone"}} {
		if got := ErrorTypeName(err); got != want {
			t.Fatalf("ErrorTypeName(%T) = %q want %q", err, got, want)
		}
	}
}
