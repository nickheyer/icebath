package provider

import (
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"time"
)

const (
	streamHeartbeatInterval = 30 * time.Second
	streamMonitorPoll       = 300 * time.Millisecond
	sleepThreshold          = 5 * time.Second
)

// Reports how long the process stood still past one check interval, ignoring gaps under five seconds
type sleepWatch struct {
	interval time.Duration
	wall     time.Time
}

// Seconds stopped since the previous check or zero
func (w *sleepWatch) slept(now time.Time) time.Duration {
	stopped := now.Round(0).Sub(w.wall.Round(0)) - w.interval
	w.wall = now
	if stopped >= sleepThreshold {
		return stopped
	}
	return 0
}

var localLoadNotice = managedLocalLoadNotice

// Display and heartbeat phase of one streamed chat completion attempt
type streamWaitMonitor struct {
	s                 *ccStreamState
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
	sleep             *sleepWatch
}

// Monitor armed at the attempt start with the stale timeout in seconds
func newStreamWaitMonitor(s *ccStreamState, baseURL string, stale float64, now time.Time) *streamWaitMonitor {
	model := "the provider"
	if s.req != nil && s.req.Model != "" {
		model = s.req.Model
	}
	m := &streamWaitMonitor{s: s, model: model, baseURL: baseURL, stale: stale, lastHeartbeat: now,
		localBase: baseURL != "" && IsLocalEndpoint(baseURL)}
	if u, err := url.Parse(baseURL); baseURL != "" && err == nil && IsLoopbackHost(u.Hostname()) {
		m.sleep = &sleepWatch{interval: streamMonitorPoll, wall: now}
	}
	return m
}

func (m *streamWaitMonitor) waitNotice(text string) error {
	return m.s.emit(StreamEvent{Kind: EventWaitNotice, Text: text})
}

func (m *streamWaitMonitor) touch(text string) error {
	return m.s.emit(StreamEvent{Kind: EventActivity, Text: text})
}

// One poll of the monitor, true when a computer sleep moved the stall clock
func (m *streamWaitMonitor) tick(now time.Time, lastChunk *time.Time) (bool, error) {
	moved := false
	if m.sleep != nil {
		if slept := m.sleep.slept(now); slept > 0 {
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
	if waitingSecs < 60 {
		return m.touch(fmt.Sprintf("waiting for stream response (%ds, no chunks yet)", waitingSecs))
	}
	var watchdog *WatchdogDeadline
	if !math.IsInf(m.stale, 0) {
		watchdog = &WatchdogDeadline{Label: "stream stale", Remaining: m.stale - float64(waitingSecs)}
	}
	phase := "first_chunk"
	if !m.s.firstChunkAt.IsZero() {
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
	if text, ok := localLoadNotice(m.baseURL, m.model, m.s.estTokens); ok {
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
	if m.s.progress == nil || m.s.firstChunkAt.IsZero() || !m.loadNoticeShown {
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
