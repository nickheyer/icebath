package cards

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
)

// Narrowest width the question block wraps at
const askMinWidth = 24

// Columns the question text and the choices stand in from the left edge
const askIndent = 3

// Questions of one ask as the prompt walks the person through them, one at a time
type asking struct {
	id      int
	qs      []harness.Question
	at      int
	idx     int
	marked  []bool
	answers []harness.Answer
	// First row of the block in view, moved only as far as keeps the highlighted choice in view
	top int
	// Rows in view since the last paint, each naming the choice it belongs to
	shown []askRow
}

// One row of the question block, the choice it belongs to or -1 for the question and the padding
type askRow struct {
	text   string
	choice int
	// Row carrying the label of its choice rather than its description
	label bool
}

// Question the person is on
func (a *asking) q() harness.Question { return a.qs[a.at] }

// Index of the row standing for an answer typed instead of picked
func (a *asking) other() int { return len(a.q().Choices) }

// Moves to the next question, false once every question is answered
func (a *asking) advance(ans harness.Answer) bool {
	a.answers = append(a.answers, ans)
	a.at++
	a.idx, a.top = 0, 0
	if a.at >= len(a.qs) {
		return false
	}
	a.marked = make([]bool, len(a.q().Choices))
	return true
}

// Labels marked on a multiple choice question, the highlighted one when none is marked
func (a *asking) picked() []string {
	q := a.q()
	var out []string
	for i, on := range a.marked {
		if on {
			out = append(out, q.Choices[i].Label)
		}
	}
	if len(out) == 0 && a.idx < len(q.Choices) {
		out = append(out, q.Choices[a.idx].Label)
	}
	return out
}

// Rows of the question on show at width w, its text wrapped, then every choice with its description beneath
func (a *asking) layout(w int, t theme.Theme) []askRow {
	w = max(w, askMinWidth)
	q := a.q()
	plain := lipgloss.NewStyle().Foreground(t.Text)
	lead := lipgloss.NewStyle().Foreground(t.Accent).Bold(true)
	count := ""
	if len(a.qs) > 1 {
		count = strconv.Itoa(a.at+1) + " of " + strconv.Itoa(len(a.qs))
	}
	room := w - askIndent
	if count != "" {
		room -= ansi.StringWidth(count) + 2
	}
	text := wrapLines(strings.TrimSpace(q.Text), max(room, 8))
	head := " " + lead.Render(t.Glyphs.Ask) + " "
	if h := strings.TrimSpace(q.Header); h != "" {
		head += lead.Render(h)
	} else {
		head += plain.Render(text[0])
		text = text[1:]
	}
	if count != "" {
		head = render.Fit(head, w-ansi.StringWidth(count)-1, t) + render.Dim(count, t)
	}
	pad := strings.Repeat(" ", askIndent)
	rows := []askRow{{choice: -1}, {text: head, choice: -1}}
	for _, l := range text {
		rows = append(rows, askRow{text: pad + plain.Render(l), choice: -1})
	}
	rows = append(rows, askRow{choice: -1})
	n := a.other() + 1
	numW := len(strconv.Itoa(n))
	markW := 0
	if q.Multi {
		markW = 2
	}
	indent := askIndent + markW + numW + 1
	body := strings.Repeat(" ", indent)
	for i := range n {
		label, desc := "Other", "type your answer below"
		if i < a.other() {
			label, desc = q.Choices[i].Label, q.Choices[i].Description
		}
		mark := strings.Repeat(" ", markW)
		if q.Multi && i < a.other() {
			mark = t.Glyphs.Todo + " "
			if a.marked[i] {
				mark = t.Glyphs.Done + " "
			}
		}
		lines := wrapLines(strings.TrimSpace(label), max(w-indent-1, 8))
		rows = append(rows, askRow{text: pad + mark + render.Dim(fmt.Sprintf("%*d", numW, i+1), t) + " " + plain.Render(lines[0]), choice: i, label: true})
		for _, l := range lines[1:] {
			rows = append(rows, askRow{text: body + plain.Render(l), choice: i, label: true})
		}
		for _, l := range wrapLines(strings.TrimSpace(desc), max(w-indent-1, 8)) {
			if l != "" {
				rows = append(rows, askRow{text: body + render.Dim(l, t), choice: i})
			}
		}
	}
	return append(rows, askRow{choice: -1})
}

// Shows the questions of an ask in the prompt, nil clearing them, the ask already shown left as it stands
func (c *Composer) SetQuestion(a *harness.Asking) {
	if a == nil {
		c.ask = nil
		return
	}
	if c.ask != nil && c.ask.id == a.ID {
		return
	}
	c.ask = &asking{id: a.ID, qs: a.Questions, marked: make([]bool, len(a.Questions[0].Choices))}
	c.comp = nil
}

// Reports whether a question waits in the prompt
func (c *Composer) Asking() bool { return c.ask != nil }

// Reports whether the question on show takes several choices
func (c *Composer) AskingMulti() bool { return c.ask != nil && c.ask.q().Multi }

// Carries out a question action, reporting whether the id was one
func (c *Composer) questionAction(id string) (tea.Cmd, bool) {
	a := c.ask
	if a == nil {
		return nil, false
	}
	n := len(a.q().Choices) + 1
	switch id {
	case "question.down":
		a.idx = (a.idx + 1) % n
	case "question.up":
		a.idx = (a.idx - 1 + n) % n
	case "question.toggle":
		if a.q().Multi && a.idx < a.other() {
			a.marked[a.idx] = !a.marked[a.idx]
		}
	case "question.pick":
		return c.answerQuestion(), true
	case "question.skip":
		return c.settle(harness.Answer{Declined: true}), true
	default:
		return nil, false
	}
	return nil, true
}

// Takes a digit typed into an empty prompt as the choice it numbers, reporting whether it was one
func (c *Composer) questionDigit(text string) (tea.Cmd, bool) {
	a := c.ask
	if a == nil || strings.TrimSpace(c.ta.Value()) != "" || len(text) != 1 || text[0] < '1' || text[0] > '9' {
		return nil, false
	}
	i := int(text[0] - '1')
	if i >= a.other() {
		return nil, false
	}
	a.idx = i
	if a.q().Multi {
		a.marked[i] = !a.marked[i]
		return nil, true
	}
	return c.answerQuestion(), true
}

// Answers the question on show with the text typed, else the choices picked, nothing when neither is there
func (c *Composer) answerQuestion() tea.Cmd {
	a := c.ask
	if a == nil {
		return nil
	}
	if text := strings.TrimSpace(c.ta.Value()); text != "" {
		return c.settle(harness.Answer{Text: text})
	}
	if a.idx >= a.other() {
		return nil
	}
	return c.settle(harness.Answer{Picked: a.picked()})
}

// Records the answer, clears the prompt and moves on, sending every answer once the last question is done
func (c *Composer) settle(ans harness.Answer) tea.Cmd {
	a := c.ask
	c.ta.Reset()
	c.comp = nil
	cmd := c.persist()
	if a.advance(ans) {
		return cmd
	}
	c.ask = nil
	id, answers := a.id, a.answers
	return tea.Batch(cmd, func() tea.Msg { return ReplyMsg{ID: id, Answers: answers} })
}

// Takes a click on a row in view, picking or marking the choice there, reporting whether one was hit
func (c *Composer) questionClick(y int) (tea.Cmd, bool) {
	a := c.ask
	if a == nil || y < 0 || y >= len(a.shown) || a.shown[y].choice < 0 {
		return nil, false
	}
	a.idx = a.shown[y].choice
	if a.idx >= a.other() {
		return nil, true
	}
	if a.q().Multi {
		a.marked[a.idx] = !a.marked[a.idx]
		return nil, true
	}
	return c.answerQuestion(), true
}

// Rows of the question block in view at width w within room rows, the highlighted choice kept in view
func (c *Composer) askRows(w, room int) []string {
	a := c.ask
	if a == nil {
		return nil
	}
	t := c.theme
	rows := a.layout(w, t)
	cur := 0
	for i, r := range rows {
		if r.choice == a.idx && r.label {
			cur = i
			break
		}
	}
	top := 0
	shown := rows
	if len(rows) > room {
		top = cur
		if room > 2 {
			top = min(max(a.top, cur-room+2), cur-1)
			top = max(0, min(top, len(rows)-room))
		}
		shown = slices.Clone(rows[top:min(top+room, len(rows))])
		more := askRow{text: strings.Repeat(" ", askIndent) + render.Dim(t.Glyphs.Ellipsis, t), choice: -1}
		if top > 0 && room > 2 {
			shown[0] = more
		}
		if top+room < len(rows) && room > 2 {
			shown[len(shown)-1] = more
		}
	}
	a.top, a.shown = top, shown
	lit := lipgloss.NewStyle().Foreground(t.Bg).Background(t.Ice).Bold(true)
	out := make([]string, len(shown))
	for i, r := range shown {
		if r.choice == a.idx && r.label {
			out[i] = lit.Render(render.Fit(ansi.Strip(r.text), w, t))
			continue
		}
		out[i] = render.Fit(r.text, w, t)
	}
	return out
}

// Dim invitation shown in the empty input while a question waits
func (c *Composer) questionPlaceholder(w int) string {
	t := c.theme
	prompt := t.TextDim
	if c.focused {
		prompt = t.Accent
	}
	hint := c.keys.Display("question.pick") + " answers, or type your own"
	if c.AskingMulti() {
		hint = c.keys.Display("question.toggle") + " marks, " + hint
	}
	line := lipgloss.NewStyle().Foreground(prompt).Render(c.ta.Prompt) + lipgloss.NewStyle().Foreground(t.TextDim).Render(hint)
	return render.Fit(line, w, t)
}
