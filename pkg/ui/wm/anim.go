package wm

// Advances every animation by dt seconds and reports whether anything is still moving
func (m *Model) Advance(dt float64) bool {
	moving := false
	if ws := m.Active(); ws != nil {
		moving = m.stepWorkspace(ws, dt, true) || moving
	}
	if m.leaving != nil {
		if m.stepWorkspace(m.leaving, dt, false) {
			moving = true
		} else {
			m.leaving = nil
		}
	}
	if m.overlay != nil {
		moving = m.step(m.overlay, dt, true, false) || moving
	}
	moving = m.stepToasts(dt) || moving
	moving = m.stepChips(dt) || moving
	m.phase += dt * flowSpeed * m.motion
	m.tick++
	return moving
}

// Steps the slide and every card of a workspace, dropping cards whose pop out settled
func (m *Model) stepWorkspace(ws *Workspace, dt float64, shown bool) bool {
	moving := false
	if m.motion == 0 {
		ws.shift.Snap()
	} else {
		moving = ws.shift.Step(dt/m.motion) || moving
	}
	for _, e := range ws.entries() {
		focused := shown && ws.focus == e && m.overlay == nil
		hovered := shown && m.hover == e
		moving = m.step(e, dt, focused, hovered) || moving
		if e.closing && e.open.Settled() && e.open.Pos < 0.01 {
			m.drop(e)
		}
	}
	return moving
}

func (m *Model) step(e *entry, dt float64, focused, hovered bool) bool {
	if m.motion == 0 {
		if !e.held {
			e.rect.Snap()
		}
		e.open.Snap()
		e.lift.Set(flag(e.held))
		e.lift.Snap()
		e.glow.Set(flag(focused))
		e.glow.Snap()
		e.hover.Set(flag(hovered))
		e.hover.Snap()
		e.pulse.Set(flag(e.urgency != Calm))
		e.pulse.Snap()
		e.decode = 1
		return false
	}
	dt /= m.motion
	moving := false
	if !e.held {
		moving = e.rect.Step(dt)
	}
	moving = e.open.Step(dt) || moving
	e.lift.Set(flag(e.held || (!e.closing && e.rect.Distance() > liftDistance)))
	moving = e.lift.Step(dt) || moving
	e.glow.Set(flag(focused))
	moving = e.glow.Step(dt) || moving
	e.hover.Set(flag(hovered))
	moving = e.hover.Step(dt) || moving
	e.pulse.Set(flag(e.urgency != Calm))
	moving = e.pulse.Step(dt) || moving
	if e.urgency != Calm {
		moving = true
	}
	if e.decode < 1 {
		e.decode = min(1, e.decode+dt/decodeTime)
		moving = true
	}
	return moving
}
