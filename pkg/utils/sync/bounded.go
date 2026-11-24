package syncutil

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"time"
)

// Error of bounded work whose goroutine ended through runtime.Goexit instead of returning or panicking
var ErrExited = errors.New("work exited without returning")

// Panic of work that ran on a goroutine of its own, carried back to the waiting caller with the stack it panicked on
type PanicError struct {
	Value any
	Stack []byte
}

// The recovered value after "panicked: "
func (e *PanicError) Error() string { return fmt.Sprintf("panicked: %v", e.Value) }

// Runs fn on a goroutine of its own that keeps ctx's values but not its cancellation and waits at most timeout for it; on expiry the goroutine is left to finish unobserved and timedOut is true, when ctx ends first its error comes back, a result ready by then winning over either, a panic in fn comes back as a *PanicError and a runtime.Goexit as ErrExited
func RunBounded[T any](ctx context.Context, timeout time.Duration, fn func(ctx context.Context) (T, error)) (value T, timedOut bool, err error) {
	type outcome struct {
		value T
		err   error
	}
	done := make(chan outcome, 1)
	worker := context.WithoutCancel(ctx)
	go func() {
		returned := false
		defer func() {
			if returned {
				return
			}
			if r := recover(); r != nil {
				done <- outcome{err: &PanicError{Value: r, Stack: debug.Stack()}}
				return
			}
			done <- outcome{err: ErrExited}
		}()
		v, err := fn(worker)
		returned = true
		done <- outcome{value: v, err: err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var lost error
	select {
	case o := <-done:
		return o.value, false, o.err
	case <-ctx.Done():
		lost = ctx.Err()
	case <-timer.C:
	}
	select {
	case o := <-done:
		return o.value, false, o.err
	default:
		if lost != nil {
			return value, false, lost
		}
		return value, true, nil
	}
}
