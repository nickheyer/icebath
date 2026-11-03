package vxl

import (
	"slices"
	"sort"
)

// Pixels a pass changed, as runs across every row, or every pixel at once
type Dirt struct {
	W, H int
	// Whether every pixel counts as changed
	All bool
	// Start and inclusive end of every run of a row, in pairs, sorted and merged once normalized
	rows [][]int32
	// Rows set aside while the runs are widened
	scratch [][]int32
}

// Empties the set over a frame of the size, keeping the storage of its rows
func (d *Dirt) Reset(w, h int) {
	d.W, d.H, d.All = max(w, 0), max(h, 0), false
	if cap(d.rows) < d.H {
		d.rows = slices.Grow(d.rows[:0], d.H)
	}
	d.rows = d.rows[:d.H]
	for y := range d.rows {
		d.rows[y] = d.rows[y][:0]
	}
}

// Marks every pixel changed
func (d *Dirt) Fill() {
	d.All = true
	for y := range d.rows {
		d.rows[y] = d.rows[y][:0]
	}
}

// Adds a run of a row, ends inclusive and clamped to the frame, safe from one goroutine per row
func (d *Dirt) Add(y, x0, x1 int) {
	if d.All || y < 0 || y >= d.H {
		return
	}
	x0, x1 = max(x0, 0), min(x1, d.W-1)
	if x0 > x1 {
		return
	}
	row := d.rows[y]
	if n := len(row); n > 0 && int(row[n-1]) >= x0-1 && int(row[n-2]) <= x0 {
		row[n-1] = int32(max(int(row[n-1]), x1))
		return
	}
	d.rows[y] = append(row, int32(x0), int32(x1))
}

// Adds a box of pixels, corners inclusive
func (d *Dirt) AddRect(x0, y0, x1, y1 int) {
	for y := max(y0, 0); y <= min(y1, d.H-1); y++ {
		d.Add(y, x0, x1)
	}
}

// Sorts and merges the runs of every row
func (d *Dirt) Norm() {
	if d.All {
		return
	}
	parallelBands(d.H, 64, func(y0, y1 int) {
		for y := y0; y < y1; y++ {
			d.rows[y] = mergeRuns(d.rows[y])
		}
	})
}

// Runs sorted by start and overlapping or touching ones joined, written over the input
func mergeRuns(row []int32) []int32 {
	n := len(row) / 2
	if n < 2 {
		return row
	}
	sorted := true
	for i := 1; i < n; i++ {
		if row[2*i] < row[2*i-2] {
			sorted = false
			break
		}
	}
	if !sorted {
		sort.Sort(runSort(row))
	}
	out := row[:2]
	for i := 1; i < n; i++ {
		x0, x1 := row[2*i], row[2*i+1]
		if last := len(out) - 1; x0 <= out[last]+1 {
			out[last] = max(out[last], x1)
			continue
		}
		out = append(out, x0, x1)
	}
	return out
}

// Runs of a row ordered by their start
type runSort []int32

func (r runSort) Len() int           { return len(r) / 2 }
func (r runSort) Less(i, j int) bool { return r[2*i] < r[2*j] }
func (r runSort) Swap(i, j int) {
	r[2*i], r[2*j] = r[2*j], r[2*i]
	r[2*i+1], r[2*j+1] = r[2*j+1], r[2*i+1]
}

// Adds every run of another set over the same frame
func (d *Dirt) Union(o *Dirt) {
	if o == nil || o.W != d.W || o.H != d.H {
		return
	}
	if o.All {
		d.Fill()
		return
	}
	if d.All {
		return
	}
	for y := range d.rows {
		if len(o.rows[y]) > 0 {
			d.rows[y] = append(d.rows[y], o.rows[y]...)
		}
	}
}

// Takes the size and every run of another set
func (d *Dirt) CopyFrom(o *Dirt) {
	d.Reset(o.W, o.H)
	if o.All {
		d.Fill()
		return
	}
	for y := range d.rows {
		d.rows[y] = append(d.rows[y][:0], o.rows[y]...)
	}
}

// Widens every run by dx pixels on either side and copies it onto the dy rows above and below
func (d *Dirt) Grow(dx, dy int) {
	if d.All || dx <= 0 && dy <= 0 {
		return
	}
	d.Norm()
	if cap(d.scratch) < d.H {
		d.scratch = slices.Grow(d.scratch[:0], d.H)
	}
	d.scratch = d.scratch[:d.H]
	for y := range d.rows {
		d.scratch[y] = append(d.scratch[y][:0], d.rows[y]...)
		d.rows[y] = d.rows[y][:0]
	}
	for y := range d.H {
		for sy := max(y-dy, 0); sy <= min(y+dy, d.H-1); sy++ {
			row := d.scratch[sy]
			for i := 0; i+1 < len(row); i += 2 {
				d.Add(y, int(row[i])-dx, int(row[i+1])+dx)
			}
		}
	}
	d.Norm()
}

// Reports whether no pixel is marked
func (d *Dirt) Empty() bool {
	if d.All {
		return d.W == 0 || d.H == 0
	}
	for y := range d.rows {
		if len(d.rows[y]) > 0 {
			return false
		}
	}
	return true
}

// Pixels marked
func (d *Dirt) Count() int {
	if d.All {
		return d.W * d.H
	}
	d.Norm()
	n := 0
	for y := range d.rows {
		row := d.rows[y]
		for i := 0; i+1 < len(row); i += 2 {
			n += int(row[i+1]-row[i]) + 1
		}
	}
	return n
}

// Calls fn for every run of a row, ends inclusive, the whole row when every pixel is marked
func (d *Dirt) Each(y int, fn func(x0, x1 int)) {
	if y < 0 || y >= d.H {
		return
	}
	if d.All {
		fn(0, d.W-1)
		return
	}
	row := d.rows[y]
	for i := 0; i+1 < len(row); i += 2 {
		fn(int(row[i]), int(row[i+1]))
	}
}

// First and last marked pixel of a row, false for a row without any
func (d *Dirt) Hull(y int) (x0, x1 int, ok bool) {
	if y < 0 || y >= d.H {
		return 0, 0, false
	}
	if d.All {
		return 0, d.W - 1, true
	}
	row := d.rows[y]
	if len(row) < 2 {
		return 0, 0, false
	}
	return int(row[0]), int(row[len(row)-1]), true
}

// Reports whether a pixel is marked
func (d *Dirt) Has(x, y int) bool {
	if y < 0 || y >= d.H || x < 0 || x >= d.W {
		return false
	}
	if d.All {
		return true
	}
	row := d.rows[y]
	for i := 0; i+1 < len(row); i += 2 {
		if int(row[i]) <= x && x <= int(row[i+1]) {
			return true
		}
	}
	return false
}

// Reports whether any pixel of a box is marked, corners inclusive
func (d *Dirt) Touches(x0, y0, x1, y1 int) bool {
	if d.All {
		return x1 >= 0 && y1 >= 0 && x0 < d.W && y0 < d.H
	}
	for y := max(y0, 0); y <= min(y1, d.H-1); y++ {
		row := d.rows[y]
		for i := 0; i+1 < len(row); i += 2 {
			if int(row[i]) <= x1 && x0 <= int(row[i+1]) {
				return true
			}
		}
	}
	return false
}

// Smallest box around every marked pixel, false when none is
func (d *Dirt) Bounds() (x0, y0, x1, y1 int, ok bool) {
	if d.All {
		return 0, 0, d.W - 1, d.H - 1, d.W > 0 && d.H > 0
	}
	d.Norm()
	x0, y0, x1, y1 = d.W, d.H, -1, -1
	for y := range d.rows {
		if row := d.rows[y]; len(row) > 1 {
			x0, x1 = min(x0, int(row[0])), max(x1, int(row[len(row)-1]))
			y0, y1 = min(y0, y), y
		}
	}
	return x0, y0, x1, y1, y1 >= 0
}

// Pixels a flush changed, the ones it drew surfaces on and the ones it lit anew
type Changes struct {
	// Every pixel whose color changed
	Color Dirt
	// Pixels whose surface, depth or facing changed
	Drawn Dirt
	// Pixels whose light changed, the drawn ones lit anew and the rest by what moved around them
	Lit Dirt
}

// Empties every set over a frame of the size
func (c *Changes) Reset(w, h int) {
	c.Color.Reset(w, h)
	c.Drawn.Reset(w, h)
	c.Lit.Reset(w, h)
}

// Marks every pixel in every set
func (c *Changes) Fill() {
	c.Color.Fill()
	c.Drawn.Fill()
	c.Lit.Fill()
}

// Adds every run of other changes
func (c *Changes) Union(o *Changes) {
	c.Color.Union(&o.Color)
	c.Drawn.Union(&o.Drawn)
	c.Lit.Union(&o.Lit)
}

// Takes every set of other changes
func (c *Changes) CopyFrom(o *Changes) {
	c.Color.CopyFrom(&o.Color)
	c.Drawn.CopyFrom(&o.Drawn)
	c.Lit.CopyFrom(&o.Lit)
}

// Sorts and merges the runs of every set
func (c *Changes) Norm() {
	c.Color.Norm()
	c.Drawn.Norm()
	c.Lit.Norm()
}

// Reports whether every pixel of every set is marked
func (c *Changes) All() bool { return c.Color.All && c.Drawn.All && c.Lit.All }
