package cards

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/tool/shell"
	"github.com/nickheyer/icebath/pkg/ui/deck"
	"github.com/nickheyer/icebath/pkg/ui/render"
)

// Bytes of job output kept per job for the tail
const tailBytes = 256 << 10

// Background jobs with a live tail of the selected one
type Jobs struct {
	env    *Env
	jobs   []shell.JobInfo
	sel    int
	tails  map[string]*tail
	vp     viewport.Model
	follow bool
	w, h   int
}

type tail struct {
	buf  []byte
	next int64
}

var jKill = key.NewBinding(key.WithKeys("K"), key.WithHelp("K", "kill"))

func NewJobs(env *Env) *Jobs {
	vp := viewport.New()
	vp.MouseWheelEnabled = true
	j := &Jobs{env: env, tails: map[string]*tail{}, vp: vp, follow: true}
	j.refresh()
	return j
}

func (j *Jobs) Title() string        { return "JOBS" }
func (j *Jobs) Glyph() string        { return "⇣" }
func (j *Jobs) Key() string          { return "jobs" }
func (j *Jobs) Init() tea.Cmd        { return nil }
func (j *Jobs) Selectable() bool     { return true }
func (j *Jobs) Hints() []key.Binding { return []key.Binding{moveHint, jKill, tFollow} }
func (j *Jobs) Claims() []string     { return []string{"j", "k", "J", "K", "g", "G", "f"} }

// Jobs known, oldest first
func (j *Jobs) List() []shell.JobInfo { return j.jobs }

func (j *Jobs) refresh() {
	if j.env.Shell != nil {
		j.jobs = j.env.Shell.Jobs()
	}
	if j.sel >= len(j.jobs) {
		j.sel = max(0, len(j.jobs)-1)
	}
	j.pull()
}

// Reads new output of the selected job into its tail without moving the cursor the model reads with
func (j *Jobs) pull() {
	if j.env.Shell == nil || j.sel >= len(j.jobs) {
		return
	}
	id := j.jobs[j.sel].ID
	tl, ok := j.tails[id]
	if !ok {
		tl = &tail{}
		j.tails[id] = tl
	}
	data, next, _, found := j.env.Shell.Tail(id, tl.next)
	if !found {
		return
	}
	tl.next = next
	tl.buf = append(tl.buf, data...)
	if len(tl.buf) > tailBytes {
		tl.buf = append([]byte(nil), tl.buf[len(tl.buf)-tailBytes:]...)
	}
}

func (j *Jobs) Update(msg tea.Msg) (deck.Card, tea.Cmd) {
	switch msg := msg.(type) {
	case harness.JobMsg:
		j.refresh()
		if msg.Event.Kind == shell.JobStarted {
			for i, jb := range j.jobs {
				if jb.ID == msg.Event.Job.ID {
					j.sel = i
				}
			}
			j.pull()
			return j, deck.RequestAttention(j, deck.Pulse)
		}
		return j, nil
	case tea.KeyPressMsg:
		switch msg.String() {
		case "j", "down":
			j.sel = min(j.sel+1, max(0, len(j.jobs)-1))
			j.pull()
		case "k", "up":
			j.sel = max(0, j.sel-1)
			j.pull()
		case "g":
			j.vp.GotoTop()
			j.follow = false
		case "G":
			j.vp.GotoBottom()
			j.follow = true
		case "J":
			j.vp.ScrollDown(1)
			j.follow = j.vp.AtBottom()
		case "K":
			return j, j.kill()
		case "f":
			j.follow = !j.follow
		}
	case tea.MouseWheelMsg:
		var cmd tea.Cmd
		j.vp, cmd = j.vp.Update(msg)
		j.follow = j.vp.AtBottom()
		return j, cmd
	case tea.MouseClickMsg:
		if msg.Button == tea.MouseLeft && msg.Y >= 1 && msg.Y-1 < len(j.jobs) {
			j.sel = msg.Y - 1
			j.pull()
		}
	}
	return j, nil
}

func (j *Jobs) kill() tea.Cmd {
	if j.sel >= len(j.jobs) {
		return nil
	}
	jb := j.jobs[j.sel]
	if !jb.Running {
		return deck.Notify("job already finished", deck.Info)
	}
	return func() tea.Msg { return KillJobMsg{Job: jb.ID} }
}

func (j *Jobs) View(w, h int) string {
	j.w, j.h = w, h
	t := j.env.Theme
	if len(j.jobs) == 0 {
		return render.Dim("no background jobs", t)
	}
	now := j.env.now()
	cols := []render.Column{{Title: "JOB"}, {Title: "COMMAND", Flex: 1}, {Title: "STATE"}, {Title: "TIME", Right: true}, {Title: "OUT", Right: true}}
	rows := make([][]string, 0, len(j.jobs))
	for _, jb := range j.jobs {
		state := "running"
		elapsed := now.Sub(jb.Started)
		if !jb.Running {
			elapsed = jb.Finished.Sub(jb.Started)
			switch {
			case jb.Error != "":
				state = render.Bad("failed", t)
			case jb.TimedOut:
				state = render.Warm("timeout", t)
			case jb.ExitCode != 0:
				state = render.Bad(fmt.Sprintf("exit %d", jb.ExitCode), t)
			default:
				state = render.Good("done", t)
			}
		}
		label := jb.Description
		if label == "" {
			label = jb.Line
		}
		rows = append(rows, []string{jb.ID, label, state, render.Elapsed(elapsed), render.Bytes(jb.Bytes)})
	}
	listH := min(len(j.jobs)+1, max(h/3, 2))
	table := strings.Split(render.Table(cols, rows, w, j.sel, t), "\n")
	if len(table) > listH {
		start := 0
		if j.sel+1 >= listH {
			start = j.sel + 2 - listH
		}
		table = append(table[:1], table[1+start:min(len(table), 1+start+listH-1)]...)
	}
	body := h - len(table) - 1
	if body < 1 {
		return strings.Join(table, "\n")
	}
	j.vp.SetWidth(w)
	j.vp.SetHeight(body)
	text := ""
	if j.sel < len(j.jobs) {
		if tl, ok := j.tails[j.jobs[j.sel].ID]; ok {
			text = string(tl.buf)
		}
	}
	j.vp.SetContent(strings.TrimRight(text, "\n"))
	if j.follow {
		j.vp.GotoBottom()
	}
	sep := render.Dim(strings.Repeat("┄", min(w, 40)), t)
	return strings.Join(append(append(table, sep), j.vp.View()), "\n")
}

func (j *Jobs) Status() string {
	n := 0
	for _, jb := range j.jobs {
		if jb.Running {
			n++
		}
	}
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("%d running", n)
}
