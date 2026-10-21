package cards

import (
	"sort"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/nickheyer/icebath/pkg/ui/deck"
	"github.com/nickheyer/icebath/pkg/ui/render"
)

// One action the palette offers
type Action struct {
	Name string
	Desc string
	Keys string
}

// Fuzzy command palette shown as an overlay
type Palette struct {
	env     *Env
	actions []Action
	query   string
	matches []scored
	sel     int
	w, h    int
}

type scored struct {
	a     Action
	score int
}

func NewPalette(env *Env, actions []Action) *Palette {
	p := &Palette{env: env, actions: actions}
	p.filter()
	return p
}

func (p *Palette) Title() string        { return "PALETTE" }
func (p *Palette) Glyph() string        { return "⌘" }
func (p *Palette) Key() string          { return "palette" }
func (p *Palette) Init() tea.Cmd        { return nil }
func (p *Palette) Capturing() bool      { return true }
func (p *Palette) MinSize() (int, int)  { return 40, 10 }
func (p *Palette) Hints() []key.Binding { return []key.Binding{openHint, releaseHint} }

// Actions matching the query, best first
func (p *Palette) Matches() []Action {
	out := make([]Action, 0, len(p.matches))
	for _, m := range p.matches {
		out = append(out, m.a)
	}
	return out
}

// Score of a fuzzy subsequence match, higher for earlier and adjacent hits, negative for none
func fuzzy(query, text string) int {
	query = strings.ToLower(query)
	text = strings.ToLower(text)
	if query == "" {
		return 0
	}
	score, last := 0, -1
	for _, q := range query {
		idx := strings.IndexRune(text[last+1:], q)
		if idx < 0 {
			return -1
		}
		pos := last + 1 + idx
		score += 10
		if pos == last+1 {
			score += 5
		}
		if pos == 0 || text[pos-1] == ' ' || text[pos-1] == '-' {
			score += 3
		}
		last = pos
	}
	return score - last
}

func (p *Palette) filter() {
	p.matches = p.matches[:0]
	for _, a := range p.actions {
		s := fuzzy(p.query, a.Name+" "+a.Desc)
		if s < 0 {
			continue
		}
		if fuzzy(p.query, a.Name) >= 0 {
			s += 20
		}
		p.matches = append(p.matches, scored{a, s})
	}
	sort.SliceStable(p.matches, func(i, j int) bool { return p.matches[i].score > p.matches[j].score })
	if p.sel >= len(p.matches) {
		p.sel = max(0, len(p.matches)-1)
	}
}

func (p *Palette) Update(msg tea.Msg) (deck.Card, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		case "esc":
			return p, deck.RequestDismiss(p)
		case "enter":
			if p.sel < len(p.matches) {
				name := p.matches[p.sel].a.Name
				return p, tea.Batch(deck.RequestDismiss(p), func() tea.Msg { return PaletteMsg{Command: name} })
			}
		case "down", "ctrl+n", "tab":
			if len(p.matches) > 0 {
				p.sel = (p.sel + 1) % len(p.matches)
			}
		case "up", "ctrl+p", "shift+tab":
			if len(p.matches) > 0 {
				p.sel = (p.sel - 1 + len(p.matches)) % len(p.matches)
			}
		case "backspace":
			if len(p.query) > 0 {
				p.query = p.query[:len(p.query)-1]
				p.filter()
			}
		default:
			if msg.Text != "" {
				p.query += msg.Text
				p.filter()
			}
		}
	case tea.MouseClickMsg:
		if msg.Button == tea.MouseLeft && msg.Y >= 2 && msg.Y-2 < len(p.matches) {
			p.sel = msg.Y - 2
			name := p.matches[p.sel].a.Name
			return p, tea.Batch(deck.RequestDismiss(p), func() tea.Msg { return PaletteMsg{Command: name} })
		}
	}
	return p, nil
}

func (p *Palette) View(w, h int) string {
	p.w, p.h = w, h
	t := p.env.Theme
	lines := []string{lipgloss.NewStyle().Foreground(t.Accent).Render("› ") + lipgloss.NewStyle().Foreground(t.Text).Render(p.query) + lipgloss.NewStyle().Foreground(t.Ice).Render("▏"), ""}
	limit := max(h-2, 1)
	from := 0
	if p.sel >= limit {
		from = p.sel - limit + 1
	}
	for i := from; i < len(p.matches) && i < from+limit; i++ {
		m := p.matches[i]
		name := render.Fit(m.a.Name, min(22, w/3))
		line := name + " " + render.Dim(m.a.Desc, t)
		if m.a.Keys != "" {
			line = render.Fit(line, max(w-len(m.a.Keys)-2, 10)) + render.Cool(m.a.Keys, t)
		}
		if i == p.sel {
			line = lipgloss.NewStyle().Foreground(t.Bg).Background(t.Ice).Bold(true).Render(render.Fit(" "+name+" "+m.a.Desc, w))
		}
		lines = append(lines, render.Truncate(line, w))
	}
	if len(p.matches) == 0 {
		lines = append(lines, render.Dim("no matching command", t))
	}
	return strings.Join(lines, "\n")
}

func (p *Palette) Cursor() *tea.Cursor {
	return tea.NewCursor(2+len([]rune(p.query)), 0)
}

func (p *Palette) Status() string { return "" }

// Keymap reference shown as an overlay
type Help struct {
	env      *Env
	sections []HelpSection
	scroll   int
}

// One group of keys in the help
type HelpSection struct {
	Title string
	Keys  [][2]string
}

func NewHelp(env *Env, sections []HelpSection) *Help { return &Help{env: env, sections: sections} }

func (h *Help) Title() string       { return "HELP" }
func (h *Help) Glyph() string       { return "?" }
func (h *Help) Key() string         { return "help" }
func (h *Help) Init() tea.Cmd       { return nil }
func (h *Help) MinSize() (int, int) { return 40, 10 }

func (h *Help) Update(msg tea.Msg) (deck.Card, tea.Cmd) {
	if k, ok := msg.(tea.KeyPressMsg); ok {
		switch k.String() {
		case "j", "down":
			h.scroll++
		case "k", "up":
			h.scroll = max(0, h.scroll-1)
		case "esc", "q", "?":
			return h, deck.RequestDismiss(h)
		}
	}
	return h, nil
}

func (h *Help) View(w, hh int) string {
	t := h.env.Theme
	var lines []string
	cols := max(1, w/36)
	for _, s := range h.sections {
		lines = append(lines, render.Heading(s.Title, t))
		var row []string
		for _, kv := range s.Keys {
			cell := render.Fit(lipgloss.NewStyle().Foreground(t.Ice).Render(render.Fit(kv[0], 12))+" "+render.Dim(kv[1], t), 34)
			row = append(row, cell)
			if len(row) == cols {
				lines = append(lines, strings.Join(row, "  "))
				row = nil
			}
		}
		if len(row) > 0 {
			lines = append(lines, strings.Join(row, "  "))
		}
		lines = append(lines, "")
	}
	h.scroll = min(h.scroll, max(0, len(lines)-hh))
	lines = lines[h.scroll:min(len(lines), h.scroll+hh)]
	return strings.Join(lines, "\n")
}

func (h *Help) Status() string { return "" }
