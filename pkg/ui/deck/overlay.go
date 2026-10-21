package deck

import (
	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/ui/anim"
)

const (
	overlayShare = 0.6
	overlayMinW  = 30
	overlayMinH  = 8
	backdropDim  = 0.55
)

// Shows a card above everything with a dimmed backdrop and first claim on input, replacing any overlay already up
func (m *Model) ShowOverlay(c Card) tea.Cmd {
	var cmds []tea.Cmd
	if m.overlay != nil {
		cmds = append(cmds, m.send(m.overlay, FocusMsg{}))
	} else if len(m.cards) > 0 {
		cmds = append(cmds, m.send(m.cards[m.focus], FocusMsg{}))
	}
	m.letGo()
	m.sel = nil
	e := &entry{id: m.nextID, card: c, glow: fast(0), hover: fast(0), lift: fast(1), pulse: fast(0), dim: -1, dirty: true, decode: 0}
	m.nextID++
	m.overlay = e
	cmds = append(cmds, c.Init())
	m.placeOverlay()
	x, y, w, h := e.rect.Target()
	e.rect = anim.NewRect(float64(x+3), float64(y+1), float64(max(1, w-6)), float64(max(1, h-2)))
	e.rect.Set(float64(x), float64(y), float64(w), float64(h))
	if m.motion == 0 {
		e.rect.Snap()
	}
	cmds = append(cmds, m.send(e, FocusMsg{Focused: true}))
	return tea.Batch(append(cmds, m.wake())...)
}

// Centers the overlay at its share of the stage, honoring the size it declares
func (m *Model) placeOverlay() {
	if m.overlay == nil || m.width == 0 || m.height == 0 {
		return
	}
	minW, minH := minSize(m.overlay.card)
	sw, sh := m.width, m.stageHeight()
	w := max(int(float64(sw)*overlayShare), overlayMinW, minW+2)
	h := max(int(float64(sh)*overlayShare), overlayMinH, minH+2)
	w = min(w, max(1, sw-2))
	h = min(h, max(1, sh))
	x := (sw - w) / 2
	y := (sh - h) / 2
	m.overlay.rect.Set(float64(x), float64(y), float64(w), float64(h))
}

// Drops the overlay and hands focus back to the focused card
func (m *Model) Dismiss() tea.Cmd {
	if m.overlay == nil {
		return nil
	}
	e := m.overlay
	m.overlay = nil
	cmds := []tea.Cmd{m.send(e, FocusMsg{})}
	e.rect.Set(e.rect.X.Target, float64(m.stageHeight()+2), e.rect.W.Target, e.rect.H.Target)
	m.leaving = append(m.leaving, e)
	if len(m.cards) > 0 {
		cmds = append(cmds, m.send(m.cards[m.focus], FocusMsg{Focused: true}))
	}
	return tea.Batch(append(cmds, m.wake())...)
}

// Card shown above the deck, nil when none is up
func (m *Model) Overlay() Card {
	if m.overlay == nil {
		return nil
	}
	return m.overlay.card
}

// Reports whether the point lies inside the overlay content
func (m *Model) inOverlay(x, y int) bool {
	if m.overlay == nil {
		return false
	}
	cx, cy, cw, ch := m.overlay.rect.Cells()
	return cw >= 3 && ch >= 3 && x > cx && x < cx+cw-1 && y > cy && y < cy+ch-1
}
