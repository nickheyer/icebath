package wm

import (
	"slices"
	"strconv"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/ui/keys"
)

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
		{ID: "window.layer.up", Title: "Window: layer up", When: func() bool { e := m.focused(); return e != nil && e.floating && e.layer < maxLayer }, Run: func() tea.Cmd { return m.ShiftLayer(1) }},
		{ID: "window.layer.down", Title: "Window: layer down", When: func() bool { e := m.focused(); return e != nil && e.floating && e.layer > 0 }, Run: func() tea.Cmd { return m.ShiftLayer(-1) }},
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
