package deck

import "math"

// Arrangement of the deck, main beside stack, one card filling the screen, or cards fanned along a time axis
type Mode int

const (
	Split Mode = iota
	Solo
	Timeline
)

func (m Mode) String() string {
	switch m {
	case Solo:
		return "solo"
	case Timeline:
		return "timeline"
	}
	return "split"
}

// Mode named by a string, false for an unknown name
func ParseMode(s string) (Mode, bool) {
	switch s {
	case "split":
		return Split, true
	case "solo":
		return Solo, true
	case "timeline":
		return Timeline, true
	}
	return Split, false
}

type box struct {
	x, y, w, h int
}

const (
	gutter         = 1
	minTile        = 6
	strip          = 1
	minOpen        = 3
	chip           = 3
	minMainW       = 8
	minStackW      = 9
	narrowWidth    = 40
	ultrawideWidth = 180
	ultrawideCards = 5
	timelineStep   = 6
	timelineInset  = 1
	timelineShare  = 0.6
)

// Sizing demands a card declares through the Pinned and Sized interfaces
type constraint struct {
	pinned bool
	minW   int
	minH   int
}

// Rows a pinned card keeps when the stack is crowded
func (c constraint) rows() int {
	if !c.pinned {
		return strip
	}
	return max(minOpen, c.minH)
}

// Resting box of every card with no card declaring constraints
func layout(w, h, n int, ratio float64, mode Mode, focus, open int) []box {
	return layoutWith(w, h, n, ratio, mode, focus, open, nil)
}

// Resting box of every card, opening the focused stack card or the remembered one when main holds focus
func layoutWith(w, h, n int, ratio float64, mode Mode, focus, open int, cons []constraint) []box {
	boxes := make([]box, n)
	if n == 0 || w <= 0 || h <= 0 {
		return boxes
	}
	if len(cons) != n {
		cons = make([]constraint, n)
	}
	focus = min(max(focus, 0), n-1)
	if n == 1 {
		boxes[0] = box{0, 0, w, h}
		return boxes
	}
	switch mode {
	case Solo:
		return soloLayout(w, h, n, focus)
	case Timeline:
		return timelineLayout(w, h, n, focus)
	}
	if w < narrowWidth {
		return columnLayout(w, h, n, focus, cons)
	}
	mainW := splitWidth(w, ratio)
	boxes[0] = box{0, 0, mainW, h}
	stackX := mainW + gutter
	stackW := max(1, w-stackX)
	if focus > 0 {
		open = focus
	}
	open = min(max(open, 1), n-1) - 1
	if w >= ultrawideWidth && n-1 >= ultrawideCards {
		return wideLayout(boxes, stackX, stackW, h, n, focus, open, cons)
	}
	ys, hs := stackLayout(h, n-1, open, cons[1:])
	for i := 1; i < n; i++ {
		boxes[i] = box{stackX, ys[i-1], stackW, hs[i-1]}
	}
	return boxes
}

// Stack spread over two columns on an ultrawide screen, the focused card open in its own column
func wideLayout(boxes []box, stackX, stackW, h, n, focus, open int, cons []constraint) []box {
	ns := n - 1
	first := (ns + 1) / 2
	colW := (stackW - gutter) / 2
	cols := [][2]int{{1, 1 + first}, {1 + first, n}}
	for c, span := range cols {
		lo, hi := span[0], span[1]
		count := hi - lo
		if count <= 0 {
			continue
		}
		o := 0
		if focus >= lo && focus < hi {
			o = focus - lo
		} else if open+1 >= lo && open+1 < hi {
			o = open + 1 - lo
		}
		ys, hs := stackLayout(h, count, o, cons[lo:hi])
		x := stackX + c*(colW+gutter)
		w := colW
		if c == 1 {
			w = stackW - colW - gutter
		}
		for i := lo; i < hi; i++ {
			boxes[i] = box{x, ys[i-lo], w, hs[i-lo]}
		}
	}
	return boxes
}

// Every card in one column on a narrow screen, the focused card open and the rest tiled or folded
func columnLayout(w, h, n, focus int, cons []constraint) []box {
	boxes := make([]box, n)
	ys, hs := stackLayout(h, n, focus, cons)
	for i := range boxes {
		boxes[i] = box{0, ys[i], w, hs[i]}
	}
	return boxes
}

// Width of the main slot, keeping both columns usable on narrow screens
func splitWidth(w int, ratio float64) int {
	hi := w - gutter - minStackW
	if hi < minMainW {
		return max(1, w/2)
	}
	return min(max(int(math.Round(float64(w)*ratio)), minMainW), hi)
}

// Tops and heights of stack cards, tiling when they fit and otherwise opening one card between rows of title strips
func stackLayout(h, ns, open int, cons []constraint) (ys, hs []int) {
	ys, hs = make([]int, ns), make([]int, ns)
	if ns == 0 {
		return ys, hs
	}
	if len(cons) != ns {
		cons = make([]constraint, ns)
	}
	open = min(max(open, 0), ns-1)
	if ns == 1 {
		hs[0] = h
		return ys, hs
	}
	if h/ns >= minTile && !anyPinned(cons) {
		y := 0
		for i := range ys {
			hi := h / ns
			if i < h%ns {
				hi++
			}
			ys[i], hs[i] = y, hi
			y += hi
		}
		return ys, hs
	}
	rest := 0
	for i, c := range cons {
		if i != open {
			rest += c.rows()
		}
	}
	openH := max(h-rest, min(h, max(minOpen, cons[open].minH)))
	y := 0
	for i := range ys {
		hi := cons[i].rows()
		if i == open {
			hi = openH
		}
		ys[i], hs[i] = y, hi
		y += hi
	}
	if over := ys[open] + openH - h; over > 0 {
		for i := range ys {
			ys[i] -= over
		}
	}
	return ys, hs
}

func anyPinned(cons []constraint) bool {
	for _, c := range cons {
		if c.pinned {
			return true
		}
	}
	return false
}

// Focused card filling the screen beside a spine of glyph chips for the rest
func soloLayout(w, h, n, focus int) []box {
	boxes := make([]box, n)
	boxes[focus] = box{0, 0, max(1, w-chip-gutter), h}
	others := n - 1
	step := float64(chip)
	if others > 1 && others*chip > h {
		step = math.Max(1, float64(h-chip)/float64(others-1))
	}
	k := 0
	for i := range boxes {
		if i == focus {
			continue
		}
		boxes[i] = box{w - chip, int(math.Round(float64(k) * step)), chip, chip}
		k++
	}
	return boxes
}

// Cards fanned along the width like a hand, the focused one centered and full height and each neighbor stepped aside and shortened by its distance
func timelineLayout(w, h, n, focus int) []box {
	boxes := make([]box, n)
	cw := max(minMainW, int(math.Round(float64(w)*timelineShare)))
	cw = min(cw, w)
	cx := (w - cw) / 2
	for i := range boxes {
		d := i - focus
		dist := d
		if dist < 0 {
			dist = -dist
		}
		x := cx + d*timelineStep
		x = max(x, chip-cw)
		x = min(x, w-chip)
		inset := min(dist*timelineInset, max(0, (h-minOpen)/2))
		boxes[i] = box{x, inset, cw, max(1, h-2*inset)}
	}
	return boxes
}

// Stacking rank, with later stack cards above earlier ones and the focused card above all
func depth(i, n, focus int) int {
	if i == focus {
		return n + 1
	}
	return i + 1
}

// Stacking rank along the timeline, falling off with distance from the focused card
func timelineDepth(i, n, focus int) int {
	d := i - focus
	if d < 0 {
		d = -d
	}
	return n - d
}
