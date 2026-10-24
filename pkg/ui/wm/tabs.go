package wm

import (
	"encoding/json"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/theme"
)

// Rows above the active card, the strip and the blank row under it
const tabHead = 2

// Several cards sharing one frame behind a tab strip, the active one receiving input and filling the rest
type Tabs struct {
	title  string
	glyph  string
	cards  []Card
	active int
	theme  theme.Theme
	spans  []int
	first  int
}

// Builds a tab group under a title and glyph holding the cards in order
func NewTabs(t theme.Theme, title, glyph string, cards ...Card) *Tabs {
	return &Tabs{title: title, glyph: glyph, cards: cards, theme: t}
}

// Replaces the palette the strip paints with
func (g *Tabs) SetTheme(t theme.Theme) { g.theme = t }

func (g *Tabs) Title() string { return g.title }
func (g *Tabs) Glyph() string { return g.glyph }
func (g *Tabs) Key() string   { return g.title }

func (g *Tabs) Init() tea.Cmd {
	cmds := make([]tea.Cmd, 0, len(g.cards))
	for _, c := range g.cards {
		cmds = append(cmds, c.Init())
	}
	return tea.Batch(cmds...)
}

// Cards held, in tab order
func (g *Tabs) Cards() []Card { return append([]Card(nil), g.cards...) }

// Tab shown, kept between runs by the key of its card
type tabsState struct {
	Tab string `json:"tab,omitempty"`
}

// Key of the active tab for the next run
func (g *Tabs) State() any {
	c, _ := g.Active()
	if c == nil {
		return tabsState{}
	}
	return tabsState{Tab: keyOf(c)}
}

// Shows the tab a saved state names, keeping the current one when no card has its key
func (g *Tabs) SetState(raw json.RawMessage) error {
	var st tabsState
	if err := json.Unmarshal(raw, &st); err != nil {
		return err
	}
	for i, c := range g.cards {
		if keyOf(c) == st.Tab {
			g.active = i
			return nil
		}
	}
	return nil
}

// Card under the active tab and its index
func (g *Tabs) Active() (Card, int) {
	if len(g.cards) == 0 {
		return nil, -1
	}
	return g.cards[g.active], g.active
}

func (g *Tabs) Len() int { return len(g.cards) }

// Reports whether the card is one of the tabs
func (g *Tabs) Holds(c Card) bool {
	for _, held := range g.cards {
		if held == c {
			return true
		}
	}
	return false
}

// Switches the active tab, telling the old and new cards about focus
func (g *Tabs) Select(i int) tea.Cmd {
	if i < 0 || i >= len(g.cards) || i == g.active {
		return nil
	}
	var cmds []tea.Cmd
	_, cmd := g.cards[g.active].Update(FocusMsg{})
	cmds = append(cmds, cmd)
	g.active = i
	_, cmd = g.cards[i].Update(FocusMsg{Focused: true})
	cmds = append(cmds, cmd)
	return tea.Batch(cmds...)
}

func (g *Tabs) cycle(d int) tea.Cmd {
	n := len(g.cards)
	if n < 2 {
		return nil
	}
	return g.Select(((g.active+d)%n + n) % n)
}

func (g *Tabs) MinSize() (int, int) {
	w, h := 0, 0
	for _, c := range g.cards {
		cw, ch := minSize(c)
		w, h = max(w, cw), max(h, ch)
	}
	return w, h + tabHead
}

func (g *Tabs) Selectable() bool {
	s, ok := g.current().(Selectable)
	return ok && s.Selectable()
}

// Takes every key as typed while the active tab does
func (g *Tabs) Raw() bool { return raw(g.current()) }

func (g *Tabs) Status() string {
	if s, ok := g.current().(Statuser); ok {
		return s.Status()
	}
	return ""
}

// Cursor of the active card moved down past the strip and its padding
func (g *Tabs) Cursor() *tea.Cursor {
	c, ok := g.current().(Cursorer)
	if !ok {
		return nil
	}
	cur := c.Cursor()
	if cur == nil {
		return nil
	}
	out := *cur
	out.Y += tabHead
	return &out
}

func (g *Tabs) current() Card {
	if len(g.cards) == 0 {
		return nil
	}
	return g.cards[g.active]
}

// Contexts of the active tab, then the tab strip's own unless the tab takes every key raw
func (g *Tabs) Contexts() []string {
	out := append([]string(nil), contextsOf(g.current())...)
	if raw(g.current()) {
		return out
	}
	return append(out, keys.Tabs)
}

func (g *Tabs) Update(msg tea.Msg) (Card, tea.Cmd) {
	switch msg := msg.(type) {
	case ActionMsg:
		switch msg.ID {
		case "tabs.next":
			return g, g.cycle(1)
		case "tabs.prev":
			return g, g.cycle(-1)
		}
		return g, g.forward(msg)
	case tea.KeyPressMsg:
		return g, g.forward(msg)
	case tea.MouseClickMsg:
		if msg.Y == 0 {
			if i := g.tabAt(msg.X); i >= 0 && msg.Button == tea.MouseLeft {
				return g, g.Select(i)
			}
			return g, nil
		}
		if msg.Y < tabHead {
			return g, nil
		}
		return g, g.forward(shiftMouse(msg))
	case tea.MouseReleaseMsg, tea.MouseWheelMsg, tea.MouseMotionMsg:
		return g, g.forward(shiftMouse(msg.(tea.MouseMsg)))
	case FocusMsg:
		return g, g.forward(msg)
	}
	cmds := make([]tea.Cmd, 0, len(g.cards))
	for i, c := range g.cards {
		next, cmd := c.Update(msg)
		if next != nil {
			g.cards[i] = next
		}
		cmds = append(cmds, cmd)
	}
	return g, tea.Batch(cmds...)
}

func (g *Tabs) forward(msg tea.Msg) tea.Cmd {
	if len(g.cards) == 0 {
		return nil
	}
	next, cmd := g.cards[g.active].Update(msg)
	if next != nil {
		g.cards[g.active] = next
	}
	return cmd
}

// Mouse event moved up past the strip and its padding so the active card sees content coordinates
func shiftMouse(msg tea.MouseMsg) tea.Msg {
	mo := msg.Mouse()
	mo.Y -= tabHead
	return relocate(msg, mo)
}

// Index of the tab under column x on the strip, or -1
func (g *Tabs) tabAt(x int) int {
	for i := 0; i+1 < len(g.spans); i++ {
		if x >= g.spans[i] && x < g.spans[i+1] {
			return g.first + i
		}
	}
	return -1
}

// Label of tab i on the strip
func (g *Tabs) label(i int) string {
	return " " + glyphOf(g.cards[i], g.theme) + " " + g.cards[i].Title() + " "
}

// Lowest first tab the strip can draw with the active one still inside w, a marker standing for those scrolled off
func (g *Tabs) scrollFor(w int) int {
	first := 0
	fits := func(from int) bool {
		x := 0
		if from > 0 {
			x = 1
		}
		for i := from; i <= g.active; i++ {
			x += ansi.StringWidth(g.label(i))
		}
		return x <= w
	}
	for first < g.active && !fits(first) {
		first++
	}
	return first
}

func (g *Tabs) View(w, h int) string {
	t := g.theme
	g.spans = g.spans[:0]
	var sb strings.Builder
	x := 0
	if len(g.cards) > 0 {
		g.first = g.scrollFor(w)
	}
	if g.first > 0 {
		sb.WriteString(lipgloss.NewStyle().Foreground(t.TextDim).Render(t.Glyphs.Back))
		x = 1
	}
	for i := g.first; i < len(g.cards); i++ {
		st := lipgloss.NewStyle().Foreground(t.TextDim)
		if i == g.active {
			st = lipgloss.NewStyle().Foreground(t.Text).Background(t.Steel).Bold(true)
		}
		seg := st.Render(g.label(i))
		sw := ansi.StringWidth(seg)
		if x+sw > w {
			break
		}
		g.spans = append(g.spans, x)
		sb.WriteString(seg)
		x += sw
	}
	g.spans = append(g.spans, x)
	if x < w {
		sb.WriteString(lipgloss.NewStyle().Foreground(t.Steel).Render(strings.Repeat(t.Glyphs.Rule, w-x)))
	}
	if h <= tabHead || len(g.cards) == 0 {
		return sb.String()
	}
	return sb.String() + "\n\n" + g.cards[g.active].View(w, h-tabHead)
}

// Keeps every tab's own motion in step, reporting whether the shown one moved
func (g *Tabs) Animate(dt float64) bool {
	moved := false
	for i, c := range g.cards {
		a, ok := c.(Animator)
		if !ok {
			continue
		}
		if a.Animate(dt) && i == g.active {
			moved = true
		}
	}
	return moved
}

// Actions of the active tab, then the tab switching one
func (g *Tabs) Hints() []Hint {
	return append(append([]Hint(nil), hintsOf(g.current())...), Hint{Action: "tabs.next", Label: "next tab"})
}
