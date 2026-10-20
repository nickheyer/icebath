package deck

import (
	"strconv"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

const (
	minHintW = 24
	minHintH = 6
	hintGap  = 2
)

// Key hint shown on the bar, clickable when it carries an action
type hint struct {
	key, desc string
	act       func() tea.Cmd
}

// Column span of a clickable hint on the bar row
type zone struct {
	x0, x1 int
	act    func() tea.Cmd
}

// Rows the hint bar takes from the bottom of the screen
func (m *Model) barRows() int {
	if m.hints && m.width >= minHintW && m.height >= minHintH {
		return 1
	}
	return 0
}

// Rows left for cards above the hint bar
func (m *Model) stageHeight() int { return m.height - m.barRows() }

// Shows or hides the hint bar and card ordinals, reflowing the cards into the space
func (m *Model) toggleHints() tea.Cmd {
	m.hints = !m.hints
	m.hintHot = -1
	m.retarget()
	return m.clock.Wake()
}

// Index of the clickable hint under the point, or -1
func (m *Model) hintAt(x, y int) int {
	if m.barRows() == 0 || y != m.height-1 {
		return -1
	}
	for i, z := range m.zones {
		if x >= z.x0 && x < z.x1 {
			return i
		}
	}
	return -1
}

// Hints that fit the current mode, focus, and capture state, led by the focused card's own keys
func (m *Model) hintsFor() []hint {
	k := m.Keys
	n := len(m.cards)
	hs := m.cardHints()
	add := func(b key.Binding, desc string, act func() tea.Cmd) {
		if l := keyLabel(b); l != "" {
			hs = append(hs, hint{l, desc, act})
		}
	}
	pair := func(a, b key.Binding, desc string, act func() tea.Cmd) {
		if la, lb := keyLabel(a), keyLabel(b); la != "" && lb != "" {
			hs = append(hs, hint{la + "/" + lb, desc, act})
		}
	}
	next := func() tea.Cmd { return m.cycle(1) }
	quit := func() tea.Cmd { return tea.Quit }
	switch {
	case n == 0:
		add(k.Help, "hide", m.toggleHints)
		add(k.Quit, "quit", quit)
		return hs
	case m.Capturing():
		hs = append(hs, hint{"tab", "next", next}, hint{"shift+tab", "prev", func() tea.Cmd { return m.cycle(-1) }}, hint{"ctrl+c", "quit", quit})
		return hs
	case m.mode == Solo:
		add(k.Back, "split", m.back)
		add(k.Next, "next", next)
	case n == 1:
		add(k.Enter, "solo", m.dive)
	case m.focus == 0:
		add(k.Enter, "solo", m.dive)
		add(k.ToStack, "stack", m.toStack)
		add(k.Next, "next", next)
	default:
		add(k.Enter, "promote", m.dive)
		add(k.ToMain, "main", m.toMain)
		add(k.Next, "next", next)
	}
	add(k.Close, "close", func() tea.Cmd { return m.Remove(m.focus) })
	add(k.Quit, "quit", quit)
	if n > 1 {
		add(k.Jump, "jump", nil)
	}
	switch {
	case m.mode == Solo || n == 1:
	case m.focus == 0:
		pair(k.Grow, k.Shrink, "resize", func() tea.Cmd { return m.resize(ratioStep) })
	default:
		pair(k.MoveDown, k.MoveUp, "move", func() tea.Cmd { return m.shift(1) })
	}
	add(k.Help, "hide", m.toggleHints)
	return hs
}

// Keys the focused card advertises, shown without click actions since the card owns them
func (m *Model) cardHints() []hint {
	if len(m.cards) == 0 {
		return nil
	}
	c, ok := m.cards[m.focus].card.(Hinter)
	if !ok {
		return nil
	}
	var hs []hint
	for _, b := range c.Hints() {
		if l := keyLabel(b); l != "" && b.Enabled() {
			hs = append(hs, hint{l, b.Help().Desc, nil})
		}
	}
	return hs
}

// Short key name of a binding from its help text or first key
func keyLabel(b key.Binding) string {
	if h := b.Help().Key; h != "" {
		return h
	}
	if ks := b.Keys(); len(ks) > 0 {
		return ks[0]
	}
	return ""
}

// Left end of the bar naming the hovered or focused card, its place in the deck, and the mode
func (m *Model) stageLabel() string {
	t := m.theme
	bg := lipgloss.NewStyle().Background(t.Bg)
	n := len(m.cards)
	if n == 0 {
		return bg.Foreground(t.TextDim).Render(emptyLabel)
	}
	i := m.focus
	if m.hover >= 0 && m.hover < n {
		i = m.hover
	}
	e := m.cards[i]
	s := bg.Foreground(t.Accent).Render(e.card.Glyph()+" ") +
		bg.Foreground(t.Text).Bold(true).Render(e.card.Title()) +
		bg.Foreground(t.TextDim).Render(" "+strconv.Itoa(i+1)+"/"+strconv.Itoa(n))
	switch {
	case i != m.focus:
	case m.Capturing():
		s += bg.Foreground(t.Pulse).Render(" capturing")
	case m.mode == Solo:
		s += bg.Foreground(t.Ice).Render(" solo")
	}
	return s
}

// Draws the hint bar along the bottom row and records the clickable spans
func (m *Model) paintHints(buf screen) {
	m.zones = m.zones[:0]
	if m.barRows() == 0 {
		return
	}
	t := m.theme
	y := m.height - 1
	bg := lipgloss.NewStyle().Background(t.Bg)
	left := ansi.Truncate(m.stageLabel(), m.width-2, "")
	lw := ansi.StringWidth(left)
	uv.NewStyledString(left).Draw(buf, uv.Rect(1, y, lw, 1))
	x := 1 + lw + 2*hintGap
	for _, h := range m.hintsFor() {
		hot := h.act != nil && len(m.zones) == m.hintHot
		kc, dc := t.Ice, t.TextDim
		if hot {
			kc, dc = t.Accent, t.Text
		}
		seg := bg.Foreground(kc).Render(h.key) + bg.Foreground(dc).Render(" "+h.desc)
		sw := ansi.StringWidth(seg)
		if x+sw > m.width-1 {
			break
		}
		uv.NewStyledString(seg).Draw(buf, uv.Rect(x, y, sw, 1))
		if h.act != nil {
			m.zones = append(m.zones, zone{x, x + sw, h.act})
		}
		x += sw + hintGap
	}
}
