package telegram

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/provider"
)

// Body that keeps running through cancellation for about a second, like a shielded PTB init
func wedgedForASecond(context.Context) (any, error) {
	for range 20 {
		time.Sleep(50 * time.Millisecond)
	}
	return nil, nil
}

func TestAwaitWithThreadDeadlineAbandonsAndRunsCleanupOnTimeout(t *testing.T) {
	cleanupRan := make(chan struct{})
	started := time.Now()
	_, err := awaitWithThreadDeadline(context.Background(), wedgedForASecond, 0.2, func(context.Context) error {
		close(cleanupRan)
		return nil
	}, "telegram", true)
	var timeout *deadlineTimeoutError
	if !errors.As(err, &timeout) || className(err) != "TimeoutError" {
		t.Fatalf("want TimeoutError, got %v", err)
	}
	if elapsed := time.Since(started); elapsed >= 800*time.Millisecond {
		t.Fatalf("returned after %v, want prompt return", elapsed)
	}
	select {
	case <-cleanupRan:
	case <-time.After(2 * time.Second):
		t.Fatal("detached cleanup never ran")
	}
}

func TestAwaitWithThreadDeadlineCleanupErrorIsSwallowed(t *testing.T) {
	_, err := awaitWithThreadDeadline(context.Background(), wedgedForASecond, 0.2, func(context.Context) error {
		return errors.New("cleanup blew up")
	}, "telegram", true)
	var timeout *deadlineTimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("want TimeoutError not the cleanup error, got %v", err)
	}
	time.Sleep(50 * time.Millisecond)
}

func TestBlockedLoopAfterExpiryDumpsDiagnostics(t *testing.T) {
	dump, grace, post := provider.DumpBlockedLoop, provider.LoopBlockedDumpGrace, provider.PostToDispatch
	t.Cleanup(func() {
		provider.DumpBlockedLoop, provider.LoopBlockedDumpGrace, provider.PostToDispatch = dump, grace, post
	})
	var mu sync.Mutex
	var dumps [][2]any
	provider.DumpBlockedLoop = func(label string, timeout time.Duration) {
		mu.Lock()
		dumps = append(dumps, [2]any{label, timeout.Seconds()})
		mu.Unlock()
	}
	provider.LoopBlockedDumpGrace = 150 * time.Millisecond
	// The dispatch goroutine stays pinned for a second so the posted expiry waits like a queued call_soon_threadsafe
	pinned := make(chan struct{})
	time.AfterFunc(time.Second, func() { close(pinned) })
	provider.PostToDispatch = func(fn func()) {
		<-pinned
		fn()
	}
	hung := func(ctx context.Context) (any, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	_, err := awaitWithThreadDeadline(context.Background(), hung, 0.05, nil, "telegram", true)
	var timeout *deadlineTimeoutError
	if !errors.As(err, &timeout) || className(err) != "TimeoutError" {
		t.Fatalf("want TimeoutError, got %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(dumps) != 1 || dumps[0] != [2]any{"telegram", 0.05} {
		t.Fatalf("dumps %v", dumps)
	}
}
