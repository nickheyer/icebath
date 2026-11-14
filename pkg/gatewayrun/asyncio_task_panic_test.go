package gatewayrun

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// A task that panics settles with that failure, is reported once and leaves later tasks running
func TestSpawnedTaskPanicIsLoggedAndTheGatewayKeepsRunning(t *testing.T) {
	logs := captureLogRecords(t, slog.LevelWarn)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	crashed := spawnGatewayTask(ctx, "crashing-watcher", func(context.Context) (any, error) {
		var missing map[string]int
		missing["boom"] = 1
		return "unreachable", nil
	})
	result, err := crashed.wait(ctx)
	if err == nil || result != nil || !strings.Contains(err.Error(), "assignment to entry in nil map") {
		t.Fatalf("crashed task settled with %v, %v", result, err)
	}
	if crashed.cancelled() {
		t.Fatal("a crashed task must not read as cancelled")
	}
	errors := logs.messages(slog.LevelError)
	if len(errors) != 1 || !strings.HasPrefix(errors[0], "Task exception was never retrieved: ") {
		t.Fatalf("error records %q", errors)
	}
	after := spawnGatewayTask(ctx, "next", func(context.Context) (any, error) { return 7, nil })
	if result, err := after.wait(ctx); err != nil || result != 7 {
		t.Fatalf("task after the crash returned %v, %v", result, err)
	}
}

// Under a started gateway the crash goes to the loop handler, which swallows a transient network failure as a warning
func TestSpawnedTaskPanicReachesTheGatewayLoopHandler(t *testing.T) {
	logs := captureLogRecords(t, slog.LevelWarn)
	loop := gateway.NewCallbackLoop()
	loop.SetExceptionHandler(gatewayLoopExceptionHandler)
	taskExceptionLoop.Store(loop)
	t.Cleanup(func() {
		taskExceptionLoop.Store(nil)
		loop.SetExceptionHandler(nil)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	task := spawnGatewayTask(ctx, "poller", func(context.Context) (any, error) {
		panic(&TimedOut{msg: "Timed out"})
	})
	if _, err := task.wait(ctx); err == nil || err.Error() != "Timed out" {
		t.Fatalf("task settled with %v", err)
	}
	if got := logs.messages(slog.LevelError); len(got) != 0 {
		t.Fatalf("a transient network failure must not log at error level: %q", got)
	}
	warnings := logs.messages(slog.LevelWarn)
	if len(warnings) != 1 || warnings[0] != "Gateway swallowed transient network error from poller: TimedOut: Timed out" {
		t.Fatalf("warnings %q", warnings)
	}
}
