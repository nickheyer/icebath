package deck

import (
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/theme"
)

// Several cards sharing one slot behind a tab strip, the active one receiving input and filling the rest
type Group struct {
	title  string
	glyph  string
	cards  []Card
	active int
	theme  theme.Theme
	w      int
	spans  []int
}

var (
	nextTab = key.NewBinding(key.WithKeys("]"), key.WithHelp("]", "next tab"))
	prevTab = key.NewBinding(key.WithKeys("["), key.WithHelp("[", "prev tab"))
)

// Builds a group under a title and glyph holding the cards in order
func NewGroup(title, glyph string, cards ...Card) *Group {
	return &Group{title: title, glyph: glyph, cards: cards, theme: theme.Blackwall()}
}

// Replaces the palette the tab strip paints with
func (g *Group) SetTheme(t theme.Theme) { g.theme = t }

func (g *Group) Title() string {
	if len(g.cards) == 0 {
		return g.title
	}
	return g.title + " " + g.cards[g.active].Title()
}

func (g *Group) Glyph() string { return g.glyph }

// Identity for saved layouts, the group title
func (g *Group) Key() string { return g.title }

func (g *Group) Init() tea.Cmd {
	cmds := make([]tea.Cmd, 0, len(g.cards))
	for _, c := range g.cards {
		cmds = append(cmds, c.Init())
	}
	return tea.Batch(cmds...)
}

// Cards held, in tab order
func (g *Group) Cards() []Card { return append([]Card(nil), g.cards...) }

// Card under the active tab
func (g *Group) Active() (Card, int) {
	if len(g.cards) == 0 {
		return nil, -1
	}
	return g.cards[g.active], g.active
}

func (g *Group) Len() int { return len(g.cards) }

// Reports whether the card is one of the tabs
func (g *Group) Holds(c Card) bool {
	for _, held := range g.cards {
		if held == c {
			return true
		}
	}
	return false
}

// Appends a tab and makes it active
func (g *Group) Add(c Card) tea.Cmd {
	g.cards = append(g.cards, c)
	cmds := []tea.Cmd{c.Init()}
	cmds = append(cmds, g.Select(len(g.cards)-1))
	return tea.Batch(cmds...)
}

// Removes the tab at i, reporting whether one was there
func (g *Group) Remove(i int) (tea.Cmd, bool) {
	if i < 0 || i >= len(g.cards) {
		return nil, false
	}
	gone := g.cards[i]
	g.cards = append(g.cards[:i], g.cards[i+1:]...)
	var cmds []tea.Cmd
	if i == g.active {
		_, cmd := gone.Update(FocusMsg{})
		cmds = append(cmds, cmd)
	}
	if g.active >= len(g.cards) {
		g.active = max(0, len(g.cards)-1)
	} else if i < g.active {
		g.active--
	}
	if len(g.cards) > 0 && len(cmds) > 0 {
		_, cmd := g.cards[g.active].Update(FocusMsg{Focused: true})
		cmds = append(cmds, cmd)
	}
	return tea.Batch(cmds...), true
}

// Switches the active tab, telling the old and new cards about focus
func (g *Group) Select(i int) tea.Cmd {
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

func (g *Group) cycle(d int) tea.Cmd {
	n := len(g.cards)
	if n < 2 {
		return nil
	}
	return g.Select(((g.active+d)%n + n) % n)
}

// Tab switching keys followed by whatever the active card claims
func (g *Group) Claims() []string {
	out := []string{"[", "]"}
	if c, ok := g.active_().(Claimer); ok {
		out = append(out, c.Claims()...)
	}
	return out
}

func (g *Group) Capturing() bool {
	c, ok := g.active_().(Capturer)
	return ok && c.Capturing()
}

func (g *Group) Pinned() bool {
	for _, c := range g.cards {
		if isPinned(c) {
			return true
		}
	}
	return false
}

func (g *Group) MinSize() (int, int) {
	w, h := 0, 0
	for _, c := range g.cards {
		cw, ch := minSize(c)
		w, h = max(w, cw), max(h, ch)
	}
	return w, h + 1
}

func (g *Group) Selectable() bool {
	s, ok := g.active_().(Selectable)
	return ok && s.Selectable()
}

func (g *Group) Status() string {
	if s, ok := g.active_().(Statuser); ok {
		return s.Status()
	}
	return ""
}

// Cursor of the active card moved down past the tab strip
func (g *Group) Cursor() *tea.Cursor {
	c, ok := g.active_().(Cursorer)
	if !ok {
		return nil
	}
	cur := c.Cursor()
	if cur == nil {
		return nil
	}
	out := *cur
	out.Y++
	return &out
}

// Tab keys followed by whatever the active card advertises
func (g *Group) Hints() []key.Binding {
	hs := []key.Binding{nextTab, prevTab}
	if h, ok := g.active_().(Hinter); ok {
		hs = append(hs, h.Hints()...)
	}
	return hs
}

func (g *Group) active_() Card {
	if len(g.cards) == 0 {
		return nil
	}
	return g.cards[g.active]
}

func (g *Group) Update(msg tea.Msg) (Card, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		case "]":
			return g, g.cycle(1)
		case "[":
			return g, g.cycle(-1)
		}
		return g, g.forward(msg)
	case tea.MouseClickMsg:
		if msg.Y == 0 {
			if i := g.tabAt(msg.X); i >= 0 && msg.Button == tea.MouseLeft {
				return g, g.Select(i)
			}
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

func (g *Group) forward(msg tea.Msg) tea.Cmd {
	if len(g.cards) == 0 {
		return nil
	}
	next, cmd := g.cards[g.active].Update(msg)
	if next != nil {
		g.cards[g.active] = next
	}
	return cmd
}

// Mouse event moved up one row so the active card sees content coordinates
func shiftMouse(msg tea.MouseMsg) tea.Msg {
	mo := msg.Mouse()
	mo.Y--
	switch msg.(type) {
	case tea.MouseClickMsg:
		return tea.MouseClickMsg(mo)
	case tea.MouseReleaseMsg:
		return tea.MouseReleaseMsg(mo)
	case tea.MouseWheelMsg:
		return tea.MouseWheelMsg(mo)
	}
	return tea.MouseMotionMsg(mo)
}

// Index of the tab under column x on the strip, or -1
func (g *Group) tabAt(x int) int {
	for i := 0; i+1 < len(g.spans); i++ {
		if x >= g.spans[i] && x < g.spans[i+1] {
			return i
		}
	}
	return -1
}

func (g *Group) View(w, h int) string {
	g.w = w
	t := g.theme
	g.spans = g.spans[:0]
	var sb strings.Builder
	x := 0
	for i, c := range g.cards {
		label := " " + c.Glyph() + " " + c.Title() + " "
		st := lipgloss.NewStyle().Foreground(t.TextDim)
		if i == g.active {
			st = lipgloss.NewStyle().Foreground(t.Text).Background(t.Steel).Bold(true)
		}
		seg := st.Render(label)
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
		sb.WriteString(lipgloss.NewStyle().Foreground(t.Steel).Render(strings.Repeat("─", w-x)))
	}
	if h <= 1 || len(g.cards) == 0 {
		return sb.String()
	}
	return sb.String() + "\n" + g.cards[g.active].View(w, h-1)
}
