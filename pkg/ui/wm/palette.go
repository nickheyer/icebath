package wm

import (
	"image/color"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/nickheyer/icebath/pkg/ui/anim"
	"github.com/nickheyer/icebath/pkg/ui/field"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/lang"
	"github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/txt"
)

// Spring the selection highlight glides between rows with, quick enough to keep up with a held key
var highlightSpring = anim.Spring{Frequency: 26, Damping: 1}

// One row the palette offers
type Item struct {
	ID     string
	Title  string
	Detail string
	Keys   string
	// Heading the row is listed under while nothing is typed, the rows of one heading coming together. Typed text finds the row by the
	// heading too. No headings show when no row names one
	Group string
	// Second listing of a row, as under a heading of the rows used last: left out once text is typed, and out of the count
	Again bool
	Run   func() tea.Cmd
	// Leaves the palette open when picked, for a row that changes what the palette shows
	Stay bool
}

// Line under the rows of a palette, such as where a pick is saved or a warning about the lit row
type Note struct {
	Text  string
	Style SegmentStyle
}

// Yes or no question a palette asks in its field and what a yes runs
type inquiry struct {
	text string
	yes  func() tea.Cmd
}

// Words for the rows a scrolled picker or table hides above and below, replaced by an app that speaks its own language
var HiddenText = func(above, below int) string {
	var parts []string
	if above > 0 {
		parts = append(parts, lang.T(txt.KitWmMoreAbove, above))
	}
	if below > 0 {
		parts = append(parts, lang.T(txt.KitWmMoreBelow, below))
	}
	return strings.Join(parts, " · ")
}

type scored struct {
	item  Item
	score int
	hits  []int
}

// One line of the sheet: a match, the heading of a group, or the gap above a heading
type line struct {
	// Match the line shows, -1 for a heading or a gap
	match int
	// Heading the line shows, empty for a match or a gap
	head string
}

// Fuzzy picker typed into from the bar field with its matches hanging below, a line of text without items
type Palette struct {
	theme theme.Theme
	items []Item
	// Text typed into the field, edited with the keys every field shares
	input   *field.Field
	matches []scored
	sel     int
	// Lines of the sheet, the matches with the headings and gaps between their groups
	lines []line
	// Text the matches were last made for: other text puts the selection back on the first match
	asked string
	// First line shown, kept so the list scrolls only when the selection leaves it or the wheel turns
	top  int
	w    int
	rows int
	// Row of the highlight, gliding after the selection
	hl          anim.Value
	glide       bool
	Placeholder string
	// Called with the text when enter is pressed on a palette without items
	OnText func(text string) tea.Cmd
	// Called once when the palette loses its place as the overlay
	OnClose func() tea.Cmd
	// Keeps the items it accepts for the typed text in their own order, the fuzzy match when nil
	Filter func(query string, it Item) bool
	// Keeps a prompt open after enter, its owner closing it or putting rows in its place
	Hold bool
	// Shows the text typed into a prompt as dots
	Masked bool
	// Keymap context of the actions the palette answers besides its own, empty for none
	Context string
	// Actions of Context by id
	Actions map[string]func() tea.Cmd
	// Buttons for the actions of Context
	Extra []Hint
	// Lines under the rows, read on every view
	Notes func() []Note
	// Step behind the close key, reporting whether it took the key
	OnBack func() bool
	// Ignores typing, picks and actions while its owner waits for an answer
	Busy bool
	ask  *inquiry
}

// Builds a palette over the items
func NewPalette(t theme.Theme, items []Item) *Palette {
	p := &Palette{theme: t, items: items, input: field.New(""), hl: anim.NewValueWith(0, highlightSpring)}
	p.filter()
	return p
}

// Builds a palette that takes a line of text, prefilled with value
func NewPrompt(t theme.Theme, placeholder, value string, submit func(text string) tea.Cmd) *Palette {
	return &Palette{theme: t, Placeholder: placeholder, input: field.New(value), OnText: submit, hl: anim.NewValueWith(0, highlightSpring)}
}

// Glides the highlight toward the selected row, jumping there when motion is off
func (p *Palette) Animate(dt float64) bool {
	p.hl.Set(float64(p.lineOf(p.sel) - p.top))
	if dt <= 0 {
		changed := p.glide || p.hl.Pos != p.hl.Target
		p.hl.Snap()
		p.glide = false
		return changed
	}
	p.glide = true
	return p.hl.Step(dt)
}

// Row the highlight is painted on
func (p *Palette) highlightRow() int {
	if !p.glide {
		return p.lineOf(p.sel) - p.top
	}
	return int(math.Round(p.hl.Pos))
}

// Line the match is shown on, the match itself while no heading is
func (p *Palette) lineOf(match int) int {
	if len(p.lines) == len(p.matches) {
		return match
	}
	for at, l := range p.lines {
		if l.match == match {
			return at
		}
	}
	return 0
}

// Match on the line, false for a heading, a gap and a line past the last
func (p *Palette) matchAt(at int) (int, bool) {
	if at < 0 || at >= len(p.lines) || p.lines[at].match < 0 {
		return 0, false
	}
	return p.lines[at].match, true
}

func (p *Palette) Title() string { return "" }
func (p *Palette) Glyph() string { return p.theme.Glyphs.Palette }
func (p *Palette) Key() string   { return "palette" }
func (p *Palette) Init() tea.Cmd { return nil }

// Keymap contexts in force, the yes and no keys while a question is asked and the palette's own context ahead of the picker keys, the field keys last
func (p *Palette) Contexts() []string {
	switch {
	case p.ask != nil:
		return []string{keys.Confirm, keys.Palette}
	case p.Context != "":
		return []string{p.Context, keys.Palette, keys.Text}
	}
	return []string{keys.Palette, keys.Text}
}

// Text typed so far
func (p *Palette) Query() string { return p.input.Value() }

// Replaces the typed text and filters the rows by it
func (p *Palette) SetQuery(text string) {
	p.input.SetValue(text)
	p.filter()
}

// Replaces the rows in place and filters them by the typed text, the selection back on the first, a palette without rows taking a line of text
func (p *Palette) SetItems(items []Item) {
	p.items = items
	p.sel, p.top = 0, 0
	p.filter()
}

// Moves the selection to the row with the id, reporting whether the typed text leaves it listed
func (p *Palette) Select(id string) bool {
	for i, m := range p.matches {
		if m.item.ID == id {
			p.sel = i
			p.reveal()
			return true
		}
	}
	return false
}

// Row under the selection, false when the typed text matches none
func (p *Palette) Lit() (Item, bool) {
	if p.sel < 0 || p.sel >= len(p.matches) {
		return Item{}, false
	}
	return p.matches[p.sel].item, true
}

// Asks a yes or no question in the field, the rows making way for the notes until it is answered
func (p *Palette) Ask(text string, yes func() tea.Cmd) { p.ask = &inquiry{text: text, yes: yes} }

// Reports whether a question waits for its answer
func (p *Palette) Asking() bool { return p.ask != nil }

// Close key while a question is asked or a step lies behind the palette: the question withdrawn, else the step taken
func (p *Palette) Back() bool {
	if p.ask != nil {
		p.ask = nil
		return true
	}
	return p.OnBack != nil && p.OnBack()
}

// Lines under the rows as the owner reads them now
func (p *Palette) notes() []Note {
	if p.Notes == nil {
		return nil
	}
	return p.Notes()
}

// Count of the rows scrolled out of the sheet above and below, empty while every match shows
func (p *Palette) Status() string {
	if len(p.items) == 0 || p.ask != nil {
		return ""
	}
	above, below := 0, 0
	for at, l := range p.lines {
		switch {
		case l.match < 0:
		case at < p.top:
			above++
		case at >= p.top+p.limit():
			below++
		}
	}
	if above == 0 && below == 0 {
		return ""
	}
	return HiddenText(above, below)
}

// Items matching the query, best first
func (p *Palette) Matches() []Item {
	out := make([]Item, 0, len(p.matches))
	for _, m := range p.matches {
		out = append(out, m.item)
	}
	return out
}

// Rows the sheet under the field wants, the matches, the detail of the lit row where the rows leave theirs out, and the notes under
// them, none for a prompt that lives in the field alone
func (p *Palette) Rows() int {
	notes := len(p.notes())
	if len(p.items) == 0 || p.ask != nil {
		return notes
	}
	return max(len(p.lines), 1) + p.under() + notes
}

// Tells the palette the columns its rows get, before its rows are counted
func (p *Palette) Measure(w int) { p.w = w }

// Widest the titles of the rows that carry a detail run, where the details start, held under the widest a name column grows
func (p *Palette) names() int {
	names := 0
	for _, it := range p.items {
		if it.Detail != "" {
			names = max(names, ansi.StringWidth(it.Title))
		}
	}
	return min(names, paletteNames)
}

// Columns a row takes to show its title, its detail past the name column and its keys whole
func rowWidth(it Item, names int) int {
	w := ansi.StringWidth(it.Title)
	if it.Detail != "" {
		w = max(w, names) + 2 + ansi.StringWidth(it.Detail)
	}
	if it.Keys != "" {
		w += 2 + ansi.StringWidth(it.Keys)
	}
	return w + 2
}

// Columns the rows want so that every one of them shows whole, whatever is typed
func (p *Palette) Wants() int {
	names, want := p.names(), 0
	for _, it := range p.items {
		want = max(want, rowWidth(it, names))
	}
	return want
}

// Reports whether the rows leave their details out because one of them could not show its own whole: a detail is whole or gone, and
// the one of the lit row is then said under the rows
func (p *Palette) terse() bool {
	return p.w > 0 && p.Wants() > p.w
}

// Lines under the rows the detail of the lit row is said on where the rows leave theirs out: as many as the longest one wraps to, so
// the sheet keeps its height as the selection moves
func (p *Palette) under() int {
	if !p.terse() {
		return 0
	}
	lines := 0
	for _, it := range p.items {
		if it.Detail != "" {
			lines = max(lines, len(strings.Split(ansi.Wordwrap(it.Detail, max(p.w-2, 1), ""), "\n")))
		}
	}
	return lines
}

// How many rows the typed text found, nothing while none is typed
func (p *Palette) Tally() string {
	if strings.TrimSpace(p.Query()) == "" {
		return ""
	}
	return strconv.Itoa(len(p.matches)) + " found"
}

// Query in w columns scrolled to keep the caret in view, the placeholder while empty and the question while one is asked, with the caret column
func (p *Palette) Input(w int, bg color.Color) (string, int) {
	t := p.theme
	base := lipgloss.NewStyle().Background(bg)
	if w <= 0 {
		return "", 0
	}
	if p.ask != nil {
		text := ansi.Truncate(p.ask.text, w, t.Glyphs.Ellipsis)
		return base.Foreground(t.Accent).Bold(true).Render(text), min(ansi.StringWidth(text)+1, w)
	}
	if p.input.Empty() {
		return base.Foreground(t.TextDim).Render(ansi.Truncate(p.Placeholder, w, t.Glyphs.Ellipsis)), 0
	}
	p.input.Masked = p.Masked
	text, caret := p.input.Line(w)
	if p.input.Whole() {
		return base.Foreground(t.Bg).Background(t.Ice).Render(text), caret
	}
	return base.Foreground(t.Text).Render(text), caret
}

// Scores a query against a text and reports the matched positions, false when the text does not hold it. Each word of the query must run
// through the text letter by letter, a letter that does not follow the one before it having to start a word, so "wfl" finds "Windows Focus
// left" and a few letters scattered through a sentence find nothing
func fuzzy(query, text string) (int, []int, bool) {
	t := []rune(strings.ToLower(text))
	score := 0
	var hits []int
	for _, term := range strings.Fields(strings.ToLower(query)) {
		s, at := match([]rune(term), t)
		if at == nil {
			return 0, nil, false
		}
		score += s
		hits = append(hits, at...)
	}
	slices.Sort(hits)
	return score, slices.Compact(hits), true
}

// Best run of one query word through the text, nil when it has none
func match(q, t []rune) (int, []int) {
	starts := func(i int) bool { return i == 0 || strings.ContainsRune(" :-./", t[i-1]) }
	best, found := 0, []int(nil)
	hits := make([]int, len(q))
	var walk func(k, from, score int)
	walk = func(k, from, score int) {
		if k == len(q) {
			if total := score - hits[0]; found == nil || total > best {
				best, found = total, slices.Clone(hits)
			}
			return
		}
		for i := from; i < len(t); i++ {
			next := k > 0 && i == hits[k-1]+1
			if t[i] != q[k] || k > 0 && !next && !starts(i) {
				continue
			}
			gain := 10
			if next {
				gain += 6
			}
			if starts(i) {
				gain += 4
			}
			hits[k] = i
			walk(k+1, i+1, score+gain)
		}
	}
	walk(0, 0, 0)
	return best, found
}

func (p *Palette) filter() {
	p.matches = p.matches[:0]
	typed := strings.TrimSpace(p.Query()) != ""
	for _, it := range p.items {
		if typed && it.Again {
			continue
		}
		if p.Filter != nil {
			if p.Filter(p.Query(), it) {
				p.matches = append(p.matches, scored{item: it})
			}
			continue
		}
		s, hits, ok := fuzzy(p.Query(), it.Title)
		// Words of the heading find the row as well, under every row found by its title alone
		if lead := []rune(it.Group + " "); !ok && it.Group != "" {
			if led, at, found := fuzzy(p.Query(), string(lead)+it.Title); found {
				s, ok = led-500, true
				for _, h := range at {
					if h >= len(lead) {
						hits = append(hits, h-len(lead))
					}
				}
			}
		}
		// A title that is the query, or starts with it, leads every looser match
		switch title, q := strings.ToLower(it.Title), strings.ToLower(strings.TrimSpace(p.Query())); {
		case q == "":
		case title == q:
			s += 2000
		case strings.HasPrefix(title, q):
			s += 1000
		}
		// A detail matches only where it holds the query as typed, and ranks under every title
		if !ok {
			at := strings.Index(strings.ToLower(it.Detail), strings.ToLower(strings.TrimSpace(p.Query())))
			if at < 0 {
				continue
			}
			s, hits = -1000-at, nil
		}
		p.matches = append(p.matches, scored{item: it, score: s, hits: hits})
	}
	if p.Query() != "" {
		sort.SliceStable(p.matches, func(i, j int) bool { return p.matches[i].score > p.matches[j].score })
	}
	// The best match of other text is the first, wherever the selection was
	if p.Query() != p.asked {
		p.asked, p.sel, p.top = p.Query(), 0, 0
	}
	if p.sel >= len(p.matches) {
		p.sel = max(0, len(p.matches)-1)
	}
	p.arrange()
	p.reveal()
}

// Lays the matches out as lines, one each, with a heading over every group and a gap between groups while nothing is typed and a row
// names a heading
func (p *Palette) arrange() {
	p.lines = p.lines[:0]
	grouped := strings.TrimSpace(p.Query()) == "" && slices.ContainsFunc(p.matches, func(m scored) bool { return m.item.Group != "" })
	for i, m := range p.matches {
		if grouped && (i == 0 || m.item.Group != p.matches[i-1].item.Group) {
			if i > 0 {
				p.lines = append(p.lines, line{match: -1})
			}
			p.lines = append(p.lines, line{match: -1, head: m.item.Group})
		}
		p.lines = append(p.lines, line{match: i})
	}
}

func (p *Palette) Update(msg tea.Msg) (Card, tea.Cmd) {
	switch msg := msg.(type) {
	case FocusMsg:
		if closed := p.OnClose; !msg.Focused && closed != nil {
			p.OnClose = nil
			return p, closed()
		}
	case ActionMsg:
		return p, p.action(msg.ID)
	case tea.KeyPressMsg, tea.PasteMsg, tea.ClipboardMsg:
		if !p.held() && p.input.Take(msg) {
			p.filter()
		}
		return p, nil
	case tea.MouseClickMsg:
		if p.held() {
			return p, nil
		}
		if msg.Button == tea.MouseLeft && p.onRow(msg.Y) {
			p.sel, _ = p.matchAt(p.top + msg.Y)
			return p, p.pick()
		}
	case tea.MouseMotionMsg:
		if !p.held() && msg.Button == tea.MouseNone && msg.X >= 0 && msg.X < p.w && p.onRow(msg.Y) {
			p.sel, _ = p.matchAt(p.top + msg.Y)
		}
	case tea.MouseWheelMsg:
		if p.held() {
			return p, nil
		}
		switch msg.Button {
		case tea.MouseWheelDown:
			p.scroll(1, msg.Y)
		case tea.MouseWheelUp:
			p.scroll(-1, msg.Y)
		}
	}
	return p, nil
}

// Reports whether typing and picks are set aside, by a question waiting for its answer or an owner waiting for a reply
func (p *Palette) held() bool { return p.Busy || p.ask != nil }

// Carries out a keymap action on the question, the query or the selection, then those of the palette's own context
func (p *Palette) action(id string) tea.Cmd {
	if q := p.ask; q != nil {
		switch id {
		case "confirm.yes", "palette.pick":
			p.ask = nil
			return q.yes()
		case "confirm.no":
			p.ask = nil
		}
		return nil
	}
	if p.Busy {
		return nil
	}
	if run := p.Actions[id]; run != nil {
		return run()
	}
	switch id {
	case "palette.pick":
		return p.pick()
	case "palette.down":
		p.move(1)
	case "palette.up":
		p.move(-1)
	case "palette.page.down":
		p.move(8)
	case "palette.page.up":
		p.move(-8)
	default:
		before := p.Query()
		cmd, _ := p.input.Action(id)
		if p.Query() != before {
			p.filter()
		}
		return cmd
	}
	return nil
}

func (p *Palette) move(d int) {
	if len(p.matches) == 0 {
		return
	}
	p.sel = min(max(p.sel+d, 0), len(p.matches)-1)
	p.reveal()
}

// Reports whether content row y shows a match
func (p *Palette) onRow(y int) bool {
	_, ok := p.matchAt(p.top + y)
	return y >= 0 && y < p.limit() && ok
}

// Scrolls the list by d rows under a still pointer at row y, the highlight staying on the row under it, else on the nearest match in view
func (p *Palette) scroll(d, y int) {
	if len(p.matches) == 0 {
		return
	}
	limit := p.limit()
	p.top = min(max(p.top+d, 0), max(len(p.lines)-limit, 0))
	if p.onRow(y) {
		p.sel, _ = p.matchAt(p.top + y)
		return
	}
	at, end := p.lineOf(p.sel), min(p.top+limit, len(p.lines))
	switch {
	case at < p.top:
		for at = p.top; at < end; at++ {
			if match, ok := p.matchAt(at); ok {
				p.sel = match
				return
			}
		}
	case at >= end:
		for at = end - 1; at >= p.top; at-- {
			if match, ok := p.matchAt(at); ok {
				p.sel = match
				return
			}
		}
	}
}

// Scrolls the least that keeps the selection in view within the lines the matches fill, the heading over the first row of a group
// coming into view with it
func (p *Palette) reveal() {
	limit := p.limit()
	at := p.lineOf(p.sel)
	first := at
	if at > 0 && p.lines[at-1].head != "" && limit > 1 {
		first = at - 1
	}
	if first < p.top {
		p.top = first
	}
	if at >= p.top+limit {
		p.top = at - limit + 1
	}
	p.top = min(max(p.top, 0), max(len(p.lines)-limit, 0))
}

// Runs the selected item, or hands the text over on a prompt, dismissing itself unless the item showed another overlay
func (p *Palette) pick() tea.Cmd {
	if len(p.items) == 0 {
		if p.OnText == nil {
			return RequestDismissOf(p)
		}
		text := p.Query()
		if p.Hold {
			return p.OnText(text)
		}
		return tea.Batch(RequestDismissOf(p), p.OnText(text))
	}
	if p.sel >= len(p.matches) {
		return nil
	}
	it := p.matches[p.sel].item
	if it.Run == nil {
		return RequestDismissOf(p)
	}
	if it.Stay {
		return it.Run()
	}
	return tea.Batch(RequestDismissOf(p), it.Run())
}

func (p *Palette) limit() int { return max(p.rows, 1) }

// Match rows of the sheet with the notes under them, the input itself painted in the field
func (p *Palette) View(w, h int) string {
	p.w = w
	notes := p.notes()
	terse, under := p.terse(), 0
	if len(p.items) > 0 && p.ask == nil {
		under = p.under()
	}
	p.rows = h - len(notes) - under
	p.reveal()
	t := p.theme
	var lines []string
	switch {
	case len(p.items) == 0 || p.ask != nil:
		// A sheet too short to keep its padding hands the rows of the padding over, the notes stay in its middle
		lines = make([]string, max(h-len(notes), 0)/2)
	case len(p.matches) == 0:
		lines = append(lines, lipgloss.NewStyle().Background(t.Surface).Foreground(t.TextDim).Render("  "+lang.T(txt.KitWmNoMatch)))
	default:
		lit := p.top + p.highlightRow()
		// The details start in one column, past the longest name of every row, so the column stays where it is as the rows scroll
		names := p.names()
		plain := lipgloss.NewStyle().Background(t.Surface)
		for at := p.top; at < len(p.lines) && at < p.top+p.limit(); at++ {
			switch l := p.lines[at]; {
			case l.match >= 0:
				lines = append(lines, p.row(p.matches[l.match], at == lit, w, names, terse))
			case l.head != "":
				head := ansi.Truncate(lang.Upper(l.head), max(w-2, 1), t.Glyphs.Ellipsis)
				lines = append(lines, plain.Render(" ")+plain.Foreground(t.TextDim).Bold(true).Render(head)+
					plain.Render(strings.Repeat(" ", max(w-1-ansi.StringWidth(head), 0))))
			default:
				lines = append(lines, plain.Render(strings.Repeat(" ", max(w, 0))))
			}
		}
	}
	// The detail the rows left out, of the lit row, on lines that are there whether or not it has one
	if under > 0 {
		var said []string
		if it, ok := p.Lit(); ok && it.Detail != "" {
			said = strings.Split(ansi.Wordwrap(it.Detail, max(w-2, 1), ""), "\n")
		}
		dim := lipgloss.NewStyle().Background(t.Surface).Foreground(t.TextDim)
		for i := range under {
			text := ""
			if i < len(said) {
				text = said[i]
			}
			lines = append(lines, dim.Render("  "+text+strings.Repeat(" ", max(w-2-ansi.StringWidth(text), 0))))
		}
	}
	for _, n := range notes {
		text := ansi.Truncate("  "+n.Text, max(w, 1), t.Glyphs.Ellipsis)
		lines = append(lines, lipgloss.NewStyle().Background(t.Surface).Foreground(segmentColor(t, n.Style)).Render(text))
	}
	return strings.Join(lines, "\n")
}

// Widest the column of names grows before a longer name pushes its own detail right
const paletteNames = 24

// One match row with hits lit, the detail dimmed in a column that starts past names cells of name unless the rows leave theirs out, and
// the keys at the right edge
func (p *Palette) row(m scored, selected bool, w, names int, terse bool) string {
	t := p.theme
	bg := t.Surface
	if selected {
		bg = t.Steel
		if !t.Plain {
			bg = theme.Mix(t.Surface, t.Ice, 0.1)
		}
	}
	fg, hit, dim, key := t.On(bg), t.Over(bg, t.Ice), t.Over(bg, t.TextDim), t.Over(bg, t.Ice)
	base := lipgloss.NewStyle().Background(bg)
	lead := base.Render(" ")
	if selected {
		lead = base.Foreground(t.Over(bg, t.Accent)).Render(t.Glyphs.Notch)
	}
	var sb strings.Builder
	runes := []rune(m.item.Title)
	hits := map[int]bool{}
	for _, h := range m.hits {
		hits[h] = true
	}
	for i, r := range runes {
		if hits[i] {
			sb.WriteString(base.Foreground(hit).Bold(true).Render(string(r)))
		} else {
			sb.WriteString(base.Foreground(fg).Render(string(r)))
		}
	}
	title := sb.String()
	right := ""
	if m.item.Keys != "" {
		right = base.Foreground(key).Render(m.item.Keys)
	}
	detail := ""
	if m.item.Detail != "" && !terse {
		gap := max(names-ansi.StringWidth(m.item.Title), 0) + 2
		detail = base.Render(strings.Repeat(" ", gap)) + base.Foreground(dim).Render(m.item.Detail)
	}
	rw := ansi.StringWidth(right)
	avail := w - 2 - rw
	if rw > 0 {
		avail -= 2
	}
	left := ansi.Truncate(title+detail, max(avail, 1), t.Glyphs.Ellipsis)
	pad := max(w-2-ansi.StringWidth(left)-rw, 1)
	line := lead + left + base.Render(strings.Repeat(" ", pad)) + right + base.Render(" ")
	return ansi.Truncate(line, w, "")
}

// Buttons of the palette: yes and no while it asks a question, else those of its own context
func (p *Palette) Hints() []Hint {
	if p.ask != nil {
		return []Hint{{Action: "confirm.yes"}, {Action: "confirm.no"}}
	}
	return p.Extra
}

// Action the palette lists and the keymap fires, the manager's own or one the app registers
type Command struct {
	ID    string
	Title string
	// Title at the moment it is shown, for a title that follows the language of the application
	Text func() string
	// What it does in a few words, shown beside the title: the description of the key action of its id when empty
	Detail string
	// Heading the palette lists it under, the commands of one heading together in the order they come
	Group string
	// Reports whether the command applies right now, always when nil
	When func() bool
	// Listed under every other command, where the way out of the application belongs
	Last bool
	Run  func() tea.Cmd
}

// Reports whether the command applies right now
func (c Command) Ready() bool { return c.When == nil || c.When() }

// TitleText is the title of the command in the language of the application, the title it carries when the application has none
func (c Command) TitleText() string {
	if c.Text != nil {
		return c.Text()
	}
	return lang.OfID("palette.", c.ID, ".title", c.Title)
}

// Headings the palette lists the manager's own commands under, and the one over the commands used last. What the focused window
// answers is listed under its kind, such as Chat
const (
	GroupRecent  = "Recent"
	GroupWindows = "Windows"
	GroupTabs    = "Tabs"
	GroupLayout  = "Layout"
	GroupSpaces  = "Spaces"
)

// Rows of the commands used last the palette lists first
const recentShown = 5

// Commands the manager answers on its own, before those the app registered
func (m *Model) builtin() []Command {
	hasTiles := func() bool { return m.Active() != nil && len(m.Active().tiles) > 1 }
	hasFocus := func() bool { return m.focused() != nil }
	hasGroup := func() bool {
		e := m.focused()
		if e == nil {
			return false
		}
		g, grouped := e.card.(*Tabs)
		return grouped && g.Len() > 1
	}
	out := []Command{
		{ID: "palette", Run: m.OpenPalette},
		{ID: "workspace.next", Group: GroupSpaces, Title: "Next space", When: func() bool { return len(m.spaces) > 1 }, Run: func() tea.Cmd { return m.Switch((m.active + 1) % len(m.spaces)) }},
		{ID: "workspace.prev", Group: GroupSpaces, Title: "Previous space", When: func() bool { return len(m.spaces) > 1 }, Run: func() tea.Cmd { return m.Switch((m.active - 1 + len(m.spaces)) % len(m.spaces)) }},
		{ID: "window.focus.left", Group: GroupWindows, Title: "Focus left", When: hasFocus, Run: func() tea.Cmd { return m.FocusDir(Left) }},
		{ID: "window.focus.right", Group: GroupWindows, Title: "Focus right", When: hasFocus, Run: func() tea.Cmd { return m.FocusDir(Right) }},
		{ID: "window.focus.up", Group: GroupWindows, Title: "Focus up", When: hasFocus, Run: func() tea.Cmd { return m.FocusDir(Up) }},
		{ID: "window.focus.down", Group: GroupWindows, Title: "Focus down", When: hasFocus, Run: func() tea.Cmd { return m.FocusDir(Down) }},
		{ID: "window.cycle", Group: GroupWindows, Title: "Focus next", When: hasFocus, Run: func() tea.Cmd { return m.Cycle(1) }},
		{ID: "window.cycle.back", Group: GroupWindows, Title: "Focus previous", When: hasFocus, Run: func() tea.Cmd { return m.Cycle(-1) }},
		{ID: "window.move.left", Group: GroupWindows, Title: "Move left", When: hasFocus, Run: func() tea.Cmd { return m.Move(Left) }},
		{ID: "window.move.right", Group: GroupWindows, Title: "Move right", When: hasFocus, Run: func() tea.Cmd { return m.Move(Right) }},
		{ID: "window.move.up", Group: GroupWindows, Title: "Move up", When: hasFocus, Run: func() tea.Cmd { return m.Move(Up) }},
		{ID: "window.move.down", Group: GroupWindows, Title: "Move down", When: hasFocus, Run: func() tea.Cmd { return m.Move(Down) }},
		{ID: "window.master", Group: GroupWindows, Title: "Make main", When: hasTiles, Run: m.SwapMaster},
		{ID: "window.zoom", Group: GroupWindows, Title: "Fullscreen", When: hasFocus, Run: m.ToggleZoom},
		{ID: "window.float", Group: GroupWindows, Title: "Float", When: hasFocus, Run: m.ToggleFloat},
		{ID: "window.close", Group: GroupWindows, Title: "Close", When: func() bool { e := m.focused(); return e != nil && closable(e.card) }, Run: m.CloseFocused},
		{ID: "window.attention", Group: GroupWindows, Title: "Go to waiting window", When: m.needsYou, Run: m.FocusAttention},
		{ID: "tabs.join", Group: GroupTabs, Title: "Merge into next window", When: func() bool {
			e := m.focused()
			return e != nil && !e.floating && besideTile(m.Active(), e) != nil
		}, Run: m.JoinTab},
		{ID: "tabs.split", Group: GroupTabs, Title: "Split into own window", When: hasGroup, Run: m.SplitTab},
		{ID: "tabs.cycle", Group: GroupTabs, Title: "Next tab", When: hasGroup, Run: func() tea.Cmd { return m.CycleTab(1) }},
		{ID: "tabs.cycle.back", Group: GroupTabs, Title: "Previous tab", When: hasGroup, Run: func() tea.Cmd { return m.CycleTab(-1) }},
		{ID: "panel.close", When: func() bool {
			e := m.focused()
			return e != nil && closable(e.card) && slices.Contains(contextsOf(e.card), keys.Panel)
		}, Run: m.CloseFocused},
		{ID: "window.grow", Group: GroupWindows, Title: "Grow", When: hasTiles, Run: func() tea.Cmd { return m.Resize(0.05) }},
		{ID: "window.shrink", Group: GroupWindows, Title: "Shrink", When: hasTiles, Run: func() tea.Cmd { return m.Resize(-0.05) }},
		{ID: "layout.reset", Group: GroupLayout, Title: "Reset sizes", When: func() bool { return m.Active() != nil }, Run: m.ResetLayout},
		{ID: "layout.toggle", Group: GroupLayout, Title: "Switch layout", When: func() bool { return m.Active() != nil }, Run: m.ToggleLayout},
		{ID: "layout.dwindle", Group: GroupLayout, Title: "Spiral", When: func() bool { ws := m.Active(); return ws != nil && ws.mode != Dwindle }, Run: func() tea.Cmd { return m.SetLayout(Dwindle) }},
		{ID: "layout.main.more", Group: GroupLayout, Title: "More main windows", When: func() bool { ws := m.Active(); return ws != nil && ws.mode == Master }, Run: func() tea.Cmd { return m.SetMainCount(1) }},
		{ID: "layout.main.fewer", Group: GroupLayout, Title: "Fewer main windows", When: func() bool { ws := m.Active(); return ws != nil && ws.mode == Master && ws.MainCount() > 1 }, Run: func() tea.Cmd { return m.SetMainCount(-1) }},
		{ID: "layout.split", Group: GroupLayout, Title: "Flip split", When: func() bool { ws := m.Active(); return ws != nil && ws.mode == Dwindle }, Run: m.FlipSplit},
		{ID: "layout.master", Group: GroupLayout, Title: "Main column", When: func() bool { ws := m.Active(); return ws != nil && ws.mode != Master }, Run: func() tea.Cmd { return m.SetLayout(Master) }},
	}
	for i := 1; i <= 9; i++ {
		n := i
		out = append(out, Command{ID: "workspace." + strconv.Itoa(n), Group: GroupSpaces, Title: "Space " + strconv.Itoa(n), When: func() bool { return n <= len(m.spaces) }, Run: func() tea.Cmd { return m.Switch(n - 1) }})
	}
	return out
}

// Sets where the commands the app contributes come from, asked each time they are listed or run and taking precedence over the built in ones
func (m *Model) SetCommandSource(source func() []Command) { m.commandSource = source }

// Every command, the app's first
func (m *Model) Commands() []Command {
	var app []Command
	if m.commandSource != nil {
		app = m.commandSource()
	}
	out := make([]Command, 0, len(app)+40)
	out = append(out, app...)
	out = append(out, m.builtin()...)
	return out
}

// Command with the id, the app's over the manager's
func (m *Model) command(id string) (Command, bool) {
	for _, c := range m.Commands() {
		if c.ID == id {
			return c, true
		}
	}
	return Command{}, false
}

// Runs the command with the id, nil when none applies
func (m *Model) Run(id string) tea.Cmd {
	if c, ok := m.command(id); ok && c.Ready() {
		return c.Run()
	}
	return nil
}

// Keymap the manager dispatches keys and paints legends from
func (m *Model) Keys() *keys.Map { return m.keys }

// Replaces the keymap
func (m *Model) SetKeys(km *keys.Map) {
	if km != nil {
		m.keys = km
		km.SetShiftApart(m.kittyKeys)
	}
}

// Fires the first ready action the key is bound to in the contexts of the target, reporting whether one was
func (m *Model) dispatch(target *entry, msg tea.KeyPressMsg) (tea.Cmd, bool) {
	ctxs := contextsOf(target.card)
	floating := target == m.overlay || target.host != nil
	grab := raw(target.card)
	// A floating card moves and closes with the palette keys, unless it takes every key as typed, as one recording a key does
	if floating && !grab && !slices.Contains(ctxs, keys.Palette) {
		ctxs = append(ctxs, keys.Palette)
	}
	for _, id := range m.keys.Lookup(ctxs, msg) {
		a, _ := m.keys.Action(id)
		if grab && !slices.Contains(ctxs, a.Context) && !passes(target.card, msg) {
			continue
		}
		if m.overlay != nil && a.Context == keys.Global && !a.Overlay {
			continue
		}
		if floating && (id == "palette.close" || id == "panel.close") && !closesItself(target.card) {
			if b, steps := target.card.(Backer); steps && id == "palette.close" && b.Back() {
				return nil, true
			}
			if target.host != nil {
				return m.Detach(target.card), true
			}
			return m.Dismiss(), true
		}
		if c, ok := m.command(id); ok {
			if !c.Ready() {
				continue
			}
			return c.Run(), true
		}
		// An action of the card's contexts it has no use for leaves the key to the next action bound to it
		if !slices.Contains(ctxs, a.Context) || !answers(target.card, id) {
			continue
		}
		return m.send(target, ActionMsg{ID: id}), true
	}
	return nil, false
}

// Fires the action at the focused card, or runs the command of that id
func (m *Model) Fire(id string) tea.Cmd {
	if c, ok := m.command(id); ok {
		if !c.Ready() {
			return m.nudge()
		}
		return c.Run()
	}
	target := m.overlay
	if target == nil {
		target = m.focused()
	}
	if target == nil {
		return nil
	}
	return m.send(target, ActionMsg{ID: id})
}

// Command ids the palette remembers at most
const maxRecent = 12

// Moves the id to the front of the recently used list
func (m *Model) remember(id string) {
	if i := slices.Index(m.recent, id); i >= 0 {
		m.recent = slices.Delete(m.recent, i, i+1)
	}
	m.recent = append([]string{id}, m.recent...)
	if len(m.recent) > maxRecent {
		m.recent = m.recent[:maxRecent]
	}
}

// Opens the palette over titled ready commands and the focused card actions, each under its heading: the commands used last, the app's,
// what the focused window answers under its kind, then the manager's own. A command the app lists last ends the rows of its heading
func (m *Model) OpenPalette() tea.Cmd {
	if _, open := m.Overlay().(*Palette); open {
		return m.Dismiss()
	}
	var items, kit, last []Item
	listed := map[string]bool{}
	list := func(into *[]Item, commands []Command) {
		for _, c := range commands {
			if !c.Ready() || listed[c.ID] || c.Title == "" {
				continue
			}
			listed[c.ID] = true
			id, run := c.ID, c.Run
			detail := c.Detail
			if a, ok := m.keys.Action(id); ok && detail == "" {
				detail = a.DocText()
			}
			it := Item{ID: id, Title: c.TitleText(), Detail: detail, Group: c.Group, Keys: m.keys.Display(id), Run: func() tea.Cmd { m.remember(id); return run() }}
			if c.Last {
				last = append(last, it)
				continue
			}
			*into = append(*into, it)
		}
	}
	if m.commandSource != nil {
		list(&items, m.commandSource())
	}
	list(&kit, m.builtin())
	// A command listed last closes the rows of its own heading, the rows of the app when it names none
	for _, it := range last {
		at := len(items)
		for i := len(items) - 1; i >= 0; i-- {
			if items[i].Group == it.Group {
				at = i + 1
				break
			}
		}
		items = slices.Insert(items, at, it)
	}
	if e := m.focused(); e != nil {
		card := shown(e.card)
		kind := card.Title()
		if k, ok := card.(Kinded); ok {
			kind = k.Kind()
		}
		for _, a := range m.keys.InContexts(contextsOf(e.card)) {
			if a.Quiet || a.Context == keys.Global || listed[a.ID] || !answers(card, a.ID) {
				continue
			}
			listed[a.ID] = true
			id := a.ID
			items = append(items, Item{ID: id, Title: sentence(a.LabelText()), Detail: a.DocText(), Group: kind, Keys: m.keys.Display(id),
				Run: func() tea.Cmd { m.remember(id); return m.send(e, ActionMsg{ID: id}) }})
		}
	}
	items = append(items, kit...)
	var recent []Item
	for _, id := range m.recent {
		if at := slices.IndexFunc(items, func(it Item) bool { return it.ID == id }); at >= 0 && len(recent) < recentShown {
			again := items[at]
			again.Group, again.Again = GroupRecent, true
			recent = append(recent, again)
		}
	}
	p := NewPalette(m.theme, append(recent, items...))
	p.Placeholder = lang.T(txt.KitWmCommandPlaceholder)
	return m.ShowOverlay(p)
}

// Label of a key action as the title of a row, its first letter a capital
func sentence(label string) string {
	if label == "" {
		return ""
	}
	first, size := utf8.DecodeRuneInString(label)
	return string(unicode.ToUpper(first)) + label[size:]
}

// Card that shows in the place of c: the active tab of a tab group, c itself otherwise
func shown(c Card) Card {
	if g, ok := c.(*Tabs); ok {
		return g.current()
	}
	return c
}
