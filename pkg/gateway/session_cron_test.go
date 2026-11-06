package gateway

import (
	"context"
	"testing"
)

func TestCronExecutionBindingIsScopedToItsContext(t *testing.T) {
	ctx := context.Background()
	if _, ok := CurrentCronExecution(ctx); ok {
		t.Fatal("unbound context reports an execution")
	}
	bound := BindCronExecution(ctx, CronOwnerExecution{DBPath: "/h/state.db", SessionID: "s", JobID: "j", ExecutionID: "e"})
	got, ok := CurrentCronExecution(bound)
	if !ok || got.DBPath != "/h/state.db" || got.SessionID != "s" || got.JobID != "j" || got.ExecutionID != "e" {
		t.Fatalf("%+v %v", got, ok)
	}
	if _, ok := CurrentCronExecution(ctx); ok {
		t.Fatal("binding leaked into the parent context")
	}
}
