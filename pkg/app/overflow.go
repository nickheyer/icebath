package app

import (
	"sort"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Flow a workspace id belongs to and whether it is overflow, empty for a workspace of no flow
func (m *Model) flowOf(id string) (string, bool) {
	for _, f := range m.app.Flows.List() {
		if id == f.ID {
			return f.ID, false
		}
		rest, ok := strings.CutPrefix(id, f.ID+".")
		if !ok {
			continue
		}
		if n, err := strconv.Atoi(rest); err == nil && n > 1 {
			return f.ID, true
		}
	}
	return "", false
}

// Workspaces of the flow, the one named after it first and its overflow after in number order
func (m *Model) flowWorkspaces(flow string) []*wm.Workspace {
	var out []*wm.Workspace
	for _, ws := range m.wm.Workspaces() {
		if have, _ := m.flowOf(ws.ID); have == flow {
			out = append(out, ws)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return overflowNumber(flow, out[i].ID) < overflowNumber(flow, out[j].ID) })
	return out
}

// Number after the flow in an overflow workspace id, one for the workspace named after the flow itself
func overflowNumber(flow, id string) int {
	if id == flow {
		return 1
	}
	n, _ := strconv.Atoi(strings.TrimPrefix(id, flow+"."))
	return n
}

// Id of the next overflow workspace of the flow, the lowest number from two no workspace holds
func (m *Model) overflowID(flow string) string {
	for n := 2; ; n++ {
		if id := flow + "." + strconv.Itoa(n); m.wm.WorkspaceByID(id) == nil {
			return id
		}
	}
}

// Workspace of the flow with room for the card, added when none has it, reporting whether the flow had none
func (m *Model) flowSeat(flow string, c wm.Card) (*wm.Workspace, tea.Cmd, bool) {
	spaces := m.flowWorkspaces(flow)
	for _, ws := range spaces {
		if m.wm.Fits(ws, c) {
			return ws, nil, false
		}
	}
	if m.wm.WorkspaceByID(flow) == nil {
		ws, cmd := m.wm.AddWorkspace(flow)
		return ws, cmd, len(spaces) == 0
	}
	ws, cmd := m.wm.AddWorkspace(m.overflowID(flow))
	return ws, cmd, false
}

// Opens a card of the flow where it fits, showing the workspace when it is the first of the flow
func (m *Model) openInFlow(flow string, c wm.Card) tea.Cmd {
	ws, made, first := m.flowSeat(flow, c)
	cmds := []tea.Cmd{made}
	if first {
		cmds = append(cmds, m.wm.Switch(m.wm.IndexOf(ws)))
	}
	return tea.Batch(append(cmds, m.wm.OpenAside(ws, c, wm.Box{}))...)
}

// Removes every overflow workspace left empty off the stage, the one on the stage kept until the person leaves it
func (m *Model) pruneOverflow() tea.Cmd {
	var cmds []tea.Cmd
	for _, ws := range m.wm.Workspaces() {
		if _, overflow := m.flowOf(ws.ID); !overflow || !ws.Empty() || ws == m.wm.Active() || len(m.wm.Workspaces()) == 1 {
			continue
		}
		delete(m.recent, ws)
		cmds = append(cmds, m.wm.RemoveWorkspace(m.wm.IndexOf(ws)))
	}
	return tea.Batch(cmds...)
}
