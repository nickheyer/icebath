package cards

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/nickheyer/icebath/pkg/facts"
	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/mission"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// One mission of a session with its objective, plan, agents, findings and cost
type Mission struct {
	env     *Env
	store   *mission.Store
	facts   *facts.Store
	session string
	id      string
	m       mission.Mission
	w       int
	h       int
}

// Builds the mission card over the stores of the session the mission belongs to
func NewMission(env *Env, store *mission.Store, facts *facts.Store, sessionID string, m mission.Mission) *Mission {
	return &Mission{env: env, store: store, facts: facts, session: sessionID, id: m.ID, m: m}
}

func (c *Mission) Title() string { return "MISSION " + c.id }
func (c *Mission) Glyph() string { return c.env.Theme.Glyphs.Mission }
func (c *Mission) Key() string   { return "mission:" + c.session + ":" + c.id }
func (c *Mission) Init() tea.Cmd { return nil }
func (c *Mission) ID() string    { return c.id }

func (c *Mission) Contexts() []string { return []string{keys.Mission, keys.Panel} }

// Mission as last reported
func (c *Mission) Mission() mission.Mission { return c.m }

func (c *Mission) Update(msg tea.Msg) (wm.Card, tea.Cmd) {
	switch msg := msg.(type) {
	case harness.MissionMsg:
		if msg.Session == c.session && msg.Mission.ID == c.id {
			c.m = msg.Mission
		}
	case harness.FactMsg, harness.EventMsg, harness.AttemptMsg:
		return c, nil
	case wm.ActionMsg:
		switch msg.ID {
		case "mission.done":
			if !c.m.Ended() {
				return c, c.state(mission.StatusDone)
			}
		case "mission.failed":
			if !c.m.Ended() {
				return c, c.state(mission.StatusFailed)
			}
		case "mission.resume":
			if c.m.Ended() {
				return c, c.state(mission.StatusActive)
			}
		case "mission.forget":
			if c.m.Ended() {
				id, sess := c.id, c.session
				return c, func() tea.Msg { return MissionRemoveMsg{Session: sess, ID: id} }
			}
		}
	}
	return c, nil
}

func (c *Mission) state(status string) tea.Cmd {
	id, sess := c.id, c.session
	return func() tea.Msg { return MissionStateMsg{Session: sess, ID: id, Status: status} }
}

// Findings recorded by the agents of this mission
func (c *Mission) findings() int {
	if c.facts == nil {
		return 0
	}
	if len(c.m.Agents) == 0 {
		return c.facts.Len()
	}
	n := 0
	for _, a := range c.m.Agents {
		n += len(c.facts.List(facts.Query{Agent: a}))
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
	lines = append(lines, render.Heading(render.Truncate(m.Objective, w-10, t), t)+" "+status)
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
	lines = append(lines, render.Truncate(strings.Join(meta, "  "), w, t))
	if m.Note != "" {
		lines = append(lines, render.Dim(render.Truncate(m.Note, w, t), t))
	}
	lines = append(lines, "")
	if len(m.Plan) == 0 {
		lines = append(lines, render.Dim("no plan", t))
	}
	for i, s := range m.Plan {
		mark, st := t.Glyphs.Todo, lipgloss.NewStyle().Foreground(t.Text)
		switch s.Status {
		case mission.StepDone:
			mark, st = t.Glyphs.Done, lipgloss.NewStyle().Foreground(t.TextDim)
		case mission.StepActive:
			mark, st = t.Glyphs.Active, lipgloss.NewStyle().Foreground(t.Ice).Bold(true)
		case mission.StepSkipped:
			mark, st = t.Glyphs.Skip, lipgloss.NewStyle().Foreground(t.TextDim).Strikethrough(true)
		}
		lines = append(lines, render.Truncate(st.Render(fmt.Sprintf("%s %d. %s", mark, i+1, s.Text)), w, t))
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

// Actions the mission answers in its current state
func (c *Mission) Hints() []wm.Hint {
	if c.m.Ended() {
		return []wm.Hint{{Action: "mission.resume"}, {Action: "mission.forget"}}
	}
	return []wm.Hint{{Action: "mission.done"}, {Action: "mission.failed"}}
}
