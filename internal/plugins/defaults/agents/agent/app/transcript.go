package app

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/charmbracelet/x/ansi"

	agentconfig "github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/config"
	toolsapp "github.com/nickheyer/icebath/internal/tools/app"
	agentv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/ui/render"
)

const preview = 12

type entry struct {
	message *sessionsv1.Message
	notice  string
}

type aside struct {
	after int
	text  string
}

type transcript struct {
	entries   []entry
	asides    []aside
	text      strings.Builder
	reasoning strings.Builder
	// Approval prompts waiting, oldest first
	approvals []*agentv1.Approval
	questions *agentv1.Questions
	state     *agentv1.State
	busy      bool
	// How far each running tool call got, by call id, until its result comes
	progress map[string]*agentv1.Progress
	// What the session does besides the reply, such as compacting its context; empty when nothing
	activity string
}

type row struct {
	text string
	fold string
}

func (t *transcript) apply(response *agentv1.WatchResponse) (taken string) {
	switch event := response.GetEvent().(type) {
	case *agentv1.WatchResponse_Message:
		t.settle()
		t.entries = append(t.entries, entry{message: event.Message})
		delete(t.progress, event.Message.GetToolCallId())
	case *agentv1.WatchResponse_Progress:
		if t.progress == nil {
			t.progress = map[string]*agentv1.Progress{}
		}
		t.progress[event.Progress.GetToolCallId()] = event.Progress
	case *agentv1.WatchResponse_Delta:
		if event.Delta.GetRestart() {
			t.text.Reset()
			t.reasoning.Reset()
		}
		t.text.WriteString(event.Delta.GetText())
		t.reasoning.WriteString(event.Delta.GetReasoning())
	case *agentv1.WatchResponse_Questions:
		if t.questions = event.Questions; event.Questions.GetId() == "" {
			t.questions = nil
		}
	case *agentv1.WatchResponse_Approvals:
		t.approvals = event.Approvals.GetPending()
	case *agentv1.WatchResponse_State:
		t.state = event.State
	case *agentv1.WatchResponse_Notice:
		t.entries = append(t.entries, entry{notice: event.Notice})
	case *agentv1.WatchResponse_Busy:
		if t.busy = event.Busy; !t.busy {
			t.settle()
			t.progress, t.activity = nil, ""
		}
	case *agentv1.WatchResponse_Activity:
		t.activity = event.Activity
	case *agentv1.WatchResponse_Rewind:
		kept := int(event.Rewind.GetMessages())
		moved := make([]int, len(t.entries)+1)
		entries := t.entries[:0:0]
		for i, e := range t.entries {
			switch {
			case e.message == nil:
				entries = append(entries, e)
			case kept > 0:
				kept--
				entries = append(entries, e)
			case taken == "" && e.message.GetRole() == sessionsv1.Role_ROLE_USER:
				taken = e.message.GetText()
			}
			moved[i+1] = len(entries)
		}
		t.entries = entries
		for i := range t.asides {
			t.asides[i].after = moved[t.asides[i].after]
		}
	}
	return taken
}

func (t *transcript) replay() {
	t.entries, t.approvals, t.questions, t.busy, t.progress, t.activity = nil, nil, nil, false, nil, ""
	t.text.Reset()
	t.reasoning.Reset()
}

func (t *transcript) note(text string) {
	t.asides = append(t.asides, aside{after: len(t.entries), text: text})
}

func (t *transcript) settle() {
	t.text.Reset()
	t.reasoning.Reset()
}

func (t *transcript) replies() []string {
	var replies []string
	for _, e := range t.entries {
		if e.message.GetRole() == sessionsv1.Role_ROLE_ASSISTANT && e.message.GetText() != "" {
			replies = append(replies, e.message.GetText())
		}
	}
	return replies
}

func (c *chat) paint(w int) []row {
	look := c.kit.Env.Theme
	t := &c.log
	var rows []row
	gap := func() {
		if len(rows) > 0 {
			rows = append(rows, row{})
		}
	}
	prose := func(text string) {
		gap()
		for _, line := range c.markup(text, w) {
			rows = append(rows, row{text: line})
		}
	}
	mark := func(open bool) string {
		if open {
			return look.Glyphs.Down
		}
		return look.Glyphs.Bullet
	}
	block := func(lines []string, bad bool) {
		for _, line := range lines {
			line = strings.ReplaceAll(ansi.Strip(line), "\t", "    ")
			for _, part := range strings.Split(ansi.Hardwrap(line, max(w-2, 1), true), "\n") {
				if bad {
					part = render.Bad(part, look)
				}
				rows = append(rows, row{text: render.Dim(look.Glyphs.Bar, look) + " " + part})
			}
		}
	}
	thinking := func(id, text string) {
		if c.thinking == agentconfig.ThinkingHidden {
			return
		}
		open := c.open(id, c.thinking == agentconfig.ThinkingOpen)
		gap()
		rows = append(rows, row{text: render.Dim(mark(open)+" Reasoning", look), fold: id})
		if open {
			for _, line := range c.markup(text, w) {
				rows = append(rows, row{text: line})
			}
		}
	}
	answered := map[int]bool{}
	answer := func(from int, call *sessionsv1.ToolCall) *sessionsv1.Message {
		for i := from + 1; i < len(t.entries); i++ {
			if message := t.entries[i].message; message.GetRole() == sessionsv1.Role_ROLE_TOOL && message.GetToolCallId() == call.GetId() && !answered[i] {
				answered[i] = true
				return message
			}
		}
		return nil
	}
	// Reply of each entry, counted by the user messages up to it, and the newest call in each reply of a tool whose view shows that one
	// alone, among the calls that succeeded
	replies := make([]int, len(t.entries))
	latest := map[string]string{}
	for i, e := range t.entries {
		if i > 0 {
			replies[i] = replies[i-1]
		}
		if e.message.GetRole() == sessionsv1.Role_ROLE_USER {
			replies[i]++
		}
		for k, tool := range e.message.GetToolCalls() {
			if view, ok := c.view(tool.GetName()); ok && view.Latest && succeeded(t.entries[i+1:], tool) {
				latest[fmt.Sprint(replies[i], ":", tool.GetName())] = fmt.Sprintf("%d:%d", i, k)
			}
		}
	}
	presented := func(id string, shown *toolsapp.Shown) {
		if len(shown.Rows) == 0 {
			rows = append(rows, row{text: ansi.Truncate(shown.Head, w, look.Glyphs.Ellipsis)})
			return
		}
		open := c.open(id, shown.Open)
		head := shown.Head
		if !open && shown.Folded != "" {
			head = shown.Folded
		}
		rows = append(rows, row{text: ansi.Truncate(render.Dim(mark(open), look)+" "+head, w, look.Glyphs.Ellipsis), fold: id})
		if open {
			for _, line := range shown.Rows {
				rows = append(rows, row{text: ansi.Truncate("  "+line, w, look.Glyphs.Ellipsis)})
			}
		}
	}
	result := func(id string, message *sessionsv1.Message) {
		lines := strings.Split(strings.TrimRight(outcome(message), "\n"), "\n")
		if len(lines) <= preview {
			block(lines, message.GetFailed())
			return
		}
		if c.open(id, c.expanded) {
			block(lines, message.GetFailed())
			rows = append(rows, row{text: render.Dim(mark(true)+" Show less", look), fold: id})
			return
		}
		block(lines[:preview], message.GetFailed())
		rows = append(rows, row{text: render.Dim(fmt.Sprintf("%s %d more lines", mark(false), len(lines)-preview), look), fold: id})
	}
	call := func(i int, id string, call *sessionsv1.ToolCall, answer *sessionsv1.Message) {
		tool := c.tools[call.GetName()]
		summary, whole := arguments(call.GetArguments(), tool.GetSubject())
		view, viewed := c.view(call.GetName())
		made := toolsapp.Call{Arguments: call.GetArguments(), Result: answer.GetText(), Failed: answer.GetFailed(), Ended: !t.busy || replies[i] < replies[len(replies)-1],
			Display: answer.GetShown(), Settings: c.desk.shown}
		if viewed && view.Line != nil {
			summary = cmp.Or(view.Line(made), summary)
		}
		name := render.Heading(cmp.Or(tool.GetTitle(), call.GetName()), look)
		head := render.ToolGlyph(tool.GetIcon(), look) + " " + name
		if answer.GetFailed() {
			head = render.Bad(look.Glyphs.Fail, look) + " " + name
		}
		gap()
		if len(whole) == 0 {
			rows = append(rows, row{text: ansi.Truncate(head+"  "+summary, w, look.Glyphs.Ellipsis)})
		} else {
			open := c.open(id+":call", c.expanded)
			rows = append(rows, row{text: ansi.Truncate(render.Dim(mark(open), look)+" "+head+"  "+summary, w, look.Glyphs.Ellipsis), fold: id + ":call"})
			if open {
				block(whole, false)
			}
		}
		switch {
		case answer != nil && viewed && view.Result != nil && !answer.GetFailed():
			if !view.Latest || latest[fmt.Sprint(replies[i], ":", call.GetName())] == id {
				if shown := view.Result(made, max(w-2, 1), look); shown != nil {
					presented(id+":result", shown)
				}
			}
		case answer != nil:
			result(id+":result", answer)
		case t.busy && len(t.approvals) == 0 && t.questions == nil:
			rows = append(rows, row{text: ansi.Truncate(render.Dim(running(t.progress[call.GetId()]), look), w, look.Glyphs.Ellipsis)})
		}
	}
	notes := t.asides
	for i, e := range t.entries {
		for len(notes) > 0 && notes[0].after <= i {
			prose(notes[0].text)
			notes = notes[1:]
		}
		if e.message == nil {
			prose(e.notice)
			continue
		}
		id := fmt.Sprint(i)
		switch e.message.GetRole() {
		case sessionsv1.Role_ROLE_USER:
			// A message the agent sent itself for the model alone, such as the request to sum up at the step limit, is not shown
			if e.message.GetDisplay() != "hidden" {
				prose("> " + strings.ReplaceAll(e.message.GetText(), "\n", "\n> "))
			}
		case sessionsv1.Role_ROLE_TOOL:
			if !answered[i] {
				gap()
				result(id+":result", e.message)
			}
		default:
			if e.message.GetReasoning() != "" {
				thinking(id+":thinking", e.message.GetReasoning())
			}
			// What a turn held for the hooks that rewrite replies said before its tool calls is never shown
			if e.message.GetText() != "" && e.message.GetDisplay() != "withheld" {
				prose(e.message.GetText())
			}
			for k, tool := range e.message.GetToolCalls() {
				call(i, fmt.Sprintf("%s:%d", id, k), tool, answer(i, tool))
			}
		}
	}
	for _, note := range notes {
		prose(note.text)
	}
	if t.reasoning.Len() > 0 {
		thinking("live:thinking", t.reasoning.String())
	}
	if t.text.Len() > 0 {
		prose(t.text.String())
	}
	rows = append(rows, c.spawnRows(w)...)
	if t.busy && t.text.Len() == 0 && t.reasoning.Len() == 0 && len(t.approvals) == 0 && t.questions == nil {
		gap()
		rows = append(rows, row{text: render.Dim("Working…", look)})
	}
	return rows
}

func (c *chat) markup(text string, w int) []string {
	if lines, ok := c.rendered[text]; ok {
		c.kept[text] = lines
		return lines
	}
	lines := strings.Split(strings.Trim(c.markdown.Render(text, w, c.kit.Env.Theme), "\n"), "\n")
	c.kept[text] = lines
	return lines
}

// How the chat shows the calls of the tool, false when they are shown as their text
// Line of a call still running: how far it got and what it does now, as far as its tool tells
func running(p *agentv1.Progress) string {
	if p == nil {
		return "Running…"
	}
	line := "Running…"
	switch {
	case p.Total != nil && p.GetTotal() > 0:
		line += fmt.Sprintf(" %d%%", int(math.Round(min(max(p.GetProgress()/p.GetTotal(), 0), 1)*100)))
	case p.GetProgress() > 0:
		line += fmt.Sprintf(" %g", p.GetProgress())
	}
	if message := strings.TrimSpace(p.GetMessage()); message != "" {
		line += " · " + message
	}
	return line
}

func (c *chat) view(tool string) (toolsapp.View, bool) {
	if c.desk.views == nil {
		return toolsapp.View{}, false
	}
	return c.desk.views.View(tool)
}

// Whether the call has an answer among the messages after it, and the answer is not a failure
func succeeded(after []entry, call *sessionsv1.ToolCall) bool {
	for _, e := range after {
		if e.message.GetRole() == sessionsv1.Role_ROLE_TOOL && e.message.GetToolCallId() == call.GetId() {
			return !e.message.GetFailed()
		}
	}
	return false
}

func (c *chat) open(id string, otherwise bool) bool {
	if open, set := c.folds[id]; set {
		return open
	}
	return otherwise
}

func outcome(message *sessionsv1.Message) string {
	var failure struct {
		Error string `json:"error"`
	}
	if message.GetFailed() && json.Unmarshal([]byte(message.GetText()), &failure) == nil && failure.Error != "" {
		return failure.Error
	}
	return message.GetText()
}

// Line that sums up the arguments of a call, the subject argument when the tool names one and else the first text argument, and the
// arguments in full when there is more to them than that line
func arguments(raw, subject string) (string, []string) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return strings.TrimSpace(raw), nil
	}
	summary, named, fields := "", false, 0
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			break
		}
		var value any
		if err := decoder.Decode(&value); err != nil {
			break
		}
		fields++
		text, ok := value.(string)
		switch {
		case !ok || named:
		case subject != "" && key == subject:
			summary, named = text, true
		case summary == "":
			summary = text
		}
	}
	first, rest, _ := strings.Cut(summary, "\n")
	if fields <= 1 && rest == "" {
		return first, nil
	}
	var pretty bytes.Buffer
	if json.Indent(&pretty, []byte(raw), "", "  ") != nil {
		return first, strings.Split(raw, "\n")
	}
	var whole []string
	for _, line := range strings.Split(pretty.String(), "\n") {
		whole = append(whole, strings.Split(strings.NewReplacer(`\n`, "\n", `\t`, "\t").Replace(line), "\n")...)
	}
	return first, whole
}

// Whether the session holds any of the conversation: a message the user sent, a reply or a tool's result
func (t *transcript) conversed() bool {
	for _, e := range t.entries {
		switch e.message.GetRole() {
		case sessionsv1.Role_ROLE_USER, sessionsv1.Role_ROLE_ASSISTANT, sessionsv1.Role_ROLE_TOOL:
			return true
		}
	}
	return false
}
