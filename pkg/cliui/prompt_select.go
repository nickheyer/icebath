package cliui

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

// Row of a list prompt, a choice or an unselectable heading above the next choice
type listRow struct {
	item      RadioItem
	primary   string
	secondary string
	tag       string
	heading   string
	index     int
}

// What ended a list prompt
type listOutcome int

const (
	listPicked listOutcome = iota
	listCancelled
	listInterrupted
	listBack
)

// What one key press asks a list prompt to do
type listAction int

const (
	actNone listAction = iota
	actRedraw
	actPick
	actCancel
	actInterrupt
	actBack
	actSuspend
)

// Most choices a list shows at once before it scrolls
const listWindow = 12

// One inline list, single or multiple choice, with search, scrolling and scoped navigation keys
type listPrompt struct {
	c          *Console
	title      string
	desc       []string
	rows       []listRow
	labels     []string
	cursor     int
	scroll     int
	multi      bool
	checked    map[int]bool
	status     func(map[int]bool) string
	searchable bool
	search     SearchState
	allowBack  bool
	guardEsc   bool
	escArmed   bool
	yesNo      bool
}

// Splits a plain choice label into its name, the explanation that trails it and a state mark after an arrow
func splitLabel(label string) (name, explanation, tag string) {
	if i := strings.Index(label, "  ← "); i > 0 {
		label, tag = strings.TrimSpace(label[:i]), strings.TrimSpace(label[i+len("  ← "):])
	}
	paren := -1
	if strings.HasSuffix(label, ")") {
		depth := 0
		for i := len(label) - 1; i > 0; i-- {
			switch label[i] {
			case ')':
				depth++
			case '(':
				depth--
			}
			if depth == 0 {
				if label[i-1] == ' ' {
					paren = i - 1
				}
				break
			}
		}
	}
	for _, sep := range []string{" — ", " – ", " - "} {
		if i := strings.Index(label, sep); i > 0 && (paren < 0 || i < paren) {
			return strings.TrimSpace(label[:i]), strings.TrimSpace(label[i+len(sep):]), tag
		}
	}
	if paren > 0 {
		return strings.TrimSpace(label[:paren]), label[paren+2 : len(label)-1], tag
	}
	return label, "", tag
}

var bracketTagRe = regexp.MustCompile(`^(.*\S)\s+\[([^\[\]]+)\]$`)

// Moves a bracketed note at the end of a name into the state mark beside it
func liftBracketTag(name, tag string) (string, string) {
	m := bracketTagRe.FindStringSubmatch(name)
	if m == nil {
		return name, tag
	}
	if tag != "" {
		return m[1], m[2] + " · " + tag
	}
	return m[1], m[2]
}

// Rows of a list from its items, a heading row before each item that opens a section
func buildListRows(items []RadioItem, headings map[int]string) []listRow {
	rows := make([]listRow, 0, len(items)+len(headings))
	for i, item := range items {
		if h, ok := headings[i]; ok {
			rows = append(rows, listRow{heading: h, index: -1})
		}
		row := listRow{item: item, index: i}
		if len(item) == 1 && item[0].Style == "" {
			row.primary, row.secondary, row.tag = splitLabel(strings.TrimSpace(item[0].Text))
			row.primary, row.tag = liftBracketTag(row.primary, row.tag)
		}
		rows = append(rows, row)
	}
	return rows
}

// Indices of the rows shown, every row without a query and the matching choices best first with one
func (p *listPrompt) shown() []int {
	if !p.searchable || strings.TrimSpace(p.search.Query) == "" {
		all := make([]int, len(p.rows))
		for i := range all {
			all[i] = i
		}
		return all
	}
	return FilterIndices(p.labels, p.search.Query)
}

// Shown rows a cursor can rest on
func (p *listPrompt) choices(shown []int) []int {
	out := make([]int, 0, len(shown))
	for _, i := range shown {
		if p.rows[i].index >= 0 {
			out = append(out, i)
		}
	}
	return out
}

// Moves the cursor by delta choices, wrapping around the ends when wrap is set
func (p *listPrompt) move(delta int, wrap bool) {
	nav := p.choices(p.shown())
	if len(nav) == 0 {
		return
	}
	pos := max(indexOf(nav, p.cursor), 0)
	next := pos + delta
	if wrap {
		next = ((next % len(nav)) + len(nav)) % len(nav)
	}
	p.cursor = nav[min(max(next, 0), len(nav)-1)]
}

// Keeps the cursor on a shown choice after the shown rows changed
func (p *listPrompt) reconcile() {
	nav := p.choices(p.shown())
	p.cursor, _ = ReconcileCursor(nav, p.cursor)
}

// Painted text of a styled row, plain runs bold and frosted under the cursor
func (p *listPrompt) segments(item RadioItem, atCursor bool) string {
	t := activeTheme()
	var b strings.Builder
	for _, seg := range item {
		style := textStyle{}
		switch seg.Style {
		case "yellow":
			style.color = t.Warn
		case "dim":
			style.color = t.Muted
		case "green":
			style.color = t.OK
		case "red":
			style.color = t.Err
		default:
			if atCursor {
				style = textStyle{color: t.Frost, bold: true}
			}
		}
		b.WriteString(p.c.paint(seg.Text, style))
	}
	return b.String()
}

// Widest name column a scrolling list draws before a longer name pushes its own explanation right
const nameColumnCap = 26

// Widest name column of a list short enough to show every row at once
const wideNameColumnCap = 40

// Width of the name column, the widest name that has an explanation up to the cap
func (p *listPrompt) nameColumn(body int) int {
	widest := 0
	for _, row := range p.rows {
		if row.secondary != "" || row.tag != "" {
			widest = max(widest, ansi.StringWidth(row.primary))
		}
	}
	limit := nameColumnCap
	if len(p.rows) <= listWindow {
		limit = wideNameColumnCap
	}
	return min(widest, max(body*45/100, 8), limit)
}

// Widest state mark a list lines up in its own column
const tagColumnCap = 30

// Width of the state mark column, zero while fewer than a third of the choices carry a mark
func (p *listPrompt) tagColumn() int {
	tagged, choices, widest := 0, 0, 0
	for _, row := range p.rows {
		if row.index < 0 {
			continue
		}
		choices++
		if row.tag != "" {
			tagged++
			widest = max(widest, ansi.StringWidth(row.tag))
		}
	}
	if tagged*3 < choices {
		return 0
	}
	return min(widest, tagColumnCap)
}

// One painted row of the list at the body width
func (p *listPrompt) rowLine(row listRow, atCursor bool, nameCol, body int) string {
	g, t := p.c.glyphs(), activeTheme()
	if row.index < 0 {
		return "    " + p.c.paint(p.c.clip(row.heading, body), textStyle{color: t.Muted, bold: true})
	}
	prefix := "  "
	if atCursor {
		prefix = p.c.paint(g.cursor, textStyle{color: t.Accent, bold: true}) + " "
	}
	used := 2
	if p.multi {
		box := p.c.paint(g.unchecked, textStyle{color: t.Faint})
		if p.checked[row.index] {
			box = p.c.paint(g.checked, textStyle{color: t.OK})
		}
		prefix += box + " "
		used += ansi.StringWidth(g.checked) + 1
	}
	room := max(body-used, 4)
	if row.primary == "" {
		return "    " + prefix + p.c.clip(p.segments(row.item, atCursor), room)
	}
	nameStyle := textStyle{}
	if atCursor {
		nameStyle = textStyle{color: t.Frost, bold: true}
	}
	name := p.c.clip(row.primary, room)
	line := "    " + prefix + p.c.paint(name, nameStyle)
	if row.secondary == "" && row.tag == "" {
		return line
	}
	width := max(ansi.StringWidth(name), nameCol)
	line += strings.Repeat(" ", width-ansi.StringWidth(name))
	rest := room - width - 2
	tagCol := p.tagColumn()
	if (row.tag != "" || tagCol > 0) && rest >= 6 {
		tag := p.c.clip(row.tag, min(rest, max(tagCol, ansi.StringWidth(row.tag))))
		pad := max(tagCol-ansi.StringWidth(tag), 0)
		line += "  " + p.c.paint(tag, textStyle{color: t.Frost}) + strings.Repeat(" ", pad)
		rest -= ansi.StringWidth(tag) + pad + 2
	}
	if row.secondary != "" && rest >= 6 {
		line += "  " + p.c.paint(p.c.clip(row.secondary, rest), textStyle{color: t.Muted})
	}
	return line
}

// Key hints for the state the prompt is in
func (p *listPrompt) hint() string {
	g, t := p.c.glyphs(), activeTheme()
	if p.escArmed {
		return "    " + p.c.paint("press esc again to cancel", textStyle{color: t.Warn})
	}
	var parts []string
	switch {
	case p.search.Active:
		parts = []string{"type to filter", "enter select", "esc clear"}
	case p.multi:
		parts = []string{g.arrows + " move", "space toggle", "enter confirm"}
	default:
		parts = []string{g.arrows + " move", "enter select"}
	}
	if !p.search.Active {
		if p.searchable {
			parts = append(parts, "/ filter")
		}
		if p.allowBack {
			parts = append(parts, g.back+" back")
		}
		parts = append(parts, "esc cancel")
	}
	return "    " + p.c.paint(strings.Join(parts, "  "+g.sep+"  "), textStyle{color: t.Faint})
}

// Painted description line, stars in the warning tone and usage gauges by how full they are
func (p *listPrompt) descLine(text string, body int) string {
	t := activeTheme()
	var b strings.Builder
	for i, part := range strings.Split(strings.TrimSpace(text), "★") {
		if i > 0 {
			b.WriteString(p.c.paint("★", textStyle{color: t.Warn}))
		}
		for _, seg := range GaugeBarSegments(part) {
			style := textStyle{color: t.Muted}
			switch seg.Style {
			case "green":
				style.color = t.OK
			case "yellow":
				style.color = t.Warn
			case "red":
				style.color = t.Err
			}
			b.WriteString(p.c.paint(seg.Text, style))
		}
	}
	indent := 4
	if lead := len(text) - len(strings.TrimLeft(text, " ")); lead >= descRowColumn {
		indent = 6 + lead - descRowColumn
	}
	return strings.Repeat(" ", indent) + p.c.clip(b.String(), body-indent+4)
}

// Leading spaces a description line carries when it lines up with the row text under it
const descRowColumn = 7

// Lines of the prompt for a terminal of maxRows by cols, the list scrolled to keep the cursor in view
func (p *listPrompt) frame(maxRows, cols int) []string {
	g, t := p.c.glyphs(), activeTheme()
	body := max(min(cols-1, 112)-4, 12)
	lines := p.c.titleLines(p.title, body)
	if len(lines) > 3 {
		lines = lines[:3]
	}
	shown := p.shown()
	total := max(len(shown), 1)
	searchRows := 0
	if p.search.Active || p.search.Query != "" {
		searchRows = 1
	}
	statusText := ""
	if p.status != nil {
		statusText = p.status(p.checked)
	}
	statusRows := 0
	if statusText != "" {
		statusRows = 1
	}
	avail := maxRows - len(lines) - searchRows - statusRows
	hintRows := 1
	if avail-hintRows < 5 {
		hintRows = 0
	}
	avail -= hintRows + 2
	want := min(total, listWindow)
	descRows, listRows := p.split(avail, total, want)
	if total > listRows {
		descRows, listRows = p.split(avail-1, total, want)
	}
	for i := 0; i < descRows; i++ {
		if i == descRows-1 && descRows < len(p.desc) {
			more := fmt.Sprintf("%s %d more description lines (enlarge terminal)", g.ellipsis, len(p.desc)-descRows+1)
			lines = append(lines, "    "+p.c.paint(p.c.clip(more, body), textStyle{color: t.Faint}))
			break
		}
		lines = append(lines, p.descLine(p.desc[i], body))
	}
	cursorPos := max(indexOf(shown, p.cursor), 0)
	p.scroll = ScrollForCursor(p.scroll, cursorPos, listRows, len(shown))
	end := min(p.scroll+listRows, len(shown))
	above := ""
	if p.scroll > 0 {
		above = "    " + p.c.paint(fmt.Sprintf("%s %d more", g.up, p.scroll), textStyle{color: t.Faint})
	}
	lines = append(lines, above)
	if searchRows == 1 {
		caret := p.c.paint(" ", textStyle{reverse: true})
		if !p.search.Active {
			caret = ""
		}
		lines = append(lines, "    "+p.c.paint("/", textStyle{color: t.Frost, bold: true})+" "+p.c.clip(p.search.Query, body-4)+caret)
	}
	if len(shown) == 0 {
		lines = append(lines, "    "+p.c.paint("No matches", textStyle{color: t.Muted}))
	}
	nameCol := p.nameColumn(body)
	for _, i := range shown[min(p.scroll, end):end] {
		lines = append(lines, p.rowLine(p.rows[i], i == p.cursor, nameCol, body))
	}
	if len(shown) > listRows {
		below := ""
		if rest := len(shown) - end; rest > 0 {
			below = "    " + p.c.paint(fmt.Sprintf("%s %d more", g.down, rest), textStyle{color: t.Faint})
		}
		lines = append(lines, below)
	}
	lines = append(lines, "")
	if statusRows == 1 {
		lines = append(lines, "    "+p.c.paint(p.c.clip(statusText, body), textStyle{color: t.Muted}))
	}
	if hintRows > 0 {
		lines = append(lines, p.hint())
	}
	return lines
}

// Rows given to the description and to the list out of the rows available, five choices kept in view first
func (p *listPrompt) split(avail, total, want int) (descRows, listRows int) {
	if len(p.desc) > 0 {
		descRows = min(len(p.desc), avail-min(total, 5))
		if descRows < 2 && descRows < len(p.desc) {
			descRows = 0
		}
	}
	return descRows, min(want, max(avail-descRows, 1))
}

// Edits the search query with one key, reporting whether the key was used and whether it confirms the match
func (p *listPrompt) searchKey(k uv.Key) (used, confirm bool) {
	switch {
	case k.Code == uv.KeyEscape:
		p.search.Active, p.search.Query = false, ""
	case k.Code == uv.KeyEnter || k.Code == uv.KeyKpEnter || isCtrl(k, 'j') || isCtrl(k, 'm'):
		return true, true
	case k.Code == uv.KeyBackspace || isCtrl(k, 'h'):
		if _, size := utf8.DecodeLastRuneInString(p.search.Query); size > 0 {
			p.search.Query = p.search.Query[:len(p.search.Query)-size]
		}
	case isCtrl(k, 'u'):
		p.search.Query = ""
	case k.Text != "" && k.Mod&(uv.ModCtrl|uv.ModAlt) == 0:
		p.search.Query += k.Text
	default:
		return false, false
	}
	p.bestMatch()
	return true, false
}

// Puts the cursor on the best match of the query and the list back at its top
func (p *listPrompt) bestMatch() {
	p.scroll = 0
	if nav := p.choices(p.shown()); len(nav) > 0 {
		p.cursor = nav[0]
	}
}

// Applies one key press and reports what it asks for
func (p *listPrompt) key(k uv.Key) listAction {
	armed := p.escArmed
	p.escArmed = false
	if isCtrl(k, 'c') {
		return actInterrupt
	}
	if isCtrl(k, 'd') {
		return actCancel
	}
	if isCtrl(k, 'z') {
		return actSuspend
	}
	if p.search.Active {
		if used, confirm := p.searchKey(k); confirm {
			return p.pick()
		} else if used {
			return actRedraw
		}
	}
	plain := k.Mod == 0
	switch {
	case k.Code == uv.KeyUp || (plain && k.Text == "k" && !p.search.Active):
		p.move(-1, true)
	case k.Code == uv.KeyDown || (plain && k.Text == "j" && !p.search.Active):
		p.move(1, true)
	case k.Code == uv.KeyPgUp:
		p.move(-listWindow, false)
	case k.Code == uv.KeyPgDown:
		p.move(listWindow, false)
	case k.Code == uv.KeyHome:
		p.move(-len(p.rows), false)
	case k.Code == uv.KeyEnd:
		p.move(len(p.rows), false)
	case k.Code == uv.KeyEnter || k.Code == uv.KeyKpEnter || isCtrl(k, 'j') || isCtrl(k, 'm'):
		return p.pick()
	case k.Code == uv.KeySpace && plain:
		if !p.multi {
			return p.pick()
		}
		if p.cursor >= 0 && p.cursor < len(p.rows) && p.rows[p.cursor].index >= 0 {
			if i := p.rows[p.cursor].index; p.checked[i] {
				delete(p.checked, i)
			} else {
				p.checked[i] = true
			}
		}
	case k.Code == uv.KeyEscape:
		if p.guardEsc && !armed && k.Mod&uv.ModAlt == 0 {
			p.escArmed = true
			return actRedraw
		}
		return actCancel
	case plain && k.Text == "q":
		return actCancel
	case k.Code == uv.KeyLeft && plain:
		if p.allowBack {
			return actBack
		}
	case plain && k.Text == "/" && p.searchable:
		p.search.Active = true
	case p.yesNo && plain && (k.Text == "y" || k.Text == "Y"):
		return p.pickIndex(0)
	case p.yesNo && plain && (k.Text == "n" || k.Text == "N"):
		return p.pickIndex(1)
	default:
		return actNone
	}
	return actRedraw
}

// Confirms the choice under the cursor when the shown rows hold one
func (p *listPrompt) pick() listAction {
	nav := p.choices(p.shown())
	if len(nav) == 0 || indexOf(nav, p.cursor) < 0 {
		return actNone
	}
	return actPick
}

// Moves the cursor to the choice with the item index and confirms it
func (p *listPrompt) pickIndex(index int) listAction {
	for i, row := range p.rows {
		if row.index == index {
			p.cursor = i
			return actPick
		}
	}
	return actNone
}

// Plain text of a choice as the answer line names it
func (p *listPrompt) rowName(row listRow) string {
	if row.primary != "" {
		return row.primary
	}
	return strings.Join(strings.Fields(RadioItemPlain(row.item)), " ")
}

// Answer line a finished prompt leaves, naming the choice, the checked items or the cancellation
func (p *listPrompt) answer(outcome listOutcome) []string {
	t := activeTheme()
	switch outcome {
	case listBack:
		return nil
	case listCancelled, listInterrupted:
		return []string{p.c.answerLine(p.title, "cancelled", textStyle{color: t.Faint})}
	}
	if !p.multi {
		return []string{p.c.answerLine(p.title, p.rowName(p.rows[p.cursor]), textStyle{})}
	}
	var names []string
	for _, row := range p.rows {
		if row.index >= 0 && p.checked[row.index] {
			names = append(names, p.rowName(row))
		}
	}
	if len(names) == 0 {
		return []string{p.c.answerLine(p.title, "none", textStyle{color: t.Muted})}
	}
	text := strings.Join(names, ", ")
	if len(names) > 3 {
		text = fmt.Sprintf("%d selected", len(names))
	}
	return []string{p.c.answerLine(p.title, text, textStyle{})}
}

// Runs the prompt on the terminal until a key ends it, leaving one answer line in its place
func (p *listPrompt) run() (outcome listOutcome, err error) {
	release, err := p.c.holdRaw()
	if err != nil {
		return listCancelled, err
	}
	defer release()
	defer p.c.graceQuiet()
	region := p.c.openRegion()
	defer region.close(nil)
	keys := p.c.newKeyReader()
	defer keys.close()
	p.reconcile()
	for {
		_, cols := region.size()
		region.paint(p.frame(region.maxRows(), cols))
		ev, kerr := keys.next()
		if errors.Is(kerr, ErrInterrupt) {
			region.close(p.answer(listInterrupted))
			return listInterrupted, nil
		}
		if kerr != nil {
			return listCancelled, kerr
		}
		switch e := ev.(type) {
		case pasteEvent:
			if p.search.Active {
				p.search.Query += strings.Join(strings.Fields(string(e)), " ")
				p.bestMatch()
			}
		case uv.Key:
			switch p.key(e) {
			case actPick:
				region.close(p.answer(listPicked))
				return listPicked, nil
			case actCancel:
				region.close(p.answer(listCancelled))
				return listCancelled, nil
			case actInterrupt:
				region.close(p.answer(listInterrupted))
				return listInterrupted, nil
			case actBack:
				region.close(nil)
				return listBack, nil
			case actSuspend:
				region.paint(nil)
				p.c.suspend()
			}
		}
	}
}
