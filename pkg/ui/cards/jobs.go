package cards

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/tool/shell"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
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
	start  int
	listH  int
	w, h   int
}

type tail struct {
	buf  []byte
	next int64
}

func NewJobs(env *Env) *Jobs {
	vp := viewport.New()
	vp.MouseWheelEnabled = true
	j := &Jobs{env: env, tails: map[string]*tail{}, vp: vp, follow: true}
	j.refresh()
	return j
}

func (j *Jobs) Title() string    { return "JOBS" }
func (j *Jobs) Glyph() string    { return j.env.Theme.Glyphs.Jobs }
func (j *Jobs) Key() string      { return "jobs" }
func (j *Jobs) Init() tea.Cmd    { return nil }
func (j *Jobs) Selectable() bool { return true }

func (j *Jobs) Contexts() []string { return []string{keys.Jobs, keys.List, keys.Panel} }

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

func (j *Jobs) Update(msg tea.Msg) (wm.Card, tea.Cmd) {
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
			return j, wm.RequestAttention(j, wm.Pulse)
		}
		return j, nil
	case wm.ActionMsg:
		switch msg.ID {
		case "jobs.stop":
			return j, j.kill()
		case "jobs.follow":
			j.follow = !j.follow
		case "jobs.scroll.down":
			j.vp.PageDown()
			j.follow = j.vp.AtBottom()
		case "jobs.scroll.up":
			j.vp.PageUp()
			j.follow = false
		default:
			if next := listMove(msg.ID, j.sel, len(j.jobs), j.listH-render.TableHead); next != j.sel {
				j.sel = next
				j.follow = true
				j.pull()
			}
		}
	case tea.MouseWheelMsg:
		var cmd tea.Cmd
		j.vp, cmd = j.vp.Update(msg)
		j.follow = j.vp.AtBottom()
		return j, cmd
	case tea.MouseClickMsg:
		if msg.Button != tea.MouseLeft {
			return j, nil
		}
		if y := msg.Y - j.listH - 1; y >= 0 && msg.X == j.w-1 && j.w >= minGutterWidth {
			switch hit, line := j.tailGutter().hit(y); hit {
			case hitPageUp:
				j.vp.PageUp()
				j.follow = false
			case hitPageDown:
				j.vp.PageDown()
				j.follow = j.vp.AtBottom()
			case hitJump:
				j.vp.SetYOffset(line)
				j.follow = j.vp.AtBottom()
			}
			return j, nil
		}
		if row := msg.Y - render.TableHead + j.start; msg.Y >= render.TableHead && msg.Y < j.listH && row < len(j.jobs) {
			j.sel = row
			j.follow = true
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
		return wm.Notify("already finished", wm.Info)
	}
	return func() tea.Msg { return KillJobMsg{Job: jb.ID} }
}

func (j *Jobs) View(w, h int) string {
	j.w, j.h = w, h
	t := j.env.Theme
	if len(j.jobs) == 0 {
		return emptyView(w, h, "no jobs", t)
	}
	now := time.Now()
	cols := []render.Column{{Title: "JOB"}, {Title: "COMMAND", Flex: 1}, {Title: "STATE"}, {Title: "TIME", Right: true}, {Title: "OUT", Right: true}}
	rows := make([][]string, 0, len(j.jobs))
	for _, jb := range j.jobs {
		state := "running"
		elapsed := now.Sub(jb.Started)
		if !jb.Running {
			elapsed = jb.Finished.Sub(jb.Started)
			switch {
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
	listH := min(len(j.jobs)+render.TableHead, max(h/3, render.TableHead+1), max(h, 1))
	table, start := window(strings.Split(render.Table(cols, rows, w, j.sel, t), "\n"), j.sel, listH)
	j.start, j.listH = start, len(table)
	body := h - len(table) - 1
	if body < 1 {
		return strings.Join(table[:min(len(table), h)], "\n")
	}
	tw := w
	if w >= minGutterWidth {
		tw = w - 2
	}
	j.vp.SetWidth(tw)
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
	sep := render.Divider(min(w, 40), t)
	tail := j.vp.View()
	if tw < w {
		tail = j.tailGutter().beside(tail, w, t)
	}
	return strings.Join(append(append(table, sep), tail), "\n")
}

// Scrollbar of the tail capped with the keys that page it
func (j *Jobs) tailGutter() gutter {
	return gutter{h: j.vp.Height(), total: j.vp.TotalLineCount(), offset: j.vp.YOffset(), visible: j.vp.Height(), up: j.env.Keys.First("jobs.scroll.up"), down: j.env.Keys.First("jobs.scroll.down")}
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

// Actions the job list answers
func (j *Jobs) Hints() []wm.Hint {
	return []wm.Hint{{Action: "jobs.stop"}, {Action: "jobs.follow"}, {Action: "jobs.scroll.down", Pair: "jobs.scroll.up", Label: "tail"}, {Action: "list.down", Pair: "list.up", Label: "move"}}
}
