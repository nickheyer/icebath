package wm

import (
	"image/color"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"

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
	Run    func() tea.Cmd
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

// Fuzzy picker typed into from the bar field with its matches hanging below, a line of text without items
type Palette struct {
	theme theme.Theme
	items []Item
	// Text typed into the field, edited with the keys every field shares
	input   *field.Field
	matches []scored
	sel     int
	// First match row shown, kept so the list scrolls only when the selection leaves it or the wheel turns
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
	p.hl.Set(float64(p.sel - p.top))
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
		return p.sel - p.top
	}
	return int(math.Round(p.hl.Pos))
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
	above, below := p.top, max(len(p.matches)-p.top-p.limit(), 0)
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

// Rows the sheet under the field wants, the matches and the notes under them, none for a prompt that lives in the field alone
func (p *Palette) Rows() int {
	notes := len(p.notes())
	if len(p.items) == 0 || p.ask != nil {
		return notes
	}
	return max(len(p.matches), 1) + notes
}

// Matches out of the items
func (p *Palette) Tally() string {
	return strconv.Itoa(len(p.matches)) + "/" + strconv.Itoa(len(p.items))
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
// through the text letter by letter, a letter that does not follow the one before it having to start a word, so "wfl" finds "Window: focus
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
	for _, it := range p.items {
		if p.Filter != nil {
			if p.Filter(p.Query(), it) {
				p.matches = append(p.matches, scored{item: it})
			}
			continue
		}
		s, hits, ok := fuzzy(p.Query(), it.Title)
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
	if p.sel >= len(p.matches) {
		p.sel = max(0, len(p.matches)-1)
	}
	p.reveal()
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
			p.sel = p.top + msg.Y
			return p, p.pick()
		}
	case tea.MouseMotionMsg:
		if !p.held() && msg.Button == tea.MouseNone && msg.X >= 0 && msg.X < p.w && p.onRow(msg.Y) {
			p.sel = p.top + msg.Y
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
	return y >= 0 && y < p.limit() && p.top+y < len(p.matches)
}

// Scrolls the list by d rows under a still pointer at row y, the highlight staying on the row under it
func (p *Palette) scroll(d, y int) {
	if len(p.matches) == 0 {
		return
	}
	limit := p.limit()
	p.top = min(max(p.top+d, 0), max(len(p.matches)-limit, 0))
	if p.onRow(y) {
		p.sel = p.top + y
		return
	}
	p.sel = min(max(p.sel, p.top), min(p.top+limit, len(p.matches))-1)
}

// Scrolls the least that keeps the selection in view within the rows the matches fill
func (p *Palette) reveal() {
	limit := p.limit()
	if p.sel < p.top {
		p.top = p.sel
	}
	if p.sel >= p.top+limit {
		p.top = p.sel - limit + 1
	}
	p.top = min(max(p.top, 0), max(len(p.matches)-limit, 0))
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
	p.rows = h - len(notes)
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
		for i := p.top; i < len(p.matches) && i < p.top+p.limit(); i++ {
			m := p.matches[i]
			lines = append(lines, p.row(m, i == lit, w))
		}
	}
	for _, n := range notes {
		text := ansi.Truncate("  "+n.Text, max(w, 1), t.Glyphs.Ellipsis)
		lines = append(lines, lipgloss.NewStyle().Background(t.Surface).Foreground(segmentColor(t, n.Style)).Render(text))
	}
	return strings.Join(lines, "\n")
}

// One match row with hits lit, the detail dimmed and the keys at the right edge
func (p *Palette) row(m scored, selected bool, w int) string {
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
	if m.item.Detail != "" {
		detail = base.Foreground(dim).Render("  " + m.item.Detail)
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
	// What it does in a few words, shown beside the title when the title alone does not say
	Detail string
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
		{ID: "workspace.next", Title: "Space: next", When: func() bool { return len(m.spaces) > 1 }, Run: func() tea.Cmd { return m.Switch((m.active + 1) % len(m.spaces)) }},
		{ID: "workspace.prev", Title: "Space: previous", When: func() bool { return len(m.spaces) > 1 }, Run: func() tea.Cmd { return m.Switch((m.active - 1 + len(m.spaces)) % len(m.spaces)) }},
		{ID: "window.focus.left", Title: "Window: focus left", When: hasFocus, Run: func() tea.Cmd { return m.FocusDir(Left) }},
		{ID: "window.focus.right", Title: "Window: focus right", When: hasFocus, Run: func() tea.Cmd { return m.FocusDir(Right) }},
		{ID: "window.focus.up", Title: "Window: focus up", When: hasFocus, Run: func() tea.Cmd { return m.FocusDir(Up) }},
		{ID: "window.focus.down", Title: "Window: focus down", When: hasFocus, Run: func() tea.Cmd { return m.FocusDir(Down) }},
		{ID: "window.cycle", Title: "Window: focus next", When: hasFocus, Run: func() tea.Cmd { return m.Cycle(1) }},
		{ID: "window.cycle.back", Title: "Window: focus previous", When: hasFocus, Run: func() tea.Cmd { return m.Cycle(-1) }},
		{ID: "window.move.left", Title: "Window: move left", When: hasFocus, Run: func() tea.Cmd { return m.Move(Left) }},
		{ID: "window.move.right", Title: "Window: move right", When: hasFocus, Run: func() tea.Cmd { return m.Move(Right) }},
		{ID: "window.move.up", Title: "Window: move up", When: hasFocus, Run: func() tea.Cmd { return m.Move(Up) }},
		{ID: "window.move.down", Title: "Window: move down", When: hasFocus, Run: func() tea.Cmd { return m.Move(Down) }},
		{ID: "window.master", Title: "Window: make main", When: hasTiles, Run: m.SwapMaster},
		{ID: "window.zoom", Title: "Window: fullscreen", When: hasFocus, Run: m.ToggleZoom},
		{ID: "window.float", Title: "Window: float", When: hasFocus, Run: m.ToggleFloat},
		{ID: "window.close", Title: "Window: close", When: func() bool { e := m.focused(); return e != nil && closable(e.card) }, Run: m.CloseFocused},
		{ID: "window.attention", Title: "Window: go to waiting window", When: m.needsYou, Run: m.FocusAttention},
		{ID: "tabs.join", Title: "Tab: merge into next window", When: func() bool {
			e := m.focused()
			return e != nil && !e.floating && besideTile(m.Active(), e) != nil
		}, Run: m.JoinTab},
		{ID: "tabs.split", Title: "Tab: split into own window", When: hasGroup, Run: m.SplitTab},
		{ID: "tabs.cycle", Title: "Tab: next", When: hasGroup, Run: func() tea.Cmd { return m.CycleTab(1) }},
		{ID: "tabs.cycle.back", Title: "Tab: previous", When: hasGroup, Run: func() tea.Cmd { return m.CycleTab(-1) }},
		{ID: "panel.close", When: func() bool {
			e := m.focused()
			return e != nil && closable(e.card) && slices.Contains(contextsOf(e.card), keys.Panel)
		}, Run: m.CloseFocused},
		{ID: "window.grow", Title: "Window: grow", When: hasTiles, Run: func() tea.Cmd { return m.Resize(0.05) }},
		{ID: "window.shrink", Title: "Window: shrink", When: hasTiles, Run: func() tea.Cmd { return m.Resize(-0.05) }},
		{ID: "layout.reset", Title: "Layout: reset sizes", When: func() bool { return m.Active() != nil }, Run: m.ResetLayout},
		{ID: "layout.toggle", Title: "Layout: toggle", When: func() bool { return m.Active() != nil }, Run: m.ToggleLayout},
		{ID: "layout.dwindle", Title: "Layout: spiral", When: func() bool { ws := m.Active(); return ws != nil && ws.mode != Dwindle }, Run: func() tea.Cmd { return m.SetLayout(Dwindle) }},
		{ID: "layout.main.more", Title: "Layout: more main windows", When: func() bool { ws := m.Active(); return ws != nil && ws.mode == Master }, Run: func() tea.Cmd { return m.SetMainCount(1) }},
		{ID: "layout.main.fewer", Title: "Layout: fewer main windows", When: func() bool { ws := m.Active(); return ws != nil && ws.mode == Master && ws.MainCount() > 1 }, Run: func() tea.Cmd { return m.SetMainCount(-1) }},
		{ID: "layout.split", Title: "Layout: flip split", When: func() bool { ws := m.Active(); return ws != nil && ws.mode == Dwindle }, Run: m.FlipSplit},
		{ID: "layout.master", Title: "Layout: main column", When: func() bool { ws := m.Active(); return ws != nil && ws.mode != Master }, Run: func() tea.Cmd { return m.SetLayout(Master) }},
	}
	for i := 1; i <= 9; i++ {
		n := i
		out = append(out, Command{ID: "workspace." + strconv.Itoa(n), Title: "Space " + strconv.Itoa(n), When: func() bool { return n <= len(m.spaces) }, Run: func() tea.Cmd { return m.Switch(n - 1) }})
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

// Opens the palette over titled ready commands and the focused card actions, recent first
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
			it := Item{ID: id, Title: c.TitleText(), Detail: c.Detail, Keys: m.keys.Display(id), Run: func() tea.Cmd { m.remember(id); return run() }}
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
			items = append(items, Item{ID: id, Title: kind + ": " + a.LabelText(), Keys: m.keys.Display(id), Run: func() tea.Cmd { m.remember(id); return m.send(e, ActionMsg{ID: id}) }})
		}
	}
	items = append(append(items, kit...), last...)
	rank := map[string]int{}
	for i, id := range m.recent {
		rank[id] = len(m.recent) - i
	}
	slices.SortStableFunc(items, func(a, b Item) int { return rank[b.ID] - rank[a.ID] })
	p := NewPalette(m.theme, items)
	p.Placeholder = lang.T(txt.KitWmCommandPlaceholder)
	return m.ShowOverlay(p)
}

// Card that shows in the place of c: the active tab of a tab group, c itself otherwise
func shown(c Card) Card {
	if g, ok := c.(*Tabs); ok {
		return g.current()
	}
	return c
}
