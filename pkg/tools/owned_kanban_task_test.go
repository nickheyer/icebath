package tools

import (
	"context"
	"sync"
	"testing"
)

func TestOwnedKanbanTaskOnlyForTheDispatcherOwnedWorker(t *testing.T) {
	t.Setenv("ICEBATH_KANBAN_TASK", " t_1 ")
	t.Setenv("ICEBATH_DELEGATED_CHILD_CONTEXT", "")
	t.Setenv("ICEBATH_NON_DISPATCHER_OWNED_CONTEXT", "")
	ctx := context.Background()
	if got := OwnedKanbanTask(ctx); got != "t_1" {
		t.Fatalf("owner sees %q", got)
	}
	if got := OwnedKanbanTask(WithDelegatedChildContext(ctx, true)); got != "" {
		t.Fatalf("delegated child sees %q", got)
	}
	env := &Env{Values: &sync.Map{}}
	env.Values.Store("session.non_dispatcher_owned", true)
	if got := OwnedKanbanTask(WithToolEnv(ctx, env)); got != "" {
		t.Fatalf("non owner session sees %q", got)
	}
	if ToolEnvFrom(WithToolEnv(ctx, env)) != env || ToolEnvFrom(ctx) != nil {
		t.Fatal("tool env not carried by the context")
	}
	t.Setenv("ICEBATH_DELEGATED_CHILD_CONTEXT", "1")
	if got := OwnedKanbanTask(ctx); got != "" {
		t.Fatalf("delegated child process sees %q", got)
	}
}
