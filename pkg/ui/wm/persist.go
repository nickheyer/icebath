package wm

// What the manager keeps between runs
type Layout struct {
	Theme  string   `json:"theme,omitempty"`
	Motion *float64 `json:"motion,omitempty"`
	Ratio  float64  `json:"ratio,omitempty"`
	Mode   string   `json:"mode,omitempty"`
	Main   int      `json:"main,omitempty"`
	// Keys of the tiles open beside the master of the workspace on the stage
	Open []string `json:"open,omitempty"`
}

// Layout as it stands, with the theme name filled in
func (m *Model) Layout() Layout {
	motion := m.motion
	l := Layout{Theme: m.theme.Name, Motion: &motion, Ratio: m.Ratio(), Mode: m.mode.String(), Main: m.main}
	if ws := m.Active(); ws != nil {
		l.Mode = ws.mode.String()
		l.Main = ws.MainCount()
		for i, e := range ws.tiles {
			if i > 0 && !e.closing {
				l.Open = append(l.Open, e.key())
			}
		}
	}
	return l
}

// Applies the ratio, layout mode and motion of a saved layout where options did not fix them
func (m *Model) Restore(l Layout) {
	if l.Ratio > 0 && !m.ratioSet {
		m.ratio = clampRatio(l.Ratio)
		for _, ws := range m.spaces {
			ws.ratio = m.ratio
		}
	}
	if mode, ok := ParseMode(l.Mode); ok && !m.modeSet {
		m.mode = mode
		for _, ws := range m.spaces {
			ws.mode = mode
		}
	}
	if l.Main > 0 {
		m.main = l.Main
		for _, ws := range m.spaces {
			ws.main = l.Main
		}
	}
	for _, ws := range m.spaces {
		if ws.mode == Dwindle && ws.tree == nil {
			ws.rebuildTree(m.stage(), m.gap)
		}
		m.retarget(ws)
	}
	if l.Motion != nil && !m.motionSet {
		m.SetMotion(*l.Motion)
	}
}
