package tools

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/state"
)

type recordedCall struct {
	name   string
	args   map[string]any
	callID string
}

// Executor over a registry holding one web_search tool that records each dispatch
func webSearchExecutor(t *testing.T) (*Executor, *fakeExecutorHost, func() []recordedCall) {
	t.Helper()
	var mu sync.Mutex
	var calls []recordedCall
	r := NewRegistry()
	r.Register(Def{Name: "web_search", Toolset: "web", Schema: map[string]any{"name": "web_search", "description": "search",
		"parameters": map[string]any{"type": "object", "properties": map[string]any{}}},
		Handler: func(_ context.Context, c *Call) string {
			mu.Lock()
			calls = append(calls, recordedCall{c.Name, c.Args, c.ToolCallID})
			mu.Unlock()
			b, _ := json.Marshal(map[string]any{"ok": c.Args["query"]})
			return string(b)
		}})
	host := &fakeExecutorHost{env: newTestToolEnv(t)}
	return NewExecutor(host, NewDispatcher(r)), host, func() []recordedCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]recordedCall(nil), calls...)
	}
}

func webSearchCall(id, args string) state.ToolCall {
	return state.ToolCall{ID: id, Type: "function", Function: state.ToolCallFunction{Name: "web_search", Arguments: args}}
}

func TestMalformedArgumentsAreRejectedWithoutBlockingValidSibling(t *testing.T) {
	bad := map[string]string{"malformed-json": "not-json", "scalar": `"scalar"`, "list": "[]", "empty": "", "truncated": `{"query": "cut off`}
	for _, mode := range []string{"sequential", "concurrent"} {
		for id, args := range bad {
			t.Run(mode+"/"+id, func(t *testing.T) {
				e, _, calls := webSearchExecutor(t)
				var messages []*state.Message
				tcs := []state.ToolCall{webSearchCall("call-bad", args), webSearchCall("call-good", `{"query": "valid"}`)}
				if mode == "sequential" {
					e.ExecuteToolCallsSequential(context.Background(), tcs, &messages, "task-1", true)
				} else {
					e.ExecuteToolCallsConcurrent(context.Background(), tcs, &messages, "task-1", true)
				}
				want := []recordedCall{{"web_search", map[string]any{"query": "valid"}, "call-good"}}
				if got := calls(); !reflect.DeepEqual(got, want) {
					t.Fatalf("executed %v", got)
				}
				var ids []string
				for _, m := range messages {
					ids = append(ids, m.ToolCallID)
				}
				if !reflect.DeepEqual(ids, []string{"call-bad", "call-good"}) {
					t.Fatalf("message ids %v", ids)
				}
				if !strings.Contains(messages[0].Text(), `"error": "Invalid tool arguments"`) {
					t.Fatalf("bad call content %q", messages[0].Text())
				}
			})
		}
	}
}

func TestDelegateTaskIsExemptFromTheSequentialDeadline(t *testing.T) {
	if !sequentialDeadlineExemptTools["delegate_task"] {
		t.Fatal("delegate_task not exempt")
	}
}

func TestImageGenerateParallelWorkerCapCanBeConfiguredLower(t *testing.T) {
	env := newTestToolEnv(t)
	env.Config = config.Tree{"image_gen": map[string]any{"max_parallel_requests": 1}}
	if got := maxWorkersForToolBatch(env, []string{"image_generate", "image_generate"}); got != 1 {
		t.Fatalf("got %d", got)
	}
	if got := maxWorkersForToolBatch(env, nil); got != 0 {
		t.Fatalf("empty batch got %d", got)
	}
}

func TestBeginToolExecutionPrintsPythonListAndJSON(t *testing.T) {
	e, host, _ := webSearchExecutor(t)
	quiet := false
	host.quiet = &quiet
	host.progress = "all"
	ref := &ToolCallRef{Name: "web_search", Args: map[string]any{"query": "q", "limit": int64(2)}, RawArgs: `{"query": "q", "limit": 2}`, CallID: "c1"}
	e.beginToolExecution(ref, 3)
	if len(host.printed) == 0 || host.printed[0] != `  📞 Tool 3: web_search(['query', 'limit']) - {"query": "q", "limit": 2}` {
		t.Fatalf("printed %q", host.printed)
	}
}
