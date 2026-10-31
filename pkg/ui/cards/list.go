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

// Opens the detail pane of the selected row when closed and closes it when open
func (l *List) ToggleDetail() tea.Cmd { return toggleDetail(l.lister, l.index()) }
