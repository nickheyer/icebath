package wm

import "math"

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
