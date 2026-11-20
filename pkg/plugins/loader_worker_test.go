package plugins

import (
	"context"
	"testing"
)

// Sees the load worker only inside a deadline bound plugin load, never on a plain context
func TestInPluginLoadWorkerFollowsTheLoadScope(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	if InPluginLoadWorker(ctx) {
		t.Fatal("a plain context reported a plugin load worker")
	}
	inside := false
	ok, err := runWithLoadDeadline(ctx, "probe", nil, func(c context.Context) (bool, error) {
		inside = InPluginLoadWorker(c)
		return true, nil
	})
	if err != nil || !ok {
		t.Fatalf("load deadline run failed: ok=%v err=%v", ok, err)
	}
	if !inside {
		t.Fatal("the deadline worker context did not report a plugin load worker")
	}
}
