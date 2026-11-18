package tty

import (
	"hash/maphash"
	"slices"
	"sort"
)

// Rows top to bot shifted by k, content moving up for positive k
type region struct{ top, bot, k int }

// Most distinct shifts a frame plan weighs
const maxShifts = 6

// Seed shared by every row hash of a process
var rowSeed = maphash.MakeSeed()

// Hash of a row by its exact cells, zero when any of its cells is unknown
func rowHash(cells []Cell, known []bool) uint64 {
	var h maphash.Hash
	h.SetSeed(rowSeed)
	for x := range cells {
		if known != nil && !known[x] {
			return 0
		}
		c := &cells[x]
		h.WriteString(c.Text)
		h.WriteString(c.Rider)
		h.WriteString(c.After)
		var b [24]byte
		b[0] = c.Width
		b[1], b[2] = c.Style.Under, c.Style.Attrs
		for i, v := range [...]uint32{uint32(c.Style.Fg), uint32(c.Style.Bg), uint32(c.Style.Ul), c.Link} {
			b[3+4*i], b[4+4*i], b[5+4*i], b[6+4*i] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
		}
		h.Write(b[:19])
	}
	if s := h.Sum64(); s != 0 {
		return s
	}
	return 1
}

// Hash of a target row by its exact cells
func wantHash(row []want, scratch []Cell) uint64 {
	for x := range row {
		scratch[x] = row[x].Cell
	}
	return rowHash(scratch, nil)
}

// Estimated bytes repainting target row from a terminal row, src nil meaning a row of unknown content
func repaintEstimate(row []want, src []Cell, known []bool) int {
	cost, inRun := 0, false
	for x := 0; x < len(row); x++ {
		c := &row[x]
		same := src != nil && known[x] && !c.blind && sameCell(&src[x], &c.Cell)
		if same {
			inRun = false
			continue
		}
		if !inRun {
			cost += 8
			inRun = true
		}
		if c.Width > 0 && c.Text != " " {
			cost += clusterCost(c)
		}
	}
	return cost
}

// Estimated bytes of one scroll op of a shift
func shiftCost(k int) int {
	n := max(k, -k)
	return 15 + min(n-1, 2*digits(n))
}

// DP state of the vertical plan, a row free, used by a shift, or lost to one
type vstate struct {
	mode, k, r int
}

const (
	vFree = iota
	vUsed
	vLost
	vLead
)

// Shift regions that make the terminal rows cheapest to bring to the target, found by a DP over rows
func (pl *planner) scrollPlan(s *screen, tgt []want) []region {
	h, w := s.h, s.w
	if h < 3 {
		return nil
	}
	scratch := make([]Cell, w)
	byHash := map[uint64][]int{}
	for y := 0; y < h; y++ {
		if hv := rowHash(s.row(y), s.rowKnown(y)); hv != 0 {
			byHash[hv] = append(byHash[hv], y)
		}
	}
	support := map[int]int{}
	for y := 0; y < h; y++ {
		hv := wantHash(tgt[y*w:(y+1)*w], scratch)
		if rowHash(s.row(y), s.rowKnown(y)) == hv {
			continue
		}
		for _, src := range byHash[hv] {
			if k := src - y; k != 0 {
				support[k]++
			}
		}
	}
	if len(support) == 0 {
		return nil
	}
	shifts := make([]int, 0, len(support))
	for k := range support {
		shifts = append(shifts, k)
	}
	sort.Slice(shifts, func(i, j int) bool {
		if support[shifts[i]] != support[shifts[j]] {
			return support[shifts[i]] > support[shifts[j]]
		}
		return shifts[i] < shifts[j]
	})
	if len(shifts) > maxShifts {
		shifts = shifts[:maxShifts]
	}
	est := func(y, k int) int {
		row := tgt[y*w : (y+1)*w]
		if k == 0 {
			return repaintEstimate(row, s.row(y), s.rowKnown(y))
		}
		return repaintEstimate(row, s.row(y+k), s.rowKnown(y+k))
	}
	full := func(y int) int { return repaintEstimate(tgt[y*w:(y+1)*w], nil, nil) }
	type node struct {
		cost   int
		parent int
		st     vstate
	}
	layers := make([][]node, h)
	ready := func(st vstate) bool {
		return st.mode == vFree || (st.mode == vUsed && st.k < 0) || (st.mode == vLost && st.r == 1)
	}
	push := func(y int, st vstate, cost, parent int) {
		for i := range layers[y] {
			if layers[y][i].st == st {
				if cost < layers[y][i].cost {
					layers[y][i] = node{cost, parent, st}
				}
				return
			}
		}
		layers[y] = append(layers[y], node{cost, parent, st})
	}
	starts := func(y, base, parent int) {
		push(y, vstate{vFree, 0, 0}, base+est(y, 0), parent)
		for _, k := range shifts {
			if k > 0 && y+k < h {
				push(y, vstate{vUsed, k, 0}, base+shiftCost(k)+est(y, k), parent)
			}
			if k < 0 && y-k < h {
				push(y, vstate{vLead, k, -k}, base+shiftCost(k)+full(y), parent)
			}
		}
	}
	starts(0, 0, -1)
	for y := 1; y < h; y++ {
		for pi, p := range layers[y-1] {
			st := p.st
			if ready(st) {
				starts(y, p.cost, pi)
			}
			switch st.mode {
			case vUsed:
				if st.k > 0 && y+st.k < h {
					push(y, st, p.cost+est(y, st.k), pi)
				}
				if st.k > 0 {
					push(y, vstate{vLost, st.k, st.k}, p.cost+full(y), pi)
				}
				if st.k < 0 {
					push(y, st, p.cost+est(y, st.k), pi)
				}
			case vLost:
				if st.r > 1 {
					push(y, vstate{vLost, st.k, st.r - 1}, p.cost+full(y), pi)
				}
			case vLead:
				if st.r > 1 {
					push(y, vstate{vLead, st.k, st.r - 1}, p.cost+full(y), pi)
				} else {
					push(y, vstate{vUsed, st.k, 0}, p.cost+est(y, st.k), pi)
				}
			}
		}
	}
	best := -1
	for i, n := range layers[h-1] {
		if ready(n.st) && (best < 0 || n.cost < layers[h-1][best].cost) {
			best = i
		}
	}
	if best < 0 {
		return nil
	}
	path := make([]vstate, h)
	for y, i := h-1, best; y >= 0; y-- {
		path[y] = layers[y][i].st
		i = layers[y][i].parent
	}
	var out []region
	for y := 0; y < h; {
		st := path[y]
		if st.mode == vFree {
			y++
			continue
		}
		t := y
		for y < h && path[y].mode != vFree && path[y].k == st.k && !(y > t && path[y].mode == vUsed && path[y-1].mode == vLost) && !(y > t && path[y].mode == vLead && path[y-1].mode != vLead) {
			y++
		}
		out = append(out, region{t, y - 1, st.k})
	}
	return slices.Clip(out)
}

// Writes each planned scroll by the shorter of an IL DL pair and DECSTBM with LF or RI
func (pl *planner) emitScroll(s *screen, regions []region) {
	for _, g := range regions {
		a, b, k := g.top, g.bot, g.k
		pair := pairCost(s, a, b, k)
		margins := marginCost(s, a, b, k)
		switch {
		case pair <= margins && k > 0:
			s.deleteLines(a, k)
			if b < s.h-1 {
				s.insertLines(b-k+1, k)
			}
		case pair <= margins:
			if b < s.h-1 {
				s.deleteLines(b+k+1, -k)
			}
			s.insertLines(a, -k)
		default:
			s.scrollRegion(a, b, k)
		}
	}
}

// Bytes the IL DL pair for a region costs from the cursor
func pairCost(s *screen, a, b, k int) int {
	n := max(k, -k)
	op := len(csiN('M', n))
	if k > 0 {
		c := motionCost(s.cursor(), 0, a, s.h) + op
		if b < s.h-1 {
			c += motionCost(cursor{0, a, false, true}, 0, b-k+1, s.h) + op
		}
		return c
	}
	c := 0
	at := s.cursor()
	if b < s.h-1 {
		c += motionCost(at, 0, b-n+1, s.h) + op
		at = cursor{0, b - n + 1, false, true}
	}
	return c + motionCost(at, 0, a, s.h) + op
}

// Bytes DECSTBM with LF or RI costs for a region
func marginCost(s *screen, a, b, k int) int {
	n := max(k, -k)
	c := n
	if k < 0 {
		c = 2 * n
	}
	if a == 0 && b == s.h-1 {
		if k > 0 {
			return c + motionCost(s.cursor(), 0, b, s.h)
		}
		return c + motionCost(s.cursor(), 0, a, s.h)
	}
	c += 4 + digits(a+1) + digits(b+1) + 3
	if k > 0 {
		return c + len(cup(nil, 0, b))
	}
	return c + len(cup(nil, 0, a))
}
