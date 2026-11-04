package provider

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

// Longest wait handed to any platform primitive, a year off Windows and the DWORD millisecond cap on it
var MaxSafeTimeout = func() time.Duration {
	if runtime.GOOS == "windows" {
		return time.Duration((0xFFFFFFFF - 1000) / 1000.0 * float64(time.Second))
	}
	return 365 * 24 * time.Hour
}()

// Grace after a deadline fires before the blocked worker is reported
const loopBlockedDumpGrace = 5 * time.Second

// Slice of a bounded wait so an interrupt is observed promptly
const boundedWaitSlice = 200 * time.Millisecond

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

// SuspectableBackend is a stateful backend flagged on timeout so its owner health checks it before reuse
type SuspectableBackend interface {
	MarkSuspect(reason string)
	EnsureHealthy() bool
}

func markBackendSuspect(backend any, label string, timeout time.Duration) {
	if backend == nil {
		return
	}
	if sb, ok := backend.(interface{ MarkSuspect(reason string) }); ok {
		sb.MarkSuspect(fmt.Sprintf("%s timed out after %.1fs", label, timeout.Seconds()))
	}
}

// dumpAllStacks writes every goroutine stack to stderr like a fault handler traceback
func dumpAllStacks() {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			_, _ = os.Stderr.Write(buf[:n])
			return
		}
		buf = make([]byte, len(buf)*2)
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

// ClampTimeoutValue clamps a numeric or string timeout, anything non numeric is unbounded with a warning
func ClampTimeoutValue(v any) (time.Duration, bool) {
	if v == nil {
		return 0, false
	}
	if d, ok := v.(time.Duration); ok {
		return ClampTimeout(d.Seconds())
	}
	if _, isBool := v.(bool); isBool {
		slog.Warn(fmt.Sprintf("clamp_timeout: non-numeric timeout %v; treating as unbounded", v))
		return 0, false
	}
	f, ok := config.AsFloat(v)
	if !ok {
		slog.Warn(fmt.Sprintf("clamp_timeout: non-numeric timeout %v; treating as unbounded", v))
		return 0, false
	}
	if math.IsNaN(f) {
		slog.Warn("clamp_timeout: NaN timeout; treating as unbounded")
		return 0, false
	}
	return ClampTimeout(f)
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

// TimeoutsSection reads the timeouts root section of the config, empty when absent or malformed
func TimeoutsSection(cfg config.Tree) map[string]any {
	if cfg == nil {
		return map[string]any{}
	}
	if m := cfg.Map("timeouts"); m != nil {
		return m
	}
	return map[string]any{}
}

func lookupDotted(section map[string]any, key string) (any, bool) {
	var node any = section
	for _, part := range strings.Split(key, ".") {
		m, ok := node.(map[string]any)
		if !ok {
			return nil, false
		}
		node, ok = m[part]
		if !ok {
			return nil, false
		}
	}
	return node, true
}

// ResolveTimeout picks timeouts.<key> over the env var over the default, invalid values fall through with a warning
func ResolveTimeout(cfg config.Tree, key string, def time.Duration, hasDefault bool, envVar string) (time.Duration, bool) {
	if raw, ok := lookupDotted(TimeoutsSection(cfg), key); ok && raw != nil {
		if _, isBool := raw.(bool); !isBool {
			if f, isNum := config.AsFloat(raw); isNum && !math.IsNaN(f) {
				return ClampTimeout(f)
			}
			if s, isStr := raw.(string); isStr {
				if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil && !math.IsNaN(f) {
					return ClampTimeout(f)
				}
			}
		}
		slog.Warn(fmt.Sprintf("timeouts.%s: invalid value %v in config.yaml; ignoring", key, raw))
	}
	if envVar != "" {
		if envRaw := strings.TrimSpace(os.Getenv(envVar)); envRaw != "" {
			if f, err := strconv.ParseFloat(envRaw, 64); err == nil {
				return ClampTimeout(f)
			}
			slog.Warn(fmt.Sprintf("invalid %s=%q; ignoring", envVar, envRaw))
		}
	}
	if !hasDefault {
		return 0, false
	}
	return ClampTimeout(def.Seconds())
}

// RunBounded runs fn under a wall clock deadline, abandoning it on expiry after marking the backend suspect
func RunBounded(ctx context.Context, fn func(ctx context.Context) (any, error), timeout time.Duration, bounded bool, label string, onTimeout func(), backend any) (BoundedResult, error) {
	start := deadlineNow()
	if !bounded {
		v, err := fn(ctx)
		return BoundedResult{Value: v, Elapsed: deadlineNow().Sub(start), Label: label}, err
	}
	type outcome struct {
		value any
		err   error
	}
	done := make(chan outcome, 1)
	workCtx, cancel := context.WithCancel(ctx)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- outcome{err: fmt.Errorf("%s panicked: %v", label, r)}
			}
		}()
		v, err := fn(workCtx)
		done <- outcome{value: v, err: err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case o := <-done:
		cancel()
		return BoundedResult{Value: o.value, Elapsed: deadlineNow().Sub(start), Timeout: timeout, Bounded: true, Label: label}, o.err
	case <-ctx.Done():
		cancel()
		return BoundedResult{Elapsed: deadlineNow().Sub(start), Timeout: timeout, Bounded: true, Label: label}, ctx.Err()
	case <-timer.C:
	}
	cancel()
	slog.Warn(fmt.Sprintf("[deadline] %q timed out after %.1fs; worker abandoned", label, timeout.Seconds()))
	markBackendSuspect(backend, label, timeout)
	if onTimeout != nil {
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Debug(fmt.Sprintf("deadline on_timeout callback failed: %v", r))
				}
			}()
			onTimeout()
		}()
	}
	go func() {
		watchdog := time.NewTimer(loopBlockedDumpGrace)
		defer watchdog.Stop()
		select {
		case <-done:
		case <-watchdog.C:
			slog.Warn(fmt.Sprintf("[deadline] %q deadline (%.0fs) expired but the worker has not returned after a further %.0fs — it appears BLOCKED in a synchronous call, which is why no timeout can fire. Dumping all goroutine stacks to stderr to identify the blocking frame.", label, timeout.Seconds(), loopBlockedDumpGrace.Seconds()))
			dumpAllStacks()
		}
	}()
	return BoundedResult{TimedOut: true, Elapsed: deadlineNow().Sub(start), Timeout: timeout, Bounded: true, Label: label}, nil
}
