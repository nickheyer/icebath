package wm

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

// Dwindle manager on a 100 by 30 screen holding one tile
func spiral(t *testing.T) (*Model, *Workspace, *stub) {
	t.Helper()
	m := still(t, 100, 30, WithLayout(Dwindle))
	a := newStub("alpha")
	ws, _ := m.AddWorkspace("ws1", a)
	return m, ws, a
}

func TestCutOfFollowsCellAspect(t *testing.T) {
	for _, c := range []struct {
		box  Box
		want bool
	}{{Box{0, 0, 90, 30}, true}, {Box{0, 0, 89, 30}, false}, {Box{0, 0, 49, 27}, false}, {Box{0, 0, 98, 27}, true}, {Box{}, true}} {
		if got := cutOf(c.box); got != c.want {
			t.Fatalf("cutOf(%v) %v", c.box, got)
		}
	}
}

func TestDwindleCutsTheFocusedTileIntoQuadrants(t *testing.T) {
	m, ws, a := spiral(t)
	b, c, d := newStub("beta"), newStub("gamma"), newStub("delta")
	m.OpenHere(b)
	ea, eb := ws.holds(a), ws.holds(b)
	if ea.box != (Box{2, 1, 48, 27}) || eb.box != (Box{51, 1, 47, 27}) || !ws.tree.vertical {
		t.Fatalf("two tiles sit side by side %v %v", ea.box, eb.box)
	}
	m.Focus(a)
	m.OpenHere(c)
	ec := ws.holds(c)
	if ea.box != (Box{2, 1, 48, 14}) || ec.box != (Box{2, 16, 48, 12}) || eb.box != (Box{51, 1, 47, 27}) {
		t.Fatalf("the third tile cuts the focused one below it %v %v %v", ea.box, ec.box, eb.box)
	}
	m.Focus(b)
	m.OpenHere(d)
	ed := ws.holds(d)
	if eb.box != (Box{51, 1, 47, 14}) || ed.box != (Box{51, 16, 47, 12}) || ea.box != (Box{2, 1, 48, 14}) || ec.box != (Box{2, 16, 48, 12}) {
		t.Fatalf("quadrants %v %v %v %v", ea.box, eb.box, ec.box, ed.box)
	}
	if cards := ws.Cards(); len(cards) != 4 || cards[0] != a || cards[1] != c || cards[2] != b || cards[3] != d {
		t.Fatalf("tiles read the tree left to right then top to bottom %v", cards)
	}
	m.Close(c)
	if ws.find("gamma") != nil || ea.box != (Box{2, 1, 48, 27}) || ws.tree.a.leaf != ea {
		t.Fatalf("closing collapses the cut onto the sibling %v", ea.box)
	}
	m.Focus(a)
	m.Move(Right)
	if eb.box != (Box{2, 1, 48, 27}) || ea.box != (Box{51, 1, 47, 14}) || ws.tree.a.leaf != eb {
		t.Fatalf("moving trades leaves %v %v", ea.box, eb.box)
	}
	m.FlipSplit()
	if ea.box != (Box{51, 1, 24, 27}) || ed.box != (Box{76, 1, 22, 27}) {
		t.Fatalf("flip turns the cut sideways %v %v", ea.box, ed.box)
	}
	m.Update(key('s', tea.ModAlt))
	if ea.box != (Box{51, 1, 47, 14}) {
		t.Fatalf("alt+s flips it back %v", ea.box)
	}
	m.SetLayout(Master)
	if cards := ws.Cards(); len(cards) != 3 || cards[0] != b || cards[1] != a || cards[2] != d || ws.tree != nil {
		t.Fatalf("master keeps the reading order of the tree %v", cards)
	}
	m.SetLayout(Dwindle)
	if eb.box != (Box{2, 1, 48, 27}) || ea.box != (Box{51, 1, 47, 14}) || ed.box != (Box{51, 16, 47, 12}) {
		t.Fatalf("back to dwindle rebuilds a spiral from the order %v %v %v", eb.box, ea.box, ed.box)
	}
}

func TestSplitForFindsTheCutAnEdgeBorders(t *testing.T) {
	m, ws, a := spiral(t)
	b := newStub("beta")
	m.OpenHere(b)
	ea, eb := ws.holds(a), ws.holds(b)
	if ws.splitFor(ea, rRight) != ws.tree || ws.splitFor(eb, rLeft) != ws.tree {
		t.Fatal("the shared edge belongs to the root cut")
	}
	for _, c := range []struct {
		e   *entry
		reg region
	}{{ea, rLeft}, {ea, rBottom}, {eb, rRight}, {eb, rBottom}, {ea, rTitle}} {
		if ws.splitFor(c.e, c.reg) != nil {
			t.Fatalf("%s %d is a stage edge", c.e.card.Title(), c.reg)
		}
	}
	m.Focus(a)
	m.Resize(0.1)
	if ea.box.W != 58 || eb.box.X != 61 {
		t.Fatalf("resize favors the focused left tile %v %v", ea.box, eb.box)
	}
	m.Focus(b)
	m.Resize(0.1)
	if ea.box.W != 48 || eb.box.X != 51 {
		t.Fatalf("resize favors the focused right tile %v %v", ea.box, eb.box)
	}
	solo := still(t, 100, 30, WithLayout(Dwindle))
	s := newStub("solo")
	solo.AddWorkspace("1", s)
	before := solo.Active().holds(s).box
	if solo.FlipSplit() != nil || solo.Active().holds(s).box != before {
		t.Fatal("a lone tile has no cut to flip")
	}
}

func TestNodeLayoutRespectsGapsAndMinimums(t *testing.T) {
	a, b := &entry{}, &entry{}
	n := &node{a: &node{leaf: a}, b: &node{leaf: b}, vertical: true, ratio: 0.01}
	n.layout(Box{0, 0, 98, 27}, 1)
	if a.box != (Box{0, 0, 20, 27}) || b.box != (Box{21, 0, 77, 27}) {
		t.Fatalf("the ratio clamps and the gap stays %v %v", a.box, b.box)
	}
	n.vertical, n.ratio = false, 0.99
	n.layout(Box{0, 0, 98, 27}, 1)
	if a.box != (Box{0, 0, 98, 22}) || b.box != (Box{0, 23, 98, 4}) {
		t.Fatalf("a stacked cut clamps too %v %v", a.box, b.box)
	}
	n.ratio = 0.3
	n.even()
	if n.ratio != 0.5 {
		t.Fatal("even resets the ratio")
	}
	if got := n.leaves(nil); len(got) != 2 || got[0] != a || got[1] != b || n.find(b) != n.b || n.find(&entry{}) != nil {
		t.Fatal("leaves and find")
	}
	n.layout(Box{0, 0, 1, 1}, 1)
	if !b.box.empty() || a.box.X < 0 {
		t.Fatalf("a box too small for a cut leaves the second side empty %v %v", a.box, b.box)
	}
}

func TestDwindleNeverPanics(t *testing.T) {
	for _, w := range []int{12, 30, 70, 100, 160} {
		for _, h := range []int{4, 9, 30} {
			m := still(t, w, h, WithLayout(Dwindle))
			a := newStub("a")
			ws, _ := m.AddWorkspace("1", a)
			var opened []*stub
			for i := 0; i < 7; i++ {
				cards := ws.Cards()
				m.Focus(cards[(i*5)%len(cards)])
				s := newStub(string(rune('b' + i)))
				m.OpenHere(s)
				opened = append(opened, s)
			}
			for i, s := range opened {
				if i%2 == 0 {
					m.Close(s)
				}
			}
			m.ResetLayout()
			m.FlipSplit()
			m.Resize(0.1)
			for _, e := range ws.live() {
				if b := e.box; !b.empty() && (b.X < 0 || b.Y < 0 || b.X+b.W > w || b.Y+b.H > h-barHeight) {
					t.Fatalf("%dx%d tile %s outside the stage %v", w, h, e.card.Title(), b)
				}
			}
			if len(ws.tree.leaves(nil)) != len(ws.live()) {
				t.Fatalf("%dx%d tree and tiles disagree", w, h)
			}
		}
	}
}
