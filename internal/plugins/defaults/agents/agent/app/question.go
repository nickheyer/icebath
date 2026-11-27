package app

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	agentv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

const questionContext = "agent.question"

var questionKeys = []keys.Action{
	{ID: "agent.question.toggle", Context: questionContext, Label: "tick", Doc: "tick or untick the choice", Default: []string{"space"}},
	{ID: "agent.question.next", Context: questionContext, Label: "next question", Doc: "go to the next question", Default: []string{"tab"}},
	{ID: "agent.question.prev", Context: questionContext, Label: "previous question", Doc: "go to the question before", Default: []string{"shift+tab"}},
	{ID: "agent.question.skip", Context: questionContext, Label: "skip", Doc: "skip this question", Default: []string{"s"}},
	{ID: "agent.question.type", Context: questionContext, Label: "type answer", Doc: "type your own answer in the prompt", Default: []string{"o"}},
}

// Questions the agent waits on, asked in the chat in place of the prompt: one question open at a time among the others, its choices picked
// with the arrows and enter or their number, an answer of one's own typed in the prompt
type inquiry struct {
	chat   *chat
	set    *agentv1.Questions
	active int
	// Row of the open question under the cursor, the last one typing an answer
	row int
	// Ticked choices of the open question
	marks map[int]bool
	// Line of the row under the cursor, kept in view
	cursor int
	// Answer typed so far to a question asking for a secret, which is typed here, masked, rather than in the prompt and its history
	secret []rune
}

// What a secret shows as, its length kept from view
const masked = "••••••••"

func (q *inquiry) Title() string {
	if len(q.set.GetQuestions()) > 1 {
		return fmt.Sprintf("Question %d of %d", q.active+1, len(q.set.GetQuestions()))
	}
	return "Question"
}

func (q *inquiry) Key() string               { return q.chat.key + ":question" }
func (q *inquiry) Init() tea.Cmd             { return nil }
func (q *inquiry) Contexts() []string        { return []string{questionContext} }
func (q *inquiry) ClosesItself() bool        { return true }
func (q *inquiry) FitIn(w, h int) (int, int) { return w, min(len(q.lines(w)), h) }

func (q *inquiry) question() *agentv1.Question {
	return q.set.GetQuestions()[q.active]
}

// Rows of the open question: its choices, then the one to type an answer; a secret only the one
func (q *inquiry) rows() int {
	if q.question().GetSecret() {
		return 1
	}
	return len(q.question().GetChoices()) + 1
}

// A secret is typed into the card itself, so the keys of picking choices are left to it as typed
func (q *inquiry) Answers(action string) bool {
	if !q.question().GetSecret() {
		return true
	}
	switch action {
	case "agent.question.toggle", "agent.question.skip", "agent.question.type":
		return false
	}
	return true
}

func (q *inquiry) Hints() []wm.Hint {
	lock := "lock answer"
	if q.open() == 1 {
		lock = "confirm and continue"
	}
	hints := []wm.Hint{{Action: "palette.pick", Label: lock}}
	if q.question().GetSecret() {
		return append(hints, wm.Hint{Label: "empty to skip"}, wm.Hint{Action: "palette.close", Label: "dismiss"})
	}
	if q.question().GetMultiSelect() {
		hints = append(hints, wm.Hint{Action: "agent.question.toggle"})
	}
	if len(q.set.GetQuestions()) > 1 {
		hints = append(hints, wm.Hint{Action: "agent.question.next"})
	}
	return append(hints, wm.Hint{Action: "agent.question.skip"}, wm.Hint{Action: "palette.close", Label: "dismiss"})
}

// Questions still without an answer
func (q *inquiry) open() int {
	count := 0
	for _, question := range q.set.GetQuestions() {
		if _, ok := q.set.GetAnswers()[question.GetId()]; !ok {
			count++
		}
	}
	return count
}

func (q *inquiry) Update(msg tea.Msg) (wm.Card, tea.Cmd) {
	switch msg := msg.(type) {
	case wm.ActionMsg:
		switch msg.ID {
		case "palette.up":
			q.row = (q.row - 1 + q.rows()) % q.rows()
		case "palette.down":
			q.row = (q.row + 1) % q.rows()
		case "agent.question.next":
			q.show(q.active + 1)
		case "agent.question.prev":
			q.show(q.active - 1)
		case "agent.question.toggle":
			if q.question().GetMultiSelect() && q.row < len(q.question().GetChoices()) {
				q.marks[q.row] = !q.marks[q.row]
			}
		case "agent.question.skip":
			return q, q.chat.respond(q.set.GetId(), q.question().GetId(), nil)
		case "agent.question.type":
			return q, q.chat.typeAnswer(q.ticked())
		case "palette.pick":
			return q, q.pick()
		case "palette.close":
			return q, q.chat.dismiss(q.set.GetId())
		}
	case tea.KeyPressMsg:
		if q.question().GetSecret() {
			switch msg.String() {
			case "backspace":
				if len(q.secret) > 0 {
					q.secret = q.secret[:len(q.secret)-1]
				}
			case "ctrl+u":
				q.secret = nil
			default:
				q.secret = append(q.secret, []rune(msg.Text)...)
			}
			return q, nil
		}
		if n, err := strconv.Atoi(msg.String()); err == nil && n >= 1 && n <= q.rows() {
			q.row = n - 1
			if q.question().GetMultiSelect() && q.row < len(q.question().GetChoices()) {
				q.marks[q.row] = !q.marks[q.row]
				return q, nil
			}
			return q, q.pick()
		}
	}
	return q, nil
}

// Locks the choice under the cursor, or every choice ticked, or goes to the prompt to type an answer; a secret is locked as typed, none
// skipping it
func (q *inquiry) pick() tea.Cmd {
	if q.question().GetSecret() {
		secret := string(q.secret)
		q.secret = nil
		return q.chat.respond(q.set.GetId(), q.question().GetId(), []string{secret})
	}
	choices := q.question().GetChoices()
	if q.row == len(choices) {
		return q.chat.typeAnswer(q.ticked())
	}
	picked := q.ticked()
	if len(picked) == 0 {
		picked = []string{choices[q.row]}
	}
	return q.chat.respond(q.set.GetId(), q.question().GetId(), picked)
}

// Choices ticked on a question that takes several
func (q *inquiry) ticked() []string {
	var picked []string
	for i, choice := range q.question().GetChoices() {
		if q.marks[i] {
			picked = append(picked, choice)
		}
	}
	return picked
}

// Opens the question, its locked answer put back on the rows
func (q *inquiry) show(index int) {
	count := len(q.set.GetQuestions())
	q.active = (index%count + count) % count
	q.row, q.marks, q.secret = 0, map[int]bool{}, nil
	question := q.question()
	answer, ok := q.set.GetAnswers()[question.GetId()]
	if !ok {
		return
	}
	for _, value := range answer.GetValues() {
		if at := slices.Index(question.GetChoices(), value); at >= 0 {
			q.row = at
			if question.GetMultiSelect() {
				q.marks[at] = true
			}
		}
	}
}

// Takes the questions as they stand now: a locked answer moves on to the first question still open
func (q *inquiry) refresh(set *agentv1.Questions) {
	q.set = set
	if _, answered := set.GetAnswers()[q.question().GetId()]; !answered {
		return
	}
	for i, question := range set.GetQuestions() {
		if _, answered := set.GetAnswers()[question.GetId()]; !answered {
			q.show(i)
			return
		}
	}
}

func (q *inquiry) lines(w int) []string {
	look := q.chat.kit.Env.Theme
	g := look.Glyphs
	wrap := func(text string, indent int) []string {
		return strings.Split(render.Wrap(text, max(w-indent, 8)), "\n")
	}
	var lines []string
	for i, question := range q.set.GetQuestions() {
		answer, answered := q.set.GetAnswers()[question.GetId()]
		mark := g.Bullet
		switch {
		case answered:
			mark = g.Check
		case i == q.active:
			mark = g.Forward
		}
		for n, line := range wrap(question.GetText(), 3) {
			head := "   "
			if n == 0 {
				head = mark + "  "
			}
			if i == q.active {
				lines = append(lines, render.Heading(head+line, look))
			} else {
				lines = append(lines, render.Dim(head+line, look))
			}
		}
		if answered {
			text, good := "(skipped)", false
			if values := answer.GetValues(); len(values) > 0 {
				text, good = strings.Join(values, ", "), true
				if question.GetSecret() {
					text = masked
				}
			}
			for _, line := range wrap(text, 5) {
				if good {
					lines = append(lines, "     "+render.Good(line, look))
				} else {
					lines = append(lines, "     "+render.Dim(line, look))
				}
			}
		}
		if i != q.active {
			continue
		}
		if question.GetSecret() {
			q.cursor = len(lines)
			lines = append(lines, "   "+g.Forward+" "+render.Warm("Secret › ", look)+strings.Repeat("•", len(q.secret))+"▏")
			continue
		}
		label := "Type your answer ›"
		if len(question.GetChoices()) > 0 {
			label = "Other (type your answer) ›"
		}
		for r, choice := range append(slices.Clone(question.GetChoices()), label) {
			box := ""
			if question.GetMultiSelect() && r < len(question.GetChoices()) {
				box = g.Todo + " "
				if q.marks[r] {
					box = g.Done + " "
				}
			}
			cursor := "  "
			if r == q.row {
				cursor = g.Forward + " "
			}
			for n, line := range wrap(fmt.Sprintf("%d  %s%s", r+1, box, choice), 7) {
				if n > 0 {
					line = "      " + line
				}
				if r == q.row {
					q.cursor = len(lines)
					lines = append(lines, "   "+cursor+render.Warm(line, look))
				} else {
					lines = append(lines, "   "+cursor+line)
				}
			}
		}
	}
	return lines
}

func (q *inquiry) View(w, h int) string {
	lines := q.lines(w)
	start := max(q.cursor-h+1, 0)
	lines = lines[start:min(start+h, len(lines))]
	for len(lines) < h {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}

// Shows the questions the agent waits on in place of the prompt, keeping the one in view as answers come in, and takes them away once settled
func (c *chat) inquire() tea.Cmd {
	set := c.log.questions
	if set == nil && c.typing != nil {
		c.typing, c.composer.Hint = nil, c.hint()
	}
	if set != nil && c.typing != nil && c.typing.set == set.GetId() {
		return nil
	}
	if c.inquiry != nil && set != nil && c.inquiry.set.GetId() == set.GetId() {
		c.inquiry.refresh(set)
		return nil
	}
	var cmds []tea.Cmd
	if c.inquiry != nil {
		cmds = append(cmds, c.kit.WM.DismissCard(c.inquiry))
		c.inquiry = nil
	}
	if set == nil || len(set.GetQuestions()) == 0 {
		return tea.Batch(cmds...)
	}
	c.inquiry = &inquiry{chat: c, set: set, marks: map[int]bool{}}
	c.inquiry.refresh(set)
	return tea.Batch(append(cmds, c.kit.WM.Attach(c, c.inquiry))...)
}

// Answer being typed in the prompt, with the choices ticked before
type answering struct {
	set, question string
	picked        []string
}

// Moves the answer to the prompt, the questions coming back once it is sent or left empty
func (c *chat) typeAnswer(picked []string) tea.Cmd {
	q := c.inquiry
	if q == nil {
		return nil
	}
	c.typing = &answering{set: q.set.GetId(), question: q.question().GetId(), picked: picked}
	c.composer.Hint = "Your answer: " + q.question().GetText()
	c.inquiry = nil
	return c.kit.WM.DismissCard(q)
}

// Leaves the typed answer and goes back to the questions
func (c *chat) untype() tea.Cmd {
	c.typing, c.composer.Hint = nil, c.hint()
	return c.inquire()
}

// Locks an answer to one question, none skipping it
func (c *chat) respond(set, question string, values []string) tea.Cmd {
	id := c.id
	return tui.Async(func() func() tea.Cmd {
		_, err := c.desk.agents.Answer(c.kit.Env.Ctx, &agentv1.AnswerRequest{SessionId: id, Id: set, QuestionId: question, Answer: &agentv1.Answer{Values: values}})
		return func() tea.Cmd {
			if err != nil {
				c.log.note("**Couldn't answer:** " + tui.Reason(err))
			}
			return nil
		}
	})
}

// Dismisses the questions, the answers locked so far kept
func (c *chat) dismiss(set string) tea.Cmd {
	id := c.id
	return tui.Async(func() func() tea.Cmd {
		_, err := c.desk.agents.Dismiss(c.kit.Env.Ctx, &agentv1.DismissRequest{SessionId: id, Id: set})
		return func() tea.Cmd {
			if err != nil {
				c.log.note("**Couldn't dismiss the questions:** " + tui.Reason(err))
			}
			return nil
		}
	})
}
