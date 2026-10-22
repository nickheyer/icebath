package wm

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestArrangeOneCardFillsTheStage(t *testing.T) {
	boxes := arrange(100, 30, 1, 0.62, nil, 1, 1)
	if len(boxes) != 1 || boxes[0] != (Box{0, 0, 100, 30}) {
		t.Fatalf("%v", boxes)
	}
	if arrange(100, 30, 0, 0.62, nil, 1, 1) != nil || arrange(0, 30, 2, 0.62, nil, 1, 1) != nil {
		t.Fatal("empty stage or no cards must give no boxes")
	}
}

func TestArrangeWideSplitsMasterLeftAndStackRight(t *testing.T) {
	boxes := arrange(100, 30, 2, 0.62, nil, 1, 1)
	if boxes[0] != (Box{0, 0, 62, 30}) {
		t.Fatalf("master %v", boxes[0])
	}
	if boxes[1] != (Box{63, 0, 37, 30}) {
		t.Fatalf("stack %v", boxes[1])
	}
	boxes = arrange(100, 30, 3, 0.62, nil, 1, 1)
	if boxes[1] != (Box{63, 0, 37, 15}) || boxes[2] != (Box{63, 16, 37, 14}) {
		t.Fatalf("stack %v %v", boxes[1], boxes[2])
	}
}

func TestArrangeNarrowPutsMasterOnTop(t *testing.T) {
	boxes := arrange(60, 40, 3, 0.62, nil, 1, 1)
	if boxes[0] != (Box{0, 0, 60, 25}) {
		t.Fatalf("master %v", boxes[0])
	}
	for _, b := range boxes[1:] {
		if b.Y < 26 || b.X != 0 || b.W != 60 {
			t.Fatalf("stack %v", b)
		}
	}
	if boxes[1].Y+boxes[1].H+1 != boxes[2].Y || boxes[2].Y+boxes[2].H != 40 {
		t.Fatalf("stack %v %v", boxes[1], boxes[2])
	}
}

func TestArrangeOverflowsIntoStackColumns(t *testing.T) {
	boxes := arrange(160, 12, 9, 0.62, nil, 1, 1)
	cols := map[int]int{}
	for _, b := range boxes[1:] {
		if b.H < minRows {
			t.Fatalf("too short %v", b)
		}
		cols[b.X]++
	}
	if len(cols) != 3 {
		t.Fatalf("stack columns %v", boxes)
	}
	for _, b := range boxes[1:] {
		if b.X+b.W > 160 || b.Y+b.H > 12 {
			t.Fatalf("outside the stage %v", b)
		}
	}
	cramped := arrange(100, 12, 9, 0.62, nil, 1, 1)
	cols = map[int]int{}
	for _, b := range cramped[1:] {
		if b.H < 1 || b.X+b.W > 100 || b.Y+b.H > 12 {
			t.Fatalf("cramped %v", b)
		}
		cols[b.X]++
	}
	if len(cols) != 2 {
		t.Fatalf("width caps the columns %v", cramped)
	}
}

func TestArrangeWeightsChangeStackHeights(t *testing.T) {
	boxes := arrange(100, 31, 3, 0.62, []float64{1, 3, 1}, 1, 1)
	if boxes[1].H <= boxes[2].H*2 {
		t.Fatalf("weights ignored %v %v", boxes[1], boxes[2])
	}
	if boxes[1].H+boxes[2].H+1 != 31 {
		t.Fatalf("stack does not fill %v %v", boxes[1], boxes[2])
	}
}

func TestSplitNeverExceedsTheTotal(t *testing.T) {
	cases := []struct {
		total, n, gap, min int
		weights            []float64
	}{{30, 3, 1, 3, nil}, {10, 4, 1, 3, nil}, {2, 5, 1, 1, nil}, {40, 2, 0, 1, []float64{1, 3}}, {7, 7, 0, 1, nil}}
	for _, c := range cases {
		sizes := split(c.total, c.weights, c.n, c.gap, c.min)
		if len(sizes) != c.n {
			t.Fatalf("%v gave %d sizes", c, len(sizes))
		}
		sum := 0
		for _, s := range sizes {
			if s < 0 {
				t.Fatalf("%v negative %v", c, sizes)
			}
			sum += s
		}
		if sum+c.gap*(c.n-1) > c.total && c.total >= c.n {
			t.Fatalf("%v over the total %v", c, sizes)
		}
	}
	if sizes := split(40, []float64{1, 3}, 2, 0, 1); sizes[0] != 10 || sizes[1] != 30 {
		t.Fatalf("%v", sizes)
	}
	if sizes := split(0, nil, 0, 1, 1); len(sizes) != 0 {
		t.Fatalf("%v", sizes)
	}
}

func TestClampRatioAndShrunk(t *testing.T) {
	if clampRatio(0) != minRatio || clampRatio(1) != maxRatio || clampRatio(0.5) != 0.5 {
		t.Fatal("ratio clamp")
	}
	x, y, w, h := shrunk(Box{10, 20, 40, 20}, 0.5)
	if x != 20 || y != 25 || w != 20 || h != 10 {
		t.Fatalf("%v %v %v %v", x, y, w, h)
	}
}

func TestBoxContains(t *testing.T) {
	b := Box{2, 3, 4, 5}
	if !b.contains(2, 3) || !b.contains(5, 7) || b.contains(6, 3) || b.contains(2, 8) || b.empty() {
		t.Fatal("contains")
	}
	if !(Box{}).empty() {
		t.Fatal("empty")
	}
}

func TestArrangeNeverPanics(t *testing.T) {
	for w := 0; w <= 60; w += 7 {
		for h := 0; h <= 60; h += 7 {
			for n := 0; n <= 9; n++ {
				boxes := arrange(w, h, n, 0.62, nil, 1, 1)
				if n > 0 && w > 0 && h > 0 && len(boxes) != n {
					t.Fatalf("%d %d %d gave %d boxes", w, h, n, len(boxes))
				}
				for _, b := range boxes {
					if b.X < 0 || b.Y < 0 || b.W < 0 || b.H < 0 {
						t.Fatalf("%d %d %d negative %v", w, h, n, b)
					}
				}
			}
		}
	}
}

func TestArrangeMainCountStacksTheMainArea(t *testing.T) {
	one := arrange(100, 30, 4, 0.62, nil, 1, 1)
	if one[0] != (Box{0, 0, 62, 30}) || one[1] != (Box{63, 0, 37, 10}) || one[2] != (Box{63, 11, 37, 9}) || one[3] != (Box{63, 21, 37, 9}) {
		t.Fatalf("one main %v", one)
	}
	two := arrange(100, 30, 4, 0.62, nil, 1, 2)
	if two[0] != (Box{0, 0, 62, 15}) || two[1] != (Box{0, 16, 62, 14}) || two[2] != (Box{63, 0, 37, 15}) || two[3] != (Box{63, 16, 37, 14}) {
		t.Fatalf("two main make quadrants %v", two)
	}
	all := arrange(100, 30, 3, 0.62, nil, 1, 3)
	if all[0] != (Box{0, 0, 100, 10}) || all[1] != (Box{0, 11, 100, 9}) || all[2] != (Box{0, 21, 100, 9}) {
		t.Fatalf("every tile in main stacks the stage %v", all)
	}
	if got := arrange(100, 30, 2, 0.62, nil, 1, 9); got[0] != (Box{0, 0, 100, 15}) || got[1] != (Box{0, 16, 100, 14}) {
		t.Fatalf("main count clamps to the tile count %v", got)
	}
	narrow := arrange(60, 30, 3, 0.62, nil, 1, 2)
	if narrow[0] != (Box{0, 0, 60, 9}) || narrow[1] != (Box{0, 10, 60, 9}) || narrow[2] != (Box{0, 20, 60, 10}) {
		t.Fatalf("narrow stages stack the main area on top %v", narrow)
	}
	weighted := arrange(100, 30, 3, 0.62, []float64{1, 3, 1}, 1, 1)
	if weighted[1].H != 22 || weighted[2].H != 7 {
		t.Fatalf("weights count per tile %v", weighted)
	}
	if got := arrange(100, 30, 1, 0.62, nil, 1, 0); got[0] != (Box{0, 0, 100, 30}) {
		t.Fatalf("a lone tile fills the stage %v", got)
	}
}

func TestMainCountCommandsAndPersistence(t *testing.T) {
	m, a, b, c := threeTiles(t)
	ws := m.Active()
	if cards := ws.Cards(); cards[0] != a || cards[1] != b || cards[2] != c {
		t.Fatalf("new tiles land at the top of the stack %v", cards)
	}
	if ws.MainCount() != 1 || m.Run("layout.main.fewer") != nil {
		t.Fatal("one main window to start, with nothing fewer to go to")
	}
	m.Update(key('=', tea.ModAlt|tea.ModShift))
	if ws.MainCount() != 2 || ws.holds(a).box != (Box{2, 1, 60, 13}) || ws.holds(b).box != (Box{2, 15, 60, 13}) || ws.holds(c).box != (Box{63, 1, 35, 27}) {
		t.Fatalf("alt+shift+= adds a main window %d %v %v %v", ws.MainCount(), ws.holds(a).box, ws.holds(b).box, ws.holds(c).box)
	}
	m.SetMainCount(5)
	if ws.MainCount() != 3 || m.SetMainCount(1) != nil {
		t.Fatal("main count clamps to the tiles and nudges past it")
	}
	l := m.Layout()
	if l.Main != 3 {
		t.Fatalf("%+v", l)
	}
	m.Update(key('-', tea.ModAlt|tea.ModShift))
	if ws.MainCount() != 2 || m.Run("layout.main.fewer") == nil || ws.MainCount() != 1 {
		t.Fatal("alt+shift+- and the command take main windows away")
	}
	m.SetLayout(Dwindle)
	if m.SetMainCount(1) != nil || m.Run("layout.main.more") != nil || ws.MainCount() != 1 {
		t.Fatal("main count only applies to the master layout")
	}
	m.SetLayout(Master)
	m.SetMainCount(1)
	m.ResetLayout()
	if ws.MainCount() != 1 {
		t.Fatal("reset returns to one main window")
	}
	fresh := still(t, 100, 30)
	fresh.AddWorkspace("1", newStub("x"))
	fresh.Restore(l)
	fresh.AddWorkspace("2", newStub("y"))
	for _, w := range fresh.Workspaces() {
		if w.MainCount() != 3 {
			t.Fatalf("restore applies the main count to every workspace, %d", w.MainCount())
		}
	}
}
