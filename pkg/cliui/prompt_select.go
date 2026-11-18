package cliui

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	uv "github.com/charmbracelet/ultraviolet"
)

// Row of a list prompt, a choice or an unselectable heading above the next choice
type listRow struct {
	item      RadioItem
	primary   string
	secondary string
	tag       string
	heading   string
	index     int
	markLeads bool
	cells     [3]measured
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
	laid       *listLayout
	rowScroll  int
	rowBelow   int
	rowAt      int
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

// Line breaks and tabs of a label as the single spaces a choice draws them as
var lineBreaks = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ", "\t", " ")

// Rows of a list from its items, a heading row before each item that opens a section
func buildListRows(items []RadioItem, headings map[int]string) []listRow {
	rows := make([]listRow, 0, len(items)+len(headings))
	marked := 0
	for i, item := range items {
		if h, ok := headings[i]; ok {
			rows = append(rows, listRow{heading: h, index: -1})
		}
		row := listRow{item: item, index: i}
		if len(item) == 1 && item[0].Style == "" {
			row.primary, row.secondary, row.tag = splitLabel(strings.TrimSpace(lineBreaks.Replace(item[0].Text)))
			row.primary, row.tag = liftBracketTag(row.primary, row.tag)
		}
		if row.tag != "" {
			marked++
		}
		rows = append(rows, row)
	}
	for i := range rows {
		rows[i].measure(marked*3 >= len(items))
	}
	return rows
}

// Measures the cells of a row, its state mark leading the explanation unless marks have a column of their own
func (r *listRow) measure(markColumn bool) {
	switch {
	case r.index < 0:
		r.cells[0] = measure(r.heading)
	case r.primary == "":
		r.cells[0] = measure(RadioItemPlain(r.item))
	case markColumn || r.tag == "":
		r.cells = [3]measured{measure(r.primary), measure(r.tag), measure(r.secondary)}
	default:
		r.markLeads = true
		r.cells = [3]measured{measure(r.primary), {}, measure(strings.TrimSuffix(r.tag+"  "+r.secondary, "  "))}
	}
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

// Styled runs of a segment row, plain runs bold and frosted under the cursor
func (p *listPrompt) segments(item RadioItem, atCursor bool) []span {
	t := promptTheme
	spans := make([]span, 0, len(item))
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
		spans = append(spans, span{seg.Text, style})
	}
	return spans
}

// Key hint rows for the state the prompt is in
func (p *listPrompt) hintLines(body int) []string {
	g, t := p.c.glyphs(), promptTheme
	if p.escArmed {
		return p.c.hintRows([]string{"press esc again to cancel"}, body, t.Warn)
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
	return p.c.hintRows(parts, body, t.Faint)
}

// Painted description rows wrapped to the body, stars in the warning tone and usage gauges by how full they are
func (p *listPrompt) descLines(body int) []string {
	t := promptTheme
	var out []string
	for _, text := range p.desc {
		var spans []span
		for i, part := range strings.Split(strings.TrimSpace(text), "★") {
			if i > 0 {
				spans = append(spans, span{"★", textStyle{color: t.Warn}})
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
				spans = append(spans, span{seg.Text, style})
			}
		}
		indent := 4
		if lead := len(text) - len(strings.TrimLeft(text, " ")); lead >= descRowColumn {
			indent = max(min(6+lead-descRowColumn, body-8), 4)
		}
		pad := strings.Repeat(" ", indent)
		out = append(out, hung(p.c.spanRows(measureSpans(spans), spans, body+4-indent), pad, pad)...)
	}
	return out
}

// Leading spaces a description line carries when it lines up with the row text under it
const descRowColumn = 5

// Description rows that fit a budget of rows, the last ones counting the rows left out
func (p *listPrompt) descShown(desc []string, budget, body int) []string {
	if budget >= len(desc) {
		return desc
	}
	g, t := p.c.glyphs(), promptTheme
	for keep := budget - 1; keep >= 0; keep-- {
		more := fmt.Sprintf("%s %d more description lines (enlarge terminal)", g.ellipsis, len(desc)-keep)
		note := hung(p.c.textRows(more, textStyle{color: t.Faint}, body), "    ", "    ")
		if keep+len(note) <= budget {
			return append(desc[:keep:keep], note...)
		}
	}
	return nil
}

// Rows of the filter query after its slash, the caret closing the last row while the filter is open
func (p *listPrompt) searchLines(body int) []string {
	t := promptTheme
	rows := measure(p.search.Query).solid().rows(body - 4)
	if len(rows) == 0 {
		rows = []string{""}
	}
	if p.search.Active {
		rows[len(rows)-1] += p.c.paint(" ", textStyle{reverse: true})
	}
	return hung(rows, "    "+p.c.paint("/", textStyle{color: t.Frost, bold: true})+" ", "      ")
}

// Lines of the prompt for a terminal of maxRows by cols, the list scrolled to its cursor row
func (p *listPrompt) frame(maxRows, cols int) []string {
	g, t := p.c.glyphs(), promptTheme
	body := bodyWidth(cols)
	lay := p.layout(body)
	lines := p.c.titleLines(p.title, body)
	if most := max(maxRows-4, 1); len(lines) > most {
		lines = lines[:most]
	}
	shown := p.shown()
	var search, status []string
	if p.search.Active || p.search.Query != "" {
		search = p.searchLines(body)
	}
	if p.status != nil {
		if text := p.status(p.checked); text != "" {
			status = hung(p.c.textRows(text, textStyle{color: t.Muted}, body), "    ", "    ")
		}
	}
	hint, desc := p.hintLines(body), p.descLines(body)
	avail := maxRows - len(lines) - len(search) - len(status)
	if avail-len(hint) < 5 {
		hint = nil
	}
	avail -= len(hint) + 2
	total, want, keep := lay.tallest(shown, len(shown)), lay.tallest(shown, listWindow), lay.tallest(shown, 5)
	descRows, view := split(len(desc), avail, keep, want)
	if total > view {
		descRows, view = split(len(desc), avail-1, keep, want)
	}
	lines = append(lines, p.descShown(desc, descRows, body)...)
	if p.rowAt != p.cursor {
		p.rowAt, p.rowScroll = p.cursor, 0
	}
	cursorPos := max(indexOf(shown, p.cursor), 0)
	first, end := p.window(lay, shown, cursorPos, view)
	over := 0
	if len(shown) > 0 {
		over = max(lay.heights[shown[cursorPos]]-view, 0)
	}
	p.rowScroll = min(p.rowScroll, over)
	p.rowBelow = over - p.rowScroll
	var rows []string
	for i := first; i < len(shown) && len(rows) < p.rowScroll+view; i++ {
		rows = append(rows, p.rowLines(p.rows[shown[i]], shown[i] == p.cursor, lay)...)
	}
	rows = rows[min(p.rowScroll, len(rows)):min(p.rowScroll+view, len(rows))]
	lines = append(append(lines, p.hidden(g.up, p.rowScroll, first)), search...)
	if len(shown) == 0 {
		lines = append(lines, "    "+p.c.paint("No matches", textStyle{color: t.Muted}))
	}
	lines = append(lines, rows...)
	if total > view {
		lines = append(lines, p.hidden(g.down, p.rowBelow, len(shown)-end))
		for range view - len(rows) {
			lines = append(lines, "")
		}
	}
	lines = append(lines, "")
	return append(append(lines, status...), hint...)
}

// Count beside an arrow of what lies past an edge of the view, cursor row lines before whole rows
func (p *listPrompt) hidden(arrow string, cut, rows int) string {
	faint := textStyle{color: promptTheme.Faint}
	switch {
	case cut == 1:
		return "    " + p.c.paint(arrow+" 1 line", faint)
	case cut > 1:
		return "    " + p.c.paint(fmt.Sprintf("%s %d lines", arrow, cut), faint)
	case rows > 0:
		return "    " + p.c.paint(fmt.Sprintf("%s %d more", arrow, rows), faint)
	}
	return ""
}

// Moves one choice up or down, first scrolling line by line through a cursor row taller than the view
func (p *listPrompt) step(delta int) {
	switch {
	case delta > 0 && p.rowBelow > 0:
		p.rowScroll++
	case delta < 0 && p.rowScroll > 0:
		p.rowScroll--
	default:
		p.move(delta, true)
	}
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
		p.step(-1)
	case k.Code == uv.KeyDown || (plain && k.Text == "j" && !p.search.Active):
		p.step(1)
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
	t := promptTheme
	switch outcome {
	case listBack:
		return nil
	case listCancelled, listInterrupted:
		return p.c.answerLines(p.title, "cancelled", textStyle{color: t.Faint}, "")
	}
	if !p.multi {
		return p.c.answerLines(p.title, p.rowName(p.rows[p.cursor]), textStyle{}, "")
	}
	var names []string
	for _, row := range p.rows {
		if row.index >= 0 && p.checked[row.index] {
			names = append(names, p.rowName(row))
		}
	}
	if len(names) == 0 {
		return p.c.answerLines(p.title, "none", textStyle{color: t.Muted}, "")
	}
	text := strings.Join(names, ", ")
	if len(names) > 3 {
		text = fmt.Sprintf("%d selected", len(names))
	}
	return p.c.answerLines(p.title, text, textStyle{}, "")
}

// Runs the prompt on the terminal until a key ends it, leaving one answer line in its place
func (p *listPrompt) run() (outcome listOutcome, err error) {
	release, err := p.c.holdRaw()
	if err != nil {
		return listCancelled, err
	}
	defer release()
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
