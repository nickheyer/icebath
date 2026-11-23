package wm

import (
	"slices"

	tea "charm.land/bubbletea/v2"
	"github.com/nickheyer/icebath/pkg/ui/lang"
	"github.com/nickheyer/icebath/pkg/ui/txt"
)

// Drops a card from the tabs, the tab beside it shown when it was the active one
func (g *Tabs) drop(c Card) bool {
	i := slices.Index(g.cards, c)
	if i < 0 {
		return false
	}
	g.cards = slices.Delete(g.cards, i, i+1)
	if i < g.active || g.active >= len(g.cards) {
		g.active = max(g.active-1, 0)
	}
	return true
}

// Held card with the key, nil when no tab has it
func (g *Tabs) held(key string) Card {
	for _, c := range g.cards {
		if keyOf(c) == key {
			return c
		}
	}
	return nil
}

// New tab group around a card, numbered so its key is unique
func (m *Model) newGroup(c Card) *Tabs {
	m.groups++
	return NewTabs(m.theme, lang.T(txt.KitWmTabsTitle, m.groups), m.theme.Glyphs.Card, c)
}

// Tile beside a tiled entry, the next one in order, else the one before it
func besideTile(ws *Workspace, e *entry) *entry {
	i := slices.Index(ws.tiles, e)
	switch {
	case i < 0:
		return nil
	case i+1 < len(ws.tiles):
		return ws.tiles[i+1]
	case i > 0:
		return ws.tiles[i-1]
	}
	return nil
}

// Moves the focused window into the tile beside it as its shown tab, the tile becoming a tab group
func (m *Model) JoinTab() tea.Cmd {
	ws, e := m.Active(), m.focused()
	if ws == nil || e == nil || e.floating {
		return m.nudge()
	}
	target := besideTile(ws, e)
	if target == nil {
		return m.nudge()
	}
	moving := e.card
	_, cmd := moving.Update(FocusMsg{})
	cmds := []tea.Cmd{cmd}
	ws.remove(e)
	g, grouped := target.card.(*Tabs)
	if !grouped {
		g = m.newGroup(target.card)
		target.card = g
	} else if shown, _ := g.Active(); shown != nil {
		_, cmd := shown.Update(FocusMsg{})
		cmds = append(cmds, cmd)
	}
	g.cards = append(g.cards, moving)
	g.active = len(g.cards) - 1
	target.dirty = true
	m.retarget(ws)
	return tea.Batch(append(cmds, m.focusEntry(target))...)
}

// Takes the shown tab of the focused group out into a tile of its own, a group left with one tab dissolving
func (m *Model) SplitTab() tea.Cmd {
	ws, e := m.Active(), m.focused()
	if ws == nil || e == nil {
		return m.nudge()
	}
	g, grouped := e.card.(*Tabs)
	if !grouped || len(g.cards) < 2 {
		return m.nudge()
	}
	c, _ := g.Active()
	_, cmd := c.Update(FocusMsg{})
	cmds := []tea.Cmd{cmd}
	g.drop(c)
	if len(g.cards) == 1 {
		e.card = g.cards[0]
	}
	e.dirty = true
	ne := m.adopt(ws, c, e.frame())
	m.retarget(ws)
	return tea.Batch(append(cmds, m.focusEntry(ne), m.wake())...)
}

// Shows the next tab of the focused group, or the one before it
func (m *Model) CycleTab(d int) tea.Cmd {
	e := m.focused()
	if e == nil {
		return m.nudge()
	}
	g, grouped := e.card.(*Tabs)
	if !grouped || len(g.cards) < 2 {
		return m.nudge()
	}
	e.dirty = true
	return tea.Batch(g.cycle(d), m.wake())
}

// Opens a card as a new shown tab of the focused tile, on its own when no tile has the focus
func (m *Model) OpenTab(c Card) tea.Cmd {
	ws, e := m.Active(), m.focused()
	if ws == nil || c == nil {
		return nil
	}
	if e == nil || e.floating {
		return m.Open(ws, c)
	}
	g, grouped := e.card.(*Tabs)
	if !grouped {
		g = m.newGroup(e.card)
		e.card = g
	}
	shown, _ := g.Active()
	_, cmd := shown.Update(FocusMsg{})
	g.cards = append(g.cards, c)
	g.active = len(g.cards) - 1
	e.dirty = true
	m.retarget(ws)
	return tea.Batch(cmd, m.routed(e, c.Init()), m.send(e, FocusMsg{Focused: ws.focus == e}), m.wake())
}

// Closes one tab of a group, the group dissolving into its last tab and closing with none left
func (m *Model) closeTab(e *entry, g *Tabs, c Card) tea.Cmd {
	_, focusCmd := c.Update(FocusMsg{})
	_, closeCmd := c.Update(ClosedMsg{})
	cmds := []tea.Cmd{focusCmd, closeCmd}
	g.drop(c)
	switch len(g.cards) {
	case 0:
		return tea.Batch(append(cmds, m.closeEntry(e))...)
	case 1:
		e.card = g.cards[0]
	}
	e.dirty = true
	if e.ws != nil && e.ws.focus == e && e.ws == m.Active() {
		cmds = append(cmds, m.send(e, FocusMsg{Focused: true}))
	}
	return tea.Batch(append(cmds, m.wake())...)
}

// Group entry holding the card as one of its tabs and the group, nil when it is not held
func (m *Model) groupOf(c Card) (*entry, *Tabs) {
	e := m.entryOf(c)
	if e == nil || e.card == c {
		return nil, nil
	}
	g, grouped := e.card.(*Tabs)
	if !grouped {
		return nil, nil
	}
	return e, g
}
