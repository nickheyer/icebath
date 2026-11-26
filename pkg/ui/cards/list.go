package cards

import (
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/lang"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/txt"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Table panel an extension builds, rows and details computed on every view
type ListSpec struct {
	Title, Glyph, Key string
	// Keymap context of its own actions
	Context string
	// Keymap contexts in force on every row besides Context, the input row included
	Shared []string
	// Action a click on the selected row runs
	Primary string
	Columns []render.Column
	Rows    func() [][]string
	Detail  func(i int) []string
	Actions map[string]func(l *List, i int) tea.Cmd
	// Actions that apply with no row under the cursor
	Free map[string]bool
	// Legend of the row under the cursor in place of every action of the context, -1 for none
	Hints   func(l *List, i int) []wm.Hint
	Empty   func() string
	OnFocus func(l *List) tea.Cmd
	// Keeps the cursor on the newest row while it sits there
	Follow bool
	// Trailer on the frame, ahead of the count of rows a scrolled table hides
	Status func() string
	// Line above the table while it returns text, such as a part of the rows that failed to load
	Banner func() string
	// Takes text typed or pasted while the table holds the keys, an unbound printable key or a paste
	Typed func(l *List, text string, pasted bool) tea.Cmd
	// Runs on a digit picking one of the first ten rows in view, which show their digit
	Quick func(l *List, i int) tea.Cmd
	// Row typed into in place while the cursor sits on it, nil for a table without one
	Input *ListInput
	// Columns that give way, first to last, when the width cannot hold every column at a readable width
	Yield []int
	// Lines the help of a hub shows for row i, the detail of the row when nil
	Help func(i int) []string
	// A list to pick from: the filter bar takes typing from the start and enter on it runs Primary on the highlighted row
	Pick bool
}

// Row of a list that takes typing in place of the table keys while the cursor sits on it
type ListInput struct {
	// Reports whether row i is the one typed into
	Row func(i int) bool
	// Column the text shows in
	Col int
	// Keymap contexts in force on the row ahead of the cell keys, their actions run from the Actions of the spec
	Contexts []string
	// Handed the trimmed text on the apply key
	Done func(l *List, text string) tea.Cmd
	// Runs on the cancel key, which clears the text when nil
	Cancel func(l *List) tea.Cmd
}

// Table card with a filter bar, detail pane and keymap actions an extension drives through its spec
type List struct {
	*lister
	// Every column of the spec and the rows of every column, the table showing those that fit
	all     []render.Column
	allRows func() [][]string
	yield   []int
	// Columns of the spec in view at the last paint, and the column of the row typed into among them
	keep     []int
	inputCol int
	help     func(i int) []string
	pick     bool
}

// Window manager interfaces the list answers through the table it wraps
var _ interface {
	wm.Card
	wm.Keyed
	wm.Glypher
	wm.Statuser
	wm.Cursorer
	wm.Hinter
	wm.Contexter
	wm.Raw
	wm.Selectable
} = (*List)(nil)

// Builds the table from the spec, each action handed the list and the row under the cursor
func NewList(env *Env, spec ListSpec) *List {
	l := &List{lister: &lister{env: env, title: spec.Title, glyph: spec.Glyph, id: spec.Key, ctx: spec.Context, primary: spec.Primary, cols: spec.Columns, rows: spec.Rows, detail: spec.Detail, free: spec.Free, empty: spec.Empty, follow: spec.Follow},
		all: spec.Columns, allRows: spec.Rows, yield: spec.Yield, help: spec.Help, pick: spec.Pick}
	if len(spec.Yield) > 0 && spec.Rows != nil {
		l.lister.rows = l.shownRows
	}
	if spec.Detail == nil && spec.Rows != nil {
		l.lister.detail = l.whole
	}
	l.actions = make(map[string]func(*lister, int) tea.Cmd, len(spec.Actions))
	for id, fn := range spec.Actions {
		l.actions[id] = func(_ *lister, i int) tea.Cmd { return fn(l, i) }
	}
	if spec.OnFocus != nil {
		l.onFocus = func(*lister) tea.Cmd { return spec.OnFocus(l) }
	}
	if spec.Hints != nil {
		l.hints = func(_ *lister, i int) []wm.Hint { return spec.Hints(l, i) }
	}
	l.status, l.banner, l.shared = spec.Status, spec.Banner, spec.Shared
	if spec.Typed != nil {
		l.typed = func(_ *lister, text string, pasted bool) tea.Cmd { return spec.Typed(l, text, pasted) }
	}
	if spec.Quick != nil {
		l.quick = func(_ *lister, i int) tea.Cmd { return spec.Quick(l, i) }
	}
	if in := spec.Input; in != nil {
		l.inputCol = in.Col
		l.input = &rowInput{row: in.Row, col: in.Col, ctxs: in.Contexts}
		if in.Done != nil {
			l.input.done = func(_ *lister, text string) tea.Cmd { return in.Done(l, text) }
		}
		if in.Cancel != nil {
			l.input.cancel = func(*lister) tea.Cmd { return in.Cancel(l) }
		}
	}
	return l
}

// Hands the message to the table and keeps the list as the card the window manager holds, a pick list picking from its filter bar and
// moving the cursor to the row whose first cell best matches what was typed
func (l *List) Update(msg tea.Msg) (wm.Card, tea.Cmd) {
	if l.pick {
		before := l.bar.Query()
		defer func() {
			if q := l.bar.Query(); q != before && q != "" {
				l.refresh()
				l.selectBest(strings.ToLower(q))
			}
		}()
		switch m := msg.(type) {
		case wm.ActionMsg:
			if m.ID == "search.accept" {
				l.bar.Accept()
				return l, l.act(l.primary)
			}
		case wm.FocusMsg:
			_, cmd := l.lister.Update(msg)
			if m.Focused {
				l.bar.Open()
			}
			return l, cmd
		}
	}
	_, cmd := l.lister.Update(msg)
	return l, cmd
}

// Reports whether the list has an action for the keymap action
func (l *List) answers(id string) bool {
	_, ok := l.actions[id]
	return ok
}

// Every column of row i at full length, the detail of a list whose spec has none
func (l *List) whole(i int) []string {
	rows := l.allRows()
	if i < 0 || i >= len(rows) {
		return nil
	}
	var lines []string
	for c, cell := range rows[i] {
		if strings.TrimSpace(ansi.Strip(cell)) == "" {
			continue
		}
		if c < len(l.all) && l.all[c].Title != "" {
			cell = render.Heading(l.all[c].Title, l.env.Theme) + "  " + cell
		}
		lines = append(lines, cell)
	}
	return lines
}

// Reports whether the table shows less than the whole of row i: a cell cut short, or a column that gave way holding text
func (l *List) cut(i int) bool {
	if l.lister.cut(i) {
		return true
	}
	if len(l.keep) == 0 || len(l.keep) == len(l.all) {
		return false
	}
	rows := l.allRows()
	if i < 0 || i >= len(rows) {
		return false
	}
	for c, cell := range rows[i] {
		if !slices.Contains(l.keep, c) && strings.TrimSpace(ansi.Strip(cell)) != "" {
			return true
		}
	}
	return false
}

// Legend of the table, a pick list naming its filter bar keys for what they do there: enter picks and esc goes back
func (l *List) Hints() []wm.Hint {
	out := l.lister.Hints()
	if l.detail != nil && !l.bar.Active() && l.edit == nil && l.ask == nil && l.capture == nil && !l.typing() && (l.detailShown() || l.cut(l.index())) {
		out = append(out, wm.Hint{Action: "list.detail"})
	}
	if !l.pick {
		return out
	}
	for i, h := range out {
		switch h.Action {
		case "search.accept":
			out[i].Label = lang.T(txt.KitHintPick)
		case "search.close":
			out[i].Label = lang.T(txt.KitHintBack)
		}
	}
	return out
}

// Puts the cursor on the shown row whose first cell is the query, else starts with it, else has a word starting with it
func (l *List) selectBest(q string) {
	best, rank := -1, 4
	for at, i := range l.shown {
		if i >= len(l.data) || len(l.data[i]) == 0 {
			continue
		}
		first := ""
		for _, cell := range l.data[i] {
			if c := strings.ToLower(strings.TrimSpace(ansi.Strip(cell))); c != "" {
				first = c
				break
			}
		}
		r := 3
		switch {
		case first == q:
			r = 0
		case strings.HasPrefix(first, q):
			r = 1
		case strings.Contains(" "+first, " "+q):
			r = 2
		}
		if r < rank {
			best, rank = at, r
		}
	}
	if best >= 0 && rank < 3 {
		l.sel = best
	}
}

// Reports whether the list is one to pick from
func (l *List) Picks() bool { return l.pick }

// Least width a flex column keeps before columns that yield give way
const yieldFloor = 16

// Paints the columns that fit the width, those that yield giving way first to last
func (l *List) View(w, h int) string {
	if len(l.yield) == 0 {
		return l.lister.View(w, h)
	}
	keep := l.fit(w)
	if !slices.Equal(keep, l.keep) {
		l.keep = keep
		l.cols = make([]render.Column, len(keep))
		for i, c := range keep {
			l.cols[i] = l.all[c]
		}
		if l.input != nil {
			l.input.col = slices.Index(keep, l.inputCol)
		}
		l.refresh()
	}
	return l.lister.View(w, h)
}

// Columns of the spec to show at width w: all of them while each keeps a readable width, else fewer by the order they yield
func (l *List) fit(w int) []int {
	rows := l.allRows()
	natural := make([]int, len(l.all))
	for i, c := range l.all {
		natural[i] = ansi.StringWidth(c.Title)
		for _, r := range rows {
			if i < len(r) {
				natural[i] = max(natural[i], ansi.StringWidth(r[i]))
			}
		}
		if c.Max > 0 {
			natural[i] = min(natural[i], c.Max)
		}
		if c.Flex > 0 {
			natural[i] = min(natural[i], yieldFloor)
		}
	}
	keep := make([]int, len(l.all))
	for i := range keep {
		keep[i] = i
	}
	room := func() int {
		n := 0
		for _, c := range keep {
			n += natural[c]
		}
		return n + 3*(len(keep)-1) + 3
	}
	for _, c := range l.yield {
		if room() <= w || len(keep) <= 1 {
			break
		}
		keep = slices.DeleteFunc(keep, func(k int) bool { return k == c })
	}
	return keep
}

// Cells of the columns in view for every row of the spec
func (l *List) shownRows() [][]string {
	rows := l.allRows()
	if len(l.keep) == 0 || len(l.keep) == len(l.all) {
		return rows
	}
	out := make([][]string, len(rows))
	for i, r := range rows {
		cells := make([]string, len(l.keep))
		for j, c := range l.keep {
			if c < len(r) {
				cells[j] = r[c]
			}
		}
		out[i] = cells
	}
	return out
}

// Lines the help of a hub shows for the row under the cursor
func (l *List) HelpLines() []string {
	i := l.index()
	switch {
	case i < 0:
		return nil
	case l.help != nil:
		return l.help(i)
	case l.detail != nil:
		return l.detail(i)
	}
	return nil
}

// Opens the filter bar holding the text, the rows narrowed to those holding it
func (l *List) Filter(text string) {
	l.bar.Open()
	l.bar.input().Insert(text)
	l.refresh()
}

// Reports whether the filter narrows the rows
func (l *List) Filtered() bool { return l.bar.Query() != "" }

// Recomputes the rows now
func (l *List) Refresh() { l.refresh() }

// Row under the cursor as an index into the rows, -1 when there is none
func (l *List) Index() int { return l.index() }

// Moves the cursor to row i of the rows, reporting whether the filter shows it
func (l *List) Select(i int) bool { return l.selectRow(i) }

// Moves the cursor by d rows within the rows the filter shows
func (l *List) Move(d int) { l.sel = min(max(l.sel+d, 0), max(len(l.shown)-1, 0)) }

// Reports whether the detail pane of the selected row is open
func (l *List) DetailShown() bool { return l.detailShown() }

// Text typed into the input row so far
func (l *List) Typed() string {
	if l.input == nil {
		return ""
	}
	return l.input.text
}

// Replaces the text of the input row
func (l *List) SetTyped(text string) {
	if l.input != nil {
		l.input.text = text
	}
}

// Opens the detail pane of the selected row when closed and closes it when open
func (l *List) ToggleDetail() tea.Cmd { return toggleDetail(l.lister, l.index()) }

// Asks a yes or no question along the bottom of the list, yes running the action
func (l *List) Confirm(text string, yes func() tea.Cmd) { l.confirm(text, yes) }
