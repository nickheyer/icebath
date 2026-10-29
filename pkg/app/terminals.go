package app

import (
	"strconv"
	"strings"
	"sync"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Key of a terminal window nothing holds yet
func (m *Model) terminalKey() string {
	m.termSeq++
	return "terminal:" + strconv.Itoa(m.termSeq)
}

// Terminal window with the id, built on first use
func (m *Model) terminal(id int) *cards.Terminal {
	if t, ok := m.terms[id]; ok {
		return t
	}
	t := cards.NewTerminal(m.env, id, "")
	m.terms[id] = t
	m.termSeq = max(m.termSeq, id)
	return t
}

// Terminal windows open in the workspace, in stage order
func (m *Model) terminalsIn(ws *wm.Workspace) []*cards.Terminal {
	var out []*cards.Terminal
	if ws == nil {
		return nil
	}
	for _, c := range ws.Cards() {
		if t, ok := c.(*cards.Terminal); ok {
			out = append(out, t)
		}
	}
	return out
}

// Ends every shell at once and waits for them
func (m *Model) closeTerminals() {
	var wg sync.WaitGroup
	for _, t := range m.terms {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t.Close()
		}()
	}
	wg.Wait()
}

// Forgets terminal windows that are no longer open anywhere
func (m *Model) pruneTerminals() {
	for id, t := range m.terms {
		if !m.wm.Has(t.Key()) {
			delete(m.terms, id)
		}
	}
}

// Opens a fresh terminal beside the session on the stage, typing the line into it when one is given
func (m *Model) openTerminal(line string) tea.Cmd {
	m.pruneTerminals()
	m.termSeq++
	t := m.terminal(m.termSeq)
	ws := m.wm.Active()
	var origin wm.Box
	if d := m.current(); d != nil {
		ws, origin = m.seat(d)
	}
	if ws == nil {
		ws, cmd := m.wm.AddWorkspace(m.workspaceID(), t)
		return tea.Batch(cmd, m.wm.Switch(m.wm.IndexOf(ws)), t.Run(line))
	}
	return tea.Batch(m.wm.OpenFrom(ws, t, origin), t.Run(line))
}

// Types the line into the terminal of the workspace of the desk, the focused one when several, opening one when none
func (m *Model) runLine(d *desk, line string) tea.Cmd {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil
	}
	ws := m.workspaceOf(d)
	if ws == nil {
		ws = m.wm.Active()
	}
	terms := m.terminalsIn(ws)
	if len(terms) == 0 {
		return m.openTerminal(line)
	}
	t := terms[0]
	for _, have := range terms {
		if wm.Card(have) == ws.Focused() {
			t = have
		}
	}
	return tea.Batch(t.Run(line), m.wm.Attention(t, wm.Calm))
}
