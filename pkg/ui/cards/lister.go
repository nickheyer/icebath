package cards

import (
	"cmp"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/field"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Yes or no question along the bottom of a table and what a yes does
type question struct {
	text string
	yes  func() tea.Cmd
}

// Row typed into in place while the cursor sits on it, the table keys making way for the text
type rowInput struct {
	// Reports whether row i of the data is the one typed into
	row func(i int) bool
	// Cell the text shows in
	col int
	// Keymap contexts in force on the row, ahead of the cell keys
	ctxs []string
	// Handed the trimmed text on the apply key
	done func(l *lister, text string) tea.Cmd
	// Runs on the cancel key, which clears the text when nil
	cancel func(l *lister) tea.Cmd
	// Text typed into the row, what the list reads and replaces
	text string
	// Editor of the text with the keys every field shares, made again when the text was replaced from outside
	in *field.Field
}

// Field editing the row text, made from the text when there is none or the text was replaced since
func (r *rowInput) field() *field.Field {
	if r.in == nil || r.in.Value() != r.text {
		r.in = field.New(r.text)
	}
	return r.in
}

// Keeps the row text after an edit of its field
func (r *rowInput) keep() { r.text = r.field().Value() }

// Cell of the selected row being typed into, the column and the text with its caret
type cellEdit struct {
	col int
	in  *field.Field
}

// Table card with a cursor, filter bar, detail pane, keymap actions, an editable cell, a row typed into, captured keys and questions
type lister struct {
	env   *Env
	title string
	glyph string
	id    string
	// Keymap context holding the actions of the tab, empty for a tab that only lists
	ctx string
	// Keymap contexts in force on every row besides ctx, the row typed into included
	shared []string
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
	// Buttons for the row under the cursor in place of every action of the context, nil for the actions as bound
	hints func(l *lister, i int) []wm.Hint
	// Line above the table while it has something to say, such as a part of the rows that failed to load
	banner func() string
	// Takes text typed or pasted while the table holds the keys, an unbound printable key or a paste
	typed func(l *lister, text string, pasted bool) tea.Cmd
	// Runs on a digit picking one of the first ten rows in view, which show their digit
	quick func(l *lister, i int) tea.Cmd
	// Row typed into in place, nil for a table without one
	input *rowInput
	// Keeps the cursor on the newest row while it sits there
	follow   bool
	data     [][]string
	shown    []int
	sel      int
	bar      searchBar
	expanded bool
	pane     detailPane
	edit     *cellEdit
	onDone   func(l *lister, text string) tea.Cmd
	capture  func(l *lister, chord string) tea.Cmd
	asking   string
	ask      *question
	status   func() string
	start    int
	// Rows the view spends above the table, rows of its head and rows of the table in view at the last paint
	lead, head, seen int
	// Whether every row fit at the last paint with no filter field, and whether the field was painted
	roomy, barred bool
	// Row of the filter field at the last paint
	barY int
	// Words the empty filter field shows
	search string
	// Shows the filter field of a list to pick from at once
	always bool
	// Lines above the rows saying what the list is
	intro func() []string
	// Heading of the group a row of the data belongs to
	group func(i int) string
	// Words the filter reads for a row of the data besides its cells
	words func(i int) string
	// First line of the rows and headings in view, and for each line in view at the last paint the shown row on it, -1 for a heading
	top      int
	lineRows []int
	// Line of the cursor row among those in view at the last paint
	selY int
	// Whether a filter narrowed the rows at the last read, and the row the cursor was on before it did, where it goes back to once the
	// filter is cleared
	narrowed bool
	before   int
	// Words of the filter at the last read: other words put the cursor on the first row they leave
	asked string
	// Lines the longest detail of a row wraps to at the width it was measured at, measured again once the rows are read again
	need, needW int
	needOK      bool
	// Columns the view put ahead of those of the spec
	shift  int
	xs     []int
	widths []int
	w, h   int
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
		return []string{keys.Edit, keys.Text}
	case l.bar.Active() && l.bar.pinned && l.ctx != "":
		// Typing only filters a list to pick from, so the row keys stay in force ahead of the filter's own
		out := append(append([]string{l.ctx}, l.shared...), l.bar.Contexts()...)
		if l.detailShown() && l.pane.overflows() {
			out = append([]string{keys.Detail}, out...)
		}
		return out
	case l.bar.Active():
		return l.bar.Contexts()
	case l.typing():
		return append(append(slices.Clone(l.input.ctxs), l.shared...), keys.Edit, keys.Text)
	}
	out := []string{keys.Filter, keys.List, keys.Panel}
	// The paging keys scroll the detail only while it has more lines than it shows, and page the rows otherwise
	if l.detailShown() && l.pane.overflows() {
		out = append([]string{keys.Detail}, out...)
	}
	out = append(slices.Clone(l.shared), out...)
	if l.ctx != "" {
		out = append([]string{l.ctx}, out...)
	}
	return out
}

// Reports whether the detail pane of the selected row is open
func (l *lister) detailShown() bool { return l.expanded && l.detail != nil && l.index() >= 0 }

// Reports whether the cursor sits on the row typed into while nothing else holds the keys
func (l *lister) typing() bool {
	if l.input == nil || l.capture != nil || l.ask != nil || l.edit != nil || l.bar.Active() {
		return false
	}
	i := l.index()
	return i >= 0 && l.input.row(i)
}

// Cell being typed into, the one opened for an edit or the row input once it holds text, nil for none
func (l *lister) cell() *render.Edit {
	if l.edit != nil {
		return &render.Edit{Col: l.edit.col, Text: l.edit.in.Value(), Cursor: l.edit.in.Cursor(), Selected: l.edit.in.Whole()}
	}
	if l.typing() && l.input.text != "" {
		return &render.Edit{Col: l.input.col, Text: l.input.text, Cursor: l.input.field().Cursor()}
	}
	return nil
}

// Follows an edit of the field typing goes into, the row input keeping its text and the filter narrowing the rows
func (l *lister) took(changed bool) {
	if !changed {
		return
	}
	switch {
	case l.bar.Active():
		l.refresh()
	case l.edit == nil && l.typing():
		l.input.keep()
	}
}

// Field typing goes into now, the cell being edited, the filter bar or the row input, nil while none takes typing
func (l *lister) typingInto() *field.Field {
	switch {
	case l.capture != nil || l.ask != nil:
		return nil
	case l.edit != nil:
		return l.edit.in
	case l.bar.Active():
		return l.bar.input()
	case l.typing():
		return l.input.field()
	}
	return nil
}

// Moves the cursor to row i of the data when the filter shows it
func (l *lister) selectRow(i int) bool {
	at := slices.Index(l.shown, i)
	if at < 0 {
		return false
	}
	l.sel = at
	return true
}

// Takes text the table keys left over, a digit picking one of the first ten rows in view and the rest going to the spec
func (l *lister) leftover(text string) tea.Cmd {
	if l.quick != nil && len(text) == 1 && text[0] >= '0' && text[0] <= '9' {
		n := int(text[0] - '0')
		if n == 0 {
			n = 10
		}
		if row := l.start + n - 1; row < len(l.shown) && n <= l.seen {
			l.sel = row
			return l.quick(l, l.index())
		}
		return nil
	}
	if l.typed != nil {
		return l.typed(l, text, false)
	}
	return nil
}

// Reads the rows again and keeps those holding the filter text, following the newest row when asked
func (l *lister) refresh() {
	last := l.sel >= len(l.shown)-1
	if l.rows != nil {
		l.data = l.rows()
	}
	l.needOK = false
	terms := strings.Fields(strings.ToLower(l.bar.Query()))
	narrowed := len(terms) > 0
	asked := strings.Join(terms, " ")
	switch {
	case narrowed && !l.narrowed:
		l.before, l.sel, last = l.sel, 0, false
	case !narrowed && l.narrowed:
		l.sel, last = l.before, false
	case narrowed && asked != l.asked:
		l.sel, last = 0, false
	}
	l.narrowed, l.asked = narrowed, asked
	l.shown = l.shown[:0]
	for i, r := range l.data {
		if len(terms) == 0 {
			l.shown = append(l.shown, i)
			continue
		}
		text := strings.ToLower(ansi.Strip(strings.Join(r, " ")))
		if l.words != nil {
			text += " " + strings.ToLower(l.words(i))
		}
		if !slices.ContainsFunc(terms, func(term string) bool { return !strings.Contains(text, term) }) {
			l.shown = append(l.shown, i)
		}
	}
	if l.follow && last {
		l.sel = len(l.shown) - 1
	}
	l.sel = min(max(l.sel, 0), max(0, len(l.shown)-1))
}

// Reports whether the cell of a row of the data in a column of the table is wider than the column, so the table cuts it
func (l *lister) cut(i, col int) bool {
	col += l.shift
	if i < 0 || i >= len(l.data) || col-l.shift >= len(l.data[i]) || col >= len(l.widths) {
		return false
	}
	return ansi.StringWidth(ansi.Strip(l.data[i][col-l.shift])) > l.widths[col]
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
	in := field.New(initial)
	in.SelectAll()
	l.edit, l.onDone = &cellEdit{col: col, in: in}, done
}

// Asks a yes or no question along the bottom, running yes on a yes
func (l *lister) confirm(text string, yes func() tea.Cmd) { l.ask = &question{text: text, yes: yes} }

// Reports whether a cell is being typed into
func (l *lister) Editing() bool { return l.edit != nil }

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
		switch in := l.typingInto(); {
		case l.capture != nil:
			if chord, ok := keys.Captured(msg); ok {
				done := l.capture
				l.capture = nil
				return l, done(l, chord)
			}
		case in != nil:
			l.took(in.Take(msg))
		case l.ask != nil:
		case msg.Text != "":
			return l, l.leftover(msg.Text)
		}
	case tea.PasteMsg, tea.ClipboardMsg:
		switch in := l.typingInto(); {
		case in != nil:
			l.took(in.Take(msg))
		case l.capture != nil || l.ask != nil:
		case l.typed != nil:
			if paste, ok := msg.(tea.PasteMsg); ok {
				return l, l.typed(l, paste.Content, true)
			}
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
		at := msg.Y - l.lead - l.head
		if at < 0 || at >= len(l.lineRows) || l.lineRows[at] < 0 {
			return l, nil
		}
		row := l.lineRows[at]
		if row == l.sel && l.primary != "" {
			return l, l.act(l.primary)
		}
		l.sel = row
	case tea.MouseMotionMsg:
		l.pane.drag(msg.Y, l.env)
	case tea.MouseReleaseMsg:
		l.pane.release()
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

// Reports whether a press at the cell takes hold of the scrollbar of the detail pane
func (l *lister) Grabs(x, y int) bool {
	return l.detailShown() && l.edit == nil && l.capture == nil && l.ask == nil && l.pane.grabs(x, y, l.w, l.env)
}

// Carries out a keymap action on the capture, question, edit, filter bar, detail, cursor or tab
func (l *lister) act(id string) tea.Cmd {
	// A list floating in an overlay moves with the palette keys the overlay answers, those its own keys leave unbound such as the paging
	// keys of a list to pick from, unless a question, an edit or a key being recorded holds the keys
	if l.ask == nil && l.edit == nil && l.capture == nil {
		switch id {
		case "palette.up":
			id = "list.up"
		case "palette.down":
			id = "list.down"
		case "palette.page.up":
			id = "list.page.up"
		case "palette.page.down":
			id = "list.page.down"
		}
	}
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
			if l.typing() && l.input.done != nil {
				return l.input.done(l, strings.TrimSpace(l.input.text))
			}
			return nil
		}
		text := strings.TrimSpace(l.edit.in.Value())
		done := l.onDone
		l.edit, l.onDone = nil, nil
		if done != nil {
			return done(l, text)
		}
		return nil
	case "edit.cancel":
		if l.edit == nil && l.typing() {
			if l.input.cancel != nil {
				return l.input.cancel(l)
			}
			l.input.text = ""
			return nil
		}
		l.edit, l.onDone = nil, nil
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
		l.sel = listMove(id, l.sel, len(l.shown), l.h-l.head)
		return nil
	}
	if l.pane.act(id) {
		l.refresh()
		return nil
	}
	if in := l.typingInto(); in != nil && strings.HasPrefix(id, "text.") {
		// Home and end have no caret to move in a search field that holds nothing: they go to the first and the last row, as they do in
		// a list with no field
		if in == l.bar.input() && in.Empty() && (id == "text.line.start" || id == "text.line.end") {
			move := map[string]string{"text.line.start": "list.top", "text.line.end": "list.bottom"}[id]
			l.sel = listMove(move, l.sel, len(l.shown), l.h-l.head)
			return nil
		}
		before := in.Value()
		cmd, _ := in.Action(id)
		l.took(in.Value() != before)
		return cmd
	}
	if fn, ok := l.actions[id]; ok && (l.free[id] || l.index() >= 0) {
		return fn(l, l.index())
	}
	return nil
}

// Lines along the bottom, the key capture prompt or the question, wrapped so it is read whole
func (l *lister) foot(w int) []string {
	t := l.env.Theme
	text, paint := "", render.Bad
	switch {
	case l.capture != nil:
		text, paint = l.asking, render.Warm
	case l.ask != nil:
		text = l.ask.text
	default:
		return nil
	}
	var lines []string
	for _, part := range wrapWords(text, max(w-2, 8)) {
		lines = append(lines, render.Fit(" "+paint(part, t), w, t))
	}
	return lines
}

// Filter field above the table with its count, shown once there is a query or more rows than fit, a short list needing none
func (l *lister) field(w int) string {
	if !l.bar.Shown() || len(l.data) == 0 || l.bar.pinned && !l.always && l.roomy && l.bar.Query() == "" {
		return ""
	}
	// A count says something only once a search narrowed the rows
	count := ""
	if l.bar.Query() != "" {
		count = strconv.Itoa(len(l.shown)) + " found"
	}
	return l.bar.View(w, count, cmp.Or(l.search, "Search"), l.env.Theme)
}

// Lines above the rows: what failed to load, then what the list is
func (l *lister) opening(w int) []string {
	t := l.env.Theme
	var lines []string
	if l.banner != nil {
		if text := l.banner(); text != "" {
			for _, part := range wrapWords(text, max(w-2, 8)) {
				lines = append(lines, render.Fit(" "+render.Warm(part, t), w, t))
			}
		}
	}
	if l.intro == nil {
		return lines
	}
	said := false
	for _, line := range l.intro() {
		for _, part := range wrapWords(line, max(w-4, 8)) {
			lines = append(lines, render.Fit("  "+render.Dim(part, t), w, t))
			said = true
		}
	}
	if said {
		lines = append(lines, "")
	}
	return lines
}

// One line under the head: a shown row, or between groups a heading or the gap over one
type flowLine struct {
	// Shown row on the line, -1 for a heading or a gap
	row int
	// Words of a heading, empty on a row and on a gap
	heading string
}

// Lines under the head: every shown row, the heading of its group over the first row of each group and a gap over each heading but the
// first. A filter drops the headings
func (l *lister) flow() []flowLine {
	lines := make([]flowLine, 0, len(l.shown))
	grouped := l.group != nil && l.bar.Query() == ""
	last := ""
	for at, i := range l.shown {
		if grouped {
			if heading := l.group(i); heading != last {
				if heading != "" {
					if at > 0 {
						lines = append(lines, flowLine{row: -1})
					}
					lines = append(lines, flowLine{row: -1, heading: heading})
				}
				last = heading
			}
		}
		lines = append(lines, flowLine{row: at})
	}
	return lines
}

// First line of the flow to show in body lines so the cursor row is in view, with the heading over it when it leads its group
func (l *lister) scroll(flow []flowLine, body int) int {
	at := slices.IndexFunc(flow, func(line flowLine) bool { return line.row == l.sel })
	if at < 0 || body <= 0 || len(flow) <= body {
		return 0
	}
	top := min(l.top, len(flow)-body)
	if at < top {
		top = at
	}
	if at >= top+body {
		top = at - body + 1
	}
	// A heading and the gap over it come into view with the first row under them
	for lead := at; lead > 0 && flow[lead-1].row < 0 && at-(lead-1) < body; lead-- {
		top = min(top, lead-1)
	}
	return max(top, 0)
}

func (l *lister) View(w, h int) string {
	l.w, l.h = w, h
	t := l.env.Theme
	if l.edit == nil {
		l.refresh()
	}
	head := l.opening(w)
	cols := l.cols
	l.shift = 0
	if l.quick != nil {
		l.shift = 1
		cols = append([]render.Column{{Title: "#", Right: true}}, cols...)
	}
	l.head = render.Head(cols)
	flow := l.flow()
	// The widths of the columns are known before the detail of a row is asked for, which may say what its row had to cut
	measured := make([][]string, len(l.shown))
	for at, i := range l.shown {
		measured[at] = l.data[i]
		if l.shift > 0 {
			measured[at] = append([]string{"0"}, l.data[i]...)
		}
	}
	l.xs, l.widths = render.Columns(cols, measured, w)
	pane := l.paneRows(w, h-len(head))
	// Whether the rows fit is told without the detail, which grows and shrinks with the row under the cursor, so the filter field does
	// not come and go as the cursor moves
	l.roomy = len(flow)+l.head <= h-len(head)
	l.barred = false
	if field := l.field(w); field != "" {
		l.barred, l.barY = true, len(head)
		head = append(head, field, "")
	}
	l.lead = len(head)
	foot := l.foot(w)
	body := h - l.lead - len(foot)
	var rows [][]string
	for _, i := range l.shown {
		rows = append(rows, l.data[i])
	}
	if len(rows) == 0 {
		l.pane.hide()
		l.start, l.seen, l.lineRows = 0, 0, nil
		empty := "Nothing here yet."
		switch {
		case l.bar.Query() != "":
			empty = "Nothing matches “" + l.bar.Query() + "”. " + l.env.keyed("search.close", "clears the search.")
		case l.empty != nil:
			empty = l.empty()
		}
		return footed(append(head, strings.Split(emptyView(w, body, render.Wrap(empty, max(w-2, 1)), t), "\n")...), foot, h)
	}
	ed := l.cell()
	room := body
	if pane > 0 {
		room = max(body-pane-1, l.head+2)
	}
	l.top = l.scroll(flow, room-l.head)
	shown := flow[l.top:min(l.top+max(room-l.head, 0), len(flow))]
	l.lineRows = l.lineRows[:0]
	l.start, l.seen, l.selY = len(l.shown), 0, 0
	for y, line := range shown {
		l.lineRows = append(l.lineRows, line.row)
		if line.row < 0 {
			continue
		}
		l.start = min(l.start, line.row)
		l.seen++
		if line.row == l.sel {
			l.selY = y
		}
	}
	if l.quick != nil {
		numbered := make([][]string, len(rows))
		for i, r := range rows {
			digit := ""
			if n := i - l.start + 1; n >= 1 && n <= 10 {
				digit = string(rune('0' + n%10))
			}
			numbered[i] = append([]string{digit}, r...)
		}
		rows = numbered
		if ed != nil {
			ed = &render.Edit{Col: ed.Col + 1, Text: ed.Text, Cursor: ed.Cursor, Selected: ed.Selected}
		}
	}
	l.xs, l.widths = render.Columns(cols, rows, w)
	table := strings.Split(render.TableEdit(cols, rows, w, l.sel, ed, t), "\n")
	lines := append(head, table[:l.head]...)
	for _, line := range shown {
		switch {
		case line.row >= 0:
			lines = append(lines, table[l.head+line.row])
		case line.heading != "":
			lines = append(lines, render.Fit("  "+render.Group(line.heading, t), w, t))
		default:
			lines = append(lines, "")
		}
	}
	// A row with nothing to say under the list takes no rule and no room
	if pane == 0 || !l.detailShown() {
		l.pane.hide()
		return footed(lines, foot, h)
	}
	lines = append(lines, render.Divider(w, t))
	rest := h - len(lines) - len(foot)
	i := l.index()
	if pane := l.pane.view(l.detail(i), i, len(lines), rest, w, l.env); rest > 0 {
		lines = append(lines, strings.Split(pane, "\n")...)
	}
	return footed(lines, foot, h)
}

// Trailer on the frame, what the tab says about its rows and then how many of them a scrolled table hides above and below
func (l *lister) Status() string {
	var parts []string
	if l.status != nil {
		if text := l.status(); text != "" {
			parts = append(parts, text)
		}
	}
	if above, below := l.start, len(l.shown)-l.start-l.seen; l.seen > 0 && (above > 0 || below > 0) {
		parts = append(parts, wm.HiddenText(above, max(below, 0)))
	}
	return strings.Join(parts, " "+l.env.Theme.Glyphs.Sep+" ")
}

// Cursor in the filter bar while it takes typing, else at the end of the cell being typed into, the start of the row input while it is empty
func (l *lister) Cursor() *tea.Cursor {
	if l.bar.Active() && l.barred && l.edit == nil && l.capture == nil && l.ask == nil {
		return tea.NewCursor(l.bar.CursorX(), l.barY)
	}
	col, n := -1, 0
	switch {
	case l.edit != nil:
		col, n = l.edit.col, ansi.StringWidth(string([]rune(l.edit.in.Value())[:l.edit.in.Cursor()]))
	case l.typing():
		col, n = l.input.col, ansi.StringWidth(string([]rune(l.input.text)[:l.input.field().Cursor()]))
	}
	if col < 0 {
		return nil
	}
	col += l.shift
	if col >= len(l.xs) || l.widths[col] == 0 {
		return nil
	}
	y := l.lead + l.head + l.selY
	x := l.xs[col] + min(n, max(l.widths[col]-1, 0))
	return tea.NewCursor(x, y)
}

// Buttons of the table now: the answers to a question, the way out of a key capture, else what can be done with the row under the cursor
func (l *lister) Hints() []wm.Hint {
	switch {
	case l.capture != nil:
		return []wm.Hint{{Action: "capture.cancel"}}
	case l.ask != nil:
		return []wm.Hint{{Action: "confirm.yes"}, {Action: "confirm.no"}}
	case l.edit != nil:
		return nil
	}
	if l.hints != nil {
		return l.hints(l, l.index())
	}
	if l.typing() {
		return nil
	}
	var out []wm.Hint
	for _, a := range l.env.Keys.InContexts([]string{l.ctx}) {
		if _, ok := l.actions[a.ID]; ok && a.Context == l.ctx && (l.free[a.ID] || l.index() >= 0) {
			out = append(out, wm.Hint{Action: a.ID})
		}
	}
	return out
}

// Rows the detail pane keeps out of h at width w: as many as the longest detail of a row wraps to, so the table holds still while the
// cursor moves, up to detailRows and half of h, none while the pane is closed
func (l *lister) paneRows(w, h int) int {
	if !l.detailShown() {
		return 0
	}
	if !l.needOK || l.needW != w {
		l.need, l.needW, l.needOK = 0, w, true
		for i := range l.data {
			if l.need = max(l.need, len(l.pane.wrap(l.detail(i), w))); l.need >= detailRows {
				break
			}
		}
	}
	return min(l.need, detailRows, max(h/2, 1))
}

func toggleDetail(l *lister, i int) tea.Cmd {
	l.expanded = !l.expanded
	return nil
}
