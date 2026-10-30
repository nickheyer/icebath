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
	h      int
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
	d.h = h
	t := d.env.Theme
	if len(d.rows) == 0 {
		return emptyView(w, h, "no agents", t)
	}
	now := time.Now()
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

// Live transcript of one child run under a header with its task, state and elapsed time
type Agent struct {
	env   *Env
	run   *harness.Run
	tr    *Transcript
	tick  int
	timer ticker
	h     int
}

// Builds the card for a child run with what it said so far, thinking included when the session shows it
func NewAgent(env *Env, run *harness.Run, thinking bool) *Agent {
	root := run.Parent
	if root == "" {
		root = run.ID
	}
	tr := NewChildTranscript(env, root, run.Name)
	tr.ShowThinking(thinking)
	if run.Session != nil {
		tr.Load(run.Session.Entries(), run.Session.Records())
	}
	return &Agent{env: env, run: run, tr: tr, timer: ticker{every: time.Second}}
}

// Run the card follows
func (a *Agent) Run() *harness.Run { return a.run }

// Loads the transcript again with or without thinking when the session it serves changed its mind
func (a *Agent) Rethink(on bool) {
	if on == a.tr.Thinking() {
		return
	}
	a.tr.ShowThinking(on)
	if a.run.Session != nil {
		a.tr.Load(a.run.Session.Entries(), a.run.Session.Records())
	}
}

// Id of the session the run serves, through the root of a child run
func (a *Agent) session() string {
	r := a.run
	for r != nil && r.Parent != "" && a.env.Control != nil {
		parent, ok := a.env.Control.Get(r.Parent)
		if !ok {
			break
		}
		r = parent
	}
	if r == nil || r.Session == nil {
		return ""
	}
	return r.Session.ID()
}

// Glides the transcript between frames
func (a *Agent) Animate(dt float64) bool { return a.tr.Animate(dt) }

func (a *Agent) Title() string       { return a.run.Name }
func (a *Agent) Glyph() string       { return a.env.Theme.Glyphs.Agent }
func (a *Agent) Key() string         { return "agent:" + a.run.ID }
func (a *Agent) Selectable() bool    { return true }
func (a *Agent) MinSize() (int, int) { return 24, 4 }

// Restarts the header clock and the transcript repaint loop, either of which may have missed a tick while closed
func (a *Agent) Init() tea.Cmd {
	a.timer.reset()
	return tea.Batch(a.schedule(), a.tr.Init())
}

func (a *Agent) schedule() tea.Cmd {
	if !a.run.Running() {
		return nil
	}
	return a.timer.start()
}

// Keymap contexts in force, the search bar while it takes typing
func (a *Agent) Contexts() []string {
	if a.tr.Searching() {
		return []string{keys.Search}
	}
	return []string{keys.Transcript, keys.Panel}
}

func (a *Agent) Update(msg tea.Msg) (wm.Card, tea.Cmd) {
	switch msg := msg.(type) {
	case tickMsg:
		if !a.timer.owns(msg) {
			_, cmd := a.tr.Update(msg)
			return a, cmd
		}
		a.tick++
		return a, a.schedule()
	case harness.EventMsg:
		_, cmd := a.tr.Update(msg)
		return a, tea.Batch(cmd, a.schedule())
	case wm.ActionMsg:
		return a, a.act(msg.ID)
	case tea.KeyPressMsg:
		if a.tr.Searching() && msg.Text != "" {
			a.tr.SearchType(msg.Text)
		}
		return a, nil
	case tea.PasteMsg:
		if a.tr.Searching() {
			a.tr.SearchType(msg.Content)
		}
		return a, nil
	case tea.MouseClickMsg, tea.MouseWheelMsg, tea.MouseReleaseMsg:
		mo := msg.(tea.MouseMsg).Mouse()
		if mo.Y < 1 {
			return a, nil
		}
		if _, click := msg.(tea.MouseClickMsg); click && a.tr.SearchShown() && mo.Y == a.h-1 {
			a.tr.Search()
			return a, nil
		}
		mo.Y--
		var shifted tea.Msg = tea.MouseWheelMsg(mo)
		switch msg.(type) {
		case tea.MouseClickMsg:
			shifted = tea.MouseClickMsg(mo)
		case tea.MouseReleaseMsg:
			shifted = tea.MouseReleaseMsg(mo)
		}
		_, cmd := a.tr.Update(shifted)
		return a, cmd
	}
	_, cmd := a.tr.Update(msg)
	return a, cmd
}

// Carries out a keymap action on the transcript or its search bar, the thinking toggle going to its session
func (a *Agent) act(id string) tea.Cmd {
	tr := a.tr
	if strings.HasPrefix(id, "search.") {
		tr.SearchAction(id)
		return nil
	}
	switch id {
	case "transcript.down":
		tr.ScrollBy(1)
	case "transcript.up":
		tr.ScrollBy(-1)
	case "transcript.page.down":
		tr.Page(1)
	case "transcript.page.up":
		tr.Page(-1)
	case "transcript.top":
		tr.Top()
	case "transcript.bottom":
		tr.Follow()
	case "transcript.search":
		tr.Search()
	case "transcript.next":
		tr.NextMatch(1)
	case "transcript.prev":
		tr.NextMatch(-1)
	case "transcript.turn.prev":
		tr.JumpTurn(-1)
	case "transcript.turn.next":
		tr.JumpTurn(1)
	case "transcript.block.next":
		tr.MoveCursor(1)
	case "transcript.block.prev":
		tr.MoveCursor(-1)
	case "transcript.toggle":
		return tr.Toggle()
	case "transcript.expand":
		tr.ExpandAll(true)
	case "transcript.collapse":
		tr.ExpandAll(false)
	case "transcript.thinking":
		session := a.session()
		if session == "" {
			return wm.Notify("that session is closed", wm.Warn)
		}
		return func() tea.Msg { return ThinkingMsg{Session: session} }
	case "transcript.follow":
		tr.ToggleFollow()
	}
	return nil
}

// Header line naming the task and the state of the run
func (a *Agent) header(w int) string {
	t := a.env.Theme
	r := a.run
	var state string
	switch {
	case r.Running():
		last, n := r.LastTool()
		state = render.Cool(render.Spinner(a.tick+a.tr.tick, t)+" "+render.Elapsed(r.Elapsed(time.Now())), t)
		if n > 0 {
			state += " " + render.Dim(itoa(n)+" tools", t)
		}
		if last != "" {
			state += " " + render.Dim(render.ToolGlyph(last, t)+" "+last, t)
		}
	case r.Err() != nil:
		state = render.Bad("failed "+render.Truncate(r.Err().Error(), w/2, t), t)
	default:
		state = render.Good("done", t) + " " + render.Dim(render.Elapsed(r.Elapsed(time.Now())), t)
	}
	desc := render.Dim(r.Description, t)
	line := state
	if r.Description != "" {
		line = desc + lipgloss.NewStyle().Foreground(t.Steel).Render(" "+t.Glyphs.Sep+" ") + state
	}
	return render.Truncate(line, w, t)
}

func (a *Agent) View(w, h int) string {
	a.h = h
	if h <= 1 {
		return a.header(w)
	}
	if a.tr.SearchShown() && h > 2 {
		return a.header(w) + "\n" + a.tr.View(w, h-2) + "\n" + a.tr.SearchView(w, "search")
	}
	return a.header(w) + "\n" + a.tr.View(w, h-1)
}

// Cursor in the search bar while it takes typing
func (a *Agent) Cursor() *tea.Cursor {
	if !a.tr.Searching() {
		return nil
	}
	return tea.NewCursor(a.tr.SearchCursor(), a.h-1)
}

func (a *Agent) Status() string {
	r := a.run
	switch {
	case r.Running():
		return "running"
	case r.Err() != nil:
		return "failed"
	}
	return "done"
}

// Actions the agent transcript answers
func (a *Agent) Hints() []wm.Hint {
	if a.tr.Searching() {
		return []wm.Hint{{Action: "search.accept"}, {Action: "search.next", Pair: "search.prev", Label: "matches"}, {Action: "search.close"}}
	}
	return []wm.Hint{{Action: "transcript.down", Pair: "transcript.up", Label: "scroll"}, {Action: "transcript.toggle"}, {Action: "transcript.search"}, {Action: "transcript.follow"}}
}
