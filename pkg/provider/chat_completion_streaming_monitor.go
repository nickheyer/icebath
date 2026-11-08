package provider

import (
	"fmt"
	"math"
	"net/http"
	"net/url"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Display, heartbeat and stale kill bookkeeping the poll loop owns
type streamingMonitor struct {
	lastHeartbeat     time.Time
	lastLoadPoll      time.Time
	waitNoticeStarted time.Time
	loadNoticeShown   bool
	loadNoticeMisses  int
	notice            WaitNoticeState
	lastKilled        int
	sameKills         int
	withheldAt        time.Time
	lifetime          streamingLifetime
}

// Attempt and elapsed seconds of the latest stream_max_lifetime kill with the cap it enforced
type streamingLifetimeKill struct {
	attempt      int
	elapsed, cap float64
}

// Opt in stream lifetime clock restarting for every attempt and after each kill
type streamingLifetime struct {
	cap     float64
	attempt int
	started time.Time
}

// Seconds the attempt ran past the cap, restarting the clock, false while under it or for a new attempt
func (l *streamingLifetime) exceeded(attempt int) (float64, bool) {
	if l.cap == 0 {
		return 0, false
	}
	now := time.Now()
	if attempt != l.attempt {
		l.attempt, l.started = attempt, now
		return 0, false
	}
	elapsed := now.Sub(l.started).Seconds()
	if elapsed <= l.cap {
		return 0, false
	}
	l.started = now
	return elapsed, true
}

func (c *streamingCall) kwargsModel(fallback string) string {
	if m, ok := c.apiKwargs["model"]; ok {
		return pyrepr.Str(m)
	}
	return fallback
}

func (c *streamingCall) currentStaleTimeout() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.staleTimeout
}

func (c *streamingCall) currentAttempt() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.current
}

func (c *streamingCall) callIsDone() bool {
	select {
	case <-c.callDone:
		return true
	default:
		return false
	}
}

func (c *streamingCall) firstChunkSeen() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.diag != nil && c.diag.FirstChunkAt != nil
}

// Polls until the call ends, logging and re-raising a monitor that dies by panic
func streamingCallMonitorLoop(c *streamingCall) {
	defer func() {
		if r := recover(); r != nil {
			streamingCallLog.Error("stream watchdog monitor loop aborted abnormally", "panic", r)
			panic(r)
		}
	}()
	c.pollUntilDone()
}

// Heartbeat, sleep skip, local load notice, stale kill, lifetime cap and interrupt abort every 300ms
func (c *streamingCall) pollUntilDone() {
	const heartbeatInterval = 30 * time.Second
	c.mon = streamingMonitor{lastHeartbeat: time.Now()}
	baseURL := c.agent.BaseURL()
	isLocalBase := baseURL != "" && IsLocalEndpoint(baseURL)
	var sleep *sleepWatch
	if u, err := url.Parse(baseURL); baseURL != "" && err == nil && IsLoopbackHost(u.Hostname()) {
		sleep = &sleepWatch{interval: streamMonitorPoll, wall: time.Now()}
	}
	stale := c.currentStaleTimeout()
	lifeCap := resolveStreamMaxLifetime(loadWireSettings(c.ctx).streamMaxLifetime, stale)
	c.mon.lifetime = streamingLifetime{cap: lifeCap, started: time.Now()}
	streamingCallLog.Debug(fmt.Sprintf("stream watchdog armed: stale=%.0fs cap=%.0fs model=%s", stale, lifeCap, c.kwargsModel("unknown")))
	for !c.callIsDone() {
		select {
		case <-c.callDone:
		case <-time.After(streamMonitorPoll):
		}
		if sleep != nil {
			c.skipSleep(sleep.slept(time.Now()).Seconds())
		}
		hbNow := time.Now()
		if isLocalBase && c.pollLocalLoadNotice(hbNow) {
			continue
		}
		if !c.mon.waitNoticeStarted.IsZero() && c.lastChunkTime().After(c.mon.waitNoticeStarted) {
			c.agent.EmitWaitNotice("")
			c.mon.waitNoticeStarted = time.Time{}
			c.mon.notice.Reset()
		}
		if hbNow.Sub(c.mon.lastHeartbeat) >= heartbeatInterval {
			c.mon.lastHeartbeat = hbNow
			c.heartbeat(int(hbNow.Sub(c.lastChunkTime()).Seconds()))
		}
		staleElapsed := time.Since(c.lastChunkTime()).Seconds()
		if staleElapsed > c.currentStaleTimeout() {
			c.mon.waitNoticeStarted = time.Time{}
			c.mon.notice.Reset()
			c.killStaleStream(staleElapsed)
		}
		c.checkStreamLifetime()
		if c.agent.InterruptRequested() {
			c.abortForInterrupt(staleElapsed)
			return
		}
	}
}

// True unless a wedged inline call stopped claiming activity and no real chunk arrived since
func (c *streamingCall) claimsLiveness() bool {
	since := c.mon.withheldAt
	return since.IsZero() || c.lastChunkTime().After(since)
}

// Moves the stall clock past a computer sleep so the request regains its full patience after wake
func (c *streamingCall) skipSleep(slept float64) {
	if slept <= 0 {
		return
	}
	now := time.Now()
	gap := secondsDuration(slept)
	c.lastChunk.Store(minTime(c.lastChunkTime().Add(gap), now).UnixNano())
	c.mon.lastHeartbeat = minTime(c.mon.lastHeartbeat.Add(gap), now)
	streamingCallLog.Info(fmt.Sprintf("Computer slept for %.0fs during a model request; the stall timer skips that time.", slept))
	c.agent.TouchActivity(fmt.Sprintf("resumed after the computer slept for %ds", int(slept)))
}

// Gateway inactivity touch, plus one neutral wait notice per silence once no chunk arrived for a minute
func (c *streamingCall) heartbeat(waitingSecs int) {
	if !c.claimsLiveness() {
		return
	}
	if waitingSecs < 60 {
		c.agent.TouchActivity(fmt.Sprintf("waiting for stream response (%ds, no chunks yet)", waitingSecs))
		return
	}
	stale := c.currentStaleTimeout()
	var watchdog *WatchdogDeadline
	if !math.IsInf(stale, 0) {
		watchdog = &WatchdogDeadline{Label: "stream stale", Remaining: stale - float64(waitingSecs)}
	}
	phase := "first_chunk"
	if c.firstChunkSeen() {
		phase = "post_chunk"
	}
	if !c.mon.notice.ShouldEmit(phase, watchdog) {
		c.agent.TouchActivity(fmt.Sprintf("waiting for stream response (%ds, %s)", waitingSecs, phase))
		return
	}
	c.mon.waitNoticeStarted = c.mon.lastHeartbeat
	c.agent.EmitWaitNotice(WaitNoticeText(c.kwargsModel("the provider"), float64(waitingSecs), phase, watchdog))
}

// Managed local server load progress in place of the neutral notice, true while it owns the line
func (c *streamingCall) pollLocalLoadNotice(now time.Time) bool {
	if c.showPromptProgress(now) {
		return true
	}
	if now.Sub(c.lastChunkTime()) < 2*time.Second || now.Sub(c.mon.lastLoadPoll) < time.Second {
		return false
	}
	c.mon.lastLoadPoll = now
	if text, ok := localLoadNotice(c.agent.BaseURL(), c.kwargsModel(""), EstimateRequestContextTokens(c.ctx, c.apiKwargs)); ok {
		c.mon.waitNoticeStarted = time.Time{}
		c.mon.notice.Reset()
		c.agent.EmitWaitNotice(text)
		c.agent.TouchActivity("local model loading")
		c.mon.loadNoticeShown, c.mon.loadNoticeMisses, c.mon.lastHeartbeat = true, 0, now
		return true
	}
	if c.mon.loadNoticeShown {
		c.mon.loadNoticeMisses++
		if c.mon.loadNoticeMisses >= 3 {
			c.mon.loadNoticeShown, c.mon.loadNoticeMisses = false, 0
			c.agent.EmitWaitNotice("")
		}
	}
	return false
}

// Prompt reading progress from llama server progress chunks at most once a second until the first token
func (c *streamingCall) showPromptProgress(now time.Time) bool {
	c.mu.Lock()
	progress, at := c.progress, c.progressAt
	firstToken := c.diag != nil && c.diag.FirstChunkAt != nil
	c.mu.Unlock()
	if progress == nil {
		return false
	}
	if firstToken {
		if c.mon.loadNoticeShown {
			c.mon.loadNoticeShown, c.mon.loadNoticeMisses = false, 0
			c.agent.EmitWaitNotice("")
		}
		return false
	}
	if now.Sub(at) > 60*time.Second || now.Sub(c.mon.lastLoadPoll) < time.Second {
		return false
	}
	c.mon.lastLoadPoll = now
	c.mon.waitNoticeStarted = time.Time{}
	c.mon.notice.Reset()
	if percent, ok := promptProgressPercent(progress); ok {
		c.agent.EmitWaitNotice(fmt.Sprintf("⚙ processing prompt — %d%%", percent))
	} else {
		c.agent.EmitWaitNotice("⚙ processing prompt")
	}
	c.mon.loadNoticeShown, c.mon.loadNoticeMisses, c.mon.lastHeartbeat = true, 0, now
	return true
}

// Cuts an attempt past agent.stream_max_lifetime like a stale kill without striking the stale breaker
func (c *streamingCall) checkStreamLifetime() {
	if c.mon.lifetime.cap == 0 || c.callIsDone() {
		return
	}
	c.mu.Lock()
	attempt, released := c.current, c.releasedAttempt
	c.mu.Unlock()
	if attempt == 0 || attempt == released {
		return
	}
	elapsed, over := c.mon.lifetime.exceeded(attempt)
	if !over {
		return
	}
	model := c.kwargsModel("unknown")
	capSeconds := c.mon.lifetime.cap
	streamingCallLog.Warn(fmt.Sprintf("Stream still running after %.0fs (stream_max_lifetime %.0fs) — killing connection. model=%s", elapsed, capSeconds, model))
	c.agent.BufferDiagnosticStatus(fmt.Sprintf("⚠️ Response still streaming after %ds (stream_max_lifetime %ds, model: %s). Aborting...", int(elapsed), int(capSeconds), model))
	c.mu.Lock()
	killed := c.attemptResponse
	c.lifetimeKill = &streamingLifetimeKill{attempt: attempt, elapsed: elapsed, cap: capSeconds}
	c.mu.Unlock()
	quietCall(func() {
		c.cancelCurrentStreamAttempt("stream_max_lifetime_kill")
		_ = c.clients.closeOnce("stream_max_lifetime_kill", nil)
	})
	streamingCallShutdownAttemptSocket(c, killed)
	c.mu.Lock()
	c.staleCounted[attempt] = true
	c.mu.Unlock()
	c.touchChunkClock()
	c.noteAttemptKill(elapsed, "stream_max_lifetime")
}

// Worker error of an attempt the cap killed, replaced by the lifetime error naming the setting
func (c *streamingCall) asLifetimeError(err error) error {
	c.mu.Lock()
	kill, current := c.lifetimeKill, c.current
	c.mu.Unlock()
	if _, isCap := err.(*StreamLifetimeExceededError); kill == nil || isCap {
		return err
	}
	if kill.attempt != current {
		return err
	}
	return &StreamLifetimeExceededError{Elapsed: kill.elapsed, Cap: kill.cap, Cause: err}
}

// Restarts the stale and cap clocks after a stale kill and announces the reconnect unless abandoned
func (c *streamingCall) rearmOrAbandonAfterKill(elapsed float64) {
	now := time.Now()
	c.lastChunk.Store(now.UnixNano())
	c.mon.lifetime.started = now
	if c.noteAttemptKill(elapsed, "stale") {
		return
	}
	if c.claimsLiveness() {
		c.agent.EmitDiagnosticWait(fmt.Sprintf("⚠ no output from provider for %ds — reconnecting...", int(elapsed)))
		c.agent.TouchActivity(fmt.Sprintf("stale stream detected after %ds, reconnecting", int(elapsed)))
	}
}

// Counts a kill of the current attempt, abandoning a wedged worker or withholding an inline call's liveness at the limit
func (c *streamingCall) noteAttemptKill(elapsed float64, kind string) bool {
	c.mu.Lock()
	attempt, released := c.current, c.releasedAttempt
	c.mu.Unlock()
	if attempt == released {
		return false
	}
	rekill := attempt != 0 && attempt == c.mon.lastKilled
	c.mon.lastKilled = attempt
	if rekill {
		c.mon.sameKills++
	} else {
		c.mon.sameKills = 1
		c.mon.withheldAt = time.Time{}
	}
	if attempt == 0 || c.mon.sameKills < staleAbandonKills {
		return false
	}
	if !c.inline {
		c.abandonWedgedWorker(elapsed, attempt, kind)
		return true
	}
	if c.mon.withheldAt.IsZero() {
		streamingCallLog.Error(fmt.Sprintf("Inline streaming call did not unwind after %d %s kills of attempt %d; no longer reporting it as active, so the inactivity watchdog can end it. model=%s",
			c.mon.sameKills, kind, attempt, c.kwargsModel("unknown")))
	}
	c.mon.withheldAt = c.lastChunkTime()
	return false
}

// Leaves the wedged worker behind and ends the call with a retryable timeout naming the kill kind
func (c *streamingCall) abandonWedgedWorker(elapsed float64, attempt int, kind string) {
	streamingCallLog.Error(fmt.Sprintf("Streaming worker did not unwind after %d %s kills of attempt %d (%.0fs); abandoning the request. model=%s",
		c.mon.sameKills, kind, attempt, elapsed, c.kwargsModel("unknown")))
	c.requestCancelled.Store(true)
	var abandon error
	if kind == "stale" {
		abandon = &TimeoutPhaseError{Phase: TimeoutPhaseRead, Err: fmt.Errorf("Provider stream stalled: no chunks and the request worker did not unwind after %d stale-stream kills; request abandoned.", c.mon.sameKills)}
	} else {
		abandon = &StreamLifetimeExceededError{Elapsed: elapsed, Cap: c.mon.lifetime.cap}
	}
	c.mu.Lock()
	c.abandonErr = abandon
	c.mu.Unlock()
	c.markCallDone()
}

// Started attempt the stale breaker has not counted yet, else zero
func (c *streamingCall) uncountedStaleAttempt() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.staleCounted[c.current] {
		return 0
	}
	return c.current
}

func (c *streamingCall) countStaleAttempt() {
	attempt := c.uncountedStaleAttempt()
	if attempt == 0 {
		return
	}
	c.mu.Lock()
	c.staleCounted[attempt] = true
	c.mu.Unlock()
	bumpStaleStreak(c.ctx)
}

// Cancels the silent attempt, aborts the request local client and shuts the killed attempt's socket
func (c *streamingCall) killStaleStream(elapsed float64) {
	est := EstimateRequestContextTokens(c.ctx, c.apiKwargs)
	model := c.kwargsModel("unknown")
	streamingCallLog.Warn(fmt.Sprintf("Stream stale for %.0fs (threshold %.0fs) — no chunks received. model=%s context=~%s tokens. Killing connection.",
		elapsed, c.currentStaleTimeout(), model, commaInt(est)))
	c.agent.BufferDiagnosticStatus(fmt.Sprintf("⚠️ No response from provider for %ds (model: %s, context: ~%s tokens). Reconnecting...", int(elapsed), model, commaInt(est)))
	c.mu.Lock()
	killed := c.attemptResponse
	c.mu.Unlock()
	quietCall(func() {
		c.cancelCurrentStreamAttempt("stale_stream_kill")
		_ = c.clients.closeOnce("stale_stream_kill", nil)
	})
	streamingCallShutdownAttemptSocket(c, killed)
	c.countStaleAttempt()
	c.rearmOrAbandonAfterKill(elapsed)
}

// Interrupt seen by the monitor, the attempt cancelled, its socket shut and the worker joined before flagging it
func (c *streamingCall) abortForInterrupt(staleElapsed float64) {
	if attempt := c.uncountedStaleAttempt(); attempt != 0 && staleElapsed <= c.currentStaleTimeout() &&
		recordInterruptedProviderWait(c.ctx, staleElapsed, c.deltasWereSent.Load()) {
		c.mu.Lock()
		c.staleCounted[attempt] = true
		c.mu.Unlock()
	}
	c.requestCancelled.Store(true)
	streamingCallLog.Debug("Force-closing streaming httpx client due to interrupt (not a network error).")
	c.mu.Lock()
	killed := c.attemptResponse
	c.mu.Unlock()
	quietCall(func() {
		c.cancelCurrentStreamAttempt("stream_interrupt_abort")
		_ = c.clients.closeOnce("stream_interrupt_abort", nil)
	})
	streamingCallShutdownAttemptSocket(c, killed)
	if !c.inline {
		joinWorkerForRelayTeardown(c.ctx, c.workerDone, "Streaming")
	}
	c.monitorInterrupted.Store(true)
}

// Shuts down both directions of the killed attempt's own socket without closing it so its reader unwinds
func streamingCallShutdownAttemptSocket(c *streamingCall, resp *http.Response) {
	c.mu.Lock()
	current, connOf := c.attemptResponse, c.attemptConn
	c.mu.Unlock()
	if resp == nil || resp != current {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			streamingCallLog.Debug("Stale stream socket shutdown failed", "exc_info", r)
		}
	}()
	if connOf != nil {
		if conn := connOf(); conn != nil {
			auxShutdownConn(conn)
			streamingCallLog.Info(fmt.Sprintf("Shut down the stale stream's socket to unblock the reader (attempt superseded; model=%s).", c.kwargsModel("unknown")))
			return
		}
	}
	streamingCallLog.Debug("Stale stream socket shutdown found no socket; pool sweep is the only abort")
}
