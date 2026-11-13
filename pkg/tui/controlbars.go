package tui

import (
	"math"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
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
		return jsString(n) + " " + noun
	}
	return jsString(n) + " " + noun + "s"
}

func listLen(v any) int {
	list, _ := v.([]any)
	return len(list)
}

// Goal bar line of an active or paused goal, parked with its barrier, nil once done or cleared
func goalLineOf(goal map[string]any) *goalLine {
	if goal == nil || (goal["status"] != "active" && goal["status"] != "paused") {
		return nil
	}
	var parts []string
	for _, p := range []string{
		jsTemplate(goal, "turns_used") + "/" + jsTemplate(goal, "max_turns") + " turns",
		countNoun(listLen(goal["subgoals"]), "subgoal"),
		countNoun(listLen(goal["gates"]), "gate"),
	} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	turns := strings.Join(parts, " · ")
	title := jsTemplate(goal, "title")
	if barrier, _ := goal["wait_barrier"].(map[string]any); goal["status"] == "active" && barrier != nil {
		until := "on " + jsTemplate(barrier, "type") + " " + jsTemplate(barrier, "target")
		if barrier["type"] == "until" {
			until = "until " + clockText(numberField(barrier, "until_at"))
		}
		reason := ""
		if truthyJS(barrier["reason"]) {
			reason = " · " + jsTemplate(barrier, "reason")
		}
		return &goalLine{Detail: until + reason + " · " + turns, Glyph: "⏳", Label: "goal parked", Title: title}
	}
	if goal["status"] == "paused" {
		reason := ""
		if truthyJS(goal["paused_reason"]) {
			reason = jsTemplate(goal, "paused_reason") + " · "
		}
		return &goalLine{Detail: reason + turns, Glyph: "⏸", Label: "goal paused", Title: title}
	}
	return &goalLine{Detail: turns, Glyph: "⊙", Label: "goal", Title: title}
}

// JavaScript ToNumber of a JSON value, null as zero and an absent value as NaN
func jsToNumber(v any, present bool) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case bool:
		if t {
			return 1
		}
		return 0
	case string:
		switch strings.TrimFunc(t, isJSSpace) {
		case "Infinity", "+Infinity":
			return math.Inf(1)
		case "-Infinity":
			return math.Inf(-1)
		}
		if n, ok := jsNumber(t); ok {
			return n
		}
		return math.NaN()
	case nil:
		if present {
			return 0
		}
	}
	return math.NaN()
}

func numberField(m map[string]any, k string) float64 {
	v, present := m[k]
	return jsToNumber(v, present)
}

// Time left rounded up, whole minutes with hours until the last minute, then seconds
func countdown(seconds float64) string {
	left := math.Max(0, math.Ceil(seconds))
	if left < 60 {
		return jsString(left) + "s"
	}
	minutes := math.Ceil(left / 60)
	hours := math.Floor(minutes / 60)
	if hours != 0 && !math.IsNaN(hours) {
		rem := jsString(math.Mod(minutes, 60))
		if len(rem) < 2 {
			rem = strings.Repeat("0", 2-len(rem)) + rem
		}
		return jsString(hours) + "h" + rem + "m"
	}
	return jsString(minutes) + "m"
}

// Cadence of a loop, self-paced or every interval in hours, minutes or seconds
func cadence(loop map[string]any) string {
	if loop["mode"] == "self_paced" {
		return "self-paced"
	}
	s := numberField(loop, "interval_seconds")
	var every string
	switch {
	case math.Mod(s, 3600) == 0:
		every = jsString(s/3600) + "h"
	case math.Mod(s, 60) == 0:
		every = jsString(s/60) + "m"
	default:
		every = jsTemplate(loop, "interval_seconds") + "s"
	}
	return "every " + every
}

// Loop bar line of an active or paused loop at a moment, nil once done or cleared
func loopLineOf(loop map[string]any, nowMs float64) *loopLine {
	if loop == nil || (loop["status"] != "active" && loop["status"] != "paused") {
		return nil
	}
	runs := jsTemplate(loop, "ticks_fired") + " run"
	if truthyJS(loop["times"]) {
		runs = jsTemplate(loop, "ticks_fired") + "/" + jsTemplate(loop, "times") + " runs"
	} else if !jsNumberEquals(loop["ticks_fired"], 1) {
		runs += "s"
	}
	prompt := jsTemplate(loop, "prompt")
	if loop["status"] == "paused" {
		reason := ""
		if truthyJS(loop["paused_reason"]) {
			reason = jsTemplate(loop, "paused_reason") + " · "
		}
		return &loopLine{Detail: reason + runs, Glyph: "⏸", Label: "loop paused", Prompt: prompt}
	}
	remaining := numberField(loop, "next_due_at") - nowMs/1000
	var when string
	switch {
	case truthyJS(loop["awaiting_response"]):
		when = "running now"
	case truthyJS(loop["deferred_by_goal"]):
		when = "deferred to the goal"
	case remaining > 0:
		when = "next in " + countdown(remaining)
	default:
		when = "due, waiting for idle"
	}
	return &loopLine{Detail: cadence(loop) + " · " + runs + " · " + when, Glyph: "↻", Label: "loop", Prompt: prompt}
}

// Goal line of the current session, nil when the snapshot belongs to another session
func (s *tuiStore) currentGoalLine() *goalLine {
	if s.goalSnapshot.Sid != s.ui.Sid {
		return nil
	}
	return goalLineOf(s.goalSnapshot.Value)
}

func (s *tuiStore) currentLoop() map[string]any {
	if s.loopSnapshot.Sid != s.ui.Sid {
		return nil
	}
	return s.loopSnapshot.Value
}

// Loop line of the current session at this moment
func (s *tuiStore) currentLoopLine() *loopLine {
	return loopLineOf(s.currentLoop(), float64(time.Now().UnixMilli()))
}

// Whether the current loop shows a countdown that repaints every second
func (s *tuiStore) loopTicking() bool {
	loop := s.currentLoop()
	return loop != nil && loop["status"] == "active" && !truthyJS(loop["awaiting_response"])
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
