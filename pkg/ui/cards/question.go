package cards

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
)

// Label the first choice carries, the best answer in the order the agent offered
const recommendedLabel = " (Recommended)"

// Row the free text answer takes among the choices
const otherLabel = "Other, type your own answer"

// One question the agent puts to the person, with choices ordered best first or none for free text
type Question struct {
	Text        string
	Choices     []string
	MultiSelect bool
}

// Answer a person gave one question, status answered, skipped or unanswered
type Answer struct {
	Question       string   `json:"question"`
	ChoicesOffered []string `json:"choices_offered"`
	Status         string   `json:"status"`
	// Chosen text, a list of texts for a multi select, nil while not answered
	Response any `json:"user_response"`
}

// Answers a person gave to a batch of questions, outcome submitted or cancelled
type ReplyMsg struct {
	Session string
	ID      string
	Outcome string
	Answers []Answer
}

// State of one question while the batch is open
type questionState struct {
	locked bool
	picks  map[int]bool
	text   string
}

// Batch of questions waiting in the composer, one active at a time
type asking struct {
	id        string
	questions []Question
	state     []questionState
	active    int
	// Highlighted choice of the active question, the other row when past the choices
	idx   int
	shown []string
	// First row of each rendered choice of the active question, keyed by choice index
	rows map[int]int
}

// Puts a batch of questions to the person, answered through the composer
func (c *Composer) Ask(id string, qs []Question) {
	a := &asking{id: id, questions: qs, state: make([]questionState, len(qs)), rows: map[int]int{}}
	for i := range a.state {
		a.state[i].picks = map[int]bool{}
	}
	c.ask = a
	c.comp = nil
	c.ta.Reset()
}

// Reports whether a batch of questions is waiting
func (c *Composer) Asking() bool { return c.ask != nil }

// Index of the free text row of the active question
func (a *asking) other() int { return len(a.questions[a.active].Choices) }

// Next question without an answer after the active one, the active one when every other is answered
func (a *asking) nextOpen() int {
	n := len(a.questions)
	for k := 1; k <= n; k++ {
		i := (a.active + k) % n
		if !a.state[i].locked {
			return i
		}
	}
	return a.active
}

// Reports whether every question has an answer or a skip
func (a *asking) done() bool {
	for _, s := range a.state {
		if !s.locked {
			return false
		}
	}
	return true
}

// Answers of every question in order, unanswered ones marked so
func (a *asking) answers() []Answer {
	out := make([]Answer, len(a.questions))
	for i, q := range a.questions {
		s := a.state[i]
		out[i] = Answer{Question: q.Text, ChoicesOffered: q.Choices, Status: "unanswered"}
		if !s.locked {
			continue
		}
		switch {
		case q.MultiSelect && len(s.picks) > 0:
			var picked []string
			for j, ch := range q.Choices {
				if s.picks[j] {
					picked = append(picked, ch)
				}
			}
			if s.text != "" {
				picked = append(picked, s.text)
			}
			out[i].Status, out[i].Response = "answered", picked
		case s.text != "":
			out[i].Status, out[i].Response = "answered", s.text
		case !q.MultiSelect && len(s.picks) == 1:
			for j := range s.picks {
				out[i].Status, out[i].Response = "answered", q.Choices[j]
			}
		default:
			out[i].Status = "skipped"
		}
	}
	return out
}

// Rows of the batch, every question on one line and the active question with its choices expanded
func (a *asking) layout(w int, t theme.Theme) []string {
	a.rows = map[int]int{}
	var rows []string
	for i, q := range a.questions {
		mark := t.Glyphs.Bullet
		switch {
		case a.state[i].locked:
			mark = t.Glyphs.Check
		case i == a.active:
			mark = t.Glyphs.Active
		}
		head := render.Fit(mark+" "+q.Text, w, t)
		if i == a.active {
			head = render.Warm(head, t)
		} else {
			head = render.Dim(head, t)
		}
		rows = append(rows, head)
		if i != a.active {
			continue
		}
		for j, ch := range q.Choices {
			label := ch
			if j == 0 {
				label += recommendedLabel
			}
			box := ""
			if q.MultiSelect {
				box = "[ ] "
				if a.state[i].picks[j] {
					box = "[x] "
				}
			}
			line := "  " + itoa(j+1) + ". " + box + label
			a.rows[j] = len(rows)
			if j == a.idx {
				line = render.Warm(line, t)
			}
			rows = append(rows, render.Fit(line, w, t))
		}
		if len(q.Choices) > 0 {
			line := "  " + itoa(len(q.Choices)+1) + ". " + otherLabel
			a.rows[len(q.Choices)] = len(rows)
			if a.idx == len(q.Choices) {
				line = render.Warm(line, t)
			}
			rows = append(rows, render.Fit(line, w, t))
		}
	}
	a.shown = rows
	return rows
}

// Rows the question batch takes above the input, cut to the room
func (c *Composer) askRows(w, room int) []string {
	if c.ask == nil {
		return nil
	}
	rows := c.ask.layout(w, c.theme)
	if len(rows) > room {
		rows = rows[len(rows)-room:]
		c.ask.shown = rows
	}
	return rows
}

// Hint shown in the empty input while a question waits
func (c *Composer) questionPlaceholder(w int) string {
	q := c.ask.questions[c.ask.active]
	hint := "type an answer, enter to skip, esc to cancel"
	if len(q.Choices) > 0 {
		hint = "pick a number or type your own, enter to lock, tab for the next question, esc to cancel"
	}
	return render.Dim(ansi.Truncate(c.theme.Glyphs.Prompt+" "+hint, max(w, 1), c.theme.Glyphs.Ellipsis), c.theme)
}

// Picks a choice by its digit while the input is empty, reporting whether the key was taken
func (c *Composer) questionDigit(text string) (tea.Cmd, bool) {
	if c.ask == nil || len(text) != 1 || text[0] < '1' || text[0] > '9' || strings.TrimSpace(c.ta.Value()) != "" {
		return nil, false
	}
	q := c.ask.questions[c.ask.active]
	n := int(text[0] - '1')
	if n > len(q.Choices) || len(q.Choices) == 0 {
		return nil, false
	}
	return c.pickChoice(n), true
}

// Highlights choice n and for a multi select toggles it, the other row only moving the highlight
func (c *Composer) pickChoice(n int) tea.Cmd {
	a := c.ask
	a.idx = n
	if n == a.other() {
		return nil
	}
	s := &a.state[a.active]
	if a.questions[a.active].MultiSelect {
		s.picks[n] = !s.picks[n]
		return nil
	}
	for k := range s.picks {
		delete(s.picks, k)
	}
	s.picks[n] = true
	return nil
}

// Carries out a keymap action on the waiting question, reporting whether the action belonged to it
func (c *Composer) questionAction(id string) (tea.Cmd, bool) {
	a := c.ask
	if a == nil {
		return nil, false
	}
	switch id {
	case "prompt.send":
		return c.lockActive(), true
	case "complete.next", "prompt.history.next":
		a.active = (a.active + 1) % len(a.questions)
		a.idx = 0
		return nil, true
	case "complete.prev", "prompt.history.prev":
		a.active = (a.active - 1 + len(a.questions)) % len(a.questions)
		a.idx = 0
		return nil, true
	case "list.down":
		a.idx = min(a.idx+1, a.other())
		return nil, true
	case "list.up":
		a.idx = max(a.idx-1, 0)
		return nil, true
	case "capture.cancel", "prompt.clear":
		return c.cancelAsk(), true
	}
	return nil, false
}

// Locks the active answer from the typed text or the highlighted choice, submitting once every question has one
func (c *Composer) lockActive() tea.Cmd {
	a := c.ask
	q := a.questions[a.active]
	s := &a.state[a.active]
	text := strings.TrimSpace(c.ta.Value())
	if text != "" {
		s.text = text
	} else if !q.MultiSelect && len(q.Choices) > 0 && a.idx < a.other() {
		for k := range s.picks {
			delete(s.picks, k)
		}
		s.picks[a.idx] = true
	}
	s.locked = true
	c.ta.Reset()
	if a.done() {
		return c.submitAsk("submitted")
	}
	a.active = a.nextOpen()
	a.idx = 0
	return nil
}

// Ends the batch, every answer locked so far kept
func (c *Composer) cancelAsk() tea.Cmd {
	return c.submitAsk("cancelled")
}

// Hands the answers to the app and clears the batch
func (c *Composer) submitAsk(outcome string) tea.Cmd {
	a := c.ask
	c.ask = nil
	c.ta.Reset()
	id, session := a.id, c.SessionID
	answers := a.answers()
	return func() tea.Msg { return ReplyMsg{Session: session, ID: id, Outcome: outcome, Answers: answers} }
}

// Picks the choice a click on row y lands on, reporting whether a choice was hit
func (c *Composer) questionClick(y int) (tea.Cmd, bool) {
	a := c.ask
	if a == nil {
		return nil, false
	}
	offset := len(a.layout(c.width, c.theme)) - len(a.shown)
	for n, row := range a.rows {
		if row-offset == y {
			return c.pickChoice(n), true
		}
	}
	return nil, false
}
