package tui

import (
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"math"
	"strconv"
	"strings"
)

const (
	longMsg    = 300
	maxHistory = 800
)

// Intro row carrying the session info the banner renders
func introMsg(info *tuigateway.SessionInfo) msg {
	return msg{Info: info, Kind: "intro", Role: "system"}
}

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

// Folded block of an asynchronous delegation batch result, headed by its display text or the finished agent count
func delegationResultMsg(text string, meta *tuigateway.DisplayMetadata, createdAt *float64) msg {
	if meta == nil {
		meta = &tuigateway.DisplayMetadata{}
	}
	label := tr("libText.messages.backgroundAgentWorkFinished")
	switch count := meta.TaskCount; {
	case count != nil && *count == 1:
		label = tr("libText.messages.backgroundAgentsFinishedOne", *count)
	case count != nil:
		label = tr("libText.messages.backgroundAgentsFinishedOther", *count)
	}
	if display := val(meta.DisplayText); strings.TrimSpace(display) != "" {
		label = display
	}
	return msg{Kind: "delegation", Role: "system", Label: label, Text: text, CreatedAt: createdAt}
}

// Transcript rows of a stored session, tool rows folded into the next reply and display only events projected
func toTranscriptMessages(rows []tuigateway.TranscriptMessage) []msg {
	var out []msg
	var pending []string
	for _, row := range rows {
		var createdAt *float64
		if ts := val(row.Timestamp); !math.IsInf(ts, 0) && !math.IsNaN(ts) && ts > 0 {
			createdAt = &ts
		}
		if row.Role == "tool" {
			name := "tool"
			if row.Name != nil {
				name = *row.Name
			}
			pending = append(pending, buildToolTrailLine(name, val(row.Context), false, "", nil))
			continue
		}
		text := val(row.Text)
		if row.Text == nil || strings.TrimFunc(text, isJSSpace) == "" {
			continue
		}
		event := func(text string) {
			out = append(out, msg{Kind: "event", Role: "system", Text: text})
			pending = nil
		}
		switch val(row.DisplayKind) {
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
		case "async_delegation_complete":
			out = append(out, delegationResultMsg(text, row.DisplayMetadata, createdAt))
			pending = nil
			continue
		case "process_complete":
			label := tr("libText.messages.backgroundProcessFinished")
			if row.DisplayMetadata != nil && row.DisplayMetadata.DisplayText != nil {
				label = *row.DisplayMetadata.DisplayText
			}
			event(label)
			continue
		}
		switch row.Role {
		case "assistant":
			out = append(out, msg{Role: row.Role, Text: text, CreatedAt: createdAt, Tools: pending})
			pending = nil
		case "user", "system":
			out = append(out, msg{Role: row.Role, Text: text, CreatedAt: createdAt})
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
		return strconv.FormatInt(h, 10) + "h " + strconv.FormatInt(m, 10) + "m"
	case m > 0:
		return strconv.FormatInt(m, 10) + "m " + strconv.FormatInt(s, 10) + "s"
	}
	return strconv.FormatInt(s, 10) + "s"
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
