package arena

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

	"github.com/nickheyer/icebath/pkg/ui/gfx"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/wm"
	"github.com/nickheyer/icebath/pkg/vxl"
)

// Rows the inspector spends above the sheet of a member before its state wraps onto more
const inspectorHead = 3

// Draws the toolbar, the dock, the newest frame of the floor, the timeline under it and the inspector
func (a *arena) Paint(buf *uv.Buffer) {
	a.w, a.h = buf.Width(), buf.Height()
	a.lay = a.arrange(a.w, a.h)
	p := cards.NewPainter(buf, a.env.Theme)
	p.Fill(0, 0, a.w, a.h, a.env.Theme.Surface)
	a.spots = map[string]spot{}
	a.hits = a.hits[:0]
	a.paintToolbar(p)
	if a.lay.dock.w > 0 {
		a.paintDock(p)
	}
	if a.off() && a.surface != nil {
		a.releaseImage()
	}
	if a.lay.world.w > 0 && a.lay.world.h > 0 {
		if a.off() {
			a.paintTiles(p)
		} else {
			a.paintWorld(p)
		}
		a.paintTape(p)
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

// Boxes of the toolbar, dock, floor, timeline and inspector, the dock and floor giving way as the card narrows
func (a *arena) arrange(w, h int) layout {
	l := a.panels(w, h)
	if a.snap != nil && l.world.w > 0 && l.world.h > 4 {
		l.tape = rect{l.world.x, l.world.y + l.world.h - 1, l.world.w, 1}
		l.world.h--
	}
	return l
}

// Boxes of the toolbar, the dock, the floor and the inspector
func (a *arena) panels(w, h int) layout {
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

// Tells the worker the size, glyph library and cell shape of the floor whenever they change, and swaps the surface when the display changes
func (a *arena) fitFloor(wb rect) {
	proto := a.protocol()
	if proto != a.proto {
		a.releaseImage()
		if proto != gfx.None {
			a.surface, a.proto = a.env.Graphics.NewSurface(proto), proto
			sf := a.surface
			a.stage.post(func(s *stage) { s.setSurface(sf) })
		}
	}
	mode := vxl.CellModeOf(a.cfg.Arena.Glyphs)
	aspect := 0.5
	if a.env.Graphics != nil {
		cw, ch := a.env.Graphics.Cell()
		aspect = float64(cw) / float64(max(ch, 1))
	}
	if wb.w == a.sentCols && wb.h == a.sentRows && mode == a.sentMode && aspect == a.sentAspect {
		return
	}
	a.sentCols, a.sentRows, a.sentMode, a.sentAspect = wb.w, wb.h, mode, aspect
	cols, rows := wb.w, wb.h
	a.stage.post(func(s *stage) { s.resize(cols, rows, mode, aspect) })
}

// Copies the newest frame of the floor into the cells, sends its pixels to the terminal, and places the labels over it
func (a *arena) paintWorld(p *cards.Painter) {
	t := a.env.Theme
	wb := a.lay.world
	a.fitFloor(wb)
	f := a.frame
	p.Fill(wb.x, wb.y, wb.w, wb.h, t.Bg)
	if f == nil {
		msg := "drawing the floor"
		p.Label(wb.x+(wb.w-ansi.StringWidth(msg))/2, wb.y+wb.h/2, msg, t.TextDim, t.Bg, false)
		a.paintNotes(p, wb)
		return
	}
	cols, rows := min(f.cols, wb.w), min(f.rows, wb.h)
	switch {
	case f.packet != nil && a.surface != nil && f.surface == a.surface:
		if !f.emitted {
			f.emitted = true
			if text := a.surface.Emit(f.packet, a.originX+wb.x, a.originY+wb.y, time.Now()); text != "" && a.env.Send != nil {
				a.env.Send(tea.RawMsg{Msg: text})
			}
		}
		a.surface.Paint(p.Cells(), wb.x, wb.y, cols, rows, t.Bg)
	case f.cells != nil:
		buf := p.Cells()
		for y := range rows {
			copy(buf.Lines[wb.y+y][wb.x:wb.x+cols], f.cells[y*f.cols:y*f.cols+cols])
		}
	}
	a.stage.painted(f.gen)
	a.paintedGen = f.gen
	if f.cols != wb.w || f.rows != wb.h {
		return
	}
	for name, sp := range f.spots {
		a.spots[name] = spot{x: wb.x + sp.x, top: wb.y + sp.top, bottom: wb.y + sp.bottom, depth: sp.depth}
	}
	var space labelSpace
	labelBg := theme.Mix(t.Bg, t.Surface, 0.5)
	for _, name := range a.labelOrder() {
		sp := a.spots[name]
		mark := f.actors[name]
		if mark.emote != "" && sp.top-2 >= wb.y && space.claim(sp.x, sp.top-2, 1) {
			p.Text(sp.x, sp.top-2, mark.emote, emoteColor(mark.emote, t), true)
		}
		if !(a.labels || name == a.sel || name == a.hover) || sp.top-1 < wb.y {
			continue
		}
		fg, bg := t.Text, labelBg
		if name == a.sel {
			fg, bg = t.Bg, t.Ice
		}
		label := " " + name + " "
		lw := ansi.StringWidth(label)
		if x := sp.x - lw/2; space.claim(x, sp.top-1, lw) {
			p.Label(max(x, wb.x), sp.top-1, label, fg, bg, name == a.sel)
		}
	}
	if a.labels {
		for _, n := range f.nodes {
			cx, cy := wb.x+n.x, wb.y+n.y
			fg, bg := statusColor(n.status, t), labelBg
			if n.id == a.selTicket {
				fg, bg = t.Bg, t.Ice
			}
			label := " " + n.id + " "
			lw := ansi.StringWidth(label)
			if x := cx - lw/2; cy >= wb.y && cy < wb.y+wb.h && space.claim(x, cy, lw) {
				p.Label(max(x, wb.x), cy, label, fg, bg, n.status == TicketReview || n.id == a.selTicket)
				a.hits = append(a.hits, hit{id: "ticket:" + n.id, r: rect{max(x, wb.x), cy, lw, 1}})
			}
		}
	}
	if f.banner != "" && wb.h > 4 {
		banner := t.Glyphs.Mission + " " + f.banner
		p.Label(wb.x+(wb.w-ansi.StringWidth(banner))/2, wb.y+1, " "+banner+" ", t.Ice, labelBg, true)
	}
	if a.snap != nil && a.lay.dock.w == 0 {
		name := a.hover
		if name == "" {
			name = a.sel
		}
		if mark, ok := f.actors[name]; ok && !mark.gone && !a.showHelp {
			if sp, ok := a.spots[name]; ok {
				a.paintTelemetry(p, name, mark, sp, a.live[name])
			}
		}
	}
	a.paintNotes(p, wb)
}

// Words over the floor on the lobby, bad settings and images the terminal cannot draw, when no dock carries them
func (a *arena) paintNotes(p *cards.Painter, wb rect) {
	t := a.env.Theme
	if a.snap == nil && a.lay.dock.w == 0 && wb.w > 20 {
		msg := "lobby, the crew lands once the agent starts a turn"
		if len(a.sprints) > 0 {
			msg = fmt.Sprintf("lobby, %s, %s browses them", render.Count(len(a.sprints), "turn"), a.env.Keys.Display(ActSprintPrev))
		}
		msg = ansi.Truncate(msg, wb.w-2, t.Glyphs.Ellipsis)
		p.Label(wb.x+(wb.w-ansi.StringWidth(msg))/2, wb.y+wb.h-2, msg, t.TextDim, t.Surface, false)
	}
	if a.lay.dock.w > 0 {
		return
	}
	row := wb.y + 2
	if a.cfgErr != nil {
		for _, line := range cards.WrapIndent("settings: "+a.cfgErr.Error(), wb.w-4, 0) {
			p.Label(wb.x+2, row, line, t.Alarm, t.Surface, false)
			row++
		}
	}
	if note := a.displayNote(); note != "" {
		for _, line := range cards.WrapIndent(note, wb.w-4, 0) {
			p.Label(wb.x+2, row, line, t.Accent, t.Surface, false)
			row++
		}
	}
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

// Names of the placed actors in the order their labels claim cells, selected first, hovered next, then nearest
func (a *arena) labelOrder() []string {
	names := make([]string, 0, len(a.spots))
	for name := range a.spots {
		names = append(names, name)
	}
	rank := func(name string) float64 {
		switch name {
		case a.sel:
			return -2
		case a.hover:
			return -1
		}
		return a.spots[name].depth
	}
	slices.SortStableFunc(names, func(p, q string) int { return cmp.Or(cmp.Compare(rank(p), rank(q)), cmp.Compare(p, q)) })
	return names
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

// Toolbar buttons in order, each pressed while what it toggles is on, the turn controls only where they act
func (a *arena) buttons() []cards.Button {
	s := a.snap
	out := []cards.Button{{ID: ActSidebar, Label: "Dock", On: a.dock}, {ID: ActInspect, Label: "Inspect", On: a.inspect}, {ID: ActView, Label: "View"}}
	if s != nil && a.replay == nil {
		switch {
		case s.Running() && s.Paused != "":
			out = append(out, cards.Button{ID: ActPause, Label: "Resume", On: true, Gap: true}, cards.Button{ID: ActStop, Label: "Stop"})
		case s.Running():
			out = append(out, cards.Button{ID: ActPause, Label: "Pause", Gap: true}, cards.Button{ID: ActStop, Label: "Stop"})
		case s.Restartable():
			out = append(out, cards.Button{ID: ActRestart, Label: "Retry", Gap: true})
		}
	}
	if s != nil {
		out = append(out, cards.Button{ID: ActReplay, Label: "Replay", On: a.replay != nil, Gap: true, Rank: 1})
	}
	if a.sprintCount > 1 {
		out = append(out, cards.Button{ID: ActSprintPrev, Label: "Prev turn", Gap: true, Rank: 2}, cards.Button{ID: ActSprintNext, Label: "Next turn", Rank: 2})
	}
	return append(out, cards.Button{ID: ActHelp, Label: "Keys", On: a.showHelp, Gap: true, Rank: 3})
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
	var lines []string
	for _, def := range actions {
		id := def.ID
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
	for _, l := range cards.WrapIndent("Click the toolbar, any dock row and any member or core on the floor, drag the world to turn the camera and scroll to zoom, drag the edge of the dock or the inspector to resize it and the title of the log to share the dock, click a section title to fold it, click or drag along the timeline to replay the turn from that moment", w-4, 0) {
		lines = append(lines, render.Dim(l, t))
	}
	h := min(len(lines)+2, wb.h-2)
	x, y := wb.x+(wb.w-w)/2, wb.y+(wb.h-h)/2
	p.Panel(x, y, w, h, "ARENA KEYS", 0.92)
	p.Lines(x+2, y+1, w-4, h-2, lines)
}

// Sheet of the selected member under a header naming it and its activity, its left edge a resize handle
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
	if model := m.Model; model != "" {
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
		state = render.Warm(t.Glyphs.Flag+" waiting on you to "+a.snap.Waiting, t)
	case live.asking:
		state = render.Cool(t.Glyphs.Ask+" asking you to "+a.snap.Waiting, t)
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

var _ wm.Painter = (*arena)(nil)

// Box of the floor while the terminal places its image under the cells, which must then keep no background
func (a *arena) Holes() []wm.Box {
	if a.surface == nil || a.proto != gfx.KittyPlaced || a.off() || a.lay.world.w <= 0 {
		return nil
	}
	wb := a.lay.world
	return []wm.Box{{X: wb.x, Y: wb.y, W: wb.w, H: wb.h}}
}

var _ wm.Transparent = (*arena)(nil)

// Card beside the hovered or selected actor while no dock shows, its name, what it does and what it spent
func (a *arena) paintTelemetry(p *cards.Painter, name string, mark actorMark, sp spot, live liveState) {
	t := a.env.Theme
	wb := a.lay.world
	m, ok := a.snap.Member(name)
	w := min(44, wb.w-4)
	if !ok || w < 16 {
		return
	}
	lines := []string{lipgloss.NewStyle().Foreground(t.Text).Bold(true).Render(m.Name) + " " + render.Dim(string(m.Role), t)}
	state := cards.WrapIndent(a.stateText(live, m), w-4, 2)
	lines = append(lines, state[:min(len(state), 3)]...)
	if mark.doing != "" && !live.running {
		lines = append(lines, render.Dim(mark.doing, t))
	}
	if n := live.tally.total(); n > 0 {
		lines = append(lines, render.Dim(render.Count(n, "tool call"), t))
	}
	if live.cost > 0 {
		lines = append(lines, render.Dim(render.Money(live.cost)+" spent", t))
	}
	h := min(len(lines)+2, wb.h)
	x := sp.x + pickReach + 1
	if x+w > wb.x+wb.w {
		x = sp.x - pickReach - 1 - w
	}
	x = min(max(x, wb.x), wb.x+wb.w-w)
	y := min(max(sp.top, wb.y), wb.y+wb.h-h)
	p.Panel(x, y, w, h, "", 0.85)
	p.Lines(x+2, y+1, w-4, h-2, lines)
}

// Cells of the timeline the track spans, between the play controls and the total time
func (a *arena) track() rect {
	tb := a.lay.tape
	left, right := 13, 7
	if a.replay != nil {
		right = 14
	}
	return rect{tb.x + left, tb.y, max(tb.w-left-right, 0), 1}
}

// Timeline under the floor with play and speed controls, the turn track marked at every exchange and the replay cursor
func (a *arena) paintTape(p *cards.Painter) {
	tb := a.lay.tape
	s := a.snap
	if tb.w <= 0 || s == nil {
		return
	}
	t := a.env.Theme
	tp, ok := a.tape(s.Sprint)
	if !ok {
		return
	}
	bg := theme.Mix(t.Surface, t.Bg, 0.5)
	p.Fill(tb.x, tb.y, tb.w, 1, bg)
	share, at := 1.0, tp.end
	glyph := "▶"
	speed := "1x"
	if r := a.replay; r != nil {
		share, at = r.share(tp), r.cursor
		speed = fmt.Sprintf("%gx", speeds[r.speed])
		if r.playing {
			glyph = "‖"
		}
	}
	hot := func(id string) color.Color {
		if a.hoverHit == id {
			return t.SteelBright
		}
		return bg
	}
	x := tb.x + 1
	p.Label(x, tb.y, " "+glyph+" ", t.Ice, hot("tape:play"), true)
	a.hits = append(a.hits, hit{id: "tape:play", r: rect{x, tb.y, 3, 1}})
	x += 3
	p.Label(x, tb.y, fmt.Sprintf("%-3s", speed), t.TextDim, hot("tape:speed"), false)
	a.hits = append(a.hits, hit{id: "tape:speed", r: rect{x, tb.y, 3, 1}})
	x += 4
	p.Label(x, tb.y, clockText(at.Sub(tp.start)), t.Text, bg, false)
	tr := a.track()
	if tr.w < 4 {
		return
	}
	cut := int(share*float64(tr.w-1) + 0.5)
	for i := range tr.w {
		fg, ch := t.Steel, "─"
		if i <= cut {
			fg, ch = theme.Mix(t.Ice, bg, 0.35), "━"
		}
		p.Label(tr.x+i, tr.y, ch, fg, bg, false)
	}
	span := tp.end.Sub(tp.start)
	for _, m := range tp.marks {
		if span <= 0 || m.Kind == MsgChat {
			continue
		}
		i := int(float64(m.At.Sub(tp.start))/float64(span)*float64(tr.w-1) + 0.5)
		if i >= 0 && i < tr.w {
			p.Label(tr.x+i, tr.y, "┃", kindColor(m.Kind, m.Text, t), bg, false)
		}
	}
	p.Label(tr.x+min(max(cut, 0), tr.w-1), tr.y, "●", t.Ice, bg, true)
	a.hits = append(a.hits, hit{id: "tape:track", r: tr})
	end := clockText(span)
	p.Label(tr.x+tr.w+1, tb.y, end, t.TextDim, bg, false)
	if a.replay != nil {
		lx := tr.x + tr.w + len(end) + 2
		p.Label(lx, tb.y, " live ", t.Bg, t.Ice, true)
		a.hits = append(a.hits, hit{id: "tape:live", r: rect{lx, tb.y, 6, 1}})
	}
}
