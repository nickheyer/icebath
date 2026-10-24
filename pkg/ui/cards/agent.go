package cards

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

type agentTick struct{}

// Live transcript of one child run under a header with its task, state and elapsed time
type Agent struct {
	env     *Env
	run     *harness.Run
	tr      *Transcript
	tick    int
	ticking bool
	w, h    int
}

// Builds the card for a child run, loading what it has said so far
func NewAgent(env *Env, run *harness.Run) *Agent {
	root := run.Parent
	if root == "" {
		root = run.ID
	}
	tr := NewChildTranscript(env, root, run.Name)
	if run.Session != nil {
		tr.Load(run.Session.Entries(), run.Session.Records())
	}
	return &Agent{env: env, run: run, tr: tr}
}

// Run the card follows
func (a *Agent) Run() *harness.Run { return a.run }

// Glides the transcript between frames
func (a *Agent) Animate(dt float64) bool { return a.tr.Animate(dt) }

func (a *Agent) Title() string       { return a.run.Name }
func (a *Agent) Glyph() string       { return a.env.Theme.Glyphs.Agent }
func (a *Agent) Key() string         { return "agent:" + a.run.ID }
func (a *Agent) Selectable() bool    { return true }
func (a *Agent) MinSize() (int, int) { return 24, 4 }

func (a *Agent) Init() tea.Cmd { return a.schedule() }

func (a *Agent) schedule() tea.Cmd {
	if a.ticking || !a.run.Running() {
		return nil
	}
	a.ticking = true
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return agentTick{} })
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
	case agentTick:
		a.ticking = false
		a.tick++
		return a, a.schedule()
	case harness.EventMsg:
		_, cmd := a.tr.Update(msg)
		return a, tea.Batch(cmd, a.schedule())
	case wm.ActionMsg:
		a.act(msg.ID)
		return a, nil
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
	case tea.MouseClickMsg, tea.MouseWheelMsg:
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
		if _, click := msg.(tea.MouseClickMsg); click {
			shifted = tea.MouseClickMsg(mo)
		}
		_, cmd := a.tr.Update(shifted)
		return a, cmd
	}
	_, cmd := a.tr.Update(msg)
	return a, cmd
}

// Carries out a keymap action on the transcript or its search bar
func (a *Agent) act(id string) {
	tr := a.tr
	if strings.HasPrefix(id, "search.") {
		tr.SearchAction(id)
		return
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
		tr.Toggle()
	case "transcript.expand":
		tr.ExpandAll(true)
	case "transcript.collapse":
		tr.ExpandAll(false)
	case "transcript.thinking":
		tr.ShowThinking(!tr.Thinking())
	case "transcript.follow":
		tr.ToggleFollow()
	}
}

// Header line naming the task and the state of the run
func (a *Agent) header(w int) string {
	t := a.env.Theme
	r := a.run
	var state string
	switch {
	case r.Running():
		last, n := r.LastTool()
		state = render.Cool(render.Spinner(a.tick+a.tr.tick, t)+" "+render.Elapsed(r.Elapsed(a.env.now())), t)
		if n > 0 {
			state += " " + render.Dim(itoa(n)+" tools", t)
		}
		if last != "" {
			state += " " + render.Dim(render.ToolGlyph(last, t)+" "+last, t)
		}
	case r.Err() != nil:
		state = render.Bad("failed "+render.Truncate(r.Err().Error(), w/2, t), t)
	default:
		state = render.Good("done", t) + " " + render.Dim(render.Elapsed(r.Elapsed(a.env.now())), t)
	}
	desc := render.Dim(r.Description, t)
	line := state
	if r.Description != "" {
		line = desc + lipgloss.NewStyle().Foreground(t.Steel).Render(" "+t.Glyphs.Sep+" ") + state
	}
	return render.Truncate(line, w, t)
}

func (a *Agent) View(w, h int) string {
	a.w, a.h = w, h
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
