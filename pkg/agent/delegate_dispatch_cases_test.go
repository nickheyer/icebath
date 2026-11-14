package agent

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

func probeUnit(configuredModel any) *delegateBatch {
	task := map[string]any{"goal": "probe"}
	return &delegateBatch{TaskList: []map[string]any{task}, TopRole: "leaf", Parent: &AIAgent{},
		Children: []delegateChild{{Index: 0, Task: task, Child: &AIAgent{model: "inherited/model"}}},
		Creds:    map[string]any{"model": configuredModel}}
}

func TestCompletionModelFallsBackToInheritedChildModel(t *testing.T) {
	var seen []string
	saved := dispatchAsyncDelegationBatchFn
	dispatchAsyncDelegationBatchFn = func(_ context.Context, _ *tools.Env, d tools.AsyncDispatch) (map[string]any, error) {
		model := ""
		if d.Model != nil {
			model = *d.Model
		}
		seen = append(seen, model)
		return nil, nil
	}
	t.Cleanup(func() { dispatchAsyncDelegationBatchFn = saved })
	dispatchUnit(context.Background(), probeUnit(nil), "", "", "", "", "")
	dispatchUnit(context.Background(), probeUnit("configured/model"), "", "", "", "", "")
	if !reflect.DeepEqual(seen, []string{"inherited/model", "configured/model"}) {
		t.Fatalf("models %q", seen)
	}
}

func parallelBatch(parent *AIAgent, n int) *delegateBatch {
	tasks := make([]map[string]any, n)
	children := make([]delegateChild, n)
	for i := range tasks {
		tasks[i] = map[string]any{"goal": fmt.Sprintf("task %d", i)}
		children[i] = delegateChild{Index: i, Task: tasks[i], Child: &AIAgent{delegateRole: "leaf"}}
	}
	return &delegateBatch{TaskList: tasks, Children: children, Parent: parent, Creds: map[string]any{}, TopRole: "leaf",
		MaxChildren: n, OverallStart: time.Now()}
}

func completedEntry(i int, summary string) *pyjson.Object {
	return pyjson.ObjectOf("task_index", i, "status", "completed", "summary", summary, "error", nil, "api_calls", 1, "duration_seconds", 0)
}

func TestInterruptDoesNotJoinWedgedChild(t *testing.T) {
	wedgedRelease := make(chan struct{})
	wedgedStarted := make(chan struct{})
	var startOnce sync.Once
	parent := &AIAgent{quietMode: true}
	saved := runSingleChildFn
	runSingleChildFn = func(_ context.Context, i int, _ string, _, _ *AIAgent, _ string, _, _ any) *pyjson.Object {
		if i == 0 {
			startOnce.Do(func() { close(wedgedStarted) })
			<-wedgedRelease
			return pyjson.ObjectOf("task_index", i, "status", "completed")
		}
		return completedEntry(i, "done")
	}
	t.Cleanup(func() { runSingleChildFn = saved })
	batch := parallelBatch(parent, 2)
	var results []*pyjson.Object
	returned := make(chan struct{})
	go func() {
		runChildrenParallel(context.Background(), batch, &results, true)
		close(returned)
	}()
	defer func() {
		close(wedgedRelease)
		select {
		case <-returned:
		case <-time.After(5 * time.Second):
		}
	}()
	select {
	case <-wedgedStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("wedged child never started")
	}
	if parent.IsInterrupted() {
		t.Fatal("parent interrupted before the test asked")
	}
	parent.interruptRequested.Store(true)
	select {
	case <-returned:
	case <-time.After(3 * time.Second):
		t.Fatal("interrupt path joined the wedged worker")
	}
	if len(results) != 2 {
		t.Fatalf("results %v", results)
	}
	byIndex := map[int]*pyjson.Object{}
	for _, e := range results {
		byIndex[entryValue(e, "task_index").(int)] = e
	}
	if entryValue(byIndex[0], "status") != "interrupted" {
		t.Fatalf("wedged entry %v", byIndex[0])
	}
	if s := entryValue(byIndex[1], "status"); s != "completed" && s != "interrupted" {
		t.Fatalf("sibling entry %v", byIndex[1])
	}
}

func TestNormalCompletionStillJoinsCleanly(t *testing.T) {
	parent := &AIAgent{quietMode: true}
	saved := runSingleChildFn
	runSingleChildFn = func(_ context.Context, i int, _ string, _, _ *AIAgent, _ string, _, _ any) *pyjson.Object {
		return completedEntry(i, fmt.Sprintf("s%d", i))
	}
	t.Cleanup(func() { runSingleChildFn = saved })
	var results []*pyjson.Object
	runChildrenParallel(context.Background(), parallelBatch(parent, 3), &results, true)
	var order []int
	for _, e := range results {
		order = append(order, entryValue(e, "task_index").(int))
		if entryValue(e, "status") != "completed" {
			t.Fatalf("entry %v", e)
		}
	}
	if !reflect.DeepEqual(order, []int{0, 1, 2}) {
		t.Fatalf("order %v", order)
	}
}
