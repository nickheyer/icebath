package tui

import (
	"math"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// One goal bar row, its glyph, label, detail and title
type goalLine struct {
	Detail, Glyph, Label, Title string
}

// One loop bar row, its glyph, label, detail and prompt
type loopLine struct {
	Detail, Glyph, Label, Prompt string
}

// Repaint tick of the loop countdown for one snapshot generation
type loopTickMsg struct{ gen int }

func countNoun(n int, noun string) string {
	if n == 0 {
		return ""
	}
	if n == 1 {
		return strconv.Itoa(n) + " " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

// Goal bar line of an active or paused goal, parked with its barrier, nil once done or cleared
func goalLineOf(goal *tuigateway.GoalSnapshot) *goalLine {
	if goal == nil || (goal.Status != "active" && goal.Status != "paused") {
		return nil
	}
	var parts []string
	for _, p := range []string{
		strconv.FormatInt(goal.TurnsUsed, 10) + "/" + strconv.FormatInt(goal.MaxTurns, 10) + " turns",
		countNoun(len(goal.Subgoals), "subgoal"),
		countNoun(len(goal.Gates), "gate"),
	} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	turns := strings.Join(parts, " · ")
	if barrier := goal.WaitBarrier; goal.Status == "active" && barrier != nil {
		var until string
		switch barrier.Type {
		case "until":
			until = "until " + clockText(val(barrier.UntilAt))
		case "pid":
			until = "on pid " + strconv.FormatInt(val(barrier.PID), 10)
		default:
			until = "on session " + val(barrier.SessionID)
		}
		reason := ""
		if barrier.Reason != "" {
			reason = " · " + barrier.Reason
		}
		return &goalLine{Detail: until + reason + " · " + turns, Glyph: "⏳", Label: "goal parked", Title: goal.Title}
	}
	if goal.Status == "paused" {
		reason := ""
		if paused := val(goal.PausedReason); paused != "" {
			reason = paused + " · "
		}
		return &goalLine{Detail: reason + turns, Glyph: "⏸", Label: "goal paused", Title: goal.Title}
	}
	return &goalLine{Detail: turns, Glyph: "⊙", Label: "goal", Title: goal.Title}
}

// Shortest decimal text of a duration count
func countText(n float64) string { return strconv.FormatFloat(n, 'f', -1, 64) }

// Time left rounded up, whole minutes with hours until the last minute, then seconds
func countdown(seconds float64) string {
	left := math.Max(0, math.Ceil(seconds))
	if left < 60 {
		return countText(left) + "s"
	}
	minutes := math.Ceil(left / 60)
	hours := math.Floor(minutes / 60)
	if hours != 0 && !math.IsNaN(hours) {
		rem := countText(math.Mod(minutes, 60))
		if len(rem) < 2 {
			rem = strings.Repeat("0", 2-len(rem)) + rem
		}
		return countText(hours) + "h" + rem + "m"
	}
	return countText(minutes) + "m"
}

// Cadence of a loop, self-paced or every interval in hours, minutes or seconds
func cadence(loop *tuigateway.LoopSnapshot) string {
	if loop.Mode == "self_paced" {
		return "self-paced"
	}
	s := loop.IntervalSeconds
	var every string
	switch {
	case math.Mod(s, 3600) == 0:
		every = countText(s/3600) + "h"
	case math.Mod(s, 60) == 0:
		every = countText(s/60) + "m"
	default:
		every = countText(s) + "s"
	}
	return "every " + every
}

// Loop bar line of an active or paused loop at a moment, nil once done or cleared
func loopLineOf(loop *tuigateway.LoopSnapshot, nowMs float64) *loopLine {
	if loop == nil || (loop.Status != "active" && loop.Status != "paused") {
		return nil
	}
	fired := strconv.FormatInt(loop.TicksFired, 10)
	runs := fired + " run"
	if loop.Times != 0 {
		runs = fired + "/" + strconv.FormatInt(loop.Times, 10) + " runs"
	} else if loop.TicksFired != 1 {
		runs += "s"
	}
	if loop.Status == "paused" {
		reason := ""
		if paused := val(loop.PausedReason); paused != "" {
			reason = paused + " · "
		}
		return &loopLine{Detail: reason + runs, Glyph: "⏸", Label: "loop paused", Prompt: loop.Prompt}
	}
	remaining := loop.NextDueAt - nowMs/1000
	var when string
	switch {
	case loop.AwaitingResponse:
		when = "running now"
	case loop.DeferredByGoal:
		when = "deferred to the goal"
	case remaining > 0:
		when = "next in " + countdown(remaining)
	default:
		when = "due, waiting for idle"
	}
	return &loopLine{Detail: cadence(loop) + " · " + runs + " · " + when, Glyph: "↻", Label: "loop", Prompt: loop.Prompt}
}

// Goal line of the current session, nil when the snapshot belongs to another session
func (s *tuiStore) currentGoalLine() *goalLine {
	if s.controls.Sid != s.ui.Sid {
		return nil
	}
	return goalLineOf(s.controls.Goal)
}

func (s *tuiStore) currentLoop() *tuigateway.LoopSnapshot {
	if s.controls.Sid != s.ui.Sid {
		return nil
	}
	return s.controls.Loop
}

// Loop line of the current session at this moment
func (s *tuiStore) currentLoopLine() *loopLine {
	return loopLineOf(s.currentLoop(), float64(time.Now().UnixMilli()))
}

// Whether the current loop shows a countdown that repaints every second
func (s *tuiStore) loopTicking() bool {
	loop := s.currentLoop()
	return loop != nil && loop.Status == "active" && !loop.AwaitingResponse
}

func (s *tuiStore) loopTick() tea.Cmd {
	gen := s.loopTickGen
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return loopTickMsg{gen: gen} })
}

// Restarts the countdown ticker after the loop snapshot or the session changed
func (s *tuiStore) restartLoopTicker() tea.Cmd {
	s.loopTickGen++
	if !s.loopTicking() {
		return nil
	}
	return s.loopTick()
}

// Next countdown tick while the same snapshot keeps ticking
func (s *tuiStore) onLoopTick(m loopTickMsg) tea.Cmd {
	if m.gen != s.loopTickGen || !s.loopTicking() {
		return nil
	}
	return s.loopTick()
}

// Text cut to a width with an ellipsis at the end of the line
func truncateEnd(text string, columns int) string {
	switch {
	case columns < 1:
		return ""
	case columns == 1:
		return "…"
	case ansi.StringWidth(text) <= columns:
		return text
	}
	return ansi.Truncate(text, columns-1, "") + "…"
}

// Bar row of a glyph, label and detail with the title or prompt filling the columns left
func controlBarView(cols, width int, glyph, label, detail, tail, standing string, t theme) string {
	head := glyph + " " + label + " · " + detail + " · "
	tone := t.Color.Warn
	if glyph == standing {
		tone = t.Color.Accent
	}
	row := paint(glyph+" "+label, tone, true) +
		paint(" · "+detail+" · ", t.Color.Muted, true) +
		paint(compactPreview(tail, max(8, cols-ansi.StringWidth(head))), t.Color.Text, true)
	return truncateEnd(row, width)
}

// Goal bar of the current session, empty while no goal stands
func goalBarView(cols, width int, line *goalLine, t theme) string {
	if line == nil {
		return ""
	}
	return controlBarView(cols, width, line.Glyph, line.Label, line.Detail, line.Title, "⊙", t)
}

// Loop bar of the current session, empty while no loop stands
func loopBarView(cols, width int, line *loopLine, t theme) string {
	if line == nil {
		return ""
	}
	return controlBarView(cols, width, line.Glyph, line.Label, line.Detail, line.Prompt, "↻", t)
}
