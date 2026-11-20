package tui

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/txt"
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
		prefix = tr(txt.LibTextMessagesMessageFallback)
	}
	return tr(txt.LibTextMessagesLongMessage, prefix)
}

// Folded block of an asynchronous delegation batch result, headed by its display text or the finished agent count
func delegationResultMsg(text string, meta *tuigateway.DisplayMetadata, createdAt *float64) msg {
	if meta == nil {
		meta = &tuigateway.DisplayMetadata{}
	}
	label := tr(txt.LibTextMessagesBackgroundAgentWorkFinished)
	switch count := meta.TaskCount; {
	case count != nil && *count == 1:
		label = tr(txt.LibTextMessagesBackgroundAgentsFinishedOne, *count)
	case count != nil:
		label = tr(txt.LibTextMessagesBackgroundAgentsFinishedOther, *count)
	}
	if display := val(meta.DisplayText); strings.TrimSpace(display) != "" {
		label = display
	}
	return msg{Kind: "delegation", Role: "system", Label: label, Text: text, CreatedAt: createdAt}
}

// Trail row and the question → answer lines a settled clarify leaves in the transcript, a blank answer read as skipped
func clarifySettledRows(questions []clarifyQuestion, answers map[string]string) (string, string) {
	rows := make([]string, len(questions))
	for i, q := range questions {
		answer := tr(txt.SessionMainSkipped)
		if trimJS(answers[q.QID]) != "" {
			answer = clarifyAnswerText(answers[q.QID], q.MultiSelect)
		}
		rows[i] = q.Question + " → " + answer
	}
	return buildToolTrailLine("clarify", trCount(txt.PromptClarifyQuestionsOne, txt.PromptClarifyQuestionsOther, len(questions)), toolVerdict{}, "", nil), strings.Join(rows, "\n")
}

// Stored result of a clarify tool call, as much of it as the transcript reads
type storedClarifyResult struct {
	Notice    *string `json:"notice"`
	Outcome   *string `json:"outcome"`
	Responses []struct {
		Question     string `json:"question"`
		UserResponse any    `json:"user_response"`
	} `json:"responses"`
}

// Trail row and text of a stored clarify result as the live turn showed it, the answers of a submitted form or the abandoned block of one that ended otherwise, found false when the result holds no responses
func storedClarifyRows(content string) (trail, text string, abandoned, found bool) {
	var result storedClarifyResult
	if json.Unmarshal([]byte(content), &result) != nil || len(result.Responses) == 0 {
		return "", "", false, false
	}
	questions := make([]clarifyQuestion, len(result.Responses))
	answers := map[string]string{}
	for i, row := range result.Responses {
		qid := "q" + strconv.Itoa(i)
		questions[i] = clarifyQuestion{QID: qid, Question: row.Question}
		switch answer := row.UserResponse.(type) {
		case string:
			answers[qid] = answer
		case []any:
			picked := make([]string, len(answer))
			for j, item := range answer {
				picked[j], _ = item.(string)
			}
			answers[qid] = strings.Join(picked, ", ")
		}
	}
	trail, text = clarifySettledRows(questions, answers)
	outcome := "submitted"
	if result.Outcome != nil {
		outcome = *result.Outcome
	}
	reason := outcome
	switch {
	case outcome == "submitted":
		return trail, text, false, true
	case outcome == "cancelled":
		reason = tr(txt.GatewayMsgClarifyCancelled)
	case outcome == "timed_out":
		reason = tr(txt.GatewayMsgClarifyTimedOut)
	case result.Notice != nil:
		reason = *result.Notice
	}
	return trail, formatAbandonedClarify(questions, answers, reason), true, true
}

// Transcript rows of a stored session, tool rows folded into the next reply, a clarify result shown as its settled rows and display only events projected
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
			if content, _ := row.Content.(string); name == "clarify" && content != "" {
				if trail, text, abandoned, found := storedClarifyRows(content); found {
					role := "user"
					if abandoned {
						role = "system"
					}
					out = append(out, msg{Kind: "trail", Role: "system", Tools: append(pending, trail)}, msg{Role: role, Text: text})
					pending = nil
					continue
				}
			}
			pending = append(pending, buildToolTrailLine(name, val(row.Context), verdictOf(row.IsError, row.Refused, row.Error), "", nil))
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
			event(tr(txt.LibTextMessagesModelChanged))
			continue
		case "auto_continue":
			event(tr(txt.LibTextMessagesResumedInterruptedTurn))
			continue
		case "personality_switch":
			event(tr(txt.LibTextMessagesPersonalityChanged))
			continue
		case "async_delegation_complete":
			out = append(out, delegationResultMsg(text, row.DisplayMetadata, createdAt))
			pending = nil
			continue
		case "process_complete":
			label := tr(txt.LibTextMessagesBackgroundProcessFinished)
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
	case "image":
		return "trail"
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

// Whether a block draws anything, a trail only with content and a visible section, a mixture of agents reference always
func blockRenders(m msg, ctx detailsCtx) bool {
	if m.Kind != "trail" || len(m.Todos) > 0 || (m.IsMoaReference && strings.TrimFunc(m.Thinking, isJSSpace) != "") {
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
