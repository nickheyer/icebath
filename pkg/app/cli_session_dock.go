package app

import (
	"fmt"
	"math"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Queued prompts the dock lists before a more line
const sessionDockQueueRows = 3

// Rereads the goal and loop managers once after automation state went stale mid turn
func sessionDockSyncAutomation(c *CLI) {
	c.uiMu.Lock()
	stale := c.automationStale
	c.automationStale = false
	goalMgr, loopMgr, sessionID := c.goalManager, c.loopManager, c.sessionID
	c.uiMu.Unlock()
	if !stale {
		return
	}
	if goalMgr != nil && goalMgr.SessionID == sessionID {
		goalMgr.Refresh()
	}
	if loopMgr != nil && loopMgr.SessionID == sessionID {
		loopMgr.Refresh(c.ctx)
	}
}

// Status line of an active or paused goal of the held manager, empty otherwise
func sessionDockGoalLine(c *CLI) string {
	c.uiMu.Lock()
	mgr, sessionID := c.goalManager, c.sessionID
	c.uiMu.Unlock()
	if mgr == nil || mgr.SessionID != sessionID || !mgr.HasGoal() {
		return ""
	}
	return mgr.StatusLine()
}

// Time left rounded up, whole minutes until the last minute and seconds after
func sessionDockCountdown(seconds float64) string {
	left := max(0, int(math.Ceil(seconds)))
	if left < 60 {
		return fmt.Sprintf("%ds", left)
	}
	total := int(math.Ceil(float64(left) / 60))
	hours, minutes := total/60, total%60
	if hours > 0 {
		return fmt.Sprintf("%dh%02dm", hours, minutes)
	}
	return fmt.Sprintf("%dm", minutes)
}

// Collapsed badge, expanded row and prompt of an active or paused loop, all empty otherwise
func sessionDockLoopRow(c *CLI, now float64) (string, string, string) {
	c.uiMu.Lock()
	mgr, sessionID := c.loopManager, c.sessionID
	c.uiMu.Unlock()
	if mgr == nil || mgr.SessionID != sessionID {
		return "", "", ""
	}
	s := mgr.State()
	if s == nil || (s.Status != "active" && s.Status != "paused") {
		return "", "", ""
	}
	var runs string
	switch {
	case s.Times != 0:
		runs = fmt.Sprintf("%d/%d runs", s.TicksFired, s.Times)
	case s.TicksFired != 1:
		runs = fmt.Sprintf("%d runs", s.TicksFired)
	default:
		runs = fmt.Sprintf("%d run", s.TicksFired)
	}
	prompt := pyrepr.Str(s.Prompt)
	if s.Status == "paused" {
		reason := ""
		if pyrepr.Truthy(s.PausedReason) {
			reason = " · " + pyrepr.Str(s.PausedReason)
		}
		return "⏸ loop paused", "⏸ loop paused" + reason + " · " + runs + " · " + prompt, prompt
	}
	var when string
	if s.AwaitingResponse {
		when = "running now"
	} else if remaining := s.NextDueAt - now; remaining > 0 {
		when = "next in " + sessionDockCountdown(remaining)
	} else {
		when = "due, waiting for idle"
	}
	return "↻ loop " + when, "↻ loop · " + s.CadenceLabel() + " · " + runs + " · " + when + " · " + prompt, prompt
}

// One line preview of a pending input item, its display text, text or string form
func sessionDockPromptText(item any) string {
	var text string
	switch v := item.(type) {
	case submittedInput:
		text = v.Text
	case *submittedInput:
		text = v.Text
	case tools.TimelineNotification:
		text = v.DisplayText
		if text == "" {
			text = v.Text
		}
	case *tools.TimelineNotification:
		text = v.DisplayText
		if text == "" {
			text = v.Text
		}
	case *voiceInputMessage:
		text = v.Text
	case *seededQueryMessage:
		text = v.Text
	default:
		text = queueItemText(item)
	}
	return strings.Join(pystr.SplitWhitespace(text, -1), " ")
}

// Previews of the gateway's queued admissions then the turns waiting in the pending input, oldest first
func sessionDockQueuedPrompts(c *CLI) []string {
	var items []any
	if store := c.gatewayQueueStore(); store != nil {
		for _, text := range publishedQueue(c, store) {
			items = append(items, text)
		}
	}
	if c.pendingInput != nil {
		items = append(items, c.pendingInput.snapshot()...)
	}
	var out []string
	for _, item := range items {
		if text := sessionDockPromptText(item); text != "" {
			out = append(out, text)
		}
	}
	return out
}
