package cards

import (
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/ui/render"
)

// Questions of one ask as the prompt walks the person through them, one at a time
type asking struct {
	id      int
	qs      []harness.Question
	at      int
	idx     int
	marked  []bool
	answers []harness.Answer
}

// Question the person is on
func (a *asking) q() harness.Question { return a.qs[a.at] }

// Index of the row standing for an answer typed instead of picked
func (a *asking) other() int { return len(a.q().Choices) }

// Rows the question block takes, the question line, one per choice and the typed answer row
func (a *asking) rows() int { return 2 + len(a.q().Choices) }

// Moves to the next question, false once every question is answered
func (a *asking) advance(ans harness.Answer) bool {
	a.answers = append(a.answers, ans)
	a.at++
	a.idx = 0
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

// Takes a click on a question row, picking or marking the choice there, reporting whether a row was hit
func (c *Composer) questionClick(y int) (tea.Cmd, bool) {
	a := c.ask
	if a == nil || y < 1 || y >= a.rows() {
		return nil, false
	}
	a.idx = y - 1
	if a.idx >= a.other() {
		return nil, true
	}
	if a.q().Multi {
		a.marked[a.idx] = !a.marked[a.idx]
		return nil, true
	}
	return c.answerQuestion(), true
}

// Question block above the input, the question then every choice with the highlighted one lit
func (c *Composer) questionRows(w int) []string {
	a := c.ask
	if a == nil {
		return nil
	}
	t := c.theme
	q := a.q()
	head := lipgloss.NewStyle().Foreground(t.Accent).Bold(true).Render(t.Glyphs.Flag + " ")
	if q.Header != "" {
		head += lipgloss.NewStyle().Foreground(t.TextDim).Render(q.Header+" "+t.Glyphs.Sep) + " "
	}
	count := ""
	if len(a.qs) > 1 {
		count = render.Dim(strconv.Itoa(a.at+1)+"/"+strconv.Itoa(len(a.qs)), t)
	}
	room := max(w-ansi.StringWidth(count)-1, 1)
	first := render.Fit(head+lipgloss.NewStyle().Foreground(t.Text).Render(q.Text), room, t)
	if count != "" {
		first += " " + count
	}
	rows := []string{first}
	for i := 0; i <= a.other(); i++ {
		label, desc := "Other", "type your own answer below"
		if i < a.other() {
			label, desc = q.Choices[i].Label, q.Choices[i].Description
		}
		mark := "  "
		if q.Multi && i < a.other() {
			mark = t.Glyphs.Todo + " "
			if a.marked[i] {
				mark = t.Glyphs.Done + " "
			}
		}
		line := " " + mark + strconv.Itoa(i+1) + " " + label
		if desc != "" {
			line = render.Fit(line, min(max(ansi.StringWidth(line)+1, 18), w), t) + render.Dim(desc, t)
		}
		st := lipgloss.NewStyle().Foreground(t.Text)
		if i == a.idx {
			st = lipgloss.NewStyle().Foreground(t.Bg).Background(t.Ice).Bold(true)
			line = ansi.Strip(line)
		}
		rows = append(rows, st.Render(render.Fit(line, w, t)))
	}
	return rows
}

// Dim invitation shown in the empty input while a question waits
func (c *Composer) questionPlaceholder(w int) string {
	t := c.theme
	prompt := t.TextDim
	if c.focused {
		prompt = t.Accent
	}
	hint := "pick with " + c.keys.Display("question.up") + " " + c.keys.Display("question.down") + " and " + c.keys.Display("question.pick") + ", or type your own answer"
	if c.AskingMulti() {
		hint = "mark with " + c.keys.Display("question.toggle") + ", answer with " + c.keys.Display("question.pick") + ", or type your own answer"
	}
	line := lipgloss.NewStyle().Foreground(prompt).Render(c.ta.Prompt) + lipgloss.NewStyle().Foreground(t.TextDim).Render(hint)
	return render.Fit(line, w, t)
}
