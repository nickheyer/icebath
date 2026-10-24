package cards

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Yes or no question along the bottom of a table and what a yes does
type question struct {
	text string
	yes  func() tea.Cmd
}

// Table card with a cursor, filter bar, detail pane, keymap actions, an editable cell, captured keys and questions
type lister struct {
	env   *Env
	title string
	glyph string
	id    string
	// Keymap context holding the actions of the tab, empty for a tab that only lists
	ctx string
	// Action a click on the selected row runs
	primary string
	cols    []render.Column
	rows    func() [][]string
	detail  func(i int) []string
	actions map[string]func(l *lister, i int) tea.Cmd
	// Actions that apply with no row under the cursor
	free map[string]bool
	// Line the table shows while it has no rows
	empty func() string
	// Runs each time the tab gains the focus
	onFocus func(l *lister) tea.Cmd
	// Keeps the cursor on the newest row while it sits there
	follow   bool
	data     [][]string
	shown    []int
	sel      int
	bar      searchBar
	expanded bool
	pane     detailPane
	edit     *render.Edit
	onDone   func(l *lister, text string) tea.Cmd
	capture  func(l *lister, chord string) tea.Cmd
	asking   string
	ask      *question
	status   func() string
	start    int
	xs       []int
	widths   []int
	w, h     int
}

func (l *lister) Title() string    { return l.title }
func (l *lister) Glyph() string    { return l.glyph }
func (l *lister) Key() string      { return l.id }
func (l *lister) Init() tea.Cmd    { l.refresh(); return nil }
func (l *lister) Selectable() bool { return true }

// Takes every key as typed while it waits for a key to bind
func (l *lister) Raw() bool { return l.capture != nil }

// Keymap contexts in force, a capture, a question, an edit or the filter bar ahead of the table
func (l *lister) Contexts() []string {
	switch {
	case l.capture != nil:
		return []string{keys.Capture}
	case l.ask != nil:
		return []string{keys.Confirm}
	case l.edit != nil:
		return []string{keys.Edit}
	case l.bar.Active():
		return l.bar.Contexts()
	}
	out := []string{keys.Filter, keys.List, keys.Panel}
	if l.detailShown() {
		out = append([]string{keys.Detail}, out...)
	}
	if l.ctx != "" {
		out = append([]string{l.ctx}, out...)
	}
	return out
}

// Reports whether the detail pane of the selected row is open
func (l *lister) detailShown() bool { return l.expanded && l.detail != nil && l.index() >= 0 }

// Reads the rows again and keeps those holding the filter text, following the newest row when asked
func (l *lister) refresh() {
	last := l.sel >= len(l.shown)-1
	if l.rows != nil {
		l.data = l.rows()
	}
	q := strings.ToLower(l.bar.Query())
	l.shown = l.shown[:0]
	for i, r := range l.data {
		if q == "" || strings.Contains(strings.ToLower(ansi.Strip(strings.Join(r, " "))), q) {
			l.shown = append(l.shown, i)
		}
	}
	if l.follow && last {
		l.sel = len(l.shown) - 1
	}
	l.sel = min(max(l.sel, 0), max(0, len(l.shown)-1))
}

// Row of the data under the cursor, -1 when there is none
func (l *lister) index() int {
	if l.sel < 0 || l.sel >= len(l.shown) {
		return -1
	}
	return l.shown[l.sel]
}

// Opens the cell of the selected row for typing, starting from its current text
func (l *lister) editCell(col int, initial string, done func(l *lister, text string) tea.Cmd) {
	l.edit, l.onDone = &render.Edit{Col: col, Text: initial, Selected: initial != ""}, done
}

// Waits for the next key and hands its chord over, the prompt naming what it is for
func (l *lister) wait(prompt string, done func(l *lister, chord string) tea.Cmd) {
	l.capture, l.asking = done, prompt
}

// Asks a yes or no question along the bottom, running yes on a yes
func (l *lister) confirm(text string, yes func() tea.Cmd) { l.ask = &question{text: text, yes: yes} }

// Reports whether a cell is being typed into
func (l *lister) Editing() bool { return l.edit != nil }

// Types text into the cell being edited, replacing it while it shows selected
func (l *lister) typeCell(text string) {
	if l.edit.Selected {
		l.edit.Text = ""
	}
	l.edit.Text += text
	l.edit.Selected = false
}

func (l *lister) Update(msg tea.Msg) (wm.Card, tea.Cmd) {
	switch msg := msg.(type) {
	case wm.FocusMsg:
		if msg.Focused {
			l.refresh()
			if l.onFocus != nil {
				return l, l.onFocus(l)
			}
			return l, nil
		}
		l.edit, l.onDone, l.capture, l.ask = nil, nil, nil, nil
		l.bar.Accept()
	case wm.ActionMsg:
		return l, l.act(msg.ID)
	case tea.KeyPressMsg:
		switch {
		case l.capture != nil:
			if chord, ok := keys.Captured(msg); ok {
				done := l.capture
				l.capture = nil
				return l, done(l, chord)
			}
		case l.edit != nil:
			if msg.Text != "" {
				l.typeCell(msg.Text)
			}
		case l.bar.Active():
			if msg.Text != "" {
				l.bar.Type(msg.Text)
				l.refresh()
			}
		}
	case tea.PasteMsg:
		switch {
		case l.edit != nil:
			l.typeCell(msg.Content)
		case l.bar.Active():
			l.bar.Type(msg.Content)
			l.refresh()
		}
	case tea.MouseClickMsg:
		if msg.Button != tea.MouseLeft || l.edit != nil || l.capture != nil || l.ask != nil {
			return l, nil
		}
		if msg.Y == l.h-1 && l.bar.Shown() {
			l.bar.Open()
			return l, nil
		}
		if l.detailShown() && l.pane.click(msg.X, msg.Y, l.w, l.env) {
			return l, nil
		}
		l.bar.Accept()
		row := msg.Y - render.TableHead + l.start
		if msg.Y < render.TableHead || row >= len(l.shown) {
			return l, nil
		}
		if row == l.sel && l.primary != "" {
			return l, l.act(l.primary)
		}
		l.sel = row
	case tea.MouseWheelMsg:
		step := 1
		if msg.Button == tea.MouseWheelUp {
			step = -1
		}
		if l.detailShown() && l.pane.wheel(msg.Y, step) {
			return l, nil
		}
		l.sel = min(max(l.sel+step, 0), max(0, len(l.shown)-1))
	}
	return l, nil
}

// Carries out a keymap action on the capture, question, edit, filter bar, detail, cursor or tab
func (l *lister) act(id string) tea.Cmd {
	switch id {
	case "capture.cancel":
		l.capture = nil
		return nil
	case "confirm.yes":
		if q := l.ask; q != nil {
			l.ask = nil
			return q.yes()
		}
		return nil
	case "confirm.no":
		l.ask = nil
		return nil
	case "edit.apply":
		if l.edit == nil {
			return nil
		}
		text := strings.TrimSpace(l.edit.Text)
		done := l.onDone
		l.edit, l.onDone = nil, nil
		if done != nil {
			return done(l, text)
		}
		return nil
	case "edit.cancel":
		l.edit, l.onDone = nil, nil
		return nil
	case "edit.delete.back":
		if l.edit != nil {
			if l.edit.Selected {
				l.edit.Text = ""
			} else if r := []rune(l.edit.Text); len(r) > 0 {
				l.edit.Text = string(r[:len(r)-1])
			}
			l.edit.Selected = false
		}
		return nil
	case "edit.clear":
		if l.edit != nil {
			l.edit.Text, l.edit.Selected = "", false
		}
		return nil
	case "edit.end":
		if l.edit != nil {
			l.edit.Selected = false
		}
		return nil
	case "search.accept":
		l.bar.Accept()
		return nil
	case "search.close":
		l.bar.Close()
		l.refresh()
		return nil
	case "search.next":
		l.sel = listMove("list.down", l.sel, len(l.shown), 1)
		return nil
	case "search.prev":
		l.sel = listMove("list.up", l.sel, len(l.shown), 1)
		return nil
	case "filter.toggle":
		l.bar.Toggle()
		l.refresh()
		return nil
	case "list.down", "list.up", "list.top", "list.bottom", "list.page.down", "list.page.up":
		l.sel = listMove(id, l.sel, len(l.shown), l.h-render.TableHead)
		return nil
	}
	if l.pane.act(id) || l.bar.Action(id) {
		l.refresh()
		return nil
	}
	if fn, ok := l.actions[id]; ok && (l.free[id] || l.index() >= 0) {
		return fn(l, l.index())
	}
	return nil
}

// Line along the bottom, the key capture prompt, the question or the open filter bar with its count
func (l *lister) foot(w int) string {
	t := l.env.Theme
	switch {
	case l.capture != nil:
		return render.Fit(" "+render.Warm(l.asking, t)+"  "+render.Dim(l.env.keyed("capture.cancel", "cancel"), t), w, t)
	case l.ask != nil:
		return render.Fit(" "+render.Bad(l.ask.text, t)+"  "+render.Dim(l.env.keyed("confirm.yes", "yes")+" "+t.Glyphs.Sep+" "+l.env.keyed("confirm.no", "no"), t), w, t)
	case l.bar.Shown():
		count := render.Count(len(l.data), "row")
		if l.bar.Query() != "" {
			count = fmt.Sprintf("%d of %d", len(l.shown), len(l.data))
		}
		return l.bar.View(w, count, "filter", t)
	}
	return ""
}

func (l *lister) View(w, h int) string {
	l.w, l.h = w, h
	t := l.env.Theme
	if l.edit == nil {
		l.refresh()
	}
	foot := l.foot(w)
	body := h
	if foot != "" {
		body--
	}
	var rows [][]string
	for _, i := range l.shown {
		rows = append(rows, l.data[i])
	}
	if len(rows) == 0 {
		l.pane.hide()
		empty := "empty"
		switch {
		case l.bar.Query() != "":
			empty = "no match"
		case l.empty != nil:
			empty = l.empty()
		}
		return footed(strings.Split(emptyView(w, body, render.Wrap(empty, max(w-2, 1)), t), "\n"), foot, h)
	}
	l.xs, l.widths = render.Columns(l.cols, rows, w)
	table := strings.Split(render.TableEdit(l.cols, rows, w, l.sel, l.edit, t), "\n")
	room := body
	if l.detailShown() {
		room = max(body/2, render.TableHead+2)
	}
	var lines []string
	lines, l.start = window(table, l.sel, room)
	if !l.detailShown() {
		l.pane.hide()
		return footed(lines, foot, h)
	}
	lines = append(lines, render.Divider(w, t))
	rest := body - len(lines)
	i := l.index()
	if pane := l.pane.view(l.detail(i), i, len(lines), rest, w, l.env); rest > 0 {
		lines = append(lines, pane)
	}
	return footed(lines, foot, h)
}

// Trailer on the frame, where the tab says where its settings live
func (l *lister) Status() string {
	if l.status == nil {
		return ""
	}
	return l.status()
}

// Cursor in the filter bar while it takes typing, else at the end of the cell being typed into
func (l *lister) Cursor() *tea.Cursor {
	if l.bar.Active() && l.edit == nil && l.capture == nil && l.ask == nil {
		return tea.NewCursor(l.bar.CursorX(), l.h-1)
	}
	if l.edit == nil || l.edit.Col >= len(l.xs) || l.widths[l.edit.Col] == 0 {
		return nil
	}
	y := render.TableHead + l.sel - l.start
	n := len([]rune(l.edit.Text))
	x := l.xs[l.edit.Col] + min(n, max(l.widths[l.edit.Col]-1, 0))
	return tea.NewCursor(x, y)
}

// Actions the table answers now, those of its tab first and then the filter and cursor
func (l *lister) Hints() []wm.Hint {
	switch {
	case l.capture != nil:
		return []wm.Hint{{Action: "capture.cancel"}}
	case l.ask != nil:
		return []wm.Hint{{Action: "confirm.yes"}, {Action: "confirm.no"}}
	case l.edit != nil:
		return []wm.Hint{{Action: "edit.apply"}, {Action: "edit.cancel"}}
	case l.bar.Active():
		return []wm.Hint{{Action: "search.accept"}, {Action: "search.next", Pair: "search.prev", Label: "move"}, {Action: "search.close"}}
	}
	var out []wm.Hint
	for _, a := range l.env.Keys.InContexts([]string{l.ctx}) {
		if _, ok := l.actions[a.ID]; ok && a.Context == l.ctx && (l.free[a.ID] || l.index() >= 0) {
			out = append(out, wm.Hint{Action: a.ID})
		}
	}
	if len(l.data) > 0 {
		out = append(out, wm.Hint{Action: "filter.toggle"}, wm.Hint{Action: "list.down", Pair: "list.up", Label: "move"})
	}
	if l.detailShown() && l.pane.overflows() {
		out = append(out, wm.Hint{Action: "detail.scroll.down", Pair: "detail.scroll.up", Label: "scroll detail"})
	}
	return out
}

func toggleDetail(l *lister, i int) tea.Cmd {
	l.expanded = !l.expanded
	return nil
}
