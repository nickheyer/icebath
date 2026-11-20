package mcp

import (
	"context"
	"testing"
	"time"
)

// Transport that hangs until its task context is cancelled
func hangingRunner(s *serverTask, ctx context.Context, call int) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

func startHanging(t *testing.T, name string) *serverTask {
	t.Helper()
	isolateHome(t)
	fastServerTimings(t)
	startTestLoop(t)
	s, calls := runnerTask(t, name, nil, hangingRunner)
	go func() { _ = s.start(context.Background(), s.rendered) }()
	waitUntil(t, "transport never started", func() bool { return calls.Load() > 0 })
	return s
}

// Fails the test unless done closes within fifteen seconds
func finishesWithin(t *testing.T, what string, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatalf("%s wedged on cancel", what)
	}
}

func TestCancelledErrorPropagationCancelledErrorIsNotSwallowedByExceptException(t *testing.T) {
	s := startHanging(t, "cancel-test")
	time.Sleep(50 * time.Millisecond)
	s.cancelTask()
	finishesWithin(t, "run", s.done)
}

func TestCancelledErrorPropagationShutdownCompletesPromptlyWhenTaskIsCancelled(t *testing.T) {
	s := startHanging(t, "shutdown-cancel-test")
	shutdownWait = 50 * time.Millisecond
	time.Sleep(50 * time.Millisecond)
	returned := make(chan struct{})
	go func() {
		s.shutdown(context.Background())
		close(returned)
	}()
	finishesWithin(t, "shutdown", returned)
	if !taskDone(s) {
		t.Fatal("task did not finish after cancel")
	}
}
