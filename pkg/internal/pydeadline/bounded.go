package pydeadline

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Result is the outcome of a bounded operation, operation errors never land here
type Result struct {
	TimedOut bool
	Value    any
	Elapsed  time.Duration
	Timeout  time.Duration
	Bounded  bool
	Label    string
}

// Value, error or recovered panic of a bounded worker
type Outcome struct {
	Value any
	Err   error
}

// Starts fn on its own goroutine and delivers its value, error or recovered panic on the returned channel
func Start(ctx context.Context, fn func(ctx context.Context) (any, error), label string) <-chan Outcome {
	done := make(chan Outcome, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- Outcome{Err: fmt.Errorf("%s panicked: %v", label, r)}
			}
		}()
		v, err := fn(ctx)
		done <- Outcome{Value: v, Err: err}
	}()
	return done
}

// Hooks of one bounded run, the clock and the two expiry callbacks
type Hooks struct {
	Now         func() time.Time
	MarkSuspect func()
	OnTimeout   func()
}

// Runs fn on a worker keeping the caller context values, abandoning the worker uncancelled on expiry
func RunBounded(ctx context.Context, fn func(ctx context.Context) (any, error), timeout time.Duration, bounded bool, label string, hooks Hooks) (Result, error) {
	now := hooks.Now
	if now == nil {
		now = time.Now
	}
	start := now()
	if !bounded {
		v, err := fn(ctx)
		return Result{Value: v, Elapsed: now().Sub(start), Label: label}, err
	}
	done := Start(context.WithoutCancel(ctx), fn, label)
	result := func(o Outcome) (Result, error) {
		return Result{Value: o.Value, Elapsed: now().Sub(start), Timeout: timeout, Bounded: true, Label: label}, o.Err
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case o := <-done:
		return result(o)
	case <-ctx.Done():
		return Result{Elapsed: now().Sub(start), Timeout: timeout, Bounded: true, Label: label}, ctx.Err()
	case <-timer.C:
	}
	select {
	case o := <-done:
		return result(o)
	default:
	}
	slog.Warn(fmt.Sprintf("[deadline] %s timed out after %.1fs; worker abandoned", pyrepr.StrRepr(label), timeout.Seconds()))
	if hooks.MarkSuspect != nil {
		hooks.MarkSuspect()
	}
	if hooks.OnTimeout != nil {
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Debug(fmt.Sprintf("deadline on_timeout callback failed: %v", r))
				}
			}()
			hooks.OnTimeout()
		}()
	}
	return Result{TimedOut: true, Elapsed: now().Sub(start), Timeout: timeout, Bounded: true, Label: label}, nil
}
