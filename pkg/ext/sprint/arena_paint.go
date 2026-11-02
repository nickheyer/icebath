package sprint

import (
	"cmp"
	"fmt"
	"image/color"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/wm"
	"github.com/nickheyer/icebath/pkg/vxl"
)

// Rows the inspector spends above the transcript of a member
const inspectorHead = 3

// Rows the strip at the top of the floor takes
const stripRows = 3

// Chip of a member on the strip with where it was painted
type chipRect struct {
	name string
	r    rect
}

// Draws the floor through the camera, then the strip, the panels, the inspector and the approval over it
func (a *arena) Paint(buf *uv.Buffer) {
	a.painted = time.Now()
	a.w, a.h = buf.Width(), buf.Height()
	a.lay = a.arrange(a.w, a.h)
	p := newPainter(buf, a.env.Theme)
	p.fill(0, 0, a.w, a.h, a.env.Theme.Surface)
	a.spots = map[string]spot{}
	a.chips = a.chips[:0]
	a.apRect, a.panelBox = rect{}, rect{}
	if a.lay.world.w > 0 && a.lay.world.h > 0 {
		a.paintWorld(p)
		a.paintStrip(p)
		a.paintPanels(p)
		a.paintApproval(p)
		if a.showHelp {
			a.paintHelp(p)
		}
	}
	if a.lay.insp.w > 0 {
		a.paintInspector(p)
	}
}

// View is never read, the arena paints its cells itself
func (a *arena) View(w, h int) string { return "" }

// Boxes of the floor and the inspector, the inspector taking the right side or the whole card when narrow
func (a *arena) arrange(w, h int) layout {
	l := layout{world: rect{0, 0, w, h}}
	if !a.inspect {
		return l
	}
	if _, ok := a.selected(); !ok {
		return l
	}
	if w < 100 {
		l.world, l.insp, l.full = rect{}, rect{0, 0, w, h}, true
		return l
	}
	iw := min(max(int(float64(w)*0.42), 44), 100)
	l.world, l.insp = rect{0, 0, w - iw, h}, rect{w - iw, 0, iw, h}
	return l
}

// Renders the platform into pixels, the static layer kept between frames, encodes the cells of the world box and places the labels
func (a *arena) paintWorld(p *painter) {
	t := a.env.Theme
	wb := a.lay.world
	fr := &a.fr
	mode := vxl.CellModeOf(a.cfg.Arena.Pixels)
	px, py := mode.Pixels()
	fr.PixelAspect = mode.PixelAspect()
	fr.Resize(wb.w*px, wb.h*py)
	sc := a.sc
	r := &a.r
	r.FogColor, r.FogNear, r.FogFar = sc.horizon, a.cam.Dist*0.95, a.cam.Dist*3
	if !a.staticOK || a.staticCam != a.cam || a.static.W != fr.W || a.static.H != fr.H || sc.dirty {
		st := &a.static
		st.PixelAspect = fr.PixelAspect
		st.Resize(fr.W, fr.H)
		st.Clear(func(x, y int) vxl.RGB {
			share := float64(y) / float64(max(st.H, 1))
			c := sc.sky(share)
			if share < 0.62 && vxl.Hash2(x, y*7+1) > 0.9975 {
				c = c.Mix(vxl.Hex(0xbfe0f8), (0.2+0.3*vxl.Hash2(y, x))*(0.62-share)/0.62)
			}
			return c
		})
		r.Begin(st, a.cam)
		sc.drawStatic(r)
		r.Flush()
		a.staticCam, a.staticOK = a.cam, true
	}
	fr.CopyFrom(&a.static)
	r.Begin(fr, a.cam)
	sc.drawDynamic(r)
	if act := a.selectedActor(); act != nil {
		sc.ring(r, act, vxl.FromColor(t.Ice), 0.85)
	}
	if a.hover != "" && a.hover != a.sel {
		sc.ring(r, sc.byName[a.hover], vxl.FromColor(t.TextDim), 0.5)
	}
	r.Flush()
	if !t.Plain {
		fr.Bloom(&a.post, 0.62, 0.8)
		fr.Vignette(0.3)
	}
	var quantize func(vxl.RGB) color.Color
	if t.Plain {
		quantize = vxl.ANSIQuantizer(t.Terminal)
	}
	fr.Encode(p.buf.Buffer, wb.x, wb.y, wb.w, wb.h, mode, quantize)
	cell := func(sx, sy float64) (int, int) {
		return wb.x + int(sx)/px, wb.y + int(sy)/py
	}
	for _, act := range sc.actors {
		if act.gone {
			continue
		}
		fx, fy, depth, ok := r.Project(act.stand())
		hx, hy, _, ok2 := r.Project(act.headPos().Add(vxl.V(0, 0.5, 0)))
		if !ok || !ok2 {
			continue
		}
		cx, bottom := cell(fx, fy)
		_, top := cell(hx, hy)
		a.spots[act.name] = spot{x: cx, top: top, bottom: bottom, depth: depth}
	}
	var space labelSpace
	for _, act := range a.labelOrder() {
		sp, ok := a.spots[act.name]
		if !ok {
			continue
		}
		if act.emote != "" && sp.top-2 >= wb.y+stripRows && space.claim(sp.x, sp.top-2, 1) {
			p.text(sp.x, sp.top-2, act.emote, emoteColor(act.emote, t), true)
		}
		if !(a.labels || act.name == a.sel || act.name == a.hover) || sp.top-1 < wb.y+stripRows {
			continue
		}
		fg := t.Text
		if act.name == a.sel {
			fg = t.Ice
		}
		lw := ansi.StringWidth(act.name)
		if x := sp.x - lw/2; space.claim(x, sp.top-1, lw) {
			p.label(x, sp.top-1, act.name, fg, theme.Mix(t.Bg, t.Surface, 0.5), act.name == a.sel)
		}
	}
	if a.labels {
		for _, id := range sc.order {
			n := sc.nodes[id]
			if n.rise < 1 {
				continue
			}
			sx, sy, _, ok := r.Project(n.core(sc.t).Add(vxl.V(0, 1.6, 0)))
			if !ok {
				continue
			}
			cx, cy := cell(sx, sy)
			fg := theme.Mix(t.TextDim, t.Text, 0.4)
			if n.status == TicketReview || n.glow > 0.3 {
				fg = t.Text
			}
			if x := cx - len(id)/2; cy >= wb.y+stripRows && space.claim(x, cy, len(id)) {
				p.text(x, cy, id, fg, n.status == TicketReview)
			}
		}
	}
	if banner := stageBanner(sc, t); banner != "" && wb.h > stripRows+4 {
		p.label(wb.x+(wb.w-ansi.StringWidth(banner))/2, wb.y+stripRows+1, " "+banner+" ", t.Ice, theme.Mix(t.Bg, t.Surface, 0.5), true)
	}
	if a.snap != nil {
		name := a.hover
		if name == "" {
			name = a.sel
		}
		if act := sc.byName[name]; act != nil && !act.gone && !a.showBoard && !a.showLog && !a.showHelp {
			if sp, ok := a.spots[name]; ok {
				a.paintTelemetry(p, act, sp, a.liveStates()[name])
			}
		}
	}
	if a.snap == nil {
		msg := "no dive yet, run /" + Name + " <request> to send a crew beyond the blackwall"
		p.label(wb.x+(wb.w-ansi.StringWidth(msg))/2, wb.y+wb.h-2, msg, t.TextDim, t.Surface, false)
	}
}

// Cells the floor labels of one paint have taken, so none is written over another
type labelSpace struct{ taken []rect }

// Claims a run of cells on a row for a label, false when it would touch one already placed
func (l *labelSpace) claim(x, y, w int) bool {
	for _, r := range l.taken {
		if y == r.y && x <= r.x+r.w && x+w >= r.x {
			return false
		}
	}
	l.taken = append(l.taken, rect{x, y, w, 1})
	return true
}

// Actors in the order their labels claim cells, the selected first, then the hovered, then the nearest
func (a *arena) labelOrder() []*actor {
	acts := slices.Clone(a.sc.actors)
	rank := func(act *actor) float64 {
		switch act.name {
		case a.sel:
			return -2
		case a.hover:
			return -1
		}
		return a.spots[act.name].depth
	}
	slices.SortStableFunc(acts, func(p, q *actor) int { return cmp.Compare(rank(p), rank(q)) })
	return acts
}

// Color an emote glyph paints in
func emoteColor(e string, t theme.Theme) color.Color {
	switch e {
	case "?":
		return t.Ice
	case "!":
		return t.Accent
	case "✓":
		return t.Alive
	}
	return t.Alarm
}

// Strip over the floor with the sprint, its stages, what it waits on and the roster of chips
func (a *arena) paintStrip(p *painter) {
	t := a.env.Theme
	wb := a.lay.world
	p.tint(wb.x, wb.y, wb.w, stripRows, t.Bg, 0.6)
	s := a.snap
	x, right := wb.x+2, wb.x+wb.w-2
	if s == nil {
		p.text(x, wb.y, t.Glyphs.Mission+" arena", t.TextDim, true)
		p.text(x, wb.y+1, "the platform is empty, nobody has jacked in", t.TextDim, false)
		if a.cfgErr != nil {
			p.text(x, wb.y+2, ansi.Truncate("settings: "+a.cfgErr.Error(), right-x, t.Glyphs.Ellipsis), t.Alarm, false)
		}
		return
	}
	x += p.text(x, wb.y, s.ID, t.Ice, true) + 3
	for i, st := range Stages {
		fg := t.TextDim
		switch {
		case s.Stage == st && s.Running():
			fg = t.Accent
		case s.Stage == st:
			fg = t.Text
		case stageIndex(s.Last) > i || s.Stage == StageDone || stageIndex(s.Stage) > i:
			fg = t.Alive
		}
		if i > 0 {
			x += p.text(x, wb.y, " "+t.Glyphs.Bullet+" ", t.Steel, false)
		}
		x += p.text(x, wb.y, strings.ToUpper(string(st)), fg, s.Stage == st)
	}
	switch s.Stage {
	case StageDone:
		x += p.text(x+2, wb.y, t.Glyphs.Success+" done", t.Alive, true) + 2
	case StageFailed:
		x += p.text(x+2, wb.y, t.Glyphs.Fail+" failed", t.Alarm, true) + 2
	}
	if done, total := s.Progress(); total > 0 {
		x += 3
		width := min(12, max(right-x-24, 4))
		p.styled(x, wb.y, width, 1, bar(float64(done)/float64(total), width, t))
		x += width + 1
		x += p.text(x, wb.y, fmt.Sprintf("%d/%d", done, total), t.Text, false)
	}
	meta := []string{render.Elapsed(s.Elapsed()) + " of " + durationText(a.cfg.Limits.Total)}
	spent := 0
	for _, n := range s.Spent {
		spent += n
	}
	if spent > 0 {
		meta = append(meta, render.Tokens(spent)+" tok")
	}
	if cost := a.cost(s); cost > 0 {
		meta = append(meta, render.Money(cost))
	}
	for len(meta) > 0 {
		line := strings.Join(meta, "   ")
		if mx := right - ansi.StringWidth(line); mx >= x+3 {
			p.text(mx, wb.y, line, t.TextDim, false)
			break
		}
		meta = meta[:len(meta)-1]
	}
	live := a.liveStates()
	status, fg := s.Status, t.TextDim
	switch {
	case s.Running() && s.Paused != "":
		status, fg = t.Glyphs.Warn+" "+s.Paused, t.Accent
	case s.Running() && s.Waiting != "" && live[s.Waiting].asking:
		status, fg = t.Glyphs.Ask+" "+s.Waiting+" asks you", t.Ice
	case s.Running() && s.Waiting != "":
		status, fg = t.Glyphs.Flag+" "+s.Waiting+" waits for you", t.Accent
	case !s.Running() && s.Err != nil && !s.Stopped():
		status, fg = t.Glyphs.Fail+" "+s.Err.Error(), t.Alarm
	case s.Stopped():
		status, fg = "stopped, "+a.env.Keys.Display(ActRestart)+" restarts at the "+string(restartStage(*s))+" stage", t.Accent
	}
	if a.cfgErr != nil {
		status, fg = "settings: "+a.cfgErr.Error(), t.Alarm
	}
	p.text(wb.x+2, wb.y+1, ansi.Truncate(status, right-wb.x-2, t.Glyphs.Ellipsis), fg, false)
	a.paintChips(p, wb.x+2, wb.y+2, right, live)
}

// Place of a stage in the order, past the last for done and failed
func stageIndex(st Stage) int {
	for i, s := range Stages {
		if s == st {
			return i
		}
	}
	return len(Stages)
}

// Duration in whole hours and minutes
func durationText(d time.Duration) string {
	d = d.Round(time.Minute)
	h, m := int(d.Hours()), int(d.Minutes())%60
	switch {
	case h == 0:
		return fmt.Sprintf("%dm", m)
	case m == 0:
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh%02dm", h, m)
}

// Dollars the members of the sprint spent, from the meter by run
func (a *arena) cost(s *Snapshot) float64 {
	if a.env.Meter == nil || a.env.Control == nil {
		return 0
	}
	snap := a.env.Meter.Snapshot()
	total := 0.0
	for _, r := range a.env.Control.Runs() {
		if r.Parent != "" {
			continue
		}
		for _, m := range s.Members {
			if m.Live != nil && r.Session == m.Live.Session {
				total += snap.Costs[r.ID]
			}
		}
	}
	return total
}

// Chips of every member in a row, each with its state mark, the selected one lit
func (a *arena) paintChips(p *painter, x, y, right int, live map[string]liveState) {
	t := a.env.Theme
	tick := int(a.sc.t * 8)
	for _, m := range a.roster() {
		st := live[m.Name]
		glyph, fg := t.Glyphs.Hollow, t.TextDim
		switch {
		case st.approval:
			glyph, fg = t.Glyphs.Flag, t.Accent
		case st.asking:
			glyph, fg = t.Glyphs.Ask, t.Ice
		case st.running:
			glyph, fg = t.Glyphs.Spinner[tick%len(t.Glyphs.Spinner)], t.Alive
		case st.failed:
			glyph, fg = t.Glyphs.Fail, t.Alarm
		}
		label := " " + glyph + " " + m.Name + " "
		if m.Ticket != "" {
			label += t.Glyphs.Sep + " " + m.Ticket + " "
		}
		lw := ansi.StringWidth(label)
		if x+lw > right {
			break
		}
		bg, tfg := theme.Mix(t.Bg, t.Surface, 0.4), t.Text
		if m.Name == a.sel {
			bg, tfg, fg = t.Ice, t.Bg, t.Bg
		} else if m.Name == a.hover {
			bg = t.Steel
		}
		p.label(x, y, label, tfg, bg, m.Name == a.sel)
		p.label(x+1, y, glyph, fg, bg, true)
		a.chips = append(a.chips, chipRect{name: m.Name, r: rect{x, y, lw, 1}})
		x += lw + 2
	}
}

// Member whose chip sits under a cell
func (a *arena) chipAt(x, y int) (string, bool) {
	for _, c := range a.chips {
		if c.r.contains(x, y) {
			return c.name, true
		}
	}
	return "", false
}

// Ticket board or exchange log laid over the left of the floor
func (a *arena) paintPanels(p *painter) {
	switch {
	case a.showBoard:
		a.paintBoard(p)
	case a.showLog:
		a.paintLog(p)
	}
}

// Box the open board or log takes, false while neither is open
func (a *arena) panelRect() (rect, bool) {
	if a.panelBox.w == 0 {
		return rect{}, false
	}
	return a.panelBox, true
}

// Table of every ticket with where it stands
func (a *arena) paintBoard(p *painter) {
	t := a.env.Theme
	wb := a.lay.world
	var lines []string
	if a.snap == nil || len(a.snap.Tickets) == 0 {
		lines = []string{render.Dim("no tickets yet", t)}
	}
	if a.snap != nil {
		for _, tk := range a.snap.Tickets {
			state := statusText(tk.Status, t)
			line := lipgloss.NewStyle().Foreground(t.Ice).Render(tk.ID) + " " + state + " " + tk.Title
			var tail []string
			if tk.Builder != "" {
				tail = append(tail, tk.Builder)
			}
			if tk.Round > 0 {
				tail = append(tail, fmt.Sprintf("sent back %d", tk.Round))
			}
			if len(tk.DependsOn) > 0 {
				tail = append(tail, "after "+strings.Join(tk.DependsOn, " "))
			}
			if len(tail) > 0 {
				line += " " + render.Dim(strings.Join(tail, " "+t.Glyphs.Sep+" "), t)
			}
			lines = append(lines, line)
			if tk.Err != "" {
				lines = append(lines, "   "+render.Bad(render.Truncate(tk.Err, 70, t), t))
			}
		}
	}
	a.panelBox = a.overlayBox(wb, 60, len(lines))
	a.boardTop = min(a.boardTop, max(len(lines)-(a.panelBox.h-2), 0))
	title := "TICKETS"
	if a.snap != nil && a.snap.Plan.Title != "" {
		title += " " + t.Glyphs.Sep + " " + a.snap.Plan.Title
	}
	p.panel(a.panelBox.x, a.panelBox.y, a.panelBox.w, a.panelBox.h, title, 0.88)
	p.lines(a.panelBox.x+2, a.panelBox.y+1, a.panelBox.w-4, a.panelBox.h-2, lines[a.boardTop:])
}

// Box a panel of that many lines takes at the left of the floor under the strip
func (a *arena) overlayBox(wb rect, width, lines int) rect {
	w := min(width, wb.w-2)
	h := min(lines+2, wb.h-stripRows-1)
	return rect{wb.x + 1, wb.y + stripRows, max(w, 10), max(h, 3)}
}

// Status of a ticket in its color
func statusText(s TicketStatus, t theme.Theme) string {
	text := fmt.Sprintf("%-7s", string(s))
	switch s {
	case TicketActive:
		return render.Warm(text, t)
	case TicketReview:
		return render.Cool(text, t)
	case TicketDone:
		return render.Good(text, t)
	case TicketFailed, TicketBlocked:
		return render.Bad(text, t)
	}
	return render.Dim(text, t)
}

// Exchanges between the team and the person, newest last
func (a *arena) paintLog(p *painter) {
	t := a.env.Theme
	wb := a.lay.world
	var lines []string
	if a.snap != nil {
		for _, m := range a.snap.Messages {
			who := m.From
			if m.To != "" {
				who += " " + t.Glyphs.Steer + " " + m.To
			}
			if who == "" {
				who = string(m.Kind)
			}
			line := render.Dim(m.At.Local().Format("15:04:05"), t) + " " + lipgloss.NewStyle().Foreground(kindColor(m.Kind, t)).Render(who)
			if m.Ticket != "" {
				line += " " + lipgloss.NewStyle().Foreground(t.Ice).Render(m.Ticket)
			}
			lines = append(lines, line+" "+firstLine(m.Text))
		}
	}
	if len(lines) == 0 {
		lines = []string{render.Dim("nothing has happened yet", t)}
	}
	a.panelBox = a.overlayBox(wb, 90, len(lines))
	rows := a.panelBox.h - 2
	a.logTop = min(a.logTop, max(len(lines)-rows, 0))
	end := len(lines) - a.logTop
	start := max(end-rows, 0)
	p.panel(a.panelBox.x, a.panelBox.y, a.panelBox.w, a.panelBox.h, "LOG", 0.88)
	p.lines(a.panelBox.x+2, a.panelBox.y+1, a.panelBox.w-4, rows, lines[start:end])
}

// Color an exchange kind paints its names in
func kindColor(k MessageKind, t theme.Theme) color.Color {
	switch k {
	case MsgVerdict:
		return t.Alive
	case MsgHandoff:
		return t.Ice
	case MsgAssign:
		return t.Accent
	case MsgQuestion, MsgAnswer:
		return t.Pulse
	case MsgFailure:
		return t.Alarm
	}
	return t.TextDim
}

// Legend of every arena key laid over the middle of the floor
func (a *arena) paintHelp(p *painter) {
	t := a.env.Theme
	wb := a.lay.world
	ids := []string{ActNext, ActPrev, ActInspect, ActSteer, ActLeave, ActOpen, ActKill, ActFollow, ActCamera, ActReset, ActOrbitLeft, ActOrbitRight, ActOrbitUp, ActOrbitDown, ActZoomIn, ActZoomOut, ActBoard, ActLog, ActLabels, ActCustomize, ActPause, ActStop, ActRestart, ActSprintNext, ActSprintPrev, ActSettings, ActHelp}
	var lines []string
	for _, id := range ids {
		act, ok := a.env.Keys.Action(id)
		if !ok {
			continue
		}
		key := a.env.Keys.DisplayAll(id)
		if key == "" {
			key = "unbound"
		}
		lines = append(lines, lipgloss.NewStyle().Foreground(t.Ice).Render(fmt.Sprintf("%-14s", key))+" "+act.Doc)
	}
	lines = append(lines, "", render.Dim("drag the platform to turn the camera, wheel to zoom, click a member to select", t))
	w := min(84, wb.w-4)
	h := min(len(lines)+2, wb.h-2)
	x, y := wb.x+(wb.w-w)/2, wb.y+(wb.h-h)/2
	p.panel(x, y, w, h, "ARENA KEYS", 0.92)
	p.lines(x+2, y+1, w-4, h-2, lines)
}

// Approval of a member laid over the bottom of the floor, answered with the approval keys
func (a *arena) paintApproval(p *painter) {
	ap := a.approval()
	if ap == nil {
		return
	}
	t := a.env.Theme
	wb := a.lay.world
	w := min(78, wb.w-4)
	h := min(16, wb.h-stripRows-2)
	if w < 20 || h < 6 {
		return
	}
	x, y := wb.x+(wb.w-w)/2, wb.y+wb.h-h-1
	a.apRect = rect{x, y, w, h}
	name := ""
	if a.snap != nil {
		for _, m := range a.snap.Members {
			if ap.Key() == "approval:"+m.SessionID() {
				name = m.Name
			}
		}
	}
	p.fill(x, y, w, h, t.Surface)
	p.panel(x, y, w, h, t.Glyphs.Approval+" APPROVAL "+t.Glyphs.Sep+" "+name, 0)
	p.styled(x+2, y+1, w-4, h-2, ap.View(w-4, h-2))
}

// Box of the approval dialog, false while none shows
func (a *arena) approvalRect() (rect, bool) {
	if a.apRect.w == 0 {
		return rect{}, false
	}
	return a.apRect, true
}

// Transcript and prompt of the selected member under a header naming it and what it is doing
func (a *arena) paintInspector(p *painter) {
	t := a.env.Theme
	ib := a.lay.insp
	m, ok := a.selected()
	s := a.inspected()
	if !ok || s == nil {
		return
	}
	p.fill(ib.x, ib.y, ib.w, ib.h, t.Surface)
	if !a.lay.full {
		for y := ib.y; y < ib.y+ib.h; y++ {
			p.label(ib.x, y, t.Glyphs.Sep, t.Steel, t.Surface, false)
		}
	}
	x0, right := ib.x+2, ib.x+ib.w-2
	bg := theme.Mix(t.Surface, t.Ice, 0.08)
	if a.typing {
		bg = theme.Mix(t.Surface, t.Ice, 0.2)
	}
	p.fill(ib.x+1, ib.y, ib.w-1, 1, bg)
	x := x0 + p.label(x0, ib.y, t.Glyphs.Notch+" "+m.Name, t.Text, bg, true)
	if role := string(m.Role); !strings.EqualFold(role, m.Name) {
		x += p.label(x, ib.y, "  "+role, t.TextDim, bg, false)
	}
	if m.Live != nil && m.Live.Endpoint.Model != "" {
		model := m.Live.Endpoint.Model
		if mx := right - ansi.StringWidth(model); mx >= x+3 {
			p.label(mx, ib.y, model, t.TextDim, bg, false)
		}
	}
	live := a.liveStates()[m.Name]
	var state string
	switch {
	case live.approval:
		state = render.Warm(t.Glyphs.Flag+" waiting for your approval", t)
	case live.asking:
		state = render.Cool(t.Glyphs.Ask+" asking you", t)
	case live.running && live.tool != "":
		state = render.Good(t.Glyphs.Live+" running", t) + "  " + render.ToolGlyph(live.tool, t) + " " + live.tool
	case live.running:
		state = render.Good(t.Glyphs.Live+" running", t)
	case live.failed:
		state = render.Bad(t.Glyphs.Fail+" last run failed", t)
	default:
		state = render.Dim(t.Glyphs.Hollow+" idle", t)
	}
	if m.Task != "" {
		state += "  " + lipgloss.NewStyle().Foreground(t.Text).Render(m.Task)
	}
	room := right - x0
	if a.snap != nil {
		if n := a.snap.Spent[m.Name]; n > 0 {
			tok := render.Tokens(n) + " tok"
			if tx := right - ansi.StringWidth(tok); tx >= x0+20 {
				p.text(tx, ib.y+1, tok, t.TextDim, false)
				room = tx - x0 - 2
			}
		}
	}
	p.styled(x0, ib.y+1, room, 1, ansi.Truncate(state, room, t.Glyphs.Ellipsis))
	p.label(ib.x+1, ib.y+2, strings.Repeat(t.Glyphs.Rule, ib.w-1), t.Steel, t.Surface, false)
	bw, bh := ib.w-1, ib.h-inspectorHead
	if bw < 10 || bh < 3 {
		return
	}
	p.styled(ib.x+1, ib.y+inspectorHead, bw, bh, s.View(bw, bh))
}

// Cursor of the prompt typed into, moved into the inspector box
func (a *arena) Cursor() *tea.Cursor {
	if !a.typing {
		return nil
	}
	s := a.inspected()
	if s == nil {
		return nil
	}
	c := s.Cursor()
	if c == nil {
		return nil
	}
	out := *c
	out.X += a.lay.insp.x + 1
	out.Y += a.lay.insp.y + inspectorHead
	return &out
}

var _ wm.Painter = (*arena)(nil)
