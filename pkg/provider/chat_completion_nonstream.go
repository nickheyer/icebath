package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

var nonStreamLog = slog.Default()

const (
	interruptedWaitStaleSeconds = 30.0
	nonStreamPollInterval       = 300 * time.Millisecond
	nonStreamWaitNoticeSilence  = 60.0
)

type nonStreamSurfaceKey struct{}

// Context carrying the sink a blocking request reports wait notices, activity and status through
func withNonStreamSurface(ctx context.Context, sink func(StreamEvent)) context.Context {
	return context.WithValue(ctx, nonStreamSurfaceKey{}, sink)
}

// Event sink of one request, the stream callback when streaming else the blocking request sink of the context
func nonStreamEmitter(ctx context.Context, fn StreamFunc) func(StreamEvent) {
	if fn != nil {
		return func(ev StreamEvent) { _ = fn(ev) }
	}
	sink, _ := ctx.Value(nonStreamSurfaceKey{}).(func(StreamEvent))
	return func(ev StreamEvent) {
		if sink != nil {
			sink(ev)
		}
	}
}

func epochSeconds(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

// Epoch seconds of a watchdog marker, nil while the marker is unset
func markerSeconds(t time.Time) *float64 {
	if t.IsZero() {
		return nil
	}
	v := epochSeconds(t)
	return &v
}

// One snapshot of the request watchdog markers in epoch seconds, nil fields are unset
type nonStreamSnapshot struct {
	lastEvent, lastProgress, retryStarted *float64
}

// Wait notice and heartbeat state of one request polled by its caller
type nonStreamWaiter struct {
	model     string
	callStart float64
	wd        codexWatchdogs
	started   *float64
	notice    WaitNoticeState
	polls     int
	surface   func(StreamEvent)
}

func newNonStreamWaiter(model string, callStart float64, wd codexWatchdogs, surface func(StreamEvent)) *nonStreamWaiter {
	return &nonStreamWaiter{model: model, callStart: callStart, wd: wd, surface: surface}
}

// Physical attempt origin, the reconnect marker else the call start
func (w *nonStreamWaiter) attemptStarted(s nonStreamSnapshot) float64 {
	if s.retryStarted != nil {
		return *s.retryStarted
	}
	return w.callStart
}

// Stream open on this attempt with no substantive model progress yet
func (w *nonStreamWaiter) preProgress(s nonStreamSnapshot) bool {
	return w.wd.progress > 0 && s.lastEvent != nil && s.lastProgress == nil
}

func (w *nonStreamWaiter) touch(text string) { w.surface(StreamEvent{Kind: EventActivity, Text: text}) }

func (w *nonStreamWaiter) waitNotice(text string) {
	w.surface(StreamEvent{Kind: EventWaitNotice, Text: text})
}

// Counts one poll and refreshes the wait notice, with a heartbeat on every hundredth poll
func (w *nonStreamWaiter) poll(now float64, s nonStreamSnapshot) {
	w.polls++
	w.emitWaitNotice(now-w.callStart, w.polls%100 == 0, s)
}

// Watchdog deadline seam the wait notice tests swap the way hermes tests patch codex_watchdog_deadline
var codexWatchdogDeadlineFn = CodexWatchdogDeadline

// Clears a notice once this request shows activity and posts one neutral notice per silence of sixty seconds
func (w *nonStreamWaiter) emitWaitNotice(elapsed float64, heartbeat bool, s nonStreamSnapshot) {
	defer func() {
		if r := recover(); r != nil {
			nonStreamLog.Debug(fmt.Sprintf("wait-notice construction failed: %v", r))
		}
	}()
	pre := w.preProgress(s)
	var activity *float64
	switch {
	case pre:
		a := w.attemptStarted(s)
		activity = &a
	case s.retryStarted != nil:
		activity = s.retryStarted
	default:
		activity = s.lastEvent
	}
	if w.started != nil && activity != nil && *activity > *w.started {
		w.waitNotice("")
		w.started = nil
		w.notice.Reset()
	}
	if !heartbeat {
		return
	}
	origin := w.callStart
	if activity != nil {
		origin = *activity
	}
	silence := w.callStart + elapsed - origin
	if silence < nonStreamWaitNoticeSilence {
		if s.retryStarted != nil {
			w.touch("waiting for first stream event after reconnect")
		} else {
			w.touch("waiting for provider response")
		}
		return
	}
	phase := "first_event"
	switch {
	case pre:
		phase = "pre_progress"
	case s.retryStarted != nil:
		phase = "reconnect"
	case s.lastEvent != nil:
		phase = "post_event"
	}
	watchdog := codexWatchdogDeadlineFn(CodexWatchdogClock{StaleTimeout: w.wd.stale, TTFBEnabled: w.wd.ttfbEnabled, TTFBTimeout: w.wd.ttfb,
		LastEventTS: s.lastEvent, LastProgressTS: s.lastProgress, RetryStartedTS: s.retryStarted, CallStart: w.callStart,
		IdleEnabled: w.wd.idleEnabled, IdleTimeout: w.wd.idle, IdleRequiresProgress: w.wd.idleRequiresProgress,
		ProgressTimeout: w.wd.progress, Elapsed: elapsed})
	if !w.notice.ShouldEmit(phase, watchdog) {
		w.touch(fmt.Sprintf("waiting for provider response (%ds, %s)", int(silence), phase))
		return
	}
	model := w.model
	if model == "" {
		model = "the provider"
	}
	w.waitNotice(WaitNoticeText(model, silence, phase, watchdog))
	started := w.callStart + elapsed
	w.started = &started
}

// Counts a user aborted pre response stall past the wait notice interval toward the stale breaker
func recordInterruptedProviderWait(ctx context.Context, elapsed float64, responseStarted bool) bool {
	if responseStarted || elapsed < interruptedWaitStaleSeconds {
		return false
	}
	bumpStaleStreak(ctx)
	streak := 0
	if s, _ := ctx.Value(staleStreakKey{}).(*RetrySession); s != nil {
		streak = s.StaleStreak()
	}
	nonStreamLog.Warn(fmt.Sprintf("Interrupted provider wait counted as stale after %.0fs with no output; consecutive stale attempts=%d.", elapsed, streak))
	return true
}

// Logs a stale non streaming kill and buffers its status line
func reportStaleNonstreamKill(surface func(StreamEvent), model string, estTokens int, elapsed, stale float64, inline bool, hint string) {
	prefix := "N"
	if inline {
		prefix = "Inline n"
	}
	model = orUnknown2(model)
	nonStreamLog.Warn(fmt.Sprintf("%son-streaming API call stale for %.0fs (threshold %.0fs). model=%s context=~%s tokens. Killing connection.", prefix, elapsed, stale, model, commaInt(estTokens)))
	if hint == "" {
		hint = "Aborting call."
	}
	surface(StreamEvent{Kind: EventStatus, Text: fmt.Sprintf("⚠️ No response from provider for %ds (non-streaming, model: %s). %s", int(elapsed), model, hint)})
}

// Records the activity line of a stale non streaming kill
func touchStaleKillActivity(surface func(StreamEvent), elapsed float64) {
	surface(StreamEvent{Kind: EventActivity, Text: fmt.Sprintf("stale non-streaming call killed after %ds", int(elapsed))})
}

// Waits up to two seconds for the worker only while Relay managed execution is live
func joinWorkerForRelayTeardown[T any](ctx context.Context, done <-chan T, label string) {
	rt := tools.GetRelayRuntime(ctx, false, "")
	if rt == nil || !rt.ManagedExecutionEnabled() {
		return
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		slog.Warn(fmt.Sprintf("%s worker still alive after interrupt abort (2.0s join timeout); Relay teardown will best-effort drain orphaned scopes (#81521).", label))
	}
}

type nonStreamResult struct {
	resp *Response
	err  error
}

// Runs one blocking request on a worker polled for wait notices, the stale kill and interrupts
func runNonStreamRequest(ctx context.Context, model string, estTokens int, stale float64, call func(context.Context) (*Response, error)) (*Response, error) {
	if inlineNonStream(ctx) {
		return runInlineNonStreamRequest(ctx, model, estTokens, stale, call)
	}
	surface := nonStreamEmitter(ctx, nil)
	w := newNonStreamWaiter(model, epochSeconds(time.Now()), codexWatchdogs{stale: stale, estTokens: estTokens}, surface)
	w.touch("waiting for non-streaming API response")
	reqCtx, abort := context.WithCancel(ctx)
	defer abort()
	done := make(chan nonStreamResult, 1)
	var cancelled atomic.Bool
	go func() {
		resp, err := call(reqCtx)
		if err != nil && cancelled.Load() {
			nonStreamLog.Debug(fmt.Sprintf("Non-streaming worker caught %T after request cancellation — exiting without surfacing a network error.", err))
		}
		done <- nonStreamResult{resp, err}
	}()
	ticker := time.NewTicker(nonStreamPollInterval)
	defer ticker.Stop()
	for {
		select {
		case r := <-done:
			return r.resp, r.err
		case <-ctx.Done():
			recordInterruptedProviderWait(ctx, epochSeconds(time.Now())-w.callStart, false)
			cancelled.Store(true)
			nonStreamLog.Debug("Force-closing httpx client due to interrupt (not a network error).")
			abort()
			joinWorkerForRelayTeardown(ctx, done, "Non-streaming")
			return nil, ctx.Err()
		case t := <-ticker.C:
			now := epochSeconds(t)
			elapsed := now - w.callStart
			w.poll(now, nonStreamSnapshot{})
			if elapsed > stale {
				reportStaleNonstreamKill(surface, model, estTokens, elapsed, stale, false, "")
				abort()
				bumpStaleStreak(ctx)
				touchStaleKillActivity(surface, elapsed)
				return awaitWorkerAfterKill(done, fmt.Sprintf("Non-streaming API call timed out after %ds with no response (threshold: %ds)", int(elapsed), int(stale)))
			}
		}
	}
}

// Gives a killed worker two seconds to land its own result, else the kill timeout
func awaitWorkerAfterKill(done <-chan nonStreamResult, message string) (*Response, error) {
	select {
	case r := <-done:
		if r.err == nil && r.resp != nil {
			return r.resp, nil
		}
		if r.err != nil && !errors.Is(r.err, context.Canceled) {
			return nil, r.err
		}
	case <-time.After(2 * time.Second):
	}
	return nil, &TimeoutPhaseError{Phase: TimeoutPhaseRead, Err: errors.New(message)}
}

// Feeds a blocking chat completion's headers to the credits capture only for a nous usage window session and response
func chatCompletionCapturingHeaders(provider string, credits *CreditsCapture, h http.Header) {
	if credits == nil || !chatCompletionCapturesHeaders(provider, credits.Credits(), h) {
		return
	}
	runHeaderCaptures(h, credits.CaptureRateLimits, func(h http.Header) { credits.CaptureCredits(h, false) },
		credits.CaptureNousModelSwitch, credits.CheckOpenRouterCacheStatus)
}

// Runs every capture so a failing one never fails the turn or skips the rest
func runHeaderCaptures(h http.Header, captures ...func(http.Header)) {
	for _, capture := range captures {
		func() {
			defer func() {
				if r := recover(); r != nil {
					nonStreamLog.Debug(fmt.Sprintf("non-streaming header capture failed: %v", r))
				}
			}()
			capture(h)
		}()
	}
}

// Whether a blocking chat completion feeds its headers, a nous usage window session answered by a usage window response
func chatCompletionCapturesHeaders(provider string, state *CreditsState, h http.Header) bool {
	return provider == "nous" && state != nil && state.IsUsageWindow() && ResponseIsUsageWindow(h)
}
