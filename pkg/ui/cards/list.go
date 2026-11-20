package cards

import (
	tea "charm.land/bubbletea/v2"

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
type List struct{ *lister }

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
	l := &List{lister: &lister{env: env, title: spec.Title, glyph: spec.Glyph, id: spec.Key, ctx: spec.Context, primary: spec.Primary, cols: spec.Columns, rows: spec.Rows, detail: spec.Detail, free: spec.Free, empty: spec.Empty, follow: spec.Follow}}
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

// Hands the message to the table and keeps the list as the card the window manager holds
func (l *List) Update(msg tea.Msg) (wm.Card, tea.Cmd) {
	_, cmd := l.lister.Update(msg)
	return l, cmd
}

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
