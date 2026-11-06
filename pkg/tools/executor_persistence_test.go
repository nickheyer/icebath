package tools

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

// Executor recording dispatch, flush and completion events in one ordered log
func persistenceExecutor(t *testing.T, persisted bool) (*Executor, *fakeExecutorHost, func() []string) {
	var mu sync.Mutex
	var events []string
	record := func(ev string) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	}
	e, host, _ := registryExecutor(t, handlerDef("web_search", func(_ context.Context, call *Call) string {
		record("dispatch " + call.ToolCallID)
		return "result-" + call.ToolCallID
	}))
	host.flush = func(msgs []*state.Message) (bool, error) {
		record("flush " + msgs[len(msgs)-1].ToolCallID)
		return persisted, nil
	}
	host.completeCb = func(callID, _ string, _ map[string]any, _ string) { record("complete " + callID) }
	return e, host, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), events...)
	}
}

func TestSequentialFlushesEachToolResultBeforeNextDispatch(t *testing.T) {
	e, _, events := persistenceExecutor(t, true)
	var messages []*state.Message
	if err := e.ExecuteToolCallsSequential(context.Background(), toolCallsFor("web_search", "c1", "c2"), &messages, "task-1", true); err != nil {
		t.Fatal(err)
	}
	if rowIDs(messages) != "c1,c2" {
		t.Fatalf("rows %s", rowIDs(messages))
	}
	if got := fmt.Sprint(events()); got != "[dispatch c1 flush c1 complete c1 dispatch c2 flush c2 complete c2]" {
		t.Fatalf("events %s", got)
	}
}

func TestConcurrentFlushesEachToolResultInOrderBeforeCompletion(t *testing.T) {
	e, _, events := persistenceExecutor(t, true)
	var messages []*state.Message
	if err := e.ExecuteToolCallsConcurrent(context.Background(), toolCallsFor("web_search", "c1", "c2"), &messages, "task-1", true); err != nil {
		t.Fatal(err)
	}
	var tail []string
	for _, ev := range events() {
		if ev != "dispatch c1" && ev != "dispatch c2" {
			tail = append(tail, ev)
		}
	}
	if rowIDs(messages) != "c1,c2" || fmt.Sprint(tail) != "[flush c1 complete c1 flush c2 complete c2]" {
		t.Fatalf("rows %s events %v", rowIDs(messages), events())
	}
}

func TestFailedToolResultPersistBlocksCompletionProjection(t *testing.T) {
	for _, mode := range []string{"sequential", "concurrent"} {
		e, host, events := persistenceExecutor(t, false)
		var messages []*state.Message
		run := e.ExecuteToolCallsSequential
		if mode == "concurrent" {
			run = e.ExecuteToolCallsConcurrent
		}
		if err := run(context.Background(), toolCallsFor("web_search", "failed-persist"), &messages, "task-1", true); err != nil {
			t.Fatal(err)
		}
		for _, ev := range events() {
			if ev == "complete failed-persist" {
				t.Fatalf("%s completion projected after a failed persist", mode)
			}
		}
		if !host.persistFail.Load() {
			t.Fatalf("%s persistence failure not recorded", mode)
		}
	}
}

func TestSegmentedBatchStopsBeforeLaterSegmentAfterPersistFailure(t *testing.T) {
	e, host, events := persistenceExecutor(t, false)
	calls := toolCallsFor("web_search", "first", "second")
	var messages []*state.Message
	segments := []BatchSegment{{Mode: "parallel", Calls: calls[:1]}, {Mode: "sequential", Calls: calls[1:]}}
	if err := e.ExecuteToolCallsSegmented(context.Background(), calls, &messages, "task-1", segments); err != nil {
		t.Fatal(err)
	}
	dispatched := []string{}
	for _, ev := range events() {
		if len(ev) > 9 && ev[:9] == "dispatch " {
			dispatched = append(dispatched, ev[9:])
		}
	}
	if fmt.Sprint(dispatched) != "[first]" || !host.persistFail.Load() {
		t.Fatalf("dispatched %v failed %v", dispatched, host.persistFail.Load())
	}
}

func TestFocusViewLeavesModelFacingMessagesIdentical(t *testing.T) {
	rows := func(mode string) string {
		e, host, _ := persistenceExecutor(t, true)
		host.progress = mode
		quiet := mode == "off"
		host.quiet = &quiet
		var messages []*state.Message
		if err := e.ExecuteToolCallsSequential(context.Background(), toolCallsFor("web_search", "c1", "c2"), &messages, "task-1", true); err != nil {
			t.Fatal(err)
		}
		out := ""
		for _, m := range messages {
			out += fmt.Sprintf("%s|%s|%v|%v;", m.Role, m.ToolCallID, m.Content, m.DisplayMetadata)
		}
		return out
	}
	if focused, verbose := rows("off"), rows("all"); focused != verbose {
		t.Fatalf("focus changed the model facing rows\n%s\n%s", focused, verbose)
	}
}
