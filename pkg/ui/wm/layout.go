package wm

import (
	"math"
	"slices"

	"github.com/nickheyer/icebath/pkg/ui/anim"
)

// Rectangle of whole cells
type Box struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

func (b Box) contains(x, y int) bool {
	return x >= b.X && x < b.X+b.W && y >= b.Y && y < b.Y+b.H
}

func (b Box) empty() bool { return b.W <= 0 || b.H <= 0 }

// Direction on the stage
type Dir int

const (
	Left Dir = iota
	Right
	Up
	Down
)

// Arrangement of the tiles of a workspace, a master beside a stack or a spiral of splits
type Mode int

const (
	Master Mode = iota
	Dwindle
)

func (m Mode) String() string {
	if m == Dwindle {
		return "dwindle"
	}
	return "master"
}

// Mode named by a string, false for an unknown name
func ParseMode(s string) (Mode, bool) {
	switch s {
	case "master":
		return Master, true
	case "dwindle":
		return Dwindle, true
	}
	return Master, false
}

const (
	minMaster    = 24
	minStack     = 18
	minRows      = 3
	minCols      = 12
	narrowWidth  = 72
	minRatio     = 0.3
	maxRatio     = 0.8
	defaultRatio = 0.62
	minSplitW    = 12
	minSplitH    = 3
	minSplit     = 0.2
	maxSplit     = 0.8
)

// Boxes for n tiles in a stage, the first mainCount stacked in the main area and the rest stacked beside or below it
func arrange(w, h, n int, ratio float64, weights []float64, gap, mainCount int) []Box {
	if n <= 0 || w <= 0 || h <= 0 {
		return nil
	}
	mainCount = min(max(mainCount, 1), n)
	boxes := make([]Box, n)
	if mainCount == n {
		copy(boxes, stackBoxes(Box{0, 0, w, h}, n, weights, gap))
		return boxes
	}
	ratio = clampRatio(ratio)
	var area, stack Box
	if w >= narrowWidth && w >= minMaster+gap+minStack {
		mw := min(max(int(math.Round(float64(w)*ratio)), minMaster), w-gap-minStack)
		area = Box{0, 0, mw, h}
		stack = Box{mw + gap, 0, w - mw - gap, h}
	} else {
		mh := min(max(int(math.Round(float64(h)*ratio)), minRows), max(h-gap-minRows, 1))
		area = Box{0, 0, w, mh}
		stack = Box{0, mh + gap, w, max(h-mh-gap, 0)}
	}
	var mw, sw []float64
	if len(weights) == n {
		mw, sw = weights[:mainCount], weights[mainCount:]
	}
	copy(boxes[:mainCount], stackBoxes(area, mainCount, mw, gap))
	copy(boxes[mainCount:], stackBoxes(stack, n-mainCount, sw, gap))
	return boxes
}

// Boxes for n stack entries inside an area, in columns when rows run short, each column split by weight
func stackBoxes(area Box, n int, weights []float64, gap int) []Box {
	out := make([]Box, n)
	if n == 0 || area.W <= 0 || area.H <= 0 {
		return out
	}
	perCol := max(1, (area.H+gap)/(minRows+gap))
	cols := (n + perCol - 1) / perCol
	cols = min(cols, max(1, (area.W+gap)/(minCols+gap)))
	cols = max(cols, 1)
	perCol = (n + cols - 1) / cols
	widths := split(area.W, nil, cols, gap, 1)
	x := area.X
	for c := 0; c < cols; c++ {
		lo, hi := c*perCol, min((c+1)*perCol, n)
		if lo >= hi {
			break
		}
		var ws []float64
		if len(weights) == n {
			ws = weights[lo:hi]
		}
		heights := split(area.H, ws, hi-lo, gap, 1)
		y := area.Y
		for i := lo; i < hi; i++ {
			out[i] = Box{x, y, widths[c], heights[i-lo]}
			y += heights[i-lo] + gap
		}
		x += widths[c] + gap
	}
	return out
}

// Sizes that share total among n parts by weight with gaps between them, every part at least minEach when it fits
func split(total int, weights []float64, n, gap, minEach int) []int {
	out := make([]int, n)
	if n == 0 {
		return out
	}
	avail := total - gap*(n-1)
	if avail < n {
		for i := range out {
			if i < avail {
				out[i] = 1
			}
		}
		return out
	}
	sum := 0.0
	ws := make([]float64, n)
	for i := range ws {
		ws[i] = 1
		if len(weights) == n && weights[i] > 0 {
			ws[i] = weights[i]
		}
		sum += ws[i]
	}
	used := 0
	for i := range out {
		out[i] = max(minEach, int(math.Floor(float64(avail)*ws[i]/sum)))
		used += out[i]
	}
	for i := n - 1; used > avail && i >= 0; i-- {
		over := min(used-avail, out[i]-1)
		out[i] -= over
		used -= over
	}
	for i := 0; used < avail; i = (i + 1) % n {
		out[i]++
		used++
	}
	return out
}

func clampRatio(r float64) float64 { return min(max(r, minRatio), maxRatio) }

func clampSplit(r float64) float64 { return min(max(r, minSplit), maxSplit) }

// Box shrunk toward its center by a share of its size, where cards pop in from and out to
func shrunk(b Box, share float64) (x, y, w, h float64) {
	w = float64(b.W) * (1 - share)
	h = float64(b.H) * (1 - share)
	x = float64(b.X) + (float64(b.W)-w)/2
	y = float64(b.Y) + (float64(b.H)-h)/2
	return x, y, w, h
}

// Box of a share of the target size centered on the origin a card opened from another grows out of
func grown(origin, target Box, share float64) (x, y, w, h float64) {
	w = float64(target.W) * share
	h = float64(target.H) * share
	x = float64(origin.X) + (float64(origin.W)-w)/2
	y = float64(origin.Y) + (float64(origin.H)-h)/2
	return x, y, w, h
}

// Split tree of the dwindle arrangement, a leaf holding a tile or a cut holding two subtrees
type node struct {
	leaf     *entry
	a, b     *node
	parent   *node
	vertical bool
	ratio    float64
	box      Box
}

// Assigns the box to the subtree, cutting it between the children by the ratio with a gap
func (n *node) layout(box Box, gap int) {
	n.box = box
	if n.leaf != nil {
		n.leaf.box = box
		return
	}
	r := clampSplit(n.ratio)
	if n.vertical {
		aw := min(max(int(math.Round(float64(box.W)*r)), minSplitW), max(box.W-gap-minSplitW, 1))
		n.a.layout(Box{box.X, box.Y, aw, box.H}, gap)
		n.b.layout(Box{box.X + aw + gap, box.Y, max(box.W-aw-gap, 0), box.H}, gap)
		return
	}
	ah := min(max(int(math.Round(float64(box.H)*r)), minSplitH), max(box.H-gap-minSplitH, 1))
	n.a.layout(Box{box.X, box.Y, box.W, ah}, gap)
	n.b.layout(Box{box.X, box.Y + ah + gap, box.W, max(box.H-ah-gap, 0)}, gap)
}

// Tiles of the subtree in reading order
func (n *node) leaves(out []*entry) []*entry {
	if n == nil {
		return out
	}
	if n.leaf != nil {
		return append(out, n.leaf)
	}
	return n.b.leaves(n.a.leaves(out))
}

// Leaf holding the tile
func (n *node) find(e *entry) *node {
	if n == nil {
		return nil
	}
	if n.leaf == e {
		return n
	}
	if f := n.a.find(e); f != nil {
		return f
	}
	return n.b.find(e)
}

// Sets every cut of the subtree back to an even split
func (n *node) even() {
	if n == nil || n.leaf != nil {
		return
	}
	n.ratio = 0.5
	n.a.even()
	n.b.even()
}

// Cut direction a box takes when a tile is opened over it, side by side only when it is three times wider than tall in cells
func cutOf(b Box) bool {
	return b.empty() || b.W >= 3*b.H
}

// Card placed in a workspace with the motion state of its frame
type entry struct {
	id       int
	card     Card
	ws       *Workspace
	rect     anim.Rect
	open     anim.Value
	lit      anim.Value
	hover    anim.Value
	pulse    anim.Value
	shake    anim.Value
	hide     anim.Value
	urgency  Urgency
	decode   float64
	weight   float64
	floating bool
	box      Box
	fbox     Box
	origin   Box
	placed   bool
	held     bool
	closing  bool
	dirty    bool
	dim      float64
	raw      screen
	tinted   screen
	view     string
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
	shift  anim.Value
	placed bool
}

// Arrangement the tiles follow
func (w *Workspace) Mode() Mode { return w.mode }

// Tiles the main area holds in the master arrangement
func (w *Workspace) MainCount() int { return max(w.main, 1) }

// Reports whether the workspace asked for attention while not shown
func (w *Workspace) Urgent() bool { return w.urgent }

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

// Puts a float on top of the pile
func (w *Workspace) addFloat(e *entry) {
	w.floats = append(w.floats, e)
}

// Raises a float to the top of the pile
func (w *Workspace) raise(e *entry) {
	if i := slices.Index(w.floats, e); i >= 0 {
		w.floats = slices.Delete(w.floats, i, i+1)
		w.addFloat(e)
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
