package cards

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Files the runs touched with their diffs and the directory the shell sits in
type Files struct {
	env      *Env
	files    []harness.Touch
	stats    map[string]diffStat
	shown    diffView
	sel      int
	expanded bool
	whole    bool
	scroll   int
	start    int
	tableEnd int
	diffTop  int
	diffRows int
	diffLen  int
	w, h     int
}

// Added and removed line counts of a change, kept until the texts differ
type diffStat struct {
	old, new   string
	adds, dels int
}

// Rendered diff of the expanded file, kept until its inputs or the theme differ
type diffView struct {
	path, old, new string
	theme          string
	w              int
	lines          []string
}

func NewFiles(env *Env) *Files {
	w := &Files{env: env, stats: map[string]diffStat{}}
	w.refresh()
	return w
}

func (c *Files) Title() string    { return "FILES" }
func (c *Files) Glyph() string    { return c.env.Theme.Glyphs.Files }
func (c *Files) Key() string      { return "files" }
func (c *Files) Init() tea.Cmd    { return nil }
func (c *Files) Selectable() bool { return true }

func (c *Files) Contexts() []string { return []string{keys.Files, keys.List, keys.Panel} }

func (c *Files) refresh() {
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
func (c *Files) stat(path, old, new string) (int, int) {
	if st, ok := c.stats[path]; ok && st.old == old && st.new == new {
		return st.adds, st.dels
	}
	adds, dels := render.Stat(render.Diff(old, new))
	c.stats[path] = diffStat{old: old, new: new, adds: adds, dels: dels}
	return adds, dels
}

// Formatted diff lines, rendered only when the path, texts, width or theme differ from the last call
func (c *Files) diff(path, old, new string, w int) []string {
	v := c.shown
	if v.path == path && v.old == old && v.new == new && v.w == w && v.theme == c.env.Theme.Name {
		return v.lines
	}
	lines := strings.Split(render.FormatDiff(render.Diff(old, new), w, c.env.Theme), "\n")
	c.shown = diffView{path: path, old: old, new: new, theme: c.env.Theme.Name, w: w, lines: lines}
	return lines
}

// Files touched, most recent first
func (c *Files) Files() []harness.Touch { return c.files }

func (c *Files) Update(msg tea.Msg) (wm.Card, tea.Cmd) {
	switch msg := msg.(type) {
	case harness.EventMsg:
		if msg.Event.Type == agent.EventToolEnd {
			c.refresh()
		}
	case wm.ActionMsg:
		switch msg.ID {
		case "files.diff":
			c.expanded = !c.expanded
			c.scroll = 0
		case "files.whole":
			c.whole = !c.whole
			c.scroll = 0
		case "files.scroll.down":
			c.scrollBy(max(c.diffRows-1, 1))
		case "files.scroll.up":
			c.scrollBy(-max(c.diffRows-1, 1))
		default:
			if next := listMove(msg.ID, c.sel, len(c.files), c.h-2-render.TableHead); next != c.sel {
				c.sel = next
				c.scroll = 0
			}
		}
	case tea.MouseClickMsg:
		if msg.Button != tea.MouseLeft {
			return c, nil
		}
		if y := msg.Y - c.diffTop; c.expanded && c.diffRows > 0 && y >= 0 && y < c.diffRows && msg.X == c.w-1 && c.w >= minGutterWidth {
			switch hit, line := c.diffGutter().hit(y); hit {
			case hitPageUp:
				c.scrollBy(-max(c.diffRows-1, 1))
			case hitPageDown:
				c.scrollBy(max(c.diffRows-1, 1))
			case hitJump:
				c.scroll = line
			}
			return c, nil
		}
		if row := msg.Y - 1 - render.TableHead + c.start; msg.Y >= 1+render.TableHead && row < len(c.files) && msg.Y < c.tableEnd {
			if row == c.sel {
				c.expanded = !c.expanded
			}
			c.sel = row
			c.scroll = 0
		}
	case tea.MouseWheelMsg:
		switch msg.Button {
		case tea.MouseWheelDown:
			c.scrollBy(1)
		case tea.MouseWheelUp:
			c.scrollBy(-1)
		}
	}
	return c, nil
}

// Moves the diff by n lines, kept inside the diff
func (c *Files) scrollBy(n int) {
	c.scroll = min(max(c.scroll+n, 0), max(c.diffLen-c.diffRows, 0))
}

// Scrollbar of the diff capped with the keys that page it
func (c *Files) diffGutter() gutter {
	return gutter{h: c.diffRows, total: c.diffLen, offset: c.scroll, visible: c.diffRows, up: c.env.Keys.First("files.scroll.up"), down: c.env.Keys.First("files.scroll.down")}
}

func (c *Files) View(w, h int) string {
	c.w, c.h = w, h
	t := c.env.Theme
	head := render.KV("cwd", "", t)
	if c.env.Work != nil {
		head = render.KV("cwd", c.env.Work.Dir(), t)
		if n := c.env.Work.Searches(); n > 0 {
			head += "  " + render.KV("searches", fmt.Sprint(n), t)
		}
	}
	lines := []string{render.Truncate(head, w, t)}
	if len(c.files) == 0 {
		return strings.Join(append(lines, emptyView(w, h-1, "no files touched", t)), "\n")
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
	room := h - 1
	if c.expanded {
		room = max(room/3, render.TableHead+2)
	}
	table, start := window(strings.Split(render.Table(cols, rows, w, c.sel, t), "\n"), c.sel, room)
	c.start = start
	lines = append(lines, table...)
	c.tableEnd = len(lines)
	c.diffRows, c.diffLen = 0, 0
	if c.expanded && c.sel < len(c.files) {
		f := c.files[c.sel]
		lines = append(lines, "")
		switch {
		case f.Change == nil:
			lines = append(lines, render.Dim("read only", t))
		default:
			old := f.Change.Old
			label := "last change"
			if c.whole && f.Original != nil {
				old = *f.Original
				label = "since start"
			}
			lines = append(lines, render.Heading(f.Path, t)+" "+render.Dim(label, t))
			dw := w
			if w >= minGutterWidth {
				dw = w - 2
			}
			diff := c.diff(f.Path, old, f.Change.New, dw)
			c.diffTop, c.diffRows, c.diffLen = len(lines), max(h-len(lines), 0), len(diff)
			c.scrollBy(0)
			shown := diff[min(c.scroll, len(diff)):]
			shown = shown[:min(len(shown), c.diffRows)]
			body := strings.Join(shown, "\n")
			if dw < w && c.diffRows > 0 {
				body = c.diffGutter().beside(body, w, t)
			}
			lines = append(lines, body)
		}
	}
	if len(lines) > h {
		lines = lines[:h]
	}
	return strings.Join(lines, "\n")
}

func (c *Files) Status() string {
	changed := 0
	for _, f := range c.files {
		if f.Change != nil {
			changed++
		}
	}
	if changed == 0 {
		return render.Count(len(c.files), "file")
	}
	return fmt.Sprintf("%d changed", changed)
}

// Actions the file list answers, the diff ones once a diff shows
func (c *Files) Hints() []wm.Hint {
	out := []wm.Hint{{Action: "files.diff"}}
	if c.expanded {
		out = append(out, wm.Hint{Action: "files.whole"}, wm.Hint{Action: "files.scroll.down", Pair: "files.scroll.up", Label: "scroll"})
	}
	return append(out, wm.Hint{Action: "list.down", Pair: "list.up", Label: "move"})
}
