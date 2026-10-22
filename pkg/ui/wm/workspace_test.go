package wm

import "testing"

func TestNeighborFindsTilesInEveryDirection(t *testing.T) {
	m, a, b, c := threeTiles(t)
	ws := m.Active()
	ea, eb, ec := ws.holds(a), ws.holds(b), ws.holds(c)
	cases := []struct {
		from *entry
		dir  Dir
		want *entry
	}{{ea, Right, eb}, {ea, Left, nil}, {ea, Up, nil}, {ea, Down, nil}, {eb, Left, ea}, {eb, Down, ec}, {eb, Up, nil}, {eb, Right, nil}, {ec, Up, eb}, {ec, Left, ea}, {ec, Down, nil}}
	for _, cs := range cases {
		if got := ws.neighbor(cs.from, cs.dir); got != cs.want {
			t.Fatalf("%s %d gave %v", cs.from.key(), cs.dir, got)
		}
	}
}

func TestSwapRaiseFindAndHolds(t *testing.T) {
	m, a, b, c := threeTiles(t)
	ws := m.Active()
	ws.swap(ws.holds(a), ws.holds(c))
	if ws.Master() != c || ws.tiles[2].card != a {
		t.Fatal("swap")
	}
	ws.swap(ws.holds(a), ws.holds(a))
	if ws.tiles[2].card != a {
		t.Fatal("self swap")
	}
	f1, f2 := newStub("f1"), newStub("f2")
	f1.float, f2.float = true, true
	m.OpenHere(f1)
	m.OpenHere(f2)
	if ws.floats[1].card != f2 {
		t.Fatal("float order")
	}
	ws.raise(ws.holds(f1))
	if ws.floats[1].card != f1 {
		t.Fatal("raise")
	}
	if ws.find("beta").card != b || ws.find("nope") != nil {
		t.Fatal("find")
	}
	if ws.holds(newStub("other")) != nil {
		t.Fatal("holds a stranger")
	}
	cards := ws.Cards()
	if len(cards) != 5 || cards[0] != c || cards[3] != f2 {
		t.Fatalf("%v", cards)
	}
	ws.remove(ws.holds(f1))
	if ws.find("f1") != nil || len(ws.floats) != 1 {
		t.Fatal("remove float")
	}
}

func TestHitTestingPrefersFloatsAndZoom(t *testing.T) {
	m, a, b, c := threeTiles(t)
	ws := m.Active()
	if ws.at(5, 5).card != a || ws.at(70, 5).card != b || ws.at(70, 20).card != c || ws.at(62, 5) != nil {
		t.Fatal("tiles")
	}
	if ws.slotAt(70, 20) != 2 || ws.slotAt(5, 5) != 0 || ws.slotAt(62, 5) != -1 {
		t.Fatal("slots")
	}
	f := newStub("dialog")
	f.float = true
	m.OpenHere(f)
	fb := ws.holds(f).frame()
	if ws.at(fb.X+1, fb.Y+1).card != f {
		t.Fatal("float on top")
	}
	m.Focus(b)
	m.ToggleZoom()
	if ws.Zoomed() != b || ws.at(5, 5).card != b {
		t.Fatal("zoom covers the stage")
	}
	if ws.at(fb.X+1, fb.Y+1).card != f {
		t.Fatal("float over zoom")
	}
	ws.remove(ws.holds(f))
	if ws.at(fb.X+1, fb.Y+1).card != b {
		t.Fatal("zoom after float removal")
	}
}

func TestWorkspaceAccessors(t *testing.T) {
	m, a, b, _ := threeTiles(t)
	ws := m.Active()
	if ws.Focused() != a || ws.Master() != a || ws.Zoomed() != nil || ws.Ratio() != defaultRatio {
		t.Fatal("accessors")
	}
	ws.SetBusy(true)
	if !ws.Busy() || ws.Urgent() {
		t.Fatal("flags")
	}
	m.Focus(b)
	if ws.Focused() != b {
		t.Fatal("focus")
	}
}
