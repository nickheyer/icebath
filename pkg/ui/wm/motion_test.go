package wm

import (
	"image/color"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/theme"
)

// Stub that moves on its own, recording the frame times it is given
type mover struct {
	*stub
	dts   []float64
	moved bool
}

func (v *mover) Animate(dt float64) bool {
	v.dts = append(v.dts, dt)
	return v.moved
}

func (v *mover) Update(msg tea.Msg) (Card, tea.Cmd) {
	v.stub.Update(msg)
	return v, nil
}

func TestDismissedOverlayFadesOutBeforeItGoes(t *testing.T) {
	m, a, _, _ := threeTiles(t, WithMotion(1))
	settle(t, m)
	m.OpenPalette()
	if m.veil.Target != 1 || m.parting != nil {
		t.Fatal("the veil rises with the overlay")
	}
	settle(t, m)
	if m.veil.Pos != 1 {
		t.Fatalf("veil %v", m.veil.Pos)
	}
	first := m.Overlay().(*Palette).Matches()[0].Title
	m.Dismiss()
	if m.Overlay() != nil || m.Capturing() || m.parting == nil || !m.parting.closing || m.veil.Target != 0 {
		t.Fatal("dismissal drops the overlay at once and keeps the sheet fading")
	}
	m.Update(key('x'))
	if keys := received[tea.KeyPressMsg](a); len(keys) != 1 {
		t.Fatal("keys reach the stage while the sheet fades")
	}
	rows := strings.Split(m.View().Content, "\n")
	if !strings.Contains(ansi.Strip(strings.Join(rows, "\n")), first) {
		t.Fatalf("the parting sheet still paints its rows, %q", first)
	}
	settle(t, m)
	if m.parting != nil || m.veil.Pos != 0 {
		t.Fatal("the sheet and the veil are gone once settled")
	}
	m.OpenPalette()
	o := newStub("over")
	m.ShowOverlay(o)
	if m.parting == nil || m.parting.card == m.Overlay() {
		t.Fatal("a replaced overlay fades out behind the new one")
	}
	m.SetMotion(0)
	if m.parting != nil || m.veil.Pos != 1 {
		t.Fatal("motion off snaps the veil and drops the sheet")
	}
	m.Dismiss()
	if m.veil.Pos != 0 {
		t.Fatal("motion off lowers the veil at once")
	}
}

func TestToastsSlideOutBeforeTheyAreForgotten(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	m := New(WithMotion(1), WithClock(func() time.Time { return now }))
	m.SetSize(100, 30)
	m.AddWorkspace("1", newStub("a"))
	m.Toast("one", Info, time.Second)
	settle(t, m)
	now = now.Add(2 * time.Second)
	m.Update(toastTickMsg{})
	if len(m.Toasts()) != 0 || len(m.toasts) != 1 || !m.toasts[0].gone {
		t.Fatal("an expired toast leaves the list while it slides out")
	}
	if !m.Advance(1.0 / 60) {
		t.Fatal("the slide out keeps the clock going")
	}
	settle(t, m)
	if len(m.toasts) != 0 {
		t.Fatal("a toast is forgotten once off the edge")
	}
	for i := 0; i < maxToasts+2; i++ {
		m.Toast("many", Warn, 0)
	}
	if len(m.Toasts()) != maxToasts || len(m.toasts) != maxToasts+2 {
		t.Fatalf("the oldest toasts slide out past the cap, %d live of %d", len(m.Toasts()), len(m.toasts))
	}
	settle(t, m)
	if len(m.toasts) != maxToasts {
		t.Fatal("the capped toasts are gone once settled")
	}
	rows := strings.Split(m.View().Content, "\n")
	row := ansi.Strip(rows[barHeight])
	at := len([]rune(row[:strings.Index(row, " ◬ many ")]))
	end := at + len([]rune(" ◬ many "))
	if g, fg, bg := glyphAt(m, at-1, barHeight); at < 2 || g != "▐" || !within(fg, m.theme.Surface, 2) || bg != m.theme.Bg {
		t.Fatalf("a toast pill caps its label with a half block of the surface on the cell behind it, got %q %v %v in %q", g, fg, bg, row)
	}
	if g, fg, _ := glyphAt(m, end, barHeight); g != "▌" || !within(fg, m.theme.Surface, 2) {
		t.Fatalf("a toast pill caps both ends %q", row)
	}
	if _, _, bg := glyphAt(m, at, barHeight); !within(bg, m.theme.Surface, 2) {
		t.Fatalf("a toast label sits on the surface %q", row)
	}
	if g, _, bg := glyphAt(m, at-2, barHeight); g != " " || bg != m.theme.Bg {
		t.Fatalf("a toast pill ends with its cap %q", row)
	}
	if row := ansi.Strip(rows[barHeight+toastGap]); !strings.Contains(row, "many") {
		t.Fatalf("toasts stack with a gap %q", row)
	}
}

func TestNudgeRattlesTheFrameSideways(t *testing.T) {
	m, a, _, _ := threeTiles(t, WithMotion(1))
	settle(t, m)
	e := m.Active().holds(a)
	m.nudge()
	if e.shake.Vel == 0 || e.rect.X.Vel != 0 {
		t.Fatal("the rattle leaves the frame spring alone")
	}
	swings, last := 0, 0.0
	for i := 0; i < 120 && m.Advance(1.0/60); i++ {
		if (e.shake.Pos > 0.5 && last <= 0.5) || (e.shake.Pos < -0.5 && last >= -0.5) {
			swings++
		}
		last = e.shake.Pos
	}
	if swings < 3 || e.shake.Pos != 0 {
		t.Fatalf("%d swings, resting at %v", swings, e.shake.Pos)
	}
	m.OpenPalette()
	m.nudge()
	if m.overlay.shake.Vel == 0 {
		t.Fatal("the overlay rattles while it holds the input")
	}
}

func TestCardsGrowOutOfTheCardThatOpenedThem(t *testing.T) {
	m, a, _, _ := threeTiles(t, WithMotion(1))
	settle(t, m)
	from := m.Active().holds(a).frame()
	f := sized{newStub("dialog"), 30, 6}
	f.float = true
	m.Update(RequestOpen(a, f)())
	e := m.Active().find("dialog")
	if e == nil || e.origin != from {
		t.Fatal("the origin is the frame of the asking card")
	}
	x, y, w, h := e.rect.Cells()
	if w >= e.box.W || h >= e.box.H || abs(x+w/2-(from.X+from.W/2)) > 1 || abs(y+h/2-(from.Y+from.H/2)) > 1 {
		t.Fatalf("the card starts small on the center of its origin, %d %d %d %d from %+v", x, y, w, h, from)
	}
	settle(t, m)
	if e.frame() != e.box {
		t.Fatal("the card lands in its box")
	}
	m.OpenHere(newStub("plain"))
	p := m.Active().find("plain")
	px, py, pw, ph := p.rect.Cells()
	if !p.origin.empty() || abs(px+pw/2-(p.box.X+p.box.W/2)) > 1 || abs(py+ph/2-(p.box.Y+p.box.H/2)) > 1 || pw >= p.box.W {
		t.Fatal("a card opened on its own pops in from its own center")
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func TestZoomFadesTheOtherTilesAway(t *testing.T) {
	m, a, b, _ := threeTiles(t, WithMotion(1))
	settle(t, m)
	ws := m.Active()
	m.ToggleZoom()
	eb := ws.holds(b)
	m.Advance(1.0 / 60)
	if eb.hide.Target != 1 || eb.hide.Pos <= 0 || eb.hide.Pos >= 0.5 {
		t.Fatalf("the other tiles start fading when the focused one zooms, %v", eb.hide.Pos)
	}
	rows := strings.Split(m.View().Content, "\n")
	if !strings.Contains(ansi.Strip(rows[eb.box.Y+barHeight]), "beta") {
		t.Fatal("a fading tile still paints")
	}
	settle(t, m)
	if eb.hide.Pos != 1 || ws.holds(a).hide.Pos != 0 {
		t.Fatal("hidden tiles settle at one, the zoomed at zero")
	}
	rows = strings.Split(m.View().Content, "\n")
	for _, r := range rows {
		if strings.Contains(ansi.Strip(r), "beta") {
			t.Fatal("a hidden tile no longer paints")
		}
	}
	m.ToggleZoom()
	settle(t, m)
	if eb.hide.Pos != 0 {
		t.Fatal("tiles come back when the zoom ends")
	}
}

func TestCardsAnimateWithScaledFrames(t *testing.T) {
	m := still(t, 100, 30, WithMotion(0.5))
	v := &mover{stub: newStub("v")}
	m.AddWorkspace("1", v)
	m.AddWorkspace("2", newStub("other"))
	m.Switch(1)
	settle(t, m)
	v.dts = nil
	m.Advance(1.0 / 60)
	if len(v.dts) != 1 || v.dts[0] != (1.0/60)/0.5 {
		t.Fatalf("cards off the stage keep their motion in step with scaled time %v", v.dts)
	}
	m.Switch(0)
	settle(t, m)
	m.View()
	e := m.Active().holds(v)
	v.moved = true
	if !m.Advance(1.0/60) || !e.dirty {
		t.Fatal("a moving card keeps the clock going and repaints")
	}
	m.SetMotion(0)
	v.dts = nil
	m.Advance(1.0 / 60)
	if len(v.dts) != 1 || v.dts[0] != 0 {
		t.Fatalf("motion off hands cards a zero frame so they snap %v", v.dts)
	}
}

// Glyph, foreground and background of one screen cell after a paint
func glyphAt(m *Model, x, y int) (string, color.Color, color.Color) {
	m.View()
	c := m.buf.CellAt(x, y)
	if c == nil {
		return "", nil, nil
	}
	return c.Content, c.Style.Fg, c.Style.Bg
}

func TestRingSitsOnTheSurfaceWithNothingOutside(t *testing.T) {
	m, a, b, _ := threeTiles(t, WithMotion(1))
	settle(t, m)
	ws := m.Active()
	ea, eb := ws.holds(a), ws.holds(b)
	top := eb.box.Y + barHeight
	midX, midY := eb.box.X+eb.box.W/2, eb.box.Y+barHeight+eb.box.H/2
	edge := eb.box.X + eb.box.W - 1
	if g, fg, bg := glyphAt(m, midX, top); g != "━" || fg != m.theme.Steel || bg != m.theme.Surface {
		t.Fatalf("an unfocused ring is a steel line on the surface, got %q %v %v", g, fg, bg)
	}
	if g, _, bg := glyphAt(m, eb.box.X, top); g != "┍" || bg != m.theme.Surface {
		t.Fatalf("the ring turns its corners with the border set on the surface, got %q %v", g, bg)
	}
	if g, _, bg := glyphAt(m, edge-1, midY); g != " " || bg != m.theme.Surface {
		t.Fatal("the surface fills every cell inside the ring")
	}
	if g, _, bg := glyphAt(m, edge, midY); g != "│" || bg != m.theme.Surface {
		t.Fatalf("the ring line sits on the surface so nothing shows between it and the content, got %q %v", g, bg)
	}
	if _, _, bg := glyphAt(m, edge+1, midY); bg != m.theme.Bg {
		t.Fatal("nothing of a card lies outside its ring")
	}
	if _, _, bg := glyphAt(m, eb.box.X+eb.box.W/2, eb.box.Y+barHeight-1); bg != m.theme.Bg {
		t.Fatal("the backdrop above a card stays the backdrop")
	}
	if g, _, bg := glyphAt(m, ea.box.X+3, ea.box.Y+barHeight); g == " " || bg != m.theme.Surface {
		t.Fatalf("the title sits on the surface, got %q %v", g, bg)
	}
	if g, _, bg := glyphAt(m, ea.box.X+3, ea.box.Y+barHeight+ea.box.H-1); g == " " || bg != m.theme.Surface {
		t.Fatalf("hints sit on the surface, got %q %v", g, bg)
	}
	if g, fg, bg := glyphAt(m, ea.box.X+ea.box.W-1, midY); g != "│" || fg == m.theme.Steel || bg != m.theme.Surface {
		t.Fatalf("the focused ring runs the focus colors over the surface, got %q %v %v", g, fg, bg)
	}
	if _, _, bg := glyphAt(m, ea.box.X+ea.box.W, midY); bg != m.theme.Bg {
		t.Fatal("the focused ring casts nothing outside the card")
	}
	f := sized{newStub("dialog"), 30, 6}
	f.float = true
	m.OpenHere(f)
	m.Focus(a)
	settle(t, m)
	e := m.Active().find("dialog")
	if g, fg, bg := glyphAt(m, e.box.X+e.box.W/2, e.box.Y+barHeight+e.box.H-1); g != "━" || fg != m.theme.Steel || bg != m.theme.Surface {
		t.Fatalf("a resting float draws the same ring as a tile, got %q %v %v", g, fg, bg)
	}
	m.OpenPalette()
	settle(t, m)
	o := m.overlay
	og, _, lineBg := glyphAt(m, o.box.X+o.box.W/2, o.box.Y+barHeight)
	_, _, aboveBg := glyphAt(m, o.box.X+o.box.W/2, o.box.Y+barHeight-1)
	_, _, besideBg := glyphAt(m, o.box.X+o.box.W/2, o.box.Y+barHeight+o.box.H)
	if og != "━" || lineBg != m.theme.Surface || aboveBg != m.theme.Surface || besideBg == m.theme.Bg || !darker(besideBg, m.theme.Bg) {
		t.Fatalf("the sheet ring sits on its surface under the field over a dimmed stage, got %q %v %v %v", og, lineBg, aboveBg, besideBg)
	}
}

func TestCardsCrossfadeOverWhatLiesBeneath(t *testing.T) {
	m, a, _, _ := threeTiles(t, WithMotion(1))
	settle(t, m)
	ea := m.Active().holds(a)
	f := sized{newStub("dialog"), 30, 6}
	f.float = true
	m.OpenHere(f)
	e := m.Active().find("dialog")
	for i := 0; i < 3; i++ {
		m.Advance(1.0 / 60)
	}
	if e.open.Pos <= 0 || e.open.Pos >= 1 {
		t.Fatalf("the dialog is part way open, %v", e.open.Pos)
	}
	x, y, w, h := e.rect.Cells()
	gap := ea.box.X + ea.box.W
	if gap <= x || gap >= x+w-1 {
		t.Fatalf("the dialog spans the gap beside the master, %d in %d..%d", gap, x, x+w)
	}
	_, _, bg := glyphAt(m, gap, y+barHeight+h/2)
	if !darker(bg, m.theme.Surface) || !darker(m.theme.Bg, bg) {
		t.Fatalf("an opening card blends its surface toward the backdrop, got %v", bg)
	}
	if g, _, bg := glyphAt(m, gap, y+barHeight); g != "━" || !darker(bg, m.theme.Surface) || !darker(m.theme.Bg, bg) {
		t.Fatalf("the ring of an opening card is drawn on its blended surface, got %q %v", g, bg)
	}
	if g, _, bg := glyphAt(m, x+w/2, y+barHeight+h/2); g != " " || bg != m.theme.Surface {
		t.Fatalf("over another surface the blend is that surface, got %q %v", g, bg)
	}
	settle(t, m)
	_, _, inner := glyphAt(m, e.box.X+1, e.box.Y+barHeight+1)
	if inner != m.theme.Surface {
		t.Fatalf("an open card is opaque, got %v", inner)
	}
	m.Close(f)
	m.Advance(1.0 / 60)
	m.Advance(1.0 / 60)
	cx, cy := ea.box.X+2, ea.box.Y+barHeight+1
	if g, _, _ := glyphAt(m, cx, cy); g == "" {
		t.Fatal("the tile beneath a closing dialog still paints")
	}
	m.ToggleZoom()
	m.Advance(1.0 / 60)
	eb := m.Active().tiles[1]
	if eb == ea {
		eb = m.Active().tiles[0]
	}
	bx, by, bw, bh := eb.rect.Cells()
	_, _, hbg := glyphAt(m, bx+bw/2, by+barHeight+bh/2)
	if eb.hide.Pos <= 0 || !darker(hbg, m.theme.Surface) {
		t.Fatalf("a tile hiding behind a zoom fades toward the backdrop, got %v at %v", hbg, eb.hide.Pos)
	}
}

// Whether a is darker than b, summing the channels
func darker(a, b color.Color) bool {
	if a == nil || b == nil {
		return false
	}
	ar, ag, ab, _ := a.RGBA()
	br, bg, bb, _ := b.RGBA()
	return ar+ag+ab < br+bg+bb
}

// Whether two colors sit within tol steps of each other on every channel
func within(a, b color.Color, tol uint32) bool {
	if a == nil || b == nil {
		return false
	}
	ar, ag, ab, _ := a.RGBA()
	br, bg, bb, _ := b.RGBA()
	gap := func(x, y uint32) uint32 {
		if x > y {
			return (x - y) >> 8
		}
		return (y - x) >> 8
	}
	return gap(ar, br) <= tol && gap(ag, bg) <= tol && gap(ab, bb) <= tol
}

func TestPlainThemesKeepGlyphBorders(t *testing.T) {
	m := still(t, 100, 30, WithTheme(theme.Plain()))
	m.AddWorkspace("1", newStub("a"))
	m.Advance(1.0 / 60)
	e := m.Active().holds(m.Focused())
	if g, _, _ := glyphAt(m, e.box.X, e.box.Y+barHeight); g != "+" {
		t.Fatalf("a plain theme draws its ascii border, got %q", g)
	}
}

func TestPointerLightsTheEdgeWhereItRests(t *testing.T) {
	m, a, _, _ := threeTiles(t, WithMotion(1))
	settle(t, m)
	e := m.Active().holds(a)
	edge := e.box.X + e.box.W - 1
	m.Update(move(edge, e.box.Y+barHeight+4, tea.MouseNone))
	if !m.hoverGrip || m.hoverEdge != rRight || m.hoverX != edge || m.hoverY != e.box.Y+4 {
		t.Fatalf("hover on a resizable edge is a grip at the pointer %v %v %d %d", m.hoverGrip, m.hoverEdge, m.hoverX, m.hoverY)
	}
	if _, cmd := m.Update(move(edge, e.box.Y+barHeight+10, tea.MouseNone)); cmd == nil {
		t.Fatal("sliding along the edge repaints the glow")
	}
	settle(t, m)
	rim := m.rim(e, e.box.W, e.box.H, 1, 1)
	near := perimIndex(e.box.W, e.box.H, e.box.W-1, 10)
	far := perimIndex(e.box.W, e.box.H, e.box.W-1, 2)
	if rim(near) == rim(far) {
		t.Fatal("the glow is brightest under the pointer")
	}
}

func TestPerimIndexWalksClockwise(t *testing.T) {
	w, h := 10, 5
	perim := 2*(w+h) - 4
	for _, c := range []struct{ x, y, k int }{{0, 0, 0}, {5, 0, 5}, {9, 0, 9}, {9, 2, 11}, {9, 4, 13}, {5, 4, 17}, {0, 4, 22}, {0, 2, 24}, {0, 1, 25}, {-3, -3, 0}, {20, 20, 13}} {
		if k := perimIndex(w, h, c.x, c.y); k != c.k || k >= perim {
			t.Fatalf("(%d,%d) -> %d, want %d", c.x, c.y, k, c.k)
		}
	}
}

func TestHintUnderThePointerLightsUp(t *testing.T) {
	m := still(t, 100, 30)
	a := hinted{newStub("alpha")}
	m.AddWorkspace("1", a)
	m.View()
	e := m.Active().holds(a)
	if len(e.hints) == 0 {
		t.Fatal("hints painted")
	}
	h := e.hints[0]
	m.Update(move(h.x0, e.box.Y+e.box.H-1+barHeight, tea.MouseNone))
	if m.hintHot != h.id {
		t.Fatalf("hover on a legend entry names its action %q", m.hintHot)
	}
	lit := strings.Split(m.View().Content, "\n")[e.box.Y+e.box.H-1+barHeight]
	m.Update(move(h.x0, e.box.Y+barHeight+3, tea.MouseNone))
	if m.hintHot != "" {
		t.Fatal("hover elsewhere clears it")
	}
	if plain := strings.Split(m.View().Content, "\n")[e.box.Y+e.box.H-1+barHeight]; plain == lit {
		t.Fatal("the lit legend paints differently")
	}
}

func TestEmptyStageInvitesThePalette(t *testing.T) {
	m := still(t, 100, 30)
	ws, _ := m.AddWorkspace("1", newStub("a"))
	m.Close(ws.tiles[0].card)
	if len(ws.entries()) != 0 {
		t.Fatal("closed")
	}
	if text := ansi.Strip(m.View().Content); !strings.Contains(text, "nothing open ∷ alt+space commands") {
		t.Fatal("an empty stage names the palette key")
	}
	m.SetEmptyHints("window.zoom", "session.fork", "nope", "palette")
	if text := ansi.Strip(m.View().Content); !strings.Contains(text, "nothing open ∷ alt+f fullscreen ∷ alt+space commands") {
		t.Fatalf("the hints name every bound action with its key, unbound and unknown ones left out\n%s", text)
	}
}

func TestPaletteHighlightGlidesAndFollowsThePointer(t *testing.T) {
	m, _, _, _ := threeTiles(t, WithMotion(1))
	settle(t, m)
	m.OpenPalette()
	settle(t, m)
	m.View()
	p := m.Overlay().(*Palette)
	if !p.glide || p.highlightRow() != 0 {
		t.Fatal("the highlight rests on the first row")
	}
	m.Update(key(tea.KeyDown))
	if p.sel != 1 || p.highlightRow() != 0 {
		t.Fatal("the highlight trails the selection")
	}
	if !m.Advance(1.0/60) || !m.overlay.dirty {
		t.Fatal("the glide repaints the palette")
	}
	settle(t, m)
	if p.highlightRow() != 1 {
		t.Fatalf("the highlight lands on the selection, row %d", p.highlightRow())
	}
	rows := strings.Split(p.View(60, 10), "\n")
	if ansi.Strip(rows[1]) == rows[1] || strings.Contains(rows[0], "\x1b[") && rows[0] == rows[1] {
		t.Fatal("the lit row is styled")
	}
	c := m.contentRect(m.overlay.frame())
	m.Update(tea.MouseMotionMsg{X: c.X + 4, Y: c.Y + barHeight + 3, Button: tea.MouseNone})
	if p.sel != 3 {
		t.Fatalf("the pointer picks the row under it, got %d", p.sel)
	}
	m.SetMotion(0)
	m.Advance(1.0 / 60)
	if p.glide || p.highlightRow() != 3 {
		t.Fatal("motion off snaps the highlight")
	}
}

func TestResizeHoldsDrawingsUntilTheSizeSettles(t *testing.T) {
	m := New(WithMotion(1))
	m.SetSize(100, 30)
	a := newStub("a")
	m.AddWorkspace("1", a)
	settle(t, m)
	m.View()
	was := a.lastSize
	m.SetSize(120, 40)
	m.View()
	if a.lastSize != was || !m.settling() {
		t.Fatalf("a card keeps its drawing right after a resize, drawn at %v", a.lastSize)
	}
	settle(t, m)
	m.View()
	if a.lastSize == was || m.settling() {
		t.Fatal("the card draws at its new size once the frames settle")
	}
	snap := still(t, 100, 30)
	b := newStub("b")
	snap.AddWorkspace("1", b)
	snap.View()
	before := b.lastSize
	snap.SetSize(120, 40)
	snap.View()
	if b.lastSize == before || snap.settling() {
		t.Fatal("with motion off a card draws at its new size at once")
	}
}

func TestCardsHoldTheirDrawingWhileTheirFrameMoves(t *testing.T) {
	m := New(WithMotion(1))
	m.SetSize(100, 30)
	a := newStub("a")
	m.AddWorkspace("1", a)
	settle(t, m)
	m.View()
	was := a.lastSize
	m.OpenHere(newStub("b"))
	m.Advance(1.0 / 60)
	m.View()
	if a.lastSize != was {
		t.Fatal("a card whose frame is still moving keeps its drawing")
	}
	settle(t, m)
	m.View()
	if a.lastSize == was {
		t.Fatal("the card draws at its resting size")
	}
}
