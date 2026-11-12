package tui

import (
	"math"
	"strings"
)

const (
	longMsg    = 300
	maxHistory = 800
)

// Intro row carrying the session info the banner renders
func introMsg(info map[string]any) msg { return msg{Info: info, Kind: "intro", Role: "system"} }

// User text as the transcript shows it, a long paste reduced to its first words
func userDisplay(text string) string {
	if unitLen(text) <= longMsg {
		return text
	}
	first, _, _ := strings.Cut(text, "\n")
	first = strings.TrimFunc(first, isJSSpace)
	words := strings.FieldsFunc(first, isJSSpace)
	prefix := first
	if len(words) > 1 {
		prefix = strings.Join(words[:min(4, len(words))], " ")
	}
	prefix = jsSlice(prefix, 0, 80)
	if prefix == "" {
		prefix = tr("libText.messages.messageFallback")
	}
	return tr("libText.messages.longMessage", prefix)
}

// Transcript rows of a stored session, tool rows folded into the next reply and display only events projected
func toTranscriptMessages(rows any) []msg {
	list, isList := rows.([]any)
	if !isList {
		return nil
	}
	var out []msg
	var pending []string
	for _, item := range list {
		row, isDict := item.(map[string]any)
		if !isDict {
			continue
		}
		role, _ := row["role"].(string)
		var createdAt *float64
		if ts, isNum := row["timestamp"].(float64); isNum && !math.IsInf(ts, 0) && !math.IsNaN(ts) && ts > 0 {
			createdAt = &ts
		}
		if role == "tool" {
			name, isStr := row["name"].(string)
			if !isStr {
				name = "tool"
			}
			context, _ := row["context"].(string)
			pending = append(pending, buildToolTrailLine(name, context, false, "", nil))
			continue
		}
		text, isStr := row["text"].(string)
		if !isStr || strings.TrimFunc(text, isJSSpace) == "" {
			continue
		}
		event := func(text string) {
			out = append(out, msg{Kind: "event", Role: "system", Text: text})
			pending = nil
		}
		switch kind, _ := row["display_kind"].(string); kind {
		case "hidden":
			continue
		case "model_switch":
			event(tr("libText.messages.modelChanged"))
			continue
		case "auto_continue":
			event(tr("libText.messages.resumedInterruptedTurn"))
			continue
		case "personality_switch":
			event(tr("libText.messages.personalityChanged"))
			continue
		case "async_delegation_complete", "process_complete":
			meta, _ := row["display_metadata"].(map[string]any)
			label := tr("libText.messages.backgroundProcessFinished")
			if kind != "process_complete" {
				count, hasCount := meta["task_count"].(float64)
				switch {
				case !hasCount:
					label = tr("libText.messages.backgroundAgentWorkFinished")
				case count == 1:
					label = tr("libText.messages.backgroundAgentsFinishedOne", count)
				default:
					label = tr("libText.messages.backgroundAgentsFinishedOther", count)
				}
			}
			if display, isStr := meta["display_text"].(string); isStr {
				label = display
			}
			event(label)
			continue
		}
		switch role {
		case "assistant":
			out = append(out, msg{Role: role, Text: text, CreatedAt: createdAt, Tools: pending})
			pending = nil
		case "user", "system":
			out = append(out, msg{Role: role, Text: text, CreatedAt: createdAt})
			pending = nil
		}
	}
	return out
}

// Duration of milliseconds as hours and minutes, minutes and seconds, or seconds
func fmtDuration(ms float64) string {
	t := int64(math.Max(0, math.Floor(ms/1000)))
	h, m, s := t/3600, (t%3600)/60, t%60
	switch {
	case h > 0:
		return jsString(float64(h)) + "h " + jsString(float64(m)) + "m"
	case m > 0:
		return jsString(float64(m)) + "m " + jsString(float64(s)) + "s"
	}
	return jsString(float64(s)) + "s"
}

// Rows with a message appended, stamped now when it carries no time, tool shelves merged
func appendTranscriptMessage(prev []msg, m msg, now func() float64) []msg {
	if m.CreatedAt == nil {
		at := now()
		m.CreatedAt = &at
	}
	return appendToolShelfMessage(prev, m)
}

// Newest rows within the history cap, the intro row kept first
func capTranscriptHistory(items []msg) []msg {
	if len(items) <= maxHistory {
		return items
	}
	if items[0].Kind == "intro" {
		return append([]msg{items[0]}, items[len(items)-(maxHistory-1):]...)
	}
	return items[len(items)-maxHistory:]
}

// Rows with the last one replaced when it has the role, appended otherwise
func upsert(prev []msg, role, text string) []msg {
	if n := len(prev); n > 0 && prev[n-1].Role == role {
		return append(append([]msg(nil), prev[:n-1]...), msg{Role: role, Text: text})
	}
	return append(append([]msg(nil), prev...), msg{Role: role, Text: text})
}

// Index of the first offset past the target
func upperBound(offsets []int, target int) int {
	lo, hi := 0, len(offsets)
	for lo < hi {
		mid := (lo + hi) >> 1
		if offsets[mid] <= target {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

// Prompt pinned above the viewport, the latest user message scrolled fully out of view
func stickyPromptFromViewport(messages []msg, offsets []int, top, bottom int, sticky bool) string {
	if sticky || len(messages) == 0 {
		return ""
	}
	first := max(0, upperBound(offsets, top)-1)
	last := max(first, upperBound(offsets, bottom)-1)
	visibleStart := min(len(messages), first)
	visibleEnd := min(len(messages)-1, last)
	for i := visibleStart; i <= visibleEnd; i++ {
		if messages[i].Role == "user" {
			return ""
		}
	}
	offsetAt := func(i int) (int, bool) {
		if i >= 0 && i < len(offsets) {
			return offsets[i], true
		}
		return 0, false
	}
	for i := min(len(messages)-1, visibleStart-1); i >= 0; i-- {
		if messages[i].Role != "user" {
			continue
		}
		end, has := offsetAt(i + 1)
		if !has {
			start, _ := offsetAt(i)
			end = start + 1
		}
		if end <= top {
			return collapseWS(userDisplay(strings.TrimFunc(messages[i].Text, isJSSpace)))
		}
		return ""
	}
	return ""
}

// Visual band of a transcript block
func messageGroup(m msg) string {
	switch m.Kind {
	case "intro", "panel":
		return "intro"
	case "slash", "event", "diff", "trail":
		return m.Kind
	}
	switch m.Role {
	case "user":
		return "user"
	case "assistant":
		return "model"
	}
	return "note"
}

// Whether a block opens with a blank row, only at a boundary between working area groups
func hasLeadGap(prev *msg, cur msg) bool {
	group := messageGroup(cur)
	switch group {
	case "diff", "event", "intro", "slash", "user":
		return false
	}
	if prev == nil {
		return false
	}
	switch prevGroup := messageGroup(*prev); prevGroup {
	case "diff", "event", "user":
		return false
	default:
		return prevGroup != group
	}
}

// Details visibility a transcript renders with
type detailsCtx struct {
	CommandOverride bool
	DetailsMode     string
	Sections        map[string]string
}

// Whether a block draws anything, a trail only with content and a visible section
func blockRenders(m msg, ctx detailsCtx) bool {
	if m.Kind != "trail" || len(m.Todos) > 0 {
		return true
	}
	if len(m.Tools) == 0 && strings.TrimFunc(m.Thinking, isJSSpace) == "" {
		return false
	}
	for _, section := range []string{"thinking", "tools", "activity"} {
		if sectionMode(section, ctx.DetailsMode, ctx.Sections, ctx.CommandOverride) != "hidden" {
			return true
		}
	}
	return false
}

// Nearest earlier block that draws, nil at the top of the transcript
func prevRenderedMsg(msgAt func(int) *msg, index int, ctx detailsCtx) *msg {
	for i := index - 1; i >= 0; i-- {
		if candidate := msgAt(i); candidate != nil && blockRenders(*candidate, ctx) {
			return candidate
		}
	}
	return nil
}
