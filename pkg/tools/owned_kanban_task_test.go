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

func TestExplicitBoardIntentIsPinnedInsideEveryFence(t *testing.T) {
	t.Setenv("ICEBATH_KANBAN_TASK", "  ")
	t.Setenv("ICEBATH_DELEGATED_CHILD_CONTEXT", "")
	ctx := context.Background()
	if ExplicitBoardIntentIsPinned(ctx) {
		t.Fatal("an unfenced caller is pinned")
	}
	if !ExplicitBoardIntentIsPinned(WithDelegatedChildContext(ctx, true)) {
		t.Fatal("an in process delegate child is not pinned")
	}
	t.Setenv("ICEBATH_KANBAN_TASK", " t_1 ")
	if !ExplicitBoardIntentIsPinned(ctx) {
		t.Fatal("a dispatched worker is not pinned")
	}
	t.Setenv("ICEBATH_KANBAN_TASK", "")
	t.Setenv("ICEBATH_DELEGATED_CHILD_CONTEXT", "/somewhere")
	if !ExplicitBoardIntentIsPinned(ctx) {
		t.Fatal("a marked descendant is not pinned")
	}
}

func TestKanbanPathIsFencedByMarkerRootAndPin(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	t.Setenv("ICEBATH_KANBAN_DB", "")
	t.Setenv("ICEBATH_DELEGATED_CHILD_CONTEXT", "")
	ctx := context.Background()
	if KanbanPathIsFenced(ctx, root+"/kanban.db") {
		t.Fatal("no marker fenced a path")
	}
	if !KanbanPathIsFenced(WithDelegatedChildContext(ctx, true), other+"/kanban.db") {
		t.Fatal("an in process delegate child is not fenced")
	}
	t.Setenv("ICEBATH_DELEGATED_CHILD_CONTEXT", "1")
	if !KanbanPathIsFenced(ctx, other+"/kanban.db") {
		t.Fatal("the legacy marker does not fence everything")
	}
	t.Setenv("ICEBATH_DELEGATED_CHILD_CONTEXT", root)
	for path, want := range map[string]bool{
		root:                          true,
		root + "/boards/x/kanban.db":  true,
		root + "/../" + "kanban.db":   false,
		other + "/kanban.db":          false,
		root + "-sibling/kanban.db":   false,
		root + "/boards/../kanban.db": true,
	} {
		if got := KanbanPathIsFenced(ctx, path); got != want {
			t.Fatalf("%s fenced %v, want %v", path, got, want)
		}
	}
	t.Setenv("ICEBATH_KANBAN_DB", " "+other+"/pinned.db ")
	if !KanbanPathIsFenced(ctx, other+"/pinned.db") || KanbanPathIsFenced(ctx, other+"/free.db") {
		t.Fatal("the dispatcher pinned db is not the only fenced path outside the root")
	}
}
