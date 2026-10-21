package cards

import (
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/tool/shell"
	"github.com/nickheyer/icebath/pkg/ui/deck"
	"github.com/nickheyer/icebath/pkg/ui/render"
)

type daemonTick struct{}

// One row of the roster, a run or a background job
type daemon struct {
	run *harness.Run
	job *shell.JobInfo
}

// Roster of agent runs and shell jobs with state, time, tokens and the last tool, each one openable or killable
type Daemons struct {
	env     *Env
	rows    []daemon
	sel     int
	tokens  map[string]int
	w, h    int
	ticking bool
}

var (
	openHint = key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "open"))
	killHint = key.NewBinding(key.WithKeys("K"), key.WithHelp("K", "kill"))
	moveHint = key.NewBinding(key.WithKeys("j", "k"), key.WithHelp("j/k", "select"))
)

func NewDaemons(env *Env) *Daemons {
	d := &Daemons{env: env, tokens: map[string]int{}}
	d.refresh()
	return d
}

func (d *Daemons) Title() string        { return "DAEMONS" }
func (d *Daemons) Glyph() string        { return "☍" }
func (d *Daemons) Key() string          { return "daemons" }
func (d *Daemons) Hints() []key.Binding { return []key.Binding{moveHint, openHint, killHint} }
func (d *Daemons) Claims() []string     { return []string{"j", "k", "K", "enter", "g", "G"} }

func (d *Daemons) Init() tea.Cmd { return d.tick() }

func (d *Daemons) tick() tea.Cmd {
	if !d.running() {
		d.ticking = false
		return nil
	}
	d.ticking = true
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return daemonTick{} })
}

func (d *Daemons) running() bool {
	for _, r := range d.rows {
		if (r.run != nil && r.run.Running()) || (r.job != nil && r.job.Running) {
			return true
		}
	}
	return false
}

// Rebuilds the rows from the controller and the shell
func (d *Daemons) refresh() {
	d.rows = d.rows[:0]
	if d.env.Control != nil {
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
			return roots[i].Started.Before(roots[j].Started)
		})
		var add func(r *harness.Run)
		add = func(r *harness.Run) {
			d.rows = append(d.rows, daemon{run: r})
			for _, child := range d.env.Control.Children(r.ID) {
				add(child)
			}
		}
		for _, r := range roots {
			add(r)
		}
	}
	if d.env.Shell != nil {
		for _, j := range d.env.Shell.Jobs() {
			jb := j
			d.rows = append(d.rows, daemon{job: &jb})
		}
	}
	if d.sel >= len(d.rows) {
		d.sel = max(0, len(d.rows)-1)
	}
}

func (d *Daemons) Update(msg tea.Msg) (deck.Card, tea.Cmd) {
	switch msg := msg.(type) {
	case harness.EventMsg:
		switch msg.Event.Type {
		case agent.EventTurnEnd:
			d.tokens[msg.Run+"/"+msg.Event.Agent] += msg.Event.Usage.Output + msg.Event.Usage.Input
		case agent.EventRunStart, agent.EventRunEnd, agent.EventSubagentStart, agent.EventSubagentEnd, agent.EventToolStart:
			d.refresh()
		}
		if !d.ticking && d.running() {
			return d, d.tick()
		}
		return d, nil
	case harness.RunDoneMsg, harness.JobMsg:
		d.refresh()
		if !d.ticking && d.running() {
			return d, d.tick()
		}
		return d, nil
	case daemonTick:
		d.refresh()
		return d, d.tick()
	case tea.KeyPressMsg:
		switch msg.String() {
		case "j", "down":
			d.sel = min(d.sel+1, max(0, len(d.rows)-1))
		case "k", "up":
			d.sel = max(0, d.sel-1)
		case "g":
			d.sel = 0
		case "G":
			d.sel = max(0, len(d.rows)-1)
		case "enter":
			return d, d.open()
		case "K":
			return d, d.kill()
		}
	case tea.MouseClickMsg:
		if msg.Button == tea.MouseLeft && msg.Y >= 1 && msg.Y-1 < len(d.rows) {
			d.sel = msg.Y - 1
			return d, d.open()
		}
	}
	return d, nil
}

func (d *Daemons) open() tea.Cmd {
	if d.sel >= len(d.rows) {
		return nil
	}
	row := d.rows[d.sel]
	if row.run != nil {
		id := row.run.ID
		return func() tea.Msg { return OpenRunMsg{Run: id} }
	}
	return deck.Notify("open the JOBS card to follow job output", deck.Info)
}

func (d *Daemons) kill() tea.Cmd {
	if d.sel >= len(d.rows) {
		return nil
	}
	row := d.rows[d.sel]
	if row.run != nil {
		if !row.run.Running() {
			return deck.Notify("run already finished", deck.Info)
		}
		id := row.run.ID
		return func() tea.Msg { return KillRunMsg{Run: id} }
	}
	if !row.job.Running {
		return deck.Notify("job already finished", deck.Info)
	}
	id := row.job.ID
	return func() tea.Msg { return KillJobMsg{Job: id} }
}

func (d *Daemons) View(w, h int) string {
	d.w, d.h = w, h
	t := d.env.Theme
	if len(d.rows) == 0 {
		return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, render.Dim("no daemons", t))
	}
	now := d.env.now()
	cols := []render.Column{{Title: ""}, {Title: "NAME", Flex: 2}, {Title: "STATE"}, {Title: "TIME", Right: true}, {Title: "TOKENS", Right: true}, {Title: "LAST", Flex: 1}}
	rows := make([][]string, 0, len(d.rows))
	for _, row := range d.rows {
		if row.run != nil {
			r := row.run
			glyph, state := d.runState(r)
			name := strings.Repeat("  ", r.Depth) + r.Name
			if r.Parent != "" && r.Description != "" {
				name += " " + render.Dim(render.Truncate(r.Description, 20), t)
			}
			last, _ := r.LastTool()
			rows = append(rows, []string{glyph, name, state, render.Elapsed(r.Elapsed(now)), render.Tokens(d.tokens[d.tokenKey(r)]), last})
			continue
		}
		j := row.job
		glyph, state := lipgloss.NewStyle().Foreground(t.Alive).Render("●"), "running"
		elapsed := now.Sub(j.Started)
		if !j.Running {
			elapsed = j.Finished.Sub(j.Started)
			switch {
			case j.Error != "":
				glyph, state = lipgloss.NewStyle().Foreground(t.Alarm).Render("●"), "failed"
			case j.TimedOut:
				glyph, state = lipgloss.NewStyle().Foreground(t.Accent).Render("●"), "timeout"
			case j.ExitCode != 0:
				glyph, state = lipgloss.NewStyle().Foreground(t.Alarm).Render("●"), "exit "+itoa(j.ExitCode)
			default:
				glyph, state = lipgloss.NewStyle().Foreground(t.TextDim).Render("●"), "done"
			}
		}
		label := j.Description
		if label == "" {
			label = j.Line
		}
		rows = append(rows, []string{glyph, j.ID + " " + render.Dim(render.Truncate(label, 24), t), state, render.Elapsed(elapsed), render.Bytes(j.Bytes), ""})
	}
	return render.Table(cols, rows, w, d.sel, t)
}

func (d *Daemons) tokenKey(r *harness.Run) string {
	root := r.ID
	if r.Parent != "" {
		root = r.Parent
	}
	return root + "/" + r.Name
}

func (d *Daemons) runState(r *harness.Run) (string, string) {
	t := d.env.Theme
	switch {
	case r.Running():
		return lipgloss.NewStyle().Foreground(t.Alive).Render("●"), "running"
	case r.Err() != nil:
		return lipgloss.NewStyle().Foreground(t.Alarm).Render("●"), "failed"
	}
	return lipgloss.NewStyle().Foreground(t.TextDim).Render("●"), "done"
}

func (d *Daemons) Status() string {
	n := 0
	for _, r := range d.rows {
		if (r.run != nil && r.run.Running()) || (r.job != nil && r.job.Running) {
			n++
		}
	}
	if n == 0 {
		return "idle"
	}
	return itoa(n) + " run"
}
