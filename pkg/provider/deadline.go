package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"runtime"
	"strconv"
	"sync/atomic"
	"time"

	syncutil "github.com/nickheyer/icebath/pkg/utils/sync"
)

// Longest wait handed to any platform primitive, a year off Windows and the DWORD millisecond cap on it
var MaxSafeTimeout = func() time.Duration {
	if runtime.GOOS == "windows" {
		return time.Duration((0xFFFFFFFF - 1000) / 1000.0 * float64(time.Second))
	}
	return 365 * 24 * time.Hour
}()

// Grace after a deadline fires before the blocked worker is reported
var LoopBlockedDumpGrace = 5 * time.Second

// Diagnostics dump of a dispatch goroutine that never processed its expiry, the stack dumper in production
var DumpBlockedLoop = dumpBlockedLoopDiagnostics

// Hands an expiry callback to the waiting dispatch goroutine, run at once in production where the waiter selects on it
var PostToDispatch = func(fn func()) { fn() }

// Clock hooks the deadline helpers use, swapped by tests
var (
	deadlineNow   = time.Now
	deadlineSleep = func(ctx context.Context, d time.Duration) error {
		if d <= 0 {
			return ctx.Err()
		}
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			return nil
		}
	}
)

// DeadlineExpiredError is an icebath enforced deadline, not the provider's timeout
type DeadlineExpiredError struct {
	Label string
	After time.Duration
}

func (e *DeadlineExpiredError) Error() string {
	return fmt.Sprintf("deadline expired after %.1fs: %s", e.After.Seconds(), e.Label)
}

// Timeout marks the error as a net.Error style timeout
func (e *DeadlineExpiredError) Timeout() bool { return true }

// SuspectableBackend is a stateful backend flagged on timeout so its owner recovers the connection before reuse
type SuspectableBackend interface {
	MarkSuspect(reason string)
}

func markBackendSuspect(backend any, label string, timeout time.Duration) {
	if backend == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			pkgLog().Debug(fmt.Sprintf("deadline mark_suspect failed: %v", r))
		}
	}()
	if sb, ok := backend.(interface{ MarkSuspect(reason string) }); ok {
		sb.MarkSuspect(fmt.Sprintf("%s timed out after %.1fs", label, timeout.Seconds()))
	}
}

// Swapped by tests to observe the all goroutine traceback dump
var stackDumpWriter io.Writer = os.Stderr

// dumpAllStacks writes every goroutine stack to stderr like a fault handler traceback
func dumpAllStacks() error {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			_, err := stackDumpWriter.Write(buf[:n])
			return err
		}
		buf = make([]byte, len(buf)*2)
	}
}

// Warns that an expiry went unprocessed past the grace window and dumps every goroutine stack
func dumpBlockedLoopDiagnostics(label string, timeout time.Duration) {
	pkgLog().Warn(fmt.Sprintf("[deadline] %s deadline (%.0fs) expired but the event loop has not processed the expiry after a further %.0fs — the loop thread appears BLOCKED in a synchronous call, which is why no asyncio timeout can fire. Dumping all thread stacks to stderr to identify the blocking frame.", strconv.Quote(label), timeout.Seconds(), LoopBlockedDumpGrace.Seconds()))
	if err := dumpAllStacks(); err != nil {
		pkgLog().Debug(fmt.Sprintf("faulthandler traceback dump failed: %v", err))
	}
}

// Runs the detached abandonment cleanup and logs its failure or panic at debug level
func runAbandonCleanup(ctx context.Context, onAbandon func(ctx context.Context) error) {
	defer func() {
		if r := recover(); r != nil {
			pkgLog().Debug(fmt.Sprintf("deadline abandon-cleanup failed: %v", r))
		}
	}()
	if err := onAbandon(ctx); err != nil {
		pkgLog().Debug(fmt.Sprintf("deadline abandon-cleanup failed: %v", err))
	}
}

// BoundedResult is the outcome of a bounded operation, operation errors never land here
type BoundedResult struct {
	TimedOut bool
	Value    any
	Elapsed  time.Duration
	Timeout  time.Duration
	Bounded  bool
	Label    string
}

// ClampTimeout maps NaN, infinite, or non positive seconds to unbounded and caps the rest
func ClampTimeout(seconds float64) (time.Duration, bool) {
	if math.IsNaN(seconds) || seconds <= 0 {
		return 0, false
	}
	if math.IsInf(seconds, 1) || seconds > MaxSafeTimeout.Seconds() {
		return MaxSafeTimeout, true
	}
	return time.Duration(seconds * float64(time.Second)), true
}

// PollUntil calls pred every interval until it holds or the timeout passes, the last sleep cut to the time left
func PollUntil[T any](pred func() (T, bool), timeout, interval time.Duration) T {
	deadline := deadlineNow().Add(timeout)
	for {
		result, ok := pred()
		if ok {
			return result
		}
		remaining := deadline.Sub(deadlineNow())
		if remaining <= 0 {
			return result
		}
		if interval < remaining {
			remaining = interval
		}
		_ = deadlineSleep(context.Background(), remaining)
	}
}

type boundedOutcome struct {
	value any
	err   error
}

// Starts fn on its own goroutine and delivers its value, error or recovered panic on the returned channel
func startBounded(ctx context.Context, fn func(ctx context.Context) (any, error), label string) <-chan boundedOutcome {
	done := make(chan boundedOutcome, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- boundedOutcome{err: fmt.Errorf("%s panicked: %v", label, r)}
			}
		}()
		v, err := fn(ctx)
		done <- boundedOutcome{value: v, err: err}
	}()
	return done
}

// RunBounded runs fn on a worker keeping the caller context values, abandoning the worker uncancelled on expiry
func RunBounded(ctx context.Context, fn func(ctx context.Context) (any, error), timeout time.Duration, bounded bool, label string, onTimeout func(), backend any) (BoundedResult, error) {
	start := deadlineNow()
	if !bounded {
		v, err := fn(ctx)
		return BoundedResult{Value: v, Elapsed: deadlineNow().Sub(start), Label: label}, err
	}
	v, timedOut, err := syncutil.RunBounded(ctx, timeout, fn)
	res := BoundedResult{Value: v, TimedOut: timedOut, Elapsed: deadlineNow().Sub(start), Timeout: timeout, Bounded: true, Label: label}
	if pe := (*syncutil.PanicError)(nil); errors.As(err, &pe) {
		err = fmt.Errorf("%s %w", label, err)
	}
	if !timedOut {
		return res, err
	}
	pkgLog().Warn(fmt.Sprintf("[deadline] %q timed out after %.1fs; worker abandoned", label, timeout.Seconds()))
	markBackendSuspect(backend, label, timeout)
	if onTimeout != nil {
		func() {
			defer func() {
				if r := recover(); r != nil {
					pkgLog().Debug(fmt.Sprintf("deadline on_timeout callback failed: %v", r))
				}
			}()
			onTimeout()
		}()
	}
	return res, nil
}

// RunBoundedAsync runs fn under a timer driven deadline, cancelling and abandoning it on expiry or caller cancellation
func RunBoundedAsync(ctx context.Context, fn func(ctx context.Context) (any, error), timeout time.Duration, bounded bool, label string, onAbandon func(ctx context.Context) error, dumpOnBlockedLoop bool, backend any) (BoundedResult, error) {
	start := deadlineNow()
	if !bounded {
		v, err := fn(ctx)
		return BoundedResult{Value: v, Elapsed: deadlineNow().Sub(start), Label: label}, err
	}
	taskCtx, cancelTask := context.WithCancel(ctx)
	done := startBounded(taskCtx, fn, label)
	expired := make(chan struct{})
	var processed atomic.Bool
	timers := []*time.Timer{time.AfterFunc(timeout, func() { PostToDispatch(func() { close(expired) }) })}
	if dumpOnBlockedLoop {
		dump, grace := DumpBlockedLoop, LoopBlockedDumpGrace
		timers = append(timers, time.AfterFunc(timeout+grace, func() {
			if !processed.Load() {
				dump(label, timeout)
			}
		}))
	}
	defer func() {
		for _, t := range timers {
			t.Stop()
		}
		processed.Store(true)
	}()
	result := func(o boundedOutcome) (BoundedResult, error) {
		cancelTask()
		return BoundedResult{Value: o.value, Elapsed: deadlineNow().Sub(start), Timeout: timeout, Bounded: true, Label: label}, o.err
	}
	select {
	case o := <-done:
		return result(o)
	case <-ctx.Done():
		cancelTask()
		return BoundedResult{Elapsed: deadlineNow().Sub(start), Timeout: timeout, Bounded: true, Label: label}, ctx.Err()
	case <-expired:
		processed.Store(true)
	}
	select {
	case o := <-done:
		return result(o)
	default:
	}
	cancelTask()
	markBackendSuspect(backend, label, timeout)
	if onAbandon != nil {
		go runAbandonCleanup(context.WithoutCancel(ctx), onAbandon)
	}
	pkgLog().Warn(fmt.Sprintf("[deadline] %s timed out after %.1fs; task abandoned", strconv.Quote(label), timeout.Seconds()))
	return BoundedResult{TimedOut: true, Elapsed: deadlineNow().Sub(start), Timeout: timeout, Bounded: true, Label: label}, nil
}
