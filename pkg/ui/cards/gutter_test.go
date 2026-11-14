package cards

import "testing"

func TestGutterDrag(t *testing.T) {
	g := gutter{h: 22, total: 400, offset: 0, visible: 22}
	ts, tl := g.track()
	if ts != 1 || tl != 20 {
		t.Fatalf("track %d %d", ts, tl)
	}
	th, thl := g.thumb()
	if th != 1 || thl < 1 {
		t.Fatalf("thumb %d %d", th, thl)
	}
	if !g.onTrack(1) || !g.onTrack(20) || g.onTrack(0) || g.onTrack(21) {
		t.Fatal("track rows")
	}
	if g.grab(1) != 0 || g.grab(15) != thl/2 {
		t.Fatalf("grab %d %d", g.grab(1), g.grab(15))
	}
	held := g.grab(th)
	if got := g.dragTo(th, held); got != 0 {
		t.Fatalf("drag to the top gives %d", got)
	}
	if got := g.dragTo(ts+tl, held); got != g.total-g.visible {
		t.Fatalf("drag past the bottom gives %d, want %d", got, g.total-g.visible)
	}
	mid := g.dragTo(ts+tl/2, held)
	if mid <= 0 || mid >= g.total-g.visible {
		t.Fatalf("drag to the middle gives %d", mid)
	}
	for y := ts; y < ts+tl; y++ {
		g.offset = g.dragTo(y, held)
		if start, _ := g.thumb(); start > y-held+1 || start < y-held-1 {
			t.Fatalf("thumb at %d after a drag to %d", start, y)
		}
	}
	still := gutter{h: 10, total: 5, visible: 10}
	if still.onTrack(3) || still.dragTo(3, 0) != 0 {
		t.Fatal("a gutter with nothing to scroll took a drag")
	}
}
