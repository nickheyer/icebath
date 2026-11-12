package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/goals"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	DefaultLoopSelfPacedCeilingSeconds = 15 * 60
	LoopCompleteMarker                 = "LOOP_COMPLETE"
)

var (
	loopCompleteRe   = pyre.MustCompile(`(?im)^\s*LOOP_COMPLETE\s*[.!]?\s*$`)
	loopDigestClock  = pyre.MustCompile(`\d{1,2}:\d{2}(:\d{2})?`)
	loopDigestDate   = pyre.MustCompile(`\d{4}-\d{2}-\d{2}`)
	loopDigestSpan   = pyre.MustCompile(`\b\d+(\.\d+)?\s*(s|sec|secs|seconds|m|min|mins|minutes|h|hr|hrs|hours)\b`)
	loopDigestSpaces = pyre.MustCompile(`\s+`)
)

// Wakeup message framing a loop prompt for its tick number and cadence
const loopWakeupPromptTemplate = "[/loop wakeup #%[1]d%[2]s]\n" +
	"Recurring task: %[3]s\n\n" +
	"This is an automatic wakeup from the /loop the user set. Perform the " +
	"task now against the CURRENT state (re-check files, processes, or " +
	"services fresh — do not assume anything from earlier iterations still " +
	"holds). Report concisely what you found or did this iteration.\n" +
	"If the task is now complete, no longer applicable, or the thing you " +
	"were watching has finished, say so and end your reply with " +
	LoopCompleteMarker + " on its own line — that stops the loop."

// Wakeup message framing a loop prompt that carries an until stop condition
const loopWakeupPromptWithUntilTemplate = "[/loop wakeup #%[1]d%[2]s]\n" +
	"Recurring task: %[3]s\n\n" +
	"Stop condition: %[4]s\n\n" +
	"This is an automatic wakeup from the /loop the user set. Perform the " +
	"task now against the CURRENT state (re-check files, processes, or " +
	"services fresh — do not assume anything from earlier iterations still " +
	"holds). Report concisely what you found or did this iteration, and " +
	"show concrete evidence of the stop condition's status.\n" +
	"If the stop condition is met, or the task is no longer applicable, say " +
	"so and end your reply with " + LoopCompleteMarker + " on its own line — " +
	"that stops the loop."

// Self paced ceiling from loops config, never below the floor
func loopSelfPacedCeilingSeconds(ctx context.Context) int64 {
	floor := loopSelfPacedFloorSeconds(ctx)
	return loopsConfigInt(ctx, "self_paced_ceiling_seconds", max(floor, DefaultLoopSelfPacedCeilingSeconds), floor)
}

// Compact hours minutes seconds rendering of a duration, 90 becoming 1m30s
func FormatLoopInterval(seconds float64) string {
	total := int64(pyint.Round(max(0, seconds), 0))
	h, rem := total/3600, total%3600
	m, s := rem/60, rem%60
	var parts []string
	if h != 0 {
		parts = append(parts, fmt.Sprintf("%dh", h))
	}
	if m != 0 {
		parts = append(parts, fmt.Sprintf("%dm", m))
	}
	if s != 0 || len(parts) == 0 {
		parts = append(parts, fmt.Sprintf("%ds", s))
	}
	return strings.Join(parts, "")
}

// Python str of a stored loop field
func loopText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return pyrepr.Str(v)
}

// Cadence of the loop, every interval or self paced with its live delay
func (s *SessionLoopState) CadenceLabel() string {
	if s.Mode == "self_paced" {
		live := ""
		if s.CurrentDelay != 0 {
			live = ", currently " + FormatLoopInterval(s.CurrentDelay)
		}
		return "self-paced" + live
	}
	return "every " + FormatLoopInterval(s.IntervalSeconds)
}

// Time until the next tick of an active loop, empty for any other status
func (s *SessionLoopState) RemainingLabel() string {
	if s.Status != "active" {
		return ""
	}
	remaining := s.NextDueAt - nowSeconds()
	if remaining <= 0 {
		return "due now"
	}
	return "next in " + FormatLoopInterval(remaining)
}

// Count of ticks with its plural suffix
func loopTicksLabel(n int64) string {
	if n != 1 {
		return fmt.Sprintf("%d ticks", n)
	}
	return fmt.Sprintf("%d tick", n)
}

// Em dash clause carrying a reason, empty without one
func loopDash(reason any) string {
	if pyrepr.Truthy(reason) {
		return " — " + loopText(reason)
	}
	return ""
}

// Reports a reply ending with the loop complete marker on its own line
func ResponseSignalsComplete(response string) bool {
	if response == "" {
		return false
	}
	ok, _ := loopCompleteRe.MatchString(response)
	return ok
}

// SHA256 of the lowered reply with clock, date and duration tokens and extra whitespace removed
func digestLoopResponse(response string) string {
	text := pystr.Lower(pystr.Strip(response))
	for _, re := range []struct {
		pattern interface {
			Replace(string, string, int, int) (string, error)
		}
		with string
	}{{loopDigestClock, ""}, {loopDigestDate, ""}, {loopDigestSpan, ""}, {loopDigestSpaces, " "}} {
		if replaced, err := re.pattern.Replace(text, re.with, -1, -1); err == nil {
			text = replaced
		}
	}
	sum := sha256.Sum256(pystr.EncodeUTF8Replace(text))
	return hex.EncodeToString(sum[:])
}

// Reports an active or paused loop
func (m *LoopManager) HasLoop() bool {
	return m.state != nil && (m.state.Status == "active" || m.state.Status == "paused")
}

// One line summary of the loop with its cadence, caps and status
func (m *LoopManager) StatusLine() string {
	s := m.state
	if s == nil || s.Status == "cleared" {
		return "No loop set. Start one with /loop [interval] <prompt>."
	}
	fired := loopTicksLabel(s.TicksFired)
	var caps []string
	switch {
	case s.Times != 0:
		caps = []string{fmt.Sprintf("%d/%d runs", s.TicksFired, s.Times)}
	case s.MaxTicks != 0:
		caps = []string{fmt.Sprintf("%d/%d budget", s.TicksFired, s.MaxTicks)}
	default:
		caps = []string{fired}
	}
	if s.Until != "" {
		caps = append(caps, "until: "+s.Until)
	}
	meta := s.CadenceLabel() + ", " + strings.Join(caps, ", ")
	prompt := loopText(s.Prompt)
	switch s.Status {
	case "active":
		remaining := s.RemainingLabel()
		tail := ""
		if s.AwaitingResponse {
			tail = ", wakeup running"
		} else if remaining != "" {
			tail = ", " + remaining
		}
		return fmt.Sprintf("↻ Loop (active, %s%s): %s", meta, tail, prompt)
	case "paused":
		return fmt.Sprintf("⏸ Loop (paused, %s%s): %s", meta, loopDash(s.PausedReason), prompt)
	case "done":
		return fmt.Sprintf("✓ Loop finished (%s%s): %s", fired, loopDash(s.LastStopReason), prompt)
	}
	return fmt.Sprintf("Loop (%s, %s): %s", loopText(s.Status), meta, prompt)
}

// Reports an active loop not mid wakeup whose next due time has passed
func (m *LoopManager) IsDue() bool {
	return m.isDueAt(nowSeconds())
}

// Due check against an explicit clock reading
func (m *LoopManager) isDueAt(now float64) bool {
	s := m.state
	return s != nil && s.Status == "active" && !s.AwaitingResponse && now >= s.NextDueAt
}

// Claims a due tick and returns its wakeup message or the raw slash command, false when nothing is due
func (m *LoopManager) FireTick(ctx context.Context) (string, bool) {
	s := m.state
	if s == nil || !m.IsDue() {
		return "", false
	}
	s.TicksFired++
	s.LastFiredAt = nowSeconds()
	s.AwaitingResponse = true
	delay := s.CurrentDelay
	if delay == 0 {
		delay = s.IntervalSeconds
	}
	if delay == 0 {
		delay = float64(loopSelfPacedFloorSeconds(ctx))
	}
	s.NextDueAt = s.LastFiredAt + delay
	m.save(ctx)
	prompt := loopText(s.Prompt)
	if strings.HasPrefix(pystr.Lstrip(prompt), "/") {
		return pystr.Strip(prompt), true
	}
	cadence := ", self-paced"
	if s.Mode == "interval" {
		cadence = ", " + s.CadenceLabel()
	}
	if s.Until != "" {
		return fmt.Sprintf(loopWakeupPromptWithUntilTemplate, s.TicksFired, cadence, prompt, s.Until), true
	}
	return fmt.Sprintf(loopWakeupPromptTemplate, s.TicksFired, cadence, prompt), true
}

// Rolls back a fired tick whose injection failed
func (m *LoopManager) AbandonTick(ctx context.Context) {
	s := m.state
	if s == nil || !s.AwaitingResponse {
		return
	}
	s.AwaitingResponse = false
	s.TicksFired = max(0, s.TicksFired-1)
	m.save(ctx)
}

// LoopDecision is the outcome of a finished wakeup turn, Status nil when no loop exists
type LoopDecision struct {
	Status  any
	Stopped bool
	Reason  string
	Message string
}

// Persists a done or paused stop and builds its decision
func (m *LoopManager) stop(ctx context.Context, status, reason, message string) LoopDecision {
	s := m.state
	s.Status = status
	if status == "done" {
		s.LastStopReason = reason
	} else {
		s.PausedReason = reason
	}
	m.save(ctx)
	return LoopDecision{Status: status, Stopped: true, Reason: reason, Message: message}
}

// Evaluates a finished wakeup turn against the marker, the until judge and the caps, then schedules the next tick
func (m *LoopManager) CompleteTick(ctx context.Context, lastResponse string) LoopDecision {
	s := m.state
	if s == nil || !s.AwaitingResponse {
		var status any
		if s != nil {
			status = s.Status
		}
		return LoopDecision{Status: status, Reason: "no tick in flight"}
	}
	s.AwaitingResponse = false
	now := nowSeconds()
	ticks := loopTicksLabel(s.TicksFired)
	if ResponseSignalsComplete(lastResponse) {
		return m.stop(ctx, "done", "agent signaled the task is complete",
			fmt.Sprintf("✓ Loop finished after %s — task complete.", ticks))
	}
	if s.Until != "" && pystr.Strip(lastResponse) != "" {
		verdict, reason := loopJudgeUntil(ctx, s.Until, lastResponse)
		if verdict == "done" {
			return m.stop(ctx, "done", "stop condition met: "+reason,
				fmt.Sprintf("✓ Loop finished after %s — %s", ticks, reason))
		}
		if verdict == "blocked" {
			why := "stop condition judged unachievable: " + reason
			return m.stop(ctx, "paused", why,
				fmt.Sprintf("⏸ Loop paused — %s. /loop resume to keep going, /loop stop to end it.", why))
		}
	}
	if s.Times != 0 && s.TicksFired >= s.Times {
		return m.stop(ctx, "done", fmt.Sprintf("completed the requested %d runs", s.Times),
			fmt.Sprintf("✓ Loop finished — ran %d/%d times.", s.Times, s.Times))
	}
	if s.MaxTicks != 0 && s.TicksFired >= s.MaxTicks {
		return m.stop(ctx, "paused", fmt.Sprintf("tick budget exhausted (%d/%d)", s.TicksFired, s.MaxTicks),
			fmt.Sprintf("⏸ Loop paused — %d/%d ticks used (loops.max_ticks). /loop resume to keep going, /loop stop to end it.",
				s.TicksFired, s.MaxTicks))
	}
	if s.Mode == "self_paced" {
		digest := digestLoopResponse(lastResponse)
		floor := loopSelfPacedFloorSeconds(ctx)
		if digest != "" && digest == s.LastResponseDigest {
			s.CurrentDelay = min(max(s.CurrentDelay, float64(floor))*2, float64(loopSelfPacedCeilingSeconds(ctx)))
		} else {
			s.CurrentDelay = float64(floor)
		}
		s.LastResponseDigest = digest
	} else {
		s.CurrentDelay = s.IntervalSeconds
	}
	s.NextDueAt = now + s.CurrentDelay
	m.save(ctx)
	return LoopDecision{Status: "active", Reason: "loop continues"}
}

// Judge the until condition of a loop asks, the goal judge
var loopJudgeGoal = goals.JudgeGoal

// Goal judge verdict on an until condition, continue with the failure class when the judge panics
func loopJudgeUntil(ctx context.Context, until, lastResponse string) (verdict, reason string) {
	defer func() {
		if r := recover(); r != nil {
			verdict, reason = "continue", "judge unavailable: "+pyrepr.TypeName(r)
		}
	}()
	result := loopJudgeGoal(ctx, until, lastResponse, goals.JudgeOptions{})
	return result.Verdict, result.Reason
}

// Reports an active goal that is not parked, which defers this session's loop tick
func GoalBlocksLoopTick(ctx context.Context, sessionID string) (blocks bool) {
	defer func() {
		if recover() != nil {
			blocks = false
		}
	}()
	mgr := goals.NewGoalManager(ctx, sessionID, goals.DefaultGoalMaxTurns)
	return mgr.IsActive() && !mgr.IsWaiting()
}
