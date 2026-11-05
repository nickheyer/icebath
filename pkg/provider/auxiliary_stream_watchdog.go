package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/sleepclock"
)

// Awake clock seam, seconds that stop while the computer sleeps
var auxAwakeSeconds = func() float64 { return sleepclock.AwakeSeconds() }

// AuxTimeoutError is the Python TimeoutError an auxiliary stream raises when its progress window lapses
type AuxTimeoutError struct {
	Message string
	Err     error
}

func (e *AuxTimeoutError) Error() string { return e.Message }
func (e *AuxTimeoutError) Unwrap() error { return e.Err }

// Implemented by a chunk stream whose blocked read another goroutine may interrupt
type auxAttemptSocket interface {
	// Pins this attempt's connection, onRelease runs before the stream hands the connection back
	auxPinAttempt(onRelease func()) (shutdown func())
}

// Accumulator a chat stream feeds, Feed reports a substantive chunk
type auxStreamFeeder interface {
	Feed(chunk map[string]any) bool
	HasFinishReason() bool
}

// Inter chunk window and optional first token window of a streamed attempt
type auxStreamWindows struct {
	Window           float64
	FirstTokenWindow float64
	HasFirstToken    bool
}

// ChatStreamWatchdog cuts a silent chat stream once its awake clock window lapses without substantive chunks
type ChatStreamWatchdog struct {
	Window           float64
	FirstTokenWindow float64
	hasFirstToken    bool
	started          float64
	mu               sync.Mutex
	deadline         float64
	hasDeadline      bool
	timer            *time.Timer
	generation       int
	done             bool
	sawProgress      bool
	fired            bool
	shutdown         func()
}

// Arms the first token window and pins the attempt connection so a late timer never cuts a pooled one
func newChatStreamWatchdog(stream any, w auxStreamWindows) *ChatStreamWatchdog {
	wd := &ChatStreamWatchdog{Window: w.Window, FirstTokenWindow: w.FirstTokenWindow, hasFirstToken: w.HasFirstToken, started: auxAwakeSeconds()}
	if s, ok := stream.(auxAttemptSocket); ok {
		wd.shutdown = s.auxPinAttempt(func() {
			wd.mu.Lock()
			wd.done = true
			wd.mu.Unlock()
		})
	}
	if w.HasFirstToken {
		wd.mu.Lock()
		wd.rearm(w.FirstTokenWindow)
		wd.mu.Unlock()
	}
	return wd
}

// Moves the deadline and replaces the pending timer only when the new deadline is earlier
func (w *ChatStreamWatchdog) rearm(window float64) {
	previous, hadPrevious := w.deadline, w.hasDeadline
	w.deadline, w.hasDeadline = auxAwakeSeconds()+window, true
	if w.timer == nil || (hadPrevious && w.deadline < previous) {
		if w.timer != nil {
			w.timer.Stop()
		}
		w.schedule(window)
	}
}

func (w *ChatStreamWatchdog) schedule(delay float64) {
	w.generation++
	gen := w.generation
	w.timer = time.AfterFunc(time.Duration(math.Max(delay, 0)*float64(time.Second)), func() { w.fire(gen) })
}

// Re-arms the inter chunk window after a substantive chunk
func (w *ChatStreamWatchdog) Progress() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sawProgress = true
	if !w.done {
		w.rearm(w.Window)
	}
}

// Shuts the attempt connection down once the awake deadline has truly passed for the current timer
func (w *ChatStreamWatchdog) fire(generation int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done || !w.hasDeadline || generation != w.generation {
		return
	}
	if remaining := w.deadline - auxAwakeSeconds(); remaining > 0 {
		w.schedule(remaining)
		return
	}
	w.fired = true
	if w.shutdown != nil {
		w.shutdown()
	}
}

// Retires the watchdog so no pending timer acts again
func (w *ChatStreamWatchdog) Finish() {
	w.mu.Lock()
	w.done = true
	timer := w.timer
	w.mu.Unlock()
	if timer != nil {
		timer.Stop()
	}
}

// Fired reports whether the window lapsed
func (w *ChatStreamWatchdog) Fired() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.fired
}

// SawProgress reports whether any substantive chunk arrived
func (w *ChatStreamWatchdog) SawProgress() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.sawProgress
}

// No progress timeout before the first output, stalled timeout after it
func (w *ChatStreamWatchdog) TimeoutError() *AuxTimeoutError {
	elapsed := auxAwakeSeconds() - w.started
	if !w.SawProgress() {
		first := 0.0
		if w.hasFirstToken {
			first = w.FirstTokenWindow
		}
		return &AuxTimeoutError{Message: fmt.Sprintf("Auxiliary chat stream produced no output within %.1fs (no-progress timeout, %.1fs elapsed)", first, elapsed)}
	}
	return &AuxTimeoutError{Message: fmt.Sprintf("Auxiliary chat stream stalled: no new output for %.1fs (%.1fs elapsed, timed out)", w.Window, elapsed)}
}

// Inter chunk and first token windows for a streamed attempt, both capped at the request timeout
func auxChatStreamWindows(ctx context.Context, client AuxClient, kwargs map[string]any, task string) auxStreamWindows {
	window := auxGetTaskNoProgressTimeout(ctx, task)
	if window == 0 {
		window = auxStreamNoProgressTimeoutSeconds
	}
	out := auxStreamWindows{Window: window}
	baseURL := ""
	if client != nil {
		baseURL = client.BaseURL()
	}
	if !IsLocalEndpoint(baseURL) {
		provider := ""
		if relay := auxRelayAuxCallContext(ctx); relay != nil {
			if p, ok := relay["stream_provider"].(string); ok {
				provider = p
			}
		}
		model, _ := kwargs["model"].(string)
		out.FirstTokenWindow, out.HasFirstToken = math.Max(window, cloudStaleTimeoutFor(ctx, provider, model, kwargs)), true
	}
	if timeout, ok := auxPositiveNumber(kwargs["timeout"]); ok {
		out.Window = math.Min(out.Window, timeout)
		if out.HasFirstToken {
			out.FirstTokenWindow = math.Min(out.FirstTokenWindow, timeout)
		}
	}
	return out
}

// Positive int or float value as seconds, booleans count as Python ints
func auxPositiveNumber(v any) (float64, bool) {
	var f float64
	switch x := v.(type) {
	case int:
		f = float64(x)
	case int64:
		f = float64(x)
	case float64:
		f = x
	case bool:
		if x {
			f = 1
		}
	default:
		return 0, false
	}
	return f, f > 0
}

// Closes a chunk stream and ignores a failing close
func auxCloseChunkStream(chunks any) {
	if c, ok := chunks.(interface{ Close() error }); ok {
		_ = c.Close()
	}
}

// Feeds the chunks into the accumulator, cutting a silent stream unless its terminal chunk already arrived
func auxConsumeChatStream(ctx context.Context, chunks AuxChatStream, acc auxStreamFeeder, noProgress *auxStreamWindows) error {
	if noProgress == nil {
		for {
			chunk, err := chunks.Next(ctx)
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			acc.Feed(chunk)
		}
	}
	watchdog := newChatStreamWatchdog(chunks, *noProgress)
	err := func() error {
		defer watchdog.Finish()
		for {
			chunk, err := chunks.Next(ctx)
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				if auxIsBaseException(err) || !watchdog.Fired() {
					return err
				}
				if !acc.HasFinishReason() {
					timeout := watchdog.TimeoutError()
					timeout.Err = err
					return timeout
				}
				return nil
			}
			if acc.Feed(chunk) {
				watchdog.Progress()
			}
		}
	}()
	if err != nil {
		return err
	}
	if watchdog.Fired() && !acc.HasFinishReason() {
		return watchdog.TimeoutError()
	}
	return nil
}
