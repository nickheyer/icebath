package sprint

import (
	"cmp"
	"fmt"
	"image/color"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/ui/cards"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/wm"
	"github.com/nickheyer/icebath/pkg/vxl"
)

// Rows the inspector spends above the transcript of a member before its state wraps onto more
const inspectorHead = 3

// Draws the toolbar, the dock, the floor through the camera, the inspector and the approval over the floor
func (a *arena) Paint(buf *uv.Buffer) {
	a.painted = time.Now()
	a.w, a.h = buf.Width(), buf.Height()
	a.lay = a.arrange(a.w, a.h)
	p := cards.NewPainter(buf, a.env.Theme)
	p.Fill(0, 0, a.w, a.h, a.env.Theme.Surface)
	a.spots = map[string]spot{}
	a.hits = a.hits[:0]
	a.apRect = rect{}
	a.paintToolbar(p)
	if a.lay.dock.w > 0 {
		a.paintDock(p)
	}
	if a.lay.world.w > 0 && a.lay.world.h > 0 {
		if a.off() {
			a.paintTiles(p)
		} else {
			a.paintWorld(p)
		}
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

// Boxes of the toolbar, the dock, the floor and the inspector, the dock and the floor giving way as the card narrows
func (a *arena) arrange(w, h int) layout {
	l := layout{bar: rect{0, 0, w, 1}}
	y, bh := 1, max(h-1, 0)
	_, picked := a.selected()
	insp := a.inspect && picked && !a.off()
	if insp && w < 100 {
		l.insp, l.full = rect{0, y, w, bh}, true
		return l
	}
	dw := 0
	if a.dock && w >= 90 {
		dw = min(max(a.dockWidth, minDockWidth), w/2)
	}
	iw := 0
	if insp {
		iw = min(max(a.inspWidth, minInspWidth), w-dw-30)
		if iw < minInspWidth {
			dw, iw = 0, min(max(a.inspWidth, minInspWidth), w-30)
		}
	}
	if dw > 0 {
		l.dock = rect{0, y, dw, bh}
	}
	l.world = rect{dw, y, w - dw - iw, bh}
	if iw > 0 {
		l.insp = rect{w - iw, y, iw, bh}
	}
	return l
}

// Renders the floor at the quality set over the cached static layer, finishes the frame, encodes cells and places labels
func (a *arena) paintWorld(p *cards.Painter) {
	t := a.env.Theme
	wb := a.lay.world
	fr := &a.fr
	mode := vxl.CellModeOf(a.cfg.Arena.Pixels)
	q := qualityOf(a.cfg.Arena.Quality)
	px, py := mode.Pixels()
	px, py = px*q.super, py*q.super
	fr.PixelAspect = mode.PixelAspect()
	imaged := a.imaged()
	if imaged {
		px, py, fr.PixelAspect = a.imagePixels(wb.w, wb.h)
	} else if a.surface != nil {
		a.releaseImage()
		a.surface = nil
	}
	fr.Resize(wb.w*px, wb.h*py)
	sc := a.sc
	r := &a.r
	sc.world.light(r, sc, a.cam.Dist)
	moving := !a.rig.settled()
	q.apply(r)
	sky := func(x, y int) vxl.RGB { return sc.sky(r.Ray(float64(x)+0.5, float64(y)+0.5)) }
	switch {
	case moving:
		r.SetLights(nil)
		r.Begin(fr, a.cam)
		fr.Clear(sky)
		sc.drawStatic(r)
		a.layerOK = false
	default:
		if !a.layerOK || a.layerCam != a.cam || !a.layer.Fits(fr.W, fr.H) || sc.dirty || a.layerWorld != sc.world.name() {
			r.SetLights(nil)
			r.Begin(fr, a.cam)
			fr.Clear(sky)
			sc.drawStatic(r)
			r.FlushInto(&a.layer)
			a.layerCam, a.layerOK, a.layerWorld = a.cam, true, sc.world.name()
		}
		a.layer.Restore(fr)
		r.Begin(fr, a.cam)
	}
	sc.drawDynamic(r)
	if act := a.selectedActor(); act != nil {
		sc.ring(r, act, vxl.FromColor(t.Ice), 0.85)
	}
	if a.hover != "" && a.hover != a.sel {
		sc.ring(r, sc.byName[a.hover], vxl.FromColor(t.TextDim), 0.5)
	}
	if n := sc.nodes[a.selTicket]; n != nil {
		sc.nodeRing(r, n, vxl.FromColor(t.Ice), 0.7)
	}
	if n := sc.nodes[a.hoverTicket]; n != nil && a.hoverTicket != a.selTicket {
		sc.nodeRing(r, n, vxl.FromColor(t.TextDim), 0.4)
	}
	if moving {
		r.Flush()
	} else {
		r.FlushOver(&a.layer)
	}
	fin := sc.world.finish()
	if !imaged {
		fin.grade.Aberration = 0
	}
	fr.Outline(fin.outline)
	if !t.Plain {
		fr.Bloom(&a.post, fin.bloomThreshold, fin.bloomStrength)
	}
	fr.Grade(fin.grade)
	if !t.Plain {
		fr.Vignette(fin.vignette)
	}
	if imaged {
		a.paintImage(p, fr, wb)
	} else {
		var quantize func(vxl.RGB) color.Color
		if t.Plain {
			quantize = vxl.ANSIQuantizer(t.Terminal)
		}
		a.enc.Encode(fr, p.Cells(), wb.x, wb.y, wb.w, wb.h, mode, quantize)
	}
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
	labelBg := theme.Mix(t.Bg, t.Surface, 0.5)
	for _, act := range a.labelOrder() {
		sp, ok := a.spots[act.name]
		if !ok {
			continue
		}
		if act.emote != "" && sp.top-2 >= wb.y && space.claim(sp.x, sp.top-2, 1) {
			p.Text(sp.x, sp.top-2, act.emote, emoteColor(act.emote, t), true)
		}
		if !(a.labels || act.name == a.sel || act.name == a.hover) || sp.top-1 < wb.y {
			continue
		}
		fg, bg := t.Text, labelBg
		if act.name == a.sel {
			fg, bg = t.Bg, t.Ice
		}
		label := " " + act.name + " "
		lw := ansi.StringWidth(label)
		if x := sp.x - lw/2; space.claim(x, sp.top-1, lw) {
			p.Label(max(x, wb.x), sp.top-1, label, fg, bg, act.name == a.sel)
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
			fg, bg := statusColor(n.status, t), labelBg
			if id == a.selTicket {
				fg, bg = t.Bg, t.Ice
			}
			label := " " + id + " "
			lw := ansi.StringWidth(label)
			if x := cx - lw/2; cy >= wb.y && cy < wb.y+wb.h && space.claim(x, cy, lw) {
				p.Label(max(x, wb.x), cy, label, fg, bg, n.status == TicketReview || id == a.selTicket)
				a.hits = append(a.hits, hit{id: "ticket:" + id, r: rect{max(x, wb.x), cy, lw, 1}})
			}
		}
	}
	if banner := stageBanner(sc, t); banner != "" && wb.h > 4 {
		p.Label(wb.x+(wb.w-ansi.StringWidth(banner))/2, wb.y+1, " "+banner+" ", t.Ice, labelBg, true)
	}
	if a.snap != nil && a.lay.dock.w == 0 {
		name := a.hover
		if name == "" {
			name = a.sel
		}
		if act := sc.byName[name]; act != nil && !act.gone && !a.showHelp {
			if sp, ok := a.spots[name]; ok {
				a.paintTelemetry(p, act, sp, a.live[name])
			}
		}
	}
	if a.snap == nil && a.lay.dock.w == 0 {
		msg := "no sprint yet, run /" + Name + " <request> to start one"
		p.Label(wb.x+(wb.w-ansi.StringWidth(msg))/2, wb.y+wb.h-2, msg, t.TextDim, t.Surface, false)
	}
	if a.cfgErr != nil && a.lay.dock.w == 0 {
		for i, line := range cards.WrapIndent("settings: "+a.cfgErr.Error(), wb.w-4, 0) {
			p.Label(wb.x+2, wb.y+2+i, line, t.Alarm, t.Surface, false)
		}
	}
}

// Pixels across and down one cell of the floor image takes and the shape of a pixel, coarser than the cell past the pixel budget of the protocol
func (a *arena) imagePixels(cols, rows int) (px, py int, aspect float64) {
	cw, ch := a.env.Graphics.Cell()
	_, most := a.env.Graphics.Budget()
	d := 1
	for (cols*cw/d)*(rows*ch/d) > most {
		d++
	}
	px, py = max(cw/d, 1), max(ch/d, 1)
	return px, py, (float64(cw) / float64(px)) / (float64(ch) / float64(py))
}

// Hands the frame to the terminal as pixels and paints the cells of the floor as the image needs them under it
func (a *arena) paintImage(p *cards.Painter, fr *vxl.Frame, wb rect) {
	if a.surface == nil {
		a.surface = a.env.Graphics.NewSurface()
	}
	a.rgb = fr.Bytes(a.rgb)
	if text := a.surface.Encode(a.rgb, fr.W, fr.H, a.originX+wb.x, a.originY+wb.y, wb.w, wb.h); text != "" && a.env.Send != nil {
		a.env.Send(tea.RawMsg{Msg: text})
	}
	a.surface.Paint(p.Cells(), wb.x, wb.y, wb.w, wb.h, a.env.Theme.Bg)
}

// Color the status of a ticket paints in
func statusColor(s TicketStatus, t theme.Theme) color.Color {
	switch s {
	case TicketActive:
		return t.Accent
	case TicketReview:
		return t.Ice
	case TicketDone:
		return t.Alive
	case TicketFailed, TicketBlocked:
		return t.Alarm
	}
	return t.TextDim
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

// Buttons of the toolbar in order, each pressed when what it toggles is on
func (a *arena) buttons() []cards.Button {
	s := a.snap
	out := []cards.Button{
		{ID: ActSidebar, Label: "Dock", On: a.dock},
		{ID: ActBoard, Label: "Tickets", On: a.dock && a.secs.IsOpen(secTickets.String())},
		{ID: ActLog, Label: "Log", On: a.dock && a.secs.IsOpen(secLog.String())},
	}
	if a.off() {
		out = append(out, cards.Button{ID: ActInspect, Label: "Talk", On: a.typing}, cards.Button{ID: ActWorld, Label: "World off", Gap: true})
	} else {
		out = append(out,
			cards.Button{ID: ActInspect, Label: "Inspect", On: a.inspect},
			cards.Button{ID: ActWorld, Label: "World " + a.cfg.Arena.World, Gap: true},
			cards.Button{ID: ActCamera, Label: "Camera " + a.rig.preset},
			cards.Button{ID: ActFollow, Label: "Follow", On: a.rig.preset == "follow"},
			cards.Button{ID: ActLabels, Label: "Labels", On: a.labels, Rank: 2},
			cards.Button{ID: ActReset, Label: "Reset view", Rank: 3},
		)
	}
	if s != nil {
		switch {
		case s.Running() && s.Paused != "":
			out = append(out, cards.Button{ID: ActPause, Label: "Resume", On: true, Gap: true}, cards.Button{ID: ActStop, Label: "Stop"})
		case s.Running():
			out = append(out, cards.Button{ID: ActPause, Label: "Pause", Gap: true}, cards.Button{ID: ActStop, Label: "Stop"})
		case s.Restartable():
			out = append(out, cards.Button{ID: ActRestart, Label: "Restart", Gap: true})
		}
	}
	if a.sprintCount > 1 {
		out = append(out, cards.Button{ID: ActSprintPrev, Label: "Prev sprint", Gap: true, Rank: 1}, cards.Button{ID: ActSprintNext, Label: "Next sprint", Rank: 1})
	}
	return append(out, cards.Button{ID: ActSettings, Label: "Settings", Gap: true, Rank: 2}, cards.Button{ID: ActHelp, Label: "Keys", On: a.showHelp})
}

// Toolbar across the top, every button clickable and labeled with its key, the ones that fit the width
func (a *arena) paintToolbar(p *cards.Painter) {
	bar := a.lay.bar
	cards.Toolbar{Keys: a.env.Keys, Hover: a.hoverHit}.Paint(p, bar.x, bar.y, bar.w, a.buttons(), func(id string, x, y, w, h int) {
		a.hits = append(a.hits, hit{id: id, r: rect{x, y, w, h}})
	})
}

// Legend of every arena key laid over the middle of the floor
func (a *arena) paintHelp(p *cards.Painter) {
	t := a.env.Theme
	wb := a.lay.world
	ids := []string{ActNext, ActPrev, ActInspect, ActSteer, ActLeave, ActOpen, ActKill, ActSidebar, ActBoard, ActLog, ActPageDown, ActPageUp, ActWorld, ActFollow, ActCamera, ActReset, ActOrbitLeft, ActOrbitRight, ActOrbitUp, ActOrbitDown, ActZoomIn, ActZoomOut, ActLabels, ActCustomize, ActPause, ActStop, ActRestart, ActSprintNext, ActSprintPrev, ActSettings, ActHelp}
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
	w := min(84, wb.w-4)
	lines = append(lines, "")
	for _, l := range cards.WrapIndent("Click the toolbar and any dock row, drag the world to turn the camera and scroll to zoom, drag the edge of the dock or the inspector to resize it and the rule between the tickets and the log to share the dock, click a section title to fold it", w-4, 0) {
		lines = append(lines, render.Dim(l, t))
	}
	h := min(len(lines)+2, wb.h-2)
	x, y := wb.x+(wb.w-w)/2, wb.y+(wb.h-h)/2
	p.Panel(x, y, w, h, "ARENA KEYS", 0.92)
	p.Lines(x+2, y+1, w-4, h-2, lines)
}

// Approval of a member laid over the bottom of the floor, answered with the approval keys
func (a *arena) paintApproval(p *cards.Painter) {
	ap := a.approval()
	if ap == nil {
		return
	}
	t := a.env.Theme
	wb := a.lay.world
	w := min(78, wb.w-4)
	h := min(16, wb.h-2)
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
	p.Fill(x, y, w, h, t.Surface)
	p.Panel(x, y, w, h, t.Glyphs.Approval+" APPROVAL "+t.Glyphs.Sep+" "+name, 0)
	p.Styled(x+2, y+1, w-4, h-2, ap.View(w-4, h-2))
}

// Box of the approval dialog, false while none shows
func (a *arena) approvalRect() (rect, bool) {
	if a.apRect.w == 0 {
		return rect{}, false
	}
	return a.apRect, true
}

// Transcript and prompt of the selected member under a header naming it and what it is doing, its left edge a resize handle
func (a *arena) paintInspector(p *cards.Painter) {
	t := a.env.Theme
	ib := a.lay.insp
	m, ok := a.selected()
	s := a.inspected()
	if !ok || s == nil {
		return
	}
	p.Fill(ib.x, ib.y, ib.w, ib.h, t.Surface)
	x0 := ib.x
	if !a.lay.full {
		p.Handle(ib.x, ib.y, ib.h, a.hoverHit == "edge:inspector" || (a.drag != nil && a.drag.kind == dragInspector))
		x0 = ib.x + 1
	}
	right := ib.x + ib.w - 1
	bg := theme.Mix(t.Surface, t.Ice, 0.08)
	if a.typing {
		bg = theme.Mix(t.Surface, t.Ice, 0.2)
	}
	p.Fill(x0, ib.y, ib.w-(x0-ib.x), 1, bg)
	x := x0 + 1
	x += p.Label(x, ib.y, t.Glyphs.Notch+" "+m.Name, t.Text, bg, true)
	if role := string(m.Role); !strings.EqualFold(role, m.Name) && !strings.HasPrefix(strings.ToLower(m.Name), role) {
		x += p.Label(x, ib.y, "  "+role, t.TextDim, bg, false)
	}
	closeLabel := " " + t.Glyphs.Close + " "
	cx := right - ansi.StringWidth(closeLabel)
	closeBg := bg
	if a.hoverHit == "inspector:close" {
		closeBg = t.SteelBright
	}
	p.Label(cx, ib.y, closeLabel, t.TextDim, closeBg, false)
	a.hits = append(a.hits, hit{id: "inspector:close", r: rect{cx, ib.y, ansi.StringWidth(closeLabel), 1}})
	if m.Live != nil && m.Live.Endpoint.Model != "" {
		model := m.Live.Endpoint.Model
		if mx := cx - 1 - ansi.StringWidth(model); mx >= x+3 {
			p.Label(mx, ib.y, model, t.TextDim, bg, false)
		}
	}
	live := a.live[m.Name]
	state := a.stateText(live, m)
	room := right - x0 - 1
	if a.snap != nil {
		if n := a.snap.Usage[m.Name].Output; n > 0 {
			tok := render.Tokens(n) + " generated"
			if tx := right - ansi.StringWidth(tok); tx >= x0+22 {
				p.Text(tx, ib.y+1, tok, t.TextDim, false)
				room = tx - x0 - 2
			}
		}
	}
	lines := cards.WrapIndent(state, max(room, 8), 2)
	if len(lines) > 3 {
		lines = lines[:3]
	}
	p.Lines(x0+1, ib.y+1, right-x0-1, len(lines), lines)
	a.inspHead = inspectorHead + len(lines) - 1
	p.Label(x0, ib.y+a.inspHead-1, strings.Repeat(t.Glyphs.Rule, ib.w-(x0-ib.x)), t.Steel, t.Surface, false)
	bw, bh := ib.w-(x0-ib.x), ib.h-a.inspHead
	if bw < 10 || bh < 3 {
		return
	}
	p.Styled(x0, ib.y+a.inspHead, bw, bh, s.View(bw, bh))
}

// Words on what a member does right now in the color of its state, with its task after
func (a *arena) stateText(live liveState, m Member) string {
	t := a.env.Theme
	var state string
	switch {
	case live.approval:
		state = render.Warm(t.Glyphs.Flag+" waiting for your approval of "+live.tool, t)
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
	return state
}

// Cursor of the prompt typed into, moved into the inspector box or the session window of the member
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
	if a.off() {
		tl, ok := a.tileOf(a.sel)
		if !ok {
			return nil
		}
		out.X += tl.r.x
		out.Y += tl.r.y + 1
		return &out
	}
	out.X += a.lay.insp.x
	if !a.lay.full {
		out.X++
	}
	out.Y += a.lay.insp.y + a.inspHead
	return &out
}

var _ wm.Painter = (*arena)(nil)
