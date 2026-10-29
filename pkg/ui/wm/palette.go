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
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/theme"
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
}

type scored struct {
	item  Item
	score int
	hits  []int
}

// Fuzzy picker typed into from the bar field with its matches hanging below, a line of text without items
type Palette struct {
	theme   theme.Theme
	items   []Item
	query   string
	cursor  int
	matches []scored
	sel     int
	// First match row shown, kept so the list scrolls only when the selection leaves it or the wheel turns
	top      int
	commands bool
	w        int
	rows     int
	// Row of the highlight, gliding after the selection
	hl          anim.Value
	glide       bool
	Placeholder string
	// Called with the text when enter is pressed on a palette without items
	OnText func(text string) tea.Cmd
}

// Builds a palette over the items
func NewPalette(t theme.Theme, items []Item) *Palette {
	p := &Palette{theme: t, items: items, hl: anim.NewValueWith(0, highlightSpring)}
	p.filter()
	return p
}

// Builds a palette that takes a line of text, prefilled with value
func NewPrompt(t theme.Theme, placeholder, value string, submit func(text string) tea.Cmd) *Palette {
	p := &Palette{theme: t, Placeholder: placeholder, query: value, cursor: len([]rune(value)), OnText: submit, hl: anim.NewValueWith(0, highlightSpring)}
	return p
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

func (p *Palette) Title() string      { return "" }
func (p *Palette) Glyph() string      { return p.theme.Glyphs.Palette }
func (p *Palette) Key() string        { return "palette" }
func (p *Palette) Init() tea.Cmd      { return nil }
func (p *Palette) Contexts() []string { return []string{keys.Palette} }

// Text typed so far
func (p *Palette) Query() string { return p.query }

// Items matching the query, best first
func (p *Palette) Matches() []Item {
	out := make([]Item, 0, len(p.matches))
	for _, m := range p.matches {
		out = append(out, m.item)
	}
	return out
}

// Rows the sheet under the field wants, none for a prompt that lives in the field alone
func (p *Palette) Rows() int {
	if len(p.items) == 0 {
		return 0
	}
	return max(len(p.matches), 1)
}

// Matches out of the items
func (p *Palette) Tally() string {
	return strconv.Itoa(len(p.matches)) + "/" + strconv.Itoa(len(p.items))
}

// Query in w columns scrolled to keep the caret in view, the placeholder while empty, with the caret column
func (p *Palette) Input(w int, bg color.Color) (string, int) {
	t := p.theme
	base := lipgloss.NewStyle().Background(bg)
	if w <= 0 {
		return "", 0
	}
	if p.query == "" {
		return base.Foreground(t.TextDim).Render(ansi.Truncate(p.Placeholder, w, t.Glyphs.Ellipsis)), 0
	}
	runes := []rune(p.query)
	from := 0
	for from < p.cursor && ansi.StringWidth(string(runes[from:p.cursor])) >= w {
		from++
	}
	return base.Foreground(t.Text).Render(ansi.Truncate(string(runes[from:]), w, "")), ansi.StringWidth(string(runes[from:p.cursor]))
}

// Score of a fuzzy subsequence match with the matched positions, negative for none
func fuzzy(query, text string) (int, []int) {
	q := []rune(strings.ToLower(query))
	t := []rune(strings.ToLower(text))
	if len(q) == 0 {
		return 0, nil
	}
	score, last := 0, -1
	hits := make([]int, 0, len(q))
	for _, r := range q {
		pos := -1
		for i := last + 1; i < len(t); i++ {
			if t[i] == r {
				pos = i
				break
			}
		}
		if pos < 0 {
			return -1, nil
		}
		score += 10
		if pos == last+1 {
			score += 6
		}
		if pos == 0 || t[pos-1] == ' ' || t[pos-1] == ':' || t[pos-1] == '-' || t[pos-1] == '.' {
			score += 4
		}
		hits = append(hits, pos)
		last = pos
	}
	return score - last, hits
}

func (p *Palette) filter() {
	p.matches = p.matches[:0]
	for _, it := range p.items {
		s, hits := fuzzy(p.query, it.Title)
		if s < 0 {
			ds, _ := fuzzy(p.query, it.Detail)
			if ds < 0 {
				continue
			}
			s, hits = ds-20, nil
		}
		p.matches = append(p.matches, scored{item: it, score: s, hits: hits})
	}
	if p.query != "" {
		sort.SliceStable(p.matches, func(i, j int) bool { return p.matches[i].score > p.matches[j].score })
	}
	if p.sel >= len(p.matches) {
		p.sel = max(0, len(p.matches)-1)
	}
	p.reveal()
}

func (p *Palette) Update(msg tea.Msg) (Card, tea.Cmd) {
	switch msg := msg.(type) {
	case ActionMsg:
		return p, p.action(msg.ID)
	case tea.KeyPressMsg:
		if msg.Text != "" {
			p.insert(msg.Text)
		}
		return p, nil
	case tea.PasteMsg:
		p.insert(msg.Content)
		return p, nil
	case tea.MouseClickMsg:
		if msg.Button == tea.MouseLeft && p.onRow(msg.Y) {
			p.sel = p.top + msg.Y
			return p, p.pick()
		}
	case tea.MouseMotionMsg:
		if msg.Button == tea.MouseNone && msg.X >= 0 && msg.X < p.w && p.onRow(msg.Y) {
			p.sel = p.top + msg.Y
		}
	case tea.MouseWheelMsg:
		switch msg.Button {
		case tea.MouseWheelDown:
			p.scroll(1, msg.Y)
		case tea.MouseWheelUp:
			p.scroll(-1, msg.Y)
		}
	}
	return p, nil
}

// Carries out a keymap action on the query or the selection
func (p *Palette) action(id string) tea.Cmd {
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
	case "palette.delete.back":
		if p.cursor > 0 {
			r := []rune(p.query)
			p.query = string(r[:p.cursor-1]) + string(r[p.cursor:])
			p.cursor--
			p.filter()
		}
	case "palette.delete.forward":
		r := []rune(p.query)
		if p.cursor < len(r) {
			p.query = string(r[:p.cursor]) + string(r[p.cursor+1:])
			p.filter()
		}
	case "palette.left":
		p.cursor = max(0, p.cursor-1)
	case "palette.right":
		p.cursor = min(len([]rune(p.query)), p.cursor+1)
	case "palette.home":
		p.cursor = 0
	case "palette.end":
		p.cursor = len([]rune(p.query))
	case "palette.clear":
		p.query, p.cursor = "", 0
		p.filter()
	case "palette.delete.word":
		runes := []rune(p.query)
		i := p.cursor
		for i > 0 && runes[i-1] == ' ' {
			i--
		}
		for i > 0 && runes[i-1] != ' ' {
			i--
		}
		r := append([]rune{}, runes[:i]...)
		r = append(r, runes[p.cursor:]...)
		p.query, p.cursor = string(r), i
		p.filter()
	}
	return nil
}

func (p *Palette) insert(text string) {
	r := []rune(p.query)
	out := append([]rune{}, r[:p.cursor]...)
	out = append(out, []rune(text)...)
	out = append(out, r[p.cursor:]...)
	p.query = string(out)
	p.cursor += len([]rune(text))
	p.filter()
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
		text := p.query
		return tea.Batch(RequestDismissOf(p), p.OnText(text))
	}
	if p.sel >= len(p.matches) {
		return nil
	}
	run := p.matches[p.sel].item.Run
	if run == nil {
		return RequestDismissOf(p)
	}
	return tea.Batch(RequestDismissOf(p), run())
}

func (p *Palette) limit() int { return max(p.rows, 1) }

// Match rows of the sheet, the input itself painted in the field
func (p *Palette) View(w, h int) string {
	p.w = w
	p.rows = h
	p.reveal()
	t := p.theme
	if len(p.items) == 0 {
		return ""
	}
	if len(p.matches) == 0 {
		return lipgloss.NewStyle().Background(t.Surface).Foreground(t.TextDim).Render("  no match")
	}
	var lines []string
	lit := p.top + p.highlightRow()
	for i := p.top; i < len(p.matches) && i < p.top+p.limit(); i++ {
		m := p.matches[i]
		lines = append(lines, p.row(m, i == lit, w))
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
	fg, hit, dim, key := t.Text, t.Ice, t.TextDim, t.Ice
	base := lipgloss.NewStyle().Background(bg)
	lead := base.Render(" ")
	if selected {
		lead = base.Foreground(t.Accent).Render(t.Glyphs.Notch)
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

// Actions the palette answers
func (p *Palette) Hints() []Hint {
	if len(p.items) == 0 {
		return []Hint{{Action: "palette.pick", Label: "confirm"}, {Action: "palette.close", Label: "cancel"}}
	}
	return []Hint{{Action: "palette.down", Pair: "palette.up", Label: "move"}, {Action: "palette.pick"}, {Action: "palette.close"}}
}

// Action the palette lists and the keymap fires, the manager's own or one the app registers
type Command struct {
	ID    string
	Title string
	// Reports whether the command applies right now, always when nil
	When func() bool
	Run  func() tea.Cmd
}

// Reports whether the command applies right now
func (c Command) Ready() bool { return c.When == nil || c.When() }

// Commands the manager answers on its own, before those the app registered
func (m *Model) builtin() []Command {
	hasTiles := func() bool { return m.Active() != nil && len(m.Active().tiles) > 1 }
	hasFocus := func() bool { return m.focused() != nil }
	out := []Command{
		{ID: "palette", Run: m.OpenPalette},
		{ID: "workspace.next", Title: "Workspace: next", When: func() bool { return len(m.spaces) > 1 }, Run: func() tea.Cmd { return m.Switch((m.active + 1) % len(m.spaces)) }},
		{ID: "workspace.prev", Title: "Workspace: previous", When: func() bool { return len(m.spaces) > 1 }, Run: func() tea.Cmd { return m.Switch((m.active - 1 + len(m.spaces)) % len(m.spaces)) }},
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
		{ID: "window.master", Title: "Window: swap master", When: hasTiles, Run: m.SwapMaster},
		{ID: "window.zoom", Title: "Window: fullscreen", When: hasFocus, Run: m.ToggleZoom},
		{ID: "window.float", Title: "Window: float", When: hasFocus, Run: m.ToggleFloat},
		{ID: "window.close", Title: "Window: close", When: func() bool { e := m.focused(); return e != nil && closable(e.card) }, Run: m.CloseFocused},
		{ID: "panel.close", When: func() bool {
			e := m.focused()
			return e != nil && closable(e.card) && slices.Contains(contextsOf(e.card), keys.Panel)
		}, Run: m.CloseFocused},
		{ID: "window.grow", Title: "Window: grow", When: hasTiles, Run: func() tea.Cmd { return m.Resize(0.05) }},
		{ID: "window.shrink", Title: "Window: shrink", When: hasTiles, Run: func() tea.Cmd { return m.Resize(-0.05) }},
		{ID: "layout.reset", Title: "Layout: reset", When: func() bool { return m.Active() != nil }, Run: m.ResetLayout},
		{ID: "layout.toggle", Title: "Layout: toggle", When: func() bool { return m.Active() != nil }, Run: m.ToggleLayout},
		{ID: "layout.dwindle", Title: "Layout: dwindle", When: func() bool { ws := m.Active(); return ws != nil && ws.mode != Dwindle }, Run: func() tea.Cmd { return m.SetLayout(Dwindle) }},
		{ID: "layout.main.more", Title: "Layout: more main", When: func() bool { ws := m.Active(); return ws != nil && ws.mode == Master }, Run: func() tea.Cmd { return m.SetMainCount(1) }},
		{ID: "layout.main.fewer", Title: "Layout: fewer main", When: func() bool { ws := m.Active(); return ws != nil && ws.mode == Master && ws.MainCount() > 1 }, Run: func() tea.Cmd { return m.SetMainCount(-1) }},
		{ID: "layout.split", Title: "Layout: flip split", When: func() bool { ws := m.Active(); return ws != nil && ws.mode == Dwindle }, Run: m.FlipSplit},
		{ID: "layout.master", Title: "Layout: master", When: func() bool { ws := m.Active(); return ws != nil && ws.mode != Master }, Run: func() tea.Cmd { return m.SetLayout(Master) }},
	}
	for i := 1; i <= 9; i++ {
		n := i
		out = append(out, Command{ID: "workspace." + strconv.Itoa(n), Title: "Workspace " + strconv.Itoa(n), When: func() bool { return n <= len(m.spaces) }, Run: func() tea.Cmd { return m.Switch(n - 1) }})
	}
	return out
}

// Replaces the commands the app contributes, which take precedence over the built in ones
func (m *Model) SetCommands(cmds []Command) { m.commands = cmds }

// Every command, the app's first
func (m *Model) Commands() []Command {
	out := make([]Command, 0, len(m.commands)+40)
	out = append(out, m.commands...)
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
		m.remember(id)
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
	}
}

// Fires the first ready action the key is bound to in the contexts of the target, reporting whether one was
func (m *Model) dispatch(target *entry, msg tea.KeyPressMsg) (tea.Cmd, bool) {
	ctxs := contextsOf(target.card)
	if target == m.overlay && !slices.Contains(ctxs, keys.Palette) {
		ctxs = append(ctxs, keys.Palette)
	}
	grab := raw(target.card)
	for _, id := range m.keys.Lookup(ctxs, msg) {
		a, _ := m.keys.Action(id)
		if grab && !slices.Contains(ctxs, a.Context) {
			continue
		}
		if m.overlay != nil && a.Context == keys.Global && !a.Overlay {
			continue
		}
		if target == m.overlay && id == "palette.close" {
			return m.Dismiss(), true
		}
		if c, ok := m.command(id); ok {
			if !c.Ready() {
				continue
			}
			m.remember(id)
			return c.Run(), true
		}
		if !slices.Contains(ctxs, a.Context) {
			continue
		}
		return m.send(target, ActionMsg{ID: id, Key: msg}), true
	}
	return nil, false
}

// Fires the action at the focused card, or runs the command of that id
func (m *Model) Fire(id string) tea.Cmd {
	if c, ok := m.command(id); ok {
		if !c.Ready() {
			return m.nudge()
		}
		m.remember(id)
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
	if m.overlay != nil {
		return m.Dismiss()
	}
	var items []Item
	listed := map[string]bool{}
	for _, c := range m.Commands() {
		if !c.Ready() || listed[c.ID] || c.Title == "" {
			continue
		}
		listed[c.ID] = true
		id, run := c.ID, c.Run
		items = append(items, Item{ID: id, Title: c.Title, Keys: m.keys.Display(id), Run: func() tea.Cmd { m.remember(id); return run() }})
	}
	if e := m.focused(); e != nil {
		title := e.card.Title()
		for _, a := range m.keys.InContexts(contextsOf(e.card)) {
			if a.Quiet || a.Context == keys.Global || listed[a.ID] {
				continue
			}
			listed[a.ID] = true
			id := a.ID
			items = append(items, Item{ID: id, Title: title + ": " + a.Label, Detail: a.Doc, Keys: m.keys.Display(id), Run: func() tea.Cmd { m.remember(id); return m.send(e, ActionMsg{ID: id}) }})
		}
	}
	rank := map[string]int{}
	for i, id := range m.recent {
		rank[id] = len(m.recent) - i
	}
	slices.SortStableFunc(items, func(a, b Item) int { return rank[b.ID] - rank[a.ID] })
	p := NewPalette(m.theme, items)
	p.commands = true
	p.Placeholder = "command"
	return m.ShowOverlay(p)
}
