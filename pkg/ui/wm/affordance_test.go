package wm

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// Stub that names its keys and refuses to close
type fixed struct{ pinned }

func (f fixed) Hints() []Hint { return []Hint{{Action: "composer.send"}} }

func (f fixed) Update(msg tea.Msg) (Card, tea.Cmd) {
	f.stub.Update(msg)
	return f, nil
}

// Row y of the screen with its colors stripped
func screenRow(m *Model, y int) string {
	return ansi.Strip(strings.Split(m.View().Content, "\n")[y])
}

// Rune at column x of a stripped screen row
func cellAt(row string, x int) rune {
	r := []rune(row)
	if x < 0 || x >= len(r) {
		return 0
	}
	return r[x]
}

func TestChipPartsOpenAndCloseWorkspaces(t *testing.T) {
	m := still(t, 100, 30)
	m.AddWorkspace("1", newStub("a"))
	newAt := func(x int) bool { return m.barAt(x).kind == barNew }
	if m.chipsEnd() != 5 || !newAt(5) || !newAt(7) || newAt(8) || m.barAt(3) != (barSpot{kind: barChip}) {
		t.Fatalf("the new chip follows the only chip, which has no close mark %d", m.chipsEnd())
	}
	m.Update(click(6, 0, tea.MouseLeft))
	if len(m.Workspaces()) != 1 || m.NewWorkspace() != nil {
		t.Fatal("the new chip nudges when no workspace.new command is registered")
	}
	opened, closed := 0, 0
	m.SetCommands([]Command{
		{ID: "workspace.new", Title: "new", Run: func() tea.Cmd { opened++; return nil }},
		{ID: "workspace.close", Title: "close", When: func() bool { return len(m.Workspaces()) > 1 }, Run: func() tea.Cmd { closed++; return m.RemoveWorkspace(m.ActiveIndex()) }},
	})
	m.Update(click(6, 0, tea.MouseLeft))
	if opened != 1 {
		t.Fatal("the new chip runs workspace.new")
	}
	m.Update(click(3, 0, tea.MouseLeft))
	if closed != 0 || len(m.Workspaces()) != 1 {
		t.Fatal("the close command stays gated while one workspace is open")
	}
	m.AddWorkspace("2", newStub("b"))
	m.Switch(1)
	if m.barAt(8) != (barSpot{kind: barClose, i: 1}) || m.barAt(6) != (barSpot{kind: barChip, i: 1}) || !newAt(10) || m.barAt(9) != (barSpot{kind: barChip, i: 1}) {
		t.Fatalf("the active chip carries the close mark %v", m.chips)
	}
	m.Update(move(8, 0, tea.MouseNone))
	if m.hot != (barSpot{kind: barClose, i: 1}) {
		t.Fatalf("hover on the close mark %v", m.hot)
	}
	m.Update(move(11, 0, tea.MouseNone))
	if m.hot.kind != barNew {
		t.Fatalf("hover on the new chip %v", m.hot)
	}
	m.Update(move(6, 0, tea.MouseNone))
	if m.hot != (barSpot{kind: barChip, i: 1}) {
		t.Fatalf("hover on the chip body %v", m.hot)
	}
	m.View()
	m.Update(click(8, 0, tea.MouseLeft))
	if closed != 1 || len(m.Workspaces()) != 1 || m.ActiveIndex() != 0 {
		t.Fatalf("the close mark runs workspace.close %d %d", closed, len(m.Workspaces()))
	}
	bare := still(t, 100, 30)
	bare.AddWorkspace("1", newStub("a"))
	bare.AddWorkspace("2", newStub("b"))
	bare.Switch(1)
	bare.Update(click(8, 0, tea.MouseLeft))
	if len(bare.Workspaces()) != 1 {
		t.Fatal("without a close command the mark removes the workspace itself")
	}
	if bare.CloseWorkspace() != nil || len(bare.Workspaces()) != 1 {
		t.Fatal("the last workspace stays")
	}
}

func TestCloseMarksOnFrames(t *testing.T) {
	m, a, b, _ := threeTiles(t)
	ws := m.Active()
	ea, eb := ws.holds(a), ws.holds(b)
	top := ea.box.Y + barHeight
	if cellAt(screenRow(m, top), closeX(ea.box)) != '✕' || cellAt(screenRow(m, eb.box.Y+barHeight), closeX(eb.box)) != '✕' {
		t.Fatal("closable frames carry a close mark")
	}
	if regionOf(eb.box, closeX(eb.box), eb.box.Y) != rClose || regionOf(eb.box, closeX(eb.box)-1, eb.box.Y) != rTitle {
		t.Fatal("the mark is its own region")
	}
	m.Update(click(closeX(eb.box), eb.box.Y+barHeight, tea.MouseLeft))
	if ws.find("beta") != nil {
		t.Fatal("clicking the mark closes the card")
	}
	held := still(t, 100, 30)
	p := pinned{newStub("pin")}
	held.AddWorkspace("1", p)
	ep := held.Active().holds(p)
	if cellAt(screenRow(held, ep.box.Y+barHeight), closeX(ep.box)) == '✕' {
		t.Fatal("a card that cannot close has no mark")
	}
	held.Update(click(closeX(ep.box), ep.box.Y+barHeight, tea.MouseLeft))
	if held.Active().Master() != p {
		t.Fatal("the mark of a card that cannot close does nothing")
	}
}

func TestHoverGripShowsWhatCanBeDragged(t *testing.T) {
	m, a, _, _ := threeTiles(t)
	ws := m.Active()
	ea := ws.holds(a)
	top := ea.box.Y + barHeight
	m.Update(move(closeX(ea.box), top, tea.MouseNone))
	if m.hoverEdge != rClose || !m.hoverGrip || !strings.Contains(screenRow(m, top), "⊹") {
		t.Fatalf("hover on the mark %d %v", m.hoverEdge, m.hoverGrip)
	}
	m.Update(move(ea.box.X+5, top, tea.MouseNone))
	if m.hoverEdge != rTitle || !m.hoverGrip || !strings.Contains(screenRow(m, top), "⊹") {
		t.Fatalf("hover on the title %d %v", m.hoverEdge, m.hoverGrip)
	}
	m.Update(move(ea.box.X, top+5, tea.MouseNone))
	if m.hoverEdge != rLeft || m.hoverGrip || strings.Contains(screenRow(m, top), "⊹") {
		t.Fatalf("the stage edge cannot be dragged %d %v", m.hoverEdge, m.hoverGrip)
	}
	m.Update(move(ea.box.X+ea.box.W-1, top+5, tea.MouseNone))
	if m.hoverEdge != rRight || !m.hoverGrip {
		t.Fatalf("the master's right edge resizes %d %v", m.hoverEdge, m.hoverGrip)
	}
	m.Update(move(ea.box.X+5, top+5, tea.MouseNone))
	if m.hoverEdge != rContent || m.hoverGrip {
		t.Fatal("content is not a grip")
	}
}

func TestCloseHintFollowsClosability(t *testing.T) {
	m := still(t, 100, 30)
	a := hinted{newStub("alpha")}
	m.AddWorkspace("1", a)
	ea := m.Active().holds(a)
	bottom := ea.box.Y + ea.box.H - 1 + barHeight
	if row := screenRow(m, bottom); !strings.Contains(row, "enter send") || !strings.Contains(row, "alt+w close") {
		t.Fatalf("%q", row)
	}
	held := still(t, 100, 30)
	f := fixed{pinned{newStub("fixed")}}
	held.AddWorkspace("1", f)
	ef := held.Active().holds(f)
	bottom = ef.box.Y + ef.box.H - 1 + barHeight
	if row := screenRow(held, bottom); !strings.Contains(row, "enter send") || strings.Contains(row, "alt+w close") {
		t.Fatalf("%q", row)
	}
}

func TestLegacyShiftedMoveKeys(t *testing.T) {
	m, a, b, _ := threeTiles(t)
	ws := m.Active()
	m.Update(key('L', tea.ModAlt))
	if ws.Master() != b || ws.Focused() != a {
		t.Fatal("alt+L moves the focused tile right like alt+shift+l")
	}
	m.Update(key('H', tea.ModAlt))
	if ws.Master() != a {
		t.Fatal("alt+H moves it back left")
	}
}

func TestLongTitlesEndInAnEllipsisBeforeTheLine(t *testing.T) {
	m := still(t, 60, 24)
	long := newStub(strings.Repeat("abcdefghij", 8))
	m.AddWorkspace("1", long)
	m.OpenHere(newStub("b"))
	e := m.Active().holds(long)
	row := screenRow(m, e.box.Y+barHeight)
	edge := []rune(row)[e.box.X : e.box.X+e.box.W]
	text := string(edge)
	if !strings.Contains(text, "⋯ ━") || strings.Contains(text, strings.Repeat("abcdefghij", 8)) {
		t.Fatalf("a title wider than its edge is cut with an ellipsis and a space before the line, got %q", text)
	}
	if !strings.HasSuffix(text, "✕━┑") {
		t.Fatalf("the close mark keeps its place after a cut title, got %q", text)
	}
}
