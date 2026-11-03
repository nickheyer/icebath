package termlab

import (
	"image"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
)

// Recorder of every change to a box of mirrored cells, stamped with the read time of the output causing it
type Watch struct {
	s     *Session
	box   image.Rectangle
	base  []uv.Cell
	cur   []uv.Cell
	marks []mark
}

// Cells one output chunk changed, as indexes into the box in row order with their new state
type mark struct {
	at    time.Time
	idx   []int
	cells []uv.Cell
}

// Starts recording the box, given in cells, from the state the mirror holds now
func (s *Session) Watch(box image.Rectangle) *Watch {
	box = box.Intersect(image.Rect(0, 0, s.cols, s.rows))
	w := &Watch{s: s, box: box}
	s.outMu.Lock()
	defer s.outMu.Unlock()
	w.base = w.read()
	w.cur = append([]uv.Cell(nil), w.base...)
	s.watches = append(s.watches, w)
	return w
}

// Stops recording
func (w *Watch) Stop() {
	w.s.outMu.Lock()
	defer w.s.outMu.Unlock()
	for i, o := range w.s.watches {
		if o == w {
			w.s.watches = append(w.s.watches[:i], w.s.watches[i+1:]...)
			return
		}
	}
}

// Cells of the box from the mirror in row order, called with the output lock held
func (w *Watch) read() []uv.Cell {
	out := make([]uv.Cell, 0, w.box.Dx()*w.box.Dy())
	base := w.s.mirror.Base()
	for y := w.box.Min.Y; y < w.box.Max.Y; y++ {
		l, ok := w.s.mirror.Row(base + uint64(y))
		for x := w.box.Min.X; x < w.box.Max.X; x++ {
			if ok && x < len(l.Cells) {
				out = append(out, l.Cells[x])
			} else {
				out = append(out, uv.Cell{})
			}
		}
	}
	return out
}

// Records the cells the chunk just fed into the mirror changed, called with the output lock held
func (w *Watch) track(at time.Time) {
	now := w.read()
	var m mark
	for i := range now {
		if !same(&now[i], &w.cur[i]) {
			m.idx = append(m.idx, i)
			m.cells = append(m.cells, now[i])
		}
	}
	if len(m.idx) > 0 {
		m.at = at
		w.marks = append(w.marks, m)
		w.cur = now
	}
}

// Reports whether two cells show equal text in an equal style
func same(a, b *uv.Cell) bool {
	return a.Content == b.Content && a.Width == b.Width && a.Style.Equal(&b.Style)
}

// State of the box after every chunk read at or before the time
func (w *Watch) At(t time.Time) []uv.Cell {
	w.s.outMu.Lock()
	defer w.s.outMu.Unlock()
	return w.stateAt(t)
}

// State of the box at a time, called with the output lock held
func (w *Watch) stateAt(t time.Time) []uv.Cell {
	out := append([]uv.Cell(nil), w.base...)
	for _, m := range w.marks {
		if m.at.After(t) {
			break
		}
		for k, i := range m.idx {
			out[i] = m.cells[k]
		}
	}
	return out
}

// Indexes of the box cells some chunk read after from and at or before to changed
func (w *Watch) Moved(from, to time.Time) map[int]bool {
	w.s.outMu.Lock()
	defer w.s.outMu.Unlock()
	out := map[int]bool{}
	for _, m := range w.marks {
		if !m.at.After(from) {
			continue
		}
		if m.at.After(to) {
			break
		}
		for _, i := range m.idx {
			out[i] = true
		}
	}
	return out
}

// Indexes of the box cells that differ between the states at the two times
func (w *Watch) Diff(from, to time.Time) []int {
	w.s.outMu.Lock()
	defer w.s.outMu.Unlock()
	a, b := w.stateAt(from), w.stateAt(to)
	var out []int
	for i := range a {
		if !same(&a[i], &b[i]) {
			out = append(out, i)
		}
	}
	return out
}

// Read time of the first chunk after the moment leaving at least the share of cells changed from that moment
func (w *Watch) Answer(after time.Time, cells []int, share float64) (time.Time, bool) {
	w.s.outMu.Lock()
	defer w.s.outMu.Unlock()
	if len(cells) == 0 {
		return time.Time{}, false
	}
	from := w.stateAt(after)
	state := append([]uv.Cell(nil), from...)
	for _, m := range w.marks {
		if !m.at.After(after) {
			continue
		}
		for k, i := range m.idx {
			state[i] = m.cells[k]
		}
		n := 0
		for _, i := range cells {
			if !same(&state[i], &from[i]) {
				n++
			}
		}
		if float64(n) >= share*float64(len(cells)) {
			return m.at, true
		}
	}
	return time.Time{}, false
}

// Screen column and row of a box index
func (w *Watch) Cell(i int) image.Point {
	return image.Pt(w.box.Min.X+i%w.box.Dx(), w.box.Min.Y+i/w.box.Dx())
}
