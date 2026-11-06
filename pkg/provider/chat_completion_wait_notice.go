package provider

import (
	"fmt"
	"math"
)

// Seconds before a watchdog fires inside which the notice says still waiting and updates once
const NearDeadlineSecs = 15.0

// Names the watchdog that would reconnect and the seconds until it fires
type WatchdogDeadline struct {
	Label     string
	Remaining float64
}

// True when a watchdog applies and fires within the near deadline window
func nearDeadline(w *WatchdogDeadline) bool { return w != nil && w.Remaining <= NearDeadlineSecs }

var waitPhaseText = map[string]string{
	"first_event":  "%ds waiting for the first provider event",
	"reconnect":    "%ds waiting for the first provider event after reconnect",
	"pre_progress": "provider stream open; %ds without substantive model progress",
	"post_event":   "provider stream active; %ds without stream events",
	"first_chunk":  "%ds waiting for the first stream chunk",
	"post_chunk":   "stream open; %ds without stream output",
}

// One neutral status line naming the wait phase and the watchdog that would reconnect
func WaitNoticeText(model string, silenceSecs float64, phase string, watchdog *WatchdogDeadline) string {
	lead := "waiting on"
	if nearDeadline(watchdog) {
		lead = "still waiting on"
	}
	text := fmt.Sprintf("⏳ %s %s — ", lead, model) + fmt.Sprintf(waitPhaseText[phase], int(silenceSecs))
	if watchdog != nil {
		text += fmt.Sprintf(" (auto-reconnect: %s watchdog in %ds)", watchdog.Label, max(0, int(watchdog.Remaining)))
	}
	return text
}

// Codex request watchdog configuration and its event clock in epoch seconds
type CodexWatchdogClock struct {
	StaleTimeout         float64
	TTFBEnabled          bool
	TTFBTimeout          float64
	LastEventTS          *float64
	LastProgressTS       *float64
	RetryStartedTS       *float64
	CallStart            float64
	IdleEnabled          bool
	IdleTimeout          float64
	IdleRequiresProgress bool
	Elapsed              float64
	ProgressTimeout      float64
}

// Earliest enabled Codex watchdog with its remaining seconds, nil when none applies or its deadline passed
func CodexWatchdogDeadline(c CodexWatchdogClock) *WatchdogDeadline {
	finite := func(f float64) bool { return !math.IsInf(f, 0) && !math.IsNaN(f) }
	var deadlines []WatchdogDeadline
	if finite(c.StaleTimeout) {
		deadlines = append(deadlines, WatchdogDeadline{"wall-clock stale", c.StaleTimeout})
	}
	attemptOffset := 0.0
	if c.RetryStartedTS != nil {
		attemptOffset = math.Max(0, *c.RetryStartedTS-c.CallStart)
	}
	switch {
	case c.LastEventTS == nil:
		if c.TTFBEnabled && finite(c.TTFBTimeout) {
			deadlines = append(deadlines, WatchdogDeadline{"TTFB", attemptOffset + c.TTFBTimeout})
		}
	case c.ProgressTimeout > 0 && c.LastProgressTS == nil:
		deadlines = append(deadlines, WatchdogDeadline{"first progress", attemptOffset + c.ProgressTimeout})
	case (!c.IdleRequiresProgress || c.LastProgressTS != nil) && c.IdleEnabled && finite(c.IdleTimeout):
		deadlines = append(deadlines, WatchdogDeadline{"stream idle", math.Max(0, *c.LastEventTS-c.CallStart) + c.IdleTimeout})
	}
	if len(deadlines) == 0 {
		return nil
	}
	best := deadlines[0]
	for _, d := range deadlines[1:] {
		if d.Remaining < best.Remaining {
			best = d
		}
	}
	if best.Remaining <= c.Elapsed {
		return nil
	}
	return &WatchdogDeadline{best.Label, best.Remaining - c.Elapsed}
}

// Remembers which wait phase and watchdog the status line currently shows
type WaitNoticeState struct {
	phase         string
	watchdogLabel string
	hasLabel      bool
	nearShown     bool
}

// Forgets the shown notice so the next silence gets a fresh one
func (w *WaitNoticeState) Reset() { *w = WaitNoticeState{} }

// True for the first notice of a silence, a phase or watchdog change, and once near the deadline
func (w *WaitNoticeState) ShouldEmit(phase string, watchdog *WatchdogDeadline) bool {
	label, hasLabel := "", watchdog != nil
	if hasLabel {
		label = watchdog.Label
	}
	near := nearDeadline(watchdog)
	emit := w.phase != phase || w.hasLabel != hasLabel || w.watchdogLabel != label || (near && !w.nearShown)
	w.phase, w.watchdogLabel, w.hasLabel = phase, label, hasLabel
	if near {
		w.nearShown = true
	}
	return emit
}
