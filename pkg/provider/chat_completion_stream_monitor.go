package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	streamHeartbeatInterval = 30 * time.Second
	streamMonitorPoll       = 300 * time.Millisecond
)

var localLoadNotice = managedLocalLoadNotice

// Kills of one dispatched attempt whose reader never unwinds before the watchdog gives up on it
var staleAbandonKills = 3

// Attempt state the display monitor reads, implemented by the chat and anthropic stream states
type streamMonitorHost interface {
	emit(StreamEvent) error
	monitorModel() string
	monitorFirstChunk() time.Time
	monitorPromptProgress() bool
	monitorEstTokens() int
}

// Display and heartbeat phase of one streamed chat completion attempt
type streamWaitMonitor struct {
	h                 streamMonitorHost
	w                 *streamWatchdog
	model             string
	baseURL           string
	localBase         bool
	stale             float64
	lastHeartbeat     time.Time
	lastLoadPoll      time.Time
	loadNoticeShown   bool
	loadNoticeMisses  int
	waitNoticeStarted time.Time
	notice            WaitNoticeState
	sleep             *tools.SleepWatch
}

// Monitor armed at the attempt start with the stale timeout in seconds
func newStreamWaitMonitor(h streamMonitorHost, baseURL string, stale float64, now time.Time) *streamWaitMonitor {
	model := "the provider"
	if name := h.monitorModel(); name != "" {
		model = name
	}
	m := &streamWaitMonitor{h: h, model: model, baseURL: baseURL, stale: stale, lastHeartbeat: now,
		localBase: baseURL != "" && IsLocalEndpoint(baseURL)}
	if u, err := url.Parse(baseURL); baseURL != "" && err == nil && IsLoopbackHost(u.Hostname()) {
		m.sleep = tools.NewSleepWatch(streamMonitorPoll.Seconds())
	}
	return m
}

func (m *streamWaitMonitor) waitNotice(text string) error {
	return m.h.emit(StreamEvent{Kind: EventWaitNotice, Text: text})
}

func (m *streamWaitMonitor) touch(text string) error {
	return m.h.emit(StreamEvent{Kind: EventActivity, Text: text})
}

// One poll of the monitor, true when a computer sleep moved the stall clock
func (m *streamWaitMonitor) tick(now time.Time, lastChunk *time.Time) (bool, error) {
	moved := false
	if m.sleep != nil {
		if seconds := m.sleep.Slept(); seconds > 0 {
			slept := time.Duration(seconds * float64(time.Second))
			moved = true
			*lastChunk = minTime(lastChunk.Add(slept), now)
			m.lastHeartbeat = minTime(m.lastHeartbeat.Add(slept), now)
			slog.Info(fmt.Sprintf("Computer slept for %.0fs during a model request; the stall timer skips that time.", slept.Seconds()))
			if err := m.touch(fmt.Sprintf("resumed after the computer slept for %ds", int(slept.Seconds()))); err != nil {
				return moved, err
			}
		}
	}
	if m.localBase {
		if shown, err := m.pollLocalLoadNotice(now, *lastChunk); err != nil || shown {
			return moved, err
		}
	}
	if !m.waitNoticeStarted.IsZero() && lastChunk.After(m.waitNoticeStarted) {
		if err := m.waitNotice(""); err != nil {
			return moved, err
		}
		m.waitNoticeStarted = time.Time{}
		m.notice.Reset()
	}
	if now.Sub(m.lastHeartbeat) >= streamHeartbeatInterval {
		m.lastHeartbeat = now
		return moved, m.heartbeat(int(now.Sub(*lastChunk).Seconds()))
	}
	return moved, nil
}

// Gateway liveness touch, plus one neutral wait notice per silence once no chunk arrived for a minute
func (m *streamWaitMonitor) heartbeat(waitingSecs int) error {
	if m.w != nil && !m.w.claimsLiveness() {
		return nil
	}
	if waitingSecs < 60 {
		return m.touch(fmt.Sprintf("waiting for stream response (%ds, no chunks yet)", waitingSecs))
	}
	var watchdog *WatchdogDeadline
	if !math.IsInf(m.stale, 0) {
		watchdog = &WatchdogDeadline{Label: "stream stale", Remaining: m.stale - float64(waitingSecs)}
	}
	phase := "first_chunk"
	if !m.h.monitorFirstChunk().IsZero() {
		phase = "post_chunk"
	}
	if !m.notice.ShouldEmit(phase, watchdog) {
		return m.touch(fmt.Sprintf("waiting for stream response (%ds, %s)", waitingSecs, phase))
	}
	m.waitNoticeStarted = m.lastHeartbeat
	return m.waitNotice(WaitNoticeText(m.model, float64(waitingSecs), phase, watchdog))
}

// Managed local server weight load progress shown instead of the neutral notice, true while it owns the line
func (m *streamWaitMonitor) pollLocalLoadNotice(now, lastChunk time.Time) (bool, error) {
	if shown, err := m.clearPromptProgress(); err != nil || shown {
		return shown, err
	}
	if now.Sub(lastChunk) < 2*time.Second || now.Sub(m.lastLoadPoll) < time.Second {
		return false, nil
	}
	m.lastLoadPoll = now
	if text, ok := localLoadNotice(m.baseURL, m.model, m.h.monitorEstTokens()); ok {
		m.waitNoticeStarted = time.Time{}
		m.notice.Reset()
		if err := m.waitNotice(text); err != nil {
			return true, err
		}
		if err := m.touch("local model loading"); err != nil {
			return true, err
		}
		m.loadNoticeShown, m.loadNoticeMisses, m.lastHeartbeat = true, 0, now
		return true, nil
	}
	if m.loadNoticeShown {
		m.loadNoticeMisses++
		if m.loadNoticeMisses >= 3 {
			m.loadNoticeShown, m.loadNoticeMisses = false, 0
			return false, m.waitNotice("")
		}
	}
	return false, nil
}

// Clears the prompt reading line once the first token lands after llama-server progress chunks
func (m *streamWaitMonitor) clearPromptProgress() (bool, error) {
	if !m.h.monitorPromptProgress() || m.h.monitorFirstChunk().IsZero() || !m.loadNoticeShown {
		return false, nil
	}
	m.loadNoticeShown, m.loadNoticeMisses = false, 0
	return false, m.waitNotice("")
}

// Prompt reading progress line that owns the display until the first token
func (m *streamWaitMonitor) showPromptProgress(now time.Time, text string) error {
	m.waitNoticeStarted = time.Time{}
	m.notice.Reset()
	m.lastLoadPoll = now
	m.loadNoticeShown, m.loadNoticeMisses, m.lastHeartbeat = true, 0, now
	return m.waitNotice(text)
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// Request scoped bookkeeping shared by the stream attempts of one call
type streamCall struct {
	mu              sync.Mutex
	attempt         int
	inline          bool
	discardedChunks int
	discardedBytes  int
	diag            *StreamDiag
	hooks           streamHooks
}

type streamCallKey struct{}

func withStreamCall(ctx context.Context, c *streamCall) context.Context {
	return context.WithValue(ctx, streamCallKey{}, c)
}

// Call of the current attempt, a lone attempt outside the reconnect loop is attempt one of a worker call
func streamCallFrom(ctx context.Context) *streamCall {
	if c, ok := ctx.Value(streamCallKey{}).(*streamCall); ok && c != nil {
		return c
	}
	return &streamCall{attempt: 1}
}

// Starts the next attempt of the call and returns its id
func (c *streamCall) start() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.attempt++
	return c.attempt
}

// Fresh upstream diagnostics for the wire call now opening, read by the retry loop when it drops
func (c *streamCall) newDiag() *StreamDiag {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.diag = StreamDiagInit()
	return c.diag
}

func (c *streamCall) currentDiag() *StreamDiag {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.diag
}

func (c *streamCall) current() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.attempt
}

// Counts a chunk a killed attempt still delivered, warning on the first one of the call
func (c *streamCall) discard(attempt int, chunk any) {
	size := 0
	if chunk != nil {
		size = pystr.Len(pyrepr.Repr(chunk))
	}
	c.mu.Lock()
	c.discardedChunks++
	c.discardedBytes += size
	chunks, bytes := c.discardedChunks, c.discardedBytes
	c.mu.Unlock()
	if chunks == 1 {
		catalogLog.Warn(fmt.Sprintf("Discarding chunk from superseded stream attempt %d (discarded_chunks=%d discarded_bytes=%d)", attempt, chunks, bytes))
		return
	}
	catalogLog.Debug(fmt.Sprintf("Discarded stale stream chunk from attempt %d (discarded_chunks=%d discarded_bytes=%d)", attempt, chunks, bytes))
}

// StreamAbandonedError ends a streaming call whose attempt reader the watchdog kills could not unwind
type StreamAbandonedError struct {
	Err error
}

func (e *StreamAbandonedError) Error() string { return e.Err.Error() }

func (e *StreamAbandonedError) Unwrap() error { return e.Err }

// Stale and lifetime kills of one streamed attempt, the wait for its reader to unwind and the give up
type streamWatchdog struct {
	requestCtx context.Context
	cancel     context.CancelFunc
	kill       *ccKill
	call       *streamCall
	attempt    int
	readerDone <-chan struct{}
	emit       func(StreamEvent) error
	classify   func(error) error
	model      string
	estTokens  int
	stale      time.Duration
	lifetime   time.Duration
	staleTimer *time.Timer
	lifeTimer  *time.Timer
	lastChunk  time.Time
	lifeStart  time.Time
	kills      int
	counted    bool
	killed     bool
	withheldAt time.Time
	abandoned  error
}

// Watchdog armed at the attempt start, the request context carries the interrupt and the call bookkeeping
func newStreamWatchdog(requestCtx context.Context, cancel context.CancelFunc, kill *ccKill, readerDone <-chan struct{}, emit func(StreamEvent) error, classify func(error) error, model string, estTokens int, t ccTimeouts, started time.Time) *streamWatchdog {
	call := streamCallFrom(requestCtx)
	w := &streamWatchdog{requestCtx: requestCtx, cancel: cancel, kill: kill, call: call, attempt: call.current(), readerDone: readerDone,
		emit: emit, classify: classify, model: orUnknown2(model), estTokens: estTokens, stale: t.stale, lifetime: t.lifetime,
		staleTimer: time.NewTimer(t.stale), lastChunk: started, lifeStart: started}
	if t.lifetime > 0 {
		w.lifeTimer = time.NewTimer(t.lifetime)
	}
	staleText, _ := pyrepr.Format(t.stale.Seconds(), ".0f")
	if t.stale >= secondsDuration(math.Inf(1)) {
		staleText = "inf"
	}
	capText, _ := pyrepr.Format(t.lifetime.Seconds(), ".0f")
	catalogLog.Debug(fmt.Sprintf("stream watchdog armed: stale=%ss cap=%ss model=%s", staleText, capText, w.model))
	return w
}

// Logs a monitor loop that dies by panic before letting the panic continue
func (w *streamWatchdog) logAbnormalExit() {
	if r := recover(); r != nil {
		catalogLog.Error("stream watchdog monitor loop aborted abnormally", "panic", r)
		panic(r)
	}
}

func (w *streamWatchdog) stop() {
	w.staleTimer.Stop()
	if w.lifeTimer != nil {
		w.lifeTimer.Stop()
	}
}

func (w *streamWatchdog) staleC() <-chan time.Time { return w.staleTimer.C }

func (w *streamWatchdog) lifeC() <-chan time.Time {
	if w.lifeTimer == nil {
		return nil
	}
	return w.lifeTimer.C
}

// Read timeout channel, silenced once a kill hands the attempt to its unwinding reader
func (w *streamWatchdog) readC(c <-chan time.Time) <-chan time.Time {
	if w.killed {
		return nil
	}
	return c
}

// Reader exit channel watched only after a kill
func (w *streamWatchdog) unwound() <-chan struct{} {
	if !w.killed {
		return nil
	}
	return w.readerDone
}

// Records a real chunk, restarting the stale window and lifting a liveness hold
func (w *streamWatchdog) chunk(now time.Time) {
	w.lastChunk = now
	resetTimer(w.staleTimer, w.stale)
}

// Re-arms the stale timer after a sleep skip moved the stall clock
func (w *streamWatchdog) moved(now time.Time) {
	resetTimer(w.staleTimer, w.stale-now.Sub(w.lastChunk))
}

func (w *streamWatchdog) claimsLiveness() bool {
	return w.withheldAt.IsZero() || w.lastChunk.After(w.withheldAt)
}

// Kills a silent attempt and re-arms, true once the same attempt was killed too often and the call is abandoned
func (w *streamWatchdog) onStale(now time.Time) bool {
	elapsed := now.Sub(w.lastChunk).Seconds()
	catalogLog.Warn(fmt.Sprintf("Stream stale for %.0fs (threshold %.0fs) — no chunks received. model=%s context=~%s tokens. Killing connection.", elapsed, w.stale.Seconds(), w.model, commaInt(w.estTokens)))
	_ = w.emit(StreamEvent{Kind: EventStatus, Text: fmt.Sprintf("⚠️ No response from provider for %ds (model: %s, context: ~%s tokens). Reconnecting...", int(elapsed), w.model, commaInt(w.estTokens))})
	w.kill.fire("stale", elapsed)
	w.cancel()
	w.killed = true
	if !w.counted {
		w.counted = true
		bumpStaleStreak(w.requestCtx)
	}
	return w.rearmOrAbandon(now, elapsed)
}

// Restarts the stale and cap clocks after a stale kill and announces the reconnect unless the attempt is abandoned
func (w *streamWatchdog) rearmOrAbandon(now time.Time, elapsed float64) bool {
	w.lastChunk = now
	resetTimer(w.staleTimer, w.stale)
	if w.lifeTimer != nil {
		w.lifeStart = now
		resetTimer(w.lifeTimer, w.lifetime)
	}
	if w.noteAttemptKill(elapsed, "stale") {
		return true
	}
	if w.claimsLiveness() {
		_ = w.emit(StreamEvent{Kind: EventWaitNotice, Diagnostic: true, Text: fmt.Sprintf("⚠ no output from provider for %ds — reconnecting...", int(elapsed))})
		_ = w.emit(StreamEvent{Kind: EventActivity, Text: fmt.Sprintf("stale stream detected after %ds, reconnecting", int(elapsed))})
	}
	return false
}

// Cuts an attempt past agent.stream_max_lifetime without feeding the stale breaker, true once it is abandoned
func (w *streamWatchdog) onLifetime(now time.Time) bool {
	if now.Sub(w.lastChunk) >= w.stale {
		return w.onStale(now)
	}
	elapsed := now.Sub(w.lifeStart).Seconds()
	capSeconds := w.lifetime.Seconds()
	catalogLog.Warn(fmt.Sprintf("Stream still running after %.0fs (stream_max_lifetime %.0fs) — killing connection. model=%s", elapsed, capSeconds, w.model))
	_ = w.emit(StreamEvent{Kind: EventStatus, Text: fmt.Sprintf("⚠️ Response still streaming after %ds (stream_max_lifetime %ds, model: %s). Aborting...", int(elapsed), int(capSeconds), w.model)})
	w.kill.capKill(elapsed)
	w.cancel()
	w.killed = true
	w.counted = true
	w.lastChunk = now
	resetTimer(w.staleTimer, w.stale)
	w.lifeStart = now
	resetTimer(w.lifeTimer, w.lifetime)
	return w.noteAttemptKill(elapsed, "stream_max_lifetime")
}

// Counts a kill of this attempt, abandoning a worker call or withholding liveness of an inline one at the limit
func (w *streamWatchdog) noteAttemptKill(elapsed float64, kind string) bool {
	rekill := w.kills > 0
	if rekill {
		w.kills++
	} else {
		w.kills = 1
		w.withheldAt = time.Time{}
	}
	if w.kills < staleAbandonKills {
		return false
	}
	if !w.call.inline {
		w.abandonWedgedWorker(elapsed, kind)
		return true
	}
	if w.withheldAt.IsZero() {
		catalogLog.Error(fmt.Sprintf("Inline streaming call did not unwind after %d %s kills of attempt %d; no longer reporting it as active, so the inactivity watchdog can end it. model=%s", w.kills, kind, w.attempt, w.model))
	}
	w.withheldAt = w.lastChunk
	return false
}

// Leaves the wedged reader behind and ends the call with a retryable timeout naming the kill kind
func (w *streamWatchdog) abandonWedgedWorker(elapsed float64, kind string) {
	catalogLog.Error(fmt.Sprintf("Streaming worker did not unwind after %d %s kills of attempt %d (%.0fs); abandoning the request. model=%s", w.kills, kind, w.attempt, elapsed, w.model))
	var cause error
	if kind == "stale" {
		cause = fmt.Errorf("Provider stream stalled: no chunks and the request worker did not unwind after %d stale-stream kills; request abandoned.", w.kills)
	} else {
		cause = &StreamLifetimeExceededError{Elapsed: elapsed, Cap: w.lifetime.Seconds()}
	}
	base := &TimeoutPhaseError{Phase: TimeoutPhaseRead, Err: &ccTimeoutError{err: cause}}
	joinWorkerForRelayTeardown(w.requestCtx, w.readerDone, "Abandoned streaming")
	w.abandoned = &StreamAbandonedError{Err: w.classify(base)}
}

// Interrupt seen by the watchdog, the pre response wait is recorded once and the reader joined for Relay teardown
func (w *streamWatchdog) interrupt(now time.Time, responseStarted bool) error {
	elapsed := now.Sub(w.lastChunk).Seconds()
	if !w.counted && elapsed <= w.stale.Seconds() && recordInterruptedProviderWait(w.requestCtx, elapsed, responseStarted) {
		w.counted = true
	}
	catalogLog.Debug("Force-closing streaming httpx client due to interrupt (not a network error).")
	w.cancel()
	if !w.call.inline {
		joinWorkerForRelayTeardown(w.requestCtx, w.readerDone, "Streaming")
	}
	return w.requestCtx.Err()
}

// Error of an attempt whose reader unwound after a kill, a cap kill names the setting with the reader error as cause
func streamKillCause(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	return err
}

// Stale kill over the header wait of an attempt, measured from the attempt start like the monitor's stale window
func armHeaderStaleKill(requestCtx context.Context, cancel context.CancelFunc, kill *ccKill, emit func(StreamEvent) error, model string, estTokens int, stale time.Duration, started time.Time) func() {
	if stale <= 0 || stale >= secondsDuration(math.Inf(1)) {
		return func() {}
	}
	var mu sync.Mutex
	stopped := false
	timer := time.AfterFunc(stale-time.Since(started), func() {
		mu.Lock()
		defer mu.Unlock()
		if stopped {
			return
		}
		elapsed := time.Since(started).Seconds()
		model := orUnknown2(model)
		catalogLog.Warn(fmt.Sprintf("Stream stale for %.0fs (threshold %.0fs) — no chunks received. model=%s context=~%s tokens. Killing connection.", elapsed, stale.Seconds(), model, commaInt(estTokens)))
		_ = emit(StreamEvent{Kind: EventStatus, Text: fmt.Sprintf("⚠️ No response from provider for %ds (model: %s, context: ~%s tokens). Reconnecting...", int(elapsed), model, commaInt(estTokens))})
		kill.fire("stale", elapsed)
		bumpStaleStreak(requestCtx)
		cancel()
	})
	return func() {
		timer.Stop()
		mu.Lock()
		stopped = true
		mu.Unlock()
	}
}
