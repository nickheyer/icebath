package cards

import (
	"slices"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// One job in steps under its own title, each step a form or a list to pick from shown in the place of the one before. A hub reads the step
// in view as its view on top, and going back from a step shows the one before as it was left
type Steps struct {
	title string
	steps []wm.Card
}

// Window manager interfaces the steps answer through the step in view
var _ interface {
	wm.Card
	wm.Contexter
	wm.Hinter
	wm.Statuser
	wm.Cursorer
	wm.Raw
	wm.Answerer
	wm.Grabber
	wm.Selectable
	Closer
	Fitted
} = (*Steps)(nil)

// Steps of a job titled as the job is, with no step yet
func NewSteps(title string) *Steps { return &Steps{title: title} }

func (s *Steps) Title() string    { return s.title }
func (s *Steps) Init() tea.Cmd    { return nil }
func (s *Steps) Selectable() bool { return true }

// Step in view, nil before the first
func (s *Steps) Step() wm.Card {
	if len(s.steps) == 0 {
		return nil
	}
	return s.steps[len(s.steps)-1]
}

// Steps shown so far, the one in view the last
func (s *Steps) Len() int { return len(s.steps) }

// Shows a step after the one in view, which waits as it was left until the person comes back to it
func (s *Steps) Next(step wm.Card) tea.Cmd {
	var cmds []tea.Cmd
	if before := s.Step(); before != nil {
		cmds = append(cmds, s.send(before, wm.FocusMsg{}))
	}
	s.steps = append(s.steps, step)
	return tea.Batch(append(cmds, step.Init(), s.send(step, wm.FocusMsg{Focused: true}))...)
}

// The step in view leaves for good and the one before comes back, false on the first step, which leaves with the steps themselves
func (s *Steps) back() (tea.Cmd, bool) {
	if len(s.steps) < 2 {
		return nil, false
	}
	gone := s.Step()
	s.steps = s.steps[:len(s.steps)-1]
	return tea.Batch(s.send(gone, wm.FocusMsg{}), closed(gone), s.send(s.Step(), wm.FocusMsg{Focused: true})), true
}

// Message handed to a step, keeping the card it answers with in its place
func (s *Steps) send(step wm.Card, msg tea.Msg) tea.Cmd {
	next, cmd := step.Update(msg)
	if i := slices.Index(s.steps, step); i >= 0 && next != nil {
		s.steps[i] = next
	}
	return cmd
}

func (s *Steps) Update(msg tea.Msg) (wm.Card, tea.Cmd) {
	if addressed, ok := msg.(wm.Addressed); ok {
		if i := slices.Index(s.steps, addressed.Addressee()); i >= 0 {
			return s, s.send(s.steps[i], msg)
		}
	}
	if step := s.Step(); step != nil {
		return s, s.send(step, msg)
	}
	return s, nil
}

// Tells every step that the job left its hub
func (s *Steps) Closed() tea.Cmd {
	var cmds []tea.Cmd
	for _, step := range s.steps {
		cmds = append(cmds, closed(step))
	}
	return tea.Batch(cmds...)
}

func (s *Steps) View(w, h int) string {
	if step := s.Step(); step != nil {
		return step.View(w, h)
	}
	return ""
}

func (s *Steps) Contexts() []string {
	if c, ok := s.Step().(wm.Contexter); ok {
		return c.Contexts()
	}
	return nil
}

func (s *Steps) Hints() []wm.Hint {
	if c, ok := s.Step().(wm.Hinter); ok {
		return c.Hints()
	}
	return nil
}

func (s *Steps) Status() string {
	if c, ok := s.Step().(wm.Statuser); ok {
		return c.Status()
	}
	return ""
}

func (s *Steps) Cursor() *tea.Cursor {
	if c, ok := s.Step().(wm.Cursorer); ok {
		return c.Cursor()
	}
	return nil
}

func (s *Steps) Raw() bool {
	r, ok := s.Step().(wm.Raw)
	return ok && r.Raw()
}

func (s *Steps) Answers(action string) bool {
	a, ok := s.Step().(wm.Answerer)
	return !ok || a.Answers(action)
}

func (s *Steps) Grabs(x, y int) bool {
	g, ok := s.Step().(wm.Grabber)
	return ok && g.Grabs(x, y)
}

// Rows the tallest step fills, so the hub holds still from step to step
func (s *Steps) Rows(w int) int {
	n := 0
	for _, step := range s.steps {
		if f, ok := step.(Fitted); ok {
			n = max(n, f.Rows(w))
		}
	}
	return n
}
