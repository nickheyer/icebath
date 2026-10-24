package wm

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/theme"
)

func TestAddWorkspaceFocusesOnlyTheFirstMaster(t *testing.T) {
	m := still(t, 100, 30)
	a, b := newStub("alpha"), newStub("beta")
	ws1, _ := m.AddWorkspace("ws1", a)
	ws2, _ := m.AddWorkspace("ws2", b)
	if a.inits != 1 || b.inits != 1 {
		t.Fatal("masters are initialized")
	}
	if !a.focused || b.focused || len(received[FocusMsg](b)) != 0 {
		t.Fatal("only the first master gains focus")
	}
	if m.Active() != ws1 || m.ActiveIndex() != 0 || m.IndexOf(ws2) != 1 || m.WorkspaceByID("ws2") != ws2 || m.WorkspaceByID("nope") != nil {
		t.Fatal("workspace lookups")
	}
	if ws := m.Workspaces(); len(ws) != 2 || ws[1] != ws2 {
		t.Fatal("workspaces")
	}
	if m.Focused() != a || m.WorkspaceOf(b) != ws2 || m.WorkspaceOf(newStub("x")) != nil {
		t.Fatal("focused and workspace of")
	}
	m.Switch(1)
	if a.focused || !b.focused || m.Active() != ws2 {
		t.Fatal("switch moves focus")
	}
}

func TestOpenIsIdempotentByKey(t *testing.T) {
	m, a, b, _ := threeTiles(t)
	ws := m.Active()
	again := newStub("beta")
	m.OpenHere(again)
	if len(ws.tiles) != 3 || ws.Focused() != b || again.inits != 0 {
		t.Fatal("second open must focus the one already open")
	}
	if c, where := m.Find("beta"); c != b || where != ws || !m.Has("beta") || m.Has("zeta") {
		t.Fatal("find")
	}
	if m.Open(nil, a) != nil || m.Open(ws, nil) != nil {
		t.Fatal("nil open")
	}
}

func TestOpenRelocatesACardFromAnotherWorkspace(t *testing.T) {
	m := still(t, 100, 30)
	a, b, shared := newStub("alpha"), newStub("beta"), newStub("shared")
	ws1, _ := m.AddWorkspace("ws1", a)
	ws2, _ := m.AddWorkspace("ws2", b)
	m.OpenHere(shared)
	if ws1.find("shared") == nil || ws1.Focused() != shared {
		t.Fatal("open in the first workspace")
	}
	m.Switch(1)
	m.OpenHere(shared)
	if ws1.find("shared") != nil || ws2.find("shared") == nil || ws2.Focused() != shared || m.Active() != ws2 {
		t.Fatal("relocate")
	}
	if ws1.Focused() != a {
		t.Fatal("the old workspace focuses its master again")
	}
	if shared.inits != 1 {
		t.Fatal("relocation must not initialize again")
	}
}

func TestClosePopsOutAndDropsOnceSettled(t *testing.T) {
	m, a, b, c := threeTiles(t, WithMotion(1))
	settle(t, m)
	ws := m.Active()
	m.Focus(c)
	m.Close(c)
	if ws.find("gamma") != nil || ws.Focused() != b || len(ws.tiles) != 3 {
		t.Fatal("closing hides the card from lookups but keeps its frame until it settles")
	}
	settle(t, m)
	if len(ws.tiles) != 2 || ws.holds(c) != nil || !b.focused || c.focused {
		t.Fatal("drop after settling")
	}
	m.Close(c)
	if m.CloseKey("alpha") == nil || len(ws.tiles) != 2 {
		t.Fatal("close by key starts the pop out")
	}
	settle(t, m)
	if len(ws.tiles) != 1 || ws.Master() != b || a.focused {
		t.Fatal("master closed")
	}
	if m.CloseKey("nope") != nil {
		t.Fatal("unknown key")
	}
}

func TestMotionZeroSnapsEverything(t *testing.T) {
	m, _, b, _ := threeTiles(t)
	ws := m.Active()
	for _, e := range ws.tiles {
		if e.rect.Distance() != 0 || e.frame() != e.box {
			t.Fatalf("%s not snapped", e.key())
		}
	}
	m.Close(b)
	if len(ws.tiles) != 2 {
		t.Fatal("motion zero drops at once")
	}
	if m.Advance(1.0 / 60) {
		t.Fatal("nothing moves without motion")
	}
	m.SetMotion(1)
	if m.Motion() != 1 {
		t.Fatal("motion")
	}
	m.SetMotion(0)
	if m.Motion() != 0 || m.nudge() != nil {
		t.Fatal("nudge is silent without motion")
	}
}

func TestSwitchSlidesAndClearsUrgency(t *testing.T) {
	m := still(t, 100, 30, WithMotion(1))
	a, b := newStub("alpha"), newStub("beta")
	ws1, _ := m.AddWorkspace("ws1", a)
	ws2, _ := m.AddWorkspace("ws2", b)
	settle(t, m)
	m.SetUrgent(ws2, true)
	m.SetUrgent(ws1, true)
	if !ws2.Urgent() || ws1.Urgent() {
		t.Fatal("urgent marks only hidden workspaces")
	}
	m.Switch(1)
	if m.leaving != ws1 || ws2.shift.Pos <= 0 || ws2.Urgent() {
		t.Fatal("switch starts the slide and clears urgency")
	}
	settle(t, m)
	if m.leaving != nil || ws2.shift.Pos != 0 {
		t.Fatal("slide settles")
	}
	if m.Switch(1) != nil && m.Active() != ws2 {
		t.Fatal("switching to the shown workspace")
	}
	m.Switch(0)
	if m.Active() != ws1 || !a.focused || b.focused {
		t.Fatal("switch back")
	}
	m.SetUrgent(nil, true)
}

func TestRemoveWorkspaceKeepsTheLastAndShiftsActive(t *testing.T) {
	m := still(t, 100, 30)
	a, b, c := newStub("a"), newStub("b"), newStub("c")
	m.AddWorkspace("1", a)
	m.AddWorkspace("2", b)
	m.AddWorkspace("3", c)
	m.Switch(2)
	m.RemoveWorkspace(2)
	if len(m.spaces) != 2 || m.ActiveIndex() != 1 || m.Active().ID != "2" || c.focused {
		t.Fatalf("remove the shown one %d", m.ActiveIndex())
	}
	m.RemoveWorkspace(0)
	if len(m.spaces) != 1 || m.ActiveIndex() != 0 || m.Active().ID != "2" {
		t.Fatalf("remove before the shown one %d", m.ActiveIndex())
	}
	m.RemoveWorkspace(0)
	if len(m.spaces) != 1 || m.RemoveWorkspace(5) != nil {
		t.Fatal("the last workspace stays")
	}
	m.AddWorkspace("4", newStub("d"))
	m.RemoveWorkspace(0)
	if m.Active().ID != "4" || m.ActiveIndex() != 0 {
		t.Fatal("removing the first shown one shows the next")
	}
}

func TestFocusMoveCycleAndMaster(t *testing.T) {
	m, a, b, c := threeTiles(t)
	ws := m.Active()
	m.FocusDir(Right)
	if ws.Focused() != b {
		t.Fatal("focus right")
	}
	m.FocusDir(Down)
	if ws.Focused() != c {
		t.Fatal("focus down")
	}
	m.FocusDir(Up)
	m.FocusDir(Left)
	if ws.Focused() != a {
		t.Fatal("focus up then left")
	}
	m.FocusDir(Left)
	if ws.Focused() != a {
		t.Fatal("focus stops at the edge")
	}
	m.Cycle(1)
	if ws.Focused() != b {
		t.Fatal("cycle")
	}
	m.Cycle(-2)
	if ws.Focused() != c {
		t.Fatal("cycle wraps")
	}
	m.SwapMaster()
	if ws.Master() != c || ws.tiles[2].card != a || ws.Focused() != c {
		t.Fatal("swap master")
	}
	m.SwapMaster()
	if ws.Master() != b || ws.Focused() != c {
		t.Fatal("master swaps with the first stack tile")
	}
	m.Focus(a)
	m.Move(Up)
	if ws.tiles[1].card != a || ws.tiles[2].card != c {
		t.Fatal("move up")
	}
	m.Move(Left)
	if ws.Master() != a || ws.tiles[1].card != b {
		t.Fatal("move left into master")
	}
	m.Move(Left)
	if ws.Master() != a {
		t.Fatal("move stops at the edge")
	}
}

func TestZoomFloatResizeAndReset(t *testing.T) {
	m, a, b, _ := threeTiles(t)
	ws := m.Active()
	m.Focus(b)
	m.ToggleZoom()
	eb := ws.holds(b)
	if ws.Zoomed() != b || eb.box != (Box{0, 0, 100, 29}) {
		t.Fatalf("zoom %v", eb.box)
	}
	m.FocusDir(Left)
	if ws.Focused() != b {
		t.Fatal("focus stays while zoomed")
	}
	m.Focus(a)
	if ws.Zoomed() != nil {
		t.Fatal("focusing another tile leaves zoom")
	}
	m.ToggleZoom()
	m.ToggleZoom()
	if ws.Zoomed() != nil {
		t.Fatal("zoom toggles off")
	}
	m.Focus(b)
	m.ToggleFloat()
	if !eb.floating || len(ws.floats) != 1 || len(ws.tiles) != 2 || ws.Focused() != b {
		t.Fatal("float")
	}
	if f := eb.box; f.X < 0 || f.Y < 0 || f.X+f.W > 100 || f.Y+f.H > 29 {
		t.Fatalf("float box %v", f)
	}
	m.Move(Right)
	moved := eb.box
	m.Move(Down)
	if moved.X <= 63 && eb.box.Y <= moved.Y {
		t.Fatalf("float nudges %v %v", moved, eb.box)
	}
	m.FocusDir(Up)
	if ws.Focused() != a {
		t.Fatal("focus from a float lands on the master")
	}
	m.Focus(b)
	m.ToggleFloat()
	if eb.floating || len(ws.tiles) != 3 || ws.tiles[1].card != b {
		t.Fatal("tile again at the top of the stack")
	}
	m.Resize(0.05)
	if math.Abs(m.Ratio()-0.67) > 1e-9 || ws.Master() != a || ws.holds(a).box != (Box{2, 1, 64, 27}) {
		t.Fatalf("resize %v %v", m.Ratio(), ws.holds(a).box)
	}
	m.Resize(1)
	if m.Ratio() != maxRatio {
		t.Fatal("resize clamps")
	}
	ws.tiles[1].weight = 3
	m.ResetLayout()
	if m.Ratio() != defaultRatio || ws.tiles[1].weight != 1 {
		t.Fatal("reset")
	}
	m.SetRatio(0.4)
	if m.Ratio() != 0.4 {
		t.Fatal("set ratio")
	}
}

func TestAttentionAndUrgency(t *testing.T) {
	m, a, b, _ := threeTiles(t, WithMotion(1))
	settle(t, m)
	m.Attention(a, Pulse)
	if m.UrgencyOf(a) != Calm || m.Active().holds(a).shake.Vel == 0 {
		t.Fatal("attention on the focused card rattles it")
	}
	m.Attention(b, Alarm)
	if m.UrgencyOf(b) != Alarm {
		t.Fatal("attention on another card lights it")
	}
	if !m.Advance(1.0 / 60) {
		t.Fatal("an urgent card keeps the clock going")
	}
	m.Focus(b)
	if m.UrgencyOf(b) != Calm || m.UrgencyOf(newStub("nope")) != Calm {
		t.Fatal("focus clears urgency")
	}
	m2 := still(t, 100, 30)
	ws1, _ := m2.AddWorkspace("1", newStub("a"))
	c := newStub("c")
	ws2, _ := m2.AddWorkspace("2", c)
	m2.Attention(c, Pulse)
	if !ws2.Urgent() || ws1.Urgent() {
		t.Fatal("attention in a hidden workspace marks it urgent")
	}
}

func TestKeysDispatchCommandsAndReachTheCard(t *testing.T) {
	m, a, b, c := threeTiles(t)
	d := newStub("delta")
	m.AddWorkspace("ws2", d)
	if k := key('2', tea.ModAlt); k.String() != "alt+2" {
		t.Fatalf("%q", k.String())
	}
	m.Update(key('2', tea.ModAlt))
	if m.ActiveIndex() != 1 {
		t.Fatal("alt+2 switches")
	}
	m.Update(key('[', tea.ModAlt))
	if m.ActiveIndex() != 0 {
		t.Fatal("alt+[ goes back")
	}
	m.Update(key('x'))
	if keys := received[tea.KeyPressMsg](a); len(keys) != 1 || keys[0].String() != "x" {
		t.Fatalf("plain keys reach the focused card %v", keys)
	}
	m.Update(key('l', tea.ModAlt))
	if m.Focused() != b {
		t.Fatal("alt+l focuses right")
	}
	m.Update(key('j', tea.ModAlt))
	if m.Focused() != c {
		t.Fatal("alt+j focuses down")
	}
	m.Update(key('f', tea.ModAlt))
	if m.Active().Zoomed() != c {
		t.Fatal("alt+f zooms")
	}
	m.Update(key('f', tea.ModAlt))
	m.Update(key('w', tea.ModAlt))
	if m.Active().find("gamma") != nil || len(m.Active().tiles) != 2 {
		t.Fatal("alt+w closes")
	}
	m.Update(key('v', tea.ModAlt))
	if !m.Active().holds(b).floating {
		t.Fatal("alt+v floats")
	}
	m.Update(key('v', tea.ModAlt))
	m.Update(key('m', tea.ModAlt))
	if m.Active().Master() != b {
		t.Fatal("alt+m swaps with master")
	}
	m.Update(key('=', tea.ModAlt))
	if m.Ratio() <= defaultRatio {
		t.Fatal("alt+= grows")
	}
	m.Update(key('-', tea.ModAlt))
	m.Update(key('r', tea.ModAlt))
	if m.Ratio() != defaultRatio {
		t.Fatal("alt+r resets")
	}
	m.Update(key('.', tea.ModAlt))
	m.Update(key(',', tea.ModAlt))
	if m.Focused() != b {
		t.Fatal("alt+. and alt+, cycle")
	}
	m.Update(key('h', tea.ModAlt|tea.ModShift))
	if m.Active().Master() != b {
		t.Fatal("move at the edge keeps the order")
	}
	m.Update(key('l', tea.ModAlt|tea.ModShift))
	if m.Active().Master() != a {
		t.Fatal("alt+shift+l moves right")
	}
	m.Update(key(']', tea.ModAlt))
	m.Update(key('1', tea.ModAlt))
	if m.ActiveIndex() != 0 {
		t.Fatal("alt+1")
	}
}

func TestPaletteKeysOpenAndEscDismisses(t *testing.T) {
	m, a, _, _ := threeTiles(t)
	m.Update(key('p', tea.ModCtrl))
	if m.Overlay() != nil {
		t.Fatal("ctrl+p belongs to the terminal host and must not open the palette")
	}
	if keys := received[tea.KeyPressMsg](a); len(keys) != 1 || keys[0].String() != "ctrl+p" {
		t.Fatalf("ctrl+p reaches the card %v", keys)
	}
	a.msgs = nil
	for _, k := range []tea.KeyPressMsg{key(tea.KeySpace, tea.ModAlt)} {
		m.Update(k)
		p, ok := m.Overlay().(*Palette)
		if !ok || !m.Capturing() {
			t.Fatalf("%s opens the palette", k.String())
		}
		m.Update(key('z'))
		if p.Query() != "z" {
			t.Fatal("overlay receives keys")
		}
		m.Update(key(tea.KeyEscape))
		if m.Overlay() != nil || m.Capturing() {
			t.Fatal("esc dismisses")
		}
	}
	m.Update(key(tea.KeySpace, tea.ModAlt))
	m.Update(key(tea.KeySpace, tea.ModAlt))
	if m.Overlay() != nil {
		t.Fatal("the palette key closes an open palette")
	}
	if len(received[tea.KeyPressMsg](a)) != 0 {
		t.Fatal("keys never leak past the overlay")
	}
	m.Update(key(tea.KeySpace, tea.ModAlt))
	m.Update(click(0, 29, tea.MouseLeft))
	if m.Overlay() != nil {
		t.Fatal("a click outside dismisses")
	}
	if m.Dismiss() != nil || m.ShowOverlay(nil) != nil {
		t.Fatal("nothing to dismiss")
	}
	o1, o2 := newStub("o1"), newStub("o2")
	m.ShowOverlay(o1)
	m.ShowOverlay(o2)
	if m.Overlay() != o2 || o1.focused || !o2.focused || o2.inits != 1 {
		t.Fatal("a new overlay replaces the old one")
	}
}

func TestMouseFocusesClosesDragsAndResizes(t *testing.T) {
	m, a, b, c := threeTiles(t)
	ws := m.Active()
	m.Update(click(70, 5, tea.MouseLeft))
	if ws.Focused() != b {
		t.Fatal("click focuses")
	}
	clicks := received[tea.MouseClickMsg](b)
	if len(clicks) != 1 || clicks[0].X != 4 || clicks[0].Y != 1 {
		t.Fatalf("content coordinates inside the padding %v", clicks)
	}
	m.Update(release(70, 5, tea.MouseLeft))
	if rel := received[tea.MouseReleaseMsg](b); len(rel) != 1 || rel[0].X != 4 {
		t.Fatalf("release reaches the card %v", rel)
	}
	m.Update(tea.MouseWheelMsg{X: 70, Y: 20, Button: tea.MouseWheelDown})
	if wh := received[tea.MouseWheelMsg](c); len(wh) != 1 || wh[0].X != 4 || wh[0].Y != 2 || ws.Focused() != b {
		t.Fatalf("wheel reaches the card under the pointer without focusing %v", wh)
	}
	m.Update(click(70, 2, tea.MouseLeft))
	m.Update(move(70, 21, tea.MouseLeft))
	if ws.tiles[1].card != c || ws.tiles[2].card != b || !ws.holds(b).held {
		t.Fatal("title drag swaps tiles live")
	}
	m.Update(release(70, 21, tea.MouseLeft))
	if ws.holds(b).held || m.grab != nil || ws.tiles[2].card != b {
		t.Fatal("release keeps the new order")
	}
	m.Update(click(61, 6, tea.MouseLeft))
	if m.resizer == nil {
		t.Fatal("the master right edge resizes")
	}
	m.Update(move(50, 6, tea.MouseLeft))
	if m.Ratio() != 0.5 || ws.holds(a).box.W != 48 {
		t.Fatalf("edge drag %v %v", m.Ratio(), ws.holds(a).box)
	}
	m.Update(release(50, 6, tea.MouseLeft))
	if m.resizer != nil {
		t.Fatal("release ends the resize")
	}
	m.Update(click(70, 5, tea.MouseMiddle))
	if ws.find("gamma") != nil {
		t.Fatal("middle click closes")
	}
	m.Update(click(5, 5, tea.MouseMiddle))
	if ws.find("alpha") != nil || ws.Master() != b || !b.focused {
		t.Fatal("the master closes on middle click and the next tile takes over")
	}
	m.Update(click(5, 5, tea.MouseRight))
	if r := received[tea.MouseClickMsg](b); len(r) == 0 || r[len(r)-1].Button != tea.MouseRight {
		t.Fatal("right click reaches the card")
	}
	_ = a
}

func TestDoubleClickOnTheTitleZooms(t *testing.T) {
	m, a, _, _ := threeTiles(t, frozen())
	ws := m.Active()
	m.Update(click(5, 2, tea.MouseLeft))
	m.Update(click(5, 2, tea.MouseLeft))
	if ws.Zoomed() != a {
		t.Fatal("double click on the title zooms")
	}
	m.Update(click(5, 1, tea.MouseLeft))
	m.Update(click(5, 1, tea.MouseLeft))
	if ws.Zoomed() != nil {
		t.Fatal("double click again unzooms, the zoomed title sitting on the stage edge")
	}
	m.Update(click(5, 5, tea.MouseLeft))
	m.Update(click(5, 5, tea.MouseLeft))
	if ws.Zoomed() != nil {
		t.Fatal("double click on content does not zoom")
	}
}

func TestMousePinnedAndStackWeightDrag(t *testing.T) {
	m := still(t, 100, 30)
	p := pinned{newStub("pin")}
	m.AddWorkspace("1", p)
	b, c := newStub("b"), newStub("c")
	m.OpenHere(b)
	m.OpenHere(c)
	ws := m.Active()
	m.Update(click(5, 5, tea.MouseMiddle))
	if ws.Master() != p {
		t.Fatal("a pinned card ignores middle click")
	}
	m.Focus(p)
	if m.CloseFocused() != nil || ws.Master() != p {
		t.Fatal("close focused leaves a pinned card")
	}
	ec := ws.holds(c)
	if ws.tiles[1] != ec {
		t.Fatal("the newest card sits at the top of the stack")
	}
	bottom := ec.box.Y + ec.box.H - 1
	m.Update(click(70, bottom+1, tea.MouseLeft))
	if m.resizer == nil || m.resizer.other == nil {
		t.Fatal("the edge between stack tiles resizes weights")
	}
	m.Update(move(70, bottom+6, tea.MouseLeft))
	if ec.weight <= ws.holds(b).weight {
		t.Fatalf("weights %v %v", ec.weight, ws.holds(b).weight)
	}
	m.Update(release(70, bottom+6, tea.MouseLeft))
	f := newStub("f")
	f.float = true
	m.OpenHere(f)
	ef := ws.holds(f)
	fb := ef.box
	m.Update(click(fb.X+2, fb.Y+1, tea.MouseLeft))
	m.Update(move(fb.X+12, fb.Y+4, tea.MouseLeft))
	m.Update(release(fb.X+12, fb.Y+4, tea.MouseLeft))
	if ef.box.X != fb.X+10 || ef.box.Y != fb.Y+3 {
		t.Fatalf("float drag %v from %v", ef.box, fb)
	}
	fb = ef.box
	m.Update(click(fb.X+fb.W-1, fb.Y+3, tea.MouseLeft))
	m.Update(move(fb.X+fb.W+4, fb.Y+3, tea.MouseLeft))
	m.Update(release(fb.X+fb.W+4, fb.Y+3, tea.MouseLeft))
	if ef.box.W != fb.W+5 {
		t.Fatalf("float resize %v from %v", ef.box, fb)
	}
}

func TestBarChipClickAndWheelSwitch(t *testing.T) {
	m := still(t, 100, 30)
	m.AddWorkspace("1", newStub("a"))
	m.AddWorkspace("2", newStub("b"))
	m.AddWorkspace("3", newStub("c"))
	m.Update(click(1, 0, tea.MouseLeft))
	if m.ActiveIndex() != 0 {
		t.Fatal("the margin before the chips is not a chip")
	}
	m.Update(click(8, 0, tea.MouseLeft))
	if m.ActiveIndex() != 1 {
		t.Fatalf("chip click %d", m.ActiveIndex())
	}
	m.Update(tea.MouseWheelMsg{X: 0, Y: 0, Button: tea.MouseWheelDown})
	if m.ActiveIndex() != 2 {
		t.Fatal("wheel down on the bar")
	}
	m.Update(tea.MouseWheelMsg{X: 0, Y: 0, Button: tea.MouseWheelUp})
	m.Update(tea.MouseWheelMsg{X: 0, Y: 0, Button: tea.MouseWheelUp})
	m.Update(tea.MouseWheelMsg{X: 0, Y: 0, Button: tea.MouseWheelUp})
	if m.ActiveIndex() != 0 {
		t.Fatal("wheel up stops at the first")
	}
	m.Update(move(8, 0, tea.MouseNone))
	if m.barHot != 1 {
		t.Fatal("hover on a chip")
	}
	m.Update(move(70, 5, tea.MouseNone))
	if m.barHot != -1 || m.hover == nil || m.hover.key() != "a" || m.hoverEdge != rContent {
		t.Fatal("hover leaves the bar for the master")
	}
	m.OpenHere(newStub("d"))
	m.Update(move(70, 5, tea.MouseNone))
	if m.hover == nil || m.hover.key() != "d" || m.hoverEdge != rContent {
		t.Fatal("hover over a card")
	}
	m.Update(move(70, 2, tea.MouseNone))
	if m.hoverEdge != rTitle {
		t.Fatal("hover on the title")
	}
	if _, cmd := m.Update(move(70, 2, tea.MouseNone)); cmd != nil {
		t.Fatal("an unchanged hover is quiet")
	}
	m.View()
	m.Update(move(96, 0, tea.MouseNone))
	if !m.menuHot || !m.onMenu(96) || m.onMenu(85) || m.onMenu(98) {
		t.Fatalf("hover on the palette button inside the margin %d %d", m.menuX, m.menuW)
	}
	m.Update(click(96, 0, tea.MouseLeft))
	if _, ok := m.Overlay().(*Palette); !ok {
		t.Fatal("the palette button opens the palette")
	}
	m.Dismiss()
	m.Update(click(96, 0, tea.MouseRight))
	if m.Overlay() != nil {
		t.Fatal("only the left button opens it")
	}
}

func TestTextSweepCopiesAndToasts(t *testing.T) {
	m, a, _, _ := threeTiles(t)
	a.sel = true
	a.body = "hello world\nsecond line"
	m.Active().holds(a).dirty = true
	m.View()
	m.Update(click(5, 4, tea.MouseLeft))
	m.Update(move(15, 4, tea.MouseLeft))
	if m.Selected() != "hello world" {
		t.Fatalf("%q", m.Selected())
	}
	_, cmd := m.Update(release(15, 4, tea.MouseLeft))
	msgs := collect(cmd, 100*time.Millisecond)
	found := false
	for _, msg := range msgs {
		if typeName(msg) == "tea.setClipboardMsg" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no clipboard command in %v", msgs)
	}
	if toasts := m.Toasts(); len(toasts) != 1 || toasts[0] != "copied 1 line" {
		t.Fatalf("%v", toasts)
	}
	if m.Selected() != "" {
		t.Fatal("selection ends on release")
	}
	m.Update(click(5, 4, tea.MouseLeft))
	m.Update(move(7, 5, tea.MouseLeft))
	if m.Selected() != "hello world\nsec" {
		t.Fatalf("%q", m.Selected())
	}
	m.Update(key('q'))
	if m.Selected() != "" {
		t.Fatal("a key drops the sweep")
	}
}

func TestIntentsReachTheManager(t *testing.T) {
	m, a, b, _ := threeTiles(t)
	ws := m.Active()
	for _, in := range []tea.Cmd{RequestFocus(b)} {
		m.Update(in())
	}
	if ws.Focused() != b {
		t.Fatal("request focus")
	}
	m.Update(RequestFloat(b, true)())
	if !ws.holds(b).floating {
		t.Fatal("request float")
	}
	m.Update(RequestFloat(b, false)())
	m.Update(RequestZoom(a, true)())
	if ws.Zoomed() != a || ws.Focused() != a {
		t.Fatal("request zoom")
	}
	m.Update(RequestZoom(a, false)())
	if ws.Zoomed() != nil {
		t.Fatal("request unzoom")
	}
	m.Update(RequestAttention(b, Pulse)())
	if m.UrgencyOf(b) != Pulse {
		t.Fatal("request attention")
	}
	sib := newStub("sibling")
	m.Update(RequestOpen(a, sib)())
	if ws.find("sibling") == nil {
		t.Fatal("request open")
	}
	m.Update(RequestClose(sib)())
	if ws.find("sibling") != nil {
		t.Fatal("request close")
	}
	ov := newStub("over")
	m.Update(RequestOverlay(ov)())
	if m.Overlay() != ov {
		t.Fatal("request overlay")
	}
	m.Update(RequestDismiss()())
	if m.Overlay() != nil {
		t.Fatal("request dismiss")
	}
	m.Update(Notify("hey", Warn)())
	m.Update(NotifyFor("yo", Info, time.Second)())
	if toasts := m.Toasts(); len(toasts) != 2 || toasts[1] != "yo" {
		t.Fatalf("%v", toasts)
	}
	m.Update(RequestFocus(newStub("stranger"))())
	m.Update(RequestOpen(a, nil)())
	m.Update(RequestOverlay(nil)())
	if ws.Focused() != b || len(ws.tiles) != 3 || m.Overlay() != nil {
		t.Fatal("intents from strangers or with nothing are ignored")
	}
}

func TestBroadcastAndFocusMessages(t *testing.T) {
	m, a, b, _ := threeTiles(t)
	d := newStub("delta")
	m.AddWorkspace("2", d)
	type custom struct{ n int }
	m.Update(custom{7})
	for _, s := range []*stub{a, b, d} {
		if got := received[custom](s); len(got) != 1 || got[0].n != 7 {
			t.Fatalf("%s missed the broadcast", s.title)
		}
	}
	m.Update(tea.BlurMsg{})
	if !m.blurred || !m.canRest() {
		t.Fatal("blur lets the clock rest")
	}
	m.Update(tea.FocusMsg{})
	if m.blurred {
		t.Fatal("focus wakes")
	}
	m.Update(tea.PasteMsg{Content: "paste"})
	if got := received[tea.PasteMsg](a); len(got) != 1 || got[0].Content != "paste" {
		t.Fatal("paste reaches the focused card")
	}
	_, cmd := m.Update(tea.WindowSizeMsg{Width: 80, Height: 20})
	if w, h := m.Size(); w != 80 || h != 20 || cmd == nil {
		t.Fatal("window size")
	}
	m.Update(key(tea.KeyEnter))
	if got := received[tea.KeyPressMsg](a); len(got) != 1 || got[0].String() != "enter" {
		t.Fatal("enter reaches the card")
	}
}

func TestViewAndCursorTranslation(t *testing.T) {
	m := still(t, 100, 30)
	s := sized{newStub("cur"), 20, 5}
	m.AddWorkspace("1", s)
	m.SetWindowTitle("icebath")
	m.SetStatus(&tea.ProgramStatus{App: "icebath"})
	m.SetProgress(tea.NewProgressBar(tea.ProgressBarDefault, 50))
	v := m.View()
	if !v.AltScreen || v.WindowTitle != "icebath" || v.ProgramStatus == nil || v.ProgressBar == nil || v.MouseMode != tea.MouseModeAllMotion {
		t.Fatal("view fields")
	}
	if v.Cursor == nil || v.Cursor.X != 6 || v.Cursor.Y != 6 {
		t.Fatalf("cursor past the border and padding %v", v.Cursor)
	}
	f := sized{newStub("dialog"), 30, 6}
	f.float = true
	m.OpenHere(f)
	fb := m.Active().holds(f).box
	if fb.W != 36 || fb.H != 10 {
		t.Fatalf("float sized from the card plus border and padding %v", fb)
	}
	v = m.View()
	if v.Cursor == nil || v.Cursor.X != fb.X+4 || v.Cursor.Y != fb.Y+barHeight+2+2 {
		t.Fatalf("float cursor %v %v", v.Cursor, fb)
	}
	m.SetSize(8, 4)
	if v := m.View(); v.Cursor != nil {
		t.Fatal("cursor hides when the frame is too small")
	}
	m.SetSize(0, 0)
	m.View()
}

func TestAdvanceRestsAndFrames(t *testing.T) {
	m, _, _, _ := threeTiles(t, WithMotion(1), WithFPS(60, 20))
	if m.Resting() {
		t.Fatal("starts awake")
	}
	settle(t, m)
	if m.Advance(1.0 / 60) {
		t.Fatal("settled")
	}
	if m.canRest() {
		t.Fatal("a shown manager keeps its ring flowing")
	}
	m.Update(tea.BlurMsg{})
	m.Update(m.wake()())
	if !m.Resting() {
		t.Fatal("a settled blurred manager rests")
	}
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
	if cmd == nil || m.Resting() {
		t.Fatal("input after rest wakes the clock")
	}
	stale := m.wake()()
	m.wake()
	if _, cmd := m.Update(stale); cmd != nil {
		t.Fatal("a stale frame is dropped")
	}
	m.Toast("x", Info, time.Millisecond)
	if m.canRest() {
		t.Fatal("toasts keep the clock going")
	}
}

// Stub that names its keys and carries a status trailer
type hinted struct{ *stub }

func (h hinted) Hints() []Hint  { return []Hint{{Action: "composer.send"}, {Action: "palette"}} }
func (h hinted) Status() string { return "3 turns" }

func (h hinted) Update(msg tea.Msg) (Card, tea.Cmd) {
	h.stub.Update(msg)
	return h, nil
}

func TestCascadeTogglesAndLightsTheFocusedBorder(t *testing.T) {
	m := New()
	if m.Cascade() {
		t.Fatal("cascade starts off")
	}
	m.SetCascade(true)
	if !m.Cascade() {
		t.Fatal("cascade toggles on")
	}
	// The comet lights the focused card's border, so its border must change when
	// the cascade toggles. Turning it off must still render without panicking.
	m, a, _, _ := threeTiles(t, WithMotion(1))
	settle(t, m)
	m.Focus(a)
	m.SetCascade(true)
	for i := 0; i < 60; i++ {
		m.Advance(1.0 / 60)
	}
	on := borderBorder(m, m.Active().holds(a).box)
	m.SetCascade(false)
	off := borderBorder(m, m.Active().holds(a).box)
	if on == off {
		t.Fatal("the focused card's border must change when the cascade toggles")
	}
}

// Rendered top border cells of a card, so two renders can be compared for the comet
func borderBorder(m *Model, b Box) string {
	rows := strings.Split(m.View().Content, "\n")
	var out strings.Builder
	for x := b.X; x < b.X+b.W; x++ {
		out.WriteString(ansi.Cut(rows[b.Y+1], x, x+1))
	}
	return out.String()
}

func TestCascadeIntensityPeaksAtTheHead(t *testing.T) {
	perim := 40
	head := 20
	// The head cell itself is fully lit
	if i := cascadeIntensity(perim, head, head); i <= 0.999 {
		t.Fatalf("head intensity should peak at 1, got %v", i)
	}
	// A cell a little behind the head is dimmer than the head
	back := cascadeIntensity(perim, head-3, head)
	if back >= 1 {
		t.Fatalf("tail should fade below the head, got %v", back)
	}
	// The cell directly opposite the head is the dimmest
	opp := cascadeIntensity(perim, head+perim/2, head)
	if opp >= back {
		t.Fatalf("the far side should be dimmest, got %v", opp)
	}
	// A negative or oversized perimeter is handled gracefully
	if cascadeIntensity(0, 0, 0) != 0 || cascadeIntensity(-5, 3, 1) != 0 {
		t.Fatal("bad perimeter returns zero")
	}
}

func TestCascadeOffByDefaultKeepsGoldenSnapshots(t *testing.T) {
	// With cascade off, the rim must not inject the comet color even on the focused card
	m := New(WithMotion(1))
	m.SetSize(100, 30)
	a := newStub("TRACE")
	m.AddWorkspace("s1", a)
	m.Focus(a)
	settle(t, m)
	if m.Cascade() {
		t.Fatal("cascade defaults off")
	}
	m.View()
}

func TestLayoutModesSpiralResizeAndToggle(t *testing.T) {
	m, a, b, c := threeTiles(t)
	ws := m.Active()
	if ws.Mode() != Master || m.Layout().Mode != "master" {
		t.Fatal("workspaces start in the master layout")
	}
	m.SetLayout(Dwindle)
	ea, eb, ec := ws.holds(a), ws.holds(b), ws.holds(c)
	if ws.Mode() != Dwindle || ea.box != (Box{2, 1, 48, 27}) || eb.box != (Box{51, 1, 47, 14}) || ec.box != (Box{51, 16, 47, 12}) {
		t.Fatalf("spiral %v %v %v", ea.box, eb.box, ec.box)
	}
	if m.SetLayout(Dwindle) != nil {
		t.Fatal("setting the same layout is quiet")
	}
	m.Focus(a)
	m.Resize(0.05)
	if ea.box.W != 53 || eb.box.X != 56 {
		t.Fatalf("resize moves the cut of the focused tile %v %v", ea.box, eb.box)
	}
	m.Focus(c)
	m.Resize(0.1)
	if eb.box.H != 11 || ec.box.Y != 13 {
		t.Fatalf("the lower tile grows its cut in its own favor %v %v", eb.box, ec.box)
	}
	m.ResetLayout()
	if ea.box.W != 48 || eb.box.H != 14 || ws.tree.ratio != 0.5 || ws.tree.b.ratio != 0.5 {
		t.Fatalf("reset evens every cut %v %v", ea.box, eb.box)
	}
	m.Update(key(tea.KeySpace, tea.ModAlt|tea.ModShift))
	if ws.Mode() != Master {
		t.Fatal("alt+shift+space toggles back to master")
	}
	m.ToggleLayout()
	if ws.Mode() != Dwindle || m.Layout().Mode != "dwindle" {
		t.Fatal("toggle")
	}
	if m.Run("layout.master") == nil || ws.Mode() != Master {
		t.Fatal("layout.master command")
	}
	if m.Run("layout.dwindle") == nil || ws.Mode() != Dwindle {
		t.Fatal("layout.dwindle command")
	}
	if m.Run("layout.dwindle") != nil {
		t.Fatal("the layout commands hide when already in that layout")
	}
	solo := still(t, 100, 30, WithLayout(Dwindle))
	s := newStub("solo")
	solo.AddWorkspace("1", s)
	if solo.Active().Mode() != Dwindle || solo.Resize(0.1) != nil || solo.Active().holds(s).box != (Box{2, 1, 96, 27}) {
		t.Fatal("a lone tile fills the inner stage and cannot resize")
	}
}

func TestDwindleEdgeDragMovesTheCut(t *testing.T) {
	m, a, b, c := threeTiles(t)
	m.SetLayout(Dwindle)
	ws := m.Active()
	ea, eb, ec := ws.holds(a), ws.holds(b), ws.holds(c)
	m.Update(click(49, 6, tea.MouseLeft))
	if m.resizer == nil || m.resizer.split != ws.tree {
		t.Fatalf("the right edge of the first tile is the root cut %+v", m.resizer)
	}
	m.Update(move(60, 6, tea.MouseLeft))
	m.Update(release(60, 6, tea.MouseLeft))
	if ea.box.W != 59 || eb.box.X != 62 || m.resizer != nil {
		t.Fatalf("drag moves the cut %v %v", ea.box, eb.box)
	}
	m.Update(click(70, 15, tea.MouseLeft))
	if m.resizer == nil || m.resizer.split != ws.tree.b {
		t.Fatalf("the bottom edge of the second tile is the inner cut %+v", m.resizer)
	}
	m.Update(move(70, 21, tea.MouseLeft))
	m.Update(release(70, 21, tea.MouseLeft))
	if eb.box.H != 20 || ec.box.Y != 22 {
		t.Fatalf("drag moves the cut down %v %v", eb.box, ec.box)
	}
	m.Update(click(62, 20, tea.MouseLeft))
	if m.resizer == nil || m.resizer.split != ws.tree {
		t.Fatalf("the left edge of a tile resolves to the cut it borders %+v", m.resizer)
	}
	m.Update(release(62, 20, tea.MouseLeft))
	m.Update(click(62, 25, tea.MouseLeft))
	if m.resizer == nil || m.resizer.split != ws.tree {
		t.Fatalf("the left edge of the lower tile borders the root cut too %+v", m.resizer)
	}
	m.Update(release(62, 25, tea.MouseLeft))
	m.Update(click(99, 10, tea.MouseLeft))
	if m.resizer != nil {
		t.Fatal("the stage edge does not resize")
	}
}

func TestLayoutModePersists(t *testing.T) {
	m, _, _, _ := threeTiles(t)
	m.SetLayout(Dwindle)
	l := m.Layout()
	if l.Mode != "dwindle" {
		t.Fatalf("%+v", l)
	}
	fresh := New()
	fresh.SetSize(100, 30)
	fresh.AddWorkspace("1", newStub("a"))
	fresh.Restore(l)
	if fresh.Active().Mode() != Dwindle || fresh.mode != Dwindle {
		t.Fatal("restore applies the mode to open and later workspaces")
	}
	ws, _ := fresh.AddWorkspace("2", newStub("b"))
	if ws.Mode() != Dwindle {
		t.Fatal("later workspaces follow the restored mode")
	}
	fixed := New(WithLayout(Master))
	fixed.SetSize(100, 30)
	fixed.AddWorkspace("1", newStub("a"))
	fixed.Restore(Layout{Mode: "dwindle"})
	if fixed.Active().Mode() != Master {
		t.Fatal("the layout option wins")
	}
	fresh.Restore(Layout{Mode: "spiral"})
	if fresh.Active().Mode() != Dwindle {
		t.Fatal("an unknown mode changes nothing")
	}
}

func TestHintsPaintOnTheFocusedCardAndStatusOnTop(t *testing.T) {
	m := still(t, 100, 30)
	a := hinted{newStub("alpha")}
	b := hinted{newStub("beta")}
	m.AddWorkspace("1", a)
	m.OpenHere(b)
	m.Focus(a)
	m.Advance(1.0 / 60)
	ws := m.Active()
	ea, eb := ws.holds(a), ws.holds(b)
	rows := strings.Split(m.View().Content, "\n")
	row := func(y int) string { return ansi.Strip(rows[y]) }
	top, bottom := ea.box.Y+barHeight, ea.box.Y+ea.box.H-1+barHeight
	if !strings.Contains(row(top), "alpha") || !strings.Contains(ansi.Cut(row(top), 0, ea.box.X+ea.box.W), "3 turns") {
		t.Fatalf("status on the top edge %q", row(top))
	}
	if !strings.Contains(row(bottom), "enter send") || !strings.Contains(row(bottom), "alt+space commands") {
		t.Fatalf("hints on the bottom edge %q", row(bottom))
	}
	bb := ansi.Cut(row(eb.box.Y+eb.box.H-1+barHeight), eb.box.X, eb.box.X+eb.box.W)
	if strings.Contains(bb, "enter send") {
		t.Fatalf("hints only on the focused card %q", bb)
	}
	m.OpenPalette()
	rows = strings.Split(m.View().Content, "\n")
	if strings.Contains(row(bottom), "enter send") {
		t.Fatal("an overlay takes the hints away from the stage")
	}
	found := false
	for _, r := range rows {
		if strings.Contains(ansi.Strip(r), "enter run") {
			found = true
		}
	}
	if !found {
		t.Fatal("the palette paints its own hints")
	}
	m.Dismiss()
	m.SetSize(30, 8)
	m.View()
}

// Stub whose trailer is longer than its frame
type pathed struct{ *stub }

func (p pathed) Status() string {
	return "/home/someone/with/a/rather/deep/tree/of/folders/.config/icebath/keybinds.json"
}

func (p pathed) Update(msg tea.Msg) (Card, tea.Cmd) {
	p.stub.Update(msg)
	return p, nil
}

func TestLongTrailersKeepTheirEnd(t *testing.T) {
	m := still(t, 60, 12)
	m.AddWorkspace("1", pathed{newStub("cfg")})
	e := m.Active().tiles[0]
	top := ansi.Strip(strings.Split(m.View().Content, "\n")[e.box.Y+barHeight])
	if !strings.Contains(top, "⋯") || !strings.Contains(top, "icebath/keybinds.json ") || !strings.Contains(top, "cfg") {
		t.Fatalf("a long trailer is cut from the left beside the title %q", top)
	}
	if w := ansi.StringWidth(top); w != 60 {
		t.Fatalf("the frame keeps its width %d", w)
	}
}

func TestRepaintRedrawsCardsWhoseStateChangedOutsideThem(t *testing.T) {
	m, a, b, _ := threeTiles(t)
	settle(t, m)
	m.View()
	a.body, b.body = "alpha changed", "beta changed"
	if strings.Contains(ansi.Strip(m.View().Content), "changed") {
		t.Fatal("a card no message reached keeps its last drawing")
	}
	m.Repaint()
	if view := ansi.Strip(m.View().Content); !strings.Contains(view, "alpha changed") || !strings.Contains(view, "beta changed") {
		t.Fatal("repaint draws every card again")
	}
}

// Stub that keeps a note between runs
type noted struct {
	*stub
	note string
}

type noteState struct {
	Note string `json:"note"`
}

func (n *noted) State() any { return noteState{Note: n.note} }

func (n *noted) SetState(raw json.RawMessage) error {
	var st noteState
	if err := json.Unmarshal(raw, &st); err != nil {
		return err
	}
	n.note = st.Note
	return nil
}

func (n *noted) Update(msg tea.Msg) (Card, tea.Cmd) {
	n.stub.Update(msg)
	return n, nil
}

func TestWorkspaceLayoutRoundTrip(t *testing.T) {
	m := still(t, 120, 40)
	a, b, c := newStub("alpha"), newStub("beta"), newStub("gamma")
	notes := &noted{stub: newStub("notes"), note: "kept"}
	f := newStub("dialog")
	f.float = true
	m.AddWorkspace("ws1", a)
	m.OpenHere(b)
	m.OpenHere(c)
	m.OpenHere(notes)
	m.SetLayout(Dwindle)
	m.Focus(b)
	m.Resize(0.1)
	m.FlipSplit()
	m.OpenHere(f)
	m.Move(Right)
	m.Focus(c)
	m.ToggleZoom()
	m.remember("window.zoom")
	l := m.Layout()
	ws := l.Workspaces[0]
	if len(l.Workspaces) != 1 || l.Active != 0 || l.Mode != "dwindle" || len(l.Recent) != 1 || ws.Tree == nil || ws.Zoom != "gamma" || ws.Focus != "gamma" || len(ws.Tiles) != 4 || len(ws.Floats) != 1 || string(ws.Cards["notes"]) != `{"note":"kept"}` {
		t.Fatalf("%+v", l)
	}
	fresh := still(t, 120, 40)
	a2 := newStub("alpha")
	ws2, _ := fresh.AddWorkspace("ws1", a2)
	fresh.Restore(l)
	made := map[string]Card{}
	resolve := func(key string) Card {
		var c Card
		switch key {
		case "beta", "gamma":
			c = newStub(key)
		case "dialog":
			s := newStub(key)
			s.float = true
			c = s
		case "notes":
			c = &noted{stub: newStub("notes")}
		default:
			return nil
		}
		made[key] = c
		return c
	}
	fresh.RestoreWorkspace(ws2, ws, resolve)
	fresh.SetSize(120, 40)
	before, _ := json.Marshal(l)
	after, _ := json.Marshal(fresh.Layout())
	if string(before) != string(after) {
		t.Fatalf("the layout comes back as saved\n%s\n%s", before, after)
	}
	if ws2.Zoomed() != made["gamma"] || ws2.Focused() != made["gamma"] || ws2.Mode() != Dwindle || made["notes"].(*noted).note != "kept" || len(ws2.floats) != 1 || ws2.floats[0].card != made["dialog"] {
		t.Fatal("zoom, focus, mode, float and card state come back")
	}
	if ws2.tree == nil || len(ws2.tree.leaves(nil)) != 4 || ws2.tree.find(ws2.holds(made["beta"])).parent.ratio == 0.5 {
		t.Fatal("the split tree comes back with its ratios")
	}
	part := still(t, 120, 40)
	a3 := newStub("alpha")
	ws3, _ := part.AddWorkspace("ws1", a3)
	part.AddWorkspace("ws2", newStub("beta"))
	part.Switch(0)
	part.RestoreWorkspace(ws3, ws, func(key string) Card {
		if key == "gamma" {
			return nil
		}
		return resolve(key)
	})
	if len(ws3.tiles) != 2 || ws3.Zoomed() != nil || ws3.Focused() != a3 || len(ws3.tree.leaves(nil)) != 2 || part.WorkspaceOf(part.spaces[1].tiles[0].card) != part.spaces[1] {
		t.Fatalf("a missing card and one open elsewhere are left out, %d tiles", len(ws3.tiles))
	}
	if part.RestoreWorkspace(nil, ws, resolve) != nil || part.RestoreWorkspace(&Workspace{}, ws, resolve) != nil {
		t.Fatal("a workspace the manager does not hold restores nothing")
	}
	bad := ws
	bad.Cards = map[string]json.RawMessage{"notes": json.RawMessage(`{`)}
	warn := still(t, 120, 40)
	ws4, _ := warn.AddWorkspace("ws1", newStub("alpha"))
	warn.RestoreWorkspace(ws4, bad, resolve)
	if len(warn.Toasts()) != 1 {
		t.Fatal("a card state that cannot be read toasts")
	}
}

func TestTabsKeepTheActiveTab(t *testing.T) {
	tabs := NewTabs(theme.Blackwall(), "CFG", "*", newStub("one"), newStub("two"), newStub("three"))
	tabs.Select(2)
	raw, err := json.Marshal(tabs.State())
	if err != nil || string(raw) != `{"tab":"three"}` {
		t.Fatalf("%s %v", raw, err)
	}
	again := NewTabs(theme.Blackwall(), "CFG", "*", newStub("one"), newStub("two"), newStub("three"))
	if err := again.SetState(raw); err != nil {
		t.Fatal(err)
	}
	if _, i := again.Active(); i != 2 {
		t.Fatalf("active %d", i)
	}
	if again.SetState(json.RawMessage(`{"tab":"nine"}`)) != nil {
		t.Fatal("an unknown tab is no error")
	}
	if _, i := again.Active(); i != 2 {
		t.Fatal("an unknown tab changes nothing")
	}
	if again.SetState(json.RawMessage(`{`)) == nil {
		t.Fatal("a broken state is an error")
	}
	empty := NewTabs(theme.Blackwall(), "CFG", "*")
	if raw, _ := json.Marshal(empty.State()); string(raw) != `{}` {
		t.Fatalf("%s", raw)
	}
}
