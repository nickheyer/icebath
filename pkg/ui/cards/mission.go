package cards

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/nickheyer/icebath/pkg/facts"
	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/mission"
	"github.com/nickheyer/icebath/pkg/ui/deck"
	"github.com/nickheyer/icebath/pkg/ui/render"
)

// One mission with its objective, plan, agents, findings and cost
type Mission struct {
	env *Env
	id  string
	m   mission.Mission
	w   int
	h   int
}

var (
	doneHint   = key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "done"))
	failHint   = key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "fail"))
	resumeHint = key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "resume"))
	forgetHint = key.NewBinding(key.WithKeys("D"), key.WithHelp("D", "forget"))
)

func NewMission(env *Env, m mission.Mission) *Mission {
	return &Mission{env: env, id: m.ID, m: m}
}

func (c *Mission) Title() string { return "PROBE " + c.id }
func (c *Mission) Glyph() string { return "◎" }
func (c *Mission) Key() string   { return "mission:" + c.id }
func (c *Mission) Init() tea.Cmd { return nil }
func (c *Mission) ID() string    { return c.id }

// Mission as last reported
func (c *Mission) Mission() mission.Mission { return c.m }

func (c *Mission) Hints() []key.Binding {
	if c.m.Ended() {
		return []key.Binding{resumeHint, forgetHint}
	}
	return []key.Binding{doneHint, failHint}
}

// Keys taken ahead of the deck, matching the hints for the current state
func (c *Mission) Claims() []string {
	if c.m.Ended() {
		return []string{"r", "D"}
	}
	return []string{"d", "x"}
}

func (c *Mission) Update(msg tea.Msg) (deck.Card, tea.Cmd) {
	switch msg := msg.(type) {
	case harness.MissionMsg:
		if msg.Mission.ID == c.id {
			c.m = msg.Mission
		}
	case harness.FactMsg, harness.EventMsg, harness.AttemptMsg:
		return c, nil
	case tea.KeyPressMsg:
		switch msg.String() {
		case "d":
			if !c.m.Ended() {
				return c, c.state(mission.StatusDone)
			}
		case "x":
			if !c.m.Ended() {
				return c, c.state(mission.StatusFailed)
			}
		case "r":
			if c.m.Ended() {
				return c, c.state(mission.StatusActive)
			}
		case "D":
			if c.m.Ended() {
				id := c.id
				return c, func() tea.Msg { return MissionRemoveMsg{ID: id} }
			}
		}
	}
	return c, nil
}

func (c *Mission) state(status string) tea.Cmd {
	id := c.id
	return func() tea.Msg { return MissionStateMsg{ID: id, Status: status} }
}

// Findings recorded by the agents of this mission
func (c *Mission) findings() int {
	if c.env.Facts == nil {
		return 0
	}
	if len(c.m.Agents) == 0 {
		return c.env.Facts.Len()
	}
	n := 0
	for _, a := range c.m.Agents {
		n += len(c.env.Facts.List(facts.Query{Agent: a}))
	}
	return n
}

// Dollars spent by runs assigned to this mission
func (c *Mission) cost() float64 {
	if c.env.Meter == nil || c.env.Control == nil {
		return 0
	}
	snap := c.env.Meter.Snapshot()
	total := 0.0
	for _, r := range c.env.Control.Runs() {
		if r.Mission == c.id && r.Parent == "" {
			total += snap.Costs[r.ID]
		}
	}
	return total
}

func (c *Mission) View(w, h int) string {
	c.w, c.h = w, h
	t := c.env.Theme
	m := c.m
	var lines []string
	status := render.Cool(m.Status, t)
	switch m.Status {
	case mission.StatusDone:
		status = render.Good(m.Status, t)
	case mission.StatusFailed, mission.StatusCancelled:
		status = render.Bad(m.Status, t)
	}
	lines = append(lines, render.Heading(render.Truncate(m.Objective, w-10), t)+" "+status)
	bar := max(w-6, 4)
	lines = append(lines, render.Bar(m.Progress(), bar, t)+" "+render.Warm(render.Percent(m.Progress()), t))
	if len(m.Agents) > 0 {
		lines = append(lines, render.KV("agents", strings.Join(m.Agents, ", "), t))
	}
	meta := []string{render.KV("findings", fmt.Sprint(c.findings()), t)}
	if cost := c.cost(); cost > 0 {
		meta = append(meta, render.KV("cost", render.Money(cost), t))
	}
	meta = append(meta, render.KV("since", m.Started.Local().Format("15:04"), t))
	if m.Error != "" {
		meta = append(meta, render.Bad(m.Error, t))
	}
	lines = append(lines, render.Truncate(strings.Join(meta, "  "), w))
	if m.Note != "" {
		lines = append(lines, render.Dim(render.Truncate(m.Note, w), t))
	}
	lines = append(lines, "")
	if len(m.Plan) == 0 {
		lines = append(lines, render.Dim("no plan yet", t))
	}
	for i, s := range m.Plan {
		mark, st := "☐", lipgloss.NewStyle().Foreground(t.Text)
		switch s.Status {
		case mission.StepDone:
			mark, st = "☑", lipgloss.NewStyle().Foreground(t.TextDim)
		case mission.StepActive:
			mark, st = "▶", lipgloss.NewStyle().Foreground(t.Ice).Bold(true)
		case mission.StepSkipped:
			mark, st = "⊘", lipgloss.NewStyle().Foreground(t.TextDim).Strikethrough(true)
		}
		lines = append(lines, render.Truncate(st.Render(fmt.Sprintf("%s %d. %s", mark, i+1, s.Text)), w))
	}
	if len(lines) > h {
		lines = lines[:h]
	}
	return strings.Join(lines, "\n")
}

func (c *Mission) Status() string {
	if c.m.Ended() {
		return c.m.Status
	}
	return render.Percent(c.m.Progress())
}
