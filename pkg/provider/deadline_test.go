package provider

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type deadlineCtxKey struct{}

type recordingSuspectBackend struct {
	mu      sync.Mutex
	reasons []string
}

func (b *recordingSuspectBackend) MarkSuspect(reason string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reasons = append(b.reasons, reason)
}

func (b *recordingSuspectBackend) EnsureHealthy() bool { return true }

func (b *recordingSuspectBackend) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.reasons)
}

type explodingSuspectBackend struct{}

func (explodingSuspectBackend) MarkSuspect(string) { panic("backend is broken") }

// Blocks until the release channel closes, ignoring the context like a cancellation shielded scope
func blockUntil(release <-chan struct{}) func(context.Context) (any, error) {
	return func(context.Context) (any, error) {
		<-release
		return "late", nil
	}
}

func TestRunBoundedSyncCompletionAndUnbounded(t *testing.T) {
	res, err := RunBounded(context.Background(), func(context.Context) (any, error) { return "ok", nil }, 5*time.Second, true, "t", nil, nil)
	if err != nil || res.TimedOut || res.Value != "ok" {
		t.Fatalf("completion %+v %v", res, err)
	}
	res, err = RunBounded(context.Background(), func(context.Context) (any, error) { return 7, nil }, 0, false, "t", nil, nil)
	if err != nil || res.TimedOut || res.Value != 7 {
		t.Fatalf("unbounded %+v %v", res, err)
	}
}

func TestRunBoundedSyncExceptionPropagatesUnchanged(t *testing.T) {
	boom := errors.New("x")
	if _, err := RunBounded(context.Background(), func(context.Context) (any, error) { return nil, boom }, 5*time.Second, true, "t", nil, nil); !errors.Is(err, boom) {
		t.Fatalf("error %v", err)
	}
}

func TestRunBoundedSyncTimeoutAbandonsWorkerAndReports(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	start := time.Now()
	res, err := RunBounded(context.Background(), blockUntil(release), 200*time.Millisecond, true, "wedged", nil, nil)
	if err != nil || !res.TimedOut || res.Value != nil || res.Label != "wedged" || time.Since(start) >= 5*time.Second {
		t.Fatalf("timeout %+v %v %v", res, err, time.Since(start))
	}
}

func TestRunBoundedSyncOnTimeoutCallback(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	var fired []bool
	res, _ := RunBounded(context.Background(), blockUntil(release), 100*time.Millisecond, true, "t", func() { fired = append(fired, true) }, nil)
	if !res.TimedOut || len(fired) != 1 {
		t.Fatalf("callback %+v %v", res, fired)
	}
	res, err := RunBounded(context.Background(), blockUntil(release), 100*time.Millisecond, true, "t", func() { panic("cleanup boom") }, nil)
	if err != nil || !res.TimedOut {
		t.Fatalf("callback failure must be swallowed %+v %v", res, err)
	}
}

func TestRunBoundedSyncInterruptLandsBeforeFullDeadline(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(150*time.Millisecond, cancel)
	start := time.Now()
	_, err := RunBounded(ctx, blockUntil(release), 10*time.Second, true, "ki", nil, nil)
	if !errors.Is(err, context.Canceled) || time.Since(start) > 2*time.Second {
		t.Fatalf("interrupt %v after %v", err, time.Since(start))
	}
}

func TestRunBoundedSyncWorkerInheritsCallerContext(t *testing.T) {
	ctx := context.WithValue(context.Background(), deadlineCtxKey{}, "from-caller")
	res, err := RunBounded(ctx, func(c context.Context) (any, error) { return c.Value(deadlineCtxKey{}), nil }, 5*time.Second, true, "ctx", nil, nil)
	if err != nil || res.TimedOut || res.Value != "from-caller" {
		t.Fatalf("context %+v %v", res, err)
	}
}

func TestRunBoundedAsyncCompletionUnboundedAndException(t *testing.T) {
	res, err := RunBoundedAsync(context.Background(), func(context.Context) (any, error) { return "ok", nil }, 5*time.Second, true, "t", nil, true, nil)
	if err != nil || res.TimedOut || res.Value != "ok" {
		t.Fatalf("completion %+v %v", res, err)
	}
	res, err = RunBoundedAsync(context.Background(), func(context.Context) (any, error) { return 7, nil }, 0, false, "t", nil, true, nil)
	if err != nil || res.TimedOut || res.Value != 7 {
		t.Fatalf("unbounded %+v %v", res, err)
	}
	boom := errors.New("x")
	if _, err := RunBoundedAsync(context.Background(), func(context.Context) (any, error) { return nil, boom }, 5*time.Second, true, "t", nil, true, nil); !errors.Is(err, boom) {
		t.Fatalf("exception %v", err)
	}
}

func TestRunBoundedAsyncTimeoutReturnsPromptly(t *testing.T) {
	start := time.Now()
	res, err := RunBoundedAsync(context.Background(), func(ctx context.Context) (any, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(30 * time.Second):
			return nil, nil
		}
	}, 200*time.Millisecond, true, "slow", nil, true, nil)
	if err != nil || !res.TimedOut || time.Since(start) >= 5*time.Second {
		t.Fatalf("timeout %+v %v %v", res, err, time.Since(start))
	}
}

func TestRunBoundedAsyncAbandonsCancellationShieldedTask(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	start := time.Now()
	res, err := RunBoundedAsync(context.Background(), blockUntil(release), 200*time.Millisecond, true, "shielded", nil, true, nil)
	if err != nil || !res.TimedOut || time.Since(start) >= 5*time.Second {
		t.Fatalf("shielded %+v %v", res, err)
	}
}

func TestRunBoundedAsyncOnAbandonCleanupRunsDetached(t *testing.T) {
	cleaned := make(chan struct{})
	res, _ := RunBoundedAsync(context.Background(), func(ctx context.Context) (any, error) { <-ctx.Done(); return nil, ctx.Err() }, 100*time.Millisecond, true, "t", func(context.Context) error { close(cleaned); return nil }, true, nil)
	if !res.TimedOut {
		t.Fatalf("result %+v", res)
	}
	select {
	case <-cleaned:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup never ran")
	}
}

func TestRunBoundedAsyncOnAbandonFailureIsSwallowed(t *testing.T) {
	buf := &lockedWriter{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)
	ran := make(chan struct{})
	res, err := RunBoundedAsync(context.Background(), func(ctx context.Context) (any, error) { <-ctx.Done(); return nil, ctx.Err() }, 50*time.Millisecond, true, "t", func(context.Context) error { defer close(ran); return errors.New("cleanup boom") }, false, nil)
	if err != nil || !res.TimedOut {
		t.Fatalf("result %+v %v", res, err)
	}
	<-ran
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(buf.String(), "deadline abandon-cleanup failed: cleanup boom") {
		if time.Now().After(deadline) {
			t.Fatalf("cleanup failure not logged: %s", buf.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRunBoundedAsyncCompletedOpNeverReportsTimeout(t *testing.T) {
	res, err := RunBoundedAsync(context.Background(), func(context.Context) (any, error) {
		time.Sleep(10 * time.Millisecond)
		return "made it", nil
	}, 5*time.Second, true, "t", nil, true, nil)
	if err != nil || res.TimedOut || res.Value != "made it" {
		t.Fatalf("completion %+v %v", res, err)
	}
}

func TestRunBoundedAsyncExternalCancellationCancelsInnerTask(t *testing.T) {
	started := make(chan struct{})
	innerCancelled := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := RunBoundedAsync(ctx, func(c context.Context) (any, error) {
			close(started)
			select {
			case <-c.Done():
				close(innerCancelled)
				return nil, c.Err()
			case <-time.After(30 * time.Second):
				return nil, nil
			}
		}, 25*time.Second, true, "t", nil, true, nil)
		errc <- err
	}()
	<-started
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("outer error %v", err)
	}
	select {
	case <-innerCancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("inner task leaked running")
	}
}

func TestDeadlineBackendMarking(t *testing.T) {
	never := func(ctx context.Context) (any, error) { <-ctx.Done(); return nil, ctx.Err() }
	backend := &recordingSuspectBackend{}
	res, _ := RunBoundedAsync(context.Background(), never, 50*time.Millisecond, true, "phase3a", nil, true, backend)
	if !res.TimedOut || backend.count() != 1 || !strings.Contains(backend.reasons[0], "phase3a") {
		t.Fatalf("async mark %+v %v", res, backend.reasons)
	}
	quick := &recordingSuspectBackend{}
	res, _ = RunBoundedAsync(context.Background(), func(context.Context) (any, error) { return "done", nil }, 5*time.Second, true, "phase3a-ok", nil, true, quick)
	if res.TimedOut || res.Value != "done" || quick.count() != 0 {
		t.Fatalf("async completion marked %+v %v", res, quick.reasons)
	}
	release := make(chan struct{})
	defer close(release)
	syncBackend := &recordingSuspectBackend{}
	res, _ = RunBounded(context.Background(), blockUntil(release), 50*time.Millisecond, true, "phase3a-sync", nil, syncBackend)
	if !res.TimedOut || syncBackend.count() != 1 || !strings.Contains(syncBackend.reasons[0], "phase3a-sync") {
		t.Fatalf("sync mark %+v %v", res, syncBackend.reasons)
	}
	res, _ = RunBoundedAsync(context.Background(), never, 50*time.Millisecond, true, "phase3a-plain", nil, true, struct{}{})
	if !res.TimedOut || res.Label != "phase3a-plain" {
		t.Fatalf("non adopting backend %+v", res)
	}
	res, err := RunBoundedAsync(context.Background(), never, 50*time.Millisecond, true, "phase3a-boom", nil, true, explodingSuspectBackend{})
	if err != nil || !res.TimedOut || res.Label != "phase3a-boom" {
		t.Fatalf("exploding backend %+v %v", res, err)
	}
	syncOK := &recordingSuspectBackend{}
	res, _ = RunBounded(context.Background(), func(context.Context) (any, error) { return "ok", nil }, 5*time.Second, true, "phase3a-sync-ok", nil, syncOK)
	if res.TimedOut || res.Value != "ok" || syncOK.count() != 0 {
		t.Fatalf("sync completion marked %+v %v", res, syncOK.reasons)
	}
}

func TestDeadlineMarkHappensBeforeCleanup(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	backend := &recordingSuspectBackend{}
	var seen []int
	res, _ := RunBounded(context.Background(), blockUntil(release), 50*time.Millisecond, true, "phase3a-order-sync", func() { seen = append(seen, backend.count()) }, backend)
	if !res.TimedOut || len(seen) != 1 || seen[0] != 1 {
		t.Fatalf("sync order %+v %v", res, seen)
	}
	asyncBackend := &recordingSuspectBackend{}
	var atCleanup atomic.Int64
	cleaned := make(chan struct{})
	res, _ = RunBoundedAsync(context.Background(), blockUntil(release), 50*time.Millisecond, true, "phase3a-order-async", func(context.Context) error {
		atCleanup.Store(int64(asyncBackend.count()))
		close(cleaned)
		return nil
	}, true, asyncBackend)
	<-cleaned
	if !res.TimedOut || atCleanup.Load() != 1 {
		t.Fatalf("async order %+v %d", res, atCleanup.Load())
	}
}

func TestDumpBlockedLoopDiagnostics(t *testing.T) {
	var stacks bytes.Buffer
	logs := &lockedWriter{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	prevWriter := stackDumpWriter
	stackDumpWriter = &stacks
	defer func() { slog.SetDefault(prev); stackDumpWriter = prevWriter }()
	dumpBlockedLoopDiagnostics("job", 3*time.Second)
	if !strings.Contains(logs.String(), "[deadline] 'job' deadline (3s) expired but the event loop has not processed the expiry after a further 5s") {
		t.Fatalf("warning %q", logs.String())
	}
	if !strings.Contains(stacks.String(), "goroutine ") || !strings.Contains(stacks.String(), "TestDumpBlockedLoopDiagnostics") {
		t.Fatalf("stack dump %q", stacks.String())
	}
}
