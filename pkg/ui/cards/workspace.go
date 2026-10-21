package cards

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/ui/deck"
	"github.com/nickheyer/icebath/pkg/ui/render"
)

// Files the run touched with their diffs and the directory the shell sits in
type Workspace struct {
	env      *Env
	files    []harness.Touch
	stats    map[string]diffStat
	shown    diffView
	sel      int
	expanded bool
	whole    bool
	scroll   int
	w, h     int
}

// Added and removed line counts of a change, kept until the texts differ
type diffStat struct {
	old, new   string
	adds, dels int
}

// Rendered diff of the expanded file, kept until its inputs differ
type diffView struct {
	path, old, new string
	w              int
	lines          []string
}

var (
	wDiff  = key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "diff"))
	wWhole = key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "since start"))
)

func NewWorkspace(env *Env) *Workspace {
	w := &Workspace{env: env, stats: map[string]diffStat{}}
	w.refresh()
	return w
}

func (c *Workspace) Title() string        { return "WORKSPACE" }
func (c *Workspace) Glyph() string        { return "▣" }
func (c *Workspace) Key() string          { return "workspace" }
func (c *Workspace) Init() tea.Cmd        { return nil }
func (c *Workspace) Selectable() bool     { return true }
func (c *Workspace) Hints() []key.Binding { return []key.Binding{moveHint, wDiff, wWhole} }
func (c *Workspace) Claims() []string     { return []string{"j", "k", "g", "G", "enter", "a", "J", "K"} }

func (c *Workspace) refresh() {
	if c.env.Work != nil {
		c.files = c.env.Work.Files()
	}
	if c.sel >= len(c.files) {
		c.sel = max(0, len(c.files)-1)
	}
	live := map[string]bool{}
	for _, f := range c.files {
		live[f.Path] = true
	}
	for path := range c.stats {
		if !live[path] {
			delete(c.stats, path)
		}
	}
}

// Line counts of the change, diffed only when the texts differ from the last call for the path
func (c *Workspace) stat(path, old, new string) (int, int) {
	if st, ok := c.stats[path]; ok && st.old == old && st.new == new {
		return st.adds, st.dels
	}
	adds, dels := render.Stat(render.Diff(old, new))
	c.stats[path] = diffStat{old: old, new: new, adds: adds, dels: dels}
	return adds, dels
}

// Formatted diff lines, rendered only when the path, texts or width differ from the last call
func (c *Workspace) diff(path, old, new string, w int) []string {
	v := c.shown
	if v.path == path && v.old == old && v.new == new && v.w == w {
		return v.lines
	}
	lines := strings.Split(render.FormatDiff(render.Diff(old, new), w, c.env.Theme), "\n")
	c.shown = diffView{path: path, old: old, new: new, w: w, lines: lines}
	return lines
}

// Files touched, most recent first
func (c *Workspace) Files() []harness.Touch { return c.files }

func (c *Workspace) Update(msg tea.Msg) (deck.Card, tea.Cmd) {
	switch msg := msg.(type) {
	case harness.EventMsg:
		if msg.Event.Type == agent.EventToolEnd {
			c.refresh()
		}
	case tea.KeyPressMsg:
		switch msg.String() {
		case "j", "down":
			c.sel = min(c.sel+1, max(0, len(c.files)-1))
			c.scroll = 0
		case "k", "up":
			c.sel = max(0, c.sel-1)
			c.scroll = 0
		case "g":
			c.sel = 0
		case "G":
			c.sel = max(0, len(c.files)-1)
		case "enter":
			c.expanded = !c.expanded
			c.scroll = 0
		case "a":
			c.whole = !c.whole
		case "J":
			c.scroll++
		case "K":
			c.scroll = max(0, c.scroll-1)
		}
	case tea.MouseClickMsg:
		if msg.Button == tea.MouseLeft && msg.Y >= 2 && msg.Y-2 < len(c.files) {
			c.sel = msg.Y - 2
			c.expanded = !c.expanded
		}
	case tea.MouseWheelMsg:
		switch msg.Button {
		case tea.MouseWheelDown:
			c.scroll++
		case tea.MouseWheelUp:
			c.scroll = max(0, c.scroll-1)
		}
	}
	return c, nil
}

func (c *Workspace) View(w, h int) string {
	c.w, c.h = w, h
	t := c.env.Theme
	head := render.KV("cwd", "", t)
	if c.env.Work != nil {
		head = render.KV("cwd", c.env.Work.Dir(), t)
		if n := c.env.Work.Searches(); n > 0 {
			head += "  " + render.KV("searches", fmt.Sprint(n), t)
		}
	}
	lines := []string{render.Truncate(head, w)}
	if len(c.files) == 0 {
		return strings.Join(append(lines, "", render.Dim("no files touched yet", t)), "\n")
	}
	cols := []render.Column{{Title: "FILE", Flex: 1}, {Title: "R", Right: true}, {Title: "W", Right: true}, {Title: "E", Right: true}, {Title: "CHANGE", Right: true}, {Title: "BY"}}
	rows := make([][]string, 0, len(c.files))
	for _, f := range c.files {
		change := ""
		if f.Change != nil {
			a, r := c.stat(f.Path, f.Change.Old, f.Change.New)
			change = render.Good(fmt.Sprintf("+%d", a), t) + " " + render.Bad(fmt.Sprintf("-%d", r), t)
		}
		rows = append(rows, []string{f.Path, fmt.Sprint(f.Reads), fmt.Sprint(f.Writes), fmt.Sprint(f.Edits), change, f.Agent})
	}
	lines = append(lines, strings.Split(render.Table(cols, rows, w, c.sel, t), "\n")...)
	if c.expanded && c.sel < len(c.files) {
		f := c.files[c.sel]
		lines = append(lines, "")
		switch {
		case f.Change == nil:
			lines = append(lines, render.Dim("read only, nothing to diff", t))
		default:
			old := f.Change.Old
			label := "last change"
			if c.whole && f.Original != nil {
				old = *f.Original
				label = "since the session started"
			}
			lines = append(lines, render.Heading(f.Path, t)+" "+render.Dim(label, t))
			diff := c.diff(f.Path, old, f.Change.New, w)
			if c.scroll > 0 && c.scroll < len(diff) {
				diff = diff[c.scroll:]
			}
			lines = append(lines, diff...)
		}
	}
	if len(lines) > h {
		lines = lines[:h]
	}
	return strings.Join(lines, "\n")
}

func (c *Workspace) Status() string {
	changed := 0
	for _, f := range c.files {
		if f.Change != nil {
			changed++
		}
	}
	if changed == 0 {
		return fmt.Sprintf("%d files", len(c.files))
	}
	return fmt.Sprintf("%d changed", changed)
}
