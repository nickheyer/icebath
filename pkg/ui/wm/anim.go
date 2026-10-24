package wm

// Advances every animation by dt seconds and reports whether anything is still moving
func (m *Model) Advance(dt float64) bool {
	moving := false
	for i, ws := range m.spaces {
		shown := i == m.active
		if shown || ws == m.leaving {
			if m.stepWorkspace(ws, dt, shown) {
				moving = true
			} else if ws == m.leaving {
				m.leaving = nil
			}
			continue
		}
		moving = m.animateCards(ws, dt) || moving
	}
	if m.overlay != nil {
		moving = m.step(m.overlay, dt, true, false) || moving
	}
	if m.parting != nil {
		moving = m.step(m.parting, dt, false, false) || moving
		if m.motion == 0 || (m.parting.open.Settled() && m.parting.open.Pos < 0.01) {
			m.parting = nil
		}
	}
	m.veil.Set(flag(m.overlay != nil))
	if m.motion == 0 {
		m.veil.Snap()
	} else {
		moving = m.veil.Step(dt/m.motion) || moving
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

// Keeps the cards of a workspace off the stage in step so they are at rest when shown again
func (m *Model) animateCards(ws *Workspace, dt float64) bool {
	moving := false
	for _, e := range ws.entries() {
		moving = m.animate(e, dt) || moving
	}
	return moving
}

// Steps the card's own motion when it has any, repainting it when its view changed
func (m *Model) animate(e *entry, dt float64) bool {
	a, ok := e.card.(Animator)
	if !ok {
		return false
	}
	if m.motion == 0 {
		dt = 0
	} else {
		dt /= m.motion
	}
	if !a.Animate(dt) {
		return false
	}
	e.dirty = true
	return dt > 0
}

// Reports whether the entry is a tile covered by another card filling its stage
func hidden(e *entry) bool {
	return e.ws != nil && e.ws.zoom != nil && e.ws.zoom != e && !e.floating
}

func (m *Model) step(e *entry, dt float64, focused, hovered bool) bool {
	if m.motion == 0 {
		if !e.held {
			e.rect.Snap()
		}
		e.open.Snap()
		e.lit.Set(flag(focused))
		e.lit.Snap()
		e.hover.Set(flag(hovered))
		e.hover.Snap()
		e.pulse.Set(flag(e.urgency != Calm))
		e.pulse.Snap()
		e.hide.Set(flag(hidden(e)))
		e.hide.Snap()
		e.shake.Snap()
		e.decode = 1
		m.animate(e, dt)
		return false
	}
	dt /= m.motion
	moving := false
	if !e.held {
		moving = e.rect.Step(dt)
	}
	moving = e.open.Step(dt) || moving
	e.lit.Set(flag(focused))
	moving = e.lit.Step(dt) || moving
	e.hover.Set(flag(hovered))
	moving = e.hover.Step(dt) || moving
	e.pulse.Set(flag(e.urgency != Calm))
	moving = e.pulse.Step(dt) || moving
	e.hide.Set(flag(hidden(e)))
	moving = e.hide.Step(dt) || moving
	moving = e.shake.Step(dt) || moving
	if e.urgency != Calm {
		moving = true
	}
	if e.decode < 1 {
		e.decode = min(1, e.decode+dt/decodeTime)
		moving = true
	}
	if a, ok := e.card.(Animator); ok && a.Animate(dt) {
		e.dirty = true
		moving = true
	}
	return moving
}
