package tui

import (
	"encoding/json"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

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
func clarifySettledRows(questions []clarifyQuestion, answers map[string]string) (toolRow, string) {
	rows := make([]string, len(questions))
	for i, q := range questions {
		answer := tr(txt.SessionMainSkipped)
		if trimJS(answers[q.QID]) != "" {
			answer = clarifyAnswerText(answers[q.QID], q.MultiSelect)
		}
		rows[i] = q.Question + " → " + answer
	}
	return namedToolRow("clarify", trCount(txt.PromptClarifyQuestionsOne, txt.PromptClarifyQuestionsOther, len(questions)), toolVerdict{}), strings.Join(rows, "\n")
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
func storedClarifyRows(content string) (trail toolRow, text string, abandoned, found bool) {
	var result storedClarifyResult
	if json.Unmarshal([]byte(content), &result) != nil || len(result.Responses) == 0 {
		return toolRow{}, "", false, false
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

// Rows a shell line a person ran leaves under its prompt row, what it printed and its exit status when it failed or printed nothing; the live
// line and a stored one both end here
func shellOutputRows(r tuigateway.ShellExecResult) []msg {
	var out []msg
	printed := trimJS(strings.Join(slices.DeleteFunc([]string{r.Stdout, r.Stderr}, func(s string) bool { return s == "" }), "\n"))
	if printed != "" {
		out = append(out, msg{Role: "system", Text: printed})
	}
	if r.Code != 0 || printed == "" {
		out = append(out, msg{Role: "system", Text: tr(txt.CommonExitCode, r.Code)})
	}
	return out
}

// Rows a finished turn leaves in the transcript: its final rows, or the failure copy in place of a bare error reply, then the line of what it
// did; the live turn and a stored turn both end here
func finishedTurnRows(done messageCompleteResult, p tuigateway.MessageCompletePayload, summary string) []msg {
	var out []msg
	if !done.WasInterrupted {
		errText := val(p.Error)
		switch {
		case val(p.Status) == "error" && !val(p.Partial) && isBareErrorText(done.FinalText, errText):
			for i, m := range done.FinalMessages {
				if !(i == len(done.FinalMessages)-1 && m.Role == "assistant" && isBareErrorText(m.Text, errText)) {
					out = append(out, m)
				}
			}
			out = append(out, msg{Role: "assistant", Text: describeTurnFailure(p)})
		case len(done.FinalMessages) > 0:
			out = done.FinalMessages
		default:
			out = []msg{{Role: "assistant", Text: done.FinalText}}
		}
	}
	if summary != "" {
		out = append(out, msg{Kind: "event", Role: "system", Text: summary})
	}
	return out
}

// Stored turn played through a turn controller of its own on a clock set to the stored times, so its rows come out of the same code the live
// turn ran: its reasoning, text, tool calls and the line of what it did
type storedTurn struct {
	store *tuiStore
	turn  *turnController
	at    float64
	out   *[]msg
	// Text of the newest reply of the turn, the completion once no tool call follows it
	last string
	// Whether a reply or a tool call came after the prompt, a turn without either left open as the background service still runs it
	fed bool
}

// Turn opened at the time of its prompt, under the transcript settings of the view
func openStoredTurn(ui uiState, at float64, out *[]msg) *storedTurn {
	st := &storedTurn{at: at, out: out}
	st.store = &tuiStore{ui: ui, now: func() time.Time { return time.UnixMilli(int64(math.Round(st.at * 1000))) }}
	st.turn = newTurnController(st.store)
	st.turn.startMessage()
	return st
}

// Moves the turn clock to a stored time, never backward
func (st *storedTurn) tick(at *float64) {
	if ts := val(at); ts > st.at {
		st.at = ts
	}
}

// Feeds a stored reply, its reasoning first as the stream brought it, the background service having left the reasoning out of a session that
// does not show it
func (st *storedTurn) reply(row tuigateway.TranscriptMessage, text string) {
	st.tick(row.Timestamp)
	if reasoning := val(row.Reasoning); strings.TrimFunc(reasoning, isJSSpace) != "" {
		st.turn.recordReasoningDelta(reasoning, true)
	}
	if text != "" {
		st.turn.recordMessageDelta(text)
	}
	st.last, st.fed = text, true
}

// Feeds a stored tool call, started when the reply asking for it was made and finished at the time of its row, a settled clarify leaving its
// questions and answers as the live dialog did
func (st *storedTurn) tool(row tuigateway.TranscriptMessage) {
	name := firstText(val(row.Name), "tool")
	id := firstText(val(row.ToolCallID), name)
	labels := toolLabelsOf(row.Labels)
	st.last, st.fed = "", true
	st.turn.recordToolStart(id, name, val(row.Context), "", labels)
	st.tick(row.Timestamp)
	content, _ := row.Content.(string)
	verdict := verdictOf(row.IsError, row.Refused, row.Error)
	if name == "clarify" && content != "" {
		if trail, text, abandoned, found := storedClarifyRows(content); found {
			role := "user"
			if abandoned {
				role = "system"
			}
			st.append(msg{Kind: "trail", Role: "system", Tools: []toolRow{trail}})
			st.append(msg{Role: role, Text: text})
			st.turn.persistedToolLabels[toolTrailLabel("clarify")] = true
		}
	}
	st.turn.summary.recordTool(name, content, verdict.Failed)
	if diff := strings.TrimFunc(stripAnsi(val(row.InlineDiff)), isJSSpace); diff != "" && st.store.ui.InlineDiffs {
		st.turn.recordInlineDiffToolComplete(diff, id, name, row.DurationS, strippedText(row.ResultText), labels, verdict)
	} else {
		st.turn.recordToolComplete(id, name, "", row.DurationS, nil, strippedText(row.ResultText), labels, verdict)
	}
	st.turn.pushImageSegment(val(row.ImagePath), id)
}

// Appends a row at the turn clock
func (st *storedTurn) append(m msg) {
	*st.out = appendTranscriptMessage(*st.out, m, func() float64 { return st.at })
}

// Ends the turn with its last reply as the completion
func (st *storedTurn) finish() {
	text := st.last
	done := st.turn.recordMessageComplete(tuigateway.MessageCompletePayload{Text: &text})
	for _, m := range finishedTurnRows(done, tuigateway.MessageCompletePayload{}, st.turn.summaryLine()) {
		st.append(m)
	}
}

// Transcript rows of a stored session under the transcript settings of the view, each turn rebuilt by the code the live turn ran and display only
// events projected
func toTranscriptMessages(rows []tuigateway.TranscriptMessage, ui uiState) []msg {
	var out []msg
	var turn *storedTurn
	settle := func() {
		if turn != nil && turn.fed {
			turn.finish()
		}
		turn = nil
	}
	for _, row := range rows {
		var createdAt *float64
		if ts := val(row.Timestamp); !math.IsInf(ts, 0) && !math.IsNaN(ts) && ts > 0 {
			createdAt = &ts
		}
		if row.Role == "tool" {
			if turn == nil {
				turn = openStoredTurn(ui, val(createdAt), &out)
			}
			turn.tool(row)
			continue
		}
		text := val(row.Text)
		reasoning := strings.TrimFunc(val(row.Reasoning), isJSSpace)
		if strings.TrimFunc(text, isJSSpace) == "" && (row.Role != "assistant" || reasoning == "") {
			if row.Role == "assistant" && turn != nil {
				turn.tick(row.Timestamp)
			}
			continue
		}
		event := func(text string) {
			settle()
			out = append(out, msg{Kind: "event", Role: "system", Text: text, CreatedAt: createdAt})
		}
		switch val(row.DisplayKind) {
		case "hidden":
			continue
		case "shell":
			settle()
			out = append(out, msg{Role: "user", Text: "!" + text, CreatedAt: createdAt})
			if row.Shell != nil {
				for _, m := range shellOutputRows(*row.Shell) {
					m.CreatedAt = createdAt
					out = append(out, m)
				}
			}
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
			settle()
			out = append(out, delegationResultMsg(text, row.DisplayMetadata, createdAt))
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
			if turn == nil {
				turn = openStoredTurn(ui, val(createdAt), &out)
			}
			turn.reply(row, text)
		case "user":
			settle()
			out = append(out, msg{Role: row.Role, Text: text, CreatedAt: createdAt})
			turn = openStoredTurn(ui, val(createdAt), &out)
		case "system":
			settle()
			out = append(out, msg{Role: row.Role, Text: text, CreatedAt: createdAt})
		}
	}
	settle()
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
