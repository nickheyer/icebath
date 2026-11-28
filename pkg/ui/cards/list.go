package cards

import (
	"slices"
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/render"
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
	// Buttons for the row under the cursor, -1 for none, the one enter presses first. Every action of the context when nil
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
	// Words the empty filter field shows, such as "Search sessions"
	Search string
	// Runs when the words searched change: a search started, its words changed, or the search cleared to none
	Searched func(l *List, query string) tea.Cmd
	// A list to pick from: typing filters it from the first key and enter runs Primary on the highlighted row
	Pick bool
	// Shows the filter field of a list to pick from at once, rather than once a key is typed or the rows outgrow the room
	Bar bool
	// Lines above the rows saying what the list is, such as what is being chosen, wrapped to the width
	Intro func() []string
	// Heading of the group row i belongs to, rows of one heading in a run sitting under it, empty for none. A filter drops the headings
	Group func(i int) string
	// Words the filter reads for row i besides its cells, such as a description the row does not show
	Words func(i int) string
	// Columns as they are now, for a list whose columns change with what it shows, such as while it is searched; Columns when nil
	Layout func() []render.Column
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
	// Runs when the words searched change, nil for a list whose owner does not follow them
	searched func(l *List, query string) tea.Cmd
	// Every column of the spec and the rows of every column, the table showing those that fit
	all     []render.Column
	allRows func() [][]string
	yield   []int
	// Columns of the spec in view at the last paint, and the column of the row typed into among them
	keep     []int
	inputCol int
	pick     bool
	// Whether the columns in view depend on the width: some yield, or some hold sentences shown whole or not at all
	fitted bool
	// Columns as they are now, nil for a list whose columns never change
	layout func() []render.Column
	// Rows of every column as they were last read, which a search reads the columns out of view from
	full [][]string
	// What draws the list again with others while it is on a hub, and whether the hub told it that it left since it was last shown
	watched []*Watched
	left    bool
}

// Lists a page draws again whenever what they show changes: each from the time it is added until its hub tells it that it left, so a page
// never keeps or draws the lists of views that closed. A list still on a hub that shows again, as the pages of a menu do, is drawn again
// the moment it is back in view and with the others from then on
type Watched struct {
	lists []*List
}

// Draws the list again with the others from now on
func (w *Watched) Add(list *List) {
	if !slices.Contains(list.watched, w) {
		list.watched = append(list.watched, w)
	}
	if !slices.Contains(w.lists, list) {
		w.lists = append(w.lists, list)
	}
}

// Recomputes the rows of every list watched now
func (w *Watched) Refresh() {
	for _, list := range slices.Clone(w.lists) {
		list.Refresh()
	}
}

// Lists watched now, in the order they were added
func (w *Watched) Lists() []*List {
	return slices.Clone(w.lists)
}

// Told by its hub that it left: what drew it again with other lists forgets it
func (l *List) Closed() tea.Cmd {
	for _, w := range l.watched {
		w.lists = slices.DeleteFunc(w.lists, func(have *List) bool { return have == l })
	}
	l.left = len(l.watched) > 0
	return nil
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
	wm.Answerer
	Closer
} = (*List)(nil)

// Builds the table from the spec, each action handed the list and the row under the cursor
func NewList(env *Env, spec ListSpec) *List {
	l := &List{lister: &lister{env: env, title: spec.Title, glyph: spec.Glyph, id: spec.Key, ctx: spec.Context, primary: spec.Primary, cols: spec.Columns, rows: spec.Rows, detail: spec.Detail, free: spec.Free, empty: spec.Empty, follow: spec.Follow},
		all: spec.Columns, allRows: spec.Rows, yield: spec.Yield, pick: spec.Pick}
	l.bar.pinned, l.search, l.always = spec.Pick, spec.Search, spec.Bar
	l.intro, l.group, l.words = spec.Intro, spec.Group, spec.Words
	l.layout = spec.Layout
	l.fitted = len(spec.Yield) > 0 || spec.Layout != nil || slices.ContainsFunc(spec.Columns, func(c render.Column) bool { return c.Whole })
	if l.fitted && spec.Rows != nil {
		l.lister.rows = l.shownRows
		// A search reads the cells of the columns out of view as well: what a row says does not change with the width
		l.words = func(i int) string {
			var said []string
			// The rows as they were read for this search, every column of them
			if i < len(l.full) {
				for c, cell := range l.full[i] {
					if !l.Shows(c) {
						said = append(said, ansi.Strip(cell))
					}
				}
			}
			if spec.Words != nil {
				said = append(said, spec.Words(i))
			}
			return strings.Join(said, " ")
		}
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
	l.searched = spec.Searched
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
	// A list back in view after its hub told it that it left missed what changed meanwhile: it reads its rows now, and is drawn again
	// with the lists it was watched with from here on
	if focus, ok := msg.(wm.FocusMsg); ok && focus.Focused && l.left {
		l.left = false
		for _, w := range l.watched {
			w.Add(l)
		}
		l.refresh()
	}
	before := l.bar.Query()
	card, cmd := l.update(msg)
	return card, tea.Batch(cmd, l.searchedSince(before))
}

// What the list's owner runs when the words searched are no longer those of before
func (l *List) searchedSince(before string) tea.Cmd {
	if l.searched == nil || l.bar.Query() == before {
		return nil
	}
	return l.searched(l, l.bar.Query())
}

func (l *List) update(msg tea.Msg) (wm.Card, tea.Cmd) {
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
				return l, l.act(l.entered())
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

// Action enter runs in a list to pick from: the first button shown for the row under the cursor, the one lit, else the primary action
func (l *List) entered() string {
	if l.hints != nil {
		if hints := l.hints(l.lister, l.index()); len(hints) > 0 {
			return hints[0].Action
		}
	}
	return l.primary
}

// Reports whether the list has a use for the keymap action: one of its own context that applies with no row in hand whenever it has it,
// another only while it shows a button for it, or, for a list without buttons of its own, while it has that action and a row for it; any
// other action always. A key whose action the list has no use
// for goes on to the next action bound to it, such as the text keys of its filter
func (l *List) Answers(id string) bool {
	if l.ctx == "" {
		return true
	}
	if a, ok := l.env.Keys.Action(id); !ok || a.Context != l.ctx {
		return true
	}
	// An action that applies with no row in hand, such as adding one, applies on every row, its button shown or not
	if _, has := l.actions[id]; has && l.free[id] {
		return true
	}
	if l.hints != nil {
		for _, hint := range l.hints(l.lister, l.index()) {
			if hint.Action == id {
				return true
			}
		}
		return false
	}
	_, has := l.actions[id]
	return has && (l.free[id] || l.index() >= 0)
}

// Puts the cursor on the shown row whose first cell of words is the query, else starts with it, else has a word starting with it. A
// cell that holds only a mark, such as the one before the choice in use, is no cell of words
func (l *List) selectBest(q string) {
	best, rank := -1, 4
	for at, i := range l.shown {
		if i >= len(l.data) || len(l.data[i]) == 0 {
			continue
		}
		first := ""
		for _, cell := range l.data[i] {
			if c := strings.ToLower(strings.TrimSpace(ansi.Strip(cell))); strings.ContainsFunc(c, unicode.IsLetter) || strings.ContainsFunc(c, unicode.IsDigit) {
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

// Paints the columns that fit the width: a column of sentences only where every cell of it shows whole, then those that yield giving way
// first to last
func (l *List) View(w, h int) string {
	if !l.fitted {
		return l.lister.View(w, h)
	}
	if l.layout != nil {
		if cols := l.layout(); !slices.Equal(cols, l.all) {
			l.all, l.keep = cols, nil
		}
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

// Columns of the spec to show at width w: all of them while each keeps a readable width, else fewer. A column of sentences goes first,
// the last such column before the others, once the width cannot hold every cell of it whole; then the columns that yield, in their order
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
		if c.Whole {
			continue
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
	for i := len(l.all) - 1; i >= 0 && room() > w && len(keep) > 1; i-- {
		if l.all[i].Whole {
			keep = slices.DeleteFunc(keep, func(k int) bool { return k == i })
		}
	}
	for _, c := range l.yield {
		if room() <= w || len(keep) <= 1 {
			break
		}
		keep = slices.DeleteFunc(keep, func(k int) bool { return k == c })
	}
	return keep
}

// Reports whether the table cuts the cell of row i in column c of the spec, because the column is narrower than the cell. The detail of
// a row then says the cell whole
func (l *List) Cut(i, c int) bool {
	if len(l.keep) > 0 {
		if c = slices.Index(l.keep, c); c < 0 {
			return false
		}
	}
	return l.lister.cut(i, c)
}

// Reports whether column c of the spec was in view at the last paint, every column before the first paint
func (l *List) Shows(c int) bool {
	return len(l.keep) == 0 || slices.Contains(l.keep, c)
}

// Cells of the columns in view for every row of the spec
func (l *List) shownRows() [][]string {
	rows := l.allRows()
	l.full = rows
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

// Opens the filter bar holding the text, the rows narrowed to those holding it
func (l *List) Filter(text string) {
	l.bar.Open()
	l.bar.input().Insert(text)
	l.refresh()
}

// Reports whether the filter narrows the rows
func (l *List) Filtered() bool { return l.bar.Query() != "" }

// Text the filter holds
func (l *List) Query() string { return l.bar.Query() }

// Names the list anew, as its hub then names it in its trail, once what it shows was renamed
func (l *List) Retitle(title string) { l.title = title }

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

// Rows the list fills: its opening lines, every row under the head and the headings between them, with room for the filter field and a
// question, and the detail pane under a rule
func (l *List) Rows(w int) int {
	table := len(l.opening(w)) + render.Head(l.cols) + len(l.flow())
	if len(l.data) == 0 {
		return hubLeast
	}
	if pane := l.paneRows(w, 2*detailRows); pane > 0 {
		return table + 1 + pane + 3
	}
	return table + 3
}

// Rows a list leaves under its table for the detail of the highlighted row when it sizes itself
const detailRows = 8

// Takes the next key pressed, under the words that ask for it, and hands its chord over
func (l *List) Capture(asking string, done func(chord string) tea.Cmd) {
	l.asking = asking
	l.capture = func(_ *lister, chord string) tea.Cmd { return done(chord) }
}

// Asks a question along the bottom of the list, its act running yes
func (l *List) Confirm(q Question, yes func() tea.Cmd) { l.confirm(q, yes) }

// Marks the row in hand rather than lighting it while on, for a list that does not have the keys now
func (l *List) Rest(on bool) { l.rest = on }

// Whether the list takes no typing of its own now: no question, key being recorded, cell or row being typed into, and nothing typed into
// its filter
func (l *List) idle() bool {
	return l.ask == nil && l.capture == nil && l.edit == nil && !l.typing() && l.Query() == ""
}
