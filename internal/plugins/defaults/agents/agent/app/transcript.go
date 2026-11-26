package app

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"

	agentconfig "github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/config"
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
	approval  *agentv1.Approval
	state     *agentv1.State
	busy      bool
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
	case *agentv1.WatchResponse_Delta:
		t.text.WriteString(event.Delta.GetText())
		t.reasoning.WriteString(event.Delta.GetReasoning())
	case *agentv1.WatchResponse_Approval:
		if t.approval = event.Approval; event.Approval.GetCommand() == "" {
			t.approval = nil
		}
	case *agentv1.WatchResponse_State:
		t.state = event.State
	case *agentv1.WatchResponse_Notice:
		t.entries = append(t.entries, entry{notice: event.Notice})
	case *agentv1.WatchResponse_Busy:
		if t.busy = event.Busy; !t.busy {
			t.settle()
		}
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
	t.entries, t.approval, t.busy = nil, nil, false
	t.text.Reset()
	t.reasoning.Reset()
}

func (t *transcript) note(text string) {
	t.asides = append(t.asides, aside{after: len(t.entries), text: text})
}

func (t *transcript) settle() {
	t.text.Reset()
	t.reasoning.Reset()
	t.approval = nil
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
		rows = append(rows, row{text: render.Dim(mark(open)+" Thinking", look), fold: id})
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
	call := func(id string, call *sessionsv1.ToolCall, answer *sessionsv1.Message) {
		tool := c.tools[call.GetName()]
		summary, whole := arguments(call.GetArguments(), tool.GetSubject())
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
		case answer != nil:
			result(id+":result", answer)
		case t.busy && t.approval == nil:
			rows = append(rows, row{text: render.Dim("Running…", look)})
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
			prose("> " + strings.ReplaceAll(e.message.GetText(), "\n", "\n> "))
		case sessionsv1.Role_ROLE_TOOL:
			if !answered[i] {
				gap()
				result(id+":result", e.message)
			}
		default:
			if e.message.GetReasoning() != "" {
				thinking(id+":thinking", e.message.GetReasoning())
			}
			if e.message.GetText() != "" {
				prose(e.message.GetText())
			}
			for k, tool := range e.message.GetToolCalls() {
				call(fmt.Sprintf("%s:%d", id, k), tool, answer(i, tool))
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
	if t.busy && t.text.Len() == 0 && t.reasoning.Len() == 0 && t.approval == nil {
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
