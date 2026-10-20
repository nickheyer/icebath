package deck

import "math"

// Arrangement of the deck, either main beside stack or one card filling the screen
type Mode int

const (
	Split Mode = iota
	Solo
)

type box struct {
	x, y, w, h int
}

const (
	gutter    = 1
	minTile   = 6
	strip     = 1
	minOpen   = 3
	chip      = 3
	minMainW  = 8
	minStackW = 9
)

// Resting box of every card, opening the focused stack card or the remembered one when main holds focus
func layout(w, h, n int, ratio float64, mode Mode, focus, open int) []box {
	boxes := make([]box, n)
	if n == 0 || w <= 0 || h <= 0 {
		return boxes
	}
	focus = min(max(focus, 0), n-1)
	if n == 1 {
		boxes[0] = box{0, 0, w, h}
		return boxes
	}
	if mode == Solo {
		return soloLayout(w, h, n, focus)
	}
	mainW := splitWidth(w, ratio)
	boxes[0] = box{0, 0, mainW, h}
	stackX := mainW + gutter
	stackW := max(1, w-stackX)
	if focus > 0 {
		open = focus
	}
	ys, hs := stackLayout(h, n-1, min(max(open, 1), n-1)-1)
	for i := 1; i < n; i++ {
		boxes[i] = box{stackX, ys[i-1], stackW, hs[i-1]}
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
func stackLayout(h, ns, open int) (ys, hs []int) {
	ys, hs = make([]int, ns), make([]int, ns)
	if ns == 1 {
		hs[0] = h
		return ys, hs
	}
	if h/ns >= minTile {
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
	openH := max(h-(ns-1)*strip, min(h, minOpen))
	y := 0
	for i := range ys {
		hi := strip
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

// Stacking rank, with later stack cards above earlier ones and the focused card above all
func depth(i, n, focus int) int {
	if i == focus {
		return n + 1
	}
	return i + 1
}
