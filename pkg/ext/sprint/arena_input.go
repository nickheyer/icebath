package sprint

import (
	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Cells the pointer may be off an actor sideways and still pick it
const pickReach = 3

// Routes a press to the inspector, the approval, the roster chips or the floor
func (a *arena) click(msg tea.MouseClickMsg) tea.Cmd {
	x, y := msg.X, msg.Y
	if a.showHelp {
		a.showHelp = false
		return nil
	}
	if a.lay.insp.contains(x, y) {
		s := a.inspected()
		if s == nil {
			return nil
		}
		if y < a.lay.insp.y+inspectorHead {
			return nil
		}
		shifted := tea.MouseClickMsg{X: x - a.lay.insp.x, Y: y - a.lay.insp.y - inspectorHead, Button: msg.Button, Mod: msg.Mod}
		var cmds []tea.Cmd
		if !a.typing && msg.Button == tea.MouseLeft {
			cmds = append(cmds, a.setTyping(true))
		}
		_, cmd := s.Update(shifted)
		return tea.Batch(append(cmds, cmd)...)
	}
	if r, ok := a.approvalRect(); ok && r.contains(x, y) {
		if ap := a.approval(); ap != nil {
			_, cmd := ap.Update(tea.MouseClickMsg{X: x - r.x - 2, Y: y - r.y - 1, Button: msg.Button, Mod: msg.Mod})
			return cmd
		}
		return nil
	}
	if name, ok := a.chipAt(x, y); ok && msg.Button == tea.MouseLeft {
		if name == a.sel {
			return a.toggleInspect()
		}
		return a.selectMember(name)
	}
	if !a.lay.world.contains(x, y) {
		return nil
	}
	cmd := a.setTyping(false)
	if msg.Button == tea.MouseLeft {
		a.drag = &drag{x: x, y: y}
	}
	return cmd
}

// Ends a camera drag, a press that never moved picking the actor under it
func (a *arena) release(msg tea.MouseReleaseMsg) tea.Cmd {
	if a.lay.insp.contains(msg.X, msg.Y) {
		if s := a.inspected(); s != nil {
			_, cmd := s.Update(tea.MouseReleaseMsg{X: msg.X - a.lay.insp.x, Y: msg.Y - a.lay.insp.y - inspectorHead, Button: msg.Button, Mod: msg.Mod})
			return cmd
		}
		return nil
	}
	if r, ok := a.approvalRect(); ok && r.contains(msg.X, msg.Y) {
		if ap := a.approval(); ap != nil {
			_, cmd := ap.Update(tea.MouseReleaseMsg{X: msg.X - r.x - 2, Y: msg.Y - r.y - 1, Button: msg.Button, Mod: msg.Mod})
			return cmd
		}
	}
	d := a.drag
	a.drag = nil
	if d == nil || d.moved {
		return nil
	}
	name, ok := a.actorAt(msg.X, msg.Y)
	if !ok {
		return nil
	}
	if name == a.sel {
		return a.toggleInspect()
	}
	return a.selectMember(name)
}

// Turns the camera while a drag goes, else tracks the actor under the pointer and hands motion to the inspector
func (a *arena) motion(msg tea.MouseMotionMsg) tea.Cmd {
	if d := a.drag; d != nil && msg.Button == tea.MouseLeft {
		dx, dy := msg.X-d.x, msg.Y-d.y
		if dx != 0 || dy != 0 {
			d.moved = true
			a.rig.yawOff -= float64(dx) * 0.018
			a.rig.pitchOff += float64(dy) * 0.045
			d.x, d.y = msg.X, msg.Y
		}
		return nil
	}
	if a.lay.insp.contains(msg.X, msg.Y) {
		a.hover = ""
		if s := a.inspected(); s != nil {
			_, cmd := s.Update(tea.MouseMotionMsg{X: msg.X - a.lay.insp.x, Y: msg.Y - a.lay.insp.y - inspectorHead, Button: msg.Button, Mod: msg.Mod})
			return cmd
		}
		return nil
	}
	if r, ok := a.approvalRect(); ok && r.contains(msg.X, msg.Y) {
		if ap := a.approval(); ap != nil {
			_, cmd := ap.Update(tea.MouseMotionMsg{X: msg.X - r.x - 2, Y: msg.Y - r.y - 1, Button: msg.Button, Mod: msg.Mod})
			return cmd
		}
	}
	if name, ok := a.chipAt(msg.X, msg.Y); ok {
		a.hover = name
		return nil
	}
	a.hover, _ = a.actorAt(msg.X, msg.Y)
	return nil
}

// Zooms the camera over the floor, scrolls the inspector, the approval dialog or an open panel elsewhere
func (a *arena) wheel(msg tea.MouseWheelMsg) tea.Cmd {
	if a.lay.insp.contains(msg.X, msg.Y) {
		if s := a.inspected(); s != nil {
			_, cmd := s.Update(tea.MouseWheelMsg{X: msg.X - a.lay.insp.x, Y: msg.Y - a.lay.insp.y - inspectorHead, Button: msg.Button, Mod: msg.Mod})
			return cmd
		}
		return nil
	}
	if r, ok := a.approvalRect(); ok && r.contains(msg.X, msg.Y) {
		if ap := a.approval(); ap != nil {
			_, cmd := ap.Update(tea.MouseWheelMsg{X: msg.X - r.x - 2, Y: msg.Y - r.y - 1, Button: msg.Button, Mod: msg.Mod})
			return cmd
		}
	}
	if r, ok := a.panelRect(); ok && r.contains(msg.X, msg.Y) {
		switch msg.Button {
		case tea.MouseWheelDown:
			a.scrollPanel(3)
		case tea.MouseWheelUp:
			a.scrollPanel(-3)
		}
		return nil
	}
	switch msg.Button {
	case tea.MouseWheelUp:
		a.rig.zoom = max(a.rig.zoom*0.9, 0.3)
	case tea.MouseWheelDown:
		a.rig.zoom = min(a.rig.zoom*1.11, 3)
	case tea.MouseWheelLeft:
		a.rig.yawOff -= 0.1
	case tea.MouseWheelRight:
		a.rig.yawOff += 0.1
	}
	return nil
}

// Scrolls the open board or log by lines
func (a *arena) scrollPanel(d int) {
	switch {
	case a.showBoard:
		a.boardTop = max(a.boardTop+d, 0)
	case a.showLog:
		a.logTop = max(a.logTop+d, 0)
	}
}

// Member whose actor stands under a cell of the last paint, the nearest to the camera when several do
func (a *arena) actorAt(x, y int) (string, bool) {
	best, bestDepth := "", 0.0
	for name, s := range a.spots {
		if x < s.x-pickReach || x > s.x+pickReach || y < s.top-1 || y > s.bottom {
			continue
		}
		if best == "" || s.depth < bestDepth {
			best, bestDepth = name, s.depth
		}
	}
	return best, best != ""
}

var _ wm.Card = (*arena)(nil)
