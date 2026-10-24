package wm

import (
	"math"
	"sort"
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

// Fuzzy picker shown as the overlay, listing commands, choices, or taking a line of text when it has no items
type Palette struct {
	theme    theme.Theme
	items    []Item
	query    string
	cursor   int
	matches  []scored
	sel      int
	commands bool
	w        int
	rows     int
	// Row of the highlight below the input line, gliding after the selection
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
	p.hl.Set(float64(p.sel - p.from()))
	if dt <= 0 {
		changed := p.glide || p.hl.Pos != p.hl.Target
		p.hl.Snap()
		p.glide = false
		return changed
	}
	p.glide = true
	return p.hl.Step(dt)
}

// Row below the input line the highlight is painted on
func (p *Palette) highlightRow() int {
	if !p.glide {
		return p.sel - p.from()
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

// Rows the palette wants below its input line
func (p *Palette) Rows() int {
	if len(p.items) == 0 {
		return 1
	}
	return max(len(p.matches), 1) + 1
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
		if msg.Button == tea.MouseLeft && msg.Y >= 1 && msg.Y-1+p.from() < len(p.matches) {
			p.sel = msg.Y - 1 + p.from()
			return p, p.pick()
		}
	case tea.MouseMotionMsg:
		if msg.Button == tea.MouseNone && msg.Y >= 1 && msg.X >= 0 && msg.X < p.w && msg.Y-1+p.from() < len(p.matches) {
			p.sel = msg.Y - 1 + p.from()
		}
	case tea.MouseWheelMsg:
		switch msg.Button {
		case tea.MouseWheelDown:
			p.move(1)
		case tea.MouseWheelUp:
			p.move(-1)
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

// First match row shown so the selection stays in view
func (p *Palette) from() int {
	limit := p.limit()
	if p.sel >= limit {
		return p.sel - limit + 1
	}
	return 0
}

func (p *Palette) limit() int { return max(p.rows-1, 1) }

func (p *Palette) View(w, h int) string {
	p.w = w
	p.rows = h
	t := p.theme
	base := lipgloss.NewStyle().Background(t.Surface)
	prompt := base.Foreground(t.Accent).Render(t.Glyphs.Prompt + " ")
	text := p.query
	if text == "" && p.Placeholder != "" {
		text = base.Foreground(t.TextDim).Render(p.Placeholder)
	} else {
		text = base.Foreground(t.Text).Render(text)
	}
	lines := []string{ansi.Truncate(prompt+text, w, "")}
	if len(p.items) == 0 {
		return strings.Join(lines, "\n")
	}
	if len(p.matches) == 0 {
		lines = append(lines, base.Foreground(t.TextDim).Render("  no match"))
		return strings.Join(lines, "\n")
	}
	from := p.from()
	lit := from + p.highlightRow()
	for i := from; i < len(p.matches) && i < from+p.limit(); i++ {
		m := p.matches[i]
		lines = append(lines, p.row(m, i == lit, w))
	}
	return strings.Join(lines, "\n")
}

// One match row with hits lit, the detail dimmed and the keys at the right edge
func (p *Palette) row(m scored, selected bool, w int) string {
	t := p.theme
	bg := t.Surface
	fg, hit, dim, key := t.Text, t.Ice, t.TextDim, t.Accent
	if selected {
		bg = t.Ice
		fg, hit, dim, key = t.Bg, t.Bg, t.Bg, t.Bg
	}
	base := lipgloss.NewStyle().Background(bg)
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
	line := base.Render(" ") + left + base.Render(strings.Repeat(" ", pad)) + right + base.Render(" ")
	return ansi.Truncate(line, w, "")
}

func (p *Palette) Cursor() *tea.Cursor {
	return tea.NewCursor(2+p.cursor, 0)
}

// Actions the palette answers
func (p *Palette) Hints() []Hint {
	if len(p.items) == 0 {
		return []Hint{{Action: "palette.pick", Label: "confirm"}, {Action: "palette.close", Label: "cancel"}}
	}
	return []Hint{{Action: "palette.down", Pair: "palette.up", Label: "move"}, {Action: "palette.pick"}, {Action: "palette.close"}}
}
