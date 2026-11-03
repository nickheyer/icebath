package sprint

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Cells the pointer may be off an actor sideways and still pick it
const pickReach = 3

// Radians the camera turns per cell the pointer drags sideways and per cell it drags up or down
const (
	dragYaw   = 0.018
	dragPitch = 0.045
)

// Routes a press to the toolbar, the panel edges, the dock rows, the inspector, the approval or the floor
func (a *arena) click(msg tea.MouseClickMsg) tea.Cmd {
	x, y := msg.X, msg.Y
	if a.showHelp {
		a.showHelp = false
		return nil
	}
	if msg.Button == tea.MouseLeft {
		switch {
		case a.lay.insp.w > 0 && !a.lay.full && x == a.lay.insp.x && y >= a.lay.insp.y:
			a.drag = &drag{kind: dragInspector, x: x, y: y, base: float64(a.lay.insp.w)}
			return a.setTyping(false)
		case a.onDock(x, y) && x == a.lay.dock.x+a.lay.dock.w-1:
			a.drag = &drag{kind: dragDock, x: x, y: y, base: float64(a.lay.dock.w)}
			return a.setTyping(false)
		case a.onDock(x, y) && y == a.secs.Split():
			a.drag = &drag{kind: dragSplit, x: x, y: y, base: 1 - a.secs.Share}
			return nil
		}
	}
	if a.off() && a.lay.world.contains(x, y) {
		return a.clickTile(msg)
	}
	if id, ok := a.hitAt(x, y); ok && msg.Button == tea.MouseLeft && !a.lay.insp.contains(x, y) {
		return a.press(id)
	}
	if a.lay.insp.contains(x, y) {
		if id, ok := a.hitAt(x, y); ok && msg.Button == tea.MouseLeft {
			return a.press(id)
		}
		s := a.inspected()
		if s == nil || y < a.lay.insp.y+a.inspHead {
			return nil
		}
		shifted := tea.MouseClickMsg{X: x - a.inspX(), Y: y - a.lay.insp.y - a.inspHead, Button: msg.Button, Mod: msg.Mod}
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
	if a.onDock(x, y) || a.lay.bar.contains(x, y) {
		return nil
	}
	if !a.lay.world.contains(x, y) {
		return nil
	}
	cmd := a.setTyping(false)
	if msg.Button == tea.MouseLeft {
		a.drag = &drag{kind: dragCamera, x: x, y: y}
	}
	return cmd
}

// Routes a press on the session windows, selecting its member and handing presses under the header to the session
func (a *arena) clickTile(msg tea.MouseClickMsg) tea.Cmd {
	if r, ok := a.approvalRect(); ok && r.contains(msg.X, msg.Y) {
		if ap := a.approval(); ap != nil {
			_, cmd := ap.Update(tea.MouseClickMsg{X: msg.X - r.x - 2, Y: msg.Y - r.y - 1, Button: msg.Button, Mod: msg.Mod})
			return cmd
		}
		return nil
	}
	tl, ok := a.tileAt(msg.X, msg.Y)
	if !ok {
		return a.setTyping(false)
	}
	var cmds []tea.Cmd
	if tl.name != a.sel {
		cmds = append(cmds, a.selectMember(tl.name))
	}
	if msg.Button != tea.MouseLeft {
		return tea.Batch(cmds...)
	}
	if msg.Y == tl.r.y {
		return tea.Batch(append(cmds, a.setTyping(!a.typing || tl.name != a.sel))...)
	}
	s := a.sessionOf(tl.name)
	if s == nil {
		return tea.Batch(cmds...)
	}
	cmds = append(cmds, a.setTyping(true))
	_, cmd := s.Update(tea.MouseClickMsg{X: msg.X - tl.r.x, Y: msg.Y - tl.r.y - 1, Button: msg.Button, Mod: msg.Mod})
	return tea.Batch(append(cmds, cmd)...)
}

// Hands a pointer message to the session of the window under it, in the coordinates of the window
func (a *arena) forwardTile(x, y int, shift func(dx, dy int) tea.Msg) tea.Cmd {
	tl, ok := a.tileAt(x, y)
	if !ok || y == tl.r.y {
		return nil
	}
	s := a.sessionOf(tl.name)
	if s == nil {
		return nil
	}
	_, cmd := s.Update(shift(tl.r.x, tl.r.y+1))
	return cmd
}

// Column the content of the inspector starts at, past its handle
func (a *arena) inspX() int {
	if a.lay.full {
		return a.lay.insp.x
	}
	return a.lay.insp.x + 1
}

// Carries out a click on a painted thing by its id
func (a *arena) press(id string) tea.Cmd {
	kind, rest, _ := strings.Cut(id, ":")
	switch kind {
	case "sec":
		for _, s := range sections {
			if s.String() == rest {
				return a.toggleSection(s)
			}
		}
	case "sprint":
		a.sprintFull = !a.sprintFull
		a.secs.Top(secSprint.String())
		return nil
	case "member", "log":
		if rest == "" {
			return nil
		}
		if _, ok := a.snap.Member(rest); !ok {
			return nil
		}
		if rest == a.sel && kind == "member" {
			return a.toggleInspect()
		}
		return a.selectMember(rest)
	case "ticket":
		if a.selTicket == rest {
			a.selTicket = ""
			a.markFloor()
			return nil
		}
		a.selTicket = rest
		a.markFloor()
		if tk, ok := a.snap.Ticket(rest); ok && tk.Builder != "" {
			if _, ok := a.snap.Member(tk.Builder); ok {
				return a.selectMember(tk.Builder)
			}
		}
		return nil
	case "inspector":
		return a.toggleInspect()
	}
	return a.act(id)
}

// Ends a drag, a press on the floor that never moved picking the actor or the node under it
func (a *arena) release(msg tea.MouseReleaseMsg) tea.Cmd {
	d := a.drag
	a.drag = nil
	if d != nil && d.kind != dragCamera {
		return nil
	}
	if a.off() && a.lay.world.contains(msg.X, msg.Y) {
		if r, ok := a.approvalRect(); ok && r.contains(msg.X, msg.Y) {
			if ap := a.approval(); ap != nil {
				_, cmd := ap.Update(tea.MouseReleaseMsg{X: msg.X - r.x - 2, Y: msg.Y - r.y - 1, Button: msg.Button, Mod: msg.Mod})
				return cmd
			}
			return nil
		}
		return a.forwardTile(msg.X, msg.Y, func(dx, dy int) tea.Msg {
			return tea.MouseReleaseMsg{X: msg.X - dx, Y: msg.Y - dy, Button: msg.Button, Mod: msg.Mod}
		})
	}
	if a.lay.insp.contains(msg.X, msg.Y) {
		if s := a.inspected(); s != nil && msg.Y >= a.lay.insp.y+a.inspHead {
			_, cmd := s.Update(tea.MouseReleaseMsg{X: msg.X - a.inspX(), Y: msg.Y - a.lay.insp.y - a.inspHead, Button: msg.Button, Mod: msg.Mod})
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
	if d == nil || d.moved {
		return nil
	}
	if name, ok := a.actorAt(msg.X, msg.Y); ok {
		if name == a.sel {
			return a.toggleInspect()
		}
		return a.selectMember(name)
	}
	if id, ok := a.nodeAt(msg.X, msg.Y); ok {
		return a.press("ticket:" + id)
	}
	return nil
}

// Turns the camera or resizes a panel while a drag goes, else tracks what the pointer is over
func (a *arena) motion(msg tea.MouseMotionMsg) tea.Cmd {
	if d := a.drag; d != nil && msg.Button == tea.MouseLeft {
		dx, dy := msg.X-d.x, msg.Y-d.y
		switch d.kind {
		case dragCamera:
			if dx != 0 || dy != 0 {
				d.moved = true
				yaw, pitch := -float64(dx)*dragYaw, float64(dy)*dragPitch
				a.stage.post(func(s *stage) { s.orbit(yaw, pitch) })
				d.x, d.y = msg.X, msg.Y
			}
		case dragDock:
			a.dockWidth = min(max(int(d.base)+dx, minDockWidth), min(maxDockWidth, a.w/2))
		case dragInspector:
			a.inspWidth = min(max(int(d.base)-dx, minInspWidth), min(maxInspWidth, a.w-30))
		case dragSplit:
			tickets, _ := a.secs.Rect(secTickets.String())
			log, _ := a.secs.Rect(secLog.String())
			if rows := float64(tickets.H - 1 + log.H - 1); tickets.H > 1 && rows > 0 {
				a.secs.Share = 1 - min(max(d.base-float64(dy)/rows, 0.2), 0.8)
			}
		}
		return nil
	}
	wasHover, wasTicket := a.hover, a.hoverTicket
	a.hover, a.hoverTicket, a.hoverHit = "", "", ""
	defer func() {
		if a.hover != wasHover || a.hoverTicket != wasTicket {
			a.markFloor()
		}
	}()
	x, y := msg.X, msg.Y
	switch {
	case a.lay.insp.w > 0 && !a.lay.full && x == a.lay.insp.x && y >= a.lay.insp.y:
		a.hoverHit = "edge:inspector"
		return nil
	case a.onDock(x, y) && x == a.lay.dock.x+a.lay.dock.w-1:
		a.hoverHit = "edge:dock"
		return nil
	case a.onDock(x, y) && y == a.secs.Split():
		a.hoverHit = "edge:split"
		return nil
	}
	if id, ok := a.hitAt(x, y); ok {
		a.hoverHit = id
		kind, rest, _ := strings.Cut(id, ":")
		switch kind {
		case "member", "log", "tile":
			if _, ok := a.snap.Member(rest); ok {
				a.hover = rest
			}
		case "ticket":
			a.hoverTicket = rest
		}
		if !a.lay.insp.contains(x, y) {
			return nil
		}
	}
	if a.off() && a.lay.world.contains(x, y) {
		if r, ok := a.approvalRect(); ok && r.contains(x, y) {
			if ap := a.approval(); ap != nil {
				_, cmd := ap.Update(tea.MouseMotionMsg{X: x - r.x - 2, Y: y - r.y - 1, Button: msg.Button, Mod: msg.Mod})
				return cmd
			}
			return nil
		}
		if tl, ok := a.tileAt(x, y); ok {
			a.hover = tl.name
		}
		return a.forwardTile(x, y, func(dx, dy int) tea.Msg {
			return tea.MouseMotionMsg{X: x - dx, Y: y - dy, Button: msg.Button, Mod: msg.Mod}
		})
	}
	if a.lay.insp.contains(x, y) {
		if s := a.inspected(); s != nil && y >= a.lay.insp.y+a.inspHead {
			_, cmd := s.Update(tea.MouseMotionMsg{X: x - a.inspX(), Y: y - a.lay.insp.y - a.inspHead, Button: msg.Button, Mod: msg.Mod})
			return cmd
		}
		return nil
	}
	if r, ok := a.approvalRect(); ok && r.contains(x, y) {
		if ap := a.approval(); ap != nil {
			_, cmd := ap.Update(tea.MouseMotionMsg{X: x - r.x - 2, Y: y - r.y - 1, Button: msg.Button, Mod: msg.Mod})
			return cmd
		}
	}
	if a.lay.world.contains(x, y) {
		a.hover, _ = a.actorAt(x, y)
		if a.hover == "" {
			a.hoverTicket, _ = a.nodeAt(x, y)
		}
	}
	return nil
}

// Zooms the camera over the floor, scrolls the section of the dock, the inspector or the approval dialog under the pointer
func (a *arena) wheel(msg tea.MouseWheelMsg) tea.Cmd {
	if a.lay.insp.contains(msg.X, msg.Y) {
		if s := a.inspected(); s != nil {
			_, cmd := s.Update(tea.MouseWheelMsg{X: msg.X - a.inspX(), Y: msg.Y - a.lay.insp.y - a.inspHead, Button: msg.Button, Mod: msg.Mod})
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
	if a.onDock(msg.X, msg.Y) {
		step := 3
		if msg.Button == tea.MouseWheelUp {
			step = -3
		}
		if id, ok := a.secs.At(msg.X, msg.Y); ok {
			a.scrollSection(id, step)
		}
		return nil
	}
	if !a.lay.world.contains(msg.X, msg.Y) {
		return nil
	}
	if a.off() {
		return a.forwardTile(msg.X, msg.Y, func(dx, dy int) tea.Msg {
			return tea.MouseWheelMsg{X: msg.X - dx, Y: msg.Y - dy, Button: msg.Button, Mod: msg.Mod}
		})
	}
	switch msg.Button {
	case tea.MouseWheelUp:
		a.stage.post(func(s *stage) { s.zoom(0.9) })
	case tea.MouseWheelDown:
		a.stage.post(func(s *stage) { s.zoom(1.11) })
	case tea.MouseWheelLeft:
		a.stage.post(func(s *stage) { s.orbit(-0.1, 0) })
	case tea.MouseWheelRight:
		a.stage.post(func(s *stage) { s.orbit(0.1, 0) })
	}
	return nil
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

// Ticket whose core label sits under a cell of the floor on the last paint
func (a *arena) nodeAt(x, y int) (string, bool) {
	if !a.lay.world.contains(x, y) {
		return "", false
	}
	for _, h := range a.hits {
		if kind, id, ok := strings.Cut(h.id, ":"); ok && kind == "ticket" && h.r.contains(x, y) && a.lay.world.contains(h.r.x, h.r.y) {
			return id, true
		}
	}
	return "", false
}

var _ wm.Card = (*arena)(nil)
