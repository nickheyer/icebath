package memory

import (
	"context"
	"testing"

	ctxutil "github.com/nickheyer/icebath/pkg/utils/ctx"
)

func TestCtxBoundRunsTheJobInACopyOfTheSpawnersPythonContext(t *testing.T) {
	scope := ctxutil.NewVar[any]("test.ctx_bound.scope", "")
	parent := ctxutil.WithStore(context.Background(), ctxutil.NewStore())
	scope.Set(parent, "turn")
	job := CtxBound(parent)
	if got := scope.Get(job); got != "turn" {
		t.Fatalf("job starts with %v, want the spawner's value", got)
	}
	scope.Set(job, "auxiliary")
	if got := scope.Get(parent); got != "turn" {
		t.Fatalf("the job's set reached the spawner: %v", got)
	}
	scope.Set(parent, "next turn")
	if got := scope.Get(job); got != "auxiliary" {
		t.Fatalf("the spawner's later set reached the job: %v", got)
	}
	thread := SpawnContextThread(parent, "test", func(ctx context.Context) { scope.Set(ctx, "thread") })
	thread.Start()
	thread.Join(-1)
	if got := scope.Get(parent); got != "next turn" {
		t.Fatalf("the thread's set reached the spawner: %v", got)
	}
}
