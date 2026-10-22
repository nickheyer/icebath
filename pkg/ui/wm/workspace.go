package wm

import (
	"slices"

	"github.com/nickheyer/icebath/pkg/ui/anim"
)

// Card placed in a workspace with the motion state of its frame
type entry struct {
	id       int
	card     Card
	ws       *Workspace
	rect     anim.Rect
	open     anim.Value
	glow     anim.Value
	hover    anim.Value
	lift     anim.Value
	pulse    anim.Value
	urgency  Urgency
	decode   float64
	weight   float64
	floating bool
	box      Box
	fbox     Box
	placed   bool
	held     bool
	closing  bool
	dirty    bool
	dim      float64
	cw, ch   int
	raw      screen
	tinted   screen
	hints    []hintSpan
}

// Columns one legend entry covers on the bottom edge, in stage coordinates
type hintSpan struct {
	x0, x1 int
	id     string
}

func (e *entry) key() string { return keyOf(e.card) }

// Group of cards bound to one session, tiled in master and stack order with dialogs floating above
type Workspace struct {
	ID     string
	tiles  []*entry
	floats []*entry
	focus  *entry
	zoom   *entry
	ratio  float64
	mode   Mode
	main   int
	tree   *node
	urgent bool
	busy   bool
	shift  anim.Value
	placed bool
}

// Arrangement the tiles follow
func (w *Workspace) Mode() Mode { return w.mode }

// Tiles the main area holds in the master arrangement
func (w *Workspace) MainCount() int { return max(w.main, 1) }

// Reports whether the workspace asked for attention while not shown
func (w *Workspace) Urgent() bool { return w.urgent }

// Reports whether the workspace was marked busy
func (w *Workspace) Busy() bool { return w.busy }

// Marks the workspace busy, shown as a dot on its chip
func (w *Workspace) SetBusy(on bool) { w.busy = on }

// Cards in the workspace, tiles first then floats
func (w *Workspace) Cards() []Card {
	out := make([]Card, 0, len(w.tiles)+len(w.floats))
	for _, e := range w.entries() {
		out = append(out, e.card)
	}
	return out
}

// Card with the input focus
func (w *Workspace) Focused() Card {
	if w.focus == nil {
		return nil
	}
	return w.focus.card
}

// Card filling the master area
func (w *Workspace) Master() Card {
	if len(w.tiles) == 0 {
		return nil
	}
	return w.tiles[0].card
}

// Card filling the stage, nil when none is zoomed
func (w *Workspace) Zoomed() Card {
	if w.zoom == nil {
		return nil
	}
	return w.zoom.card
}

// Share of the stage the master area takes
func (w *Workspace) Ratio() float64 { return w.ratio }

func (w *Workspace) entries() []*entry {
	out := make([]*entry, 0, len(w.tiles)+len(w.floats))
	out = append(out, w.tiles...)
	out = append(out, w.floats...)
	return out
}

func (w *Workspace) find(key string) *entry {
	for _, e := range w.entries() {
		if !e.closing && e.key() == key {
			return e
		}
	}
	return nil
}

func (w *Workspace) holds(c Card) *entry {
	for _, e := range w.entries() {
		if e.card == c {
			return e
		}
		if h, ok := e.card.(Holder); ok && h.Holds(c) {
			return e
		}
	}
	return nil
}

func (w *Workspace) remove(e *entry) {
	w.removeLeaf(e)
	if i := slices.Index(w.tiles, e); i >= 0 {
		w.tiles = slices.Delete(w.tiles, i, i+1)
	}
	if i := slices.Index(w.floats, e); i >= 0 {
		w.floats = slices.Delete(w.floats, i, i+1)
	}
	if w.zoom == e {
		w.zoom = nil
	}
	if w.focus == e {
		w.focus = nil
	}
}

// Raises a float to the top of the pile
func (w *Workspace) raise(e *entry) {
	if i := slices.Index(w.floats, e); i >= 0 && i != len(w.floats)-1 {
		w.floats = slices.Delete(w.floats, i, i+1)
		w.floats = append(w.floats, e)
	}
}

// Trades two tiles while focus stays on the same card
func (w *Workspace) swap(a, b *entry) {
	i, j := slices.Index(w.tiles, a), slices.Index(w.tiles, b)
	if i < 0 || j < 0 || i == j {
		return
	}
	w.tiles[i], w.tiles[j] = w.tiles[j], w.tiles[i]
	na, nb := w.tree.find(a), w.tree.find(b)
	if na != nil && nb != nil {
		na.leaf, nb.leaf = b, a
	}
}

// Tile whose box lies nearest beyond the edge of e in the direction, nil at the stage edge
func (w *Workspace) neighbor(e *entry, d Dir) *entry {
	var best *entry
	bestDist := 0
	for _, o := range w.tiles {
		if o == e || o.closing {
			continue
		}
		a, b := e.box, o.box
		var dist int
		switch d {
		case Left:
			if b.X+b.W > a.X || !overlaps(a.Y, a.H, b.Y, b.H) {
				continue
			}
			dist = a.X - (b.X + b.W)
		case Right:
			if b.X < a.X+a.W || !overlaps(a.Y, a.H, b.Y, b.H) {
				continue
			}
			dist = b.X - (a.X + a.W)
		case Up:
			if b.Y+b.H > a.Y || !overlaps(a.X, a.W, b.X, b.W) {
				continue
			}
			dist = a.Y - (b.Y + b.H)
		case Down:
			if b.Y < a.Y+a.H || !overlaps(a.X, a.W, b.X, b.W) {
				continue
			}
			dist = b.Y - (a.Y + a.H)
		}
		if best == nil || dist < bestDist {
			best, bestDist = o, dist
		}
	}
	return best
}

func overlaps(a, aw, b, bw int) bool {
	return a < b+bw && b < a+aw
}

// Tile under the stage point, floats first from the top of the pile
func (w *Workspace) at(x, y int) *entry {
	for i := len(w.floats) - 1; i >= 0; i-- {
		if e := w.floats[i]; !e.closing && e.frame().contains(x, y) {
			return e
		}
	}
	if w.zoom != nil {
		if w.zoom.frame().contains(x, y) {
			return w.zoom
		}
		return nil
	}
	for _, e := range w.tiles {
		if !e.closing && e.frame().contains(x, y) {
			return e
		}
	}
	return nil
}

// Box the frame of the entry is painted in right now
func (e *entry) frame() Box {
	x, y, w, h := e.rect.Cells()
	return Box{x, y, w, h}
}

// Slot index of the tile whose resting box holds the stage point, -1 when none does
func (w *Workspace) slotAt(x, y int) int {
	for i, e := range w.tiles {
		if e.box.contains(x, y) {
			return i
		}
	}
	return -1
}

// Tiles of the workspace that are not on their way out
func (w *Workspace) live() []*entry {
	out := make([]*entry, 0, len(w.tiles))
	for _, e := range w.tiles {
		if !e.closing {
			out = append(out, e)
		}
	}
	return out
}

// Adds a tile, cutting the focused tile in the dwindle tree or landing at the top of the master stack
func (w *Workspace) addTile(e *entry) {
	if w.mode == Dwindle {
		at := w.focus
		if at == nil || at.floating || at.closing {
			at = nil
		}
		w.insertLeaf(e, at)
		w.syncTiles()
		return
	}
	at := min(w.MainCount(), len(w.tiles))
	w.tiles = slices.Insert(w.tiles, at, e)
}

// Cuts the leaf of at, or the last leaf, in two with e on the right or below
func (w *Workspace) insertLeaf(e, at *entry) {
	nl := &node{leaf: e, ratio: 0.5}
	if w.tree == nil {
		w.tree = nl
		return
	}
	target := w.tree.find(at)
	if target == nil {
		if last := w.tree.leaves(nil); len(last) > 0 {
			target = w.tree.find(last[len(last)-1])
		}
	}
	if target == nil {
		w.tree = nl
		return
	}
	cut := &node{a: target, b: nl, parent: target.parent, vertical: cutOf(target.box), ratio: 0.5, box: target.box}
	switch {
	case target.parent == nil:
		w.tree = cut
	case target.parent.a == target:
		target.parent.a = cut
	default:
		target.parent.b = cut
	}
	target.parent, nl.parent = cut, cut
}

// Drops the leaf of e, its sibling taking the place of their cut
func (w *Workspace) removeLeaf(e *entry) {
	n := w.tree.find(e)
	if n == nil {
		return
	}
	p := n.parent
	if p == nil {
		w.tree = nil
		return
	}
	sib := p.a
	if sib == n {
		sib = p.b
	}
	sib.parent = p.parent
	switch {
	case p.parent == nil:
		w.tree = sib
	case p.parent.a == p:
		p.parent.a = sib
	default:
		p.parent.b = sib
	}
}

// Flips the cut that holds the tile, reporting whether there was one
func (w *Workspace) flipSplit(e *entry) bool {
	n := w.tree.find(e)
	if n == nil || n.parent == nil {
		return false
	}
	n.parent.vertical = !n.parent.vertical
	return true
}

// Cut whose boundary is the edge of the tile, nil when that edge is the stage edge
func (w *Workspace) splitFor(e *entry, reg region) *node {
	cur := w.tree.find(e)
	if cur == nil {
		return nil
	}
	for p := cur.parent; p != nil; cur, p = p, p.parent {
		switch reg {
		case rRight:
			if p.vertical && p.a == cur {
				return p
			}
		case rLeft:
			if p.vertical && p.b == cur {
				return p
			}
		case rBottom:
			if !p.vertical && p.a == cur {
				return p
			}
		}
	}
	return nil
}

// Rebuilds the tree from the tile order as a spiral laid out in the stage box
func (w *Workspace) rebuildTree(stage Box, gap int) {
	w.tree = nil
	var prev *entry
	for _, e := range w.live() {
		w.insertLeaf(e, prev)
		w.tree.layout(stage, gap)
		prev = e
	}
}

// Orders the tiles as the tree reads them, cards on their way out trailing
func (w *Workspace) syncTiles() {
	out := w.tree.leaves(nil)
	for _, e := range w.tiles {
		if e.closing {
			out = append(out, e)
		}
	}
	w.tiles = out
}
