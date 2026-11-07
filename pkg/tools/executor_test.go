package tools

import (
	"context"
	"reflect"
	"testing"
)

func TestToolSearchScopedNamesFollowsHostSelection(t *testing.T) {
	ctx := context.Background()
	host := &fakeExecutorHost{env: newTestToolEnv(t)}
	all := ToolSearchScopedNames(ctx, host)
	if got := NewExecutor(host, nil).toolSearchScopedNames(ctx); !reflect.DeepEqual(got, all) {
		t.Fatalf("executor form %v differs from exported form %v", got, all)
	}
	for name := range all {
		ts := Default.ToolsetForTool(ctx, "", name)
		if ts == "" {
			continue
		}
		host.disabled = []string{ts}
		if ToolSearchScopedNames(ctx, host)[name] {
			t.Fatalf("%s still scoped with toolset %s disabled", name, ts)
		}
		return
	}
}

func TestTrackedWorkerIDsListsLiveWorkersOnly(t *testing.T) {
	e := NewExecutor(nil, nil)
	_, a, releaseA := e.registerWorker(context.Background())
	_, b, releaseB := e.registerWorker(context.Background())
	got := map[WorkerID]bool{}
	for _, id := range e.TrackedWorkerIDs() {
		got[id] = true
	}
	if !reflect.DeepEqual(got, map[WorkerID]bool{a: true, b: true}) {
		t.Fatalf("tracked %v", got)
	}
	releaseA()
	releaseB()
	if ids := e.TrackedWorkerIDs(); len(ids) != 0 {
		t.Fatalf("released workers still tracked %v", ids)
	}
}
