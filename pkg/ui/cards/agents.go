package cards

import (
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Roster of every agent across the open sessions with state, time, tokens and tools called, each openable or killable
type Agents struct {
	env    *Env
	rows   []*harness.Run
	sel    int
	tokens map[string]int
	start  int
	w, h   int
	timer  ticker
}

func NewAgents(env *Env) *Agents {
	d := &Agents{env: env, tokens: map[string]int{}, timer: ticker{every: time.Second}}
	d.refresh()
	return d
}

func (d *Agents) Title() string { return "AGENTS" }
func (d *Agents) Glyph() string { return d.env.Theme.Glyphs.Agent }
func (d *Agents) Key() string   { return "agents" }

// Restarts the refresh loop the card may have missed a tick of while closed
func (d *Agents) Init() tea.Cmd {
	d.timer.reset()
	return d.tick()
}

func (d *Agents) Contexts() []string { return []string{keys.Agents, keys.List, keys.Panel} }

// Schedules the next refresh while any run goes and none is pending
func (d *Agents) tick() tea.Cmd {
	if !d.running() {
		return nil
	}
	return d.timer.start()
}

func (d *Agents) running() bool {
	for _, r := range d.rows {
		if r.Running() {
			return true
		}
	}
	return false
}

// Rebuilds the rows from the controller, running roots first and children under their parents
func (d *Agents) refresh() {
	d.rows = d.rows[:0]
	if d.env.Control == nil {
		return
	}
	var roots []*harness.Run
	for _, r := range d.env.Control.Runs() {
		if r.Parent == "" {
			roots = append(roots, r)
		}
	}
	sort.SliceStable(roots, func(i, j int) bool {
		if roots[i].Running() != roots[j].Running() {
			return roots[i].Running()
		}
		return roots[i].Started.After(roots[j].Started)
	})
	var add func(r *harness.Run)
	add = func(r *harness.Run) {
		d.rows = append(d.rows, r)
		for _, child := range d.env.Control.Children(r.ID) {
			add(child)
		}
	}
	for _, r := range roots {
		add(r)
	}
	if d.sel >= len(d.rows) {
		d.sel = max(0, len(d.rows)-1)
	}
}

// Runs listed, in display order
func (d *Agents) Rows() []*harness.Run { return append([]*harness.Run(nil), d.rows...) }

func (d *Agents) Update(msg tea.Msg) (wm.Card, tea.Cmd) {
	switch msg := msg.(type) {
	case harness.EventMsg:
		switch msg.Event.Type {
		case agent.EventTurnEnd:
			d.tokens[msg.Run+"/"+msg.Event.Agent] += msg.Event.Usage.Output + msg.Event.Usage.Input
		case agent.EventRunStart, agent.EventRunEnd, agent.EventSubagentStart, agent.EventSubagentEnd, agent.EventToolStart:
			d.refresh()
		}
		return d, d.tick()
	case harness.RunDoneMsg:
		d.refresh()
		return d, d.tick()
	case tickMsg:
		if !d.timer.owns(msg) {
			return d, nil
		}
		d.refresh()
		return d, d.tick()
	case wm.ActionMsg:
		switch msg.ID {
		case "agents.open":
			return d, d.open()
		case "agents.stop":
			return d, d.kill()
		}
		d.sel = listMove(msg.ID, d.sel, len(d.rows), d.h-render.TableHead)
	case tea.MouseClickMsg:
		row := msg.Y - render.TableHead + d.start
		if msg.Button == tea.MouseLeft && msg.Y >= render.TableHead && row < len(d.rows) {
			if row == d.sel {
				return d, d.open()
			}
			d.sel = row
		}
	case tea.MouseWheelMsg:
		switch msg.Button {
		case tea.MouseWheelDown:
			d.sel = min(d.sel+1, max(0, len(d.rows)-1))
		case tea.MouseWheelUp:
			d.sel = max(0, d.sel-1)
		}
	}
	return d, nil
}

func (d *Agents) open() tea.Cmd {
	if d.sel >= len(d.rows) {
		return nil
	}
	id := d.rows[d.sel].ID
	return func() tea.Msg { return OpenRunMsg{Run: id} }
}

func (d *Agents) kill() tea.Cmd {
	if d.sel >= len(d.rows) {
		return nil
	}
	r := d.rows[d.sel]
	if !r.Running() {
		return wm.Notify("already finished", wm.Info)
	}
	id := r.ID
	return func() tea.Msg { return KillRunMsg{Run: id} }
}

// Title of the session a run serves, through its root
func (d *Agents) sessionOf(r *harness.Run) string {
	for r != nil && r.Parent != "" {
		parent, ok := d.env.Control.Get(r.Parent)
		if !ok {
			break
		}
		r = parent
	}
	if r == nil || r.Session == nil {
		return ""
	}
	if t := r.Session.Title(); t != "" {
		return t
	}
	return r.Session.ID()
}

func (d *Agents) View(w, h int) string {
	d.w, d.h = w, h
	t := d.env.Theme
	if len(d.rows) == 0 {
		return emptyView(w, h, "no agents", t)
	}
	now := d.env.now()
	cols := []render.Column{{Title: ""}, {Title: "NAME", Flex: 2}, {Title: "SESSION", Flex: 2}, {Title: "STATE"}, {Title: "TIME", Right: true}, {Title: "TOKENS", Right: true}, {Title: "TOOLS", Flex: 1}}
	rows := make([][]string, 0, len(d.rows))
	for _, r := range d.rows {
		glyph, state := d.runState(r)
		name := strings.Repeat("  ", r.Depth) + r.Name
		if r.Parent != "" && r.Description != "" {
			name += " " + render.Dim(render.Truncate(r.Description, 20, t), t)
		}
		last, calls := r.LastTool()
		tools := itoa(calls)
		if last != "" {
			tools += " " + last
		}
		rows = append(rows, []string{glyph, name, render.Truncate(d.sessionOf(r), 24, t), state, render.Elapsed(r.Elapsed(now)), render.Tokens(d.tokens[d.tokenKey(r)]), tools})
	}
	table, start := window(strings.Split(render.Table(cols, rows, w, d.sel, t), "\n"), d.sel, h)
	d.start = start
	return strings.Join(table, "\n")
}

func (d *Agents) tokenKey(r *harness.Run) string {
	root := r.ID
	if r.Parent != "" {
		root = r.Parent
	}
	return root + "/" + r.Name
}

func (d *Agents) runState(r *harness.Run) (string, string) {
	t := d.env.Theme
	switch {
	case r.Running():
		return lipgloss.NewStyle().Foreground(t.Alive).Render(t.Glyphs.Live), "running"
	case r.Err() != nil:
		return lipgloss.NewStyle().Foreground(t.Alarm).Render(t.Glyphs.Live), "failed"
	}
	return lipgloss.NewStyle().Foreground(t.TextDim).Render(t.Glyphs.Hollow), "done"
}

func (d *Agents) Status() string {
	n := 0
	for _, r := range d.rows {
		if r.Running() {
			n++
		}
	}
	if n == 0 {
		return "idle"
	}
	return itoa(n) + " running"
}

// Actions the roster answers
func (d *Agents) Hints() []wm.Hint {
	return []wm.Hint{{Action: "agents.open"}, {Action: "agents.stop"}, {Action: "list.down", Pair: "list.up", Label: "move"}}
}
